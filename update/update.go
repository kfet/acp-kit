// Package update implements the `!update` chat command shared by every
// relay: update the agent binary (fir) and/or the relay binary on disk,
// then trigger the relay's graceful SIGHUP reload so the new images are
// actually running.
//
// A relay holds ONE long-lived agent process, so `fir update` alone
// changes nothing that is running — only the relay's re-exec (which
// respawns the agent) picks up the new binary. That is why every
// successful update here ends in exactly one reload, never a hard
// restart: a restart loses the messages queued while the process is
// down, a graceful reload does not.
//
// Surface:
//
//	!update                  agent + relay, one reload
//	!update fir              agent only, then reload
//	!update relay            relay only, then reload
//	!update fir --rollback   restore fir.prev, then reload
//	!update --check          report versions; changes nothing
//	... --force              cancel in-flight turns first
//
// On a fleet-managed host (Config.Fleet) every update form except
// --check runs Config.ConvergeCmd instead — see converge.go.
//
// Everything relay-specific is a hook in Config (UpdateAgent,
// UpdateSelf, CancelAll, WaitIdle, Reload…). The relay decides WHO is
// asking (Request.Requester) and delivers Result.Text before calling
// Result.After, so the reply is posted before the reload starts.
//
// Across the reload the outcome is reported by a marker file written
// just before it: the new image calls Resume at startup, which posts
// "fir X → Y, relay A → B" into the requesting conversation and deletes
// the marker.
package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Config wires an Updater to one relay.
type Config struct {
	// RelayName is the relay's binary/display name, e.g. "zulip-acp".
	RelayName string
	// RelayVersion is the compiled-in version of the running relay.
	RelayVersion string
	// RelayBin is the relay binary on disk. Default: os.Executable.
	RelayBin string
	// AgentName is the agent's display name. Default "fir".
	AgentName string
	// AgentBin is the resolved agent binary on disk. Required for
	// anything touching the agent (including --rollback's fir.prev).
	AgentBin string

	// Owners lists the requester ids allowed to run !update (including
	// --check). Empty means nobody.
	Owners []string
	// StateDir holds the lock file and the reload marker.
	StateDir string

	// Fleet marks a host managed by converge/dist.lock. An update there
	// runs ConvergeCmd (a shell command: resolve the newest versions,
	// commit the lock, apply it) instead of updating binaries in place,
	// which would drift the host from its lock. With no ConvergeCmd a
	// fleet host refuses `!update`. LockFile, when set, is the
	// dist.lock to compare against.
	Fleet       bool
	LockFile    string
	ConvergeCmd string
	// ConvergeWrap is an argv prefix for the converge job, e.g.
	// `systemd-run --user --scope --quiet` so the job leaves the relay's
	// cgroup and survives a hard restart of the relay's unit.
	ConvergeWrap []string
	// ConvergeTimeout bounds how long the job is watched. Default 30m.
	// PollInterval is how often its rc file is checked. Default 2s.
	ConvergeTimeout time.Duration
	PollInterval    time.Duration
	// Sleep waits d or until ctx is done. Default: a timer.
	Sleep func(ctx context.Context, d time.Duration) error
	// SecretEnvNames are dropped from the converge job's environment:
	// it needs none of the relay's credentials.
	SecretEnvNames []string
	// Logf logs errors that have no reply to go to. Optional.
	Logf func(format string, args ...any)
	// RelayLockKey / AgentLockKey are the dist.lock keys. Defaults:
	// RelayName with "-" → "_", and AgentName.
	RelayLockKey string
	AgentLockKey string

	// UpdateAgent and UpdateSelf perform the on-disk updates and
	// return their combined output. Required.
	UpdateAgent func(ctx context.Context) (string, error)
	UpdateSelf  func(ctx context.Context) (string, error)

	// AgentVersion reports the RUNNING agent's version (e.g. from the
	// ACP initialize agentInfo). Optional.
	AgentVersion func() string
	// AgentPID returns the running agent's pid (0 if none). Optional.
	AgentPID func() int

	// CancelAll cancels every in-flight turn and returns a human label
	// per cancelled conversation. WaitIdle blocks until no turn is in
	// flight or ctx is done. Both are needed for --force.
	CancelAll func() []string
	WaitIdle  func(ctx context.Context) error
	// DrainTimeout bounds WaitIdle after CancelAll. Default 30s.
	DrainTimeout time.Duration

	// Reload triggers the graceful reload. Default: SIGHUP to self.
	Reload func() error
	// Version returns the version printed by bin. Default runs
	// `bin --version` and takes the last word of the first line.
	Version func(ctx context.Context, bin string) string
	// ProcExe resolves a pid's executable. Default reads /proc/<pid>/exe.
	ProcExe func(pid int) string
	// Now is the clock. Default time.Now.
	Now func() time.Time
}

// Request is one `!update` invocation.
type Request struct {
	ConvID    string // opaque relay conversation token, echoed by Resume
	Requester string // the relay's id for the sender, matched against Owners
	Who       string // display name for the report; optional
	Text      string // the raw message
	// Post posts into the requesting conversation after Handle has
	// returned. A fleet host's converge job reports through it when it
	// ends without a reload.
	Post func(text string) error
}

// Result is what the relay renders. When After is non-nil the relay
// must post Text first and then call After, which triggers the reload.
type Result struct {
	Text  string
	After func() error
}

// Updater runs `!update` for one relay.
type Updater struct {
	cfg Config

	mu   sync.Mutex
	lock *os.File
}

// markerTTL bounds how old a marker Resume will still report: a marker
// from a reload that never happened must not surface hours later.
const markerTTL = time.Hour

// HelpLine is the `!help` bullet for this command.
const HelpLine = "- `!update [fir|relay] [--check|--force|--rollback]` — owner only: update fir and/or the relay, then reload gracefully (a fleet host runs its converge command)\n"

// New constructs an Updater, filling defaults.
func New(cfg Config) *Updater {
	if cfg.AgentName == "" {
		cfg.AgentName = "fir"
	}
	if cfg.RelayBin == "" {
		cfg.RelayBin, _ = os.Executable()
	}
	if p, err := filepath.EvalSymlinks(cfg.AgentBin); cfg.AgentBin != "" && err == nil {
		// Swap the real file, never the symlink pointing at it: replacing
		// a link with a regular file would orphan a managed install.
		cfg.AgentBin = p
	}
	if cfg.RelayLockKey == "" {
		cfg.RelayLockKey = strings.ReplaceAll(cfg.RelayName, "-", "_")
	}
	if cfg.AgentLockKey == "" {
		cfg.AgentLockKey = cfg.AgentName
	}
	if cfg.DrainTimeout == 0 {
		cfg.DrainTimeout = 30 * time.Second
	}
	if cfg.Reload == nil {
		cfg.Reload = func() error { return syscall.Kill(os.Getpid(), syscall.SIGHUP) }
	}
	if cfg.Version == nil {
		cfg.Version = BinVersion
	}
	if cfg.ProcExe == nil {
		cfg.ProcExe = func(pid int) string {
			p, _ := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
			return p
		}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.ConvergeTimeout == 0 {
		cfg.ConvergeTimeout = 30 * time.Minute
	}
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 2 * time.Second
	}
	if cfg.Sleep == nil {
		cfg.Sleep = defaultSleep
	}
	return &Updater{cfg: cfg}
}

// CommandHook returns an UpdateAgent/UpdateSelf hook that runs bin with
// args and returns its combined output.
func CommandHook(bin string, args ...string) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		out, err := exec.CommandContext(ctx, bin, args...).CombinedOutput()
		return string(out), err
	}
}

// BinVersion runs `bin --version` and returns the last word of its
// first line ("fir 1.18.4" → "1.18.4", "0.36.0" → "0.36.0"), or "?"
// when it cannot be run.
func BinVersion(ctx context.Context, bin string) string {
	out, err := exec.CommandContext(ctx, bin, "--version").Output()
	if err != nil {
		return "?"
	}
	line, _, _ := strings.Cut(string(out), "\n")
	f := strings.Fields(line)
	if len(f) == 0 {
		return "?"
	}
	return strings.TrimPrefix(f[len(f)-1], "v")
}

// Op is a parsed `!update` invocation.
type Op struct {
	Agent, Relay bool
	Check        bool
	Force        bool
	Rollback     bool
}

// IsCommand reports whether text is an `!update` command (any sigil,
// any case on the verb).
func IsCommand(text string) bool {
	t := strings.TrimSpace(text)
	if t == "" || !strings.ContainsRune("/!.", rune(t[0])) {
		return false
	}
	f := strings.Fields(t[1:])
	return len(f) > 0 && strings.EqualFold(f[0], "update")
}

// Parse parses the text of an `!update` command. The caller has
// already established IsCommand(text).
func Parse(text string) (Op, error) {
	var op Op
	var targets int
	f := strings.Fields(strings.TrimSpace(text))
	if len(f) > 0 {
		f = f[1:]
	}
	for _, a := range f {
		switch strings.ToLower(a) {
		case "fir", "agent":
			op.Agent = true
			targets++
		case "relay", "self":
			op.Relay = true
			targets++
		case "--check", "-check", "check":
			op.Check = true
		case "--force", "-force":
			op.Force = true
		case "--rollback", "-rollback", "rollback":
			op.Rollback = true
		default:
			if strings.ContainsAny(a, "0123456789") {
				return op, fmt.Errorf("version pins are not accepted from chat (%q) — `!update` always takes the latest release", a)
			}
			return op, fmt.Errorf("unknown argument %q", a)
		}
	}
	if targets == 0 {
		op.Agent, op.Relay = true, !op.Rollback
	}
	if op.Rollback && op.Relay {
		return op, errors.New("--rollback applies to fir only: `!update fir --rollback`")
	}
	if op.Check && (op.Force || op.Rollback) {
		return op, errors.New("--check changes nothing and takes no other flag")
	}
	return op, nil
}

const usage = "Usage: `!update [fir|relay] [--check|--force]`, or `!update fir --rollback`."

// Handle runs one `!update`.
func (u *Updater) Handle(ctx context.Context, req Request) Result {
	op, err := Parse(req.Text)
	if err != nil {
		return Result{Text: "❌ " + err.Error() + "\n\n" + usage}
	}
	if !u.owner(req.Requester) {
		return Result{Text: "⛔ `!update` is owner-only on this relay."}
	}
	if op.Check {
		return Result{Text: u.report(ctx)}
	}
	if op.Force && (u.cfg.CancelAll == nil || u.cfg.WaitIdle == nil) {
		return Result{Text: "❌ `--force` is not supported by this relay (it cannot cancel turns)."}
	}
	if u.cfg.Fleet {
		switch {
		case op.Rollback:
			return Result{Text: "⛔ This host is fleet-managed (converge/dist.lock): a rollback would drift it " +
				"from its lock. Pin the older version in dist.lock and run `!update`.\n\n" + u.report(ctx)}
		case u.cfg.ConvergeCmd == "":
			return Result{Text: "⛔ This host is fleet-managed (converge/dist.lock) and no converge command " +
				"is configured, so `!update` cannot run here. Configure one, or run converge by hand.\n\n" + u.report(ctx)}
		}
		if err := u.acquire(); err != nil {
			return Result{Text: "⏳ " + err.Error()}
		}
		res, started := u.converge(ctx, req, op)
		if !started {
			u.release()
		}
		return res
	}
	if op.Agent && u.cfg.AgentBin == "" {
		return Result{Text: "❌ The agent binary is unknown here, so it cannot be updated."}
	}
	if err := u.acquire(); err != nil {
		return Result{Text: "⏳ " + err.Error()}
	}
	res := u.run(ctx, req, op)
	if res.After == nil {
		u.release()
	}
	return res
}

// run does the work while the lock is held.
func (u *Updater) run(ctx context.Context, req Request, op Op) Result {
	m := Marker{
		ConvID: req.ConvID, Requester: req.Requester, Who: req.Who,
		OldAgent: u.runningAgent(), OldRelay: u.cfg.RelayVersion,
		Rollback: op.Rollback, At: u.cfg.Now(),
	}
	var log strings.Builder
	switch {
	case op.Rollback:
		if err := u.rollback(); err != nil {
			return Result{Text: "❌ Rollback failed: " + err.Error()}
		}
		fmt.Fprintf(&log, "↩️ Restored `%s` from `%s.prev`.\n", u.cfg.AgentName, filepath.Base(u.cfg.AgentBin))
	case op.Agent:
		// Snapshot first, promote to .prev only if the binary actually
		// changed: a no-op update must not overwrite the useful .prev.
		cand := u.cfg.AgentBin + ".prev.new"
		if err := copyFile(u.cfg.AgentBin, cand); err != nil {
			return Result{Text: "❌ Could not keep a `.prev` copy of the agent: " + err.Error() + ". Nothing was changed."}
		}
		before := u.cfg.Version(ctx, u.cfg.AgentBin)
		out, err := u.cfg.UpdateAgent(ctx)
		if err != nil {
			os.Remove(cand)
			return Result{Text: fmt.Sprintf("❌ `%s update` failed: %v\n%s", u.cfg.AgentName, err, fence(out))}
		}
		if u.cfg.Version(ctx, u.cfg.AgentBin) != before {
			mustNot(os.Rename(cand, u.cfg.AgentBin+".prev"), "promote .prev")
		} else {
			os.Remove(cand)
		}
	}
	if op.Relay {
		out, err := u.cfg.UpdateSelf(ctx)
		if err != nil {
			return Result{Text: fmt.Sprintf("❌ `%s update` failed: %v\n%s", u.cfg.RelayName, err, fence(out))}
		}
	}
	if op.Force {
		cancelled := u.cfg.CancelAll()
		if len(cancelled) > 0 {
			log.WriteString("🛑 Cancelled turns in: " + strings.Join(cancelled, ", ") + "\n")
		}
		dctx, cancel := context.WithTimeout(ctx, u.cfg.DrainTimeout)
		err := u.cfg.WaitIdle(dctx)
		cancel()
		if err != nil {
			return Result{Text: log.String() + fmt.Sprintf("⚠️ Turns still running %s after cancel — NOT reloading "+
				"(a hard restart would lose queued messages). The new binaries are on disk; retry `!update --force` "+
				"or reload once idle.", u.cfg.DrainTimeout)}
		}
	}
	if err := u.writeMarker(m); err != nil {
		return Result{Text: log.String() + "❌ Could not write the reload marker: " + err.Error() + ". Not reloading."}
	}
	log.WriteString(fmt.Sprintf("🔄 Updated on disk: %s %s, %s %s. Reloading gracefully — I'll report here when back.",
		u.cfg.AgentName, u.cfg.Version(ctx, u.cfg.AgentBin), u.cfg.RelayName, u.cfg.Version(ctx, u.cfg.RelayBin)))
	return Result{Text: log.String(), After: func() error {
		if err := u.cfg.Reload(); err != nil {
			_ = os.Remove(u.markerPath())
			u.release()
			return err
		}
		return nil
	}}
}

func (u *Updater) owner(id string) bool {
	for _, o := range u.cfg.Owners {
		if id != "" && o == id {
			return true
		}
	}
	return false
}

func (u *Updater) runningAgent() string {
	if u.cfg.AgentVersion != nil {
		if v := u.cfg.AgentVersion(); v != "" {
			return v
		}
	}
	return "?"
}

// rollback swaps AgentBin and AgentBin.prev, so a second rollback undoes
// the first.
func (u *Updater) rollback() error {
	bin, prev := u.cfg.AgentBin, u.cfg.AgentBin+".prev"
	if _, err := os.Stat(prev); err != nil {
		return fmt.Errorf("no %s to roll back to", filepath.Base(prev))
	}
	keep := bin + ".rollback"
	if err := copyFile(bin, keep); err != nil {
		return err
	}
	if err := copyFile(prev, bin); err != nil {
		return err
	}
	return os.Rename(keep, prev)
}

// copyFile copies src to dst atomically (temp + rename), preserving mode.
// Rename, not overwrite, so replacing a running binary never hits ETXTBSY.
func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	st, err := os.Stat(src)
	mustNot(err, "stat after read")
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, b, st.Mode().Perm()); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

func fence(out string) string {
	out = strings.TrimSpace(out)
	if out == "" {
		return ""
	}
	if len(out) > 1500 {
		out = "…" + out[len(out)-1500:]
	}
	return "```\n" + out + "\n```"
}

// acquire takes the cross-process update lock. It is held until the
// reload's exec (the fd is close-on-exec) or released on failure.
func (u *Updater) acquire() error {
	if err := os.MkdirAll(u.cfg.StateDir, 0o755); err != nil {
		return fmt.Errorf("cannot create state dir: %v", err)
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	f, err := os.OpenFile(filepath.Join(u.cfg.StateDir, "update.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("cannot open update lock: %v", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return errors.New("another update is already in progress")
	}
	u.lock = f
	return nil
}

func (u *Updater) release() {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.lock != nil {
		u.lock.Close()
		u.lock = nil
	}
}

// report renders --check: versions on disk, running and locked.
func (u *Updater) report(ctx context.Context) string {
	locked := u.locked()
	var sb strings.Builder
	sb.WriteString("| | on disk | running | locked |\n|---|---|---|---|\n")
	agentDisk, agentRun := "?", u.runningAgent()
	if u.cfg.AgentBin != "" {
		agentDisk = u.cfg.Version(ctx, u.cfg.AgentBin)
	}
	if u.cfg.AgentPID != nil {
		if pid := u.cfg.AgentPID(); pid > 0 {
			agentRun += exeNote(u.cfg.ProcExe(pid))
		}
	}
	relayRun := u.cfg.RelayVersion + exeNote(u.cfg.ProcExe(os.Getpid()))
	fmt.Fprintf(&sb, "| %s | %s | %s | %s |\n", u.cfg.AgentName, agentDisk, agentRun, orDash(locked[u.cfg.AgentLockKey]))
	fmt.Fprintf(&sb, "| %s | %s | %s | %s |\n", u.cfg.RelayName, u.cfg.Version(ctx, u.cfg.RelayBin), relayRun, orDash(locked[u.cfg.RelayLockKey]))
	if u.cfg.Fleet {
		sb.WriteString("\nFleet-managed host: converge owns versions here.")
	}
	return sb.String()
}

// exeNote flags a running image whose file was replaced on disk.
func exeNote(exe string) string {
	if strings.HasSuffix(exe, " (deleted)") {
		return " (binary replaced; reload pending)"
	}
	return ""
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// locked reads the string entries of the dist.lock, if any.
func (u *Updater) locked() map[string]string {
	out := map[string]string{}
	if u.cfg.LockFile == "" {
		return out
	}
	b, err := os.ReadFile(u.cfg.LockFile)
	if err != nil {
		return out
	}
	var raw map[string]any
	if json.Unmarshal(b, &raw) != nil {
		return out
	}
	for k, v := range raw {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}

// Marker records an update in flight across the reload.
type Marker struct {
	ConvID    string    `json:"conv_id"`
	Requester string    `json:"requester"`
	Who       string    `json:"who,omitempty"`
	OldAgent  string    `json:"old_agent"`
	OldRelay  string    `json:"old_relay"`
	Rollback  bool      `json:"rollback,omitempty"`
	At        time.Time `json:"at"`
	// Converge marks a fleet host's converge job: the report waits for
	// the job, not just for the reload. OldLock is dist.lock before it.
	Converge bool              `json:"converge,omitempty"`
	OldLock  map[string]string `json:"old_lock,omitempty"`
}

func (u *Updater) markerPath() string { return filepath.Join(u.cfg.StateDir, "update-marker.json") }

func (u *Updater) writeMarker(m Marker) error {
	b, err := json.Marshal(m)
	mustNot(err, "marshal marker")
	tmp := u.markerPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, u.markerPath())
}

// Resume is called once by the new image at startup. If an update
// marker exists it posts the outcome into the requesting conversation
// via post and deletes the marker (even when post fails, so a broken
// conversation cannot make every future start re-post). No marker is
// not an error.
func (u *Updater) Resume(ctx context.Context, post func(convID, text string) error) error {
	m, err := readMarker(u.markerPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		os.RemoveAll(u.markerPath())
		return err
	}
	if age := u.cfg.Now().Sub(m.At); age > markerTTL {
		os.RemoveAll(u.markerPath())
		return fmt.Errorf("update marker is %s old; discarded without reporting", age.Round(time.Second))
	}
	if m.Converge {
		// The converge job reloaded us and may still be running. The
		// update lock did not survive the exec (close-on-exec), so take
		// it again: a second `!update` must not start a second job. The
		// watcher reports when the job ends and removes the marker.
		held := u.acquire() == nil
		go func() {
			u.watch(ctx, u.convergeGone, func(text string) error { return post(m.ConvID, text) })
			if held {
				u.release()
			}
		}()
		return nil
	}
	defer os.RemoveAll(u.markerPath())
	newAgent := "?"
	if u.cfg.AgentBin != "" {
		newAgent = u.cfg.Version(ctx, u.cfg.AgentBin)
	}
	verb := "Updated"
	if m.Rollback {
		verb = "Rolled back"
	}
	text := fmt.Sprintf("✅ %s and reloaded: %s %s → %s, %s %s → %s.", verb,
		u.cfg.AgentName, m.OldAgent, newAgent, u.cfg.RelayName, m.OldRelay, u.cfg.RelayVersion)
	if m.Who != "" {
		text += " (requested by " + m.Who + ")"
	}
	return post(m.ConvID, text)
}
