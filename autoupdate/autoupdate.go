// Package autoupdate keeps a relay current without an operator typing
// `!update`: it polls for a newer release, stages and verifies it, asks
// the relay's owners for approval through a Surface (one chat message,
// edited in place), applies it at the next idle point through the
// relay's graceful reload, and rolls back on its own when the new image
// fails its health gate.
//
// Modes (Config.Mode, the relay's `auto_update` setting):
//
//	off     never polls
//	notify  posts the offer; the download happens on approval
//	stage   downloads + verifies first, then posts the offer (default)
//	auto    stages and applies with no prompt — refused on a fleet host
//
// Safety rails, all here so every relay gets the same ones:
//
//   - the old binary is kept as <bin>.prev;
//   - after the re-exec the new image must pass Config.HealthProbe
//     (resume its inherited queue, one successful round-trip) within
//     Config.HealthTimeout, else it restores .prev, blocks the version,
//     reloads, and the old image reports why;
//   - at most one automatic apply per Config.Cooldown; after a rollback
//     nothing applies automatically until an owner acts;
//   - a newer release supersedes a pending offer (one prompt, not two);
//   - quiet hours hold an approved apply until they end.
//
// A fleet-managed host (Config.Fleet) never resolves versions itself:
// the trigger is dist.lock drift ("lock wants vX, running vY"), and
// approval runs the relay's `!update` converge path (Config.Updater).
//
// Resolution goes through distkit's anonymous /releases/latest
// redirect, which spends no GitHub API quota (so no ETag dance is
// needed); download, sha256 verification and the atomic swap are
// distkit's too.
package autoupdate

import (
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/kfet/acp-kit/update"
	"github.com/kfet/distkit"
)

// Mode is the `auto_update` setting.
type Mode string

// The four modes. The zero value parses as Stage.
const (
	Off    Mode = "off"
	Notify Mode = "notify"
	Stage  Mode = "stage"
	Auto   Mode = "auto"
)

// ParseMode parses an `auto_update` value; "" means Stage.
func ParseMode(s string) (Mode, error) {
	switch m := Mode(strings.ToLower(strings.TrimSpace(s))); m {
	case "":
		return Stage, nil
	case Off, Notify, Stage, Auto:
		return m, nil
	}
	return "", fmt.Errorf("auto_update: unknown mode %q (want off, notify, stage or auto)", s)
}

// Decision is an owner's answer to an offer.
type Decision int

// The three answers a relay maps its reactions/buttons onto.
const (
	// ApplyNow applies at the next idle point.
	ApplyNow Decision = iota + 1
	// Tomorrow applies at the first idle point a day from now.
	Tomorrow
	// SkipVersion drops the offer and never offers that version again.
	SkipVersion
)

// Surface is how a relay shows the offer. Post creates the one message
// (returning its id); every later state is an Edit of it.
type Surface interface {
	Post(ctx context.Context, text string) (id string, err error)
	Edit(ctx context.Context, id, text string) error
}

// Config wires a Manager to one relay.
type Config struct {
	Mode Mode
	// Dist is the relay's distkit configuration (Repo, Binary…).
	// Version is overwritten with RelayVersion.
	Dist distkit.Config

	RelayName    string
	RelayVersion string
	// RelayBin is the binary that gets swapped. Default os.Executable.
	RelayBin string

	// AgentName/AgentBin/AgentVersion/UpdateAgent cover the fir-skew
	// case: a release whose notes say "requires fir >= X" while the
	// running agent is older is offered and applied with the agent as
	// one unit, one reload.
	AgentName    string
	AgentBin     string
	AgentVersion func() string
	UpdateAgent  func(ctx context.Context) (string, error)

	// Owners are the ids whose decisions count.
	Owners []string
	// StateDir holds autoupdate.json.
	StateDir string
	Surface  Surface
	// ConvID is the relay conversation token offers live in. A fleet
	// converge reports there after its reload (update.Resume).
	ConvID string

	// Fleet marks a converge-managed host; LockFile is its dist.lock.
	// Updater runs the converge on approval.
	Fleet        bool
	LockFile     string
	RelayLockKey string
	AgentLockKey string
	Updater      *update.Updater

	// Idle reports that no turn is in flight. Default: always idle (the
	// graceful reload drains anyway).
	Idle func() bool
	// Reload triggers the graceful reload (SIGHUP). Required.
	Reload func() error
	// HealthProbe is retried by the new image until it succeeds or
	// HealthTimeout passes: the relay resumes its inherited queue and
	// makes one real round-trip. Default: healthy once started.
	HealthProbe func(ctx context.Context) error

	// QuietHours "HH:MM-HH:MM" in local time; no apply starts inside.
	QuietHours string

	Interval      time.Duration // poll period, default 6h
	Jitter        time.Duration // ± around Interval, default 30m
	Tick          time.Duration // pending-apply check, default 1m
	HealthTimeout time.Duration // default 2m
	Cooldown      time.Duration // between automatic applies, default 24h
	RemindAfter   time.Duration // one reminder, default 72h
	Snooze        time.Duration // the "tomorrow" delay, default 24h

	// Seams. Defaults: distkit.Check/Download, GitHub release notes,
	// time.Now, a timer, math/rand.
	Check    func(ctx context.Context) (*distkit.Status, error)
	Download func(ctx context.Context, rel *distkit.Release, dir string) (string, error)
	Notes    func(ctx context.Context, tag string) (string, error)
	Now      func() time.Time
	Sleep    func(ctx context.Context, d time.Duration) error
	Rand     func(n int64) int64
	Logf     func(format string, args ...any)
}

// Manager runs the auto-update loop for one relay.
type Manager struct {
	cfg   Config
	quiet quietHours
	mu    sync.Mutex
	st    persisted
}

// New validates cfg and fills defaults.
func New(cfg Config) (*Manager, error) {
	if cfg.Mode == "" {
		cfg.Mode = Stage
	}
	if cfg.Fleet && cfg.Mode == Auto {
		// A fleet host's versions are the lock's; installing without a
		// human would drift it. Ask instead.
		cfg.Mode = Stage
		if cfg.Logf != nil {
			cfg.Logf("autoupdate: auto_update=auto refused on a fleet-managed host; using stage (owner approval runs converge)")
		}
	}
	if cfg.Mode != Off && (cfg.Surface == nil || cfg.Reload == nil || cfg.StateDir == "") {
		return nil, fmt.Errorf("autoupdate: Surface, Reload and StateDir are required")
	}
	if cfg.Fleet && cfg.Mode != Off && cfg.Updater == nil {
		return nil, fmt.Errorf("autoupdate: a fleet host needs Updater to converge")
	}
	q, err := parseQuiet(cfg.QuietHours)
	if err != nil {
		return nil, err
	}
	if cfg.RelayBin == "" {
		cfg.RelayBin, _ = os.Executable()
		cfg.RelayBin = strings.TrimSuffix(cfg.RelayBin, " (deleted)")
	}
	// Swap and restore the real files, never a symlink pointing at
	// them: replacing a link with a regular file orphans a managed
	// install.
	for _, p := range []*string{&cfg.RelayBin, &cfg.AgentBin} {
		if r, err := filepath.EvalSymlinks(*p); *p != "" && err == nil {
			*p = r
		}
	}
	if cfg.AgentName == "" {
		cfg.AgentName = "fir"
	}
	if cfg.RelayLockKey == "" {
		cfg.RelayLockKey = strings.ReplaceAll(cfg.RelayName, "-", "_")
	}
	if cfg.AgentLockKey == "" {
		cfg.AgentLockKey = cfg.AgentName
	}
	def := func(d *time.Duration, v time.Duration) {
		if *d == 0 {
			*d = v
		}
	}
	def(&cfg.Interval, 6*time.Hour)
	def(&cfg.Jitter, 30*time.Minute)
	def(&cfg.Tick, time.Minute)
	def(&cfg.HealthTimeout, 2*time.Minute)
	def(&cfg.Cooldown, 24*time.Hour)
	def(&cfg.RemindAfter, 72*time.Hour)
	def(&cfg.Snooze, 24*time.Hour)
	dist := cfg.Dist
	dist.Version = cfg.RelayVersion
	dist.Anonymous = true
	dist.Stdout, dist.Stderr = io.Discard, io.Discard
	if cfg.Check == nil {
		cfg.Check = func(ctx context.Context) (*distkit.Status, error) { return distkit.Check(ctx, dist) }
	}
	if cfg.Download == nil {
		cfg.Download = func(ctx context.Context, rel *distkit.Release, dir string) (string, error) {
			return distkit.Download(ctx, dist, rel, dir)
		}
	}
	if cfg.Notes == nil {
		cfg.Notes = githubNotes(nil, githubAPI, dist.Repo)
	}
	if cfg.Idle == nil {
		cfg.Idle = func() bool { return true }
	}
	if cfg.HealthProbe == nil {
		cfg.HealthProbe = func(context.Context) error { return nil }
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Sleep == nil {
		cfg.Sleep = sleep
	}
	if cfg.Rand == nil {
		cfg.Rand = rand.Int64N
	}
	m := &Manager{cfg: cfg, quiet: q}
	m.st = loadState(m.statePath(), m.logf)
	return m, nil
}

// Mode reports the effective mode (auto degrades to stage on a fleet host).
func (m *Manager) Mode() Mode { return m.cfg.Mode }

// Run resumes an apply in progress (health gate / rollback report),
// then polls until ctx ends. Call it once, in its own goroutine, after
// the relay can post.
func (m *Manager) Run(ctx context.Context) {
	m.resume(ctx)
	if m.cfg.Mode == Off {
		return
	}
	next := m.cfg.Now().Add(m.jitter(m.cfg.Tick * 5))
	for {
		if !m.cfg.Now().Before(next) {
			m.Poll(ctx)
			next = m.cfg.Now().Add(m.cfg.Interval + m.jitter(m.cfg.Jitter))
		}
		m.Tick(ctx)
		if m.cfg.Sleep(ctx, m.cfg.Tick) != nil {
			return
		}
	}
}

// jitter returns a random duration in [-j, j].
func (m *Manager) jitter(j time.Duration) time.Duration {
	return time.Duration(m.cfg.Rand(int64(2*j)+1)) - j
}

func (m *Manager) owner(id string) bool {
	for _, o := range m.cfg.Owners {
		if id != "" && o == id {
			return true
		}
	}
	return false
}

func (m *Manager) logf(format string, args ...any) {
	if m.cfg.Logf != nil {
		m.cfg.Logf(format, args...)
	}
}

func (m *Manager) statePath() string { return filepath.Join(m.cfg.StateDir, "autoupdate.json") }

func (m *Manager) post(ctx context.Context, text string) string {
	id, err := m.cfg.Surface.Post(ctx, text)
	if err != nil {
		m.logf("autoupdate: post: %v", err)
	}
	return id
}

func (m *Manager) edit(ctx context.Context, id, text string) {
	if id == "" {
		m.post(ctx, text)
		return
	}
	if err := m.cfg.Surface.Edit(ctx, id, text); err != nil {
		m.logf("autoupdate: edit: %v", err)
	}
}

func (m *Manager) save() {
	if err := saveState(m.statePath(), m.st); err != nil {
		m.logf("autoupdate: save state: %v", err)
	}
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
