package command

import (
	"errors"
	"mime"
	"path"
	"strings"
	"unicode"
)

// OutFile is a file the agent hands to the relay to deliver into a
// conversation. The bytes are already in memory: the relay decides how
// they reach the chat (an upload API, a message attachment, ...).
type OutFile struct {
	// Name is the file name shown in the chat. Broker.PostFile reduces
	// it to a safe base name before the relay sees it.
	Name string
	// MimeType is the declared media type, "application/octet-stream"
	// when unknown. Untrusted: it is whatever the agent claimed.
	MimeType string
	// Data is the file content.
	Data []byte
	// Caption is optional text to accompany the file. A relay that
	// cannot attach text to a file may post it separately or drop it.
	Caption string
}

// FilePoster is the OPTIONAL capability behind the agent-facing
// `send_file` tool: it delivers a file into a conversation as an
// attachment, out of band, exactly like Poster does for text.
//
// The blast radius is Poster's: PostFileTo takes the conversation token
// the tool call was bound to and nothing else, so an agent can only
// ever send a file to the conversation that is talking to it. It
// returns the ids of the messages it put on the surface, oldest first,
// or nil when the relay has no such ids.
type FilePoster interface {
	PostFileTo(convID string, f OutFile) ([]string, error)
}

// SetFilePoster supplies the FilePoster capability separately from the
// Controller; see SetPoster. It takes precedence over a FilePoster
// implemented by the Poster or by the Controller.
func (b *Broker) SetFilePoster(p FilePoster) { b.filePost = p }

// filePoster returns the FilePoster capability, looked up in the same
// order as every other optional capability: explicit setter, then the
// Poster (a relay commonly implements both on one type), then the
// Controller.
func (b *Broker) filePoster() (FilePoster, bool) {
	if b.filePost != nil {
		return b.filePost, true
	}
	if fp, ok := b.post.(FilePoster); ok {
		return fp, true
	}
	fp, ok := b.ctrl.(FilePoster) // a nil Controller asserts to false
	return fp, ok
}

// CanPostFile reports whether the relay can deliver files out of band.
func (b *Broker) CanPostFile() bool { _, ok := b.filePoster(); return ok }

// PostFile delivers f into the conversation as an attachment. The name
// is reduced to a safe base name (no directories, no control
// characters) and an empty media type becomes
// "application/octet-stream". An empty file is refused: no chat surface
// renders one usefully.
func (b *Broker) PostFile(convID string, f OutFile) ([]string, error) {
	p, ok := b.filePoster()
	if !ok {
		return nil, errors.New("this relay cannot send files")
	}
	if len(f.Data) == 0 {
		return nil, errors.New("nothing to send: the file is empty")
	}
	f.Name = SafeFileName(f.Name)
	f.MimeType = safeMediaType(f.MimeType)
	return p.PostFileTo(convID, f)
}

// safeMediaType returns mt re-serialised from its parsed form, so a
// relay can put it in a header, or "application/octet-stream" when it
// does not parse.
func safeMediaType(mt string) string {
	t, params, err := mime.ParseMediaType(mt)
	if err != nil {
		return "application/octet-stream"
	}
	return mime.FormatMediaType(t, params)
}

// SafeFileName reduces an untrusted file name to a base name a relay
// can show and store: path separators, control and format characters
// (including bidi overrides that could spoof an extension) go, and
// an empty, "." or ".." result becomes "file".
func SafeFileName(name string) string {
	name = strings.ReplaceAll(name, `\`, "/")
	name = path.Base(name)
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	switch name {
	case "", ".", "..", "/":
		return "file"
	}
	return name
}
