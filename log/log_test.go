package log

import (
	"bytes"
	stdlog "log"
	"testing"
)

func TestRegisterRespectsEnv(t *testing.T) {
	SetEnabled(false)
	t.Setenv("ACP_KIT_TEST_LOG", "1")
	if !Register("ACP_KIT_TEST_LOG") {
		t.Fatal("Register: expected enabled")
	}
	if !Enabled() {
		t.Fatal("Enabled: false")
	}

	SetEnabled(false)
	t.Setenv("ACP_KIT_TEST_LOG", "0")
	if Register("ACP_KIT_TEST_LOG") {
		t.Fatal("Register: should be false when env is falsy")
	}

	SetEnabled(false)
	if Register("") {
		t.Fatal("Register: empty env should be no-op")
	}
}

func TestDebugfGatedAndAliased(t *testing.T) {
	var buf bytes.Buffer
	stdlog.SetOutput(&buf)
	stdlog.SetFlags(0)
	t.Cleanup(func() { stdlog.SetOutput(nil); stdlog.SetFlags(stdlog.LstdFlags) })

	SetEnabled(false)
	Debugf("nope %d", 1)
	Logf("nope %d", 2)
	if buf.Len() != 0 {
		t.Fatalf("expected no output when disabled, got %q", buf.String())
	}

	SetEnabled(true)
	Debugf("hi %d", 3)
	Logf("bye %d", 4)
	got := buf.String()
	if want := "[dbg] hi 3\n[dbg] bye 4\n"; got != want {
		t.Fatalf("output = %q want %q", got, want)
	}
}

func TestTruthy(t *testing.T) {
	for _, s := range []string{"1", "true", "TRUE", "yes", " on ", "y", "t"} {
		if !truthy(s) {
			t.Fatalf("truthy(%q) = false", s)
		}
	}
	for _, s := range []string{"", "0", "no", "off", "false", "n"} {
		if truthy(s) {
			t.Fatalf("truthy(%q) = true", s)
		}
	}
}
