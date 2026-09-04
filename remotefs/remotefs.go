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

// DefaultTimeout bounds a single control operation (Mkdir) unless
// WithTimeout says otherwise. Provisioning sits in front of session creation and prompt
// dispatch, so it must fail fast rather than hang a conversation on an
// unreachable box.
const DefaultTimeout = 30 * time.Second

// DefaultTransferTimeout bounds Push and Fetch unless
// WithTransferTimeout says otherwise. Copying an attachment is not a control operation: a hundred
// megabytes over a domestic uplink is minutes, and holding it to the
// same deadline as a mkdir would fail perfectly healthy transfers.
const DefaultTransferTimeout = 5 * time.Minute

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

	// Fetch makes a path on the agent's host readable HERE and returns
	// the local path to read. It is the direction the agent produces
	// in: a file the agent wrote (an attachment it wants delivered)
	// lives on its disk, not the relay's.
	//
	// dstDir is a caller-owned scratch directory to copy into; the
	// caller removes it. A local agent copies nothing and returns
	// remotePath unchanged, so callers must always use the returned
	// path and never assume dstDir was touched.
	Fetch(ctx context.Context, remotePath, dstDir string) (string, error)
}

// Local is the Provisioner for an agent running on this machine: the
// relay's own filesystem is the agent's filesystem, so Mkdir and Push
// do nothing and Fetch is the identity. It is the correct value for
// "no remote configured".
var Local Provisioner = local{}

type local struct{}

func (local) Mkdir(context.Context, string) error        { return nil }
func (local) Push(context.Context, string, string) error { return nil }

// Fetch on a local agent is the identity: the file is already here.
func (local) Fetch(_ context.Context, remotePath, _ string) (string, error) {
	return remotePath, nil
}

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
	xfer    time.Duration
}

// hostOK matches ssh destinations we are willing to pass to ssh(1): a
// ~/.ssh/config alias, user@host, an IPv4/IPv6-ish literal. The point is
// not to validate reachability but to reject a "host" that ssh would
// read as an option — `-oProxyCommand=...` is arbitrary local execution
// — and to reject whitespace, which would split into extra argv.
func hostOK(s string) bool {
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
	return &SSH{host: host, timeout: DefaultTimeout, xfer: DefaultTransferTimeout}, nil
}

// WithTimeout returns a copy of s bounding each control operation
// (Mkdir) by d. A non-positive d selects DefaultTimeout.
func (s *SSH) WithTimeout(d time.Duration) *SSH {
	if d <= 0 {
		d = DefaultTimeout
	}
	return &SSH{host: s.host, timeout: d, xfer: s.xfer}
}

// WithTransferTimeout returns a copy of s bounding each byte-moving
// operation (Push, Fetch) by d. A non-positive d selects
// DefaultTransferTimeout.
func (s *SSH) WithTransferTimeout(d time.Duration) *SSH {
	if d <= 0 {
		d = DefaultTransferTimeout
	}
	return &SSH{host: s.host, timeout: s.timeout, xfer: d}
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
		return s.opErr(ctx, s.timeout, "mkdir "+dir, err, stderr.String())
	}
	return nil
}

// Push streams src as a tar archive into dstParent on the agent's host.
func (s *SSH) Push(ctx context.Context, src, dstParent string) error {
	ctx, cancel := context.WithTimeout(ctx, s.xfer)
	defer cancel()

	remote := "mkdir -p -- " + ShellQuote(dstParent) +
		" && tar -x -f - -C " + ShellQuote(dstParent)

	// -C parent <base>: the archive holds one top-level entry named
	// base, so extraction under dstParent lands it at dstParent/base —
	// the documented Push contract — regardless of how deep src is
	// locally.
	tarCmd := exec.CommandContext(ctx, "tar", "-c", "-f", "-",
		"-C", filepath.Dir(src), filepath.Base(src))
	sshCmd := exec.CommandContext(ctx, "ssh", s.sshArgv(remote)...)

	// The two processes are joined by an OS pipe, not an io.Pipe: ssh
	// must be able to die early — refused mkdir, auth failure after the
	// banner — and have that reach tar as EPIPE. An io.Pipe would leave
	// os/exec's copier goroutine blocked forever on a write no one will
	// read, and WaitDelay cannot break it because the block is in the
	// io.Pipe, not in a file descriptor.
	rd := mustPipe(tarCmd.StdoutPipe())
	sshCmd.Stdin = rd

	var tarErrBuf, sshErrBuf strings.Builder
	tarCmd.Stderr = limitWriter(&tarErrBuf, maxStderr)
	sshCmd.Stderr = limitWriter(&sshErrBuf, maxStderr)
	tarCmd.WaitDelay = waitDelay
	sshCmd.WaitDelay = waitDelay

	if err := tarCmd.Start(); err != nil {
		return s.opErr(ctx, s.xfer, "push "+src+" (local tar)", err, tarErrBuf.String())
	}
	if err := sshCmd.Start(); err != nil {
		// Drop the read end BEFORE waiting: tar is already writing, and
		// with this process still holding the pipe it would block on a
		// full buffer until the transfer deadline instead of taking an
		// immediate EPIPE.
		rd.Close()
		_ = tarCmd.Wait()
		return s.opErr(ctx, s.xfer, "push "+src, err, sshErrBuf.String())
	}
	// ssh holds the read end now; drop ours so tar sees EPIPE the
	// moment ssh exits, instead of writing into a pipe kept alive by
	// this process.
	rd.Close()

	sshErr := sshCmd.Wait()
	tarErr := tarCmd.Wait()

	// ssh first when both failed: a dead ssh gives tar an EPIPE that
	// says nothing, while ssh's own stderr names the actual cause. The
	// converse also happens — a local tar that dies mid-stream makes
	// the REMOTE tar fail on a truncated archive — so when both spoke,
	// both are quoted.
	if sshErr != nil {
		return s.opErr(ctx, s.xfer, "push "+src, sshErr, withLocal(sshErrBuf.String(), tarErrBuf.String()))
	}
	if tarErr != nil {
		return s.opErr(ctx, s.xfer, "push "+src+" (local tar)", tarErr, tarErrBuf.String())
	}
	return nil
}

// Fetch copies remotePath from the agent's host into dstDir and returns
// the local path, dstDir/<basename(remotePath)>.
func (s *SSH) Fetch(ctx context.Context, remotePath, dstDir string) (string, error) {
	remotePath = filepath.Clean(remotePath)
	ctx, cancel := context.WithTimeout(ctx, s.xfer)
	defer cancel()

	// -h: the agent may well have written through a symlink, and a link
	// entry would extract here as a dangling one pointing into a
	// filesystem this machine does not have.
	remote := "tar -c -h -f - -C " + ShellQuote(filepath.Dir(remotePath)) +
		" -- " + ShellQuote(filepath.Base(remotePath))
	sshCmd := exec.CommandContext(ctx, "ssh", s.sshArgv(remote)...)
	tarCmd := exec.CommandContext(ctx, "tar", "-x", "-f", "-", "-C", dstDir)

	rd := mustPipe(sshCmd.StdoutPipe())
	tarCmd.Stdin = rd

	var sshErrBuf, tarErrBuf strings.Builder
	sshCmd.Stderr = limitWriter(&sshErrBuf, maxStderr)
	tarCmd.Stderr = limitWriter(&tarErrBuf, maxStderr)
	sshCmd.WaitDelay = waitDelay
	tarCmd.WaitDelay = waitDelay

	if err := sshCmd.Start(); err != nil {
		return "", s.opErr(ctx, s.xfer, "fetch "+remotePath, err, sshErrBuf.String())
	}
	if err := tarCmd.Start(); err != nil {
		// Same reason as Push: release the pipe so ssh is not left
		// blocked writing into it.
		rd.Close()
		_ = sshCmd.Wait()
		return "", s.opErr(ctx, s.xfer, "fetch "+remotePath+" (local tar)", err, tarErrBuf.String())
	}
	rd.Close()

	tarErr := tarCmd.Wait()
	sshErr := sshCmd.Wait()

	// ssh first again: local tar failing on a truncated stream is a
	// symptom of the remote side having failed — but if the local tar
	// also had something to say, say it.
	if sshErr != nil {
		return "", s.opErr(ctx, s.xfer, "fetch "+remotePath, sshErr, withLocal(sshErrBuf.String(), tarErrBuf.String()))
	}
	if tarErr != nil {
		return "", s.opErr(ctx, s.xfer, "fetch "+remotePath+" (local tar)", tarErr, tarErrBuf.String())
	}
	return filepath.Join(dstDir, filepath.Base(remotePath)), nil
}

// opErr renders a failed operation with the host, the underlying error
// and whatever the command said on stderr — the difference between
// "wrong key" and "wrong hostname" is the whole diagnosis, and the relay
// surfaces this text to the user.
func (s *SSH) opErr(ctx context.Context, bound time.Duration, op string, err error, stderr string) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		err = fmt.Errorf("%w (timed out after %s)", err, bound)
	}
	if msg := strings.TrimSpace(stderr); msg != "" {
		return fmt.Errorf("remotefs: %s on %s: %w: %s", op, s.host, err, msg)
	}
	return fmt.Errorf("remotefs: %s on %s: %w", op, s.host, err)
}

// withLocal appends the local tar's complaint to the remote's, so a
// failure caused on THIS side is not reported purely in the far side's
// words ("this does not look like a tar archive").
func withLocal(remote, local string) string {
	local = strings.TrimSpace(local)
	if local == "" {
		return remote
	}
	return strings.TrimSpace(remote) + " (local tar: " + local + ")"
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
