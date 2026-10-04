package autoupdate

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kfet/acp-kit/update"
	"github.com/kfet/distkit"
)

type fakeSurface struct {
	mu      sync.Mutex
	n       int
	msgs    map[string]string
	order   []string
	postErr error
	editErr error
}

func (f *fakeSurface) Post(_ context.Context, text string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.postErr != nil {
		return "", f.postErr
	}
	f.n++
	id := fmt.Sprint(f.n)
	if f.msgs == nil {
		f.msgs = map[string]string{}
	}
	f.msgs[id] = text
	f.order = append(f.order, id)
	return id, nil
}

func (f *fakeSurface) Edit(_ context.Context, id, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.editErr != nil {
		return f.editErr
	}
	f.msgs[id] = text
	return nil
}

func (f *fakeSurface) get(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.msgs[id]
}

type env struct {
	t       *testing.T
	dir     string
	bin     string
	agent   string
	surf    *fakeSurface
	now     time.Time
	target  string
	checkE  error
	dlErr   error
	notes   string
	notesE  error
	reloads int
	reloadE error
	logs    []string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	e := &env{t: t, dir: dir, bin: filepath.Join(dir, "relay"), agent: filepath.Join(dir, "fir"),
		surf: &fakeSurface{}, now: time.Date(2026, 10, 4, 12, 0, 0, 0, time.Local), target: "v1.1.0"}
	write(t, e.bin, "old-relay")
	write(t, e.agent, "old-fir")
	return e
}

func write(t *testing.T, p, s string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(s), 0o755); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, _ := os.ReadFile(p)
	return string(b)
}

func (e *env) cfg() Config {
	return Config{
		Mode: Stage, RelayName: "zulip-acp", RelayVersion: "1.0.0", RelayBin: e.bin,
		AgentBin: e.agent, AgentVersion: func() string { return "1.2.0" },
		UpdateAgent: func(context.Context) (string, error) { write(e.t, e.agent, "new-fir"); return "", nil },
		Owners:      []string{"own"}, StateDir: e.dir, Surface: e.surf,
		Reload: func() error { e.reloads++; return e.reloadE },
		Check: func(context.Context) (*distkit.Status, error) {
			if e.checkE != nil {
				return nil, e.checkE
			}
			return &distkit.Status{Target: e.target, Release: &distkit.Release{TagName: e.target}}, nil
		},
		Download: func(_ context.Context, rel *distkit.Release, dir string) (string, error) {
			if e.dlErr != nil {
				return "", e.dlErr
			}
			p := filepath.Join(dir, "relay")
			write(e.t, p, "relay-"+rel.TagName)
			return p, nil
		},
		Notes: func(context.Context, string) (string, error) { return e.notes, e.notesE },
		Now:   func() time.Time { return e.now },
		Sleep: func(ctx context.Context, d time.Duration) error { e.now = e.now.Add(d); return ctx.Err() },
		Rand:  func(n int64) int64 { return n / 2 },
		Logf:  func(f string, a ...any) { e.logs = append(e.logs, fmt.Sprintf(f, a...)) },
	}
}

func (e *env) mgr(c Config) *Manager {
	e.t.Helper()
	m, err := New(c)
	if err != nil {
		e.t.Fatal(err)
	}
	return m
}

func TestParseMode(t *testing.T) {
	for in, want := range map[string]Mode{"": Stage, " Auto ": Auto, "off": Off, "notify": Notify, "stage": Stage} {
		if got, err := ParseMode(in); err != nil || got != want {
			t.Errorf("ParseMode(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := ParseMode("yes"); err == nil {
		t.Error("want error")
	}
}

func TestNewValidation(t *testing.T) {
	e := newEnv(t)
	c := e.cfg()
	c.Surface = nil
	if _, err := New(c); err == nil {
		t.Error("want missing surface error")
	}
	c = e.cfg()
	c.Fleet = true
	if _, err := New(c); err == nil {
		t.Error("want fleet without updater error")
	}
	c = e.cfg()
	c.QuietHours = "nope"
	if _, err := New(c); err == nil {
		t.Error("want quiet hours error")
	}
	// Defaults, off mode needs nothing.
	m, err := New(Config{Mode: Off, RelayName: "x-y", StateDir: e.dir})
	if err != nil || m.Mode() != Off || m.cfg.RelayLockKey != "x_y" || m.cfg.AgentLockKey != "fir" || m.cfg.RelayBin == "" {
		t.Fatalf("%v %+v", err, m.cfg)
	}
	if !m.cfg.Idle() || m.cfg.HealthProbe(context.Background()) != nil || m.cfg.Rand(10) < 0 {
		t.Error("defaults")
	}
	m.Poll(context.Background()) // off: no-op
	m.Run(context.Background())  // off: returns after resume
	// Default zero mode = stage; auto on fleet degrades to stage.
	c = e.cfg()
	c.Mode, c.Fleet, c.Updater = Auto, true, update.New(update.Config{})
	if m := e.mgr(c); m.Mode() != Stage {
		t.Error("fleet auto must degrade")
	}
	c = e.cfg()
	c.Mode = ""
	if m := e.mgr(c); m.Mode() != Stage {
		t.Error("default stage")
	}
}

func TestDefaultSeams(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()
	e := newEnv(t)
	m, err := New(Config{Mode: Stage, StateDir: e.dir, Surface: e.surf, Reload: func() error { return nil },
		RelayVersion: "1.0.0", RelayBin: e.bin,
		Dist: distkit.Config{Repo: "a/b", Binary: "relay", APIBase: srv.URL, DownloadBase: srv.URL}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := m.cfg.Check(ctx); err == nil {
		t.Error("check against 404 server should fail")
	}
	if _, err := m.cfg.Download(ctx, &distkit.Release{TagName: "v1"}, e.dir); err == nil {
		t.Error("download should fail")
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := m.cfg.Notes(cctx, "v1"); err == nil {
		t.Error("notes with cancelled ctx should fail")
	}
	if sleep(cctx, time.Hour) == nil {
		t.Error("sleep should see cancel")
	}
	if sleep(ctx, time.Millisecond) != nil {
		t.Error("sleep")
	}
}

func TestGithubNotes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/a/b/releases/tags/v1":
			fmt.Fprint(w, `{"body":"- x"}`)
		case "/repos/a/b/releases/tags/bad":
			fmt.Fprint(w, `{`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	f := githubNotes(srv.Client(), srv.URL, "a/b")
	ctx := context.Background()
	if s, err := f(ctx, "v1"); err != nil || s != "- x" {
		t.Errorf("%q %v", s, err)
	}
	if _, err := f(ctx, "bad"); err == nil {
		t.Error("bad json")
	}
	if _, err := f(ctx, "v2"); err == nil {
		t.Error("404")
	}
	if _, err := githubNotes(nil, "http://\x7f", "a/b")(ctx, "v1"); err == nil {
		t.Error("bad url")
	}
}

func TestHelpers(t *testing.T) {
	if newer("1.0", "0.1.0") || newer("1.0.0", "x") || !newer("v1.10.0", "1.9.9") || newer("1.0.0", "1.0.0") || newer("1.a.0", "1.0.0") {
		t.Error("newer")
	}
	if !newer("1.0.1-rc1", "1.0.0") {
		t.Error("suffix")
	}
	if ensureV("") != "" || ensureV("v1") != "v1" || ensureV("1") != "v1" {
		t.Error("ensureV")
	}
	if requiredAgent("Requires fir >= v1.25.0") != "1.25.0" || requiredAgent("nothing") != "" {
		t.Error("requiredAgent")
	}
	h := highlights("intro\n- a\n* BREAKING: b\n- c\n- d\n", 3)
	if strings.Join(h, "|") != "BREAKING: b|a|c" {
		t.Errorf("%v", h)
	}
	for _, s := range []string{"25:00-1:00", "1:61-2:00", "x-1", "1:00", "1:xx-2"} {
		if _, err := parseQuiet(s); err == nil {
			t.Errorf("parseQuiet(%q) should fail", s)
		}
	}
	q, _ := parseQuiet("22:00-07")
	at := func(h int) time.Time { return time.Date(2026, 1, 1, h, 0, 0, 0, time.Local) }
	if !q.in(at(23)) || !q.in(at(3)) || q.in(at(12)) {
		t.Error("wrap")
	}
	q, _ = parseQuiet("9-17")
	if !q.in(at(10)) || q.in(at(18)) || (quietHours{}).in(at(1)) {
		t.Error("day")
	}
	if len(readLock("/nonexistent")) != 0 {
		t.Error("readLock")
	}
	dir := t.TempDir()
	write(t, filepath.Join(dir, "bad"), "{")
	if len(readLock(filepath.Join(dir, "bad"))) != 0 {
		t.Error("readLock bad")
	}
	if copyFile("/nonexistent", filepath.Join(dir, "x")) == nil {
		t.Error("copy missing")
	}
	if copyFile(dir, filepath.Join(dir, "x")) == nil {
		t.Error("copy dir")
	}
	write(t, filepath.Join(dir, "f"), "x")
	if copyFile(filepath.Join(dir, "f"), filepath.Join(dir, "no", "x")) == nil {
		t.Error("copy into missing dir")
	}
	if saveState(filepath.Join(dir, "no", "s.json"), persisted{}) == nil {
		t.Error("save into missing dir")
	}
	var logged string
	write(t, filepath.Join(dir, "s.json"), "{")
	if p := loadState(filepath.Join(dir, "s.json"), func(f string, a ...any) { logged = f }); p.Offer != nil || logged == "" {
		t.Error("corrupt state")
	}
}

func TestStageApplyHealthy(t *testing.T) {
	e := newEnv(t)
	e.notes = "- fix\n- BREAKING: thing"
	m := e.mgr(e.cfg())
	ctx := context.Background()
	m.Poll(ctx)
	id := e.surf.order[0]
	msg := e.surf.get(id)
	if !strings.Contains(msg, "zulip-acp v1.1.0 staged, sha verified (running v1.0.0)") || !strings.Contains(msg, "- BREAKING: thing\n- fix") {
		t.Fatalf("offer: %q", msg)
	}
	m.Poll(ctx) // same version: no second prompt
	if len(e.surf.order) != 1 {
		t.Fatal("duplicate prompt")
	}
	if m.Decide(ctx, id, "stranger", ApplyNow) || m.Decide(ctx, "nope", "own", ApplyNow) || m.Decide(ctx, id, "own", Decision(9)) {
		t.Fatal("non-owner / wrong msg / bad decision accepted")
	}
	if !strings.Contains(m.Status(), "awaiting an owner") {
		t.Error(m.Status())
	}
	if !m.Decide(ctx, id, "own", ApplyNow) {
		t.Fatal("owner decision rejected")
	}
	if !strings.Contains(m.Status(), "approved by own") {
		t.Error(m.Status())
	}
	m.Tick(ctx)
	if e.reloads != 1 || read(t, e.bin) != "relay-v1.1.0" || read(t, e.bin+".prev") != "old-relay" {
		t.Fatalf("apply: reloads=%d bin=%q", e.reloads, read(t, e.bin))
	}
	if !strings.Contains(m.Status(), "health gate") {
		t.Error(m.Status())
	}
	m.Poll(ctx) // applying: no poll
	// New image.
	c := e.cfg()
	c.RelayVersion = "1.1.0"
	calls := 0
	c.HealthProbe = func(context.Context) error {
		calls++
		if calls < 2 {
			return errors.New("not yet")
		}
		return nil
	}
	m2 := e.mgr(c)
	m2.resume(ctx)
	if !strings.Contains(e.surf.get(id), "v1.0.0 → v1.1.0 healthy") {
		t.Fatal(e.surf.get(id))
	}
	if !strings.Contains(m2.Status(), "No update pending") {
		t.Error(m2.Status())
	}
}

func TestHealthFailRollsBack(t *testing.T) {
	e := newEnv(t)
	e.notes = "requires fir >= 1.5.0"
	m := e.mgr(e.cfg())
	ctx := context.Background()
	m.Poll(ctx)
	id := e.surf.order[0]
	if !strings.Contains(e.surf.get(id), "needs fir ≥ 1.5.0") {
		t.Fatal(e.surf.get(id))
	}
	m.Decide(ctx, id, "own", ApplyNow)
	m.Tick(ctx)
	if read(t, e.agent) != "new-fir" || read(t, e.agent+".prev") != "old-fir" {
		t.Fatal("agent not updated with relay")
	}
	c := e.cfg()
	c.RelayVersion = "1.1.0"
	c.HealthProbe = func(context.Context) error { return errors.New("zulip down") }
	m2 := e.mgr(c)
	m2.resume(ctx)
	if read(t, e.bin) != "old-relay" || read(t, e.agent) != "old-fir" || e.reloads != 2 {
		t.Fatalf("rollback: bin=%q agent=%q reloads=%d", read(t, e.bin), read(t, e.agent), e.reloads)
	}
	// Old image reports.
	m3 := e.mgr(e.cfg())
	m3.resume(ctx)
	if !strings.Contains(e.surf.get(id), "rolled back to v1.0.0") || !strings.Contains(e.surf.get(id), "zulip down") {
		t.Fatal(e.surf.get(id))
	}
	st := m3.Status()
	if !strings.Contains(st, "Halted") || !strings.Contains(st, "Skipped: v1.1.0") {
		t.Error(st)
	}
	m3.Poll(ctx) // blocked: no new offer
	if len(e.surf.order) != 1 {
		t.Error("blocked version offered again")
	}
}

func TestHealthWrongVersionAndShutdown(t *testing.T) {
	e := newEnv(t)
	m := e.mgr(e.cfg())
	ctx := context.Background()
	m.st.Applying = &applying{Version: "v1.1.0", From: "v1.0.0", MsgID: "x"}
	e.surf.msgs = map[string]string{"x": ""}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	c := e.cfg()
	c.RelayVersion = "1.1.0"
	c.HealthProbe = func(ctx context.Context) error { return ctx.Err() }
	m2 := e.mgr(c)
	m2.st.Applying = m.st.Applying
	m2.resume(cctx) // shutting down: leave it
	if m2.st.Applying == nil || m2.st.Applying.RolledBack {
		t.Fatal("must leave the gate to the next image")
	}
	e.reloadE = errors.New("hup")
	write(t, e.bin+".prev", "prev")
	m.resume(ctx) // still running the old version: roll back
	if !m.st.Applying.RolledBack || !strings.Contains(m.st.Applying.Reason, "did not start") || len(e.logs) == 0 {
		t.Fatalf("%+v %v", m.st.Applying, e.logs)
	}
}

func TestGateBootsAndSaveFailure(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	c := e.cfg()
	c.RelayVersion = "1.1.0"
	c.HealthProbe = func(context.Context) error { t.Fatal("probe after boot loop"); return nil }
	m := e.mgr(c)
	write(t, e.bin+".prev", "prev")
	m.st.Applying = &applying{Version: "v1.1.0", From: "v1.0.0", Boots: 3, At: e.now}
	m.resume(ctx)
	if !m.st.Applying.RolledBack || !strings.Contains(m.st.Applying.Reason, "restarted 3 times") || read(t, e.bin) != "prev" {
		t.Fatalf("%+v", m.st.Applying)
	}
	// State cannot be saved before the reload: no reload, restored.
	e2 := newEnv(t)
	m2 := e2.mgr(e2.cfg())
	m2.Poll(ctx)
	m2.Decide(ctx, "1", "own", ApplyNow)
	os.Chmod(e2.dir, 0o555)
	defer os.Chmod(e2.dir, 0o755)
	m2.Tick(ctx)
	if e2.reloads != 0 {
		t.Fatal("reloaded without a saved gate")
	}
}

func TestSymlinkResolved(t *testing.T) {
	e := newEnv(t)
	link := filepath.Join(e.dir, "link")
	os.Symlink(e.agent, link)
	c := e.cfg()
	c.AgentBin = link
	if m := e.mgr(c); m.cfg.AgentBin != e.agent {
		t.Fatal(m.cfg.AgentBin)
	}
}

func TestRestoreErrorsLogged(t *testing.T) {
	e := newEnv(t)
	m := e.mgr(e.cfg())
	m.restore(&applying{Agent: true})
	if len(e.logs) != 2 {
		t.Fatal(e.logs)
	}
	m.cfg.AgentVersion = nil
	if m.runningAgent() != "?" {
		t.Error("runningAgent")
	}
}

func TestSupersedeSkipTomorrowRemind(t *testing.T) {
	e := newEnv(t)
	c := e.cfg()
	c.QuietHours = "00:00-06:00"
	m := e.mgr(c)
	ctx := context.Background()
	m.Poll(ctx)
	first := e.surf.order[0]
	e.target = "v1.2.0"
	m.Poll(ctx)
	if len(e.surf.order) != 2 || !strings.Contains(e.surf.get(first), "superseded by v1.2.0") {
		t.Fatal(e.surf.get(first))
	}
	second := e.surf.order[1]
	if m.Decide(ctx, first, "own", ApplyNow) {
		t.Fatal("decision on superseded offer")
	}
	// Reminder once after 72h.
	e.now = e.now.Add(73 * time.Hour)
	m.Poll(ctx)
	m.Poll(ctx)
	if len(e.surf.order) != 3 || !strings.Contains(e.surf.get(e.surf.order[2]), "Reminder") {
		t.Fatal(e.surf.order)
	}
	// Tomorrow: not applied today, not during quiet hours, applied after.
	m.Decide(ctx, second, "own", Tomorrow)
	m.Tick(ctx)
	if e.reloads != 0 {
		t.Fatal("applied too early")
	}
	e.now = time.Date(e.now.Year(), e.now.Month(), e.now.Day()+2, 3, 0, 0, 0, time.Local)
	m.Tick(ctx)
	if e.reloads != 0 {
		t.Fatal("applied in quiet hours")
	}
	e.now = e.now.Add(4 * time.Hour)
	m.Tick(ctx)
	if e.reloads != 1 {
		t.Fatal("not applied")
	}
	// Skip.
	e2 := newEnv(t)
	m2 := e2.mgr(e2.cfg())
	m2.Poll(ctx)
	id := e2.surf.order[0]
	m2.Decide(ctx, id, "own", SkipVersion)
	if !strings.Contains(e2.surf.get(id), "skipped by own") {
		t.Fatal(e2.surf.get(id))
	}
	m2.Poll(ctx)
	if len(e2.surf.order) != 1 {
		t.Fatal("skipped version re-offered")
	}
}

func TestUpToDateDropsStaleOffer(t *testing.T) {
	e := newEnv(t)
	m := e.mgr(e.cfg())
	ctx := context.Background()
	m.Poll(ctx)
	staged := m.st.Offer.Staged
	// Installed by other means.
	c := e.cfg()
	c.RelayVersion = "1.1.0"
	m2 := e.mgr(c)
	m2.Poll(ctx)
	if m2.st.Offer != nil {
		t.Fatal("stale offer kept")
	}
	if _, err := os.Stat(staged); err == nil {
		t.Fatal("staged file kept")
	}
	m2.Poll(ctx) // nothing at all
}

func TestPollErrors(t *testing.T) {
	e := newEnv(t)
	m := e.mgr(e.cfg())
	ctx := context.Background()
	e.checkE = errors.New("net")
	m.Poll(ctx)
	e.checkE = nil
	e.notesE = errors.New("no notes")
	m.Poll(ctx)
	e.notesE = nil
	e.dlErr = errors.New("sha mismatch")
	m.Poll(ctx)
	if m.st.Offer != nil || len(e.surf.order) != 0 {
		t.Fatal("offer despite failed download")
	}
	if len(e.logs) != 3 {
		t.Fatal(e.logs)
	}
	// Staging dir cannot be created.
	c := e.cfg()
	c.RelayBin = filepath.Join(e.dir, "missing", "relay")
	e.dlErr = nil
	e.mgr(c).Poll(ctx)
	// Surface failures are logged, not fatal; an empty id edit posts.
	e.surf.postErr = errors.New("post")
	m.Poll(ctx)
	e.surf.postErr = nil
	m.edit(ctx, "", "fallback")
	e.surf.editErr = errors.New("edit")
	m.edit(ctx, "1", "x")
	// Unwritable state dir.
	c = e.cfg()
	c.StateDir = filepath.Join(e.dir, "nostate")
	e.mgr(c).Poll(ctx)
}

func TestNotifyMode(t *testing.T) {
	e := newEnv(t)
	c := e.cfg()
	c.Mode = Notify
	m := e.mgr(c)
	ctx := context.Background()
	m.Poll(ctx)
	id := e.surf.order[0]
	if !strings.Contains(e.surf.get(id), "v1.1.0 available") {
		t.Fatal(e.surf.get(id))
	}
	m.Decide(ctx, id, "own", ApplyNow)
	m.Tick(ctx)
	if read(t, e.bin) != "relay-v1.1.0" {
		t.Fatal("not applied")
	}
	// Release moved on between offer and approval.
	e2 := newEnv(t)
	c = e2.cfg()
	c.Mode = Notify
	m2 := e2.mgr(c)
	m2.Poll(ctx)
	e2.target = "v1.3.0"
	m2.Decide(ctx, e2.surf.order[0], "own", ApplyNow)
	m2.Tick(ctx)
	if !strings.Contains(e2.surf.get(e2.surf.order[0]), "latest release is now v1.3.0") {
		t.Fatal(e2.surf.get(e2.surf.order[0]))
	}
}

func TestApplyFailures(t *testing.T) {
	ctx := context.Background()
	run := func(t *testing.T, notes string, mut func(e *env, c *Config)) (*env, string) {
		e := newEnv(t)
		e.notes = notes
		c := e.cfg()
		mut(e, &c)
		m := e.mgr(c)
		m.Poll(ctx)
		id := e.surf.order[0]
		m.Decide(ctx, id, "own", ApplyNow)
		m.Tick(ctx)
		return e, e.surf.get(id)
	}
	need := "requires fir 1.9.0"
	if _, msg := run(t, need, func(e *env, c *Config) { c.UpdateAgent = nil }); !strings.Contains(msg, "cannot update fir") {
		t.Error(msg)
	}
	if _, msg := run(t, need, func(e *env, c *Config) { c.AgentBin = filepath.Join(e.dir, "nofir") }); !strings.Contains(msg, "keep fir.prev") {
		t.Error(msg)
	}
	if _, msg := run(t, need, func(e *env, c *Config) {
		c.UpdateAgent = func(context.Context) (string, error) { return "boom\n", errors.New("exit 1") }
	}); !strings.Contains(msg, "fir update: exit 1: boom") {
		t.Error(msg)
	}
	if _, msg := run(t, "", func(e *env, c *Config) {
		c.Download = func(_ context.Context, _ *distkit.Release, dir string) (string, error) {
			os.Remove(e.bin) // .prev copy fails
			p := filepath.Join(dir, "relay")
			write(e.t, p, "x")
			return p, nil
		}
	}); !strings.Contains(msg, "keep .prev") {
		t.Error(msg)
	}
	if _, msg := run(t, "", func(e *env, c *Config) {
		c.Download = func(_ context.Context, _ *distkit.Release, dir string) (string, error) {
			return filepath.Join(dir, "gone"), nil // rename fails
		}
	}); !strings.Contains(msg, "Apply failed") {
		t.Error(msg)
	}
	e, _ := run(t, "", func(e *env, c *Config) { e.reloadE = errors.New("hup") })
	if read(t, e.bin) != "old-relay" || !strings.Contains(e.surf.get("1"), "not applied (hup)") {
		t.Errorf("reload failure: %q %q", read(t, e.bin), e.surf.get("1"))
	}
}

func TestAutoModeCooldownAndHalt(t *testing.T) {
	e := newEnv(t)
	c := e.cfg()
	c.Mode = Auto
	busy := true
	c.Idle = func() bool { return !busy }
	m := e.mgr(c)
	ctx := context.Background()
	m.Poll(ctx)
	if !strings.Contains(e.surf.get("1"), "auto_update=auto") {
		t.Fatal(e.surf.get("1"))
	}
	m.Tick(ctx)
	if e.reloads != 0 {
		t.Fatal("applied while busy")
	}
	busy = false
	m.Tick(ctx)
	if e.reloads != 1 || m.st.LastAuto.IsZero() {
		t.Fatal("auto apply")
	}
	// Healthy, then a new release within cooldown: prompt instead.
	m.st.Applying = nil
	e.target = "v1.2.0"
	m.Poll(ctx)
	if m.st.Offer.Approved {
		t.Fatal("auto applied within cooldown")
	}
	// An auto-approved offer that hits a halt before applying is demoted.
	m.st.Offer.Approved, m.st.Offer.Who = true, "auto"
	m.st.Halted = true
	m.Tick(ctx)
	if m.st.Offer.Approved || e.reloads != 1 {
		t.Fatal("halt ignored")
	}
}

func TestRunLoop(t *testing.T) {
	e := newEnv(t)
	c := e.cfg()
	ctx, cancel := context.WithCancel(context.Background())
	polls := 0
	c.Check = func(context.Context) (*distkit.Status, error) {
		polls++
		if polls == 2 {
			cancel()
		}
		return &distkit.Status{Target: "v1.0.0"}, nil
	}
	m := e.mgr(c)
	m.Run(ctx)
	if polls != 2 {
		t.Fatal(polls)
	}
}

func TestFleetLockOffer(t *testing.T) {
	e := newEnv(t)
	lock := filepath.Join(e.dir, "dist.lock")
	write(t, lock, `{"zulip_acp":"0.9.0","fir":"1.2.0","n":1}`)
	stamp := filepath.Join(e.dir, "converged")
	c := e.cfg()
	c.Fleet, c.LockFile, c.ConvID = true, lock, "conv"
	c.Updater = update.New(update.Config{
		RelayName: "zulip-acp", RelayVersion: "1.0.0", RelayBin: e.bin, AgentBin: e.agent,
		Owners: []string{"own"}, StateDir: filepath.Join(e.dir, "u"), Fleet: true, LockFile: lock,
		ConvergeCmd: "touch " + stamp, PollInterval: time.Millisecond,
		Version: func(context.Context, string) string { return "1.0.0" },
	})
	m := e.mgr(c)
	ctx := context.Background()
	m.Poll(ctx)
	id := e.surf.order[0]
	if !strings.Contains(e.surf.get(id), "dist.lock wants zulip-acp v0.9.0, running v1.0.0") {
		t.Fatal(e.surf.get(id))
	}
	m.Poll(ctx) // same drift: no new prompt
	// Agent drift too: supersedes.
	write(t, lock, `{"zulip_acp":"0.9.0","fir":"1.3.0"}`)
	m.Poll(ctx)
	id = e.surf.order[len(e.surf.order)-1]
	if !strings.Contains(e.surf.get(id), "(fir 1.3.0, running 1.2.0)") {
		t.Fatal(e.surf.get(id))
	}
	m.Decide(ctx, id, "own", ApplyNow)
	m.Tick(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(e.surf.get(id), "Converge") && !strings.Contains(e.surf.get(id), "Updated via converge") {
		if time.Now().After(deadline) {
			t.Fatal(e.surf.get(id))
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := os.Stat(stamp); err != nil {
		t.Fatal("converge did not run")
	}
	// Skip a lock offer, then converged by other means drops it.
	write(t, lock, `{"zulip_acp":"1.1.0"}`)
	m.Poll(ctx)
	id = e.surf.order[len(e.surf.order)-1]
	m.Decide(ctx, id, "own", SkipVersion)
	m.Poll(ctx)
	if m.st.Offer != nil {
		t.Fatal("skipped lock offer re-posted")
	}
	write(t, lock, `{"zulip_acp":"1.2.0"}`)
	m.Poll(ctx)
	write(t, lock, `{"zulip_acp":"1.0.0"}`)
	m.Poll(ctx)
	if m.st.Offer != nil {
		t.Fatal("converged lock offer kept")
	}
	if !strings.Contains(m.Status(), "fleet host") {
		t.Error(m.Status())
	}
}
