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

	fire func(context.Context, Item) error
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
		Fire: func(ctx context.Context, it Item) error {
			h.mu.Lock()
			h.fired = append(h.fired, it)
			f := h.fire
			h.mu.Unlock()
			if f != nil {
				return f(ctx, it)
			}
			return nil
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
	s, err := Open(Config{Path: filepath.Join(t.TempDir(), "s.json"), Fire: func(context.Context, Item) error { return nil }})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if s.cfg.MaxDepth != DefaultMaxDepth || s.cfg.MaxPerConv != DefaultMaxPerConv ||
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
	_, err := Open(Config{Path: filepath.Join(dir, "s.json"), Fire: func(context.Context, Item) error { return nil }})
	if err == nil {
		t.Fatal("want read error")
	}
}

func TestOpenCorruptFileIsAnError(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.json")
	if err := os.WriteFile(p, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(Config{Path: p, Fire: func(context.Context, Item) error { return nil }}); err == nil {
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
			if _, err := h.store.Add(tc.conv, tc.text, tc.at, tc.every); err == nil {
				t.Fatal("want error")
			}
		})
	}
}

func TestAddPersistsAndLists(t *testing.T) {
	h := newHarness(t, nil)
	a, err := h.store.Add("conv-1", "later", epoch.Add(2*time.Hour), 0)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	b, err := h.store.Add("conv-1", "sooner", epoch.Add(time.Hour), time.Hour)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := h.store.Add("conv-2", "elsewhere", epoch.Add(time.Hour), 0); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if a.Depth != 1 || b.Depth != 1 {
		t.Fatalf("want depth 1, got %d/%d", a.Depth, b.Depth)
	}

	got := h.store.List("conv-1")
	if len(got) != 2 || got[0].ID != b.ID || got[1].ID != a.ID {
		t.Fatalf("List(conv-1) = %+v, want sooner first", got)
	}
	if len(h.store.List("")) != 3 {
		t.Fatalf("List(all) = %d, want 3", len(h.store.List("")))
	}

	// Reopen: the file round-trips, repeat interval included.
	re, err := Open(Config{Path: h.path, Fire: func(context.Context, Item) error { return nil }, Now: h.clock.now})
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
			if _, err := h.store.Add("c", "x", epoch.Add(time.Hour), 0); err != nil {
				t.Fatalf("Add %d: %v", i, err)
			}
		}
		if _, err := h.store.Add("c", "x", epoch.Add(time.Hour), 0); err == nil {
			t.Fatal("want per-conv cap error")
		}
		// A different conversation is unaffected.
		if _, err := h.store.Add("other", "x", epoch.Add(time.Hour), 0); err != nil {
			t.Fatalf("other conv: %v", err)
		}
	})
	t.Run("total", func(t *testing.T) {
		h := newHarness(t, func(c *Config) { c.MaxTotal = 1 })
		if _, err := h.store.Add("c", "x", epoch.Add(time.Hour), 0); err != nil {
			t.Fatalf("Add: %v", err)
		}
		if _, err := h.store.Add("d", "x", epoch.Add(time.Hour), 0); err == nil {
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
	if _, err := h.store.Add("c", "x", epoch.Add(time.Hour), 0); err == nil {
		t.Fatal("want save error")
	}
	if got := h.store.List(""); len(got) != 0 {
		t.Fatalf("failed Add left state behind: %+v", got)
	}
}

func TestRemove(t *testing.T) {
	h := newHarness(t, nil)
	it, err := h.store.Add("c", "x", epoch.Add(time.Hour), 0)
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
	it, err := h.store.Add("c", "x", epoch.Add(time.Hour), 0)
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
	it, err := h.store.Add("c", "ping", epoch.Add(time.Minute), 0)
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
	if _, err := h.store.Add("c", "ping", epoch.Add(time.Minute), time.Hour); err != nil {
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
	if _, err := h.store.Add("c", "ping", epoch, 0); err != nil {
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
	if _, err := h.store.Add("c", "ping", epoch, time.Hour); err != nil {
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

func TestFireGoneRemoveFailureIsLogged(t *testing.T) {
	h := newHarness(t, nil)
	done := make(chan struct{})
	h.fire = func(context.Context, Item) error {
		// Remove the item first, so the store's own removal fails.
		_ = h.store.Remove("", h.store.List("")[0].ID)
		close(done)
		return ErrGone
	}
	if _, err := h.store.Add("c", "ping", epoch, time.Hour); err != nil {
		t.Fatalf("Add: %v", err)
	}
	h.run()
	h.tick()
	<-done
	h.cancel()
	<-h.done
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.logs) < 2 {
		t.Fatalf("want both the failure and the drop logged, got %v", h.logs)
	}
}

func TestDuePersistFailureStillFires(t *testing.T) {
	h := newHarness(t, nil)
	done := make(chan struct{})
	h.fire = func(context.Context, Item) error { close(done); return nil }
	if _, err := h.store.Add("c", "ping", epoch, 0); err != nil {
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

// TestDepthCapStopsRunaway is the money test: a scheduled turn that
// schedules another must terminate.
func TestDepthCapStopsRunaway(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.MaxDepth = 2 })
	var (
		mu     sync.Mutex
		depths []int
		errs   []error
	)
	fired := make(chan struct{}, 8)
	h.fire = func(_ context.Context, it Item) error {
		mu.Lock()
		depths = append(depths, it.Depth)
		mu.Unlock()
		// Every scheduled turn tries to schedule another one.
		_, err := h.store.Add(it.Conv, "again", h.clock.now().Add(time.Minute), 0)
		mu.Lock()
		errs = append(errs, err)
		mu.Unlock()
		fired <- struct{}{}
		return nil
	}
	if _, err := h.store.Add("c", "start", epoch, 0); err != nil {
		t.Fatalf("Add: %v", err)
	}
	h.run()

	// Fire depth 1: it arms a depth-2 child.
	h.tick()
	<-fired
	// Fire depth 2: its attempt to arm a depth-3 child must be refused.
	h.clock.advance(2 * time.Minute)
	h.tick()
	<-fired
	h.clock.advance(2 * time.Minute)
	h.tick()

	h.cancel()
	<-h.done

	mu.Lock()
	defer mu.Unlock()
	if len(depths) != 2 || depths[0] != 1 || depths[1] != 2 {
		t.Fatalf("depths = %v, want [1 2]", depths)
	}
	if errs[0] != nil {
		t.Fatalf("depth-2 child refused: %v", errs[0])
	}
	if errs[1] == nil {
		t.Fatal("depth-3 child was allowed — recursion is unbounded")
	}
	if got := h.store.List(""); len(got) != 0 {
		t.Fatalf("chain did not terminate: %+v", got)
	}
}

func TestPopDepthTracksConcurrentFires(t *testing.T) {
	h := newHarness(t, nil)
	// Two items, same conversation, different depths, both firing.
	s := h.store
	s.depths["c"] = []int{1, 2}
	if got := s.parentDepthLocked("c"); got != 2 {
		t.Fatalf("parentDepth = %d, want 2", got)
	}
	s.popDepth(Item{Conv: "c", Depth: 2})
	if got := s.parentDepthLocked("c"); got != 1 {
		t.Fatalf("parentDepth after pop = %d, want 1", got)
	}
	s.popDepth(Item{Conv: "c", Depth: 1})
	if _, still := s.depths["c"]; still {
		t.Fatal("empty depth list was not cleaned up")
	}
	// Popping something that is not there is a no-op, not a panic.
	s.popDepth(Item{Conv: "c", Depth: 9})
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
		Fire: func(context.Context, Item) error { return nil },
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
	s, err := Open(Config{Path: filepath.Join(sub, "s.json"), Fire: func(context.Context, Item) error { return nil }, Now: func() time.Time { return epoch }})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// The directory exists, so MkdirAll succeeds and only the write can
	// fail — the branch a missing parent never reaches.
	if err := os.Chmod(sub, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sub, 0o755) })
	if _, err := s.Add("c", "x", epoch.Add(time.Hour), 0); err == nil {
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
		depths: map[string][]int{},
	}
	if err := s.saveLocked(); err == nil {
		t.Fatal("want rename error")
	}
}
