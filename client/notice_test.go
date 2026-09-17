package client

import (
	"context"
	"encoding/json"
	"testing"

	acp "github.com/coder/acp-go-sdk"
)

// noticeSink is a SessionUpdateSink that also implements NoticeSink.
type noticeSink struct {
	updates []acp.SessionNotification
	notices []Notice
	err     error
}

func (s *noticeSink) OnUpdate(_ context.Context, n acp.SessionNotification) error {
	s.updates = append(s.updates, n)
	return nil
}

func (s *noticeSink) OnNotice(_ context.Context, n Notice) error {
	s.notices = append(s.notices, n)
	return s.err
}

// plainSink implements only SessionUpdateSink.
type plainSink struct{ updates int }

func (s *plainSink) OnUpdate(context.Context, acp.SessionNotification) error {
	s.updates++
	return nil
}

func newAgentWithSink(sid acp.SessionId, sink SessionUpdateSink) *AgentProc {
	a := &AgentProc{sinks: make(map[acp.SessionId]SessionUpdateSink)}
	if sink != nil {
		a.sinks[sid] = sink
	}
	return a
}

func TestDispatch_NoticeRoutedToNoticeSink(t *testing.T) {
	sink := &noticeSink{}
	a := newAgentWithSink("s1", sink)

	params, _ := json.Marshal(Notice{
		SessionId: "s1", Level: NoticeLevelWarning, Kind: "provider_retry", Text: "⏳ retrying in 30s",
	})
	resp, rerr := a.dispatch(context.Background(), NoticeMethod, params)
	if rerr != nil {
		t.Fatalf("dispatch returned error: %v", rerr)
	}
	if resp != nil {
		t.Errorf("a notification must produce no result, got %v", resp)
	}
	if len(sink.notices) != 1 {
		t.Fatalf("expected 1 notice, got %d", len(sink.notices))
	}
	got := sink.notices[0]
	if got.Kind != "provider_retry" || got.Level != NoticeLevelWarning || got.Text != "⏳ retrying in 30s" {
		t.Errorf("unexpected notice: %+v", got)
	}
	// A notice must NEVER reach the answer stream.
	if len(sink.updates) != 0 {
		t.Errorf("notice leaked into the session update stream: %d updates", len(sink.updates))
	}
}

func TestDispatch_NoticeSinkErrorIsSwallowed(t *testing.T) {
	sink := &noticeSink{err: context.Canceled}
	a := newAgentWithSink("s1", sink)
	params, _ := json.Marshal(Notice{SessionId: "s1", Text: "hi"})

	if _, rerr := a.dispatch(context.Background(), NoticeMethod, params); rerr != nil {
		t.Fatalf("a failing notice must not fail the turn: %v", rerr)
	}
}

func TestDispatch_NoticeDroppedWhenSinkLacksInterface(t *testing.T) {
	sink := &plainSink{}
	a := newAgentWithSink("s1", sink)
	params, _ := json.Marshal(Notice{SessionId: "s1", Text: "hi"})

	if _, rerr := a.dispatch(context.Background(), NoticeMethod, params); rerr != nil {
		t.Fatalf("unexpected error: %v", rerr)
	}
	if sink.updates != 0 {
		t.Errorf("notice must not be delivered as an update, got %d", sink.updates)
	}
}

func TestDispatch_NoticeForUnknownSessionIsDropped(t *testing.T) {
	a := newAgentWithSink("s1", nil)
	params, _ := json.Marshal(Notice{SessionId: "nope", Text: "hi"})

	if _, rerr := a.dispatch(context.Background(), NoticeMethod, params); rerr != nil {
		t.Fatalf("unexpected error: %v", rerr)
	}
}

func TestDispatch_MalformedNoticeIsDropped(t *testing.T) {
	sink := &noticeSink{}
	a := newAgentWithSink("s1", sink)

	if _, rerr := a.dispatch(context.Background(), NoticeMethod, json.RawMessage("not-json")); rerr != nil {
		t.Fatalf("a malformed notice must not fail the turn: %v", rerr)
	}
	if len(sink.notices) != 0 {
		t.Errorf("expected no notices, got %d", len(sink.notices))
	}
}

// TestDispatch_UnknownExtensionMethodIgnored pins the spec rule that
// implementations SHOULD ignore unrecognized notifications — this is what
// makes version skew between relay and agent safe.
func TestDispatch_UnknownExtensionMethodIgnored(t *testing.T) {
	a := newAgentWithSink("s1", &noticeSink{})

	resp, rerr := a.dispatch(context.Background(), "_com.example/whatever", json.RawMessage(`{}`))
	if rerr != nil {
		t.Errorf("unknown extension method must be ignored, got %v", rerr)
	}
	if resp != nil {
		t.Errorf("expected no result, got %v", resp)
	}
}

// TestDispatch_UnknownCoreMethodStillErrors guards against the extension
// escape hatch swallowing genuinely unsupported core methods.
func TestDispatch_UnknownCoreMethodStillErrors(t *testing.T) {
	a := newAgentWithSink("s1", &noticeSink{})

	if _, rerr := a.dispatch(context.Background(), "terminal/create", json.RawMessage(`{}`)); rerr == nil {
		t.Error("expected MethodNotFound for an unadvertised core method")
	}
}

func TestParseNoticeCap(t *testing.T) {
	cases := []struct {
		name string
		meta string
		want bool
	}{
		{"advertised", `{"agentCapabilities":{"_meta":{"dev.acp-kit":{"notice":{"method":"_dev.acp-kit/notice","version":1}}}}}`, true},
		{"wrong method", `{"agentCapabilities":{"_meta":{"dev.acp-kit":{"notice":{"method":"_other/notice"}}}}}`, false},
		{"namespace without notice", `{"agentCapabilities":{"_meta":{"dev.acp-kit":{"status-line":{}}}}}`, false},
		{"namespace not an object", `{"agentCapabilities":{"_meta":{"dev.acp-kit":"yes"}}}`, false},
		{"no meta", `{"agentCapabilities":{}}`, false},
		{"garbage", `not-json`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseCaps(json.RawMessage(tc.meta)).Notices; got != tc.want {
				t.Errorf("Notices = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAgentSupportsNotices(t *testing.T) {
	a := &AgentProc{caps: Caps{Notices: true}}
	if !a.AgentSupportsNotices() {
		t.Error("expected true")
	}
	if (&AgentProc{}).AgentSupportsNotices() {
		t.Error("expected false")
	}
}

func TestNotice_JSONShape(t *testing.T) {
	raw, err := json.Marshal(Notice{SessionId: "s1", Level: "warning", Kind: "provider_retry", Text: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"sessionId":"s1","level":"warning","kind":"provider_retry","text":"hi"}`
	if string(raw) != want {
		t.Errorf("wire shape drift:\n got %s\nwant %s", raw, want)
	}
}
