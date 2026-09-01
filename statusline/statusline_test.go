package statusline

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestExtensionID(t *testing.T) {
	// Pin the wire constant. Changing this breaks every consumer
	// (poe-acp, slack-acp, fir agent) — bump the version suffix
	// instead of editing in place.
	if ExtensionID != "dev.acp-kit.status-line/v1" {
		t.Fatalf("ExtensionID = %q, want dev.acp-kit.status-line/v1", ExtensionID)
	}
}

func TestProviderEmoji(t *testing.T) {
	cases := []struct {
		slug, want string
	}{
		{"anthropic", "🏛️"},
		{"Claude", "🏛️"},
		{"  CLAUDE  ", "🏛️"},
		{"openai", "🌐"},
		{"codex", "🌐"},
		{"poe", "👻"},
		{"google", "✨"},
		{"gemini", "✨"},
		{"antigravity", "✨"},
		{"copilot", "🐙"},
		{"github", "🐙"},
		{"xai", "✖️"},
		{"grok", "✖️"},
		{"mistral", "🌪️"},
		{"llama", "🦙"},
		{"openrouter", "🔀"},
		{"groq", "⚡"},
		{"deepseek", "🐋"},
		{"cohere", "🔗"},
		{"sakana", "🐡"},
		{"", ""},
		{"unknown-provider", ""},
	}
	for _, c := range cases {
		if got := ProviderEmoji(c.slug); got != c.want {
			t.Errorf("ProviderEmoji(%q) = %q, want %q", c.slug, got, c.want)
		}
	}
}

func TestProviderEmojiForModel(t *testing.T) {
	cases := []struct {
		modelID, want string
	}{
		{"anthropic/claude-sonnet-4-5", "🏛️"},
		{"openai/gpt-5", "🌐"},
		{"google/gemini-3-pro", "✨"},
		{"unknown/model", ""},
		{"noslash", ""},
		{"", ""},
		{"/leading-slash", ""},
	}
	for _, c := range cases {
		if got := ProviderEmojiForModel(c.modelID); got != c.want {
			t.Errorf("ProviderEmojiForModel(%q) = %q, want %q", c.modelID, got, c.want)
		}
	}
}

func TestParseMeta_MapStringAny(t *testing.T) {
	meta := map[string]any{
		ExtensionID: map[string]any{
			"mood": "engaged",
			"plan": "3/8",
		},
	}
	mood, plan, ok := ParseMeta(meta)
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if mood != "engaged" || plan != "3/8" {
		t.Errorf("got (%q,%q), want (engaged,3/8)", mood, plan)
	}
}

func TestParseMeta_RawMessage(t *testing.T) {
	raw := json.RawMessage(`{"mood":"calm","plan":"5/10"}`)
	meta := map[string]any{ExtensionID: raw}
	mood, plan, ok := ParseMeta(meta)
	if !ok {
		t.Fatal("ok = false")
	}
	if mood != "calm" || plan != "5/10" {
		t.Errorf("got (%q,%q), want (calm,5/10)", mood, plan)
	}
}

func TestParseMeta_AbsentExtension(t *testing.T) {
	meta := map[string]any{"some.other.ext/v1": map[string]any{}}
	mood, plan, ok := ParseMeta(meta)
	if ok || mood != "" || plan != "" {
		t.Errorf("got (%q,%q,%v), want (\"\",\"\",false)", mood, plan, ok)
	}
}

func TestParseMeta_NilMeta(t *testing.T) {
	mood, plan, ok := ParseMeta(nil)
	if ok || mood != "" || plan != "" {
		t.Errorf("got (%q,%q,%v), want (\"\",\"\",false)", mood, plan, ok)
	}
}

func TestParseMeta_PresentButEmpty(t *testing.T) {
	// ok=true even when mood/plan are absent — the relay should
	// know the agent participates in the extension.
	meta := map[string]any{ExtensionID: map[string]any{}}
	mood, plan, ok := ParseMeta(meta)
	if !ok {
		t.Fatal("ok = false, want true (key present)")
	}
	if mood != "" || plan != "" {
		t.Errorf("got (%q,%q), want empty strings", mood, plan)
	}
}

func TestParseMeta_TrimsAndCaps(t *testing.T) {
	long := strings.Repeat("x", MaxFieldRunes+10)
	meta := map[string]any{
		ExtensionID: map[string]any{
			"mood": "  " + long + "  ",
			"plan": "  short  ",
		},
	}
	mood, plan, _ := ParseMeta(meta)
	if mood != strings.Repeat("x", MaxFieldRunes) {
		t.Errorf("mood = %q, want %d 'x'", mood, MaxFieldRunes)
	}
	if plan != "short" {
		t.Errorf("plan = %q, want %q", plan, "short")
	}
}

func TestParseMeta_RuneSafeCap(t *testing.T) {
	// Multi-byte runes must not get sliced mid-codepoint.
	emoji := strings.Repeat("🌲", MaxFieldRunes+5)
	meta := map[string]any{
		ExtensionID: map[string]any{"mood": emoji, "plan": ""},
	}
	mood, _, _ := ParseMeta(meta)
	want := strings.Repeat("🌲", MaxFieldRunes)
	if mood != want {
		t.Errorf("mood = %q, want %q", mood, want)
	}
}

func TestParseMeta_NonStringFieldsIgnored(t *testing.T) {
	// Forward-compat: future schemas might use objects or numbers
	// for sub-fields. We currently treat them as absent rather than
	// reject the whole payload.
	meta := map[string]any{
		ExtensionID: map[string]any{
			"mood": 42,
			"plan": []string{"a", "b"},
		},
	}
	mood, plan, ok := ParseMeta(meta)
	if !ok {
		t.Fatal("ok = false")
	}
	if mood != "" || plan != "" {
		t.Errorf("got (%q,%q), want empty", mood, plan)
	}
}

func TestParseMeta_DefaultBranchRemarshal(t *testing.T) {
	// Exercise the default branch in ParseMeta's type switch: the
	// extension value isn't a map[string]any or json.RawMessage but
	// can be re-marshalled into the expected shape.
	type Payload struct {
		Mood string `json:"mood"`
		Plan string `json:"plan"`
	}
	meta := map[string]any{
		ExtensionID: Payload{Mood: "engaged", Plan: "3/8"},
	}
	mood, plan, ok := ParseMeta(meta)
	if !ok {
		t.Fatal("ok = false")
	}
	if mood != "engaged" || plan != "3/8" {
		t.Errorf("got (%q,%q), want (engaged,3/8)", mood, plan)
	}
}

func TestParseMeta_DefaultBranchUnmarshalable(t *testing.T) {
	// A value that json.Marshal cannot encode falls through cleanly:
	// the extension is "present" but mood/plan stay empty.
	meta := map[string]any{
		ExtensionID: make(chan int),
	}
	mood, plan, ok := ParseMeta(meta)
	if !ok {
		t.Fatal("ok = false (extension key was present)")
	}
	if mood != "" || plan != "" {
		t.Errorf("got (%q,%q), want empty", mood, plan)
	}
}

func TestSegments(t *testing.T) {
	cases := []struct {
		name string
		in   Status
		want []string
	}{
		{
			"all three",
			Status{ProviderEmoji: "🏛️", Mood: "engaged", Plan: "3/8"},
			[]string{"🏛️", "engaged", "3/8"},
		},
		{
			"mood only",
			Status{Mood: "calm"},
			[]string{"calm"},
		},
		{
			"plan only",
			Status{Plan: "5/10"},
			[]string{"5/10"},
		},
		{
			"empty status",
			Status{},
			[]string{},
		},
		{
			"whitespace fields are dropped",
			Status{ProviderEmoji: "   ", Mood: "  ", Plan: " "},
			[]string{},
		},
		{
			"oversize labels get capped",
			Status{Mood: strings.Repeat("a", MaxFieldRunes+5), Plan: "ok"},
			[]string{strings.Repeat("a", MaxFieldRunes), "ok"},
		},
		{
			"emoji and model are one space-joined segment",
			Status{ProviderEmoji: "🏛️", Model: "opus-4.5", Mood: "steady", Plan: "2/5"},
			[]string{"🏛️ opus-4.5", "steady", "2/5"},
		},
		{
			"model only degrades to the bare name",
			Status{Model: "gpt-5-codex", Mood: "steady"},
			[]string{"gpt-5-codex", "steady"},
		},
		{
			"emoji only degrades to the bare emoji",
			Status{ProviderEmoji: "👻", Mood: "steady"},
			[]string{"👻", "steady"},
		},
		{
			"whitespace model is dropped",
			Status{ProviderEmoji: "🏛️", Model: "  "},
			[]string{"🏛️"},
		},
		{
			"oversize model gets capped",
			Status{ProviderEmoji: "🏛️", Model: strings.Repeat("z", MaxFieldRunes+4)},
			[]string{"🏛️ " + strings.Repeat("z", MaxFieldRunes)},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Segments(c.in)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("Segments(%+v) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

// TestShortModelName pins the derivation table from the brief: the
// label a user reads next to the provider emoji.
func TestShortModelName(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		// The brief's table.
		{"anthropic/claude-opus-4-5-20251001", "opus-4.5"},
		{"anthropic/claude-sonnet-4-5", "sonnet-4.5"},
		{"openai/gpt-5-codex", "gpt-5-codex"},
		{"google/gemini-3-pro-preview", "gemini-3-pro"},
		{"poe/Claude-Opus-4.5", "opus-4.5"},
		{"", ""},
		{"no-slash-thing", "no-slash-thi"}, // capped at MaxFieldRunes

		// Meaningful family prefixes survive.
		{"xai/grok-4", "grok-4"},
		// Every dash between two digits becomes a dot — including the
		// one before a parameter count.
		{"meta/llama-3-3-70b", "llama-3.3.70"},
		{"deepseek/deepseek-chat", "deepseek-cha"},

		// Decorations unwind in any order and repeatedly.
		{"google/gemini-3-pro-preview-20251101", "gemini-3-pro"},
		{"anthropic/claude-opus-4-5-latest", "opus-4.5"},

		// A vendor echo that is the WHOLE name is not stripped to "".
		{"anthropic/claude-", "claude-"},
		{"anthropic/anthropic-x", "x"},

		// Digit-adjacency only: a dash before a word stays a dash, and a
		// bare eight-digit-looking tail that isn't a date stamp is kept.
		{"openai/gpt-4o-mini", "gpt-4o-mini"},
		{"x/thing-1234567", "thing-1234567"[:MaxFieldRunes]},

		// Whitespace and casing.
		{"  Anthropic/Claude-Sonnet-4-5  ", "sonnet-4.5"},
		{"/leading-slash", "leading-slas"},
	}
	for _, c := range cases {
		if got := ShortModelName(c.in); got != c.want {
			t.Errorf("ShortModelName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestCapRunes(t *testing.T) {
	cases := []struct {
		s    string
		n    int
		want string
	}{
		{"hello", 5, "hello"},
		{"hello", 3, "hel"},
		{"hello", 0, ""},
		{"hello", -1, ""},
		{"", 5, ""},
		{"🌲🌲🌲🌲", 2, "🌲🌲"},
	}
	for _, c := range cases {
		if got := CapRunes(c.s, c.n); got != c.want {
			t.Errorf("CapRunes(%q, %d) = %q, want %q", c.s, c.n, got, c.want)
		}
	}
}

// TestMaxTrailingFieldRunes_WorstCaseWidth pins the widened trailing
// cap and the worst-case width of a rendered status line, which is the
// mobile-safety budget: the leading identity segment is emoji + space +
// model (both capped), mood and plan are unchanged, and only the last
// segment uses the wider trailing cap.
func TestMaxTrailingFieldRunes_WorstCaseWidth(t *testing.T) {
	if MaxTrailingFieldRunes <= MaxFieldRunes {
		t.Fatalf("MaxTrailingFieldRunes = %d, want > MaxFieldRunes (%d)",
			MaxTrailingFieldRunes, MaxFieldRunes)
	}
	// A relay joins segments with " • " (3 runes) and appends the
	// trailing activity segment plus up to three animation dots.
	const sep = " • "
	segs := Segments(Status{
		ProviderEmoji: "🏛️",
		Model:         strings.Repeat("d", MaxFieldRunes+9),
		Mood:          strings.Repeat("m", MaxFieldRunes+9),
		Plan:          strings.Repeat("p", MaxFieldRunes+9),
	})
	line := strings.Join(segs, sep) + sep +
		CapRunes(strings.Repeat("a", MaxTrailingFieldRunes+50), MaxTrailingFieldRunes) + "..."
	// 2 emoji runes + 1 space + 12 model + 3 sep + 12 mood + 3 sep + 12
	// plan + 3 sep + 36 activity + 3 dots = 87 runes worst case.
	const wantWorstCase = 2 + 1 + MaxFieldRunes + 3 + MaxFieldRunes + 3 + MaxFieldRunes +
		3 + MaxTrailingFieldRunes + 3
	if got := len([]rune(line)); got != wantWorstCase {
		t.Errorf("worst-case line width = %d runes, want %d (line %q)", got, wantWorstCase, line)
	}
}
