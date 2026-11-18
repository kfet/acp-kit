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
// — because that is what lets the store know which schedule a call to
// Add or Retime made during the turn belongs to.
//
// # Runaway control
//
// A scheduled prompt costs real money every time it fires, so every
// item carries a budget, and the budget — not a chain-depth rule — is
// what makes everything terminate:
//
//   - Until is a deadline (default 24h after arming, at most MaxUntil)
//     and MaxFires a fire count (default DefaultMaxFires). The item is
//     removed when either is spent.
//   - A scheduled turn cannot buy more budget. It may re-time its own
//     item (Retime) or add a one-shot (Add) that inherits the parent's
//     remaining Until and fires; it may not start a new repeat. Only a
//     turn that was NOT started by a schedule arms a fresh budget.
//   - Fire reports whether the turn was silent (nothing worth
//     posting). Each silent fire doubles the next interval, up to
//     MaxBackoff; a non-silent fire resets it to Every. After
//     SilentLimit silent fires in a row the item is removed and the
//     relay is told through Stopped, so it can say so once.
//   - MaxPerConv and MaxTotal cap how many items can be armed at once,
//     and MinInterval floors a repeat.
//
// Attribution starts when Fire is CALLED, not when the turn it starts
// begins, so a relay that makes Fire queue behind a human turn will
// treat that human turn as scheduled. That over-restricts rather than
// under-restricts, which is the direction a safety bound should err in.
//
// None of that removes the need for a human to be able to LOOK: List
// is what the relay's `!schedules` command renders, and Remove is what
// kills one.
package schedule

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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
	DefaultMaxPerConv  = 10
	DefaultMaxTotal    = 100
	DefaultMinInterval = time.Minute
	DefaultTick        = time.Second
	// DefaultUntil is how long a budget lasts when the caller names no
	// deadline.
	DefaultUntil = 24 * time.Hour
	// MaxUntil is the furthest deadline any budget may have.
	MaxUntil = 7 * 24 * time.Hour
	// DefaultMaxFires is how many fires a budget allows when the caller
	// names no count.
	DefaultMaxFires = 20
	// MaxBackoff caps the interval a silent streak stretches a repeat
	// to (a repeat slower than this to begin with is left alone).
	MaxBackoff = 6 * time.Hour
	// SilentLimit is how many silent fires in a row remove an item.
	SilentLimit = 3
	// oneShotGrace is the budget a migrated one-shot gets past its At.
	oneShotGrace = time.Hour
	// MaxTextLen bounds one scheduled prompt. A schedule is stored
	// and replayed unattended; an unbounded blob is a way to make every
	// future turn expensive by accident.
	MaxTextLen = 4000
)

// SilentNotice is the one line a relay posts when an item is removed
// for StopSilent.
const SilentNotice = "stopped: no news after 3 checks — reply here to resume"

// Stop says why the store removed an item on its own.
type Stop string

// Reasons passed to Config.Stopped.
const (
	// StopSilent: SilentLimit silent fires in a row.
	StopSilent Stop = "silent"
	// StopFires: MaxFires spent.
	StopFires Stop = "fires"
	// StopUntil: the deadline passed.
	StopUntil Stop = "until"
)

// Result is what a Fire callback reports about the turn it ran.
type Result struct {
	// Silent is true when the turn had nothing worth posting (a
	// heartbeat, an explicit silent marker).
	Silent bool
}

// Budget bounds one item. Zero fields mean the store's defaults.
type Budget struct {
	Until    time.Time
	MaxFires int
}

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
	// Every, when non-zero, is the base repeat interval.
	Every time.Duration `json:"every,omitempty"`
	// Fires counts claimed fires, so a human can see a repeat working.
	Fires int `json:"fires,omitempty"`
	// Until is the budget's deadline (UTC): no fire happens after it.
	Until time.Time `json:"until"`
	// MaxFires is the budget's fire count: the item is removed once
	// Fires reaches it.
	MaxFires int `json:"max_fires"`
	// Silent is the current run of silent fires.
	Silent int `json:"silent,omitempty"`
	// Created is when the item was armed (UTC).
	Created time.Time `json:"created"`
}

// FiresLeft is how many fires the budget still allows.
func (i Item) FiresLeft() int { return max(i.MaxFires-i.Fires, 0) }

// Interval is the gap before the next repeat: Every doubled once per
// silent fire in the current streak, capped at MaxBackoff (or Every,
// when that is larger). Zero for a one-shot.
func (i Item) Interval() time.Duration {
	if i.Every <= 0 {
		return 0
	}
	ceiling := max(MaxBackoff, i.Every)
	d := i.Every
	for n := 0; n < i.Silent && d < ceiling; n++ {
		d *= 2
	}
	return min(d, ceiling)
}

// itemJSON is the on-disk shape. Every is rendered as a Go duration
// string so the file stays readable and hand-editable by an operator,
// which an int64 of nanoseconds would not be. Version-1 files also
// carry a depth field; it is ignored.
type itemJSON struct {
	ID       string    `json:"id"`
	Conv     string    `json:"conv"`
	Text     string    `json:"text"`
	At       time.Time `json:"at"`
	Every    string    `json:"every,omitempty"`
	Fires    int       `json:"fires,omitempty"`
	Until    time.Time `json:"until"`
	MaxFires int       `json:"max_fires"`
	Silent   int       `json:"silent,omitempty"`
	Created  time.Time `json:"created"`
}

// MarshalJSON implements json.Marshaler.
func (i Item) MarshalJSON() ([]byte, error) {
	j := itemJSON{ID: i.ID, Conv: i.Conv, Text: i.Text, At: i.At, Fires: i.Fires,
		Until: i.Until, MaxFires: i.MaxFires, Silent: i.Silent, Created: i.Created}
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
	*i = Item{ID: j.ID, Conv: j.Conv, Text: j.Text, At: j.At, Fires: j.Fires,
		Until: j.Until, MaxFires: j.MaxFires, Silent: j.Silent, Created: j.Created}
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

// currentVersion 2 added budgets (until, max_fires, silent) and dropped
// depth. Open migrates version 1 in place.
const currentVersion = 2

// Config configures a Store.
type Config struct {
	// Path is the JSON file the store persists to. Required.
	Path string
	// Fire delivers one due item into its conversation. It MUST block
	// for the whole turn the prompt starts; see the package doc.
	// Returning ErrGone removes the item. Required.
	Fire func(ctx context.Context, it Item) (Result, error)
	// Stopped, when set, is told about every item the store removes on
	// its own (budget spent, silent streak). It is called without the
	// store's lock held.
	Stopped func(it Item, why Stop)

	// MaxPerConv, MaxTotal and MinInterval bound runaway scheduling;
	// zero means the corresponding Default above.
	MaxPerConv  int
	MaxTotal    int
	MinInterval time.Duration
	// DefaultUntil and DefaultMaxFires are a fresh budget's defaults;
	// zero means the package defaults. DefaultUntil is capped at
	// MaxUntil.
	DefaultUntil    time.Duration
	DefaultMaxFires int

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

	mu    sync.Mutex
	items map[string]*Item
	// firing holds one entry per fire running right now. An entry is
	// a pointer, not a value in a map keyed by conv, so Rekey can move
	// a running fire to its conversation's new token and the fire
	// still finds its own entry when it ends.
	firing map[*firing]struct{}

	wg sync.WaitGroup
}

// Open loads the store at cfg.Path, creating an empty one when the file
// does not exist. A corrupt file is an error, not a silent reset: an
// operator must see that armed work was lost. An older file is migrated
// and rewritten in the current format.
func Open(cfg Config) (*Store, error) {
	if cfg.Path == "" {
		return nil, errors.New("schedule: Path is required")
	}
	if cfg.Fire == nil {
		return nil, errors.New("schedule: Fire is required")
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
	if cfg.DefaultUntil <= 0 {
		cfg.DefaultUntil = DefaultUntil
	}
	cfg.DefaultUntil = min(cfg.DefaultUntil, MaxUntil)
	if cfg.DefaultMaxFires <= 0 {
		cfg.DefaultMaxFires = DefaultMaxFires
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
	if cfg.Stopped == nil {
		cfg.Stopped = func(Item, Stop) {}
	}
	s := &Store{cfg: cfg, items: map[string]*Item{}, firing: map[*firing]struct{}{}}
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
	if f.Version < currentVersion {
		s.migrateLocked()
		if err := s.saveLocked(); err != nil {
			return nil, err
		}
		cfg.Logf("schedule: migrated %d items in %s from version %d to %d", len(f.Items), cfg.Path, f.Version, currentVersion)
	}
	return s, nil
}

// migrateLocked gives every item that predates budgets one: a repeat
// gets a fresh default budget from now (or from its next fire, if
// later); a one-shot gets until an hour past its fire time (or now). Fires
// already done do not count against the new budget. Caller holds mu
// (or owns the store exclusively, as Open does).
func (s *Store) migrateLocked() {
	now := s.cfg.Now()
	for _, it := range s.items {
		if it.Until.IsZero() {
			base := it.At
			if now.After(base) {
				base = now
			}
			grace := oneShotGrace
			if it.Every > 0 {
				grace = s.cfg.DefaultUntil
			}
			it.Until = base.Add(grace).UTC()
		}
		if it.MaxFires <= 0 {
			it.MaxFires = it.Fires + s.cfg.DefaultMaxFires
		}
	}
}

// Add arms a new schedule for conv.
//
// Called from a turn a schedule started (in the same conversation),
// Add only arms a one-shot, and that one-shot inherits the parent's
// remaining budget; b can only tighten it. Called from any other turn
// it arms a fresh budget from b.
func (s *Store) Add(conv, text string, at time.Time, every time.Duration, b Budget) (Item, error) {
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
	if err := s.checkEvery(every); err != nil {
		return Item{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.items) >= s.cfg.MaxTotal {
		return Item{}, fmt.Errorf("schedule: the relay already has %d schedules armed, which is the limit", s.cfg.MaxTotal)
	}
	if s.countLocked(conv) >= s.cfg.MaxPerConv {
		return Item{}, fmt.Errorf("schedule: this conversation already has %d schedules armed, which is the limit", s.cfg.MaxPerConv)
	}
	parent := s.parentLocked(conv)
	var until time.Time
	var fires int
	if parent != nil {
		if every != 0 {
			return Item{}, fmt.Errorf("schedule: a scheduled turn cannot start a new repeat — leave schedule %s running, or re-time it", parent.id)
		}
		if parent.left <= 0 {
			return Item{}, fmt.Errorf("schedule: schedule %s has spent its budget; a scheduled turn cannot extend it", parent.id)
		}
		until, fires = parent.until, parent.left
		if !b.Until.IsZero() && b.Until.Before(until) {
			until = b.Until
		}
		if b.MaxFires > 0 && b.MaxFires < fires {
			fires = b.MaxFires
		}
	} else {
		var err error
		if until, fires, err = s.freshBudget(now, b); err != nil {
			return Item{}, err
		}
	}
	if at.After(until) {
		return Item{}, fmt.Errorf("schedule: %s is after the budget's deadline %s", at.UTC().Format(time.RFC3339), until.UTC().Format(time.RFC3339))
	}
	it := &Item{
		ID:       s.newIDLocked(),
		Conv:     conv,
		Text:     text,
		At:       at.UTC(),
		Every:    every,
		Until:    until.UTC(),
		MaxFires: fires,
		Created:  now.UTC(),
	}
	s.items[it.ID] = it
	// A child spends one of the parent's fires, so one turn cannot fan
	// out into many full-budget children.
	var p *Item
	if parent != nil {
		p = s.items[parent.id]
		if p != nil {
			p.MaxFires--
		}
	}
	if err := s.commitLocked(func() {
		delete(s.items, it.ID)
		if p != nil {
			p.MaxFires++
		}
	}); err != nil {
		return Item{}, err
	}
	if parent != nil {
		parent.children = append(parent.children, it.ID)
		parent.left--
	}
	return *it, nil
}

// Retime moves an armed item to at and sets its repeat to every.
//
// Called from a turn a schedule started, it may only touch that
// schedule (or a one-shot the same turn armed), keeps the budget, and
// b must be zero. Called from any other turn it arms a fresh budget
// from b and clears the silent streak.
func (s *Store) Retime(conv, id string, at time.Time, every time.Duration, b Budget) (Item, error) {
	now := s.cfg.Now()
	if at.Before(now) {
		return Item{}, errors.New("schedule: that time is in the past")
	}
	if err := s.checkEvery(every); err != nil {
		return Item{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	it, ok := s.items[id]
	if !ok || (conv != "" && it.Conv != conv) {
		return Item{}, fmt.Errorf("schedule: no schedule %q here", id)
	}
	old := *it
	parent := s.parentLocked(it.Conv)
	if parent != nil {
		if !parent.owns(id) {
			return Item{}, fmt.Errorf("schedule: a scheduled turn can only re-time its own schedule (%s)", parent.id)
		}
		if b != (Budget{}) {
			return Item{}, errors.New("schedule: a scheduled turn cannot change a budget")
		}
	} else {
		until, fires, err := s.freshBudget(now, b)
		if err != nil {
			return Item{}, err
		}
		it.Until, it.MaxFires, it.Silent = until.UTC(), it.Fires+fires, 0
	}
	if at.After(it.Until) {
		*it = old
		return Item{}, fmt.Errorf("schedule: %s is after the budget's deadline %s", at.UTC().Format(time.RFC3339), it.Until.Format(time.RFC3339))
	}
	it.At, it.Every = at.UTC(), every
	if err := s.commitLocked(func() { *it = old }); err != nil {
		return Item{}, err
	}
	if parent != nil && id == parent.id {
		parent.retimed = true
	}
	return *it, nil
}

// checkEvery enforces MinInterval. A negative interval lands here too,
// which is the point: one rule covers every value that is not zero or
// sane.
func (s *Store) checkEvery(every time.Duration) error {
	if every != 0 && every < s.cfg.MinInterval {
		return fmt.Errorf("schedule: repeat interval %s is below the %s minimum", every, s.cfg.MinInterval)
	}
	return nil
}

// freshBudget resolves b against the defaults for a human-armed item.
func (s *Store) freshBudget(now time.Time, b Budget) (time.Time, int, error) {
	until, fires := b.Until, b.MaxFires
	if until.IsZero() {
		until = now.Add(s.cfg.DefaultUntil)
	}
	if until.After(now.Add(MaxUntil)) {
		return time.Time{}, 0, fmt.Errorf("schedule: a budget may last at most %s", MaxUntil)
	}
	if fires < 0 {
		return time.Time{}, 0, errors.New("schedule: max fires must be positive")
	}
	if fires == 0 {
		fires = s.cfg.DefaultMaxFires
	}
	return until, fires, nil
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

// Rekey moves every item armed for conversation from to conversation
// to, and returns how many it moved.
//
// A relay whose conversation token can change — zulip-acp keys a
// conversation by its topic, and a topic can be renamed — calls it when
// the token changes. Without it, every item armed for the old token
// fires into a conversation that no longer exists and is dropped as
// gone.
//
// The fires that run in from right now move too, so a schedule that a
// running scheduled turn arms after the move is still attributed to its
// parent. A move is not an arm: MaxPerConv is not checked, so nothing
// armed is lost when two conversations become one.
//
// from == to, or an empty token, is a no-op.
func (s *Store) Rekey(from, to string) (int, error) {
	if from == to || from == "" || to == "" {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var fires []*firing
	for f := range s.firing {
		if f.conv == from {
			f.conv = to
			fires = append(fires, f)
		}
	}
	var moved []*Item
	for _, it := range s.items {
		if it.Conv == from {
			it.Conv = to
			moved = append(moved, it)
		}
	}
	if len(moved) == 0 {
		return 0, nil
	}
	err := s.commitLocked(func() {
		for _, f := range fires {
			f.conv = from
		}
		for _, it := range moved {
			it.Conv = from
		}
	})
	if err != nil {
		return 0, err
	}
	return len(moved), nil
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
			for _, f := range s.due() {
				s.start(ctx, f)
			}
		}
	}
}

// stopped is one removal to report once the lock is released.
type stopped struct {
	it  Item
	why Stop
}

// due claims every item whose time has come, advancing or removing it
// in the SAME critical section as the claim, so two ticks can never
// fire one item twice. An item past its deadline is removed unfired.
func (s *Store) due() []*firing {
	now := s.cfg.Now()
	s.mu.Lock()
	var out []*firing
	var stops []stopped
	for id, it := range s.items {
		if it.At.After(now) {
			continue
		}
		if now.After(it.Until) {
			delete(s.items, id)
			stops = append(stops, stopped{*it, StopUntil})
			continue
		}
		f := &firing{conv: it.Conv, id: it.ID, until: it.Until, item: *it, at: now}
		it.Fires++
		f.left = it.FiresLeft()
		switch {
		case it.Every == 0:
			delete(s.items, id)
		case f.left == 0:
			delete(s.items, id)
			stops = append(stops, stopped{*it, StopFires})
		default:
			it.At = nextAfter(it.At, it.Interval(), now)
		}
		s.firing[f] = struct{}{}
		out = append(out, f)
	}
	if len(out)+len(stops) > 0 {
		if err := s.commitLocked(func() {}); err != nil {
			// The claim already happened in memory; the items fire
			// either way. Undoing it would double-fire on the next
			// tick, which is strictly worse than a stale file.
			s.cfg.Logf("schedule: persisting fired items: %v", err)
		}
	}
	s.mu.Unlock()
	s.report(stops)
	sort.Slice(out, func(a, b int) bool { return itemLess(out[a].item, out[b].item) })
	return out
}

// report hands removals to Stopped. Caller does NOT hold mu.
func (s *Store) report(stops []stopped) {
	for _, st := range stops {
		s.cfg.Logf("schedule: removed %s (%s)", st.it.ID, st.why)
		s.cfg.Stopped(st.it, st.why)
	}
}

// nextAfter advances at by every until it is in the future, so a relay
// that was down for a day does not fire a minutely schedule 1440 times
// on startup. Catching up on missed work is not what a schedule means.
func nextAfter(at time.Time, every time.Duration, now time.Time) time.Time {
	next := at.Add(every)
	if !next.After(now) {
		// Jump straight to the first slot after now, in one step.
		missed := now.Sub(at) / every
		next = at.Add((missed + 1) * every)
	}
	return next.UTC()
}

// firing is one running fire. Caller holds mu to read or write any
// field after start.
type firing struct {
	conv string
	// id is the item being fired; item is its state as claimed.
	id   string
	item Item
	// at is when the fire was claimed.
	at time.Time
	// until and left are the budget a one-shot armed by this turn
	// inherits.
	until time.Time
	left  int
	// children are one-shots this turn armed; they inherit its
	// silent streak when it ends.
	children []string
	// retimed is set when the turn re-timed its own item, so the
	// end-of-fire reschedule does not overwrite it.
	retimed bool
}

// owns says whether this fire may re-time item id.
func (f *firing) owns(id string) bool {
	return id == f.id || slices.Contains(f.children, id)
}

// start runs one fire. Its firing entry was registered by due, so any
// schedule the turn creates is attributed to it.
func (s *Store) start(ctx context.Context, f *firing) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		res, err := s.cfg.Fire(ctx, f.item)
		s.endFire(f, res, err)
	}()
}

// endFire forgets a finished fire and applies its outcome: ErrGone
// removes the item; any other error is logged and changes nothing; a
// result updates the silent streak of the item and of every one-shot
// the turn armed.
func (s *Store) endFire(f *firing, res Result, err error) {
	s.mu.Lock()
	delete(s.firing, f)
	var stops []stopped
	switch {
	case errors.Is(err, ErrGone):
		if it, ok := s.items[f.id]; ok {
			delete(s.items, f.id)
			s.persistLocked(func() { s.items[f.id] = it })
		}
		s.cfg.Logf("schedule: dropped %s — its conversation is gone", f.id)
	case err != nil:
		s.cfg.Logf("schedule: firing %s: %v", f.id, err)
	default:
		streak := 0
		if res.Silent {
			streak = f.item.Silent + 1
		}
		changed, noticed := false, false
		for _, id := range append([]string{f.id}, f.children...) {
			it, ok := s.items[id]
			if !ok {
				continue
			}
			changed = true
			it.Silent = streak
			if streak >= SilentLimit {
				delete(s.items, id)
				// One notice per fire, however many items it ends.
				if !noticed {
					noticed = true
					stops = append(stops, stopped{*it, StopSilent})
				}
				continue
			}
			if id == f.id && !f.retimed {
				it.At = nextAfter(f.item.At, it.Interval(), s.cfg.Now())
				if it.At.After(it.Until) {
					delete(s.items, id)
					stops = append(stops, stopped{*it, StopUntil})
				}
			}
		}
		if changed {
			s.persistLocked(func() {})
		}
	}
	s.mu.Unlock()
	s.report(stops)
}

// persistLocked commits and logs a failure: the fire already happened,
// so there is no caller to tell. Caller holds mu.
func (s *Store) persistLocked(undo func()) {
	if err := s.commitLocked(undo); err != nil {
		s.cfg.Logf("schedule: persisting after fire: %v", err)
	}
}

// parentLocked is the scheduled turn firing in conv right now — the one
// with the least budget left, when several are — or nil. Caller holds
// mu.
func (s *Store) parentLocked(conv string) *firing {
	var best *firing
	for f := range s.firing {
		if f.conv == conv && (best == nil || f.left < best.left || (f.left == best.left && f.id < best.id)) {
			best = f
		}
	}
	return best
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
		_, _ = rand.Read(b[:]) // never fails since Go 1.24
		id := "s" + hex.EncodeToString(b[:])
		if _, taken := s.items[id]; !taken {
			return id
		}
	}
}

// itemLess orders soonest first, then by id so the order is total.
func itemLess(a, b Item) bool {
	if !a.At.Equal(b.At) {
		return a.At.Before(b.At)
	}
	return a.ID < b.ID
}

// sortItems orders items soonest first, then by id so the order is
// total and a listing never shuffles between calls.
func sortItems(out []Item) {
	sort.Slice(out, func(a, b int) bool { return itemLess(out[a], out[b]) })
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
	b, err := marshalIndent(file{Version: currentVersion, Items: items}, "", "  ")
	if err != nil {
		return fmt.Errorf("schedule: marshal: %w", err)
	}
	b = append(b, '\n')
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

// marshalIndent is a seam so tests can make the marshal fail.
var marshalIndent = json.MarshalIndent
