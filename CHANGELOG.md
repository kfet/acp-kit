# Changelog

All notable changes to acp-kit will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html)
once it leaves v0.

## [Unreleased]

## [0.2.0] - 2026-05-28

### Added

- `terminal` package — agent-side ACP terminal driver. Exports a narrow `Conn`
  interface (the terminal subset of the agent-side connection), a `State` that
  tracks one session's foreground and background terminals (safe for concurrent
  use), and operations `Exec` (foreground with optional timeout), `StartBackground`
  / `BackgroundOutput` / `KillBackground`, and `CleanupPending` / `CleanupBackground`.
  Foreground timeouts surface as `*TimeoutError` and context cancellation as
  `ErrAborted`. Extracted from fir's ACP mode so any ACP agent can delegate shell
  execution to a terminal-capable client. 100% covered.

## [0.1.4] - 2026-05-27

### Added

- `statusline` package — wire contract for the `dev.acp-kit.status-line/v1` ACP extension. Exports `ExtensionID`, `MaxFieldRunes`, `Status`, `ProviderEmoji` / `ProviderEmojiForModel`, `ParseMeta`, `Segments`, and `CapRunes`. Relay-specific renderers (markdown vs Slack mrkdwn, animated vs static) stay in each consumer; this is just the shared core. Replaces the duplicated `internal/statusline` packages in poe-acp and slack-acp; both should now import `github.com/kfet/acp-kit/statusline` and keep only their local Header/Spinner.

### Changed

- Doc comments and tests in `client` now reference the new `dev.acp-kit.status-line/v1` extension id (was `dev.poe-acp.status-line/v1`). The old id is dead — consumers must rename.

## [0.1.3] - 2026-05-27

### Fixed
- `client.AgentProc.ResumeSession` now decodes the response into
  `acp.ResumeSessionResponse` (was `json.RawMessage`) and caches
  `resp.Models` into the agent's model state under the lock,
  mirroring `NewSession`. Previously, `Models()` returned empty data
  on resumed sessions even though the agent sent a full model list.

## [0.1.2] - 2026-05-26

### Added
- `client.Config.ClientMeta` — extra entries merged into outgoing
  `clientCapabilities._meta` at Initialize. Lets consumers advertise
  support for custom ACP extensions (e.g.
  `dev.poe-acp.status-line/v1`) without forking the handshake.
- `client.Caps.Extensions` — parsed agent-side `_meta` entries from
  `agentCapabilities._meta`, with the kit-owned `session.systemPrompt`
  key filtered out (still surfaced via `Caps.SystemPrompt`). Lets
  consumers probe for advertised extensions by key.

### Changed
- `client.Caps` is now uncomparable (contains a map field). Callers
  using `== Caps{}` must switch to field-by-field checks. No effect
  on struct-literal construction. `poe-acp` and `slack-acp` only
  construct `Caps`; the only equality call site was the kit's own
  `TestParseHelpersIgnoreGarbage`, updated in this release.

## [0.1.1] - 2026-05-24

### Added
- `client.ReadOnlyPermissions` and `client.DenyAllPermissions` — built-in
  policies promoted from `poe-acp`. (The published v0.1.0 tag was cut before
  the implementation landed; v0.1.1 is the first tag that actually carries
  this code.)

### Changed
- Internal cleanup: collapsed the per-call-site `must*` panic helpers in
  `client` and `attachments` into a single `mustNot(err, label)` per
  package. No public API impact.

### Removed
- `client/auth` sub-package. The `AuthMethod` and `AuthResult` types it
  defined are now declared directly in the `client` package; the names
  consumers used (`client.AuthMethod`, `client.AuthResult`) are unchanged.
  Direct importers of `github.com/kfet/acp-kit/client/auth` would break,
  but neither `poe-acp` nor `slack-acp` imported it.

## [0.1.0] - 2026-05-22

### Added
- Initial extraction of shared ACP relay primitives from `poe-acp` and `slack-acp`.
- `client` — stdio ACP child process client (`Start`, sessions, prompts, caps, model selection, auth, fs callbacks). Built-in permission policies: `AllowAllPermissions`, `ReadOnlyPermissions` (heuristic — rejects titles containing write/edit/bash/exec/run/delete/rm), `DenyAllPermissions`.
- `client/auth` — auth method/result schema.
- `state` — conversation-key → ACP-session manager with stable cwd allocation, best-effort resume, idle GC, and two-regime system-prompt fallback.
- `attachments` — `os.Root`-sandboxed attachment store + ACP `ResourceLink` / embedded text `Resource` block builders.
- `skills` — embedded + host fir-style skill loader and `<available_skills>` catalog formatter.
- `sysprompt` — base/extra/catalog composer with disabled toggle.
- `paths` — XDG state/config path resolvers.
- `log` — opt-in `atomic.Bool` debug logger.
- `Makefile`, `.covignore`, and `make`-driven 100% coverage gate via covgate. `make` runs `fmt`, `tidy`, `vet`, race+cover, e2e, and license check.
