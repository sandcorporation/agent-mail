# Architecture and API reference

The Go HTTP server authenticates each request by constant-time comparison against the configured `AGENT_MAIL_TOKENS` role/token map. That role is the sender identity and recipient authorization set. SQLite stores immutable message content plus recipient-specific state transitions in `role_states`; the database is mounted on a persistent volume. The CLI sends JSON over HTTPS/HTTP to the configured endpoint. The dashboard is served by the same process and treats message fields as text.

## State model

`pending` means newly delivered. Opening as recipient records `read`; `/ack` records `acknowledged`; `/complete` records `complete` (status filters also accept `completed`). Each is explicit and recipient scoped. Listing and watch have no state side effects. State is monotonic by intent but endpoints allow an explicit later state update; clients should use the furthest state reached.

## Delivery model

Every message receives a monotonically increasing SQLite ID. A client stores its cursor and polls `GET /api/watch?after=id`; server returns IDs greater than that cursor for the caller's role, or times out. The cursor is a global message ID, so it can safely skip unrelated roles' IDs. Clients may replay from an older cursor and see duplicates, and should dedupe by ID. No watcher claims or consumes an event. The watcher is not a session wake mechanism.

`GET /api/events` is the push form of the same model. A subscriber registers with an in-process hub for its role before it queries SQLite, so mail stored between the query and the wait still wakes it. A wake-up carries no data: the stream re-reads pending messages with an ID above its cursor, and SQLite remains the only source of truth. Each subscriber holds at most one queued wake-up, so a send never blocks on a slow or stalled stream and a coalesced wake-up still covers every stored message. Because the single SQLite connection commits IDs in order, the stream emits messages in ascending ID order and a client may resume with `Last-Event-ID` (the last message ID it received) without gaps; without a cursor it receives every message still `pending` for the role. Only messages still `pending` for the recipient are emitted. Event data is a summary (`id`, `sender`, `recipient`, `subject`, `priority`, `created_at`, optional `task_id`/`thread_id`, and `body` only for urgent mail); priority is fixed to `normal` until messages store one. Keep-alive comments (`: ping`, `AGENT_MAIL_SSE_KEEPALIVE`, default 20s) keep idle connections open through reverse proxies; the HTTP server sets no write timeout, and shutdown (SIGINT/SIGTERM) ends every stream before the process exits. Subscribers are per process, which matches the single-writer SQLite deployment.

## Security boundary

Bearer tokens grant one role. Only that role can act as sender, and only configured role names may receive a message. Senders can inspect their own sent messages; only recipients can update state or reply. The dashboard token is held in browser local storage; use a trusted browser profile and private network. Deploy behind a private interface or private reverse proxy with TLS when traffic leaves a trusted host network.
