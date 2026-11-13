#!/usr/bin/env bash
# Tests for scripts/ci/promote.sh using a local bare repository as origin. No network or tokens.
#   bash scripts/ci/promote_test.sh
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
promote="$here/promote.sh"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

export GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@example.com GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@example.com
export GIT_CONFIG_GLOBAL=/dev/null
git init -q --bare -b main "$tmp/origin.git"
git clone -q "$tmp/origin.git" "$tmp/work" 2>/dev/null
cd "$tmp/work"
commit() { git commit -q --allow-empty -m "$1"; git rev-parse HEAD; }
A=$(commit a); B=$(commit b); C=$(commit c)
git push -q origin HEAD:main
git checkout -q --detach "$A"
X=$(commit x) # commit outside main
git checkout -q main

pass=0 fail=0
head_of() { git ls-remote --heads origin "refs/heads/production" | awk '{print $1}'; }
check() { # name, expected exit code, expected production head, expected output text, command...
  local name="$1" want_rc="$2" want_head="$3" want_out="$4"; shift 4
  local out rc=0
  out="$("$@" 2>&1)" || rc=$?
  if [ "$rc" = "$want_rc" ] && [ "$(head_of)" = "$want_head" ] && grep -qF -- "$want_out" <<<"$out"; then
    pass=$((pass + 1))
  else
    fail=$((fail + 1))
    echo "FAIL: $name (rc=$rc want $want_rc, head=$(head_of) want $want_head)"; printf '  | %s\n' "$out"
  fi
}

check 'creates the branch' 0 "$A" 'creating it' bash "$promote" ff "$A"
check 'same sha is a no-op' 0 "$A" 'nothing to do' bash "$promote" ff "$A"
check 'fast-forward' 0 "$C" 'fast-forward' bash "$promote" ff "$C"
check 'late older run is skipped' 0 "$C" 'skipped' bash "$promote" ff "$B"
check 'diverged sha fails' 1 "$C" 'diverged' bash "$promote" ff "$X"
check 'sha shape is checked' 2 "$C" '40 lowercase' bash "$promote" ff "${C:0:12}"
check 'unknown mode' 2 "$C" 'usage' bash "$promote" rollback "$A"
echo "promote.sh tests: $pass passed, $fail failed"
[ "$fail" = 0 ]
