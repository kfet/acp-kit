package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
)

// syncBuf is a goroutine-safe log sink.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// waitFor polls cond until it holds or fails the test after 10s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// supervised starts the fake agent under supervision with fast backoff.
func supervised(t *testing.T, ctx context.Context, mut func(*Config), env ...string) (*AgentProc, *syncBuf) {
	t.Helper()
	logs := &syncBuf{}
	cfg := Config{
		Command:    []string{selfExecutable(t), "-test.run", "^$"},
		Env:        append(append(os.Environ(), fakeAgentEnv+"=1"), env...),
		Stderr:     io.Discard,
		RespawnMin: 10 * time.Millisecond,
		RespawnMax: 40 * time.Millisecond,
		Logger:     slog.New(slog.NewTextHandler(logs, nil)),
	}
	if mut != nil {
		mut(&cfg)
	}
	a, err := Start(ctx, cfg)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a, logs
}

func text(s string) []acp.ContentBlock { return []acp.ContentBlock{acp.TextBlock(s)} }

func pidOf(a *AgentProc) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cur.cmd.Process.Pid
}

func killChild(t *testing.T, a *AgentProc) {
	t.Helper()
	a.mu.Lock()
	g := a.cur
	a.mu.Unlock()
	if err := g.cmd.Process.Kill(); err != nil {
		t.Fatalf("kill: %v", err)
	}
	<-g.done
}

func (r *recSink) texts() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, n := range r.got {
		if c := n.Update.AgentMessageChunk; c != nil && c.Content.Text != nil {
			out = append(out, string(n.SessionId)+":"+c.Content.Text.Text)
		}
	}
	return out
}

// The incident: the agent child is killed out from under the relay. The
// exit is logged with its signal and stderr tail, the child is re-spawned,
// and the relay's next turn on its existing session simply works.
func TestRespawnKilledAgentNextTurnSucceeds(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	a, logs := supervised(t, ctx, nil)
	sink := &recSink{}
	sid, err := a.NewSession(ctx, t.TempDir(), sink, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Prompt(ctx, sid, text("hi")); err != nil {
		t.Fatal(err)
	}
	old := pidOf(a)
	killChild(t, a)

	if _, err := a.Prompt(ctx, sid, text("again")); err != nil {
		t.Fatalf("turn after agent death: %v", err)
	}
	if pidOf(a) == old || a.Restarts() != 1 {
		t.Fatalf("no respawn: pid %d→%d restarts %d", old, pidOf(a), a.Restarts())
	}
	select {
	case <-a.Done():
		t.Fatal("Done closed on a supervised respawn")
	default:
	}
	if a.Err() != nil {
		t.Fatalf("Err = %v while running", a.Err())
	}
	got := sink.texts()
	if len(got) != 2 || !strings.HasPrefix(got[1], string(sid)+":reply from ") {
		t.Fatalf("sink got %q", got)
	}
	l := logs.String()
	for _, want := range []string{"exited unexpectedly", "signal=killed", "fake agent up pid=", "session resumed after respawn", "acp agent respawned"} {
		if !strings.Contains(l, want) {
			t.Errorf("log missing %q:\n%s", want, l)
		}
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(a.Err(), ErrAgentClosed) {
		t.Fatalf("Err after Close = %v", a.Err())
	}
}

// A turn that is in flight when the child dies fails fast and says why,
// with the exit code and the child's last stderr in the log.
func TestInFlightTurnFailsFastOnDeath(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	a, logs := supervised(t, ctx, nil)
	sid, err := a.NewSession(ctx, t.TempDir(), &recSink{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = a.Prompt(ctx, sid, text("die"))
	if !errors.Is(err, ErrAgentDied) {
		t.Fatalf("err = %v, want ErrAgentDied", err)
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 3 {
		t.Fatalf("err = %v, want exit status 3", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("slow failure: %s", d)
	}
	waitFor(t, "exit log", func() bool { return strings.Contains(logs.String(), "dying now") })
	if !strings.Contains(logs.String(), "exit_code=3") {
		t.Fatalf("log: %s", logs.String())
	}
	if _, err := a.Prompt(ctx, sid, text("hi")); err != nil {
		t.Fatal(err)
	}
}

// A turn blocked on an agent that is killed (not one that exits itself)
// is released by the reaper, not left hanging.
func TestHungTurnReleasedOnKill(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	a, _ := supervised(t, ctx, nil)
	sid, err := a.NewSession(ctx, t.TempDir(), &recSink{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	errc := make(chan error, 1)
	go func() {
		_, err := a.Prompt(ctx, sid, text("hang"))
		errc <- err
	}()
	time.Sleep(100 * time.Millisecond)
	killChild(t, a)
	if err := <-errc; !errors.Is(err, ErrAgentDied) {
		t.Fatalf("err = %v, want ErrAgentDied", err)
	}
}

// An agent without session/resume gets session/load, and the history it
// replays does not leak into the relay's sink.
func TestRespawnRecoversViaLoad(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	a, logs := supervised(t, ctx, nil, fakeCapsEnv+"=load")
	sink := &recSink{}
	sid, err := a.NewSession(ctx, t.TempDir(), sink, nil)
	if err != nil {
		t.Fatal(err)
	}
	killChild(t, a)
	if _, err := a.Prompt(ctx, sid, text("hi")); err != nil {
		t.Fatal(err)
	}
	for _, s := range sink.texts() {
		if strings.Contains(s, "replayed") {
			t.Fatalf("replay leaked: %q", sink.texts())
		}
	}
	if !strings.Contains(logs.String(), "session loaded after respawn") {
		t.Fatal(logs.String())
	}
}

// With neither resume nor load working, a fresh session is created and
// its new wire id is mapped back to the id the relay holds.
func TestRespawnRecoversViaNewSession(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	a, logs := supervised(t, ctx, nil, fakeResumeFailEnv+"=1", fakeLoadFailEnv+"=1")
	sink := &recSink{}
	sid, err := a.NewSession(ctx, t.TempDir(), sink, nil)
	if err != nil {
		t.Fatal(err)
	}
	killChild(t, a)
	if _, err := a.Prompt(ctx, sid, text("hi")); err != nil {
		t.Fatal(err)
	}
	got := sink.texts()
	if len(got) != 1 || !strings.HasPrefix(got[0], string(sid)+":") {
		t.Fatalf("sink got %q, want updates under %s", got, sid)
	}
	g, wire, _ := a.peek(ctx, sid)
	if wire == sid || g == nil {
		t.Fatalf("wire = %s, want a fresh id", wire)
	}
	if err := a.Cancel(ctx, sid); err != nil {
		t.Fatal(err)
	}
	l := logs.String()
	for _, want := range []string{"session/resume after respawn failed", "session/load after respawn failed", "history lost"} {
		if !strings.Contains(l, want) {
			t.Errorf("log missing %q", want)
		}
	}
	a.DropSession(sid)
	if c, _ := a.callerFor(wire); c != wire {
		t.Fatalf("alias survived DropSession: %s", c)
	}
}

// A child that dies again while its session is being re-established
// fails that turn with ErrAgentDied rather than falling through.
func TestRecoveryDeathIsReported(t *testing.T) {
	for _, tc := range []struct{ caps, method string }{
		{"", "session/resume"},
		{"load", "session/load"},
	} {
		t.Run(tc.method, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			marker := filepath.Join(t.TempDir(), "die")
			a, _ := supervised(t, ctx, nil, fakeCapsEnv+"="+tc.caps, fakeDieOnEnv+"="+marker)
			sid, err := a.NewSession(ctx, t.TempDir(), &recSink{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(marker, []byte(tc.method), 0o600); err != nil {
				t.Fatal(err)
			}
			killChild(t, a)
			if _, err := a.Prompt(ctx, sid, text("hi")); !errors.Is(err, ErrAgentDied) {
				t.Fatalf("err = %v, want ErrAgentDied", err)
			}
		})
	}
}

// Respawn keeps retrying with backoff while the agent cannot start, and
// recovers once it can.
func TestRespawnBacksOffUntilAgentStarts(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	broken := filepath.Join(t.TempDir(), "broken")
	a, logs := supervised(t, ctx, func(c *Config) { c.RespawnHealthy = time.Nanosecond }, fakeBrokenEnv+"="+broken)
	sid, err := a.NewSession(ctx, t.TempDir(), &recSink{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(broken, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	killChild(t, a)
	waitFor(t, "two failed respawns", func() bool { return strings.Count(logs.String(), "respawn failed") >= 2 })
	_ = os.Remove(broken)
	if _, err := a.Prompt(ctx, sid, text("hi")); err != nil {
		t.Fatal(err)
	}
}

// Close during backoff ends supervision promptly; callers waiting for the
// respawn are released.
func TestCloseDuringBackoff(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	a, logs := supervised(t, ctx, func(c *Config) { c.RespawnMin = time.Hour })
	sid, err := a.NewSession(ctx, t.TempDir(), &recSink{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	killChild(t, a)
	waitFor(t, "backoff", func() bool { return strings.Contains(logs.String(), "respawning") })

	short, stop := context.WithTimeout(ctx, 30*time.Millisecond)
	_, err = a.Prompt(short, sid, text("hi"))
	stop()
	if !errors.Is(err, ErrAgentDied) || !strings.Contains(err.Error(), "waiting for respawn") {
		t.Fatalf("err = %v", err)
	}

	errc := make(chan error, 1)
	go func() {
		_, err := a.Prompt(ctx, sid, text("hi"))
		errc <- err
	}()
	time.Sleep(20 * time.Millisecond)
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(a.Err(), ErrAgentClosed) {
		t.Fatalf("Err = %v", a.Err())
	}
	if err := <-errc; !errors.Is(err, ErrAgentDied) {
		t.Fatalf("waiting turn: %v", err)
	}
}

// Cancelling the context given to Start ends supervision for good.
func TestStartContextEndsSupervision(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	a, _ := supervised(t, ctx, nil)
	cancel()
	<-a.Done()
	if !errors.Is(a.Err(), context.Canceled) {
		t.Fatalf("Err = %v", a.Err())
	}
	if _, err := a.ListSessions(context.Background(), ""); !errors.Is(err, ErrAgentDied) {
		t.Fatalf("err = %v", err)
	}
}

// A Close that lands while a respawn is in progress wins: the new child
// is torn down (whether it failed to start or started fine).
func TestCloseRacingRespawn(t *testing.T) {
	for _, brokenChild := range []bool{true, false} {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		broken := filepath.Join(t.TempDir(), "broken")
		a, logs := supervised(t, ctx, func(c *Config) { c.RespawnMin = 300 * time.Millisecond }, fakeBrokenEnv+"="+broken)
		if brokenChild {
			_ = os.WriteFile(broken, nil, 0o600)
		}
		killChild(t, a)
		waitFor(t, "backoff", func() bool { return strings.Contains(logs.String(), "respawning") })
		// Flag the shutdown without waking the sleeping supervisor, so it
		// discovers it only after its spawn attempt.
		a.closing.Store(true)
		<-a.Done()
		if !errors.Is(a.Err(), ErrAgentClosed) {
			t.Fatalf("broken=%v: Err = %v", brokenChild, a.Err())
		}
		cancel()
	}
}

// Close landing while a respawned child hangs in initialize does not wait
// out the handshake timeout.
func TestCloseDuringRespawnHandshake(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	marker := filepath.Join(t.TempDir(), "hang")
	a, logs := supervised(t, ctx, nil, fakeHangOnEnv+"="+marker)
	if err := os.WriteFile(marker, []byte(acp.AgentMethodInitialize), 0o600); err != nil {
		t.Fatal(err)
	}
	killChild(t, a)
	waitFor(t, "backoff", func() bool { return strings.Contains(logs.String(), "respawning") })
	time.Sleep(300 * time.Millisecond)
	start := time.Now()
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("Close took %s", d)
	}
}

// A child that closes stdout but lingers is killed after diedGrace, so
// the turn fails with ErrAgentDied and the next one reaches a new child.
func TestWedgedChildIsKilled(t *testing.T) {
	old := diedGrace
	diedGrace = 50 * time.Millisecond
	t.Cleanup(func() { diedGrace = old })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	a, _ := supervised(t, ctx, nil)
	sid, err := a.NewSession(ctx, t.TempDir(), &recSink{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Prompt(ctx, sid, text("closeout")); !errors.Is(err, ErrAgentDied) {
		t.Fatalf("err = %v", err)
	}
	if _, err := a.Prompt(ctx, sid, text("hi")); err != nil {
		t.Fatal(err)
	}
}

// Agent-level errors on a healthy child are passed through untouched.
func TestAgentErrorPassesThrough(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	a, _ := supervised(t, ctx, nil)
	_, err := a.Authenticate(ctx, "noop", "", "", false)
	var re *acp.RequestError
	if !errors.As(err, &re) || re.Code != -32601 {
		t.Fatalf("err = %v", err)
	}
	cctx, stop := context.WithCancel(ctx)
	stop()
	if _, err := a.ListSessions(cctx, ""); err == nil {
		t.Fatal("cancelled ctx: want error")
	}
}

// A write to a dead child's stdin that beats the reaper is reported as
// ErrAgentDied once the reaper catches up; if the child never exits it is
// killed after diedGrace.
func TestPipeErrorWaitsForReaper(t *testing.T) {
	old := diedGrace
	diedGrace = 50 * time.Millisecond
	t.Cleanup(func() { diedGrace = old })
	for _, reaped := range []bool{true, false} {
		cmd := exec.Command("sleep", "30")
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		a := newProc(context.Background(), Config{Logger: discardLogger()})
		g := &gen{cmd: cmd, started: time.Now(), done: make(chan struct{})}
		a.cur = g
		a.startReaper(g)
		inR, inW := io.Pipe()
		outR, outW := io.Pipe()
		_ = inR.Close() // writes now fail with "closed pipe"
		g.conn = acp.NewConnection(a.dispatch, inW, outR)
		if reaped {
			go func() { time.Sleep(10 * time.Millisecond); _ = cmd.Process.Kill() }()
		}
		_, err := rpc[any](context.Background(), g, "x", nil)
		if !errors.Is(err, ErrAgentDied) {
			t.Fatalf("reaped=%v: err = %v", reaped, err)
		}
		_ = cmd.Process.Kill()
		<-g.done
		_ = outW.Close()
	}
}

func TestStartBrokenChildFails(t *testing.T) {
	broken := filepath.Join(t.TempDir(), "broken")
	_ = os.WriteFile(broken, nil, 0o600)
	_, err := Start(context.Background(), Config{
		Command: []string{selfExecutable(t), "-test.run", "^$"},
		Env:     append(os.Environ(), fakeAgentEnv+"=1", fakeBrokenEnv+"="+broken),
		Stderr:  io.Discard,
	})
	if err == nil {
		t.Fatal("want error")
	}
}

func TestExitStatus(t *testing.T) {
	if c, s := exitStatus(ErrAgentExited); c != 0 || s != "" {
		t.Fatal(c, s)
	}
	if c, _ := exitStatus(errors.New("x")); c != -1 {
		t.Fatal(c)
	}
	err := exec.Command("sh", "-c", "exit 5").Run()
	if c, s := exitStatus(err); c != 5 || s != "" {
		t.Fatal(c, s)
	}
	cmd := exec.Command("sleep", "30")
	_ = cmd.Start()
	_ = cmd.Process.Signal(syscall.SIGTERM)
	if c, s := exitStatus(cmd.Wait()); c != -1 || s != "terminated" {
		t.Fatal(c, s)
	}
}

func TestStderrTail(t *testing.T) {
	var nilTail *stderrTail
	if nilTail.String() != "" {
		t.Fatal("nil tail")
	}
	tl := newStderrTail(2)
	_, _ = tl.Write([]byte("a\nb\nc"))
	if got := tl.String(); got != "a\nb\nc" {
		t.Fatalf("%q", got)
	}
	_, _ = tl.Write([]byte("d\n"))
	if got := tl.String(); got != "b\ncd" {
		t.Fatalf("%q", got)
	}
	tl = newStderrTail(3)
	_, _ = tl.Write([]byte(strings.Repeat("x", maxTailLine+10)))
	_, _ = tl.Write([]byte("yy\n"))
	if got := tl.String(); len(got) != maxTailLine {
		t.Fatalf("len %d", len(got))
	}
}

func TestRespawnTimings(t *testing.T) {
	lo, hi, h := Config{}.respawnTimings()
	if lo != defaultRespawnMin || hi != defaultRespawnMax || h != defaultRespawnHealthy {
		t.Fatal(lo, hi, h)
	}
	lo, hi, _ = Config{RespawnMin: 2 * time.Minute}.respawnTimings()
	if lo != 2*time.Minute || hi != 2*time.Minute {
		t.Fatal(lo, hi)
	}
	if newProc(context.Background(), Config{}).logger() != slog.Default() {
		t.Fatal("default logger")
	}
}

// stale makes every tracked session look like it belongs to an earlier
// child, as after a respawn, without spawning anything.
func stale(a *AgentProc) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.genSeq++
	a.cur.n = a.genSeq
}

func TestRecoveryInProcess(t *testing.T) {
	ctx := context.Background()
	// happyAgent resumes (with a model list): the session keeps its id
	// and the resumed model snapshot is adopted.
	pc := startPaired(t, Config{Command: []string{"x"}, Logger: discardLogger()}, happyAgent(t))
	sid, err := pc.agent.NewSession(ctx, t.TempDir(), &recSink{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	stale(pc.agent)
	if err := pc.agent.SetModel(ctx, sid, "p/m3"); err != nil {
		t.Fatal(err)
	}
	if _, cur := pc.agent.Models(); cur != "p/m3" {
		t.Fatalf("current model %q", cur)
	}

	// Nothing works: the turn fails with a clear recovery error.
	bad := func(_ context.Context, method string, _ json.RawMessage) (any, *acp.RequestError) {
		if method == acp.AgentMethodInitialize {
			return map[string]any{"protocolVersion": acp.ProtocolVersionNumber, "agentCapabilities": map[string]any{}}, nil
		}
		if method == acp.AgentMethodSessionNew {
			return map[string]any{"sessionId": "s1"}, nil
		}
		return nil, acp.NewInternalError(nil)
	}
	calls := 0
	failSecondNew := func(c context.Context, method string, p json.RawMessage) (any, *acp.RequestError) {
		if method == acp.AgentMethodSessionNew {
			calls++
			if calls > 1 {
				return nil, acp.NewInternalError(nil)
			}
		}
		return bad(c, method, p)
	}
	pc = startPaired(t, Config{Command: []string{"x"}, Logger: discardLogger()}, failSecondNew)
	sid, err = pc.agent.NewSession(ctx, t.TempDir(), &recSink{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	stale(pc.agent)
	if err := pc.agent.SetConfigOption(ctx, sid, "c", "v"); err == nil || !strings.Contains(err.Error(), "re-establish session") {
		t.Fatalf("err = %v", err)
	}
}

// A muted wire id drops notices as well as updates.
func TestMutedNoticeDropped(t *testing.T) {
	pc := startPaired(t, Config{Command: []string{"x"}}, happyAgent(t))
	pc.agent.setMuted("m", true)
	pc.agent.handleNotice(context.Background(), json.RawMessage(`{"sessionId":"m"}`))
	if _, ok := pc.agent.callerFor("m"); ok {
		t.Fatal("muted")
	}
}

// Once the AgentProc is finished every call fails fast with ErrAgentDied.
func TestFinishedAgentFailsEveryCall(t *testing.T) {
	ctx := context.Background()
	pc := startPaired(t, Config{Command: []string{"x"}}, happyAgent(t))
	a := pc.agent
	done := make(chan struct{})
	close(done)
	a.mu.Lock()
	a.cur = &gen{done: done, err: ErrAgentClosed}
	a.mu.Unlock()
	errs := []error{
		a.SetModel(ctx, "s", "m"),
		a.SetConfigOption(ctx, "s", "c", "v"),
		a.ReleaseSession(ctx, "s"),
		a.ResumeSession(ctx, "/", "s", &recSink{}),
		a.Cancel(ctx, "s"),
	}
	_, err := a.Authenticate(ctx, "m", "", "", false)
	errs = append(errs, err)
	for i, err := range errs {
		if !errors.Is(err, ErrAgentDied) || !errors.Is(err, ErrAgentClosed) {
			t.Fatalf("call %d: %v", i, err)
		}
	}
}
