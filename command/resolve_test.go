package command

import (
	"strings"
	"testing"

	"github.com/kfet/acp-kit/client"
)

func ids(ss ...string) []client.ModelInfo {
	out := make([]client.ModelInfo, len(ss))
	for i, s := range ss {
		out[i] = client.ModelInfo{ID: s}
	}
	return out
}

func TestResolveModel(t *testing.T) {
	all := ids(
		"anthropic/claude-opus-4-5",
		"anthropic/claude-opus-5-5-fast",
		"anthropic/claude-opus-5-5",
		"anthropic/claude-3-5-sonnet-20241022",
		"openrouter/claude-3-5-sonnet-20241022",
		"openai/gpt-4o-2024-08-06",
		"bare-model",
	)
	cases := []struct {
		q     string
		exact bool
		want  []string // best-first candidates
	}{
		{"", false, nil},
		{"anthropic/claude-opus-5-5", true, []string{"anthropic/claude-opus-5-5"}},
		{"anth/opus55", false, []string{"anthropic/claude-opus-5-5", "anthropic/claude-opus-5-5-fast"}},
		{"opus-5-5", false, []string{"anthropic/claude-opus-5-5", "anthropic/claude-opus-5-5-fast"}},
		{"opus5", false, []string{"anthropic/claude-opus-5-5", "anthropic/claude-opus-5-5-fast"}},
		{"opus45", false, []string{"anthropic/claude-opus-4-5"}},
		{"opus-5-5-fast", false, []string{"anthropic/claude-opus-5-5-fast"}},
		{"Claude Opus 5.5 Fast", false, []string{"anthropic/claude-opus-5-5-fast"}},
		// same model under two providers: ambiguous, shorter id first
		{"claude-3-5-sonnet-20241022", false, []string{"anthropic/claude-3-5-sonnet-20241022", "openrouter/claude-3-5-sonnet-20241022"}},
		{"sonnet35", false, nil}, // order matters: 35 comes before sonnet
		{"35sonnet", false, []string{"anthropic/claude-3-5-sonnet-20241022", "openrouter/claude-3-5-sonnet-20241022"}},
		{"or/35son", false, []string{"openrouter/claude-3-5-sonnet-20241022"}},
		// dates never satisfy a query
		{"sonnet2024", false, nil},
		{"4o", false, []string{"openai/gpt-4o-2024-08-06"}},
		{"csonnet", false, nil}, // letter runs do not bridge digits
		{"bm", false, []string{"bare-model"}},
		{"x/bare", false, nil},
		{"gemini", false, nil},
		{"anth/", false, nil},
		{"*", false, nil},
		{"é", false, nil},
		{"a/*", false, nil},
	}
	for _, c := range cases {
		exact, got := ResolveModel(all, c.q)
		var gids []string
		for _, m := range got {
			gids = append(gids, m.ID)
		}
		if exact != c.exact || strings.Join(gids, ",") != strings.Join(c.want, ",") {
			t.Errorf("ResolveModel(%q) = %v %v, want %v %v", c.q, exact, gids, c.exact, c.want)
		}
	}
}

func TestTokenise(t *testing.T) {
	got := tokenise("claude-opus-4-5-20251101")
	var parts []string
	for _, tk := range got {
		parts = append(parts, tk.s)
	}
	if strings.Join(parts, "|") != "claude|opus|45|20251101" || !got[3].date || got[2].date {
		t.Fatalf("tokenise = %+v", got)
	}
	// a date run followed by more digits stays separate
	if got := tokenise("x-202410-1"); len(got) != 3 {
		t.Fatalf("tokenise date+digits = %+v", got)
	}
}

func TestModelAlias(t *testing.T) {
	b := withCtrl(&fakeCtrl{models: ids("p/m")})
	for _, s := range []string{"!m", "!m x", "/M p/m"} {
		if !b.IsCommand(s) {
			t.Errorf("%q should be a command", s)
		}
	}
	for _, s := range []string{"!me", "/me waves", "!msg hi", "!m\tx"} {
		if b.IsCommand(s) {
			t.Errorf("%q must not be a command", s)
		}
	}
	if !strings.Contains(b.help().Text, "`!m`") {
		t.Error("help must mention !m")
	}
}

func TestResolveModelSpanBeatsLength(t *testing.T) {
	_, got := ResolveModel(ids("x/za-zc", "x/zzzazzzc"), "ac")
	if len(got) != 2 || got[0].ID != "x/zzzazzzc" {
		t.Fatalf("tighter span must rank first: %v", got)
	}
}

func TestResolveModelBestTierOnly(t *testing.T) {
	_, got := ResolveModel(ids("x/oxpxuxs", "x/claude-opus", "x/o-pusx"), "opus")
	if len(got) != 2 || got[0].ID != "x/claude-opus" || got[1].ID != "x/o-pusx" {
		t.Fatalf("only prefix-tier candidates expected: %v", got)
	}
}
