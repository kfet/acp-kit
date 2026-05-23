package client

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/signal"
	"syscall"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
)

// fakeAgentEnv is set on child invocations of this test binary so TestMain
// runs as a tiny ACP agent over stdin/stdout. ignoreSigintEnv makes the
// fake agent install a SIGINT handler that ignores the signal so the Close
// SIGKILL fallback branch can be exercised deterministically.
const (
	fakeAgentEnv    = "ACP_KIT_FAKE_AGENT"
	ignoreSigintEnv = "ACP_KIT_FAKE_AGENT_IGNORE_SIGINT"
)

func TestMain(m *testing.M) {
	if os.Getenv(fakeAgentEnv) == "1" {
		runFakeAgent()
		return
	}
	os.Exit(m.Run())
}

func runFakeAgent() {
	if os.Getenv(ignoreSigintEnv) == "1" {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGINT)
		go func() {
			for range ch {
				// swallow
			}
		}()
	}
	handler := func(_ context.Context, method string, _ json.RawMessage) (any, *acp.RequestError) {
		switch method {
		case acp.AgentMethodInitialize:
			return map[string]any{
				"protocolVersion": acp.ProtocolVersionNumber,
				"agentCapabilities": map[string]any{
					"loadSession":         true,
					"sessionCapabilities": map[string]any{"list": map[string]any{}, "resume": map[string]any{}},
					"promptCapabilities":  map[string]any{"embeddedContext": true},
					"_meta":               map[string]any{"session.systemPrompt": map[string]any{"version": 1}},
				},
				"authMethods": []map[string]any{{"id": "noop", "name": "Noop"}},
			}, nil
		case acp.AgentMethodSessionNew:
			return map[string]any{"sessionId": "sess-1"}, nil
		case acp.AgentMethodSessionPrompt:
			return map[string]any{"stopReason": "end_turn"}, nil
		}
		return nil, acp.NewMethodNotFound(method)
	}
	_ = acp.NewConnection(handler, os.Stdout, os.Stdin)
	select {} // block until parent kills us
}

func selfExecutable(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	return exe
}

func TestSubprocLifecycle(t *testing.T) {
	exe := selfExecutable(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a, err := Start(ctx, Config{
		Command: []string{exe, "-test.run", "^$"},
		Env:     append(os.Environ(), fakeAgentEnv+"=1"),
		Stderr:  io.Discard,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	caps := a.Caps()
	if !caps.SystemPrompt || !caps.ListSessions {
		t.Fatalf("caps: %#v", caps)
	}
	cwd := t.TempDir()
	sid, err := a.NewSession(ctx, cwd, &recSink{}, nil)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if _, err := a.Prompt(ctx, sid, []acp.ContentBlock{acp.TextBlock("hi")}); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	start := time.Now()
	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("Close too slow: %s", d)
	}
}

func TestSubprocKillFallback(t *testing.T) {
	exe := selfExecutable(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a, err := Start(ctx, Config{
		Command:    []string{exe, "-test.run", "^$"},
		Env:        append(os.Environ(), fakeAgentEnv+"=1", ignoreSigintEnv+"=1"),
		Stderr:     io.Discard,
		CloseGrace: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Don't even use it — go straight to Close to exercise the SIGINT→SIGKILL fallback.
	start := time.Now()
	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("Close too slow: %s", d)
	}
}
