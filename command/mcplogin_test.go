package command

import (
	"context"
	"errors"
	"strings"
	"testing"

	acp "github.com/coder/acp-go-sdk"
	"github.com/kfet/acp-kit/client"
)

// sessAuth is a fakeAuth that can also authenticate within a session.
type sessAuth struct {
	fakeAuth
	sess []sessCall
	// replies are consumed one per AuthenticateSession call; the last
	// one repeats.
	replies []reply
}

type sessCall struct {
	sessionID string
	call
}

type reply struct {
	res client.AuthResult
	err error
}

func (s *sessAuth) AuthenticateSession(_ context.Context, sessionID, methodID, id, redirect string, cancel bool) (client.AuthResult, error) {
	s.sess = append(s.sess, sessCall{sessionID, call{methodID, id, redirect, cancel}})
	r := s.replies[0]
	if len(s.replies) > 1 {
		s.replies = s.replies[1:]
	}
	return r.res, r.err
}

// locCtrl is a Controller that also implements SessionLocator.
type locCtrl struct {
	fakeCtrl
	sids map[string]string
}

func (c *locCtrl) ACPSessionID(convID string) (string, bool) {
	sid, ok := c.sids[convID]
	return sid, ok
}

func mcpBroker(replies ...reply) (*Broker, *sessAuth) {
	a := &sessAuth{fakeAuth: *newFake(), replies: replies}
	b := New(a)
	b.SetController(&locCtrl{
		fakeCtrl: fakeCtrl{agentCmds: []client.CommandInfo{{Name: "mcp", Description: "MCP servers"}}},
		sids:     map[string]string{"c1": "sess-1"},
	})
	return b, a
}

var needsRedirect = reply{res: client.AuthResult{State: "needs_redirect", ID: "p-1", URL: "https://idp/authorize", Instructions: "pick an org"}}

func TestMCPLogin_StartPasteComplete(t *testing.T) {
	ctx := context.Background()
	b, a := mcpBroker(needsRedirect, reply{res: client.AuthResult{State: "ok"}})

	if !b.IsCommand("!mcp login github") || !b.IsCommand("!MCP Login github") {
		t.Fatal("mcp login <server> must be a broker command")
	}
	out, err := b.Handle(ctx, "c1", "!mcp login github")
	if err != nil || out == nil {
		t.Fatalf("Handle = %v, %v", out, err)
	}
	for _, want := range []string{"https://idp/authorize", "`github`", "paste", "even if", "pick an org", "!mcp login cancel"} {
		if !strings.Contains(out.Text, want) {
			t.Errorf("reply missing %q: %s", want, out.Text)
		}
	}
	if out.URL != "https://idp/authorize" || out.Instructions != "pick an org" {
		t.Errorf("outcome = %#v", out)
	}
	if !b.HasPending("c1") {
		t.Fatal("expected a pending login")
	}
	want1 := sessCall{"sess-1", call{method: "mcp:github"}}
	if len(a.sess) != 1 || a.sess[0] != want1 {
		t.Fatalf("call 1 = %#v", a.sess)
	}
	if len(a.calls) != 0 {
		t.Fatalf("plain Authenticate must not be used: %#v", a.calls)
	}

	out, _ = b.Handle(ctx, "c1", "  https://localhost:1/cb?code=x&state=y ")
	if !strings.Contains(out.Text, "MCP server `github` is authorised") {
		t.Fatalf("complete reply = %q", out.Text)
	}
	want2 := sessCall{"sess-1", call{"mcp:github", "p-1", "https://localhost:1/cb?code=x&state=y", false}}
	if len(a.sess) != 2 || a.sess[1] != want2 {
		t.Fatalf("call 2 = %#v", a.sess)
	}
	if b.HasPending("c1") {
		t.Fatal("pending must clear")
	}
}

func TestMCPLogin_CompleteFailureAndStates(t *testing.T) {
	ctx := context.Background()
	b, _ := mcpBroker(needsRedirect, reply{err: errors.New("state mismatch")})
	b.Handle(ctx, "c1", "!mcp login github")
	out, _ := b.Handle(ctx, "c1", "https://bad")
	if !strings.Contains(out.Text, "state mismatch") || !strings.Contains(out.Text, "`!mcp login github`") {
		t.Fatalf("failure reply = %q", out.Text)
	}

	b, _ = mcpBroker(needsRedirect, reply{res: client.AuthResult{State: "cancelled"}})
	b.Handle(ctx, "c1", "!mcp login github")
	if out, _ := b.Handle(ctx, "c1", "https://x"); out.Text != "Login cancelled." {
		t.Fatalf("cancelled = %q", out.Text)
	}

	b, _ = mcpBroker(needsRedirect)
	b.Handle(ctx, "c1", "!mcp login github")
	if out, _ := b.Handle(ctx, "c1", "   "); !strings.Contains(out.Text, "`!mcp login cancel`") {
		t.Fatalf("empty paste = %q", out.Text)
	}
}

func TestMCPLogin_Cancel(t *testing.T) {
	ctx := context.Background()
	for _, cmd := range []string{"!mcp login cancel", "!login cancel", "!mcp LOGIN Cancel"} {
		b, a := mcpBroker(needsRedirect, reply{res: client.AuthResult{State: "cancelled"}})
		b.Handle(ctx, "c1", "!mcp login github")
		out, _ := b.Handle(ctx, "c1", cmd)
		if out.Text != "Login cancelled." {
			t.Fatalf("%s: reply = %q", cmd, out.Text)
		}
		want := sessCall{"sess-1", call{"mcp:github", "p-1", "", true}}
		if len(a.sess) != 2 || a.sess[1] != want {
			t.Fatalf("%s: cancel call = %#v", cmd, a.sess)
		}
		if b.HasPending("c1") {
			t.Fatalf("%s: pending must clear", cmd)
		}
	}

	// Agent rejects the cancel: still cancelled locally.
	b, _ := mcpBroker(needsRedirect, reply{err: errors.New("gone")})
	b.Handle(ctx, "c1", "!mcp login github")
	if out, _ := b.Handle(ctx, "c1", "!mcp login cancel"); !strings.Contains(out.Text, "gone") {
		t.Fatalf("cancel err reply = %q", out.Text)
	}

	// Nothing pending.
	b, _ = mcpBroker(needsRedirect)
	if !b.IsCommand("!mcp login cancel") {
		t.Fatal("mcp login cancel must be a command")
	}
	if out, _ := b.Handle(ctx, "c1", "!mcp login cancel"); out.Text != "No login in progress." {
		t.Fatalf("idle cancel = %q", out.Text)
	}
}

func TestMCPLogin_AlreadyPending(t *testing.T) {
	ctx := context.Background()
	b, a := mcpBroker(needsRedirect)
	b.Handle(ctx, "c1", "!mcp login github")
	// While pending, a sigil command is the paste path — so test the
	// guard directly, as a second start would reach it.
	out, _ := b.startMCP(ctx, "c1", "slack")
	if !strings.Contains(out.Text, "already in progress (mcp:github)") || !strings.Contains(out.Text, "`!mcp login cancel`") {
		t.Fatalf("already pending = %q", out.Text)
	}
	if len(a.sess) != 1 {
		t.Fatalf("no second authenticate expected: %#v", a.sess)
	}
	// And a provider login started over a pending mcp login.
	out, _ = b.start(ctx, "c1", "anthropic")
	if !strings.Contains(out.Text, "`!mcp login cancel`") {
		t.Fatalf("provider over mcp = %q", out.Text)
	}
}

func TestMCPLogin_StartOutcomes(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		r    reply
		want string
	}{
		{"ok", reply{res: client.AuthResult{State: "ok"}}, "MCP server `github` is authorised"},
		{"empty", reply{}, "MCP server `github` is authorised"},
		{"cancelled", reply{res: client.AuthResult{State: "cancelled"}}, "Login cancelled."},
		{"weird", reply{res: client.AuthResult{State: "weird"}}, `unexpected state: "weird"`},
		{"agent error", reply{err: acp.NewInternalError(map[string]any{"error": "unknown MCP server: github"})}, "Couldn't start login for MCP server `github`"},
	}
	for _, c := range cases {
		b, _ := mcpBroker(c.r)
		out, err := b.Handle(ctx, "c1", "!mcp login github")
		if err != nil || out == nil || !strings.Contains(out.Text, c.want) {
			t.Errorf("%s: %v, %v", c.name, out, err)
		}
		if b.HasPending("c1") {
			t.Errorf("%s: nothing should be pending", c.name)
		}
	}
}

// An agent that does not know mcp:* ids (old fir) falls back to the
// passthrough: Handle declines and Passthrough rewrites.
func TestMCPLogin_OldAgentFallsBackToPassthrough(t *testing.T) {
	ctx := context.Background()
	for _, err := range []error{
		acp.NewInternalError(map[string]any{"error": "unknown auth method: mcp:github"}),
		acp.NewMethodNotFound("authenticate"),
	} {
		b, _ := mcpBroker(reply{err: err})
		out, herr := b.Handle(ctx, "c1", "!mcp login github")
		if out != nil || herr != nil {
			t.Fatalf("%v: Handle = %v, %v; want nil, nil", err, out, herr)
		}
		if b.HasPending("c1") {
			t.Fatal("nothing should be pending")
		}
		if rw, ok := b.Passthrough("!mcp login github"); !ok || rw != "/mcp login github" {
			t.Fatalf("passthrough = %q, %v", rw, ok)
		}
	}
}

func TestMCPLogin_NoSession(t *testing.T) {
	b, a := mcpBroker(needsRedirect)
	out, _ := b.Handle(context.Background(), "c-new", "!mcp login github")
	if !strings.Contains(out.Text, "no agent session") || !strings.Contains(out.Text, "`!mcp login github`") {
		t.Fatalf("no-session reply = %q", out.Text)
	}
	if len(a.sess) != 0 {
		t.Fatal("must not authenticate without a session")
	}
}

// Other mcp subcommands, a bare `mcp login`, and brokers without the
// capabilities all stay plain passthrough.
func TestMCPLogin_PassthroughCases(t *testing.T) {
	ctx := context.Background()
	b, _ := mcpBroker(needsRedirect)
	for _, text := range []string{"!mcp", "!mcp logout github", "!mcp reload", "!mcp login", "!mcp login   ", "!mcplogin x", "!mcp loginx y"} {
		if b.IsCommand(text) {
			t.Errorf("%q must not be a broker command", text)
		}
		if out, _ := b.Handle(ctx, "c1", text); out != nil {
			t.Errorf("%q: Handle = %v", text, out)
		}
	}

	// Plain Authenticator (no session capability).
	plain := New(newFake())
	plain.SetController(&locCtrl{sids: map[string]string{"c1": "s"}})
	// Session-capable authenticator but a Controller without a locator.
	noLoc := New(&sessAuth{fakeAuth: *newFake(), replies: []reply{needsRedirect}})
	noLoc.SetController(&fakeCtrl{})
	// No Controller at all.
	noCtrl := New(&sessAuth{fakeAuth: *newFake(), replies: []reply{needsRedirect}})
	for name, br := range map[string]*Broker{"plain": plain, "noLoc": noLoc, "noCtrl": noCtrl} {
		if br.IsCommand("!mcp login github") || br.IsCommand("!mcp login cancel") {
			t.Errorf("%s: mcp login must stay passthrough", name)
		}
		if out, _ := br.Handle(ctx, "c1", "!mcp login github"); out != nil {
			t.Errorf("%s: Handle = %v", name, out)
		}
		if out, _ := br.Handle(ctx, "c1", "!mcp login cancel"); out != nil {
			t.Errorf("%s: idle cancel = %v", name, out)
		}
		if strings.Contains(br.help().Text, "mcp login") {
			t.Errorf("%s: help must not list mcp login", name)
		}
	}
}

func TestMCPLogin_Help(t *testing.T) {
	b, _ := mcpBroker(needsRedirect)
	h := b.help().Text
	if !strings.Contains(h, "`!mcp login <server>|cancel`") {
		t.Fatalf("help missing mcp login: %s", h)
	}
	if !strings.Contains(SkillMarkdown(), "`!mcp login <server>|cancel`") {
		t.Fatal("skill missing mcp login")
	}
}

func TestMustSessionAuth_Panics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic")
		}
	}()
	mustSessionAuth(newFake())
}

func TestMCPLogin_NewSessionDropsPending(t *testing.T) {
	ctx := context.Background()
	b, a := mcpBroker(needsRedirect, reply{res: client.AuthResult{State: "cancelled"}})
	b.Handle(ctx, "c1", "!mcp login github")
	if err := b.NewSession("c1"); err != nil {
		t.Fatal(err)
	}
	if b.HasPending("c1") {
		t.Fatal("mcp login must not outlive its session")
	}
	want := sessCall{"sess-1", call{"mcp:github", "p-1", "", true}}
	if len(a.sess) != 2 || a.sess[1] != want {
		t.Fatalf("cancel = %#v", a.sess)
	}

	// A provider login survives a reset.
	f := newFake()
	f.res = client.AuthResult{State: "needs_redirect", ID: "x"}
	pb := New(f)
	pb.SetController(&fakeCtrl{})
	pb.Handle(ctx, "c1", "!login anthropic")
	if err := pb.NewSession("c1"); err != nil || !pb.HasPending("c1") {
		t.Fatalf("provider login dropped: %v", err)
	}
	// A failed reset keeps the mcp login.
	b, _ = mcpBroker(needsRedirect)
	b.ctrl.(*locCtrl).resetErr = errors.New("no")
	b.Handle(ctx, "c1", "!mcp login github")
	if err := b.NewSession("c1"); err == nil || !b.HasPending("c1") {
		t.Fatalf("failed reset: %v", err)
	}
}

func TestRekeyMovesPendingLogin(t *testing.T) {
	ctx := context.Background()
	b, a := mcpBroker(needsRedirect, reply{res: client.AuthResult{State: "cancelled"}})
	b.Handle(ctx, "c1", "!mcp login github")
	b.Rekey("c1", "c1") // no-op
	if !b.HasPending("c1") {
		t.Fatal("a no-op Rekey dropped the login")
	}
	b.Rekey("c1", "moved")
	if b.HasPending("c1") || !b.HasPending("moved") {
		t.Fatal("the pending login did not follow the conversation")
	}
	// Nothing pending under from: nothing happens.
	b.Rekey("c1", "elsewhere")
	if b.HasPending("elsewhere") || !b.HasPending("moved") {
		t.Fatal("a Rekey with nothing to move changed state")
	}
	if len(a.sess) != 1 {
		t.Fatalf("a move cancelled something: %#v", a.sess)
	}
}

func TestRekeyClashKeepsDestination(t *testing.T) {
	ctx := context.Background()
	b, a := mcpBroker(needsRedirect, needsRedirect, reply{res: client.AuthResult{State: "cancelled"}})
	b.ctrl.(*locCtrl).sids["c2"] = "sess-2"
	b.Handle(ctx, "c1", "!mcp login github")
	b.Handle(ctx, "c2", "!mcp login github")
	b.Rekey("c1", "c2")
	if b.HasPending("c1") || !b.HasPending("c2") {
		t.Fatal("a clash must keep the destination's login and drop the source's")
	}
	want := sessCall{"sess-1", call{"mcp:github", "p-1", "", true}}
	if len(a.sess) != 3 || a.sess[2] != want {
		t.Fatalf("the source login was not cancelled: %#v", a.sess)
	}
}
