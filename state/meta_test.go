package state

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMetaRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if got, err := ReadMetas(dir); err != nil || len(got) != 0 {
		t.Fatalf("empty: %v %v", got, err)
	}
	if err := WriteMeta(dir, "c1", []byte(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	if err := WriteMeta(dir, "c1", []byte(`{"a":2}`)); err != nil {
		t.Fatal(err)
	}
	// A dir without meta, a file, and a dot dir are left out.
	if err := os.MkdirAll(filepath.Join(dir, "convs", "c2"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "convs", ".hidden"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "convs", "stray"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ReadMetas(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || string(got["c1"]) != `{"a":2}` {
		t.Fatalf("got %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "convs", "c1", MetaFile+".tmp")); !os.IsNotExist(err) {
		t.Fatalf("tmp left: %v", err)
	}
}

func TestWriteMetaErrors(t *testing.T) {
	dir := t.TempDir()
	if err := WriteMeta(dir, "../x", nil); err == nil {
		t.Fatal("bad key accepted")
	}
	// convs is a file: mkdir fails.
	if err := os.WriteFile(filepath.Join(dir, "convs"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteMeta(dir, "c1", nil); err == nil {
		t.Fatal("mkdir error not reported")
	}
	if _, err := ReadMetas(dir); err == nil {
		t.Fatal("readdir error not reported")
	}

	dir = t.TempDir()
	cd := filepath.Join(dir, "convs", "c1")
	// tmp path is a dir: write fails.
	if err := os.MkdirAll(filepath.Join(cd, MetaFile+".tmp"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteMeta(dir, "c1", nil); err == nil {
		t.Fatal("write error not reported")
	}

	dir = t.TempDir()
	cd = filepath.Join(dir, "convs", "c1")
	// target is a non-empty dir: rename fails.
	if err := os.MkdirAll(filepath.Join(cd, MetaFile, "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteMeta(dir, "c1", nil); err == nil {
		t.Fatal("rename error not reported")
	}
	// meta is a dir: read fails.
	if _, err := ReadMetas(dir); err == nil {
		t.Fatal("read error not reported")
	}
}
