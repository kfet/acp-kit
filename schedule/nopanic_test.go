package schedule

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestSaveMarshalError(t *testing.T) {
	s, err := Open(Config{Path: filepath.Join(t.TempDir(), "s.json"), Fire: func(context.Context, Item) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	old := marshalIndent
	t.Cleanup(func() { marshalIndent = old })
	marshalIndent = func(any, string, string) ([]byte, error) { return nil, errors.New("boom") }
	if _, err := s.Add("c", "x", time.Now().Add(time.Hour), 0); err == nil {
		t.Fatal("want marshal error")
	}
}
