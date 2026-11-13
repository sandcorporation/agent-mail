#!/usr/bin/env bash
# Move the production branch. Called by GitHub Actions (promote.yml) only — people do not push production.
#
#   scripts/ci/promote.sh ff <sha>   fast-forward production to a main sha whose checks and image passed.
#                                    If production is already newer (an older run finished late), skip — not a failure.
# production only moves forward (protect it against force-push and deletion). Roll back with a revert PR on main.
#
# Environment: PROMOTE_BRANCH (default production), MAIN_BRANCH (default main), PROMOTE_REMOTE (default origin).
# Push credentials come from actions/checkout (GITHUB_TOKEN, contents: write only in the promote job); this script never reads a token.
# Tests: scripts/ci/promote_test.sh
set -euo pipefail

mode="${1:-}"
sha="${2:-}"
branch="${PROMOTE_BRANCH:-production}"
main="${MAIN_BRANCH:-main}"
remote="${PROMOTE_REMOTE:-origin}"

usage() { echo "usage: $0 ff <40-character commit sha>" >&2; exit 2; }
case "$mode" in ff) ;; *) usage ;; esac
[[ "$sha" =~ ^[0-9a-f]{40}$ ]] || { echo "sha must be 40 lowercase hex characters: '$sha'" >&2; exit 2; }

remote_head() {
  git ls-remote --heads "$remote" "refs/heads/$1" | awk '{print $1}'
}

fetch_commits() {
  git fetch -q --no-tags "$remote" "+refs/heads/$main:refs/remotes/$remote/$main"
  if [ -n "$(remote_head "$branch")" ]; then
    git fetch -q --no-tags "$remote" "+refs/heads/$branch:refs/remotes/$remote/$branch"
  fi
}

promote_ff() {
  fetch_commits
  local cur
  cur="$(remote_head "$branch")"
  if [ -z "$cur" ]; then
    echo "== $branch does not exist — creating it at $sha"
    git push -q "$remote" "$sha:refs/heads/$branch"
    return
  fi
  if [ "$cur" = "$sha" ]; then
    echo "== $branch is already $sha — nothing to do"
    return
  fi
  if git merge-base --is-ancestor "$cur" "$sha"; then
    echo "== $branch: $cur -> $sha (fast-forward)"
    # Not forced: if someone moved the branch meanwhile the server rejects the push and we look again.
    if git push -q "$remote" "$sha:refs/heads/$branch"; then
      return
    fi
    local now
    now="$(remote_head "$branch")"
    git fetch -q --no-tags "$remote" "+refs/heads/$branch:refs/remotes/$remote/$branch"
    if git merge-base --is-ancestor "$sha" "$now"; then
      echo "== skipped: $branch was promoted to a newer commit ($now) during the push"
      return
    fi
    echo "$branch push rejected — current head $now does not contain $sha" >&2
    exit 1
  fi
  if git merge-base --is-ancestor "$sha" "$cur"; then
    echo "== skipped: $branch is already at a newer commit ($cur); this run finished late"
    return
  fi
  echo "$branch ($cur) and $sha have diverged — cannot fast-forward. Check $branch for commits outside $main" >&2
  exit 1
}

"promote_$mode"
