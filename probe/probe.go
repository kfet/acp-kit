// Package probe makes an agent's startup model probe survive a slow
// agent.
//
// The probe opens a fresh ACP session purely to learn the agent's model
// list, and it runs immediately after the agent process starts. That is
// a race: an agent command like
//
//	fir --mode acp --mcp-config <big mcp.json> --wait-mcp
//
// blocks until every MCP server is up, which can take well past any
// single fixed deadline. Losing the race produced three faces of the
// same failure in production —
//
//	probe: new session: context deadline exceeded
//	probe: new session: peer disconnected before response
//	probe: new session: context canceled
//
// — on roughly three startups in seven. The one-shot probe treated a
// slow-but-healthy agent as a permanently broken one.
//
// Models therefore retries with exponential backoff inside a larger
// overall budget, so readiness is waited for rather than sampled once.
// It stays best-effort: the model list drives cosmetic surfaces — a
// status-line segment, a model menu — so exhausting the budget must be
// logged and tolerated, never fatal. A relay that refuses to serve
// messages because a menu is not ready has a worse bug than the one
// this package fixes.
//
// Every relay in this family needs this, and each needs it at the same
// moment: immediately after client.Start, off the critical path, in a
// goroutine. It lives here rather than in a relay so the retry policy,
// its tuning and the "did we even ask yet?" bookkeeping (see Tracker)
// have exactly one implementation.
//
// Retrying is safe because ProbeModels is idempotent — it returns early
// once the model list is cached, so a retry that races a
// concurrently-created real session costs nothing.
//
// One cost is accepted and must not be "optimised" away: ProbeModels
// creates a throwaway session in a temp dir and drops its sink, and ACP
// has no session/delete, so that session stays idle inside the agent
// for the agent's lifetime. It buys a model list that is correct from
// the first interaction instead of from the first conversation.
package probe

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"
)

// Default tuning. The budget is generous because the failure it covers
// is "MCP servers are still starting", which is measured in minutes on
// a cold machine; the cost of waiting is only a late emoji.
const (
	DefaultBudget  = 5 * time.Minute
	DefaultAttempt = 30 * time.Second
	DefaultInitial = time.Second
	DefaultMax     = 15 * time.Second
)

// Status says how far the model probe has got. It exists because
// "Models() is empty" answers two different questions with one value:
// nobody has asked the agent yet, or the agent was asked and genuinely
// has no models (no provider authenticated). Only the second is the
// user's problem to fix, so a relay that renders the empty list must be
// able to tell them apart.
type Status int32

// Probe states. The zero value is StatusPending, so a Tracker that has
// never run reports the truth.
const (
	// StatusPending: no attempt has finished. Either the probe has not
	// been started or it is still retrying.
	StatusPending Status = iota

	// StatusProbed: ProbeModels returned nil. The agent has been asked,
	// so an empty model list is the agent's real answer.
	StatusProbed

	// StatusFailed: the budget was exhausted, or the context was
	// cancelled, without a successful probe.
	StatusFailed
)

// String implements fmt.Stringer.
func (s Status) String() string {
	switch s {
	case StatusPending:
		return "pending"
	case StatusProbed:
		return "probed"
	case StatusFailed:
		return "failed"
	}
	return fmt.Sprintf("Status(%d)", int32(s))
}

// Tracker records the outcome of a probe so another goroutine — a chat
// handler rendering a model menu, typically — can ask what happened.
// Its zero value is a usable Tracker in StatusPending, and all methods
// are safe for concurrent use.
type Tracker struct {
	s atomic.Int32
}

// Status returns the last recorded probe state.
func (t *Tracker) Status() Status { return Status(t.s.Load()) }

// set records a terminal state. Unexported: the only thing allowed to
// move a Tracker is a probe run.
func (t *Tracker) set(s Status) { t.s.Store(int32(s)) }

// Prober is the slice of acp-kit's *client.AgentProc that this package
// needs. Narrow by design: it keeps the retry policy testable without a
// live agent subprocess.
type Prober interface {
	ProbeModels(ctx context.Context) error
}

// Config tunes Models. The zero value of every field is usable.
type Config struct {
	// Prober is the agent to probe.
	Prober Prober

	// Tracker, when non-nil, records this run's outcome so a concurrent
	// reader can distinguish "not asked yet" from "asked, no models".
	Tracker *Tracker

	// Budget bounds the total time spent across all attempts.
	// Default DefaultBudget.
	Budget time.Duration

	// Attempt bounds a single probe RPC. Default DefaultAttempt.
	Attempt time.Duration

	// Initial and Max bound the exponential backoff between attempts.
	// Defaults DefaultInitial / DefaultMax.
	Initial, Max time.Duration

	// Logf receives one line per failed attempt. Optional.
	Logf func(format string, args ...any)

	// Now and After are injection points for tests, defaulting to
	// time.Now and time.After. They exist so the retry loop can be
	// exercised on a fake clock with no sleeping and no wall-clock
	// polling.
	Now   func() time.Time
	After func(d time.Duration) <-chan time.Time
}

func (c *Config) applyDefaults() {
	if c.Budget <= 0 {
		c.Budget = DefaultBudget
	}
	if c.Attempt <= 0 {
		c.Attempt = DefaultAttempt
	}
	if c.Initial <= 0 {
		c.Initial = DefaultInitial
	}
	if c.Max <= 0 {
		c.Max = DefaultMax
	}
	if c.Logf == nil {
		c.Logf = func(string, ...any) {}
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.After == nil {
		c.After = time.After
	}
}

// Models probes the agent for its model list, retrying a failing probe
// with exponential backoff until it succeeds, the budget runs out, or
// ctx is cancelled.
//
// It returns nil once the agent has been probed. The error from the
// final attempt is returned when the budget is exhausted, and ctx's
// error when cancelled; callers are expected to log either and carry on
// with an empty model list.
//
// Config.Tracker, if set, is updated on every exit so a concurrent
// reader can tell "not asked yet" from "asked, and the answer was no
// models".
func Models(ctx context.Context, cfg Config) error {
	cfg.applyDefaults()

	err := cfg.run(ctx)
	if cfg.Tracker != nil {
		// Recorded on EVERY exit, so a Tracker is never left reporting
		// "still starting" for a probe that has finished. A failed
		// probe is a different sentence to the user than a successful
		// one that found nothing.
		if err == nil {
			cfg.Tracker.set(StatusProbed)
		} else {
			cfg.Tracker.set(StatusFailed)
		}
	}
	return err
}

// run is the retry loop proper. Defaults are already applied.
func (cfg Config) run(ctx context.Context) error {
	deadline := cfg.Now().Add(cfg.Budget)
	backoff := cfg.Initial

	for attempt := 1; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}

		attemptCtx, cancel := context.WithTimeout(ctx, cfg.Attempt)
		err := cfg.Prober.ProbeModels(attemptCtx)
		cancel()
		if err == nil {
			return nil
		}

		// Stop if another attempt plus its backoff would run past the
		// budget; retrying beyond it only delays the inevitable log.
		if !cfg.Now().Add(backoff).Before(deadline) {
			return fmt.Errorf("probe models: giving up after %d attempt(s) within %s budget: %w", attempt, cfg.Budget, err)
		}

		cfg.Logf("probe models attempt %d failed, retrying in %s (agent may still be starting): %v", attempt, backoff, err)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-cfg.After(backoff):
		}

		if backoff *= 2; backoff > cfg.Max {
			backoff = cfg.Max
		}
	}
}
