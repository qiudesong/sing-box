#!/usr/bin/env bash
set -euo pipefail

# --release requires an existing immutable custom tag on main.
release=false
if [[ "${1:-}" == --release ]]; then
  release=true
  shift
fi
requested=${1:-}
if [[ -z "$requested" && "${GITHUB_REF:-}" == refs/tags/* ]]; then
  requested=${GITHUB_REF#refs/tags/}
fi
if [[ -z "$requested" ]]; then
  requested=$(git tag --points-at HEAD --list 'v*-custom' --sort=-version:refname | head -n 1)
fi

if [[ -n "$requested" ]]; then
  tag=v${requested#v}
  if [[ ! "$tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-(alpha|beta|rc)\.[0-9]+)?(-[1-9][0-9]*)?-custom$ ]] &&
     { "$release" || [[ ! "$tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-(alpha|beta|rc)\.[0-9]+)?-dev\.[0-9]+\.g[0-9a-f]+-custom$ ]]; }; then
    echo "ERROR: expected vX.Y.Z[-alpha.N|-beta.N|-rc.N][-REVISION]-custom" >&2
    exit 1
  fi
  if [[ "$(git rev-parse "refs/tags/${tag}^{commit}")" != "$(git rev-parse HEAD)" ]]; then
    echo "ERROR: $tag must already exist and point at the checked-out commit" >&2
    exit 1
  fi
  if "$release"; then
    git merge-base --is-ancestor HEAD refs/remotes/origin/main || {
      echo "ERROR: custom releases must come from origin/main" >&2
      exit 1
    }
  fi
  printf '%s\n' "${tag#v}"
elif "$release"; then
  echo "ERROR: create and push a -custom tag before publishing" >&2
  exit 1
else
  base=$(git describe --tags --abbrev=0 --match 'v[0-9]*' --exclude '*-custom')
  count=$(git rev-list --count "${base}..HEAD")
  printf '%s-dev.%s.g%s-custom\n' "${base#v}" "$count" "$(git rev-parse --short=12 HEAD)"
fi
