package interject

import (
	"context"
	"errors"
	"strings"
	"testing"

	acp "github.com/coder/acp-go-sdk"
	"github.com/kfet/acp-kit/client"
)

type fake struct {
	forkErr, promptErr error
	out                string
	prompts            []string
	closed             []acp.SessionId
}

func (f *fake) ForkSession(_ context.Context, _ string, _ acp.SessionId, _ string, sink client.SessionUpdateSink) (acp.SessionId, error) {
	if f.forkErr != nil {
		return "", f.forkErr
	}
	_ = sink.OnUpdate(context.Background(), acp.SessionNotification{Update: acp.UpdateAgentMessageText(f.out)})
	_ = sink.OnUpdate(context.Background(), acp.SessionNotification{Update: acp.UpdateAgentThoughtText("x")})
	return "fork", nil
}

func (f *fake) Prompt(_ context.Context, _ acp.SessionId, p []acp.ContentBlock) (acp.StopReason, error) {
	f.prompts = append(f.prompts, p[0].Text.Text)
	return acp.StopReasonEndTurn, f.promptErr
}

func (f *fake) CloseSession(_ context.Context, sid acp.SessionId) error {
	f.closed = append(f.closed, sid)
	return nil
}

func TestNewDisabled(t *testing.T) {
	if New(&fake{}, Config{}) != nil || New(nil, Config{Enabled: true}) != nil {
		t.Fatal("want nil")
	}
	var ij *Interjector
	if ij.Take("c") != "" || ij.Queued("c") {
		t.Fatal("nil receiver")
	}
	if New(&fake{}, Config{Enabled: true, MinConf: 3}).minConf != 3 {
		t.Fatal("minConf")
	}
}

func TestParse(t *testing.T) {
	cases := []struct {
		in   string
		body string
		act  Action
		conf int
	}{
		{"hello\n2 8", "hello", Queue, 8},
		{"hello\n2 x", "hello\n2 x", None, -1},
		{"hello\n5 8", "hello\n5 8", None, -1},
		{"hello", "hello", None, -1},
		{"3 9", "", Steer, 9},
	}
	for _, c := range cases {
		b, a, n := Parse(c.in)
		if b != c.body || a != c.act || n != c.conf {
			t.Errorf("%q: %q %v %d", c.in, b, a, n)
		}
	}
}

func TestActionString(t *testing.T) {
	for a, w := range map[Action]string{None: "answered", Queue: "queued", Steer: "steering", Abort: "stopping"} {
		if a.String() != w {
			t.Error(a)
		}
	}
	if (Reply{Action: Queue}).Tag() != "*(quick reply · queued)*" {
		t.Error("tag")
	}
}

func TestAskFlow(t *testing.T) {
	f := &fake{out: "it is 4\n2 8"}
	ij := New(f, Config{Enabled: true})
	r, err := ij.Ask(context.Background(), "c", "/", "main", "what is 2+2")
	if err != nil || r.Text != "it is 4" || r.Action != Queue || r.Conf != 8 {
		t.Fatal(r, err)
	}
	if len(f.closed) != 1 || !ij.Queued("c") {
		t.Fatal("close/queued")
	}
	f.out = "maybe\n4 3" // low conf → None
	r, _ = ij.Ask(context.Background(), "c", "/", "main", "stop?")
	if r.Action != None {
		t.Fatal(r)
	}
	if !strings.Contains(f.prompts[1], "[earlier side Q&A this turn]\nQ: what is 2+2\nA: it is 4") {
		t.Fatal(f.prompts[1])
	}
	p := ij.Take("c")
	if !strings.Contains(p, "Q: stop?\nA: maybe") || !strings.Contains(p, "[queued messages from the user]\nwhat is 2+2") {
		t.Fatal(p)
	}
	if ij.Take("c") != "" || ij.Queued("c") {
		t.Fatal("not reset")
	}
}

func TestTakeQAOnly(t *testing.T) {
	ij := New(&fake{out: "a\n1 9"}, Config{Enabled: true})
	_, _ = ij.Ask(context.Background(), "c", "/", "m", "q")
	if p := ij.Take("c"); strings.Contains(p, "queued") || !strings.Contains(p, "Q: q") {
		t.Fatal(p)
	}
	ij.side["d"] = &side{queued: []string{"x"}}
	if p := ij.Take("d"); strings.Contains(p, "side branch") {
		t.Fatal(p)
	}
}

func TestAskErrors(t *testing.T) {
	boom := errors.New("boom")
	ij := New(&fake{forkErr: boom}, Config{Enabled: true})
	if _, err := ij.Ask(context.Background(), "c", "/", "m", "q"); err != boom {
		t.Fatal(err)
	}
	f := &fake{promptErr: boom}
	ij = New(f, Config{Enabled: true})
	if _, err := ij.Ask(context.Background(), "c", "/", "m", "q"); err != boom || len(f.closed) != 1 {
		t.Fatal(err)
	}
	ij = New(&fake{out: "3 9"}, Config{Enabled: true})
	if r, err := ij.Ask(context.Background(), "c", "/", "m", "q"); err != nil || r.Action != Steer {
		t.Fatal(r, err)
	}
	ij = New(&fake{out: "  "}, Config{Enabled: true})
	if _, err := ij.Ask(context.Background(), "c", "/", "m", "q"); err == nil {
		t.Fatal("want empty-answer error")
	}
}

var _ Agent = (*client.AgentProc)(nil)
