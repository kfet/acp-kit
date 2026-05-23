package sysprompt

import "testing"

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
