// Package terminal drives ACP client-side terminals for command execution.
//
// When an ACP client advertises the terminal capability, an agent can delegate
// shell command execution to the client so the client renders the live
// terminal in its UI. This package implements that delegation: foreground
// execution with optional timeout, a bounded pool of named background
// commands, and cleanup of leaked terminals when a turn is cancelled.
//
// All operations go through the small [Conn] interface — the subset of the ACP
// agent-side connection the terminal protocol needs — so callers can supply a
// real *acp.AgentSideConnection or a fake in tests. A [State] value tracks the
// terminals owned by one session and is safe for concurrent use.
package terminal

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	acp "github.com/coder/acp-go-sdk"
)

// DefaultMaxBytes is the output byte limit requested for each ACP terminal.
const DefaultMaxBytes = 50 * 1024

// DefaultMaxBackground is the default maximum number of concurrent background
// terminals a single [State] permits.
const DefaultMaxBackground = 10

// ErrAborted is returned by [State.Exec] when the supplied context is
// cancelled before the command completes.
var ErrAborted = errors.New("aborted")

// TimeoutError is returned by [State.Exec] when a foreground command exceeds
// its timeout. The killed terminal's partial output is discarded.
type TimeoutError struct {
	// Seconds is the timeout that elapsed.
	Seconds int
}

func (e *TimeoutError) Error() string {
	return fmt.Sprintf("command timed out after %ds", e.Seconds)
}

// Conn is the subset of the ACP agent-side connection the terminal protocol
// uses. *acp.AgentSideConnection satisfies it.
type Conn interface {
	SessionUpdate(ctx context.Context, params acp.SessionNotification) error
	CreateTerminal(ctx context.Context, params acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error)
	KillTerminal(ctx context.Context, params acp.KillTerminalRequest) (acp.KillTerminalResponse, error)
	ReleaseTerminal(ctx context.Context, params acp.ReleaseTerminalRequest) (acp.ReleaseTerminalResponse, error)
	TerminalOutput(ctx context.Context, params acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error)
	WaitForTerminalExit(ctx context.Context, params acp.WaitForTerminalExitRequest) (acp.WaitForTerminalExitResponse, error)
}

// ExecResult is the outcome of a foreground command run via [State.Exec].
type ExecResult struct {
	// ExitCode is the command's exit code, or nil if the client did not
	// report one.
	ExitCode *int
	// Output is the captured terminal output.
	Output string
}

// State tracks the ACP terminals owned by a single session. The zero value is
// not usable; construct one with [NewState]. State is safe for concurrent use.
type State struct {
	// maxBackground caps concurrent background terminals.
	maxBackground int

	mu sync.Mutex
	// pendingBash maps a toolCallID to the terminalId of an in-flight
	// foreground command.
	pendingBash map[string]string
	// background maps a commandID to its terminalId.
	background map[string]string
}

// NewState returns a State that permits up to [DefaultMaxBackground]
// concurrent background terminals.
func NewState() *State {
	return NewStateWithLimit(DefaultMaxBackground)
}

// NewStateWithLimit returns a State that permits up to limit concurrent
// background terminals. A limit <= 0 falls back to [DefaultMaxBackground].
func NewStateWithLimit(limit int) *State {
	if limit <= 0 {
		limit = DefaultMaxBackground
	}
	return &State{
		maxBackground: limit,
		pendingBash:   make(map[string]string),
		background:    make(map[string]string),
	}
}

// TakePending reports whether toolCallID has a still-pending foreground or
// background terminal and, if so, removes the entry. Agents use this on a
// tool-end event to decide whether the tool's output was already rendered by
// the client's terminal (so the agent can skip emitting duplicate text).
func (s *State) TakePending(toolCallID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.pendingBash[toolCallID]; ok {
		delete(s.pendingBash, toolCallID)
		return true
	}
	return false
}

// embedInToolCall sends a tool_call update that embeds a terminal in the tool
// call UI. Delivery failures are non-fatal and ignored.
func embedInToolCall(conn Conn, sessionID, toolCallID, terminalID string) {
	_ = conn.SessionUpdate(context.Background(), acp.SessionNotification{
		SessionId: acp.SessionId(sessionID),
		Update: acp.UpdateToolCall(
			acp.ToolCallId(toolCallID),
			acp.WithUpdateContent([]acp.ToolCallContent{acp.ToolTerminalRef(terminalID)}),
		),
	})
}

// Exec runs command via the client's terminal in the foreground, embeds the
// terminal in the tool call identified by toolCallID, and waits for it to
// exit. A timeout > 0 kills the command after that many seconds and returns a
// *[TimeoutError]. If ctx is cancelled, Exec returns [ErrAborted].
func (s *State) Exec(ctx context.Context, conn Conn, sessionID, toolCallID, command, cwd string, timeout int) (*ExecResult, error) {
	outputByteLimit := DefaultMaxBytes
	terminal, err := conn.CreateTerminal(ctx, acp.CreateTerminalRequest{
		SessionId:       acp.SessionId(sessionID),
		Command:         command,
		Cwd:             &cwd,
		OutputByteLimit: &outputByteLimit,
	})
	if err != nil {
		return nil, fmt.Errorf("create terminal: %w", err)
	}
	termID := terminal.TerminalId

	s.mu.Lock()
	s.pendingBash[toolCallID] = termID
	s.mu.Unlock()
	embedInToolCall(conn, sessionID, toolCallID, termID)

	sid := acp.SessionId(sessionID)

	if ctx.Err() != nil {
		s.mu.Lock()
		delete(s.pendingBash, toolCallID)
		s.mu.Unlock()
		conn.KillTerminal(context.Background(), acp.KillTerminalRequest{SessionId: sid, TerminalId: termID})
		conn.ReleaseTerminal(context.Background(), acp.ReleaseTerminalRequest{SessionId: sid, TerminalId: termID})
		return nil, ErrAborted
	}

	execCtx := ctx
	var cancel context.CancelFunc
	timedOut := false
	if timeout > 0 {
		execCtx, cancel = context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
		defer cancel()
	}

	exitResult, waitErr := conn.WaitForTerminalExit(execCtx, acp.WaitForTerminalExitRequest{SessionId: sid, TerminalId: termID})
	if waitErr != nil && execCtx.Err() != nil && timeout > 0 {
		timedOut = true
		conn.KillTerminal(context.Background(), acp.KillTerminalRequest{SessionId: sid, TerminalId: termID})
	} else if waitErr != nil {
		s.mu.Lock()
		delete(s.pendingBash, toolCallID)
		s.mu.Unlock()
		conn.ReleaseTerminal(context.Background(), acp.ReleaseTerminalRequest{SessionId: sid, TerminalId: termID})
		return nil, waitErr
	}

	output, _ := conn.TerminalOutput(context.Background(), acp.TerminalOutputRequest{SessionId: sid, TerminalId: termID})
	conn.ReleaseTerminal(context.Background(), acp.ReleaseTerminalRequest{SessionId: sid, TerminalId: termID})
	s.mu.Lock()
	delete(s.pendingBash, toolCallID)
	s.mu.Unlock()

	if ctx.Err() != nil {
		return nil, ErrAborted
	}
	if timedOut {
		return nil, &TimeoutError{Seconds: timeout}
	}

	return &ExecResult{ExitCode: exitResult.ExitCode, Output: output.Output}, nil
}

// StartBackground starts command as a background terminal, embeds it in the
// tool call identified by toolCallID, and returns its command ID. It fails
// once the session already has the maximum number of background terminals.
func (s *State) StartBackground(ctx context.Context, conn Conn, sessionID, command, cwd, toolCallID string) (string, error) {
	s.mu.Lock()
	atLimit := len(s.background) >= s.maxBackground
	s.mu.Unlock()
	if atLimit {
		return "", fmt.Errorf("maximum of %d background commands reached. Kill an existing one with bash_kill first", s.maxBackground)
	}

	outputByteLimit := DefaultMaxBytes
	terminal, err := conn.CreateTerminal(ctx, acp.CreateTerminalRequest{
		SessionId:       acp.SessionId(sessionID),
		Command:         command,
		Cwd:             &cwd,
		OutputByteLimit: &outputByteLimit,
	})
	if err != nil {
		return "", fmt.Errorf("create terminal: %w", err)
	}

	cmdID := terminal.TerminalId
	s.mu.Lock()
	s.background[cmdID] = cmdID
	s.pendingBash[toolCallID] = cmdID
	s.mu.Unlock()
	embedInToolCall(conn, sessionID, toolCallID, cmdID)
	return cmdID, nil
}

// BackgroundOutput returns the current output and status of the background
// command identified by commandID.
func (s *State) BackgroundOutput(ctx context.Context, conn Conn, sessionID, commandID string) (output string, isRunning bool, exitCode *int, err error) {
	s.mu.Lock()
	termID, ok := s.background[commandID]
	s.mu.Unlock()
	if !ok {
		return "", false, nil, fmt.Errorf("no background command found with ID: %s", commandID)
	}

	result, err := conn.TerminalOutput(ctx, acp.TerminalOutputRequest{
		SessionId: acp.SessionId(sessionID), TerminalId: termID,
	})
	if err != nil {
		return "", false, nil, err
	}
	isRunning = result.ExitStatus == nil
	if result.ExitStatus != nil {
		exitCode = result.ExitStatus.ExitCode
	}
	return result.Output, isRunning, exitCode, nil
}

// KillBackground kills the background command identified by commandID and
// returns its final output.
func (s *State) KillBackground(ctx context.Context, conn Conn, sessionID, commandID string) (output string, exitCode *int, err error) {
	s.mu.Lock()
	termID, ok := s.background[commandID]
	s.mu.Unlock()
	if !ok {
		return "", nil, fmt.Errorf("no background command found with ID: %s", commandID)
	}

	sid := acp.SessionId(sessionID)
	conn.KillTerminal(ctx, acp.KillTerminalRequest{SessionId: sid, TerminalId: termID})
	result, _ := conn.TerminalOutput(ctx, acp.TerminalOutputRequest{SessionId: sid, TerminalId: termID})
	conn.ReleaseTerminal(ctx, acp.ReleaseTerminalRequest{SessionId: sid, TerminalId: termID})

	s.mu.Lock()
	delete(s.background, commandID)
	s.mu.Unlock()

	if result.ExitStatus != nil {
		exitCode = result.ExitStatus.ExitCode
	}
	return result.Output, exitCode, nil
}

// CleanupPending kills and releases all foreground terminals still pending
// (not yet acknowledged by a tool-end event). This handles a session
// cancelled before the foreground command finished.
func (s *State) CleanupPending(ctx context.Context, conn Conn, sessionID string) {
	s.mu.Lock()
	pending := make(map[string]string, len(s.pendingBash))
	for k, v := range s.pendingBash {
		pending[k] = v
	}
	s.pendingBash = make(map[string]string)
	s.mu.Unlock()

	sid := acp.SessionId(sessionID)
	for _, termID := range pending {
		conn.KillTerminal(ctx, acp.KillTerminalRequest{SessionId: sid, TerminalId: termID})
		conn.ReleaseTerminal(ctx, acp.ReleaseTerminalRequest{SessionId: sid, TerminalId: termID})
	}
}

// CleanupBackground kills and releases all background terminals.
func (s *State) CleanupBackground(ctx context.Context, conn Conn, sessionID string) {
	s.mu.Lock()
	terminals := make(map[string]string, len(s.background))
	for k, v := range s.background {
		terminals[k] = v
	}
	s.background = make(map[string]string)
	s.mu.Unlock()

	sid := acp.SessionId(sessionID)
	for _, termID := range terminals {
		conn.KillTerminal(ctx, acp.KillTerminalRequest{SessionId: sid, TerminalId: termID})
		conn.ReleaseTerminal(ctx, acp.ReleaseTerminalRequest{SessionId: sid, TerminalId: termID})
	}
}
