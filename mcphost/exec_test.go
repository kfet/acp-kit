package mcphost

import (
	"bufio"
	"encoding/json"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// --- the end-to-end proof ----------------------------------------------
//
// TestSurvivesHostRestart is the regression test for the production bug:
// the consumer re-execs itself in place (same PID, new binary), so the
// mcphost.Host is torn down and a fresh one is constructed, while the
// agent's redirector subprocess keeps running untouched. Before the fix
// the redirector's tools vanished for the rest of the session, because
//
//  1. the socket path was a fresh MkdirTemp on every process start,
//  2. Close() unlinked the socket and removed its directory, and
//  3. tokens were minted in memory, so the successor rejected the
//     redirector's existing token,
//
// and the redirector was a one-shot pipe that exited when the socket died.
//
// The test drives a real redirector over a real unix socket, restarts the
// Host underneath it on the same stable path with the token registry
// seeded from the predecessor's exported blob, and asserts the agent's
// tool calls keep working — with `initialize` replayed to the successor,
// whose per-connection MCP state is gone.
func TestSurvivesHostRestart(t *testing.T) {
	shortenRedirBackoff(t)

	dir := filepath.Join(t.TempDir(), "mcp") // stable path, created by New
	rt1, rt2 := &recordTool{}, &recordTool{}

	h1 := hostAt(t, dir, rt1)
	tok := tokenFor(t, h1, "conv-live")
	if err := h1.Listen(); err != nil {
		t.Fatalf("listen: %v", err)
	}

	// The agent side: a live redirector, driven over pipes the way the
	// agent drives it over stdio.
	rd := startRedir(t, h1.SocketPath(), tok)

	rd.request(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`)
	rd.send(t, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if got := rd.call(t, 2, "/before"); got != "did /before" {
		t.Fatalf("pre-restart call = %q", got)
	}

	// --- the exec ---------------------------------------------------
	// Predecessor hands the token registry to the successor through the
	// environment (never disk) and closes WITHOUT unlinking the socket.
	blob := h1.ExportTokens()
	if blob == "" {
		t.Fatal("ExportTokens returned an empty blob")
	}
	if err := h1.CloseForExec(); err != nil {
		t.Fatalf("CloseForExec: %v", err)
	}

	h2 := hostAt(t, dir, rt2)
	if err := h2.SeedTokens(blob); err != nil {
		t.Fatalf("SeedTokens: %v", err)
	}
	if h2.SocketPath() != h1.SocketPath() {
		t.Fatalf("socket path moved across restart: %q -> %q", h1.SocketPath(), h2.SocketPath())
	}
	if err := h2.Listen(); err != nil {
		t.Fatalf("successor listen: %v", err)
	}

	// --- the next turn ----------------------------------------------
	// Same redirector process, same token, new host. This is exactly what
	// broke live: the tools came back "not found" forever.
	if got := rd.call(t, 3, "/after"); got != "did /after" {
		t.Fatalf("post-restart call = %q", got)
	}

	// The successor saw the call bound to the right session key, proving
	// the seeded registry (not a fresh mint) resolved the token.
	rt2.mu.Lock()
	defer rt2.mu.Unlock()
	if !rt2.called || rt2.key != "conv-live" || rt2.path != "/after" {
		t.Fatalf("successor handler: called=%v key=%q path=%q", rt2.called, rt2.key, rt2.path)
	}

	// tools/list still works too — the whole surface came back, not just
	// the one method.
	var m map[string]any
	if err := json.Unmarshal([]byte(rd.request(t, `{"jsonrpc":"2.0","id":4,"method":"tools/list"}`)), &m); err != nil {
		t.Fatalf("decode tools/list: %v", err)
	}
	tools := m["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != "do" {
		t.Fatalf("tools/list after restart = %v", tools)
	}

	rd.close(t)
}

// --- redirector harness -------------------------------------------------

// redirHarness runs a real redirector against a socket, with pipes
// standing in for the agent's stdio.
type redirHarness struct {
	inW  *io.PipeWriter
	outR *bufio.Reader
	errc chan error
}

func startRedir(t *testing.T, socket, token string) *redirHarness {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	h := &redirHarness{inW: inW, outR: bufio.NewReader(outR), errc: make(chan error, 1)}
	go func() {
		err := redirect(socket, token, inR, outW)
		outW.Close()
		h.errc <- err
	}()
	return h
}

func (h *redirHarness) send(t *testing.T, line string) {
	t.Helper()
	if _, err := h.inW.Write([]byte(line + "\n")); err != nil {
		t.Fatalf("write to redir: %v", err)
	}
}

// request writes a request and returns the matching response line.
func (h *redirHarness) request(t *testing.T, line string) string {
	t.Helper()
	h.send(t, line)
	return h.readLine(t)
}

// readLine returns the next line the redirector emitted to the agent.
func (h *redirHarness) readLine(t *testing.T) string {
	t.Helper()
	type result struct {
		line string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		l, err := h.outR.ReadString('\n')
		ch <- result{l, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("read from redir: %v", r.err)
		}
		return strings.TrimSpace(r.line)
	case <-time.After(15 * time.Second):
		t.Fatal("timed out waiting for a response through the redirector")
		return ""
	}
}

// read returns the next decoded line from the redirector.
func (h *redirHarness) read(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(h.readLine(t)), &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return m
}

// expectID asserts the next line the agent sees carries the given id.
func (h *redirHarness) expectID(t *testing.T, want string) map[string]any {
	t.Helper()
	m := h.read(t)
	if got := rawIDOf(m); got != want {
		t.Fatalf("next line to the agent has id %s, want %s (%v)", got, want, m)
	}
	return m
}

// call issues a tools/call for the "do" tool and returns its text.
func (h *redirHarness) call(t *testing.T, id int, path string) string {
	t.Helper()
	req := `{"jsonrpc":"2.0","id":` + strconv.Itoa(id) +
		`,"method":"tools/call","params":{"name":"do","arguments":{"path":"` + path + `"}}}`
	var m map[string]any
	if err := json.Unmarshal([]byte(h.request(t, req)), &m); err != nil {
		t.Fatalf("decode tools/call response: %v", err)
	}
	res, ok := m["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result in %v", m)
	}
	return res["content"].([]any)[0].(map[string]any)["text"].(string)
}

func (h *redirHarness) close(t *testing.T) {
	t.Helper()
	// Keep draining: the proxy writes to the agent synchronously, so a
	// test that stops reading mid-stream would wedge it on stdout.
	drained := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, h.outR); close(drained) }()
	h.inW.Close()
	select {
	case err := <-h.errc:
		if err != nil {
			t.Fatalf("redirect: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("redirector did not exit after stdin EOF")
	}
	<-drained
}

// --- host harness -------------------------------------------------------

// hostAt builds a Host pinned to a caller-supplied stable directory, the
// way a consumer would pin it under its StateDir so the path survives a
// re-exec.
func hostAt(t *testing.T, dir string, rt *recordTool) *Host {
	t.Helper()
	h, err := New(Config{
		Dir:             dir,
		RedirSubcommand: "mcp-serve",
		ServerName:      "test",
		EnvSocket:       "TEST_SOCK",
		EnvToken:        "TEST_TOK",
		RedirCommand:    "/bin/true",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.Tool("do", "does a thing", map[string]any{
		"type":       "object",
		"properties": map[string]any{"path": map[string]any{"type": "string"}},
		"required":   []string{"path"},
	}, rt.handler)
	t.Cleanup(func() { h.Close() })
	return h
}

// tokenFor mints a session token the way session/new would.
func tokenFor(t *testing.T, h *Host, key string) string {
	t.Helper()
	for _, e := range h.ServerConfigForSession(key)[0].Stdio.Env {
		if e.Name == "TEST_TOK" {
			return e.Value
		}
	}
	t.Fatalf("no token in server config for %q", key)
	return ""
}

// shortenRedirBackoff makes the reconnect backoff test-fast.
func shortenRedirBackoff(t *testing.T) {
	t.Helper()
	oMin, oMax, oGive, oRep := redirBackoffMin, redirBackoffMax, redirGiveUp, redirReplayTimeout
	redirBackoffMin, redirBackoffMax = time.Millisecond, 5*time.Millisecond
	redirGiveUp, redirReplayTimeout = 3*time.Second, 300*time.Millisecond
	t.Cleanup(func() {
		redirBackoffMin, redirBackoffMax, redirGiveUp, redirReplayTimeout = oMin, oMax, oGive, oRep
	})
}
