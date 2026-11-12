// Package interject answers a message that arrives while a
// conversation's main turn is still running, without stopping it.
//
// The relay forks the main session (session/fork), asks the fork the
// new message, posts the fork's short answer at once, and lets the
// fork's own verdict decide what happens to the main turn:
//
//	None   — the answer was enough; main never sees the exchange.
//	Inform — the side Q&A (plus an optional note from the fork) is
//	         injected into the running main turn, which keeps going.
//	Abort  — the relay cancels the main turn.
//
// When the agent advertises the fir steer extension (SteerCap), Ask
// delivers natively: None and Abort are logged with
// _fir/session/steer mode "record" (a side_query transcript entry the
// LLM never sees) and Inform is sent with mode "steer". Reply.Delivered
// reports it.
//
// Without the extension — or when steer finds no running turn — the
// exchange is kept and Take hands it back once as a prefix for the next
// main prompt; the relay handles an undelivered Inform as before
// (cancel main, re-prompt). Every interrupt forks afresh; earlier side
// Q&A of the same turn is carried into the fork as text until Take.
package interject

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	acp "github.com/coder/acp-go-sdk"
	"github.com/kfet/acp-kit/client"
)

// Action is the fork's verdict on the main turn.
type Action int

// Actions, numbered as the fork reports them.
const (
	None Action = iota + 1
	Inform
	Abort
)

// String returns the action's tag word.
func (a Action) String() string {
	switch a {
	case Inform:
		return "told main"
	case Abort:
		return "stopping"
	default:
		return "answered"
	}
}

// DefaultMinConf is the confidence below which a verdict becomes None.
const DefaultMinConf = 7

// SteerCap is the agentCapabilities._meta key of the fir steer
// extension; SteerMethod is its method.
const (
	SteerCap    = "dev.fir.steer/v1"
	SteerMethod = "_fir/session/steer"
)

// Config is the relay's opt-in. The zero value is disabled.
type Config struct {
	Enabled bool `json:"enabled" yaml:"enabled"`
	// MinConf (0-9) is the confidence a verdict needs to act; 0 means
	// DefaultMinConf.
	MinConf int `json:"min_conf" yaml:"min_conf"`
}

// Agent is the agent surface used. *client.AgentProc satisfies it.
type Agent interface {
	ForkSession(ctx context.Context, cwd string, parent acp.SessionId, at string, sink client.SessionUpdateSink) (acp.SessionId, error)
	Prompt(ctx context.Context, sid acp.SessionId, prompt []acp.ContentBlock) (acp.StopReason, error)
	CloseSession(ctx context.Context, sid acp.SessionId) error
	Caps() client.Caps
	ExtMethod(ctx context.Context, sid acp.SessionId, method string, params map[string]any) (json.RawMessage, error)
}

// Reply is the fork's answer.
type Reply struct {
	Text   string // the answer, verdict and NOTE lines stripped
	Note   string // optional note from the fork for the main agent; Inform only
	Action Action // after the confidence gate
	Conf   int    // -1 when the verdict line did not parse
	// Delivered: the agent took the exchange natively (record or
	// steer), so it is not in Take. An undelivered Inform is the
	// relay's to handle (cancel main and re-prompt).
	Delivered bool
}

// Interjector keeps the per-conversation side state. Safe for
// concurrent use.
type Interjector struct {
	agent   Agent
	minConf int

	mu   sync.Mutex
	side map[string]*side
}

type side struct {
	qa      []string // every exchange this turn, for the next fork
	pending []string // undelivered exchanges, for Take
}

// New returns an Interjector, or nil when cfg is disabled — so a relay
// can test `ij != nil`.
func New(agent Agent, cfg Config) *Interjector {
	if !cfg.Enabled || agent == nil {
		return nil
	}
	mc := cfg.MinConf
	if mc <= 0 {
		mc = DefaultMinConf
	}
	return &Interjector{agent: agent, minConf: mc, side: map[string]*side{}}
}

// Instruction is the rule appended to every fork prompt.
const Instruction = `[side question while your main task keeps running in another branch]
Answer the message above briefly. Do NOT call any tools.
Optionally, just before the last line, add one line "NOTE: <text>" — a note to your main task, sent with action 2.
End with a final line of exactly two numbers separated by one space: ACTION CONF
ACTION: 1 = just answer, the main task never sees this; 2 = also tell the main task now (it keeps running); 3 = stop the main task.
CONF: your confidence 0-9.`

// Ask forks parent, asks it text, delivers the verdict to parent when
// the agent supports it, and returns the answer. On a fork error
// (including client.ErrForkUnsupported) the caller falls back to its
// old behaviour. The fork is closed before Ask returns. Bound ctx: the
// fork prompt has no timeout of its own. Text may be empty when the
// fork gave only a verdict.
func (i *Interjector) Ask(ctx context.Context, conv, cwd string, parent acp.SessionId, text string) (Reply, error) {
	col := &collector{}
	fork, err := i.agent.ForkSession(ctx, cwd, parent, "", col)
	if err != nil {
		return Reply{}, err
	}
	defer func() { _ = i.agent.CloseSession(context.WithoutCancel(ctx), fork) }()
	if _, err := i.agent.Prompt(ctx, fork, []acp.ContentBlock{acp.TextBlock(i.prompt(conv, text))}); err != nil {
		return Reply{}, err
	}
	r := Parse(col.text())
	if r.Conf < i.minConf {
		r.Action = None
	}
	if r.Action != Inform {
		r.Note = ""
	}
	if r.Text == "" && r.Conf < 0 {
		return Reply{}, errors.New("interject: fork returned no answer")
	}
	qa := "Q: " + text + "\nA: " + r.Text
	if r.Note != "" {
		qa += "\nNote from the side branch: " + r.Note
	}
	if _, ok := i.agent.Caps().Extensions[SteerCap]; ok {
		r.Delivered = i.deliver(ctx, parent, fork, text, qa, r)
	}
	i.mu.Lock()
	s := i.sideOf(conv)
	s.qa = append(s.qa, qa)
	if !r.Delivered {
		s.pending = append(s.pending, qa)
	}
	i.mu.Unlock()
	return r, nil
}

// deliver sends the exchange to parent via SteerMethod and reports
// whether the agent accepted it. Any error (e.g. no running turn) means
// not delivered.
func (i *Interjector) deliver(ctx context.Context, parent, fork acp.SessionId, q, qa string, r Reply) bool {
	p := map[string]any{"mode": "record", "question": q, "answer": r.Text, "forkSessionId": string(fork)}
	if r.Action == Inform {
		p = map[string]any{"mode": "steer", "prompt": []acp.ContentBlock{acp.TextBlock(
			"[while you were working, the user asked and a side branch answered]\n" + qa)}}
	}
	_, err := i.agent.ExtMethod(ctx, parent, SteerMethod, p)
	return err == nil
}

func (i *Interjector) sideOf(conv string) *side {
	s := i.side[conv]
	if s == nil {
		s = &side{}
		i.side[conv] = s
	}
	return s
}

func (i *Interjector) prompt(conv, text string) string {
	var b strings.Builder
	i.mu.Lock()
	if s := i.side[conv]; s != nil && len(s.qa) > 0 {
		b.WriteString("[earlier side Q&A this turn]\n")
		b.WriteString(strings.Join(s.qa, "\n\n"))
		b.WriteString("\n\n")
	}
	i.mu.Unlock()
	b.WriteString(text)
	b.WriteString("\n\n")
	b.WriteString(Instruction)
	return b.String()
}

// Take returns the prefix for conv's next main prompt — the side Q&A
// the agent did not take natively — and resets conv's side state. ""
// when there is none. Call it when building every main prompt: it is
// what resets the side state for the next turn.
func (i *Interjector) Take(conv string) string {
	if i == nil {
		return ""
	}
	i.mu.Lock()
	s := i.side[conv]
	delete(i.side, conv)
	i.mu.Unlock()
	if s == nil || len(s.pending) == 0 {
		return ""
	}
	return "[while you were working, the user asked and a side branch answered]\n" +
		strings.Join(s.pending, "\n\n") + "\n\n"
}

// Tag is the marker a relay puts on the posted answer.
func (r Reply) Tag() string { return fmt.Sprintf("*(quick reply · %s)*", r.Action) }

// Parse splits the fork's output into the answer, an optional "NOTE:"
// line just before the verdict, and the verdict line "ACTION CONF". An
// unparseable last line yields the whole text, None and conf -1.
func Parse(out string) Reply {
	out = strings.TrimSpace(out)
	idx := strings.LastIndexByte(out, '\n')
	f := strings.Fields(out[idx+1:])
	if len(f) != 2 {
		return Reply{Text: out, Action: None, Conf: -1}
	}
	a, err1 := strconv.Atoi(f[0])
	c, err2 := strconv.Atoi(f[1])
	if err1 != nil || err2 != nil || a < 1 || a > 3 || c < 0 || c > 9 {
		return Reply{Text: out, Action: None, Conf: -1}
	}
	r := Reply{Text: strings.TrimSpace(out[:idx+1]), Action: Action(a), Conf: c}
	j := strings.LastIndexByte(r.Text, '\n')
	if n, ok := strings.CutPrefix(strings.TrimSpace(r.Text[j+1:]), "NOTE:"); ok {
		r.Note = strings.TrimSpace(n)
		r.Text = strings.TrimSpace(r.Text[:j+1])
	}
	return r
}

type collector struct {
	mu sync.Mutex
	b  strings.Builder
}

func (c *collector) OnUpdate(_ context.Context, n acp.SessionNotification) error {
	if m := n.Update.AgentMessageChunk; m != nil && m.Content.Text != nil {
		c.mu.Lock()
		c.b.WriteString(m.Content.Text.Text)
		c.mu.Unlock()
	}
	return nil
}

func (c *collector) text() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.b.String()
}
