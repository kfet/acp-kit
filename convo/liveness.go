package convo

import (
	"context"

	"github.com/kfet/acp-kit/client"
)

// Clock is one armed turn bound. Progress resets it; Wrap returns a sink
// that resets it on every progress-bearing session update.
type Clock interface {
	Progress()
	Wrap(down client.SessionUpdateSink) client.SessionUpdateSink
}

// Liveness is the relay's turn-bound policy. Arm returns the armed clock,
// a context that is cancelled (with a cause) when the bound trips, and a
// stop func that disarms it. Relays arm it where their turn really
// starts — after any queue wait, just before prompting — so a turn is
// never charged for time spent behind another one.
type Liveness interface {
	Arm(ctx context.Context) (Clock, context.Context, context.CancelFunc)
}

// ProgressClock is the default Liveness: acp-kit's progress-resetting
// no-progress timeout plus an optional absolute ceiling. See
// client.TurnLivenessConfig.
type ProgressClock client.TurnLivenessConfig

// Arm implements Liveness.
func (p ProgressClock) Arm(ctx context.Context) (Clock, context.Context, context.CancelFunc) {
	return client.StartTurnLiveness(ctx, client.TurnLivenessConfig(p))
}

// Unbounded is a Liveness that never trips: for relays whose turns are
// bounded elsewhere (e.g. by the HTTP request that carries them).
type Unbounded struct{}

// Arm implements Liveness.
func (Unbounded) Arm(ctx context.Context) (Clock, context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(ctx)
	return nopClock{}, ctx, cancel
}

type nopClock struct{}

func (nopClock) Progress() {}

func (nopClock) Wrap(down client.SessionUpdateSink) client.SessionUpdateSink { return down }
