// Copyright 2021 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package aggregatorservice contains the functions needed for handling the aggregation requests.
package aggregatorservice

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"

	log "github.com/golang/glog"
	"cloud.google.com/go/pubsub"
	"cloud.google.com/go/storage"
	"github.com/google/privacy-sandbox-aggregation-service/pipeline/ioutils"
	"github.com/google/privacy-sandbox-aggregation-service/service/query"
	"github.com/google/privacy-sandbox-aggregation-service/service/utils"
)

// DataflowCfg contains parameters necessary for running pipelines on Dataflow.
type DataflowCfg struct {
	Project         string
	Region          string
	TempLocation    string
	StagingLocation string
}

// ServerCfg contains file URIs necessary for the service.
type ServerCfg struct {
	PrivateKeyParamsURI             string
	DpfAggregatePartialReportBinary string
}

// SharedInfoHandler handles HTTP requests for the information shared with other helpers.
type SharedInfoHandler struct {
	SharedInfo *query.HelperSharedInfo
}

func (h *SharedInfoHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	b, err := json.Marshal(h.SharedInfo)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		log.Error(err)
	}

	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, b)
}

// QueryHandler handles the request in the pubsub messages.
type QueryHandler struct {
	ServerCfg                 ServerCfg
	PipelineRunner            string
	DataflowCfg               DataflowCfg
	Origin                    string
	SharedDir                 string
	RequestPubSubTopic        string
	RequestPubsubSubscription string

	PubSubTopicClient, PubSubSubscriptionClient *pubsub.Client
	GCSClient                                   *storage.Client
}

// Setup creates the cloud API clients.
func (h *QueryHandler) Setup(ctx context.Context) error {
	topicProject, _, err := utils.ParsePubSubResourceName(h.RequestPubSubTopic)
	if err != nil {
		return err
	}
	h.PubSubTopicClient, err = pubsub.NewClient(ctx, topicProject)
	if err != nil {
		return err
	}

	subscriptionProject, _, err := utils.ParsePubSubResourceName(h.RequestPubsubSubscription)
	if err != nil {
		return err
	}

	if subscriptionProject == topicProject {
		h.PubSubSubscriptionClient = h.PubSubTopicClient
	} else {
		h.PubSubSubscriptionClient, err = pubsub.NewClient(ctx, subscriptionProject)
		if err != nil {
			return err
		}

	}

	h.GCSClient, err = storage.NewClient(ctx)
	return err
}

// Close closes the cloud API clients.
func (h *QueryHandler) Close() {
	h.PubSubTopicClient.Close()
	h.PubSubSubscriptionClient.Close()
	h.GCSClient.Close()
}

// SetupPullRequests gets ready to pull requests contained in a PubSub message subscription, and handles the request.
func (h *QueryHandler) SetupPullRequests(ctx context.Context) error {
	_, subID, err := utils.ParsePubSubResourceName(h.RequestPubsubSubscription)
	if err != nil {
		return err
	}
	sub := h.PubSubSubscriptionClient.Subscription(subID)

	// Only allow pulling one message at a time to avoid overloading the memory.
	sub.ReceiveSettings.Synchronous = true
	sub.ReceiveSettings.MaxOutstandingMessages = 1
	return sub.Receive(ctx, func(ctx context.Context, msg *pubsub.Message) {
		request := &query.AggregateRequest{}
		err := json.Unmarshal(msg.Data, request)
		if err != nil {
			log.Error(err)
			msg.Nack()
			return
		}
		if err := h.aggregatePartialReportHierarchy(ctx, request); err != nil {
			log.Error(err)
			msg.Nack()
			return
		}
		msg.Ack()
	})
}

func (h *QueryHandler) aggregatePartialReportHierarchy(ctx context.Context, request *query.AggregateRequest) error {
	config, err := query.ReadExpansionConfigFile(ctx, request.ExpandConfigURI)
	if err != nil {
		return nil
	}

	finalLevel := int32(len(config.PrefixLengths)) - 1
	if request.Level > finalLevel {
		return fmt.Errorf("expect request level <= finalLevel %d, got %d", finalLevel, request.Level)
	}

	// If it is not the first-level aggregation, check if the result from the partner helper is ready for the previous level.
	if request.Level > 0 {
		exist, err := utils.IsGCSObjectExist(ctx, h.GCSClient,
			query.GetRequestPartialResultURI(request.PartnerSharedInfo.SharedDir, request.QueryID, request.Level-1),
		)
		if err != nil {
			return err
		}
		if !exist {
			// When the partial result from the partner helper is not ready, nack the message with an error.
			return fmt.Errorf("result from %s for level %s is not ready", request.PartnerSharedInfo.Origin, request.QueryID)
		}
	}

	request, err = query.GetRequestParams(ctx, config, request,
		h.SharedDir,
		request.PartnerSharedInfo.SharedDir,
	)
	if err != nil {
		return err
	}

	var outputResultURI string
	// The final-level results are not supposed to be shared with the partner helpers.
	if request.Level == finalLevel {
		outputResultURI = query.GetRequestPartialResultURI(request.ResultDir, request.QueryID, request.Level)
	} else {
		outputResultURI = query.GetRequestPartialResultURI(h.SharedDir, request.QueryID, request.Level)
	}

	args := []string{
		"--partial_report_file=" + request.PartialReportURI,
		"--sum_parameters_file=" + request.SumParamsURI,
		"--prefixes_file=" + request.PrefixesURI,
		"--partial_histogram_file=" + outputResultURI,
		"--epsilon=" + fmt.Sprintf("%f", request.TotalEpsilon*config.PrivacyBudgetPerPrefix[request.Level]),
		"--private_key_params_uri=" + h.ServerCfg.PrivateKeyParamsURI,
		"--runner=" + h.PipelineRunner,
	}

	if h.PipelineRunner == "dataflow" {
		args = append(args,
			"--project="+h.DataflowCfg.Project,
			"--region="+h.DataflowCfg.Region,
			"--temp_location="+h.DataflowCfg.TempLocation,
			"--staging_location="+h.DataflowCfg.StagingLocation,
			"--worker_binary="+h.ServerCfg.DpfAggregatePartialReportBinary,
		)
	}

	str := h.ServerCfg.DpfAggregatePartialReportBinary
	for _, s := range args {
		str = fmt.Sprintf("%s\n%s", str, s)
	}
	log.Infof("Running command\n%s", str)

	cmd := exec.CommandContext(ctx, h.ServerCfg.DpfAggregatePartialReportBinary, args...)
	var out bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	err = cmd.Run()
	if err != nil {
		log.Errorf("%s: %s", err, stderr.String())
		return err
	}
	log.Infof("output of cmd: %s", out.String())

	if request.Level == finalLevel {
		log.Infof("query %q complete", request.QueryID)
		return nil
	}

	// If the hierarchical query is not finished yet, publish the requests for the next-level aggregation.
	request.Level++
	_, topic, err := utils.ParsePubSubResourceName(h.RequestPubSubTopic)
	if err != nil {
		return err
	}
	return utils.PublishRequest(ctx, h.PubSubTopicClient, topic, request)
}

// ReadHelperSharedInfo reads the helper shared info from a URL.
func ReadHelperSharedInfo(ctx context.Context, url string) (*query.HelperSharedInfo, error) {
	b, err := ioutils.ReadBytes(ctx, url)
	if err != nil {
		return nil, err
	}
	info := &query.HelperSharedInfo{}
	if err := json.Unmarshal(b, info); err != nil {
		return nil, err
	}
	return info, nil
}