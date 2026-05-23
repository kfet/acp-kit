// Defensive helpers for attachment storage paths the production caller cannot
// realistically trigger. Excluded from coverage via the `_must.go` suffix
// rule in .covignore.
package attachments

// mustOpenFallback panics if OpenFile fails on the hash-derived fallback name.
// Reaching this path requires the per-message directory to reject every
// possible safe name, which only happens under disk-level failures (full
// disk, immutable filesystem, etc.) — not reachable from a unit test.
func mustOpenFallback(err error) {
	if err != nil {
		panic("attachments: fallback OpenFile: " + err.Error())
	}
}

// mustClose panics if closing the just-written attachment file errors. Only
// reachable on platforms whose filesystems surface deferred I/O errors at
// close time — not reachable from a unit test.
func mustClose(err error) {
	if err != nil {
		panic("attachments: close: " + err.Error())
	}
}

// mustOpenRoot panics if os.OpenRoot fails on a directory MkdirAll just
// created. Only reachable when the directory is destroyed between MkdirAll
// and OpenRoot — not reachable from a unit test.
func mustOpenRoot(err error) {
	if err != nil {
		panic("attachments: OpenRoot: " + err.Error())
	}
}

// mustMkdirAt panics if Root.Mkdir errors with something other than ErrExist.
// Only reachable on filesystem-level failure inside an already-validated
// per-message dir — not reachable from a unit test.
func mustMkdirAt(err error) {
	if err != nil {
		panic("attachments: Mkdir within Root: " + err.Error())
	}
}

// mustOpenRootAt panics if Root.OpenRoot fails on a directory the same code
// just created inside the parent Root. Not reachable from a unit test.
func mustOpenRootAt(err error) {
	if err != nil {
		panic("attachments: OpenRoot within Root: " + err.Error())
	}
}
