package convo

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/kfet/acp-kit/client"
)

// --- fakes ----------------------------------------------------------------

type fakeAgent struct {
	mu       sync.Mutex
	models   []client.ModelInfo
	current  string
	setErr   error
	setCalls []string
}

func (a *fakeAgent) Models() ([]client.ModelInfo, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.models, a.current
}
func (a *fakeAgent) AvailableCommands() []client.CommandInfo {
	return []client.CommandInfo{{Name: "reload"}}
}

// richAgent adds every optional capability.
type richAgent struct{ fakeAgent }

func (a *richAgent) SetModel(_ context.Context, sid acp.SessionId, id string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.setCalls = append(a.setCalls, string(sid)+"="+id)
	return a.setErr
}
func (a *richAgent) AgentInfo() client.AgentInfo { return client.AgentInfo{Name: "fir", Version: "1"} }
func (a *richAgent) SessionStats(sid acp.SessionId) (client.SessionStats, bool) {
	if sid == "nostats" {
		return client.SessionStats{}, false
	}
	return client.SessionStats{Thinking: "high", ContextUsed: 5, ContextSize: 10,
		Cost: &client.Cost{Amount: 0.5, Currency: "USD"}}, true
}
func (a *richAgent) AuthMethods() []client.AuthMethod { return nil }
func (a *richAgent) Authenticate(context.Context, string, string, string, bool) (client.AuthResult, error) {
	return client.AuthResult{}, nil
}

type fakeSessions struct {
	mu      sync.Mutex
	live    map[string]acp.SessionId
	last    time.Time
	cancels []string
	resets  []string
}

func (s *fakeSessions) Live(conv string) (acp.SessionId, time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sid, ok := s.live[conv]
	return sid, s.last, ok
}
func (s *fakeSessions) Cancel(_ context.Context, conv string) {
	s.mu.Lock()
	s.cancels = append(s.cancels, conv)
	s.mu.Unlock()
}
func (s *fakeSessions) Len() int { return len(s.live) }

type resetSessions struct{ fakeSessions }

func (s *resetSessions) Reset(conv string) error {
	s.resets = append(s.resets, conv)
	delete(s.live, conv)
	return nil
}

type recSink struct {
	mu      sync.Mutex
	replies []string
	err     error
}

func (r *recSink) Reply(_ context.Context, _ *In, text string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.replies = append(r.replies, text)
	return r.err
}
func (r *recSink) all() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.replies, "\n---\n")
}

type failSink struct {
	recSink
	failed []error
}

func (f *failSink) Fail(_ context.Context, _ *In, err error) error {
	f.failed = append(f.failed, err)
	return nil
}

type failAuth struct{}

func (failAuth) AuthMethods() []client.AuthMethod {
	return []client.AuthMethod{{ID: "p", Name: "P"}}
}
func (failAuth) Authenticate(context.Context, string, string, string, bool) (client.AuthResult, error) {
	return client.AuthResult{}, errors.New("boom")
}

func newM(t *testing.T, cfg Config) *Manager {
	t.Helper()
	m, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func ctxT(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// --- overrides ------------------------------------------------------------

func TestValidateModel(t *testing.T) {
	if ValidateModel(nil, "x") != nil {
		t.Fatal("empty catalogue must accept")
	}
	ms := []client.ModelInfo{{ID: "a"}}
	if ValidateModel(ms, "a") != nil {
		t.Fatal("known rejected")
	}
	if err := ValidateModel(ms, "b"); !errors.Is(err, ErrUnknownModel) || !strings.Contains(err.Error(), `"b"`) {
		t.Fatalf("err = %v", err)
	}
}

func TestOverridesMemory(t *testing.T) {
	o, _ := NewOverrides(nil)
	if _, ok := o.Get("c"); ok {
		t.Fatal("empty")
	}
	_ = o.Set("c", "m1")
	if id, ok := o.Pending("c", "s1"); !ok || id != "m1" {
		t.Fatal("pending")
	}
	o.MarkApplied("c", "m1", "s1")
	if _, ok := o.Pending("c", "s1"); ok {
		t.Fatal("applied still pending")
	}
	if _, ok := o.Pending("c", "s2"); !ok {
		t.Fatal("new session must re-apply")
	}
	o.MarkApplied("c", "other", "s2") // stale choice: ignored
	if _, ok := o.Pending("c", "s2"); !ok {
		t.Fatal("stale mark applied")
	}
	if _, ok := o.Pending("none", "s"); ok {
		t.Fatal("none")
	}
	_ = o.Carry("c", "d")
	if id, _ := o.Get("d"); id != "m1" || o.Len() != 1 {
		t.Fatal("carry")
	}
	if o.Carry("missing", "x") != nil {
		t.Fatal("carry missing")
	}
	_ = o.Set("d", "")
	if o.Len() != 0 {
		t.Fatal("clear")
	}
}

func TestFileStore(t *testing.T) {
	dir := t.TempDir()
	fs := &FileStore{Path: filepath.Join(dir, "sub", "ov.json")}
	o, err := NewOverrides(fs)
	if err != nil || o.Len() != 0 {
		t.Fatal(err)
	}
	if err := o.Set("a", "m"); err != nil {
		t.Fatal(err)
	}
	_ = o.Set("b", "n")
	_ = o.Carry("b", "c")
	o2, _ := NewOverrides(fs)
	if id, _ := o2.Get("a"); id != "m" {
		t.Fatal("persisted a")
	}
	if id, _ := o2.Get("c"); id != "n" || o2.Len() != 2 {
		t.Fatal("persisted carry")
	}
	// Empty values in the file are ignored.
	_ = os.WriteFile(fs.Path, []byte(`{"z":""}`), 0o600)
	o3, _ := NewOverrides(fs)
	if o3.Len() != 0 {
		t.Fatal("empty value loaded")
	}
	// Corrupt file.
	_ = os.WriteFile(fs.Path, []byte(`{`), 0o600)
	if _, err := NewOverrides(fs); err == nil {
		t.Fatal("corrupt accepted")
	}
	if err := fs.Save("a", "b"); err == nil {
		t.Fatal("save over corrupt")
	}
	// Unreadable path (a directory).
	if _, err := (&FileStore{Path: dir}).Load(); err == nil {
		t.Fatal("dir read")
	}
	// Parent is a file: MkdirAll fails.
	blocker := filepath.Join(dir, "blk")
	_ = os.WriteFile(blocker, nil, 0o600)
	if err := (&FileStore{Path: filepath.Join(blocker, "x", "y.json")}).Save("a", "b"); err == nil {
		t.Fatal("read under file")
	}
	// Read-only parent: the file reads as absent, but MkdirAll fails.
	ro := filepath.Join(dir, "ro")
	_ = os.MkdirAll(ro, 0o500)
	t.Cleanup(func() { _ = os.Chmod(ro, 0o755) })
	if err := (&FileStore{Path: filepath.Join(ro, "x", "y.json")}).Save("a", "b"); err == nil {
		t.Fatal("mkdir in read-only dir")
	}
	// Temp file cannot be written: its name is a directory.
	p := filepath.Join(dir, "t.json")
	_ = os.MkdirAll(p+".tmp", 0o755)
	if err := (&FileStore{Path: p}).Save("a", "b"); err == nil {
		t.Fatal("tmp write")
	}
}

func TestMustJSONPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("no panic")
		}
	}()
	mustJSONWith(func(any) ([]byte, error) { return nil, errors.New("x") }, nil)
}

// --- active ---------------------------------------------------------------

func TestActiveTokensAndStop(t *testing.T) {
	a := NewActive()
	var cancelled []string
	a.OnCancel = func(_ context.Context, c string) error { cancelled = append(cancelled, c); return errors.New("wire") }
	ctx := context.Background()
	n := 0
	t1 := a.Begin("c", func() { n++ }, "v")
	if !a.Running("c") || a.Len() != 1 {
		t.Fatal("running")
	}
	if got, _ := a.Get("c"); got.Value != "v" {
		t.Fatal("value")
	}
	for _, tok := range []struct {
		conv string
		tok  uint64
	}{{"c", 0}, {"c", t1.Token + 99}, {"x", t1.Token}} {
		if ok, err := a.CancelToken(ctx, tok.conv, tok.tok); ok || err != nil {
			t.Fatal("stale token cancelled")
		}
	}
	if ok, err := a.CancelToken(ctx, "c", t1.Token); !ok || err == nil || n != 1 {
		t.Fatal("owned token")
	}
	// A successor displaces t1; t1's End must not remove it.
	t2 := a.Begin("c", nil, nil)
	a.End(t1)
	a.End(t1)
	<-t1.Ended()
	if !a.Running("c") {
		t.Fatal("successor removed")
	}
	if ok, _ := a.CancelToken(ctx, "c", t1.Token); ok {
		t.Fatal("old token hit successor")
	}
	if !a.Stop(ctx, "c") || a.Stop(ctx, "c") {
		t.Fatal("stop")
	}
	a.End(t2)
	if len(cancelled) != 2 {
		t.Fatalf("OnCancel = %v", cancelled)
	}
}

func TestActiveCancelAllAndWait(t *testing.T) {
	a := NewActive()
	ctx := ctxT(t)
	ta := a.Begin("a", nil, nil)
	tb := a.Begin("b", nil, nil)
	go func() { time.Sleep(10 * time.Millisecond); a.End(ta); a.End(tb) }()
	if got := a.CancelAll(ctx); strings.Join(got, ",") != "a,b" {
		t.Fatalf("CancelAll = %v", got)
	}
	if err := a.WaitCancelled(ctx); err != nil {
		t.Fatal(err)
	}
	// Unwound turns from an earlier CancelAll are forgotten.
	tf := a.Begin("f", nil, nil)
	a.CancelAll(ctx)
	a.End(tf)
	a.CancelAll(ctx)
	if len(a.cancelled) != 0 {
		t.Fatalf("cancelled grew: %d", len(a.cancelled))
	}
	tg := a.Begin("g", nil, nil)
	a.CancelAll(ctx)
	a.CancelAll(ctx) // still unwinding: kept
	if len(a.cancelled) != 1 {
		t.Fatalf("unwinding turn forgotten: %d", len(a.cancelled))
	}
	a.End(tg)
	_ = a.WaitCancelled(ctx)
	// WaitCancelled honours ctx.
	td := a.Begin("d", nil, nil)
	_ = a.CancelAll(ctx)
	short, cancel := context.WithTimeout(ctx, 5*time.Millisecond)
	defer cancel()
	if err := a.WaitCancelled(short); err == nil {
		t.Fatal("waited forever?")
	}
	a.End(td)
	// WaitIdle honours ctx while something is registered.
	te := a.Begin("e", nil, nil)
	short2, cancel2 := context.WithTimeout(ctx, 5*time.Millisecond)
	defer cancel2()
	if err := a.WaitIdle(short2); err == nil {
		t.Fatal("idle with turn registered")
	}
	a.End(te)
	if a.WaitIdle(ctx) != nil {
		t.Fatal("idle")
	}
}

func TestActiveCancelAllRace(t *testing.T) {
	a := NewActive()
	a.Begin("c", nil, nil)
	a.Begin("z", nil, nil)
	// The first cancel ends the other turn before CancelAll reaches it:
	// a turn that already ended is not claimed.
	a.OnCancel = func(context.Context, string) error {
		a.mu.Lock()
		a.m = map[string]*Turn{}
		a.mu.Unlock()
		return nil
	}
	if got := a.CancelAll(context.Background()); len(got) != 1 {
		t.Fatalf("got %v", got)
	}
}

// --- queue ----------------------------------------------------------------

func TestQueueShedding(t *testing.T) {
	type it struct {
		id    int
		react bool
	}
	var shed []int
	q := NewQueue[it](2, func(i it) bool { return i.react }, func(i it) { shed = append(shed, i.id) })
	if !q.Push(it{1, true}) || !q.Push(it{2, false}) {
		t.Fatal("push")
	}
	// Full: user item sheds oldest reaction.
	if !q.Push(it{3, false}) || len(shed) != 1 || shed[0] != 1 {
		t.Fatalf("shed = %v", shed)
	}
	// Full of user items: a reaction is refused, a user item grows.
	if q.Push(it{4, true}) {
		t.Fatal("reaction accepted over full user queue")
	}
	if !q.Push(it{5, false}) || q.Len() != 3 || q.Items()[0].id != 2 {
		t.Fatal("user refused")
	}
	stop := make(chan struct{})
	got, ok := q.PopOrWait(stop)
	if !ok || got.id != 2 || q.Idle() {
		t.Fatal("pop")
	}
	if q.StopIfIdle() {
		t.Fatal("stopped while busy")
	}
	q.Finish()
	pending, inFlight := q.Stop()
	if len(pending) != 2 || inFlight {
		t.Fatal("stop")
	}
	if q.Push(it{6, false}) {
		t.Fatal("push after stop")
	}
	close(stop)
	if _, ok := q.PopOrWait(stop); ok {
		t.Fatal("pop after stop")
	}
}

func TestQueueDefaultsAndWait(t *testing.T) {
	q := NewQueue[int](0, nil, nil)
	if q.cap != DefaultQueueCap {
		t.Fatal("cap")
	}
	for i := 0; i <= DefaultQueueCap; i++ {
		q.Push(i)
	}
	// Wake from notify.
	q2 := NewQueue[int](1, nil, nil)
	done := make(chan int)
	go func() { v, _ := q2.PopOrWait(nil); done <- v }()
	time.Sleep(5 * time.Millisecond)
	q2.Push(7)
	if <-done != 7 {
		t.Fatal("wake")
	}
	if q2.WaitIdle(time.Millisecond) {
		t.Fatal("idle while in flight")
	}
	go func() { time.Sleep(5 * time.Millisecond); q2.Finish() }()
	if !q2.WaitIdle(time.Second) {
		t.Fatal("not idle")
	}
	if !q2.StopIfIdle() {
		t.Fatal("stop idle")
	}
}

// --- liveness -------------------------------------------------------------

func TestLiveness(t *testing.T) {
	c, ctx, stop := ProgressClock{NoProgressTimeout: time.Hour}.Arm(context.Background())
	c.Progress()
	if c.Wrap(nil) == nil || ctx.Err() != nil {
		t.Fatal("progress clock")
	}
	stop()
	u, uctx, ustop := Unbounded{}.Arm(context.Background())
	u.Progress()
	var s client.SessionUpdateSink
	if u.Wrap(s) != nil {
		t.Fatal("nop wrap")
	}
	ustop()
	if uctx.Err() == nil {
		t.Fatal("stop did not cancel")
	}
}

func TestActiveClaim(t *testing.T) {
	a := NewActive()
	ctx := ctxT(t)
	waited := make(chan string, 4)
	a.OnWait = func(c string) { waited <- c }
	busy := a.Begin("c", nil, nil)
	got := make(chan *Turn)
	go func() {
		tn, err := a.Claim(ctx, "c", nil, "claimed")
		if err != nil {
			t.Error(err)
		}
		got <- tn
	}()
	if <-waited != "c" {
		t.Fatal("did not wait")
	}
	a.End(busy)
	if tn := <-got; tn.Value != "claimed" || !a.Running("c") {
		t.Fatal("claim")
	}
	short, cancel := context.WithTimeout(ctx, 5*time.Millisecond)
	defer cancel()
	if _, err := a.Claim(short, "c", nil, nil); err == nil {
		t.Fatal("claimed a busy conversation")
	}
}
