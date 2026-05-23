# acp-kit agent notes

- Keep packages narrow. No `util` / `common` / `internal` grab bags.
- Do not add a permission-policy package — kit defaults to allow-all and consumers can pass their own `PermissionFunc`.
- This repo is local-only until a remote is explicitly created.
- Always run `make` before reporting completion. It enforces 100% coverage via covgate (see `.covignore`).
- Unreachable error branches go into `_must.go` panic helpers with a doc comment explaining why, not into line-level coverage exclusions.
