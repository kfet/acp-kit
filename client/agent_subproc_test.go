package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
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

// Fake-agent knobs, all read from the child's environment.
const (
	fakeCapsEnv       = "ACP_KIT_FAKE_CAPS"        // "" = resume+load, "load" = load only, "none"
	fakeResumeFailEnv = "ACP_KIT_FAKE_RESUME_FAIL" // "1": session/resume errors
	fakeLoadFailEnv   = "ACP_KIT_FAKE_LOAD_FAIL"   // "1": session/load errors
	fakeBrokenEnv     = "ACP_KIT_FAKE_BROKEN"      // path: if it exists, exit 1 at startup
	fakeDieOnEnv      = "ACP_KIT_FAKE_DIE_ON"      // path: exit 4 on the method named in it
	fakeHangOnEnv     = "ACP_KIT_FAKE_HANG_ON"     // path: never answer the method named in it
)

// runFakeAgent is a tiny ACP agent. Prompt text drives failure modes:
// "die" exits 3 after a stderr line, "hang" never answers, "closeout"
// closes stdout and lingers; anything else streams one chunk and ends.
func runFakeAgent() {
	if p := os.Getenv(fakeBrokenEnv); p != "" {
		if _, err := os.Stat(p); err == nil {
			fmt.Fprintln(os.Stderr, "fake agent: broken on purpose")
			os.Exit(1)
		}
	}
	if os.Getenv(ignoreSigintEnv) == "1" {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGINT)
		go func() {
			for range ch {
				// swallow
			}
		}()
	}
	caps := map[string]any{
		"loadSession":         true,
		"sessionCapabilities": map[string]any{"list": map[string]any{}, "resume": map[string]any{}},
		"promptCapabilities":  map[string]any{"embeddedContext": true, "image": true, "audio": true},
		"_meta":               map[string]any{"session.systemPrompt": map[string]any{"version": 1}},
	}
	switch os.Getenv(fakeCapsEnv) {
	case "load":
		caps["sessionCapabilities"] = map[string]any{}
	case "none":
		caps["loadSession"] = false
		caps["sessionCapabilities"] = map[string]any{}
	}
	fmt.Fprintf(os.Stderr, "fake agent up pid=%d\n", os.Getpid())
	var conn *acp.Connection
	say := func(ctx context.Context, sid acp.SessionId, text string) {
		_ = conn.SendNotification(ctx, acp.ClientMethodSessionUpdate, acp.SessionNotification{
			SessionId: sid, Update: acp.UpdateAgentMessageText(text),
		})
	}
	handler := func(ctx context.Context, method string, params json.RawMessage) (any, *acp.RequestError) {
		if p := os.Getenv(fakeDieOnEnv); p != "" {
			if b, err := os.ReadFile(p); err == nil && string(b) == method {
				os.Exit(4)
			}
		}
		if p := os.Getenv(fakeHangOnEnv); p != "" {
			if b, err := os.ReadFile(p); err == nil && string(b) == method {
				select {}
			}
		}
		switch method {
		case acp.AgentMethodInitialize:
			return map[string]any{
				"protocolVersion":   acp.ProtocolVersionNumber,
				"agentCapabilities": caps,
				"authMethods":       []map[string]any{{"id": "noop", "name": "Noop"}},
			}, nil
		case acp.AgentMethodSessionNew:
			return map[string]any{"sessionId": fmt.Sprintf("sess-%d", os.Getpid())}, nil
		case "session/resume":
			if os.Getenv(fakeResumeFailEnv) == "1" {
				return nil, acp.NewInternalError(map[string]any{"error": "no resume"})
			}
			return map[string]any{}, nil
		case acp.AgentMethodSessionLoad:
			if os.Getenv(fakeLoadFailEnv) == "1" {
				return nil, acp.NewInternalError(map[string]any{"error": "no load"})
			}
			var p acp.LoadSessionRequest
			_ = json.Unmarshal(params, &p)
			say(ctx, p.SessionId, "replayed history")
			return map[string]any{}, nil
		case acp.AgentMethodSessionPrompt:
			var p acp.PromptRequest
			_ = json.Unmarshal(params, &p)
			text := ""
			if len(p.Prompt) > 0 && p.Prompt[0].Text != nil {
				text = p.Prompt[0].Text.Text
			}
			switch text {
			case "die":
				fmt.Fprintln(os.Stderr, "fake agent: dying now")
				os.Exit(3)
			case "hang":
				select {}
			case "closeout":
				_ = os.Stdout.Close()
				time.Sleep(time.Hour)
			}
			say(ctx, p.SessionId, "reply from "+strconv.Itoa(os.Getpid()))
			return map[string]any{"stopReason": "end_turn"}, nil
		}
		return nil, acp.NewMethodNotFound(method)
	}
	conn = acp.NewConnection(handler, os.Stdout, os.Stdin)
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
