#!/usr/bin/env bash
# Question source of truth: runs the server's end-to-end question tests
# (internal/server/questions_sot_test.go) and fails when the selection runs
# fewer tests than expected, so a rename cannot silently skip them.
#   bash scripts/ci/question_sot.sh
set -euo pipefail
pattern='^TestQuestionSingleSourceOfTruth_'
want=7
out="$(go test -count=1 -v -run "$pattern" ./internal/server/ 2>&1)" || { echo "$out"; exit 1; }
echo "$out" | grep -E '^(=== RUN|--- |    --- |ok|FAIL|PASS)' || true
ran=$(grep -c '^--- PASS: TestQuestionSingleSourceOfTruth_' <<<"$out" || true)
if [ "$ran" -lt "$want" ]; then
  echo "question source of truth: ran $ran tests matching $pattern, want at least $want"
  exit 1
fi
echo "question source of truth: $ran tests passed"
