// Package interop drives mcphost — and the relaytool set on it — with
// the real MCP go-sdk client fir pins, over the real agent path: the
// redirector runs as a stdio subprocess (sdk.CommandTransport, as fir's
// commandTransport builds it) and dials the host's unix socket.
//
// It exists because acp-kit's hand-written JSON-RPC tests once passed
// while every real client silently fell back to the legacy initialize
// handshake (server/discover was not in the SEP-2575 shape), so the
// files extension never switched on. Anything that makes the SDK fall
// back, fail to decode a reply, or drift off the wire shape fails here.
//
// Golden transcripts live in testdata/; rewrite them with
// `go test ./interop -update` and review the diff.
package interop

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kfet/acp-kit/mcphost"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	redirEnv = "INTEROP_REDIR" // makes the test binary act as the redirector
	envSock  = "INTEROP_SOCK"
	envTok   = "INTEROP_TOK"
	convKey  = "conv-interop"
)

var update = flag.Bool("update", false, "rewrite golden transcripts in testdata/")

func TestMain(m *testing.M) {
	if os.Getenv(redirEnv) == "1" {
		if err := mcphost.RunRedir(mcphost.RedirConfig{EnvSocket: envSock, EnvToken: envTok}); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// latest is the newest protocol version the pinned SDK speaks.
func latest() string { return sdk.SupportedProtocolVersions()[0] }

// hostConfig is the Host config every test uses; dir "" = fresh temp dir.
func hostConfig(t *testing.T, dir string) mcphost.Config {
	t.Helper()
	cfg := mcphost.Config{
		RedirCommand: os.Args[0], RedirSubcommand: "mcp-serve",
		ServerName: "interop", ServerInfoVersion: "1.2.3",
		EnvSocket: envSock, EnvToken: envTok,
	}
	if dir == "" {
		d, err := os.MkdirTemp("/tmp", "ia") // short: unix socket path limit
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.RemoveAll(d) })
		cfg.BaseDir = d
	} else {
		cfg.Dir = dir
	}
	return cfg
}

func listen(t *testing.T, h *mcphost.Host) {
	t.Helper()
	if err := h.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })
}

// firCaps is what fir sends for a server with files enabled
// (pkg/mcp filesClientCapabilities + filesClientCaps).
func firCaps() *sdk.ClientCapabilities {
	caps := &sdk.ClientCapabilities{RootsV2: &sdk.RootCapabilities{ListChanged: true}}
	caps.AddExtension(mcphost.FilesExtension, map[string]any{"upload": map[string]any{}, "download": map[string]any{}})
	return caps
}

// ---- wire tap ---------------------------------------------------------------

// entry is one exchange on the wire, as the client saw it.
type entry struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
	Notify bool            `json:"notification,omitempty"`
}

// tap records the exchanges, and does what fir's filesTransport does:
// moves an upload_required result's uploads into _meta so they survive
// SDK decoding.
type tap struct {
	inner   sdk.Transport
	mu      sync.Mutex
	log     []*entry
	pending map[string]*entry
}

func (tp *tap) Connect(ctx context.Context) (sdk.Connection, error) {
	c, err := tp.inner.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &tapConn{Connection: c, tp: tp}, nil
}

func (tp *tap) methods() []string {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	var out []string
	for _, e := range tp.log {
		out = append(out, e.Method)
	}
	return out
}

// lastResult is the raw result of the newest reply to method.
func (tp *tap) lastResult(method string) json.RawMessage {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	for i := len(tp.log) - 1; i >= 0; i-- {
		if tp.log[i].Method == method && tp.log[i].Result != nil {
			return tp.log[i].Result
		}
	}
	return nil
}

// reset forgets what was recorded so far.
func (tp *tap) reset() {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	tp.log = nil
}

type tapConn struct {
	sdk.Connection
	tp *tap
}

func (c *tapConn) Write(ctx context.Context, m jsonrpc.Message) error {
	if r, ok := m.(*jsonrpc.Request); ok {
		e := &entry{Method: r.Method, Params: slices.Clone(r.Params), Notify: !r.ID.IsValid()}
		c.tp.mu.Lock()
		c.tp.log = append(c.tp.log, e)
		if r.ID.IsValid() {
			c.tp.pending[fmt.Sprint(r.ID.Raw())] = e
		}
		c.tp.mu.Unlock()
	}
	return c.Connection.Write(ctx, m)
}

func (c *tapConn) Read(ctx context.Context) (jsonrpc.Message, error) {
	m, err := c.Connection.Read(ctx)
	if r, ok := m.(*jsonrpc.Response); ok && err == nil {
		c.tp.mu.Lock()
		if e := c.tp.pending[fmt.Sprint(r.ID.Raw())]; e != nil {
			e.Result = slices.Clone(r.Result)
			if r.Error != nil {
				e.Error = r.Error.Error()
			}
		}
		c.tp.mu.Unlock()
		if len(r.Result) > 0 {
			r.Result = firRewriteUploadRequired(r.Result)
		}
	}
	return m, err
}

const uploadRequiredMeta = "test/uploadRequired"

// firRewriteUploadRequired mirrors fir's rewriteUploadRequired.
func firRewriteUploadRequired(raw json.RawMessage) json.RawMessage {
	var res map[string]json.RawMessage
	if json.Unmarshal(raw, &res) != nil || string(res["resultType"]) != `"upload_required"` {
		return raw
	}
	res["_meta"] = json.RawMessage(`{"` + uploadRequiredMeta + `":{"uploads":` + string(res["uploads"]) + `}}`)
	delete(res, "uploads")
	res["resultType"] = json.RawMessage(`"complete"`)
	res["content"] = json.RawMessage("[]")
	out, _ := json.Marshal(res)
	return out
}

// ---- connecting -------------------------------------------------------------

// redirCmd builds the agent's redirector subprocess from the McpServer
// config the Host advertises for sessionKey.
func redirCmd(h *mcphost.Host, sessionKey string) *exec.Cmd {
	srv := h.ServerConfigForSession(sessionKey)[0].Stdio
	cmd := exec.Command(srv.Command, srv.Args...)
	cmd.Env = append(os.Environ(), redirEnv+"=1")
	for _, e := range srv.Env {
		cmd.Env = append(cmd.Env, e.Name+"="+e.Value)
	}
	return cmd
}

// connect connects the real SDK client the way fir does. version ""
// lets the SDK pick (latest, via server/discover).
func connect(t *testing.T, h *mcphost.Host, caps *sdk.ClientCapabilities, version string) (*sdk.ClientSession, *tap) {
	t.Helper()
	return connectCmd(t, redirCmd(h, convKey), caps, version)
}

func connectCmd(t *testing.T, cmd *exec.Cmd, caps *sdk.ClientCapabilities, version string) (*sdk.ClientSession, *tap) {
	t.Helper()
	tp := &tap{inner: &sdk.CommandTransport{Command: cmd}, pending: map[string]*entry{}}
	cl := sdk.NewClient(&sdk.Implementation{Name: "fir", Version: "dev"}, &sdk.ClientOptions{Capabilities: caps})
	var opts *sdk.ClientSessionOptions
	if version != "" {
		opts = &sdk.ClientSessionOptions{ProtocolVersion: version}
	}
	cs, err := cl.Connect(ctxT(t), tp, opts)
	if err != nil {
		t.Fatalf("connect (%s): %v", version, err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs, tp
}

func ctxT(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func callTool(t *testing.T, cs *sdk.ClientSession, p *sdk.CallToolParams) *sdk.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(ctxT(t), p)
	if err != nil {
		t.Fatalf("CallTool %s: %v", p.Name, err)
	}
	return res
}

func text(r *sdk.CallToolResult) string {
	var b strings.Builder
	for _, c := range r.Content {
		if tc, ok := c.(*sdk.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func toolNames(t *testing.T, cs *sdk.ClientSession) []string {
	t.Helper()
	res, err := cs.ListTools(ctxT(t), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	var names []string
	for _, tl := range res.Tools {
		names = append(names, tl.Name)
	}
	return names
}

// ---- golden transcripts -----------------------------------------------------

// redact replaces volatile values (signed request state) in place.
func redact(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			if k == "requestState" {
				x[k] = "<requestState>"
				continue
			}
			x[k] = redact(val)
		}
	case []any:
		for i := range x {
			x[i] = redact(x[i])
		}
	}
	return v
}

// golden compares tp's transcript, canonicalised and redacted, with
// testdata/<name>.json.
func golden(t *testing.T, name string, tp *tap) {
	t.Helper()
	tp.mu.Lock()
	raw, _ := json.Marshal(tp.log)
	tp.mu.Unlock()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	got, _ := json.MarshalIndent(redact(v), "", "  ")
	got = append(got, '\n')
	path := filepath.Join("testdata", name+".json")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test ./interop -update)", err)
	}
	if string(got) != string(want) {
		t.Fatalf("wire transcript drifted from %s (if intended, -update and review):\n--- got\n%s", path, got)
	}
}
