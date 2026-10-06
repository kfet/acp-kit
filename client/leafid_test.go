package client

import (
	"context"
	"errors"
	"testing"

	acp "github.com/coder/acp-go-sdk"
)

// turnFake is a TurnPrompter that reports a fixed leaf id.
type turnFake struct {
	leaf string
	err  error
}

func (f turnFake) Prompt(ctx context.Context, sid acp.SessionId, p []acp.ContentBlock) (acp.StopReason, error) {
	tr, err := f.PromptTurn(ctx, sid, p)
	return tr.Stop, err
}

func (f turnFake) PromptTurn(context.Context, acp.SessionId, []acp.ContentBlock) (TurnResult, error) {
	if f.err != nil {
		return TurnResult{}, f.err
	}
	return TurnResult{Stop: acp.StopReasonEndTurn, LeafID: f.leaf}, nil
}

// plainFake is a Prompter only.
type plainFake struct{}

func (plainFake) Prompt(context.Context, acp.SessionId, []acp.ContentBlock) (acp.StopReason, error) {
	return acp.StopReasonEndTurn, nil
}

func TestLeafIDOf(t *testing.T) {
	for _, c := range []struct {
		meta map[string]any
		want string
	}{
		{nil, ""},
		{map[string]any{"leafId": 3}, ""},
		{map[string]any{"leafId": "x"}, "x"},
	} {
		if got := leafIDOf(c.meta); got != c.want {
			t.Errorf("leafIDOf(%v) = %q, want %q", c.meta, got, c.want)
		}
	}
}

func TestPromptTurnFallsBackToPrompt(t *testing.T) {
	tr, err := promptTurn(context.Background(), plainFake{}, "s", nil)
	if err != nil || tr.Stop != acp.StopReasonEndTurn || tr.LeafID != "" {
		t.Fatalf("got %+v %v", tr, err)
	}
}

func TestAbstainAndRefuseCarryLeafID(t *testing.T) {
	ctx := context.Background()
	vs := NewValidatingSink(nil)
	ar, err := PromptAbstainable(ctx, turnFake{leaf: "L1"}, "s", nil, vs, "")
	if err != nil || !ar.Abstained || ar.LeafID != "L1" {
		t.Fatalf("abstain: %+v %v", ar, err)
	}
	rr, err := PromptValidated(ctx, turnFake{leaf: "L2"}, "s", nil, vs, RefuseConfig{})
	if err != nil || rr.LeafID != "L2" {
		t.Fatalf("refuse: %+v %v", rr, err)
	}
	if _, err := PromptValidated(ctx, turnFake{err: errors.New("x")}, "s", nil, vs, RefuseConfig{}); err == nil {
		t.Fatal("want error")
	}
}
