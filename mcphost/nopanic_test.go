package mcphost

import (
	"errors"
	"os"
	"strings"
	"testing"
)

func TestListenChmodError(t *testing.T) {
	h := newTestHost(t)
	old := chmod
	t.Cleanup(func() { chmod = old })
	chmod = func(string, os.FileMode) error { return errors.New("boom") }
	if err := h.Listen(); err == nil || !strings.Contains(err.Error(), "chmod socket") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(h.socket); !os.IsNotExist(err) {
		t.Fatalf("socket left behind: %v", err)
	}
}
