#!/usr/bin/env bash
set -euo pipefail

version=${1:?Usage: apk_version.sh VERSION}
# APK supports known suffixes only; retain the full custom version in filenames.
if [[ "$version" =~ ^([0-9]+\.[0-9]+\.[0-9]+)(-(alpha|beta|rc)\.([0-9]+))?(-([1-9][0-9]*))?(-dev\.([0-9]+)\.g[0-9a-f]+)?-custom$ ]]; then
  apk_version=${BASH_REMATCH[1]}
  [[ -z "${BASH_REMATCH[3]}" ]] || apk_version+="_${BASH_REMATCH[3]}${BASH_REMATCH[4]}"
  [[ -z "${BASH_REMATCH[8]}" ]] || apk_version+="_git${BASH_REMATCH[8]}"
  apk_version+="_p${BASH_REMATCH[6]:-1}"
  printf '%s-r0\n' "$apk_version"
else
  echo "ERROR: unsupported custom APK version: $version" >&2
  exit 1
fi
