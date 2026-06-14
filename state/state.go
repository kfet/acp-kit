// Package state maps relay conversation keys to ACP sessions and stable cwd dirs.
package state

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/kfet/acp-kit/client"
	kitlog "github.com/kfet/acp-kit/log"
)

// Agent is the subset of *client.AgentProc needed by Manager.
type Agent interface {
	Caps() client.Caps
	NewSession(ctx context.Context, cwd string, sink client.SessionUpdateSink, systemPromptBlocks []acp.ContentBlock) (acp.SessionId, error)
	ListSessions(ctx context.Context, cwd string) ([]client.SessionInfo, error)
	ResumeSession(ctx context.Context, cwd string, sid acp.SessionId, sink client.SessionUpdateSink) error
	Cancel(ctx context.Context, sid acp.SessionId) error
	DropSession(sid acp.SessionId)
	RebindSink(sid acp.SessionId, sink client.SessionUpdateSink)
}

// CwdFor returns and creates the stable cwd for key. A custom CwdFor is
// responsible for its own path validation and mkdirs. The default stores each
// key as one safe path component under <StateDir>/convs/<key>.
type CwdFor func(stateDir, key string) (string, error)

// Config configures a Manager.
type Config struct {
	Agent       Agent
	StateDir    string
	IdleTimeout time.Duration // 0 => 30 minutes

	// CwdFor optionally overrides cwd layout. If nil, keys must be single safe
	// path components and are stored under <StateDir>/convs/<key>.
	CwdFor CwdFor

	// SystemPrompt is durable per-session instruction text. If Provider is set,
	// it is called for each new/resumed session and overrides SystemPrompt.
	SystemPrompt         string
	SystemPromptProvider func() string

	// SystemPromptForKey, if set, resolves the durable system prompt per
	// conversation key. It takes precedence over SystemPromptProvider and
	// SystemPrompt and is evaluated at session creation/resume. Use it for
	// per-conversation persona (e.g. a different prompt per Slack channel).
	SystemPromptForKey func(key string) string
}

// Session holds manager-side state for one ACP session.
type Session struct {
	Key       string
	SessionID acp.SessionId
	Cwd       string

	// Mu serializes prompt submission for this session. ACP allows one
	// outstanding prompt per session at a time.
	Mu sync.Mutex

	lastUsed                  time.Time
	systemPrompt              string
	pendingSystemPromptInline bool
}

// Manager owns the key -> session map, stable cwd allocation, best-effort
// resume, and idle GC. GC drops in-memory sessions but never deletes cwd dirs.
type Manager struct {
	cfg Config

	root *os.Root

	mu    sync.Mutex
	byKey map[string]*Session
}

// New constructs a Manager. Call Run(ctx) to start idle GC.
func New(cfg Config) (*Manager, error) {
	if cfg.Agent == nil {
		return nil, fmt.Errorf("state: nil agent")
	}
	if cfg.StateDir == "" {
		return nil, fmt.Errorf("state: empty StateDir")
	}
	if cfg.IdleTimeout == 0 {
		cfg.IdleTimeout = 30 * time.Minute
	}
	if err := os.MkdirAll(cfg.StateDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir state dir: %w", err)
	}
	root, err := os.OpenRoot(cfg.StateDir)
	if err != nil {
		return nil, fmt.Errorf("open state dir as root: %w", err)
	}
	return &Manager{cfg: cfg, root: root, byKey: make(map[string]*Session)}, nil
}

// Close releases the os.Root handle used by the default cwd allocator.
func (m *Manager) Close() error {
	m.mu.Lock()
	root := m.root
	m.root = nil
	m.mu.Unlock()
	if root == nil {
		return nil
	}
	return root.Close()
}

// StateDir returns the configured state root.
func (m *Manager) StateDir() string { return m.cfg.StateDir }

// Agent returns the underlying ACP agent.
func (m *Manager) Agent() Agent { return m.cfg.Agent }

// GetOrCreate returns an existing session for key or creates/resumes one.
func (m *Manager) GetOrCreate(ctx context.Context, key string, sink client.SessionUpdateSink) (*Session, error) {
	m.mu.Lock()
	if s, ok := m.byKey[key]; ok {
		s.lastUsed = time.Now()
		m.mu.Unlock()
		m.cfg.Agent.RebindSink(s.SessionID, sink)
		return s, nil
	}
	m.mu.Unlock()

	cwd, err := m.cwdFor(key)
	if err != nil {
		return nil, err
	}

	sys := m.systemPrompt(key)
	sid, resumed := m.tryResume(ctx, cwd, sink)
	caps := m.cfg.Agent.Caps()
	pendingInline := false
	if !resumed {
		var sysBlocks []acp.ContentBlock
		if sys != "" {
			if caps.SystemPrompt {
				sysBlocks = []acp.ContentBlock{acp.TextBlock(sys)}
			} else {
				pendingInline = true
			}
		}
		var nerr error
		sid, nerr = m.cfg.Agent.NewSession(ctx, cwd, sink, sysBlocks)
		if nerr != nil {
			return nil, fmt.Errorf("new acp session: %w", nerr)
		}
	} else if sys != "" && !caps.SystemPrompt {
		// Only non-cap agents need re-arming on resume. Cap-path agents are
		// trusted to restore the durable system prompt themselves.
		pendingInline = true
	}

	s := &Session{Key: key, SessionID: sid, Cwd: cwd, lastUsed: time.Now(), systemPrompt: sys, pendingSystemPromptInline: pendingInline}
	m.mu.Lock()
	defer m.mu.Unlock()
	if other, ok := m.byKey[key]; ok {
		m.cfg.Agent.DropSession(sid)
		other.lastUsed = time.Now()
		m.cfg.Agent.RebindSink(other.SessionID, sink)
		return other, nil
	}
	m.byKey[key] = s
	if resumed {
		kitlog.Debugf("state: resumed session %s for %s in %s", sid, key, cwd)
	} else {
		kitlog.Debugf("state: new session %s for %s in %s", sid, key, cwd)
	}
	return s, nil
}

// Touch marks a session as recently used.
func (m *Manager) Touch(s *Session) {
	m.mu.Lock()
	s.lastUsed = time.Now()
	m.mu.Unlock()
}

// Cancel sends session/cancel if key has a live session.
func (m *Manager) Cancel(ctx context.Context, key string) {
	m.mu.Lock()
	s, ok := m.byKey[key]
	m.mu.Unlock()
	if !ok {
		return
	}
	_ = m.cfg.Agent.Cancel(ctx, s.SessionID)
}

// Run drives idle GC until ctx is cancelled.
func (m *Manager) Run(ctx context.Context) {
	period := m.cfg.IdleTimeout / 4
	if period <= 0 {
		period = time.Minute
	}
	t := time.NewTicker(period)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.GCOnce()
		}
	}
}

// GCOnce drops stale in-memory sessions and leaves their cwd dirs intact.
func (m *Manager) GCOnce() {
	cutoff := time.Now().Add(-m.cfg.IdleTimeout)
	m.mu.Lock()
	var stale []*Session
	for key, s := range m.byKey {
		if s.lastUsed.Before(cutoff) {
			stale = append(stale, s)
			delete(m.byKey, key)
		}
	}
	m.mu.Unlock()
	for _, s := range stale {
		kitlog.Debugf("state: GC session %s (%s); cwd %s retained", s.SessionID, s.Key, s.Cwd)
		m.cfg.Agent.DropSession(s.SessionID)
	}
}

// Len returns the number of live sessions.
func (m *Manager) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.byKey)
}

// checkpointFile is the per-conversation checkpoint filename stored under the
// conversation's cwd.
const checkpointFile = ".checkpoint"

// Known reports whether a session for key already exists — either live in
// memory or as a persisted cwd on disk. Disk is the source of truth across
// restarts: the in-memory map is empty after a restart, but cwd dirs survive,
// so a relay that only follows conversations it has been engaged in can use
// Known as a restart-stable membership check.
//
// The disk check uses the default cwd layout (<StateDir>/convs/<key>). When a
// custom CwdFor is configured, Known consults only the in-memory map, since the
// custom layout's existence semantics are the caller's to define.
func (m *Manager) Known(key string) bool {
	m.mu.Lock()
	_, ok := m.byKey[key]
	root := m.root
	m.mu.Unlock()
	if ok {
		return true
	}
	if m.cfg.CwdFor != nil || root == nil {
		return false
	}
	if validateKeyComponent(key) != nil {
		return false
	}
	fi, err := root.Stat(filepath.Join("convs", key))
	return err == nil && fi.IsDir()
}

// Checkpoint returns the opaque checkpoint string persisted for key, or "" if
// none has been set. The Manager stores and returns it verbatim; the relay owns
// its meaning (e.g. a last-processed external event id). Allocates the cwd if
// absent.
func (m *Manager) Checkpoint(key string) (string, error) {
	cwd, err := m.cwdFor(key)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(filepath.Join(cwd, checkpointFile))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read checkpoint: %w", err)
	}
	return strings.TrimSpace(string(b)), nil
}

// SetCheckpoint persists value as the opaque checkpoint for key under its cwd,
// atomically (write-temp + rename). Allocates the cwd if absent.
func (m *Manager) SetCheckpoint(key, value string) error {
	cwd, err := m.cwdFor(key)
	if err != nil {
		return err
	}
	tmp := filepath.Join(cwd, checkpointFile+".tmp")
	if err := os.WriteFile(tmp, []byte(value), 0o644); err != nil {
		return fmt.Errorf("write checkpoint: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(cwd, checkpointFile)); err != nil {
		return fmt.Errorf("commit checkpoint: %w", err)
	}
	return nil
}

// TakePendingSystemPrompt returns inline fallback text for the next user prompt
// and clears the pending flag. Call with s.Mu held.
func (m *Manager) TakePendingSystemPrompt(s *Session) string {
	if !s.pendingSystemPromptInline {
		return ""
	}
	s.pendingSystemPromptInline = false
	return s.systemPrompt
}

func (m *Manager) systemPrompt(key string) string {
	if m.cfg.SystemPromptForKey != nil {
		return strings.TrimSpace(m.cfg.SystemPromptForKey(key))
	}
	if m.cfg.SystemPromptProvider != nil {
		return strings.TrimSpace(m.cfg.SystemPromptProvider())
	}
	return strings.TrimSpace(m.cfg.SystemPrompt)
}

func (m *Manager) cwdFor(key string) (string, error) {
	if m.cfg.CwdFor != nil {
		return m.cfg.CwdFor(m.cfg.StateDir, key)
	}
	if err := validateKeyComponent(key); err != nil {
		return "", fmt.Errorf("key %q: %w", key, err)
	}
	if m.root == nil {
		return "", fmt.Errorf("state: closed")
	}
	rel := filepath.Join("convs", key)
	// key is a single validated path component, so this cannot escape StateDir;
	// the os.Root handle provides defense-in-depth against future bugs.
	if err := m.root.MkdirAll(rel, 0o755); err != nil {
		return "", fmt.Errorf("mkdir cwd: %w", err)
	}
	return filepath.Join(m.cfg.StateDir, rel), nil
}

func validateKeyComponent(s string) error {
	if s == "" {
		return fmt.Errorf("empty")
	}
	if s == "." || s == ".." {
		return fmt.Errorf("reserved name")
	}
	if strings.ContainsAny(s, `/\\`) || strings.ContainsRune(s, 0) {
		return fmt.Errorf("contains path separator or null byte")
	}
	if s[0] == '.' {
		return fmt.Errorf("leading dot")
	}
	return nil
}

func (m *Manager) tryResume(ctx context.Context, cwd string, sink client.SessionUpdateSink) (acp.SessionId, bool) {
	caps := m.cfg.Agent.Caps()
	if !caps.ListSessions || !caps.ResumeSession {
		return "", false
	}
	sessions, err := m.cfg.Agent.ListSessions(ctx, cwd)
	if err != nil {
		kitlog.Debugf("state: list sessions for %s: %v", cwd, err)
		return "", false
	}
	if len(sessions) == 0 {
		return "", false
	}
	sid := acp.SessionId(sessions[0].SessionId)
	if err := m.cfg.Agent.ResumeSession(ctx, cwd, sid, sink); err != nil {
		kitlog.Debugf("state: resume %s in %s: %v", sid, cwd, err)
		return "", false
	}
	return sid, true
}
