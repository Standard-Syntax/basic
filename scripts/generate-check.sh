#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
snapshot="$(mktemp -d)"
trap 'rm -rf "$snapshot"' EXIT

cd "$repo_root"
find python/src/harness_agents/_generated -type d -name __pycache__ -prune -exec rm -rf {} +
mkdir -p "$snapshot/go" "$snapshot/python"
if [[ -d go/gen ]]; then cp -a go/gen/. "$snapshot/go/"; fi
if [[ -d python/src/harness_agents/_generated ]]; then
  cp -a python/src/harness_agents/_generated/. "$snapshot/python/"
fi

make --no-print-directory generate
diff -ruN "$snapshot/go" go/gen
diff -ruN "$snapshot/python" python/src/harness_agents/_generated
