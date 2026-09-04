// Defensive helper for the process-to-process pipe. Excluded from
// coverage via the `_must.go` suffix rule in .covignore.
package remotefs

import "io"

// mustPipe returns the pipe end or panics. (*exec.Cmd).StdoutPipe fails
// only on caller misuse — Stdout already set, or the command already
// started — neither of which the callers here do, and on kernel-level
// pipe allocation failure, which no test can provoke.
func mustPipe(rd io.ReadCloser, err error) io.ReadCloser {
	if err != nil {
		panic("remotefs: stdout pipe: " + err.Error())
	}
	return rd
}
