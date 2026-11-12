// Package interject answers a message that arrives while a
// conversation's main turn is still running, without stopping it.
//
// The relay forks the main session (session/fork), asks the fork the
// new message, posts the fork's short answer at once, and lets the
// fork's own verdict decide what happens to the main turn:
//
//	None  — the answer was enough; main continues untouched.
//	Queue — main continues; the message is held for the next prompt.
//	Steer — cancel main and re-prompt it with the message (the old
//	        behaviour).
//	Abort — cancel main only.
//
// Nothing is spliced into the main transcript. The side Q&A of the
// running turn (and any queued messages) is handed back once, by Take,
// as a prefix for the next main prompt. Every interrupt forks afresh,
// so each sees the main turn's latest progress; earlier side Q&A of the
// same turn is carried into the fork as text.
package interject

import (
	"context"
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
	Queue
	Steer
	Abort
)

// String returns the action's tag word.
func (a Action) String() string {
	switch a {
	case Queue:
		return "queued"
	case Steer:
		return "steering"
	case Abort:
		return "stopping"
	default:
		return "answered"
	}
}

// DefaultMinConf is the confidence below which a verdict becomes None.
const DefaultMinConf = 7

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
}

// Reply is the fork's answer.
type Reply struct {
	Text   string // the answer, verdict line stripped
	Action Action // after the confidence gate
	Conf   int    // -1 when the verdict line did not parse
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
	qa     []string // "Q: …\nA: …"
	queued []string
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
End with a final line of exactly two numbers separated by one space: ACTION CONF
ACTION: 1 = just answer, main task continues; 2 = also queue the message for the main task's next turn; 3 = redirect the main task now with this message; 4 = stop the main task.
CONF: your confidence 0-9.`

// Ask forks parent, asks it text and returns the answer. On a fork
// error (including client.ErrForkUnsupported) the caller falls back to
// its old behaviour. The fork is closed before Ask returns. Bound ctx:
// the fork prompt has no timeout of its own. Text may be empty when the
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
	body, act, conf := Parse(col.text())
	if conf < i.minConf {
		act = None
	}
	if body == "" && conf < 0 {
		return Reply{}, errors.New("interject: fork returned no answer")
	}
	i.mu.Lock()
	s := i.sideOf(conv)
	s.qa = append(s.qa, "Q: "+text+"\nA: "+body)
	if act == Queue {
		s.queued = append(s.queued, text)
	}
	i.mu.Unlock()
	return Reply{Text: body, Action: act, Conf: conf}, nil
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
// digest and queued messages — and resets conv's side state. "" when
// there is none. Call it when building every main prompt: that is the
// merge, and it is what resets the side state for the next turn.
func (i *Interjector) Take(conv string) string {
	if i == nil {
		return ""
	}
	i.mu.Lock()
	s := i.side[conv]
	delete(i.side, conv)
	i.mu.Unlock()
	if s == nil {
		return ""
	}
	var b strings.Builder
	if len(s.qa) > 0 {
		b.WriteString("[while you were working, the user asked and a side branch answered]\n")
		b.WriteString(strings.Join(s.qa, "\n\n"))
		b.WriteString("\n\n")
	}
	if len(s.queued) > 0 {
		b.WriteString("[queued messages from the user]\n")
		b.WriteString(strings.Join(s.queued, "\n\n"))
		b.WriteString("\n\n")
	}
	return b.String()
}

// Queued reports whether conv holds queued messages that need a main
// turn to deliver them.
func (i *Interjector) Queued(conv string) bool {
	if i == nil {
		return false
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	s := i.side[conv]
	return s != nil && len(s.queued) > 0
}

// Tag is the marker a relay puts on the posted answer.
func (r Reply) Tag() string { return fmt.Sprintf("*(quick reply · %s)*", r.Action) }

// Parse splits the fork's output into the answer and the verdict line.
// An unparseable last line yields the whole text, None and conf -1.
func Parse(out string) (body string, act Action, conf int) {
	out = strings.TrimSpace(out)
	idx := strings.LastIndexByte(out, '\n')
	last := out[idx+1:]
	f := strings.Fields(last)
	if len(f) == 2 {
		a, err1 := strconv.Atoi(f[0])
		c, err2 := strconv.Atoi(f[1])
		if err1 == nil && err2 == nil && a >= 1 && a <= 4 && c >= 0 && c <= 9 {
			return strings.TrimSpace(out[:idx+1]), Action(a), c
		}
	}
	return out, None, -1
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
