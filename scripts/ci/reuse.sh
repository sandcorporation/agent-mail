#!/usr/bin/env bash
# Decide whether a test run can be reused instead of running the suite again on the same tree (#339).
#
#   scripts/ci/reuse.sh check <sha>     promote.yml: exit 0 ("재사용: run R …") when a same-repository pull
#                                       request whose merge produced <sha> has a successful CI run (ci.yml,
#                                       pull_request) on a head with the same git tree as <sha>, in which the
#                                       test and windows jobs and their test steps succeeded. Exit 1 (full run)
#                                       otherwise: different tree, fork, failure, skipped tests, not found.
#   scripts/ci/reuse.sh release <sha>   release.yml: exit 0 when <sha> has a successful promote run whose
#                                       promote job succeeded (its tests ran or were reused there). Exit 1 otherwise.
#
# The decision uses only values the GitHub API returns (head_sha, conclusion, event, repository, job and step
# conclusions) and git trees (git rev-parse <sha>^{tree}); never an artifact or text a pull request run wrote,
# since a pull request run uses the pull request's own workflow files.
# Any error (API, git, unexpected JSON) means a full run.
#
# Environment: GITHUB_REPOSITORY (owner/name), GH_TOKEN for gh, REUSE_GH (default gh; tests replace it),
# REUSE_REMOTE (default origin).
# Tests: scripts/ci/reuse_test.sh
set -uo pipefail

mode="${1:-}"
sha="${2:-}"
repo="${GITHUB_REPOSITORY:-}"
gh="${REUSE_GH:-gh}"
remote="${REUSE_REMOTE:-origin}"

usage() { echo "usage: $0 check|release <40-character commit sha>" >&2; exit 2; }
case "$mode" in check | release) ;; *) usage ;; esac
[[ "$sha" =~ ^[0-9a-f]{40}$ ]] || { echo "sha must be 40 lowercase hex characters: '$sha'" >&2; exit 2; }
[ -n "$repo" ] || { echo "GITHUB_REPOSITORY is unset" >&2; exit 2; }

# The test steps whose success a reused run must show (ci.yml step names).
test_step="Go tests (all packages)"
windows_step="Server and domain tests"

full() { echo "재사용 없음: $* — 시험을 전부 돌린다"; exit 1; }
api() { "$gh" api "$1"; }

tree_of() {
  git cat-file -e "$1^{commit}" 2>/dev/null || git fetch -q --no-tags --depth=1 "$remote" "$1" 2>/dev/null || return 1
  git rev-parse --verify -q "$1^{tree}"
}

# job_ok <jobs json> <job name> <step name>: the job and its test step succeeded.
job_ok() {
  jq -e --arg job "$2" --arg step "$3" '
    [.jobs[] | select(.name == $job)] as $j
    | ($j | length) == 1 and $j[0].conclusion == "success"
      and ([$j[0].steps[] | select(.name == $step and .conclusion == "success")] | length) == 1' <<<"$1" >/dev/null
}

check() {
  local pulls tree
  tree="$(tree_of "$sha")" || full "commit $sha not found"
  pulls="$(api "repos/$repo/commits/$sha/pulls")" || full "could not list the pull requests of $sha"
  local n=0 line
  while IFS=$'\t' read -r number head head_repo; do
    [ -n "$number" ] || continue
    n=$((n + 1))
    if [ "$head_repo" != "$repo" ]; then
      echo "PR #$number: head is in $head_repo (fork) — not reused"
      continue
    fi
    local head_tree
    if ! head_tree="$(tree_of "$head")"; then
      echo "PR #$number: head $head not found"
      continue
    fi
    if [ "$head_tree" != "$tree" ]; then
      echo "PR #$number: head $head tree $head_tree differs from $sha tree $tree"
      continue
    fi
    local runs run
    runs="$(api "repos/$repo/actions/runs?head_sha=$head&event=pull_request&per_page=100")" || { echo "PR #$number: could not list runs"; continue; }
    run="$(jq -r --arg head "$head" --arg repo "$repo" '
      [.workflow_runs[] | select(.path == ".github/workflows/ci.yml" and .event == "pull_request"
        and .head_sha == $head and .head_repository.full_name == $repo
        and .status == "completed" and .conclusion == "success")]
      | sort_by(.id) | last | .id // empty' <<<"$runs")" || run=""
    if [ -z "$run" ]; then
      echo "PR #$number: no successful CI run on head $head"
      continue
    fi
    local jobs
    jobs="$(api "repos/$repo/actions/runs/$run/jobs?per_page=100")" || { echo "PR #$number: could not list the jobs of run $run"; continue; }
    if ! job_ok "$jobs" test "$test_step" || ! job_ok "$jobs" windows "$windows_step"; then
      echo "PR #$number: run $run did not pass test ($test_step) and windows ($windows_step) — tests skipped or failed"
      continue
    fi
    echo "재사용: run $run (PR #$number, head $head, tree $tree)"
    exit 0
  done < <(jq -r '.[] | [.number, .head.sha, .head.repo.full_name // ""] | @tsv' <<<"$pulls" 2>/dev/null)
  [ "$n" -gt 0 ] || full "no pull request for $sha"
  full "no reusable run for $sha"
}

release() {
  local runs run jobs
  runs="$(api "repos/$repo/actions/runs?head_sha=$sha&per_page=100")" || full "could not list the runs of $sha"
  for run in $(jq -r --arg sha "$sha" '
      [.workflow_runs[] | select(.path == ".github/workflows/promote.yml" and .head_sha == $sha
        and (.event == "push" or .event == "workflow_dispatch") and .head_branch == "main"
        and .status == "completed" and .conclusion == "success")]
      | sort_by(.id) | reverse | .[].id' <<<"$runs" 2>/dev/null); do
    jobs="$(api "repos/$repo/actions/runs/$run/jobs?per_page=100")" || continue
    if jq -e '[.jobs[] | select(.name == "promote" and .conclusion == "success")] | length == 1' <<<"$jobs" >/dev/null; then
      echo "재사용: promote run $run passed $sha — release tests skipped"
      exit 0
    fi
  done
  full "no successful promote run for $sha"
}

"$mode"
