package update

// mustNot panics with label and err's message if err is non-nil. Used for
// branches a caller cannot trigger: os.Stat failing on a file that was
// read successfully a moment earlier, and json.Marshal of a struct made
// only of strings, a bool and a time.Time.
func mustNot(err error, label string) {
	if err != nil {
		panic("update: " + label + ": " + err.Error())
	}
}
