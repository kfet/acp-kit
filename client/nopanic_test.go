package client

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
	"testing"
)

func TestSpawnPipeErrors(t *testing.T) {
	boom := errors.New("boom")
	oldIn, oldOut := stdinPipe, stdoutPipe
	t.Cleanup(func() { stdinPipe, stdoutPipe = oldIn, oldOut })

	stdinPipe = func(*exec.Cmd) (io.WriteCloser, error) { return nil, boom }
	if _, err := Start(context.Background(), Config{Command: []string{"true"}}); err == nil || !strings.Contains(err.Error(), "stdin pipe") {
		t.Fatalf("stdin: err = %v", err)
	}
	stdinPipe = oldIn
	stdoutPipe = func(*exec.Cmd) (io.ReadCloser, error) { return nil, boom }
	if _, err := Start(context.Background(), Config{Command: []string{"true"}}); err == nil || !strings.Contains(err.Error(), "stdout pipe") {
		t.Fatalf("stdout: err = %v", err)
	}
}

func TestProbeModelsTempDirError(t *testing.T) {
	t.Setenv("TMPDIR", "/nonexistent/acp-kit-tmp")
	a := &AgentProc{}
	if err := a.ProbeModels(context.Background()); err == nil || !strings.Contains(err.Error(), "probe models") {
		t.Fatalf("err = %v", err)
	}
}
