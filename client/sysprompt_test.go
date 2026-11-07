package client

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	acp "github.com/coder/acp-go-sdk"
)

func TestParseSystemPromptCaps(t *testing.T) {
	for _, tc := range []struct {
		name     string
		raw      string
		via      SystemPromptVia
		override bool
	}{
		{"none", `{"agentCapabilities":{}}`, SystemPromptNone, false},
		{"stable", `{"agentCapabilities":{"sessionCapabilities":{"systemPrompt":{}}}}`, SystemPromptStable, false},
		{"stable null", `{"agentCapabilities":{"sessionCapabilities":{"systemPrompt":null}}}`, SystemPromptNone, false},
		{"stable not object", `{"agentCapabilities":{"loadSession":true,"sessionCapabilities":{"systemPrompt":true}}}`, SystemPromptNone, false},
		{"mode null", `{"agentCapabilities":{"sessionCapabilities":{"systemPrompt":{"mode":null}}}}`, SystemPromptStable, false},
		{"override null", `{"agentCapabilities":{"sessionCapabilities":{"systemPrompt":{"mode":{"override":null}}}}}`, SystemPromptStable, false},
		{"mode not object", `{"agentCapabilities":{"sessionCapabilities":{"systemPrompt":{"mode":true}}}}`, SystemPromptStable, false},
		{"override not object", `{"agentCapabilities":{"sessionCapabilities":{"systemPrompt":{"mode":{"override":1}}}}}`, SystemPromptStable, false},
		{"override", `{"agentCapabilities":{"sessionCapabilities":{"systemPrompt":{"mode":{"override":{}}}}}}`, SystemPromptStable, true},
		{"legacy", `{"agentCapabilities":{"_meta":{"session.systemPrompt":{"version":1}}}}`, SystemPromptLegacyMeta, false},
		{"both", `{"agentCapabilities":{"sessionCapabilities":{"systemPrompt":{}},"_meta":{"session.systemPrompt":{"version":1}}}}`, SystemPromptStable, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := parseCaps(json.RawMessage(tc.raw))
			if c.SystemPromptVia != tc.via || c.SystemPromptOverride != tc.override || c.SystemPrompt != (tc.via != SystemPromptNone) {
				t.Fatalf("caps %+v", c)
			}
			if _, ok := c.Extensions[legacySystemPromptKey]; ok {
				t.Fatal("legacy key leaked into Extensions")
			}
		})
	}
}

// spAgent fakes an agent with configurable system-prompt caps and records
// the raw params of every session lifecycle request.
type spAgent struct {
	mu       sync.Mutex
	agentCap map[string]any
	failRes  bool // session/resume fails
	failLoad bool // session/load fails
	seen     map[string][]map[string]any
}

func (f *spAgent) handle(_ context.Context, method string, p json.RawMessage) (any, *acp.RequestError) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if method == acp.AgentMethodInitialize {
		return map[string]any{"protocolVersion": acp.ProtocolVersionNumber, "agentCapabilities": f.agentCap}, nil
	}
	var m map[string]any
	_ = json.Unmarshal(p, &m)
	if f.seen == nil {
		f.seen = map[string][]map[string]any{}
	}
	f.seen[method] = append(f.seen[method], m)
	switch method {
	case acp.AgentMethodSessionNew:
		return map[string]any{"sessionId": fmt.Sprintf("s-new-%d", len(f.seen[method]))}, nil
	case "session/resume":
		if f.failRes {
			return nil, acp.NewInternalError(nil)
		}
		return map[string]any{}, nil
	case acp.AgentMethodSessionLoad:
		if f.failLoad {
			return nil, acp.NewInternalError(nil)
		}
		return map[string]any{}, nil
	case "session/fork":
		return map[string]any{"sessionId": "s-child"}, nil
	case acp.AgentMethodSessionSetConfigOption:
		return map[string]any{}, nil
	}
	return nil, acp.NewMethodNotFound(method)
}

func (f *spAgent) last(t *testing.T, method string) map[string]any {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	l := f.seen[method]
	if len(l) == 0 {
		t.Fatalf("no %s request", method)
	}
	return l[len(l)-1]
}

func stableCaps(extra map[string]any) map[string]any {
	sc := map[string]any{"systemPrompt": map[string]any{}}
	for k, v := range extra {
		sc[k] = v
	}
	return map[string]any{"loadSession": true, "sessionCapabilities": sc}
}

func legacyCaps(extra map[string]any) map[string]any {
	return map[string]any{"loadSession": true, "sessionCapabilities": extra,
		"_meta": map[string]any{legacySystemPromptKey: map[string]any{"version": 1}}}
}

// wantStable asserts params carry systemPrompt {mode: append, content: [text]}
// and no legacy _meta entry.
func wantStable(t *testing.T, params map[string]any, text string) {
	t.Helper()
	sp, ok := params["systemPrompt"].(map[string]any)
	if !ok || sp["mode"] != "append" {
		t.Fatalf("systemPrompt = %#v", params["systemPrompt"])
	}
	content, _ := sp["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content = %#v", sp["content"])
	}
	if b, _ := content[0].(map[string]any); b["type"] != "text" || b["text"] != text {
		t.Fatalf("block = %#v", content[0])
	}
	if meta, _ := params["_meta"].(map[string]any); meta[legacySystemPromptKey] != nil {
		t.Fatalf("legacy _meta sent alongside stable: %v", meta)
	}
}

func wantNoStable(t *testing.T, params map[string]any) {
	t.Helper()
	if _, ok := params["systemPrompt"]; ok {
		t.Fatalf("systemPrompt must be absent: %v", params)
	}
}

func wantLegacy(t *testing.T, params map[string]any) {
	t.Helper()
	meta, _ := params["_meta"].(map[string]any)
	sp, _ := meta[legacySystemPromptKey].(map[string]any)
	if blocks, _ := sp["blocks"].([]any); len(blocks) != 1 {
		t.Fatalf("legacy _meta = %#v", params["_meta"])
	}
}

func TestSystemPromptStableNewResumeFork(t *testing.T) {
	ctx := context.Background()
	f := &spAgent{agentCap: stableCaps(map[string]any{"resume": map[string]any{}, "fork": map[string]any{}})}
	a := startPaired(t, Config{Command: []string{"x"}, Logger: discardLogger()}, f.handle).agent
	if c := a.Caps(); c.SystemPromptVia != SystemPromptStable || !c.SystemPrompt {
		t.Fatalf("caps %+v", c)
	}
	sid, err := a.NewSessionWithMeta(ctx, "/cwd", &recSink{}, text("sys"), map[string]any{"host": "h"})
	if err != nil {
		t.Fatal(err)
	}
	p := f.last(t, acp.AgentMethodSessionNew)
	wantStable(t, p, "sys")
	if meta, _ := p["_meta"].(map[string]any); meta["host"] != "h" {
		t.Fatalf("_meta = %v", p["_meta"])
	}

	// Empty blocks: the spec forbids an empty content array.
	if _, err := a.NewSession(ctx, "/cwd", &recSink{}, []acp.ContentBlock{}); err != nil {
		t.Fatal(err)
	}
	p = f.last(t, acp.AgentMethodSessionNew)
	wantNoStable(t, p)
	if _, ok := p["_meta"]; ok {
		t.Fatalf("_meta must be absent: %v", p)
	}

	if err := a.ResumeSessionWithSystemPrompt(ctx, "/cwd", "r1", &recSink{}, text("sys")); err != nil {
		t.Fatal(err)
	}
	wantStable(t, f.last(t, "session/resume"), "sys")
	if err := a.ResumeSession(ctx, "/cwd", "r2", &recSink{}); err != nil {
		t.Fatal(err)
	}
	wantNoStable(t, f.last(t, "session/resume"))

	// Fork re-sends the parent's prompt; the child inherits it.
	child, err := a.ForkSession(ctx, "/cwd", sid, "", &recSink{})
	if err != nil {
		t.Fatal(err)
	}
	wantStable(t, f.last(t, "session/fork"), "sys")
	stale(a)
	if err := a.SetConfigOption(ctx, child, "c", "v"); err != nil {
		t.Fatal(err)
	}
	if p := f.last(t, "session/resume"); p["sessionId"] != "s-child" {
		t.Fatalf("resumed %v", p["sessionId"])
	} else {
		wantStable(t, p, "sys")
	}
	// A fork of an untracked parent carries none.
	if _, err := a.ForkSession(ctx, "/cwd", "unknown", "", &recSink{}); err != nil {
		t.Fatal(err)
	}
	wantNoStable(t, f.last(t, "session/fork"))
}

// Respawn recovery re-sends the stable prompt on load and on the
// new-session fallback.
func TestSystemPromptStableRecoveryLoadAndNew(t *testing.T) {
	ctx := context.Background()
	f := &spAgent{agentCap: stableCaps(nil)}
	a := startPaired(t, Config{Command: []string{"x"}, Logger: discardLogger()}, f.handle).agent
	sid, err := a.NewSession(ctx, "/cwd", &recSink{}, text("sys"))
	if err != nil {
		t.Fatal(err)
	}
	stale(a)
	if err := a.SetConfigOption(ctx, sid, "c", "v"); err != nil {
		t.Fatal(err)
	}
	wantStable(t, f.last(t, acp.AgentMethodSessionLoad), "sys")

	f.mu.Lock()
	f.failLoad = true
	f.mu.Unlock()
	stale(a)
	if err := a.SetConfigOption(ctx, sid, "c", "v"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	n := len(f.seen[acp.AgentMethodSessionNew])
	f.mu.Unlock()
	if n != 2 {
		t.Fatalf("session/new count %d", n)
	}
	wantStable(t, f.last(t, acp.AgentMethodSessionNew), "sys")
}

// Legacy-key agents keep the deprecated path: _meta on session/new only.
func TestSystemPromptLegacyPath(t *testing.T) {
	ctx := context.Background()
	f := &spAgent{agentCap: legacyCaps(map[string]any{"resume": map[string]any{}, "fork": map[string]any{}}), failRes: true, failLoad: true}
	a := startPaired(t, Config{Command: []string{"x"}, Logger: discardLogger()}, f.handle).agent
	if c := a.Caps(); c.SystemPromptVia != SystemPromptLegacyMeta || !c.SystemPrompt || c.SystemPromptOverride {
		t.Fatalf("caps %+v", c)
	}
	sid, err := a.NewSession(ctx, "/cwd", &recSink{}, text("sys"))
	if err != nil {
		t.Fatal(err)
	}
	p := f.last(t, acp.AgentMethodSessionNew)
	wantLegacy(t, p)
	wantNoStable(t, p)

	if _, err := a.ForkSession(ctx, "/cwd", sid, "", &recSink{}); err != nil {
		t.Fatal(err)
	}
	wantNoStable(t, f.last(t, "session/fork"))

	// Recovery: resume and load fail, the new-session fallback carries
	// the legacy entry again; resume/load never carry a prompt.
	stale(a)
	if err := a.SetConfigOption(ctx, sid, "c", "v"); err != nil {
		t.Fatal(err)
	}
	wantNoStable(t, f.last(t, "session/resume"))
	wantNoStable(t, f.last(t, acp.AgentMethodSessionLoad))
	p = f.last(t, acp.AgentMethodSessionNew)
	wantLegacy(t, p)
	wantNoStable(t, p)

	if err := a.ResumeSessionWithSystemPrompt(ctx, "/cwd", "r1", &recSink{}, text("sys")); err == nil {
		t.Fatal("want resume error")
	}
	wantNoStable(t, f.last(t, "session/resume"))
}

// An agent advertising neither path never gets the stable field.
func TestSystemPromptNeitherCap(t *testing.T) {
	f := &spAgent{agentCap: map[string]any{}}
	a := startPaired(t, Config{Command: []string{"x"}}, f.handle).agent
	if c := a.Caps(); c.SystemPrompt || c.SystemPromptVia != SystemPromptNone {
		t.Fatalf("caps %+v", c)
	}
	if _, err := a.NewSession(context.Background(), "/cwd", &recSink{}, text("sys")); err != nil {
		t.Fatal(err)
	}
	wantNoStable(t, f.last(t, acp.AgentMethodSessionNew))
}

// The legacy offer is still made at initialize (deprecated, one release).
func TestLegacySystemPromptOfferStillSent(t *testing.T) {
	var init map[string]any
	h := func(_ context.Context, method string, p json.RawMessage) (any, *acp.RequestError) {
		if method == acp.AgentMethodInitialize {
			_ = json.Unmarshal(p, &init)
			return map[string]any{"protocolVersion": acp.ProtocolVersionNumber, "agentCapabilities": map[string]any{}}, nil
		}
		return nil, acp.NewMethodNotFound(method)
	}
	startPaired(t, Config{Command: []string{"x"}}, h)
	cc, _ := init["clientCapabilities"].(map[string]any)
	meta, _ := cc["_meta"].(map[string]any)
	if meta[legacySystemPromptKey] == nil {
		t.Fatalf("clientCapabilities = %v", cc)
	}
}
