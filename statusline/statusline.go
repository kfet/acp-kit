// Package statusline defines the wire contract for the
// dev.acp-kit.status-line/v1 ACP extension: a tiny mood/plan status
// line that agents emit on session/update._meta so chat relays
// (poe-acp, slack-acp, …) can render a compact identity + progress
// line alongside the assistant's reply.
//
// Scope: only the shared, relay-agnostic pieces live here — the
// extension id, length cap, Status type, provider→emoji map, model
// short-name derivation, and ParseMeta. Each relay keeps its own
// renderer because the markup (poe markdown vs. Slack mrkdwn), the
// surface (animated spinner vs. static placeholder) and the placement
// (poe-acp renders it as an italic footer under the answer) differ.
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

// MaxTrailingFieldRunes caps the LAST segment of a status line — the
// live activity label (the running tool / progress verb a relay
// appends after mood and plan). It is deliberately wider than
// MaxFieldRunes: the trailing segment has nothing after it, so an
// oversize value can only spill at the very end of the line instead of
// pushing the mood/plan header off a narrow screen, and a real
// progress string ("wait poll 7: step 1/1 Bash") is meaningless when
// clipped to twelve runes.
//
// Only the trailing segment may use this cap — earlier fields stay at
// MaxFieldRunes so the header itself keeps its mobile-safe width.
const MaxTrailingFieldRunes = 36

// Status is the renderable state of one status header. Relays build
// this from ParseMeta (mood/plan) and their own provider-resolution
// logic (provider emoji).
type Status struct {
	// ProviderEmoji is the relay-resolved emoji for the model
	// servicing the turn. Empty means unknown provider or the relay
	// has no per-turn model concept — segment is then dropped.
	ProviderEmoji string
	// Model is the short, human-readable name of the model servicing
	// the turn ("opus-4.5", "gpt-5-codex"), normally derived from the
	// fully qualified model id with ShortModelName. It renders in the
	// SAME segment as ProviderEmoji, separated by a single space —
	// emoji and model name are one unit ("🏛️ opus-4.5"), not two
	// bullet-separated fields. Empty means unknown.
	Model string
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

// modelSuffixes are trailing decorations stripped from a model id: they
// pin a snapshot or a release channel and carry nothing the user reads
// off a status line. The date stamp (-YYYYMMDD) is handled separately.
var modelSuffixes = []string{"-latest", "-preview"}

// modelVendorPrefixes are leading vendor echoes stripped from a model
// name because the provider emoji already says the same thing
// ("🏛️ claude-opus-4.5" is redundant). Only vendor names that duplicate
// the emoji are listed: prefixes that carry meaning as part of the model
// family — gpt-, gemini-, grok-, llama-, deepseek- — are deliberately
// KEPT, because "gpt-5-codex" without its prefix is not a name anyone
// recognises.
var modelVendorPrefixes = []string{"claude-", "anthropic-"}

// ShortModelName derives the compact model label shown next to the
// provider emoji from a fully qualified model id ("<provider>/<model>",
// the convention fir uses). It is a display helper, not an identifier:
// the result is lossy by design and must never be fed back onto the
// wire.
//
// Rules, applied in order to the part after "<provider>/" (the whole
// string when there is no '/'):
//
//  1. drop the "<provider>/" prefix
//  2. drop a trailing date stamp "-YYYYMMDD" and a trailing "-latest" /
//     "-preview" (repeatedly, so "-preview-20251101" fully unwinds)
//  3. drop a leading vendor echo already carried by the emoji
//     ("claude-", "anthropic-"); keep meaningful family prefixes
//  4. turn version dashes BETWEEN DIGITS into dots ("4-5" → "4.5"),
//     leaving name dashes ("gpt-5-codex") alone
//  5. lowercase and cap to MaxFieldRunes
//
// An empty id returns "" — callers treat that as "drop the segment".
func ShortModelName(modelID string) string {
	s := strings.TrimSpace(modelID)
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[i+1:]
	}
	s = trimModelDecorations(s)
	for _, p := range modelVendorPrefixes {
		if len(s) > len(p) && strings.EqualFold(s[:len(p)], p) {
			s = s[len(p):]
			break
		}
	}
	return CapRunes(strings.ToLower(dashesToDots(s)), MaxFieldRunes)
}

// trimModelDecorations strips trailing release-channel words and date
// stamps until none is left, so "-preview-20251101" unwinds fully.
func trimModelDecorations(s string) string {
	for {
		trimmed := false
		for _, suf := range modelSuffixes {
			if len(s) > len(suf) && strings.EqualFold(s[len(s)-len(suf):], suf) {
				s, trimmed = s[:len(s)-len(suf)], true
			}
		}
		if t, ok := trimDateStamp(s); ok {
			s, trimmed = t, true
		}
		if !trimmed {
			return s
		}
	}
}

// trimDateStamp removes a trailing "-YYYYMMDD" snapshot marker. It only
// matches exactly eight digits after a dash, so a version segment
// ("-4", "-5") and a parameter count ("-70b") are left alone.
func trimDateStamp(s string) (string, bool) {
	const n = 9 // "-" + 8 digits
	if len(s) <= n || s[len(s)-n] != '-' {
		return s, false
	}
	for _, c := range s[len(s)-n+1:] {
		if c < '0' || c > '9' {
			return s, false
		}
	}
	return s[:len(s)-n], true
}

// dashesToDots rewrites a dash that sits BETWEEN two digits as a dot,
// which is how model versions are actually spoken: "sonnet-4-5" is
// "sonnet 4.5", while "gpt-5-codex" keeps its dash because "codex" is a
// name, not a version component.
func dashesToDots(s string) string {
	b := []byte(s)
	for i := 1; i < len(b)-1; i++ {
		if b[i] == '-' && isDigit(b[i-1]) && isDigit(b[i+1]) {
			b[i] = '.'
		}
	}
	return string(b)
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

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

// Segments returns the non-empty header segments in order: the
// model identity (provider emoji + short model name), mood, plan. Empty
// entries are dropped so a missing mood doesn't leave a stray
// separator. Relays use this as the building block for their own
// Header/Spinner renderers, joining with their preferred separator and
// wrapping in surface-specific markup.
//
// Emoji and model name form a SINGLE segment joined by one space
// ("🏛️ opus-4.5"): they name one thing, and a bullet between them would
// read as two independent fields. Either half alone degrades to just
// that half.
func Segments(s Status) []string {
	out := make([]string, 0, 3)
	if id := modelSegment(s); id != "" {
		out = append(out, id)
	}
	if m := CapRunes(strings.TrimSpace(s.Mood), MaxFieldRunes); m != "" {
		out = append(out, m)
	}
	if p := CapRunes(strings.TrimSpace(s.Plan), MaxFieldRunes); p != "" {
		out = append(out, p)
	}
	return out
}

// modelSegment renders the emoji+model unit, dropping whichever half is
// absent and returning "" when both are.
func modelSegment(s Status) string {
	e := strings.TrimSpace(s.ProviderEmoji)
	m := CapRunes(strings.TrimSpace(s.Model), MaxFieldRunes)
	switch {
	case e != "" && m != "":
		return e + " " + m
	case e != "":
		return e
	default:
		return m
	}
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
