// Package remotefs provisions filesystem state on the host where the ACP
// agent actually runs.
//
// A relay hands the agent absolute paths over the wire — the session cwd
// (`session/new.cwd`) and any files it stages for a prompt (attachments,
// scratch input). When the agent is a local subprocess those paths are
// trivially valid. When the agent is reached over ssh
// (`--agent-cmd "ssh -T box fir --mode acp"`) they are NOT: the relay
// created the directory on its own disk, and the agent — which takes
// `cwd` as-is, with no stat and no mkdir — silently falls back to $HOME.
// Every session then shares one directory and every staged file is
// missing. Nothing errors; the failure is entirely invisible.
//
// A Provisioner closes that gap. The relay tells it, explicitly, where
// the agent lives; Mkdir and Push then make the paths real over there
// before they are put on the wire.
//
// The relay must be told: an agent command line is an opaque shell
// string and nothing in the ACP handshake reports the agent's host. So
// remoteness is operator configuration, and its absence means Local —
// the pure no-op — which is exactly today's local-subprocess behaviour.
package remotefs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// DefaultTimeout bounds a single provisioning operation when SSH.Timeout
// is zero. Provisioning sits in front of session creation and prompt
// dispatch, so it must fail fast rather than hang a conversation on an
// unreachable box.
const DefaultTimeout = 30 * time.Second

// maxStderr caps how much of a failed command's stderr is quoted back in
// the error. ssh says what matters in its first line ("Permission denied
// (publickey)", "Could not resolve hostname"); the rest is banner noise.
const maxStderr = 4 << 10

// waitDelay bounds how long we keep waiting on a killed command's output
// pipes. ssh(1) can leave the pipe held open after the process itself is
// gone (or, on timeout, by whatever it spawned), and without this the
// "bounded" operation would block on the copy long past its deadline.
const waitDelay = 500 * time.Millisecond

// Provisioner makes relay-side paths exist on the agent's host.
//
// Implementations must be safe for concurrent use: a relay provisions
// one conversation while others are mid-prompt.
type Provisioner interface {
	// Mkdir ensures dir (an absolute path) exists on the agent's host,
	// creating parents as needed. Succeeds if it already exists.
	Mkdir(ctx context.Context, dir string) error

	// Push copies the local directory src onto the agent's host so that
	// it lands at dstParent/<basename(src)>, creating dstParent first.
	// Existing files at the destination are overwritten.
	Push(ctx context.Context, src, dstParent string) error
}

// Local is the Provisioner for an agent running on this machine: the
// relay's own filesystem is the agent's filesystem, so both operations
// are no-ops. It is the correct zero value for "no remote configured".
var Local Provisioner = local{}

type local struct{}

func (local) Mkdir(context.Context, string) error        { return nil }
func (local) Push(context.Context, string, string) error { return nil }

// SSH provisions over ssh(1), driving tar(1) on both ends for Push.
//
// Every local invocation is argv — there is no local shell, so nothing
// the relay computes (a conversation id, an attachment name) can be
// reinterpreted as syntax here. The single remote command string IS
// re-split by the login shell, so paths crossing that boundary are
// single-quoted by ShellQuote.
//
// tar rather than scp deliberately: scp's handling of the remote path
// changed with OpenSSH 9 (legacy shell expansion → SFTP, where the path
// is literal), so no single quoting discipline is correct for both. A
// tar stream piped into one explicit `sh` command is version-independent
// and carries odd filenames intact.
type SSH struct {
	host    string
	timeout time.Duration
}

// hostOK matches ssh destinations we are willing to pass to ssh(1): a
// ~/.ssh/config alias, user@host, an IPv4/IPv6-ish literal. The point is
// not to validate reachability but to reject a "host" that ssh would
// read as an option — `-oProxyCommand=...` is arbitrary local execution
// — and to reject whitespace, which would split into extra argv.
var hostOK = func(s string) bool {
	if s == "" || strings.HasPrefix(s, "-") {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '_', r == '-', r == '@', r == ':', r == '[', r == ']':
		default:
			return false
		}
	}
	return true
}

// ErrBadHost reports a destination that is empty or not a plain ssh
// destination (see hostOK).
var ErrBadHost = errors.New("remotefs: invalid ssh host")

// New returns an SSH Provisioner for the given ssh destination, using
// DefaultTimeout per operation.
func New(host string) (*SSH, error) {
	if !hostOK(host) {
		return nil, fmt.Errorf("%w: %q", ErrBadHost, host)
	}
	return &SSH{host: host, timeout: DefaultTimeout}, nil
}

// WithTimeout returns a copy of s bounding each operation by d. A
// non-positive d selects DefaultTimeout.
func (s *SSH) WithTimeout(d time.Duration) *SSH {
	if d <= 0 {
		d = DefaultTimeout
	}
	return &SSH{host: s.host, timeout: d}
}

// Host reports the ssh destination, for logs and error messages.
func (s *SSH) Host() string { return s.host }

// sshArgv builds the argv for running one remote command string.
// BatchMode turns a missing key into an immediate failure instead of a
// password prompt against a nonexistent tty; ConnectTimeout bounds the
// dial itself so an asleep host does not consume the whole budget in
// TCP retries; "--" stops any host that looks like an option.
func (s *SSH) sshArgv(remote string) []string {
	return []string{
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=10",
		"--", s.host, remote,
	}
}

// Mkdir runs `mkdir -p` on the agent's host.
func (s *SSH) Mkdir(ctx context.Context, dir string) error {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "ssh", s.sshArgv("mkdir -p -- "+ShellQuote(dir))...)
	cmd.WaitDelay = waitDelay
	var stderr strings.Builder
	cmd.Stderr = limitWriter(&stderr, maxStderr)
	if err := cmd.Run(); err != nil {
		return s.opErr(ctx, "mkdir "+dir, err, stderr.String())
	}
	return nil
}

// Push streams src as a tar archive into dstParent on the agent's host.
func (s *SSH) Push(ctx context.Context, src, dstParent string) error {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	remote := "mkdir -p -- " + ShellQuote(dstParent) +
		" && tar -x -f - -C " + ShellQuote(dstParent)

	// -C parent <base>: the archive holds one top-level entry named
	// base, so extraction under dstParent lands it at
	// dstParent/base — the documented Push contract — regardless of
	// how deep src is locally.
	tarCmd := exec.CommandContext(ctx, "tar", "-c", "-f", "-",
		"-C", filepath.Dir(src), filepath.Base(src))
	sshCmd := exec.CommandContext(ctx, "ssh", s.sshArgv(remote)...)
	tarCmd.WaitDelay = waitDelay
	sshCmd.WaitDelay = waitDelay

	pr, pw := io.Pipe()
	tarCmd.Stdout = pw
	sshCmd.Stdin = pr

	var tarErrBuf, sshErrBuf strings.Builder
	tarCmd.Stderr = limitWriter(&tarErrBuf, maxStderr)
	sshCmd.Stderr = limitWriter(&sshErrBuf, maxStderr)

	if err := sshCmd.Start(); err != nil {
		pr.Close()
		pw.Close()
		return s.opErr(ctx, "push "+src, err, sshErrBuf.String())
	}
	tarErr := tarCmd.Run()
	// Closing the write end is what lets the remote tar see EOF and
	// exit; on a local tar failure it also unblocks ssh's stdin read.
	pw.Close()
	sshErr := sshCmd.Wait()
	pr.Close()

	if tarErr != nil {
		return s.opErr(ctx, "push "+src+" (local tar)", tarErr, tarErrBuf.String())
	}
	if sshErr != nil {
		return s.opErr(ctx, "push "+src, sshErr, sshErrBuf.String())
	}
	return nil
}

// opErr renders a failed operation with the host, the underlying error
// and whatever the command said on stderr — the difference between
// "wrong key" and "wrong hostname" is the whole diagnosis, and the relay
// surfaces this text to the user.
func (s *SSH) opErr(ctx context.Context, op string, err error, stderr string) error {
	if ctx.Err() != nil {
		err = fmt.Errorf("%w (timed out after %s)", err, s.timeout)
	}
	if msg := strings.TrimSpace(stderr); msg != "" {
		return fmt.Errorf("remotefs: %s on %s: %w: %s", op, s.host, err, msg)
	}
	return fmt.Errorf("remotefs: %s on %s: %w", op, s.host, err)
}

// ShellQuote wraps s in single quotes for a POSIX remote login shell,
// escaping embedded single quotes the only way sh allows: end the
// quoted run, emit an escaped quote, start a new run.
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// limitWriter returns a writer that forwards at most n bytes to w and
// silently discards the rest, so a chatty command cannot balloon an
// error message.
func limitWriter(w io.Writer, n int) io.Writer { return &capped{w: w, left: n} }

type capped struct {
	w    io.Writer
	left int
}

func (c *capped) Write(p []byte) (int, error) {
	if c.left <= 0 {
		return len(p), nil
	}
	keep := p
	if len(keep) > c.left {
		keep = keep[:c.left]
	}
	c.left -= len(keep)
	if _, err := c.w.Write(keep); err != nil {
		return 0, err
	}
	return len(p), nil
}
