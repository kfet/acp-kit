package convo

import (
	"context"
	"testing"
)

// A login can end between HasPending and Handle; the broker then returns
// no outcome. command must report "not handled", never panic.
func TestCommandNoOutcomeForwards(t *testing.T) {
	m := newM(t, Config{Agent: &fakeAgent{}})
	if m.command(context.Background(), &In{Conv: "c", Text: "hello"}) {
		t.Fatal("nil outcome must not count as handled")
	}
}
