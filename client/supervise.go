package client

// Agent supervision.
//
// An AgentProc runs a sequence of child "generations". Each generation is
// one spawned process plus the ACP connection over its stdio, and has its
// own reaper goroutine (the only caller of that child's cmd.Wait). When the
// current generation exits and the exit was not asked for by Close, the
// reaper logs the exit status together with the tail of the child's stderr
// and — unless Config.NoRespawn — hands off to a supervisor goroutine that
// re-spawns the agent with capped exponential backoff and re-runs
// initialize.
//
// Sessions survive a respawn from the relay's point of view: the id the
// relay holds keeps working. Every session opened through this AgentProc
// is recorded (sessRec) with the generation it lives on; the first
// session-scoped call after a respawn re-establishes it on the new child —
// session/resume, else session/load (its history replay is muted), else a
// fresh session/new whose new wire id is aliased back to the relay's id.
//
// Requests in flight when a child dies fail fast with ErrAgentDied rather
// than hanging: each request is bound to its generation and cancelled the
// moment that generation is reaped.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	acp "github.com/coder/acp-go-sdk"
)

// ErrAgentClosed is the exit result reported by Err when the agent
// process went away because Close asked it to. It is the marker for
// "this death was expected"; anything else Err reports is an unexpected
// exit that the relay should treat as an outage.
var ErrAgentClosed = errors.New("acp agent closed")

// ErrAgentExited is the exit result reported when the agent process
// exited on its own with status 0. It exists so every terminated child
// has a non-nil exit result — a clean exit is still an outage for a relay
// that expected a long-lived child.
var ErrAgentExited = errors.New("acp agent exited")

// ErrAgentDied is returned (wrapped, together with the child's exit
// result) by calls that were in flight when the agent child died, and by
// calls made while a replacement child is being spawned. With supervision
// on, retrying after a moment will reach the respawned agent.
var ErrAgentDied = errors.New("acp agent died")

// closeGentleSignal is the signal Close sends first. Overridable in tests so
// the kill-fallback branch can be exercised with a child that ignores SIGINT.
var closeGentleSignal os.Signal = os.Interrupt

// Tunables. Variables so tests can shrink them.
var (
	// stderrTailLines is how many trailing stderr lines are kept per
	// child for the exit log.
	stderrTailLines = 50
	// waitDelay bounds how long cmd.Wait waits for the child's stdio to
	// drain after it exits. Without it a grandchild that inherited
	// stderr would keep Wait (and so death detection) blocked forever.
	waitDelay = 2 * time.Second
	// diedGrace is how long a failed request waits for its child to be
	// reaped when the connection dropped first, so the error it returns
	// can say why the agent went away. Past it the child is killed.
	diedGrace = 2 * time.Second
	// handshakeTimeout bounds initialize on a respawned child.
	handshakeTimeout = 30 * time.Second
)

const (
	defaultRespawnMin     = time.Second
	defaultRespawnMax     = 60 * time.Second
	defaultRespawnHealthy = 60 * time.Second
)

// gen is one child generation.
type gen struct {
	n       uint64
	cmd     *exec.Cmd
	conn    *acp.Connection
	tail    *stderrTail // nil for in-process fakes
	started time.Time
	done    chan struct{} // closed by the reaper once cmd.Wait returned
	err     error         // exit result; valid once done is closed
}

func (g *gen) dead() bool {
	select {
	case <-g.done:
		return true
	default:
		return false
	}
}

// hasProcess reports whether g is a real, started child.
func (g *gen) hasProcess() bool { return g.cmd != nil && g.cmd.Process != nil }

// sessRec is what is needed to re-establish one relay session on a new
// child. Its fields are guarded by mu, which also serialises recovery.
type sessRec struct {
	mu   sync.Mutex
	cwd  string
	meta map[string]any // session/new _meta, reused by the new-session fallback
	gen  uint64         // generation the session is live on
	wire acp.SessionId  // id the current child knows it by
}

// newProc builds an AgentProc with no child attached yet.
func newProc(ctx context.Context, cfg Config) *AgentProc {
	return &AgentProc{
		cfg:      cfg,
		ctx:      ctx,
		sinks:    make(map[acp.SessionId]SessionUpdateSink),
		stats:    make(map[acp.SessionId]SessionStats),
		curModel: make(map[acp.SessionId]string),
		sess:     make(map[acp.SessionId]*sessRec),
		alias:    make(map[acp.SessionId]acp.SessionId),
		muted:    make(map[acp.SessionId]bool),
		done:     make(chan struct{}),
		closeCh:  make(chan struct{}),
		genCh:    make(chan struct{}),
	}
}

func (a *AgentProc) logger() *slog.Logger {
	if a.cfg.Logger != nil {
		return a.cfg.Logger
	}
	return slog.Default()
}

// Start launches the agent process, performs Initialize (capturing caps),
// and returns a ready-to-use AgentProc. Unless cfg.NoRespawn is set, the
// child is supervised: see the package-level notes in supervise.go.
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
	a := newProc(ctx, cfg)
	a.respawn = !cfg.NoRespawn
	if err := a.spawn(ctx); err != nil {
		return nil, err
	}
	return a, nil
}

// spawn starts one child generation and, once initialize succeeded,
// installs it as current. On failure the child is killed and reaped.
func (a *AgentProc) spawn(ctx context.Context) error {
	cfg := a.cfg
	cmd := exec.CommandContext(a.ctx, cfg.Command[0], cfg.Command[1:]...) //nolint:gosec // user-configured command
	cmd.Dir = cfg.Cwd
	if env := cfg.scrubbedEnv(); env != nil {
		cmd.Env = env
	}
	dst := cfg.Stderr
	if dst == nil {
		dst = os.Stderr
	}
	tail := newStderrTail(stderrTailLines)
	cmd.Stderr = io.MultiWriter(dst, tail)
	cmd.WaitDelay = waitDelay
	stdin, err := cmd.StdinPipe()
	mustNot(err, "stdin pipe")
	stdout, err := cmd.StdoutPipe()
	mustNot(err, "stdout pipe")
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start agent: %w", err)
	}
	g, err := a.attach(ctx, cmd, tail, stdin, stdout)
	if err != nil {
		_ = cmd.Process.Kill()
		<-g.done
		return err
	}
	return nil
}

// connect performs the post-spawn ACP handshake and returns a wired
// AgentProc. Package-private; real callers go through Start. Tests use it to
// drive the handshake against an in-process fake agent over io.Pipe pairs.
// An AgentProc built this way never respawns.
func connect(ctx context.Context, cfg Config, cmd *exec.Cmd, stdin io.WriteCloser, stdout io.Reader) (*AgentProc, error) {
	a := newProc(ctx, cfg)
	if _, err := a.attach(ctx, cmd, nil, stdin, stdout); err != nil {
		return nil, err
	}
	return a, nil
}

// errClosing is attach's result when Close won the race against a respawn.
var errClosing = errors.New("acp agent closing")

// attach wires a started child into a new generation, runs initialize and
// installs the generation as current. The generation is returned even on
// error so the caller can wait for it to be reaped.
func (a *AgentProc) attach(ctx context.Context, cmd *exec.Cmd, tail *stderrTail, stdin io.WriteCloser, stdout io.Reader) (*gen, error) {
	g := &gen{cmd: cmd, tail: tail, started: time.Now(), done: make(chan struct{})}
	// The reaper starts before the handshake so a child that dies during
	// initialize is still reaped (and fails the handshake fast).
	a.startReaper(g)
	g.conn = acp.NewConnection(a.dispatch, stdin, stdout)

	clientMeta := map[string]any{
		"session.systemPrompt": map[string]any{"version": 1},
	}
	for k, v := range a.cfg.ClientMeta {
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
	// A raw response, so the unstable sessionCapabilities sub-object
	// the SDK's typed struct drops can be read.
	raw, err := rpc[json.RawMessage](ctx, g, acp.AgentMethodInitialize, initParams)
	if err != nil {
		return g, fmt.Errorf("acp initialize: %w", err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closing.Load() {
		return g, errClosing
	}
	a.genSeq++
	g.n = a.genSeq
	a.cur = g
	close(a.genCh)
	a.genCh = make(chan struct{})
	a.caps = parseCaps(raw)
	a.authMethods = parseAuthMethods(raw)
	a.agentInfo = parseAgentInfo(raw)
	return g, nil
}

// startReaper launches the one goroutine that calls cmd.Wait for g. A
// child that was never started (the in-process fakes used by tests) has
// nothing to reap, so g.done stays open.
func (a *AgentProc) startReaper(g *gen) {
	if !g.hasProcess() {
		return
	}
	go a.reap(g)
}

// reap waits for g's child, classifies the exit, publishes it, and reacts.
//
// cmd.Wait also closes the parent's ends of the stdio pipes; anything the
// child wrote that the ACP read loop has not consumed is lost. An agent
// that died is not going to finish its response anyway.
func (a *AgentProc) reap(g *gen) {
	err := g.cmd.Wait()
	switch {
	case a.closing.Load():
		err = ErrAgentClosed
	case err == nil:
		err = ErrAgentExited
	}
	g.err = err
	close(g.done)
	a.onExit(g)
}

// onExit reacts to the exit of generation g. Only the current generation
// matters: a child that died during its own handshake is handled by
// whoever spawned it.
func (a *AgentProc) onExit(g *gen) {
	a.mu.Lock()
	cur := a.cur == g
	a.mu.Unlock()
	if !cur {
		return
	}
	if a.closing.Load() {
		a.finish(ErrAgentClosed)
		return
	}
	if a.ctx.Err() == nil {
		a.logExit(g)
	}
	if !a.respawn {
		a.finish(g.err)
		return
	}
	go a.supervise(g)
}

// logExit records an unexpected exit: the exit status (code or signal),
// how long the child ran, and the last lines of its stderr.
func (a *AgentProc) logExit(g *gen) {
	code, sig := exitStatus(g.err)
	a.logger().Error("acp agent exited unexpectedly",
		"err", g.err,
		"exit_code", code,
		"signal", sig,
		"pid", g.cmd.Process.Pid,
		"uptime", time.Since(g.started).Round(time.Millisecond),
		"respawn", a.respawn,
		"stderr_tail", g.tail.String(),
	)
}

// exitStatus extracts the exit code and, if the child was killed by a
// signal, the signal's name. Code is -1 when unknown or signalled.
func exitStatus(err error) (code int, signal string) {
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		if errors.Is(err, ErrAgentExited) {
			return 0, ""
		}
		return -1, ""
	}
	if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return -1, ws.Signal().String()
	}
	return ee.ExitCode(), ""
}

// supervise re-spawns the agent after generation dead exited, retrying
// with capped exponential backoff until it succeeds, Close is called, or
// the Start context ends.
func (a *AgentProc) supervise(dead *gen) {
	lo, hi, healthy := a.cfg.respawnTimings()
	if time.Since(dead.started) >= healthy {
		a.backoff = 0
	}
	for {
		a.backoff = min(max(a.backoff*2, lo), hi)
		a.logger().Warn("acp agent: respawning", "in", a.backoff)
		select {
		case <-a.closeCh:
			a.finish(ErrAgentClosed)
			return
		case <-a.ctx.Done():
			a.finish(a.ctx.Err())
			return
		case <-time.After(a.backoff):
		}
		hctx, cancel := context.WithTimeout(a.ctx, handshakeTimeout)
		go func() {
			select {
			case <-a.closeCh:
				cancel()
			case <-hctx.Done():
			}
		}()
		err := a.spawn(hctx)
		cancel()
		if err == nil {
			n := a.restarts.Add(1)
			a.logger().Info("acp agent respawned", "restarts", n)
			return
		}
		if a.closing.Load() {
			a.finish(ErrAgentClosed)
			return
		}
		a.logger().Error("acp agent: respawn failed", "err", err)
	}
}

func (c Config) respawnTimings() (lo, hi, healthy time.Duration) {
	lo, hi, healthy = c.RespawnMin, c.RespawnMax, c.RespawnHealthy
	if lo <= 0 {
		lo = defaultRespawnMin
	}
	if hi < lo {
		hi = max(defaultRespawnMax, lo)
	}
	if healthy <= 0 {
		healthy = defaultRespawnHealthy
	}
	return lo, hi, healthy
}

// finish marks the AgentProc permanently gone. First call wins.
func (a *AgentProc) finish(err error) {
	a.finishOnce.Do(func() {
		a.exitErr.Store(&err)
		close(a.done)
	})
}

// Restarts reports how many times the agent has been re-spawned.
func (a *AgentProc) Restarts() int { return int(a.restarts.Load()) }

// Done returns a channel that is closed once the AgentProc is permanently
// finished: after Close, after the context given to Start ends, or — only
// with Config.NoRespawn — when the child exits. With supervision on (the
// default) an unexpected exit does NOT close Done: the child is re-spawned.
// Call Err for the result. Modelled on context.Context.Done/Err.
func (a *AgentProc) Done() <-chan struct{} { return a.done }

// Err reports why the AgentProc is finished: nil while it is running (or
// re-spawning), ErrAgentClosed after a Close, and otherwise the terminal
// cause — with NoRespawn, ErrAgentExited for a clean self-exit or the
// *exec.ExitError from the child. Safe to call concurrently; never blocks.
func (a *AgentProc) Err() error {
	if p := a.exitErr.Load(); p != nil {
		return *p
	}
	return nil
}

// Close terminates the agent process and stops supervision. Returns after
// the process has exited (or been force-killed).
func (a *AgentProc) Close() error {
	a.mu.Lock()
	g := a.cur
	a.mu.Unlock()
	if g == nil || !g.hasProcess() {
		return nil
	}
	// Mark the shutdown deliberate BEFORE signalling, so the exit is
	// classified as ErrAgentClosed and no respawn is attempted.
	a.closing.Store(true)
	a.closeOnce.Do(func() { close(a.closeCh) })
	// Re-read: a respawn may have installed a new child before closing
	// was set.
	a.mu.Lock()
	g = a.cur
	a.mu.Unlock()
	if !g.dead() {
		grace := a.cfg.CloseGrace
		if grace <= 0 {
			grace = 2 * time.Second
		}
		_ = g.cmd.Process.Signal(closeGentleSignal)
		select {
		case <-g.done:
		case <-time.After(grace):
			_ = g.cmd.Process.Kill()
		}
	}
	<-a.done
	return nil
}

// live returns the current generation. If its child is dead and a
// replacement is being spawned, live waits for it (bounded by ctx); if the
// AgentProc is finished it fails with ErrAgentDied.
func (a *AgentProc) live(ctx context.Context) (*gen, error) {
	for {
		a.mu.Lock()
		g, next := a.cur, a.genCh
		a.mu.Unlock()
		if !g.dead() {
			return g, nil
		}
		if !a.respawn {
			return nil, diedErr(g)
		}
		select {
		case <-next:
		case <-a.done:
			return nil, diedErr(g)
		case <-ctx.Done():
			return nil, fmt.Errorf("%w (%w); waiting for respawn: %w", ErrAgentDied, g.err, ctx.Err())
		}
	}
}

func diedErr(g *gen) error {
	return fmt.Errorf("%w (%w)", ErrAgentDied, g.err)
}

// rpc sends one request on generation g. The request is abandoned the
// moment g's child is reaped, and any failure caused by the child dying
// is reported as ErrAgentDied wrapping its exit result. A child whose
// stdio broke but which does not exit within diedGrace is killed.
func rpc[T any](ctx context.Context, g *gen, method string, params any) (T, error) {
	rctx, stop := context.WithCancel(ctx)
	defer stop()
	go func() {
		select {
		case <-g.done:
			stop()
		case <-rctx.Done():
		}
	}()
	res, err := acp.SendRequest[T](g.conn, rctx, method, params)
	if err == nil || !g.hasProcess() || ctx.Err() != nil {
		return res, err
	}
	// The connection usually notices the dead pipe before the reaper
	// has the exit status: give the reaper a moment.
	select {
	case <-g.conn.Done():
	default:
		if !g.dead() && !isPipeErr(err) {
			return res, err
		}
	}
	select {
	case <-g.done:
		return res, diedErr(g)
	case <-time.After(diedGrace):
		// Alive but not talking: a wedged child would fail every
		// later call the same way. Kill it so supervision takes over.
		_ = g.cmd.Process.Kill()
		<-g.done
		return res, diedErr(g)
	}
}

// isPipeErr reports whether err is a write to a child's closed stdin —
// the SDK reports those as JSON-RPC internal errors carrying the OS text.
func isPipeErr(err error) bool {
	s := err.Error()
	return strings.Contains(s, "broken pipe") || strings.Contains(s, "file already closed") || strings.Contains(s, "closed pipe")
}

// track records a session opened on generation g for respawn recovery.
func (a *AgentProc) track(sid acp.SessionId, cwd string, meta map[string]any, g *gen) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.untrackLocked(sid)
	a.sess[sid] = &sessRec{cwd: cwd, meta: meta, gen: g.n, wire: sid}
}

// untrackLocked forgets sid and any wire alias pointing at it. a.mu held.
func (a *AgentProc) untrackLocked(sid acp.SessionId) {
	delete(a.sess, sid)
	for w, c := range a.alias {
		if c == sid {
			delete(a.alias, w)
		}
	}
}

// callerFor maps a wire session id from the agent to the id the relay
// holds. ok is false for updates that belong to a muted session/load
// replay.
func (a *AgentProc) callerFor(wire acp.SessionId) (sid acp.SessionId, ok bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.muted[wire] {
		return "", false
	}
	if c, found := a.alias[wire]; found {
		return c, true
	}
	return wire, true
}

// peek returns the live generation and sid's wire id on it, without
// re-establishing a stale session. For calls (cancel, release) that are
// meaningless for a session the new child has never seen.
func (a *AgentProc) peek(ctx context.Context, sid acp.SessionId) (*gen, acp.SessionId, error) {
	g, err := a.live(ctx)
	if err != nil {
		return nil, "", err
	}
	a.mu.Lock()
	rec := a.sess[sid]
	a.mu.Unlock()
	if rec == nil {
		return g, sid, nil
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return g, rec.wire, nil
}

// route returns the live generation and sid's wire id on it, first
// re-establishing the session if it was opened on an earlier child.
func (a *AgentProc) route(ctx context.Context, sid acp.SessionId) (*gen, acp.SessionId, error) {
	g, err := a.live(ctx)
	if err != nil {
		return nil, "", err
	}
	a.mu.Lock()
	rec := a.sess[sid]
	a.mu.Unlock()
	if rec == nil {
		return g, sid, nil
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.gen == g.n {
		return g, rec.wire, nil
	}
	if err := a.recoverSession(ctx, g, sid, rec); err != nil {
		return nil, "", err
	}
	return g, rec.wire, nil
}

// recoverSession re-establishes rec on generation g: session/resume if the
// agent supports it, else session/load (history replay muted), else a new
// session under the same cwd and _meta. rec.mu held.
func (a *AgentProc) recoverSession(ctx context.Context, g *gen, sid acp.SessionId, rec *sessRec) error {
	caps := a.Caps()
	mcp := a.cfg.mcpFor(rec.cwd)
	log := a.logger().With("session", sid, "wire", rec.wire)
	if caps.ResumeSession {
		resp, err := rpc[sessionResponse](ctx, g, "session/resume", resumeSessionRequest{
			SessionId: string(rec.wire), Cwd: rec.cwd, McpServers: mcp,
		})
		if err == nil {
			a.adopt(sid, rec, g, rec.wire, resp)
			log.Info("acp agent: session resumed after respawn")
			return nil
		}
		if errors.Is(err, ErrAgentDied) {
			return err
		}
		log.Warn("acp agent: session/resume after respawn failed", "err", err)
	}
	if caps.LoadSession {
		a.setMuted(rec.wire, true)
		resp, err := rpc[sessionResponse](ctx, g, acp.AgentMethodSessionLoad, acp.LoadSessionRequest{
			SessionId: rec.wire, Cwd: rec.cwd, McpServers: mcp,
		})
		a.setMuted(rec.wire, false)
		if err == nil {
			a.adopt(sid, rec, g, rec.wire, resp)
			log.Info("acp agent: session loaded after respawn")
			return nil
		}
		if errors.Is(err, ErrAgentDied) {
			return err
		}
		log.Warn("acp agent: session/load after respawn failed", "err", err)
	}
	resp, err := rpc[sessionResponse](ctx, g, acp.AgentMethodSessionNew, acp.NewSessionRequest{
		Cwd: rec.cwd, McpServers: mcp, Meta: rec.meta,
	})
	if err != nil {
		return fmt.Errorf("re-establish session %s after agent respawn: %w", sid, err)
	}
	a.adopt(sid, rec, g, resp.SessionId, resp)
	log.Warn("acp agent: session re-created after respawn; history lost", "new_wire", resp.SessionId)
	return nil
}

func (a *AgentProc) setMuted(wire acp.SessionId, on bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if on {
		a.muted[wire] = true
	} else {
		delete(a.muted, wire)
	}
}

// adopt records that rec now lives on g under wire. rec.mu held.
func (a *AgentProc) adopt(sid acp.SessionId, rec *sessRec, g *gen, wire acp.SessionId, resp sessionResponse) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.alias, rec.wire)
	if wire != sid {
		a.alias[wire] = sid
	}
	rec.wire = wire
	rec.gen = g.n
	a.noteConfig(sid, resp.ConfigOptions)
	a.noteModels(sid, resp.modelState())
}

// stderrTail is an io.Writer keeping the last n lines written to it.
type stderrTail struct {
	mu      sync.Mutex
	n       int
	lines   []string
	partial []byte
}

// maxTailLine caps one retained line, so a child spewing one endless line
// cannot grow the buffer without bound.
const maxTailLine = 4096

func newStderrTail(n int) *stderrTail { return &stderrTail{n: n} }

func (t *stderrTail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	rest := p
	for len(rest) > 0 {
		i := strings.IndexByte(string(rest), '\n')
		chunk := rest
		if i >= 0 {
			chunk = rest[:i]
		}
		if room := maxTailLine - len(t.partial); room > 0 {
			t.partial = append(t.partial, chunk[:min(len(chunk), room)]...)
		}
		if i < 0 {
			break
		}
		t.push(string(t.partial))
		t.partial = t.partial[:0]
		rest = rest[i+1:]
	}
	return len(p), nil
}

func (t *stderrTail) push(line string) {
	if len(t.lines) == t.n {
		copy(t.lines, t.lines[1:])
		t.lines = t.lines[:t.n-1]
	}
	t.lines = append(t.lines, line)
}

// String returns the retained lines, oldest first, plus any unterminated
// final line. Nil-safe.
func (t *stderrTail) String() string {
	if t == nil {
		return ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	out := strings.Join(t.lines, "\n")
	if len(t.partial) > 0 {
		if out != "" {
			out += "\n"
		}
		out += string(t.partial)
	}
	return out
}
