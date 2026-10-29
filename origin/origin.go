// Package origin defines the "dev.acp-kit.origin/v1" _meta entry: who sent a
// message, and from where, on a chat platform a relay bridges to an ACP agent.
//
// Relays attach it to session/new, session/load, session/resume,
// session/fork and every session/prompt. It is the ONLY place identity
// travels: relays must not put identity text into prompt content. Agents
// read it to pick a per-user cwd, credentials or policy.
//
// On session/new, session/fork and session/prompt the entry describes the
// triggering message. On session/resume and session/load it may describe
// the message that created the session instead (acp-kit re-sends that one
// when it recovers a session after an agent respawn), so agents should key
// per-message behaviour on prompts only.
//
// The cwd a relay sends alongside it is opaque (it may be a placeholder);
// the agent chooses the real working directory.
package origin

import (
	"context"
	"encoding/json"
)

// Key is the _meta key the origin entry is stored under.
const Key = "dev.acp-kit.origin/v1"

// Origin identifies the chat message that triggered an ACP call. Every
// field is optional; IDs are the platform's own, as opaque strings. It must
// stay comparable (no maps or slices): IsZero relies on ==.
type Origin struct {
	Platform  string `json:"platform,omitempty"` // e.g. "slack", "zulip", "poe"
	TeamID    string `json:"team_id,omitempty"`  // workspace / realm / site
	UserID    string `json:"user_id,omitempty"`
	ChannelID string `json:"channel_id,omitempty"`
	ThreadID  string `json:"thread_id,omitempty"` // thread / topic / conversation
	MessageID string `json:"message_id,omitempty"`
	IsDM      bool   `json:"is_dm"`
	IsMention bool   `json:"is_mention"`
}

// IsZero reports whether o carries no information.
func (o Origin) IsZero() bool { return o == Origin{} }

// Merge returns meta with o stored under Key. It never modifies meta: the
// result is a fresh map. A zero o returns meta unchanged (possibly nil).
func (o Origin) Merge(meta map[string]any) map[string]any {
	if o.IsZero() {
		return meta
	}
	out := make(map[string]any, len(meta)+1)
	for k, v := range meta {
		out[k] = v
	}
	out[Key] = o
	return out
}

// FromMeta reads the origin entry from a decoded _meta map. It accepts the
// entry either as an Origin value (in-process) or as the generic
// map/JSON-decoded form. ok is false when the key is absent or malformed.
func FromMeta(meta map[string]any) (o Origin, ok bool) {
	v, present := meta[Key]
	if !present {
		return Origin{}, false
	}
	if o, isO := v.(Origin); isO {
		return o, true
	}
	b, err := json.Marshal(v)
	if err != nil {
		return Origin{}, false
	}
	return Decode(b)
}

// FromRawMeta reads the origin entry from an undecoded _meta object.
func FromRawMeta(raw json.RawMessage) (Origin, bool) {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return Origin{}, false
	}
	v, present := m[Key]
	if !present {
		return Origin{}, false
	}
	return Decode(v)
}

// Decode parses one encoded origin entry (the value under Key). A null or
// empty entry reads as absent.
func Decode(b []byte) (Origin, bool) {
	var o Origin
	if json.Unmarshal(b, &o) != nil || o.IsZero() {
		return Origin{}, false
	}
	return o, true
}

type ctxKey struct{}

// With returns a context carrying o. acp-kit's client attaches it to the
// _meta of every session call made with that context.
func With(ctx context.Context, o Origin) context.Context {
	return context.WithValue(ctx, ctxKey{}, o)
}

// From returns the origin carried by ctx, if any.
func From(ctx context.Context) (Origin, bool) {
	o, ok := ctx.Value(ctxKey{}).(Origin)
	return o, ok
}
