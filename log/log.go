// Package log provides a tiny opt-in debug logger for ACP relay code.
package log

import (
	stdlog "log"
	"os"
	"strings"
	"sync/atomic"
)

var enabled atomic.Bool

// Register enables debug logging when envVar is set to a truthy value.
// It returns the resulting enabled state. Empty envVar is ignored.
func Register(envVar string) bool {
	if envVar != "" && truthy(os.Getenv(envVar)) {
		enabled.Store(true)
	}
	return enabled.Load()
}

// SetEnabled forces debug logging on or off.
func SetEnabled(on bool) { enabled.Store(on) }

// Enabled reports whether debug logging is enabled.
func Enabled() bool { return enabled.Load() }

// Debugf logs a debug message when enabled. Output goes through the
// standard library log package and is prefixed with "[dbg] ".
func Debugf(format string, args ...any) {
	if !enabled.Load() {
		return
	}
	stdlog.Printf("[dbg] "+format, args...)
}

// Logf is an alias for Debugf kept for painless migration from existing relays.
func Logf(format string, args ...any) { Debugf(format, args...) }

func truthy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on", "y", "t":
		return true
	}
	return false
}
