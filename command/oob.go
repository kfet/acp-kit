package command

import (
	"errors"

	"github.com/kfet/acp-kit/mcphost"
)

// UploadTargeter is the OPTIONAL capability behind out-of-band uploads
// for `send_file`: instead of the agent's client pushing file bytes
// through the relay's MCP socket, the relay hands it a target on the
// chat platform's own https upload endpoint, the client uploads there,
// and the relay posts the already-stored file.
//
// Every method is bound to the conversation the tool call came from.
// UploadTargetFor must mint a SCOPED, short-lived target: it is sent to
// the agent's client verbatim, so it must never carry the relay's own
// API credentials (mcphost refuses Authorization and Cookie headers). A
// platform whose upload API needs the bot token must be fronted by a
// one-shot relay-hosted endpoint instead; see docs/files-oob.md.
type UploadTargeter interface {
	// UploadTargetFor mints a target for one file.
	UploadTargetFor(convID string, spec mcphost.UploadSpec) (mcphost.UploadTarget, error)
	// FinishUploadFor confirms the upload behind ref and reports what
	// the platform stored. It must fail when nothing arrived.
	FinishUploadFor(convID string, spec mcphost.UploadSpec, ref string) (mcphost.UploadedFile, error)
	// PostUploadedTo posts the stored file into the conversation, with
	// an optional caption, and returns the message ids it created.
	PostUploadedTo(convID string, f mcphost.UploadedFile, caption string) ([]string, error)
}

// InboundAttachment is a file a user attached in the conversation.
type InboundAttachment struct {
	ID       string // relay-chosen, stable within the conversation
	Name     string
	MimeType string
	Size     int64 // -1 when unknown
}

// DownloadLinker is the OPTIONAL capability behind the
// `list_attachments` / `fetch_attachment` tools: it exposes files users
// attached in the conversation as MCP download targets — absolute https
// URLs the platform already serves (a temporary signed URL, a
// relay-hosted proxy link), never one that needs the relay's
// credentials.
type DownloadLinker interface {
	// AttachmentsIn lists the conversation's recent inbound attachments,
	// newest first.
	AttachmentsIn(convID string) ([]InboundAttachment, error)
	// DownloadLinkFor returns a download target for one of them. Size
	// must be exact; a link should be single-use or expire quickly.
	DownloadLinkFor(convID, attachmentID string) (mcphost.DownloadTarget, error)
}

// SetUploadTargeter supplies UploadTargeter separately; it takes
// precedence over one implemented by the Poster or the Controller.
func (b *Broker) SetUploadTargeter(u UploadTargeter) { b.upload = u }

// SetDownloadLinker supplies DownloadLinker separately; it takes
// precedence over one implemented by the Poster or the Controller.
func (b *Broker) SetDownloadLinker(d DownloadLinker) { b.links = d }

// capability looks up an optional capability: explicit setter, then the
// Poster, then the Controller.
func capability[T any](set T, ok bool, b *Broker) (T, bool) {
	if ok {
		return set, true
	}
	if v, ok := b.post.(T); ok {
		return v, true
	}
	v, ok := b.ctrl.(T) // a nil Controller asserts to false
	return v, ok
}

// UploadTargeter returns the relay's UploadTargeter, if any.
func (b *Broker) UploadTargeter() (UploadTargeter, bool) {
	return capability(b.upload, b.upload != nil, b)
}

// DownloadLinker returns the relay's DownloadLinker, if any.
func (b *Broker) DownloadLinker() (DownloadLinker, bool) {
	return capability(b.links, b.links != nil, b)
}

// PostUploaded posts a file stored by an out-of-band upload, with its
// name reduced to a safe base name and its media type normalised.
func (b *Broker) PostUploaded(convID string, f mcphost.UploadedFile, caption string) ([]string, error) {
	u, ok := b.UploadTargeter()
	if !ok {
		return nil, errors.New("this relay cannot accept uploads")
	}
	f.Name = SafeFileName(f.Name)
	f.MimeType = safeMediaType(f.MimeType)
	return u.PostUploadedTo(convID, f, caption)
}
