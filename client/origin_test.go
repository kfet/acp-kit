package client

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	acp "github.com/coder/acp-go-sdk"

	"github.com/kfet/acp-kit/origin"
)

// Every session call made with an origin-carrying ctx puts the origin
// entry in its _meta; calls without one leave it off.
func TestOrigin_OnEverySessionCall(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]json.RawMessage{}
	pc := startPaired(t, Config{Command: []string{"x"}}, func(_ context.Context, method string, params json.RawMessage) (any, *acp.RequestError) {
		mu.Lock()
		seen[method] = append(json.RawMessage(nil), params...)
		mu.Unlock()
		switch method {
		case acp.AgentMethodInitialize:
			return map[string]any{"protocolVersion": acp.ProtocolVersionNumber, "agentCapabilities": map[string]any{
				"sessionCapabilities": map[string]any{"resume": map[string]any{}, "fork": map[string]any{}},
			}}, nil
		case acp.AgentMethodSessionNew, "session/fork":
			return map[string]any{"sessionId": "sess-" + method}, nil
		case "session/resume":
			return map[string]any{}, nil
		case acp.AgentMethodSessionPrompt:
			return map[string]any{"stopReason": "end_turn"}, nil
		}
		return nil, acp.NewMethodNotFound(method)
	})
	o := origin.Origin{Platform: "slack", UserID: "U1", ThreadID: "t", IsMention: true}
	ctx := origin.With(context.Background(), o)

	sid, err := pc.agent.NewSession(ctx, "/cwd", &recSink{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := pc.agent.ResumeSession(ctx, "/cwd", "old", &recSink{}); err != nil {
		t.Fatal(err)
	}
	if _, err := pc.agent.ForkSession(ctx, "/cwd", sid, "", &recSink{}); err != nil {
		t.Fatal(err)
	}
	if _, err := pc.agent.Prompt(ctx, sid, []acp.ContentBlock{acp.TextBlock("hi")}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, m := range []string{acp.AgentMethodSessionNew, "session/resume", "session/fork", acp.AgentMethodSessionPrompt} {
		meta, _ := metaOf(t, seen[m])
		got, ok := origin.FromMeta(meta)
		if !ok || got != o {
			t.Errorf("%s: origin = %+v %v (%s)", m, got, ok, seen[m])
		}
	}
	if pc.agent.sess[sid].meta[origin.Key] == nil {
		t.Error("origin not recorded for respawn recovery")
	}
}

func TestOrigin_AbsentWithoutContext(t *testing.T) {
	var seen json.RawMessage
	pc := startPaired(t, Config{Command: []string{"x"}}, captureNew(&seen))
	if _, err := pc.agent.NewSession(context.Background(), "/cwd", &recSink{}, nil); err != nil {
		t.Fatal(err)
	}
	if _, present := metaOf(t, seen); present {
		t.Fatalf("_meta must be absent: %s", seen)
	}
}

func TestOriginOnly(t *testing.T) {
	if originOnly(map[string]any{"session.systemPrompt": 1}) != nil {
		t.Fatal("non-origin meta must drop to nil")
	}
	o := origin.Origin{UserID: "U"}
	got := originOnly(o.Merge(map[string]any{"x": 1}))
	if len(got) != 1 || got[origin.Key] != o {
		t.Fatalf("got %v", got)
	}
}
