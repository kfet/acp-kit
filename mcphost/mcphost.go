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
//   - That subprocess is a **thin reconnecting proxy** (see RunRedir): it
//     dials the host's unix socket, writes one newline-terminated preamble
//     line `{"token":"<tok>"}`, then forwards lines in both directions. It
//     knows just enough MCP to replay `initialize` after a reconnect.
//   - The Host owns the MCP state machine: it runs the loop once per
//     accepted socket connection, after the preamble token is resolved to
//     a session key. The session key is bound server-side from the token,
//     so a client can never spoof which session it attaches to.
//
// # Surviving a consumer re-exec
//
// A consumer that hot-reloads by syscall.Exec'ing its own new binary in
// place (same PID) tears the Host down and builds a fresh one, while the
// agent's redirector subprocess — a child of the AGENT, not of the
// consumer — keeps running untouched. Three things make that seamless,
// and a consumer that reloads MUST use all three:
//
//   - Config.Dir pins the socket to a stable path instead of a fresh
//     MkdirTemp one, so the successor binds where the redirector is
//     already pointed.
//   - ExportTokens/SeedTokens carry the sessionKey→token registry across
//     the exec through the ENVIRONMENT (never disk — that would create a
//     new secret at rest), so the successor still recognises tokens the
//     predecessor minted.
//   - CloseForExec shuts the predecessor down without unlinking the
//     socket or removing a caller-supplied directory; plain Close is the
//     real-shutdown path that cleans up.
//
// The redirector rides out the gap by redialling with backoff and
// replaying `initialize`, because the successor's per-connection MCP
// state is gone. Nothing is required of consumers that do not reload:
// every one of these is additive and off by default.
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
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

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
	// Dir, when non-empty, is a FIXED directory the Host uses for its
	// socket — typically one under the consumer's StateDir. It is created
	// if missing and tightened to 0700, and it is NEVER removed by Close:
	// the Host did not create it in the general case and the whole point
	// of a fixed path is that it outlives the process.
	//
	// Set this if the consumer re-execs itself in place and needs the
	// socket path to be stable across the exec. Leave it empty (the
	// default) to get a fresh private MkdirTemp directory under BaseDir,
	// which Close does remove.
	Dir string
	// BaseDir is the directory under which the private socket directory is
	// created. Ignored when Dir is set. Defaults to $XDG_RUNTIME_DIR, then
	// os.TempDir().
	BaseDir string
	// DirPrefix is the MkdirTemp prefix for the private socket directory.
	// Ignored when Dir is set. Defaults to "mcphost-".
	DirPrefix string
	// SocketName is the socket file name inside the socket directory.
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
	ownDir bool // dir was created by us (MkdirTemp) → Close removes it

	regMu   sync.Mutex
	byToken map[string]string // token -> sessionKey
	byKey   map[string]string // sessionKey -> current token

	tools   map[string]*tool
	toolsIn []*tool // registration order (stable tools/list output)

	ln net.Listener
	wg sync.WaitGroup

	connMu sync.Mutex
	conns  map[io.Closer]struct{} // live accepted connections
	closed bool
}

type tool struct {
	name        string
	description string
	inputSchema map[string]any
	handler     Handler
}

// osExecutable is os.Executable, indirected for tests to exercise the
// resolve-error path. osChmod is os.Chmod, likewise.
var (
	osExecutable = os.Executable
	osChmod      = os.Chmod
)

// New creates a Host: it validates the config, prepares the private 0700
// socket directory, and computes the socket path. It does not bind the
// socket yet — register tools with Tool (and, on a reload, seed the token
// registry with SeedTokens), then call Listen.
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
	dir, ownDir, err := socketDir(cfg)
	if err != nil {
		return nil, err
	}
	return &Host{
		cfg:     cfg,
		dir:     dir,
		ownDir:  ownDir,
		socket:  filepath.Join(dir, cfg.SocketName),
		byToken: make(map[string]string),
		byKey:   make(map[string]string),
		tools:   make(map[string]*tool),
		conns:   make(map[io.Closer]struct{}),
	}, nil
}

// socketDir prepares the directory holding the socket and reports whether
// this process created it (and may therefore remove it again).
//
// A caller-supplied Config.Dir is created if missing and tightened to
// 0700 either way — a consumer's StateDir subtree is commonly 0755, and
// we will not leave the socket in a directory other uids can traverse —
// but it is never ours to delete.
func socketDir(cfg Config) (dir string, ownDir bool, err error) {
	if cfg.Dir != "" {
		if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
			return "", false, fmt.Errorf("mcphost: create socket dir: %w", err)
		}
		if err := osChmod(cfg.Dir, 0o700); err != nil {
			return "", false, fmt.Errorf("mcphost: tighten socket dir: %w", err)
		}
		return cfg.Dir, false, nil
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
	d, err := os.MkdirTemp(base, cfg.DirPrefix)
	if err != nil {
		return "", false, fmt.Errorf("mcphost: create socket dir: %w", err)
	}
	return d, true, nil
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

// tokenRegistry is the wire form of the sessionKey→token registry.
type tokenRegistry struct {
	Version int               `json:"v"`
	Tokens  map[string]string `json:"tokens"` // sessionKey -> token
}

const tokenRegistryVersion = 1

// ExportTokens serialises the sessionKey→token registry into an opaque,
// env-safe blob (base64 of JSON, no padding, no shell metacharacters).
//
// It exists for one purpose: a consumer that re-execs itself in place can
// hand the blob to its successor in an environment variable, so tokens
// already delivered to running agents keep working. Pass it to SeedTokens
// on the successor, before Listen.
//
// The blob is a bearer credential for every live session. Put it in the
// environment of the process you are becoming and nowhere else — in
// particular do NOT write it to a file, which would create a new secret
// at rest for no reason. An empty registry still yields a non-empty blob.
func (h *Host) ExportTokens() string {
	h.regMu.Lock()
	reg := tokenRegistry{Version: tokenRegistryVersion, Tokens: make(map[string]string, len(h.byKey))}
	for key, tok := range h.byKey {
		reg.Tokens[key] = tok
	}
	h.regMu.Unlock()
	b, _ := json.Marshal(reg) // map[string]string cannot fail to marshal
	return base64.RawURLEncoding.EncodeToString(b)
}

// SeedTokens restores a registry exported by a predecessor's
// ExportTokens, merging it into this Host's registry. Call it after New
// and before Listen.
//
// An empty blob is a no-op returning nil, so a consumer can pass its
// reload env var through unconditionally on a cold start. A malformed or
// future-versioned blob is an error: silently starting with no tokens is
// exactly the failure this feature exists to prevent, so the consumer
// gets to decide (mint fresh sessions, or refuse to come up).
func (h *Host) SeedTokens(blob string) error {
	if blob == "" {
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(blob)
	if err != nil {
		return fmt.Errorf("mcphost: decode token registry: %w", err)
	}
	var reg tokenRegistry
	if err := json.Unmarshal(raw, &reg); err != nil {
		return fmt.Errorf("mcphost: parse token registry: %w", err)
	}
	if reg.Version != tokenRegistryVersion {
		return fmt.Errorf("mcphost: token registry version %d is not supported", reg.Version)
	}
	h.regMu.Lock()
	for key, tok := range reg.Tokens {
		if old, ok := h.byKey[key]; ok {
			delete(h.byToken, old)
		}
		h.byToken[tok] = key
		h.byKey[key] = tok
	}
	h.regMu.Unlock()
	return nil
}

// socketProbeTimeout bounds the liveness probe Listen runs against a
// pre-existing socket file.
var socketProbeTimeout = 250 * time.Millisecond

// Listen binds the unix socket, tightens it to 0600, and starts accepting
// connections. Call Close (real shutdown) or CloseForExec (reload) to
// stop.
//
// A socket file already sitting at the path is dealt with first. It is
// stale in both of the cases we care about: after a same-PID exec the
// previous holder WAS this very process and is gone, and after a crash
// nobody is listening — so unlinking it is correct rather than racy. The
// case we must not get wrong is a genuinely live holder (a second
// consumer instance pointed at the same Config.Dir), so we dial the
// socket first and refuse to steal the path if anything answers.
func (h *Host) Listen() error {
	if _, err := os.Lstat(h.socket); err == nil {
		if c, derr := net.DialTimeout("unix", h.socket, socketProbeTimeout); derr == nil {
			_ = c.Close()
			return fmt.Errorf("mcphost: socket %s is already served by a live process", h.socket)
		}
		if rerr := os.Remove(h.socket); rerr != nil {
			return fmt.Errorf("mcphost: remove stale socket: %w", rerr)
		}
	}
	ln, err := net.Listen("unix", h.socket)
	if err != nil {
		return err
	}
	// Own the unlink ourselves: Go's UnixListener removes the socket file
	// on Close by default, which would defeat CloseForExec — the whole
	// point of which is to leave the path in place for the successor.
	// Close does the removal explicitly instead. net.Listen("unix", …)
	// always yields a *net.UnixListener.
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
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
		if !h.track(c) {
			_ = c.Close() // shutting down; do not start a new handler
			continue
		}
		h.wg.Add(1)
		go func() {
			defer h.wg.Done()
			defer h.untrack(c)
			h.handle(c)
		}()
	}
}

// track registers a live connection so shutdown can close it, reporting
// false if the Host is already shutting down.
func (h *Host) track(c io.Closer) bool {
	h.connMu.Lock()
	defer h.connMu.Unlock()
	if h.closed {
		return false
	}
	h.conns[c] = struct{}{}
	return true
}

func (h *Host) untrack(c io.Closer) {
	h.connMu.Lock()
	delete(h.conns, c)
	h.connMu.Unlock()
}

// Close performs a real shutdown: it stops accepting, drops live
// connections, waits for in-flight handlers, unlinks the socket, and
// removes the socket directory if this Host created it. A directory
// supplied via Config.Dir is left alone.
func (h *Host) Close() error {
	h.shutdown()
	if h.ln != nil {
		_ = os.Remove(h.socket)
	}
	if h.ownDir {
		_ = os.RemoveAll(h.dir)
	}
	return nil
}

// CloseForExec is the reload counterpart of Close: it stops accepting,
// drops live connections and waits for in-flight handlers, but leaves the
// socket file and its directory in place so the successor process binds
// the same path the agents' redirectors are already pointed at.
//
// Use it immediately before syscall.Exec, having first captured
// ExportTokens for the successor's environment. Redirectors see their
// connection drop and redial with backoff, so the gap is invisible as
// long as the successor binds promptly.
func (h *Host) CloseForExec() error {
	h.shutdown()
	return nil
}

// shutdown stops the accept loop, closes every live connection so their
// handlers return, and waits for them. Closing the listener alone is not
// enough: an attached redirector holds its connection open indefinitely
// (that is the whole point of it), so wg.Wait would never return.
func (h *Host) shutdown() {
	h.connMu.Lock()
	h.closed = true
	conns := make([]io.Closer, 0, len(h.conns))
	for c := range h.conns {
		conns = append(conns, c)
	}
	h.connMu.Unlock()
	if h.ln != nil {
		// Close errors on a listener are not actionable; best-effort.
		_ = h.ln.Close()
	}
	for _, c := range conns {
		_ = c.Close()
	}
	h.wg.Wait()
}

// newToken returns a 16-byte random hex token. crypto/rand failure is
// fatal-grade and handled in mcphost_must.go.
func newToken() string {
	b := make([]byte, 16)
	mustRand(b)
	return hex.EncodeToString(b)
}
