// Package schedule stores conversation-scoped scheduled prompts and
// fires them back into the conversation they were created in.
//
// # Why a relay needs this at all
//
// Host-level cron (and its friendlier cousins) can already run a
// command at a time. What it cannot do is deliver into a CONVERSATION:
// a scheduled prompt here re-enters the same conv the schedule was
// created from, so the agent answering it has the topic's whole
// history, and its answer streams into that topic through the relay's
// ordinary path. That context is the entire reason this exists, and it
// is why "just shell out to cron" is the wrong layer. Host chores stay
// in host cron; only conversation-scoped work belongs here.
//
// # Shape
//
// A Store owns a JSON file, a tick loop, and a Fire callback supplied
// by the relay. Fire is called SYNCHRONOUSLY for the whole turn the
// scheduled prompt starts — it must not return until that turn is over
// — because that is what makes the recursion depth of a schedule
// created *by* a scheduled turn exactly knowable (see Add).
//
// # Runaway control
//
// A scheduled prompt whose turn schedules another is unbounded
// recursion that costs real money. Three independent bounds apply, and
// all three are on by default:
//
//   - MaxDepth caps the length of a schedule→turn→schedule chain. An
//     item created outside any firing turn has depth 1; one created
//     inside a firing turn has its parent's depth plus one, and Add
//     refuses anything above MaxDepth. Every chain therefore
//     terminates.
//   - MaxPerConv and MaxTotal cap how many items can be armed at once,
//     so breadth is bounded even where depth is not reached.
//   - MinInterval floors a repeat, so `every` cannot become a busy
//     loop.
//
// None of that removes the need for a human to be able to LOOK: List
// is what the relay's `!schedules` command renders, and Remove is what
// kills one.
package schedule

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// ErrGone is returned by a Fire callback whose conversation no longer
// exists — retired with `!new`, or in a channel the relay has stopped
// serving. The item is removed rather than retried: a schedule for a
// conversation nobody can reach is pure cost.
var ErrGone = errors.New("schedule: conversation is gone")

// Defaults for Config. Every one of these is a safety bound, not a
// tuning knob; raise them deliberately.
const (
	DefaultMaxDepth    = 3
	DefaultMaxPerConv  = 10
	DefaultMaxTotal    = 100
	DefaultMinInterval = time.Minute
	DefaultTick        = time.Second
	// MaxTextLen bounds one scheduled prompt. A schedule is stored
	// forever and replayed unattended; an unbounded blob is a way to
	// make every future turn expensive by accident.
	MaxTextLen = 4000
)

// Item is one armed schedule. It is the type both front ends speak —
// the relay's `!schedules` listing and the agent's list_schedules tool
// — so there is exactly one shape and nothing to drift.
type Item struct {
	// ID is short, opaque and unique across the store. It is what a
	// human types into `!unschedule`.
	ID string `json:"id"`
	// Conv is the relay's opaque conversation token, exactly as the
	// command broker uses it. The store never parses it.
	Conv string `json:"conv"`
	// Text is the prompt injected into the conversation on fire.
	Text string `json:"text"`
	// At is when it next fires (UTC).
	At time.Time `json:"at"`
	// Every, when non-zero, re-arms the item this far after each fire.
	Every time.Duration `json:"every,omitempty"`
	// Depth is the recursion depth: 1 for an item armed by a human
	// turn, parent+1 for one armed inside a firing scheduled turn.
	Depth int `json:"depth"`
	// Fires counts completed fires, so a human can see a repeat working.
	Fires int `json:"fires,omitempty"`
	// Created is when the item was armed (UTC).
	Created time.Time `json:"created"`
}

// itemJSON is the on-disk shape. Every is rendered as a Go duration
// string so the file stays readable and hand-editable by an operator,
// which an int64 of nanoseconds would not be.
type itemJSON struct {
	ID      string    `json:"id"`
	Conv    string    `json:"conv"`
	Text    string    `json:"text"`
	At      time.Time `json:"at"`
	Every   string    `json:"every,omitempty"`
	Depth   int       `json:"depth"`
	Fires   int       `json:"fires,omitempty"`
	Created time.Time `json:"created"`
}

// MarshalJSON implements json.Marshaler.
func (i Item) MarshalJSON() ([]byte, error) {
	j := itemJSON{ID: i.ID, Conv: i.Conv, Text: i.Text, At: i.At, Depth: i.Depth, Fires: i.Fires, Created: i.Created}
	if i.Every > 0 {
		j.Every = i.Every.String()
	}
	return json.Marshal(j)
}

// UnmarshalJSON implements json.Unmarshaler.
func (i *Item) UnmarshalJSON(b []byte) error {
	var j itemJSON
	if err := json.Unmarshal(b, &j); err != nil {
		return err
	}
	*i = Item{ID: j.ID, Conv: j.Conv, Text: j.Text, At: j.At, Depth: j.Depth, Fires: j.Fires, Created: j.Created}
	if j.Every != "" {
		d, err := time.ParseDuration(j.Every)
		if err != nil {
			return fmt.Errorf("schedule: item %q: parse every %q: %w", j.ID, j.Every, err)
		}
		i.Every = d
	}
	return nil
}

// file is the on-disk document.
type file struct {
	Version int    `json:"version"`
	Items   []Item `json:"items"`
}

const currentVersion = 1

// Config configures a Store.
type Config struct {
	// Path is the JSON file the store persists to. Required.
	Path string
	// Fire delivers one due item into its conversation. It MUST block
	// for the whole turn the prompt starts; see the package doc.
	// Returning ErrGone removes the item. Required.
	Fire func(ctx context.Context, it Item) error

	// MaxDepth, MaxPerConv, MaxTotal and MinInterval bound runaway
	// scheduling; zero means the corresponding Default above.
	MaxDepth    int
	MaxPerConv  int
	MaxTotal    int
	MinInterval time.Duration

	// Tick is how often the loop looks for due items. Zero means
	// DefaultTick. Ignored when Ticks is set.
	Tick time.Duration
	// Ticks, when non-nil, replaces the internal ticker. Tests drive
	// it by hand so no test ever waits on a wall clock.
	Ticks <-chan time.Time

	// Now is the clock. Defaults to time.Now.
	Now func() time.Time
	// Logf receives operational messages.
	Logf func(format string, args ...any)
}

// Store is the persisted set of armed schedules. Safe for concurrent
// use.
type Store struct {
	cfg Config

	mu     sync.Mutex
	items  map[string]*Item
	depths map[string][]int // conv -> depths of the fires running right now

	wg sync.WaitGroup
}

// Open loads the store at cfg.Path, creating an empty one when the file
// does not exist. A corrupt file is an error, not a silent reset: an
// operator must see that armed work was lost.
func Open(cfg Config) (*Store, error) {
	if cfg.Path == "" {
		return nil, errors.New("schedule: Path is required")
	}
	if cfg.Fire == nil {
		return nil, errors.New("schedule: Fire is required")
	}
	if cfg.MaxDepth <= 0 {
		cfg.MaxDepth = DefaultMaxDepth
	}
	if cfg.MaxPerConv <= 0 {
		cfg.MaxPerConv = DefaultMaxPerConv
	}
	if cfg.MaxTotal <= 0 {
		cfg.MaxTotal = DefaultMaxTotal
	}
	if cfg.MinInterval <= 0 {
		cfg.MinInterval = DefaultMinInterval
	}
	if cfg.Tick <= 0 {
		cfg.Tick = DefaultTick
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	s := &Store{cfg: cfg, items: map[string]*Item{}, depths: map[string][]int{}}
	b, err := os.ReadFile(cfg.Path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("schedule: read %s: %w", cfg.Path, err)
	}
	var f file
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("schedule: parse %s: %w", cfg.Path, err)
	}
	for idx := range f.Items {
		it := f.Items[idx]
		s.items[it.ID] = &it
	}
	return s, nil
}

// Add arms a new schedule for conv.
//
// depth is derived, never supplied: it is one more than the depth of
// the scheduled turn currently firing in this conversation, or 1 when
// no scheduled turn is running. That is the whole recursion guard, and
// it works precisely because Fire blocks for the duration of the turn.
func (s *Store) Add(conv, text string, at time.Time, every time.Duration) (Item, error) {
	if conv == "" {
		return Item{}, errors.New("schedule: conversation is required")
	}
	if text == "" {
		return Item{}, errors.New("schedule: text is required")
	}
	if len(text) > MaxTextLen {
		return Item{}, fmt.Errorf("schedule: text is %d bytes, limit is %d", len(text), MaxTextLen)
	}
	now := s.cfg.Now()
	if at.Before(now) {
		return Item{}, errors.New("schedule: that time is in the past")
	}
	// A negative interval lands here too, which is the point: there is
	// one rule ("a repeat may not be faster than MinInterval") and it
	// already covers every value that is not zero or sane.
	if every != 0 && every < s.cfg.MinInterval {
		return Item{}, fmt.Errorf("schedule: repeat interval %s is below the %s minimum", every, s.cfg.MinInterval)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	depth := s.parentDepthLocked(conv) + 1
	if depth > s.cfg.MaxDepth {
		return Item{}, fmt.Errorf("schedule: refusing to nest schedules more than %d deep — this turn was itself scheduled", s.cfg.MaxDepth)
	}
	if len(s.items) >= s.cfg.MaxTotal {
		return Item{}, fmt.Errorf("schedule: the relay already has %d schedules armed, which is the limit", s.cfg.MaxTotal)
	}
	if s.countLocked(conv) >= s.cfg.MaxPerConv {
		return Item{}, fmt.Errorf("schedule: this conversation already has %d schedules armed, which is the limit", s.cfg.MaxPerConv)
	}
	it := &Item{
		ID:      s.newIDLocked(),
		Conv:    conv,
		Text:    text,
		At:      at.UTC(),
		Every:   every,
		Depth:   depth,
		Created: now.UTC(),
	}
	s.items[it.ID] = it
	if err := s.commitLocked(func() { delete(s.items, it.ID) }); err != nil {
		return Item{}, err
	}
	return *it, nil
}

// List returns the armed items for conv, soonest first. An empty conv
// lists every conversation's, which is what an operator wants.
func (s *Store) List(conv string) []Item {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Item, 0, len(s.items))
	for _, it := range s.items {
		if conv == "" || it.Conv == conv {
			out = append(out, *it)
		}
	}
	sortItems(out)
	return out
}

// Remove disarms an item. conv scopes the lookup: a conversation can
// only ever cancel its own schedules, which matters because the id is
// short and a caller could otherwise guess into another conversation.
// An empty conv removes by id regardless of owner (operator scope).
func (s *Store) Remove(conv, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	it, ok := s.items[id]
	if !ok || (conv != "" && it.Conv != conv) {
		return fmt.Errorf("schedule: no schedule %q here", id)
	}
	delete(s.items, id)
	return s.commitLocked(func() { s.items[id] = it })
}

// Run drives the fire loop until ctx is done. It returns once every
// in-flight fire has finished, so a relay can shut down cleanly.
func (s *Store) Run(ctx context.Context) {
	ticks := s.cfg.Ticks
	if ticks == nil {
		t := time.NewTicker(s.cfg.Tick)
		defer t.Stop()
		ticks = t.C
	}
	s.loop(ctx, ticks)
	s.wg.Wait()
}

// loop is the testable core: it takes the tick channel, so a test drives
// every branch by hand instead of racing a real ticker.
func (s *Store) loop(ctx context.Context, ticks <-chan time.Time) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			for _, it := range s.due() {
				s.start(ctx, it)
			}
		}
	}
}

// due claims every item whose time has come, advancing or removing it
// in the SAME critical section as the claim, so two ticks can never
// fire one item twice.
func (s *Store) due() []Item {
	now := s.cfg.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Item
	for id, it := range s.items {
		if it.At.After(now) {
			continue
		}
		claimed := *it
		it.Fires++
		if it.Every > 0 {
			it.At = nextAfter(it.At, it.Every, now)
		} else {
			delete(s.items, id)
		}
		out = append(out, claimed)
	}
	if len(out) == 0 {
		return nil
	}
	sortItems(out)
	if err := s.commitLocked(func() {}); err != nil {
		// The claim already happened in memory; the items fire either
		// way. Undoing it would double-fire on the next tick, which is
		// strictly worse than a stale file.
		s.cfg.Logf("schedule: persisting fired items: %v", err)
	}
	return out
}

// nextAfter advances at by every until it is in the future, so a relay
// that was down for a day does not fire a minutely schedule 1440 times
// on startup. Catching up on missed work is not what a schedule means.
func nextAfter(at time.Time, every time.Duration, now time.Time) time.Time {
	next := at.Add(every)
	if next.After(now) {
		return next
	}
	// Jump straight to the first slot after now, in one step.
	missed := now.Sub(at) / every
	return at.Add((missed + 1) * every).UTC()
}

// start runs one fire, tracking its depth for the whole turn so any
// schedule the turn creates is attributed to it.
func (s *Store) start(ctx context.Context, it Item) {
	s.mu.Lock()
	s.depths[it.Conv] = append(s.depths[it.Conv], it.Depth)
	s.mu.Unlock()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer s.popDepth(it)
		err := s.cfg.Fire(ctx, it)
		if err == nil {
			return
		}
		if errors.Is(err, ErrGone) {
			// Removing by empty conv: the conversation is gone, so
			// scoping the removal to it would be circular.
			if rerr := s.Remove("", it.ID); rerr != nil {
				s.cfg.Logf("schedule: dropping %s for a gone conversation: %v", it.ID, rerr)
			}
			s.cfg.Logf("schedule: dropped %s — its conversation is gone", it.ID)
			return
		}
		s.cfg.Logf("schedule: firing %s: %v", it.ID, err)
	}()
}

// popDepth removes one recorded depth for the conversation.
func (s *Store) popDepth(it Item) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.depths[it.Conv]
	for i, v := range d {
		if v == it.Depth {
			s.depths[it.Conv] = append(d[:i], d[i+1:]...)
			break
		}
	}
	if len(s.depths[it.Conv]) == 0 {
		delete(s.depths, it.Conv)
	}
}

// parentDepthLocked is the deepest scheduled turn firing in conv right
// now, or 0 when none is. Caller holds mu.
func (s *Store) parentDepthLocked(conv string) int {
	max := 0
	for _, d := range s.depths[conv] {
		if d > max {
			max = d
		}
	}
	return max
}

// countLocked counts the items armed for conv. Caller holds mu.
func (s *Store) countLocked(conv string) int {
	n := 0
	for _, it := range s.items {
		if it.Conv == conv {
			n++
		}
	}
	return n
}

// newIDLocked mints an unused id. Caller holds mu.
func (s *Store) newIDLocked() string {
	for {
		var b [4]byte
		mustRandom(b[:])
		id := "s" + hex.EncodeToString(b[:])
		if _, taken := s.items[id]; !taken {
			return id
		}
	}
}

// sortItems orders items soonest first, then by id so the order is
// total and a listing never shuffles between calls.
func sortItems(out []Item) {
	sort.Slice(out, func(a, b int) bool {
		if !out[a].At.Equal(out[b].At) {
			return out[a].At.Before(out[b].At)
		}
		return out[a].ID < out[b].ID
	})
}

// commitLocked persists the store and, on failure, runs undo so the
// in-memory state cannot disagree with what the caller was told. Caller
// holds mu.
func (s *Store) commitLocked(undo func()) error {
	if err := s.saveLocked(); err != nil {
		undo()
		return err
	}
	return nil
}

// saveLocked writes the whole store atomically. Caller holds mu.
func (s *Store) saveLocked() error {
	items := make([]Item, 0, len(s.items))
	for _, it := range s.items {
		items = append(items, *it)
	}
	sortItems(items)
	b := append(mustMarshal(file{Version: currentVersion, Items: items}), '\n')
	if err := os.MkdirAll(filepath.Dir(s.cfg.Path), 0o755); err != nil {
		return fmt.Errorf("schedule: mkdir: %w", err)
	}
	tmp := s.cfg.Path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("schedule: write: %w", err)
	}
	if err := os.Rename(tmp, s.cfg.Path); err != nil {
		return fmt.Errorf("schedule: commit: %w", err)
	}
	return nil
}
