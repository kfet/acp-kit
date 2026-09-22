package client

import (
	"context"
	"encoding/json"
	"testing"

	acp "github.com/coder/acp-go-sdk"
)

func TestParseAgentInfo(t *testing.T) {
	title := "Fir"
	raw, _ := json.Marshal(map[string]any{"agentInfo": acp.Implementation{Name: "fir", Title: &title, Version: "1.2.3"}})
	if got := parseAgentInfo(raw); got != (AgentInfo{Name: "fir", Title: "Fir", Version: "1.2.3"}) {
		t.Fatalf("parseAgentInfo = %#v", got)
	}
	if got := parseAgentInfo(json.RawMessage(`{"agentInfo":{"name":"x","version":"0"}}`)); got != (AgentInfo{Name: "x", Version: "0"}) {
		t.Fatalf("no title = %#v", got)
	}
	if got := parseAgentInfo(json.RawMessage(`{}`)); got != (AgentInfo{}) {
		t.Fatalf("absent = %#v", got)
	}
}

// statsAgent reports agentInfo at initialize and a thought_level
// option on session/new and session/resume.
func statsAgent(_ context.Context, method string, _ json.RawMessage) (any, *acp.RequestError) {
	opts := []map[string]any{{
		"type": "select", "id": "thinking", "name": "Thinking", "category": "thought_level",
		"currentValue": "medium",
		"options":      []map[string]any{{"value": "low", "name": "Low"}, {"value": "medium", "name": "Medium"}},
	}}
	switch method {
	case acp.AgentMethodInitialize:
		return map[string]any{
			"protocolVersion": acp.ProtocolVersionNumber,
			"agentInfo":       map[string]any{"name": "fir", "version": "1.18.4"},
		}, nil
	case acp.AgentMethodSessionNew:
		return map[string]any{"sessionId": "s1", "configOptions": opts}, nil
	case "session/resume":
		return map[string]any{"configOptions": opts}, nil
	}
	return nil, acp.NewMethodNotFound(method)
}

func TestSessionStats(t *testing.T) {
	pc := startPaired(t, Config{Command: []string{"x"}}, statsAgent)
	a := pc.agent
	ctx := context.Background()
	if got := a.AgentInfo(); got.Name != "fir" || got.Version != "1.18.4" {
		t.Fatalf("AgentInfo = %#v", got)
	}
	sid, err := a.NewSession(ctx, "/cwd", &recSink{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if st, ok := a.SessionStats(sid); !ok || st.Thinking != "medium" {
		t.Fatalf("after new: %#v %v", st, ok)
	}
	// A usage_update without cost, then with cost.
	_ = a.sessionUpdate(ctx, acp.SessionNotification{SessionId: sid, Update: acp.SessionUpdate{
		UsageUpdate: &acp.SessionUsageUpdate{Used: 10, Size: 100},
	}})
	if st, _ := a.SessionStats(sid); st.ContextUsed != 10 || st.ContextSize != 100 || st.Cost != nil {
		t.Fatalf("usage no cost: %#v", st)
	}
	_ = a.sessionUpdate(ctx, acp.SessionNotification{SessionId: sid, Update: acp.SessionUpdate{
		UsageUpdate: &acp.SessionUsageUpdate{Used: 20, Size: 100, Cost: &acp.Cost{Amount: 0.5, Currency: "USD"}},
	}})
	cat := acp.SessionConfigOptionCategoryThoughtLevel
	_ = a.sessionUpdate(ctx, acp.SessionNotification{SessionId: sid, Update: acp.SessionUpdate{
		ConfigOptionUpdate: &acp.SessionConfigOptionUpdate{ConfigOptions: []acp.SessionConfigOption{
			{Select: &acp.SessionConfigOptionSelect{Id: "thinking", Category: &cat, CurrentValue: "high"}},
		}},
	}})
	st, _ := a.SessionStats(sid)
	if st.Thinking != "high" || st.ContextUsed != 20 || st.Cost == nil || *st.Cost != (Cost{0.5, "USD"}) {
		t.Fatalf("after updates: %#v", st)
	}
	// A config update with no thought_level leaves thinking alone.
	_ = a.sessionUpdate(ctx, acp.SessionNotification{SessionId: sid, Update: acp.SessionUpdate{
		ConfigOptionUpdate: &acp.SessionConfigOptionUpdate{},
	}})
	if st, _ := a.SessionStats(sid); st.Thinking != "high" {
		t.Fatalf("thinking lost: %#v", st)
	}
	a.DropSession(sid)
	if _, ok := a.SessionStats(sid); ok {
		t.Fatal("stats survive DropSession")
	}
	// A late update for the dropped session adds nothing back.
	_ = a.sessionUpdate(ctx, acp.SessionNotification{SessionId: sid, Update: acp.SessionUpdate{
		UsageUpdate: &acp.SessionUsageUpdate{Used: 1, Size: 2},
	}})
	if _, ok := a.SessionStats(sid); ok {
		t.Fatal("late update re-added stats")
	}
	if err := a.ResumeSession(ctx, "/cwd", "s2", &recSink{}); err != nil {
		t.Fatal(err)
	}
	if st, ok := a.SessionStats("s2"); !ok || st.Thinking != "medium" {
		t.Fatalf("after resume: %#v %v", st, ok)
	}
}
