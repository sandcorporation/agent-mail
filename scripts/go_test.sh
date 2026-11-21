#!/usr/bin/env bash
# Tests for scripts/go (cache-dir guard) and scripts/hooks/go-cache-guard.sh. No docker needed.
set -u
cd "$(dirname "${BASH_SOURCE[0]}")/.."
fail=0
check() { # name expected actual
  if [ "$2" = "$3" ]; then echo "ok   $1"; else echo "FAIL $1: want $2 got $3"; fail=1; fi
}

SWITCHBOARD_GO_CACHE=/tmp/x scripts/go version >/dev/null 2>&1; check "go: /tmp cache refused" 2 $?
SWITCHBOARD_GO_CACHE=/tmp scripts/go version >/dev/null 2>&1; check "go: /tmp itself refused" 2 $?
msg=$(SWITCHBOARD_GO_CACHE=/tmp/x scripts/go version 2>&1 >/dev/null); check "go: one stderr line" 1 "$(printf '%s\n' "$msg" | wc -l)"

if [ -d /dev/shm ] && [ "$(stat -f -c %T /dev/shm)" = tmpfs ]; then
  SWITCHBOARD_GO_CACHE=/dev/shm/swgo-test scripts/go version >/dev/null 2>&1; check "go: tmpfs cache refused" 2 $?
else
  echo "skip go: tmpfs cache refused (no tmpfs at /dev/shm)"
fi

check "go: explicit cache dir" /var/tmp-not/x "$(SWITCHBOARD_GO_CACHE=/var/tmp-not/x scripts/go --cache-dir)"
check "go: XDG default" /xdg/switchboard-go "$(env -u SWITCHBOARD_GO_CACHE XDG_CACHE_HOME=/xdg scripts/go --cache-dir)"
check "go: HOME default" /h/.cache/switchboard-go "$(env -u SWITCHBOARD_GO_CACHE -u XDG_CACHE_HOME HOME=/h scripts/go --cache-dir)"

hook() { printf '{"tool_name":"Bash","tool_input":{"command":"%s"}}' "$1" | bash scripts/hooks/go-cache-guard.sh 2>/dev/null; }
hook 'docker run --rm -v $PWD:/src -w /src golang:1.25 go test ./...'; check "hook: docker run golang blocked" 2 $?
hook 'GOCACHE=/tmp/c go build ./...'; check "hook: GOCACHE=/tmp blocked" 2 $?
hook 'export GOMODCACHE=/tmp/claude-1000/m && go test'; check "hook: GOMODCACHE=/tmp blocked" 2 $?
hook 'scripts/go test ./...'; check "hook: scripts/go allowed" 0 $?
hook 'GOCACHE=$HOME/.cache/x go test ./...'; check "hook: disk cache allowed" 0 $?
hook 'docker run --rm alpine true'; check "hook: other docker allowed" 0 $?

exit $fail
