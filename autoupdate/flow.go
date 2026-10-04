package autoupdate

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/kfet/acp-kit/update"
	"github.com/kfet/distkit"
)

// Poll checks once for a new release (or, on a fleet host, for
// dist.lock drift) and posts or supersedes the offer. Run calls it on
// its schedule; a relay may call it for an immediate check.
func (m *Manager) Poll(ctx context.Context) {
	if m.cfg.Mode == Off {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.st.Applying != nil {
		return // the health gate owns the state until it settles
	}
	if m.cfg.Fleet {
		m.pollLock(ctx)
	} else {
		m.pollRelease(ctx)
	}
	m.remind(ctx)
	m.save()
}

func (m *Manager) pollRelease(ctx context.Context) {
	st, err := m.cfg.Check(ctx)
	if err != nil {
		m.logf("autoupdate: check: %v", err)
		return
	}
	cur := ensureV(m.cfg.RelayVersion)
	if !newer(st.Target, cur) {
		if o := m.st.Offer; o != nil && !newer(o.Version, cur) {
			m.drop() // installed by other means meanwhile
		}
		return
	}
	if m.st.blocked(st.Target) || (m.st.Offer != nil && m.st.Offer.Version == st.Target) {
		return
	}
	o := &offer{Version: st.Target, Posted: m.cfg.Now()}
	notes, err := m.cfg.Notes(ctx, st.Target)
	if err != nil {
		// Without the notes a "requires fir" would be missed and the
		// apply would fail its gate: retry next poll.
		m.logf("autoupdate: notes for %s: %v", st.Target, err)
		return
	}
	o.Notes = highlights(notes, 3)
	if req := requiredAgent(notes); req != "" && newer(req, m.runningAgent()) {
		o.Agent = req
	}
	if m.cfg.Mode != Notify {
		path, err := m.stage(ctx, st.Release)
		if err != nil {
			m.logf("autoupdate: stage %s: %v", st.Target, err)
			return // retried next poll
		}
		o.Staged = path
	}
	if m.cfg.Mode == Auto && !m.st.Halted && !m.cfg.Now().Before(m.st.LastAuto.Add(m.cfg.Cooldown)) {
		o.Approved, o.Who, o.NotBefore = true, "auto", o.Posted
	}
	m.replace(ctx, o)
}

// stage downloads and verifies rel next to the relay binary, so the
// later swap is an atomic same-filesystem rename.
func (m *Manager) stage(ctx context.Context, rel *distkit.Release) (string, error) {
	dir, err := distkit.StagingDir(m.cfg.RelayBin, filepath.Base(m.cfg.RelayBin))
	if err != nil {
		return "", err
	}
	path, err := m.cfg.Download(ctx, rel, dir)
	if err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	return path, nil
}

func (m *Manager) pollLock(ctx context.Context) {
	lock := readLock(m.cfg.LockFile)
	wantRelay, wantAgent := ensureV(lock[m.cfg.RelayLockKey]), lock[m.cfg.AgentLockKey]
	runAgent := m.runningAgent()
	drift := wantRelay != "" && wantRelay != ensureV(m.cfg.RelayVersion)
	agentDrift := wantAgent != "" && runAgent != "?" && strings.TrimPrefix(wantAgent, "v") != strings.TrimPrefix(runAgent, "v")
	if !drift && !agentDrift {
		if m.st.Offer != nil {
			m.drop() // converged by other means
		}
		return
	}
	key := wantRelay + "/" + wantAgent
	if m.st.blocked("lock:"+key) || (m.st.Offer != nil && m.st.Offer.Version+"/"+m.st.Offer.Agent == key) {
		return
	}
	m.replace(ctx, &offer{Lock: true, Version: wantRelay, Agent: wantAgent, Posted: m.cfg.Now()})
}

// replace makes o the pending offer, superseding the old one.
func (m *Manager) replace(ctx context.Context, o *offer) {
	if old := m.st.Offer; old != nil {
		m.edit(ctx, old.MsgID, fmt.Sprintf("~~%s~~ superseded by %s — see below.", m.headline(old), o.Version))
		m.drop()
	}
	o.MsgID = m.post(ctx, m.render(o))
	m.st.Offer = o
}

// drop clears the offer and its staged download.
func (m *Manager) drop() {
	if o := m.st.Offer; o != nil && o.Staged != "" {
		os.RemoveAll(filepath.Dir(o.Staged))
	}
	m.st.Offer = nil
}

func (m *Manager) remind(ctx context.Context) {
	o := m.st.Offer
	if o == nil || o.Approved || o.Reminded || m.cfg.Now().Before(o.Posted.Add(m.cfg.RemindAfter)) {
		return
	}
	o.Reminded = true
	m.post(ctx, fmt.Sprintf(":bell: Reminder: %s is still waiting for an owner decision on the update message above. (Last reminder.)", o.Version))
}

func (m *Manager) runningAgent() string {
	if m.cfg.AgentVersion != nil {
		if v := m.cfg.AgentVersion(); v != "" {
			return v
		}
	}
	return "?"
}

// headline is the first line of an offer.
func (m *Manager) headline(o *offer) string {
	if o.Lock {
		s := fmt.Sprintf(":package: dist.lock wants %s %s, running %s", m.cfg.RelayName, o.Version, ensureV(m.cfg.RelayVersion))
		if o.Agent != "" {
			s += fmt.Sprintf(" (%s %s, running %s)", m.cfg.AgentName, o.Agent, m.runningAgent())
		}
		return s
	}
	verb := "available"
	if o.Staged != "" {
		verb = "staged, sha verified"
	}
	s := fmt.Sprintf(":package: %s %s %s (running %s)", m.cfg.RelayName, o.Version, verb, ensureV(m.cfg.RelayVersion))
	if o.Agent != "" {
		s += fmt.Sprintf(" — needs %s ≥ %s (running %s), updated together", m.cfg.AgentName, o.Agent, m.runningAgent())
	}
	return s
}

// render is the offer message: headline, changelog, the question.
func (m *Manager) render(o *offer) string {
	var b strings.Builder
	b.WriteString(m.headline(o))
	for _, n := range o.Notes {
		b.WriteString("\n- " + n)
	}
	switch {
	case o.Approved && o.Who == "auto":
		b.WriteString("\n\nauto_update=auto: applying at the next idle point.")
	case o.Approved:
		fmt.Fprintf(&b, "\n\nApproved by %s: applying at the first idle point after %s.", o.Who, o.NotBefore.Format("Mon 15:04"))
	case o.Lock:
		b.WriteString("\n\nApply (runs converge)? :check: apply when idle · :clock: tomorrow · :no_entry: skip")
	default:
		b.WriteString("\n\n:check: apply when idle · :clock: tomorrow · :no_entry: skip this version")
	}
	return b.String()
}

// Decide records an owner's decision on the offer message msgID. It
// reports whether the decision was taken: false for a non-owner, an
// unknown message, or a settled offer.
func (m *Manager) Decide(ctx context.Context, msgID, who string, d Decision) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	o := m.st.Offer
	if !m.owner(who) || o == nil || o.MsgID == "" || o.MsgID != msgID {
		return false
	}
	m.st.Halted = false // a human acted
	switch d {
	case ApplyNow, Tomorrow:
		o.Approved, o.Who, o.NotBefore = true, who, m.cfg.Now()
		if d == Tomorrow {
			o.NotBefore = o.NotBefore.Add(m.cfg.Snooze)
		}
		m.edit(ctx, o.MsgID, m.render(o))
	case SkipVersion:
		if o.Lock {
			m.st.block("lock:" + o.Version + "/" + o.Agent)
		} else {
			m.st.block(o.Version)
		}
		m.edit(ctx, o.MsgID, fmt.Sprintf("~~%s~~ skipped by %s.", m.headline(o), who))
		m.drop()
	default:
		return false
	}
	m.save()
	return true
}

// Status renders the pending state for `!update --check`.
func (m *Manager) Status() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var b strings.Builder
	fmt.Fprintf(&b, "Auto-update: %s", m.cfg.Mode)
	if m.cfg.Fleet {
		b.WriteString(" (fleet host: follows dist.lock)")
	}
	b.WriteString(".")
	if m.st.Halted {
		b.WriteString(" Halted after a rollback — approve an offer to resume.")
	}
	switch o := m.st.Offer; {
	case m.st.Applying != nil:
		fmt.Fprintf(&b, "\nApplying %s (health gate pending).", m.st.Applying.Version)
	case o == nil:
		b.WriteString("\nNo update pending.")
	case o.Approved:
		fmt.Fprintf(&b, "\nPending: %s, approved by %s, not before %s.", o.Version, o.Who, o.NotBefore.Format(time.RFC3339))
	default:
		fmt.Fprintf(&b, "\nPending: %s, awaiting an owner decision.", o.Version)
	}
	if len(m.st.Blocked) > 0 {
		b.WriteString("\nSkipped: " + strings.Join(m.st.Blocked, ", "))
	}
	return b.String()
}

// Tick applies an approved offer once it is due, outside quiet hours,
// and the relay is idle.
func (m *Manager) Tick(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o := m.st.Offer
	now := m.cfg.Now()
	if o == nil || !o.Approved || now.Before(o.NotBefore) || m.quiet.in(now) || !m.cfg.Idle() {
		return
	}
	if o.Who == "auto" && (m.st.Halted || now.Before(m.st.LastAuto.Add(m.cfg.Cooldown))) {
		o.Approved, o.Who = false, ""
		m.edit(ctx, o.MsgID, m.render(o))
		m.save()
		return
	}
	if o.Lock {
		m.converge(ctx, o)
	} else {
		m.apply(ctx, o)
	}
	m.save()
}

// converge runs the fleet converge for a lock offer.
func (m *Manager) converge(ctx context.Context, o *offer) {
	m.st.Offer = nil
	m.edit(ctx, o.MsgID, m.headline(o)+"\n\nApproved by "+o.Who+": running converge.")
	var mu sync.Mutex
	done := false
	res := m.cfg.Updater.Handle(ctx, update.Request{
		ConvID: m.cfg.ConvID, Requester: o.Who, Who: o.Who, Text: "!update",
		Post: func(text string) error {
			mu.Lock()
			defer mu.Unlock()
			done = true
			m.edit(context.WithoutCancel(ctx), o.MsgID, m.headline(o)+"\n\n"+text)
			return nil
		},
	})
	mu.Lock()
	defer mu.Unlock()
	if !done { // a fast job may already have reported
		m.edit(ctx, o.MsgID, m.headline(o)+"\n\n"+res.Text)
	}
}

// apply swaps the binaries and reloads.
func (m *Manager) apply(ctx context.Context, o *offer) {
	fail := func(err error) {
		// Not blocked: the failure may be transient, so the next poll
		// offers the version again.
		m.drop()
		m.edit(ctx, o.MsgID, fmt.Sprintf("%s\n\n:x: Apply failed: %v. Nothing was changed; it will be offered again.", m.headline(o), err))
	}
	if o.Staged == "" { // notify mode: download on approval
		st, err := m.cfg.Check(ctx)
		if err == nil && st.Target != o.Version {
			err = fmt.Errorf("the latest release is now %s", st.Target)
		}
		var path string
		if err == nil {
			path, err = m.stage(ctx, st.Release)
		}
		if err != nil {
			fail(err)
			return
		}
		o.Staged = path
	}
	a := &applying{Version: o.Version, From: ensureV(m.cfg.RelayVersion), MsgID: o.MsgID, At: m.cfg.Now()}
	if o.Agent != "" {
		if m.cfg.UpdateAgent == nil || m.cfg.AgentBin == "" {
			fail(fmt.Errorf("it needs %s ≥ %s and this relay cannot update %s", m.cfg.AgentName, o.Agent, m.cfg.AgentName))
			return
		}
		if err := copyFile(m.cfg.AgentBin, m.cfg.AgentBin+".prev"); err != nil {
			fail(fmt.Errorf("keep %s.prev: %w", m.cfg.AgentName, err))
			return
		}
		if out, err := m.cfg.UpdateAgent(ctx); err != nil {
			fail(fmt.Errorf("%s update: %w: %s", m.cfg.AgentName, err, strings.TrimSpace(out)))
			return
		}
		a.Agent = true
	}
	if err := copyFile(m.cfg.RelayBin, m.cfg.RelayBin+".prev"); err != nil {
		fail(fmt.Errorf("keep .prev: %w", err))
		return
	}
	if err := os.Rename(o.Staged, m.cfg.RelayBin); err != nil {
		fail(err)
		return
	}
	os.RemoveAll(filepath.Dir(o.Staged))
	if o.Who == "auto" {
		m.st.LastAuto = a.At
	}
	m.st.Offer, m.st.Applying = nil, a
	m.edit(ctx, o.MsgID, fmt.Sprintf("%s\n\n:arrows_counterclockwise: Installed; reloading gracefully (approved by %s). Health check follows.", m.headline(o), o.Who))
	// The new image reads this; without it there is no health gate.
	err := saveState(m.statePath(), m.st)
	if err == nil {
		err = m.cfg.Reload()
	}
	if err != nil {
		m.restore(a)
		m.st.Applying = nil
		m.st.block(a.Version)
		m.edit(ctx, a.MsgID, fmt.Sprintf(":x: %s %s: not applied (%v); restored %s.", m.cfg.RelayName, a.Version, err, a.From))
	}
}

// restore puts the .prev binaries back.
func (m *Manager) restore(a *applying) {
	if err := copyFile(m.cfg.RelayBin+".prev", m.cfg.RelayBin); err != nil {
		m.logf("autoupdate: restore %s: %v", m.cfg.RelayBin, err)
	}
	if a.Agent {
		if err := copyFile(m.cfg.AgentBin+".prev", m.cfg.AgentBin); err != nil {
			m.logf("autoupdate: restore %s: %v", m.cfg.AgentBin, err)
		}
	}
}

// resume settles an apply that spanned the reload: in the new image it
// runs the health gate, in the old image (after a rollback) it reports.
func (m *Manager) resume(ctx context.Context) {
	m.mu.Lock()
	a := m.st.Applying
	m.mu.Unlock()
	if a == nil {
		return
	}
	if a.RolledBack {
		m.mu.Lock()
		m.edit(ctx, a.MsgID, fmt.Sprintf(":rewind: %s %s rolled back to %s: %s. Version blocked; automatic applies halted until an owner acts.",
			m.cfg.RelayName, a.Version, a.From, a.Reason))
		m.st.Applying = nil
		m.save()
		m.mu.Unlock()
		return
	}
	m.mu.Lock()
	a.Boots++
	m.save()
	m.mu.Unlock()
	err := m.health(ctx, a)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err == nil {
		m.edit(ctx, a.MsgID, fmt.Sprintf(":check: %s %s → %s healthy.", m.cfg.RelayName, a.From, a.Version))
		m.st.Applying = nil
		m.save()
		return
	}
	if ctx.Err() != nil {
		return // shutting down: the next image re-runs the gate
	}
	m.restore(a)
	a.RolledBack, a.Reason = true, err.Error()
	m.st.block(a.Version)
	m.st.Halted = true
	m.save()
	if err := m.cfg.Reload(); err != nil {
		m.logf("autoupdate: reload after rollback: %v", err)
	}
}

// health waits for the probe to succeed within HealthTimeout.
func (m *Manager) health(ctx context.Context, a *applying) error {
	if ensureV(m.cfg.RelayVersion) != a.Version {
		return fmt.Errorf("the new image did not start (running %s)", ensureV(m.cfg.RelayVersion))
	}
	if a.Boots > 3 {
		return fmt.Errorf("the new image restarted %d times", a.Boots-1)
	}
	// From the apply, not from this start: a restart must not reset it.
	deadline := a.At.Add(m.cfg.HealthTimeout)
	if d := m.cfg.Now().Add(m.cfg.HealthTimeout / 2); d.After(deadline) {
		deadline = d // a slow start still gets a fair probe window
	}
	for {
		err := m.cfg.HealthProbe(ctx)
		if err == nil {
			return nil
		}
		if !m.cfg.Now().Before(deadline) || m.cfg.Sleep(ctx, 5*time.Second) != nil {
			return fmt.Errorf("unhealthy after %s: %v", m.cfg.HealthTimeout, err)
		}
	}
}

// readLock reads the string entries of a dist.lock.
func readLock(path string) map[string]string {
	out := map[string]string{}
	b, err := os.ReadFile(path)
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

// copyFile copies src to dst atomically, preserving mode.
func copyFile(src, dst string) error {
	st, err := os.Stat(src)
	if err != nil {
		return err
	}
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, b, st.Mode().Perm()); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}
