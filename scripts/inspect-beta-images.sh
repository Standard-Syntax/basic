#!/usr/bin/env bash
set -euo pipefail

revision=${SOURCE_REVISION:?SOURCE_REVISION is required}

inspect_image() {
  local image=$1 component=$2 entrypoint=$3
  local user actual_entry actual_revision actual_component container archive listing
  user=$(docker image inspect --format '{{.Config.User}}' "$image")
  actual_entry=$(docker image inspect --format '{{json .Config.Entrypoint}}' "$image")
  actual_revision=$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' "$image")
  actual_component=$(docker image inspect --format '{{index .Config.Labels "io.standard-syntax.basic.component"}}' "$image")
  test "$user" = '65532:65532'
  test "$actual_entry" = "[\"$entrypoint\"]"
  test "$actual_revision" = "$revision"
  test "$actual_component" = "$component"
  container=$(docker create "$image")
  archive=$(mktemp)
  listing=$(mktemp)
  trap 'docker rm -f "$container" >/dev/null 2>&1 || true; rm -f "$archive" "$listing"' RETURN
  docker export "$container" >"$archive"
  tar -tf "$archive" >"$listing"
  grep -qx 'usr/bin/git' "$listing"
  if grep -Eq '(^|/)(docker|sh|bash|apk|apt|apt-get)$' "$listing"; then
    echo "$component image contains prohibited administration tooling" >&2
    return 1
  fi
  docker run --rm --entrypoint /usr/bin/git "$image" --version | grep -qx 'git version 2.52.0'
  docker rm -f "$container" >/dev/null
  rm -f "$archive" "$listing"
  trap - RETURN
}

inspect_image basic-api-service:beta api-service /api-service
inspect_image basic-workflow-service:beta workflow-service /workflow-service
