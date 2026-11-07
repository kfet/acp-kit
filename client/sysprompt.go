package client

// Client-provided system prompt.
//
// ACP v1 stabilized the client-provided system prompt on 2026-11-06: an
// agent advertises agentCapabilities.sessionCapabilities.systemPrompt (a
// non-null object, optionally with mode.override), and the client then
// sends params.systemPrompt = {mode, content} on session/new and re-sends
// the same object on session/load, session/resume and session/fork.
//
// The SDK has no types for this yet; see sysprompt_sdkshim.go.
//
// Before the stable capability, acp-kit used its own extension: the
// "session.systemPrompt" key in clientCapabilities._meta and
// agentCapabilities._meta, with the prompt in session/new
// _meta["session.systemPrompt"].blocks. That path is kept as a fallback for
// agents that only advertise the old key. It is DEPRECATED and will be
// removed in the release after the one that introduced the stable path.

import (
	acp "github.com/coder/acp-go-sdk"
)

// SystemPromptVia names how the agent accepts a client system prompt.
type SystemPromptVia string

const (
	// SystemPromptNone: the agent advertised neither path. A consumer
	// that wants the prompt to reach the model must inline it into user
	// content (see state.Manager.TakePendingSystemPrompt).
	SystemPromptNone SystemPromptVia = ""
	// SystemPromptStable: the agent advertised the stable ACP capability
	// agentCapabilities.sessionCapabilities.systemPrompt. The prompt is
	// sent as params.systemPrompt on session/new and re-sent on
	// session/load, session/resume and session/fork.
	SystemPromptStable SystemPromptVia = "stable"
	// SystemPromptLegacyMeta: the agent advertised only the acp-kit
	// extension agentCapabilities._meta["session.systemPrompt"]. The
	// prompt is sent once, in session/new _meta.
	//
	// Deprecated: kept for one release for agents that predate the
	// stable capability.
	SystemPromptLegacyMeta SystemPromptVia = "legacy-meta"
)

// legacySystemPromptKey is the acp-kit extension key for the pre-stable
// system prompt, in clientCapabilities._meta, agentCapabilities._meta and
// session/new _meta.
//
// Deprecated: see SystemPromptLegacyMeta.
const legacySystemPromptKey = "session.systemPrompt"

// systemPromptModeAppend is the only mode acp-kit sends. Every agent that
// advertises the capability must support it; override is opt-in and
// reported via Caps.SystemPromptOverride for consumers that need to know.
const systemPromptModeAppend = "append"

// stableSystemPrompt returns params.systemPrompt for blocks: non-nil only
// when the agent advertised the stable capability and blocks is non-empty
// (the spec forbids an empty content array).
func stableSystemPrompt(c Caps, blocks []acp.ContentBlock) *systemPromptParam {
	if c.SystemPromptVia != SystemPromptStable || len(blocks) == 0 {
		return nil
	}
	return &systemPromptParam{Mode: systemPromptModeAppend, Content: blocks}
}

// withLegacySystemPrompt returns meta plus the deprecated
// _meta["session.systemPrompt"].blocks entry, for session/new only. It
// applies whenever the agent did not advertise the stable capability —
// both for legacy-key agents and, as before the stable path existed, for
// agents advertising nothing (they ignore the unknown key). meta is not
// mutated. nil blocks leaves meta as is.
//
// Deprecated: remove with SystemPromptLegacyMeta.
func withLegacySystemPrompt(c Caps, meta map[string]any, blocks []acp.ContentBlock) map[string]any {
	if blocks == nil || c.SystemPromptVia == SystemPromptStable {
		return meta
	}
	out := make(map[string]any, len(meta)+1)
	for k, v := range meta {
		out[k] = v
	}
	out[legacySystemPromptKey] = map[string]any{"blocks": blocks}
	return out
}
