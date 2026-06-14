package state

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kfet/acp-kit/client"
)

func TestKnownMemoryHit(t *testing.T) {
	ag := &fakeAgent{}
	m := newManagerT(t, Config{Agent: ag})
	if m.Known("conv1") {
		t.Fatal("unknown before create")
	}
	if _, err := m.GetOrCreate(context.Background(), "conv1", stubSink{}); err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	if !m.Known("conv1") {
		t.Fatal("known after create (memory)")
	}
}

func TestKnownDiskHitAfterGC(t *testing.T) {
	ag := &fakeAgent{}
	m := newManagerT(t, Config{Agent: ag})
	if _, err := m.GetOrCreate(context.Background(), "conv1", stubSink{}); err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	// Drop the in-memory entry (simulating a restart / idle GC) but leave the
	// cwd dir on disk. Known must still report true from disk.
	m.mu.Lock()
	delete(m.byKey, "conv1")
	m.mu.Unlock()
	if !m.Known("conv1") {
		t.Fatal("known from disk after in-memory drop")
	}
}

func TestKnownDiskMissForUnseenKey(t *testing.T) {
	m := newManagerT(t, Config{Agent: &fakeAgent{}})
	if m.Known("never-seen") {
		t.Fatal("unseen key must be unknown")
	}
}

func TestKnownInvalidKey(t *testing.T) {
	m := newManagerT(t, Config{Agent: &fakeAgent{}})
	if m.Known("../escape") {
		t.Fatal("invalid key must be unknown")
	}
}

func TestKnownCustomCwdForIsMemoryOnly(t *testing.T) {
	dir := t.TempDir()
	ag := &fakeAgent{}
	m := newManagerT(t, Config{
		Agent:    ag,
		StateDir: dir,
		CwdFor: func(stateDir, key string) (string, error) {
			p := filepath.Join(stateDir, "custom", key)
			return p, os.MkdirAll(p, 0o755)
		},
	})
	if _, err := m.GetOrCreate(context.Background(), "conv1", stubSink{}); err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	// Still in memory → known.
	if !m.Known("conv1") {
		t.Fatal("known while in memory")
	}
	m.mu.Lock()
	delete(m.byKey, "conv1")
	m.mu.Unlock()
	// Custom CwdFor → disk is not consulted, so it's now unknown.
	if m.Known("conv1") {
		t.Fatal("custom CwdFor must be memory-only")
	}
}

func TestKnownAfterClose(t *testing.T) {
	m := newManagerT(t, Config{Agent: &fakeAgent{}})
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if m.Known("conv1") {
		t.Fatal("closed manager reports unknown")
	}
}

func TestCheckpointRoundTrip(t *testing.T) {
	m := newManagerT(t, Config{Agent: &fakeAgent{}})
	got, err := m.Checkpoint("conv1")
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if got != "" {
		t.Fatalf("empty checkpoint = %q", got)
	}
	if err := m.SetCheckpoint("conv1", "1700000000.0001"); err != nil {
		t.Fatalf("SetCheckpoint: %v", err)
	}
	got, err = m.Checkpoint("conv1")
	if err != nil {
		t.Fatalf("Checkpoint after set: %v", err)
	}
	if got != "1700000000.0001" {
		t.Fatalf("checkpoint = %q", got)
	}
	// Overwrite.
	if err := m.SetCheckpoint("conv1", "1700000001.0002"); err != nil {
		t.Fatalf("SetCheckpoint overwrite: %v", err)
	}
	got, _ = m.Checkpoint("conv1")
	if got != "1700000001.0002" {
		t.Fatalf("overwritten checkpoint = %q", got)
	}
}

func TestCheckpointInvalidKey(t *testing.T) {
	m := newManagerT(t, Config{Agent: &fakeAgent{}})
	if _, err := m.Checkpoint("../bad"); err == nil {
		t.Fatal("expected error for invalid key")
	}
	if err := m.SetCheckpoint("../bad", "x"); err == nil {
		t.Fatal("expected error for invalid key on set")
	}
}

func TestCheckpointReadError(t *testing.T) {
	m := newManagerT(t, Config{Agent: &fakeAgent{}})
	// Make the checkpoint path a directory so ReadFile fails with a non
	// not-exist error.
	cwd := filepath.Join(m.StateDir(), "convs", "conv1")
	if err := os.MkdirAll(filepath.Join(cwd, checkpointFile), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if _, err := m.Checkpoint("conv1"); err == nil {
		t.Fatal("expected read error when checkpoint is a directory")
	}
}

func TestSetCheckpointWriteError(t *testing.T) {
	m := newManagerT(t, Config{Agent: &fakeAgent{}})
	// Pre-create the .tmp path as a directory so WriteFile fails.
	cwd := filepath.Join(m.StateDir(), "convs", "conv1")
	if err := os.MkdirAll(filepath.Join(cwd, checkpointFile+".tmp"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := m.SetCheckpoint("conv1", "x"); err == nil {
		t.Fatal("expected write error")
	}
}

func TestSetCheckpointCommitError(t *testing.T) {
	m := newManagerT(t, Config{Agent: &fakeAgent{}})
	// Make the final checkpoint path a non-empty directory so rename-over
	// fails on commit while the temp write succeeds.
	cwd := filepath.Join(m.StateDir(), "convs", "conv1")
	target := filepath.Join(cwd, checkpointFile)
	if err := os.MkdirAll(filepath.Join(target, "child"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := m.SetCheckpoint("conv1", "x"); err == nil {
		t.Fatal("expected commit error")
	}
}

func TestSystemPromptForKeyResolver(t *testing.T) {
	ag := &fakeAgent{caps: client.Caps{SystemPrompt: true}}
	m := newManagerT(t, Config{
		Agent: ag,
		// Provider would be used if ForKey were absent; ForKey must win.
		SystemPromptProvider: func() string { return "provider" },
		SystemPromptForKey: func(key string) string {
			if key == "vip" {
				return "VIP persona"
			}
			return "default persona"
		},
	})
	if _, err := m.GetOrCreate(context.Background(), "vip", stubSink{}); err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	if len(ag.newBlocks) != 1 || ag.newBlocks[0].Text == nil || ag.newBlocks[0].Text.Text != "VIP persona" {
		t.Fatalf("newBlocks = %v", ag.newBlocks)
	}
}
