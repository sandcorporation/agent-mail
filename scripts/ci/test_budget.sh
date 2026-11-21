#!/usr/bin/env bash
# Time budget for single tests (#339). Reads a `go test -json` stream from a file, prints a short log (package
# results and the output of failed tests), and fails when go test failed or when a top-level test took longer
# than the budget. Subtests count toward their top-level test.
#
#   go test -json -timeout 10m ./... >"$RUNNER_TEMP/go-test.json"; rc=$?
#   bash scripts/ci/test_budget.sh <seconds> <go-test.json> [allow-list] || rc=1
#
# Tests already over the budget are listed in the allow list (default scripts/ci/test_budget_allow.txt), one
# "<package> <TestName>" per line, each tagged with the issue that brings it under budget. A listed test that is
# back under budget is reported so its line can go.
# Tests: scripts/ci/test_budget_test.sh
set -uo pipefail

limit="${1:-}"
file="${2:-}"
allow="${3:-$(dirname "${BASH_SOURCE[0]}")/test_budget_allow.txt}"
[[ "$limit" =~ ^[0-9]+$ ]] && [ -f "$file" ] || { echo "usage: $0 <seconds> <go-test.json> [allow-list]" >&2; exit 2; }

# Lines that are not JSON (build errors, a panic in TestMain) are printed as they are.
events="$(jq -cR 'fromjson? // {Action: "output", Output: (. + "\n")}' "$file")" || { echo "test_budget: cannot read $file" >&2; exit 2; }

# The log: package-level output and the output of failed tests, in order.
jq -rs '
  ([.[] | select(.Action == "fail" and .Test != null) | "\(.Package) \(.Test)"] | unique) as $failed
  | .[] | select(.Action == "output")
  | select(.Test == null or (("\(.Package) \(.Test)") as $k | any($failed[]; . == $k or startswith($k + "/"))))
  | .Output' <<<"$events" | tr -d '\r' | grep -v '^=== \(RUN\|PAUSE\|CONT\)' || true

rc=0
if jq -se 'any(.[]; .Action == "fail")' <<<"$events" >/dev/null; then
  echo "test_budget: go test reported failures (above)"
  rc=1
fi
if ! jq -se 'any(.[]; .Action == "pass" or .Action == "skip")' <<<"$events" >/dev/null; then
  echo "test_budget: no test results in $file"
  rc=1
fi

allowed="$(sed -e 's/#.*//' "$allow" 2>/dev/null | awk 'NF >= 2 {print $1 " " $2}')"
slow="$(jq -rs --argjson limit "$limit" '
  [.[] | select((.Action == "pass" or .Action == "fail") and .Test != null and (.Test | contains("/") | not)
    and (.Elapsed // 0) > $limit) | "\(.Package) \(.Test) \(.Elapsed)"] | .[]' <<<"$events")"
while read -r pkg test elapsed; do
  [ -n "$pkg" ] || continue
  if grep -qxF -- "$pkg $test" <<<"$allowed"; then
    echo "test_budget: allowed $pkg $test ${elapsed}s (over ${limit}s, see the allow list)"
  else
    echo "test_budget: FAIL $pkg $test took ${elapsed}s, over the ${limit}s budget"
    rc=1
  fi
done <<<"$slow"
while read -r pkg test; do
  [ -n "$pkg" ] || continue
  grep -q "^$pkg $test " <<<"$slow" && continue
  if jq -se --arg p "$pkg" --arg t "$test" 'any(.[]; .Action == "pass" and .Package == $p and .Test == $t)' <<<"$events" >/dev/null; then
    echo "test_budget: $pkg $test is now under ${limit}s; remove it from the allow list"
  fi
done <<<"$allowed"
[ "$rc" = 0 ] && echo "test_budget: no test over ${limit}s outside the allow list"
exit "$rc"
