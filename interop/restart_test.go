package interop

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kfet/acp-kit/relaytool"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestHostRestart: the consumer re-execs, the Host is rebuilt on the same
// socket path with the token registry handed over, and the real client's
// session — same redirector subprocess — keeps working, on both
// handshakes (the redirector replays initialize on the legacy one).
func TestHostRestart(t *testing.T) {
	for _, v := range []string{"", "2025-06-18"} {
		t.Run("v="+v, func(t *testing.T) {
			d, err := os.MkdirTemp("/tmp", "ir") // short: unix socket path limit
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.RemoveAll(d) })
			dir := filepath.Join(d, "m")
			h1, _, _ := relayHost(t, dir)
			if err := h1.Listen(); err != nil {
				t.Fatal(err)
			}
			cs, _ := connect(t, h1, firCaps(), v)
			if r := callTool(t, cs, &sdk.CallToolParams{Name: relaytool.ToolStatus}); r.IsError {
				t.Fatalf("before: %q", text(r))
			}
			blob := h1.ExportTokens()
			if err := h1.CloseForExec(); err != nil {
				t.Fatal(err)
			}

			h2, _, _ := relayHost(t, dir)
			if err := h2.SeedTokens(blob); err != nil {
				t.Fatal(err)
			}
			listen(t, h2)
			if r := callTool(t, cs, &sdk.CallToolParams{Name: relaytool.ToolStatus}); r.IsError {
				t.Fatalf("after restart: %q", text(r))
			}
			if got := cs.InitializeResult().ProtocolVersion; v == "" && got != latest() {
				t.Fatalf("protocol %s after restart", got)
			}
			// Close the session while the host is still up: a redirector
			// whose host is gone redials and does not notice stdin EOF
			// until it gives up (see BACKLOG), so the SDK would wait out
			// its terminate grace.
			_ = cs.Close()
		})
	}
}
