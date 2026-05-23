package state

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/kfet/acp-kit/client"
)

type fakeAgent struct {
	mu        sync.Mutex
	caps      client.Caps
	listResp  []client.SessionInfo
	listErr   error
	resumeErr error
	newErr    error
	cancelErr error
	newID     acp.SessionId
	newBlocks []acp.ContentBlock
	drops     []acp.SessionId
	rebinds   int
	cancels   int
}

func (f *fakeAgent) Caps() client.Caps { return f.caps }
func (f *fakeAgent) NewSession(_ context.Context, _ string, _ client.SessionUpdateSink, blocks []acp.ContentBlock) (acp.SessionId, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.newBlocks = blocks
	if f.newErr != nil {
		return "", f.newErr
	}
	if f.newID == "" {
		return "sess-1", nil
	}
	return f.newID, nil
}
func (f *fakeAgent) ListSessions(context.Context, string) ([]client.SessionInfo, error) {
	return f.listResp, f.listErr
}
func (f *fakeAgent) ResumeSession(context.Context, string, acp.SessionId, client.SessionUpdateSink) error {
	return f.resumeErr
}
func (f *fakeAgent) Cancel(context.Context, acp.SessionId) error {
	f.mu.Lock()
	f.cancels++
	f.mu.Unlock()
	return f.cancelErr
}
func (f *fakeAgent) DropSession(sid acp.SessionId) {
	f.mu.Lock()
	f.drops = append(f.drops, sid)
	f.mu.Unlock()
}
func (f *fakeAgent) RebindSink(acp.SessionId, client.SessionUpdateSink) {
	f.mu.Lock()
	f.rebinds++
	f.mu.Unlock()
}

type stubSink struct{}

func (stubSink) OnUpdate(context.Context, acp.SessionNotification) error { return nil }

func newManagerT(t *testing.T, cfg Config) *Manager {
	t.Helper()
	if cfg.StateDir == "" {
		cfg.StateDir = t.TempDir()
	}
	m, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

func TestNewValidatesConfig(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("expected nil-agent error")
	}
	if _, err := New(Config{Agent: &fakeAgent{}}); err == nil {
		t.Fatal("expected empty StateDir error")
	}
}

func TestSystemPromptMetaWhenCapAdvertised(t *testing.T) {
	ag := &fakeAgent{caps: client.Caps{SystemPrompt: true}}
	m := newManagerT(t, Config{Agent: ag, SystemPrompt: "sys"})
	s, err := m.GetOrCreate(context.Background(), "conv1", stubSink{})
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	if len(ag.newBlocks) != 1 {
		t.Fatalf("newBlocks = %v", ag.newBlocks)
	}
	if got := m.TakePendingSystemPrompt(s); got != "" {
		t.Fatalf("pending = %q", got)
	}
}

func TestSystemPromptInlineFallbackAndProvider(t *testing.T) {
	ag := &fakeAgent{}
	calls := 0
	m := newManagerT(t, Config{Agent: ag, SystemPromptProvider: func() string {
		calls++
		return " provider-sys "
	}})
	s, err := m.GetOrCreate(context.Background(), "conv1", stubSink{})
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	if calls == 0 {
		t.Fatal("provider not called")
	}
	if ag.newBlocks != nil {
		t.Fatalf("newBlocks should be nil (no cap): %v", ag.newBlocks)
	}
	if got := m.TakePendingSystemPrompt(s); got != "provider-sys" {
		t.Fatalf("pending = %q", got)
	}
	if got := m.TakePendingSystemPrompt(s); got != "" {
		t.Fatalf("second pending = %q", got)
	}
}

func TestResumeHappyAndRearmForNonCapAgent(t *testing.T) {
	ag := &fakeAgent{
		caps:     client.Caps{ListSessions: true, ResumeSession: true},
		listResp: []client.SessionInfo{{SessionId: "existing-1"}},
	}
	m := newManagerT(t, Config{Agent: ag, SystemPrompt: "sys"})
	s, err := m.GetOrCreate(context.Background(), "conv1", stubSink{})
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	if s.SessionID != "existing-1" {
		t.Fatalf("sid = %q", s.SessionID)
	}
	// Non-cap agent → resume re-arms inline sysprompt.
	if got := m.TakePendingSystemPrompt(s); got != "sys" {
		t.Fatalf("expected re-armed inline, got %q", got)
	}
}

func TestResumeListErrorFallsBackToNew(t *testing.T) {
	ag := &fakeAgent{
		caps:    client.Caps{ListSessions: true, ResumeSession: true},
		listErr: errors.New("list boom"),
	}
	m := newManagerT(t, Config{Agent: ag})
	s, err := m.GetOrCreate(context.Background(), "conv1", stubSink{})
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	if s.SessionID != "sess-1" {
		t.Fatalf("expected fresh sess-1, got %q", s.SessionID)
	}
}

func TestResumeNoSessionsFallsBackToNew(t *testing.T) {
	ag := &fakeAgent{caps: client.Caps{ListSessions: true, ResumeSession: true}}
	m := newManagerT(t, Config{Agent: ag})
	s, _ := m.GetOrCreate(context.Background(), "conv1", stubSink{})
	if s.SessionID != "sess-1" {
		t.Fatalf("sid = %q", s.SessionID)
	}
}

func TestResumeResumeErrorFallsBack(t *testing.T) {
	ag := &fakeAgent{
		caps:      client.Caps{ListSessions: true, ResumeSession: true},
		listResp:  []client.SessionInfo{{SessionId: "x"}},
		resumeErr: errors.New("nope"),
	}
	m := newManagerT(t, Config{Agent: ag})
	s, _ := m.GetOrCreate(context.Background(), "conv1", stubSink{})
	if s.SessionID != "sess-1" {
		t.Fatalf("sid = %q", s.SessionID)
	}
}

func TestNewSessionErrorPropagates(t *testing.T) {
	ag := &fakeAgent{newErr: errors.New("fail")}
	m := newManagerT(t, Config{Agent: ag})
	if _, err := m.GetOrCreate(context.Background(), "conv1", stubSink{}); err == nil {
		t.Fatal("expected error")
	}
}

func TestGetOrCreateHotPath(t *testing.T) {
	ag := &fakeAgent{}
	m := newManagerT(t, Config{Agent: ag})
	s1, _ := m.GetOrCreate(context.Background(), "conv1", stubSink{})
	s2, _ := m.GetOrCreate(context.Background(), "conv1", stubSink{})
	if s1 != s2 {
		t.Fatal("expected same session pointer")
	}
	if ag.rebinds != 1 {
		t.Fatalf("rebinds = %d", ag.rebinds)
	}
}

func TestCustomCwdFor(t *testing.T) {
	dir := t.TempDir()
	ag := &fakeAgent{}
	cwdRet := ""
	m := newManagerT(t, Config{Agent: ag, StateDir: dir, CwdFor: func(stateDir, key string) (string, error) {
		cwdRet = stateDir + "/custom/" + key
		return cwdRet, nil
	}})
	if _, err := m.GetOrCreate(context.Background(), "weird/key", stubSink{}); err != nil {
		t.Fatalf("custom cwd: %v", err)
	}
	if cwdRet == "" {
		t.Fatal("custom CwdFor not called")
	}
}

func TestCustomCwdForError(t *testing.T) {
	ag := &fakeAgent{}
	m := newManagerT(t, Config{Agent: ag, CwdFor: func(string, string) (string, error) {
		return "", errors.New("no cwd")
	}})
	if _, err := m.GetOrCreate(context.Background(), "k", stubSink{}); err == nil {
		t.Fatal("expected cwd error")
	}
}

func TestDefaultCwdForRejectsBadKey(t *testing.T) {
	ag := &fakeAgent{}
	m := newManagerT(t, Config{Agent: ag})
	for _, bad := range []string{"", ".", "..", "a/b", ".hidden", string([]byte{'a', 0})} {
		if _, err := m.GetOrCreate(context.Background(), bad, stubSink{}); err == nil {
			t.Fatalf("expected error for %q", bad)
		}
	}
}

func TestDefaultCwdForAfterClose(t *testing.T) {
	ag := &fakeAgent{}
	m := newManagerT(t, Config{Agent: ag})
	_ = m.Close()
	if _, err := m.GetOrCreate(context.Background(), "ok", stubSink{}); err == nil {
		t.Fatal("expected closed error")
	}
}

func TestCloseIdempotent(t *testing.T) {
	ag := &fakeAgent{}
	m := newManagerT(t, Config{Agent: ag})
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close repeat: %v", err)
	}
}

func TestAccessors(t *testing.T) {
	ag := &fakeAgent{}
	dir := t.TempDir()
	m := newManagerT(t, Config{Agent: ag, StateDir: dir})
	if m.StateDir() != dir {
		t.Fatalf("StateDir = %q", m.StateDir())
	}
	if m.Agent() != ag {
		t.Fatal("Agent accessor mismatch")
	}
}

func TestCancelKnownAndUnknown(t *testing.T) {
	ag := &fakeAgent{}
	m := newManagerT(t, Config{Agent: ag})
	m.Cancel(context.Background(), "conv1") // unknown: no-op
	if _, err := m.GetOrCreate(context.Background(), "conv1", stubSink{}); err != nil {
		t.Fatal(err)
	}
	m.Cancel(context.Background(), "conv1")
	if ag.cancels != 1 {
		t.Fatalf("cancels = %d", ag.cancels)
	}
}

func TestGCDropsStaleSessions(t *testing.T) {
	ag := &fakeAgent{}
	m := newManagerT(t, Config{Agent: ag, IdleTimeout: time.Millisecond})
	s, _ := m.GetOrCreate(context.Background(), "conv1", stubSink{})
	// Backdate.
	m.mu.Lock()
	s.lastUsed = time.Now().Add(-time.Hour)
	m.mu.Unlock()
	if m.Len() != 1 {
		t.Fatalf("Len pre-gc = %d", m.Len())
	}
	m.GCOnce()
	if m.Len() != 0 {
		t.Fatalf("Len post-gc = %d", m.Len())
	}
	if len(ag.drops) != 1 || ag.drops[0] != s.SessionID {
		t.Fatalf("drops = %v", ag.drops)
	}
}

func TestRunCancellableAndZeroPeriodFallback(t *testing.T) {
	ag := &fakeAgent{}
	m := newManagerT(t, Config{Agent: ag, IdleTimeout: time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit on ctx cancel")
	}

	// IdleTimeout/4 == 0 path: simulate by directly invoking the period
	// guard via a separate manager with tiny timeout.
	m2 := newManagerT(t, Config{Agent: &fakeAgent{}, IdleTimeout: 3 * time.Nanosecond})
	ctx2, cancel2 := context.WithCancel(context.Background())
	done2 := make(chan struct{})
	go func() { m2.Run(ctx2); close(done2) }()
	time.Sleep(20 * time.Millisecond)
	cancel2()
	<-done2
}

func TestTakePendingNoOpWhenFlagClear(t *testing.T) {
	ag := &fakeAgent{caps: client.Caps{SystemPrompt: true}}
	m := newManagerT(t, Config{Agent: ag, SystemPrompt: "sys"})
	s, _ := m.GetOrCreate(context.Background(), "conv1", stubSink{})
	if got := m.TakePendingSystemPrompt(s); got != "" {
		t.Fatalf("got = %q", got)
	}
}

func TestSystemPromptProviderEmpty(t *testing.T) {
	ag := &fakeAgent{}
	m := newManagerT(t, Config{Agent: ag, SystemPromptProvider: func() string { return "  " }})
	s, _ := m.GetOrCreate(context.Background(), "conv1", stubSink{})
	if got := m.TakePendingSystemPrompt(s); got != "" {
		t.Fatalf("expected empty pending, got %q", got)
	}
}

func TestNewMkdirAndOpenRootErrors(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("requires non-root")
	}
	// Use a path under a file → MkdirAll fails.
	tmp := t.TempDir()
	bad := filepath.Join(tmp, "afile")
	if err := os.WriteFile(bad, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Config{Agent: &fakeAgent{}, StateDir: filepath.Join(bad, "sub")}); err == nil {
		t.Fatal("expected mkdir error")
	}

	// Now: MkdirAll succeeds but OpenRoot fails. Make a dir then chmod 0
	// so OpenRoot is denied. macOS: opening a 0000 dir with O_RDONLY fails
	// for non-root users.
	dir := filepath.Join(tmp, "noaccess")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	if _, err := New(Config{Agent: &fakeAgent{}, StateDir: dir}); err == nil {
		t.Skip("OpenRoot did not fail on this platform; skipping")
	}
}

func TestTouch(t *testing.T) {
	ag := &fakeAgent{}
	m := newManagerT(t, Config{Agent: ag})
	s, _ := m.GetOrCreate(context.Background(), "conv1", stubSink{})
	old := s.lastUsed
	time.Sleep(time.Millisecond)
	m.Touch(s)
	if !s.lastUsed.After(old) {
		t.Fatalf("Touch did not update lastUsed: %v vs %v", old, s.lastUsed)
	}
}

func TestRunFiresGCOnce(t *testing.T) {
	ag := &fakeAgent{}
	m := newManagerT(t, Config{Agent: ag, IdleTimeout: 4 * time.Millisecond})
	s, _ := m.GetOrCreate(context.Background(), "conv1", stubSink{})
	m.mu.Lock()
	s.lastUsed = time.Now().Add(-time.Hour)
	m.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	deadline := time.Now().Add(2 * time.Second)
	for m.Len() != 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	cancel()
	<-done
	if m.Len() != 0 {
		t.Fatalf("Run did not GC: Len=%d", m.Len())
	}
}

func TestCwdForMkdirError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("requires non-root")
	}
	ag := &fakeAgent{}
	dir := t.TempDir()
	m := newManagerT(t, Config{Agent: ag, StateDir: dir})
	// Chmod the StateDir read-only so the per-conv MkdirAll fails.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	if _, err := m.GetOrCreate(context.Background(), "willfail", stubSink{}); err == nil {
		t.Skip("MkdirAll succeeded despite chmod; skipping")
	}
}

func TestRaceLoserBranch(t *testing.T) {
	ag := &fakeAgent{newID: "loser"}
	var mgr *Manager
	winner := &Session{Key: "conv1", SessionID: "winner", lastUsed: time.Now()}
	mgr = newManagerT(t, Config{
		Agent: ag,
		CwdFor: func(sd, key string) (string, error) {
			// Insert the "winner" session before NewSession runs, so the
			// post-NewSession map check finds an existing entry and we hit
			// the race-loser branch.
			mgr.mu.Lock()
			mgr.byKey[key] = winner
			mgr.mu.Unlock()
			return sd + "/c/" + key, nil
		},
	})
	got, err := mgr.GetOrCreate(context.Background(), "conv1", stubSink{})
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	if got != winner {
		t.Fatal("expected to return existing winner from race")
	}
	dropped := false
	for _, sid := range ag.drops {
		if sid == "loser" {
			dropped = true
		}
	}
	if !dropped {
		t.Fatalf("loser sid was not dropped; drops=%v", ag.drops)
	}
}
