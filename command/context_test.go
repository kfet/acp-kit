package command

import (
	"context"
	"strings"
	"testing"

	"github.com/kfet/acp-kit/client"
)

type senderCtxKey struct{}

// ctxCtrl is a locCtrl that also implements the context-aware
// refinements: it keys sessions and status by the sender on ctx.
type ctxCtrl struct{ locCtrl }

func sender(ctx context.Context) string { s, _ := ctx.Value(senderCtxKey{}).(string); return s }

func (c *ctxCtrl) ACPSessionIDContext(ctx context.Context, convID string) (string, bool) {
	return c.ACPSessionID(convID + "/" + sender(ctx))
}
func (c *ctxCtrl) StatusForContext(ctx context.Context, convID string) SessionStatus {
	return SessionStatus{EffectiveModel: "m-" + sender(ctx), HasSession: true}
}
func (c *ctxCtrl) RelayInfoContext(ctx context.Context, convID string) RelayInfo {
	return RelayInfo{SessionID: "sid-" + sender(ctx)}
}

func (c *ctxCtrl) StopTurn(string) bool { return false }
func (c *ctxCtrl) StopTurnContext(ctx context.Context, _ string) bool {
	return sender(ctx) == "7"
}

// TestContextCapabilities: a Controller implementing ContextStatusReporter
// and ContextSessionLocator gets the command's context, so `!status` and
// `!mcp login` can act on the sender's own session.
func TestContextCapabilities(t *testing.T) {
	a := &sessAuth{fakeAuth: *newFake(), replies: []reply{{res: client.AuthResult{State: "ok"}}}}
	b := New(a)
	b.SetController(&ctxCtrl{locCtrl{sids: map[string]string{"c1/7": "sess-7"}}})
	ctx := context.WithValue(context.Background(), senderCtxKey{}, "7")

	out, err := b.Handle(ctx, "c1", "!status")
	if err != nil || !strings.Contains(out.Text, "model: `m-7`") || !strings.Contains(out.Text, "active `sid-7`") {
		t.Fatalf("status = %v, %v", out, err)
	}
	if _, err := b.Handle(ctx, "c1", "!mcp login gh"); err != nil {
		t.Fatal(err)
	}
	if len(a.sess) != 1 || a.sess[0].sessionID != "sess-7" {
		t.Fatalf("mcp login session = %#v", a.sess)
	}
	// Without the sender the context lookup misses.
	out, _ = b.Handle(context.Background(), "c1", "!mcp login gh")
	if !strings.Contains(out.Text, "no agent session") {
		t.Fatalf("no-sender reply = %q", out.Text)
	}
	if out, _ := b.Handle(ctx, "c1", "!stop"); !strings.Contains(out.Text, "Interrupted") {
		t.Fatalf("stop = %q", out.Text)
	}
	if out, _ := b.Handle(context.Background(), "c1", "!stop"); !strings.Contains(out.Text, "Nothing is running") {
		t.Fatalf("no-sender stop = %q", out.Text)
	}
	// Status (no context) still reaches the context methods.
	if s, _ := b.Status("c1"); !strings.Contains(s, "model: `m-`") {
		t.Fatalf("Status = %q", s)
	}
}
