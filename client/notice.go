// Notices: out-of-band operational messages from the agent.
package client

import (
	"context"
	"encoding/json"

	acp "github.com/coder/acp-go-sdk"
)

// NoticeMethod is the ACP extension method carrying agent notices.
//
// It exists because the alternative does not work. An agent that reports
// operational events — a provider rate-limit retry, a failed compaction — as
// agent message text puts them on the SAME ordered stream as the model's
// answer tokens, byte-identical and indistinguishable. A notice emitted by a
// background goroutine then lands wherever the stream happens to be, which in
// practice means mid-sentence, inside the user's reply. No separator or
// formatting can fix that: two independent writers share one stream.
//
// A distinct JSON-RPC method breaks the tie. The relay can route notices to a
// status line or placeholder, and the answer stream stays the answer.
//
// The name is namespaced under acp-kit rather than any one agent so sibling
// relays (zulip-acp, poe-acp, slack-acp) share a single handler.
//
// Version skew is safe by construction: the ACP spec says implementations
// SHOULD ignore unrecognized notifications, so an old relay drops notices from
// a new agent, and a new relay simply never sees them from an old agent.
//
// See: https://agentclientprotocol.com/protocol/extensibility
const NoticeMethod = "_dev.acp-kit/notice"

// Notice levels.
const (
	NoticeLevelInfo    = "info"
	NoticeLevelWarning = "warning"
	NoticeLevelError   = "error"
)

// Notice is the payload of a NoticeMethod notification.
type Notice struct {
	// SessionId identifies the session the notice concerns.
	SessionId acp.SessionId `json:"sessionId"`
	// Level is one of "info", "warning", "error".
	Level string `json:"level"`
	// Kind is a stable machine-readable discriminator, e.g.
	// "provider_retry" or "compaction_failed". Relays may switch on it;
	// unknown kinds must be treated as generic.
	Kind string `json:"kind"`
	// Text is a human-readable one-liner, safe to show in a status area.
	Text string `json:"text"`
}

// NoticeSink is an OPTIONAL interface a SessionUpdateSink may also
// implement to receive notices for its session. Sinks that do not
// implement it never see notices, which are then dropped — the correct
// outcome, since a notice is never part of the answer.
type NoticeSink interface {
	OnNotice(ctx context.Context, n Notice) error
}

// AgentSupportsNotices reports whether the agent advertised the acp-kit
// notice extension in its initialize response's agentCapabilities._meta.
//
// A relay does not need this to receive notices — dispatch handles them
// unconditionally — but it is useful for deciding whether the agent will
// report retries at all, versus silently stalling.
func (a *AgentProc) AgentSupportsNotices() bool {
	return a.caps.Notices
}

// handleNotice decodes a NoticeMethod notification and hands it to the
// session's sink if that sink implements NoticeSink.
//
// A malformed or unroutable notice is DROPPED, not errored: a notice is
// decoration on a turn, never worth failing one over.
func (a *AgentProc) handleNotice(ctx context.Context, params json.RawMessage) {
	var n Notice
	if err := json.Unmarshal(params, &n); err != nil {
		return
	}
	sink, ok := a.sinkFor(n.SessionId).(NoticeSink)
	if !ok {
		return
	}
	_ = sink.OnNotice(ctx, n)
}

// parseNoticeCap reads the agentCapabilities._meta entry for the acp-kit
// namespace and reports whether it advertises the notice extension:
//
//	{"dev.acp-kit": {"notice": {"method": "_dev.acp-kit/notice", "version": 1}}}
func parseNoticeCap(exts map[string]json.RawMessage) bool {
	raw, ok := exts["dev.acp-kit"]
	if !ok {
		return false
	}
	var ns struct {
		Notice struct {
			Method string `json:"method"`
		} `json:"notice"`
	}
	if err := json.Unmarshal(raw, &ns); err != nil {
		return false
	}
	return ns.Notice.Method == NoticeMethod
}
