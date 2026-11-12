package interject

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	acp "github.com/coder/acp-go-sdk"
	"github.com/kfet/acp-kit/client"
)

type fake struct {
	forkErr, promptErr, extErr error
	steer                      bool
	out                        string
	prompts                    []string
	closed                     []acp.SessionId
	ext                        []map[string]any
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

func (f *fake) Caps() client.Caps {
	if !f.steer {
		return client.Caps{}
	}
	return client.Caps{Extensions: map[string]json.RawMessage{SteerCap: json.RawMessage(`{}`)}}
}

func (f *fake) ExtMethod(_ context.Context, sid acp.SessionId, method string, p map[string]any) (json.RawMessage, error) {
	if sid != "main" || method != SteerMethod {
		panic("bad ext call")
	}
	f.ext = append(f.ext, p)
	return nil, f.extErr
}

func TestNewDisabled(t *testing.T) {
	if New(&fake{}, Config{}) != nil || New(nil, Config{Enabled: true}) != nil {
		t.Fatal("want nil")
	}
	var ij *Interjector
	if ij.Take("c") != "" {
		t.Fatal("nil receiver")
	}
	if New(&fake{}, Config{Enabled: true, MinConf: 3}).minConf != 3 {
		t.Fatal("minConf")
	}
}

func TestParse(t *testing.T) {
	cases := []struct {
		in   string
		want Reply
	}{
		{"hello\n2 8", Reply{Text: "hello", Action: Inform, Conf: 8}},
		{"hello\nNOTE:  check x \n2 8", Reply{Text: "hello", Note: "check x", Action: Inform, Conf: 8}},
		{"NOTE: n\n1 9", Reply{Note: "n", Action: None, Conf: 9}},
		{"hello\n2 x", Reply{Text: "hello\n2 x", Action: None, Conf: -1}},
		{"hello\n4 8", Reply{Text: "hello\n4 8", Action: None, Conf: -1}},
		{"hello", Reply{Text: "hello", Action: None, Conf: -1}},
		{"3 9", Reply{Action: Abort, Conf: 9}},
	}
	for _, c := range cases {
		if r := Parse(c.in); r != c.want {
			t.Errorf("%q: %+v", c.in, r)
		}
	}
}

func TestActionString(t *testing.T) {
	for a, w := range map[Action]string{None: "answered", Inform: "told main", Abort: "stopping"} {
		if a.String() != w {
			t.Error(a)
		}
	}
	if (Reply{Action: Inform}).Tag() != "*(quick reply · told main)*" {
		t.Error("tag")
	}
}

func TestAskFallback(t *testing.T) {
	f := &fake{out: "it is 4\nNOTE: hi\n2 8"}
	ij := New(f, Config{Enabled: true})
	r, err := ij.Ask(context.Background(), "c", "/", "main", "what is 2+2")
	if err != nil || r.Text != "it is 4" || r.Note != "hi" || r.Action != Inform || r.Conf != 8 || r.Delivered {
		t.Fatal(r, err)
	}
	if len(f.closed) != 1 || len(f.ext) != 0 {
		t.Fatal("close/ext")
	}
	f.out = "maybe\nNOTE: x\n3 3" // low conf → None, note dropped
	r, _ = ij.Ask(context.Background(), "c", "/", "main", "stop?")
	if r.Action != None || r.Note != "" {
		t.Fatal(r)
	}
	if !strings.Contains(f.prompts[1], "[earlier side Q&A this turn]\nQ: what is 2+2\nA: it is 4\nNote from the side branch: hi") {
		t.Fatal(f.prompts[1])
	}
	p := ij.Take("c")
	if !strings.Contains(p, "Q: stop?\nA: maybe") || !strings.Contains(p, "Q: what is 2+2") {
		t.Fatal(p)
	}
	if ij.Take("c") != "" {
		t.Fatal("not reset")
	}
}

func TestAskNative(t *testing.T) {
	f := &fake{steer: true, out: "a\n1 9"}
	var logs []string
	ij := New(f, Config{Enabled: true, Logf: func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }})
	r, _ := ij.Ask(context.Background(), "c", "/", "main", "q")
	if !r.Delivered || f.ext[0]["mode"] != "record" || f.ext[0]["question"] != "q" || f.ext[0]["answer"] != "a" || f.ext[0]["forkSessionId"] != "fork" {
		t.Fatal(r, f.ext)
	}
	f.out = "b\nNOTE: n\n2 9"
	r, _ = ij.Ask(context.Background(), "c", "/", "main", "q2")
	pr := f.ext[1]["prompt"].([]acp.ContentBlock)[0].Text.Text
	if !r.Delivered || f.ext[1]["mode"] != "steer" || !strings.Contains(pr, "Q: q2\nA: b\nNote from the side branch: n") {
		t.Fatal(r, pr)
	}
	if !strings.Contains(f.prompts[1], "Q: q\nA: a") {
		t.Fatal("fork context")
	}
	if ij.Take("c") != "" {
		t.Fatal("delivered must not be in Take")
	}
	f.extErr = errors.New("-32002")
	r, _ = ij.Ask(context.Background(), "c", "/", "main", "q3")
	if r.Delivered || !strings.Contains(ij.Take("c"), "Q: q3") {
		t.Fatal(r)
	}
	if len(logs) != 1 || logs[0] != "interject: _fir/session/steer steer to main refused: -32002" {
		t.Fatalf("logs = %q", logs)
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
	if r, err := ij.Ask(context.Background(), "c", "/", "m", "q"); err != nil || r.Action != Abort {
		t.Fatal(r, err)
	}
	ij = New(&fake{out: "  "}, Config{Enabled: true})
	if _, err := ij.Ask(context.Background(), "c", "/", "m", "q"); err == nil {
		t.Fatal("want empty-answer error")
	}
}

var _ Agent = (*client.AgentProc)(nil)
