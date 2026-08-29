package client

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
)

// reapedProc wires a started child process into a bare AgentProc with the
// reaper running, without an ACP handshake. It isolates the exit
// classification from the protocol.
func reapedProc(t *testing.T, argv ...string) *AgentProc {
	t.Helper()
	cmd := exec.Command(argv[0], argv[1:]...) //nolint:gosec // test-controlled argv
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %v: %v", argv, err)
	}
	a := &AgentProc{cmd: cmd, done: make(chan struct{})}
	a.startReaper()
	return a
}

func TestReaperCleanExitIsAgentExited(t *testing.T) {
	a := reapedProc(t, "true")
	<-a.Done()
	if err := a.Err(); !errors.Is(err, ErrAgentExited) {
		t.Fatalf("Err = %v, want ErrAgentExited", err)
	}
	if errors.Is(a.Err(), ErrAgentClosed) {
		t.Fatal("clean self-exit must not read as a deliberate close")
	}
}

func TestReaperFailedExitCarriesExitError(t *testing.T) {
	a := reapedProc(t, "sh", "-c", "exit 3")
	<-a.Done()
	var ee *exec.ExitError
	if err := a.Err(); !errors.As(err, &ee) {
		t.Fatalf("Err = %v, want *exec.ExitError", err)
	}
	if ee.ExitCode() != 3 {
		t.Fatalf("exit code = %d, want 3", ee.ExitCode())
	}
}

// A never-started child has nothing to reap: Done stays open, Err reports
// "still running", and Close is a no-op. This is the in-process fake case.
func TestReaperNoProcess(t *testing.T) {
	a := &AgentProc{cmd: &exec.Cmd{}, done: make(chan struct{})}
	a.startReaper()
	select {
	case <-a.Done():
		t.Fatal("Done closed for a never-started child")
	default:
	}
	if err := a.Err(); err != nil {
		t.Fatalf("Err = %v, want nil", err)
	}
	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := (&AgentProc{}).Close(); err != nil {
		t.Fatalf("Close (nil cmd): %v", err)
	}
}

// Close must consume the single reaper's result rather than starting a
// second cmd.Wait: a double Wait returns "Wait was already called" and,
// under -race, a second close(done) would panic. Two Closes in a row and a
// clean ErrAgentClosed prove the reaper stayed the sole waiter.
func TestCloseConsumesReaperResult(t *testing.T) {
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
	if err := a.Err(); err != nil {
		t.Fatalf("Err before Close = %v, want nil", err)
	}
	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	<-a.Done()
	if err := a.Err(); !errors.Is(err, ErrAgentClosed) {
		t.Fatalf("Err = %v, want ErrAgentClosed", err)
	}
	if err := a.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if err := a.Err(); !errors.Is(err, ErrAgentClosed) {
		t.Fatalf("Err after second Close = %v, want ErrAgentClosed", err)
	}
}

// The incident shape: the agent dies on its own while the relay still holds
// it. Done fires, Err classifies it as unexpected, and subsequent ACP calls
// fail — so a relay can ask Err instead of string-matching broken pipes.
func TestUnexpectedAgentDeathIsObservable(t *testing.T) {
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

	if err := a.cmd.Process.Kill(); err != nil {
		t.Fatalf("kill: %v", err)
	}
	<-a.Done()
	got := a.Err()
	if got == nil || errors.Is(got, ErrAgentClosed) {
		t.Fatalf("Err = %v, want an unexpected-exit error", got)
	}
	var ee *exec.ExitError
	if !errors.As(got, &ee) {
		t.Fatalf("Err = %v, want *exec.ExitError", got)
	}
	if _, nerr := a.NewSession(ctx, t.TempDir(), &recSink{}, []acp.ContentBlock{}); nerr == nil {
		t.Fatal("NewSession on a dead agent: want error")
	}
}
