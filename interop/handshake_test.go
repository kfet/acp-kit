package interop

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/kfet/acp-kit/mcphost"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func plainHost(t *testing.T) *mcphost.Host {
	t.Helper()
	h, err := mcphost.New(hostConfig(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	h.Tool("echo", "echoes", map[string]any{"type": "object"}, func(_ string, args json.RawMessage) (string, error) {
		return "echo " + string(args), nil
	})
	listen(t, h)
	return h
}

// TestNoSilentFallback is the guard for the v0.45.0 bug: by default the
// real client must negotiate the newest version the SDK speaks, through
// server/discover, never through a fallback initialize. If the SDK
// moves to a newer revision mcphost does not advertise, this fails —
// on purpose: decide, do not silently degrade.
func TestNoSilentFallback(t *testing.T) {
	h := plainHost(t)
	cs, tp := connect(t, h, firCaps(), "")
	if got := cs.InitializeResult().ProtocolVersion; got != latest() {
		t.Fatalf("negotiated %s, the SDK's newest is %s: the client silently fell back", got, latest())
	}
	m := tp.methods()
	if len(m) == 0 || m[0] != "server/discover" || slices.Contains(m, "initialize") {
		t.Fatalf("handshake = %v, want server/discover and no initialize", m)
	}
	if si := cs.InitializeResult().ServerInfo; si == nil || si.Name != "interop" || si.Version != "1.2.3" {
		t.Fatalf("serverInfo = %+v", si)
	}
	if _, ok := cs.InitializeResult().Capabilities.Extensions[mcphost.FilesExtension]; !ok {
		t.Fatalf("files extension missing: %+v", cs.InitializeResult().Capabilities)
	}
}

// TestEveryVersionNegotiates: each protocol version the SDK supports,
// requested explicitly, is the version the session ends up on, over the
// handshake that version calls for, and a tool round-trips on it.
func TestEveryVersionNegotiates(t *testing.T) {
	h := plainHost(t)
	for _, v := range sdk.SupportedProtocolVersions() {
		t.Run(v, func(t *testing.T) {
			cs, tp := connect(t, h, firCaps(), v)
			if got := cs.InitializeResult().ProtocolVersion; got != v {
				t.Fatalf("asked for %s, negotiated %s", v, got)
			}
			want := "initialize"
			if v >= "2026-07-28" {
				want = "server/discover"
			}
			if m := tp.methods(); len(m) == 0 || m[0] != want {
				t.Fatalf("handshake = %v, want %s first", m, want)
			}
			if err := cs.Ping(ctxT(t), nil); err != nil {
				t.Fatalf("ping: %v", err)
			}
			if r := callTool(t, cs, &sdk.CallToolParams{Name: "echo", Arguments: map[string]any{"a": 1}}); r.IsError || text(r) != `echo {"a":1}` {
				t.Fatalf("echo = %q", text(r))
			}
			if v < "2026-07-28" && !slices.Contains(tp.methods(), "notifications/initialized") {
				t.Fatalf("legacy handshake sent no notifications/initialized: %v", tp.methods())
			}
			golden(t, "handshake-"+v, tp)
		})
	}
}

// TestErrors: protocol errors and tool errors reach the real client as
// such, and the connection survives them.
func TestErrors(t *testing.T) {
	h := plainHost(t)
	cs, tp := connect(t, h, firCaps(), "")
	tp.reset()
	if r := callTool(t, cs, &sdk.CallToolParams{Name: "nope"}); !r.IsError {
		t.Fatalf("unknown tool: %q not isError", text(r))
	}
	if _, err := cs.ListPrompts(ctxT(t), nil); err == nil {
		t.Fatal("prompts/list: want method-not-found")
	}
	if r := callTool(t, cs, &sdk.CallToolParams{Name: "echo"}); r.IsError {
		t.Fatalf("connection did not survive errors: %q", text(r))
	}
	golden(t, "errors", tp)
}

// TestNotificationsAndInputResponses: mcphost ignores client
// notifications it has no use for (progress) and MRTR inputResponses
// (it never asks for input), and the session carries on.
func TestNotificationsAndInputResponses(t *testing.T) {
	h := plainHost(t)
	cs, tp := connect(t, h, firCaps(), "")
	if err := cs.NotifyProgress(ctxT(t), &sdk.ProgressNotificationParams{ProgressToken: "p1", Progress: 1}); err != nil {
		t.Fatalf("progress: %v", err)
	}
	r := callTool(t, cs, &sdk.CallToolParams{Name: "echo", Arguments: map[string]any{}, InputResponses: sdk.InputResponseMap{
		"q": &sdk.ElicitResult{Action: "accept"},
	}})
	if r.IsError || text(r) != "echo {}" {
		t.Fatalf("echo after notification + inputResponses = %q", text(r))
	}
	if !slices.Contains(tp.methods(), "notifications/progress") {
		t.Fatalf("progress notification not sent: %v", tp.methods())
	}
}
