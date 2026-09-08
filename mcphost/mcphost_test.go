package mcphost

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
)

// --- test tools ---------------------------------------------------------

// recordTool is a Handler that records its last call and echoes an
// argument back, mirroring how a real consumer decodes raw args and binds
// the resolved session key.
type recordTool struct {
	mu     sync.Mutex
	key    string
	path   string
	called bool
	err    error
}

func (r *recordTool) handler(sessionKey string, args json.RawMessage) (string, error) {
	var a struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", errors.New("invalid params: " + err.Error())
	}
	r.mu.Lock()
	r.key, r.path, r.called = sessionKey, a.Path, true
	r.mu.Unlock()
	if r.err != nil {
		return "", r.err
	}
	return "did " + a.Path, nil
}

func noopHandler(string, json.RawMessage) (string, error) { return "ok", nil }

func newTestHost(t *testing.T) *Host {
	t.Helper()
	h, err := New(Config{
		BaseDir:         "/tmp",
		DirPrefix:       "mh",
		RedirSubcommand: "mcp-serve",
		ServerName:      "test",
		EnvSocket:       "TEST_SOCK",
		EnvToken:        "TEST_TOK",
		RedirCommand:    "/bin/true",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { h.Close() })
	return h
}

// driveMCP runs the MCP loop over canned input lines against host h with
// session key "k", returning decoded JSON-RPC response objects.
func driveMCP(t *testing.T, h *Host, lines ...string) []map[string]any {
	t.Helper()
	in := strings.NewReader(strings.Join(lines, "\n") + "\n")
	var out bytes.Buffer
	if err := h.runMCP(in, &out, "k"); err != nil {
		t.Fatalf("runMCP: %v", err)
	}
	var resps []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("bad response line %q: %v", l, err)
		}
		resps = append(resps, m)
	}
	return resps
}

// --- New / Config -------------------------------------------------------

func TestNew_Validation(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("want error for missing RedirSubcommand")
	}
	if _, err := New(Config{RedirSubcommand: "x"}); err == nil {
		t.Fatal("want error for missing ServerName")
	}
	if _, err := New(Config{RedirSubcommand: "x", ServerName: "s"}); err == nil {
		t.Fatal("want error for missing env names")
	}
	if _, err := New(Config{RedirSubcommand: "x", ServerName: "s", EnvSocket: "S"}); err == nil {
		t.Fatal("want error for missing EnvToken")
	}
}

func TestNew_DefaultsAndExecutable(t *testing.T) {
	h, err := New(Config{
		BaseDir: "/tmp", DirPrefix: "mh",
		RedirSubcommand: "sub", ServerName: "srv",
		EnvSocket: "S", EnvToken: "T",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer h.Close()
	if h.cfg.SocketName != "mcp.sock" {
		t.Errorf("SocketName default = %q", h.cfg.SocketName)
	}
	if h.cfg.ServerInfoName != "srv" || h.cfg.ServerInfoVersion != "1" {
		t.Errorf("serverInfo defaults = %q/%q", h.cfg.ServerInfoName, h.cfg.ServerInfoVersion)
	}
	if h.cfg.RedirCommand == "" {
		t.Error("RedirCommand should default to os.Executable()")
	}
	if !strings.HasSuffix(h.SocketPath(), "/mcp.sock") {
		t.Errorf("SocketPath = %q", h.SocketPath())
	}
}

func TestNew_ExecutableError(t *testing.T) {
	old := osExecutable
	osExecutable = func() (string, error) { return "", errors.New("no exe") }
	defer func() { osExecutable = old }()
	if _, err := New(Config{
		BaseDir: "/tmp", RedirSubcommand: "sub", ServerName: "srv",
		EnvSocket: "S", EnvToken: "T",
	}); err == nil {
		t.Fatal("want error when os.Executable fails")
	}
}

func TestNew_MkdirError(t *testing.T) {
	if _, err := New(Config{
		BaseDir: "/no/such/dir", RedirSubcommand: "sub", ServerName: "srv",
		EnvSocket: "S", EnvToken: "T", RedirCommand: "/bin/true",
	}); err == nil {
		t.Fatal("want MkdirTemp error")
	}
}

func TestNew_DefaultBaseDirXDG(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	h, err := New(Config{
		RedirSubcommand: "sub", ServerName: "srv",
		EnvSocket: "S", EnvToken: "T", RedirCommand: "/bin/true",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.Close()
}

func TestNew_DefaultBaseDirTemp(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "")
	h, err := New(Config{
		RedirSubcommand: "sub", ServerName: "srv",
		EnvSocket: "S", EnvToken: "T", RedirCommand: "/bin/true",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.Close()
}

// --- Tool registration --------------------------------------------------

func TestTool_RegisterAndReplace(t *testing.T) {
	h := newTestHost(t)
	h.Tool("a", "desc-a", map[string]any{"type": "object"}, noopHandler)
	h.Tool("b", "desc-b", map[string]any{"type": "object"}, noopHandler)
	if len(h.toolsIn) != 2 {
		t.Fatalf("want 2 tools, got %d", len(h.toolsIn))
	}
	// Replace "a" in place: order preserved, count unchanged.
	h.Tool("a", "desc-a2", map[string]any{"type": "object"}, noopHandler)
	if len(h.toolsIn) != 2 || h.toolsIn[0].name != "a" || h.toolsIn[0].description != "desc-a2" {
		t.Fatalf("replace-in-place failed: %+v", h.toolsIn)
	}
}

// --- ServerConfigForSession + registry ---------------------------------

func TestServerConfigForSession(t *testing.T) {
	h := newTestHost(t)
	srvs := h.ServerConfigForSession("conv-1")
	if len(srvs) != 1 || srvs[0].Stdio == nil {
		t.Fatalf("want 1 stdio server, got %+v", srvs)
	}
	st := srvs[0].Stdio
	if st.Name != "test" || st.Command != "/bin/true" {
		t.Errorf("server cfg = %+v", st)
	}
	if len(st.Args) != 1 || st.Args[0] != "mcp-serve" {
		t.Errorf("args = %v", st.Args)
	}
	var sock, tok string
	for _, e := range st.Env {
		switch e.Name {
		case "TEST_SOCK":
			sock = e.Value
		case "TEST_TOK":
			tok = e.Value
		}
	}
	if sock != h.SocketPath() || tok == "" {
		t.Fatalf("env sock=%q tok=%q", sock, tok)
	}
	// Token resolves to the session key server-side.
	if key, ok := h.resolve(tok); !ok || key != "conv-1" {
		t.Fatalf("resolve = %q,%v", key, ok)
	}
	// Re-register rotates the token and drops the old one.
	srvs2 := h.ServerConfigForSession("conv-1")
	tok2 := ""
	for _, e := range srvs2[0].Stdio.Env {
		if e.Name == "TEST_TOK" {
			tok2 = e.Value
		}
	}
	if tok2 == tok {
		t.Fatal("token not rotated")
	}
	if _, ok := h.resolve(tok); ok {
		t.Fatal("old token still resolves")
	}
	if _, ok := h.resolve("nope"); ok {
		t.Fatal("unknown token resolved")
	}
}

// --- MCP state machine --------------------------------------------------

func hostWithTool(t *testing.T, rt *recordTool) *Host {
	h := newTestHost(t)
	h.Tool("do", "does a thing", map[string]any{
		"type":       "object",
		"properties": map[string]any{"path": map[string]any{"type": "string"}},
		"required":   []string{"path"},
	}, rt.handler)
	return h
}

func TestMCP_InitializeAndList(t *testing.T) {
	h := hostWithTool(t, &recordTool{})
	r := driveMCP(t, h,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
	)
	if len(r) != 2 {
		t.Fatalf("want 2 responses, got %d: %v", len(r), r)
	}
	init := r[0]["result"].(map[string]any)
	if init["protocolVersion"] != "2025-06-18" {
		t.Errorf("protocolVersion = %v", init["protocolVersion"])
	}
	si := init["serverInfo"].(map[string]any)
	if si["name"] != "test" || si["version"] != "1" {
		t.Errorf("serverInfo = %v", si)
	}
	tools := r[1]["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != "do" {
		t.Fatalf("tools = %v", tools)
	}
}

func TestMCP_ToolCall_Success(t *testing.T) {
	rt := &recordTool{}
	h := hostWithTool(t, rt)
	r := driveMCP(t, h,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"do","arguments":{"path":"/tmp/x"}}}`,
	)
	res := r[0]["result"].(map[string]any)
	if _, isErr := res["isError"]; isErr {
		t.Fatalf("tool returned error: %v", res)
	}
	txt := res["content"].([]any)[0].(map[string]any)["text"].(string)
	if txt != "did /tmp/x" {
		t.Fatalf("text = %q", txt)
	}
	if !rt.called || rt.key != "k" || rt.path != "/tmp/x" {
		t.Fatalf("handler args: called=%v key=%q path=%q", rt.called, rt.key, rt.path)
	}
}

func TestMCP_ToolCall_HandlerError(t *testing.T) {
	h := hostWithTool(t, &recordTool{err: errors.New("boom")})
	r := driveMCP(t, h,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"do","arguments":{"path":"/x"}}}`,
	)
	if r[0]["result"].(map[string]any)["isError"] != true {
		t.Fatalf("want isError on handler failure: %v", r[0])
	}
}

func TestMCP_ToolCall_BadArguments(t *testing.T) {
	h := hostWithTool(t, &recordTool{})
	// path is a number; handler's json.Unmarshal fails → invalid params.
	r := driveMCP(t, h,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"do","arguments":{"path":123}}}`,
	)
	if r[0]["result"].(map[string]any)["isError"] != true {
		t.Fatal("want isError on bad arguments")
	}
}

func TestMCP_ToolCall_BadParams(t *testing.T) {
	h := hostWithTool(t, &recordTool{})
	r := driveMCP(t, h,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":123}}`,
	)
	if r[0]["result"].(map[string]any)["isError"] != true {
		t.Fatal("want isError on bad params")
	}
}

func TestMCP_ToolCall_UnknownTool(t *testing.T) {
	h := hostWithTool(t, &recordTool{})
	r := driveMCP(t, h,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"other","arguments":{}}}`,
	)
	if r[0]["result"].(map[string]any)["isError"] != true {
		t.Fatal("want isError for unknown tool")
	}
}

func TestMCP_UnknownMethod(t *testing.T) {
	h := hostWithTool(t, &recordTool{})
	r := driveMCP(t, h, `{"jsonrpc":"2.0","id":9,"method":"bogus"}`)
	if r[0]["error"] == nil {
		t.Fatal("want error for unknown method")
	}
}

func TestMCP_NotificationPingBadJSON(t *testing.T) {
	h := hostWithTool(t, &recordTool{})
	r := driveMCP(t, h,
		`not json at all`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":3,"method":"ping"}`,
	)
	if len(r) != 1 {
		t.Fatalf("only ping should reply; got %d: %v", len(r), r)
	}
	if _, ok := r[0]["result"]; !ok {
		t.Fatalf("ping result missing: %v", r[0])
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("read boom") }

func TestRunMCP_ReadError(t *testing.T) {
	h := newTestHost(t)
	var out strings.Builder
	if err := h.runMCP(errReader{}, &out, "k"); err == nil {
		t.Fatal("want read error")
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("write boom") }

func TestRunMCP_WriteError(t *testing.T) {
	h := newTestHost(t)
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n")
	if err := h.runMCP(in, failWriter{}, "k"); err == nil {
		t.Fatal("want write error propagated")
	}
}

func TestRunMCP_ReusesBufioReader(t *testing.T) {
	h := newTestHost(t)
	br := bufio.NewReader(strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n"))
	var out bytes.Buffer
	if err := h.runMCP(br, &out, "k"); err != nil {
		t.Fatalf("runMCP: %v", err)
	}
	if !strings.Contains(out.String(), `"result"`) {
		t.Fatalf("want ping result, got %q", out.String())
	}
}

// --- Listener preamble auth + integration ------------------------------

func dialPreamble(t *testing.T, sock, token string) net.Conn {
	t.Helper()
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if _, err := c.Write([]byte(`{"token":"` + token + `"}` + "\n")); err != nil {
		t.Fatalf("write preamble: %v", err)
	}
	return c
}

func TestHost_ValidTokenFullMCP(t *testing.T) {
	rt := &recordTool{}
	h := hostWithTool(t, rt)
	tok := ""
	for _, e := range h.ServerConfigForSession("conv-7")[0].Stdio.Env {
		if e.Name == "TEST_TOK" {
			tok = e.Value
		}
	}
	if err := h.Listen(); err != nil {
		t.Fatalf("listen: %v", err)
	}
	c := dialPreamble(t, h.SocketPath(), tok)
	defer c.Close()
	_, _ = c.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"do","arguments":{"path":"/tmp/a"}}}` + "\n"))
	br := bufio.NewReader(c)
	line, err := br.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(line, &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, isErr := m["result"].(map[string]any)["isError"]; isErr {
		t.Fatalf("tool error: %v", m)
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if !rt.called || rt.key != "conv-7" || rt.path != "/tmp/a" {
		t.Fatalf("handler: called=%v key=%q path=%q", rt.called, rt.key, rt.path)
	}
}

func TestHost_UnknownToken(t *testing.T) {
	h := hostWithTool(t, &recordTool{})
	if err := h.Listen(); err != nil {
		t.Fatal(err)
	}
	c := dialPreamble(t, h.SocketPath(), "bogus")
	defer c.Close()
	br := bufio.NewReader(c)
	if _, err := br.ReadBytes('\n'); err == nil {
		t.Fatal("want EOF after unknown-token rejection")
	}
}

func TestHost_MalformedPreamble(t *testing.T) {
	h := hostWithTool(t, &recordTool{})
	if err := h.Listen(); err != nil {
		t.Fatal(err)
	}
	c, err := net.Dial("unix", h.SocketPath())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, _ = c.Write([]byte("not json\n"))
	br := bufio.NewReader(c)
	if _, err := br.ReadBytes('\n'); err == nil {
		t.Fatal("want EOF after malformed preamble")
	}
}

func TestHost_HangupBeforePreamble(t *testing.T) {
	h := hostWithTool(t, &recordTool{})
	if err := h.Listen(); err != nil {
		t.Fatal(err)
	}
	c, err := net.Dial("unix", h.SocketPath())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// Half-close the write side so the server's ReadBytes sees EOF with an
	// empty line (the len(line)==0 hangup branch in handle). Then block on
	// a read until the server closes its end (io.EOF), which proves
	// handle() actually ran before the test tears down the listener —
	// otherwise the accept→handle goroutine could race Close() and the
	// hangup branch would be flaky-uncovered.
	uc, ok := c.(*net.UnixConn)
	if !ok {
		t.Fatalf("want *net.UnixConn, got %T", c)
	}
	if err := uc.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}
	if _, err := io.Copy(io.Discard, c); err != nil {
		t.Fatalf("read until server close: %v", err)
	}
}

func TestHost_CloseRemovesSocketAndDir(t *testing.T) {
	h := hostWithTool(t, &recordTool{})
	if err := h.Listen(); err != nil {
		t.Fatal(err)
	}
	sock := h.SocketPath()
	if err := h.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Errorf("socket not removed: %v", err)
	}
}

func TestHost_CloseWithoutListen(t *testing.T) {
	h := newTestHost(t)
	dir := h.dir
	if err := h.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("dir not removed: %v", err)
	}
}

func TestListen_BadPath(t *testing.T) {
	h := newTestHost(t)
	os.RemoveAll(h.dir) // socket's parent dir gone → net.Listen fails
	if err := h.Listen(); err == nil {
		t.Fatal("want listen error on missing dir")
	}
}
