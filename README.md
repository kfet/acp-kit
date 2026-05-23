# acp-kit

Reusable Go packages for ACP-backed chat relays such as `poe-acp` and `slack-acp`.

Module:

```go
module github.com/kfet/acp-kit
```

Requires Go 1.25+ (uses `os.Root` sandboxing and the `tool` go.mod directive).

## Packages

- `client` — stdio ACP child process client: initialize, sessions, prompts, caps, model selection, auth hooks, fs callbacks.
- `client/auth` — small schema for ACP auth method/result metadata.
- `state` — conversation-key to ACP-session manager: stable cwd allocation, best-effort resume, idle GC, system-prompt fallback regime.
- `attachments` — cwd-local attachment sandbox plus ACP `ResourceLink` / embedded text resource blocks.
- `skills` — load embedded/host fir-style skills and format `<available_skills>` catalogs.
- `sysprompt` — compose base relay prompt, operator extra text, and skill catalogs.
- `paths` — XDG state/config path helpers.
- `log` — opt-in debug logging.

## Contracts worth preserving

- Idle GC drops only in-memory session bindings; it never removes conversation cwds.
- `state` uses two system-prompt regimes:
  - if the agent advertises `session.systemPrompt`, the prompt is sent via `session/new._meta`;
  - otherwise it is exposed once through `TakePendingSystemPrompt`, and re-armed after resume.
- `skills.LoadBuiltin` requires an app-specific prefix so different relays do not collide in `$TMPDIR`.
- `attachments.Store` writes through `os.Root`; hostile filenames cannot escape the per-message directory.
