package sysprompt

import (
	"testing"
	"time"
)

func TestComposeJoinsTrimmedNonEmpty(t *testing.T) {
	got := Compose("  base  ", "", "cat\n")
	if want := "base\n\ncat"; got != want {
		t.Fatalf("Compose = %q want %q", got, want)
	}
}

func TestComposeAllEmpty(t *testing.T) {
	if got := Compose("", "  ", "\n\n"); got != "" {
		t.Fatalf("Compose all empty = %q", got)
	}
}

func TestResolveDisabled(t *testing.T) {
	if got := Resolve("base", "extra", true, "cat"); got != "" {
		t.Fatalf("Resolve disabled = %q", got)
	}
}

func TestResolveEnabled(t *testing.T) {
	got := Resolve("base", "extra", false, "cat")
	if got != "base\n\nextra\n\ncat" {
		t.Fatalf("Resolve = %q", got)
	}
}

func TestLivenessNote(t *testing.T) {
	const suffix = " with no text and no tool call. Tool calls and poll progress reset it. Context survives a cancel."
	for _, tc := range []struct {
		name   string
		window time.Duration
		want   string
	}{
		{"default on zero", 0, "Turn watchdog: 2m0s" + suffix},
		{"default on negative", -5 * time.Second, "Turn watchdog: 2m0s" + suffix},
		{"configured window", 90 * time.Second, "Turn watchdog: 1m30s" + suffix},
		{"sub-minute window", 30 * time.Second, "Turn watchdog: 30s" + suffix},
		{"long window", 10 * time.Minute, "Turn watchdog: 10m0s" + suffix},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := LivenessNote(tc.window); got != tc.want {
				t.Fatalf("LivenessNote(%s) = %q, want %q", tc.window, got, tc.want)
			}
		})
	}
}
