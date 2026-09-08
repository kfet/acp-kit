# Changelog

All notable changes to acp-kit will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html)
once it leaves v0.

## [Unreleased]

## [0.16.3] - 2026-09-09

### Fixed

- `client.TurnLiveness`: the no-progress timer could lose a race it
  should have won — it fires, then a progress update lands before the
  callback takes the lock — and report a working turn as wedged.
  Re-checked against the last progress instant, so such a turn is given
  the remainder of its window instead of being cut.

## [0.16.2] - 2026-09-09

### Fixed

- `client.AgentProc.Prompt` now sends `session/cancel` when its context
  is cancelled. Abandoning the JSON-RPC request only stopped the CLIENT
  waiting — the agent never heard about it and its in-flight tool kept
  running. Measured in the wild: a relay gave up on a turn at 10m00s and
  the agent went on writing files for another minute. Relays that
  cancelled explicitly (on a superseding message) were unaffected; every
  timeout path was not.

## [0.16.1] - 2026-09-09

### Changed

- `client.TurnLiveness`: the optional `MaxTurnDuration` ceiling is now a
  real context deadline rather than a second timer. It needs no reset,
  its cause still propagates, and — unlike a bare cancel — the cap is
  visible to anything downstream that asks `ctx.Deadline()`.

## [0.16.0] - 2026-09-09

### Added

- `client.StartTurnLiveness` / `client.TurnLiveness`: bound a prompt turn
  by PROGRESS instead of wall-clock. `Wrap` decorates the session's
  `SessionUpdateSink`; agent message/thought chunks and
  `tool_call` / `tool_call_update` reset a no-progress clock, everything
  else (plans, available commands, mode changes — the frames a wedged
  agent's harness keeps emitting) does not. The returned context is
  cancelled with cause `ErrNoProgress`, or `ErrTurnCeiling` for the
  OPT-IN, off-by-default absolute cap `MaxTurnDuration`; the returned stop
  func cancels with a plain `context.Canceled`, so a relay can tell a
  wedged turn from a superseded one and say so.

  This replaces the plain `context.WithTimeout(parent, 10m)` every relay
  had. A wall-clock cap punishes exactly the turns working hardest: a
  zulip-acp turn was killed at 10m00s while its agent was mid-tool-call,
  and the tool went on writing files a minute after the relay gave up.
  The classifier is exported as `client.IsProgress` so consumers do not
  reimplement it. Named for the condition it detects, NOT "idle" —
  `state.Config.IdleTimeout` is session GC and unrelated.

## [0.15.0] - 2026-09-08

### Added

- `client.Caps.Image` and `client.Caps.Audio`: the ACP
  `agentCapabilities.promptCapabilities.image` / `.audio` flags, parsed
  at initialize alongside `embeddedContext`. A relay that ingests an
  inbound attachment needs to know whether it may put an image in front
  of the model as a `ContentBlock::Image` or must fall back to naming
  the file on disk; without this the answer was unreachable and every
  relay would have had to re-parse the initialize envelope itself.

### Fixed

- `schedule`: the "not yet due" branch of `Store.due` was covered only
  by map-iteration luck, so the 100% coverage gate failed at random.
  Driven deterministically now.

## [0.14.0] - 2026-09-08

### Added

- `skills.LoadBuiltinIn`: extract an embedded bundle into a directory the
  APP owns instead of `$TMPDIR`. A relay hands the agent absolute skill
  paths and promises they are stable for the session; a relay that
  re-execs itself in place on a self-update broke that promise, because
  the extraction root was keyed to the process's temp directory — and
  nothing ever removed the old roots, so they accumulated one per
  released version (784 leaked directories on one live host). When a base
  is supplied, stale generations of the same app are garbage collected,
  in the new location and in the legacy `$TMPDIR` one. `LoadBuiltin` is
  unchanged for callers that pass nothing.

## [0.13.0] - 2026-09-08

### Fixed

- `mcphost`: an agent silently lost **every** loopback tool for the rest of its
  session whenever the consumer re-execed itself in place. Observed live: a
  relay self-updated mid-session and the agent's `mcp__relay__*` calls returned
  "tool not found" from then on, so a promise it had made to schedule a
  follow-up had no mechanism behind it. Four independent causes, all fixed:
  the socket path was a fresh `MkdirTemp` on every process start; `Close`
  unlinked the socket and removed its directory; tokens were minted in memory,
  so the successor rejected the token the agent's redirector already held; and
  the redirector was a one-shot pipe that exited when the socket went away.
- `mcphost`: `Close` no longer hangs. Closing the listener left accepted
  connections open, and an attached redirector holds its connection open
  indefinitely, so `wg.Wait` never returned. Shutdown now drops live
  connections.

### Added

- `mcphost.Config.Dir` pins the socket to a caller-supplied fixed directory
  instead of a fresh `MkdirTemp` one. The directory is created if missing and
  tightened to 0700, and is never removed by `Close`.
- `mcphost.Host.ExportTokens` / `SeedTokens` carry the sessionKey→token
  registry across an exec as an opaque env-safe blob. Environment only —
  deliberately never disk, which would create a new secret at rest.
- `mcphost.Host.CloseForExec` shuts down without unlinking the socket, for the
  reload path; `Close` remains the real-shutdown path and cleans up.
- The redirector is now a thin **reconnecting** proxy: it redials on a 50ms→2s
  schedule, gives up after ~30s and then exits (so the agent sees a dead server
  rather than a hung one), and **replays `initialize`** on each new connection
  because the host's MCP state is per-connection. A request in flight when the
  connection dropped is failed back with a JSON-RPC error rather than replayed,
  so a `tools/call` side effect is never duplicated. The agent sees exactly one
  answer per request id: a response the proxy is no longer waiting for — the
  host answered, then died before the write side noticed — is dropped rather
  than delivered alongside the error already sent for that id.
- `mcphost.Listen` refuses to bind over a socket a live process is still
  serving, while still clearing a genuinely stale one.

All of the above is additive: consumers that do not set `Config.Dir` are
unaffected.

## [0.12.3] - 2026-09-04

### Fixed

- `remotefs`: doc comments named exported fields (`SSH.Timeout`,
  `SSH.TransferTimeout`) that do not exist, and described `Local` as
  three no-ops when `Fetch` is the identity.

## [0.12.2] - 2026-09-04

### Fixed

- `remotefs`: a timed-out transfer named the control-operation deadline
  ("timed out after 30s") instead of its own.
- `remotefs`: when the second process fails to start, the pipe's read end
  is released before waiting on the first. The writer was left blocking
  on a full pipe buffer until the transfer deadline instead of taking an
  immediate EPIPE — invisible for a small payload, minutes for a real one.

## [0.12.1] - 2026-09-04

### Fixed

- `remotefs`: a transfer no longer shares a `mkdir`'s deadline — `Push`
  and `Fetch` are bounded by `DefaultTransferTimeout` (5m, settable with
  `WithTransferTimeout`), because a hundred megabytes over a domestic
  uplink is minutes and 30s was failing healthy copies.
- `remotefs`: when both ends fail, the local tar is quoted alongside the
  remote's message. A local tar dying mid-stream makes the REMOTE one
  fail on a truncated archive, and reporting only the far side blamed
  the wrong machine.
- `remotefs.Fetch`: follows symlinks on the remote (`tar -h`) — an agent
  that wrote through a link produced a dangling one here — and cleans
  the path before splitting it.

## [0.12.0] - 2026-09-04

### Added

- `remotefs`: provision relay-side paths on the host where the ACP agent
  actually runs. A relay that spawns its agent over ssh
  (`--agent-cmd "ssh -T box fir --mode acp"`) hands the agent absolute
  paths — the session cwd and any staged prompt files — that exist only
  on the relay's own disk; the agent takes `cwd` as-is with no stat and
  no mkdir, so it silently falls back to `$HOME` and every staged file is
  missing, with nothing anywhere reporting an error. `remotefs.New(host)`
  returns a `Provisioner` whose `Mkdir` and `Push` make those paths real
  over there first; `remotefs.Local` is the no-op for a local agent and
  the correct value when no remote is configured. ssh runs in BatchMode
  with a bounded timeout, is invoked as argv (no local shell), and quotes
  every path crossing the remote login shell. `Push` streams a tar over
  ssh rather than using scp, whose remote-path handling changed with
  OpenSSH 9 such that no single quoting discipline is correct for both.
  `Fetch` is the other direction — a file the AGENT produced, on its
  disk, made readable here — because an agent that writes an attachment
  for the relay to upload writes it over there.

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
