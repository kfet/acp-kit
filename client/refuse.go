package client

import (
	"context"
	"strings"
	"sync"

	acp "github.com/coder/acp-go-sdk"
)

// Validator inspects the complete assistant message text produced during a
// single prompt turn and decides whether it may be delivered to the user.
type Validator interface {
	// Validate returns ok=true to accept the output. To refuse it, return
	// ok=false with a short, agent-facing reason describing what is wrong.
	// The reason is fed back to the agent as the next prompt so it can
	// regenerate a compliant message.
	Validate(text string) (reason string, ok bool)
}

// ValidatorFunc adapts a plain function to a Validator.
type ValidatorFunc func(text string) (reason string, ok bool)

// Validate implements Validator.
func (f ValidatorFunc) Validate(text string) (string, bool) { return f(text) }

// Prompter is the subset of an ACP agent client that PromptValidated drives.
// *AgentProc satisfies it.
type Prompter interface {
	Prompt(ctx context.Context, sid acp.SessionId, prompt []acp.ContentBlock) (acp.StopReason, error)
}

// ValidatingSink wraps a downstream SessionUpdateSink. While a turn is in
// progress it buffers AgentMessageChunk updates (the assistant's visible
// message) instead of forwarding them, so an orchestrator can validate the
// complete message and either Commit it (flush downstream) or Drop it
// (discard) before the user ever sees it. Every other update — thoughts,
// tool calls, plans, command catalogs — passes through live, so progress
// keeps streaming.
//
// Holding the message until validation is deliberate: a generic ACP
// transport cannot assume it can retract bytes already sent, so buffering
// the visible message is the only transport-agnostic way to guarantee that
// rejected output never reaches the user.
type ValidatingSink struct {
	down SessionUpdateSink

	mu   sync.Mutex
	buf  []acp.SessionNotification // buffered agent-message-chunk updates, in order
	text strings.Builder           // accumulated message text for the current attempt
}

// NewValidatingSink wraps down. Install the returned sink as the session's
// SessionUpdateSink (e.g. via AgentProc.NewSession) and drive turns through
// PromptValidated.
func NewValidatingSink(down SessionUpdateSink) *ValidatingSink {
	return &ValidatingSink{down: down}
}

// OnUpdate implements SessionUpdateSink. Agent message chunks are buffered;
// all other updates are forwarded downstream immediately.
func (v *ValidatingSink) OnUpdate(ctx context.Context, n acp.SessionNotification) error {
	if c := n.Update.AgentMessageChunk; c != nil {
		v.mu.Lock()
		v.buf = append(v.buf, n)
		if c.Content.Text != nil {
			v.text.WriteString(c.Content.Text.Text)
		}
		v.mu.Unlock()
		return nil
	}
	return v.down.OnUpdate(ctx, n)
}

// Text returns the assistant message text accumulated for the current attempt.
func (v *ValidatingSink) Text() string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.text.String()
}

func (v *ValidatingSink) reset() {
	v.mu.Lock()
	v.buf = nil
	v.text.Reset()
	v.mu.Unlock()
}

// Commit flushes the buffered message chunks downstream in order, then clears.
func (v *ValidatingSink) Commit(ctx context.Context) error {
	v.mu.Lock()
	buf := v.buf
	v.buf = nil
	v.text.Reset()
	v.mu.Unlock()
	for _, n := range buf {
		if err := v.down.OnUpdate(ctx, n); err != nil {
			return err
		}
	}
	return nil
}

// CommitText discards whatever was buffered and flushes a single synthetic
// message chunk carrying text. Used to deliver a transformed (e.g. escaped)
// fallback when regeneration is exhausted.
func (v *ValidatingSink) CommitText(ctx context.Context, sid acp.SessionId, text string) error {
	v.reset()
	return v.down.OnUpdate(ctx, acp.SessionNotification{
		SessionId: sid,
		Update: acp.SessionUpdate{
			AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{Content: acp.TextBlock(text)},
		},
	})
}

// Drop discards buffered message chunks (used before a regeneration).
func (v *ValidatingSink) Drop() { v.reset() }

// RefuseConfig configures PromptValidated.
type RefuseConfig struct {
	// Validator decides whether the assistant message is acceptable.
	// If nil, output is always accepted (PromptValidated becomes a plain
	// buffered prompt).
	Validator Validator
	// MaxRefusals caps how many times the agent is re-prompted to
	// regenerate. 0 validates once and never re-prompts.
	MaxRefusals int
	// BuildReprompt builds the corrective prompt fed back to the agent
	// after a refusal. If nil, a single text block holding the reason is
	// used.
	BuildReprompt func(reason string) []acp.ContentBlock
	// Fallback transforms the rejected text into a deliverable form when
	// regeneration is exhausted (e.g. mechanically escape offending
	// tokens). If nil, the last (still-rejected) output is delivered as-is.
	Fallback func(text string) string
}

// RefuseResult reports the outcome of PromptValidated.
type RefuseResult struct {
	Stop     acp.StopReason // stop reason of the delivered turn
	Refusals int            // number of regenerations triggered
	Accepted bool           // a generation passed validation
	FellBack bool           // Fallback was applied after exhaustion
}

// PromptValidated runs an ACP prompt, validates the assistant's complete
// message via vs (which MUST be the session's sink), and on refusal
// re-prompts the agent with a short reason up to cfg.MaxRefusals times. The
// visible message is delivered downstream only once accepted — or, on
// exhaustion, after cfg.Fallback. Non-message updates stream live throughout.
//
// This is the generic "refuse LLM output" construct: a transport-agnostic
// way for an ACP client to reject and regenerate an agent's visible message
// before the user sees it, with a deterministic fallback so a turn can never
// wedge against a stubborn model.
func PromptValidated(ctx context.Context, agent Prompter, sid acp.SessionId, prompt []acp.ContentBlock, vs *ValidatingSink, cfg RefuseConfig) (RefuseResult, error) {
	cur := prompt
	var res RefuseResult
	for {
		vs.reset()
		stop, err := agent.Prompt(ctx, sid, cur)
		if err != nil {
			return res, err
		}
		res.Stop = stop

		if cfg.Validator == nil {
			res.Accepted = true
			return res, vs.Commit(ctx)
		}
		reason, ok := cfg.Validator.Validate(vs.Text())
		if ok {
			res.Accepted = true
			return res, vs.Commit(ctx)
		}
		if res.Refusals >= cfg.MaxRefusals {
			if cfg.Fallback != nil {
				res.FellBack = true
				return res, vs.CommitText(ctx, sid, cfg.Fallback(vs.Text()))
			}
			return res, vs.Commit(ctx) // best effort: nothing better to do
		}
		vs.Drop()
		res.Refusals++
		if cfg.BuildReprompt != nil {
			cur = cfg.BuildReprompt(reason)
		} else {
			cur = []acp.ContentBlock{acp.TextBlock(reason)}
		}
	}
}
