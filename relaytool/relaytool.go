// Package relaytool exposes a relay's own bot interface to the ACP
// agent as self-hosted MCP tools, so the agent can drive the relay from
// inside a turn.
//
// # Why MCP and not an ACP extension
//
// ACP has no agent-initiated message. The agent speaks only inside a
// turn it was prompted for, and a relay's streaming sink is bound per
// turn, so an out-of-turn session/update has nowhere to land. But an
// MCP tool call runs agent→client, which ACP fully supports. The
// loopback is therefore the correct mechanism and needs no protocol
// extension.
//
// # Identity is the foundation
//
// mcphost binds the session key SERVER-SIDE from the connection's
// token, so a tool call already knows, unspoofably, which conversation
// it came from. Nothing here ever accepts a conversation id as a tool
// argument. Every tool acts on the conversation the call came from, and
// only that one. Take that guarantee away and `post` becomes a
// realm-wide megaphone for anything that can prompt-inject the agent.
//
// # One implementation, two front ends
//
// Every tool here calls an exported action on *command.Broker — the
// same method the `!command` a human types calls. There is no second
// implementation to drift. Adding a tool means adding a Broker action
// (and, where it makes sense for a human, a `!command` for it), never
// reaching past the Broker to the relay's Controller.
//
// # What is deliberately NOT exposed
//
// A loopback tool must never destroy the turn that is calling it. Two
// commands fall foul of that rule:
//
//   - `stop`. An agent cancelling its own in-flight turn either does
//     nothing or kills the very turn whose tool call asked for it,
//     leaving the tool result undeliverable and the user staring at
//     "superseded". Deferring it to end-of-turn would make it a no-op
//     by definition. There is no coherent reading, so there is no tool.
//   - `new_session`, in its immediate form. Resetting a session
//     typically cancels the turn in flight, which is the same
//     foot-gun. It IS a coherent thing for an agent to want — "this
//     task is done, clear the context" — so it is exposed as a
//     DEFERRED action: the tool records the intent, and the relay
//     applies it by calling Tools.EndTurn when the turn is over. Same
//     Controller, same implementation, honest timing.
package relaytool

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/kfet/acp-kit/command"
	"github.com/kfet/acp-kit/mcphost"
)

// Tool names exposed to the agent.
const (
	ToolStatus        = "status"
	ToolListModels    = "list_models"
	ToolSetModel      = "set_model"
	ToolNewSession    = "new_session"
	ToolPost          = "post"
	ToolSchedule      = "schedule"
	ToolListSchedules = "list_schedules"
	ToolUnschedule    = "unschedule"
)

// Config configures a Tools.
type Config struct {
	// Broker is the shared command surface. Required, and it must
	// already have its Controller wired.
	Broker *command.Broker
	// ConvToken maps the mcphost session key — resolved server-side
	// from the connection token — to the Broker's opaque conversation
	// token. They are not always the same string: zulip-acp keys
	// sessions by conv-id but brokers by conversation KEY, precisely
	// so `!new` can replace the former without invalidating the
	// latter. Returning ok=false rejects the call, which is what
	// happens when the conversation has since been retired.
	//
	// Nil means the two are identical, which is the simple case.
	ConvToken func(sessionKey string) (convToken string, ok bool)
	// Logf receives operational messages.
	Logf func(format string, args ...any)
}

// Tool is one registered tool, kept as data so the set can be built and
// asserted on without a socket, a subprocess or an MCP handshake.
type Tool struct {
	Name        string
	Description string
	Schema      map[string]any
	Handler     mcphost.Handler
}

// Tools is the registered tool set plus the deferred-action state that
// new_session needs.
type Tools struct {
	cfg Config

	mu      sync.Mutex
	pending map[string]bool // conv token -> reset requested this turn
}

// New constructs a Tools.
func New(cfg Config) (*Tools, error) {
	if cfg.Broker == nil {
		return nil, errors.New("relaytool: Broker is required")
	}
	if cfg.ConvToken == nil {
		cfg.ConvToken = func(k string) (string, bool) { return k, true }
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	return &Tools{cfg: cfg, pending: map[string]bool{}}, nil
}

// Register installs the tool set on h, in Tools order.
func (t *Tools) Register(h *mcphost.Host) {
	for _, x := range t.Tools() {
		h.Tool(x.Name, x.Description, x.Schema, x.Handler)
	}
}

// Tools builds the tool set as data, so it can be exercised without a
// socket, a subprocess or an MCP handshake.
//
// Capability-dependent tools are included only when the relay can
// actually perform them: advertising `post` on a relay with no way to
// speak out of band would be a tool that exists solely to fail.
func (t *Tools) Tools() []Tool {
	out := []Tool{{
		Name: ToolStatus,
		Description: "Report this conversation's relay status: current model, session, working directory, " +
			"relay version and uptime, and how many prompts are scheduled here.",
		Schema: objectSchema(nil),
		Handler: t.wrap(func(conv string, _ json.RawMessage) (string, error) {
			return t.cfg.Broker.Status(conv)
		}),
	}, {
		Name:        ToolListModels,
		Description: "List the models the agent process can switch to, optionally filtered by substring.",
		Schema: objectSchema(map[string]any{
			"filter": strProp("Case-insensitive substring to narrow the list. Omit for all models."),
		}),
		Handler: t.wrap(func(conv string, args json.RawMessage) (string, error) {
			var a struct {
				Filter string `json:"filter"`
			}
			if err := decode(args, &a); err != nil {
				return "", err
			}
			return t.cfg.Broker.ModelList(a.Filter)
		}),
	}, {
		Name: ToolSetModel,
		Description: "Switch this conversation to a different model. Takes effect from the next turn, " +
			"not the one running now.",
		Schema: objectSchemaReq(map[string]any{
			"model_id": strProp("Exact model id, as returned by list_models."),
		}, "model_id"),
		Handler: t.wrap(func(conv string, args json.RawMessage) (string, error) {
			var a struct {
				ModelID string `json:"model_id"`
			}
			if err := decode(args, &a); err != nil {
				return "", err
			}
			if a.ModelID == "" {
				return "", errors.New("model_id is required")
			}
			if err := t.cfg.Broker.SelectModel(conv, a.ModelID); err != nil {
				return "", err
			}
			return "Model set to " + a.ModelID + " for this conversation, from the next turn.", nil
		}),
	}, {
		Name: ToolNewSession,
		Description: "Clear this conversation's context and start a fresh session. DEFERRED: it is applied " +
			"once the current turn ends, so this turn finishes normally and the next one starts clean.",
		Schema: objectSchema(nil),
		Handler: t.wrap(func(conv string, _ json.RawMessage) (string, error) {
			t.mu.Lock()
			t.pending[conv] = true
			t.mu.Unlock()
			return "A fresh session is armed: this turn finishes as usual, then the context is cleared.", nil
		}),
	}}

	if t.cfg.Broker.CanPost() {
		out = append(out, Tool{
			Name: ToolPost,
			Description: "Send a message into this conversation right now, outside the answer you are streaming. " +
				"Use it for progress on a long task, or to report a result that arrives after the turn " +
				"that started it has ended. It always posts here — there is no way to address anywhere else.",
			Schema: objectSchemaReq(map[string]any{
				"text": strProp("Message body, in the chat's markdown."),
			}, "text"),
			Handler: t.wrap(func(conv string, args json.RawMessage) (string, error) {
				var a struct {
					Text string `json:"text"`
				}
				if err := decode(args, &a); err != nil {
					return "", err
				}
				if err := t.cfg.Broker.Post(conv, a.Text); err != nil {
					return "", err
				}
				return "Posted to this conversation.", nil
			}),
		})
	}

	if t.cfg.Broker.CanSchedule() {
		out = append(out, t.scheduleTools()...)
	}
	return out
}

// scheduleTools is the scheduling trio.
func (t *Tools) scheduleTools() []Tool {
	return []Tool{{
		Name: ToolSchedule,
		Description: "Schedule a prompt to be sent to you later IN THIS CONVERSATION, with its full history. " +
			"On fire it starts a normal turn whose answer is posted here. Use it for conversation-scoped " +
			"follow-ups (\"check whether that deploy landed in 20 minutes\"), not for host chores.",
		Schema: objectSchemaReq(map[string]any{
			"text":  strProp("The prompt to send yourself when it fires."),
			"in":    strProp("Delay from now as a Go duration, e.g. \"45m\", \"2h30m\". Use this or at."),
			"at":    strProp("Absolute RFC3339 time, e.g. \"2026-09-02T09:00:00Z\". Use this or in."),
			"every": strProp("Optional repeat interval as a Go duration, e.g. \"24h\". Omit for one-shot."),
		}, "text"),
		Handler: t.wrap(func(conv string, args json.RawMessage) (string, error) {
			var a struct {
				Text  string `json:"text"`
				In    string `json:"in"`
				At    string `json:"at"`
				Every string `json:"every"`
			}
			if err := decode(args, &a); err != nil {
				return "", err
			}
			at, err := parseWhen(a.In, a.At)
			if err != nil {
				return "", err
			}
			var every time.Duration
			if a.Every != "" {
				if every, err = time.ParseDuration(a.Every); err != nil {
					return "", fmt.Errorf("every: %w", err)
				}
			}
			it, err := t.cfg.Broker.Schedule(conv, strings.TrimSpace(a.Text), at, every)
			if err != nil {
				return "", err
			}
			msg := fmt.Sprintf("Scheduled %s for %s", it.ID, it.At.UTC().Format(time.RFC3339))
			if it.Every > 0 {
				msg += ", repeating every " + it.Every.String()
			}
			return msg + ".", nil
		}),
	}, {
		Name:        ToolListSchedules,
		Description: "List the prompts scheduled in this conversation, soonest first.",
		Schema:      objectSchema(nil),
		Handler: t.wrap(func(conv string, _ json.RawMessage) (string, error) {
			items, err := t.cfg.Broker.ScheduleList(conv)
			if err != nil {
				return "", err
			}
			return command.RenderSchedules(items), nil
		}),
	}, {
		Name:        ToolUnschedule,
		Description: "Cancel a scheduled prompt in this conversation by id.",
		Schema: objectSchemaReq(map[string]any{
			"id": strProp("Schedule id, as returned by schedule or list_schedules."),
		}, "id"),
		Handler: t.wrap(func(conv string, args json.RawMessage) (string, error) {
			var a struct {
				ID string `json:"id"`
			}
			if err := decode(args, &a); err != nil {
				return "", err
			}
			if err := t.cfg.Broker.Unschedule(conv, a.ID); err != nil {
				return "", err
			}
			return "Cancelled " + a.ID + ".", nil
		}),
	}}
}

// EndTurn applies whatever the agent deferred during the turn that has
// just finished in convToken. The relay calls it once per completed
// turn, AFTER the answer has been posted.
//
// Today that is only new_session. It is a method rather than a callback
// so the relay's turn path has exactly one thing to remember.
func (t *Tools) EndTurn(convToken string) {
	t.mu.Lock()
	want := t.pending[convToken]
	delete(t.pending, convToken)
	t.mu.Unlock()
	if !want {
		return
	}
	if err := t.cfg.Broker.NewSession(convToken); err != nil {
		t.cfg.Logf("relaytool: deferred new session: %v", err)
		return
	}
	t.cfg.Logf("relaytool: fresh session applied at the agent's request")
}

// wrap resolves the mcphost session key to a broker conversation token
// before the handler runs, so no tool body ever sees a raw session key
// and no tool body can be written that takes a conversation as an
// argument.
func (t *Tools) wrap(fn func(conv string, args json.RawMessage) (string, error)) mcphost.Handler {
	return func(sessionKey string, args json.RawMessage) (string, error) {
		conv, ok := t.cfg.ConvToken(sessionKey)
		if !ok {
			return "", errors.New("this conversation is no longer active")
		}
		return fn(conv, args)
	}
}

// parseWhen resolves the `in` / `at` pair into an absolute time.
// Exactly one must be given: accepting both and picking a winner would
// silently ignore half of what the agent asked for.
func parseWhen(in, at string) (time.Time, error) {
	switch {
	case in != "" && at != "":
		return time.Time{}, errors.New("pass either in or at, not both")
	case in != "":
		d, err := time.ParseDuration(in)
		if err != nil {
			return time.Time{}, fmt.Errorf("in: %w", err)
		}
		return time.Now().Add(d), nil
	case at != "":
		ts, err := time.Parse(time.RFC3339, at)
		if err != nil {
			return time.Time{}, fmt.Errorf("at: %w", err)
		}
		return ts, nil
	default:
		return time.Time{}, errors.New("pass in (a duration) or at (an RFC3339 time)")
	}
}

// decode unmarshals a tool's arguments, turning a decode failure into
// the message the agent sees.
func decode(args json.RawMessage, v any) error {
	if len(args) == 0 {
		return nil // a no-argument call may send nothing at all
	}
	if err := json.Unmarshal(args, v); err != nil {
		return errors.New("invalid params: " + err.Error())
	}
	return nil
}

// strProp is a string property with a description.
func strProp(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

// objectSchema builds an object schema with no required properties.
func objectSchema(props map[string]any) map[string]any {
	if props == nil {
		props = map[string]any{}
	}
	return map[string]any{"type": "object", "properties": props}
}

// objectSchemaReq builds an object schema with required properties.
func objectSchemaReq(props map[string]any, required ...string) map[string]any {
	s := objectSchema(props)
	s["required"] = required
	return s
}
