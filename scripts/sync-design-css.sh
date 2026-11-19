#!/usr/bin/env bash
# Copies the design system stylesheet (dist/sandcorp.css of
# sandcorporation/sandcorp-design at a tag) into the dashboard's embedded
# assets and rewrites its header comment (source, tag, copy date).
#   bash scripts/sync-design-css.sh v0.8.0
# With DESIGN_REPO=<path of a local sandcorp-design checkout> the file is read
# from that checkout's tag; otherwise it is downloaded from GitHub.
set -euo pipefail
tag="${1:?usage: sync-design-css.sh <tag>}"
repo=sandcorporation/sandcorp-design
dst="$(cd "$(dirname "$0")/.." && pwd)/internal/dashboard/assets/sandcorp.css"
tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT
if [ -n "${DESIGN_REPO:-}" ]; then
  git -C "$DESIGN_REPO" show "$tag:dist/sandcorp.css" >"$tmp"
else
  curl -fsSL "https://raw.githubusercontent.com/$repo/$tag/dist/sandcorp.css" >"$tmp"
fi
{
  printf '/* Copied from github.com/%s, tag %s, file dist/sandcorp.css, on %s.\n' "$repo" "$tag" "$(date -u +%F)"
  printf '   Do not edit here. To update: bash scripts/sync-design-css.sh <tag> */\n'
  cat "$tmp"
  printf '\n'
} >"$dst"
echo "wrote $dst from $repo@$tag"
