package client

import (
	"context"
	"testing"

	acp "github.com/coder/acp-go-sdk"
)

func TestPromptAbstainable_DeliversNormalReply(t *testing.T) {
	cap := &capSink{}
	vs := NewValidatingSink(cap)
	ag := &scriptAgent{vs: vs, turns: []string{"here is a useful answer"}}
	res, err := PromptAbstainable(context.Background(), ag, "s", []acp.ContentBlock{acp.TextBlock("hi")}, vs, "<<SILENT>>")
	if err != nil {
		t.Fatalf("PromptAbstainable: %v", err)
	}
	if res.Abstained {
		t.Fatal("should not abstain on a normal reply")
	}
	if len(cap.msgs) != 1 || cap.msgs[0] != "here is a useful answer" {
		t.Fatalf("delivered = %v", cap.msgs)
	}
}

func TestPromptAbstainable_AbstainsOnSentinel(t *testing.T) {
	cap := &capSink{}
	vs := NewValidatingSink(cap)
	// Trailing whitespace must still match after trimming.
	ag := &scriptAgent{vs: vs, turns: []string{"<<SILENT>>\n"}}
	res, err := PromptAbstainable(context.Background(), ag, "s", []acp.ContentBlock{acp.TextBlock("hi")}, vs, "<<SILENT>>")
	if err != nil {
		t.Fatalf("PromptAbstainable: %v", err)
	}
	if !res.Abstained {
		t.Fatal("should abstain on sentinel")
	}
	if len(cap.msgs) != 0 {
		t.Fatalf("nothing should be delivered, got %v", cap.msgs)
	}
}

func TestPromptAbstainable_AbstainsOnEmpty(t *testing.T) {
	cap := &capSink{}
	vs := NewValidatingSink(cap)
	ag := &scriptAgent{vs: vs, turns: []string{"   "}}
	res, err := PromptAbstainable(context.Background(), ag, "s", []acp.ContentBlock{acp.TextBlock("hi")}, vs, "<<SILENT>>")
	if err != nil {
		t.Fatalf("PromptAbstainable: %v", err)
	}
	if !res.Abstained {
		t.Fatal("should abstain on empty output")
	}
	if len(cap.msgs) != 0 {
		t.Fatalf("nothing delivered, got %v", cap.msgs)
	}
}

func TestPromptAbstainable_BlankSentinelOnlyEmptyAbstains(t *testing.T) {
	cap := &capSink{}
	vs := NewValidatingSink(cap)
	ag := &scriptAgent{vs: vs, turns: []string{"<<SILENT>>"}}
	res, err := PromptAbstainable(context.Background(), ag, "s", []acp.ContentBlock{acp.TextBlock("hi")}, vs, "")
	if err != nil {
		t.Fatalf("PromptAbstainable: %v", err)
	}
	if res.Abstained {
		t.Fatal("blank sentinel must not match the literal text")
	}
	if len(cap.msgs) != 1 || cap.msgs[0] != "<<SILENT>>" {
		t.Fatalf("delivered = %v", cap.msgs)
	}
}

func TestPromptAbstainable_PromptErrorPropagates(t *testing.T) {
	cap := &capSink{}
	vs := NewValidatingSink(cap)
	if _, err := PromptAbstainable(context.Background(), errAgent{}, "s", nil, vs, "<<SILENT>>"); err == nil {
		t.Fatal("expected prompt error")
	}
}
