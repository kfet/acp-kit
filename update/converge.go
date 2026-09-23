package update

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// A fleet-managed host (Config.Fleet) takes its versions from
// converge/dist.lock. Updating one binary in place would drift the host
// from its lock, so on such a host `!update` runs Config.ConvergeCmd
// instead: the operator's own "resolve newest, commit the lock, apply"
// flow. That command normally reloads the relay itself, so the relay
// cannot simply wait for it:
//
//   - The command runs detached (own session, output to a log file,
//     exit status to an rc file), prefixed by Config.ConvergeWrap so it
//     can leave the relay's cgroup and survive a hard restart.
//   - A marker records the old versions and the old lock.
//   - The old image reports when nothing reloaded; the new image (from
//     Resume) adopts the marker when the job reloaded it. The two never
//     run at once — the reload is an exec in place — so the marker is
//     removed only AFTER the post: an exec between the two costs a
//     duplicate report, never a lost one.

// convergeFiles returns the log, rc and pid paths of the converge job.
func (u *Updater) convergeFiles() (logPath, rcPath, pidPath string) {
	p := filepath.Join(u.cfg.StateDir, "update-converge")
	return p + ".log", p + ".rc", p + ".pid"
}

// convergeGone reports whether the job recorded in the pid file has
// exited. It is how a re-exec'd image, which cannot wait on the job,
// tells a job that died without an rc file from one still running.
func (u *Updater) convergeGone() bool {
	_, _, pidPath := u.convergeFiles()
	b, _ := os.ReadFile(pidPath)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return pid > 0 && pidGone(pid)
}

// converge starts ConvergeCmd while the update lock is held.
// started reports whether the job is running; the watcher then owns the
// update lock and releases it.
func (u *Updater) converge(ctx context.Context, req Request, op Op) (res Result, started bool) {
	var log strings.Builder
	if op.Force {
		if msg, ok := u.drain(ctx, &log); !ok {
			return Result{Text: msg}, false
		}
	}
	logPath, rcPath, pidPath := u.convergeFiles()
	os.Remove(rcPath)
	os.Remove(pidPath)
	lf, err := os.Create(logPath)
	if err != nil {
		return Result{Text: log.String() + "❌ Could not create the converge log: " + err.Error()}, false
	}
	defer lf.Close()
	m := Marker{
		ConvID: req.ConvID, Requester: req.Requester, Who: req.Who,
		OldAgent: u.runningAgent(), OldRelay: u.cfg.RelayVersion, At: u.cfg.Now(),
		Converge: true, OldLock: u.locked(),
	}
	if err := u.writeMarker(m); err != nil {
		return Result{Text: log.String() + "❌ Could not write the update marker: " + err.Error() + ". Nothing was run."}, false
	}
	// The subshell keeps an `exit` inside the command from skipping the
	// rc write.
	script := "echo $$ > " + strconv.Quote(pidPath) + "\n(\n" + u.cfg.ConvergeCmd + "\n)\necho $? > " + strconv.Quote(rcPath) + "\n"
	argv := append(append([]string{}, u.cfg.ConvergeWrap...), "sh", "-c", script)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdout, cmd.Stderr = lf, lf
	cmd.Env = scrubEnv(os.Environ(), u.cfg.SecretEnvNames)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		os.Remove(u.markerPath())
		return Result{Text: log.String() + "❌ Could not start the converge command: " + err.Error()}, false
	}
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()
	exited := func() bool {
		select {
		case <-done:
			return true
		default:
			return false
		}
	}
	wctx := context.WithoutCancel(ctx)
	go func() {
		u.watch(wctx, exited, req.Post)
		u.release()
	}()
	fmt.Fprintf(&log, "🔄 Fleet-managed host: running converge (log `%s`). I'll report here when it is done.", logPath)
	return Result{Text: log.String()}, true
}

// drain cancels every in-flight turn and waits for idle (--force).
func (u *Updater) drain(ctx context.Context, log *strings.Builder) (string, bool) {
	cancelled := u.cfg.CancelAll()
	if len(cancelled) > 0 {
		log.WriteString("🛑 Cancelled turns in: " + strings.Join(cancelled, ", ") + "\n")
	}
	dctx, cancel := context.WithTimeout(ctx, u.cfg.DrainTimeout)
	defer cancel()
	if err := u.cfg.WaitIdle(dctx); err != nil {
		return log.String() + fmt.Sprintf("⚠️ Turns still running %s after cancel — nothing was changed. Retry `!update --force` once idle.", u.cfg.DrainTimeout), false
	}
	return "", true
}

// watch waits for the converge job to finish, then posts the summary
// through post and removes the marker. exited reports whether the job's
// process is gone. A job that exits without an rc file, or runs past
// ConvergeTimeout, is reported as failed. When ctx ends first (this
// image is shutting down) the marker is left for the next image.
func (u *Updater) watch(ctx context.Context, exited func() bool, post func(string) error) {
	_, rcPath, _ := u.convergeFiles()
	deadline := u.cfg.Now().Add(u.cfg.ConvergeTimeout)
	for {
		// Check exit BEFORE the rc file: a job that exits right after
		// the rc check is then still seen with its rc on the next pass.
		gone := exited()
		if _, err := os.Stat(rcPath); err == nil || gone {
			break
		}
		if !u.cfg.Now().Before(deadline) {
			break
		}
		if u.cfg.Sleep(ctx, u.cfg.PollInterval) != nil {
			return
		}
	}
	m, err := readMarker(u.markerPath())
	if err != nil {
		return
	}
	if err := post(u.convergeReport(ctx, m)); err != nil {
		u.logf("update: converge report: %v", err)
	}
	os.Remove(u.markerPath())
}

// convergeReport renders the outcome of a finished converge job.
func (u *Updater) convergeReport(ctx context.Context, m Marker) string {
	logPath, rcPath, _ := u.convergeFiles()
	b, err := os.ReadFile(rcPath)
	rc := strings.TrimSpace(string(b))
	if err != nil || rc != "0" {
		if err != nil {
			rc = "unknown — it did not finish"
		}
		out, _ := os.ReadFile(logPath)
		return fmt.Sprintf("❌ Converge failed (exit %s). Last output:\n%s", rc, fence(string(out)))
	}
	newAgent := "?"
	if u.cfg.AgentBin != "" {
		newAgent = u.cfg.Version(ctx, u.cfg.AgentBin)
	}
	lockDiff := diffLock(m.OldLock, u.locked())
	if newAgent == m.OldAgent && u.cfg.RelayVersion == m.OldRelay && lockDiff == "" {
		return "✅ Already up to date.\n\n" + u.report(ctx)
	}
	text := fmt.Sprintf("✅ Updated via converge: %s %s → %s, %s %s → %s.",
		u.cfg.AgentName, m.OldAgent, newAgent, u.cfg.RelayName, m.OldRelay, u.cfg.RelayVersion)
	if lockDiff != "" {
		text += "\ndist.lock: " + lockDiff + "."
	}
	if m.Who != "" {
		text += " (requested by " + m.Who + ")"
	}
	return text
}

// diffLock renders the changed string entries of a dist.lock, sorted,
// ignoring the resolution timestamp.
func diffLock(before, after map[string]string) string {
	keys := map[string]bool{}
	for k := range before {
		keys[k] = true
	}
	for k := range after {
		keys[k] = true
	}
	var out []string
	for k := range keys {
		if k != "resolved_at" && before[k] != after[k] {
			out = append(out, fmt.Sprintf("%s %s → %s", k, orDash(before[k]), orDash(after[k])))
		}
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

// readMarker reads and parses a marker file.
func readMarker(path string) (Marker, error) {
	var m Marker
	b, err := os.ReadFile(path)
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, fmt.Errorf("update marker: %w", err)
	}
	return m, nil
}

// defaultSleep waits d or until ctx is done.
func defaultSleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// pidGone reports whether pid has exited. After a re-exec in place
// the job is still this process's child, so a finished job is a zombie
// that kill(0) would call alive: reap it first. ECHILD means it is not
// our child, and kill(0) then answers.
func pidGone(pid int) bool {
	var ws syscall.WaitStatus
	if wpid, err := syscall.Wait4(pid, &ws, syscall.WNOHANG, nil); err == nil {
		return wpid == pid
	}
	return syscall.Kill(pid, 0) != nil
}

func (u *Updater) logf(format string, args ...any) {
	if u.cfg.Logf != nil {
		u.cfg.Logf(format, args...)
	}
}

// scrubEnv returns env without the variables named in drop.
func scrubEnv(env, drop []string) []string {
	out := make([]string, 0, len(env))
next:
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		for _, d := range drop {
			if name == d {
				continue next
			}
		}
		out = append(out, kv)
	}
	return out
}
