# Changelog

All notable changes to acp-kit will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html)
once it leaves v0.

## [Unreleased]

## [0.3.0] - 2026-07-06

### Added

- Add `mcphost`: generic session-scoped MCP-over-unix-socket host with
  per-session token auth (extracted from poe-acp).
- New `mcphost` package: a generic, self-hosted MCP server that a consumer
  advertises to an ACP agent as a stdio MCP server. It owns a per-process
  unix socket (private 0700 dir, 0600 sock, stale cleanup), a token →
  session-key registry (server-side, unspoofable), the MCP JSON-RPC loop,
  and dispatch to consumer-registered tools via `Host.Tool`. Includes the
  dumb redirector entrypoint (`RunRedir` / `MaybeRunRedir`) with a preamble
  framing identical to poe-acp's original `mcpattach`. Zero consumer-specific
  logic; tool names/schemas/behaviour are supplied by the consumer. Extracted
  from poe-acp for reuse across relays.

## [0.2.6] - 2026-06-14

### Added

- Ambient-participation substrate in `state.Manager` (originated in slack-acp's
  ambient-threads design; `poe-acp` gets these for free):
  - `Known(key)` — restart-stable membership check, backed by the on-disk cwd
    (the in-memory map is empty after a restart but cwd dirs survive). Default
    cwd layout only; memory-only when a custom `CwdFor` is set.
  - `Checkpoint(key)` / `SetCheckpoint(key, value)` — an opaque per-conversation
    string persisted under the cwd (atomic write-temp+rename). The relay owns
    its meaning (e.g. last-processed external event id) for dedup + gap
    detection; the Manager never interprets it.
  - `Config.SystemPromptForKey func(key string) string` — conversation-keyed
    system-prompt resolver, evaluated at session creation; takes precedence over
    `SystemPromptProvider`/`SystemPrompt`. Enables per-conversation persona.
- `client.PromptAbstainable` + `AbstainResult` — generic *decline* construct on
  top of `ValidatingSink`: runs a prompt and, if the complete message (trimmed)
  equals a sentinel or is empty, suppresses delivery entirely (posts nothing);
  otherwise flushes downstream. The prompt always reaches the session, so the
  agent stays caught up even when it stays silent. Transport-agnostic; no tool
  plumbing.

## [0.2.5] - 2026-06-14

### Added

- `client.Config.MCPServersForSession func(cwd string) []acp.McpServer` — an
  optional hook letting a client supply per-session MCP servers in
  `session/new` and `session/resume` (e.g. a client-hosted stdio tool server),
  without changing the `NewSession` signature. Nil (default) preserves the
  previous behavior (empty MCP server list).

## [0.2.4] - 2026-06-12

### Added

- Generic *refuse-LLM-output* construct in `client`: `ValidatingSink`, `PromptValidated`, `Validator`/`ValidatorFunc`, `RefuseConfig`, `RefuseResult`. A transport-agnostic way for an ACP client to validate an agent's complete visible message and, on refusal, re-prompt the agent with a short reason to regenerate — before the user ever sees it. `ValidatingSink` buffers `AgentMessageChunk` updates (thoughts/tool-calls/plans still stream live) so rejected output is never delivered; `PromptValidated` runs the refuse/regenerate loop with a `MaxRefusals` cap and an optional deterministic `Fallback` transform so a turn can never wedge against a stubborn model.

## [0.2.3] - 2026-06-07

### Added

- `AgentProc.ReleaseSession(ctx, sid)` — issues the `session/release` ACP RPC so a relay can explicitly tear down an in-memory agent session (freeing its extension/MCP subprocesses) without killing the whole agent process. Plus `SessionNotFoundCode` (-32001, the shared agent/relay contract code) and `IsSessionNotFound(err)` helper so relays can detect a released/reaped session — returned by the agent from prompt/release/etc. on an unknown session — and transparently re-create it.

## [0.2.2] - 2026-06-02

### Added

- `AgentProc.AvailableCommands() []CommandInfo` — snapshot of the agent's advertised slash-command catalog, captured from `availableCommandsUpdate` session notifications as they arrive (consistent with how `Models`/`Caps`/`AuthMethods` are snapshotted). Lets a relay enumerate and validate agent commands (e.g. to expose `/reload` as a chat command) without re-implementing the notification plumbing. New `CommandInfo{Name, Description}` type.

## [0.2.1] - 2026-05-28

### Added

- `terminal.State.TakePending(toolCallID)` — atomic check-and-remove of a pending terminal, so agents can tell on a tool-end event whether the client terminal already rendered the output and skip emitting duplicate text.

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
