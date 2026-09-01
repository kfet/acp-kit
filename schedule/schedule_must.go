package schedule

import (
	"crypto/rand"
	"encoding/json"
)

// mustRandom fills b with cryptographically-random bytes. crypto/rand
// on the platforms we target does not fail; a failure means the system
// CSPRNG is unavailable, which is fatal-grade — a guessable schedule id
// would let one conversation cancel another's work. Excluded from
// coverage via the _must.go rule.
func mustRandom(b []byte) {
	if _, err := rand.Read(b); err != nil {
		panic("schedule: crypto/rand: " + err.Error())
	}
}

// mustMarshal encodes the on-disk document. Every field is a string,
// int, time.Time or a slice of those, so encoding cannot fail; a
// failure would mean the type changed to something unmarshalable, which
// is a build-time mistake and not a runtime condition to handle.
// Excluded from coverage via the _must.go rule.
func mustMarshal(v any) []byte {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		panic("schedule: marshal: " + err.Error())
	}
	return b
}
