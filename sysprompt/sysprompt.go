// Package sysprompt composes relay-owned durable system prompt text.
package sysprompt

import (
	"fmt"
	"strings"
	"time"

	"github.com/kfet/acp-kit/client"
)

// LivenessNote renders the turn-watchdog disclosure for the agent's system
// prompt from the live window, so the text can never drift from the knob the
// relay actually arms (client.TurnLivenessConfig.NoProgressTimeout). A window
// <=0 renders the same default StartTurnLiveness falls back to.
//
// The wording is deliberately minimal; callers append it to a composed prompt.
func LivenessNote(window time.Duration) string {
	if window <= 0 {
		window = client.DefaultNoProgressTimeout
	}
	return fmt.Sprintf("Turn watchdog: %s with no text and no tool call. "+
		"Tool calls and poll progress reset it. Context survives a cancel.", window)
}

// Compose joins the base relay prompt, optional operator extra text, and
// optional skills catalog. Empty pieces are skipped and pieces are separated
// by a blank line.
func Compose(base, extra, catalog string) string {
	parts := make([]string, 0, 3)
	for _, p := range []string{base, extra, catalog} {
		p = strings.TrimSpace(p)
		if p != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, "\n\n")
}

// Resolve returns an empty prompt when disabled, otherwise Compose(base, extra, catalog).
func Resolve(base, extra string, disabled bool, catalog string) string {
	if disabled {
		return ""
	}
	return Compose(base, extra, catalog)
}
