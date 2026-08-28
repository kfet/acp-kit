package client

import (
	"context"
	"encoding/json"
	"testing"

	acp "github.com/coder/acp-go-sdk"
)

// captureNew answers initialize + session/new and records the raw
// session/new params so tests can assert on the exact wire shape.
func captureNew(seen *json.RawMessage) func(ctx context.Context, method string, params json.RawMessage) (any, *acp.RequestError) {
	return func(_ context.Context, method string, params json.RawMessage) (any, *acp.RequestError) {
		switch method {
		case acp.AgentMethodInitialize:
			return map[string]any{"protocolVersion": acp.ProtocolVersionNumber, "agentCapabilities": map[string]any{}}, nil
		case acp.AgentMethodSessionNew:
			*seen = append(json.RawMessage(nil), params...)
			return map[string]any{"sessionId": "sess-A"}, nil
		}
		return nil, acp.NewMethodNotFound(method)
	}
}

// metaOf decodes the `_meta` object of a captured session/new request.
// present=false means the key was absent from the wire entirely.
func metaOf(t *testing.T, raw json.RawMessage) (meta map[string]any, present bool) {
	t.Helper()
	var env map[string]json.RawMessage
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal session/new params: %v", err)
	}
	rawMeta, ok := env["_meta"]
	if !ok || string(rawMeta) == "null" {
		return nil, false
	}
	if err := json.Unmarshal(rawMeta, &meta); err != nil {
		t.Fatalf("unmarshal _meta: %v", err)
	}
	return meta, true
}

// NewSession (and NewSessionWithMeta with no extra entries and no system
// prompt) must leave `_meta` off the wire entirely.
func TestNewSession_NoMetaWhenNothingToSend(t *testing.T) {
	var seen json.RawMessage
	pc := startPaired(t, Config{Command: []string{"x"}}, captureNew(&seen))

	if _, err := pc.agent.NewSession(context.Background(), "/cwd", &recSink{}, nil); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if _, present := metaOf(t, seen); present {
		t.Fatalf("_meta must be absent, got %s", seen)
	}

	if _, err := pc.agent.NewSessionWithMeta(context.Background(), "/cwd", &recSink{}, nil, map[string]any{}); err != nil {
		t.Fatalf("NewSessionWithMeta: %v", err)
	}
	if _, present := metaOf(t, seen); present {
		t.Fatalf("_meta must be absent for empty extraMeta, got %s", seen)
	}
}

func TestNewSessionWithMeta_ExtraEntries(t *testing.T) {
	var seen json.RawMessage
	pc := startPaired(t, Config{Command: []string{"x"}}, captureNew(&seen))

	sid, err := pc.agent.NewSessionWithMeta(context.Background(), "/cwd", &recSink{}, nil,
		map[string]any{"host": "zboxserver"})
	if err != nil {
		t.Fatalf("NewSessionWithMeta: %v", err)
	}
	if sid != "sess-A" {
		t.Fatalf("sid = %q", sid)
	}
	meta, present := metaOf(t, seen)
	if !present {
		t.Fatalf("_meta missing: %s", seen)
	}
	if meta["host"] != "zboxserver" {
		t.Fatalf("_meta.host = %#v, want zboxserver", meta["host"])
	}
	if _, ok := meta["session.systemPrompt"]; ok {
		t.Fatal("session.systemPrompt must be absent when no blocks are passed")
	}
}

// The reserved system-prompt key is owned by systemPromptBlocks and wins
// over a same-named entry in extraMeta; unrelated entries survive.
func TestNewSessionWithMeta_SystemPromptWins(t *testing.T) {
	var seen json.RawMessage
	pc := startPaired(t, Config{Command: []string{"x"}}, captureNew(&seen))

	_, err := pc.agent.NewSessionWithMeta(context.Background(), "/cwd", &recSink{},
		[]acp.ContentBlock{acp.TextBlock("sys")},
		map[string]any{"host": "boxy", "session.systemPrompt": "hijacked"})
	if err != nil {
		t.Fatalf("NewSessionWithMeta: %v", err)
	}
	meta, present := metaOf(t, seen)
	if !present {
		t.Fatalf("_meta missing: %s", seen)
	}
	if meta["host"] != "boxy" {
		t.Fatalf("_meta.host = %#v", meta["host"])
	}
	sp, ok := meta["session.systemPrompt"].(map[string]any)
	if !ok {
		t.Fatalf("session.systemPrompt = %#v, want object with blocks", meta["session.systemPrompt"])
	}
	blocks, ok := sp["blocks"].([]any)
	if !ok || len(blocks) != 1 {
		t.Fatalf("blocks = %#v", sp["blocks"])
	}
}
