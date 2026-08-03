#!/usr/bin/env bash
set -euo pipefail

docker_cli=${DOCKER_CLI:-docker}
required=v0.36.0

if ! output=$("$docker_cli" buildx version 2>&1); then
  echo "Buildx $required is required; install the Docker CLI plugin before building images" >&2
  exit 2
fi

version=$(awk 'NR == 1 {print $2}' <<<"$output")
if [[ $version != "$required" ]]; then
  echo "Buildx $required is required; found ${version:-unknown}" >&2
  exit 2
fi
