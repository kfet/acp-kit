package mcphost

import (
	"os"
	"path/filepath"
	"testing"
)

// shortTempDir is a temp dir with a short path. A unix socket path is
// limited to about 104 bytes on macOS, and t.TempDir there is under a
// long /var/folders path.
func shortTempDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "mh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	d, err = filepath.EvalSymlinks(d)
	if err != nil {
		t.Fatal(err)
	}
	return d
}
