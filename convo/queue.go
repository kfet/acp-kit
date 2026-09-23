package convo

import (
	"sync"
	"time"
)

// DefaultQueueCap is the soft cap NewQueue uses when given cap <= 0.
const DefaultQueueCap = 32

// Queue is a per-conversation FIFO of pending turns, consumed by exactly
// one runner goroutine that owns every prompt for the conversation — ACP
// allows one outstanding prompt per session, so turns are serialised.
//
// Items may be SHEDDABLE (out-of-band work such as a reaction event).
// Past the soft cap the oldest sheddable item is dropped to make room;
// an incoming sheddable item with nothing older to shed is refused.
// Non-sheddable items (real user turns) are never dropped: the queue
// grows past the cap rather than lose one.
type Queue[T any] struct {
	cap       int
	sheddable func(T) bool
	onShed    func(T)

	mu       sync.Mutex
	q        []T
	inFlight bool
	stopped  bool
	notify   chan struct{} // cap 1: "non-empty or stopped"
	idleCh   chan struct{} // cap 1: poked by Finish for WaitIdle
}

// NewQueue returns an empty queue. sheddable (nil = nothing sheds)
// classifies items; onShed (may be nil) is told about an item dropped
// on overflow — push never finalises it itself.
func NewQueue[T any](cap int, sheddable func(T) bool, onShed func(T)) *Queue[T] {
	if cap <= 0 {
		cap = DefaultQueueCap
	}
	if sheddable == nil {
		sheddable = func(T) bool { return false }
	}
	if onShed == nil {
		onShed = func(T) {}
	}
	return &Queue[T]{cap: cap, sheddable: sheddable, onShed: onShed,
		notify: make(chan struct{}, 1), idleCh: make(chan struct{}, 1)}
}

// Push appends item, reporting whether it was accepted. It is refused
// when the queue is stopped, or when it is sheddable, the queue is full
// and no older sheddable item exists.
func (q *Queue[T]) Push(item T) bool {
	q.mu.Lock()
	if q.stopped {
		q.mu.Unlock()
		return false
	}
	var shed []T
	if len(q.q) >= q.cap {
		i := -1
		for j, r := range q.q {
			if q.sheddable(r) {
				i = j
				break
			}
		}
		switch {
		case i >= 0:
			shed = append(shed, q.q[i])
			q.q = append(q.q[:i], q.q[i+1:]...)
		case q.sheddable(item):
			q.mu.Unlock()
			return false
		}
	}
	q.q = append(q.q, item)
	select {
	case q.notify <- struct{}{}:
	default:
	}
	q.mu.Unlock()
	for _, s := range shed {
		q.onShed(s)
	}
	return true
}

// PopOrWait blocks until an item is available (marking it in flight) or
// stop fires (ok=false).
func (q *Queue[T]) PopOrWait(stop <-chan struct{}) (item T, ok bool) {
	for {
		q.mu.Lock()
		if len(q.q) > 0 {
			item = q.q[0]
			var zero T
			q.q[0] = zero
			q.q = q.q[1:]
			q.inFlight = true
			q.mu.Unlock()
			return item, true
		}
		q.mu.Unlock()
		select {
		case <-stop:
			return item, false
		case <-q.notify:
		}
	}
}

// Finish marks the in-flight item done. Call after every popped item,
// before releasing whoever waits on it.
func (q *Queue[T]) Finish() {
	q.mu.Lock()
	q.inFlight = false
	q.mu.Unlock()
	select {
	case q.idleCh <- struct{}{}:
	default:
	}
}

// Idle reports whether nothing is queued or in flight.
func (q *Queue[T]) Idle() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return !q.inFlight && len(q.q) == 0
}

// Len returns the number of queued (not in-flight) items.
func (q *Queue[T]) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.q)
}

// Items returns a copy of the queued (not in-flight) items, oldest first.
func (q *Queue[T]) Items() []T {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]T(nil), q.q...)
}

// WaitIdle blocks until Idle or d elapses, reporting Idle.
func (q *Queue[T]) WaitIdle(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	for {
		if q.Idle() {
			return true
		}
		select {
		case <-q.idleCh:
		case <-t.C:
			return q.Idle()
		}
	}
}

// StopIfIdle atomically checks idleness and, only if idle, closes the
// queue. Checking and closing in separate steps would leave a gap where
// a pushed item is detached with nobody left to run it.
func (q *Queue[T]) StopIfIdle() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.inFlight || len(q.q) > 0 {
		return false
	}
	q.stopped = true
	return true
}

// Stop closes the queue and detaches pending items, returning them and
// whether one is still in flight. The caller finalises the returned
// items — deliberately not done here, so Stop is safe under a caller's
// lock that must never be held across a sink write.
func (q *Queue[T]) Stop() (pending []T, inFlight bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.stopped = true
	pending, q.q = q.q, nil
	return pending, q.inFlight
}
