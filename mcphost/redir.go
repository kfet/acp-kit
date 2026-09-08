package mcphost

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"
)

// RedirConfig configures the redirector entrypoint. Subcommand and the
// env var names must match the Host that spawned it (they run in the same
// consumer binary).
type RedirConfig struct {
	// Subcommand is the primary argv token the redirector dispatches on.
	Subcommand string
	// Aliases are additional accepted argv tokens (e.g. a deprecated
	// legacy name).
	Aliases []string
	// EnvSocket / EnvToken are the env var names carrying the socket path
	// and token.
	EnvSocket string
	EnvToken  string
}

// MaybeRunRedir inspects os.Args: if os.Args[1] matches the configured
// subcommand or one of its aliases, it runs the redirector and returns
// handled=true with the run result. Otherwise it returns handled=false
// and the consumer continues normal startup. Call it early in main.
func MaybeRunRedir(cfg RedirConfig) (handled bool, err error) {
	if len(os.Args) <= 1 {
		return false, nil
	}
	arg := os.Args[1]
	if arg != cfg.Subcommand {
		match := false
		for _, a := range cfg.Aliases {
			if arg == a {
				match = true
				break
			}
		}
		if !match {
			return false, nil
		}
	}
	return true, RunRedir(cfg)
}

// RunRedir is the redirector entrypoint. It reads the socket path and
// token from the env the parent set, dials the socket, and proxies MCP
// JSON-RPC lines between the agent's stdio and the host, reconnecting
// across a host restart. See redirect for the proxy's semantics.
func RunRedir(cfg RedirConfig) error {
	socket := os.Getenv(cfg.EnvSocket)
	token := os.Getenv(cfg.EnvToken)
	if socket == "" {
		return fmt.Errorf("mcphost redir: %s not set", cfg.EnvSocket)
	}
	return redirect(socket, token, os.Stdin, os.Stdout)
}

// Reconnect policy. Vars, not consts, so tests can compress the schedule.
//
// The redirector outlives the host process: a consumer that re-execs in
// place is unreachable for the moment between the predecessor releasing
// the socket and the successor binding it. Redialling on a 50ms→2s
// schedule closes a sub-second gap invisibly, and giving up after ~30s
// then EXITING is deliberate — the agent must see a dead MCP server it
// can report, not a hung one it waits on forever.
var (
	redirBackoffMin = 50 * time.Millisecond
	redirBackoffMax = 2 * time.Second
	redirGiveUp     = 30 * time.Second
	// redirReplayTimeout bounds the wait for the replayed initialize
	// response, so a wedged host cannot hang the proxy.
	redirReplayTimeout = 10 * time.Second
	// redirDial is net.Dial on the unix socket, indirected so tests can
	// substitute a connection with controllable failure behaviour.
	redirDial = func(socket string) (net.Conn, error) { return net.Dial("unix", socket) }
)

// redirReinitID is the JSON-RPC id used for the replayed initialize. It
// is deliberately not a number and namespaced, so it can never collide
// with an id the agent chose; its response is consumed by the proxy and
// never forwarded.
const redirReinitID = `"__mcphost_reinit"`

// redirErrConnLost is the JSON-RPC error code reported for a request that
// was in flight when the host went away. It sits in the implementation-
// defined server range (-32000..-32099).
const redirErrConnLost = -32000

// redirect proxies MCP JSON-RPC lines between the agent (in/out) and the
// host's unix socket, surviving a host restart.
//
// It is a thin STATEFUL proxy rather than a pipe, because the host's MCP
// state is per-connection: a successor that just bound the socket has
// never seen this session's `initialize`, so the proxy replays it (and
// the `notifications/initialized` that followed, if any) before forwarding
// anything else. Without that, every subsequent tools/* call would hit an
// uninitialised server.
//
// A request that was in flight when the connection dropped is failed back
// to the agent with a JSON-RPC error rather than replayed. Replaying is
// the wrong default here: a tools/call may already have run to completion
// server-side, and duplicating a side effect (posting a message twice) is
// worse than a clean, retryable error. Either way the agent is never left
// waiting on a response that will not come.
//
// It returns when the agent closes stdin (nil), or when the host stays
// unreachable past redirGiveUp (error).
func redirect(socket, token string, in io.Reader, out io.Writer) error {
	p := &proxy{socket: socket, token: token, out: out, pending: make(map[string]bool)}
	return p.run(in)
}

// proxy holds the connection to the host plus the small amount of MCP
// session state needed to rebuild it.
type proxy struct {
	socket string
	token  string
	out    io.Writer

	// outMu serialises writes to the agent: responses forwarded from the
	// host race the synthetic errors emitted when a connection drops.
	outMu sync.Mutex

	// mu guards everything below. It is also held across a reconnect, so
	// no agent traffic can overtake the replayed handshake.
	mu      sync.Mutex
	conn    net.Conn
	br      *bufio.Reader
	gen     uint64 // bumped on every successful (re)connect
	closing bool
	err     error

	initParams json.RawMessage // params of the agent's initialize
	haveInit   bool
	initedNote bool            // agent sent notifications/initialized
	pending    map[string]bool // raw JSON ids awaiting a response

	done chan struct{}
}

func (p *proxy) run(in io.Reader) error {
	p.mu.Lock()
	err := p.connectLocked()
	p.mu.Unlock()
	if err != nil {
		return err
	}
	p.done = make(chan struct{})
	go p.fromHost()
	// fromAgent runs alongside rather than inline: if the proxy dies (the
	// host never comes back), run must return so the process can exit,
	// instead of blocking forever on a stdin read that will not complete.
	go p.fromAgent(in)
	<-p.done

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.conn != nil {
		_ = p.conn.Close()
		p.conn = nil
	}
	return p.err
}

// --- connection management ---------------------------------------------

// connectLocked drops any dead connection, fails everything that was in
// flight on it, redials with backoff, and replays the MCP handshake. On
// the first call there is nothing pending and no handshake to replay, so
// it is just a dial.
func (p *proxy) connectLocked() error {
	if p.conn != nil {
		_ = p.conn.Close()
		p.conn = nil
	}
	p.failPendingLocked()
	if err := p.dialLocked(); err != nil {
		return err
	}
	return p.replayLocked()
}

// dialLocked redials the socket on the backoff schedule and writes the
// auth preamble.
func (p *proxy) dialLocked() error {
	deadline := time.Now().Add(redirGiveUp)
	backoff := redirBackoffMin
	var conn net.Conn
	for {
		c, err := redirDial(p.socket)
		if err == nil {
			conn = c
			break
		}
		if !time.Now().Add(backoff).Before(deadline) {
			return fmt.Errorf("mcphost redir: dial %s: %w", p.socket, err)
		}
		time.Sleep(backoff)
		if backoff *= 2; backoff > redirBackoffMax {
			backoff = redirBackoffMax
		}
	}
	pre, _ := json.Marshal(struct {
		Token string `json:"token"`
	}{Token: p.token}) // a string field cannot fail to marshal
	if _, err := conn.Write(append(pre, '\n')); err != nil {
		_ = conn.Close()
		return fmt.Errorf("mcphost redir: write preamble: %w", err)
	}
	p.conn = conn
	p.br = bufio.NewReaderSize(conn, 1<<20)
	p.gen++
	return nil
}

// replayLocked re-runs the MCP handshake on a freshly dialled connection
// and swallows its response, so the agent never sees the second
// initialize it did not send.
func (p *proxy) replayLocked() error {
	if !p.haveInit {
		return nil
	}
	req, _ := json.Marshal(rpcMessage{
		JSONRPC: "2.0",
		ID:      json.RawMessage(redirReinitID),
		Method:  "initialize",
		Params:  p.initParams,
	}) // fields are all pre-validated JSON
	if _, err := p.conn.Write(append(req, '\n')); err != nil {
		return fmt.Errorf("mcphost redir: replay initialize: %w", err)
	}
	// Bound the wait: a wedged host must not hang the proxy forever.
	_ = p.conn.SetReadDeadline(time.Now().Add(redirReplayTimeout))
	defer func() { _ = p.conn.SetReadDeadline(time.Time{}) }()
	for {
		line, err := p.br.ReadBytes('\n')
		if len(line) > 0 {
			var msg rpcMessage
			if json.Unmarshal(line, &msg) == nil && rawID(msg.ID) == redirReinitID {
				break // our replay's response; consumed, not forwarded
			}
			// A fresh connection carries no other traffic, but forward
			// anything unexpected rather than dropping it silently.
			p.write(line)
		}
		if err != nil {
			return fmt.Errorf("mcphost redir: replay initialize: %w", err)
		}
	}
	if p.initedNote {
		const note = `{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n"
		if _, err := p.conn.Write([]byte(note)); err != nil {
			return fmt.Errorf("mcphost redir: replay initialized: %w", err)
		}
	}
	return nil
}

// failPendingLocked answers every request that was in flight on the dead
// connection with a JSON-RPC error, so the agent is never left waiting.
func (p *proxy) failPendingLocked() {
	for id := range p.pending {
		resp, _ := json.Marshal(rpcMessage{
			JSONRPC: "2.0",
			ID:      json.RawMessage(id),
			Error: &rpcError{
				Code:    redirErrConnLost,
				Message: "mcphost: lost the connection to the tool host mid-request; please retry",
			},
		}) // id is JSON we already parsed
		p.write(append(resp, '\n'))
	}
	clear(p.pending)
}

// failLocked records the first fatal error and tears the proxy down.
func (p *proxy) failLocked(err error) {
	if p.err == nil {
		p.err = err
	}
	p.closing = true
	p.failPendingLocked()
	if p.conn != nil {
		_ = p.conn.Close()
		p.conn = nil
	}
}

// --- the two directions -------------------------------------------------

// fromAgent forwards the agent's lines to the host until stdin hits EOF.
func (p *proxy) fromAgent(in io.Reader) {
	br := bufio.NewReaderSize(in, 1<<20)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			if !p.forward(ensureNewline(line)) {
				return
			}
		}
		if err != nil {
			p.closeInput()
			return
		}
	}
}

// forward remembers whatever handshake state the line carries, then
// writes it to the host, reconnecting once if the connection has died.
// It reports false when the proxy must shut down.
func (p *proxy) forward(line []byte) bool {
	var msg rpcMessage
	parsed := json.Unmarshal(line, &msg) == nil

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closing {
		// The proxy has already given up or seen stdin EOF. Starting a
		// fresh backoff loop here would park this line for another 30s
		// behind a host that is not coming back.
		return false
	}
	if parsed {
		switch {
		case msg.Method == "initialize" && len(msg.ID) > 0:
			p.initParams = append(json.RawMessage(nil), msg.Params...)
			p.haveInit = true
		case msg.Method == "notifications/initialized":
			p.initedNote = true
		}
	}
	// Two attempts: the current connection, then a freshly dialled one.
	for attempt := 0; attempt < 2; attempt++ {
		if p.conn == nil {
			if err := p.connectLocked(); err != nil {
				p.failLocked(err)
				return false
			}
		}
		if _, err := p.conn.Write(line); err != nil {
			_ = p.conn.Close()
			p.conn = nil
			continue
		}
		// Record the in-flight id only once the bytes are away, and under
		// the same lock hold, so a concurrent reconnect can never fail a
		// request it did not actually lose.
		if parsed && len(msg.ID) > 0 && msg.Method != "" {
			p.pending[rawID(msg.ID)] = true
		}
		return true
	}
	p.failLocked(errors.New("mcphost redir: cannot deliver to the tool host"))
	return false
}

// fromHost forwards the host's lines to the agent, reconnecting whenever
// the connection drops, until the proxy is shutting down or the host
// stays unreachable.
func (p *proxy) fromHost() {
	defer close(p.done)
	for {
		p.mu.Lock()
		br, gen := p.br, p.gen
		p.mu.Unlock()

		if br != nil {
			for {
				line, err := br.ReadBytes('\n')
				if len(line) > 0 {
					p.deliver(ensureNewline(line))
				}
				if err != nil {
					break
				}
			}
		}

		p.mu.Lock()
		switch {
		case p.closing:
			p.mu.Unlock()
			return
		case p.gen != gen:
			// forward() already rebuilt the connection; pick it up.
		default:
			if err := p.connectLocked(); err != nil {
				p.failLocked(err)
				p.mu.Unlock()
				return
			}
		}
		p.mu.Unlock()
	}
}

// deliver passes a line from the host to the agent.
//
// A response is forwarded only if we are still waiting for it. The window
// that makes this necessary: the host answers a request and then dies, so
// the real response is sitting in our buffer while the write side has
// already noticed the drop and failed that id back to the agent. Without
// this check the agent would see both an error and a result for one id.
// Anything carrying a method is host-originated traffic, not an answer to
// us, and is always forwarded.
func (p *proxy) deliver(line []byte) {
	var msg rpcMessage
	if json.Unmarshal(line, &msg) == nil && len(msg.ID) > 0 && msg.Method == "" {
		p.mu.Lock()
		expected := p.pending[rawID(msg.ID)]
		delete(p.pending, rawID(msg.ID))
		p.mu.Unlock()
		if !expected {
			return
		}
	}
	p.write(line)
}

// closeInput handles the agent closing stdin: half-close towards the host
// so its MCP loop terminates, and stop reconnecting.
func (p *proxy) closeInput() {
	p.mu.Lock()
	p.closing = true
	if p.conn != nil {
		halfCloseWrite(p.conn)
	}
	p.mu.Unlock()
}

func (p *proxy) write(b []byte) {
	p.outMu.Lock()
	_, _ = p.out.Write(b)
	p.outMu.Unlock()
}

// --- small helpers ------------------------------------------------------

// rawID normalises a JSON-RPC id to a comparable string. Ids are echoed
// verbatim by the host, so byte equality modulo surrounding space is the
// right identity here.
func rawID(id json.RawMessage) string { return string(bytes.TrimSpace(id)) }

// ensureNewline guarantees the line-delimited framing survives a final
// unterminated line.
func ensureNewline(line []byte) []byte {
	if len(line) > 0 && line[len(line)-1] == '\n' {
		return line
	}
	return append(line, '\n')
}

// halfCloseWrite closes only the write half of conn if supported (real
// unix sockets do; net.Pipe does not), so the server sees EOF on stdin
// while we keep reading its response.
func halfCloseWrite(conn net.Conn) {
	if cw, ok := conn.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
}
