package paths

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestStateDirXDG(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/x/state")
	if got, want := StateDir("acp-kit"), filepath.Join("/x/state", "acp-kit"); got != want {
		t.Fatalf("StateDir = %q want %q", got, want)
	}
}

func TestStateDirHomeFallback(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "")
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", "C:/u")
	} else {
		t.Setenv("HOME", "/u")
	}
	got := StateDir("acp-kit")
	if !strings.Contains(got, "acp-kit") || !strings.Contains(got, ".local") {
		t.Fatalf("StateDir fallback unexpected: %q", got)
	}
}

func TestConfigPathXDG(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/x/cfg")
	if got, want := ConfigPath("acp-kit", "main.json"), filepath.Join("/x/cfg", "acp-kit", "main.json"); got != want {
		t.Fatalf("ConfigPath = %q want %q", got, want)
	}
}

func TestConfigPathDefaultsName(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/x/cfg")
	if got, want := ConfigPath("acp-kit", ""), filepath.Join("/x/cfg", "acp-kit", "config.json"); got != want {
		t.Fatalf("ConfigPath default name = %q want %q", got, want)
	}
}

func TestConfigPathHomeFallback(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", "C:/u")
	} else {
		t.Setenv("HOME", "/u")
	}
	got := ConfigPath("acp-kit", "x.json")
	if !strings.Contains(got, ".config") || !strings.HasSuffix(got, "x.json") {
		t.Fatalf("ConfigPath fallback unexpected: %q", got)
	}
}

func TestStateAndConfigTmpFallback(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "")
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", "")
	}
	if got := StateDir("acp-kit"); !strings.Contains(got, "acp-kit") {
		t.Fatalf("StateDir tmp fallback: %q", got)
	}
	if got := ConfigPath("acp-kit", ""); !strings.Contains(got, "config.json") {
		t.Fatalf("ConfigPath tmp fallback: %q", got)
	}
}
