package update

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fleetHarness is a fleet host whose converge job is cmd. The returned
// channel receives every text the job's watcher posts.
func fleetHarness(t *testing.T, cmd string, mut func(*Config)) (*harness, chan string) {
	t.Helper()
	posts := make(chan string, 4)
	h := newHarness(t, func(c *Config) {
		c.Fleet = true
		c.ConvergeCmd = cmd
		c.PollInterval = time.Millisecond
		if mut != nil {
			mut(c)
		}
	})
	h.post = func(s string) error { posts <- s; return nil }
	return h, posts
}

func writeLock(t *testing.T, path, fir string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(`{"fir":"`+fir+`","zulip_acp":"0.1.0","resolved_at":"x","exts":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFleetRefusals(t *testing.T) {
	h, _ := fleetHarness(t, "", nil)
	if r := h.do("!update", "42"); !strings.Contains(r.Text, "no converge command") || r.After != nil {
		t.Fatal(r.Text)
	}
	h, _ = fleetHarness(t, "true", nil)
	if r := h.u.Handle(context.Background(), Request{Requester: "42", Text: "!update"}); !strings.Contains(r.Text, "no Request.Post") {
		t.Fatal(r.Text)
	}
	if r := h.do("!update fir --rollback", "42"); !strings.Contains(r.Text, "rollback would drift") {
		t.Fatal(r.Text)
	}
	if h.agentRuns+h.selfRuns != 0 {
		t.Fatal("binaries touched on a fleet host")
	}
}

func TestConvergeUpToDate(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "go")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("UPD_SECRET", "s3cret")
	h, posts := fleetHarness(t, `[ -z "$UPD_SECRET" ] && read x < `+fifo, func(c *Config) { c.SecretEnvNames = []string{"UPD_SECRET"} })
	r := h.do("!update", "42")
	if r.After != nil || !strings.Contains(r.Text, "running converge") {
		t.Fatal(r.Text)
	}
	// The job holds the update lock while it runs.
	if r = h.do("!update relay", "42"); !strings.Contains(r.Text, "already in progress") {
		t.Fatal(r.Text)
	}
	os.WriteFile(fifo, []byte("\n"), 0o600)
	if got := <-posts; !strings.HasPrefix(got, "✅ Already up to date.") || !strings.Contains(got, "| fir |") {
		t.Fatal(got)
	}
	if h.agentRuns+h.selfRuns != 0 {
		t.Fatal("converge path ran the in-place updaters")
	}
}

func TestConvergeLockChanged(t *testing.T) {
	lock := filepath.Join(t.TempDir(), "dist.lock")
	h, posts := fleetHarness(t, "", func(c *Config) { c.LockFile = lock })
	writeLock(t, lock, "1.0.0")
	h.u.cfg.ConvergeCmd = `printf '{"fir":"2.0.0","zulip_acp":"0.1.0","resolved_at":"y"}' > ` + lock
	h.do("!update", "42")
	want := "✅ Updated via converge: fir 1.0.0 → 1.0.0, zulip-acp 0.1.0 → 0.1.0.\ndist.lock: fir 1.0.0 → 2.0.0. (requested by Kfet)"
	if got := <-posts; got != want {
		t.Fatalf("%q", got)
	}
}

func TestConvergeFailures(t *testing.T) {
	h, posts := fleetHarness(t, "echo boom; exit 3", nil)
	h.do("!update", "42")
	if got := <-posts; !strings.Contains(got, "Converge failed (exit 3)") || !strings.Contains(got, "boom") {
		t.Fatal(got)
	}
	// A job that ends without an rc file.
	h, posts = fleetHarness(t, "true", func(c *Config) { c.ConvergeWrap = []string{"sh", "-c", "exit 0", "x"} })
	h.do("!update", "42")
	if got := <-posts; !strings.Contains(got, "did not finish") {
		t.Fatal(got)
	}
	// Timeout.
	h, posts = fleetHarness(t, "sleep 0.2", func(c *Config) { c.ConvergeTimeout = -1 })
	h.do("!update", "42")
	if got := <-posts; !strings.Contains(got, "did not finish") {
		t.Fatal(got)
	}
	// Start failure.
	h, _ = fleetHarness(t, "true", func(c *Config) { c.ConvergeWrap = []string{"/nonexistent/wrap"} })
	if r := h.do("!update", "42"); !strings.Contains(r.Text, "Could not start") {
		t.Fatal(r.Text)
	}
	if _, err := os.Stat(h.u.markerPath()); !os.IsNotExist(err) {
		t.Fatal("marker kept after start failure")
	}
	if r := h.do("!update", "42"); strings.Contains(r.Text, "in progress") {
		t.Fatal("lock kept after start failure")
	}
	// Log and marker cannot be written.
	h, _ = fleetHarness(t, "true", nil)
	logPath, _, _ := h.u.convergeFiles()
	os.MkdirAll(logPath, 0o755)
	if r := h.do("!update", "42"); !strings.Contains(r.Text, "converge log") {
		t.Fatal(r.Text)
	}
	h, _ = fleetHarness(t, "true", nil)
	os.MkdirAll(h.u.markerPath(), 0o755)
	if r := h.do("!update", "42"); !strings.Contains(r.Text, "update marker") {
		t.Fatal(r.Text)
	}
	// The lock is already held.
	h, _ = fleetHarness(t, "true", nil)
	h.u.acquire()
	if r := h.do("!update", "42"); !strings.Contains(r.Text, "already in progress") {
		t.Fatal(r.Text)
	}
}

func TestConvergeForce(t *testing.T) {
	h, posts := fleetHarness(t, "true", nil)
	h.cancelled = []string{"#a > t"}
	r := h.do("!update --force", "42")
	if !strings.Contains(r.Text, "Cancelled turns in: #a > t") || !strings.Contains(r.Text, "running converge") {
		t.Fatal(r.Text)
	}
	<-posts
	h, _ = fleetHarness(t, "true", nil)
	h.waitErr = context.DeadlineExceeded
	if r = h.do("!update --force", "42"); !strings.Contains(r.Text, "nothing was changed") {
		t.Fatal(r.Text)
	}
}

// TestConvergeResume is the reload case: the job re-exec'd the relay,
// and the new image reports once the job has ended.
func TestConvergeResume(t *testing.T) {
	h, _ := fleetHarness(t, "", nil)
	os.MkdirAll(h.u.cfg.StateDir, 0o755)
	logPath, rcPath, _ := h.u.convergeFiles()
	os.WriteFile(logPath, []byte("log"), 0o644)
	h.u.writeMarker(Marker{ConvID: "c9", OldAgent: "1.0.0", OldRelay: "0.0.9", At: time.Unix(0, 0), Converge: true})
	os.WriteFile(rcPath, []byte("0\n"), 0o644)
	got := make(chan string, 1)
	if err := h.u.Resume(context.Background(), func(c, t string) error { got <- c + ": " + t; return nil }); err != nil {
		t.Fatal(err)
	}
	if s := <-got; s != "c9: ✅ Updated via converge: fir 1.0.0 → 1.0.0, zulip-acp 0.0.9 → 0.1.0." {
		t.Fatal(s)
	}
	// A job that died without an rc file, found through its pid file,
	// in an image whose lock is already taken.
	h, _ = fleetHarness(t, "", nil)
	h.u.acquire()
	_, _, pidPath := h.u.convergeFiles()
	cmd := exec.Command("true")
	cmd.Start()
	os.WriteFile(pidPath, []byte(strconv.Itoa(cmd.Process.Pid)), 0o644)
	h.u.writeMarker(Marker{ConvID: "c9", At: time.Unix(0, 0), Converge: true})
	if err := h.u.Resume(context.Background(), func(_, t string) error { got <- t; return errors.New("post") }); err != nil {
		t.Fatal(err)
	}
	if s := <-got; !strings.Contains(s, "did not finish") {
		t.Fatal(s)
	}
}

func TestWatchEdges(t *testing.T) {
	var logged []string
	h, _ := fleetHarness(t, "", func(c *Config) {
		c.Logf = func(f string, a ...any) { logged = append(logged, f) }
	})
	os.MkdirAll(h.u.cfg.StateDir, 0o755)
	gone := func() bool { return true }
	called := false
	post := func(string) error { called = true; return errors.New("x") }
	// No marker: nothing to report.
	h.u.watch(context.Background(), gone, post)
	// Unparseable marker.
	os.WriteFile(h.u.markerPath(), []byte("nope"), 0o644)
	h.u.watch(context.Background(), gone, post)
	// This image shuts down first: no report, the marker stays.
	h.u.writeMarker(Marker{Converge: true})
	h.u.cfg.Sleep = func(context.Context, time.Duration) error { return context.Canceled }
	h.u.watch(context.Background(), func() bool { return false }, post)
	if _, err := os.Stat(h.u.markerPath()); err != nil {
		t.Fatal("marker dropped on shutdown")
	}
	if called {
		t.Fatal("posted without a valid marker")
	}
	// Post error is logged.
	h.u.writeMarker(Marker{Converge: true})
	h.u.watch(context.Background(), gone, post)
	if !called || len(logged) != 1 {
		t.Fatal(logged)
	}
	h.u.cfg.Logf = nil
	h.u.logf("dropped")
}

func TestConvergeHelpers(t *testing.T) {
	if e := scrubEnv([]string{"A=1", "KEY=s", "B"}, []string{"KEY"}); strings.Join(e, ",") != "A=1,B" {
		t.Fatal(e)
	}
	if d := diffLock(map[string]string{"a": "1", "resolved_at": "x", "b": "2"}, map[string]string{"a": "2", "c": "3"}); d != "a 1 → 2, b 2 → —, c — → 3" {
		t.Fatal(d)
	}
	if pidGone(os.Getpid()) {
		t.Fatal("self gone")
	}
	cmd := exec.Command("true")
	cmd.Start()
	for !pidGone(cmd.Process.Pid) {
	}
	if defaultSleep(context.Background(), time.Nanosecond) != nil {
		t.Fatal("sleep")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if defaultSleep(ctx, time.Hour) == nil {
		t.Fatal("cancelled sleep")
	}
	h, _ := fleetHarness(t, "", nil)
	if h.u.convergeGone() {
		t.Fatal("no pid file means not known gone")
	}
}
