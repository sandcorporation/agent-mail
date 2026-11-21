#!/usr/bin/env bash
# Tests for scripts/ci/docs_only.sh. No network.
#   bash scripts/ci/docs_only_test.sh
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
docs="$here/docs_only.sh"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

pass=0 fail=0
want() { # name, expected exit code, expected output text (or ""), command...
  local name="$1" want_rc="$2" want_out="$3"; shift 3
  local out rc=0
  out="$("$@" 2>&1)" || rc=$?
  if [ "$rc" = "$want_rc" ] && { [ -z "$want_out" ] || grep -qF -- "$want_out" <<<"$out"; }; then
    pass=$((pass + 1))
  else
    fail=$((fail + 1))
    echo "FAIL: $name (rc=$rc want $want_rc, want text: $want_out)"; printf '  | %s\n' "$out"
  fi
}
classify() { printf '%s' "$1" | bash "$docs" classify; }

want 'readme' 0 '' classify $'README.md\n'
want 'docs and nested md' 0 '' classify $'docs/hosting-pc.md\ndocs/img/a.png\ninternal/x/NOTES.md\n'
want 'crlf' 0 '' classify $'README.md\r\n'
want 'go file' 1 '' classify $'README.md\ninternal/server/server.go\n'
want 'workflow' 1 '' classify $'.github/workflows/ci.yml\n'
want 'md-like name' 1 '' classify $'README.md.go\n'
want 'docs prefix only' 1 '' classify $'docsx/a.go\n'
want 'nothing changed' 1 '' classify ''

export GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@example.com GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@example.com
export GIT_CONFIG_GLOBAL=/dev/null
git init -q --bare -b main "$tmp/origin.git"
git -C "$tmp/origin.git" config uploadpack.allowAnySHA1InWant true
git clone -q "$tmp/origin.git" "$tmp/w" 2>/dev/null
cd "$tmp/w"
echo a >a.go && git add a.go && git commit -q -m base && BASE=$(git rev-parse HEAD)
echo d >README.md && git add README.md && git commit -q -m docs && DOCS=$(git rev-parse HEAD)
echo b >>a.go && git add a.go && git commit -q -m code && CODE=$(git rev-parse HEAD)
git push -q origin "$CODE:refs/heads/main" "$DOCS:refs/pull/1/merge"
cd "$tmp" && git clone -q --no-local --depth=1 "file://$tmp/origin.git" c 2>/dev/null && cd c
scope() { # head, event
  git checkout -q --detach "$1" 2>/dev/null || { git fetch -q --depth=1 origin "$1" && git checkout -q --detach "$1"; }
  : >"$tmp/out"
  GITHUB_OUTPUT="$tmp/out" GITHUB_EVENT_NAME="$2" BASE_SHA="$BASE" bash "$docs" scope >/dev/null 2>&1
  cat "$tmp/out"
}
want 'scope: docs-only pull request' 0 'docs_only=true' scope "$DOCS" pull_request
want 'scope: code pull request' 0 'docs_only=false' scope "$CODE" pull_request
want 'scope: push is never docs-only' 0 'docs_only=false' scope "$DOCS" push
want 'scope: unknown base runs everything' 0 'docs_only=false' env BASE_SHA_OVERRIDE=1 bash -c ': >"'"$tmp"'/out"; git checkout -q --detach '"$DOCS"'; GITHUB_OUTPUT="'"$tmp"'/out" GITHUB_EVENT_NAME=pull_request BASE_SHA='"$(printf 'f%.0s' {1..40})"' bash "'"$docs"'" scope >/dev/null 2>&1; cat "'"$tmp"'/out"'

echo "docs_only.sh tests: $pass passed, $fail failed"
[ "$fail" = 0 ]
