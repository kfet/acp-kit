package terminal

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	acp "github.com/coder/acp-go-sdk"
)

// fakeConn is a programmable Conn for tests. Each method delegates to a
// function field when set, else returns a benign zero response.
type fakeConn struct {
	mu sync.Mutex

	createFn  func(acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error)
	waitFn    func(ctx context.Context, p acp.WaitForTerminalExitRequest) (acp.WaitForTerminalExitResponse, error)
	outputFn  func(acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error)
	updateErr error

	updates  int
	killed   []string
	released []string
}

func (c *fakeConn) SessionUpdate(_ context.Context, _ acp.SessionNotification) error {
	c.mu.Lock()
	c.updates++
	c.mu.Unlock()
	return c.updateErr
}

func (c *fakeConn) CreateTerminal(_ context.Context, p acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error) {
	if c.createFn != nil {
		return c.createFn(p)
	}
	return acp.CreateTerminalResponse{TerminalId: "term-1"}, nil
}

func (c *fakeConn) KillTerminal(_ context.Context, p acp.KillTerminalRequest) (acp.KillTerminalResponse, error) {
	c.mu.Lock()
	c.killed = append(c.killed, p.TerminalId)
	c.mu.Unlock()
	return acp.KillTerminalResponse{}, nil
}

func (c *fakeConn) ReleaseTerminal(_ context.Context, p acp.ReleaseTerminalRequest) (acp.ReleaseTerminalResponse, error) {
	c.mu.Lock()
	c.released = append(c.released, p.TerminalId)
	c.mu.Unlock()
	return acp.ReleaseTerminalResponse{}, nil
}

func (c *fakeConn) TerminalOutput(_ context.Context, p acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error) {
	if c.outputFn != nil {
		return c.outputFn(p)
	}
	return acp.TerminalOutputResponse{Output: "out"}, nil
}

func (c *fakeConn) WaitForTerminalExit(ctx context.Context, p acp.WaitForTerminalExitRequest) (acp.WaitForTerminalExitResponse, error) {
	if c.waitFn != nil {
		return c.waitFn(ctx, p)
	}
	return acp.WaitForTerminalExitResponse{}, nil
}

func intp(n int) *int { return &n }

func TestNewStateWithLimit(t *testing.T) {
	if got := NewState().maxBackground; got != DefaultMaxBackground {
		t.Errorf("NewState limit = %d, want %d", got, DefaultMaxBackground)
	}
	if got := NewStateWithLimit(0).maxBackground; got != DefaultMaxBackground {
		t.Errorf("limit<=0 should fall back to %d, got %d", DefaultMaxBackground, got)
	}
	if got := NewStateWithLimit(3).maxBackground; got != 3 {
		t.Errorf("limit = %d, want 3", got)
	}
}

func TestTimeoutError(t *testing.T) {
	err := &TimeoutError{Seconds: 7}
	if !strings.Contains(err.Error(), "7s") {
		t.Errorf("TimeoutError message = %q, want it to mention 7s", err.Error())
	}
}

func TestExec_CreateTerminalError(t *testing.T) {
	conn := &fakeConn{createFn: func(acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error) {
		return acp.CreateTerminalResponse{}, errors.New("boom")
	}}
	_, err := NewState().Exec(context.Background(), conn, "s", "tc", "echo hi", "/tmp", 0)
	if err == nil || !strings.Contains(err.Error(), "create terminal") {
		t.Fatalf("want create terminal error, got %v", err)
	}
}

func TestExec_Success(t *testing.T) {
	conn := &fakeConn{
		waitFn: func(context.Context, acp.WaitForTerminalExitRequest) (acp.WaitForTerminalExitResponse, error) {
			return acp.WaitForTerminalExitResponse{ExitCode: intp(0)}, nil
		},
		outputFn: func(acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error) {
			return acp.TerminalOutputResponse{Output: "hello\n"}, nil
		},
	}
	s := NewState()
	res, err := s.Exec(context.Background(), conn, "s", "tc", "echo hello", "/tmp", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Output != "hello\n" || res.ExitCode == nil || *res.ExitCode != 0 {
		t.Errorf("unexpected result %+v", res)
	}
	// pending entry must be cleared, terminal released.
	s.mu.Lock()
	n := len(s.pendingBash)
	s.mu.Unlock()
	if n != 0 {
		t.Errorf("pending not cleared: %d", n)
	}
	if len(conn.released) != 1 {
		t.Errorf("want 1 release, got %d", len(conn.released))
	}
	if conn.updates != 1 {
		t.Errorf("want 1 embed update, got %d", conn.updates)
	}
}

func TestExec_AbortedBeforeWait(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before Exec runs
	conn := &fakeConn{}
	_, err := NewState().Exec(ctx, conn, "s", "tc", "sleep 1", "/tmp", 0)
	if !errors.Is(err, ErrAborted) {
		t.Fatalf("want ErrAborted, got %v", err)
	}
	if len(conn.killed) != 1 || len(conn.released) != 1 {
		t.Errorf("aborted exec should kill+release: killed=%v released=%v", conn.killed, conn.released)
	}
}

func TestExec_AbortedDuringWait(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	conn := &fakeConn{
		waitFn: func(context.Context, acp.WaitForTerminalExitRequest) (acp.WaitForTerminalExitResponse, error) {
			cancel() // cancel after the pre-wait check has passed
			return acp.WaitForTerminalExitResponse{ExitCode: intp(0)}, nil
		},
	}
	_, err := NewState().Exec(ctx, conn, "s", "tc", "cmd", "/tmp", 0)
	if !errors.Is(err, ErrAborted) {
		t.Fatalf("want ErrAborted, got %v", err)
	}
}

func TestExec_WaitErrorNonTimeout(t *testing.T) {
	conn := &fakeConn{
		waitFn: func(context.Context, acp.WaitForTerminalExitRequest) (acp.WaitForTerminalExitResponse, error) {
			return acp.WaitForTerminalExitResponse{}, errors.New("wait failed")
		},
	}
	s := NewState()
	_, err := s.Exec(context.Background(), conn, "s", "tc", "cmd", "/tmp", 0)
	if err == nil || !strings.Contains(err.Error(), "wait failed") {
		t.Fatalf("want wait failed error, got %v", err)
	}
	if len(conn.released) != 1 {
		t.Errorf("wait error should still release, got %d", len(conn.released))
	}
}

func TestExec_Timeout(t *testing.T) {
	conn := &fakeConn{
		waitFn: func(ctx context.Context, _ acp.WaitForTerminalExitRequest) (acp.WaitForTerminalExitResponse, error) {
			<-ctx.Done() // block until the per-command timeout fires
			return acp.WaitForTerminalExitResponse{}, ctx.Err()
		},
	}
	var to *TimeoutError
	_, err := NewState().Exec(context.Background(), conn, "s", "tc", "sleep 100", "/tmp", 1)
	if !errors.As(err, &to) || to.Seconds != 1 {
		t.Fatalf("want *TimeoutError{Seconds:1}, got %v", err)
	}
	if len(conn.killed) != 1 {
		t.Errorf("timeout should kill the terminal, killed=%v", conn.killed)
	}
}

func TestStartBackground_AtLimit(t *testing.T) {
	s := NewStateWithLimit(1)
	conn := &fakeConn{}
	if _, err := s.StartBackground(context.Background(), conn, "s", "cmd", "/tmp", "tc1"); err != nil {
		t.Fatalf("first start should succeed: %v", err)
	}
	_, err := s.StartBackground(context.Background(), conn, "s", "cmd", "/tmp", "tc2")
	if err == nil || !strings.Contains(err.Error(), "maximum of 1") {
		t.Fatalf("want at-limit error, got %v", err)
	}
}

func TestStartBackground_CreateError(t *testing.T) {
	conn := &fakeConn{createFn: func(acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error) {
		return acp.CreateTerminalResponse{}, errors.New("nope")
	}}
	_, err := NewState().StartBackground(context.Background(), conn, "s", "cmd", "/tmp", "tc")
	if err == nil || !strings.Contains(err.Error(), "create terminal") {
		t.Fatalf("want create terminal error, got %v", err)
	}
}

func TestBackgroundOutput(t *testing.T) {
	s := NewState()
	conn := &fakeConn{
		createFn: func(acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error) {
			return acp.CreateTerminalResponse{TerminalId: "bg-1"}, nil
		},
	}
	cmdID, err := s.StartBackground(context.Background(), conn, "s", "cmd", "/tmp", "tc")
	if err != nil {
		t.Fatal(err)
	}

	// Not found.
	if _, _, _, err := s.BackgroundOutput(context.Background(), conn, "s", "missing"); err == nil {
		t.Error("want not-found error")
	}

	// Running (nil ExitStatus).
	conn.outputFn = func(acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error) {
		return acp.TerminalOutputResponse{Output: "running"}, nil
	}
	out, running, code, err := s.BackgroundOutput(context.Background(), conn, "s", cmdID)
	if err != nil || !running || code != nil || out != "running" {
		t.Errorf("running case: out=%q running=%v code=%v err=%v", out, running, code, err)
	}

	// Exited (ExitStatus set).
	conn.outputFn = func(acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error) {
		return acp.TerminalOutputResponse{Output: "done", ExitStatus: &acp.TerminalExitStatus{ExitCode: intp(2)}}, nil
	}
	_, running, code, err = s.BackgroundOutput(context.Background(), conn, "s", cmdID)
	if err != nil || running || code == nil || *code != 2 {
		t.Errorf("exited case: running=%v code=%v err=%v", running, code, err)
	}

	// Output error.
	conn.outputFn = func(acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error) {
		return acp.TerminalOutputResponse{}, errors.New("io")
	}
	if _, _, _, err := s.BackgroundOutput(context.Background(), conn, "s", cmdID); err == nil {
		t.Error("want output error")
	}
}

func TestKillBackground(t *testing.T) {
	s := NewState()
	conn := &fakeConn{createFn: func(acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error) {
		return acp.CreateTerminalResponse{TerminalId: "bg-9"}, nil
	}}
	cmdID, err := s.StartBackground(context.Background(), conn, "s", "cmd", "/tmp", "tc")
	if err != nil {
		t.Fatal(err)
	}

	// Not found.
	if _, _, err := s.KillBackground(context.Background(), conn, "s", "missing"); err == nil {
		t.Error("want not-found error")
	}

	// With exit status.
	conn.outputFn = func(acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error) {
		return acp.TerminalOutputResponse{Output: "final", ExitStatus: &acp.TerminalExitStatus{ExitCode: intp(0)}}, nil
	}
	out, code, err := s.KillBackground(context.Background(), conn, "s", cmdID)
	if err != nil || out != "final" || code == nil || *code != 0 {
		t.Errorf("kill: out=%q code=%v err=%v", out, code, err)
	}
	s.mu.Lock()
	_, present := s.background[cmdID]
	s.mu.Unlock()
	if present {
		t.Error("killed command should be removed from background map")
	}
}

func TestKillBackground_NoExitStatus(t *testing.T) {
	s := NewState()
	conn := &fakeConn{createFn: func(acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error) {
		return acp.CreateTerminalResponse{TerminalId: "bg"}, nil
	}}
	cmdID, _ := s.StartBackground(context.Background(), conn, "s", "cmd", "/tmp", "tc")
	conn.outputFn = func(acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error) {
		return acp.TerminalOutputResponse{Output: "x"}, nil // ExitStatus nil
	}
	_, code, err := s.KillBackground(context.Background(), conn, "s", cmdID)
	if err != nil || code != nil {
		t.Errorf("want nil code, got code=%v err=%v", code, err)
	}
}

func TestCleanupPending(t *testing.T) {
	s := NewState()
	conn := &fakeConn{
		waitFn: func(context.Context, acp.WaitForTerminalExitRequest) (acp.WaitForTerminalExitResponse, error) {
			return acp.WaitForTerminalExitResponse{}, errors.New("never finishes cleanly")
		},
	}
	// Seed a pending entry directly (simulating an in-flight foreground cmd).
	s.mu.Lock()
	s.pendingBash["tc"] = "term-x"
	s.mu.Unlock()

	s.CleanupPending(context.Background(), conn, "s")
	if len(conn.killed) != 1 || len(conn.released) != 1 {
		t.Errorf("cleanup should kill+release pending: killed=%v released=%v", conn.killed, conn.released)
	}
	s.mu.Lock()
	n := len(s.pendingBash)
	s.mu.Unlock()
	if n != 0 {
		t.Errorf("pending not cleared: %d", n)
	}
}

func TestCleanupBackground(t *testing.T) {
	s := NewState()
	conn := &fakeConn{createFn: func(acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error) {
		return acp.CreateTerminalResponse{TerminalId: "bg"}, nil
	}}
	if _, err := s.StartBackground(context.Background(), conn, "s", "cmd", "/tmp", "tc"); err != nil {
		t.Fatal(err)
	}
	s.CleanupBackground(context.Background(), conn, "s")
	if len(conn.killed) != 1 || len(conn.released) != 1 {
		t.Errorf("cleanup should kill+release background: killed=%v released=%v", conn.killed, conn.released)
	}
	s.mu.Lock()
	n := len(s.background)
	s.mu.Unlock()
	if n != 0 {
		t.Errorf("background not cleared: %d", n)
	}
}
