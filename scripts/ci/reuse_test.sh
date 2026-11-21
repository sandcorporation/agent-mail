#!/usr/bin/env bash
# Tests for scripts/ci/reuse.sh: a fake gh serves canned API answers, a local bare repository is origin.
# No network or tokens.
#   bash scripts/ci/reuse_test.sh
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
reuse="$here/reuse.sh"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

export GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@example.com GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@example.com
export GIT_CONFIG_GLOBAL=/dev/null
export GITHUB_REPOSITORY=team/switchboard
git init -q --bare -b main "$tmp/origin.git"
git -C "$tmp/origin.git" config uploadpack.allowAnySHA1InWant true
git clone -q "$tmp/origin.git" "$tmp/work" 2>/dev/null
cd "$tmp/work"
echo a >a.txt && git add a.txt && git commit -q -m base && BASE=$(git rev-parse HEAD)
echo b >b.txt && git add b.txt && git commit -q -m 'squash of #7' && C=$(git rev-parse HEAD)
git push -q origin HEAD:main
# Pull request heads, only on origin (fetched by sha when needed): SAME has C's tree on another parent,
# OTHER a different tree.
SAME=$(git commit-tree "$C^{tree}" -p "$BASE" -m 'pr head')
echo c >c.txt && git add c.txt && OTHER=$(git commit-tree "$(git write-tree)" -p "$BASE" -m 'pr head 2') && git reset -q --hard "$C"
git push -q origin "$SAME:refs/pull/7/head" "$OTHER:refs/pull/8/head"
# A fresh clone over file:// (no hard-linked objects) has main only, like a CI checkout.
cd "$tmp" && git clone -q --no-local "file://$tmp/origin.git" ci 2>/dev/null && cd ci
! git cat-file -e "$SAME^{commit}" 2>/dev/null || { echo "setup: the pull request head is already local"; exit 1; }

# Fake gh: `gh api <path>` prints $FAKE/<path with / ? & = as _>.json, or fails when there is none.
cat >"$tmp/gh" <<'EOF'
#!/usr/bin/env bash
[ "$1" = api ] || exit 2
f="$FAKE/$(printf '%s' "$2" | tr '/?&=' '____').json"
[ -f "$f" ] || { echo "fake gh: no $f" >&2; exit 1; }
cat "$f"
EOF
chmod +x "$tmp/gh"
export REUSE_GH="$tmp/gh"

fixture() { # path json
  printf '%s' "$2" >"$FAKE/$(printf '%s' "$1" | tr '/?&=' '____').json"
}
fresh() { FAKE="$tmp/fake-$1"; export FAKE; mkdir -p "$FAKE"; }
pulls() { # number head head_repo
  fixture "repos/team/switchboard/commits/$C/pulls" "[{\"number\":$1,\"head\":{\"sha\":\"$2\",\"repo\":{\"full_name\":\"$3\"}}}]"
}
run_json() { # id path conclusion head
  printf '{"id":%s,"path":"%s","event":"pull_request","head_sha":"%s","head_repository":{"full_name":"team/switchboard"},"status":"completed","conclusion":"%s"}' "$1" "$2" "$4" "$3"
}
runs() { # head runs...
  local head="$1"; shift
  local IFS=,
  fixture "repos/team/switchboard/actions/runs?head_sha=$head&event=pull_request&per_page=100" "{\"workflow_runs\":[$*]}"
}
jobs() { # run test_step_conclusion windows_step_conclusion [windows_job_conclusion]
  fixture "repos/team/switchboard/actions/runs/$1/jobs?per_page=100" "{\"jobs\":[
    {\"name\":\"test\",\"conclusion\":\"success\",\"steps\":[{\"name\":\"Scope\",\"conclusion\":\"success\"},{\"name\":\"Go tests (all packages)\",\"conclusion\":\"$2\"}]},
    {\"name\":\"windows\",\"conclusion\":\"${4:-success}\",\"steps\":[{\"name\":\"Server and domain tests\",\"conclusion\":\"$3\"}]}]}"
}

pass=0 fail=0
check() { # name, expected exit code, expected output text, command...
  local name="$1" want_rc="$2" want_out="$3"; shift 3
  local out rc=0
  out="$("$@" 2>&1)" || rc=$?
  if [ "$rc" = "$want_rc" ] && grep -qF -- "$want_out" <<<"$out"; then
    pass=$((pass + 1))
  else
    fail=$((fail + 1))
    echo "FAIL: $name (rc=$rc want $want_rc, want text: $want_out)"; printf '  | %s\n' "$out"
  fi
}

fresh same
pulls 7 "$SAME" team/switchboard
runs "$SAME" "$(run_json 11 .github/workflows/ci.yml success "$SAME")" "$(run_json 12 .github/workflows/ci.yml failure "$SAME")" "$(run_json 13 .github/workflows/other.yml success "$SAME")"
jobs 11 success success
check 'same tree is reused' 0 "재사용: run 11 (PR #7, head $SAME" bash "$reuse" check "$C"

fresh newest
pulls 7 "$SAME" team/switchboard
runs "$SAME" "$(run_json 11 .github/workflows/ci.yml success "$SAME")" "$(run_json 14 .github/workflows/ci.yml success "$SAME")"
jobs 14 success success
check 'the newest successful run is reused' 0 'run 14' bash "$reuse" check "$C"

fresh other
pulls 8 "$OTHER" team/switchboard
runs "$OTHER" "$(run_json 21 .github/workflows/ci.yml success "$OTHER")"
jobs 21 success success
check 'a different tree runs everything' 1 'differs from' bash "$reuse" check "$C"

fresh fork
pulls 7 "$SAME" someone/switchboard
runs "$SAME" "$(run_json 11 .github/workflows/ci.yml success "$SAME")"
jobs 11 success success
check 'a fork runs everything' 1 '(fork)' bash "$reuse" check "$C"

fresh failed
pulls 7 "$SAME" team/switchboard
runs "$SAME" "$(run_json 12 .github/workflows/ci.yml failure "$SAME")" "$(run_json 13 .github/workflows/other.yml success "$SAME")"
check 'a failed run runs everything' 1 'no successful CI run' bash "$reuse" check "$C"

fresh windows
pulls 7 "$SAME" team/switchboard
runs "$SAME" "$(run_json 11 .github/workflows/ci.yml success "$SAME")"
jobs 11 success success failure
check 'a failed windows job runs everything' 1 'did not pass' bash "$reuse" check "$C"

fresh docs
pulls 7 "$SAME" team/switchboard
runs "$SAME" "$(run_json 11 .github/workflows/ci.yml success "$SAME")"
jobs 11 skipped skipped
check 'skipped tests (docs-only run) are not reused' 1 'did not pass' bash "$reuse" check "$C"

fresh none
pulls 7 "$SAME" team/switchboard
runs "$SAME"
check 'no run runs everything' 1 'no successful CI run' bash "$reuse" check "$C"

fresh nopr
fixture "repos/team/switchboard/commits/$C/pulls" '[]'
check 'no pull request runs everything' 1 'no pull request' bash "$reuse" check "$C"

fresh apierr
check 'an API error runs everything' 1 'could not list' bash "$reuse" check "$C"

fresh badjson
fixture "repos/team/switchboard/commits/$C/pulls" 'not json'
check 'unexpected JSON runs everything' 1 '재사용 없음' bash "$reuse" check "$C"

check 'unknown commit runs everything' 1 'not found' bash "$reuse" check "$(printf 'f%.0s' {1..40})"
check 'sha shape is checked' 2 '40 lowercase' bash "$reuse" check "${C:0:12}"
check 'unknown mode' 2 'usage' bash "$reuse" reuse "$C"

promote_runs() { # runs...
  local IFS=,
  fixture "repos/team/switchboard/actions/runs?head_sha=$C&per_page=100" "{\"workflow_runs\":[$*]}"
}
promote_run() { # id conclusion [branch]
  printf '{"id":%s,"path":".github/workflows/promote.yml","event":"push","head_branch":"%s","head_sha":"%s","status":"completed","conclusion":"%s"}' "$1" "${3:-main}" "$C" "$2"
}
promote_jobs() { # id promote_conclusion
  fixture "repos/team/switchboard/actions/runs/$1/jobs?per_page=100" "{\"jobs\":[{\"name\":\"check\",\"conclusion\":\"success\"},{\"name\":\"promote\",\"conclusion\":\"$2\"}]}"
}

fresh rel
promote_runs "$(promote_run 31 success)" "$(promote_run 30 failure)"
promote_jobs 31 success
check 'release skips tests after a successful promote' 0 'promote run 31' bash "$reuse" release "$C"

fresh relskipped
promote_runs "$(promote_run 31 success)"
promote_jobs 31 skipped
check 'release runs tests when promote skipped its job' 1 'no successful promote run' bash "$reuse" release "$C"

fresh relbranch
promote_runs "$(promote_run 31 success work)"
promote_jobs 31 success
check 'release runs tests for a promote run off main' 1 'no successful promote run' bash "$reuse" release "$C"

fresh relfailed
promote_runs "$(promote_run 30 failure)"
check 'release runs tests after a failed promote' 1 'no successful promote run' bash "$reuse" release "$C"

fresh relapi
check 'release runs tests on an API error' 1 'could not list' bash "$reuse" release "$C"

echo "reuse.sh tests: $pass passed, $fail failed"
[ "$fail" = 0 ]
