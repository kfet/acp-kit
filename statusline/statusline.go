// Package statusline defines the wire contract for the
// dev.acp-kit.status-line/v1 ACP extension: a tiny mood/plan header
// that agents emit on session/update._meta so chat relays (poe-acp,
// slack-acp, …) can render a compact status line ahead of the
// assistant's reply.
//
// Scope: only the shared, relay-agnostic pieces live here — the
// extension id, length cap, Status type, provider→emoji map, and
// ParseMeta. Each relay keeps its own renderer because the markup
// (poe markdown vs. Slack mrkdwn) and the surface (animated spinner
// vs. static placeholder) differ.
//
// Wire shape:
//
//	_meta: {
//	  "dev.acp-kit.status-line/v1": {
//	    "mood": "<short label, ≤12 runes>",
//	    "plan": "<short label, ≤12 runes>"
//	  }
//	}
//
// Both fields are opaque short strings; agents stay polite within
// ~12 runes per field, ParseMeta enforces the cap on the consumer
// side. The payload can ride along on any session/update kind
// (agent_message_chunk, agent_thought_chunk, tool_call, …) — relays
// read _meta irrespective of update kind.
package statusline

import (
	"encoding/json"
	"strings"
)

// ExtensionID is the _meta key both sides use to advertise support
// and to carry per-update mood/plan payloads.
const ExtensionID = "dev.acp-kit.status-line/v1"

// MaxFieldRunes caps the rendered length of mood and plan. Mobile
// chat surfaces have very little horizontal room; an oversize agent
// label must not push the header off-screen or wrap.
const MaxFieldRunes = 12

// Status is the renderable state of one status header. Relays build
// this from ParseMeta (mood/plan) and their own provider-resolution
// logic (provider emoji).
type Status struct {
	// ProviderEmoji is the relay-resolved emoji for the model
	// servicing the turn. Empty means unknown provider or the relay
	// has no per-turn model concept — segment is then dropped.
	ProviderEmoji string
	// Mood is the agent-supplied mood label (opaque string).
	Mood string
	// Plan is the agent-supplied plan progress label (opaque string).
	Plan string
}

// ProviderEmojiForModel resolves the provider emoji from a fully
// qualified model id of the form "<provider>/<model>" (the convention
// fir uses for its SessionModelState.currentModelId). An id with no
// '/' or an empty id returns "" — caller treats that as "drop the
// segment".
func ProviderEmojiForModel(modelID string) string {
	i := strings.IndexByte(modelID, '/')
	if i <= 0 {
		return ""
	}
	return ProviderEmoji(modelID[:i])
}

// ProviderEmoji maps a provider slug (case-insensitive) to the emoji
// shown in the status header. Returns "" for unknown providers, which
// callers treat as a dropped segment.
//
// The mapping is relay-owned by design — the relay knows which
// provider is currently servicing the turn — but it's kept here so
// every relay renders the same agent with the same emoji. Add new
// providers as they appear in fir's models registry.
func ProviderEmoji(slug string) string {
	switch strings.ToLower(strings.TrimSpace(slug)) {
	case "anthropic", "claude":
		return "🏛️"
	case "openai", "codex":
		return "🌐"
	case "poe":
		return "👻"
	case "google", "gemini", "google-antigravity", "antigravity":
		return "✨"
	case "copilot", "github-copilot", "github":
		return "🐙"
	case "sakana":
		return "🐡"
	case "xai", "grok":
		return "✖️"
	case "mistral", "mistralai":
		return "🌪️"
	case "meta", "meta-llama", "llama":
		return "🦙"
	case "openrouter":
		return "🔀"
	case "groq":
		return "⚡"
	case "deepseek":
		return "🐋"
	case "cohere":
		return "🔗"
	default:
		return ""
	}
}

// ParseMeta extracts the v1 mood/plan fields from a session/update
// _meta map. Returns (mood, plan, ok). ok is true if the extension
// key was present, regardless of whether mood/plan themselves were
// set. Both fields are returned trimmed and capped to MaxFieldRunes.
//
// Unknown sub-keys are ignored; non-string values are treated as
// absent rather than rejected (forward compat).
//
// The ACP SDK decodes _meta as map[string]any with sub-objects
// landing as either map[string]any or json.RawMessage depending on
// call path; both are handled here.
func ParseMeta(meta map[string]any) (mood, plan string, ok bool) {
	if meta == nil {
		return "", "", false
	}
	raw, present := meta[ExtensionID]
	if !present {
		return "", "", false
	}
	var payload struct {
		Mood string `json:"mood"`
		Plan string `json:"plan"`
	}
	switch v := raw.(type) {
	case map[string]any:
		if s, ok := v["mood"].(string); ok {
			payload.Mood = s
		}
		if s, ok := v["plan"].(string); ok {
			payload.Plan = s
		}
	case json.RawMessage:
		_ = json.Unmarshal(v, &payload)
	default:
		// Best-effort: re-marshal whatever it is and try again.
		if b, err := json.Marshal(v); err == nil {
			_ = json.Unmarshal(b, &payload)
		}
	}
	return CapRunes(strings.TrimSpace(payload.Mood), MaxFieldRunes),
		CapRunes(strings.TrimSpace(payload.Plan), MaxFieldRunes),
		true
}

// Segments returns the non-empty header segments in order: provider
// emoji, mood, plan. Empty entries are dropped so a missing mood
// doesn't leave a stray separator. Relays use this as the building
// block for their own Header/Spinner renderers, joining with their
// preferred separator and wrapping in surface-specific markup.
func Segments(s Status) []string {
	out := make([]string, 0, 3)
	if e := strings.TrimSpace(s.ProviderEmoji); e != "" {
		out = append(out, e)
	}
	if m := CapRunes(strings.TrimSpace(s.Mood), MaxFieldRunes); m != "" {
		out = append(out, m)
	}
	if p := CapRunes(strings.TrimSpace(s.Plan), MaxFieldRunes); p != "" {
		out = append(out, p)
	}
	return out
}

// CapRunes truncates s to at most n runes. No ellipsis is appended:
// the cap is tight (12 runes) and the agent picks the label, so an
// ellipsis would only steal another character of meaning. Returns ""
// when n <= 0.
func CapRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
