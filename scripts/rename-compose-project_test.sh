#!/usr/bin/env bash
# Local test for scripts/rename-compose-project.sh against Docker. Not run in CI (needs a Docker daemon).
# Uses only throwaway names (projects sbrt-old-<pid>/sbrt-new-<pid>, port 18097) and removes them at the end.
#   IMAGE=ghcr.io/sandcorporation/switchboard:<sha> bash scripts/rename-compose-project_test.sh
set -euo pipefail
here="$(cd "$(dirname "$0")/.." && pwd)"
script="$here/scripts/rename-compose-project.sh"
image=${IMAGE:?set IMAGE to a switchboard image}
tag=$$
old=sbrt-old-$tag
new=sbrt-new-$tag
tmp="$(mktemp -d)"
port=18097

cleanup() {
	set +e
	docker compose -p "$old" -f "$tmp/old.yaml" --env-file "$tmp/.env" down >/dev/null 2>&1
	docker compose -p "$new" -f "$tmp/new.yaml" --env-file "$tmp/.env" down >/dev/null 2>&1
	docker volume rm "${old}_agent_mail_data" "${new}_switchboard_data" >/dev/null 2>&1
	docker run --rm -v "$tmp":/t alpine:3.22 sh -c 'rm -rf /t/backup' >/dev/null 2>&1
	rm -rf "$tmp"
}
trap cleanup EXIT

# Old layout: the compose.yaml before the rename (service agent-mail, volume agent_mail_data); new layout: this checkout.
git -C "$here" show b40379ab4fadca406c35b1660a3026f11e38e319:compose.yaml >"$tmp/old.yaml" # last commit with the old names
grep -q '^  agent-mail:' "$tmp/old.yaml" || { echo "old compose.yaml must have the agent-mail service" >&2; exit 1; }
cp "$here/compose.yaml" "$tmp/new.yaml"
sed -i "s/:8097:8080/:$port:8080/" "$tmp/old.yaml" "$tmp/new.yaml"
tok=t-$tag-$RANDOM$RANDOM
printf 'SWITCHBOARD_TOKENS=test/a=%s,test/b=%s-b\nMAIL_BIND_ADDRESS=127.0.0.1\n' "$tok" "$tok" >"$tmp/.env"
chmod 600 "$tmp/.env"
base=http://127.0.0.1:$port

up_old() {
	SWITCHBOARD_IMAGE=$image docker compose -p "$old" -f "$tmp/old.yaml" --env-file "$tmp/.env" up -d --no-build >/dev/null 2>&1
	for _ in $(seq 60); do curl -fsS "$base/health" >/dev/null 2>&1 && return 0; sleep 1; done
	echo "old project did not start" >&2; return 1
}
send() { # n
	curl -fsS -o /dev/null -H "Authorization: Bearer $tok" -H "X-Switchboard-Role: test/a" -H 'Content-Type: application/json' \
		-H "Idempotency-Key: k$1" -d "{\"recipient\":\"test/b\",\"subject\":\"m$1\",\"body\":\"body $1\"}" "$base/api/messages"
}
count() { # -> number of messages test/b sees
	curl -fsS -H "Authorization: Bearer $tok-b" -H "X-Switchboard-Role: test/b" "$base/api/messages?box=inbox" | grep -o '"subject":"m[0-9]*"' | sort -u | wc -l
}
run() { bash "$script" --old-project "$old" --old-volume "${old}_agent_mail_data" --new-project "$new" \
	--compose-file "$tmp/new.yaml" --env-file "$tmp/.env" --backup-dir "$tmp/backup" "$@"; }
old_running() { docker ps -q --filter "label=com.docker.compose.project=$old" | grep -q .; }

up_old
n=${MESSAGES:-80} # the inbox list returns at most 100
for i in $(seq "$n"); do send "$i"; done
before=$(count)
echo "seeded: $before messages"
[ "$before" -eq "$n" ]

echo "== dry run"
run --dry-run
old_running && ! docker volume inspect "${new}_switchboard_data" >/dev/null 2>&1

for step in stop create copy verify backup start; do
	echo "== failure injected after $step"
	if run --fail-at "$step" >"$tmp/out" 2>&1; then cat "$tmp/out"; echo "expected failure" >&2; exit 1; fi
	old_running || { cat "$tmp/out"; echo "old container not running after failure at $step" >&2; exit 1; }
	[ "$(count)" -eq "$before" ] || { echo "old data changed" >&2; exit 1; }
	if [ "$step" = start ]; then
		# The new project ran: its volume is kept for inspection. Remove it the way an operator would.
		docker compose -p "$new" -f "$tmp/new.yaml" --env-file "$tmp/.env" down >/dev/null 2>&1
		docker volume rm "${new}_switchboard_data" >/dev/null
	fi
	docker volume inspect "${new}_switchboard_data" >/dev/null 2>&1 && { echo "new volume left after $step" >&2; exit 1; }
	grep -q 'old container healthy' "$tmp/out" || { cat "$tmp/out"; exit 1; }
done

echo "== move"
send 9999 # mail written just before the stop must arrive
before=$((before + 1))
run | tee "$tmp/out"
old_running && { echo "old container still running" >&2; exit 1; }
docker ps --filter "label=com.docker.compose.project=$new" --format '{{.Names}} {{.Status}}'
after=$(count)
echo "after move: $after messages (before $before)"
[ "$after" -eq "$before" ]
docker volume inspect "${new}_switchboard_data" -f '{{json .Labels}}'
# Compose must adopt the volume without the "not created by Docker Compose" warning.
if SWITCHBOARD_IMAGE=$image docker compose -p "$new" -f "$tmp/new.yaml" --env-file "$tmp/.env" up -d --no-build 2>&1 | grep -i 'already exists'; then
	echo "compose warned about the volume" >&2; exit 1
fi
send 10000 && [ "$(count)" -eq $((before + 1)) ]
docker volume inspect "${old}_agent_mail_data" >/dev/null # old volume kept
compgen -G "$tmp/backup/mail-before-rename-*.db" >/dev/null
echo "PASS"
