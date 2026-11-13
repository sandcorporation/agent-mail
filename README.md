# Agent Mail

Agent Mail is a small self hosted mailbox for stable, role addressed agent identities. The service owns the identity boundary and durable message store; identities such as `sandcorp/boss` or `codemit/worker-1` are configured roles, not Orca session or terminal handles. A role keeps the same inbox after an agent session restarts.

## Design decisions

- Go service and CLI share one module; SQLite is local to the service and persisted on a named Docker volume. SQLite keeps deployment small and avoids a separate database service.
- Every API call uses a bearer token mapped server side to exactly one role. Roles are configured explicitly as `role=token`; senders cannot impersonate another sender, and recipients must be configured roles.
- The dashboard is served by the service and uses browser text nodes for message content. It never renders message markdown as HTML. The browser stores its selected token in local storage on that browser profile.
- Watch is durable long polling. The cursor is the last delivered global message ID. Multiple watchers can replay from the same cursor and receive duplicate notifications; consumers should deduplicate by message ID. A watch only reports new mail; it does not mark mail read or wake a stopped LLM.

## Message and status semantics

Messages have sender, recipient, subject, markdown or plain text body, UTC timestamp, optional task ID, thread ID, and reply-to ID. New recipient messages are `pending`. Opening with `read` marks the message `read`; `ack` explicitly accepts responsibility and stores `acknowledged`; `complete` records finished work. These are recipient-side states. Inbox listing and watch do not consume or mutate messages. Outbox lists messages authored by the current role. Search matches subject, body, and task ID; the service returns at most 100 newest matches.

A thread's root ID is shared in `thread_id`; `reply_to` points at the immediate parent. Idempotency keys are unique per sender and key. A retry with the same key returns the original message and does not create a second one. Keep a key stable for retries of one logical send and use a fresh key for a new message.

## Build and run locally

Requires Go 1.24+ for development. End users install a compiled release binary and do not need Go. Create distinct random tokens per role and set them in the environment (never commit token values):

```sh
export AGENT_MAIL_TOKENS='sandcorp/boss=REPLACE_WITH_RANDOM,codemit/coordinator=REPLACE_WITH_RANDOM,codemit/worker-1=REPLACE_WITH_RANDOM'
export DB_PATH="$PWD/mail.db"
go run ./cmd/agent-mail
```

The service listens on `:8080` by default. Visit `http://localhost:8080`. Health endpoint: `/health`.

Configure a CLI (token argument is stored with user-only file permissions):

```sh
agent-mail config set --endpoint http://localhost:8080 --role codemit/worker-1 --token YOUR_ROLE_TOKEN
agent-mail send --to codemit/coordinator --subject 'Build ready' --task task-123 --body-file report.md --idempotency-key task-123-report
cat report.md | agent-mail send --to codemit/coordinator --subject 'Build ready' --body-file /dev/stdin
agent-mail inbox [--q term] [--json]
agent-mail outbox --json
agent-mail read 42
agent-mail reply 42 --subject 'Re: Build ready' --body 'Thanks'
agent-mail ack 42
agent-mail complete 42
agent-mail watch --after 0 --json
```

`send` and `reply` accept `--body`, `--body-file`, or piped stdin. `--json` returns machine-readable results for list/read/send/status operations. `watch` is a reconnecting long poll; save its printed cursor and pass it back with `--after` after restarting. Notifications can repeat after reconnect by design.

## Docker deployment

The Compose file binds to loopback by default (`127.0.0.1:8097`). For private network access, set `MAIL_BIND_ADDRESS` in the external `.env` to the intended private interface address; do not bind publicly without a TLS reverse proxy and appropriate network controls. Set `AGENT_MAIL_TOKENS` in an external `.env` file with mode `0600`, build and start with `docker compose up -d --build`. The container runs as nonroot, drops capabilities, has a read-only root filesystem, health check, restart policy, and a named volume. Never put credentials in the Compose file or source control.

Backup and restore while stopped (SQLite WAL is checkpointed on clean shutdown):

```sh
docker compose stop
mkdir -p backup
docker run --rm -v agent-mail-mvp_agent_mail_data:/data -v "$PWD/backup:/backup" alpine sh -c 'cp /data/mail.db /backup/mail.db'
docker compose start
# Restore: stop service, copy a verified backup back to /data/mail.db, then start.
```

Use the actual volume name from `docker volume ls` if Compose prefixes it differently. Retain and protect the token file separately from database backups. For safe online backups use SQLite's backup API or briefly stop the container as above.

## API

All `/api/*` routes require `Authorization: Bearer <role-token>`. Unauthorized requests return 401; a recipient outside the configured role set returns 403.

- `POST /api/messages` with `{recipient,subject,body,task_id?,thread_id?,reply_to?}`. Optional `Idempotency-Key` header.
- `GET /api/messages?box=inbox|outbox&q=...&status=...`
- `GET /api/messages/{id}`; recipient read marks `read`, sender may inspect own outbox messages.
- `POST /api/messages/{id}/reply` with `{subject,body,recipient?}`.
- `POST /api/messages/{id}/ack`, `/complete`; recipient only.
- `GET /api/watch?after=<message-id>` waits up to 25 seconds, returns `{messages:[ids],cursor:id,timeout?:true}`. Replay with the returned cursor. IDs are durable; concurrent watchers independently replay and can see duplicates.
- `GET /health` is unauthenticated and returns `{ok:true}`.

Orca integration belongs in a separate wake/session adapter. This service has no Orca terminal handle dependency and cannot start a stopped session. An adapter may poll/watch and route a role notification to an available session; mailbox durability and explicit acknowledgment remain authoritative.

## Install and uninstall CLI

GitHub Actions builds versioned archives for Windows, Linux, and macOS (amd64 and arm64 where supported), plus SHA-256 checksums. Download the archive for your platform from Releases, verify `checksums.txt`, and place `agent-mail`/`agent-mail.exe` on PATH. Configure the endpoint, role, and token as above. Config is stored at the OS user config directory under `agent-mail/config.json` with user-only permissions. To uninstall, remove the binary and that config directory; removing config deletes the local token copy. Server messages remain until the operator removes the service volume.

## Tests and limitations

Run `go test ./...`. Current tests cover SQLite persistence across database reopen, retry idempotency, auth rejection, watch cursor replay, and status filtering. This first release uses a single SQLite writer, bounded newest-first listing, basic substring search, bearer tokens without token rotation UI, and long polling rather than push streaming. Back up the data volume and restrict network access to private/Tailscale paths.
