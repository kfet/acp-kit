// Defensive helpers for agent-spawn paths the production caller cannot
// trigger. Excluded from coverage via the `_must.go` suffix rule in
// .covignore.
package client

// mustNot panics with label and err's message if err is non-nil. Used to mark
// branches that are unreachable from a unit test — typically kernel-level
// pipe allocation failures or $TMPDIR being unwritable.
func mustNot(err error, label string) {
	if err != nil {
		panic("client: " + label + ": " + err.Error())
	}
}
