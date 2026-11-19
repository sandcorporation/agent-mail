#!/usr/bin/env bash
# Terminal hosting end to end: runs TestE2E_* (internal/server/e2e_test.go)
# against the built binary and this OS's terminal backend (tmux on Linux, the
# ConPTY keeper on Windows), and fails when fewer steps passed than expected,
# so a rename or a skip cannot silently drop them.
#   bash scripts/ci/e2e.sh
set -euo pipefail
pattern='^TestE2E_'
want=12
out="$(SWITCHBOARD_TEST_E2E=require SWITCHBOARD_TEST_TMUX=require go test -count=1 -v -timeout 15m -run "$pattern" ./internal/server/ 2>&1)" || { echo "$out"; exit 1; }
echo "$out" | grep -E '^(=== RUN|--- |    --- |ok|FAIL|PASS)' || true
ran=$(grep -c '^    --- PASS: TestE2E_' <<<"$out" || true)
if [ "$ran" -lt "$want" ]; then
  echo "end to end: $ran steps passed under $pattern, want at least $want"
  exit 1
fi
# The install document's walk-through for this OS (docs/hosting-pc.md).
case "$(go env GOOS)" in
  linux) doc=TestE2E_HostingDocLinux ;;
  windows) doc=TestE2E_HostingDocWindows ;;
  *) doc= ;;
esac
for t in ${doc:+$doc TestE2E_HostingDocPortCheckRefusesABoundPort TestE2E_UpOnABoundPortExitsAtOnce}; do
  if ! grep -q "^--- PASS: $t " <<<"$out"; then
    echo "end to end: $t did not pass"
    exit 1
  fi
done
echo "end to end: $ran steps passed${doc:+, $doc passed}"
