package client

import (
	"context"
	"strings"
	"testing"

	acp "github.com/coder/acp-go-sdk"
)

// capSink records what actually reaches the downstream transport.
type capSink struct {
	msgs  []string // delivered agent-message texts, in order
	other int      // count of non-message updates passed through
}

func (c *capSink) OnUpdate(_ context.Context, n acp.SessionNotification) error {
	if ch := n.Update.AgentMessageChunk; ch != nil {
		if ch.Content.Text != nil {
			c.msgs = append(c.msgs, ch.Content.Text.Text)
		}
		return nil
	}
	c.other++
	return nil
}

// scriptAgent emits one scripted message per Prompt call into vs, optionally
// preceded by a live (non-message) thought update.
type scriptAgent struct {
	vs       *ValidatingSink
	turns    []string
	emitThnk bool
	n        int
	prompts  [][]acp.ContentBlock
}

func (a *scriptAgent) Prompt(ctx context.Context, sid acp.SessionId, p []acp.ContentBlock) (acp.StopReason, error) {
	a.prompts = append(a.prompts, p)
	if a.emitThnk {
		_ = a.vs.OnUpdate(ctx, acp.SessionNotification{SessionId: sid, Update: acp.SessionUpdate{
			AgentThoughtChunk: &acp.SessionUpdateAgentThoughtChunk{Content: acp.TextBlock("thinking")},
		}})
	}
	text := ""
	if a.n < len(a.turns) {
		text = a.turns[a.n]
	}
	a.n++
	_ = a.vs.OnUpdate(ctx, acp.SessionNotification{SessionId: sid, Update: acp.SessionUpdate{
		AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{Content: acp.TextBlock(text)},
	}})
	return acp.StopReasonEndTurn, nil
}

func rejectIfContains(bad string) Validator {
	return ValidatorFunc(func(text string) (string, bool) {
		if strings.Contains(text, bad) {
			return "output contains forbidden token " + bad, false
		}
		return "", true
	})
}

func TestPromptValidated_AcceptFirstTry(t *testing.T) {
	cap := &capSink{}
	vs := NewValidatingSink(cap)
	ag := &scriptAgent{vs: vs, turns: []string{"all good"}}
	res, err := PromptValidated(context.Background(), ag, "s", []acp.ContentBlock{acp.TextBlock("hi")}, vs,
		RefuseConfig{Validator: rejectIfContains("BAD"), MaxRefusals: 2})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Accepted || res.Refusals != 0 || res.FellBack {
		t.Fatalf("res=%+v", res)
	}
	if len(cap.msgs) != 1 || cap.msgs[0] != "all good" {
		t.Fatalf("delivered=%v", cap.msgs)
	}
}

func TestPromptValidated_RefuseThenAccept(t *testing.T) {
	cap := &capSink{}
	vs := NewValidatingSink(cap)
	ag := &scriptAgent{vs: vs, turns: []string{"this is BAD", "now clean"}}
	res, err := PromptValidated(context.Background(), ag, "s", []acp.ContentBlock{acp.TextBlock("hi")}, vs,
		RefuseConfig{Validator: rejectIfContains("BAD"), MaxRefusals: 2})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Accepted || res.Refusals != 1 || res.FellBack {
		t.Fatalf("res=%+v", res)
	}
	// The user must never have seen the BAD output.
	if len(cap.msgs) != 1 || cap.msgs[0] != "now clean" {
		t.Fatalf("delivered=%v (BAD must not leak)", cap.msgs)
	}
	// The refusal reason must have been fed back as the 2nd prompt.
	if len(ag.prompts) != 2 {
		t.Fatalf("prompts=%d", len(ag.prompts))
	}
	if got := ag.prompts[1][0].Text.Text; !strings.Contains(got, "forbidden token BAD") {
		t.Fatalf("reprompt=%q", got)
	}
}

func TestPromptValidated_ExhaustFallback(t *testing.T) {
	cap := &capSink{}
	vs := NewValidatingSink(cap)
	ag := &scriptAgent{vs: vs, turns: []string{"BAD one", "BAD two", "BAD three", "BAD four"}}
	res, err := PromptValidated(context.Background(), ag, "s", []acp.ContentBlock{acp.TextBlock("hi")}, vs,
		RefuseConfig{
			Validator:   rejectIfContains("BAD"),
			MaxRefusals: 2,
			Fallback:    func(text string) string { return strings.ReplaceAll(text, "BAD", "B-A-D") },
		})
	if err != nil {
		t.Fatal(err)
	}
	if res.Accepted || !res.FellBack || res.Refusals != 2 {
		t.Fatalf("res=%+v", res)
	}
	// 1 initial + 2 regenerations = 3 prompts.
	if len(ag.prompts) != 3 {
		t.Fatalf("prompts=%d", len(ag.prompts))
	}
	// Delivered output is the escaped fallback of the last attempt, no raw BAD.
	if len(cap.msgs) != 1 || strings.Contains(cap.msgs[0], "BAD") {
		t.Fatalf("delivered=%v", cap.msgs)
	}
}

func TestValidatingSink_NonMessagePassesThroughLive(t *testing.T) {
	cap := &capSink{}
	vs := NewValidatingSink(cap)
	// A thought update must reach downstream immediately, before any commit.
	err := vs.OnUpdate(context.Background(), acp.SessionNotification{SessionId: "s", Update: acp.SessionUpdate{
		AgentThoughtChunk: &acp.SessionUpdateAgentThoughtChunk{Content: acp.TextBlock("live")},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if cap.other != 1 {
		t.Fatalf("expected live passthrough, other=%d", cap.other)
	}
	// A message chunk must be held until Commit.
	_ = vs.OnUpdate(context.Background(), acp.SessionNotification{SessionId: "s", Update: acp.SessionUpdate{
		AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{Content: acp.TextBlock("held")},
	}})
	if len(cap.msgs) != 0 {
		t.Fatalf("message leaked before commit: %v", cap.msgs)
	}
	if err := vs.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(cap.msgs) != 1 || cap.msgs[0] != "held" {
		t.Fatalf("after commit: %v", cap.msgs)
	}
}

func TestPromptValidated_NilValidatorIsPassthrough(t *testing.T) {
	cap := &capSink{}
	vs := NewValidatingSink(cap)
	ag := &scriptAgent{vs: vs, turns: []string{"whatever"}, emitThnk: true}
	res, err := PromptValidated(context.Background(), ag, "s", []acp.ContentBlock{acp.TextBlock("hi")}, vs, RefuseConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Accepted || res.Refusals != 0 {
		t.Fatalf("res=%+v", res)
	}
	if cap.other != 1 || len(cap.msgs) != 1 || cap.msgs[0] != "whatever" {
		t.Fatalf("msgs=%v other=%d", cap.msgs, cap.other)
	}
}

// errSink fails on the first message delivery.
type errSink struct{ called int }

func (e *errSink) OnUpdate(_ context.Context, n acp.SessionNotification) error {
	if n.Update.AgentMessageChunk != nil {
		e.called++
		return context.Canceled
	}
	return nil
}

func TestPromptValidated_CommitErrorPropagates(t *testing.T) {
	vs := NewValidatingSink(&errSink{})
	ag := &scriptAgent{vs: vs, turns: []string{"ok"}}
	_, err := PromptValidated(context.Background(), ag, "s", []acp.ContentBlock{acp.TextBlock("hi")}, vs,
		RefuseConfig{Validator: rejectIfContains("BAD")})
	if err == nil {
		t.Fatal("expected commit error to propagate")
	}
}

// errAgent fails the prompt call.
type errAgent struct{}

func (errAgent) Prompt(context.Context, acp.SessionId, []acp.ContentBlock) (acp.StopReason, error) {
	return "", context.Canceled
}

func TestPromptValidated_PromptErrorPropagates(t *testing.T) {
	vs := NewValidatingSink(&capSink{})
	_, err := PromptValidated(context.Background(), errAgent{}, "s", nil, vs, RefuseConfig{})
	if err == nil {
		t.Fatal("expected prompt error to propagate")
	}
}

func TestPromptValidated_ExhaustNoFallbackDeliversAsIs(t *testing.T) {
	cap := &capSink{}
	vs := NewValidatingSink(cap)
	ag := &scriptAgent{vs: vs, turns: []string{"BAD a", "BAD b"}}
	res, err := PromptValidated(context.Background(), ag, "s", []acp.ContentBlock{acp.TextBlock("hi")}, vs,
		RefuseConfig{Validator: rejectIfContains("BAD"), MaxRefusals: 1}) // no Fallback
	if err != nil {
		t.Fatal(err)
	}
	if res.Accepted || res.FellBack || res.Refusals != 1 {
		t.Fatalf("res=%+v", res)
	}
	if len(cap.msgs) != 1 || cap.msgs[0] != "BAD b" {
		t.Fatalf("delivered=%v", cap.msgs)
	}
}

func TestPromptValidated_CustomBuildReprompt(t *testing.T) {
	cap := &capSink{}
	vs := NewValidatingSink(cap)
	ag := &scriptAgent{vs: vs, turns: []string{"BAD", "clean"}}
	res, err := PromptValidated(context.Background(), ag, "s", []acp.ContentBlock{acp.TextBlock("hi")}, vs,
		RefuseConfig{
			Validator:     rejectIfContains("BAD"),
			MaxRefusals:   1,
			BuildReprompt: func(reason string) []acp.ContentBlock { return []acp.ContentBlock{acp.TextBlock("FIXIT: " + reason)} },
		})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Accepted || res.Refusals != 1 {
		t.Fatalf("res=%+v", res)
	}
	if got := ag.prompts[1][0].Text.Text; !strings.HasPrefix(got, "FIXIT: ") {
		t.Fatalf("custom reprompt not used: %q", got)
	}
}
