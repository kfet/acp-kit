// Package mcphost implements a generic, self-hosted MCP server that a
// consumer advertises to an ACP agent as a stdio MCP server. It exposes
// consumer-registered tools to the agent while running the MCP JSON-RPC
// state machine in the consumer's own process, reached over a
// per-process unix socket with per-session token authentication.
//
// Transport design (deliberately minimal, stdlib-only, no MCP SDK):
//   - The agent spawns a redirector subprocess over MCP **stdio**
//     (newline-delimited JSON-RPC 2.0). Stdio needs no port and no
//     capability negotiation, so it works on every deployment.
//   - That subprocess is a **dumb redirector** (see RunRedir): it dials
//     the host's unix socket, writes one newline-terminated preamble line
//     `{"token":"<tok>"}`, then io.Copy's stdin↔socket in both
//     directions. It has no MCP knowledge.
//   - The Host owns the MCP state machine: it runs the loop once per
//     accepted socket connection, after the preamble token is resolved to
//     a session key. The session key is bound server-side from the token,
//     so a client can never spoof which session it attaches to.
//
// Security: each ACP session is minted a fresh random token bound to its
// session key in a registry. Only a same-uid process holding that token
// (delivered via the spawned subprocess's env) can attach, and only to
// the one session the token maps to. The socket lives in a private 0700
// directory and the socket file itself is chmod'd 0600.
//
// mcphost contains zero consumer-specific logic: tool names, schemas, and
// behaviour are all supplied by the consumer via Host.Tool.
package mcphost

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"

	acp "github.com/coder/acp-go-sdk"
)

// Handler implements one registered tool. It receives the session key
// resolved server-side from the connection's preamble token (never
// client-supplied, so it cannot be spoofed) plus the raw JSON arguments
// object from the tools/call request. It returns the success text shown
// to the agent, or an error whose message is surfaced as an MCP tool
// error (isError). Argument decoding and validation are the handler's
// responsibility.
type Handler func(sessionKey string, args json.RawMessage) (string, error)

// Config configures a Host. Zero-value fields fall back to the documented
// defaults; RedirSubcommand, ServerName, EnvSocket and EnvToken are
// required.
type Config struct {
	// BaseDir is the directory under which the private socket directory is
	// created. Defaults to $XDG_RUNTIME_DIR, then os.TempDir().
	BaseDir string
	// DirPrefix is the MkdirTemp prefix for the private socket directory.
	// Defaults to "mcphost-".
	DirPrefix string
	// SocketName is the socket file name inside the private directory.
	// Defaults to "mcp.sock".
	SocketName string

	// RedirCommand is the executable the agent spawns for the redirector.
	// Defaults to os.Executable().
	RedirCommand string
	// RedirSubcommand is the argv token the redirector binary dispatches
	// on (e.g. "mcp-serve"). Required.
	RedirSubcommand string

	// ServerName is the MCP server name advertised to the agent in the
	// session/new McpServer config (e.g. "poe"). Required.
	ServerName string
	// ServerInfoName is the serverInfo.name returned from initialize.
	// Defaults to ServerName.
	ServerInfoName string
	// ServerInfoVersion is the serverInfo.version returned from
	// initialize. Defaults to "1".
	ServerInfoVersion string

	// EnvSocket / EnvToken are the env var names carrying the socket path
	// and per-session token to the spawned redirector. Required.
	EnvSocket string
	EnvToken  string
}

// Host owns the unix socket, the token→sessionKey registry, the set of
// registered tools, and the accept loop that runs the MCP JSON-RPC loop
// over each accepted connection.
type Host struct {
	cfg    Config
	dir    string
	socket string

	regMu   sync.Mutex
	byToken map[string]string // token -> sessionKey
	byKey   map[string]string // sessionKey -> current token

	tools   map[string]*tool
	toolsIn []*tool // registration order (stable tools/list output)

	ln net.Listener
	wg sync.WaitGroup
}

type tool struct {
	name        string
	description string
	inputSchema map[string]any
	handler     Handler
}

// osExecutable is os.Executable, indirected for tests to exercise the
// resolve-error path.
var osExecutable = os.Executable

// New creates a Host: it validates the config, creates a private 0700
// directory, and computes the socket path. It does not bind the socket
// yet — register tools with Tool, then call Listen. Close removes the
// directory.
func New(cfg Config) (*Host, error) {
	if cfg.RedirSubcommand == "" {
		return nil, fmt.Errorf("mcphost: RedirSubcommand is required")
	}
	if cfg.ServerName == "" {
		return nil, fmt.Errorf("mcphost: ServerName is required")
	}
	if cfg.EnvSocket == "" || cfg.EnvToken == "" {
		return nil, fmt.Errorf("mcphost: EnvSocket and EnvToken are required")
	}
	if cfg.DirPrefix == "" {
		cfg.DirPrefix = "mcphost-"
	}
	if cfg.SocketName == "" {
		cfg.SocketName = "mcp.sock"
	}
	if cfg.ServerInfoName == "" {
		cfg.ServerInfoName = cfg.ServerName
	}
	if cfg.ServerInfoVersion == "" {
		cfg.ServerInfoVersion = "1"
	}
	if cfg.RedirCommand == "" {
		exe, err := osExecutable()
		if err != nil {
			return nil, fmt.Errorf("mcphost: resolve executable: %w", err)
		}
		cfg.RedirCommand = exe
	}
	base := cfg.BaseDir
	if base == "" {
		base = os.Getenv("XDG_RUNTIME_DIR")
		if base == "" {
			base = os.TempDir()
		}
	}
	// Private 0700 dir (MkdirTemp default perms) so only this uid can
	// reach the socket; the socket file itself is further chmod'd 0600.
	dir, err := os.MkdirTemp(base, cfg.DirPrefix)
	if err != nil {
		return nil, fmt.Errorf("mcphost: create socket dir: %w", err)
	}
	return &Host{
		cfg:     cfg,
		dir:     dir,
		socket:  filepath.Join(dir, cfg.SocketName),
		byToken: make(map[string]string),
		byKey:   make(map[string]string),
		tools:   make(map[string]*tool),
	}, nil
}

// Tool registers a tool by name with its JSON input schema and handler.
// Registration order is preserved in tools/list. Registering the same
// name twice replaces the earlier entry in place. Not safe to call
// concurrently with the accept loop; register all tools before Listen.
func (h *Host) Tool(name, description string, inputSchema map[string]any, handler Handler) {
	t := &tool{name: name, description: description, inputSchema: inputSchema, handler: handler}
	if _, exists := h.tools[name]; !exists {
		h.toolsIn = append(h.toolsIn, t)
	} else {
		for i, ex := range h.toolsIn {
			if ex.name == name {
				h.toolsIn[i] = t
				break
			}
		}
	}
	h.tools[name] = t
}

// SocketPath returns the unix socket path (known before Listen).
func (h *Host) SocketPath() string { return h.socket }

// ServerConfigForSession mints a fresh token bound to sessionKey and
// returns the acp McpServer(s) to advertise for that session's
// session/new. The server is a Stdio McpServer whose Command is the
// configured redirector binary, Args the redirector subcommand, and Env
// carries the socket path and token. Any previous token for the same
// session key is invalidated.
func (h *Host) ServerConfigForSession(sessionKey string) []acp.McpServer {
	tok := h.register(sessionKey)
	return []acp.McpServer{{Stdio: &acp.McpServerStdio{
		Name:    h.cfg.ServerName,
		Command: h.cfg.RedirCommand,
		Args:    []string{h.cfg.RedirSubcommand},
		Env: []acp.EnvVariable{
			{Name: h.cfg.EnvSocket, Value: h.socket},
			{Name: h.cfg.EnvToken, Value: tok},
		},
	}}}
}

// register mints a fresh random token bound to sessionKey, invalidating
// any previous token for the same key, and returns it.
func (h *Host) register(sessionKey string) string {
	tok := newToken()
	h.regMu.Lock()
	if old, ok := h.byKey[sessionKey]; ok {
		delete(h.byToken, old) // rotate: drop the key's previous token
	}
	h.byToken[tok] = sessionKey
	h.byKey[sessionKey] = tok
	h.regMu.Unlock()
	return tok
}

// resolve returns the session key bound to token, if any.
func (h *Host) resolve(token string) (sessionKey string, ok bool) {
	h.regMu.Lock()
	sessionKey, ok = h.byToken[token]
	h.regMu.Unlock()
	return sessionKey, ok
}

// Listen creates the unix socket (replacing any stale one), tightens it
// to 0600, and starts accepting connections. Call Close to stop and clean
// up.
func (h *Host) Listen() error {
	_ = os.Remove(h.socket) // clear stale socket from a prior run
	ln, err := net.Listen("unix", h.socket)
	if err != nil {
		return err
	}
	mustChmod(h.socket)
	h.ln = ln
	h.wg.Add(1)
	go h.serve()
	return nil
}

func (h *Host) serve() {
	defer h.wg.Done()
	for {
		c, err := h.ln.Accept()
		if err != nil {
			return // listener closed
		}
		h.wg.Add(1)
		go func() {
			defer h.wg.Done()
			h.handle(c)
		}()
	}
}

// Close stops accepting, waits for in-flight handlers, removes the socket
// file, and removes the private directory.
func (h *Host) Close() error {
	if h.ln != nil {
		// Close errors on a listener are not actionable; best-effort.
		_ = h.ln.Close()
		h.wg.Wait()
		_ = os.Remove(h.socket)
	}
	_ = os.RemoveAll(h.dir)
	return nil
}

// newToken returns a 16-byte random hex token. crypto/rand failure is
// fatal-grade and handled in mcphost_must.go.
func newToken() string {
	b := make([]byte, 16)
	mustRand(b)
	return hex.EncodeToString(b)
}
