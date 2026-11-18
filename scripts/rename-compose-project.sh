#!/usr/bin/env bash
# shellcheck disable=SC2016 # the single-quoted scripts run inside the helper container, with their own $1/$2
# Moves the running deployment from one Compose project to another (project, service and data volume names), on the
# deploy host. Run from the directory that holds the new compose.yaml and the deploy .env (the Jenkins job workspace),
# or point at them with --compose-file / --env-file.
#
#   scripts/rename-compose-project.sh --dry-run   # checks and prints the plan, changes nothing
#   scripts/rename-compose-project.sh             # the move with the defaults below
#
# Steps: check -> build the sqlite helper image -> stop the old container -> create the new volume (with the Compose
# labels, so Compose adopts it without a warning) -> copy the files (owner and mode kept) -> verify (sha256 of every
# file, owner and mode, PRAGMA integrity_check, row count per table) -> SQLite backup into --backup-dir -> start the new
# project with the image the old container ran -> wait for its health check.
# On any failure the new project is stopped, the old container is started again and the script exits non-zero.
# The old volume and the old (stopped) container are never removed. Nothing here prints .env values.
# Order of the whole move (Jenkins job, merge): docs/rename-compose-project.md. Test: scripts/rename-compose-project_test.sh.
set -euo pipefail

old_project=agent-mail
old_service=agent-mail
old_volume=agent-mail_agent_mail_data
new_project=switchboard
new_service=switchboard
new_volume_key=switchboard_data
compose_file=compose.yaml
env_file=.env
backup_dir="$PWD/rename-compose-backup"
db_file=mail.db
helper_base=alpine:3.22
helper_image=switchboard-rename-helper:local
health_timeout=120
start_new=1
dry_run=0
fail_at=""

usage() {
	cat <<'EOF'
Usage: rename-compose-project.sh [options]
  --old-project NAME      Compose project now running          (default agent-mail)
  --old-service NAME      its service                          (default agent-mail)
  --old-volume NAME       its data volume, full Docker name     (default agent-mail_agent_mail_data)
  --new-project NAME      new Compose project                  (default switchboard)
  --new-service NAME      service in --compose-file            (default switchboard)
  --new-volume-key NAME   volume key in --compose-file         (default switchboard_data;
                          Docker name <new-project>_<key>, e.g. switchboard_switchboard_data)
  --compose-file PATH     compose file of the new names        (default ./compose.yaml)
  --env-file PATH         deploy .env (never printed)          (default ./.env)
  --backup-dir PATH       where the SQLite backup goes         (default ./rename-compose-backup)
  --db-file NAME          database file inside the volume      (default mail.db)
  --helper-base IMAGE     base of the sqlite helper image      (default alpine:3.22)
  --health-timeout SEC    wait for the new container           (default 120)
  --no-start              stop after copy, verify and backup; start the old container again
  --dry-run               check and print the plan only
  --fail-at STEP          test only: fail after STEP (stop|create|copy|verify|backup|start)
EOF
}

while [ $# -gt 0 ]; do
	case "$1" in
	--old-project) old_project=$2; shift 2 ;;
	--old-service) old_service=$2; shift 2 ;;
	--old-volume) old_volume=$2; shift 2 ;;
	--new-project) new_project=$2; shift 2 ;;
	--new-service) new_service=$2; shift 2 ;;
	--new-volume-key) new_volume_key=$2; shift 2 ;;
	--compose-file) compose_file=$2; shift 2 ;;
	--env-file) env_file=$2; shift 2 ;;
	--backup-dir) backup_dir=$2; shift 2 ;;
	--db-file) db_file=$2; shift 2 ;;
	--helper-base) helper_base=$2; shift 2 ;;
	--health-timeout) health_timeout=$2; shift 2 ;;
	--no-start) start_new=0; shift ;;
	--dry-run) dry_run=1; shift ;;
	--fail-at) fail_at=$2; shift 2 ;;
	-h | --help) usage; exit 0 ;;
	*) echo "unknown option: $1" >&2; usage >&2; exit 2 ;;
	esac
done

new_volume="${new_project}_${new_volume_key}"
t0=$(date +%s)
log() { printf '%s [+%ss] %s\n' "$(date +%H:%M:%S)" "$(($(date +%s) - t0))" "$*"; }
die() { log "ERROR: $*" >&2; exit 1; }
maybe_fail() { if [ "$fail_at" = "$1" ]; then die "injected failure after step '$1' (--fail-at)"; fi; }
compose_new() { docker compose -p "$new_project" -f "$compose_file" --env-file "$env_file" "$@"; }

# State for rollback.
old_id=""
old_was_running=0
stopped_old=0
created_new_volume=0
started_new=0
stopped_at=""

wait_healthy() { # container-id timeout
	local id=$1 limit=$2 s=0 st
	while :; do
		st=$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "$id" 2>/dev/null || echo gone)
		case "$st" in
		healthy) return 0 ;;
		unhealthy | exited | dead | gone) log "container $id: $st"; return 1 ;;
		esac
		[ "$s" -ge "$limit" ] && { log "container $id: still $st after ${limit}s"; return 1; }
		sleep 2; s=$((s + 2))
	done
}

rollback() {
	local rc=$?
	trap - EXIT
	[ "$rc" -eq 0 ] && return 0
	set +e
	log "rolling back (exit $rc)"
	if [ "$started_new" -eq 1 ]; then
		log "stopping the new project $new_project (its volume $new_volume is kept)"
		compose_new stop "$new_service" >/dev/null 2>&1
	elif [ "$created_new_volume" -eq 1 ]; then
		log "removing the new volume $new_volume (never used by a server)"
		docker volume rm "$new_volume" >/dev/null
	fi
	if [ "$stopped_old" -eq 1 ]; then
		log "starting the old container $old_id again"
		if docker start "$old_id" >/dev/null && wait_healthy "$old_id" "$health_timeout"; then
			log "old container healthy; down for $(($(date +%s) - stopped_at))s"
		else
			log "OLD CONTAINER DID NOT BECOME HEALTHY: check 'docker logs $old_id'"
		fi
	fi
	log "failed; old volume $old_volume untouched"
	exit "$rc"
}
trap rollback EXIT

# 1. Checks (read-only).
command -v docker >/dev/null || die "docker not found"
docker compose version >/dev/null || die "docker compose not available"
[ -f "$compose_file" ] || die "compose file not found: $compose_file"
[ -f "$env_file" ] || die "env file not found: $env_file"
case "$backup_dir" in /*) ;; *) backup_dir="$PWD/$backup_dir" ;; esac
docker volume inspect "$old_volume" >/dev/null 2>&1 || die "old volume not found: $old_volume"
if docker volume inspect "$new_volume" >/dev/null 2>&1; then
	die "new volume already exists: $new_volume (remove it after checking it is not in use, then run again)"
fi
mapfile -t ids < <(docker ps -aq --filter "label=com.docker.compose.project=$old_project" --filter "label=com.docker.compose.service=$old_service")
[ "${#ids[@]}" -eq 1 ] || die "expected one container of $old_project/$old_service, found ${#ids[@]}"
old_id=${ids[0]}
[ "$(docker inspect -f '{{.State.Running}}' "$old_id")" = true ] && old_was_running=1
docker inspect -f '{{range .Mounts}}{{if eq .Type "volume"}}{{.Name}}{{"\n"}}{{end}}{{end}}' "$old_id" | grep -qx "$old_volume" ||
	die "container $old_id does not mount $old_volume"
image=$(docker inspect -f '{{.Config.Image}}' "$old_id")
mapfile -t new_ids < <(docker ps -aq --filter "label=com.docker.compose.project=$new_project")
[ "${#new_ids[@]}" -eq 0 ] || die "project $new_project already has containers"
compose_new config --services 2>/dev/null | grep -qx "$new_service" || die "service $new_service not in $compose_file (or the env file lacks a required key)"
compose_new config --volumes 2>/dev/null | grep -qx "$new_volume_key" || die "volume key $new_volume_key not in $compose_file"

log "old: project $old_project, service $old_service, container $old_id (running: $old_was_running), volume $old_volume"
log "new: project $new_project, service $new_service, volume $new_volume, image $image"
log "backup: $backup_dir"
if [ "$dry_run" -eq 1 ]; then
	log "dry run: would build $helper_image, stop $old_id, create and fill $new_volume, verify, back up, start $new_project"
	exit 0
fi

# 2. Helper image with sqlite3, built before the stop so the download is not downtime.
log "building helper image $helper_image from $helper_base"
printf 'FROM %s\nRUN apk add --no-cache sqlite\n' "$helper_base" | docker build -q -t "$helper_image" - >/dev/null
helper() { docker run --rm --network none "$@"; }
umask 077
mkdir -p "$backup_dir"

# 3. Stop the old container. Downtime starts.
stopped_at=$(date +%s)
if [ "$old_was_running" -eq 1 ]; then
	log "stopping $old_id"
	docker stop -t 30 "$old_id" >/dev/null
	stopped_old=1
fi
maybe_fail stop

# 4. New volume with the labels Compose sets, so `up` adopts it instead of warning that it was not created by Compose.
docker volume create --label "com.docker.compose.project=$new_project" --label "com.docker.compose.volume=$new_volume_key" \
	"$new_volume" >/dev/null
created_new_volume=1
log "created $new_volume"
maybe_fail create

# 5. Copy every file (WAL and SHM too: the container is stopped, so the set is consistent), keeping owner and mode.
helper -v "$old_volume":/old:ro -v "$new_volume":/new "$helper_image" sh -euc '
	cp -a /old/. /new/
	chown "$(stat -c %u:%g /old)" /new
	chmod "$(stat -c %a /old)" /new'
log "copied"
maybe_fail copy

# 6. Verify. SQLite runs on scratch copies inside the helper: opening the volume itself as root could leave
# root-owned -wal/-shm files the server cannot open.
listing='set -eu; cd "$1"; find . -exec stat -c "%n %u:%g %a %F" {} + | sort; find . -type f -exec sha256sum {} + | sort -k2'
a=$(helper -v "$old_volume":/v:ro "$helper_image" sh -c "$listing" sh /v)
b=$(helper -v "$new_volume":/v:ro "$helper_image" sh -c "$listing" sh /v)
[ "$a" = "$b" ] || die "file list, owner, mode or sha256 differs between $old_volume and $new_volume"
log "sha256, owner and mode match ($(printf '%s\n' "$a" | grep -c ' \./') files)"
counts='cp -a "$1"/. /tmp/db/ && cd /tmp/db && test -f "$2" &&
	test "$(sqlite3 "$2" "PRAGMA integrity_check")" = ok &&
	for t in $(sqlite3 "$2" "SELECT name FROM sqlite_master WHERE type='"'"'table'"'"' AND name NOT LIKE '"'"'sqlite_%'"'"' ORDER BY name"); do
		printf "%s %s\n" "$t" "$(sqlite3 "$2" "SELECT count(*) FROM \"$t\"")"
	done'
ca=$(helper --tmpfs /tmp/db -v "$old_volume":/v:ro "$helper_image" sh -c "$counts" sh /v "$db_file") || die "integrity check failed on a copy of $old_volume"
cb=$(helper --tmpfs /tmp/db -v "$new_volume":/v:ro "$helper_image" sh -c "$counts" sh /v "$db_file") || die "integrity check failed on a copy of $new_volume"
[ -n "$ca" ] || die "no tables found in $db_file"
[ "$ca" = "$cb" ] || die "row counts differ"
log "integrity_check ok; rows per table:"
printf '%s\n' "$ca" | sed 's/^/    /'
maybe_fail verify

# 7. SQLite backup of the new copy, outside both volumes.
stamp=$(date +%Y%m%d-%H%M%S)
helper --tmpfs /tmp/db -v "$new_volume":/v:ro -v "$backup_dir":/out "$helper_image" sh -euc '
	cp -a /v/. /tmp/db/
	sqlite3 "/tmp/db/$1" ".backup /out/$2"
	test "$(sqlite3 "/out/$2" "PRAGMA integrity_check")" = ok
	chmod 600 "/out/$2"' sh "$db_file" "mail-before-rename-$stamp.db"
log "backup $backup_dir/mail-before-rename-$stamp.db"
maybe_fail backup

if [ "$start_new" -eq 0 ]; then
	log "--no-start: copy verified; starting the old container again and removing nothing"
	created_new_volume=0 # keep the verified copy
	if [ "$stopped_old" -eq 1 ]; then
		docker start "$old_id" >/dev/null
		wait_healthy "$old_id" "$health_timeout" || die "old container did not become healthy"
	fi
	log "done; $new_volume holds a copy taken at the stop, older than the live data from now on"
	exit 0
fi

# 8. Start the new project with the image the old container ran.
log "starting $new_project/$new_service"
started_new=1
SWITCHBOARD_IMAGE=$image compose_new up -d --no-build --no-deps "$new_service" 2>&1 | grep -v '^ *\(Network\|Container\|Volume\)' || true
new_id=$(compose_new ps -q "$new_service")
[ -n "$new_id" ] || die "new container not created"
wait_healthy "$new_id" "$health_timeout" || die "new container did not become healthy"
docker exec "$new_id" wget -q -O /dev/null http://127.0.0.1:8080/health || die "/health failed in the new container"
maybe_fail start
log "new container $new_id healthy; down for $(($(date +%s) - stopped_at))s"
log "done. Old container $old_id is stopped (not removed); old volume $old_volume is kept."
