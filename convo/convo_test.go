package convo

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/kfet/acp-kit/client"
	"github.com/kfet/acp-kit/command"
	"github.com/kfet/acp-kit/schedule"
)

type badStore struct{ loadErr, saveErr error }

func (b badStore) Load() (map[string]string, error) { return nil, b.loadErr }
func (b badStore) Save(string, string) error        { return b.saveErr }

func TestNewErrors(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("nil agent accepted")
	}
	if _, err := New(Config{Agent: &fakeAgent{}, Store: badStore{loadErr: errors.New("x")}}); err == nil {
		t.Fatal("bad store accepted")
	}
}

func TestDefaultsAndAccessors(t *testing.T) {
	m := newM(t, Config{Agent: &fakeAgent{}})
	if m.Broker() == nil || m.Overrides() == nil || m.Active() == nil {
		t.Fatal("accessors")
	}
	_, ctx, stop := m.Arm(context.Background())
	stop()
	if ctx.Err() == nil {
		t.Fatal("arm")
	}
	// No-auth agent: !login reports rather than panics.
	sink := &recSink{}
	m.Dispatch(context.Background(), In{Conv: "c", Text: "!login", Sink: sink})
	if sink.all() == "" {
		t.Fatal("no login reply")
	}
	if _, err := (noAuth{}).Authenticate(context.Background(), "", "", "", false); err == nil {
		t.Fatal("noAuth")
	}
	if m2 := newM(t, Config{Agent: &fakeAgent{}, NoCommands: true}); m2.Broker() != nil {
		t.Fatal("NoCommands")
	}
	own := command.New(failAuth{})
	if m3 := newM(t, Config{Agent: &fakeAgent{}, Broker: own}); m3.Broker() != own {
		t.Fatal("own broker")
	}
}

// The motivating slack-acp bug: with the model broken, every standard
// command still answers from the relay and never reaches the agent.
func TestStandardCommandsNeverPrompt(t *testing.T) {
	ag := &richAgent{fakeAgent{models: []client.ModelInfo{{ID: "good"}, {ID: "broken"}}, current: "broken"}}
	ss := &resetSessions{fakeSessions{live: map[string]acp.SessionId{"c": "s1"}}}
	var prompts []string
	var mu sync.Mutex
	block := make(chan struct{})
	started := make(chan struct{})
	sink := &recSink{}
	m := newM(t, Config{Agent: ag, Sessions: ss, Sink: sink, Mode: Supersede,
		Run: func(ctx context.Context, _ *Turn, _ *In, p string) error {
			mu.Lock()
			prompts = append(prompts, p)
			mu.Unlock()
			close(started)
			select {
			case <-block:
			case <-ctx.Done():
			}
			return ctx.Err()
		}})
	ctx := ctxT(t)
	if r := m.Dispatch(ctx, In{Conv: "c", Text: "hello"}); r.Handled || r.Prompt != "hello" {
		t.Fatalf("prompt: %+v", r)
	}
	<-started
	turn, _ := m.Active().Get("c")
	for _, cmd := range []string{"!model good", "!status", "!stop", "!new", "!model"} {
		if r := m.Dispatch(ctx, In{Conv: "c", Text: cmd}); !r.Handled {
			t.Fatalf("%s not handled", cmd)
		}
	}
	close(block)
	<-turn.Ended()
	mu.Lock()
	defer mu.Unlock()
	if len(prompts) != 1 {
		t.Fatalf("prompts = %v", prompts)
	}
	out := sink.all()
	for _, want := range []string{"good", "Interrupted"} {
		if !strings.Contains(out, want) {
			t.Errorf("replies missing %q:\n%s", want, out)
		}
	}
	if id, _ := m.Overrides().Get("c"); id != "good" {
		t.Fatal("override not set")
	}
	if len(ss.resets) != 1 || len(ss.cancels) == 0 {
		t.Fatalf("resets=%v cancels=%v", ss.resets, ss.cancels)
	}
	// Lazy apply on the next turn's session.
	m.ApplyModel(ctx, "c", "s2")
	m.ApplyModel(ctx, "c", "s2")
	if len(ag.setCalls) != 1 || ag.setCalls[0] != "s2=good" {
		t.Fatalf("set calls %v", ag.setCalls)
	}
	ag.setErr = errors.New("nope")
	m.ApplyModel(ctx, "c", "s3")
	if _, ok := m.Overrides().Pending("c", "s3"); !ok {
		t.Fatal("failed apply marked")
	}
	m.ApplyModel(ctx, "none", "s3")
	// Agent without SetModel: no-op.
	m2 := newM(t, Config{Agent: &fakeAgent{}})
	_ = m2.Overrides().Set("c", "x")
	m2.ApplyModel(ctx, "c", "s")
}

func TestFiltersAndPassthrough(t *testing.T) {
	ag := &fakeAgent{}
	sink := &recSink{err: errors.New("post failed")}
	var ran []string
	m := newM(t, Config{Agent: ag, Sink: sink, Logf: t.Logf,
		Before: []Filter{func(_ context.Context, in *In) Verdict {
			switch {
			case strings.HasPrefix(in.Text, "/me"):
				return Forward
			case in.Text == "eat":
				return Handled
			case strings.HasPrefix(in.Text, "!!"):
				in.Text = "!" + in.Text[2:]
				return Forward
			}
			return Pass
		}},
		Extra: []Command{{Match: func(s string) (string, bool) { return strings.CutPrefix(s, "!opts") },
			Run: func(_ context.Context, _ *In, arg string) { ran = append(ran, "opts:"+arg) }, Help: []string{"- `!opts`"}}},
		After: []Filter{func(_ context.Context, in *In) Verdict {
			switch in.Text {
			case "!bogus":
				return Handled
			case "later":
				return Forward
			}
			return Pass
		}},
		Hooks: Hooks{Decorate: func(text, out string) string { return out + "\n[decorated]" }},
	})
	ctx := context.Background()
	cases := []struct {
		text, prompt string
		handled      bool
	}{
		{"/me waves", "/me waves", false},
		{"eat", "", true},
		{"!!help", "!help", false},
		{"!opts x", "", true},
		{"!reload", "/reload", false},
		{"!bogus", "", true},
		{"later", "later", false},
		{"plain", "plain", false},
		{"!help", "", true},
	}
	for _, c := range cases {
		r := m.Dispatch(ctx, In{Conv: "c", Text: c.text})
		if r.Handled != c.handled || r.Prompt != c.prompt {
			t.Errorf("%q: %+v", c.text, r)
		}
	}
	if len(ran) != 1 || ran[0] != "opts: x" {
		t.Fatalf("ran %v", ran)
	}
	if out := sink.all(); !strings.Contains(out, "[decorated]") || !strings.Contains(out, "!opts") {
		t.Fatalf("help: %s", out)
	}
	// NoCommands: sigil text is just a prompt; nil sink is tolerated.
	m2 := newM(t, Config{Agent: ag, NoCommands: true})
	if r := m2.Dispatch(ctx, In{Conv: "c", Text: "!help"}); r.Handled {
		t.Fatal("handled without broker")
	}
	m3 := newM(t, Config{Agent: ag})
	if r := m3.Dispatch(ctx, In{Conv: "c", Text: "!help"}); !r.Handled {
		t.Fatal("no sink")
	}
}

func TestCommandFailure(t *testing.T) {
	ctx := context.Background()
	rs := &recSink{}
	m := newM(t, Config{Agent: &fakeAgent{}, Auth: failAuth{}, Logf: t.Logf})
	m.Dispatch(ctx, In{Conv: "c", Text: "!login p", Sink: rs})
	if !strings.Contains(rs.all(), "Command failed: authenticate p: boom") {
		t.Fatalf("got %q", rs.all())
	}
	fs := &failSink{}
	m.Dispatch(ctx, In{Conv: "c", Text: "!login p", Sink: fs})
	if len(fs.failed) != 1 {
		t.Fatal("Failer not used")
	}
}

func TestSerialRunner(t *testing.T) {
	var mu sync.Mutex
	var order []string
	gate := make(chan struct{})
	var errs []error
	m := newM(t, Config{Agent: &fakeAgent{}, Mode: Serial,
		OnError: func(_ string, err error) { mu.Lock(); errs = append(errs, err); mu.Unlock() }})
	ctx := ctxT(t)
	var afters int
	run := func(name string, wait bool) Job {
		return Job{Conv: "c", Value: name, Run: func(ctx context.Context, tn *Turn) error {
			if wait {
				<-gate
			}
			mu.Lock()
			order = append(order, tn.Value.(string))
			mu.Unlock()
			if name == "b" {
				return errors.New("b failed")
			}
			return nil
		}, After: func(*Turn) { mu.Lock(); afters++; mu.Unlock() }}
	}
	m.Start(ctx, run("a", true))
	m.Start(ctx, run("b", false))
	m.Start(ctx, run("c", false))
	for m.Pending("c") != 2 {
		time.Sleep(time.Millisecond)
	}
	close(gate)
	if err := m.WaitIdle(ctx); err != nil {
		t.Fatal(err)
	}
	if strings.Join(order, "") != "abc" || afters != 3 || len(errs) != 1 {
		t.Fatalf("order=%v afters=%d errs=%v", order, afters, errs)
	}
	// WaitIdle honours ctx while a lane is busy.
	hold := make(chan struct{})
	m.Start(ctx, Job{Conv: "d", Run: func(context.Context, *Turn) error { <-hold; return nil }})
	short, cancel := context.WithTimeout(ctx, 5*time.Millisecond)
	defer cancel()
	if m.WaitIdle(short) == nil {
		t.Fatal("idle while busy")
	}
	close(hold)
	_ = m.WaitIdle(ctx)
	// Default OnError path logs.
	m2 := newM(t, Config{Agent: &fakeAgent{}, Logf: t.Logf})
	m2.Start(ctx, Job{Conv: "x", Run: func(context.Context, *Turn) error { return errors.New("logged") }})
	_ = m2.WaitIdle(ctx)
}

func TestSupersede(t *testing.T) {
	m := newM(t, Config{Agent: &fakeAgent{}, Mode: Supersede})
	ctx := ctxT(t)
	first := make(chan error, 1)
	m.Start(ctx, Job{Conv: "c", Run: func(ctx context.Context, _ *Turn) error {
		<-ctx.Done()
		first <- ctx.Err()
		return nil
	}})
	m.Start(ctx, Job{Conv: "c", Run: func(context.Context, *Turn) error { return nil }})
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatalf("first not superseded: %v", err)
	}
	_ = m.WaitIdle(ctx)
}

func TestControllerHooksAndStatus(t *testing.T) {
	ag := &richAgent{fakeAgent{models: []client.ModelInfo{{ID: "b"}, {ID: "a"}}, current: "a"}}
	now := time.Unix(1000, 0)
	ss := &fakeSessions{live: map[string]acp.SessionId{"c": "s1", "n": "nostats"}, last: now.Add(-time.Minute)}
	var changed string
	m := newM(t, Config{Agent: ag, Sessions: ss, NoStop: true, Version: "v1", AgentCmd: "fir",
		StartTime: now.Add(-time.Hour), Now: func() time.Time { return now },
		Store: badStore{saveErr: errors.New("disk")}, Logf: t.Logf,
		ModelOrder: func(ms []client.ModelInfo) []client.ModelInfo { return ms[1:] },
		Hooks: Hooks{
			Resolve:      func(tok string) (string, bool) { return tok, tok != "missing" },
			ModelChanged: func(_, conv, id string) { changed = conv + "=" + id },
		}})
	ctl := m.Controller()
	if _, ok := ctl.(command.TurnStopper); ok {
		t.Fatal("NoStop still stops")
	}
	if ms, _ := ctl.AvailableModels(); len(ms) != 1 || ms[0].ID != "a" {
		t.Fatalf("order %v", ms)
	}
	if len(ctl.AgentCommands()) != 1 {
		t.Fatal("commands")
	}
	if !errors.Is(ctl.SetModelOverride("missing", "a"), ErrNoConversation) {
		t.Fatal("missing conv")
	}
	if ctl.SetModelOverride("c", "b") == nil {
		t.Fatal("unordered-out model accepted")
	}
	if err := ctl.SetModelOverride("c", "a"); err != nil || changed != "c=a" {
		t.Fatal(err, changed)
	}
	st := ctl.StatusFor("c")
	if !st.HasSession || st.OverrideModel != "a" || st.Cost != "0.50 USD" || st.LastActivity != "1m0s" || st.Thinking != "high" {
		t.Fatalf("status %+v", st)
	}
	if st := ctl.StatusFor("n"); !st.HasSession || st.Thinking != "" {
		t.Fatalf("nostats %+v", st)
	}
	if st := ctl.StatusFor("missing"); st.HasSession || st.EffectiveModel != "a" {
		t.Fatalf("missing %+v", st)
	}
	ss.last = time.Time{}
	if st := ctl.StatusFor("c"); st.LastActivity != "" {
		t.Fatal("zero last")
	}
	ri := ctl.RelayInfo("c")
	if ri.SessionID != "s1" || ri.AgentName != "fir" || ri.Uptime != "1h0m0s" || ri.ActiveSessions != 2 || ri.EffectiveModel != "a" {
		t.Fatalf("relay %+v", ri)
	}
	if ri := ctl.RelayInfo("gone"); ri.SessionID != "" {
		t.Fatal("gone sid")
	}
	if ri := ctl.RelayInfo("missing"); ri.EffectiveModel != "" {
		t.Fatal("missing eff")
	}
	if ctl.ResetSession("missing") != nil || ctl.ResetSession("c") != nil {
		t.Fatal("reset without Resetter")
	}
	if m.EffectiveModel("zz") != "a" {
		t.Fatal("effective default")
	}

	// Decorating hooks, default resolve, no sessions.
	var reset string
	m2 := newM(t, Config{Agent: &fakeAgent{}, Hooks: Hooks{
		Reset: func(_ context.Context, tok string) error { reset = tok; return nil },
		Status: func(_, _ string, _ bool, st command.SessionStatus) command.SessionStatus {
			st.Where = "here"
			return st
		},
		RelayInfo: func(_, _ string, _ bool, ri command.RelayInfo) command.RelayInfo { ri.Version = "hooked"; return ri },
	}})
	c2 := m2.Controller()
	if c2.StatusFor("x").Where != "here" || c2.RelayInfo("x").Version != "hooked" {
		t.Fatal("hooks")
	}
	if c2.SetModelOverride("x", "anything") != nil {
		t.Fatal("empty catalogue")
	}
	_ = c2.ResetSession("tok")
	if reset != "tok" {
		t.Fatal("reset hook")
	}
	ts := c2.(command.TurnStopper)
	if ts.StopTurn("x") {
		t.Fatal("nothing to stop")
	}
	m3 := newM(t, Config{Agent: &fakeAgent{}, Hooks: Hooks{Resolve: func(string) (string, bool) { return "", false }}})
	if m3.Controller().(command.TurnStopper).StopTurn("x") {
		t.Fatal("unresolved stop")
	}
}

func TestSinkFuncTokenAndPlainStats(t *testing.T) {
	var got, tok string
	sf := SinkFunc(func(_ context.Context, in *In, text string) error { got, tok = text, in.token(); return nil })
	m := newM(t, Config{Agent: &fakeAgent{}, Sink: sf,
		Sessions: &fakeSessions{live: map[string]acp.SessionId{"c": "s"}}})
	m.Dispatch(context.Background(), In{Conv: "c", Token: "T", Text: "!status"})
	if tok != "T" || !strings.Contains(got, "active") {
		t.Fatalf("tok=%q got=%q", tok, got)
	}
	if st := m.Controller().StatusFor("c"); !st.HasSession || st.Thinking != "" {
		t.Fatalf("%+v", st)
	}
}

type capsCtl struct{ posted []string }

func (c *capsCtl) PostTo(conv, text string) error { c.posted = append(c.posted, text); return nil }
func (c *capsCtl) CanSchedule() bool              { return true }
func (c *capsCtl) Schedule(string, string, time.Time, time.Duration) (schedule.Item, error) {
	return schedule.Item{}, nil
}
func (c *capsCtl) Schedules(string) []schedule.Item { return nil }
func (c *capsCtl) Unschedule(string, string) error  { return nil }

func TestCapabilitiesReachBroker(t *testing.T) {
	c := &capsCtl{}
	m := newM(t, Config{Agent: &fakeAgent{}, Poster: c, Scheduler: c})
	if !m.Broker().CanPost() || !m.Broker().CanSchedule() {
		t.Fatal("capabilities not wired")
	}
}
