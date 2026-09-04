package remotefs

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stubDir builds a directory of executable stubs and puts it on PATH as
// the ONLY entry, so every exec.Command in the package resolves to a
// script we control. Returns the directory (stubs write their argv and
// stdin there for assertions).
func stubDir(t *testing.T, scripts map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range scripts {
		p := filepath.Join(dir, name)
		// The stubs themselves need coreutils, but PATH is about to be
		// narrowed to this directory so the package under test can only
		// resolve the stubs; give the scripts their own PATH back.
		script := "#!/bin/sh\nPATH=/usr/bin:/bin:/usr/sbin:/sbin\nexport PATH\n" + body
		if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
			t.Fatalf("write stub %s: %v", name, err)
		}
	}
	t.Setenv("PATH", dir)
	return dir
}

// recordSSH is a stub ssh that saves its argv one-per-line and drains
// stdin into a file, then succeeds.
const recordSSH = `
: > "$0.argv"
for a in "$@"; do printf '%s\n' "$a" >> "$0.argv"; done
cat > "$0.stdin"
`

func readLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"/plain/path":      `'/plain/path'`,
		"with space":       `'with space'`,
		"$HOME`id`;rm -rf": "'$HOME`id`;rm -rf'",
		"it's":             `'it'\''s'`,
		"":                 `''`,
		"a\nb":             "'a\nb'",
	}
	for in, want := range cases {
		if got := ShellQuote(in); got != want {
			t.Errorf("ShellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNewRejectsNonHosts(t *testing.T) {
	bad := []string{
		"", "-oProxyCommand=id", "miki box", "a;b", "a\tb", "host$x", "-",
	}
	for _, h := range bad {
		if _, err := New(h); !errors.Is(err, ErrBadHost) {
			t.Errorf("New(%q) err = %v, want ErrBadHost", h, err)
		}
	}
	for _, h := range []string{"miki", "kfet@10.0.0.4", "box.local", "[fe80::1]:22", "a_b-c"} {
		s, err := New(h)
		if err != nil {
			t.Fatalf("New(%q): %v", h, err)
		}
		if s.Host() != h {
			t.Errorf("Host() = %q, want %q", s.Host(), h)
		}
		if s.timeout != DefaultTimeout {
			t.Errorf("timeout = %v, want %v", s.timeout, DefaultTimeout)
		}
	}
}

func TestWithTimeout(t *testing.T) {
	s, err := New("miki")
	if err != nil {
		t.Fatal(err)
	}
	if got := s.WithTimeout(5 * time.Second).timeout; got != 5*time.Second {
		t.Errorf("timeout = %v, want 5s", got)
	}
	if got := s.WithTimeout(0).timeout; got != DefaultTimeout {
		t.Errorf("timeout = %v, want default", got)
	}
	if got := s.WithTimeout(-1).timeout; got != DefaultTimeout {
		t.Errorf("timeout = %v, want default", got)
	}
	if s.timeout != DefaultTimeout {
		t.Errorf("receiver mutated: %v", s.timeout)
	}
}

func TestLocalIsNoOp(t *testing.T) {
	// PATH holds nothing: a Local that shelled out would fail here.
	stubDir(t, nil)
	if err := Local.Mkdir(t.Context(), "/nope"); err != nil {
		t.Errorf("Local.Mkdir: %v", err)
	}
	if err := Local.Push(t.Context(), "/nope", "/nope"); err != nil {
		t.Errorf("Local.Push: %v", err)
	}
}

func TestMkdirArgv(t *testing.T) {
	dir := stubDir(t, map[string]string{"ssh": recordSSH})
	s, err := New("miki")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Mkdir(t.Context(), "/home/kfet/convs/it's here"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	want := []string{
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=10",
		"--", "miki",
		`mkdir -p -- '/home/kfet/convs/it'\''s here'`,
	}
	got := readLines(t, filepath.Join(dir, "ssh.argv"))
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("argv =\n%q\nwant\n%q", got, want)
	}
}

func TestMkdirFailureQuotesStderr(t *testing.T) {
	stubDir(t, map[string]string{
		"ssh": "echo 'Permission denied (publickey).' >&2\nexit 255\n",
	})
	s, _ := New("miki")
	err := s.Mkdir(t.Context(), "/home/kfet/x")
	if err == nil {
		t.Fatal("want error")
	}
	for _, want := range []string{"mkdir /home/kfet/x", "on miki", "Permission denied (publickey)."} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

func TestMkdirFailureWithoutStderr(t *testing.T) {
	stubDir(t, map[string]string{"ssh": "exit 3\n"})
	s, _ := New("miki")
	err := s.Mkdir(t.Context(), "/x")
	if err == nil || !strings.Contains(err.Error(), "remotefs: mkdir /x on miki") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), ": : ") {
		t.Errorf("empty stderr should not be appended: %v", err)
	}
}

func TestMkdirTimeoutIsNamed(t *testing.T) {
	stubDir(t, map[string]string{"ssh": "sleep 30\n"})
	s, _ := New("miki")
	err := s.WithTimeout(50*time.Millisecond).Mkdir(t.Context(), "/x")
	if err == nil {
		t.Fatal("want error")
	}
	if !strings.Contains(err.Error(), "timed out after 50ms") {
		t.Errorf("error %q does not name the timeout", err)
	}
}

func TestMkdirSSHMissing(t *testing.T) {
	stubDir(t, nil)
	s, _ := New("miki")
	if err := s.Mkdir(t.Context(), "/x"); err == nil {
		t.Fatal("want error when ssh is not on PATH")
	}
}

// pushSrc creates <tmp>/msg-1/report.txt and returns the msg-1 dir.
func pushSrc(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	src := filepath.Join(base, "msg-1")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "report.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	return src
}

func TestPushStreamsTarAndBuildsRemoteCommand(t *testing.T) {
	src := pushSrc(t)
	realTar, err := exec.LookPath("tar")
	if err != nil {
		t.Skipf("tar not available: %v", err)
	}
	dir := stubDir(t, map[string]string{"ssh": recordSSH})
	if err := os.Symlink(realTar, filepath.Join(dir, "tar")); err != nil {
		t.Fatal(err)
	}

	s, _ := New("miki")
	if err := s.Push(t.Context(), src, "/home/kfet/convs/c1/.poe-attachments"); err != nil {
		t.Fatalf("Push: %v", err)
	}

	argv := readLines(t, filepath.Join(dir, "ssh.argv"))
	remote := argv[len(argv)-1]
	want := `mkdir -p -- '/home/kfet/convs/c1/.poe-attachments' && tar -x -f - -C '/home/kfet/convs/c1/.poe-attachments'`
	if remote != want {
		t.Errorf("remote command = %q, want %q", remote, want)
	}

	// The stream ssh received must be a tar whose single top-level
	// entry is the source's basename — that is what makes the file
	// land at dstParent/msg-1/report.txt on the far side.
	out, terr := exec.Command(realTar, "-t", "-f", filepath.Join(dir, "ssh.stdin")).Output()
	if terr != nil {
		t.Fatalf("tar -t: %v", terr)
	}
	if !strings.Contains(string(out), "msg-1/report.txt") {
		t.Errorf("tar stream listing = %q, want msg-1/report.txt", out)
	}
}

func TestPushSSHStartFails(t *testing.T) {
	src := pushSrc(t)
	stubDir(t, map[string]string{"tar": "exit 0\n"}) // no ssh
	s, _ := New("miki")
	err := s.Push(t.Context(), src, "/dst")
	if err == nil || !strings.Contains(err.Error(), "push "+src) {
		t.Fatalf("err = %v", err)
	}
}

func TestPushLocalTarFails(t *testing.T) {
	src := pushSrc(t)
	stubDir(t, map[string]string{
		"ssh": recordSSH,
		"tar": "echo 'tar: no such file' >&2\nexit 2\n",
	})
	s, _ := New("miki")
	err := s.Push(t.Context(), src, "/dst")
	if err == nil {
		t.Fatal("want error")
	}
	if !strings.Contains(err.Error(), "(local tar)") || !strings.Contains(err.Error(), "tar: no such file") {
		t.Errorf("err = %v", err)
	}
}

func TestPushRemoteFails(t *testing.T) {
	src := pushSrc(t)
	realTar, err := exec.LookPath("tar")
	if err != nil {
		t.Skipf("tar not available: %v", err)
	}
	dir := stubDir(t, map[string]string{
		"ssh": "cat > /dev/null\necho 'tar: cannot open' >&2\nexit 2\n",
	})
	if err := os.Symlink(realTar, filepath.Join(dir, "tar")); err != nil {
		t.Fatal(err)
	}
	s, _ := New("miki")
	perr := s.Push(t.Context(), src, "/dst")
	if perr == nil {
		t.Fatal("want error")
	}
	if !strings.Contains(perr.Error(), "tar: cannot open") || strings.Contains(perr.Error(), "local tar") {
		t.Errorf("err = %v", perr)
	}
}

type errWriter struct{ err error }

func (e errWriter) Write([]byte) (int, error) { return 0, e.err }

func TestCappedWriter(t *testing.T) {
	var sb strings.Builder
	w := limitWriter(&sb, 4)
	n, err := w.Write([]byte("abcdef"))
	if n != 6 || err != nil {
		t.Fatalf("Write = %d, %v", n, err)
	}
	// Past the cap the writer keeps accepting and keeps discarding.
	if n, err = w.Write([]byte("ghi")); n != 3 || err != nil {
		t.Fatalf("Write past cap = %d, %v", n, err)
	}
	if sb.String() != "abcd" {
		t.Errorf("kept %q, want %q", sb.String(), "abcd")
	}

	boom := errors.New("boom")
	if _, err := limitWriter(errWriter{boom}, 8).Write([]byte("x")); !errors.Is(err, boom) {
		t.Errorf("err = %v, want boom", err)
	}
}

var _ Provisioner = (*SSH)(nil)

// TestPushRemoteExitsWithoutReading is the regression for the pipe
// deadlock: ssh dies before draining stdin, so the local tar must see
// EPIPE and the whole Push must return rather than block forever on a
// write nobody will read.
func TestPushRemoteExitsWithoutReading(t *testing.T) {
	src := pushSrcBig(t)
	realTar, err := exec.LookPath("tar")
	if err != nil {
		t.Skipf("tar not available: %v", err)
	}
	dir := stubDir(t, map[string]string{
		"ssh": "echo 'mkdir: Read-only file system' >&2\nexit 1\n",
	})
	if err := os.Symlink(realTar, filepath.Join(dir, "tar")); err != nil {
		t.Fatal(err)
	}
	s, _ := New("miki")
	// A generous per-op timeout: if this test only passes because the
	// deadline fired, the deadlock is still there.
	done := make(chan error, 1)
	go func() { done <- s.WithTimeout(time.Minute).Push(t.Context(), src, "/dst") }()
	select {
	case perr := <-done:
		if perr == nil {
			t.Fatal("want error")
		}
		if !strings.Contains(perr.Error(), "Read-only file system") {
			t.Errorf("err = %v, want the remote's own stderr", perr)
		}
		if strings.Contains(perr.Error(), "local tar") {
			t.Errorf("err = %v, want ssh's failure to win over tar's EPIPE", perr)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Push blocked: the tar->ssh pipe deadlocked")
	}
}

// pushSrcBig makes a source big enough that tar cannot finish writing
// into the pipe buffer before ssh exits.
func pushSrcBig(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	src := filepath.Join(base, "msg-1")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	blob := make([]byte, 4<<20)
	if err := os.WriteFile(filepath.Join(src, "big.bin"), blob, 0o644); err != nil {
		t.Fatal(err)
	}
	return src
}

func TestPushLocalTarStartFails(t *testing.T) {
	src := pushSrc(t)
	stubDir(t, map[string]string{"ssh": recordSSH}) // no tar
	s, _ := New("miki")
	err := s.Push(t.Context(), src, "/dst")
	if err == nil || !strings.Contains(err.Error(), "local tar") {
		t.Fatalf("err = %v", err)
	}
}

func TestLocalFetchIsIdentity(t *testing.T) {
	stubDir(t, nil)
	got, err := Local.Fetch(t.Context(), "/on/agent/f.txt", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got != "/on/agent/f.txt" {
		t.Errorf("Fetch = %q, want the path unchanged", got)
	}
}

func TestFetchCopiesBack(t *testing.T) {
	realTar, err := exec.LookPath("tar")
	if err != nil {
		t.Skipf("tar not available: %v", err)
	}
	// The stub ssh runs the remote command locally, which is exactly
	// what a `tar -c` on the far side would produce.
	dir := stubDir(t, map[string]string{
		"ssh": `: > "$0.argv"
for a in "$@"; do printf '%s\n' "$a" >> "$0.argv"; done
eval "$7"
`,
	})
	if err := os.Symlink(realTar, filepath.Join(dir, "tar")); err != nil {
		t.Fatal(err)
	}
	remote := filepath.Join(t.TempDir(), "out.txt")
	if err := os.WriteFile(remote, []byte("delivered"), 0o644); err != nil {
		t.Fatal(err)
	}

	dst := t.TempDir()
	s, _ := New("miki")
	local, ferr := s.Fetch(t.Context(), remote, dst)
	if ferr != nil {
		t.Fatalf("Fetch: %v", ferr)
	}
	if want := filepath.Join(dst, "out.txt"); local != want {
		t.Fatalf("Fetch = %q, want %q", local, want)
	}
	b, rerr := os.ReadFile(local)
	if rerr != nil || string(b) != "delivered" {
		t.Fatalf("content = %q err=%v", b, rerr)
	}
	argv := readLines(t, filepath.Join(dir, "ssh.argv"))
	want := `tar -c -f - -C ` + ShellQuote(filepath.Dir(remote)) + ` -- 'out.txt'`
	if argv[len(argv)-1] != want {
		t.Errorf("remote command = %q, want %q", argv[len(argv)-1], want)
	}
}

func TestFetchRemoteFails(t *testing.T) {
	realTar, err := exec.LookPath("tar")
	if err != nil {
		t.Skipf("tar not available: %v", err)
	}
	dir := stubDir(t, map[string]string{
		"ssh": "echo 'tar: no such file' >&2\nexit 2\n",
	})
	if err := os.Symlink(realTar, filepath.Join(dir, "tar")); err != nil {
		t.Fatal(err)
	}
	s, _ := New("miki")
	_, ferr := s.Fetch(t.Context(), "/gone/x.txt", t.TempDir())
	if ferr == nil || !strings.Contains(ferr.Error(), "tar: no such file") {
		t.Fatalf("err = %v", ferr)
	}
	if strings.Contains(ferr.Error(), "local tar") {
		t.Errorf("remote failure must win: %v", ferr)
	}
}

func TestFetchSSHStartFails(t *testing.T) {
	stubDir(t, map[string]string{"tar": "exit 0\n"}) // no ssh
	s, _ := New("miki")
	if _, err := s.Fetch(t.Context(), "/x", t.TempDir()); err == nil {
		t.Fatal("want error when ssh is not on PATH")
	}
}

func TestFetchLocalTarStartFails(t *testing.T) {
	stubDir(t, map[string]string{"ssh": "exit 0\n"}) // no tar
	s, _ := New("miki")
	_, err := s.Fetch(t.Context(), "/x", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "local tar") {
		t.Fatalf("err = %v", err)
	}
}

func TestFetchLocalTarFails(t *testing.T) {
	stubDir(t, map[string]string{
		"ssh": "exit 0\n",
		"tar": "cat > /dev/null\necho 'tar: bad archive' >&2\nexit 2\n",
	})
	s, _ := New("miki")
	_, err := s.Fetch(t.Context(), "/x", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "tar: bad archive") {
		t.Fatalf("err = %v", err)
	}
}
