# Repository guidance

- Keep role identities independent of agent session or terminal handles.
- Do not log bearer tokens or put credentials in source, images, or committed configuration.
- Preserve message durability, idempotent sends, and non-consuming inbox/watch behavior.
- Render user-provided content as text; do not inject it as HTML.
- Run `go test ./...` for Go changes. GitHub Actions is the CI and release system.
- Keep deployment private and avoid modifying unrelated services.

## Package map

- `cmd/switchboard` holds `main` only. The server is assembled in `internal/server` (settings, router, role auth, admin guard, sweep).
- Domain packages: `identity` (roles, slots, groups, invites), `adminauth`, `mail`, `presence`, `asks` (questions, decisions, permissions), `insight` (metrics, alerts, Discord), `dashboard` (admin page, collaborators view). Leaves: `cli`, `wire`, `store`, `config`, `live` (the dashboard change bus).
- Domain packages never import `server` or `dashboard`. Pass cross-domain effects in as functions instead of importing upward. Read env and header names through `internal/config`; do not hard-code a prefix.
- Full table and import rules: ARCHITECTURE.md, "Package map".
