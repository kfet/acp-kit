package client

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"

	acp "github.com/coder/acp-go-sdk"
)

// perSessionAgent opens session sess-<n> on model p/m<n> and accepts
// set_config_option, failing it for session "sess-bad".
func perSessionAgent() func(context.Context, string, json.RawMessage) (any, *acp.RequestError) {
	var n atomic.Int64
	opts := func(cur string) []map[string]any {
		return []map[string]any{{
			"type": "select", "id": "model", "name": "Model", "category": "model",
			"currentValue": cur,
			"options":      []map[string]any{{"value": "p/m1", "name": "1"}, {"value": "p/m2", "name": "2"}},
		}}
	}
	return func(_ context.Context, method string, params json.RawMessage) (any, *acp.RequestError) {
		switch method {
		case acp.AgentMethodInitialize:
			return map[string]any{"protocolVersion": acp.ProtocolVersionNumber,
				"agentCapabilities": map[string]any{"sessionCapabilities": map[string]any{"resume": map[string]any{}}}}, nil
		case acp.AgentMethodSessionNew:
			i := n.Add(1)
			return map[string]any{"sessionId": fmt.Sprintf("sess-%d", i), "configOptions": opts(fmt.Sprintf("p/m%d", i))}, nil
		case "session/resume":
			return map[string]any{"configOptions": opts("p/m9")}, nil
		case acp.AgentMethodSessionSetConfigOption:
			var r setSessionConfigOptionRequest
			_ = json.Unmarshal(params, &r)
			if r.Value == "bad" {
				return nil, acp.NewInternalError(nil)
			}
			return map[string]any{}, nil
		}
		return nil, acp.NewMethodNotFound(method)
	}
}

func wantModel(t *testing.T, a *AgentProc, sid acp.SessionId, want string) {
	t.Helper()
	got, ok := a.CurrentModel(sid)
	if want == "" {
		if ok {
			t.Fatalf("CurrentModel(%s) = %q, want unknown", sid, got)
		}
		return
	}
	if !ok || got != want {
		t.Fatalf("CurrentModel(%s) = %q,%v want %q", sid, got, ok, want)
	}
}

func TestCurrentModelPerSession(t *testing.T) {
	pc := startPaired(t, Config{Command: []string{"x"}}, perSessionAgent())
	a := pc.agent
	ctx := context.Background()

	s1, err := a.NewSession(ctx, "/a", &recSink{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	wantModel(t, a, s1, "p/m1")
	// session/new on B must not overwrite A.
	s2, err := a.NewSession(ctx, "/b", &recSink{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	wantModel(t, a, s1, "p/m1")
	wantModel(t, a, s2, "p/m2")
	if _, cur := a.Models(); cur != "p/m2" {
		t.Fatalf("Models current = %q", cur)
	}

	// SetModel updates only its own session.
	if err := a.SetModel(ctx, s2, "p/m1"); err != nil {
		t.Fatal(err)
	}
	wantModel(t, a, s2, "p/m1")
	wantModel(t, a, s1, "p/m1")
	if err := a.SetModel(ctx, s1, "p/m2"); err != nil {
		t.Fatal(err)
	}
	wantModel(t, a, s1, "p/m2")
	wantModel(t, a, s2, "p/m1")

	// A failed switch leaves the model alone.
	if err := a.SetModel(ctx, s1, "bad"); err == nil {
		t.Fatal("want error")
	}
	wantModel(t, a, s1, "p/m2")

	// Resume records its own session's model.
	if err := a.ResumeSession(ctx, "/c", "s3", &recSink{}); err != nil {
		t.Fatal(err)
	}
	wantModel(t, a, "s3", "p/m9")
	wantModel(t, a, s1, "p/m2")

	// A config option update from the agent moves it too.
	a.noteUpdate(s1, acp.SessionUpdate{ConfigOptionUpdate: &acp.SessionConfigOptionUpdate{
		ConfigOptions: []acp.SessionConfigOption{modelOpt("p/m1")}}})
	wantModel(t, a, s1, "p/m1")
	wantModel(t, a, s2, "p/m1")

	// Dropping forgets it; a late SetModel cannot re-add it.
	a.DropSession(s1)
	wantModel(t, a, s1, "")
	wantModel(t, a, "never", "")
}

func modelOpt(cur string) acp.SessionConfigOption {
	var o acp.SessionConfigOption
	b, _ := json.Marshal(map[string]any{
		"type": "select", "id": "model", "name": "Model", "category": "model",
		"currentValue": cur, "options": []map[string]any{{"value": cur, "name": cur}},
	})
	if err := json.Unmarshal(b, &o); err != nil {
		panic(err)
	}
	return o
}
