#!/usr/bin/env bash
# Answered rule golden: runs TestAnsweredRuleGolden (internal/mail/
# answered_golden_test.go), which pins what counts as an answer to a directive
# to mail.AnsweredRuleVersion, and fails when the selection ran no test, so a
# rename cannot silently skip it.
#   bash scripts/ci/answered_golden.sh
set -euo pipefail
pattern='^TestAnsweredRuleGolden$'
out="$(go test -count=1 -v -run "$pattern" ./internal/mail/ 2>&1)" || { echo "$out"; exit 1; }
echo "$out" | grep -E '^(=== RUN|--- |ok|FAIL|PASS)' || true
if ! grep -q '^--- PASS: TestAnsweredRuleGolden ' <<<"$out"; then
  echo "answered rule golden: no test matching $pattern ran"
  exit 1
fi
echo "answered rule golden: passed"
