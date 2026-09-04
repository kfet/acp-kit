# acp-kit

[![CI](https://github.com/kfet/acp-kit/actions/workflows/ci.yml/badge.svg)](https://github.com/kfet/acp-kit/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/kfet/acp-kit.svg)](https://pkg.go.dev/github.com/kfet/acp-kit)
[![Go Report Card](https://goreportcard.com/badge/github.com/kfet/acp-kit)](https://goreportcard.com/report/github.com/kfet/acp-kit)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

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
- `command` — the shared relay chat-command surface: the `!login` family and its two-call interactive-auth bridge, plus `!help` / `!status` / `!model` / `!new` / `!stop` / `!schedules` over a relay-supplied `Controller`. All session controls are implemented once as exported *actions*, so the `!command` a human types and the MCP tool an agent calls run the same code.
- `mcphost` — generic self-hosted MCP server: unix socket, dumb redirector subprocess, per-session token auth, MCP JSON-RPC loop. Zero consumer-specific logic.
- `relaytool` — the **agent→relay loopback**: exposes the relay's own bot interface to the agent as MCP tools (`status`, `list_models`, `set_model`, `new_session`, `post`, `schedule`, `list_schedules`, `unschedule`) over `mcphost` + `command`.
- `schedule` — durable, conversation-scoped scheduled prompts with depth, breadth and rate bounds, for relays that can inject a prompt out of band.
- `statusline` — wire contract for the `dev.acp-kit.status-line/v1` ACP extension: mood/plan payload that agents emit on `session/update._meta`, plus provider-emoji and short-model-name derivation, so relays can render a compact `🏛️ opus-4.5 • steady • 2/5` status line.
- `terminal` — agent-side ACP terminal driver: foreground exec with timeout, a bounded pool of background commands, and leak cleanup, over a narrow `Conn` interface.
- `sysprompt` — compose base relay prompt, operator extra text, and skill catalogs.
- `remotefs` — make relay-side paths (session cwd, staged prompt files) exist on the host where the agent actually runs, for relays whose agent is reached over ssh. `Fetch` brings a file the agent produced back the other way. `Local` is the no-op/identity for a local agent.
- `paths` — XDG state/config path helpers.
- `log` — opt-in debug logging.

## Adopting the loopback (`relaytool`)

A relay needs four things, in this order:

1. A `mcphost.Host` created **before** the agent starts, with
   `client.Config.MCPServersForSession` returning
   `host.ServerConfigForSession(<conversation key>)`, and
   `mcphost.MaybeRunRedir` intercepted at the top of `main`.
2. `relaytool.New{Broker, ConvToken}`, where `ConvToken` maps the mcphost
   session key to whatever opaque token the relay hands the broker. Pass nil
   when they are the same string.
3. `tools.Register(host)` **after** the `Controller` is wired, because the
   capability-dependent tools are chosen by asking the broker what the
   Controller can do.
4. `tools.EndTurn(convToken)` once per completed turn, after the turn is no
   longer in flight. Without it a deferred `new_session` never applies.

Implement `command.Poster` and/or `command.Scheduler` on the Controller to get
`post` and the scheduling tools; implement neither and they are simply not
advertised. `poe-acp` implements neither — it answers one HTTP request per
turn and has nothing to speak on afterwards — and gets the read/steer subset.
`zulip-acp` implements both.

## Contracts worth preserving

- Idle GC drops only in-memory session bindings; it never removes conversation cwds.
- `state` uses two system-prompt regimes:
  - if the agent advertises `session.systemPrompt`, the prompt is sent via `session/new._meta`;
  - otherwise it is exposed once through `TakePendingSystemPrompt`, and re-armed after resume.
- `skills.LoadBuiltin` requires an app-specific prefix so different relays do not collide in `$TMPDIR`.
- `attachments.Store` writes through `os.Root`; hostile filenames cannot escape the per-message directory.
- `remotefs` exists because an agent command line is opaque and the ACP handshake never reports the agent's host: remoteness must be operator configuration. A remote agent that receives a nonexistent `cwd` does not fail — it falls back to `$HOME` — so provisioning failures must be surfaced loudly by the caller rather than fallen through.
- `mcphost` binds a tool call's session key **server-side from the connection token**. `relaytool` therefore never accepts a conversation as a tool argument — every loopback tool acts on the conversation the call came from, and only that one. Do not add a `target` parameter without a threat model.
- A loopback tool must never destroy the turn that is calling it. That is why `relaytool` exposes no `stop`, and why `new_session` is deferred to `Tools.EndTurn`.
