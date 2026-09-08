package mcphost

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- Redirector: happy path ---------------------------------------------

func TestRedir_PreambleAndBidirectionalCopy(t *testing.T) {
	rt := &recordTool{}
	h := hostWithTool(t, rt)
	tok := tokenFor(t, h, "conv-9")
	if err := h.Listen(); err != nil {
		t.Fatal(err)
	}
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"do","arguments":{"path":"/redir/p"}}}` + "\n")
	var out lockedBuffer
	if err := redirect(h.SocketPath(), tok, in, &out); err != nil {
		t.Fatalf("redirect: %v", err)
	}
	if !strings.Contains(out.String(), "did /redir/p") {
		t.Fatalf("want server reply piped to stdout, got %q", out.String())
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if !rt.called || rt.path != "/redir/p" {
		t.Fatalf("not driven through redir: called=%v path=%q", rt.called, rt.path)
	}
}

// A line the agent sends without a trailing newline must still be framed
// correctly on the wire.
func TestRedir_UnterminatedFinalLine(t *testing.T) {
	rt := &recordTool{}
	h := hostWithTool(t, rt)
	tok := tokenFor(t, h, "c")
	if err := h.Listen(); err != nil {
		t.Fatal(err)
	}
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"do","arguments":{"path":"/nonl"}}}`)
	var out lockedBuffer
	if err := redirect(h.SocketPath(), tok, in, &out); err != nil {
		t.Fatalf("redirect: %v", err)
	}
	if !strings.Contains(out.String(), "did /nonl") {
		t.Fatalf("unterminated line not forwarded: %q", out.String())
	}
}

func TestRedir_DialError(t *testing.T) {
	shortenRedirBackoff(t)
	err := redirect("/no/such/socket", "tok", strings.NewReader(""), &lockedBuffer{})
	if err == nil {
		t.Fatal("want dial error")
	}
	if !strings.Contains(err.Error(), "dial") {
		t.Fatalf("err = %v", err)
	}
}

// --- Redirector: reconnect ---------------------------------------------

// TestRedir_ReconnectReplaysInitialize proves the proxy is stateful: the
// successor host is a fresh process that never saw this session's
// initialize, so the proxy must replay it — and must NOT leak the
// replay's response to the agent.
func TestRedir_ReconnectReplaysInitialize(t *testing.T) {
	shortenRedirBackoff(t)
	sock := filepath.Join(t.TempDir(), "mcp.sock")

	type conn struct {
		inits int
	}
	var mu sync.Mutex
	seen := &conn{}

	ln := fakeHost(t, sock, func(c net.Conn, br *bufio.Reader) {
		for {
			line, err := br.ReadBytes('\n')
			if len(line) > 0 {
				var m rpcMessage
				if json.Unmarshal(line, &m) == nil && len(m.ID) > 0 {
					if m.Method == "initialize" {
						mu.Lock()
						seen.inits++
						mu.Unlock()
					}
					writeResult(c, m.ID, map[string]any{"ok": m.Method})
				}
			}
			if err != nil {
				return
			}
		}
	})

	rd := newProxyHarness(t, sock, "tok")
	rd.send(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"x"}}`)
	rd.expectID(t, "1")
	rd.send(t, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	rd.send(t, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	rd.expectID(t, "2")

	// Drop every live connection, as a re-exec does.
	ln.dropConns()

	rd.send(t, `{"jsonrpc":"2.0","id":3,"method":"tools/list"}`)
	// The very next thing the agent sees must be its own id 3 — never the
	// replayed initialize's response.
	rd.expectID(t, "3")

	mu.Lock()
	defer mu.Unlock()
	if seen.inits != 2 {
		t.Fatalf("initialize seen %d times, want 2 (original + replay)", seen.inits)
	}
	rd.close(t)
}

// An in-flight request must be answered — with a JSON-RPC error, not a
// replay (a tools/call may already have had its side effect) and never
// with silence.
func TestRedir_InFlightRequestFailsCleanly(t *testing.T) {
	shortenRedirBackoff(t)
	sock := filepath.Join(t.TempDir(), "mcp.sock")

	block := make(chan struct{})
	ln := fakeHost(t, sock, func(c net.Conn, br *bufio.Reader) {
		for {
			line, err := br.ReadBytes('\n')
			if len(line) > 0 {
				var m rpcMessage
				if json.Unmarshal(line, &m) == nil && len(m.ID) > 0 {
					if m.Method == "slow" {
						<-block // never answer: the connection dies first
						return
					}
					writeResult(c, m.ID, map[string]any{})
				}
			}
			if err != nil {
				return
			}
		}
	})
	defer close(block)

	rd := newProxyHarness(t, sock, "tok")
	rd.send(t, `{"jsonrpc":"2.0","id":7,"method":"slow"}`)
	// Wait until the host has definitely taken the request.
	time.Sleep(50 * time.Millisecond)
	ln.dropConns()

	m := rd.read(t)
	if rawIDOf(m) != "7" {
		t.Fatalf("want a response for the in-flight id 7, got %v", m)
	}
	e, ok := m["error"].(map[string]any)
	if !ok {
		t.Fatalf("want a JSON-RPC error for the lost request, got %v", m)
	}
	if int(e["code"].(float64)) != redirErrConnLost {
		t.Fatalf("error code = %v", e["code"])
	}
	rd.close(t)
}

// With no host ever coming back, the proxy must give up and EXIT so the
// agent sees a dead server rather than hanging forever.
func TestRedir_GivesUpAndExits(t *testing.T) {
	shortenRedirBackoff(t)
	sock := filepath.Join(t.TempDir(), "mcp.sock")
	ln := fakeHost(t, sock, func(net.Conn, *bufio.Reader) {})

	rd := newProxyHarness(t, sock, "tok")
	ln.shutdown() // listener closed and socket unlinked: gone for good

	select {
	case err := <-rd.errc:
		if err == nil {
			t.Fatal("want an error when the host never returns")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("proxy hung instead of giving up")
	}
	rd.inW.Close()
}

// The agent's writes must also drive a reconnect when the read side has
// not noticed the drop yet.
func TestRedir_ForwardReconnects(t *testing.T) {
	shortenRedirBackoff(t)
	sock := filepath.Join(t.TempDir(), "mcp.sock")
	ln := fakeHost(t, sock, echoHost)

	rd := newProxyHarness(t, sock, "tok")
	rd.send(t, `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	rd.expectID(t, "1")

	ln.dropConns()
	// Hammer the write side; whichever goroutine notices first, the agent
	// must still get an answer.
	for i := 0; i < 5; i++ {
		rd.send(t, `{"jsonrpc":"2.0","id":9,"method":"ping"}`)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		m := rd.read(t)
		if rawIDOf(m) == "9" && m["result"] != nil {
			rd.close(t)
			return
		}
	}
	t.Fatal("no post-reconnect response for id 9")
}

// A stray line on a fresh connection, arriving before the replayed
// initialize's response, is forwarded rather than dropped.
func TestRedir_ReplayForwardsStrayLine(t *testing.T) {
	shortenRedirBackoff(t)
	sock := filepath.Join(t.TempDir(), "mcp.sock")
	var round int
	var mu sync.Mutex
	ln := fakeHost(t, sock, func(c net.Conn, br *bufio.Reader) {
		mu.Lock()
		round++
		r := round
		mu.Unlock()
		for {
			line, err := br.ReadBytes('\n')
			if len(line) > 0 {
				var m rpcMessage
				if json.Unmarshal(line, &m) == nil && len(m.ID) > 0 {
					if r > 1 && m.Method == "initialize" {
						// Emit an unrelated notification-shaped line first.
						_, _ = c.Write([]byte(`{"jsonrpc":"2.0","method":"notifications/message","params":{}}` + "\n"))
					}
					writeResult(c, m.ID, map[string]any{})
				}
			}
			if err != nil {
				return
			}
		}
	})

	rd := newProxyHarness(t, sock, "tok")
	rd.send(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	rd.expectID(t, "1")
	ln.dropConns()
	rd.send(t, `{"jsonrpc":"2.0","id":2,"method":"ping"}`)

	sawStray := false
	for i := 0; i < 3; i++ {
		m := rd.read(t)
		if m["method"] == "notifications/message" {
			sawStray = true
			continue
		}
		if rawIDOf(m) == "2" {
			break
		}
	}
	if !sawStray {
		t.Fatal("stray line on the fresh connection was dropped")
	}
	rd.close(t)
}

// --- Redirector: dial/replay failure branches ---------------------------

// stubConn wraps a net.Pipe end so a test can make Write fail.
type stubConn struct {
	net.Conn
	writeErr error
}

func (s *stubConn) Write(b []byte) (int, error) {
	if s.writeErr != nil {
		return 0, s.writeErr
	}
	return s.Conn.Write(b)
}

func withDial(t *testing.T, f func(string) (net.Conn, error)) {
	t.Helper()
	old := redirDial
	redirDial = f
	t.Cleanup(func() { redirDial = old })
}

func TestRedir_PreambleWriteError(t *testing.T) {
	shortenRedirBackoff(t)
	withDial(t, func(string) (net.Conn, error) {
		a, b := net.Pipe()
		b.Close()
		return &stubConn{Conn: a, writeErr: errors.New("nope")}, nil
	})
	err := redirect("/ignored", "tok", strings.NewReader(""), &lockedBuffer{})
	if err == nil || !strings.Contains(err.Error(), "preamble") {
		t.Fatalf("want preamble write error, got %v", err)
	}
}

// net.Pipe does not implement CloseWrite: exercise halfCloseWrite's
// unsupported branch. Real unix sockets take the other one everywhere
// else in this file.
func TestHalfCloseWrite_Unsupported(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	halfCloseWrite(a) // must not panic and must leave the conn usable
	go func() { _, _ = a.Write([]byte("x")) }()
	buf := make([]byte, 1)
	if _, err := b.Read(buf); err != nil {
		t.Fatalf("conn unusable after a no-op half-close: %v", err)
	}
}

func TestRedir_ReplayWriteError(t *testing.T) {
	shortenRedirBackoff(t)
	sock := filepath.Join(t.TempDir(), "mcp.sock")
	ln := fakeHost(t, sock, echoHost)

	var round int
	var mu sync.Mutex
	withDial(t, func(s string) (net.Conn, error) {
		c, err := net.Dial("unix", s)
		if err != nil {
			return nil, err
		}
		mu.Lock()
		round++
		second := round == 2
		mu.Unlock()
		if second {
			// Preamble succeeds, the replayed initialize does not.
			return &failAfterConn{Conn: c, after: 1}, nil
		}
		return c, nil
	})

	rd := newProxyHarness(t, sock, "tok")
	rd.send(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	rd.expectID(t, "1")
	ln.dropConns()

	select {
	case err := <-rd.errc:
		if err == nil || !strings.Contains(err.Error(), "replay initialize") {
			t.Fatalf("want replay write error, got %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("proxy hung on a failing replay")
	}
	rd.inW.Close()
}

func TestRedir_ReplayReadError(t *testing.T) {
	shortenRedirBackoff(t)
	sock := filepath.Join(t.TempDir(), "mcp.sock")
	var round int
	var mu sync.Mutex
	ln := fakeHost(t, sock, func(c net.Conn, br *bufio.Reader) {
		mu.Lock()
		round++
		r := round
		mu.Unlock()
		for {
			line, err := br.ReadBytes('\n')
			if len(line) > 0 {
				var m rpcMessage
				if json.Unmarshal(line, &m) == nil && len(m.ID) > 0 {
					if r > 1 {
						continue // never answer the replayed initialize
					}
					writeResult(c, m.ID, map[string]any{})
				}
			}
			if err != nil {
				return
			}
		}
	})

	rd := newProxyHarness(t, sock, "tok")
	rd.send(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	rd.expectID(t, "1")
	ln.dropConns()

	select {
	case err := <-rd.errc:
		if err == nil {
			t.Fatal("want an error when the replay is never answered")
		}
	case <-time.After(25 * time.Second):
		t.Fatal("proxy hung on an unanswered replay")
	}
	rd.inW.Close()
}

func TestRedir_ReplayInitializedNoteWriteError(t *testing.T) {
	shortenRedirBackoff(t)
	sock := filepath.Join(t.TempDir(), "mcp.sock")
	ln := fakeHost(t, sock, echoHost)

	var round int
	var mu sync.Mutex
	withDial(t, func(s string) (net.Conn, error) {
		c, err := net.Dial("unix", s)
		if err != nil {
			return nil, err
		}
		mu.Lock()
		round++
		second := round == 2
		mu.Unlock()
		if second {
			// preamble + replayed initialize go through; the replayed
			// notifications/initialized does not.
			return &failAfterConn{Conn: c, after: 2}, nil
		}
		return c, nil
	})

	rd := newProxyHarness(t, sock, "tok")
	rd.send(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	rd.expectID(t, "1")
	rd.send(t, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	time.Sleep(30 * time.Millisecond)
	ln.dropConns()

	select {
	case err := <-rd.errc:
		if err == nil || !strings.Contains(err.Error(), "replay initialized") {
			t.Fatalf("want replay-initialized error, got %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("proxy hung")
	}
	rd.inW.Close()
}

// forward() gives up after a fresh connection also refuses the write.
func TestRedir_ForwardUndeliverable(t *testing.T) {
	shortenRedirBackoff(t)
	p := &proxy{socket: "/ignored", token: "t", out: &lockedBuffer{}, pending: map[string]bool{}}
	withDial(t, func(string) (net.Conn, error) {
		a, b := net.Pipe()
		go func() { bufio.NewReader(b).ReadBytes('\n'); b.Close() }()
		return &failAfterConn{Conn: a, after: 1}, nil
	})
	if p.forward([]byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n")) {
		t.Fatal("want forward to report failure")
	}
	if p.err == nil {
		t.Fatal("want a recorded error")
	}
}

// --- RunRedir / MaybeRunRedir ------------------------------------------

func TestRunRedir_MissingSocket(t *testing.T) {
	os.Unsetenv("TEST_SOCK")
	if err := RunRedir(RedirConfig{EnvSocket: "TEST_SOCK", EnvToken: "TEST_TOK"}); err == nil {
		t.Fatal("want error when socket env unset")
	}
}

func TestRunRedir_Happy(t *testing.T) {
	h := hostWithTool(t, &recordTool{})
	tok := tokenFor(t, h, "c1")
	if err := h.Listen(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_SOCK", h.SocketPath())
	t.Setenv("TEST_TOK", tok)
	pr, pw, _ := os.Pipe()
	oldStdin := os.Stdin
	os.Stdin = pr
	defer func() { os.Stdin = oldStdin }()
	pw.Close() // immediate EOF on stdin
	if err := RunRedir(RedirConfig{EnvSocket: "TEST_SOCK", EnvToken: "TEST_TOK"}); err != nil {
		t.Fatalf("RunRedir: %v", err)
	}
}

func TestMaybeRunRedir(t *testing.T) {
	shortenRedirBackoff(t)
	oldArgs := os.Args
	defer func() { os.Args = oldArgs }()

	// No args → not handled.
	os.Args = []string{"prog"}
	if handled, _ := MaybeRunRedir(RedirConfig{Subcommand: "mcp-serve"}); handled {
		t.Fatal("no args should not be handled")
	}

	// Non-matching subcommand → not handled.
	os.Args = []string{"prog", "serve"}
	if handled, _ := MaybeRunRedir(RedirConfig{Subcommand: "mcp-serve", Aliases: []string{"mcp-attach"}}); handled {
		t.Fatal("non-matching arg should not be handled")
	}

	// Matching subcommand → handled (RunRedir errors: env unset).
	os.Unsetenv("TEST_SOCK")
	os.Args = []string{"prog", "mcp-serve"}
	handled, err := MaybeRunRedir(RedirConfig{Subcommand: "mcp-serve", EnvSocket: "TEST_SOCK", EnvToken: "TEST_TOK"})
	if !handled || err == nil {
		t.Fatalf("want handled=true with error, got handled=%v err=%v", handled, err)
	}

	// Matching alias → handled.
	os.Args = []string{"prog", "mcp-attach"}
	handled, _ = MaybeRunRedir(RedirConfig{Subcommand: "mcp-serve", Aliases: []string{"mcp-attach"}, EnvSocket: "TEST_SOCK", EnvToken: "TEST_TOK"})
	if !handled {
		t.Fatal("alias should be handled")
	}
}

// --- helpers ------------------------------------------------------------

// lockedBuffer is a bytes.Buffer safe for the proxy's concurrent writers.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// failAfterConn lets the first `after` writes through and fails the rest.
type failAfterConn struct {
	net.Conn
	mu    sync.Mutex
	n     int
	after int
}

func (f *failAfterConn) Write(b []byte) (int, error) {
	f.mu.Lock()
	f.n++
	fail := f.n > f.after
	f.mu.Unlock()
	if fail {
		return 0, errors.New("write refused")
	}
	return f.Conn.Write(b)
}

// fakeHost is a minimal stand-in for a Host: it accepts on sock, eats the
// preamble line, and runs handler per connection. dropConns simulates the
// process being replaced; shutdown makes the host gone for good.
type fakeHostSrv struct {
	ln    net.Listener
	mu    sync.Mutex
	conns []net.Conn
	sock  string
}

func fakeHost(t *testing.T, sock string, handler func(net.Conn, *bufio.Reader)) *fakeHostSrv {
	t.Helper()
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("fake host listen: %v", err)
	}
	f := &fakeHostSrv{ln: ln, sock: sock}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			f.mu.Lock()
			f.conns = append(f.conns, c)
			f.mu.Unlock()
			go func() {
				defer c.Close()
				br := bufio.NewReaderSize(c, 1<<20)
				if _, err := br.ReadBytes('\n'); err != nil { // preamble
					return
				}
				handler(c, br)
			}()
		}
	}()
	t.Cleanup(f.shutdown)
	return f
}

func (f *fakeHostSrv) dropConns() {
	f.mu.Lock()
	conns := f.conns
	f.conns = nil
	f.mu.Unlock()
	for _, c := range conns {
		c.Close()
	}
}

func (f *fakeHostSrv) shutdown() {
	f.ln.Close()
	os.Remove(f.sock)
	f.dropConns()
}

// shutdownAfter stops the host once n further connections have been made.
func (f *fakeHostSrv) shutdownAfter(int) {
	f.dropConns()
	go func() { time.Sleep(time.Second); f.shutdown() }()
}

func echoHost(c net.Conn, br *bufio.Reader) {
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			var m rpcMessage
			if json.Unmarshal(line, &m) == nil && len(m.ID) > 0 {
				writeResult(c, m.ID, map[string]any{})
			}
		}
		if err != nil {
			return
		}
	}
}

func writeResult(w net.Conn, id json.RawMessage, result any) {
	b, _ := json.Marshal(rpcMessage{JSONRPC: "2.0", ID: id, Result: result})
	_, _ = w.Write(append(b, '\n'))
}

func rawIDOf(m map[string]any) string {
	b, _ := json.Marshal(m["id"])
	return string(b)
}

// newProxyHarness drives a live proxy over pipes, as the agent drives it
// over stdio.
func newProxyHarness(t *testing.T, sock, token string) *redirHarness {
	t.Helper()
	return startRedir(t, sock, token)
}

// --- response bookkeeping ----------------------------------------------

// The agent must never see two answers for one id. A host that answered
// just before it died leaves the real response sitting in the buffer while
// the write side already failed the request, so a response the proxy is no
// longer waiting for must be dropped.
func TestDeliver_DropsUnexpectedResponse(t *testing.T) {
	var out lockedBuffer
	p := &proxy{out: &out, pending: map[string]bool{"1": true}}

	p.deliver([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}` + "\n"))
	p.deliver([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}` + "\n")) // late duplicate
	if n := strings.Count(out.String(), `"id":1`); n != 1 {
		t.Fatalf("agent saw %d responses for id 1, want exactly 1:\n%s", n, out.String())
	}

	// Host-originated traffic (anything with a method) is not a response
	// to us and must still be forwarded.
	p.deliver([]byte(`{"jsonrpc":"2.0","method":"notifications/message"}` + "\n"))
	p.deliver([]byte(`{"jsonrpc":"2.0","id":9,"method":"roots/list"}` + "\n"))
	if !strings.Contains(out.String(), "notifications/message") {
		t.Error("host notification was dropped")
	}
	if !strings.Contains(out.String(), "roots/list") {
		t.Error("host request was dropped")
	}
}

// Once the proxy has given up, a late line from the agent must not start a
// fresh 30s reconnect.
func TestForward_StopsOnceClosing(t *testing.T) {
	dialed := false
	withDial(t, func(string) (net.Conn, error) {
		dialed = true
		return nil, errors.New("unreachable")
	})
	p := &proxy{out: &lockedBuffer{}, pending: map[string]bool{}, closing: true}
	start := time.Now()
	if p.forward([]byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n")) {
		t.Fatal("want forward to refuse once the proxy is closing")
	}
	if dialed {
		t.Error("a closing proxy must not redial")
	}
	if time.Since(start) > time.Second {
		t.Error("forward blocked instead of returning immediately")
	}
}
