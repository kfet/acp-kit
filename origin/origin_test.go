package origin

import (
	"context"
	"encoding/json"
	"testing"
)

var sample = Origin{
	Platform: "slack", TeamID: "T1", UserID: "U1", ChannelID: "C1",
	ThreadID: "1.2", MessageID: "1.3", IsDM: true, IsMention: true,
}

func TestWireShape(t *testing.T) {
	b, err := json.Marshal(sample.Merge(nil))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"dev.acp-kit.origin/v1":{"platform":"slack","team_id":"T1","user_id":"U1","channel_id":"C1","thread_id":"1.2","message_id":"1.3","is_dm":true,"is_mention":true}}`
	if string(b) != want {
		t.Fatalf("got %s", b)
	}
	got, ok := FromRawMeta(b)
	if !ok || got != sample {
		t.Fatalf("round trip: %+v %v", got, ok)
	}
}

func TestMerge(t *testing.T) {
	if (Origin{}).Merge(nil) != nil {
		t.Fatal("zero origin must not allocate")
	}
	in := map[string]any{"a": 1}
	out := sample.Merge(in)
	if len(in) != 1 || out["a"] != 1 || out[Key] != sample {
		t.Fatalf("merge: in=%v out=%v", in, out)
	}
}

func TestFromMeta(t *testing.T) {
	if _, ok := FromMeta(nil); ok {
		t.Fatal("absent")
	}
	if o, ok := FromMeta(sample.Merge(nil)); !ok || o != sample {
		t.Fatal("typed value")
	}
	var generic map[string]any
	b, _ := json.Marshal(sample.Merge(nil))
	_ = json.Unmarshal(b, &generic)
	if o, ok := FromMeta(generic); !ok || o != sample {
		t.Fatal("generic value")
	}
	if _, ok := FromMeta(map[string]any{Key: func() {}}); ok {
		t.Fatal("unmarshalable")
	}
	if _, ok := FromMeta(map[string]any{Key: "str"}); ok {
		t.Fatal("wrong type")
	}
}

func TestFromRawMeta(t *testing.T) {
	for _, raw := range []string{`nope`, `{}`, `{"dev.acp-kit.origin/v1":null}`, `{"dev.acp-kit.origin/v1":{}}`, `{"dev.acp-kit.origin/v1":1}`} {
		if _, ok := FromRawMeta(json.RawMessage(raw)); ok {
			t.Fatalf("%s accepted", raw)
		}
	}
}

func TestContext(t *testing.T) {
	if _, ok := From(context.Background()); ok {
		t.Fatal("empty ctx")
	}
	if o, ok := From(With(context.Background(), sample)); !ok || o != sample {
		t.Fatal("ctx round trip")
	}
	if !(Origin{}).IsZero() || sample.IsZero() {
		t.Fatal("IsZero")
	}
}
