package remotefs

import (
	"errors"
	"io"
	"os/exec"
	"strings"
	"testing"
)

func TestStdoutPipeErrors(t *testing.T) {
	old := stdoutPipe
	t.Cleanup(func() { stdoutPipe = old })
	stdoutPipe = func(*exec.Cmd) (io.ReadCloser, error) { return nil, errors.New("boom") }
	s, _ := New("miki")
	if err := s.Push(t.Context(), pushSrc(t), "/dst"); err == nil || !strings.Contains(err.Error(), "stdout pipe") {
		t.Fatalf("push: err = %v", err)
	}
	if _, err := s.Fetch(t.Context(), "/x", t.TempDir()); err == nil || !strings.Contains(err.Error(), "stdout pipe") {
		t.Fatalf("fetch: err = %v", err)
	}
}
