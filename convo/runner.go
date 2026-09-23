package convo

import (
	"context"
	"sync"
)

// Job is one turn submitted to the Manager's turn runner.
type Job struct {
	Conv  string
	Value any // becomes Turn.Value
	Run   func(ctx context.Context, t *Turn) error
	// After runs once the turn has fully unwound and is no longer
	// registered — so a deferred action the turn asked for (a reset,
	// say) cannot cancel the very turn that requested it.
	After func(t *Turn)
}

// Start submits job under the configured Mode and returns at once.
//
// The turn's context is detached from ctx's cancellation (an inbound
// request ending must not kill the turn it started) and is cancelled
// only by Active.Stop / CancelToken — `!stop`, a superseding follow-up,
// or a caller that owns the turn's token. In Serial mode `!stop` stops
// the running turn only; turns already queued behind it still run.
func (m *Manager) Start(ctx context.Context, job Job) {
	base := context.WithoutCancel(ctx)
	if m.cfg.Mode == Supersede {
		// Registered on the caller's goroutine, atomically with
		// cancelling the displaced turn, so a burst of follow-ups can
		// never leave two turns running.
		tctx, cancel := context.WithCancel(base)
		t := m.active.Supersede(base, job.Conv, cancel, job.Value)
		go m.exec(t, tctx, cancel, job)
		return
	}
	m.lanes.push(base, job)
}

// run executes one job start to finish on the calling goroutine.
func (m *Manager) run(base context.Context, job Job) {
	tctx, cancel := context.WithCancel(base)
	m.exec(m.active.Begin(job.Conv, cancel, job.Value), tctx, cancel, job)
}

func (m *Manager) exec(t *Turn, tctx context.Context, cancel context.CancelFunc, job Job) {
	func() {
		defer m.active.End(t)
		defer cancel()
		if err := job.Run(tctx, t); err != nil {
			if m.cfg.OnError != nil {
				m.cfg.OnError(job.Conv, err)
			} else {
				m.cfg.Logf("convo: turn for %s failed: %v", job.Conv, err)
			}
		}
	}()
	if job.After != nil {
		job.After(t)
	}
}

// WaitIdle blocks until no turn is running or queued, or ctx is done.
func (m *Manager) WaitIdle(ctx context.Context) error {
	if err := m.lanes.waitIdle(ctx); err != nil {
		return err
	}
	return m.active.WaitIdle(ctx)
}

type queued struct {
	base context.Context
	job  Job
}

// lanes is the Serial-mode runner: one FIFO per conversation, drained
// by a goroutine that exists only while the lane has work.
type lanes struct {
	m    *Manager
	mu   sync.Mutex
	cond *sync.Cond
	byID map[string][]queued
}

func newLanes(m *Manager) *lanes {
	l := &lanes{m: m, byID: map[string][]queued{}}
	l.cond = sync.NewCond(&l.mu)
	return l
}

func (l *lanes) push(base context.Context, job Job) {
	l.mu.Lock()
	q, busy := l.byID[job.Conv]
	l.byID[job.Conv] = append(q, queued{base, job})
	l.mu.Unlock()
	if !busy {
		go l.drain(job.Conv)
	}
}

// drain runs conv's jobs in order. The lane stays in byID (possibly
// empty) while a job runs, which is what tells push not to start a
// second drainer.
func (l *lanes) drain(conv string) {
	for {
		l.mu.Lock()
		q := l.byID[conv]
		if len(q) == 0 {
			delete(l.byID, conv)
			l.cond.Broadcast()
			l.mu.Unlock()
			return
		}
		next := q[0]
		l.byID[conv] = q[1:]
		l.mu.Unlock()
		l.m.run(next.base, next.job)
	}
}

// Pending returns how many turns are queued (not yet running) for conv.
func (m *Manager) Pending(conv string) int {
	m.lanes.mu.Lock()
	defer m.lanes.mu.Unlock()
	return len(m.lanes.byID[conv])
}

func (l *lanes) waitIdle(ctx context.Context) error {
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			l.mu.Lock()
			l.cond.Broadcast()
			l.mu.Unlock()
		case <-stop:
		}
	}()
	l.mu.Lock()
	defer l.mu.Unlock()
	for len(l.byID) > 0 && ctx.Err() == nil {
		l.cond.Wait()
	}
	return ctx.Err()
}
