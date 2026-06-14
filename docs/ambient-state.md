# Relay state: membership, checkpoints, abstain

`acp-kit/state.Manager` maps a relay's conversation key → an ACP session and a
stable per-conversation cwd, with idle GC. Relays (`poe-acp`, `slack-acp`) share
it instead of each reimplementing the conv→session lifecycle.

This note records four capabilities added to support **ambient participation** —
a relay where the agent is a continuous participant in a conversation/thread and
chooses, per message, whether to reply. The design originated in `slack-acp`
(see its `docs/ambient-threads.md`); these pieces are the generic substrate, so
`poe-acp` gets them for free.

## Why these live in acp-kit, not the relay

Each capability is transport-agnostic. The relay owns the *semantics* (what an
"event id" is, when to abstain); the Manager owns *persistence and lifecycle*.
Keeping them here avoids two near-identical copies drifting apart.

### 1. Membership — `Known(key)`

"Do we already have a session for this conversation?" — backed by the on-disk
cwd, not just the in-memory map. After a process restart the in-memory map is
empty but the cwd dirs survive, so disk is the source of truth for membership.

*Reasoning:* a relay that follows a conversation only after it's been engaged
(e.g. a Slack thread the bot was `@`-mentioned into) needs a restart-stable
"are we in this one?" check. Memory alone loses that across restarts; the cwd
already persists for session resume, so reuse it.

### 2. Per-conversation checkpoint

An opaque string persisted per key under the cwd — the relay's "last processed
external event id." The Manager stores and returns it; it never interprets it.

*Reasoning:* two unrelated needs collapse into one piece of state —
**deduplication** (transports with at-least-once delivery re-send; drop anything
`<=` the checkpoint) and **gap detection** (after an outage, the relay fetches
history since the checkpoint to backfill). Slack stores a message `ts`; another
transport stores whatever monotonic id it has. Opaque keeps it general.

### 3. Abstain sink

A `SessionUpdateSink` wrapper that buffers the agent's output and, if the
complete response equals a configured sentinel (e.g. `<<SILENT>>`) or is empty,
suppresses delivery entirely — the relay posts nothing.

*Reasoning:* in ambient participation the agent must be able to *decline*. The
sentinel is a cross-agent contract (works with any ACP agent, no tool plumbing).
The abstained message still flows through the session, so the agent stays caught
up on the conversation even when it says nothing. `poe-acp` has an analogous
"discard this response" notion (out-of-band reaction turns), so this is not
Slack-specific.

### 4. Conversation-keyed system prompt

Generalises `Config.SystemPrompt string` → a resolver `func(ConvKey) string`,
evaluated at session creation.

*Reasoning:* one bot, many conversations with different cultures — a Slack
`@ops` bot reads differently in `#incidents` vs `#random`; a poe bot may want
per-conversation persona. A single static string can't express that; a resolver
keyed on the conv can, while a constant resolver reproduces today's behaviour.

## What stays in the relay

Transport-specific concerns do **not** belong here:

- Slack `ts` semantics and `conversations.replies` backfill.
- Slack event subscriptions / scopes.
- Formatting contracts (mrkdwn, etc.).

The Manager provides the storage and lifecycle hooks; the relay supplies the
meaning.

## Principle

Config sets priors, persona, cost, and reach. **Whether to reply to a given
message is the agent's judgement, never config.** These four pieces exist to give
the agent enough continuity (membership, no-gap context) and enough agency (a way
to decline) to make that judgement well.
