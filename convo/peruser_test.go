package convo

import (
	"context"
	"strings"
	"sync"
	"testing"

	acp "github.com/coder/acp-go-sdk"
	"github.com/kfet/acp-kit/client"
	"github.com/kfet/acp-kit/command"
)

// perUserAgent can drive `!mcp login` (SessionAuthenticator) and
// reports a model per session.
type perUserAgent struct {
	modelAgent
	mu   sync.Mutex
	sids []string
}

func (a *perUserAgent) AuthMethods() []client.AuthMethod { return nil }
func (a *perUserAgent) Authenticate(context.Context, string, string, string, bool) (client.AuthResult, error) {
	return client.AuthResult{}, nil
}
func (a *perUserAgent) AuthenticateSession(_ context.Context, sid, _, _, _ string, _ bool) (client.AuthResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sids = append(a.sids, sid)
	return client.AuthResult{State: "ok"}, nil
}

// perUserKey is zulip-acp's thread_user key shape: conv, or conv-u<user>.
func perUserKey(conv, sender string) string {
	if sender == "" {
		return conv
	}
	return conv + "-u" + sender
}

// TestPerUserSessionCommands: with Hooks.SessionKey, `!status`, `!stop`
// and `!mcp login` act on the SENDER's own session, not the conversation's
// bare key — and each sender sees their own.
func TestPerUserSessionCommands(t *testing.T) {
	ag := &perUserAgent{modelAgent: modelAgent{
		fakeAgent: fakeAgent{models: []client.ModelInfo{{ID: "a"}, {ID: "b"}}, current: "a"},
		per:       map[acp.SessionId]string{"s7": "b"},
	}}
	ss := &fakeSessions{live: map[string]acp.SessionId{"c-u7": "s7", "c-u8": "s8"}}
	sink := &recSink{}
	m := newM(t, Config{Agent: ag, Sessions: ss, Sink: sink, Hooks: Hooks{SessionKey: perUserKey}})
	ctx := ctxT(t)

	m.Dispatch(ctx, In{Conv: "c", Sender: "7", Text: "!status"})
	got := sink.all()
	if !strings.Contains(got, "session: active `s7`") || !strings.Contains(got, "model: `b`") {
		t.Fatalf("sender 7 status:\n%s", got)
	}
	sink.replies = nil
	m.Dispatch(ctx, In{Conv: "c", Sender: "8", Text: "!status"})
	if got := sink.all(); !strings.Contains(got, "session: active `s8`") || !strings.Contains(got, "model: `a`") {
		t.Fatalf("sender 8 status:\n%s", got)
	}
	sink.replies = nil
	m.Dispatch(ctx, In{Conv: "c", Sender: "9", Text: "!status"})
	if got := sink.all(); !strings.Contains(got, "none yet") {
		t.Fatalf("sender with no session:\n%s", got)
	}

	sink.replies = nil
	m.Dispatch(ctx, In{Conv: "c", Sender: "8", Text: "!mcp login gh"})
	if len(ag.sids) != 1 || ag.sids[0] != "s8" {
		t.Fatalf("mcp login sessionId = %v, reply:\n%s", ag.sids, sink.all())
	}

	// `!status` reports, and `!stop` stops, the sender's own turn.
	cancelled := false
	m.Active().Begin("c-u7", func() { cancelled = true }, nil)
	sink.replies = nil
	m.Dispatch(ctx, In{Conv: "c", Sender: "8", Text: "!status"})
	m.Dispatch(ctx, In{Conv: "c", Sender: "8", Text: "!stop"})
	if got := sink.all(); strings.Contains(got, "turn running") || !strings.Contains(got, "Nothing is running") || cancelled {
		t.Fatalf("sender 8 saw sender 7's turn:\n%s", got)
	}
	sink.replies = nil
	m.Dispatch(ctx, In{Conv: "c", Sender: "7", Text: "!status"})
	m.Dispatch(ctx, In{Conv: "c", Sender: "7", Text: "!stop"})
	if got := sink.all(); !strings.Contains(got, "turn running: yes") || !strings.Contains(got, "Interrupted") || !cancelled {
		t.Fatalf("sender 7 own turn:\n%s", got)
	}
	if len(ss.cancels) != 1 || ss.cancels[0] != "c-u7" {
		t.Fatalf("session/cancel went to %v", ss.cancels)
	}

	// A command with no sender asks the hook with "" — here the shared
	// key, which has no session.
	sink.replies = nil
	m.Dispatch(ctx, In{Conv: "c", Text: "!mcp login gh"})
	if got := sink.all(); !strings.Contains(got, "no agent session") || len(ag.sids) != 1 {
		t.Fatalf("no-sender mcp login: %v\n%s", ag.sids, got)
	}

	// Context-free reads (relaytool, a relay's own panel) know no
	// sender either.
	ctl := m.Controller()
	if st := ctl.StatusFor("c"); st.HasSession {
		t.Fatal("context-free StatusFor found a per-user session")
	}
	if ri := ctl.RelayInfo("c"); ri.SessionID != "" {
		t.Fatalf("context-free RelayInfo session %q", ri.SessionID)
	}
	if _, ok := ctl.(command.SessionLocator).ACPSessionID("c"); ok {
		t.Fatal("context-free ACPSessionID found a per-user session")
	}
}

// TestSharedSessionIgnoresSender: without Hooks.SessionKey a sender
// changes nothing — one session per conversation, as before.
func TestSharedSessionIgnoresSender(t *testing.T) {
	ag := &perUserAgent{modelAgent: modelAgent{per: map[acp.SessionId]string{}}}
	ss := &fakeSessions{live: map[string]acp.SessionId{"c": "s1"}}
	sink := &recSink{}
	m := newM(t, Config{Agent: ag, Sessions: ss, Sink: sink})
	ctx := ctxT(t)

	m.Dispatch(ctx, In{Conv: "c", Sender: "7", Text: "!status"})
	if got := sink.all(); !strings.Contains(got, "session: active `s1`") {
		t.Fatalf("status:\n%s", got)
	}
	m.Dispatch(ctx, In{Conv: "c", Sender: "7", Text: "!mcp login gh"})
	if len(ag.sids) != 1 || ag.sids[0] != "s1" {
		t.Fatalf("mcp login sessionId = %v", ag.sids)
	}
	if ri := m.Controller().RelayInfo("c"); ri.SessionID != "s1" {
		t.Fatalf("RelayInfo session %q", ri.SessionID)
	}
	cancelled := false
	m.Active().Begin("c", func() { cancelled = true }, nil)
	if !m.Controller().(command.TurnStopper).StopTurn("c") || !cancelled {
		t.Fatal("context-free StopTurn missed the shared turn")
	}
}
