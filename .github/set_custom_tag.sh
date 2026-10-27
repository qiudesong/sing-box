#!/usr/bin/env bash
set -euo pipefail

# Build tools expect a local exact tag. Never overwrite an existing tag.
version=${1:?Usage: set_custom_tag.sh VERSION}
[[ "$version" == *-custom ]] || { echo "ERROR: version must end in -custom" >&2; exit 1; }
tag=v${version#v}
if git show-ref --verify --quiet "refs/tags/$tag"; then
  [[ "$(git rev-parse "refs/tags/${tag}^{commit}")" == "$(git rev-parse HEAD)" ]] || {
    echo "ERROR: refusing to move existing tag $tag" >&2
    exit 1
  }
else
  git tag "$tag" HEAD
fi
