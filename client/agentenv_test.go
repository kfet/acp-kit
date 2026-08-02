package client

import (
	"os"
	"slices"
	"testing"
)

// contains reports whether env holds an exact KEY=VALUE entry.
func contains(env []string, kv string) bool { return slices.Contains(env, kv) }

func TestScrubbedEnv_DropByName(t *testing.T) {
	cfg := Config{
		Env:            []string{"KEEP=1", "SECRET_TOKEN=xyz", "ALSO=2"},
		SecretEnvNames: []string{"SECRET_TOKEN"},
	}
	got := cfg.scrubbedEnv()
	if contains(got, "SECRET_TOKEN=xyz") {
		t.Fatalf("SECRET_TOKEN not dropped: %v", got)
	}
	if !contains(got, "KEEP=1") || !contains(got, "ALSO=2") {
		t.Fatalf("non-secret entries lost: %v", got)
	}
}

func TestScrubbedEnv_DropByValueUnderBespokeName(t *testing.T) {
	// The value is a secret even though the variable has an arbitrary name
	// the scrubber could not have known to list.
	cfg := Config{
		Env:     []string{"WEIRD_NAME=super-secret", "KEEP=fine"},
		Secrets: []string{"super-secret"},
	}
	got := cfg.scrubbedEnv()
	if contains(got, "WEIRD_NAME=super-secret") {
		t.Fatalf("secret value not dropped: %v", got)
	}
	if !contains(got, "KEEP=fine") {
		t.Fatalf("non-secret entry lost: %v", got)
	}
}

func TestScrubbedEnv_NilEnvMaterialisesAndScrubs(t *testing.T) {
	// The critical path: Env is nil ("inherit os.Environ()"). A declared
	// secret that lives in the real process environment MUST be dropped, and
	// the result must be an explicit non-nil slice — otherwise cmd.Env stays
	// nil and the child inherits the full environment, secret included.
	const name = "ACP_KIT_TEST_SECRET_NIL_ENV"
	const val = "leak-me-not"
	t.Setenv(name, val)

	cfg := Config{SecretEnvNames: []string{name}}
	got := cfg.scrubbedEnv()
	if got == nil {
		t.Fatal("scrubbedEnv returned nil for nil Env with secrets declared")
	}
	if contains(got, name+"="+val) {
		t.Fatalf("secret survived nil-Env scrub: %v", name)
	}
	// A sibling non-secret variable from the real environment survives.
	t.Setenv("ACP_KIT_TEST_KEEP_NIL_ENV", "ok")
	got = cfg.scrubbedEnv()
	if !contains(got, "ACP_KIT_TEST_KEEP_NIL_ENV=ok") {
		t.Fatalf("non-secret env var lost during nil-Env scrub")
	}
}

func TestScrubbedEnv_NilEnvDropByValue(t *testing.T) {
	// nil-Env path, but dropping by literal value under fir's own var name.
	const val = "poeacp-inbound-bearer"
	t.Setenv("POEACP_ACCESS_KEY", val)
	cfg := Config{Secrets: []string{val}}
	got := cfg.scrubbedEnv()
	if got == nil {
		t.Fatal("expected non-nil env")
	}
	if contains(got, "POEACP_ACCESS_KEY="+val) {
		t.Fatalf("secret value survived nil-Env scrub")
	}
}

func TestScrubbedEnv_EmptySecretsIgnored(t *testing.T) {
	// An empty-string secret must never cause an empty-valued variable to be
	// dropped, nor count as "secrets declared".
	cfg := Config{
		Env:     []string{"EMPTY=", "KEEP=1"},
		Secrets: []string{""},
	}
	got := cfg.scrubbedEnv()
	if !contains(got, "EMPTY=") || !contains(got, "KEEP=1") {
		t.Fatalf("empty secret dropped something: %v", got)
	}
	// Only empty secrets ⇒ no secrets declared ⇒ pure passthrough (same slice).
	if &got[0] != &cfg.Env[0] {
		t.Fatalf("empty-only Secrets should be a pure passthrough, not a copy")
	}
}

func TestScrubbedEnv_CallerSliceNotMutated(t *testing.T) {
	orig := []string{"KEEP=1", "SECRET=drop", "TAIL=2"}
	snapshot := slices.Clone(orig)
	cfg := Config{Env: orig, SecretEnvNames: []string{"SECRET"}}
	_ = cfg.scrubbedEnv()
	if !slices.Equal(orig, snapshot) {
		t.Fatalf("caller slice mutated: %v", orig)
	}
}

func TestScrubbedEnv_PassthroughNoSecrets(t *testing.T) {
	// Non-nil Env, no secrets: returned unchanged (same backing array).
	env := []string{"A=1", "B=2"}
	cfg := Config{Env: env}
	got := cfg.scrubbedEnv()
	if &got[0] != &env[0] {
		t.Fatalf("no-secret passthrough must not copy")
	}
	// Nil Env, no secrets: stays nil so Start inherits os.Environ().
	if got := (Config{}).scrubbedEnv(); got != nil {
		t.Fatalf("nil Env with no secrets must stay nil, got %v", got)
	}
}

func TestScrubbedEnv_NonEmptyValueAmongEmptySecretsCounts(t *testing.T) {
	// hasSecrets must skip empties yet still trip on a real value.
	cfg := Config{
		Env:     []string{"X=real", "Y=keep"},
		Secrets: []string{"", "real", ""},
	}
	got := cfg.scrubbedEnv()
	if contains(got, "X=real") {
		t.Fatalf("real secret among empties not dropped: %v", got)
	}
	if !contains(got, "Y=keep") {
		t.Fatalf("non-secret lost: %v", got)
	}
}

// Guard: ensure os.Environ is actually reachable in the nil path so the
// materialisation branch is genuinely exercised (belt-and-braces).
func TestScrubbedEnv_NilEnvUsesProcessEnviron(t *testing.T) {
	t.Setenv("ACP_KIT_TEST_PRESENT", "yes")
	cfg := Config{SecretEnvNames: []string{"ACP_KIT_TEST_ABSENT"}}
	got := cfg.scrubbedEnv()
	if !contains(got, "ACP_KIT_TEST_PRESENT=yes") {
		t.Fatalf("nil-Env scrub did not draw from os.Environ(): %v missing", "ACP_KIT_TEST_PRESENT")
	}
	_ = os.Environ // keep os import meaningful if trimmed
}
