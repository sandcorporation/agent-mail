#!/usr/bin/env bash
# Docs-only pull requests skip the test steps of ci.yml's test and windows jobs (#339); the jobs keep their
# names (required checks) and end green.
#
#   scripts/ci/docs_only.sh classify   reads changed paths (one per line) on stdin; exit 0 when every path is
#                                      documentation (*.md anywhere, or under docs/), 1 otherwise (also for none).
#   scripts/ci/docs_only.sh scope      in a ci.yml job: writes docs_only=true|false to $GITHUB_OUTPUT. Only a
#                                      pull_request run can be docs-only; it compares the checked-out merge commit
#                                      with the base commit (BASE_SHA). Any error means false (run everything).
# Tests: scripts/ci/docs_only_test.sh
set -uo pipefail

classify() {
  local n=0 f
  while IFS= read -r f; do
    f="${f%$'\r'}"
    [ -n "$f" ] || continue
    n=$((n + 1))
    case "$f" in
      docs/* | *.md) ;;
      *) return 1 ;;
    esac
  done
  [ "$n" -gt 0 ]
}

scope() {
  local out="${GITHUB_OUTPUT:-/dev/stdout}" docs=false files
  if [ "${GITHUB_EVENT_NAME:-}" = pull_request ] && [[ "${BASE_SHA:-}" =~ ^[0-9a-f]{40}$ ]]; then
    if git fetch -q --no-tags --depth=1 origin "$BASE_SHA" && files="$(git diff --name-only "$BASE_SHA" HEAD)"; then
      if classify <<<"$files"; then docs=true; fi
      echo "changed files: $(wc -l <<<"$files" | tr -d ' ')"
    else
      echo "could not compare with the base commit; running everything"
    fi
  fi
  if [ "$docs" = true ]; then
    echo "docs-only pull request: the test steps are skipped"
  else
    echo "not docs-only: running everything"
  fi
  echo "docs_only=$docs" >>"$out"
}

case "${1:-}" in
  classify) classify ;;
  scope) scope ;;
  *) echo "usage: $0 classify|scope" >&2; exit 2 ;;
esac
