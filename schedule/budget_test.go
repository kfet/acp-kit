package schedule

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// fireOnce claims every due item and ends each fire with res, as the
// loop would, but synchronously.
func (h *harness) fireOnce(res Result, during func(f *firing)) []*firing {
	fs := h.store.due()
	for _, f := range fs {
		if during != nil {
			during(f)
		}
		h.store.endFire(f, res, nil)
	}
	return fs
}

func (h *harness) stopList() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.stops)
}

func TestIntervalAndFiresLeft(t *testing.T) {
	if (Item{}).Interval() != 0 {
		t.Fatal("one-shot has no interval")
	}
	it := Item{Every: time.Hour, Silent: 2}
	if it.Interval() != 4*time.Hour {
		t.Fatalf("got %s", it.Interval())
	}
	it.Silent = 10
	if it.Interval() != MaxBackoff {
		t.Fatalf("cap: got %s", it.Interval())
	}
	slow := Item{Every: 12 * time.Hour, Silent: 2}
	if slow.Interval() != 12*time.Hour {
		t.Fatalf("slow repeat must not stretch: %s", slow.Interval())
	}
	if (Item{MaxFires: 1, Fires: 3}).FiresLeft() != 0 {
		t.Fatal("FiresLeft must not go negative")
	}
}

func TestDefaultUntilIsCapped(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.DefaultUntil = 30 * 24 * time.Hour })
	if h.store.cfg.DefaultUntil != MaxUntil {
		t.Fatalf("DefaultUntil = %s", h.store.cfg.DefaultUntil)
	}
}

func TestAddBudgetValidation(t *testing.T) {
	h := newHarness(t, nil)
	if _, err := h.store.Add("c", "x", epoch.Add(time.Hour), 0, Budget{Until: epoch.Add(8 * 24 * time.Hour)}); err == nil {
		t.Fatal("want MaxUntil error")
	}
	if _, err := h.store.Add("c", "x", epoch.Add(time.Hour), 0, Budget{MaxFires: -1}); err == nil {
		t.Fatal("want negative max fires error")
	}
	if _, err := h.store.Add("c", "x", epoch.Add(3*time.Hour), 0, Budget{Until: epoch.Add(2 * time.Hour)}); err == nil {
		t.Fatal("want at-after-until error")
	}
	it, err := h.store.Add("c", "x", epoch.Add(time.Hour), 0, Budget{Until: epoch.Add(2 * time.Hour), MaxFires: 5})
	if err != nil || it.MaxFires != 5 || !it.Until.Equal(epoch.Add(2*time.Hour)) {
		t.Fatalf("explicit budget: %+v %v", it, err)
	}
}

func TestMaxFiresStopsRepeat(t *testing.T) {
	h := newHarness(t, nil)
	it, _ := h.store.Add("c", "x", epoch, time.Minute, Budget{MaxFires: 2})
	h.fireOnce(Result{}, nil)
	h.clock.advance(time.Minute)
	if n := len(h.fireOnce(Result{}, nil)); n != 1 {
		t.Fatalf("second fire: %d", n)
	}
	if len(h.store.List("")) != 0 {
		t.Fatal("spent budget must remove the item")
	}
	if got := h.stopList(); !slices.Equal(got, []string{"fires:" + it.ID}) {
		t.Fatalf("stops = %v", got)
	}
}

func TestUntilStopsUnfired(t *testing.T) {
	h := newHarness(t, nil)
	it, _ := h.store.Add("c", "x", epoch, time.Minute, Budget{Until: epoch.Add(time.Minute)})
	h.clock.advance(2 * time.Minute)
	if n := len(h.fireOnce(Result{}, nil)); n != 0 {
		t.Fatalf("expired item fired %d times", n)
	}
	if got := h.stopList(); !slices.Equal(got, []string{"until:" + it.ID}) {
		t.Fatalf("stops = %v", got)
	}
}

func TestUntilStopsAfterLastFire(t *testing.T) {
	h := newHarness(t, nil)
	it, _ := h.store.Add("c", "x", epoch, time.Hour, Budget{Until: epoch.Add(30 * time.Minute)})
	h.fireOnce(Result{}, nil)
	if len(h.store.List("")) != 0 {
		t.Fatal("a next slot past the deadline must remove the item")
	}
	if got := h.stopList(); !slices.Equal(got, []string{"until:" + it.ID}) {
		t.Fatalf("stops = %v", got)
	}
}

func TestSilentStreakBacksOffThenStops(t *testing.T) {
	h := newHarness(t, nil)
	it, _ := h.store.Add("c", "x", epoch, time.Hour, Budget{})
	h.fireOnce(Result{Silent: true}, nil)
	got := h.store.List("")[0]
	if got.Silent != 1 || !got.At.Equal(epoch.Add(2*time.Hour)) {
		t.Fatalf("after 1 silent: %+v", got)
	}
	// A non-silent fire resets the interval.
	h.clock.advance(2 * time.Hour)
	h.fireOnce(Result{}, nil)
	got = h.store.List("")[0]
	if got.Silent != 0 || !got.At.Equal(epoch.Add(3*time.Hour)) {
		t.Fatalf("after reset: %+v", got)
	}
	for range SilentLimit {
		h.clock.advance(MaxBackoff)
		h.fireOnce(Result{Silent: true}, nil)
	}
	if len(h.store.List("")) != 0 {
		t.Fatal("silent streak must remove the item")
	}
	if got := h.stopList(); !slices.Equal(got, []string{"silent:" + it.ID}) {
		t.Fatalf("stops = %v", got)
	}
}

func TestFireErrorKeepsStreak(t *testing.T) {
	h := newHarness(t, nil)
	h.store.Add("c", "x", epoch, time.Hour, Budget{})
	for _, f := range h.store.due() {
		h.store.endFire(f, Result{Silent: true}, os.ErrClosed)
	}
	if got := h.store.List("")[0]; got.Silent != 0 {
		t.Fatalf("an error must not count as silent: %+v", got)
	}
}

func TestScheduledTurnLimits(t *testing.T) {
	h := newHarness(t, nil)
	parent, _ := h.store.Add("c", "p", epoch, time.Hour, Budget{MaxFires: 3, Until: epoch.Add(10 * time.Hour)})
	other, _ := h.store.Add("c", "o", epoch.Add(5*time.Hour), time.Hour, Budget{})
	var child Item
	h.fireOnce(Result{Silent: true}, func(f *firing) {
		if f.id != parent.ID {
			t.Fatalf("unexpected fire %s", f.id)
		}
		if _, err := h.store.Add("c", "r", epoch.Add(time.Minute), time.Hour, Budget{}); err == nil || !strings.Contains(err.Error(), parent.ID) {
			t.Errorf("a new repeat from a scheduled turn must be refused: %v", err)
		}
		var err error
		child, err = h.store.Add("c", "k", epoch.Add(time.Minute), 0, Budget{Until: epoch.Add(5 * time.Hour), MaxFires: 1})
		if err != nil || child.MaxFires != 1 || !child.Until.Equal(epoch.Add(5*time.Hour)) {
			t.Errorf("child: %+v %v", child, err)
		}
		loose, err := h.store.Add("c", "k", epoch.Add(time.Minute), 0, Budget{Until: epoch.Add(7 * 24 * time.Hour), MaxFires: 99})
		if err != nil || loose.MaxFires != 1 || !loose.Until.Equal(epoch.Add(10*time.Hour)) {
			t.Errorf("a child cannot loosen the budget: %+v %v", loose, err)
		}
		if _, err := h.store.Add("c", "k", epoch.Add(11*time.Hour), 0, Budget{}); err == nil {
			t.Error("child past the parent's deadline must be refused")
		}
		if _, err := h.store.Retime("c", other.ID, epoch.Add(time.Hour), 0, Budget{}); err == nil {
			t.Error("retiming another schedule must be refused")
		}
		if _, err := h.store.Retime("c", parent.ID, epoch.Add(time.Hour), time.Hour, Budget{MaxFires: 9}); err == nil {
			t.Error("changing the budget must be refused")
		}
		if _, err := h.store.Retime("c", child.ID, epoch.Add(2*time.Minute), 0, Budget{}); err != nil {
			t.Errorf("retiming own child: %v", err)
		}
		if f.retimed {
			t.Error("retiming a child must not pin the parent's time")
		}
		if _, err := h.store.Retime("c", parent.ID, epoch.Add(11*time.Hour), time.Hour, Budget{}); err == nil || f.retimed {
			t.Errorf("a failed retime must not pin the parent's time: %v", err)
		}
		if _, err := h.store.Retime("c", parent.ID, epoch.Add(9*time.Hour), time.Hour, Budget{}); err != nil {
			t.Errorf("retiming own item: %v", err)
		}
	})
	got := map[string]Item{}
	for _, it := range h.store.List("") {
		got[it.ID] = it
	}
	if p := got[parent.ID]; !p.At.Equal(epoch.Add(9*time.Hour)) || p.Silent != 1 || p.MaxFires != 1 {
		t.Fatalf("own retime must stick; each child spends a parent fire: %+v", p)
	}
	if c := got[child.ID]; c.Silent != 1 {
		t.Fatalf("child must inherit the streak: %+v", c)
	}
}

func TestSpentParentCannotArmChild(t *testing.T) {
	h := newHarness(t, nil)
	h.store.Add("c", "p", epoch, 0, Budget{MaxFires: 1})
	h.fireOnce(Result{}, func(*firing) {
		if _, err := h.store.Add("c", "k", epoch.Add(time.Minute), 0, Budget{}); err == nil {
			t.Error("a spent budget must not arm a child")
		}
	})
}

func TestChildOfSilentChainStops(t *testing.T) {
	h := newHarness(t, nil)
	h.store.Add("c", "p", epoch, 0, Budget{})
	for i := range SilentLimit {
		h.fireOnce(Result{Silent: true}, func(*firing) {
			if _, err := h.store.Add("c", "again", h.clock.now().Add(time.Minute), 0, Budget{}); err != nil {
				t.Errorf("link %d: %v", i, err)
			}
		})
		h.clock.advance(time.Minute)
	}
	if n := len(h.store.List("")); n != 0 {
		t.Fatalf("a silent one-shot chain must stop, %d left", n)
	}
	if got := h.stopList(); len(got) != 1 || !strings.HasPrefix(got[0], "silent:") {
		t.Fatalf("stops = %v", got)
	}
}

func TestParentPicksTightestBudget(t *testing.T) {
	h := newHarness(t, nil)
	a, b := &firing{conv: "c", id: "a", left: 5}, &firing{conv: "c", id: "b", left: 2}
	c := &firing{conv: "c", id: "c", left: 2}
	h.store.firing[a], h.store.firing[b], h.store.firing[c] = struct{}{}, struct{}{}, struct{}{}
	if p := h.store.parentLocked("c"); p != b {
		t.Fatalf("parent = %+v", p)
	}
	if h.store.parentLocked("x") != nil {
		t.Fatal("no fire in x")
	}
}

func TestRetimeByHuman(t *testing.T) {
	h := newHarness(t, nil)
	it, _ := h.store.Add("c", "x", epoch.Add(time.Hour), time.Hour, Budget{MaxFires: 2})
	h.store.items[it.ID].Silent = 2
	h.store.items[it.ID].Fires = 2
	got, err := h.store.Retime("c", it.ID, epoch.Add(2*time.Hour), 2*time.Hour, Budget{MaxFires: 4})
	if err != nil || got.Silent != 0 || got.MaxFires != 6 || got.Every != 2*time.Hour || !got.At.Equal(epoch.Add(2*time.Hour)) {
		t.Fatalf("retime: %+v %v", got, err)
	}
	for name, call := range map[string]func() error{
		"missing":    func() error { _, err := h.store.Retime("c", "nope", epoch.Add(time.Hour), 0, Budget{}); return err },
		"wrong conv": func() error { _, err := h.store.Retime("d", it.ID, epoch.Add(time.Hour), 0, Budget{}); return err },
		"past":       func() error { _, err := h.store.Retime("c", it.ID, epoch.Add(-time.Hour), 0, Budget{}); return err },
		"every": func() error {
			_, err := h.store.Retime("c", it.ID, epoch.Add(time.Hour), time.Second, Budget{})
			return err
		},
		"budget": func() error {
			_, err := h.store.Retime("c", it.ID, epoch.Add(time.Hour), 0, Budget{MaxFires: -1})
			return err
		},
		"deadline": func() error {
			_, err := h.store.Retime("c", it.ID, epoch.Add(3*time.Hour), 0, Budget{Until: epoch.Add(2 * time.Hour)})
			return err
		},
	} {
		if call() == nil {
			t.Errorf("%s: want error", name)
		}
	}
	if cur := h.store.List("")[0]; cur.MaxFires != 6 {
		t.Fatalf("a failed retime must not change the item: %+v", cur)
	}
	breakDir(t, h.path)
	if _, err := h.store.Retime("", it.ID, epoch.Add(5*time.Hour), 0, Budget{}); err == nil {
		t.Fatal("want save error")
	}
	if cur := h.store.List("")[0]; !cur.At.Equal(epoch.Add(2 * time.Hour)) {
		t.Fatalf("a failed save must be undone: %+v", cur)
	}
}

// TestMigrateRealV1File opens a copy of a live version-1 file and
// checks nothing is lost and the file is rewritten as version 2.
func TestMigrateRealV1File(t *testing.T) {
	src, err := os.ReadFile("testdata/schedules-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var old struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(src, &old); err != nil {
		t.Fatal(err)
	}
	// Add a repeat whose next fire is in the past, and one in the
	// future, so both migration bases are exercised.
	now := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	var doc map[string]any
	_ = json.Unmarshal(src, &doc)
	doc["items"] = append(doc["items"].([]any),
		map[string]any{"id": "rpast", "conv": "c", "text": "t", "at": now.Add(-time.Hour), "every": "1h", "depth": 2, "fires": 7, "created": now},
		map[string]any{"id": "rfut", "conv": "c", "text": "t", "at": now.Add(48 * time.Hour), "every": "1h", "depth": 1, "created": now})
	src, _ = json.Marshal(doc)
	path := filepath.Join(t.TempDir(), "schedules.json")
	if err := os.WriteFile(path, src, 0o600); err != nil {
		t.Fatal(err)
	}
	var logs []string
	s, err := Open(Config{Path: path, Fire: noFire, Now: func() time.Time { return now },
		Logf: func(f string, _ ...any) { logs = append(logs, f) }})
	if err != nil {
		t.Fatal(err)
	}
	items := s.List("")
	if len(items) != len(old.Items)+2 {
		t.Fatalf("lost items: %d of %d", len(items), len(old.Items)+2)
	}
	for _, it := range items {
		switch it.ID {
		case "rpast":
			if !it.Until.Equal(now.Add(DefaultUntil)) || it.MaxFires != 7+DefaultMaxFires {
				t.Errorf("rpast: %+v", it)
			}
		case "rfut":
			if !it.Until.Equal(now.Add(72*time.Hour)) || it.MaxFires != DefaultMaxFires {
				t.Errorf("rfut: %+v", it)
			}
		default:
			if !it.Until.Equal(it.At.Add(time.Hour)) || it.MaxFires != DefaultMaxFires || it.Text == "" || it.Conv == "" {
				t.Errorf("one-shot %s: %+v", it.ID, it)
			}
		}
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), `"version": 2`) || strings.Contains(string(b), `"depth"`) || !strings.Contains(string(b), `"max_fires"`) {
		t.Fatalf("not rewritten in the new format:\n%s", b)
	}
	if len(logs) != 1 {
		t.Fatalf("want one migration log, got %v", logs)
	}
	// Reopening a current file does not migrate again.
	logs = nil
	if _, err := Open(Config{Path: path, Fire: noFire, Now: func() time.Time { return now },
		Logf: func(f string, _ ...any) { logs = append(logs, f) }}); err != nil || len(logs) != 0 {
		t.Fatalf("reopen: %v %v", err, logs)
	}
}

func TestMigrateSaveFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "schedules.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"items":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path+".tmp", 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(Config{Path: path, Fire: noFire}); err == nil {
		t.Fatal("want the migration save error")
	}
}

func noFire(_ context.Context, _ Item) (Result, error) { return Result{}, nil }

func TestDueOrdersClaims(t *testing.T) {
	h := newHarness(t, nil)
	b, _ := h.store.Add("c", "b", epoch.Add(time.Minute), 0, Budget{})
	a, _ := h.store.Add("c", "a", epoch, 0, Budget{})
	h.clock.advance(time.Minute)
	fs := h.store.due()
	if len(fs) != 2 || fs[0].id != a.ID || fs[1].id != b.ID {
		t.Fatalf("order: %+v", fs)
	}
}

func TestChildCommitFailureRestoresParent(t *testing.T) {
	h := newHarness(t, nil)
	p, _ := h.store.Add("c", "p", epoch, time.Hour, Budget{MaxFires: 5})
	h.fireOnce(Result{}, func(f *firing) {
		breakDir(t, h.path)
		if _, err := h.store.Add("c", "k", epoch.Add(time.Minute), 0, Budget{}); err == nil {
			t.Error("want save error")
		}
		if f.left != 4 || h.store.items[p.ID].MaxFires != 5 {
			t.Errorf("failed child must not spend: left %d, %+v", f.left, h.store.items[p.ID])
		}
	})
}

func TestOneSilentNoticePerFire(t *testing.T) {
	h := newHarness(t, nil)
	p, _ := h.store.Add("c", "p", epoch, time.Hour, Budget{})
	h.store.items[p.ID].Silent = SilentLimit - 1
	h.fireOnce(Result{Silent: true}, func(*firing) {
		for range 2 {
			if _, err := h.store.Add("c", "k", epoch.Add(time.Minute), 0, Budget{}); err != nil {
				t.Error(err)
			}
		}
	})
	if n := len(h.store.List("")); n != 0 {
		t.Fatalf("%d items left", n)
	}
	if got := h.stopList(); len(got) != 1 {
		t.Fatalf("want one notice, got %v", got)
	}
}

func TestMigrateOverdueOneShotSurvives(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")
	now := epoch
	doc := `{"version":1,"items":[{"id":"old","conv":"c","text":"t","at":"2026-09-01T00:00:00Z","depth":1,"created":"2026-08-01T00:00:00Z"}]}`
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(Config{Path: path, Fire: noFire, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if it := s.List("")[0]; !it.Until.Equal(now.Add(time.Hour)) {
		t.Fatalf("overdue one-shot must still fire: %+v", it)
	}
}
