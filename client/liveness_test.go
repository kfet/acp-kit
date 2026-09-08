package client

import (
	"context"
	"errors"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
)

// recordSink counts what reached the downstream sink.
type recordSink struct {
	n   int
	err error
}

func (r *recordSink) OnUpdate(context.Context, acp.SessionNotification) error {
	r.n++
	return r.err
}

func note(u acp.SessionUpdate) acp.SessionNotification {
	return acp.SessionNotification{SessionId: "s1", Update: u}
}

func TestIsProgress(t *testing.T) {
	progress := map[string]acp.SessionUpdate{
		"agent message": {AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{Content: acp.TextBlock("hi")}},
		"agent thought": {AgentThoughtChunk: &acp.SessionUpdateAgentThoughtChunk{Content: acp.TextBlock("hm")}},
		"tool call":     {ToolCall: &acp.SessionUpdateToolCall{ToolCallId: "t1"}},
		"tool update":   {ToolCallUpdate: &acp.SessionToolCallUpdate{ToolCallId: "t1"}},
	}
	for name, u := range progress {
		if !IsProgress(u) {
			t.Errorf("%s must count as progress", name)
		}
	}
	cosmetic := map[string]acp.SessionUpdate{
		"empty":        {},
		"user chunk":   {UserMessageChunk: &acp.SessionUpdateUserMessageChunk{Content: acp.TextBlock("hi")}},
		"plan":         {Plan: &acp.SessionUpdatePlan{}},
		"commands":     {AvailableCommandsUpdate: &acp.SessionAvailableCommandsUpdate{}},
		"mode":         {CurrentModeUpdate: &acp.SessionCurrentModeUpdate{}},
		"session info": {SessionInfoUpdate: &acp.SessionSessionInfoUpdate{}},
	}
	for name, u := range cosmetic {
		if IsProgress(u) {
			t.Errorf("%s must NOT count as progress", name)
		}
	}
}

// waitCause blocks until ctx is done and returns its cause.
func waitCause(t *testing.T, ctx context.Context) error {
	t.Helper()
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-time.After(2 * time.Second):
		t.Fatal("turn context was never cancelled")
		return nil
	}
}

func TestTurnLiveness_NoProgressCuts(t *testing.T) {
	_, ctx, stop := StartTurnLiveness(context.Background(), TurnLivenessConfig{NoProgressTimeout: 20 * time.Millisecond})
	defer stop()
	if got := waitCause(t, ctx); !errors.Is(got, ErrNoProgress) {
		t.Fatalf("cause = %v, want ErrNoProgress", got)
	}
}

func TestTurnLiveness_ProgressResetsTheClock(t *testing.T) {
	live, ctx, stop := StartTurnLiveness(context.Background(), TurnLivenessConfig{NoProgressTimeout: 60 * time.Millisecond})
	defer stop()
	rec := &recordSink{}
	sink := live.Wrap(rec)

	// Five tool_call updates spread over well past the window: a
	// long-running tool must never be cut.
	deadline := time.Now().Add(180 * time.Millisecond)
	for time.Now().Before(deadline) {
		if err := sink.OnUpdate(ctx, note(acp.SessionUpdate{ToolCallUpdate: &acp.SessionToolCallUpdate{ToolCallId: "t1"}})); err != nil {
			t.Fatalf("OnUpdate: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := ctx.Err(); err != nil {
		t.Fatalf("turn cut while making progress: %v", context.Cause(ctx))
	}
	if rec.n == 0 {
		t.Fatal("updates must still reach the downstream sink")
	}
	// Once progress stops, the window applies again.
	if got := waitCause(t, ctx); !errors.Is(got, ErrNoProgress) {
		t.Fatalf("cause = %v, want ErrNoProgress", got)
	}
}

func TestTurnLiveness_CosmeticUpdateDoesNotReset(t *testing.T) {
	live, ctx, stop := StartTurnLiveness(context.Background(), TurnLivenessConfig{NoProgressTimeout: 60 * time.Millisecond})
	defer stop()
	sink := live.Wrap(&recordSink{})
	// Spinner-equivalent frames at a fast cadence: the turn must still be cut.
	go func() {
		for i := 0; i < 100; i++ {
			_ = sink.OnUpdate(context.Background(), note(acp.SessionUpdate{Plan: &acp.SessionUpdatePlan{}}))
			time.Sleep(5 * time.Millisecond)
		}
	}()
	if got := waitCause(t, ctx); !errors.Is(got, ErrNoProgress) {
		t.Fatalf("cause = %v, want ErrNoProgress", got)
	}
}

func TestTurnLiveness_CeilingOffByDefault(t *testing.T) {
	live, ctx, stop := StartTurnLiveness(context.Background(), TurnLivenessConfig{NoProgressTimeout: 40 * time.Millisecond})
	defer stop()
	if _, ok := ctx.Deadline(); ok {
		t.Fatal("MaxTurnDuration <=0 must set no deadline")
	}
	sink := live.Wrap(&recordSink{})
	// Keep making progress for far longer than any default ceiling would be.
	for i := 0; i < 10; i++ {
		_ = sink.OnUpdate(ctx, note(acp.SessionUpdate{AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{Content: acp.TextBlock("x")}}))
		time.Sleep(15 * time.Millisecond)
	}
	if err := ctx.Err(); err != nil {
		t.Fatalf("no ceiling was configured, yet the turn was cut: %v", context.Cause(ctx))
	}
}

func TestTurnLiveness_CeilingCutsDespiteProgress(t *testing.T) {
	live, ctx, stop := StartTurnLiveness(context.Background(), TurnLivenessConfig{
		NoProgressTimeout: time.Hour,
		MaxTurnDuration:   40 * time.Millisecond,
	})
	defer stop()
	sink := live.Wrap(&recordSink{})
	go func() {
		for i := 0; i < 100; i++ {
			_ = sink.OnUpdate(context.Background(), note(acp.SessionUpdate{ToolCall: &acp.SessionUpdateToolCall{ToolCallId: "t1"}}))
			time.Sleep(5 * time.Millisecond)
		}
	}()
	if _, ok := ctx.Deadline(); !ok {
		t.Fatal("a configured ceiling must be visible as a deadline")
	}
	if got := waitCause(t, ctx); !errors.Is(got, ErrTurnCeiling) {
		t.Fatalf("cause = %v, want ErrTurnCeiling", got)
	}
	stop() // the ceiling's cause must survive a later stop
	if got := context.Cause(ctx); !errors.Is(got, ErrTurnCeiling) {
		t.Fatalf("cause after stop = %v, want ErrTurnCeiling", got)
	}
}

func TestTurnLiveness_StopIsPlainCanceled(t *testing.T) {
	_, ctx, stop := StartTurnLiveness(context.Background(), TurnLivenessConfig{NoProgressTimeout: time.Hour})
	stop()
	got := waitCause(t, ctx)
	if !errors.Is(got, context.Canceled) {
		t.Fatalf("cause = %v, want context.Canceled", got)
	}
	if errors.Is(got, ErrNoProgress) || errors.Is(got, ErrTurnCeiling) {
		t.Fatal("a user-initiated stop must be distinguishable from a liveness cut")
	}
	stop() // idempotent
}

func TestTurnLiveness_DefaultWindow(t *testing.T) {
	live, _, stop := StartTurnLiveness(context.Background(), TurnLivenessConfig{})
	defer stop()
	if live.window != defaultNoProgressTimeout {
		t.Fatalf("window = %s, want %s", live.window, defaultNoProgressTimeout)
	}
}

// A progress update landing after the turn was already cut must neither
// resurrect the context nor re-arm a timer nobody will stop.
func TestTurnLiveness_ProgressAfterCutIsInert(t *testing.T) {
	live, ctx, stop := StartTurnLiveness(context.Background(), TurnLivenessConfig{NoProgressTimeout: 20 * time.Millisecond})
	defer stop()
	if got := waitCause(t, ctx); !errors.Is(got, ErrNoProgress) {
		t.Fatalf("cause = %v, want ErrNoProgress", got)
	}
	rec := &recordSink{}
	sink := live.Wrap(rec)
	if err := sink.OnUpdate(context.Background(), note(acp.SessionUpdate{ToolCall: &acp.SessionUpdateToolCall{ToolCallId: "t1"}})); err != nil {
		t.Fatalf("OnUpdate: %v", err)
	}
	if rec.n != 1 {
		t.Fatal("a late update must still be forwarded downstream")
	}
	if !errors.Is(context.Cause(ctx), ErrNoProgress) {
		t.Fatal("a late update must not change the settled cause")
	}
	live.mu.Lock()
	done := live.done
	live.mu.Unlock()
	if !done {
		t.Fatal("a late update must not un-settle the watcher")
	}
}

// The decorator is transparent: a downstream error propagates unchanged.
func TestTurnLiveness_WrapPropagatesDownstreamError(t *testing.T) {
	live, _, stop := StartTurnLiveness(context.Background(), TurnLivenessConfig{NoProgressTimeout: time.Hour})
	defer stop()
	want := errors.New("boom")
	sink := live.Wrap(&recordSink{err: want})
	if err := sink.OnUpdate(context.Background(), note(acp.SessionUpdate{Plan: &acp.SessionUpdatePlan{}})); !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}
