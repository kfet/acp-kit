package state

import (
	"context"
	"errors"
	"testing"

	acp "github.com/coder/acp-go-sdk"
	"github.com/kfet/acp-kit/client"
)

// spAgent is a fakeAgent that also re-sends the system prompt on resume.
type spAgent struct {
	fakeAgent
	spResumes   int
	plainResume int
	spBlocks    []acp.ContentBlock
	spErr       error
}

func (f *spAgent) ResumeSessionWithSystemPrompt(_ context.Context, _ string, _ acp.SessionId, _ client.SessionUpdateSink, blocks []acp.ContentBlock) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.spResumes++
	f.spBlocks = blocks
	return f.spErr
}

func (f *spAgent) ResumeSession(ctx context.Context, cwd string, sid acp.SessionId, sink client.SessionUpdateSink) error {
	f.mu.Lock()
	f.plainResume++
	f.mu.Unlock()
	return f.fakeAgent.ResumeSession(ctx, cwd, sid, sink)
}

var _ SystemPromptResumer = (*client.AgentProc)(nil)

func resumable(via client.SystemPromptVia) client.Caps {
	return client.Caps{ListSessions: true, ResumeSession: true, SystemPrompt: via != client.SystemPromptNone, SystemPromptVia: via}
}

// Stable cap: resume re-sends the prompt and needs no inline re-arming.
func TestResumeStableResendsSystemPrompt(t *testing.T) {
	ag := &spAgent{fakeAgent: fakeAgent{caps: resumable(client.SystemPromptStable), listResp: []client.SessionInfo{{SessionId: "old"}}}}
	m := newManagerT(t, Config{Agent: ag, SystemPrompt: "sys"})
	s, err := m.GetOrCreate(context.Background(), "k", stubSink{})
	if err != nil {
		t.Fatal(err)
	}
	if s.SessionID != "old" || ag.spResumes != 1 || ag.plainResume != 0 {
		t.Fatalf("sid=%s sp=%d plain=%d", s.SessionID, ag.spResumes, ag.plainResume)
	}
	if len(ag.spBlocks) != 1 || ag.spBlocks[0].Text == nil || ag.spBlocks[0].Text.Text != "sys" {
		t.Fatalf("blocks %+v", ag.spBlocks)
	}
	if got := m.TakePendingSystemPrompt(s); got != "" {
		t.Fatalf("pending %q", got)
	}
}

// Legacy cap: blocks are handed over too (the client decides the wire
// form); still no inline re-arming.
func TestResumeLegacyNoRearm(t *testing.T) {
	ag := &spAgent{fakeAgent: fakeAgent{caps: resumable(client.SystemPromptLegacyMeta), listResp: []client.SessionInfo{{SessionId: "old"}}}}
	m := newManagerT(t, Config{Agent: ag, SystemPrompt: "sys"})
	s, err := m.GetOrCreate(context.Background(), "k", stubSink{})
	if err != nil {
		t.Fatal(err)
	}
	if ag.spResumes != 1 || m.TakePendingSystemPrompt(s) != "" {
		t.Fatalf("sp=%d", ag.spResumes)
	}
}

// No prompt configured: the plain resume is used even by a resumer.
func TestResumeNoPromptUsesPlainResume(t *testing.T) {
	ag := &spAgent{fakeAgent: fakeAgent{caps: resumable(client.SystemPromptStable), listResp: []client.SessionInfo{{SessionId: "old"}}}}
	m := newManagerT(t, Config{Agent: ag})
	if _, err := m.GetOrCreate(context.Background(), "k", stubSink{}); err != nil {
		t.Fatal(err)
	}
	if ag.spResumes != 0 || ag.plainResume != 1 {
		t.Fatalf("sp=%d plain=%d", ag.spResumes, ag.plainResume)
	}
}

// Neither cap: the resumer is not asked to carry blocks, and the prompt
// is re-armed inline.
func TestResumeNeitherCapRearmsInline(t *testing.T) {
	ag := &spAgent{fakeAgent: fakeAgent{caps: resumable(client.SystemPromptNone), listResp: []client.SessionInfo{{SessionId: "old"}}}}
	m := newManagerT(t, Config{Agent: ag, SystemPrompt: "sys"})
	s, err := m.GetOrCreate(context.Background(), "k", stubSink{})
	if err != nil {
		t.Fatal(err)
	}
	if ag.spResumes != 0 || ag.plainResume != 1 {
		t.Fatalf("sp=%d plain=%d", ag.spResumes, ag.plainResume)
	}
	if got := m.TakePendingSystemPrompt(s); got != "sys" {
		t.Fatalf("pending %q", got)
	}
}

// A failed resume falls back to a new session carrying the blocks.
func TestResumeWithSystemPromptFailsFallsBackToNew(t *testing.T) {
	ag := &spAgent{fakeAgent: fakeAgent{caps: resumable(client.SystemPromptStable), listResp: []client.SessionInfo{{SessionId: "old"}}}, spErr: errors.New("nope")}
	m := newManagerT(t, Config{Agent: ag, SystemPrompt: "sys"})
	s, err := m.GetOrCreate(context.Background(), "k", stubSink{})
	if err != nil {
		t.Fatal(err)
	}
	if s.SessionID != "sess-1" || len(ag.newBlocks) != 1 || m.TakePendingSystemPrompt(s) != "" {
		t.Fatalf("sid=%s blocks=%v", s.SessionID, ag.newBlocks)
	}
}
