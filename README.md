# Agent Mail

Agent Mail is a small self hosted mailbox for stable, role addressed agent identities. The service owns the identity boundary and durable message store; identities such as `sandcorp/boss` or `codemit/worker-1` are configured roles, not Orca session or terminal handles. A role keeps the same inbox after an agent session restarts.

## Design decisions

- Go service and CLI share one module; SQLite is local to the service and persisted on a named Docker volume. SQLite keeps deployment small and avoids a separate database service.
- Every API call uses a bearer credential mapped server side to exactly one role. Existing `AGENT_MAIL_TOKENS=role=token,...` deployments are imported into SQLite at startup. New roles can enroll at runtime when the operator sets an external `AGENT_MAIL_ENROLLMENT_TOKEN`; senders cannot impersonate another sender, and recipients must be registered roles.
- The dashboard is a read-only administrator view of all roles and mailboxes. It uses same-origin requests and never receives role credentials; message content is rendered as text.
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

Configure a CLI (read the token from a protected file; the token is stored with user-only file permissions):

```sh
agent-mail config set --endpoint http://localhost:8080 --role codemit/worker-1 --token-file /path/to/role-token
agent-mail --profile codemit-worker config set --endpoint http://localhost:8080 --role codemit/worker-1 --token-file /path/to/role-token
agent-mail send --to codemit/coordinator --subject 'Build ready' --task task-123 --body-file report.md --idempotency-key task-123-report
cat report.md | agent-mail send --to codemit/coordinator --subject 'Build ready' --body-file /dev/stdin
agent-mail inbox [--q term] [--status pending|read|acknowledged|complete] [--json]
agent-mail outbox --json
agent-mail read 42
agent-mail reply 42 --subject 'Re: Build ready' --body 'Thanks'
agent-mail ack 42
agent-mail complete 42
agent-mail watch --after 0 --json
agent-mail wait --timeout 1800
```

`send` and `reply` accept `--body`, `--body-file`, or piped stdin. `--json` returns machine-readable results for list/read/send/status operations. `watch` is a reconnecting long poll; save its printed cursor and pass it back with `--after` after restarting. Notifications can repeat after reconnect by design.

`wait` checks the inbox for `pending` messages (every `--interval` seconds, default 15), prints them as `#ID sender | subject | task=ID` lines (or a JSON array with `--json`) and exits 0. It never marks anything read, and it returns immediately when pending mail already exists, so it is safe to run as a background "wake me when mail arrives" step. With no pending mail before `--timeout` (default 1800 seconds, `0` = no limit) it exits 3. Credential errors exit 1 at once; transient network errors are retried.

Every command accepts only its own options; `agent-mail <command> --help` lists them. An unknown command or option, a missing option value, or an invalid `--status` exits with code 2 instead of being ignored. Exit codes: 0 success, 1 error, 2 usage error, 3 `wait` timed out.

Use a separate `--profile NAME` for each local session or role; each profile has its own protected credential file. `--profile NAME` (or `--profile=NAME`) works before or after the command, including `config set`, and takes precedence over `AGENT_MAIL_PROFILE`. A profile named by either must already have a saved config; the CLI exits with an error rather than falling back to the default profile. `AGENT_MAIL_PROFILE`, `AGENT_MAIL_ENDPOINT`, `AGENT_MAIL_ROLE`, and `AGENT_MAIL_TOKEN` can select or override a profile for isolated process environments. The server always derives the sender role from the credential, regardless of caller-supplied message fields. To enroll a role without restarting the server, set the operator-only enrollment credential in the deployment's external `.env`, then run `agent-mail register --endpoint URL --role ROLE --token-file ROLE_TOKEN_FILE --enrollment-token-file ENROLLMENT_TOKEN_FILE`. A role owner can resume with the same role credential; registration cannot replace an existing role credential. Keep the enrollment credential private and rotate or remove it when enrollment is closed.

## Docker deployment

The Compose file binds to loopback by default (`127.0.0.1:8097`). For private network access, set `MAIL_BIND_ADDRESS` in the external `.env` to the intended private interface address; do not bind publicly without a TLS reverse proxy and appropriate network controls. Set `AGENT_MAIL_TOKENS` and, when enabling dynamic enrollment, `AGENT_MAIL_ENROLLMENT_TOKEN` in an external `.env` file with mode `0600`; build and start with `docker compose up -d --build`. The enrollment value is optional so existing deployments continue to start; registration is disabled until it is set. Existing role definitions and message history stay in the SQLite volume. The container runs as nonroot, drops capabilities, has a read-only root filesystem, health check, restart policy, and a named volume. Never put credentials in the Compose file or source control.

### Automated deployment (Actions image + Jenkins)

`docker compose up -d --build` above builds locally (the image is tagged `agent-mail:local`). The production path never builds on the server:

1. A pull request runs `CI` (tests, vet, build).
2. A push to `main` runs `promote`: the same checks in parallel with `image`, which builds `linux/amd64` and pushes `ghcr.io/sandcorporation/agent-mail:<commit sha>`. When both pass, `production` is fast-forwarded to that commit (`scripts/ci/promote.sh`). `production` only moves forward; roll back with a revert PR.
3. A Jenkins job using the `Jenkinsfile` watches `production` only. Through the `sandcorp` shared library (`sandcorpDeploy`) it waits for the GHCR image of the `production` head, pulls it on the node labelled `agent-mail-deploy`, recreates the `agent-mail` service with `AGENT_MAIL_IMAGE` set to that image (`--no-build`), waits for the container health check and runs `/health` inside the container. If the new container does not become healthy it switches back to the previous successfully deployed image. The `ROLLBACK_SHA` job parameter deploys an older published image.

Nothing deploys until such a job exists. Host-specific values are not in this repository: the deploy node is chosen by its Jenkins label, and secrets plus `MAIL_BIND_ADDRESS` come from the `.env` file in the job workspace on that node (mode `0600`, same keys as above), or, optionally, from an env file outside the workspace named by the node's `DEPLOY_ENV_FILE` environment variable. The workspace `.env` is the default; note that Jenkins can wipe the workspace when a checkout fails, so keep a copy of it. The deploy stops before replacing anything if `AGENT_MAIL_TOKENS` or `MAIL_BIND_ADDRESS` is empty. Keep the Compose project name `agent-mail` so the existing `agent-mail_agent_mail_data` volume is reused; moving from a manual deployment only recreates the container. The deploy node needs Docker with Compose and pull access to the GHCR package.

### Protected administrator dashboard

The dashboard provides read-only access to all messages and registered role names. Its `/api/admin/*` endpoints return `401` unless `AGENT_MAIL_ADMIN_PROXY_TOKEN` is configured and the request includes the matching `X-Agent-Mail-Admin` header. The browser does not know this value. Configure a separate random secret in the private deployment `.env` and in the service's private reverse-proxy configuration; do not reuse any role or Authelia credential. The proxy must authenticate the browser session with Authelia before forwarding and must overwrite (never pass through) any incoming `X-Agent-Mail-Admin` header with the configured secret. Keep the backend bound to loopback/private networking so clients cannot bypass the proxy boundary. Requests without the secret cannot use admin APIs, and these endpoints only accept `GET`.

For nginx, keep your existing Authelia protection on the dashboard host and add this directive inside only this service's already-protected `location /` (substitute the private secret from your secret-management process; do not commit it):

```nginx
proxy_set_header X-Agent-Mail-Admin "<same random value as AGENT_MAIL_ADMIN_PROXY_TOKEN>";
```

This replaces any browser-supplied admin header, while leaving ordinary role-token API requests intact for CLI use. The dashboard itself does not send an `Authorization` header. Validate nginx configuration before reloading it. If Authelia protects only selected paths, also protect `/api/admin/` with the same authenticated session check.

Backup and restore while stopped (SQLite WAL is checkpointed on clean shutdown):

```sh
docker compose stop
mkdir -p backup
docker run --rm -v agent-mail_agent_mail_data:/data -v "$PWD/backup:/backup" alpine sh -c 'cp /data/mail.db /backup/mail.db'
docker compose start
# Restore: stop service, copy a verified backup back to /data/mail.db, then start.
```

Use the actual volume name from `docker volume ls` if Compose prefixes it differently. Retain and protect the token file separately from database backups. For safe online backups use SQLite's backup API or briefly stop the container as above.

## API

All `/api/*` routes except registration require `Authorization: Bearer <role-token>` and `X-Agent-Mail-Role: <expected-role>`. The server checks that the selected role matches the credential, so a session cannot accidentally use another profile's identity. Unauthorized or mismatched requests return 401; a recipient outside the registered role set returns 403. `POST /api/register` accepts `{role,credential}` and requires the operator enrollment bearer credential for new roles. Existing roles may only be resumed by presenting their same role credential.

- `POST /api/messages` with `{recipient,subject,body,task_id?,thread_id?,reply_to?}`. Optional `Idempotency-Key` header.
- `GET /api/messages?box=inbox|outbox&q=...&status=...`
- `GET /api/messages/{id}`; recipient read marks `read`, sender may inspect own outbox messages.
- `POST /api/messages/{id}/reply` with `{subject,body,recipient?}`.
- `POST /api/messages/{id}/ack`, `/complete`; recipient only.
- `GET /api/watch?after=<message-id>` waits up to 25 seconds, returns `{messages:[ids],cursor:id,timeout?:true}`. Replay with the returned cursor. IDs are durable; concurrent watchers independently replay and can see duplicates.
- `GET /health` is unauthenticated and returns `{ok:true}`.
- `POST /api/register` durably registers a new role without a service restart, or verifies credential ownership to resume an existing role.

Orca integration belongs in a separate wake/session adapter. This service has no Orca terminal handle dependency and cannot start a stopped session. An adapter may poll/watch and route a role notification to an available session; mailbox durability and explicit acknowledgment remain authoritative.

## Install and uninstall CLI

GitHub Actions builds versioned archives for Windows, Linux, and macOS (amd64 and arm64 where supported), plus SHA-256 checksums. Download the archive for your platform from Releases, verify `checksums.txt`, and place `agent-mail`/`agent-mail.exe` on PATH. Configure the endpoint, role, and token as above. Config is stored at the OS user config directory under `agent-mail/config.json` with user-only permissions. To uninstall, remove the binary and that config directory; removing config deletes the local token copy. Server messages remain until the operator removes the service volume.

## Tests and limitations

Run `go test ./...`. Current tests cover SQLite persistence across database reopen, retry idempotency, auth rejection, watch cursor replay, and status filtering. This first release uses a single SQLite writer, bounded newest-first listing, basic substring search, bearer tokens without token rotation UI, and long polling rather than push streaming. Back up the data volume and restrict network access to private/Tailscale paths.
