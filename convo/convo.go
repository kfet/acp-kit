// Package convo is the shared conversation → ACP-session manager core
// every relay (poe-acp, zulip-acp, slack-acp) puts between its chat
// surface and the agent.
//
// A relay used to hand-write the same five things: a sticky per-
// conversation `!model` override, a turn runner (serial FIFO or
// supersede-on-follow-up), race-safe cancellation, a turn liveness
// bound, and a command.Controller over all of it — and one relay
// (slack-acp) never wrote the Controller at all, so `!model` reached a
// broken model as a prompt. Manager owns those; the relay injects what
// only it knows:
//
//   - Sink: where a command's reply goes (a Zulip message, an SSE
//     frame, a Slack thread post).
//   - Filters: ingress rules that run before and after the broker —
//     Zulip's /me /poll /todo passthrough, the `!!` escape, relay-only
//     commands such as `!opts`, `!archive` and `!branch`.
//   - Liveness: the turn-bound policy (ProgressClock by default).
//   - Store: optional persistence for model overrides.
//   - Hooks: relay-shaped Controller behaviour — re-keying `!new`,
//     token → conversation resolution, status decoration.
//   - Sessions: the conversation → ACP session map. acp-kit/state's
//     Manager is the shared implementation; a relay with its own
//     session semantics (poe-acp's per-host agents and transcript
//     divergence) plugs its map in through the same interface.
//
// Dispatch is the single entry point: relay filters, then the command
// broker (standard `!commands` are on by default), then the passthrough
// rewrite, then the prompt — run by the Manager's turn runner when the
// relay supplies Run, or handed back to the caller when it streams the
// turn itself.
package convo

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/kfet/acp-kit/client"
	"github.com/kfet/acp-kit/command"
)

// Agent is the agent surface the Manager needs. *client.AgentProc
// satisfies it, and also the optional ModelSetter, Informer and
// StatsReporter the Manager uses when present.
type Agent interface {
	Models() (models []client.ModelInfo, currentID string)
	AvailableCommands() []client.CommandInfo
}

// ModelSetter applies a model to one session (ApplyModel).
type ModelSetter interface {
	SetModel(ctx context.Context, sid acp.SessionId, modelID string) error
}

// Informer reports the agent's identity (`!status`).
type Informer interface{ AgentInfo() client.AgentInfo }

// StatsReporter reports per-session ACP stats (`!status`).
type StatsReporter interface {
	SessionStats(sid acp.SessionId) (client.SessionStats, bool)
}

// Sessions is the conversation → ACP session map. *state.Manager
// satisfies it (and Resetter).
type Sessions interface {
	// Live reports conv's live session without creating one.
	Live(conv string) (sid acp.SessionId, lastUsed time.Time, ok bool)
	// Cancel sends session/cancel for conv's live session, if any.
	Cancel(ctx context.Context, conv string)
	// Len is the number of live sessions.
	Len() int
}

// Resetter is the optional Sessions capability behind the default `!new`:
// drop conv's session so the next turn starts fresh.
type Resetter interface{ Reset(conv string) error }

// Sink delivers a command's reply to the conversation.
type Sink interface {
	Reply(ctx context.Context, in *In, text string) error
}

// Failer is an optional Sink capability for a failed command. Without it
// the Manager replies "Command failed: <err>".
type Failer interface {
	Fail(ctx context.Context, in *In, err error) error
}

// SinkFunc adapts a function to Sink.
type SinkFunc func(ctx context.Context, in *In, text string) error

// Reply implements Sink.
func (f SinkFunc) Reply(ctx context.Context, in *In, text string) error { return f(ctx, in, text) }

// In is one inbound message.
type In struct {
	// Conv is the relay's conversation id — the session key.
	Conv string
	// Token is the broker's opaque conversation token; "" means Conv.
	// A relay that re-keys conversations (zulip-acp's `!new`) passes a
	// stable token here and resolves it with Config.Resolve.
	Token string
	Text  string
	// Sink overrides Config.Sink for this message (poe-acp: the
	// request's SSE stream).
	Sink Sink
	// Meta is relay-owned context for filters and Run (the Zulip
	// message, the Slack event).
	Meta any
}

func (in *In) token() string {
	if in.Token != "" {
		return in.Token
	}
	return in.Conv
}

// Verdict is a Filter's decision.
type Verdict int

const (
	// Pass continues dispatch with the (possibly rewritten) text.
	Pass Verdict = iota
	// Handled means the filter consumed the message.
	Handled
	// Forward skips every remaining classification and sends the text
	// to the agent as a prompt.
	Forward
)

// Filter is an ingress rule. It may rewrite in.Text.
type Filter func(ctx context.Context, in *In) Verdict

// Command is a relay-only chat command (zulip-acp's `!opts`,
// `!archive`, `!branch`).
type Command struct {
	// Match reports whether text invokes the command, and its argument.
	Match func(text string) (arg string, ok bool)
	Run   func(ctx context.Context, in *In, arg string)
	// Help lines appended to the broker's `!help`.
	Help []string
}

// Filter returns the command as a Before filter.
func (c Command) Filter() Filter {
	return func(ctx context.Context, in *In) Verdict {
		arg, ok := c.Match(in.Text)
		if !ok {
			return Pass
		}
		c.Run(ctx, in, arg)
		return Handled
	}
}

// Mode is the turn runner's policy for a message arriving while a turn
// runs in the same conversation.
type Mode int

const (
	// Serial queues it behind the running turn (FIFO).
	Serial Mode = iota
	// Supersede cancels the running turn and starts the new one.
	Supersede
)

// Hooks are relay-shaped Controller behaviour. Every hook is optional.
type Hooks struct {
	// Resolve maps a broker token to the conversation id. ok=false means
	// the conversation does not exist yet. Default: the token itself.
	Resolve func(token string) (conv string, ok bool)
	// Reset replaces the default `!new` (stop the turn, drop the
	// session). zulip-acp retires the conversation and re-keys it.
	Reset func(ctx context.Context, token string) error
	// Status and RelayInfo post-process the Manager's snapshot.
	Status    func(token, conv string, ok bool, st command.SessionStatus) command.SessionStatus
	RelayInfo func(token, conv string, ok bool, ri command.RelayInfo) command.RelayInfo
	// ModelChanged runs after a successful `!model <id>`.
	ModelChanged func(token, conv, modelID string)
	// Decorate post-processes a broker reply before it is sent.
	Decorate func(text, reply string) string
}

// ErrNoConversation is returned by SetModelOverride for a token that
// resolves to no conversation.
var ErrNoConversation = errors.New("there is no conversation here yet — send a message first")

// RunFunc executes one prompt turn. It runs on the Manager's turn
// runner: ctx is cancelled by `!stop`, a superseding follow-up, or
// Active.CancelToken.
type RunFunc func(ctx context.Context, t *Turn, in *In, prompt string) error

// Config configures a Manager.
type Config struct {
	Agent    Agent
	Sessions Sessions // optional

	// Broker is the command broker. Nil builds a standard one over Auth
	// (or Agent, when it can authenticate) unless NoCommands is set.
	Broker     *command.Broker
	Auth       command.Authenticator
	NoCommands bool
	// Poster and Scheduler are the optional command capabilities behind
	// out-of-band posting and `!schedules`, handed to the broker.
	Poster    command.Poster
	Scheduler command.Scheduler

	// Overrides holds the `!model` choices. Nil = in-memory, or
	// persistent when Store is set.
	Overrides *Overrides
	Store     Store

	Mode     Mode
	Liveness Liveness // nil = ProgressClock{NoProgressTimeout: 2m}
	// NoStop leaves `!stop` unadvertised (command.TurnStopper not
	// implemented) for relays with no in-flight turn a later message can
	// reach.
	NoStop bool

	Sink    Sink
	Before  []Filter  // before the broker
	After   []Filter  // after the passthrough rewrite
	Extra   []Command // relay-only commands, run after Before
	Run     RunFunc   // nil = Dispatch hands the prompt back
	OnError func(conv string, err error)

	Hooks      Hooks
	ModelOrder func([]client.ModelInfo) []client.ModelInfo

	Version   string
	AgentCmd  string
	StartTime time.Time
	Now       func() time.Time
	Logf      func(format string, args ...any)
}

// Manager is the conversation core. See the package doc.
type Manager struct {
	cfg    Config
	broker *command.Broker
	ov     *Overrides
	active *Active
	lanes  *lanes
}

// New builds a Manager and wires its Controller into the broker.
func New(cfg Config) (*Manager, error) {
	if cfg.Agent == nil {
		return nil, errors.New("convo: Agent is required")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logf == nil {
		cfg.Logf = log.Printf
	}
	if cfg.Liveness == nil {
		cfg.Liveness = ProgressClock{NoProgressTimeout: 2 * time.Minute}
	}
	ov := cfg.Overrides
	if ov == nil {
		var err error
		if ov, err = NewOverrides(cfg.Store); err != nil {
			return nil, err
		}
	}
	m := &Manager{cfg: cfg, broker: cfg.Broker, ov: ov, active: NewActive()}
	m.lanes = newLanes(m)
	m.active.OnCancel = func(ctx context.Context, conv string) error {
		if cfg.Sessions != nil {
			cfg.Sessions.Cancel(ctx, conv)
		}
		return nil
	}
	if m.broker == nil && !cfg.NoCommands {
		auth := cfg.Auth
		if auth == nil {
			auth, _ = cfg.Agent.(command.Authenticator)
		}
		if auth == nil {
			auth = noAuth{}
		}
		m.broker = command.New(auth)
	}
	if m.broker != nil {
		m.broker.SetController(m.Controller())
		if cfg.Poster != nil {
			m.broker.SetPoster(cfg.Poster)
		}
		if cfg.Scheduler != nil {
			m.broker.SetScheduler(cfg.Scheduler)
		}
		for _, c := range cfg.Extra {
			m.broker.AddHelp(c.Help...)
		}
	}
	return m, nil
}

// Broker returns the command broker (nil with NoCommands).
func (m *Manager) Broker() *command.Broker { return m.broker }

// Overrides returns the model-override table.
func (m *Manager) Overrides() *Overrides { return m.ov }

// Active returns the in-flight turn registry.
func (m *Manager) Active() *Active { return m.active }

// Arm arms the configured Liveness policy for one turn.
func (m *Manager) Arm(ctx context.Context) (Clock, context.Context, context.CancelFunc) {
	return m.cfg.Liveness.Arm(ctx)
}

// Result is what Dispatch did with a message.
type Result struct {
	// Handled: the message was consumed (a command, or a filter).
	Handled bool
	// Prompt is the text to send to the agent when not Handled. With
	// Config.Run set it has already been submitted to the turn runner;
	// otherwise the caller runs it.
	Prompt string
}

// Dispatch routes one inbound message: Before filters, relay commands,
// the broker, the passthrough rewrite, After filters, then the prompt.
func (m *Manager) Dispatch(ctx context.Context, in In) Result {
	if in.Sink == nil {
		in.Sink = m.cfg.Sink
	}
	if !m.classify(ctx, &in) {
		return Result{Handled: true}
	}
	if m.cfg.Run != nil {
		text := in.Text
		m.Start(ctx, Job{Conv: in.Conv, Run: func(ctx context.Context, t *Turn) error {
			return m.cfg.Run(ctx, t, &in, text)
		}})
	}
	return Result{Prompt: in.Text}
}

// classify runs every rule; false means the message was consumed.
func (m *Manager) classify(ctx context.Context, in *In) bool {
	before := append([]Filter(nil), m.cfg.Before...)
	for _, c := range m.cfg.Extra {
		before = append(before, c.Filter())
	}
	for _, f := range before {
		switch f(ctx, in) {
		case Handled:
			return false
		case Forward:
			return true
		}
	}
	if b := m.broker; b != nil {
		tok := in.token()
		if b.HasPending(tok) || b.IsCommand(in.Text) {
			m.command(ctx, in)
			return false
		}
		if rw, ok := b.Passthrough(in.Text); ok {
			in.Text = rw
		}
	}
	for _, f := range m.cfg.After {
		switch f(ctx, in) {
		case Handled:
			return false
		case Forward:
			return true
		}
	}
	return true
}

func (m *Manager) command(ctx context.Context, in *In) {
	out, err := m.broker.Handle(ctx, in.token(), in.Text)
	if err != nil {
		m.cfg.Logf("convo: command %q in %s: %v", in.Text, in.Conv, err)
		if f, ok := in.Sink.(Failer); ok {
			m.deliver(f.Fail(ctx, in, err), in)
			return
		}
		m.reply(ctx, in, fmt.Sprintf("Command failed: %v", err))
		return
	}
	text := mustOutcome(out).Text
	if m.cfg.Hooks.Decorate != nil {
		text = m.cfg.Hooks.Decorate(in.Text, text)
	}
	m.reply(ctx, in, text)
}

func (m *Manager) reply(ctx context.Context, in *In, text string) {
	if text == "" || in.Sink == nil {
		return
	}
	m.deliver(in.Sink.Reply(ctx, in, text), in)
}

func (m *Manager) deliver(err error, in *In) {
	if err != nil {
		m.cfg.Logf("convo: reply in %s: %v", in.Conv, err)
	}
}

// ApplyModel pushes conv's sticky override to session sid, at most once
// per session. Lazy on purpose: applying at `!model` time would need a
// live session, and creating one from a settings command is a side
// effect nobody asked for. A failure is logged and the turn continues on
// whatever model the session already had — far better than refusing to
// answer over a preference.
func (m *Manager) ApplyModel(ctx context.Context, conv string, sid acp.SessionId) {
	id, ok := m.ov.Pending(conv, sid)
	if !ok {
		return
	}
	ms, ok := m.cfg.Agent.(ModelSetter)
	if !ok {
		return
	}
	if err := ms.SetModel(ctx, sid, id); err != nil {
		m.cfg.Logf("convo: selecting model %s for %s: %v", id, conv, err)
		return
	}
	m.ov.MarkApplied(conv, id, sid)
}

// EffectiveModel is conv's override, else the agent's current model.
func (m *Manager) EffectiveModel(conv string) string {
	if id, ok := m.ov.Get(conv); ok {
		return id
	}
	_, cur := m.cfg.Agent.Models()
	return cur
}

// --- command.Controller -------------------------------------------------

// Controller returns the Manager's command.Controller. It implements
// command.TurnStopper unless Config.NoStop.
func (m *Manager) Controller() command.Controller {
	if m.cfg.NoStop {
		return controller{m}
	}
	return stopController{controller{m}}
}

type controller struct{ m *Manager }

type stopController struct{ controller }

func (c controller) resolve(token string) (string, bool) {
	if c.m.cfg.Hooks.Resolve != nil {
		return c.m.cfg.Hooks.Resolve(token)
	}
	return token, true
}

func (c controller) AvailableModels() ([]client.ModelInfo, string) {
	models, cur := c.m.cfg.Agent.Models()
	if c.m.cfg.ModelOrder != nil {
		models = c.m.cfg.ModelOrder(models)
	}
	return models, cur
}

func (c controller) AgentCommands() []client.CommandInfo {
	return c.m.cfg.Agent.AvailableCommands()
}

func (c controller) SetModelOverride(token, modelID string) error {
	conv, ok := c.resolve(token)
	if !ok {
		return ErrNoConversation
	}
	models, _ := c.AvailableModels()
	if err := ValidateModel(models, modelID); err != nil {
		return err
	}
	if err := c.m.ov.Set(conv, modelID); err != nil {
		c.m.cfg.Logf("convo: persisting model override for %s: %v", conv, err)
	}
	if c.m.cfg.Hooks.ModelChanged != nil {
		c.m.cfg.Hooks.ModelChanged(token, conv, modelID)
	}
	return nil
}

func (c controller) ResetSession(token string) error {
	if c.m.cfg.Hooks.Reset != nil {
		return c.m.cfg.Hooks.Reset(context.Background(), token)
	}
	conv, ok := c.resolve(token)
	if !ok {
		return nil
	}
	c.m.active.Stop(context.Background(), conv)
	if r, ok := c.m.cfg.Sessions.(Resetter); ok {
		return r.Reset(conv)
	}
	return nil
}

func (c controller) StatusFor(token string) command.SessionStatus {
	conv, ok := c.resolve(token)
	models, cur := c.AvailableModels()
	st := command.SessionStatus{EffectiveModel: cur, DefaultModel: cur, ModelsAvailable: len(models)}
	if ok {
		if id, has := c.m.ov.Get(conv); has {
			st.OverrideModel, st.EffectiveModel = id, id
		}
		// Only a relay that can stop a turn reports one running: the
		// status line offers `!stop` for it.
		st.TurnRunning = !c.m.cfg.NoStop && c.m.active.Running(conv)
		c.addSession(&st, conv)
	}
	if c.m.cfg.Hooks.Status != nil {
		st = c.m.cfg.Hooks.Status(token, conv, ok, st)
	}
	return st
}

func (c controller) addSession(st *command.SessionStatus, conv string) {
	if c.m.cfg.Sessions == nil {
		return
	}
	sid, last, live := c.m.cfg.Sessions.Live(conv)
	if !live {
		return
	}
	st.HasSession = true
	if !last.IsZero() {
		st.LastActivity = max(c.m.cfg.Now().Sub(last), 0).Round(time.Second).String()
	}
	sr, ok := c.m.cfg.Agent.(StatsReporter)
	if !ok {
		return
	}
	ss, ok := sr.SessionStats(sid)
	if !ok {
		return
	}
	st.Thinking = ss.Thinking
	st.ContextUsed, st.ContextSize = ss.ContextUsed, ss.ContextSize
	if ss.Cost != nil {
		st.Cost = strings.TrimSpace(fmt.Sprintf("%.2f %s", ss.Cost.Amount, ss.Cost.Currency))
	}
}

func (c controller) RelayInfo(token string) command.RelayInfo {
	conv, ok := c.resolve(token)
	models, _ := c.AvailableModels()
	ri := command.RelayInfo{
		Version:         c.m.cfg.Version,
		AgentCmd:        c.m.cfg.AgentCmd,
		ModelsAvailable: len(models),
	}
	if in, ok := c.m.cfg.Agent.(Informer); ok {
		ai := in.AgentInfo()
		ri.AgentName, ri.AgentVersion = ai.Name, ai.Version
	}
	if !c.m.cfg.StartTime.IsZero() {
		ri.Uptime = c.m.cfg.Now().Sub(c.m.cfg.StartTime).Round(time.Second).String()
	}
	if s := c.m.cfg.Sessions; s != nil {
		ri.ActiveSessions = s.Len()
		if ok {
			if sid, _, live := s.Live(conv); live {
				ri.SessionID = string(sid)
			}
		}
	}
	if ok {
		ri.EffectiveModel = c.m.EffectiveModel(conv)
	}
	if c.m.cfg.Hooks.RelayInfo != nil {
		ri = c.m.cfg.Hooks.RelayInfo(token, conv, ok, ri)
	}
	return ri
}

// StopTurn implements command.TurnStopper.
func (c stopController) StopTurn(token string) bool {
	conv, ok := c.resolve(token)
	if !ok {
		return false
	}
	return c.m.active.Stop(context.Background(), conv)
}

// noAuth is the Authenticator of an agent that cannot log in: `!login`
// then reports no providers instead of dereferencing nothing.
type noAuth struct{}

func (noAuth) AuthMethods() []client.AuthMethod { return nil }

func (noAuth) Authenticate(context.Context, string, string, string, bool) (client.AuthResult, error) {
	return client.AuthResult{}, errors.New("this agent does not support login")
}
