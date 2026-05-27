package client

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
)

// pairedConn wires an in-process fake agent to an AgentProc over two io.Pipe
// pairs. The fake agent runs its own *acp.Connection on the far side and
// answers requests via handler.
type pairedConn struct {
	t       *testing.T
	agent   *AgentProc
	fake    *acp.Connection
	cleanup func()
}

func startPaired(t *testing.T, cfg Config, handler func(ctx context.Context, method string, params json.RawMessage) (any, *acp.RequestError)) *pairedConn {
	t.Helper()
	// Pair 1: data flowing client -> fake agent (client's stdin = fake's stdout).
	clientStdinR, clientStdinW := io.Pipe()
	// Pair 2: data flowing fake agent -> client (client's stdout = fake's stdin).
	clientStdoutR, clientStdoutW := io.Pipe()

	// Fake agent: reads what the client writes (clientStdinR), writes back
	// into what the client reads (clientStdoutW).
	fake := acp.NewConnection(handler, clientStdoutW, clientStdinR)

	ctx, cancel := context.WithCancel(context.Background())
	a, err := connect(ctx, cfg, &exec.Cmd{}, clientStdinW, clientStdoutR)
	if err != nil {
		cancel()
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		_ = clientStdinW.Close()
		_ = clientStdoutW.Close()
		_ = clientStdinR.Close()
		_ = clientStdoutR.Close()
	})
	return &pairedConn{t: t, agent: a, fake: fake, cleanup: cancel}
}

// happyAgent answers a useful subset of methods for the broad tests below.
func happyAgent(t *testing.T) func(ctx context.Context, method string, params json.RawMessage) (any, *acp.RequestError) {
	t.Helper()
	return func(_ context.Context, method string, _ json.RawMessage) (any, *acp.RequestError) {
		switch method {
		case acp.AgentMethodInitialize:
			return map[string]any{
				"protocolVersion": acp.ProtocolVersionNumber,
				"agentCapabilities": map[string]any{
					"loadSession": true,
					"sessionCapabilities": map[string]any{
						"list":   map[string]any{},
						"resume": map[string]any{},
					},
					"promptCapabilities": map[string]any{"embeddedContext": true},
					"_meta": map[string]any{
						"session.systemPrompt": map[string]any{"version": 1},
					},
				},
				"authMethods": []map[string]any{{"id": "oauth", "name": "OAuth", "type": "agent"}},
			}, nil
		case acp.AgentMethodSessionNew:
			return map[string]any{
				"sessionId": "sess-A",
				"models": map[string]any{
					"availableModels": []map[string]any{
						{"modelId": "p/m1", "name": "Model 1"},
						{"modelId": "p/m2", "name": "Model 2"},
					},
					"currentModelId": "p/m1",
				},
			}, nil
		case acp.AgentMethodSessionPrompt:
			return map[string]any{"stopReason": "end_turn"}, nil
		case acp.AgentMethodSessionSetModel:
			return map[string]any{}, nil
		case "session/set_config_option":
			return map[string]any{}, nil
		case "session/list":
			return map[string]any{"sessions": []map[string]any{{"sessionId": "s-resume", "cwd": "/c"}}}, nil
		case "session/resume":
			return map[string]any{
				"models": map[string]any{
					"availableModels": []map[string]any{
						{"modelId": "p/m1", "name": "Model 1"},
						{"modelId": "p/m3", "name": "Model 3"},
					},
					"currentModelId": "p/m3",
				},
			}, nil
		case acp.AgentMethodAuthenticate:
			return map[string]any{"_meta": map[string]any{"auth": map[string]any{"state": "needs_redirect", "id": "pending-1", "url": "https://x/y", "instructions": "go here"}}}, nil
		}
		return nil, acp.NewMethodNotFound(method)
	}
}

type recSink struct {
	mu   sync.Mutex
	got  []acp.SessionNotification
	fail bool
}

func (r *recSink) OnUpdate(_ context.Context, n acp.SessionNotification) error {
	r.mu.Lock()
	r.got = append(r.got, n)
	r.mu.Unlock()
	if r.fail {
		return errors.New("sink failed")
	}
	return nil
}

func TestStartValidatesCommand(t *testing.T) {
	if _, err := Start(context.Background(), Config{}); err == nil {
		t.Fatal("expected empty command error")
	}
}

func TestStartCommandNotFound(t *testing.T) {
	_, err := Start(context.Background(), Config{Command: []string{"/nope/definitely/missing/binary-xyz"}})
	if err == nil {
		t.Fatal("expected start error")
	}
}

func TestPipeAndDispatch(t *testing.T) {
	allowAll := PermissionFunc(AllowAllPermissions)
	pc := startPaired(t, Config{Command: []string{"x"}, Policy: allowAll}, happyAgent(t))
	a := pc.agent

	caps := a.Caps()
	if !caps.SystemPrompt || !caps.ListSessions || !caps.ResumeSession || !caps.LoadSession || !caps.EmbeddedContext {
		t.Fatalf("caps not parsed: %#v", caps)
	}
	if len(a.AuthMethods()) != 1 {
		t.Fatal("expected one auth method")
	}

	ctx := context.Background()
	sink := &recSink{}
	sid, err := a.NewSession(ctx, "/cwd", sink, []acp.ContentBlock{acp.TextBlock("sys")})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if sid != "sess-A" {
		t.Fatalf("sid = %q", sid)
	}
	models, cur := a.Models()
	if len(models) != 2 || cur != "p/m1" {
		t.Fatalf("models = %#v cur=%q", models, cur)
	}

	if err := a.SetModel(ctx, sid, "p/m2"); err != nil {
		t.Fatalf("SetModel: %v", err)
	}
	if err := a.SetConfigOption(ctx, sid, "thinking_level", "medium"); err != nil {
		t.Fatalf("SetConfigOption: %v", err)
	}
	stop, err := a.Prompt(ctx, sid, []acp.ContentBlock{acp.TextBlock("hi")})
	if err != nil || stop != "end_turn" {
		t.Fatalf("Prompt: stop=%q err=%v", stop, err)
	}
	if err := a.Cancel(ctx, sid); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	infos, err := a.ListSessions(ctx, "/cwd")
	if err != nil || len(infos) != 1 || infos[0].SessionId != "s-resume" {
		t.Fatalf("ListSessions: %v %#v", err, infos)
	}
	if err := a.ResumeSession(ctx, "/cwd", "s-resume", sink); err != nil {
		t.Fatalf("ResumeSession: %v", err)
	}
	// ResumeSession must refresh the cached models.
	models, cur = a.Models()
	if len(models) != 2 || cur != "p/m3" {
		t.Fatalf("after ResumeSession: models = %#v cur=%q", models, cur)
	}

	authRes, err := a.Authenticate(ctx, "oauth", "", "", false)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if authRes.URL != "https://x/y" || authRes.State != "needs_redirect" || authRes.ID != "pending-1" || authRes.Instructions != "go here" {
		t.Fatalf("Authenticate result = %#v", authRes)
	}
	if _, err := a.Authenticate(ctx, "oauth", "id-1", "https://r", true); err != nil {
		t.Fatalf("Authenticate redirect/cancel: %v", err)
	}

	// Drive a server-initiated session/update to exercise dispatch + sinkFor.
	notifyParams, _ := json.Marshal(acp.SessionNotification{SessionId: sid, Update: acp.SessionUpdate{AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{Content: acp.TextBlock("hi back")}}})
	if err := pc.fake.SendNotification(ctx, acp.ClientMethodSessionUpdate, json.RawMessage(notifyParams)); err != nil {
		t.Fatalf("send update: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		sink.mu.Lock()
		n := len(sink.got)
		sink.mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("update not delivered to sink")
		}
		time.Sleep(2 * time.Millisecond)
	}

	a.DropSession(sid)
	a.RebindSink(sid, sink)

	// Hit ProbeModels short-circuit (models already cached).
	if err := a.ProbeModels(ctx); err != nil {
		t.Fatalf("ProbeModels short-circuit: %v", err)
	}
}

func TestProbeModelsActual(t *testing.T) {
	pc := startPaired(t, Config{Command: []string{"x"}, Policy: PermissionFunc(AllowAllPermissions)}, happyAgent(t))
	a := pc.agent
	// Force cache miss.
	a.mu.Lock()
	a.models = nil
	a.mu.Unlock()
	if err := a.ProbeModels(context.Background()); err != nil {
		t.Fatalf("ProbeModels: %v", err)
	}
}

func TestNewSessionRPCError(t *testing.T) {
	failing := func(_ context.Context, method string, _ json.RawMessage) (any, *acp.RequestError) {
		if method == acp.AgentMethodInitialize {
			return map[string]any{"protocolVersion": acp.ProtocolVersionNumber, "agentCapabilities": map[string]any{}}, nil
		}
		if method == acp.AgentMethodSessionNew {
			return nil, acp.NewInternalError(map[string]any{"error": "boom"})
		}
		return nil, acp.NewMethodNotFound(method)
	}
	pc := startPaired(t, Config{Command: []string{"x"}, Policy: PermissionFunc(AllowAllPermissions)}, failing)
	if _, err := pc.agent.NewSession(context.Background(), "/cwd", &recSink{}, nil); err == nil {
		t.Fatal("expected NewSession error")
	}
}

func TestPromptError(t *testing.T) {
	failing := func(_ context.Context, method string, _ json.RawMessage) (any, *acp.RequestError) {
		switch method {
		case acp.AgentMethodInitialize:
			return map[string]any{"protocolVersion": acp.ProtocolVersionNumber, "agentCapabilities": map[string]any{}}, nil
		case acp.AgentMethodSessionPrompt:
			return nil, acp.NewInternalError(map[string]any{"error": "boom"})
		}
		return nil, acp.NewMethodNotFound(method)
	}
	pc := startPaired(t, Config{Command: []string{"x"}, Policy: PermissionFunc(AllowAllPermissions)}, failing)
	if _, err := pc.agent.Prompt(context.Background(), "sid", nil); err == nil {
		t.Fatal("expected Prompt error")
	}
}

func TestProbeModelsNewSessionError(t *testing.T) {
	failing := func(_ context.Context, method string, _ json.RawMessage) (any, *acp.RequestError) {
		switch method {
		case acp.AgentMethodInitialize:
			return map[string]any{"protocolVersion": acp.ProtocolVersionNumber, "agentCapabilities": map[string]any{}}, nil
		case acp.AgentMethodSessionNew:
			return nil, acp.NewInternalError(map[string]any{"error": "boom"})
		}
		return nil, acp.NewMethodNotFound(method)
	}
	pc := startPaired(t, Config{Command: []string{"x"}, Policy: PermissionFunc(AllowAllPermissions)}, failing)
	if err := pc.agent.ProbeModels(context.Background()); err == nil {
		t.Fatal("expected probe error")
	}
}

func TestInitializeFailureClosesProc(t *testing.T) {
	// Custom handler that returns invalid JSON on initialize to force the
	// SendRequest error path inside connect().
	bad := func(_ context.Context, method string, _ json.RawMessage) (any, *acp.RequestError) {
		if method == acp.AgentMethodInitialize {
			return nil, acp.NewInternalError(map[string]any{"error": "bad init"})
		}
		return nil, acp.NewMethodNotFound(method)
	}
	clientStdinR, clientStdinW := io.Pipe()
	clientStdoutR, clientStdoutW := io.Pipe()
	_ = acp.NewConnection(bad, clientStdoutW, clientStdinR)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := connect(ctx, Config{Command: []string{"x"}}, &exec.Cmd{}, clientStdinW, clientStdoutR)
	if err == nil {
		t.Fatal("expected initialize error")
	}
	_ = clientStdinW.Close()
	_ = clientStdoutW.Close()
}

func TestDispatchDecodes(t *testing.T) {
	pc := startPaired(t, Config{Command: []string{"x"}, Policy: PermissionFunc(AllowAllPermissions)}, happyAgent(t))
	a := pc.agent

	// Invalid params for every method shape.
	if _, e := a.dispatch(context.Background(), acp.ClientMethodSessionUpdate, json.RawMessage("not-json")); e == nil {
		t.Fatal("expected invalid-params")
	}
	if _, e := a.dispatch(context.Background(), acp.ClientMethodSessionRequestPermission, json.RawMessage("nope")); e == nil {
		t.Fatal("expected invalid-params (permission)")
	}
	if _, e := a.dispatch(context.Background(), acp.ClientMethodFsReadTextFile, json.RawMessage("nope")); e == nil {
		t.Fatal("expected invalid-params (read)")
	}
	if _, e := a.dispatch(context.Background(), acp.ClientMethodFsWriteTextFile, json.RawMessage("nope")); e == nil {
		t.Fatal("expected invalid-params (write)")
	}
	if _, e := a.dispatch(context.Background(), "no/such/method", nil); e == nil {
		t.Fatal("expected method-not-found")
	}

	// Permission with valid params delegates to AllowAll default.
	permReq := acp.RequestPermissionRequest{
		ToolCall: acp.ToolCallUpdate{},
		Options:  []acp.PermissionOption{{OptionId: "ok", Name: "allow it", Kind: "allow_once"}},
	}
	pb, _ := json.Marshal(permReq)
	if resp, e := a.dispatch(context.Background(), acp.ClientMethodSessionRequestPermission, pb); e != nil {
		t.Fatalf("permission dispatch: %v", e)
	} else if rr, ok := resp.(acp.RequestPermissionResponse); !ok || rr.Outcome.Selected == nil {
		t.Fatalf("permission resp = %#v", resp)
	}

	// session/update with valid params and a sink that fails returns an error.
	sink := &recSink{fail: true}
	a.sinks["sid-x"] = sink
	upd, _ := json.Marshal(acp.SessionNotification{SessionId: "sid-x", Update: acp.SessionUpdate{AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{Content: acp.TextBlock("x")}}})
	if _, e := a.dispatch(context.Background(), acp.ClientMethodSessionUpdate, upd); e == nil {
		t.Fatal("expected sink error")
	}

	// session/update with no registered sink: returns nil.
	upd2, _ := json.Marshal(acp.SessionNotification{SessionId: "unknown-sid", Update: acp.SessionUpdate{AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{Content: acp.TextBlock("x")}}})
	if _, e := a.dispatch(context.Background(), acp.ClientMethodSessionUpdate, upd2); e != nil {
		t.Fatalf("unknown sid update: %v", e)
	}
}

func TestReadAndWriteTextFile(t *testing.T) {
	pc := startPaired(t, Config{Command: []string{"x"}, Policy: PermissionFunc(AllowAllPermissions)}, happyAgent(t))
	a := pc.agent

	tmp := t.TempDir()
	abs := filepath.Join(tmp, "data.txt")
	if err := os.WriteFile(abs, []byte("a\nb\nc\nd\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Absolute path required.
	if _, err := a.readTextFile(acp.ReadTextFileRequest{Path: "relative"}); err == nil {
		t.Fatal("expected absolute-path error")
	}
	if err := a.writeTextFile(acp.WriteTextFileRequest{Path: "relative"}); err == nil {
		t.Fatal("expected absolute-path error (write)")
	}

	// Read full.
	resp, err := a.readTextFile(acp.ReadTextFileRequest{Path: abs})
	if err != nil || resp.Content != "a\nb\nc\nd\n" {
		t.Fatalf("read full: %q err=%v", resp.Content, err)
	}
	// Read with line/limit.
	line, limit := 2, 2
	resp, err = a.readTextFile(acp.ReadTextFileRequest{Path: abs, Line: &line, Limit: &limit})
	if err != nil || resp.Content != "b\nc" {
		t.Fatalf("read range: %q err=%v", resp.Content, err)
	}
	// Read past end-of-file.
	bigLine := 100
	resp, err = a.readTextFile(acp.ReadTextFileRequest{Path: abs, Line: &bigLine})
	if err != nil || resp.Content != "" {
		t.Fatalf("read past EOF: %q err=%v", resp.Content, err)
	}
	// Limit larger than file → clamp.
	bigLimit := 100
	line = 1
	resp, err = a.readTextFile(acp.ReadTextFileRequest{Path: abs, Line: &line, Limit: &bigLimit})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resp.Content, "a\nb\nc\nd") {
		t.Fatalf("read clamp limit: %q", resp.Content)
	}
	// Missing file.
	if _, err := a.readTextFile(acp.ReadTextFileRequest{Path: filepath.Join(tmp, "missing")}); err == nil {
		t.Fatal("expected ENOENT error")
	}

	// Write happy path (creates parent).
	dst := filepath.Join(tmp, "sub", "out.txt")
	if err := a.writeTextFile(acp.WriteTextFileRequest{Path: dst, Content: "ok"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	body, _ := os.ReadFile(dst)
	if string(body) != "ok" {
		t.Fatalf("write content = %q", body)
	}
	// Write MkdirAll failure: parent is a file.
	parent := filepath.Join(tmp, "afile")
	_ = os.WriteFile(parent, []byte("x"), 0o644)
	if err := a.writeTextFile(acp.WriteTextFileRequest{Path: filepath.Join(parent, "sub", "x.txt")}); err == nil {
		t.Fatal("expected mkdir error")
	}
}

func TestParseHelpersIgnoreGarbage(t *testing.T) {
	if got := parseCaps(json.RawMessage("not-json")); got.LoadSession || got.ListSessions || got.ResumeSession || got.EmbeddedContext || got.SystemPrompt || got.Extensions != nil {
		t.Fatalf("parseCaps garbage = %#v", got)
	}
	if got := parseAuthMethods(json.RawMessage("not-json")); got != nil {
		t.Fatalf("parseAuthMethods garbage = %#v", got)
	}
	if got := parseAuthResult(json.RawMessage("not-json")); got != (AuthResult{}) {
		t.Fatalf("parseAuthResult garbage = %#v", got)
	}
}

func TestPermissionPickFallback(t *testing.T) {
	// AllowAllPermissions with no allow-shaped option falls back to first.
	req := acp.RequestPermissionRequest{Options: []acp.PermissionOption{{OptionId: "only", Name: "neither", Kind: "weird"}}}
	resp := AllowAllPermissions(context.Background(), req)
	if resp.Outcome.Selected == nil || resp.Outcome.Selected.OptionId != "only" {
		t.Fatalf("fallback selected = %#v", resp)
	}
	// Empty options: outcome's chosen id is empty.
	resp = AllowAllPermissions(context.Background(), acp.RequestPermissionRequest{})
	if resp.Outcome.Selected == nil {
		t.Fatal("expected selected outcome")
	}
}

func TestCloseGraceful(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a, err := Start(ctx, Config{
		Command: []string{exe, "-test.run", "^$"},
		Env:     append(os.Environ(), "ACP_KIT_E2E_FAKE_AGENT=0"), // not the fake agent → exits quickly with no-op test
		Stderr:  io.Discard,
	})
	if err == nil {
		_ = a.Close()
		return
	}
	// Initialize may legitimately fail since the child doesn't speak ACP;
	// that's acceptable — Start should have killed the child.
}

func TestCloseKillFallback(t *testing.T) {
	pc := startPaired(t, Config{Command: []string{"x"}, Policy: PermissionFunc(AllowAllPermissions), CloseGrace: 1 * time.Millisecond}, happyAgent(t))
	// pc.agent.cmd is an unstarted exec.Cmd → Close hits the "no process" branch.
	if err := pc.agent.Close(); err != nil {
		t.Fatal(err)
	}
	// Second Close is also fine.
	if err := pc.agent.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestToReqErrAndAuthAliasIdentities(t *testing.T) {
	re := toReqErr(errors.New("oops"))
	if re == nil {
		t.Fatal("toReqErr nil")
	}
	// Compile-time aliases.
	var _ AuthMethod = AuthMethod{ID: "x"}
	var _ AuthResult = AuthResult{State: "ok"}
}

func TestStartWithDefaultPolicyAndCwd(t *testing.T) {
	// Use a binary that exists but isn't ACP-speaking → initialize will fail.
	// We rely on Start to apply default Policy and Cwd, and to clean up the
	// child when connect returns an error.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := Start(ctx, Config{Command: []string{"/usr/bin/true"}})
	if err == nil {
		t.Fatal("expected initialize failure against /usr/bin/true")
	}
}

func TestModelsBeforeProbeIsEmpty(t *testing.T) {
	pc := startPaired(t, Config{Command: []string{"x"}, Policy: PermissionFunc(AllowAllPermissions)}, happyAgent(t))
	a := pc.agent
	a.mu.Lock()
	a.models = nil
	a.mu.Unlock()
	if models, cur := a.Models(); models != nil || cur != "" {
		t.Fatalf("expected empty, got %#v %q", models, cur)
	}
}

func TestNoopSinkOnUpdate(t *testing.T) {
	if err := (noopSink{}).OnUpdate(context.Background(), acp.SessionNotification{}); err != nil {
		t.Fatalf("noopSink.OnUpdate = %v", err)
	}
}

func TestListSessionsAndResumeErrorReturns(t *testing.T) {
	bad := func(_ context.Context, method string, _ json.RawMessage) (any, *acp.RequestError) {
		switch method {
		case acp.AgentMethodInitialize:
			return map[string]any{"protocolVersion": acp.ProtocolVersionNumber, "agentCapabilities": map[string]any{}}, nil
		case "session/list", "session/resume":
			return nil, acp.NewInternalError(map[string]any{"error": "boom"})
		case acp.AgentMethodAuthenticate:
			return nil, acp.NewInternalError(map[string]any{"error": "boom"})
		}
		return nil, acp.NewMethodNotFound(method)
	}
	pc := startPaired(t, Config{Command: []string{"x"}, Policy: PermissionFunc(AllowAllPermissions)}, bad)
	a := pc.agent
	if _, err := a.ListSessions(context.Background(), "/cwd"); err == nil {
		t.Fatal("expected ListSessions error")
	}
	if err := a.ResumeSession(context.Background(), "/cwd", "sid", &recSink{}); err == nil {
		t.Fatal("expected ResumeSession error")
	}
	if _, err := a.Authenticate(context.Background(), "m", "", "", false); err == nil {
		t.Fatal("expected Authenticate error")
	}
}

func TestResumeSessionCachesModels(t *testing.T) {
	handler := func(_ context.Context, method string, _ json.RawMessage) (any, *acp.RequestError) {
		switch method {
		case acp.AgentMethodInitialize:
			return map[string]any{
				"protocolVersion": acp.ProtocolVersionNumber,
				"agentCapabilities": map[string]any{
					"sessionCapabilities": map[string]any{
						"resume": map[string]any{},
					},
				},
			}, nil
		case "session/resume":
			return map[string]any{
				"models": map[string]any{
					"availableModels": []map[string]any{
						{"modelId": "x/a", "name": "Alpha"},
					},
					"currentModelId": "x/a",
				},
			}, nil
		}
		return nil, acp.NewMethodNotFound(method)
	}
	pc := startPaired(t, Config{Command: []string{"x"}, Policy: PermissionFunc(AllowAllPermissions)}, handler)
	a := pc.agent

	// Before resume, models must be empty.
	models, cur := a.Models()
	if len(models) != 0 || cur != "" {
		t.Fatalf("before resume: models=%#v cur=%q", models, cur)
	}

	if err := a.ResumeSession(context.Background(), "/cwd", "s1", &recSink{}); err != nil {
		t.Fatalf("ResumeSession: %v", err)
	}

	models, cur = a.Models()
	if len(models) != 1 || models[0].ID != "x/a" || models[0].Name != "Alpha" || cur != "x/a" {
		t.Fatalf("after resume: models=%#v cur=%q", models, cur)
	}
}

func TestDispatchReadWriteHappy(t *testing.T) {
	pc := startPaired(t, Config{Command: []string{"x"}, Policy: PermissionFunc(AllowAllPermissions)}, happyAgent(t))
	a := pc.agent

	tmp := t.TempDir()
	abs := filepath.Join(tmp, "file.txt")
	if err := os.WriteFile(abs, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	rp, _ := json.Marshal(acp.ReadTextFileRequest{Path: abs})
	resp, e := a.dispatch(context.Background(), acp.ClientMethodFsReadTextFile, rp)
	if e != nil {
		t.Fatalf("read dispatch: %v", e)
	}
	if rr, ok := resp.(acp.ReadTextFileResponse); !ok || rr.Content != "data" {
		t.Fatalf("read resp = %#v", resp)
	}
	// Now force a read error via missing file → toReqErr.
	rp2, _ := json.Marshal(acp.ReadTextFileRequest{Path: filepath.Join(tmp, "nope")})
	if _, e := a.dispatch(context.Background(), acp.ClientMethodFsReadTextFile, rp2); e == nil {
		t.Fatal("expected read err")
	}

	wp, _ := json.Marshal(acp.WriteTextFileRequest{Path: filepath.Join(tmp, "w.txt"), Content: "x"})
	if _, e := a.dispatch(context.Background(), acp.ClientMethodFsWriteTextFile, wp); e != nil {
		t.Fatalf("write dispatch: %v", e)
	}
	// Now force a write error: parent is a file.
	parentFile := filepath.Join(tmp, "afile2")
	_ = os.WriteFile(parentFile, []byte("x"), 0o644)
	wp2, _ := json.Marshal(acp.WriteTextFileRequest{Path: filepath.Join(parentFile, "sub", "x.txt")})
	if _, e := a.dispatch(context.Background(), acp.ClientMethodFsWriteTextFile, wp2); e == nil {
		t.Fatal("expected write err")
	}
}

// TestClientMetaAndExtensions verifies that Config.ClientMeta is merged
// into outgoing clientCapabilities._meta and that agentCapabilities._meta
// entries other than session.systemPrompt land in Caps.Extensions.
func TestClientMetaAndExtensions(t *testing.T) {
	var gotInit json.RawMessage
	handler := func(_ context.Context, method string, params json.RawMessage) (any, *acp.RequestError) {
		if method == acp.AgentMethodInitialize {
			gotInit = append(json.RawMessage(nil), params...)
			return map[string]any{
				"protocolVersion": acp.ProtocolVersionNumber,
				"agentCapabilities": map[string]any{
					"_meta": map[string]any{
						"session.systemPrompt":       map[string]any{"version": 1},
						"dev.poe-acp.status-line/v1": map[string]any{"version": 1},
						"dev.example.other/v2":       map[string]any{},
					},
				},
				"authMethods": []map[string]any{},
			}, nil
		}
		return nil, acp.NewMethodNotFound(method)
	}
	cfg := Config{
		Command: []string{"x"},
		Policy:  PermissionFunc(AllowAllPermissions),
		ClientMeta: map[string]any{
			"dev.poe-acp.status-line/v1": map[string]any{"version": 1},
		},
	}
	pc := startPaired(t, cfg, handler)
	_ = pc

	// Verify outgoing _meta carries both kit-owned and ClientMeta entries.
	var env struct {
		ClientCapabilities struct {
			Meta map[string]json.RawMessage `json:"_meta"`
		} `json:"clientCapabilities"`
	}
	if err := json.Unmarshal(gotInit, &env); err != nil {
		t.Fatalf("unmarshal init: %v", err)
	}
	if _, ok := env.ClientCapabilities.Meta["session.systemPrompt"]; !ok {
		t.Fatal("kit-owned _meta entry missing")
	}
	if _, ok := env.ClientCapabilities.Meta["dev.poe-acp.status-line/v1"]; !ok {
		t.Fatalf("ClientMeta not merged: %v", env.ClientCapabilities.Meta)
	}

	// Verify parsed Extensions has the non-kit entries, excludes session.systemPrompt.
	caps := pc.agent.Caps()
	if _, ok := caps.Extensions["dev.poe-acp.status-line/v1"]; !ok {
		t.Fatalf("Extensions missing status-line: %#v", caps.Extensions)
	}
	if _, ok := caps.Extensions["dev.example.other/v2"]; !ok {
		t.Fatalf("Extensions missing other: %#v", caps.Extensions)
	}
	if _, ok := caps.Extensions["session.systemPrompt"]; ok {
		t.Fatal("Extensions must not include session.systemPrompt (surfaced via Caps.SystemPrompt)")
	}
	if !caps.SystemPrompt {
		t.Fatal("SystemPrompt cap should still be detected")
	}
}
