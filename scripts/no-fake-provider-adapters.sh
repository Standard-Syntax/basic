#!/usr/bin/env bash
set -euo pipefail

production_paths=(
  go/cmd/workflow-service
  go/internal/reasoning/gateway
)
if (($# > 0)); then
  production_paths=("$@")
fi
readonly -a production_paths

readonly removed_pattern='FakeProvider|FakeProviderMode|FakeImplementationAdapter|FakeReviewAdapter|NewFakeImplementationAdapter|NewFakeReviewAdapter|fake_implementation_proposal_path|fake_review_proposal_path|readProtoJSON|deterministic-fake'
readonly alternate_branch_pattern='\b(if|switch)\b[^{]*[Pp]rovider\.Mode[^{]*\{'

scan_production() {
  local pattern=$1
  local multiline=$2

  if command -v rg >/dev/null 2>&1; then
    if [[ "$multiline" == true ]]; then
      rg -Un \
        --glob '*.go' \
        --glob '!**/*_test.go' \
        "$pattern" \
        "${production_paths[@]}"
    else
      rg -n \
        --glob '*.go' \
        --glob '!**/*_test.go' \
        "$pattern" \
        "${production_paths[@]}"
    fi
  else
    if [[ "$multiline" == true ]]; then
      grep -RPzn \
        --include='*.go' \
        --exclude='*_test.go' \
        -- \
        "$pattern" \
        "${production_paths[@]}"
    else
      grep -REn \
        --include='*.go' \
        --exclude='*_test.go' \
        -- \
        "$pattern" \
        "${production_paths[@]}"
    fi
  fi
}

status=0
for scan in "$removed_pattern:false" "$alternate_branch_pattern:true"; do
  pattern=${scan%:*}
  multiline=${scan##*:}
  if scan_production "$pattern" "$multiline"; then
    status=1
  else
    rc=$?
    if ((rc > 1)); then
      echo "unable to scan shipped provider composition (scanner exit $rc)" >&2
      exit "$rc"
    fi
  fi
done

if ((status != 0)); then
  echo "shipped fake provider or alternate provider composition path detected" >&2
  exit "$status"
fi
