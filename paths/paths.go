// Package paths resolves conventional XDG state/config locations for relay apps.
package paths

import (
	"os"
	"path/filepath"
)

// StateDir returns the default per-app state directory.
// Order: $XDG_STATE_HOME/<app> → $HOME/.local/state/<app> → $TMPDIR/<app>.
func StateDir(app string) string {
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, app)
	}
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".local", "state", app)
	}
	return filepath.Join(os.TempDir(), app)
}

// ConfigPath returns the default per-app JSON config path.
// Order: $XDG_CONFIG_HOME/<app>/<name> → $HOME/.config/<app>/<name> → $TMPDIR/<app>/<name>.
func ConfigPath(app, name string) string {
	if name == "" {
		name = "config.json"
	}
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, app, name)
	}
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".config", app, name)
	}
	return filepath.Join(os.TempDir(), app, name)
}
