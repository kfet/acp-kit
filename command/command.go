// Package command is the shared chat-command surface every relay puts
// in front of an ACP agent: it brokers interactive OAuth login
// (bridging one chat turn into the agent's _meta.auth.interactive
// two-call protocol) and the session-control commands (!status,
// !model, !new, !stop, !help).
//
// The user-facing surface is deliberately small: !help, !status,
// !model [filter|id], !new, !stop, !login [provider|cancel]. Older
// spellings (!models, !relay, !bot, !cancel-login, !whoami, !reset)
// still work as undocumented aliases so nothing a user has learned
// ever breaks.
//
// # What lives here and what does not
//
// Everything surface-independent: sigil handling, command
// classification, the login state machine, the passthrough allowlist,
// and the rendering — which stays shared because Poe, Zulip and Slack
// all read CommonMark-ish markdown and the strings are legal in all
// three verbatim. Duplicating identical prose per relay would recreate
// exactly the fork this package exists to remove. When a surface
// genuinely needs different markup, THAT is the moment to introduce a
// renderer interface, and not before.
//
// What does not live here: anything that knows how a message arrives
// or is posted. The relay owns delivery, gating (who may speak to the
// bot), and any surface-specific pre-filter — e.g. zulip-acp must let
// Zulip's own /me, /poll and /todo widgets through untouched before
// the broker ever sees them, using the exported StripSigil helper.
//
// # Login
//
// Each conversation can have at most one in-flight login. The first
// login command (e.g. "!login anthropic") calls the agent's
// authenticate to produce a URL; the next user turn from the same
// conversation submits the pasted redirect URL. The broker holds no
// goroutines — the actual blocking on user input happens inside the
// agent, parked across turns. The relay only remembers which
// conversation has a pending login for which method.
//
// # Sigils
//
// Commands accept the sigils "/", "!", and "." but user-facing prose
// suggests "!" (DisplaySigil). Poe's chat client intercepts
// "/"-prefixed messages as native slash commands and rejects unknown
// ones before they reach the bot; Zulip's slash commands are handled
// client-side against /json/command and never reach a bot at all,
// while /me, /poll and /todo DO arrive as messages or widgets. So "/"
// is unsafe to advertise on either surface, and "!"/"." pass straight
// through on both.
package command

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/kfet/acp-kit/client"
)

// Authenticator is the agent-side surface the broker depends on. The full
// *client.AgentProc satisfies it.
type Authenticator interface {
	AuthMethods() []client.AuthMethod
	Authenticate(ctx context.Context, methodID, id, redirect string, cancel bool) (client.AuthResult, error)
}

// Controller is the per-conversation session-control surface used by
// the non-auth commands (!status, !model, !new). Each relay implements
// it over whatever it uses for session state. Optional: if nil, those
// commands report unavailable. The dependency edge is one-way — a
// relay's session layer never imports this package.
//
// convID is an OPAQUE, relay-chosen conversation token. The broker
// only ever hands it back; it never parses it and never assumes it is
// stable. Implementations are free to re-key the underlying session or
// identity behind it — zulip-acp's ResetSession retires a journal entry
// and allocates a fresh conversation id, which the broker neither sees
// nor needs to.
type Controller interface {
	AvailableModels() (models []client.ModelInfo, currentID string)
	SetModelOverride(convID, modelID string) error
	ResetSession(convID string) error
	StatusFor(convID string) SessionStatus
	AgentCommands() []client.CommandInfo
	RelayInfo(convID string) RelayInfo
}

// TurnStopper is the OPTIONAL capability behind `!stop`. A Controller
// that implements it enables the command; one that does not leaves
// `!stop` unrecognised, so the text forwards to the agent as ordinary
// prose exactly as before.
//
// That conditionality is the point. Only a relay that streams a turn
// has anything to interrupt: poe-acp answers one HTTP request per turn
// and has no in-flight turn a later message could reach, while
// zulip-acp streams into an editable message and very much does.
// Advertising a command that cannot work on half the relays would be
// worse than not having it.
//
// StopTurn reports whether it actually stopped something, so the reply
// can tell interrupting from doing nothing.
type TurnStopper interface {
	StopTurn(convID string) bool
}

// stopper returns the Controller's TurnStopper capability, if it has
// one.
func (b *Broker) stopper() (TurnStopper, bool) {
	if b.ctrl == nil {
		return nil, false
	}
	ts, ok := b.ctrl.(TurnStopper)
	return ts, ok
}

// passthroughAllow is the curated set of agent-advertised commands the
// relay is willing to forward as chat commands (`!reload` → `/reload`).
// Kept deliberately small and safe: read-only, non-destructive, or
// explicitly account-scoped (logout) or config-reload (mcp) operations the user invokes
// directly. Commands outside this set never reach the agent via the
// command surface (the user's literal text still does).
var passthroughAllow = map[string]bool{
	"reload":    true,
	"logout":    true,
	"compact":   true,
	"session":   true,
	"changelog": true,
	"mcp":       true,
	"skills":    true,
}

// Deliberately NOT allowlisted, and not an oversight:
//
//   - resume, continue — the relay owns the conversation→session mapping.
//     Letting the agent switch its own session underneath the relay would
//     desync that mapping (the relay would keep prompting into a session
//     the agent has moved on from). !new is the supported way to change
//     session state. Do not "fix" this by adding them here.
//   - name, share, export — side-effecting or account/artefact-scoped
//     operations that make no sense driven from a Poe chat turn.

// SessionStatus is a race-free snapshot of a conversation's relay state.
// Every field is optional: the renderer prints only what is set, so a
// relay reports what it actually knows rather than padding the rest
// with plausible-looking blanks.
type SessionStatus struct {
	EffectiveModel  string // override if set, else the configured default
	DefaultModel    string
	OverrideModel   string // "" when no !model override is active
	Thinking        string
	HasSession      bool
	ModelsAvailable int

	// ConvID is the relay's own identifier for this conversation,
	// shown so a human can find its state on disk. Empty when the
	// relay has no such notion, or has not allocated one yet.
	ConvID string
	// StateDir is the conversation's working directory. Empty when the
	// relay does not give a conversation its own directory.
	StateDir string
	// Where renders the conversation in human terms — "#fleet >
	// \"hacking\"", "DM with Kfet". Empty when the surface has only one
	// place for a conversation to be.
	Where string
	// TurnRunning reports whether a turn is in flight right now. Only
	// meaningful on a relay that can tell; see TurnStopper.
	TurnRunning bool
}

// RelayInfo is a snapshot of relay-process realtime state, surfaced by
// the !relay chat command.
type RelayInfo struct {
	Version         string
	Uptime          string // pre-formatted (e.g. "3h2m1s"); "" if unknown
	AgentCmd        string
	ModelsAvailable int
	ActiveSessions  int    // live conv sessions tracked by the router
	SessionID       string // this conv's live agent session id; "" if none
	EffectiveModel  string // override if set, else configured default
}

// Broker tracks per-conversation pending logins.
type Broker struct {
	a    Authenticator
	ctrl Controller // optional; set via SetController for session commands

	mu      sync.Mutex
	pending map[string]pendingEntry // convID → in-flight login
}

// SetController wires the session-control surface (the router) used by
// !status/!model/!new. Call once at startup, after construction,
// to break the broker↔router construction cycle. Safe before any turns.
func (b *Broker) SetController(c Controller) { b.ctrl = c }

// Passthrough decides whether text is an allowlisted agent command and,
// if so, returns the prompt text to forward to the agent (e.g. "!reload"
// → "/reload"). ok=false means the turn is not a passthrough command and
// should be handled normally. The command must be both allowlisted and
// actually advertised by the agent. Requires a wired Controller.
func (b *Broker) Passthrough(text string) (rewritten string, ok bool) {
	if b.ctrl == nil {
		return "", false
	}
	body, has := stripSigil(strings.TrimSpace(text))
	if !has || body == "" {
		return "", false
	}
	body = foldVerb(body)
	name := body
	if i := strings.IndexByte(body, ' '); i >= 0 {
		name = body[:i]
	}
	if !passthroughAllow[name] || !b.agentHasCommand(name) {
		return "", false
	}
	return "/" + body, true
}

// agentHasCommand reports whether the agent currently advertises name.
func (b *Broker) agentHasCommand(name string) bool {
	for _, c := range b.ctrl.AgentCommands() {
		if c.Name == name {
			return true
		}
	}
	return false
}

// passthroughCommands returns the allowlisted commands the agent
// currently advertises, for display in !help.
func (b *Broker) passthroughCommands() []client.CommandInfo {
	if b.ctrl == nil {
		return nil
	}
	var out []client.CommandInfo
	for _, c := range b.ctrl.AgentCommands() {
		if passthroughAllow[c.Name] {
			out = append(out, c)
		}
	}
	return out
}

// pendingEntry is the per-conversation in-flight login state.
type pendingEntry struct {
	methodID string
	// authID is the opaque id returned by the agent on call 1; passed
	// back on call 2 / cancel so the agent can disambiguate concurrent
	// pending logins for the same methodID.
	authID string
}

// New constructs a Broker.
func New(a Authenticator) *Broker {
	return &Broker{a: a, pending: make(map[string]pendingEntry)}
}

// HasPending reports whether the conversation is waiting for a pasted
// redirect URL.
func (b *Broker) HasPending(convID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.pending[convID]
	return ok
}

// commandSigils are the leading characters that introduce a relay
// command. Poe's chat client intercepts "/"-prefixed messages as native
// slash commands and rejects unknown ones before they ever reach the
// bot, so a bare "/login" usually never arrives (and is flaky when it
// does — see docs). "!" and "." pass straight through untouched. We
// accept all three on input and echo DisplaySigil in user-facing prose.
const commandSigils = "/!."

// DisplaySigil is the command prefix shown in user-facing messages. It
// is deliberately not "/" so the suggested commands survive Poe's
// client-side slash-command interceptor.
const DisplaySigil = "!"

// StripSigil removes a single leading command sigil from t and reports
// whether one was present. Exported for relays that must apply a
// surface-specific policy BEFORE the broker sees a message — e.g.
// zulip-acp has to recognise Zulip's own /me, /poll and /todo, which
// unlike Zulip's client-side slash commands do arrive as real messages
// and widgets, and pass them through untouched.
//
// Whitespace is trimmed here, so callers need not.
func StripSigil(t string) (body string, ok bool) {
	return stripSigil(strings.TrimSpace(t))
}

// stripSigil removes a single leading command sigil from t (which must
// already be TrimSpace'd) and reports whether one was present.
func stripSigil(t string) (body string, ok bool) {
	if t == "" {
		return t, false
	}
	if strings.IndexByte(commandSigils, t[0]) >= 0 {
		return t[1:], true
	}
	return t, false
}

// foldVerb lower-cases the command WORD of a sigil-stripped body and
// leaves its argument exactly as typed.
//
// Command names are case-insensitive — "!NEW" and "!New" are "!new" —
// because a phone keyboard capitalises the first letter of a message
// by default, which would otherwise make every command typed at the
// start of a line silently fail. The argument must NOT be folded: a
// model id like "anthropic/Claude-Opus" is a literal the agent has to
// match, and case-folding it would break the one command that takes an
// id.
func foldVerb(body string) string {
	i := strings.IndexByte(body, ' ')
	if i < 0 {
		return strings.ToLower(body)
	}
	return strings.ToLower(body[:i]) + body[i:]
}

// isLoginBody reports whether a sigil-stripped command body is one of the
// login-family commands.
func isLoginBody(body string) bool {
	return body == "login" || strings.HasPrefix(body, "login ") ||
		body == "logins" || // alias for "list"
		body == "cancel-login"
}

// isCancelLoginBody reports whether a sigil-stripped body aborts an
// in-flight login: the documented `login cancel`, or the undocumented
// `cancel-login` alias kept for backwards compatibility.
func isCancelLoginBody(body string) bool {
	if body == "cancel-login" {
		return true
	}
	rest, ok := strings.CutPrefix(body, "login ")
	return ok && strings.TrimSpace(rest) == "cancel"
}

// IsLoginCommand reports whether text is a login/logins/cancel-login
// command under any accepted sigil (/, !, .). Trims leading whitespace
// so users can paste the command after a thought.
func IsLoginCommand(text string) bool {
	body, ok := stripSigil(strings.TrimSpace(text))
	if !ok {
		return false
	}
	return isLoginBody(foldVerb(body))
}

// isSessionBody reports whether a sigil-stripped body is one of the
// session-control commands (handled only when a Controller is wired).
//
// `stop` is conditional on the TurnStopper capability — see there. Note
// the spelling: `cancel` is NOT an alias for it, because `!login
// cancel` and the older `!cancel-login` already own that word. A
// `!cancel` that sometimes aborted a login and sometimes killed a turn
// would be the worst possible ambiguity in the one command a user
// reaches for when something has gone wrong.
func (b *Broker) isSessionBody(body string) bool {
	if body == "stop" {
		_, ok := b.stopper()
		return ok
	}
	switch {
	case body == "status" || body == "whoami":
		return true
	case body == "relay" || body == "bot":
		return true
	case body == "models" || strings.HasPrefix(body, "models "):
		return true
	case body == "model" || strings.HasPrefix(body, "model "):
		return true
	case body == "new" || body == "reset":
		return true
	}
	return false
}

// IsCommand reports whether text is any relay command THIS broker
// handles (the login family, "help", or a session command), under any
// accepted sigil. A relay uses it to decide whether to route a turn to
// the broker instead of forwarding it to the agent.
//
// It is a method rather than a package function because the answer
// depends on the wired Controller: `!stop` is a command only where the
// relay can actually stop a turn.
func (b *Broker) IsCommand(text string) bool {
	body, ok := stripSigil(strings.TrimSpace(text))
	if !ok {
		return false
	}
	body = foldVerb(body)
	return body == "help" || isLoginBody(body) || b.isSessionBody(body)
}

// Outcome is what the HTTP handler should render to the user. Exactly
// one of Text / URL / Error is set; Done is always implied (the auth
// turn never streams further chunks).
type Outcome struct {
	// Text is a plain message to stream as a Poe `text` event.
	Text string
	// URL, if non-empty, is the auth URL to surface to the user. Text
	// may also be set with prose preceding/following the URL.
	URL string
	// Instructions accompany URL.
	Instructions string
}

// Handle dispatches one user turn. Behaviour:
//
//  1. If the conversation has a pending login, treat any non-command
//     text as the pasted redirect URL.
//  2. Otherwise interpret !login [provider|cancel] (and the older
//     !logins / !cancel-login spellings).
//
// Returns (nil, nil) if the turn is not auth-related and should be
// forwarded to the normal prompt path.
func (b *Broker) Handle(ctx context.Context, convID, text string) (*Outcome, error) {
	t := strings.TrimSpace(text)
	body, hasSigil := stripSigil(t)
	body = foldVerb(body)

	// Help is stateless and never collides with a pasted redirect URL
	// (those never carry a sigil), so it wins even mid-login.
	if hasSigil && body == "help" {
		return b.help(), nil
	}

	// Pasted redirect URL for an in-flight login wins over command parsing.
	if entry, ok := b.peek(convID); ok {
		if hasSigil && isCancelLoginBody(body) {
			return b.cancel(ctx, convID, entry)
		}
		return b.complete(ctx, convID, entry, t)
	}

	if !hasSigil {
		return nil, nil
	}

	switch {
	case isCancelLoginBody(body):
		return &Outcome{Text: "No login in progress."}, nil
	case body == "login" || body == "logins":
		return b.list(), nil
	case strings.HasPrefix(body, "login "):
		// body has a provider arg: "login <provider>".
		rest := strings.TrimSpace(strings.TrimPrefix(body, "login"))
		return b.start(ctx, convID, rest)
	case body == "relay" || body == "bot":
		// Undocumented aliases: !relay folded into !status.
		return b.status(convID), nil
	case body == "status" || body == "whoami":
		return b.status(convID), nil
	case body == "models" || strings.HasPrefix(body, "models "):
		// Undocumented alias: !models folded into !model.
		return b.models(strings.TrimSpace(strings.TrimPrefix(body, "models"))), nil
	case body == "model" || strings.HasPrefix(body, "model "):
		return b.model(convID, strings.TrimSpace(strings.TrimPrefix(body, "model"))), nil
	case body == "new" || body == "reset":
		return b.reset(convID), nil
	case body == "stop":
		return b.stop(convID), nil
	default:
		// Sigil-prefixed but not a login command — not ours.
		return nil, nil
	}
}

// OfferLogin renders the onboarding message shown when the agent reports
// that no usable provider is connected (an "Authentication required"
// prompt error). It lists the loginable providers using the Poe-safe
// DisplaySigil. Safe for concurrent use. Returned text is empty-safe:
// it always yields actionable guidance, even with no OAuth methods.
func (b *Broker) OfferLogin() string {
	loginable := filterLoginable(b.a.AuthMethods())
	if len(loginable) == 0 {
		return "⚠️ This bot has no LLM provider connected, and the agent " +
			"advertises no interactive login methods. Set a provider API key " +
			"in the agent's environment (e.g. `ANTHROPIC_API_KEY`) and restart."
	}
	var sb strings.Builder
	sb.WriteString("⚠️ No LLM provider is connected yet, so I can't answer. " +
		"Connect one by sending one of these (the leading `" + DisplaySigil +
		"` matters — Poe swallows a leading `/`):\n\n")
	for _, m := range loginable {
		shortID := strings.TrimPrefix(m.ID, "oauth-")
		fmt.Fprintf(&sb, "- `%slogin %s` — %s\n", DisplaySigil, shortID, m.Name)
	}
	sb.WriteString("\nThen open the URL I reply with, authenticate, and paste " +
		"the page's URL back here to finish.")
	return sb.String()
}

// help lists the relay commands the broker understands.
func (b *Broker) help() *Outcome {
	s := DisplaySigil
	var sb strings.Builder
	sb.WriteString("Available commands:\n\n")
	sb.WriteString("- `" + s + "help` — show this message\n")
	if b.ctrl != nil {
		sb.WriteString("- `" + s + "status` — model, session and relay info\n")
		sb.WriteString("- `" + s + "model [filter|id]` — list/filter models, or switch\n")
		sb.WriteString("- `" + s + "new` — start a fresh session (clears context)\n")
		if _, ok := b.stopper(); ok {
			sb.WriteString("- `" + s + "stop` — interrupt the turn currently running\n")
		}
	}
	sb.WriteString("- `" + s + "login [provider|cancel]` — connect a provider (e.g. `" + s +
		"login anthropic`), or abort a login in progress\n")
	if pt := b.passthroughCommands(); len(pt) > 0 {
		sb.WriteString("\nAgent commands:\n\n")
		for _, c := range pt {
			fmt.Fprintf(&sb, "- `%s%s`", s, c.Name)
			if c.Description != "" {
				fmt.Fprintf(&sb, " — %s", c.Description)
			}
			sb.WriteString("\n")
		}
	}
	return &Outcome{Text: sb.String()}
}

// status renders one compact snapshot of everything the user might ask
// about: the conversation's model / thinking / session (the old !status)
// plus the relay process's version / uptime / sessions (the old !relay).
// Rendered as a short bullet list — it has to read well on a phone, so
// no tables and no wide lines; conversation state first, relay after.
func (b *Broker) status(convID string) *Outcome {
	if b.ctrl == nil {
		return &Outcome{Text: "Session control is unavailable."}
	}
	st := b.ctrl.StatusFor(convID)
	ri := b.ctrl.RelayInfo(convID)
	var sb strings.Builder
	sb.WriteString("**Status**\n\n")
	if st.Where != "" {
		fmt.Fprintf(&sb, "- here: %s\n", st.Where)
	}
	fmt.Fprintf(&sb, "- model: `%s`", st.EffectiveModel)
	if st.OverrideModel != "" {
		fmt.Fprintf(&sb, " (set via %smodel)", DisplaySigil)
	}
	sb.WriteString("\n")
	if st.Thinking != "" {
		fmt.Fprintf(&sb, "- thinking: %s\n", st.Thinking)
	}
	sess := "none yet (fresh on next message)"
	if st.HasSession {
		sess = "active"
	}
	if ri.SessionID != "" {
		sess += " `" + ri.SessionID + "`"
	}
	fmt.Fprintf(&sb, "- session: %s\n", sess)
	if st.ConvID != "" {
		fmt.Fprintf(&sb, "- conversation: `%s`\n", st.ConvID)
	}
	if st.StateDir != "" {
		fmt.Fprintf(&sb, "- state dir: `%s`\n", st.StateDir)
	}
	if st.TurnRunning {
		fmt.Fprintf(&sb, "- turn running: yes — `%sstop` interrupts it\n", DisplaySigil)
	}
	fmt.Fprintf(&sb, "- models available: %d\n", st.ModelsAvailable)
	if ri.Version != "" {
		fmt.Fprintf(&sb, "- relay: `%s`\n", ri.Version)
	}
	if ri.Uptime != "" {
		fmt.Fprintf(&sb, "- uptime: %s\n", ri.Uptime)
	}
	if ri.AgentCmd != "" {
		fmt.Fprintf(&sb, "- agent: `%s`\n", ri.AgentCmd)
	}
	fmt.Fprintf(&sb, "- active conversations: %d\n", ri.ActiveSessions)
	return &Outcome{Text: sb.String()}
}

// modelsListCap bounds how many models a model listing prints in one
// message.
const modelsListCap = 40

// models lists available model ids, optionally filtered by substring.
// It backs bare `!model`, `!model <filter>` and the undocumented
// `!models [filter]` alias.
func (b *Broker) models(filter string) *Outcome {
	if b.ctrl == nil {
		return &Outcome{Text: "Session control is unavailable."}
	}
	all, current := b.ctrl.AvailableModels()
	if len(all) == 0 {
		return &Outcome{Text: fmt.Sprintf("No models available — connect a provider with `%slogin`.", DisplaySigil)}
	}
	f := strings.ToLower(filter)
	matched := make([]client.ModelInfo, 0, len(all))
	for _, m := range all {
		if f == "" || strings.Contains(strings.ToLower(m.ID), f) {
			matched = append(matched, m)
		}
	}
	var sb strings.Builder
	if filter == "" {
		fmt.Fprintf(&sb, "%d models available (current: `%s`). `%smodel <id>` to switch, `%smodel <filter>` to narrow:\n\n",
			len(all), current, DisplaySigil, DisplaySigil)
	} else {
		fmt.Fprintf(&sb, "%d model(s) match %q (current: `%s`):\n\n", len(matched), filter, current)
	}
	if len(matched) == 0 {
		fmt.Fprintf(&sb, "(none match %q — try `%smodel` for the full list)\n", filter, DisplaySigil)
		return &Outcome{Text: sb.String()}
	}
	for i, m := range matched {
		if i >= modelsListCap {
			fmt.Fprintf(&sb, "…and %d more (filter to narrow).\n", len(matched)-modelsListCap)
			break
		}
		marker := ""
		if m.ID == current {
			marker = " ←"
		}
		fmt.Fprintf(&sb, "- `%s`%s\n", m.ID, marker)
	}
	return &Outcome{Text: sb.String()}
}

// model implements `!model [filter|id]`:
//
//	no arg          → list every available model
//	arg == model id → switch this chat to it
//	anything else   → treat the arg as a list filter
//
// Exact-id match wins over the filter reading, so a model id that also
// happens to be a substring of other ids still switches. A filter that
// narrows to exactly one model deliberately does NOT auto-switch —
// silently changing model off an approximate match would be surprising.
func (b *Broker) model(convID, arg string) *Outcome {
	if b.ctrl == nil {
		return &Outcome{Text: "Session control is unavailable."}
	}
	if arg == "" {
		return b.models("")
	}
	all, _ := b.ctrl.AvailableModels()
	for _, m := range all {
		if m.ID == arg {
			return b.setModel(convID, arg)
		}
	}
	return b.models(arg)
}

// setModel switches the sticky model for the conversation.
func (b *Broker) setModel(convID, id string) *Outcome {
	if err := b.ctrl.SetModelOverride(convID, id); err != nil {
		return &Outcome{Text: fmt.Sprintf("❌ %v. Use `%smodel` to see available ids.", err, DisplaySigil)}
	}
	return &Outcome{Text: fmt.Sprintf("✅ Model set to `%s` for this chat — applies from your next message.", id)}
}

// reset drops the conversation's live session so the next turn is fresh.
func (b *Broker) reset(convID string) *Outcome {
	if b.ctrl == nil {
		return &Outcome{Text: "Session control is unavailable."}
	}
	if err := b.ctrl.ResetSession(convID); err != nil {
		return &Outcome{Text: fmt.Sprintf("Couldn't reset: %v.", err)}
	}
	return &Outcome{Text: "🧹 Fresh session — previous context cleared. Your model choice is kept."}
}

// stop interrupts the turn currently running for the conversation.
//
// Unreachable unless the Controller implements TurnStopper, because
// IsCommand refuses to classify `!stop` as a command otherwise and
// Handle is only reached for recognised commands.
func (b *Broker) stop(convID string) *Outcome {
	ts, ok := b.stopper()
	if !ok {
		return &Outcome{Text: "Session control is unavailable."}
	}
	if !ts.StopTurn(convID) {
		return &Outcome{Text: "Nothing is running here."}
	}
	return &Outcome{Text: "🛑 Interrupted."}
}

// list renders the available login methods.
func (b *Broker) list() *Outcome {
	methods := b.a.AuthMethods()
	loginable := filterLoginable(methods)
	if len(loginable) == 0 {
		return &Outcome{Text: "No OAuth login methods available."}
	}
	var sb strings.Builder
	sb.WriteString("Available login methods:\n\n")
	for _, m := range loginable {
		shortID := strings.TrimPrefix(m.ID, "oauth-")
		fmt.Fprintf(&sb, "- `%slogin %s` — %s", DisplaySigil, shortID, m.Name)
		if m.Description != "" {
			fmt.Fprintf(&sb, " (%s)", m.Description)
		}
		sb.WriteString("\n")
	}
	return &Outcome{Text: sb.String()}
}

// start initiates a login.
func (b *Broker) start(ctx context.Context, convID, provider string) (*Outcome, error) {
	methodID, err := b.resolveMethodID(provider)
	if err != nil {
		return &Outcome{Text: err.Error()}, nil
	}

	// Refuse a second concurrent login for this conv.
	b.mu.Lock()
	if existing, ok := b.pending[convID]; ok {
		b.mu.Unlock()
		return &Outcome{Text: fmt.Sprintf("A login is already in progress (%s). Paste the redirect URL or send `%slogin cancel`.", existing.methodID, DisplaySigil)}, nil
	}
	b.mu.Unlock()

	res, err := b.a.Authenticate(ctx, methodID, "", "", false)
	if err != nil {
		return nil, fmt.Errorf("authenticate %s: %w", methodID, err)
	}
	switch res.State {
	case "needs_redirect":
		if res.ID == "" {
			// Agent supports the protocol but didn't return an id —
			// fall back to single-pending-per-method semantics by
			// passing "" on call 2. This still works against older
			// fir builds.
		}
		b.mu.Lock()
		b.pending[convID] = pendingEntry{methodID: methodID, authID: res.ID}
		b.mu.Unlock()
		text := fmt.Sprintf("Open this URL to authenticate, then paste the URL of the page you land on (even if it fails to load):\n\n%s\n", res.URL)
		if res.Instructions != "" {
			text += "\n" + res.Instructions + "\n"
		}
		return &Outcome{Text: text, URL: res.URL, Instructions: res.Instructions}, nil
	case "ok", "":
		return &Outcome{Text: fmt.Sprintf("✅ Already authenticated (%s).", methodID)}, nil
	case "cancelled":
		return &Outcome{Text: "Login cancelled."}, nil
	default:
		return &Outcome{Text: fmt.Sprintf("Login returned unexpected state: %q.", res.State)}, nil
	}
}

// complete submits the pasted redirect URL.
func (b *Broker) complete(ctx context.Context, convID string, entry pendingEntry, redirect string) (*Outcome, error) {
	if redirect == "" {
		return &Outcome{Text: fmt.Sprintf("Empty paste — send the redirect URL or `%slogin cancel`.", DisplaySigil)}, nil
	}
	res, err := b.a.Authenticate(ctx, entry.methodID, entry.authID, redirect, false)
	// Always drop the pending entry — a failed paste means the user must
	// start over (matches the TUI: a bad paste aborts and prints the error).
	b.mu.Lock()
	delete(b.pending, convID)
	b.mu.Unlock()
	if err != nil {
		return &Outcome{Text: fmt.Sprintf("❌ Login failed: %v\n\nSend `%slogin %s` to try again.", err, DisplaySigil, strings.TrimPrefix(entry.methodID, "oauth-"))}, nil
	}
	switch res.State {
	case "ok", "":
		return &Outcome{Text: fmt.Sprintf("✅ Authenticated (%s).", entry.methodID)}, nil
	case "cancelled":
		return &Outcome{Text: "Login cancelled."}, nil
	default:
		return &Outcome{Text: fmt.Sprintf("Login returned unexpected state: %q.", res.State)}, nil
	}
}

// cancel cancels an in-flight login.
func (b *Broker) cancel(ctx context.Context, convID string, entry pendingEntry) (*Outcome, error) {
	b.mu.Lock()
	delete(b.pending, convID)
	b.mu.Unlock()
	if _, err := b.a.Authenticate(ctx, entry.methodID, entry.authID, "", true); err != nil {
		return &Outcome{Text: fmt.Sprintf("Login cancelled (agent reported: %v).", err)}, nil
	}
	return &Outcome{Text: "Login cancelled."}, nil
}

// peek returns the pending entry for convID.
func (b *Broker) peek(convID string) (pendingEntry, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	e, ok := b.pending[convID]
	return e, ok
}

// resolveMethodID maps a user-typed provider name to the full method id
// advertised by the agent. Accepts both the short form ("anthropic") and
// the full form ("oauth-anthropic"). Case-insensitive on the short form.
func (b *Broker) resolveMethodID(provider string) (string, error) {
	methods := b.a.AuthMethods()
	loginable := filterLoginable(methods)
	if len(loginable) == 0 {
		return "", errors.New("the agent advertises no OAuth login methods")
	}
	want := strings.ToLower(strings.TrimSpace(provider))
	for _, m := range loginable {
		if m.ID == provider {
			return m.ID, nil
		}
		if strings.EqualFold(strings.TrimPrefix(m.ID, "oauth-"), want) {
			return m.ID, nil
		}
	}
	available := make([]string, 0, len(loginable))
	for _, m := range loginable {
		available = append(available, strings.TrimPrefix(m.ID, "oauth-"))
	}
	sort.Strings(available)
	return "", fmt.Errorf("unknown provider %q. Available: %s", provider, strings.Join(available, ", "))
}

// filterLoginable returns only OAuth/agent-typed methods we can actually
// drive over Poe (env_var and terminal methods aren't usable here).
func filterLoginable(methods []client.AuthMethod) []client.AuthMethod {
	out := methods[:0:0]
	for _, m := range methods {
		// type=="" defaults to "agent" per the RFD.
		if m.Type == "" || m.Type == "agent" {
			out = append(out, m)
		}
	}
	return out
}
