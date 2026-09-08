// Package client wraps acp-go-sdk's low-level Connection for ACP relays. It
// manages a single stdio child agent process (e.g. fir --mode acp) and
// dispatches inbound server-initiated ACP calls — session updates, permission
// requests, fs reads/writes — back to the relay.
//
// One AgentProc runs one ACP child process. It can serve many sessions
// concurrently — each NewSession/ResumeSession registers a per-session
// sink that receives the stream of session/update notifications.
//
// The client talks to acp.Connection directly (rather than
// acp.ClientSideConnection) so it can issue the unstable session/list and
// session/resume methods that the SDK doesn't model. Standard methods are
// sent via acp.SendRequest with the SDK's typed request/response structs.
//
// Security: the fs methods (ReadTextFile / WriteTextFile) currently require
// absolute paths but do not sandbox to the session cwd. That is adequate for
// trusted-agent relay deployments; do not expose this client to untrusted
// agents.
package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	acp "github.com/coder/acp-go-sdk"
)

// SessionUpdateSink receives streaming updates for a single ACP session.
// Each consumer (e.g. a relay) implements this to forward the stream to its
// own transport (SSE, WebSocket, IM channel, ...).
type SessionUpdateSink interface {
	OnUpdate(ctx context.Context, n acp.SessionNotification) error
}

// PermissionPolicy decides how to respond to session/request_permission.
type PermissionPolicy interface {
	Decide(ctx context.Context, req acp.RequestPermissionRequest) acp.RequestPermissionResponse
}

// Caps captures the agent capabilities the relay cares about, parsed
// from the initialize response.
type Caps struct {
	// LoadSession is the standard agentCapabilities.loadSession bool.
	LoadSession bool
	// ListSessions reflects agentCapabilities.sessionCapabilities.list
	// (unstable RFD).
	ListSessions bool
	// ResumeSession reflects agentCapabilities.sessionCapabilities.resume
	// (unstable RFD).
	ResumeSession bool
	// EmbeddedContext reflects
	// agentCapabilities.promptCapabilities.embeddedContext: when true,
	// the relay may emit ContentBlock::Resource (with TextResourceContents)
	// in prompt requests instead of a bare ResourceLink, avoiding an
	// agent-side fetch.
	EmbeddedContext bool
	// Image reflects agentCapabilities.promptCapabilities.image: when
	// true, the relay may include ContentBlock::Image (base64 data plus
	// a mime type) in prompt requests, so an image a human attached
	// reaches the model as an image rather than as a path it has to go
	// and read. False is the ACP default, and the consumer must then
	// fall back to naming the file.
	Image bool
	// Audio reflects agentCapabilities.promptCapabilities.audio, the
	// same deal as Image for ContentBlock::Audio.
	Audio bool
	// SystemPrompt reflects agentCapabilities._meta["session.systemPrompt"].
	// When true the consumer may pass a system-prompt block list via
	// session/new._meta and the agent will treat it as durable across
	// compaction. When false the consumer must fall back to inlining the
	// same content on first prompt (and re-arm on resume).
	SystemPrompt bool
	// Extensions captures arbitrary entries from agentCapabilities._meta
	// other than the kit-owned "session.systemPrompt". Consumers can
	// probe for custom extension ids (e.g. "dev.acp-kit.status-line/v1")
	// to discover advertised support. Values are the raw JSON bytes of
	// the entry — typically `{}` or `{"version": N}`. nil when the
	// agent advertised no _meta.
	Extensions map[string]json.RawMessage
}

// SessionInfo is one entry from a session/list response.
type SessionInfo struct {
	SessionId string  `json:"sessionId"`
	Cwd       string  `json:"cwd,omitempty"`
	Title     *string `json:"title,omitempty"`
	UpdatedAt string  `json:"updatedAt,omitempty"`
}

// listSessionsRequest mirrors the unstable RFD for session/list.
type listSessionsRequest struct {
	Cwd string `json:"cwd,omitempty"`
}

type listSessionsResponse struct {
	Sessions []SessionInfo `json:"sessions"`
}

// resumeSessionRequest mirrors the unstable RFD for session/resume.
type resumeSessionRequest struct {
	SessionId  string          `json:"sessionId"`
	Cwd        string          `json:"cwd,omitempty"`
	McpServers []acp.McpServer `json:"mcpServers,omitempty"`
}

// Config configures an AgentProc.
type Config struct {
	// Command is the argv used to spawn the agent (e.g. []string{"fir", "--mode", "acp"}).
	Command []string
	// Cwd is the working directory for the child process.
	Cwd string
	// MCPServersForSession, when set, returns the MCP servers to include
	// in session/new and session/resume for a session at the given cwd.
	// This lets a client hand the agent client-hosted MCP servers (e.g. a
	// per-session stdio tool server) without changing the NewSession
	// signature. Nil (the default) means no MCP servers — identical to the
	// previous hardcoded empty list.
	MCPServersForSession func(cwd string) []acp.McpServer
	// Env is the environment for the child. If nil, os.Environ() is used.
	//
	// SECURITY: this env, or the inherited os.Environ() when Env is nil, is
	// handed verbatim to a process that a chat relay drives with text from
	// people who are not the operator. Any credential the child can read it
	// can use — to impersonate the relay, read its history, or re-scope its
	// reach — and no prompt-level guard can take that back. If the relay's
	// own inbound secret leaks into the child's environment, declare it in
	// SecretEnvNames (by variable name) or Secrets (by literal value) so it
	// is dropped before the child ever starts. The child still legitimately
	// needs provider credentials it is meant to use (e.g. ANTHROPIC_API_KEY,
	// POE_API_KEY); scrub only the relay's own secrets, not those.
	Env []string
	// SecretEnvNames are environment variable names dropped from the child's
	// environment before it is spawned — in BOTH the Env-provided and the
	// nil-Env (inherit os.Environ()) paths.
	SecretEnvNames []string
	// Secrets are literal secret VALUES; any variable whose value matches one
	// is dropped whatever it is named. Empty strings are ignored (a
	// config-file deployment legitimately has no token in the environment).
	Secrets []string
	// Policy decides permission responses. If nil, AllowAllPermissions is used.
	Policy PermissionPolicy
	// CloseGrace is how long Close waits after SIGINT before SIGKILL. Default 2s.
	CloseGrace time.Duration
	// Stderr is where the child's stderr is forwarded. If nil, os.Stderr.
	Stderr io.Writer
	// ClientMeta carries extra entries to merge into the outgoing
	// clientCapabilities._meta map at Initialize. Use this to advertise
	// support for custom ACP extensions (keyed by extension id, e.g.
	// "dev.acp-kit.status-line/v1"). Keys collide last-wins with
	// kit-owned entries (e.g. "session.systemPrompt"); pick distinct
	// extension ids to avoid clobber.
	ClientMeta map[string]any
}

// AgentProc wraps a single stdio-connected ACP agent process and the ACP
// connection driving it.
type AgentProc struct {
	cfg Config

	cmd  *exec.Cmd
	conn *acp.Connection
	caps Caps

	// Process liveness. done is closed by the single reaper goroutine
	// (see startReaper) once cmd.Wait has returned; exitErr holds the
	// classified exit result, stored before done is closed. closing is
	// set by Close before it signals the child, so the reaper can tell a
	// deliberate shutdown from an unexpected death.
	done    chan struct{}
	exitErr atomic.Pointer[error]
	closing atomic.Bool

	mu     sync.Mutex
	sinks  map[acp.SessionId]SessionUpdateSink // active session sinks
	models *modelState                         // cached model list (nil until first NewSession or Probe)

	authMethods []AuthMethod // parsed from initialize response

	// availableCommands is the latest agent-advertised command catalog,
	// snapshotted from session/update notifications. Nil until the agent
	// sends an availableCommandsUpdate.
	availableCommands []CommandInfo
}

// hasSecrets reports whether any secret is actually declared — a name in
// SecretEnvNames, or a non-empty literal value in Secrets. Empty value
// strings are ignored, so a config-file deployment that carries no token in
// its environment reads as "nothing to scrub".
func (c Config) hasSecrets() bool {
	if len(c.SecretEnvNames) > 0 {
		return true
	}
	for _, s := range c.Secrets {
		if s != "" {
			return true
		}
	}
	return false
}

// scrubbedEnv returns the environment to hand the child, with the relay's own
// secrets removed. It is the single decision point for cmd.Env in Start, and
// it closes the nil-Env footgun:
//
//   - No secrets declared → pure passthrough. Config.Env is returned as-is,
//     so a nil Env stays nil and Start inherits os.Environ() exactly as
//     before (backwards compatible).
//   - Secrets declared, Env non-nil → the provided slice is scrubbed.
//   - Secrets declared, Env nil → "inherit everything" would otherwise hand
//     the child the full environment INCLUDING the secrets, making the scrub
//     a silent no-op in exactly the case that matters most. So os.Environ()
//     is materialised, scrubbed, and returned explicitly.
//
// The result is never nil when secrets are declared (same reason: a nil
// cmd.Env inherits the parent environment). The caller's Env slice is never
// mutated — a fresh slice is always built.
func (c Config) scrubbedEnv() []string {
	if !c.hasSecrets() {
		return c.Env
	}
	env := c.Env
	if env == nil {
		env = os.Environ()
	}
	return scrubEnv(env, c.SecretEnvNames, c.Secrets)
}

// scrubEnv returns a fresh (never nil) copy of the KEY=VALUE slice env with
// every entry whose name is in names, or whose value literally equals a
// non-empty entry in secrets, removed. env is not mutated.
func scrubEnv(env, names, secrets []string) []string {
	dropName := make(map[string]struct{}, len(names))
	for _, n := range names {
		dropName[n] = struct{}{}
	}
	dropVal := make(map[string]struct{}, len(secrets))
	for _, s := range secrets {
		if s == "" {
			continue
		}
		dropVal[s] = struct{}{}
	}
	out := make([]string, 0, len(env))
	for _, kv := range env {
		name, val, _ := strings.Cut(kv, "=")
		if _, ok := dropName[name]; ok {
			continue
		}
		if _, ok := dropVal[val]; ok {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// mcpFor returns the configured MCP servers for a session cwd, or an
// empty (non-nil) slice when no hook is set.
func (c Config) mcpFor(cwd string) []acp.McpServer {
	if c.MCPServersForSession == nil {
		return []acp.McpServer{}
	}
	if s := c.MCPServersForSession(cwd); s != nil {
		return s
	}
	return []acp.McpServer{}
}

// Start launches the agent process, performs Initialize (capturing caps),
// and returns a ready-to-use AgentProc.
func Start(ctx context.Context, cfg Config) (*AgentProc, error) {
	if len(cfg.Command) == 0 {
		return nil, fmt.Errorf("client: empty Command")
	}
	if cfg.Policy == nil {
		cfg.Policy = PermissionFunc(AllowAllPermissions)
	}
	if cfg.Cwd == "" {
		cfg.Cwd = os.TempDir()
	}

	cmd := exec.CommandContext(ctx, cfg.Command[0], cfg.Command[1:]...) //nolint:gosec // user-configured command
	cmd.Dir = cfg.Cwd
	if env := cfg.scrubbedEnv(); env != nil {
		cmd.Env = env
	}
	if cfg.Stderr != nil {
		cmd.Stderr = cfg.Stderr
	} else {
		cmd.Stderr = os.Stderr
	}
	stdin, err := cmd.StdinPipe()
	mustNot(err, "stdin pipe")
	stdout, err := cmd.StdoutPipe()
	mustNot(err, "stdout pipe")
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start agent: %w", err)
	}
	a, err := connect(ctx, cfg, cmd, stdin, stdout)
	if err != nil {
		_ = cmd.Process.Kill()
		return nil, err
	}
	return a, nil
}

// connect performs the post-spawn ACP handshake and returns a wired
// AgentProc. Package-private; real callers go through Start. Tests use it to
// drive the handshake against an in-process fake agent over io.Pipe pairs.
func connect(ctx context.Context, cfg Config, cmd *exec.Cmd, stdin io.WriteCloser, stdout io.Reader) (*AgentProc, error) {
	a := &AgentProc{
		cfg:   cfg,
		cmd:   cmd,
		sinks: make(map[acp.SessionId]SessionUpdateSink),
		done:  make(chan struct{}),
	}
	// Exactly one goroutine ever calls cmd.Wait. It starts before the
	// handshake so a child that dies during Initialize is still reaped
	// (and so Start's error path does not leak a zombie).
	a.startReaper()
	a.conn = acp.NewConnection(a.dispatch, stdin, stdout)

	// Use a raw map for the response so we can read the unstable
	// sessionCapabilities sub-object that the SDK's typed struct drops.
	clientMeta := map[string]any{
		"session.systemPrompt": map[string]any{"version": 1},
	}
	for k, v := range cfg.ClientMeta {
		clientMeta[k] = v
	}
	initParams := acp.InitializeRequest{
		ProtocolVersion: acp.ProtocolVersionNumber,
		ClientCapabilities: acp.ClientCapabilities{
			Fs:       acp.FileSystemCapabilities{ReadTextFile: true, WriteTextFile: true},
			Terminal: false,
			Meta:     clientMeta,
		},
	}
	raw, err := acp.SendRequest[json.RawMessage](a.conn, ctx, acp.AgentMethodInitialize, initParams)
	if err != nil {
		return nil, fmt.Errorf("acp initialize: %w", err)
	}
	a.caps = parseCaps(raw)
	a.authMethods = parseAuthMethods(raw)
	return a, nil
}

// parseAuthMethods extracts the authMethods array from a raw initialize
// response. Reads only the fields the relay actually uses; extra _meta
// is ignored.
func parseAuthMethods(raw json.RawMessage) []AuthMethod {
	var env struct {
		AuthMethods []AuthMethod `json:"authMethods"`
	}
	_ = json.Unmarshal(raw, &env)
	return env.AuthMethods
}

// parseCaps extracts agentCapabilities.{loadSession,sessionCapabilities.{list,resume}}
// from a raw initialize response. Missing fields default to false.
func parseCaps(raw json.RawMessage) Caps {
	var env struct {
		AgentCapabilities struct {
			LoadSession         bool `json:"loadSession"`
			SessionCapabilities struct {
				List   *json.RawMessage `json:"list"`
				Resume *json.RawMessage `json:"resume"`
			} `json:"sessionCapabilities"`
			PromptCapabilities struct {
				EmbeddedContext bool `json:"embeddedContext"`
				Image           bool `json:"image"`
				Audio           bool `json:"audio"`
			} `json:"promptCapabilities"`
			Meta map[string]json.RawMessage `json:"_meta"`
		} `json:"agentCapabilities"`
	}
	_ = json.Unmarshal(raw, &env)
	_, sysPrompt := env.AgentCapabilities.Meta["session.systemPrompt"]
	var exts map[string]json.RawMessage
	if len(env.AgentCapabilities.Meta) > 0 {
		exts = make(map[string]json.RawMessage, len(env.AgentCapabilities.Meta))
		for k, v := range env.AgentCapabilities.Meta {
			if k == "session.systemPrompt" {
				continue
			}
			exts[k] = v
		}
		if len(exts) == 0 {
			exts = nil
		}
	}
	return Caps{
		LoadSession:     env.AgentCapabilities.LoadSession,
		ListSessions:    env.AgentCapabilities.SessionCapabilities.List != nil,
		ResumeSession:   env.AgentCapabilities.SessionCapabilities.Resume != nil,
		EmbeddedContext: env.AgentCapabilities.PromptCapabilities.EmbeddedContext,
		Image:           env.AgentCapabilities.PromptCapabilities.Image,
		Audio:           env.AgentCapabilities.PromptCapabilities.Audio,
		SystemPrompt:    sysPrompt,
		Extensions:      exts,
	}
}

// Caps returns the agent's advertised capabilities (parsed at Initialize).
func (a *AgentProc) Caps() Caps { return a.caps }

// NewSession creates a new ACP session and wires the given sink to receive
// its updates. Returns the ACP session id.
//
// systemPromptBlocks, when non-nil, is sent in the request's _meta under
// "session.systemPrompt".blocks. Callers should only pass it when
// Caps().SystemPrompt is true; agents that haven't advertised the cap
// will simply ignore the unknown _meta key, but skipping it keeps the
// wire clean.
func (a *AgentProc) NewSession(ctx context.Context, cwd string, sink SessionUpdateSink, systemPromptBlocks []acp.ContentBlock) (acp.SessionId, error) {
	return a.NewSessionWithMeta(ctx, cwd, sink, systemPromptBlocks, nil)
}

// NewSessionWithMeta is NewSession plus caller-supplied `_meta` entries on
// the session/new request. Relays use it to pass create-time placement or
// routing hints an agent understands (e.g. a `host` entry telling a
// tmux-multiplexing agent which SSH host to run the session's pane on).
//
// extraMeta is merged into the request's `_meta`; the reserved
// "session.systemPrompt" key is owned by systemPromptBlocks and overwrites
// any same-named entry. An empty/nil extraMeta with nil systemPromptBlocks
// leaves `_meta` absent from the wire entirely, exactly as NewSession does.
func (a *AgentProc) NewSessionWithMeta(ctx context.Context, cwd string, sink SessionUpdateSink, systemPromptBlocks []acp.ContentBlock, extraMeta map[string]any) (acp.SessionId, error) {
	req := acp.NewSessionRequest{
		Cwd:        cwd,
		McpServers: a.cfg.mcpFor(cwd),
	}
	if len(extraMeta) > 0 || systemPromptBlocks != nil {
		meta := make(map[string]any, len(extraMeta)+1)
		for k, v := range extraMeta {
			meta[k] = v
		}
		if systemPromptBlocks != nil {
			meta["session.systemPrompt"] = map[string]any{
				"blocks": systemPromptBlocks,
			}
		}
		req.Meta = meta
	}
	resp, err := acp.SendRequest[sessionResponse](a.conn, ctx, acp.AgentMethodSessionNew, req)
	if err != nil {
		return "", err
	}
	a.mu.Lock()
	a.sinks[resp.SessionId] = sink
	if ms := resp.modelState(); ms != nil {
		a.models = ms
	}
	a.mu.Unlock()
	return resp.SessionId, nil
}

// SetModel selects the session's model. New-style agents (ACP >= 0.13) are
// driven through session/set_config_option using the id of the "model"
// config option; older agents through the session/set_model RPC.
func (a *AgentProc) SetModel(ctx context.Context, sid acp.SessionId, modelID string) error {
	a.mu.Lock()
	configID := ""
	if a.models != nil {
		configID = a.models.configID
	}
	a.mu.Unlock()
	if configID != "" {
		return a.SetConfigOption(ctx, sid, configID, modelID)
	}
	_, err := acp.SendRequest[json.RawMessage](a.conn, ctx, agentMethodSessionSetModel, setSessionModelRequest{
		SessionId: sid,
		ModelId:   modelID,
	})
	return err
}

// setSessionConfigOptionRequest is the params for session/set_config_option.
// It is wire-identical to the SDK's SetSessionConfigOptionValueId variant,
// and is what older (pre-0.13) agents such as fir already accept.
type setSessionConfigOptionRequest struct {
	SessionId acp.SessionId `json:"sessionId"`
	ConfigId  string        `json:"configId"`
	Value     string        `json:"value"`
}

// SetConfigOption calls the session/set_config_option RPC. Used for the
// model selector on new-style agents, and for thinking_level and similar
// dropdown-style knobs.
func (a *AgentProc) SetConfigOption(ctx context.Context, sid acp.SessionId, configID, value string) error {
	_, err := acp.SendRequest[json.RawMessage](a.conn, ctx, acp.AgentMethodSessionSetConfigOption, setSessionConfigOptionRequest{
		SessionId: sid,
		ConfigId:  configID,
		Value:     value,
	})
	return err
}

// SessionNotFoundCode is the stable JSON-RPC error code an ACP agent returns
// when a request references a session it no longer holds in memory (released
// or idle-reaped). It is the shared contract between the agent (fir) and
// relays: on this code a relay can drop its cached session and re-create it.
const SessionNotFoundCode = -32001

// releaseSessionRequest is the params for the session/release RPC.
type releaseSessionRequest struct {
	SessionId acp.SessionId `json:"sessionId"`
}

// ReleaseSession asks the agent to tear down and forget an in-memory session,
// freeing its extension/MCP subprocesses. The on-disk session is left intact.
// Returns a *acp.RequestError with code SessionNotFoundCode if the agent does
// not hold the session (see IsSessionNotFound).
func (a *AgentProc) ReleaseSession(ctx context.Context, sid acp.SessionId) error {
	_, err := acp.SendRequest[json.RawMessage](a.conn, ctx, "session/release", releaseSessionRequest{
		SessionId: sid,
	})
	return err
}

// IsSessionNotFound reports whether err is the typed ACP session-not-found
// error (JSON-RPC code SessionNotFoundCode). Relays use it to distinguish a
// recoverable "agent forgot this session" condition from other failures.
func IsSessionNotFound(err error) bool {
	if err == nil {
		return false
	}
	var re *acp.RequestError
	if errors.As(err, &re) {
		return re.Code == SessionNotFoundCode
	}
	return false
}

// ModelInfo is one entry in the agent's available-models list.
type ModelInfo struct {
	ID   string // "provider/modelID"
	Name string // human-readable label
}

// CommandInfo is one agent-advertised command (from an
// availableCommandsUpdate session notification).
type CommandInfo struct {
	Name        string // command name, e.g. "reload" (invoked as "/reload")
	Description string
}

// AvailableCommands returns a snapshot of the agent's last-advertised
// command catalog. Empty until the agent sends an availableCommandsUpdate.
// Safe for concurrent use.
func (a *AgentProc) AvailableCommands() []CommandInfo {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]CommandInfo, len(a.availableCommands))
	copy(out, a.availableCommands)
	return out
}

// Models returns a snapshot of the agent's last-seen available models.
// Empty until a session has been created (or ProbeModels has run).
func (a *AgentProc) Models() (models []ModelInfo, currentID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.models == nil {
		return nil, ""
	}
	out := make([]ModelInfo, len(a.models.models))
	copy(out, a.models.models)
	return out, a.models.current
}

// ProbeModels creates a throwaway session in the agent's cwd to read its
// available-models list, then cancels it. The cached snapshot is returned
// from Models() afterwards. Idempotent: a no-op if Models() already has
// a list.
func (a *AgentProc) ProbeModels(ctx context.Context) error {
	a.mu.Lock()
	if a.models != nil {
		a.mu.Unlock()
		return nil
	}
	a.mu.Unlock()

	probeCwd, err := os.MkdirTemp("", "acp-kit-probe-*")
	mustNot(err, "probe mkdir tmp")
	defer os.RemoveAll(probeCwd)

	// Use a noop sink — we don't care about updates.
	sid, err := a.NewSession(ctx, probeCwd, noopSink{}, nil)
	if err != nil {
		return fmt.Errorf("probe: new session: %w", err)
	}
	// Drop the sink; the probe session is never prompted so it stays
	// idle in the agent for the AgentProc's lifetime (no session/delete
	// RPC exists). Cost: one map entry on the agent side.
	a.mu.Lock()
	delete(a.sinks, sid)
	a.mu.Unlock()
	return nil
}

type noopSink struct{}

func (noopSink) OnUpdate(context.Context, acp.SessionNotification) error { return nil }

// ListSessions calls the unstable session/list. Caller must check Caps().ListSessions first.
func (a *AgentProc) ListSessions(ctx context.Context, cwd string) ([]SessionInfo, error) {
	resp, err := acp.SendRequest[listSessionsResponse](a.conn, ctx, "session/list", listSessionsRequest{Cwd: cwd})
	if err != nil {
		return nil, err
	}
	return resp.Sessions, nil
}

// ResumeSession calls the unstable session/resume and registers the sink
// for the resumed session. Caller must check Caps().ResumeSession first.
// The given sid is the agent-returned identifier (as listed by ListSessions).
func (a *AgentProc) ResumeSession(ctx context.Context, cwd string, sid acp.SessionId, sink SessionUpdateSink) error {
	resp, err := acp.SendRequest[sessionResponse](a.conn, ctx, "session/resume", resumeSessionRequest{
		SessionId:  string(sid),
		Cwd:        cwd,
		McpServers: a.cfg.mcpFor(cwd),
	})
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.sinks[sid] = sink
	if ms := resp.modelState(); ms != nil {
		a.models = ms
	}
	a.mu.Unlock()
	return nil
}

// Prompt sends a user message to the session. Returns the stop reason.
// The prompt is a sequence of ACP content blocks; callers build these
// from the latest user text plus any attachments.
func (a *AgentProc) Prompt(ctx context.Context, sid acp.SessionId, prompt []acp.ContentBlock) (acp.StopReason, error) {
	resp, err := acp.SendRequest[acp.PromptResponse](a.conn, ctx, acp.AgentMethodSessionPrompt, acp.PromptRequest{
		SessionId: sid,
		Prompt:    prompt,
	})
	if err != nil {
		return "", err
	}
	return resp.StopReason, nil
}

// Cancel requests cancellation of an in-flight prompt for a session.
func (a *AgentProc) Cancel(ctx context.Context, sid acp.SessionId) error {
	return a.conn.SendNotification(ctx, acp.AgentMethodSessionCancel, acp.CancelNotification{SessionId: sid})
}

// DropSession removes the sink for a session.
func (a *AgentProc) DropSession(sid acp.SessionId) {
	a.mu.Lock()
	delete(a.sinks, sid)
	a.mu.Unlock()
}

// RebindSink replaces the sink for an existing session id.
func (a *AgentProc) RebindSink(sid acp.SessionId, sink SessionUpdateSink) {
	a.mu.Lock()
	a.sinks[sid] = sink
	a.mu.Unlock()
}

// Authenticate invokes the ACP authenticate RPC. Modes:
//
//   - id == "" && redirect == "" && !cancel : start an interactive login.
//     The agent returns a fresh id (and URL) in AuthResult.
//   - id != "" && redirect != ""            : submit the pasted redirect.
//   - id != "" && cancel == true            : cancel that pending login.
//
// methodID must match the id advertised in the initialize response (e.g.
// "oauth-anthropic"). Requires an agent that supports the
// _meta.auth.interactive extension; older agents will run the legacy
// blocking flow and may return an empty AuthResult.
func (a *AgentProc) Authenticate(ctx context.Context, methodID, id, redirect string, cancel bool) (AuthResult, error) {
	authMeta := map[string]any{"interactive": true}
	if id != "" {
		authMeta["id"] = id
	}
	if redirect != "" {
		authMeta["redirect"] = redirect
	}
	if cancel {
		authMeta["cancel"] = true
	}
	params := map[string]any{
		"methodId": methodID,
		"_meta":    map[string]any{"auth": authMeta},
	}
	raw, err := acp.SendRequest[json.RawMessage](a.conn, ctx, acp.AgentMethodAuthenticate, params)
	if err != nil {
		return AuthResult{}, err
	}
	return parseAuthResult(raw), nil
}

// parseAuthResult extracts state/id/url/instructions from a raw authenticate
// response. Missing fields → zero values.
func parseAuthResult(raw json.RawMessage) AuthResult {
	var env struct {
		Meta struct {
			Auth struct {
				State        string `json:"state"`
				ID           string `json:"id"`
				URL          string `json:"url"`
				Instructions string `json:"instructions"`
			} `json:"auth"`
		} `json:"_meta"`
	}
	_ = json.Unmarshal(raw, &env)
	return AuthResult{
		State:        env.Meta.Auth.State,
		ID:           env.Meta.Auth.ID,
		URL:          env.Meta.Auth.URL,
		Instructions: env.Meta.Auth.Instructions,
	}
}

// AuthMethods returns the auth methods the agent advertised at Initialize.
// Empty if the agent didn't advertise any (or initialize hasn't run yet).
func (a *AgentProc) AuthMethods() []AuthMethod {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]AuthMethod, len(a.authMethods))
	copy(out, a.authMethods)
	return out
}

// closeGentleSignal is the signal Close sends first. Overridable in tests so
// the kill-fallback branch can be exercised with a child that ignores SIGINT.
var closeGentleSignal os.Signal = os.Interrupt

// ErrAgentClosed is the exit result reported by Err when the agent
// process went away because Close asked it to. It is the marker for
// "this death was expected"; anything else Err reports is an unexpected
// exit that the relay should treat as an outage.
var ErrAgentClosed = errors.New("acp agent closed")

// ErrAgentExited is the exit result reported by Err when the agent
// process exited on its own with status 0. It exists so Err is non-nil
// for EVERY terminated agent — a clean exit is still an outage for a
// relay that expected a long-lived child.
var ErrAgentExited = errors.New("acp agent exited")

// startReaper launches the one and only goroutine that calls cmd.Wait
// for this AgentProc. A child that was never started (Process == nil —
// the in-process fakes used by tests) has nothing to reap, so done stays
// open and Err keeps reporting "still running".
func (a *AgentProc) startReaper() {
	if a.cmd == nil || a.cmd.Process == nil {
		return
	}
	go a.reap()
}

// reap waits for the child, classifies the exit, publishes it and
// releases everyone blocked on Done.
//
// It calls cmd.Wait — the one place that ever does — which also closes
// the parent's ends of the stdio pipes. Anything the child wrote and the
// ACP read loop has not consumed by then is lost. That is the same tail
// truncation Close has always had, now also on the unexpected-exit path,
// and it is the price of never leaking the pipe fds: an agent that dies
// is not going to finish its response anyway.
func (a *AgentProc) reap() {
	err := a.cmd.Wait()
	switch {
	case a.closing.Load():
		// Close (or a Close racing a spontaneous exit) — expected.
		err = ErrAgentClosed
	case err == nil:
		err = ErrAgentExited
	}
	a.exitErr.Store(&err)
	close(a.done)
}

// Done returns a channel that is closed once the agent process has
// exited, for any reason. It never carries a value; call Err for the
// exit result. Modelled on context.Context.Done/Err.
//
// A relay watches this to notice that its agent died out from under it:
//
//	go func() {
//		<-agent.Done()
//		if err := agent.Err(); !errors.Is(err, client.ErrAgentClosed) {
//			log.Printf("agent exited unexpectedly: %v", err)
//			os.Exit(1) // let the supervisor rebuild us
//		}
//	}()
func (a *AgentProc) Done() <-chan struct{} { return a.done }

// Err reports why the agent process is gone: nil while it is still
// running, ErrAgentClosed after a Close, ErrAgentExited after a clean
// self-exit, and otherwise the *exec.ExitError (or wait error) from the
// child. It is safe to call concurrently at any time and never blocks,
// so callers can use it to classify a failed ACP call as "the agent is
// dead" rather than string-matching broken-pipe errors.
func (a *AgentProc) Err() error {
	if p := a.exitErr.Load(); p != nil {
		return *p
	}
	return nil
}

// Close terminates the agent process. Returns after the process has
// exited (or been force-killed). The exit itself is observed by the
// single reaper goroutine — Close consumes that result via Done rather
// than racing a second cmd.Wait.
func (a *AgentProc) Close() error {
	if a.cmd == nil || a.cmd.Process == nil {
		return nil
	}
	// Mark the shutdown deliberate BEFORE signalling, so the reaper
	// classifies the exit as ErrAgentClosed and no watcher mistakes an
	// orderly stop for an outage.
	a.closing.Store(true)
	// Try a gentle stop first; fall through to Kill after a short grace.
	grace := a.cfg.CloseGrace
	if grace <= 0 {
		grace = 2 * time.Second
	}
	_ = a.cmd.Process.Signal(closeGentleSignal)
	select {
	case <-a.done:
		return nil
	case <-time.After(grace):
		_ = a.cmd.Process.Kill()
		<-a.done
		return nil
	}
}

// ---- Inbound dispatch (server-initiated calls from the agent) ----

// dispatch routes inbound JSON-RPC requests to the appropriate handler.
// Mirrors the SDK's ClientSideConnection.handle but lives here so we can
// own the underlying Connection.
func (a *AgentProc) dispatch(ctx context.Context, method string, params json.RawMessage) (any, *acp.RequestError) {
	switch method {
	case acp.ClientMethodSessionUpdate:
		var p acp.SessionNotification
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, acp.NewInvalidParams(map[string]any{"error": err.Error()})
		}
		if err := a.sessionUpdate(ctx, p); err != nil {
			return nil, toReqErr(err)
		}
		return nil, nil
	case acp.ClientMethodSessionRequestPermission:
		var p acp.RequestPermissionRequest
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, acp.NewInvalidParams(map[string]any{"error": err.Error()})
		}
		return a.cfg.Policy.Decide(ctx, p), nil
	case acp.ClientMethodFsReadTextFile:
		var p acp.ReadTextFileRequest
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, acp.NewInvalidParams(map[string]any{"error": err.Error()})
		}
		resp, err := a.readTextFile(p)
		if err != nil {
			return nil, toReqErr(err)
		}
		return resp, nil
	case acp.ClientMethodFsWriteTextFile:
		var p acp.WriteTextFileRequest
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, acp.NewInvalidParams(map[string]any{"error": err.Error()})
		}
		if err := a.writeTextFile(p); err != nil {
			return nil, toReqErr(err)
		}
		return acp.WriteTextFileResponse{}, nil
	default:
		// Terminal methods and any unknown call: we never advertised
		// the capability, so the agent shouldn't be calling these.
		return nil, acp.NewMethodNotFound(method)
	}
}

func toReqErr(err error) *acp.RequestError {
	return acp.NewInternalError(map[string]any{"error": err.Error()})
}

func (a *AgentProc) sinkFor(sid acp.SessionId) SessionUpdateSink {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sinks[sid]
}

// sessionUpdate fans out to the per-session sink.
func (a *AgentProc) sessionUpdate(ctx context.Context, params acp.SessionNotification) error {
	// Snapshot the agent's advertised command catalog as it arrives, so
	// AvailableCommands() reflects the latest update regardless of which
	// session carried it. (Commands are a process-global concern in fir
	// and similar agents.)
	if u := params.Update.AvailableCommandsUpdate; u != nil {
		cmds := make([]CommandInfo, 0, len(u.AvailableCommands))
		for _, c := range u.AvailableCommands {
			cmds = append(cmds, CommandInfo{Name: c.Name, Description: c.Description})
		}
		a.mu.Lock()
		a.availableCommands = cmds
		a.mu.Unlock()
	}
	if s := a.sinkFor(params.SessionId); s != nil {
		return s.OnUpdate(ctx, params)
	}
	return nil
}

// readTextFile reads from disk relative to the agent's cwd.
func (a *AgentProc) readTextFile(params acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error) {
	if !filepath.IsAbs(params.Path) {
		return acp.ReadTextFileResponse{}, fmt.Errorf("path must be absolute: %s", params.Path)
	}
	b, err := os.ReadFile(params.Path) //nolint:gosec // path is agent-driven within its own cwd
	if err != nil {
		return acp.ReadTextFileResponse{}, err
	}
	content := string(b)
	if params.Line != nil || params.Limit != nil {
		lines := strings.Split(content, "\n")
		start := 0
		if params.Line != nil && *params.Line > 0 {
			start = *params.Line - 1
			if start > len(lines) {
				start = len(lines)
			}
		}
		end := len(lines)
		if params.Limit != nil && *params.Limit > 0 && start+*params.Limit < end {
			end = start + *params.Limit
		}
		content = strings.Join(lines[start:end], "\n")
	}
	return acp.ReadTextFileResponse{Content: content}, nil
}

// writeTextFile writes to disk. Skeleton: gated to agent cwd in the future.
func (a *AgentProc) writeTextFile(params acp.WriteTextFileRequest) error {
	if !filepath.IsAbs(params.Path) {
		return fmt.Errorf("path must be absolute: %s", params.Path)
	}
	if err := os.MkdirAll(filepath.Dir(params.Path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(params.Path, []byte(params.Content), 0o644) //nolint:gosec // ditto
}
