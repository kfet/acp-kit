package relaytool

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kfet/acp-kit/client"
	"github.com/kfet/acp-kit/command"
	"github.com/kfet/acp-kit/mcphost"
	"github.com/kfet/acp-kit/schedule"
)

// --- fakes ---------------------------------------------------------------

// ctrl is a command.Controller. loopback controls whether it also
// satisfies Poster and Scheduler.
type ctrl struct {
	models   []client.ModelInfo
	current  string
	setErr   error
	resets   []string
	resetErr error
}

func (c *ctrl) AvailableModels() ([]client.ModelInfo, string) { return c.models, c.current }
func (c *ctrl) SetModelOverride(string, string) error         { return c.setErr }
func (c *ctrl) ResetSession(conv string) error {
	c.resets = append(c.resets, conv)
	return c.resetErr
}
func (c *ctrl) StatusFor(string) command.SessionStatus {
	return command.SessionStatus{EffectiveModel: "m1"}
}
func (c *ctrl) AgentCommands() []client.CommandInfo { return nil }
func (c *ctrl) RelayInfo(string) command.RelayInfo  { return command.RelayInfo{Version: "test"} }

// loopCtrl adds the optional Poster and Scheduler capabilities.
type loopCtrl struct {
	ctrl
	posted   [][2]string
	postErr  error
	armed    []schedule.Item
	addErr   error
	unschErr error
}

func (c *loopCtrl) PostTo(conv, text string) error {
	c.posted = append(c.posted, [2]string{conv, text})
	return c.postErr
}

func (c *loopCtrl) Schedule(conv, text string, at time.Time, every time.Duration) (schedule.Item, error) {
	if c.addErr != nil {
		return schedule.Item{}, c.addErr
	}
	it := schedule.Item{ID: "s01", Conv: conv, Text: text, At: at, Every: every, Depth: 1}
	c.armed = append(c.armed, it)
	return it, nil
}

func (c *loopCtrl) CanSchedule() bool                { return true }
func (c *loopCtrl) Schedules(string) []schedule.Item { return c.armed }
func (c *loopCtrl) Unschedule(string, string) error  { return c.unschErr }

// --- harness -------------------------------------------------------------

func newTools(t *testing.T, c command.Controller, tweak func(*Config)) *Tools {
	t.Helper()
	b := command.New(nil)
	b.SetController(c)
	cfg := Config{Broker: b}
	if tweak != nil {
		tweak(&cfg)
	}
	tools, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return tools
}

// call invokes a tool by name with the given session key and arguments.
func call(t *testing.T, tools *Tools, name, sessionKey, args string) (string, error) {
	t.Helper()
	for _, x := range tools.Tools() {
		if x.Name == name {
			return x.Handler(sessionKey, json.RawMessage(args))
		}
	}
	t.Fatalf("tool %q is not registered", name)
	return "", nil
}

func names(tools *Tools) []string {
	out := make([]string, 0, len(tools.Tools()))
	for _, x := range tools.Tools() {
		out = append(out, x.Name)
	}
	return out
}

func has(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// --- tests ---------------------------------------------------------------

func TestNewRequiresBroker(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("want error without a Broker")
	}
}

// TestNoTurnDestroyingTools pins the rule the package doc states: a
// loopback tool must never destroy the turn calling it. `stop` has no
// tool at all, and `new_session` exists only in its deferred form.
func TestNoTurnDestroyingTools(t *testing.T) {
	got := names(newTools(t, &loopCtrl{}, nil))
	if has(got, "stop") {
		t.Fatalf("stop must not be exposed: %v", got)
	}
	if !has(got, ToolNewSession) {
		t.Fatalf("new_session missing: %v", got)
	}
}

func TestCapabilityGating(t *testing.T) {
	plain := names(newTools(t, &ctrl{}, nil))
	for _, n := range []string{ToolStatus, ToolListModels, ToolSetModel, ToolNewSession} {
		if !has(plain, n) {
			t.Fatalf("%s missing from the base surface: %v", n, plain)
		}
	}
	for _, n := range []string{ToolPost, ToolSchedule, ToolListSchedules, ToolUnschedule} {
		if has(plain, n) {
			t.Fatalf("%s advertised without the capability: %v", n, plain)
		}
	}
	full := names(newTools(t, &loopCtrl{}, nil))
	for _, n := range []string{ToolPost, ToolSchedule, ToolListSchedules, ToolUnschedule} {
		if !has(full, n) {
			t.Fatalf("%s missing with the capability: %v", n, full)
		}
	}
}

// TestConversationIsNeverAnArgument is the identity guarantee: no tool
// schema may accept anything that names a conversation.
func TestConversationIsNeverAnArgument(t *testing.T) {
	banned := []string{"conv", "conversation", "conv_id", "channel", "topic", "session", "to", "target", "user"}
	for _, x := range newTools(t, &loopCtrl{}, nil).Tools() {
		props, _ := x.Schema["properties"].(map[string]any)
		for name := range props {
			for _, b := range banned {
				if strings.Contains(name, b) {
					t.Fatalf("tool %s takes %q — a conversation must come from the token, never an argument", x.Name, name)
				}
			}
		}
	}
}

func TestConvTokenMapping(t *testing.T) {
	c := &loopCtrl{}
	tools := newTools(t, c, func(cfg *Config) {
		cfg.ConvToken = func(k string) (string, bool) {
			if k == "known" {
				return "tok:known", true
			}
			return "", false
		}
	})
	if _, err := call(t, tools, ToolStatus, "gone", `{}`); err == nil {
		t.Fatal("want refusal for an unmapped session key")
	}
	if _, err := call(t, tools, ToolPost, "known", `{"text":"hi"}`); err != nil {
		t.Fatalf("post: %v", err)
	}
	if len(c.posted) != 1 || c.posted[0][0] != "tok:known" {
		t.Fatalf("post did not use the mapped token: %v", c.posted)
	}
}

func TestStatusAndModels(t *testing.T) {
	c := &ctrl{models: []client.ModelInfo{{ID: "m1"}, {ID: "m2"}}, current: "m1"}
	tools := newTools(t, c, nil)

	got, err := call(t, tools, ToolStatus, "conv", ``)
	if err != nil || !strings.Contains(got, "model: `m1`") {
		t.Fatalf("status = %q, %v", got, err)
	}
	got, err = call(t, tools, ToolListModels, "conv", `{"filter":"m2"}`)
	if err != nil || !strings.Contains(got, "m2") || strings.Contains(got, "- `m1`") {
		t.Fatalf("list_models = %q, %v", got, err)
	}
	if _, err := call(t, tools, ToolListModels, "conv", `{`); err == nil {
		t.Fatal("want decode error")
	}
}

func TestSetModel(t *testing.T) {
	c := &ctrl{models: []client.ModelInfo{{ID: "m1"}}}
	tools := newTools(t, c, nil)
	if _, err := call(t, tools, ToolSetModel, "conv", `{}`); err == nil {
		t.Fatal("want error for a missing model_id")
	}
	if _, err := call(t, tools, ToolSetModel, "conv", `{`); err == nil {
		t.Fatal("want decode error")
	}
	got, err := call(t, tools, ToolSetModel, "conv", `{"model_id":"m1"}`)
	if err != nil || !strings.Contains(got, "m1") {
		t.Fatalf("set_model = %q, %v", got, err)
	}
	c.setErr = errors.New("unknown model")
	if _, err := call(t, tools, ToolSetModel, "conv", `{"model_id":"m9"}`); err == nil {
		t.Fatal("want the Controller's error surfaced")
	}
}

// TestNewSessionIsDeferred is the whole justification for exposing it:
// the tool must not touch the session until the turn has ended.
func TestNewSessionIsDeferred(t *testing.T) {
	c := &ctrl{}
	var logs []string
	tools := newTools(t, c, func(cfg *Config) {
		cfg.Logf = func(f string, _ ...any) { logs = append(logs, f) }
	})
	got, err := call(t, tools, ToolNewSession, "conv", `{}`)
	if err != nil || !strings.Contains(got, "armed") {
		t.Fatalf("new_session = %q, %v", got, err)
	}
	if len(c.resets) != 0 {
		t.Fatal("new_session reset the session mid-turn — it must be deferred")
	}
	tools.EndTurn("conv")
	if len(c.resets) != 1 || c.resets[0] != "conv" {
		t.Fatalf("EndTurn did not apply the reset: %v", c.resets)
	}
	// A second EndTurn does nothing: the intent was consumed.
	tools.EndTurn("conv")
	if len(c.resets) != 1 {
		t.Fatalf("deferred reset fired twice: %v", c.resets)
	}
	// A turn that armed nothing is a no-op.
	tools.EndTurn("other")
	if len(c.resets) != 1 {
		t.Fatalf("EndTurn reset a conversation that asked for nothing: %v", c.resets)
	}
	// A failing reset is logged, not panicked.
	c.resetErr = errors.New("nope")
	if _, err := call(t, tools, ToolNewSession, "conv", `{}`); err != nil {
		t.Fatalf("new_session: %v", err)
	}
	tools.EndTurn("conv")
	if len(logs) != 2 {
		t.Fatalf("logs = %v, want the success and the failure", logs)
	}
}

func TestPostTool(t *testing.T) {
	c := &loopCtrl{}
	tools := newTools(t, c, nil)
	if _, err := call(t, tools, ToolPost, "conv", `{`); err == nil {
		t.Fatal("want decode error")
	}
	if _, err := call(t, tools, ToolPost, "conv", `{"text":""}`); err == nil {
		t.Fatal("want refusal for empty text")
	}
	got, err := call(t, tools, ToolPost, "conv", `{"text":"progress"}`)
	if err != nil || !strings.Contains(got, "Posted") {
		t.Fatalf("post = %q, %v", got, err)
	}
	c.postErr = errors.New("send failed")
	if _, err := call(t, tools, ToolPost, "conv", `{"text":"x"}`); err == nil {
		t.Fatal("want the relay's error surfaced")
	}
}

func TestScheduleTool(t *testing.T) {
	c := &loopCtrl{}
	tools := newTools(t, c, nil)

	if _, err := call(t, tools, ToolSchedule, "conv", `{`); err == nil {
		t.Fatal("want decode error")
	}
	if _, err := call(t, tools, ToolSchedule, "conv", `{"text":"x"}`); err == nil {
		t.Fatal("want error with neither in nor at")
	}
	if _, err := call(t, tools, ToolSchedule, "conv", `{"text":"x","in":"1h","at":"2026-09-02T09:00:00Z"}`); err == nil {
		t.Fatal("want error with both in and at")
	}
	if _, err := call(t, tools, ToolSchedule, "conv", `{"text":"x","in":"soon"}`); err == nil {
		t.Fatal("want error for an unparsable duration")
	}
	if _, err := call(t, tools, ToolSchedule, "conv", `{"text":"x","at":"tomorrow"}`); err == nil {
		t.Fatal("want error for an unparsable time")
	}
	if _, err := call(t, tools, ToolSchedule, "conv", `{"text":"x","in":"1h","every":"often"}`); err == nil {
		t.Fatal("want error for an unparsable repeat")
	}

	got, err := call(t, tools, ToolSchedule, "conv", `{"text":" check it ","at":"2026-09-02T09:00:00Z","every":"24h"}`)
	if err != nil || !strings.Contains(got, "s01") || !strings.Contains(got, "repeating every 24h") {
		t.Fatalf("schedule = %q, %v", got, err)
	}
	if c.armed[0].Text != "check it" {
		t.Fatalf("text not trimmed: %q", c.armed[0].Text)
	}
	if _, err := call(t, tools, ToolSchedule, "conv", `{"text":"x","in":"90m"}`); err != nil {
		t.Fatalf("relative schedule: %v", err)
	}

	c.addErr = errors.New("cap reached")
	if _, err := call(t, tools, ToolSchedule, "conv", `{"text":"x","in":"1h"}`); err == nil {
		t.Fatal("want the store's error surfaced")
	}
}

func TestListAndUnscheduleTools(t *testing.T) {
	c := &loopCtrl{armed: []schedule.Item{{ID: "s01", At: time.Unix(0, 0).UTC(), Text: "x"}}}
	tools := newTools(t, c, nil)
	got, err := call(t, tools, ToolListSchedules, "conv", ``)
	if err != nil || !strings.Contains(got, "s01") {
		t.Fatalf("list_schedules = %q, %v", got, err)
	}
	if _, err := call(t, tools, ToolUnschedule, "conv", `{`); err == nil {
		t.Fatal("want decode error")
	}
	out, err := call(t, tools, ToolUnschedule, "conv", `{"id":"s01"}`)
	if err != nil || !strings.Contains(out, "s01") {
		t.Fatalf("unschedule = %q, %v", out, err)
	}
	c.unschErr = errors.New("no such schedule")
	if _, err := call(t, tools, ToolUnschedule, "conv", `{"id":"s99"}`); err == nil {
		t.Fatal("want the store's error surfaced")
	}
}

// TestRegisterOnAHost proves the data-shaped tool set actually installs
// on a real mcphost.Host.
func TestRegisterOnAHost(t *testing.T) {
	h, err := mcphost.New(mcphost.Config{
		BaseDir:         t.TempDir(),
		RedirCommand:    "/bin/true",
		RedirSubcommand: "mcp-serve",
		ServerName:      "relay",
		EnvSocket:       "S",
		EnvToken:        "T",
	})
	if err != nil {
		t.Fatalf("mcphost.New: %v", err)
	}
	defer func() { _ = h.Close() }()
	newTools(t, &loopCtrl{}, nil).Register(h)
}

// TestNoControllerSurfacesAsToolErrors: a Broker with no Controller
// still registers the base tools, and each reports the failure rather
// than pretending.
func TestNoControllerSurfacesAsToolErrors(t *testing.T) {
	b := command.New(nil)
	tools, err := New(Config{Broker: b})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, name := range []string{ToolStatus, ToolListModels} {
		if _, err := call(t, tools, name, "conv", `{}`); !errors.Is(err, command.ErrNoController) {
			t.Fatalf("%s err = %v", name, err)
		}
	}
}

// TestScheduleToolsAreDefensive covers the capability check inside the
// scheduling handlers. Tools() never registers them without a
// Scheduler, so this is only reachable by building the set directly —
// which is exactly why the branch must stay: it is what would keep the
// tool honest if the gating in Tools() ever changed.
func TestScheduleToolsAreDefensive(t *testing.T) {
	tools := newTools(t, &ctrl{}, nil)
	for _, x := range tools.scheduleTools() {
		if x.Name != ToolListSchedules {
			continue
		}
		if _, err := x.Handler("conv", nil); err == nil {
			t.Fatal("want a refusal without the Scheduler capability")
		}
	}
}

func TestDecodeAcceptsNoArguments(t *testing.T) {
	tools := newTools(t, &ctrl{models: []client.ModelInfo{{ID: "m1"}}, current: "m1"}, nil)
	if _, err := call(t, tools, ToolListModels, "conv", ``); err != nil {
		t.Fatalf("empty arguments: %v", err)
	}
}

func TestDefaultLogfIsSilent(t *testing.T) {
	c := &ctrl{resetErr: errors.New("nope")}
	tools := newTools(t, c, nil) // no Logf configured
	if _, err := call(t, tools, ToolNewSession, "conv", `{}`); err != nil {
		t.Fatalf("new_session: %v", err)
	}
	tools.EndTurn("conv")
}
