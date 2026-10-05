package client

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	acp "github.com/coder/acp-go-sdk"
)

// forkAgent fakes an agent advertising fork (with _meta.at when withAt),
// recording every session/fork and session/resume request it sees.
type forkAgent struct {
	mu      sync.Mutex
	withAt  bool
	forks   []map[string]any
	resumes []string
	forkErr bool
	noSid   bool
	broken  bool // session/new and session/resume fail
}

func (f *forkAgent) handle(_ context.Context, method string, p json.RawMessage) (any, *acp.RequestError) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch method {
	case acp.AgentMethodInitialize:
		fork := map[string]any{}
		if f.withAt {
			fork["_meta"] = map[string]any{"at": map[string]any{}}
		}
		return map[string]any{"protocolVersion": acp.ProtocolVersionNumber, "agentCapabilities": map[string]any{
			"sessionCapabilities": map[string]any{"resume": map[string]any{}, "fork": fork},
		}}, nil
	case acp.AgentMethodSessionNew, "session/resume":
		if f.broken {
			return nil, acp.NewInternalError(nil)
		}
	}
	switch method {
	case acp.AgentMethodSessionNew:
		return map[string]any{"sessionId": "parent"}, nil
	case "session/fork":
		var m map[string]any
		_ = json.Unmarshal(p, &m)
		f.forks = append(f.forks, m)
		if f.forkErr {
			return nil, acp.NewInternalError(nil)
		}
		if f.noSid {
			return map[string]any{}, nil
		}
		return map[string]any{"sessionId": "child", "models": map[string]any{
			"availableModels": []map[string]any{{"modelId": "x/a", "name": "A"}}, "currentModelId": "x/a",
		}}, nil
	case "session/resume":
		var m struct{ SessionId string }
		_ = json.Unmarshal(p, &m)
		f.resumes = append(f.resumes, m.SessionId)
		return map[string]any{}, nil
	case acp.AgentMethodSessionSetConfigOption:
		return map[string]any{"configOptions": []any{}}, nil
	}
	return nil, acp.NewMethodNotFound(method)
}

func TestParseCapsFork(t *testing.T) {
	c := parseCaps(json.RawMessage(`{"agentCapabilities":{"sessionCapabilities":{"fork":{"_meta":{"at":{}}}}}}`))
	if !c.ForkSession || !c.ForkAt {
		t.Fatalf("caps %+v", c)
	}
	c = parseCaps(json.RawMessage(`{"agentCapabilities":{"sessionCapabilities":{"fork":{}}}}`))
	if !c.ForkSession || c.ForkAt {
		t.Fatalf("caps %+v", c)
	}
	c = parseCaps(json.RawMessage(`{"agentCapabilities":{}}`))
	if c.ForkSession || c.ForkAt {
		t.Fatalf("caps %+v", c)
	}
}

func TestForkSession(t *testing.T) {
	ctx := context.Background()
	f := &forkAgent{withAt: true}
	pc := startPaired(t, Config{Command: []string{"x"}, Logger: discardLogger()}, f.handle)
	a := pc.agent
	parent, err := a.NewSession(ctx, "/cwd", &recSink{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	sink := &recSink{}
	child, err := a.ForkSession(ctx, "/cwd", parent, "e42", sink)
	if err != nil || child != "child" {
		t.Fatalf("child=%q err=%v", child, err)
	}
	if _, err := a.ForkSession(ctx, "/cwd", parent, "", sink); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	got := f.forks
	f.mu.Unlock()
	if got[0]["sessionId"] != "parent" || got[0]["cwd"] != "/cwd" || got[0]["mcpServers"] == nil {
		t.Fatalf("fork req %v", got[0])
	}
	if m, _ := got[0]["_meta"].(map[string]any); m["at"] != "e42" {
		t.Fatalf("meta %v", got[0]["_meta"])
	}
	if _, ok := got[1]["_meta"]; ok {
		t.Fatalf("no-at fork carried _meta: %v", got[1])
	}
	if a.sinkFor("child") != sink {
		t.Fatal("child sink not registered")
	}
	if _, cur := a.Models(); cur != "x/a" {
		t.Fatalf("models not noted: %q", cur)
	}
	// After a respawn the child is recovered by resuming it.
	stale(a)
	if err := a.SetConfigOption(ctx, child, "c", "v"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.resumes) != 1 || f.resumes[0] != "child" {
		t.Fatalf("resumes %v", f.resumes)
	}
}

func TestForkSessionErrors(t *testing.T) {
	ctx := context.Background()
	// No fork cap at all.
	none := func(_ context.Context, method string, _ json.RawMessage) (any, *acp.RequestError) {
		if method == acp.AgentMethodInitialize {
			return map[string]any{"protocolVersion": acp.ProtocolVersionNumber, "agentCapabilities": map[string]any{}}, nil
		}
		return nil, acp.NewMethodNotFound(method)
	}
	pc := startPaired(t, Config{Command: []string{"x"}, Logger: discardLogger()}, none)
	if _, err := pc.agent.ForkSession(ctx, "/cwd", "p", "", &recSink{}); !errors.Is(err, ErrForkUnsupported) {
		t.Fatalf("err = %v", err)
	}
	// Fork without _meta.at: a fork point is refused.
	f := &forkAgent{}
	pc = startPaired(t, Config{Command: []string{"x"}, Logger: discardLogger()}, f.handle)
	if _, err := pc.agent.ForkSession(ctx, "/cwd", "p", "e1", &recSink{}); !errors.Is(err, ErrForkUnsupported) {
		t.Fatalf("err = %v", err)
	}
	// Agent error.
	f.mu.Lock()
	f.forkErr = true
	f.mu.Unlock()
	if _, err := pc.agent.ForkSession(ctx, "/cwd", "p", "", &recSink{}); err == nil {
		t.Fatal("expected error")
	}
	// Empty sessionId.
	f.mu.Lock()
	f.forkErr, f.noSid = false, true
	f.mu.Unlock()
	if _, err := pc.agent.ForkSession(ctx, "/cwd", "p", "", &recSink{}); err == nil {
		t.Fatal("expected error")
	}
	// Parent cannot be re-established after a respawn: route fails.
	parent, err := pc.agent.NewSession(ctx, "/cwd", &recSink{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.broken = true
	f.mu.Unlock()
	stale(pc.agent)
	if _, err := pc.agent.ForkSession(ctx, "/cwd", parent, "", &recSink{}); err == nil {
		t.Fatalf("expected error")
	}
}
