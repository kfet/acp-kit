// Package client: defensive helpers for agent-spawn paths the production
// caller cannot trigger. Excluded from coverage via the `_must.go` suffix
// rule in .covignore.
package client

// mustPipe panics if StdinPipe/StdoutPipe returns an error. exec.Cmd only
// fails to allocate a pipe when the kernel refuses os.Pipe — not reachable
// from a unit test, and the child has not been Started yet so there is
// nothing to clean up.
func mustPipe(err error, which string) {
	if err != nil {
		panic("client: " + which + " pipe: " + err.Error())
	}
}

// mustTempDir panics if os.MkdirTemp fails inside ProbeModels. Only
// reachable when $TMPDIR is unwritable; every other test in this package
// proves the contrary in the same process.
func mustTempDir(err error) {
	if err != nil {
		panic("client: probe mkdir tmp: " + err.Error())
	}
}
