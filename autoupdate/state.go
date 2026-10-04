package autoupdate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// offer is the one pending update.
type offer struct {
	// Lock marks a fleet-host offer: dist.lock drift, applied by converge.
	Lock    bool   `json:"lock,omitempty"`
	Version string `json:"version"`
	// Agent is the agent version that goes with it: the release's
	// required minimum, or the lock's pin. Empty when none is needed.
	Agent  string   `json:"agent,omitempty"`
	Staged string   `json:"staged,omitempty"`
	Notes  []string `json:"notes,omitempty"`
	MsgID  string   `json:"msg_id,omitempty"`
	// Posted is when the prompt went up; Reminded once RemindAfter fired.
	Posted   time.Time `json:"posted"`
	Reminded bool      `json:"reminded,omitempty"`
	// Approved by Who (or "auto"), not before NotBefore.
	Approved  bool      `json:"approved,omitempty"`
	Who       string    `json:"who,omitempty"`
	NotBefore time.Time `json:"not_before,omitempty"`
}

// applying records an apply across the reload.
type applying struct {
	Version string    `json:"version"`
	From    string    `json:"from"`
	MsgID   string    `json:"msg_id,omitempty"`
	Agent   bool      `json:"agent,omitempty"` // the agent was updated too
	At      time.Time `json:"at"`
	// Boots counts new-image starts that reached the gate; a binary
	// that keeps restarting is rolled back without another probe.
	Boots int `json:"boots,omitempty"`
	// RolledBack is set by the failed new image; Reason says why.
	RolledBack bool   `json:"rolled_back,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

type persisted struct {
	Offer    *offer    `json:"offer,omitempty"`
	Applying *applying `json:"applying,omitempty"`
	Blocked  []string  `json:"blocked,omitempty"`
	LastAuto time.Time `json:"last_auto,omitempty"`
	// Halted stops automatic applies after a rollback until an owner acts.
	Halted bool `json:"halted,omitempty"`
}

func (p *persisted) blocked(v string) bool {
	for _, b := range p.Blocked {
		if b == v {
			return true
		}
	}
	return false
}

func (p *persisted) block(v string) {
	if !p.blocked(v) {
		p.Blocked = append(p.Blocked, v)
	}
}

// loadState reads the state file; a missing or corrupt one is empty.
func loadState(path string, logf func(string, ...any)) persisted {
	var p persisted
	b, err := os.ReadFile(path)
	if err != nil {
		return p
	}
	if err := json.Unmarshal(b, &p); err != nil {
		logf("autoupdate: %s: %v (starting fresh)", path, err)
		return persisted{}
	}
	return p
}

func saveState(path string, p persisted) error {
	b, _ := json.MarshalIndent(p, "", "  ") // plain structs: cannot fail
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// quietHours is a daily local-time window [from, to) in minutes; it may
// wrap midnight. from == to means none.
type quietHours struct{ from, to int }

func parseQuiet(s string) (quietHours, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return quietHours{}, nil
	}
	a, b, ok := strings.Cut(s, "-")
	from, err1 := clock(a)
	to, err2 := clock(b)
	if !ok || err1 != nil || err2 != nil {
		return quietHours{}, fmt.Errorf("quiet hours %q: want HH:MM-HH:MM", s)
	}
	return quietHours{from, to}, nil
}

func clock(s string) (int, error) {
	h, mm, _ := strings.Cut(strings.TrimSpace(s), ":")
	hi, err := strconv.Atoi(h)
	mi := 0
	if err == nil && mm != "" {
		mi, err = strconv.Atoi(mm)
	}
	if err != nil || hi < 0 || hi > 23 || mi < 0 || mi > 59 {
		return 0, fmt.Errorf("bad time %q", s)
	}
	return hi*60 + mi, nil
}

func (q quietHours) in(t time.Time) bool {
	m := t.Hour()*60 + t.Minute()
	switch {
	case q.from == q.to:
		return false
	case q.from < q.to:
		return m >= q.from && m < q.to
	default:
		return m >= q.from || m < q.to
	}
}

// semver parses the leading X.Y.Z of v (a "v" prefix and any suffix
// ignored); ok is false when there is none.
func semver(v string) (out [3]int, ok bool) {
	parts := strings.SplitN(strings.TrimPrefix(strings.TrimSpace(v), "v"), ".", 3)
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		end := 0
		for end < len(p) && p[end] >= '0' && p[end] <= '9' {
			end++
		}
		n, err := strconv.Atoi(p[:end])
		if err != nil {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// newer reports whether a > b. Unparseable versions are never newer.
func newer(a, b string) bool {
	x, ok1 := semver(a)
	y, ok2 := semver(b)
	if !ok1 || !ok2 {
		return false
	}
	for i := range x {
		if x[i] != y[i] {
			return x[i] > y[i]
		}
	}
	return false
}

func ensureV(v string) string {
	if v == "" || strings.HasPrefix(v, "v") {
		return v
	}
	return "v" + v
}

var requiresRe = regexp.MustCompile(`(?i)requires?\s+fir\s*(?:>=|≥|v)?\s*v?(\d+\.\d+\.\d+)`)

// requiredAgent returns the minimum fir version a release's notes
// declare ("requires fir >= 1.25.0"), or "".
func requiredAgent(notes string) string {
	if m := requiresRe.FindStringSubmatch(notes); m != nil {
		return m[1]
	}
	return ""
}

// highlights picks up to n bullet lines from release notes, breaking
// changes first.
func highlights(notes string, n int) []string {
	var breaking, rest []string
	for _, l := range strings.Split(notes, "\n") {
		l = strings.TrimSpace(l)
		if !strings.HasPrefix(l, "- ") && !strings.HasPrefix(l, "* ") {
			continue
		}
		l = strings.TrimSpace(l[2:])
		if strings.Contains(strings.ToLower(l), "breaking") {
			breaking = append(breaking, l)
		} else {
			rest = append(rest, l)
		}
	}
	out := append(breaking, rest...)
	if len(out) > n {
		out = out[:n]
	}
	return out
}

const githubAPI = "https://api.github.com"

// githubNotes fetches a release's body from the unauthenticated GitHub
// API. One call per new release, so the 60/h anonymous quota is ample.
func githubNotes(client *http.Client, base, repo string) func(context.Context, string) (string, error) {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return func(ctx context.Context, tag string) (string, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/repos/"+repo+"/releases/tags/"+tag, nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		resp, err := client.Do(req)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("release notes: HTTP %d", resp.StatusCode)
		}
		var r struct {
			Body string `json:"body"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&r); err != nil {
			return "", fmt.Errorf("release notes: %w", err)
		}
		return r.Body, nil
	}
}
