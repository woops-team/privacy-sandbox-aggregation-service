#include "pipeline/distributed_point_function_c_bridge.h"

#include <alloca.h>

#include <cstdint>
#include <cstdlib>
#include <iostream>
#include <memory>
#include <string>
#include <vector>

#include "absl/numeric/int128.h"
#include "absl/status/status.h"
#include "absl/status/statusor.h"
#include "absl/strings/escaping.h"
#include "absl/types/span.h"
#include "pipeline/cbytes.h"
#include "pipeline/cbytes_utils.h"
#include "dpf/distributed_point_function.h"
#include "dpf/distributed_point_function.pb.h"

using ::convagg::crypto::AllocateCBytes;
using ::convagg::crypto::StrToCBytes;
using ::distributed_point_functions::DistributedPointFunction;
using ::distributed_point_functions::DpfKey;
using ::distributed_point_functions::DpfParameters;
using ::distributed_point_functions::EvaluationContext;

int CGenerateKeys(const struct CBytes* param, uint64_t alpha, uint64_t beta,
                  struct CBytes* out_key1, struct CBytes* out_key2,
                  struct CBytes* out_error) {
  DpfParameters parameters;
  if (!parameters.ParseFromArray(param->c, param->l)) {
    StrToCBytes("fail to parse DpfParameter", out_error);
    return static_cast<int>(absl::StatusCode::kInvalidArgument);
  }

  absl::StatusOr<std::unique_ptr<DistributedPointFunction>> dpf =
      DistributedPointFunction::Create(parameters);
  if (!dpf.ok()) {
    StrToCBytes(dpf.status().message(), out_error);
    return dpf.status().raw_code();
  }

  absl::StatusOr<std::pair<DpfKey, DpfKey>> keys =
      dpf.value()->GenerateKeys(absl::uint128(alpha), absl::uint128(beta));
  if (!keys.ok()) {
    StrToCBytes(keys.status().message(), out_error);
    return keys.status().raw_code();
  }

  if (!AllocateCBytes(keys->first.ByteSizeLong(), out_key1) ||
      !keys->first.SerializeToArray(out_key1->c, out_key1->l)) {
    StrToCBytes("fail to copy DpfKey", out_error);
    return static_cast<int>(absl::StatusCode::kInternal);
  }
  if (!AllocateCBytes(keys->second.ByteSizeLong(), out_key2) ||
      !keys->second.SerializeToArray(out_key2->c, out_key2->l)) {
    StrToCBytes("fail to copy DpfKey", out_error);
    return static_cast<int>(absl::StatusCode::kInternal);
  }

  return static_cast<int>(absl::StatusCode::kOk);
}

int CCreateEvaluationContext(const struct CBytes* param,
                             const struct CBytes* key,
                             struct CBytes* out_eval_context,
                             struct CBytes* out_error) {
  DpfParameters parameters;
  if (!parameters.ParseFromArray(param->c, param->l)) {
    StrToCBytes("fail to parse DpfParameter", out_error);
    return static_cast<int>(absl::StatusCode::kInvalidArgument);
  }

  absl::StatusOr<std::unique_ptr<DistributedPointFunction>> dpf =
      DistributedPointFunction::Create(parameters);
  if (!dpf.ok()) {
    StrToCBytes(dpf.status().message(), out_error);
    return dpf.status().raw_code();
  }

  DpfKey dpf_key;
  if (!dpf_key.ParseFromArray(key->c, key->l)) {
    StrToCBytes("fail to parse DpfKey", out_error);
    return static_cast<int>(absl::StatusCode::kInvalidArgument);
  }

  absl::StatusOr<EvaluationContext> eval_context =
      (*dpf)->CreateEvaluationContext(dpf_key);
  if (!eval_context.ok()) {
    StrToCBytes(eval_context.status().message(), out_error);
    return eval_context.status().raw_code();
  }

  if (!AllocateCBytes(eval_context->ByteSizeLong(), out_eval_context) ||
      !eval_context->SerializeToArray(out_eval_context->c,
                                      out_eval_context->l)) {
    StrToCBytes("fail to copy EvaluationContext", out_error);
    return static_cast<int>(absl::StatusCode::kInternal);
  }

  return static_cast<int>(absl::StatusCode::kOk);
}

int CEvaluateNext64(const struct CBytes* param, const uint64_t* prefixes,
                    int64_t prefixes_size, CBytes* mutable_context,
                    struct CUInt64Vec* out_vec, struct CBytes* out_error) {
  DpfParameters parameters;
  if (!parameters.ParseFromArray(param->c, param->l)) {
    StrToCBytes("fail to parse DpfParameter", out_error);
    return static_cast<int>(absl::StatusCode::kInvalidArgument);
  }

  absl::StatusOr<std::unique_ptr<DistributedPointFunction>> dpf =
      DistributedPointFunction::Create(parameters);
  if (!dpf.ok()) {
    StrToCBytes(dpf.status().message(), out_error);
    return dpf.status().raw_code();
  }

  std::vector<absl::uint128> prefixes_128(prefixes_size);
  for (int i = 0; i < prefixes_size; i++) {
    prefixes_128[i] = absl::uint128(prefixes[i]);
  }
  EvaluationContext eval_context;
  if (!eval_context.ParseFromArray(mutable_context->c, mutable_context->l)) {
    StrToCBytes("fail to parse EvaluationContext", out_error);
    return static_cast<int>(absl::StatusCode::kInvalidArgument);
  }

  auto maybe_result =
      dpf.value()->EvaluateNext<uint64_t>(prefixes_128, eval_context);
  if (!maybe_result.ok()) {
    StrToCBytes(maybe_result.status().message(), out_error);
    return maybe_result.status().raw_code();
  }
  free(mutable_context->c);
  if (!AllocateCBytes(eval_context.ByteSizeLong(), mutable_context) ||
      !eval_context.SerializeToArray(mutable_context->c, mutable_context->l)) {
    StrToCBytes("fail to copy EvaluationContext", out_error);
    return static_cast<int>(absl::StatusCode::kInternal);
  }

  int size = maybe_result->size();
  out_vec->vec_size = size;
  out_vec->vec = (uint64_t*)calloc(size, sizeof(uint64_t));
  if (out_vec->vec == nullptr) {
    StrToCBytes("fail to allocate memory for expanded vector", out_error);
    return static_cast<int>(absl::StatusCode::kInternal);
  }
  for (int i = 0; i < size; i++) {
    out_vec->vec[i] = (uint64_t)((*maybe_result)[i]);
  }
  return static_cast<int>(absl::StatusCode::kOk);
}
