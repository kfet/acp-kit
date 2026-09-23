package convo

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"
)

// tokenSeq issues process-unique turn tokens. Zero is never issued, so a
// zero token always means "no turn".
var tokenSeq atomic.Uint64

// Turn is one registered in-flight turn.
//
// Its Token identifies it for race-safe cancellation: a canceller that
// captured the token of the turn it owns can never, by accident, cancel
// the NEXT turn of the same conversation (see Active.CancelToken).
type Turn struct {
	Conv  string
	Token uint64
	// Value is relay-owned per-turn state (poe-acp: the live sink for
	// its MCP attach tool; zulip-acp: the turn's pending topic rename).
	Value any

	cancel context.CancelFunc
	ended  chan struct{}
	once   sync.Once
}

// Ended is closed when the turn has fully unwound (Active.End).
func (t *Turn) Ended() <-chan struct{} { return t.ended }

// Active is the registry of in-flight turns, one per conversation.
//
// It is the race-safe half of cancellation. Stop cancels whatever is
// running (`!stop`, a superseding follow-up); CancelToken cancels only
// the turn a caller proves it owns, closing the window where turn N's
// owner cancels turn N+1 after N finished and N+1 started.
type Active struct {
	// OnCancel, when set, is called after a turn's context is
	// cancelled, to tell the agent (session/cancel). Called without the
	// registry lock held. Its error is returned by CancelToken.
	OnCancel func(ctx context.Context, conv string) error
	// OnWait, when set, is called (under the registry lock — it must not
	// call back into Active) each time Claim finds conv busy and parks.
	OnWait func(conv string)

	mu        sync.Mutex
	cond      *sync.Cond
	m         map[string]*Turn
	cancelled []*Turn
}

// NewActive returns an empty registry.
func NewActive() *Active {
	a := &Active{m: map[string]*Turn{}}
	a.cond = sync.NewCond(&a.mu)
	return a
}

// Begin registers a new turn for conv and returns it. cancel (may be
// nil) is what Stop and CancelToken call. A turn already registered for
// conv is displaced from the registry but NOT cancelled — see Supersede.
func (a *Active) Begin(conv string, cancel context.CancelFunc, value any) *Turn {
	t := newTurn(conv, cancel, value)
	a.mu.Lock()
	a.m[conv] = t
	a.cond.Broadcast()
	a.mu.Unlock()
	return t
}

func newTurn(conv string, cancel context.CancelFunc, value any) *Turn {
	return &Turn{Conv: conv, Token: tokenSeq.Add(1), Value: value, cancel: cancel, ended: make(chan struct{})}
}

// Supersede registers a new turn for conv and cancels the one it
// displaces, if any, in one step: two follow-ups racing each other can
// never both be left running, as a separate Stop then Begin could.
func (a *Active) Supersede(ctx context.Context, conv string, cancel context.CancelFunc, value any) *Turn {
	t := newTurn(conv, cancel, value)
	a.mu.Lock()
	old, ok := a.m[conv]
	a.m[conv] = t
	a.cond.Broadcast()
	a.mu.Unlock()
	if ok {
		_ = a.fire(ctx, old) // best effort: the turn is stopped either way
	}
	return t
}

// Claim waits until conv has no registered turn and then registers a new
// one, without releasing the lock in between — so a turn that must never
// supersede another (a scheduled prompt, a batched reaction) cannot have
// a message arriving in the gap displaced either. Returns ctx.Err() if it
// gives up first, in which case nothing is registered.
func (a *Active) Claim(ctx context.Context, conv string, cancel context.CancelFunc, value any) (*Turn, error) {
	stop := a.wakeOnDone(ctx)
	defer close(stop)
	a.mu.Lock()
	defer a.mu.Unlock()
	for ctx.Err() == nil {
		if _, busy := a.m[conv]; !busy {
			t := newTurn(conv, cancel, value)
			a.m[conv] = t
			return t, nil
		}
		if a.OnWait != nil {
			a.OnWait(conv)
		}
		a.cond.Wait()
	}
	return nil, ctx.Err()
}

// wakeOnDone broadcasts the condition once ctx is done, so a waiter
// re-checks it; close the returned channel to release the helper.
func (a *Active) wakeOnDone(ctx context.Context) chan struct{} {
	stop := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			a.mu.Lock()
			a.cond.Broadcast()
			a.mu.Unlock()
		case <-stop:
		}
	}()
	return stop
}

// End marks t unwound: closes Ended and, if t is still conv's registered
// turn, removes it. Idempotent.
func (a *Active) End(t *Turn) {
	t.once.Do(func() { close(t.ended) })
	a.mu.Lock()
	if cur, ok := a.m[t.Conv]; ok && cur == t {
		delete(a.m, t.Conv)
		a.cond.Broadcast()
	}
	a.mu.Unlock()
}

// Get returns conv's registered turn.
func (a *Active) Get(conv string) (*Turn, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	t, ok := a.m[conv]
	return t, ok
}

// Running reports whether conv has a registered turn.
func (a *Active) Running(conv string) bool {
	_, ok := a.Get(conv)
	return ok
}

// Stop cancels and deregisters conv's turn, reporting whether there was
// one. `!stop` uses the answer to tell interrupting from doing nothing.
func (a *Active) Stop(ctx context.Context, conv string) bool {
	a.mu.Lock()
	t, ok := a.m[conv]
	if ok {
		delete(a.m, conv)
		a.cond.Broadcast()
	}
	a.mu.Unlock()
	if ok {
		_ = a.fire(ctx, t) // best effort: the turn is stopped either way
	}
	return ok
}

// CancelToken cancels conv's turn only if it is the one identified by
// token, reporting whether it did and OnCancel's error. A stale or zero
// token is a no-op. The turn stays registered until its runner calls End.
//
// Residual window: the token is compared under the lock but the cancel
// is issued after releasing it (never hold a lock across a wire write),
// so a turn ending in those few instructions may still be followed by a
// cancel of its successor's session. The seconds-wide window is gone.
func (a *Active) CancelToken(ctx context.Context, conv string, token uint64) (bool, error) {
	if token == 0 {
		return false, nil
	}
	a.mu.Lock()
	t, ok := a.m[conv]
	a.mu.Unlock()
	if !ok || t.Token != token {
		return false, nil
	}
	return true, a.fire(ctx, t)
}

func (a *Active) fire(ctx context.Context, t *Turn) error {
	if t.cancel != nil {
		t.cancel()
	}
	if a.OnCancel != nil {
		return a.OnCancel(ctx, t.Conv)
	}
	return nil
}

// CancelAll stops every registered turn and returns their conversations,
// sorted. WaitCancelled then waits for them to unwind.
func (a *Active) CancelAll(ctx context.Context) []string {
	a.mu.Lock()
	var ts []*Turn
	for _, t := range a.m {
		ts = append(ts, t)
	}
	// Forget turns an earlier CancelAll stopped that have since unwound,
	// so repeated calls without WaitCancelled cannot grow the list.
	kept := a.cancelled[:0]
	for _, t := range a.cancelled {
		select {
		case <-t.ended:
		default:
			kept = append(kept, t)
		}
	}
	a.cancelled = append(kept, ts...)
	a.mu.Unlock()
	var out []string
	for _, t := range ts {
		// Only report what this call actually stopped: a turn that
		// ended between the snapshot and here is not ours to claim.
		a.mu.Lock()
		cur, ok := a.m[t.Conv]
		live := ok && cur == t
		if live {
			delete(a.m, t.Conv)
			a.cond.Broadcast()
		}
		a.mu.Unlock()
		if live {
			_ = a.fire(ctx, t)
			out = append(out, t.Conv)
		}
	}
	sort.Strings(out)
	return out
}

// WaitCancelled blocks until every turn CancelAll stopped has unwound,
// then until nothing is registered, or ctx is done.
func (a *Active) WaitCancelled(ctx context.Context) error {
	a.mu.Lock()
	ts := a.cancelled
	a.cancelled = nil
	a.mu.Unlock()
	for _, t := range ts {
		select {
		case <-t.ended:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return a.WaitIdle(ctx)
}

// Len returns the number of registered turns.
func (a *Active) Len() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.m)
}

// WaitIdle blocks until no turn is registered or ctx is done.
func (a *Active) WaitIdle(ctx context.Context) error {
	stop := a.wakeOnDone(ctx)
	defer close(stop)
	a.mu.Lock()
	defer a.mu.Unlock()
	for len(a.m) > 0 && ctx.Err() == nil {
		a.cond.Wait()
	}
	return ctx.Err()
}
