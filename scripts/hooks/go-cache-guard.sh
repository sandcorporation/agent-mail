#!/usr/bin/env bash
# Claude Code PreToolUse hook (Bash). Reads the hook JSON on stdin and refuses commands
# that run a golang image by hand or point Go caches under /tmp. Use scripts/go instead.
input=$(cat)
if printf '%s' "$input" | grep -Eq 'docker.*[ ]run[ ].*golang:|GO(MOD)?CACHE=["'"'"']?/tmp'; then
  echo "Go caches must not live under /tmp (RAM). Run Go through scripts/go (cache in ~/.cache/switchboard-go) instead of docker run golang: or GOCACHE=/tmp." >&2
  exit 2
fi
exit 0
