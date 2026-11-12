//go:build e2e

package interject_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/kfet/acp-kit/client"
	"github.com/kfet/acp-kit/interject"
)

// mockLLM is an OpenAI-compatible chat server. The main turn calls
// bash once (a command that blocks until release is closed), then
// answers. A fork prompt gets the scripted side reply.
type mockLLM struct {
	side    string
	release chan struct{}
	toolRun chan struct{} // closed when the main turn asked for the tool

	mu       sync.Mutex
	requests []string // every request body
	once     sync.Once
}

func (m *mockLLM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	m.mu.Lock()
	m.requests = append(m.requests, string(body))
	m.mu.Unlock()
	var req struct {
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(body, &req)
	last := req.Messages[len(req.Messages)-1].Role
	switch {
	case strings.Contains(string(body), "Name this session"):
		sseText(w, "e2e-session")
	case strings.Contains(string(body), "[side question"):
		sseText(w, m.side)
	case last == "tool" || last == "user" && strings.Contains(string(body), "tool_call_id"):
		sseText(w, "MAIN_DONE")
	default:
		m.once.Do(func() { close(m.toolRun) })
		sseTool(w, map[string]any{"command": "while [ ! -e " + releaseFile + " ]; do sleep 0.05; done; echo TOOL_OK"})
	}
}

var releaseFile string

func sseChunk(w http.ResponseWriter, delta map[string]any, finish any) {
	b, _ := json.Marshal(map[string]any{"id": "x", "object": "chat.completion.chunk", "model": "mock",
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}})
	fmt.Fprintf(w, "data: %s\n\n", b)
	w.(http.Flusher).Flush()
}

func sseText(w http.ResponseWriter, s string) {
	w.Header().Set("Content-Type", "text/event-stream")
	sseChunk(w, map[string]any{"role": "assistant", "content": s}, nil)
	sseChunk(w, map[string]any{}, "stop")
	fmt.Fprint(w, "data: [DONE]\n\n")
}

func sseTool(w http.ResponseWriter, args map[string]any) {
	w.Header().Set("Content-Type", "text/event-stream")
	a, _ := json.Marshal(args)
	sseChunk(w, map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"index": 0, "id": "call_1", "type": "function",
		"function": map[string]any{"name": "bash", "arguments": string(a)}}}}, nil)
	sseChunk(w, map[string]any{}, "tool_calls")
	fmt.Fprint(w, "data: [DONE]\n\n")
}

type nopSink struct{}

func (nopSink) OnUpdate(context.Context, acp.SessionNotification) error { return nil }

// TestE2EFirInterject drives a real fir binary ($FIR_BIN) in ACP mode:
// a main turn runs a blocking bash tool for longer than fir's idle TTL, a
// side question arrives, and the verdict must reach fir natively without
// stopping the turn. Run: FIR_BIN=$(which fir) go test -tags e2e ./interject/
func TestE2EFirInterject(t *testing.T) {
	bin := os.Getenv("FIR_BIN")
	if bin == "" {
		t.Skip("FIR_BIN not set")
	}
	for _, tc := range []struct {
		name   string
		side   string
		action interject.Action
	}{
		{"inform", "Side answer.\nNOTE: hello main\n2 9", interject.Inform},
		{"none", "Side answer.\n1 9", interject.None},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			releaseFile = filepath.Join(dir, "release")
			m := &mockLLM{side: tc.side, toolRun: make(chan struct{})}
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			srv := &http.Server{Handler: m}
			go srv.Serve(ln)
			defer srv.Close()
			agentDir := filepath.Join(dir, "agent")
			os.MkdirAll(agentDir, 0o755)
			os.WriteFile(filepath.Join(agentDir, "models.json"), []byte(fmt.Sprintf(`{"providers":{"mock":{"baseUrl":"http://%s/v1","apiKey":"k","api":"openai-completions","models":[{"id":"mock-model","name":"M","contextWindow":128000,"maxTokens":4096}]}}}`, ln.Addr())), 0o644)
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			ap, err := client.Start(ctx, client.Config{
				Command: []string{bin, "--mode", "acp", "--provider", "mock", "--model", "mock-model", "--acp-session-idle-ttl", "1s"},
				Cwd:     dir,
				Env:     append(os.Environ(), "FIR_AGENT_DIR="+agentDir),
			})
			if err != nil {
				t.Fatal(err)
			}
			defer ap.Close()
			sid, err := ap.NewSession(ctx, dir, nopSink{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			type res struct {
				sr  acp.StopReason
				err error
			}
			done := make(chan res, 1)
			go func() {
				sr, err := ap.Prompt(ctx, sid, []acp.ContentBlock{acp.TextBlock("do the long thing")})
				done <- res{sr, err}
			}()
			<-m.toolRun
			// Let fir's idle reaper (1s TTL, 1s tick) pass over the
			// running session; it is a real timer inside fir.
			time.Sleep(3 * time.Second)
			ij := interject.New(ap, interject.Config{Enabled: true})
			rep, err := ij.Ask(ctx, "c", dir, sid, "what are you doing?")
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("reply %+v", rep)
			if !rep.Delivered || rep.Action != tc.action {
				t.Errorf("Delivered=%v Action=%v", rep.Delivered, rep.Action)
			}
			os.WriteFile(releaseFile, nil, 0o644)
			r := <-done
			if r.err != nil || r.sr != acp.StopReasonEndTurn {
				t.Fatalf("main turn: %v %v", r.sr, r.err)
			}
			m.mu.Lock()
			last := m.requests[len(m.requests)-1]
			m.mu.Unlock()
			steered := strings.Contains(last, "hello main")
			if steered != (tc.action == interject.Inform) {
				t.Errorf("steer text in final main request = %v", steered)
			}
			if p := ij.Take("c"); p != "" {
				t.Errorf("Take = %q, want none", p)
			}
		})
	}
}
