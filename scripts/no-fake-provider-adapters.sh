#!/usr/bin/env bash
set -euo pipefail

readonly production_paths=(
  go/cmd/workflow-service
  go/internal/reasoning/gateway
)
readonly removed_pattern='FakeProvider|FakeProviderMode|FakeImplementationAdapter|FakeReviewAdapter|NewFakeImplementationAdapter|NewFakeReviewAdapter|fake_implementation_proposal_path|fake_review_proposal_path|readProtoJSON|deterministic-fake'
readonly alternate_branch_pattern='(if|switch).*[Pp]rovider\.Mode'

status=0
for pattern in "$removed_pattern" "$alternate_branch_pattern"; do
  if rg -n \
    --glob '*.go' \
    --glob '!**/*_test.go' \
    "$pattern" \
    "${production_paths[@]}"; then
    status=1
  else
    rc=$?
    if ((rc > 1)); then
      echo "unable to scan shipped provider composition (rg exit $rc)" >&2
      exit "$rc"
    fi
  fi
done

if ((status != 0)); then
  echo "shipped fake provider or alternate provider composition path detected" >&2
  exit "$status"
fi
