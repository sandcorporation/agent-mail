# Move to a new host

Placeholders:

| Placeholder | Meaning |
| --- | --- |
| `old-host` | current host |
| `new-host` | target host |
| `mail.example` | public name behind the reverse proxy |
| `10.0.0.1` / `10.0.0.2` | private address of `old-host` / `new-host` |
| `203.0.113.2` | public address of `new-host` (if it serves the proxy) |
| `<sha>` | commit at the head of `production` |

Deployment names, set in each shell below. They are bound to the running deployment and its data, so they stay until a separate migration:

```sh
PROJECT=agent-mail                        # 대장 결정 전 유지: Compose project (compose.yaml, Jenkinsfile)
VOLUME=${PROJECT}_agent_mail_data         # data volume of that project
IMAGE=ghcr.io/sandcorporation/agent-mail  # 대장 결정 전 유지: image Jenkins pulls (also pushed as .../switchboard)
LABEL=agent-mail-deploy                   # 대장 결정 전 유지: Jenkins deploy node label
BACKUP=/srv/backup/agent-mail             # 대장 결정 전 유지: backup directory on the host
```

Addresses side by side:

| | `old-host` | `new-host` |
| --- | --- | --- |
| Private (`MAIL_BIND_ADDRESS`, clients and proxy upstream) | `10.0.0.1:8097` | `10.0.0.2:8097` |
| Public (DNS for `mail.example`) | current record | `203.0.113.2`, or unchanged if the proxy stays where it is |

Expected downtime: from step 2 to the end of step 4, about 5 minutes for a database of a few hundred MB. Clients that use `mail.example` also wait for the DNS TTL in step 5; lower it to 60 s a day before.

## 1. Prepare `new-host`

1. Docker with Compose, pull access to `$IMAGE`. The image is multi-arch (`linux/amd64`, `linux/arm64`).
2. Check the architecture is served:

   ```sh
   docker buildx imagetools inspect "$IMAGE:<sha>"
   ```

3. Jenkins node with the label `$LABEL` (the old node keeps the label until step 4).
4. `.env` (mode `0600`) in the job workspace or at `DEPLOY_ENV_FILE`, same keys as on `old-host`; `MAIL_BIND_ADDRESS=10.0.0.2`.
5. Empty volume and a dry start:

   ```sh
   docker volume create "$VOLUME"
   AGENT_MAIL_IMAGE="$IMAGE:<sha>" docker compose -p "$PROJECT" up -d --no-build
   curl -fsS http://10.0.0.2:8097/health          # {"ok":true,"version":"<short sha>"}
   docker compose -p "$PROJECT" down               # keep the volume
   docker run --rm -v "$VOLUME":/data alpine rm -f /data/mail.db /data/mail.db-wal /data/mail.db-shm
   ```

6. Firewall: `10.0.0.2:8097` reachable from the proxy and the clients' private network only.

## 2. Freeze `old-host`

```sh
docker compose -p "$PROJECT" stop
```

A clean stop checkpoints the WAL into `mail.db`. Do not start it again.

## 3. Copy

On `old-host`:

```sh
mkdir -p ~/move
docker run --rm -v "$VOLUME":/data alpine sh -c \
  'apk add -q sqlite && sqlite3 /data/mail.db ".backup /data/move.db" && sqlite3 /data/move.db "PRAGMA integrity_check"'
docker run --rm -v "$VOLUME":/data -v ~/move:/out alpine sh -c 'mv /data/move.db /out/ && sha256sum /out/move.db'
scp ~/move/move.db new-host:move/
```

On `new-host`:

```sh
sha256sum ~/move/move.db                        # must match old-host
docker run --rm -v "$VOLUME":/data -v ~/move:/in alpine sh -c \
  'cp /in/move.db /data/mail.db && chown "$(stat -c %u:%g /data)" /data/mail.db && sha256sum /data/mail.db'
```

## 4. Start on `new-host`

1. Move the `$LABEL` label from the old node to the new node and run the Jenkins job (or `docker compose -p "$PROJECT" up -d --no-build` with `AGENT_MAIL_IMAGE=$IMAGE:<sha>`).
2. Health and version:

   ```sh
   curl -fsS http://10.0.0.2:8097/health          # version = short <sha>
   ```

3. Role smoke from a client, pointed at the new address:

   ```sh
   export SWITCHBOARD_ENDPOINT=http://10.0.0.2:8097
   switchboard inbox                                # existing mail is there
   switchboard send --to <own role> --subject move-smoke --body ok
   switchboard inbox                                # move-smoke arrives
   ```

4. Dashboard through the proxy: messages and roles from before the move are listed.

## 5. Switch proxy and DNS

1. Proxy upstream for `mail.example`: `10.0.0.1:8097` -> `10.0.0.2:8097`; validate the configuration, then reload.
2. DNS for `mail.example` -> `203.0.113.2` if the proxy moved.
3. Clients that use the private address directly: update `SWITCHBOARD_ENDPOINT` in their profiles.
4. `curl -fsS https://mail.example/health` returns the new version.

## 6. Keep `old-host` for 7 days

- Container stopped, volume and `.env` untouched, no Jenkins label.
- `docker compose -p "$PROJECT" stop` only; never `down -v`.
- After 7 days without rollback: remove the container, then the volume.

## Rollback

Before any client wrote to `new-host`:

1. Stop `new-host`: `docker compose -p "$PROJECT" stop`.
2. Put the label back on the old node, start `old-host`: `docker compose -p "$PROJECT" start`.
3. Proxy upstream and DNS back to `old-host`.

After clients wrote to `new-host`: run steps 2-3 in the other direction (freeze `new-host`, copy its `mail.db` to `old-host`, verify sha256), then the three steps above.

## Backups on `new-host`

What to back up:

- the data volume: `mail.db` only, taken with SQLite's backup API (never a file copy of a live WAL database: `mail.db` without its `-wal` is incomplete);
- the `.env` file, stored separately from the database backups;
- not the image (rebuildable from the commit).

Backup hook (online, container keeps running):

```sh
#!/bin/sh
set -eu
out=/srv/backup/agent-mail  # 대장 결정 전 유지: backup directory (BACKUP above)
VOLUME=agent-mail_agent_mail_data  # 대장 결정 전 유지: data volume (VOLUME above)
stamp=$(date +%Y%m%d-%H%M%S)
mkdir -p "$out"
docker run --rm -v "$VOLUME":/data -v "$out":/out alpine sh -c \
  "apk add -q sqlite && sqlite3 /data/mail.db \".backup /out/mail-$stamp.db\" && sqlite3 /out/mail-$stamp.db 'PRAGMA integrity_check'"
find "$out" -name 'mail-*.db' -mtime +14 -delete
```

Register the hook (or `$BACKUP`) with the host's backup job, and test a restore once:

```sh
docker compose -p "$PROJECT" stop
docker run --rm -v "$VOLUME":/data -v "$BACKUP":/in alpine sh -c \
  'rm -f /data/mail.db-wal /data/mail.db-shm && cp /in/mail-<stamp>.db /data/mail.db && chown "$(stat -c %u:%g /data)" /data/mail.db'
docker compose -p "$PROJECT" start
```
