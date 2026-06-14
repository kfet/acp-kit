package client

import (
	"context"
	"strings"

	acp "github.com/coder/acp-go-sdk"
)

// AbstainResult reports the outcome of PromptAbstainable.
type AbstainResult struct {
	// Stop is the stop reason of the turn.
	Stop acp.StopReason
	// Abstained is true when the agent declined to respond and nothing was
	// delivered downstream.
	Abstained bool
}

// PromptAbstainable runs an ACP prompt through vs (which MUST be the session's
// sink) and lets the agent decline to respond. If the complete assistant
// message, trimmed, equals sentinel or is empty, the buffered output is
// discarded and nothing is delivered downstream (Abstained=true). Otherwise the
// buffered message is flushed downstream. Non-message updates (thoughts, plans,
// tool calls) stream live throughout — callers that must suppress those before
// the abstain verdict should gate them in the downstream sink.
//
// The prompt always reaches the session regardless of the verdict, so the agent
// stays caught up on the conversation even when it stays silent. This is the
// generic decline construct for ambient participation: a transport-agnostic way
// for any ACP agent to opt out of replying via a sentinel string, with no tool
// plumbing. A blank sentinel disables sentinel-matching (only an empty message
// abstains).
func PromptAbstainable(ctx context.Context, agent Prompter, sid acp.SessionId, prompt []acp.ContentBlock, vs *ValidatingSink, sentinel string) (AbstainResult, error) {
	vs.Drop()
	stop, err := agent.Prompt(ctx, sid, prompt)
	res := AbstainResult{Stop: stop}
	if err != nil {
		return res, err
	}
	text := strings.TrimSpace(vs.Text())
	if text == "" || (sentinel != "" && text == strings.TrimSpace(sentinel)) {
		vs.Drop()
		res.Abstained = true
		return res, nil
	}
	return res, vs.Commit(ctx)
}
