// Defensive helpers for attachment storage paths the production caller cannot
// realistically trigger. Excluded from coverage via the `_must.go` suffix
// rule in .covignore.
package attachments

// mustNot panics with label and err's message if err is non-nil. Used to mark
// branches that are unreachable from a unit test — typically because they
// require kernel/filesystem failure between two adjacent system calls (e.g.
// MkdirAll then OpenRoot on the same path) or post-Close I/O surfacing.
func mustNot(err error, label string) {
	if err != nil {
		panic("attachments: " + label + ": " + err.Error())
	}
}
