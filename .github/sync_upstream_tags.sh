#!/usr/bin/env bash
set -euo pipefail

upstream=${1:-https://github.com/SagerNet/sing-box.git}
git fetch --no-tags "$upstream" 'refs/tags/*:refs/upstream-tags/*'
pending=()
while IFS= read -r ref; do
  tag=${ref#refs/upstream-tags/}
  [[ "$tag" != *-custom ]] || continue
  if git show-ref --verify --quiet "refs/tags/$tag"; then
    [[ "$(git rev-parse "$ref")" == "$(git rev-parse "refs/tags/$tag")" ]] || {
      echo "ERROR: upstream tag $tag differs; refusing to overwrite it" >&2
      exit 1
    }
  else
    pending+=("$ref:refs/tags/$tag")
  fi
done < <(git for-each-ref --format='%(refname)' refs/upstream-tags/)
if (( ${#pending[@]} )); then
  git push --atomic origin "${pending[@]}"
fi
