#!/usr/bin/env bash
set -euo pipefail

if (($# < 2)); then
  echo "usage: build-image.sh IMAGE BUILD_ARGUMENT..." >&2
  exit 2
fi

image=$1
shift
docker_cli=${DOCKER_CLI:-docker}
script_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
"$script_root/require-buildx.sh"

iid_file=$(mktemp)
trap 'rm -f -- "$iid_file"' EXIT
"$docker_cli" buildx build --load --iidfile "$iid_file" -t "$image" "$@" .

iid=$(tr -d '\r\n' <"$iid_file")
loaded=$("$docker_cli" image inspect --format '{{.Id}}' "$image")
if [[ ! $iid =~ ^sha256:[a-f0-9]{64}$ ]] || [[ $loaded != "$iid" ]]; then
  echo "loaded image ID mismatch for $image: iidfile=$iid inspect=$loaded" >&2
  exit 1
fi
