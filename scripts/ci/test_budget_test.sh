#!/usr/bin/env bash
# Tests for scripts/ci/test_budget.sh with canned `go test -json` streams.
#   bash scripts/ci/test_budget_test.sh
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
budget="$here/test_budget.sh"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

ev() { # action package [test elapsed output]
  jq -cn --arg a "$1" --arg p "$2" --arg t "${3:-}" --arg e "${4:-0}" --arg o "${5:-}" '
    {Action: $a, Package: $p} + (if $t != "" then {Test: $t} else {} end)
    + (if $a == "output" then {Output: $o} else {Elapsed: ($e | tonumber)} end)'
}
P=example.com/x/pkg
fast() { ev pass $P TestFast 0.5; ev output $P "" 0 $'ok  \texample.com/x/pkg\t0.6s\n'; ev pass $P "" 0.6; }
printf '%s\n' "$(fast)" >"$tmp/fast.json"
{ ev output $P TestSlow 0 $'=== RUN   TestSlow\n'; ev pass $P TestSlow/sub 31; ev pass $P TestSlow 31.5; fast; } >"$tmp/slow.json"
{ ev output $P TestBad 0 $'    bad_test.go:9: want 1 got 2\n'; ev fail $P TestBad 0.1
  ev output $P TestQuiet 0 $'quiet output\n'; ev pass $P TestQuiet 0.1
  ev output $P "" 0 $'FAIL\texample.com/x/pkg\t0.3s\n'; ev fail $P "" 0.3; } >"$tmp/fail.json"
{ echo '# example.com/x/pkg'; echo 'pkg/x.go:3:1: syntax error'; ev fail $P "" 0; } >"$tmp/build.json"
: >"$tmp/empty.json"
echo "$P TestSlow # #340" >"$tmp/allow.txt"
echo "$P TestFast # #340" >"$tmp/allow-fast.txt"
: >"$tmp/none.txt"

pass=0 fail=0
want() { # name, expected exit code, expected text, [unexpected text], command...
  local name="$1" want_rc="$2" want_out="$3" not="$4"; shift 4
  local out rc=0
  out="$("$@" 2>&1)" || rc=$?
  if [ "$rc" = "$want_rc" ] && grep -qF -- "$want_out" <<<"$out" && { [ -z "$not" ] || ! grep -qF -- "$not" <<<"$out"; }; then
    pass=$((pass + 1))
  else
    fail=$((fail + 1))
    echo "FAIL: $name (rc=$rc want $want_rc, want: $want_out, not: $not)"; printf '  | %s\n' "$out"
  fi
}
want 'fast tests pass' 0 'no test over 30s' '' bash "$budget" 30 "$tmp/fast.json" "$tmp/none.txt"
want 'package line is printed' 0 'ok  	example.com/x/pkg' '' bash "$budget" 30 "$tmp/fast.json" "$tmp/none.txt"
want 'a slow test fails' 1 "FAIL $P TestSlow took 31.5s" 'TestSlow/sub' bash "$budget" 30 "$tmp/slow.json" "$tmp/none.txt"
want 'a slow test under a higher budget passes' 0 'no test over 60s' '' bash "$budget" 60 "$tmp/slow.json" "$tmp/none.txt"
want 'an allowed slow test passes' 0 "allowed $P TestSlow 31.5s" '' bash "$budget" 30 "$tmp/slow.json" "$tmp/allow.txt"
want 'an allowed test back under budget is reported' 0 'remove it from the allow list' '' bash "$budget" 30 "$tmp/fast.json" "$tmp/allow-fast.txt"
want 'a failed test fails and shows its output' 1 'want 1 got 2' 'quiet output' bash "$budget" 30 "$tmp/fail.json" "$tmp/none.txt"
want 'a failed package line is printed' 1 'FAIL	example.com/x/pkg' '' bash "$budget" 30 "$tmp/fail.json" "$tmp/none.txt"
want 'a build error fails and is printed' 1 'syntax error' '' bash "$budget" 30 "$tmp/build.json" "$tmp/none.txt"
want 'no results fails' 1 'no test results' '' bash "$budget" 30 "$tmp/empty.json" "$tmp/none.txt"
want 'RUN lines are dropped' 1 'took 31.5s' '=== RUN' bash "$budget" 30 "$tmp/slow.json" "$tmp/none.txt"
want 'usage' 2 'usage' '' bash "$budget" 30s "$tmp/fast.json"
want 'the repository allow list is tagged' 0 '' '' bash -c "! sed -e '/^#/d' -e '/^\$/d' '$here/test_budget_allow.txt' | grep -v '#340'"

echo "test_budget.sh tests: $pass passed, $fail failed"
[ "$fail" = 0 ]
