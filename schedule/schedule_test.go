package schedule

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// fixedClock is a hand-driven clock: no test here ever waits on a wall
// clock, so every branch is covered deterministically.
type fixedClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fixedClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fixedClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

var epoch = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

// harness builds a Store over a temp file with a hand-driven clock and
// tick channel, plus a fire recorder.
type harness struct {
	t      *testing.T
	store  *Store
	clock  *fixedClock
	ticks  chan time.Time
	path   string
	cancel context.CancelFunc
	done   chan struct{}

	mu    sync.Mutex
	fired []Item
	logs  []string

	fire   func(context.Context, Item) error
	result Result
	stops  []string
}

func newHarness(t *testing.T, tweak func(*Config)) *harness {
	t.Helper()
	h := &harness{
		t:     t,
		clock: &fixedClock{t: epoch},
		ticks: make(chan time.Time),
		path:  filepath.Join(t.TempDir(), "sched", "schedules.json"),
	}
	cfg := Config{
		Path:  h.path,
		Ticks: h.ticks,
		Now:   h.clock.now,
		Fire: func(ctx context.Context, it Item) (Result, error) {
			h.mu.Lock()
			h.fired = append(h.fired, it)
			f := h.fire
			res := h.result
			h.mu.Unlock()
			if f != nil {
				return res, f(ctx, it)
			}
			return res, nil
		},
		Stopped: func(it Item, why Stop) {
			h.mu.Lock()
			h.stops = append(h.stops, string(why)+":"+it.ID)
			h.mu.Unlock()
		},
		Logf: func(format string, args ...any) {
			h.mu.Lock()
			h.logs = append(h.logs, format)
			h.mu.Unlock()
		},
	}
	if tweak != nil {
		tweak(&cfg)
	}
	s, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	h.store = s
	return h
}

// run starts the loop and returns a stop func that waits for it.
func (h *harness) run() {
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	h.done = make(chan struct{})
	go func() {
		h.store.Run(ctx)
		close(h.done)
	}()
	h.t.Cleanup(func() {
		cancel()
		<-h.done
	})
}

// tick drives one loop iteration and waits for it to be observed.
func (h *harness) tick() { h.ticks <- h.clock.now() }

func (h *harness) firedIDs() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, len(h.fired))
	for i, it := range h.fired {
		out[i] = it.ID
	}
	return out
}

func TestOpenRequiresPathAndFire(t *testing.T) {
	if _, err := Open(Config{}); err == nil {
		t.Fatal("want error for missing Path")
	}
	if _, err := Open(Config{Path: "x"}); err == nil {
		t.Fatal("want error for missing Fire")
	}
}

func TestOpenDefaults(t *testing.T) {
	s, err := Open(Config{Path: filepath.Join(t.TempDir(), "s.json"), Fire: func(context.Context, Item) (Result, error) { return Result{}, nil }})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if s.cfg.DefaultUntil != DefaultUntil || s.cfg.DefaultMaxFires != DefaultMaxFires || s.cfg.MaxPerConv != DefaultMaxPerConv ||
		s.cfg.MaxTotal != DefaultMaxTotal || s.cfg.MinInterval != DefaultMinInterval || s.cfg.Tick != DefaultTick {
		t.Fatalf("defaults not applied: %+v", s.cfg)
	}
	if s.cfg.Now == nil || s.cfg.Logf == nil {
		t.Fatal("Now/Logf not defaulted")
	}
	s.cfg.Logf("noop %d", 1)
	if s.cfg.Now().IsZero() {
		t.Fatal("default clock is zero")
	}
}

func TestOpenReadError(t *testing.T) {
	dir := t.TempDir()
	// A directory where the file should be: ReadFile fails with
	// something that is not ErrNotExist.
	if err := os.MkdirAll(filepath.Join(dir, "s.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := Open(Config{Path: filepath.Join(dir, "s.json"), Fire: func(context.Context, Item) (Result, error) { return Result{}, nil }})
	if err == nil {
		t.Fatal("want read error")
	}
}

func TestOpenCorruptFileIsAnError(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.json")
	if err := os.WriteFile(p, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(Config{Path: p, Fire: func(context.Context, Item) (Result, error) { return Result{}, nil }}); err == nil {
		t.Fatal("want parse error")
	}
}

func TestAddValidation(t *testing.T) {
	h := newHarness(t, nil)
	future := epoch.Add(time.Hour)

	cases := []struct {
		name              string
		conv, text        string
		at                time.Time
		every             time.Duration
		wantErrSubstrHint string
	}{
		{name: "no conv", text: "x", at: future},
		{name: "no text", conv: "c", at: future},
		{name: "text too long", conv: "c", text: string(make([]byte, MaxTextLen+1)), at: future},
		{name: "past", conv: "c", text: "x", at: epoch.Add(-time.Second)},
		{name: "repeat too fast", conv: "c", text: "x", at: future, every: time.Second},
		{name: "negative repeat", conv: "c", text: "x", at: future, every: -time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := h.store.Add(tc.conv, tc.text, tc.at, tc.every, Budget{}); err == nil {
				t.Fatal("want error")
			}
		})
	}
}

func TestAddPersistsAndLists(t *testing.T) {
	h := newHarness(t, nil)
	a, err := h.store.Add("conv-1", "later", epoch.Add(2*time.Hour), 0, Budget{})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	b, err := h.store.Add("conv-1", "sooner", epoch.Add(time.Hour), time.Hour, Budget{})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := h.store.Add("conv-2", "elsewhere", epoch.Add(time.Hour), 0, Budget{}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if a.MaxFires != DefaultMaxFires || !b.Until.Equal(epoch.Add(DefaultUntil)) {
		t.Fatalf("want default budget, got %+v / %+v", a, b)
	}

	got := h.store.List("conv-1")
	if len(got) != 2 || got[0].ID != b.ID || got[1].ID != a.ID {
		t.Fatalf("List(conv-1) = %+v, want sooner first", got)
	}
	if len(h.store.List("")) != 3 {
		t.Fatalf("List(all) = %d, want 3", len(h.store.List("")))
	}

	// Reopen: the file round-trips, repeat interval included.
	re, err := Open(Config{Path: h.path, Fire: func(context.Context, Item) (Result, error) { return Result{}, nil }, Now: h.clock.now})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	back := re.List("conv-1")
	if len(back) != 2 || back[0].Every != time.Hour || back[0].Text != "sooner" {
		t.Fatalf("round-trip lost data: %+v", back)
	}
}

func TestSortItemsBreaksTiesByID(t *testing.T) {
	items := []Item{{ID: "sb", At: epoch}, {ID: "sa", At: epoch}}
	sortItems(items)
	if items[0].ID != "sa" {
		t.Fatalf("tie not broken by id: %+v", items)
	}
}

func TestAddCaps(t *testing.T) {
	t.Run("per conv", func(t *testing.T) {
		h := newHarness(t, func(c *Config) { c.MaxPerConv = 2 })
		for i := 0; i < 2; i++ {
			if _, err := h.store.Add("c", "x", epoch.Add(time.Hour), 0, Budget{}); err != nil {
				t.Fatalf("Add %d: %v", i, err)
			}
		}
		if _, err := h.store.Add("c", "x", epoch.Add(time.Hour), 0, Budget{}); err == nil {
			t.Fatal("want per-conv cap error")
		}
		// A different conversation is unaffected.
		if _, err := h.store.Add("other", "x", epoch.Add(time.Hour), 0, Budget{}); err != nil {
			t.Fatalf("other conv: %v", err)
		}
	})
	t.Run("total", func(t *testing.T) {
		h := newHarness(t, func(c *Config) { c.MaxTotal = 1 })
		if _, err := h.store.Add("c", "x", epoch.Add(time.Hour), 0, Budget{}); err != nil {
			t.Fatalf("Add: %v", err)
		}
		if _, err := h.store.Add("d", "x", epoch.Add(time.Hour), 0, Budget{}); err == nil {
			t.Fatal("want total cap error")
		}
	})
}

func TestAddCommitFailureUndoes(t *testing.T) {
	h := newHarness(t, nil)
	// Replace the parent dir with a file so MkdirAll fails.
	if err := os.RemoveAll(filepath.Dir(h.path)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Dir(h.path), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.Add("c", "x", epoch.Add(time.Hour), 0, Budget{}); err == nil {
		t.Fatal("want save error")
	}
	if got := h.store.List(""); len(got) != 0 {
		t.Fatalf("failed Add left state behind: %+v", got)
	}
}

func TestRemove(t *testing.T) {
	h := newHarness(t, nil)
	it, err := h.store.Add("c", "x", epoch.Add(time.Hour), 0, Budget{})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := h.store.Remove("c", "nope"); err == nil {
		t.Fatal("want unknown-id error")
	}
	// A different conversation cannot cancel it.
	if err := h.store.Remove("other", it.ID); err == nil {
		t.Fatal("want cross-conversation refusal")
	}
	if err := h.store.Remove("c", it.ID); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if len(h.store.List("")) != 0 {
		t.Fatal("Remove left the item")
	}
}

func TestRemoveCommitFailureRestores(t *testing.T) {
	h := newHarness(t, nil)
	it, err := h.store.Add("c", "x", epoch.Add(time.Hour), 0, Budget{})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := os.RemoveAll(filepath.Dir(h.path)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Dir(h.path), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.store.Remove("c", it.ID); err == nil {
		t.Fatal("want save error")
	}
	if len(h.store.List("")) != 1 {
		t.Fatal("failed Remove dropped the item anyway")
	}
}

func TestFireOneShot(t *testing.T) {
	h := newHarness(t, nil)
	fired := make(chan struct{})
	h.fire = func(context.Context, Item) error { close(fired); return nil }
	it, err := h.store.Add("c", "ping", epoch.Add(time.Minute), 0, Budget{})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	h.run()

	h.tick() // nothing due yet
	if len(h.store.List("")) != 1 {
		t.Fatal("item fired early")
	}
	h.clock.advance(2 * time.Minute)
	h.tick()
	<-fired
	if got := h.firedIDs(); len(got) != 1 || got[0] != it.ID {
		t.Fatalf("fired = %v", got)
	}
	if len(h.store.List("")) != 0 {
		t.Fatal("one-shot survived its fire")
	}
}

func TestFireRepeatRearms(t *testing.T) {
	h := newHarness(t, nil)
	fired := make(chan Item, 4)
	h.fire = func(_ context.Context, it Item) error { fired <- it; return nil }
	if _, err := h.store.Add("c", "ping", epoch.Add(time.Minute), time.Hour, Budget{}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	h.run()
	h.clock.advance(2 * time.Minute)
	h.tick()
	first := <-fired
	if first.Fires != 0 {
		t.Fatalf("first fire reported Fires=%d", first.Fires)
	}
	armed := h.store.List("c")
	if len(armed) != 1 || !armed[0].At.Equal(epoch.Add(time.Minute).Add(time.Hour).UTC()) {
		t.Fatalf("re-arm wrong: %+v", armed)
	}
	if armed[0].Fires != 1 {
		t.Fatalf("Fires = %d, want 1", armed[0].Fires)
	}
}

func TestNextAfterSkipsMissedSlots(t *testing.T) {
	at := epoch
	// Down for a day with a minutely repeat: one jump, not 1440 fires.
	got := nextAfter(at, time.Minute, epoch.Add(24*time.Hour))
	if !got.Equal(epoch.Add(24*time.Hour + time.Minute)) {
		t.Fatalf("nextAfter = %v", got)
	}
	got = nextAfter(at, time.Hour, epoch.Add(time.Minute))
	if !got.Equal(epoch.Add(time.Hour)) {
		t.Fatalf("nextAfter (no catch-up) = %v", got)
	}
}

func TestFireErrorIsLogged(t *testing.T) {
	h := newHarness(t, nil)
	done := make(chan struct{})
	h.fire = func(context.Context, Item) error { close(done); return errors.New("boom") }
	if _, err := h.store.Add("c", "ping", epoch, 0, Budget{}); err != nil {
		// epoch is not in the past relative to itself, so this is fine.
		t.Fatalf("Add: %v", err)
	}
	h.run()
	h.tick()
	<-done
	h.cancel()
	<-h.done
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.logs) == 0 {
		t.Fatal("fire error was not logged")
	}
}

func TestFireGoneRemovesItem(t *testing.T) {
	h := newHarness(t, nil)
	done := make(chan struct{})
	h.fire = func(context.Context, Item) error { close(done); return ErrGone }
	if _, err := h.store.Add("c", "ping", epoch, time.Hour, Budget{}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	h.run()
	h.tick()
	<-done
	h.cancel()
	<-h.done
	if got := h.store.List(""); len(got) != 0 {
		t.Fatalf("ErrGone left %d items armed", len(got))
	}
}

func TestFireGonePersistFailureIsLogged(t *testing.T) {
	h := newHarness(t, nil)
	done := make(chan struct{})
	h.fire = func(context.Context, Item) error {
		breakDir(t, h.path)
		close(done)
		return ErrGone
	}
	if _, err := h.store.Add("c", "ping", epoch, time.Hour, Budget{}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	h.run()
	h.tick()
	<-done
	h.cancel()
	<-h.done
	h.mu.Lock()
	defer h.mu.Unlock()
	if !hasLog(h.logs, "schedule: persisting after fire: %v") {
		t.Fatalf("want the persist failure logged, got %v", h.logs)
	}
	if len(h.store.items) != 1 {
		t.Fatal("a failed removal must be undone")
	}
}

// breakDir replaces the store's directory with a file so every save
// fails.
func breakDir(t *testing.T, path string) {
	t.Helper()
	if err := os.RemoveAll(filepath.Dir(path)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Dir(path), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func hasLog(logs []string, want string) bool {
	for _, l := range logs {
		if l == want {
			return true
		}
	}
	return false
}

func TestDuePersistFailureStillFires(t *testing.T) {
	h := newHarness(t, nil)
	done := make(chan struct{})
	h.fire = func(context.Context, Item) error { close(done); return nil }
	if _, err := h.store.Add("c", "ping", epoch, 0, Budget{}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	// Break the path AFTER arming, so only the post-claim save fails.
	if err := os.RemoveAll(filepath.Dir(h.path)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Dir(h.path), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.run()
	h.tick()
	<-done
	h.cancel()
	<-h.done
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.logs) == 0 {
		t.Fatal("persist failure was not logged")
	}
}

func TestRekeyMovesItems(t *testing.T) {
	h := newHarness(t, nil)
	for _, c := range []string{"a", "a", "b"} {
		if _, err := h.store.Add(c, "x", epoch.Add(time.Hour), 0, Budget{}); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	n, err := h.store.Rekey("a", "z")
	if err != nil || n != 2 {
		t.Fatalf("Rekey = %d, %v; want 2, nil", n, err)
	}
	if got := h.store.List("a"); len(got) != 0 {
		t.Fatalf("items left under the old token: %+v", got)
	}
	if got := h.store.List("z"); len(got) != 2 {
		t.Fatalf("List(z) = %+v, want 2 items", got)
	}
	if got := h.store.List("b"); len(got) != 1 {
		t.Fatalf("an unrelated conversation moved: %+v", got)
	}
	// The move is on disk.
	s2, err := Open(Config{Path: h.path, Fire: func(context.Context, Item) (Result, error) { return Result{}, nil }})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got := s2.List("z"); len(got) != 2 {
		t.Fatalf("reopened List(z) = %+v, want 2 items", got)
	}
}

func TestRekeyNoOps(t *testing.T) {
	h := newHarness(t, nil)
	if _, err := h.store.Add("a", "x", epoch.Add(time.Hour), 0, Budget{}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	for _, c := range [][2]string{{"a", "a"}, {"", "a"}, {"a", ""}, {"none", "z"}} {
		if n, err := h.store.Rekey(c[0], c[1]); n != 0 || err != nil {
			t.Fatalf("Rekey(%q, %q) = %d, %v; want 0, nil", c[0], c[1], n, err)
		}
	}
	if got := h.store.List("a"); len(got) != 1 {
		t.Fatalf("a no-op moved items: %+v", got)
	}
}

func TestRekeyCommitFailureUndoes(t *testing.T) {
	h := newHarness(t, nil)
	if _, err := h.store.Add("a", "x", epoch.Add(time.Hour), 0, Budget{}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	f := &firing{conv: "a", id: "p", left: 1}
	h.store.firing[f] = struct{}{}
	// Replace the parent dir with a file so MkdirAll fails.
	if err := os.RemoveAll(filepath.Dir(h.path)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Dir(h.path), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if n, err := h.store.Rekey("a", "z"); err == nil || n != 0 {
		t.Fatalf("Rekey = %d, %v; want 0 and a save error", n, err)
	}
	if got := h.store.List("a"); len(got) != 1 {
		t.Fatalf("a failed Rekey moved items: %+v", got)
	}
	if f.conv != "a" {
		t.Fatalf("a failed Rekey moved a running fire to %q", f.conv)
	}
}

// TestRekeyCarriesRunningBudget pins the budget across a move: a
// scheduled turn whose conversation is re-keyed mid-turn must still arm
// its children on the parent's remaining budget, not a fresh one.
func TestRekeyCarriesRunningBudget(t *testing.T) {
	h := newHarness(t, nil)
	type result struct {
		it  Item
		err error
	}
	got := make(chan result, 1)
	h.fire = func(_ context.Context, it Item) error {
		if _, err := h.store.Rekey(it.Conv, "moved"); err != nil {
			got <- result{err: err}
			return nil
		}
		child, err := h.store.Add("moved", "child", h.clock.now().Add(time.Hour), 0, Budget{})
		got <- result{child, err}
		return nil
	}
	if _, err := h.store.Add("c", "start", epoch, 0, Budget{}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	h.run()
	h.tick()
	r := <-got
	if r.err != nil {
		t.Fatalf("child: %v", r.err)
	}
	if r.it.MaxFires != DefaultMaxFires-1 || !r.it.Until.Equal(epoch.Add(DefaultUntil)) {
		t.Fatalf("child did not inherit the parent's budget: %+v", r.it)
	}
}

func TestItemJSONErrors(t *testing.T) {
	var it Item
	if err := json.Unmarshal([]byte(`{"every":"nonsense"}`), &it); err == nil {
		t.Fatal("want duration parse error")
	}
	if err := json.Unmarshal([]byte(`[]`), &it); err == nil {
		t.Fatal("want type error")
	}
	b, err := Item{ID: "s1", Every: time.Hour}.MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	if !contains(string(b), `"every":"1h0m0s"`) {
		t.Fatalf("every not rendered readably: %s", b)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

func TestRunUsesInternalTicker(t *testing.T) {
	// Exercises the default-ticker branch of Run without waiting on it:
	// the context is already cancelled, so the loop returns at once.
	s, err := Open(Config{
		Path: filepath.Join(t.TempDir(), "s.json"),
		Tick: time.Millisecond,
		Fire: func(context.Context, Item) (Result, error) { return Result{}, nil },
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.Run(ctx)
}

func TestSaveWriteFailure(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "ro")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := Open(Config{Path: filepath.Join(sub, "s.json"), Fire: func(context.Context, Item) (Result, error) { return Result{}, nil }, Now: func() time.Time { return epoch }})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// The directory exists, so MkdirAll succeeds and only the write can
	// fail — the branch a missing parent never reaches.
	if err := os.Chmod(sub, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sub, 0o755) })
	if _, err := s.Add("c", "x", epoch.Add(time.Hour), 0, Budget{}); err == nil {
		t.Fatal("want write error")
	}
}

func TestSaveRenameFailure(t *testing.T) {
	dir := t.TempDir()
	// The destination path is a NON-EMPTY directory: mkdir and the temp
	// write both succeed, and only the rename fails.
	target := filepath.Join(dir, "s.json")
	if err := os.MkdirAll(filepath.Join(target, "occupied"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := &Store{
		cfg:    Config{Path: target, Now: func() time.Time { return epoch }, Logf: func(string, ...any) {}},
		items:  map[string]*Item{},
		firing: map[*firing]struct{}{},
	}
	if err := s.saveLocked(); err == nil {
		t.Fatal("want rename error")
	}
}

// TestDueSkipsFutureItem pins the "not yet" branch of due().
//
// Every other test reaches it only incidentally — whichever item a map
// iteration happens to visit while another is already due — which made
// the branch's coverage depend on map order, i.e. on nothing. A 100%
// gate that fails once in ten runs teaches nobody anything, so the
// branch is driven directly here: two items, exactly one of them due.
func TestDueSkipsFutureItem(t *testing.T) {
	h := newHarness(t, nil)
	now, err := h.store.Add("conv-1", "now", epoch, 0, Budget{})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := h.store.Add("conv-1", "later", epoch.Add(time.Hour), 0, Budget{}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	claimed := h.store.due()
	if len(claimed) != 1 || claimed[0].id != now.ID {
		t.Fatalf("due() = %#v, want only %s", claimed, now.ID)
	}
	if got := len(h.store.List("conv-1")); got != 1 {
		t.Fatalf("after due(), %d items remain, want 1 (the future one)", got)
	}
}
