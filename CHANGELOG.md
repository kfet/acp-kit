# Changelog

All notable changes to acp-kit will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html)
once it leaves v0.

## [Unreleased]

## [0.10.0] - 2026-09-02

### Added

- `statusline.Status.Model` and `statusline.ShortModelName`: the compact
  model label shown next to the provider emoji. `ShortModelName` derives it
  from a fully qualified `<provider>/<model>` id by dropping the provider
  prefix, trailing `-YYYYMMDD` / `-latest` / `-preview` decorations and a
  leading vendor echo already carried by the emoji (`claude-`,
  `anthropic-`), rewriting version dashes between digits as dots, then
  lowercasing and capping to `MaxFieldRunes` — e.g.
  `anthropic/claude-opus-4-5-20251001` → `opus-4.5`. Meaningful family
  prefixes (`gpt-`, `gemini-`, `grok-`, `llama-`, `deepseek-`) are kept.

### Changed

- `statusline.Segments` now emits the provider emoji and model name as ONE
  space-joined segment (`🏛️ opus-4.5`) rather than the emoji alone, so a
  relay joining with `" • "` renders `🏛️ opus-4.5 • steady • 2/5`. Either
  half alone degrades to just that half; a `Status` with no `Model` renders
  exactly as before.

## [0.9.1] - 2026-09-01

### Fixed

- `command.Scheduler` gains `CanSchedule()`, and `Broker.scheduler` is now the
  single gate for the whole scheduling surface — `!help`, the commands,
  `!status` and the MCP tools. A relay that implements `Scheduler` but has the
  feature switched off (zulip-acp without `relay_mcp`) no longer advertises
  commands nothing can serve.

## [0.9.0] - 2026-09-01

### Added

- **`relaytool`: the agent→relay loopback.** Exposes a relay's own bot
  interface to the ACP agent as self-hosted MCP tools, so the agent can drive
  the relay from inside a turn. ACP has no agent-initiated message and a
  relay's streaming sink is bound per turn, but an MCP tool call runs
  agent→client — so the loopback is the correct mechanism and needs no
  protocol extension. Tools: `status`, `list_models`, `set_model`,
  `new_session`, and (capability-gated) `post`, `schedule`, `list_schedules`,
  `unschedule`.
- **`schedule`: durable, conversation-scoped scheduled prompts.** A prompt
  armed here re-enters the conversation it was created in — same conv, same
  session, full history — and its answer streams back through the relay's
  ordinary path. Host cron cannot do that, which is the whole reason this
  exists; host chores stay in host cron. Runaway control is first-class and on
  by default: `MaxDepth` bounds a schedule→turn→schedule chain (so every chain
  terminates), `MaxPerConv` / `MaxTotal` bound breadth, `MinInterval` floors a
  repeat, and a missed window is skipped rather than replayed.
- `command.Scheduler.CanSchedule()`: a type assertion can only say a relay
  *could* schedule, not that the operator switched it on. Everything —
  `!help`, the commands, `!status` and the MCP tools — gates on this one
  answer, so a relay with scheduling implemented but disabled advertises
  nothing.
- `command.Poster` and `command.Scheduler`: optional `Controller`
  capabilities, in the same shape as `TurnStopper`. A relay that answers one
  HTTP request per turn (poe-acp) cannot speak out of band, so it implements
  neither and the corresponding tools are never advertised.
- `command`: `!schedules` and `!unschedule <id>`, so a human can see and kill
  what the agent has armed. Registered only when the `Controller` implements
  `Scheduler`.
- `command`: the session controls are now exported **actions** — `Status`,
  `ModelList`, `SelectModel`, `NewSession`, `Post`, `Schedule`,
  `ScheduleList`, `Unschedule` — and the `!` handlers are thin renderers over
  them. This is what lets the chat surface and the MCP surface be one
  implementation with two front ends rather than two implementations that
  drift.

### Notes on what is deliberately absent

- **No `stop` tool.** An agent cancelling its own in-flight turn either does
  nothing or kills the very turn whose tool call asked for it, leaving the
  result undeliverable. There is no coherent reading, so there is no tool.
- **`new_session` is deferred**, not immediate: resetting a session cancels
  the turn in flight, which is the same foot-gun. The tool records the intent
  and the relay applies it via `Tools.EndTurn` once the turn is over — same
  `Controller`, same implementation, honest timing.
- **`post` has no target parameter.** The conversation comes from the
  mcphost session key, bound server-side from the connection token. An agent
  that could post into arbitrary channels would be a realm-wide megaphone for
  anything that can prompt-inject it, so in v1 that is not a config toggle —
  it is not expressible.

## [0.8.0] - 2026-09-01

### Added

- **`command`: the shared relay chat-command surface**, promoted from
  `poe-acp/internal/command` so `poe-acp` and `zulip-acp` stop carrying two
  copies of the same 650-line broker. Covers the `!login` family and its
  two-call `_meta.auth.interactive` bridge, `!help` / `!status` /
  `!model [filter|id]` / `!new`, the undocumented back-compat aliases
  (`!models`, `!relay`, `!bot`, `!whoami`, `!reset`, `!cancel-login`), and the
  curated agent-command passthrough allowlist. Sigils `/`, `!` and `.` are
  accepted on input; `!` is the `DisplaySigil`. Rendering stays in this
  package: Poe, Zulip and Slack all read CommonMark-ish markdown and the
  strings are legal in all three verbatim.
- `command.TurnStopper`: an optional Controller capability enabling `!stop`.
  A Controller that does not implement it leaves `!stop` unrecognised, so the
  text forwards to the agent unchanged — only a relay that streams a turn has
  something to interrupt. Deliberately **not** spelled `!cancel`, which
  `!login cancel` / `!cancel-login` already own.
- `command.SessionStatus` gains optional `ConvID`, `StateDir`, `Where` and
  `TurnRunning` fields for relays that give a conversation its own identity,
  directory and place. The renderer prints only what is set, so a controller
  that leaves them empty produces the same output as before.
- `command.StripSigil`, exported for relays that must pre-filter by surface
  before the broker sees a message (zulip-acp passes Zulip's `/me`, `/poll`
  and `/todo` through untouched).

### Changed

- `command` names are matched case-insensitively on the verb only; the
  argument keeps its case, since a model id is a literal the agent must match.
- `IsCommand` is a `*Broker` method rather than a package function: whether
  `!stop` is a command depends on the wired Controller.

## [0.7.0] - 2026-08-29

### Added

- `(*client.AgentProc).Done()` / `.Err()`: agent-process liveness. A single
  reaper goroutine owns `cmd.Wait`, publishes the classified exit
  (`ErrAgentClosed` for a deliberate `Close`, `ErrAgentExited` for a clean
  self-exit, the `*exec.ExitError` otherwise) and closes `Done`. Relays can
  now notice an agent that died out from under them instead of failing every
  turn with `broken pipe`.

### Fixed

- `(*client.AgentProc).Close` no longer starts a second `cmd.Wait` racing the
  reaper; it consumes the reaper's result via `Done`.
- A child that dies during the ACP handshake is now reaped instead of leaking
  a zombie: the reaper starts as soon as the process does.

## [0.6.0] - 2026-08-28

### Added

- `client.AgentProc.NewSessionWithMeta`: create a session with extra `_meta`
  entries on the `session/new` request, for create-time placement/routing
  hints an agent understands (e.g. a `host` entry telling a
  tmux-multiplexing agent which SSH host to run the session's pane on).
  `NewSession` delegates to it with no extra entries, so the wire shape is
  unchanged for existing callers: `_meta` stays absent unless system-prompt
  blocks or extra entries are supplied. The reserved `session.systemPrompt`
  key remains owned by `systemPromptBlocks` and wins over a same-named
  extra entry.

## [0.5.0] - 2026-08-27

### Added

- `statusline.MaxTrailingFieldRunes` (36): a wider cap for the LAST status-line
  segment (the live activity/tool label). Earlier fields stay at
  `MaxFieldRunes` (12) so the header keeps its mobile-safe width.

## [0.4.0] - 2026-08-02

### Added

- `client.Config.SecretEnvNames` and `client.Config.Secrets`: declare the
  relay's own secrets so `Start` scrubs them from the spawned agent's
  environment before the child ever runs. `SecretEnvNames` drops by variable
  name; `Secrets` drops by literal value whatever the variable is named
  (empty strings ignored). The mechanism lives next to the `Env` footgun it
  closes: when `Env` is nil ("inherit `os.Environ()`") and any secret is
  declared, `Start` now materialises, scrubs, and assigns `cmd.Env`
  explicitly — leaving it nil would inherit the full environment *including*
  the secrets, a silent no-op in exactly the case that matters most. Only
  explicitly-declared names/values are dropped; provider credentials the
  agent legitimately needs (e.g. `ANTHROPIC_API_KEY`, `POE_API_KEY`) are
  untouched. Backwards compatible: a caller that sets neither field gets
  today's behaviour exactly (nil `Env` stays nil, non-nil `Env` passes
  through unchanged).

### Changed

- Bump `github.com/coder/acp-go-sdk` v0.12.2 → v0.13.5 and
  `github.com/kfet/covgate` v0.1.0 → v0.1.2.
- Model handling now speaks both generations of the ACP wire protocol. The
  SDK's v0.13 release removed the unstable per-session model API
  (`SessionModelState`, `session/set_model`) in favour of generic session
  config options. `client` prefers the new style — the `session/new` /
  `session/resume` `configOptions` entry whose category is `"model"`, set
  via `session/set_config_option` — and falls back to the old `"models"`
  object plus the `session/set_model` RPC for older agents (e.g. `fir`
  pinned to acp-go-sdk v0.6.3). The public API (`client.ModelInfo`,
  `AgentProc.Models`, `AgentProc.SetModel`, `AgentProc.ProbeModels`) is
  unchanged.

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
