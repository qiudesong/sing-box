#!/usr/bin/env bash
set -euo pipefail

# Run only in an ephemeral Actions checkout. Never reset a shared branch or
# discard commits made by a human on the maintenance branch.
: "${GITHUB_REPOSITORY:?}"
: "${GH_TOKEN:?A dedicated UPSTREAM_PR_TOKEN is required to trigger PR CI}"
branch=feature/upstream-stable
git fetch origin main stable
if git merge-base --is-ancestor origin/stable origin/main; then
  exit 0
fi
git config user.name 'github-actions[bot]'
git config user.email '41898282+github-actions[bot]@users.noreply.github.com'

if git ls-remote --exit-code --heads origin "$branch" >/dev/null 2>&1; then
  git fetch origin "$branch"
  git switch --create "$branch" "origin/$branch"
  # Both merges preserve earlier manual conflict resolutions. On conflict keep
  # the published branch intact and report the new target SHA in the PR body.
  conflict=false
  for source in origin/main origin/stable; do
    if ! git merge --no-edit "$source"; then
      git merge --abort
      conflict=true
      break
    fi
  done
  if [[ "$conflict" == false ]]; then
    git push origin "HEAD:refs/heads/$branch"
  fi
else
  # Starting at stable gives reviewers a concrete PR even if its merge with
  # main conflicts. Resolve on this feature branch, never on the mirror.
  git switch --create "$branch" origin/stable
  git push origin "HEAD:refs/heads/$branch"
  conflict=false
fi

body=$(mktemp)
trap 'rm -f "$body"' EXIT
{
  printf 'Merge upstream stable into main while retaining custom behavior and release policy.\n\n'
  printf 'Upstream stable commit: `%s`.\n\n' "$(git rev-parse origin/stable)"
  printf 'Required checks: normal cross-platform CI, BGP configuration/DNS/outbound contracts, Linux network integration, and custom release safeguards.\n\n'
  printf 'Manual review and merge required. Resolve conflicts on `%s`; do not modify the stable/testing mirrors or force-push main.\n' "$branch"
  if [[ "$conflict" == true ]]; then
    printf '\nAutomatic update encountered a merge conflict; the published feature branch was preserved. Merge the current main and stable commits into it manually.\n'
  fi
} > "$body"
pr=$(gh pr list --repo "$GITHUB_REPOSITORY" --base main --head "$branch" --state open --json number --jq '.[0].number // empty')
if [[ -n "$pr" ]]; then
  gh pr edit "$pr" --repo "$GITHUB_REPOSITORY" --body-file "$body"
else
  gh pr create --repo "$GITHUB_REPOSITORY" --base main --head "$branch" --title 'Merge upstream stable into main' --body-file "$body"
fi
