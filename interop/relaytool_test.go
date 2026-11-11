package interop

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kfet/acp-kit/client"
	"github.com/kfet/acp-kit/command"
	"github.com/kfet/acp-kit/mcphost"
	"github.com/kfet/acp-kit/relaytool"
	"github.com/kfet/acp-kit/schedule"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

var fixedTime = time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)

// relay is a Controller with every optional capability, so the full
// relaytool set registers.
type relay struct {
	mu       sync.Mutex
	posted   []string
	files    []command.OutFile
	uploaded []mcphost.UploadedFile
	armed    []schedule.Item
	unsched  []string
	done     map[string]int64 // upload ref -> stored bytes
}

func (r *relay) AvailableModels() ([]client.ModelInfo, string) {
	return []client.ModelInfo{{ID: "m1", Name: "Model One"}, {ID: "m2", Name: "Model Two"}}, "m1"
}
func (r *relay) SetModelOverride(string, string) error { return nil }
func (r *relay) ResetSession(string) error             { return nil }
func (r *relay) StatusFor(string) command.SessionStatus {
	return command.SessionStatus{EffectiveModel: "m1", HasSession: true}
}
func (r *relay) AgentCommands() []client.CommandInfo { return nil }
func (r *relay) RelayInfo(string) command.RelayInfo {
	return command.RelayInfo{Version: "test", Uptime: "1h"}
}
func (r *relay) PostTo(_, text string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.posted = append(r.posted, text)
	return nil
}
func (r *relay) CanSchedule() bool { return true }
func (r *relay) Schedule(conv, text string, at time.Time, every time.Duration) (schedule.Item, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	it := schedule.Item{ID: "s01", Conv: conv, Text: text, At: at, Every: every, Depth: 1}
	r.armed = append(r.armed, it)
	return it, nil
}
func (r *relay) Schedules(string) []schedule.Item {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.armed)
}
func (r *relay) Unschedule(_, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.unsched = append(r.unsched, id)
	return nil
}
func (r *relay) PostFileTo(_ string, f command.OutFile) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.files = append(r.files, f)
	return []string{"msg-file"}, nil
}
func (r *relay) UploadTargetFor(_ string, s mcphost.UploadSpec) (mcphost.UploadTarget, error) {
	return mcphost.UploadTarget{URL: "https://uploads.example/u/" + s.FileID, Ref: "ref-" + s.FileID, ExpiresAt: fixedTime}, nil
}
func (r *relay) FinishUploadFor(_ string, _ mcphost.UploadSpec, ref string) (mcphost.UploadedFile, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n, ok := r.done[ref]
	if !ok {
		return mcphost.UploadedFile{}, errNotUploaded
	}
	return mcphost.UploadedFile{Ref: ref, Size: n}, nil
}
func (r *relay) PostUploadedTo(_ string, f mcphost.UploadedFile, _ string) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.uploaded = append(r.uploaded, f)
	return []string{"msg-up"}, nil
}
func (r *relay) AttachmentsIn(string) ([]command.InboundAttachment, error) {
	return []command.InboundAttachment{{ID: "a1", Name: "report.txt", MimeType: "text/plain", Size: 5}}, nil
}
func (r *relay) DownloadLinkFor(_, id string) (mcphost.DownloadTarget, error) {
	return mcphost.DownloadTarget{URL: "https://downloads.example/" + id, Name: "report.txt", MimeType: "text/plain",
		Size: 5, Digest: digest([]byte("hello")), ExpiresAt: fixedTime}, nil
}

// put stands in for the client's https upload.
func (r *relay) put(ref string, n int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.done == nil {
		r.done = map[string]int64{}
	}
	r.done[ref] = n
}

var errNotUploaded = errors.New("nothing uploaded")

func digest(b []byte) string {
	s := sha256.Sum256(b)
	return base64.RawURLEncoding.EncodeToString(s[:])
}

func relayHost(t *testing.T, dir string) (*mcphost.Host, *relaytool.Tools, *relay) {
	t.Helper()
	rl := &relay{}
	b := command.New(nil)
	b.SetController(rl)
	tools, err := relaytool.New(relaytool.Config{Broker: b})
	if err != nil {
		t.Fatal(err)
	}
	h, err := mcphost.New(hostConfig(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	tools.Register(h)
	return h, tools, rl
}

// toolCase is one relaytool tool's interop round-trip.
type toolCase struct {
	args  map[string]any
	check func(t *testing.T, r *sdk.CallToolResult, rl *relay)
}

func wantText(sub string) func(*testing.T, *sdk.CallToolResult, *relay) {
	return func(t *testing.T, r *sdk.CallToolResult, _ *relay) {
		t.Helper()
		if !strings.Contains(text(r), sub) {
			t.Fatalf("text %q lacks %q", text(r), sub)
		}
	}
}

// toolCases must name every tool relaytool registers:
// TestEveryRelayToolHasACase fails otherwise.
var toolCases = map[string]toolCase{
	relaytool.ToolStatus:     {nil, wantText("m1")},
	relaytool.ToolListModels: {map[string]any{"filter": "two"}, wantText("m2")},
	relaytool.ToolSetModel:   {map[string]any{"model_id": "m2"}, wantText("m2")},
	relaytool.ToolNewSession: {nil, nil},
	relaytool.ToolPost: {map[string]any{"text": "hello chat"}, func(t *testing.T, _ *sdk.CallToolResult, rl *relay) {
		if !slices.Contains(rl.posted, "hello chat") {
			t.Fatalf("posted = %v", rl.posted)
		}
	}},
	relaytool.ToolSchedule: {map[string]any{"text": "wake up", "at": "2030-01-02T03:04:05Z"}, func(t *testing.T, _ *sdk.CallToolResult, rl *relay) {
		if len(rl.armed) != 1 || !rl.armed[0].At.Equal(fixedTime) {
			t.Fatalf("armed = %+v", rl.armed)
		}
	}},
	relaytool.ToolListSchedules: {nil, wantText("s01")},
	relaytool.ToolUnschedule: {map[string]any{"id": "s01"}, func(t *testing.T, _ *sdk.CallToolResult, rl *relay) {
		if !slices.Contains(rl.unsched, "s01") {
			t.Fatalf("unscheduled = %v", rl.unsched)
		}
	}},
	relaytool.ToolSendFile: {map[string]any{"caption": "c", "file": map[string]any{
		"id": "f1", "name": "a.txt", "mimeType": "text/plain", "size": 2,
		"digest": map[string]any{"algorithm": "sha-256", "value": digest([]byte("hi"))},
		"data":   base64.StdEncoding.EncodeToString([]byte("hi")),
	}}, func(t *testing.T, _ *sdk.CallToolResult, rl *relay) {
		if len(rl.files) != 1 || string(rl.files[0].Data) != "hi" || rl.files[0].Name != "a.txt" {
			t.Fatalf("files = %+v", rl.files)
		}
	}},
	relaytool.ToolListAttachments: {nil, wantText("a1")},
	relaytool.ToolFetchAttachment: {map[string]any{"id": "a1"}, func(t *testing.T, r *sdk.CallToolResult, _ *relay) {
		fx, _ := r.Meta[mcphost.FilesExtension].(map[string]any)
		ds, _ := fx["downloads"].([]any)
		if len(ds) != 1 || ds[0].(map[string]any)["url"] != "https://downloads.example/a1" {
			t.Fatalf("downloads = %v", r.Meta)
		}
	}},
}

// order is a call order that makes stateful cases meaningful
// (list_schedules and unschedule after schedule).
func order(names []string) []string {
	out := slices.Clone(names)
	rank := func(n string) int {
		switch n {
		case relaytool.ToolListSchedules:
			return 1
		case relaytool.ToolUnschedule:
			return 2
		}
		return 0
	}
	slices.SortStableFunc(out, func(a, b string) int { return rank(a) - rank(b) })
	return out
}

func registered(tools *relaytool.Tools) []string {
	var out []string
	for _, x := range tools.Tools() {
		out = append(out, x.Name)
	}
	return out
}

// TestEveryRelayToolHasACase: a newly registered tool without an interop
// case fails here.
func TestEveryRelayToolHasACase(t *testing.T) {
	_, tools, _ := relayHost(t, "")
	for _, n := range registered(tools) {
		if _, ok := toolCases[n]; !ok {
			t.Errorf("relaytool tool %q has no interop case in toolCases", n)
		}
	}
	for n := range toolCases {
		if !slices.Contains(registered(tools), n) {
			t.Errorf("toolCases names %q, which relaytool does not register (on a full-capability relay)", n)
		}
	}
}

// TestRelayToolsRoundTrip lists and calls every relaytool tool through
// the real client, on the newest protocol and on the legacy one.
func TestRelayToolsRoundTrip(t *testing.T) {
	for _, v := range []string{"", "2025-06-18"} {
		name := v
		if name == "" {
			name = "latest"
		}
		t.Run(name, func(t *testing.T) {
			h, tools, rl := relayHost(t, "")
			listen(t, h)
			cs, tp := connect(t, h, firCaps(), v)
			tp.reset()

			names := registered(tools)
			wantListed := names
			if v != "" {
				// Before 2026-07-28 the SDK sends no clientCapabilities
				// _meta, so x-mcp-file tools stay hidden.
				wantListed = slices.DeleteFunc(slices.Clone(names), func(n string) bool { return n == relaytool.ToolSendFile })
			}
			if got := toolNames(t, cs); !slices.Equal(got, wantListed) {
				t.Fatalf("tools/list = %v, want %v", got, wantListed)
			}
			for _, n := range order(names) {
				c := toolCases[n]
				r := callTool(t, cs, &sdk.CallToolParams{Name: n, Arguments: c.args})
				if r.IsError {
					t.Fatalf("%s: isError: %q", n, text(r))
				}
				switch {
				case v != "" && n == relaytool.ToolFetchAttachment:
					// No clientCapabilities _meta before 2026-07-28: a note, not a target.
					wantText("does not accept MCP file downloads")(t, r, rl)
				case c.check != nil:
					c.check(t, r, rl)
				}
			}
			golden(t, "relaytool-"+name, tp)
		})
	}
}

// TestRelayToolErrors: bad arguments come back as tool errors.
func TestRelayToolErrors(t *testing.T) {
	h, _, _ := relayHost(t, "")
	listen(t, h)
	cs, tp := connect(t, h, firCaps(), "")
	tp.reset()
	for _, p := range []*sdk.CallToolParams{
		{Name: relaytool.ToolSetModel},
		{Name: relaytool.ToolSchedule, Arguments: map[string]any{"text": "x", "in": "soon"}},
		{Name: relaytool.ToolFetchAttachment},
		{Name: relaytool.ToolSendFile, Arguments: map[string]any{"file": map[string]any{"id": "f", "mimeType": "text/plain", "size": 3, "data": "aGk="}}},
	} {
		if r := callTool(t, cs, p); !r.IsError {
			t.Errorf("%s %v: want isError, got %q", p.Name, p.Arguments, text(r))
		}
	}
	golden(t, "relaytool-errors", tp)
}

// TestRelaySendFileOutOfBand: send_file's upload_required + requestState
// retry through the real client.
func TestRelaySendFileOutOfBand(t *testing.T) {
	h, _, rl := relayHost(t, "")
	listen(t, h)
	cs, tp := connect(t, h, firCaps(), "")
	tp.reset()
	args := map[string]any{"file": map[string]any{"id": "f2", "name": "big.txt", "mimeType": "text/plain", "size": 42}}

	first := callTool(t, cs, &sdk.CallToolParams{Name: relaytool.ToolSendFile, Arguments: args})
	ur, ok := first.Meta[uploadRequiredMeta].(map[string]any)
	if !ok || first.RequestState == "" {
		t.Fatalf("no upload_required: meta %v", first.Meta)
	}
	if ups, _ := ur["uploads"].([]any); len(ups) != 1 || ups[0].(map[string]any)["url"] != "https://uploads.example/u/f2" {
		t.Fatalf("uploads = %v", ur["uploads"])
	}
	shape(t, "upload_required", tp.lastResult("tools/call"), checkUploadRequired)

	rl.put("ref-f2", 42)
	r := callTool(t, cs, &sdk.CallToolParams{Name: relaytool.ToolSendFile, Arguments: args, RequestState: first.RequestState})
	if r.IsError || len(rl.uploaded) != 1 || rl.uploaded[0].Size != 42 {
		t.Fatalf("retry = %q, uploaded %+v", text(r), rl.uploaded)
	}
	// requestState is single-use: a replay is refused.
	if r := callTool(t, cs, &sdk.CallToolParams{Name: relaytool.ToolSendFile, Arguments: args, RequestState: first.RequestState}); !r.IsError {
		t.Fatalf("replayed requestState accepted: %q", text(r))
	}
	golden(t, "relaytool-send-file-oob", tp)
}

// TestRelayDownloadShape: fetch_attachment's download targets.
func TestRelayDownloadShape(t *testing.T) {
	h, _, _ := relayHost(t, "")
	listen(t, h)
	cs, tp := connect(t, h, firCaps(), "")
	callTool(t, cs, &sdk.CallToolParams{Name: relaytool.ToolFetchAttachment, Arguments: map[string]any{"id": "a1"}})
	shape(t, "downloads", tp.lastResult("tools/call"), checkDownloads)

	// A client that cannot take downloads gets a note instead.
	plain, _ := connect(t, h, nil, "")
	r := callTool(t, plain, &sdk.CallToolParams{Name: relaytool.ToolFetchAttachment, Arguments: map[string]any{"id": "a1"}})
	if _, ok := r.Meta[mcphost.FilesExtension]; ok || !strings.Contains(text(r), "does not accept MCP file downloads") {
		t.Fatalf("meta %v text %q", r.Meta, text(r))
	}
	// And without the files capability the x-mcp-file tool is hidden.
	if slices.Contains(toolNames(t, plain), relaytool.ToolSendFile) {
		t.Fatal("send_file listed to a client without the files extension")
	}
}

// TestRelayToolsMinimalRelay: on a relay with no optional capability the
// real client sees exactly the base tools, and each works.
func TestRelayToolsMinimalRelay(t *testing.T) {
	b := command.New(nil)
	b.SetController(minimal{&relay{}})
	tools, err := relaytool.New(relaytool.Config{Broker: b})
	if err != nil {
		t.Fatal(err)
	}
	h, err := mcphost.New(hostConfig(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	tools.Register(h)
	listen(t, h)
	cs, _ := connect(t, h, firCaps(), "")
	want := []string{relaytool.ToolStatus, relaytool.ToolListModels, relaytool.ToolSetModel, relaytool.ToolNewSession}
	if got := toolNames(t, cs); !slices.Equal(got, want) {
		t.Fatalf("tools/list = %v, want %v", got, want)
	}
	for _, n := range want {
		if r := callTool(t, cs, &sdk.CallToolParams{Name: n, Arguments: toolCases[n].args}); r.IsError {
			t.Fatalf("%s: %q", n, text(r))
		}
	}
}

// minimal exposes only command.Controller.
type minimal struct{ r *relay }

func (m minimal) AvailableModels() ([]client.ModelInfo, string) { return m.r.AvailableModels() }
func (m minimal) SetModelOverride(c, id string) error           { return m.r.SetModelOverride(c, id) }
func (m minimal) ResetSession(c string) error                   { return m.r.ResetSession(c) }
func (m minimal) StatusFor(c string) command.SessionStatus      { return m.r.StatusFor(c) }
func (m minimal) AgentCommands() []client.CommandInfo           { return m.r.AgentCommands() }
func (m minimal) RelayInfo(c string) command.RelayInfo          { return m.r.RelayInfo(c) }
