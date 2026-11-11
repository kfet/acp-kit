package relaytool

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/kfet/acp-kit/command"
	"github.com/kfet/acp-kit/mcphost"
)

// uploadProvider adapts the Broker's UploadTargeter to mcphost, mapping
// the session key to the conversation exactly as every tool does.
type uploadProvider struct {
	t *Tools
	u command.UploadTargeter
}

var errInactive = errors.New("this conversation is no longer active")

func (p uploadProvider) UploadTarget(sessionKey string, spec mcphost.UploadSpec) (mcphost.UploadTarget, error) {
	conv, ok := p.t.cfg.ConvToken(sessionKey)
	if !ok {
		return mcphost.UploadTarget{}, errInactive
	}
	return p.u.UploadTargetFor(conv, spec)
}

func (p uploadProvider) FinishUpload(sessionKey string, spec mcphost.UploadSpec, ref string) (mcphost.UploadedFile, error) {
	conv, ok := p.t.cfg.ConvToken(sessionKey)
	if !ok {
		return mcphost.UploadedFile{}, errInactive
	}
	return p.u.FinishUploadFor(conv, spec, ref)
}

// call adapts a conversation-bound CallHandler body to both handler
// forms of a Tool.
func (t *Tools) call(fn func(conv string, c mcphost.Call) (mcphost.Result, error)) (mcphost.Handler, mcphost.CallHandler) {
	full := func(c mcphost.Call) (mcphost.Result, error) {
		conv, ok := t.cfg.ConvToken(c.SessionKey)
		if !ok {
			return mcphost.Result{}, errInactive
		}
		return fn(conv, c)
	}
	plain := func(sessionKey string, args json.RawMessage) (string, error) {
		r, err := full(mcphost.Call{SessionKey: sessionKey, Args: args})
		return r.Text, err
	}
	return plain, full
}

// sendFileTool delivers a file into the conversation. Its `file`
// argument carries x-mcp-file, so the CLIENT fills it from a file the
// host can read (the model never emits the bytes), and mcphost lists
// the tool only to clients that declared the files extension. Small
// files arrive inline and go through FilePoster; with an
// UploadTargeter, larger ones are uploaded straight to the platform and
// posted with PostUploadedTo.
func (t *Tools) sendFileTool() Tool {
	plain, full := t.call(func(conv string, c mcphost.Call) (mcphost.Result, error) {
		var a struct {
			File    json.RawMessage `json:"file"`
			Caption string          `json:"caption"`
		}
		if err := decode(c.Args, &a); err != nil {
			return mcphost.Result{}, err
		}
		var name string
		var size int64
		var ids []string
		if up, ok := c.Files["file"]; ok {
			var err error
			if ids, err = t.cfg.Broker.PostUploaded(conv, up, a.Caption); err != nil {
				return mcphost.Result{}, err
			}
			name, size = up.Name, up.Size
		} else {
			f, err := mcphost.DecodeFile(a.File)
			if err != nil {
				return mcphost.Result{}, err
			}
			if ids, err = t.cfg.Broker.PostFile(conv, command.OutFile{Name: f.Name, MimeType: f.MimeType, Data: f.Data, Caption: a.Caption}); err != nil {
				return mcphost.Result{}, err
			}
			name, size = f.Name, int64(len(f.Data))
		}
		msg := fmt.Sprintf("Sent %s (%d bytes) to this conversation", command.SafeFileName(name), size)
		if len(ids) > 0 {
			msg += " as message id(s): " + strings.Join(ids, ", ")
		}
		return mcphost.Result{Text: msg + "."}, nil
	})
	return Tool{
		Name: ToolSendFile,
		Description: "Send a file into this conversation as an attachment, right now, outside the answer " +
			"you are streaming. Use it to hand the user an artifact you produced: a report, an image, " +
			"a log, an archive. It always sends here — there is no way to address anywhere else.",
		Schema: objectSchemaReq(map[string]any{
			"file": mcphost.FileSchema(mcphost.FileInputDescriptor{MaxSize: t.cfg.MaxFileBytes},
				"The file to send."),
			"caption": strProp("Optional text to accompany the file. Not every relay shows it."),
		}, "file"),
		Handler: plain,
		Call:    full,
	}
}

// attachmentTools exposes the conversation's inbound attachments as MCP
// downloads: list them, then fetch one by id. The client downloads the
// bytes itself from the platform's https URL; they never pass through
// the relay's MCP socket or the model.
func (t *Tools) attachmentTools() []Tool {
	listPlain, listFull := t.call(func(conv string, _ mcphost.Call) (mcphost.Result, error) {
		d, _ := t.cfg.Broker.DownloadLinker()
		as, err := d.AttachmentsIn(conv)
		if err != nil {
			return mcphost.Result{}, err
		}
		if len(as) == 0 {
			return mcphost.Result{Text: "No attachments in this conversation."}, nil
		}
		var b strings.Builder
		b.WriteString("Attachments in this conversation, newest first (id, name, type, size):")
		for _, a := range as {
			size := "unknown size"
			if a.Size >= 0 {
				size = fmt.Sprintf("%d bytes", a.Size)
			}
			fmt.Fprintf(&b, "\n- %s: %s (%s, %s)", a.ID, command.SafeFileName(a.Name), a.MimeType, size)
		}
		return mcphost.Result{Text: b.String()}, nil
	})
	fetchPlain, fetchFull := t.call(func(conv string, c mcphost.Call) (mcphost.Result, error) {
		var a struct {
			ID string `json:"id"`
		}
		if err := decode(c.Args, &a); err != nil {
			return mcphost.Result{}, err
		}
		if a.ID == "" {
			return mcphost.Result{}, errors.New("id is required: get one from " + ToolListAttachments)
		}
		d, _ := t.cfg.Broker.DownloadLinker()
		tgt, err := d.DownloadLinkFor(conv, a.ID)
		if err != nil {
			return mcphost.Result{}, err
		}
		tgt.Name = command.SafeFileName(tgt.Name)
		return mcphost.Result{
			Text:      fmt.Sprintf("Attachment %s (%d bytes) is ready to download.", tgt.Name, tgt.Size),
			Downloads: []mcphost.DownloadTarget{tgt},
		}, nil
	})
	return []Tool{{
		Name: ToolListAttachments,
		Description: "List files users attached in this conversation, newest first, with the id " +
			ToolFetchAttachment + " takes.",
		Schema:  objectSchema(nil),
		Handler: listPlain,
		Call:    listFull,
	}, {
		Name: ToolFetchAttachment,
		Description: "Download a file a user attached in this conversation, by the id " + ToolListAttachments +
			" gives. The file is saved locally and its path reported in the result.",
		Schema:  objectSchemaReq(map[string]any{"id": strProp("Attachment id from " + ToolListAttachments + ".")}, "id"),
		Handler: fetchPlain,
		Call:    fetchFull,
	}}
}
