#!/usr/bin/env bash
set -euo pipefail

requested=${1:?Usage: upload_custom_release.sh VERSION FILE_OR_DIRECTORY...}
shift
version=$(bash "$(dirname "$0")/custom_version.sh" --release "$requested")
tag=v$version

# Always choose this fork explicitly, including when gh sees an upstream remote.
repository=${GITHUB_REPOSITORY:-}
if [[ -z "$repository" ]]; then
  origin=$(git remote get-url origin)
  case "$origin" in
    git@github.com:*) repository=${origin#git@github.com:} ;;
    https://github.com/*) repository=${origin#https://github.com/} ;;
    ssh://git@github.com/*) repository=${origin#ssh://git@github.com/} ;;
    *) echo "ERROR: cannot resolve origin to a GitHub repository" >&2; exit 1 ;;
  esac
  repository=${repository%.git}
fi
export GH_REPO=$repository

files=()
for path in "$@"; do
  if [[ -d "$path" ]]; then
    for file in "$path"/*; do
      [[ ! -f "$file" ]] || files+=("$file")
    done
  elif [[ -f "$path" ]]; then
    files+=("$path")
  else
    echo "ERROR: release artifact does not exist: $path" >&2
    exit 1
  fi
done
(( ${#files[@]} )) || { echo "ERROR: no release artifacts" >&2; exit 1; }

if draft=$(gh release view "$tag" --json isDraft --jq .isDraft 2>/dev/null); then
  [[ "$draft" == true ]] || { echo "ERROR: refusing to replace a published release" >&2; exit 1; }
else
  prerelease=false
  [[ "$version" != *-alpha.* && "$version" != *-beta.* && "$version" != *-rc.* ]] || prerelease=true
  gh release create "$tag" --verify-tag --draft --latest=false --prerelease="$prerelease" \
    --title "sing-box $version" --notes "Custom build from commit $(git rev-parse HEAD). See AGENTS.md for branch and tag policy."
fi
gh release upload "$tag" --clobber -- "${files[@]}"
