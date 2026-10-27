#!/usr/bin/env bash
set -euo pipefail

version=${VERSION:-$(bash "$(dirname "$0")/custom_version.sh")}
[[ "$version" == *-custom ]] || { echo "ERROR: refusing to publish an upstream version" >&2; exit 1; }
if [[ "$version" == *-dev.* ]]; then
  track=custom-testing
  docker_tag=edge-custom
elif [[ "$version" =~ -(alpha|beta|rc)\. ]]; then
  track=custom-beta
  docker_tag=latest-custom-beta
else
  track=custom
  docker_tag=latest-custom
fi
name=sing-box-custom
printf 'track=%s name=%s docker_tag=%s\n' "$track" "$name" "$docker_tag" >&2
printf 'TRACK=%s\nNAME=%s\nDOCKER_TAG=%s\n' "$track" "$name" "$docker_tag" >> "${GITHUB_ENV:?}"
