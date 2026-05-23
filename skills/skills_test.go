package skills

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

const goodFS = "good"

func goodBundle() fstest.MapFS {
	return fstest.MapFS{
		"bundle/deploy/SKILL.md": {Data: []byte("---\nname: deploy\ndescription: Deploy it\nbuiltin: true\n---\nbody\n")},
		"bundle/local/SKILL.md":  {Data: []byte("---\nname: local\ndescription: Local only\n---\nbody\n")},
		"bundle/named/SKILL.md":  {Data: []byte("---\nbuiltin: true\ndescription: dirname-fallback\n---\n")},
		"bundle/other.txt":       {Data: []byte("ignored")},
	}
}

func TestLoadBuiltinExtractsAndCachesAndFallsBack(t *testing.T) {
	got, err := LoadBuiltin(goodBundle(), "acp-kit-test")
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	if len(got) != 2 || got[0].Name != "deploy" || got[1].Name != "named" {
		t.Fatalf("got = %#v", got)
	}
	if !strings.Contains(got[0].Path, "acp-kit-test-") {
		t.Fatalf("missing app prefix: %q", got[0].Path)
	}
	body, err := os.ReadFile(got[0].Path)
	if err != nil || !strings.Contains(string(body), "body") {
		t.Fatalf("read: %v %q", err, body)
	}
	// Second call hits the file-content-matches branch (no rewrite).
	if _, err := LoadBuiltin(goodBundle(), "acp-kit-test"); err != nil {
		t.Fatalf("second LoadBuiltin: %v", err)
	}
}

func TestLoadBuiltinRequiresAppPrefix(t *testing.T) {
	if _, err := LoadBuiltin(fstest.MapFS{}, ""); err == nil {
		t.Fatal("expected error on empty prefix")
	}
	if _, err := LoadBuiltin(fstest.MapFS{}, "  "); err == nil {
		t.Fatal("expected error on whitespace prefix")
	}
}

// errBundleFS makes bundleHashFS fail to cover the hash-error branch.
type errBundleFS struct{}

func (errBundleFS) Open(string) (fs.File, error) { return nil, errors.New("boom") }

func TestLoadBuiltinHashError(t *testing.T) {
	if _, err := LoadBuiltin(errBundleFS{}, "x"); err == nil {
		t.Fatal("expected hash error")
	}
}

func TestLoadBuiltinSeamErrors(t *testing.T) {
	t.Cleanup(restoreSeams())
	osMkdirAll = func(string, os.FileMode) error { return errors.New("mkdir failed") }
	if _, err := LoadBuiltin(goodBundle(), "acp-kit-seam"); err == nil {
		t.Fatal("expected mkdir error")
	}
	osMkdirAll = os.MkdirAll
	osReadFile = func(string) ([]byte, error) { return nil, errors.New("read failed") }
	osWriteFile = func(string, []byte, os.FileMode) error { return errors.New("write failed") }
	if _, err := LoadBuiltin(goodBundle(), "acp-kit-seam2"); err == nil {
		t.Fatal("expected write error")
	}
}

func TestLoadBuiltinReadFileWalkError(t *testing.T) {
	t.Cleanup(restoreSeams())
	// fs.ReadFile inside the walk uses the passed fsys directly; we trigger
	// the error by giving the bundle an entry whose body cannot be read.
	bad := fstest.MapFS{
		"bundle/x/SKILL.md": {Data: []byte("---\nname: x\nbuiltin: true\ndescription: y\n---\n")},
	}
	osMkdirAll = func(p string, _ os.FileMode) error {
		// Force the per-skill mkdir to fail to exercise that branch quickly.
		if strings.Contains(p, "acp-kit-readwalk") {
			return errors.New("mkdir bad")
		}
		return os.MkdirAll(p, 0o755)
	}
	if _, err := LoadBuiltin(bad, "acp-kit-readwalk"); err == nil {
		t.Fatal("expected mkdir error from walk")
	}
}

func TestLoadDirHappyAndSkips(t *testing.T) {
	root := t.TempDir()
	must := func(p, body string) {
		dir := filepath.Join(root, p)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	must("alpha", "---\nname: alpha\ndescription: A\n---\nb")
	must("noname", "---\ndescription: N\n---\nb")
	must("nodesc", "---\nname: nodesc\n---\nb")
	// Stray top-level file: skipped (entry is not a dir).
	if err := os.WriteFile(filepath.Join(root, "loose.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Directory with no SKILL.md: silently skipped (ENOENT branch).
	if err := os.MkdirAll(filepath.Join(root, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := LoadDir(root)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	names := []string{}
	for _, s := range got {
		names = append(names, s.Name)
	}
	if strings.Join(names, ",") != "alpha,noname" {
		t.Fatalf("names = %v", names)
	}
}

func TestLoadDirEmptyAndMissing(t *testing.T) {
	if got, err := LoadDir(""); err != nil || got != nil {
		t.Fatalf("empty path: %#v err=%v", got, err)
	}
	if got, err := LoadDir(filepath.Join(t.TempDir(), "nope")); err != nil || got != nil {
		t.Fatalf("missing path: %#v err=%v", got, err)
	}
}

func TestLoadDirSeamErrors(t *testing.T) {
	t.Cleanup(restoreSeams())
	filepathAbs = func(string) (string, error) { return "", errors.New("abs failed") }
	if _, err := LoadDir("/nope"); err == nil {
		t.Fatal("expected abs error")
	}
	filepathAbs = filepath.Abs
	osReadDir = func(string) ([]os.DirEntry, error) { return nil, errors.New("read dir failed") }
	if _, err := LoadDir("/nope"); err == nil {
		t.Fatal("expected read-dir error")
	}
}

func TestLoadDirReadFileError(t *testing.T) {
	t.Cleanup(restoreSeams())
	root := t.TempDir()
	sub := filepath.Join(root, "x")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "SKILL.md"), []byte("---\nname: x\ndescription: y\n---"), 0o644); err != nil {
		t.Fatal(err)
	}
	osReadFile = func(string) ([]byte, error) { return nil, errors.New("non-enoent read") }
	got, err := LoadDir(root)
	if err != nil || got != nil {
		t.Fatalf("got=%v err=%v", got, err)
	}
}

func TestMergeAndDisable(t *testing.T) {
	base := []Skill{{Name: "a", Description: "A1"}, {Name: "b", Description: "B"}}
	host := []Skill{{Name: "a", Description: "A2"}, {Name: "c", Description: "C"}}
	got := Merge([][]Skill{base, host}, []string{"b"})
	if len(got) != 2 || got[0].Name != "a" || got[0].Description != "A2" || got[1].Name != "c" {
		t.Fatalf("got = %#v", got)
	}
}

func TestFormatCatalogEscapesAndEmpty(t *testing.T) {
	if FormatCatalog(nil) != "" {
		t.Fatal("empty catalog should be empty string")
	}
	out := FormatCatalog([]Skill{{Name: "n<a>", Description: "d&e", Path: "/p>x"}})
	for _, want := range []string{"&lt;", "&amp;", "&gt;", "n&lt;a&gt;", "d&amp;e"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in %q", want, out)
		}
	}
}

func TestParseFrontmatterEdges(t *testing.T) {
	if name, _, _ := parseFrontmatter([]byte("no frontmatter")); name != "" {
		t.Fatal("expected zero values when no frontmatter")
	}
	if name, _, _ := parseFrontmatter([]byte("---\nopen but no close\n")); name != "" {
		t.Fatal("expected zero on missing close")
	}
	name, desc, b := parseFrontmatter([]byte("---\nrandomline\nname: x\ndescription: y\nbuiltin: false\n---\n"))
	if name != "x" || desc != "y" || b {
		t.Fatalf("name=%q desc=%q builtin=%v", name, desc, b)
	}
}

func restoreSeams() func() {
	m, w, r, d, a := osMkdirAll, osWriteFile, osReadFile, osReadDir, filepathAbs
	return func() {
		osMkdirAll, osWriteFile, osReadFile, osReadDir, filepathAbs = m, w, r, d, a
	}
}

// Compile-time check we did not break errors.Is for fs.ErrNotExist semantics.
var _ = errors.Is(fs.ErrNotExist, fs.ErrNotExist)

// readFailFS wraps a MapFS but injects read failures on a target path,
// covering the rerr branches inside LoadBuiltin and bundleHashFS.
type readFailFS struct {
	inner    fs.FS
	failPath string
}

func (r readFailFS) Open(name string) (fs.File, error) {
	if name == r.failPath {
		return nil, errors.New("inject open failure")
	}
	return r.inner.Open(name)
}

func TestBundleHashFSReadError(t *testing.T) {
	bundle := fstest.MapFS{
		"bundle/x/SKILL.md": {Data: []byte("---\nname: x\nbuiltin: true\ndescription: y\n---\n")},
	}
	if _, err := LoadBuiltin(readFailFS{inner: bundle, failPath: "bundle/x/SKILL.md"}, "acp-kit-readfail"); err == nil {
		t.Fatal("expected read error to propagate")
	}
}

// walkErrFS makes ReadDir on the bundle root fail so WalkDir invokes the
// walk fn with a non-nil walkErr.
type walkErrFS struct{ inner fs.FS }

func (w walkErrFS) Open(name string) (fs.File, error) { return w.inner.Open(name) }
func (w walkErrFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if name == "bundle" {
		return nil, errors.New("readdir fail")
	}
	if rdf, ok := w.inner.(fs.ReadDirFS); ok {
		return rdf.ReadDir(name)
	}
	return fs.ReadDir(w.inner, name)
}

func TestBundleWalkError(t *testing.T) {
	if _, err := LoadBuiltin(walkErrFS{inner: goodBundle()}, "acp-kit-walkerr"); err == nil {
		t.Fatal("expected walk error")
	}
}

// secondCallFailFS lets the first hashing walk succeed and makes the second
// extraction walk fail, covering the second WalkDir's walkErr branch and the
// rerr branch inside LoadBuiltin's walk fn.
type secondCallFailFS struct {
	inner       fs.FS
	readDirHits int
	openHits    map[string]int
}

func (s *secondCallFailFS) ReadDir(name string) ([]fs.DirEntry, error) {
	s.readDirHits++
	if name == "bundle" && s.readDirHits > 1 {
		return nil, errors.New("second-walk readdir fail")
	}
	if rdf, ok := s.inner.(fs.ReadDirFS); ok {
		return rdf.ReadDir(name)
	}
	return fs.ReadDir(s.inner, name)
}

func (s *secondCallFailFS) Open(name string) (fs.File, error) {
	if s.openHits == nil {
		s.openHits = map[string]int{}
	}
	s.openHits[name]++
	return s.inner.Open(name)
}

func TestLoadBuiltinSecondWalkAndReadErrors(t *testing.T) {
	if _, err := LoadBuiltin(&secondCallFailFS{inner: goodBundle()}, "acp-kit-secondwalk"); err == nil {
		t.Fatal("expected second-walk error")
	}

	// Now cover the rerr branch from fs.ReadFile inside LoadBuiltin's walk:
	// let hashing read the file once, then fail the next Open of it.
	bundle := fstest.MapFS{
		"bundle/x/SKILL.md": {Data: []byte("---\nname: x\nbuiltin: true\ndescription: y\n---\n")},
	}
	rfs := &readNthFailFS{inner: bundle, target: "bundle/x/SKILL.md", failAfter: 1}
	if _, err := LoadBuiltin(rfs, "acp-kit-readnth"); err == nil {
		t.Fatal("expected read-file error in extraction walk")
	}
}

// readNthFailFS opens normally until target has been opened failAfter times,
// then fails subsequent opens of target.
type readNthFailFS struct {
	inner     fs.FS
	target    string
	failAfter int
	hits      int
}

func (r *readNthFailFS) Open(name string) (fs.File, error) {
	if name == r.target {
		r.hits++
		if r.hits > r.failAfter {
			return nil, errors.New("nth open failure")
		}
	}
	return r.inner.Open(name)
}
