package convo

import (
	"encoding/json"

	"github.com/kfet/acp-kit/command"
)

// mustJSON marshals a map[string]string. encoding/json cannot fail on
// that type — every key and value is a string — so an error here is a
// broken standard library, not a condition a caller can handle.
func mustJSON(m map[string]string) []byte { return mustJSONWith(json.Marshal, m) }

func mustJSONWith(marshal func(any) ([]byte, error), v any) []byte {
	b, err := marshal(v)
	if err != nil {
		panic("convo: marshal overrides: " + err.Error())
	}
	return b
}

// mustOutcome asserts the broker's contract: once Dispatch has
// established that a message is a command — a login is pending, or
// Broker.IsCommand said so — Broker.Handle returns an Outcome. The two
// are defined against the same command list with the same verb folding,
// so a nil here means acp-kit itself drifted; crashing on the first
// affected command beats silently eating a user's message.
func mustOutcome(out *command.Outcome) *command.Outcome {
	if out == nil {
		panic("convo: acp-kit/command returned no outcome for a recognised command — IsCommand and Handle disagree")
	}
	return out
}
