package client

import (
	"context"
	"errors"
	"sync"
	"time"

	acp "github.com/coder/acp-go-sdk"
)

// Sentinel causes reported through context.Cause on a context returned by
// StartTurnLiveness. They exist so a relay can tell WHY a turn ended and say
// something useful to the user: a plain context.Canceled means a human (or a
// superseding message) stopped the turn, ErrNoProgress means the agent went
// quiet, ErrTurnCeiling means an operator-configured hard cap fired.
var (
	// ErrNoProgress is the cause when a turn produced no evidence of agent
	// progress within TurnLivenessConfig.NoProgressTimeout.
	ErrNoProgress = errors.New("acp: agent made no progress within the liveness window")
	// ErrTurnCeiling is the cause when a turn ran past the opt-in absolute
	// ceiling TurnLivenessConfig.MaxTurnDuration, regardless of progress.
	ErrTurnCeiling = errors.New("acp: turn exceeded its absolute ceiling")
)

// defaultNoProgressTimeout bounds a wedged turn when
// TurnLivenessConfig.NoProgressTimeout is unset. Two minutes of total silence
// from an agent — not one token, not one tool call — is already far outside
// normal behaviour, while being generous enough never to cut a turn that is
// merely slow.
const defaultNoProgressTimeout = 2 * time.Minute

// TurnLivenessConfig configures StartTurnLiveness.
type TurnLivenessConfig struct {
	// NoProgressTimeout is how long a turn may go with NO evidence of agent
	// progress before it is cancelled with cause ErrNoProgress. The clock is
	// reset by every progress update (see IsProgress) and by nothing else,
	// so a legitimately long-running tool is never cut while a genuinely
	// wedged agent is still detected — even one whose harness keeps emitting
	// cosmetic frames (plans, spinners, mode changes).
	//
	// This is deliberately NOT called an "idle timeout": acp-kit already has
	// state.Config.IdleTimeout, which reaps whole IDLE SESSIONS long after
	// their turns finished. This bound is about one in-flight turn and about
	// the absence of PROGRESS, not the absence of a user. Naming it after
	// the condition it detects keeps the two from ever being confused.
	//
	// <=0 falls back to defaultNoProgressTimeout (2m).
	NoProgressTimeout time.Duration

	// MaxTurnDuration is an OPTIONAL absolute wall-clock ceiling on the turn,
	// enforced regardless of progress, with cause ErrTurnCeiling.
	//
	// <=0 (the default) means NO ceiling: while the agent keeps making
	// progress the turn runs for as long as it needs. A wall-clock cap
	// punishes exactly the turns that are working hardest, and gets worse as
	// tool use deepens, so it is opt-in for operators who deliberately want a
	// hard upper bound. It is NOT the wedge guard — that is
	// NoProgressTimeout's job.
	MaxTurnDuration time.Duration

	// NoProgressCause optionally replaces ErrNoProgress as the cause
	// reported when the no-progress window expires, and TurnCeilingCause
	// replaces ErrTurnCeiling for the ceiling. They exist because the
	// layer that owns the timeouts is the only one that knows the
	// NUMBERS, and a relay that wants to tell its user "no output for
	// 2m0s" must be able to put that sentence on the cause rather than
	// re-deriving it wherever the cause is rendered.
	//
	// A replacement is honoured only if it still satisfies
	// errors.Is(cause, <the sentinel it replaces>) — otherwise it is
	// IGNORED and the sentinel is used. The cross-relay promise is that
	// `errors.Is(context.Cause(ctx), client.ErrNoProgress)` identifies a
	// wedge cut everywhere; a custom sentence may ride along on top of
	// that promise but may never break it. Wrap, do not replace:
	//
	//	func (c myCause) Unwrap() error { return client.ErrNoProgress }
	//
	// nil (the default) means the bare sentinel.
	NoProgressCause  error
	TurnCeilingCause error
}

// causeOr returns want if it wraps sentinel, else sentinel. See
// TurnLivenessConfig.NoProgressCause for why a mis-wrapped cause is
// dropped rather than honoured.
func causeOr(want, sentinel error) error {
	if want != nil && errors.Is(want, sentinel) {
		return want
	}
	return sentinel
}

// IsProgress reports whether a session/update is evidence that the agent is
// doing work, as opposed to merely being alive.
//
// Progress is agent output — message and thought chunks — plus tool_call and
// tool_call_update: a running tool is the whole reason a turn may be long, so
// it must count. Everything else is explicitly not progress. Plans, available
// commands, mode and config changes, session info and echoed user message
// chunks are bookkeeping an agent (or its harness) can emit while wedged; if
// they reset the clock, a hung turn whose spinner keeps ticking is never
// detected, which is the exact failure this construct exists to catch.
//
// The rule is exported so consumers can classify updates the same way — and
// so it is testable in one place rather than reimplemented per relay.
func IsProgress(u acp.SessionUpdate) bool {
	return u.AgentMessageChunk != nil ||
		u.AgentThoughtChunk != nil ||
		u.ToolCall != nil ||
		u.ToolCallUpdate != nil
}

// TurnLiveness bounds one prompt turn by PROGRESS rather than wall-clock.
//
// Create one per turn with StartTurnLiveness, run the turn on the context it
// returns, and install Wrap(sink) as the session's SessionUpdateSink. Updates
// pass through untouched; the ones that count as progress (IsProgress) reset
// the no-progress clock.
//
// It is a watcher, not just a decorator — it owns timers and cancels a
// context — which is why it is not named like the package's other sink
// decorator, ValidatingSink: a sink that silently cancels contexts would be a
// surprise, so the cancelling half is the named type and the sink is
// something you ask it for.
type TurnLiveness struct {
	window time.Duration

	mu           sync.Mutex
	timer        *time.Timer // no-progress timer
	lastProgress time.Time
	cancel       context.CancelCauseFunc
	done         bool

	// stopCeiling releases the ceiling context. Never nil — it is
	// context.CancelFunc(func(){}) when no ceiling is configured.
	stopCeiling context.CancelFunc
}

// StartTurnLiveness begins watching a turn. It returns the watcher, a context
// derived from parent that is cancelled with cause ErrNoProgress or
// ErrTurnCeiling when the turn stops making progress, and a stop function.
//
// The stop function releases the timers and cancels the returned context with
// a plain context.Canceled cause — so it doubles as the caller's own cancel
// handle for a user-initiated stop, and context.Cause tells the two apart.
// Call it exactly as you would a context.CancelFunc: always, on every path.
//
// Construction is split from Wrap because relays create the turn context (and
// stash its cancel func in an in-flight map) before they build the sink.
func StartTurnLiveness(parent context.Context, cfg TurnLivenessConfig) (*TurnLiveness, context.Context, context.CancelFunc) {
	window := cfg.NoProgressTimeout
	if window <= 0 {
		window = defaultNoProgressTimeout
	}
	// The ceiling is a plain deadline on an intermediate context rather
	// than a second timer: it needs no reset, its cause propagates to
	// the child, and — unlike a bare cancel — it makes the cap visible
	// to anything downstream that asks ctx.Deadline().
	base, stopCeiling := parent, context.CancelFunc(func() {})
	if cfg.MaxTurnDuration > 0 {
		base, stopCeiling = context.WithDeadlineCause(parent,
			time.Now().Add(cfg.MaxTurnDuration), causeOr(cfg.TurnCeilingCause, ErrTurnCeiling))
	}
	ctx, cancel := context.WithCancelCause(base)
	l := &TurnLiveness{window: window, lastProgress: time.Now(), cancel: cancel, stopCeiling: stopCeiling}
	// Armed under the lock: a window short enough to fire during
	// construction would otherwise race its own field assignment.
	wedged := causeOr(cfg.NoProgressCause, ErrNoProgress)
	l.mu.Lock()
	l.timer = time.AfterFunc(window, func() { l.fire(wedged) })
	l.mu.Unlock()
	return l, ctx, func() { l.fire(context.Canceled) }
}

// fire settles the turn exactly once: stop the timer, cancel with cause,
// release the ceiling context. Later calls — the caller's stop func after a
// cut, a progress update racing the no-progress timer — are no-ops, so the
// first cause reported wins and a settled turn can never be resurrected.
//
// The one case that must NOT settle is the no-progress timer losing a race
// it should have won: the timer fires, and progress lands before the
// callback takes the lock. Resetting the timer inside progress is not
// enough — the callback is already running — so the deadline is re-checked
// against lastProgress here, and a turn that made progress within the
// window is given the remainder rather than reported as wedged.
//
// Cancelling with a cause on a context whose PARENT already ended (the
// ceiling deadline) is itself a no-op, so the ceiling's cause survives.
func (l *TurnLiveness) fire(cause error) {
	l.mu.Lock()
	if l.done {
		l.mu.Unlock()
		return
	}
	if errors.Is(cause, ErrNoProgress) {
		if left := l.window - time.Since(l.lastProgress); left > 0 {
			l.timer.Reset(left)
			l.mu.Unlock()
			return
		}
	}
	l.done = true
	l.timer.Stop()
	l.mu.Unlock()
	l.cancel(cause)
	l.stopCeiling()
}

// Progress restarts the no-progress window: evidence that the agent is
// doing work. Call it for — and ONLY for — updates that satisfy
// IsProgress; a relay that resets this clock on bookkeeping frames
// (plans, spinners, mode changes) can never detect a wedged agent, which
// is the whole failure this construct exists to catch.
//
// Most consumers should install Wrap(sink) instead and never call this:
// Wrap applies IsProgress for them. It is exported for relays whose ACP
// sink is session-lifetime rather than per-turn — poe-acp enqueues every
// session/update onto one drain goroutine and attaches the per-turn sink
// in-band, so a per-turn decorator has nowhere to live; it classifies
// with IsProgress in the drain and feeds the watcher directly.
//
// It deliberately does not touch the ceiling — that is an absolute cap,
// by definition unaffected by progress — and does nothing once the turn
// has settled: a stray update arriving after the cut must not re-arm a
// timer nobody will ever stop.
func (l *TurnLiveness) Progress() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.done {
		return
	}
	l.lastProgress = time.Now()
	l.timer.Reset(l.window)
}

// Wrap returns a SessionUpdateSink that forwards every update to down and
// resets the no-progress clock (Progress) on those that count as progress. Install the
// result as the session's sink for the duration of the turn.
func (l *TurnLiveness) Wrap(down SessionUpdateSink) SessionUpdateSink {
	return &livenessSink{live: l, down: down}
}

// livenessSink is the pass-through half of TurnLiveness.
type livenessSink struct {
	live *TurnLiveness
	down SessionUpdateSink
}

// OnUpdate implements SessionUpdateSink.
func (s *livenessSink) OnUpdate(ctx context.Context, n acp.SessionNotification) error {
	if IsProgress(n.Update) {
		s.live.Progress()
	}
	return s.down.OnUpdate(ctx, n)
}
