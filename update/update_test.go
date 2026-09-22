package update

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type harness struct {
	u                   *Updater
	dir, bin            string
	agentRuns, selfRuns int
	reloads             int
	cancelled           []string
	waitErr, reloadErr  error
	agentErr, selfErr   error
	versions            map[string]string
}

func newHarness(t *testing.T, mut func(*Config)) *harness {
	t.Helper()
	h := &harness{dir: t.TempDir()}
	h.bin = filepath.Join(h.dir, "fir")
	if err := os.WriteFile(h.bin, []byte("v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	h.versions = map[string]string{h.bin: "1.0.0", "/relay": "0.1.0"}
	cfg := Config{
		RelayName: "zulip-acp", RelayVersion: "0.1.0", RelayBin: "/relay",
		AgentBin: h.bin, Owners: []string{"42"}, StateDir: filepath.Join(h.dir, "state"),
		UpdateAgent: func(context.Context) (string, error) {
			h.agentRuns++
			os.WriteFile(h.bin, []byte("v2"), 0o755)
			h.versions[h.bin] = "2.0.0"
			return "ok", h.agentErr
		},
		UpdateSelf: func(context.Context) (string, error) {
			h.selfRuns++
			h.versions["/relay"] = "0.2.0"
			return "relay out", h.selfErr
		},
		AgentVersion: func() string { return "1.0.0" },
		CancelAll:    func() []string { return h.cancelled },
		WaitIdle:     func(context.Context) error { return h.waitErr },
		Reload:       func() error { h.reloads++; return h.reloadErr },
		Version:      func(_ context.Context, b string) string { return h.versions[b] },
		ProcExe:      func(int) string { return "/x (deleted)" },
		Now:          func() time.Time { return time.Unix(0, 0) },
	}
	if mut != nil {
		mut(&cfg)
	}
	h.u = New(cfg)
	return h
}

func (h *harness) do(text, who string) Result {
	return h.u.Handle(context.Background(), Request{ConvID: "conv1", Requester: who, Who: "Kfet", Text: text})
}

func TestIsCommand(t *testing.T) {
	for in, want := range map[string]bool{
		"!update": true, ".UPDATE fir": true, "/update --check": true,
		"update": false, "!updates": false, "": false, "!": false, "!status": false,
	} {
		if got := IsCommand(in); got != want {
			t.Errorf("IsCommand(%q)=%v", in, got)
		}
	}
}

func TestParse(t *testing.T) {
	cases := map[string]Op{
		"!update":                {Agent: true, Relay: true},
		"!update fir":            {Agent: true},
		"!update relay --force":  {Relay: true, Force: true},
		"!update --check":        {Agent: true, Relay: true, Check: true},
		"!update fir --rollback": {Agent: true, Rollback: true},
		"!update --rollback":     {Agent: true, Rollback: true},
		"!update agent self":     {Agent: true, Relay: true},
	}
	for in, want := range cases {
		got, err := Parse(in)
		if err != nil || got != want {
			t.Errorf("Parse(%q)=%+v,%v want %+v", in, got, err, want)
		}
	}
	for in, want := range map[string]string{
		"!update fir 1.2.3":        "version pins",
		"!update bogus":            "unknown argument",
		"!update relay --rollback": "fir only",
		"!update --check --force":  "--check",
	} {
		if _, err := Parse(in); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Parse(%q) err=%v", in, err)
		}
	}
}

func TestAuthz(t *testing.T) {
	h := newHarness(t, nil)
	for _, who := range []string{"", "7"} {
		if r := h.do("!update --check", who); !strings.Contains(r.Text, "owner-only") || r.After != nil {
			t.Fatalf("got %q", r.Text)
		}
	}
	if h.agentRuns+h.selfRuns != 0 {
		t.Fatal("ran update")
	}
	if r := h.do("!update 9.9", "42"); !strings.Contains(r.Text, "Usage") {
		t.Fatal(r.Text)
	}
}

func TestCheck(t *testing.T) {
	lock := filepath.Join(t.TempDir(), "dist.lock")
	os.WriteFile(lock, []byte(`{"zulip_acp":"0.0.9","fir":"0.9.0","exts":{}}`), 0o644)
	h := newHarness(t, func(c *Config) {
		c.LockFile, c.Fleet = lock, true
		c.AgentVersion = func() string { return "0.9.9" }
		c.AgentPID = func() int { return 5 }
	})
	r := h.do("!update --check", "42")
	for _, want := range []string{"| fir | 1.0.0 | 0.9.9 (binary replaced; reload pending) | 0.9.0 |", "| zulip-acp | 0.1.0 | 0.1.0 (binary replaced", "| 0.0.9 |", "Fleet-managed"} {
		if !strings.Contains(r.Text, want) {
			t.Fatalf("missing %q in:\n%s", want, r.Text)
		}
	}
	if h.reloads != 0 || r.After != nil {
		t.Fatal("check must not reload")
	}
}

func TestLockedEdgeCases(t *testing.T) {
	d := t.TempDir()
	bad := filepath.Join(d, "bad")
	os.WriteFile(bad, []byte("{"), 0o644)
	for _, lf := range []string{"", filepath.Join(d, "missing"), bad} {
		h := newHarness(t, func(c *Config) {
			c.LockFile = lf
			c.AgentVersion = nil
			c.AgentBin = ""
			c.AgentPID = func() int { return 0 }
			c.ProcExe = func(int) string { return "/x" }
		})
		if m := h.u.locked(); len(m) != 0 {
			t.Fatal(m)
		}
		if r := h.u.report(context.Background()); !strings.Contains(r, "| fir | ? | ? | — |") {
			t.Fatal(r)
		}
	}
}

func TestFleetRefusal(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Fleet = true })
	r := h.do("!update", "42")
	if !strings.Contains(r.Text, "fleet-managed") || r.After != nil || h.agentRuns != 0 {
		t.Fatal(r.Text)
	}
	r = h.do("!update relay --force", "42")
	if r.After == nil || h.selfRuns != 1 {
		t.Fatal(r.Text)
	}
}

func TestFullUpdateAndMarkerRoundTrip(t *testing.T) {
	h := newHarness(t, nil)
	r := h.do("!update", "42")
	if r.After == nil || h.agentRuns != 1 || h.selfRuns != 1 {
		t.Fatalf("%q", r.Text)
	}
	if b, _ := os.ReadFile(h.bin + ".prev"); string(b) != "v1" {
		t.Fatalf("prev=%q", b)
	}
	// Concurrent update refused while the lock is held.
	if r2 := h.do("!update", "42"); !strings.Contains(r2.Text, "already in progress") {
		t.Fatal(r2.Text)
	}
	if err := r.After(); err != nil || h.reloads != 1 {
		t.Fatal(err)
	}
	// New image.
	h2 := newHarness(t, func(c *Config) { c.StateDir = h.u.cfg.StateDir; c.AgentBin = h.bin; c.RelayVersion = "0.2.0" })
	h2.versions[h.bin] = "2.0.0"
	var conv, text string
	err := h2.u.Resume(context.Background(), func(c, t string) error { conv, text = c, t; return nil })
	if err != nil || conv != "conv1" || text != "✅ Updated and reloaded: fir 1.0.0 → 2.0.0, zulip-acp 0.1.0 → 0.2.0. (requested by Kfet)" {
		t.Fatalf("%v %q %q", err, conv, text)
	}
	if _, err := os.Stat(h2.u.markerPath()); !os.IsNotExist(err) {
		t.Fatal("marker not deleted")
	}
	if err := h2.u.Resume(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
}

func TestResumeErrors(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.AgentBin = "" })
	os.MkdirAll(h.u.cfg.StateDir, 0o755)
	os.WriteFile(h.u.markerPath(), []byte("nope"), 0o644)
	if err := h.u.Resume(context.Background(), nil); err == nil {
		t.Fatal("want parse error")
	}
	h.u.writeMarker(Marker{ConvID: "c", Rollback: true, OldAgent: "1", At: time.Unix(0, 0)})
	var text string
	boom := errors.New("boom")
	if err := h.u.Resume(context.Background(), func(_, t string) error { text = t; return boom }); err != boom {
		t.Fatal(err)
	}
	if !strings.HasPrefix(text, "✅ Rolled back and reloaded: fir 1 → ?") {
		t.Fatal(text)
	}
	if _, err := os.Stat(h.u.markerPath()); !os.IsNotExist(err) {
		t.Fatal("marker kept after failed post")
	}
	// Unreadable marker (a directory).
	os.Mkdir(h.u.markerPath(), 0o755)
	if err := h.u.Resume(context.Background(), nil); err == nil {
		t.Fatal("want read error")
	}
}

func TestForce(t *testing.T) {
	h := newHarness(t, nil)
	h.cancelled = []string{"#a > t1", "DM"}
	r := h.do("!update fir --force", "42")
	if r.After == nil || !strings.Contains(r.Text, "Cancelled turns in: #a > t1, DM") {
		t.Fatal(r.Text)
	}
	r.After()

	h = newHarness(t, nil)
	h.waitErr = context.DeadlineExceeded
	r = h.do("!update relay --force", "42")
	if r.After != nil || !strings.Contains(r.Text, "NOT reloading") || h.reloads != 0 {
		t.Fatal(r.Text)
	}
	// Lock released: a retry proceeds.
	h.waitErr = nil
	if r = h.do("!update relay --force", "42"); r.After == nil {
		t.Fatal(r.Text)
	}
}

func TestFailures(t *testing.T) {
	h := newHarness(t, nil)
	h.agentErr = errors.New("net")
	if r := h.do("!update", "42"); !strings.Contains(r.Text, "`fir update` failed") || h.selfRuns != 0 {
		t.Fatal(r.Text)
	}
	h.agentErr, h.selfErr = nil, errors.New("x")
	if r := h.do("!update relay", "42"); !strings.Contains(r.Text, "`zulip-acp update` failed") || !strings.Contains(r.Text, "relay out") {
		t.Fatal(r.Text)
	}
	h.selfErr, h.reloadErr = nil, errors.New("nohup")
	r := h.do("!update relay", "42")
	if err := r.After(); err == nil {
		t.Fatal("want reload error")
	}
	if _, err := os.Stat(h.u.markerPath()); !os.IsNotExist(err) {
		t.Fatal("marker left after failed reload")
	}
	h.reloadErr = nil
	if r := h.do("!update relay", "42"); r.After == nil {
		t.Fatal("lock not released after failed reload: " + r.Text)
	}

	// Unknown agent binary.
	h = newHarness(t, func(c *Config) { c.AgentBin = "" })
	if r := h.do("!update fir", "42"); !strings.Contains(r.Text, "unknown") {
		t.Fatal(r.Text)
	}
	// Missing agent binary: .prev copy fails.
	h = newHarness(t, func(c *Config) { c.AgentBin = filepath.Join(t.TempDir(), "nope") })
	if r := h.do("!update fir", "42"); !strings.Contains(r.Text, ".prev") {
		t.Fatal(r.Text)
	}
	// Unwritable .prev destination (a directory is in the way of the temp file).
	h = newHarness(t, nil)
	os.MkdirAll(filepath.Join(h.bin+".prev.new.tmp", "f"), 0o755)
	if r := h.do("!update fir", "42"); !strings.Contains(r.Text, "Nothing was changed") {
		t.Fatal(r.Text)
	}
	// State dir is a file.
	f := filepath.Join(t.TempDir(), "f")
	os.WriteFile(f, nil, 0o644)
	h = newHarness(t, func(c *Config) { c.StateDir = f })
	if r := h.do("!update relay", "42"); !strings.Contains(r.Text, "state dir") {
		t.Fatal(r.Text)
	}
	// Lock path is a directory.
	h = newHarness(t, nil)
	os.MkdirAll(filepath.Join(h.u.cfg.StateDir, "update.lock"), 0o755)
	if r := h.do("!update relay", "42"); !strings.Contains(r.Text, "update lock") {
		t.Fatal(r.Text)
	}
	// Marker cannot be written.
	h = newHarness(t, nil)
	os.MkdirAll(filepath.Join(h.u.cfg.StateDir, "update-marker.json.tmp", "x"), 0o755)
	if r := h.do("!update relay", "42"); !strings.Contains(r.Text, "reload marker") || r.After != nil {
		t.Fatal(r.Text)
	}
}

func TestRollback(t *testing.T) {
	h := newHarness(t, nil)
	if r := h.do("!update fir --rollback", "42"); !strings.Contains(r.Text, "no fir.prev") {
		t.Fatal(r.Text)
	}
	r := h.do("!update fir", "42")
	r.After()
	h.u.release() // the exec would drop it
	if b, _ := os.ReadFile(h.bin); string(b) != "v2" {
		t.Fatal(string(b))
	}
	r = h.do("!update fir --rollback", "42")
	if r.After == nil || !strings.Contains(r.Text, "Restored `fir` from `fir.prev`") {
		t.Fatal(r.Text)
	}
	r.After()
	h.u.release() // the exec would drop it
	a, _ := os.ReadFile(h.bin)
	p, _ := os.ReadFile(h.bin + ".prev")
	if string(a) != "v1" || string(p) != "v2" || h.agentRuns != 1 {
		t.Fatalf("bin=%q prev=%q", a, p)
	}
	// copy of current fails: .rollback.tmp blocked.
	os.MkdirAll(filepath.Join(h.bin+".rollback.tmp", "x"), 0o755)
	if r := h.do("!update --rollback", "42"); !strings.Contains(r.Text, "Rollback failed") {
		t.Fatal(r.Text)
	}
	os.RemoveAll(h.bin + ".rollback.tmp")
	// install of prev fails: bin.tmp blocked.
	os.MkdirAll(filepath.Join(h.bin+".tmp", "x"), 0o755)
	if r := h.do("!update --rollback", "42"); !strings.Contains(r.Text, "Rollback failed") {
		t.Fatal(r.Text)
	}
}

func TestDefaultsAndHelpers(t *testing.T) {
	u := New(Config{RelayName: "poe-acp"})
	c := u.cfg
	if c.AgentName != "fir" || c.RelayLockKey != "poe_acp" || c.AgentLockKey != "fir" || c.DrainTimeout != 30*time.Second || c.RelayBin == "" {
		t.Fatalf("%+v", c)
	}
	if c.ProcExe(os.Getpid()) == "" || c.Now().IsZero() {
		t.Fatal("defaults")
	}
	if c.Reload == nil || !strings.Contains(HelpLine, "!update") {
		t.Fatal("reload default")
	}
	// runningAgent never reports the on-disk version as running.
	h := newHarness(t, func(c *Config) { c.AgentVersion = func() string { return "" } })
	if v := h.u.runningAgent(); v != "?" {
		t.Fatal(v)
	}
	if _, err := Parse("!"); err != nil {
		t.Fatal(err)
	}
	if fence("") != "" || !strings.HasPrefix(fence(strings.Repeat("x", 2000)), "```\n…") {
		t.Fatal("fence")
	}
}

func TestBinVersionAndCommandHook(t *testing.T) {
	d := t.TempDir()
	script := func(name, body string) string {
		p := filepath.Join(d, name)
		os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755)
		return p
	}
	ctx := context.Background()
	for bin, want := range map[string]string{
		script("a", "echo 'fir v1.18.4'; echo License"): "1.18.4",
		script("b", "echo 0.36.0"):                      "0.36.0",
		script("c", "true"):                             "?",
		script("d", "exit 1"):                           "?",
	} {
		if got := BinVersion(ctx, bin); got != want {
			t.Errorf("%s: %q", bin, got)
		}
	}
	out, err := CommandHook(script("e", `echo "$@"`), "update", "-y")(ctx)
	if err != nil || out != "update -y\n" {
		t.Fatal(out, err)
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip()
	}
}

func TestDefaultReloadSendsSIGHUP(t *testing.T) {
	// Exercised in a child process so the SIGHUP cannot hit the test binary.
	if os.Getenv("UPDATE_RELOAD_CHILD") == "1" {
		signalIgnoreHUP()
		if err := New(Config{}).cfg.Reload(); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestDefaultReloadSendsSIGHUP")
	cmd.Env = append(os.Environ(), "UPDATE_RELOAD_CHILD=1")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
}

func TestForceUnsupported(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.CancelAll = nil })
	if r := h.do("!update --force", "42"); !strings.Contains(r.Text, "not supported") || h.agentRuns != 0 {
		t.Fatal(r.Text)
	}
}

func TestNoopUpdateKeepsPrev(t *testing.T) {
	h := newHarness(t, nil)
	h.u.cfg.UpdateAgent = func(context.Context) (string, error) { return "latest", nil }
	os.WriteFile(h.bin+".prev", []byte("old"), 0o755)
	r := h.do("!update fir", "42")
	if r.After == nil {
		t.Fatal(r.Text)
	}
	if b, _ := os.ReadFile(h.bin + ".prev"); string(b) != "old" {
		t.Fatalf("prev clobbered: %q", b)
	}
	if _, err := os.Stat(h.bin + ".prev.new"); !os.IsNotExist(err) {
		t.Fatal("candidate left behind")
	}
}

func TestStaleMarker(t *testing.T) {
	h := newHarness(t, nil)
	os.MkdirAll(h.u.cfg.StateDir, 0o755)
	h.u.writeMarker(Marker{ConvID: "c", At: time.Unix(0, 0).Add(-2 * time.Hour)})
	posted := false
	err := h.u.Resume(context.Background(), func(string, string) error { posted = true; return nil })
	if err == nil || posted || !strings.Contains(err.Error(), "discarded") {
		t.Fatal(err)
	}
	if _, err := os.Stat(h.u.markerPath()); !os.IsNotExist(err) {
		t.Fatal("stale marker kept")
	}
}

func TestAgentBinSymlinkResolved(t *testing.T) {
	d := t.TempDir()
	real := filepath.Join(d, "fir-real")
	os.WriteFile(real, nil, 0o755)
	link := filepath.Join(d, "fir")
	os.Symlink(real, link)
	if got := New(Config{AgentBin: link}).cfg.AgentBin; got != real {
		t.Fatal(got)
	}
	if got := New(Config{AgentBin: "/nonexistent/fir"}).cfg.AgentBin; got != "/nonexistent/fir" {
		t.Fatal(got)
	}
}
