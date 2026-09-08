package mcphost

import (
	"bufio"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- Config.Dir ---------------------------------------------------------

func TestNew_StableDirIsCreatedAndTightened(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "mcp")
	h := hostAt(t, dir, &recordTool{})
	if h.ownDir {
		t.Error("a caller-supplied Dir is not ours to remove")
	}
	if h.SocketPath() != filepath.Join(dir, "mcp.sock") {
		t.Errorf("SocketPath = %q", h.SocketPath())
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Errorf("socket dir perms = %o, want 0700", fi.Mode().Perm())
	}
}

// A pre-existing group/other-readable directory is tightened, not
// accepted as-is: a consumer's StateDir subtree is commonly 0755.
func TestNew_StableDirPreExistingLoosePerms(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "loose")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	hostAt(t, dir, &recordTool{})
	fi, _ := os.Stat(dir)
	if fi.Mode().Perm() != 0o700 {
		t.Errorf("perms = %o, want 0700", fi.Mode().Perm())
	}
}

func TestNew_StableDirMkdirError(t *testing.T) {
	f := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(f, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Config{
		Dir: f, RedirSubcommand: "s", ServerName: "n",
		EnvSocket: "S", EnvToken: "T", RedirCommand: "/bin/true",
	}); err == nil {
		t.Fatal("want an error when Dir is not a directory")
	}
}

func TestNew_StableDirChmodError(t *testing.T) {
	old := osChmod
	osChmod = func(string, os.FileMode) error { return errors.New("chmod boom") }
	defer func() { osChmod = old }()
	if _, err := New(Config{
		Dir: filepath.Join(t.TempDir(), "d"), RedirSubcommand: "s", ServerName: "n",
		EnvSocket: "S", EnvToken: "T", RedirCommand: "/bin/true",
	}); err == nil {
		t.Fatal("want an error when the socket dir cannot be tightened")
	}
}

// Close must not delete a directory the caller owns — that would undo the
// whole point of a stable path.
func TestClose_KeepsCallerSuppliedDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "mcp")
	h := hostAt(t, dir, &recordTool{})
	if err := h.Listen(); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("caller-supplied dir removed by Close: %v", err)
	}
	if _, err := os.Stat(h.SocketPath()); !os.IsNotExist(err) {
		t.Errorf("Close should still unlink the socket: %v", err)
	}
}

// CloseForExec leaves the socket path intact for the successor.
func TestCloseForExec_KeepsSocketPath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "mcp")
	h := hostAt(t, dir, &recordTool{})
	if err := h.Listen(); err != nil {
		t.Fatal(err)
	}
	if err := h.CloseForExec(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(h.SocketPath()); err != nil {
		t.Fatalf("CloseForExec unlinked the socket: %v", err)
	}
}

// Shutdown must not hang while a client is attached: an attached
// redirector holds its connection open indefinitely, so closing the
// listener alone would leave wg.Wait blocked forever.
func TestClose_DropsAttachedConnections(t *testing.T) {
	h := hostWithTool(t, &recordTool{})
	tok := tokenFor(t, h, "c")
	if err := h.Listen(); err != nil {
		t.Fatal(err)
	}
	c := dialPreamble(t, h.SocketPath(), tok)
	defer c.Close()
	// Make sure the server-side handler is actually running.
	_, _ = c.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n"))
	if _, err := bufio.NewReader(c).ReadBytes('\n'); err != nil {
		t.Fatalf("ping: %v", err)
	}
	done := make(chan struct{})
	go func() { h.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Close hung with a client attached")
	}
}

// A connection accepted while the Host is already shutting down is
// dropped rather than handed to a handler.
func TestServe_RefusesConnectionsWhileClosing(t *testing.T) {
	h := hostWithTool(t, &recordTool{})
	tok := tokenFor(t, h, "c")
	if err := h.Listen(); err != nil {
		t.Fatal(err)
	}
	h.connMu.Lock()
	h.closed = true
	h.connMu.Unlock()
	c, err := net.Dial("unix", h.SocketPath())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, _ = c.Write([]byte(`{"token":"` + tok + `"}` + "\n"))
	// The connection is dropped without a handler ever running, so the
	// read ends immediately — cleanly or with a reset, both are "gone".
	if err := c.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	n, _ := io.Copy(io.Discard, c)
	if n != 0 {
		t.Fatalf("a refused connection produced %d bytes of MCP traffic", n)
	}
}

// --- Listen: stale vs live socket ---------------------------------------

// After a same-PID exec the socket file left behind was OURS, so removing
// it is correct rather than racy. The same holds for a crashed process.
func TestListen_RemovesStaleSocket(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "mcp.sock")
	// A leftover socket file nobody is listening on.
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close()
	if _, err := os.Stat(sock); err != nil {
		t.Fatalf("stale socket not left behind: %v", err)
	}
	h := hostAt(t, dir, &recordTool{})
	if err := h.Listen(); err != nil {
		t.Fatalf("Listen over a stale socket: %v", err)
	}
}

// A genuinely live holder must not be stolen from.
func TestListen_RefusesLiveHolder(t *testing.T) {
	dir := t.TempDir()
	h1 := hostAt(t, dir, &recordTool{})
	if err := h1.Listen(); err != nil {
		t.Fatal(err)
	}
	h2 := hostAt(t, dir, &recordTool{})
	err := h2.Listen()
	if err == nil {
		t.Fatal("want a refusal to bind over a live listener")
	}
	if !strings.Contains(err.Error(), "live process") {
		t.Fatalf("err = %v", err)
	}
}

func TestListen_StaleSocketRemoveError(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "mcp")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(sub, "mcp.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close()
	h := hostAt(t, sub, &recordTool{})
	// Make the parent read-only so the unlink fails.
	if err := os.Chmod(sub, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(sub, 0o700) })
	if err := h.Listen(); err == nil || !strings.Contains(err.Error(), "stale socket") {
		t.Fatalf("want a stale-socket removal error, got %v", err)
	}
}

// --- Token registry across the exec -------------------------------------

func TestSeedTokens_EmptyBlobIsANoop(t *testing.T) {
	h := newTestHost(t)
	if err := h.SeedTokens(""); err != nil {
		t.Fatalf("SeedTokens(\"\"): %v", err)
	}
}

func TestSeedTokens_RotatesAnExistingKey(t *testing.T) {
	h1 := newTestHost(t)
	tok1 := tokenFor(t, h1, "conv")

	h2 := newTestHost(t)
	stale := tokenFor(t, h2, "conv") // h2 minted its own first
	if err := h2.SeedTokens(h1.ExportTokens()); err != nil {
		t.Fatalf("SeedTokens: %v", err)
	}
	if key, ok := h2.resolve(tok1); !ok || key != "conv" {
		t.Fatalf("seeded token does not resolve: %q %v", key, ok)
	}
	if _, ok := h2.resolve(stale); ok {
		t.Fatal("the superseded token still resolves")
	}
}

func TestSeedTokens_Rejects(t *testing.T) {
	h := newTestHost(t)
	if err := h.SeedTokens("!!!not base64!!!"); err == nil {
		t.Error("want a decode error")
	}
	if err := h.SeedTokens(base64.RawURLEncoding.EncodeToString([]byte("not json"))); err == nil {
		t.Error("want a parse error")
	}
	future := base64.RawURLEncoding.EncodeToString([]byte(`{"v":99,"tokens":{}}`))
	if err := h.SeedTokens(future); err == nil {
		t.Error("want a version error")
	}
}

// forward() must report failure (not spin) when it cannot even get a
// connection to write to.
func TestRedir_ForwardCannotConnect(t *testing.T) {
	shortenRedirBackoff(t)
	withDial(t, func(string) (net.Conn, error) { return nil, errors.New("no host") })
	p := &proxy{socket: "/ignored", token: "t", out: &lockedBuffer{}, pending: map[string]bool{}}
	if p.forward([]byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n")) {
		t.Fatal("want forward to fail when the host is unreachable")
	}
	if p.err == nil {
		t.Fatal("want a recorded error")
	}
}

// --- fromAgent bail-out -------------------------------------------------

// When the host is unreachable for a write even on a fresh connection,
// the agent-side reader must stop rather than spin.
func TestRedir_AgentLoopStopsWhenUndeliverable(t *testing.T) {
	shortenRedirBackoff(t)
	sock := filepath.Join(t.TempDir(), "mcp.sock")
	fakeHost(t, sock, echoHost)
	withDial(t, func(s string) (net.Conn, error) {
		c, err := net.Dial("unix", s)
		if err != nil {
			return nil, err
		}
		return &failAfterConn{Conn: c, after: 1}, nil // preamble only
	})
	inR, inW := io.Pipe()
	go func() {
		inW.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n"))
	}()
	defer inW.Close()
	done := make(chan error, 1)
	go func() { done <- redirect(sock, "tok", inR, &lockedBuffer{}) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("want an undeliverable error")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("proxy hung on an undeliverable request")
	}
}
