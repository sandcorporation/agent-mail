# Repository guidance

- Keep role identities independent of agent session or terminal handles.
- Do not log bearer tokens or put credentials in source, images, or committed configuration.
- Preserve message durability, idempotent sends, and non-consuming inbox/watch behavior.
- Render user-provided content as text; do not inject it as HTML.
- Run `go test ./...` for Go changes. GitHub Actions is the CI and release system.
- Keep deployment private and avoid modifying unrelated services.
