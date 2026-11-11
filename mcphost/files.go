package mcphost

// Server side of the draft MCP extension io.modelcontextprotocol/files
// (github.com/modelcontextprotocol/experimental-ext-file-uploads,
// specification/draft/file-uploads.md). Out-of-band transfers, which need
// an https endpoint the relay supplies, are in oob.go.
//
// What is implemented, and why the rest is not:
//
//   - Inline uploads: a tool marks a top-level argument with
//     x-mcp-file (see FileSchema) and the client sends the bytes as
//     base64 in the FileInput's `data`. The Host advertises
//     maxInlineBytes and validates every file argument — shape, decoded
//     size, maxSize, accept, digest, duplicate ids, the per-request
//     inline total — before the handler runs, answering isError without
//     running the tool on any failure, as the spec requires.
//   - Out-of-band uploads (`upload`, upload_required) and download
//     targets are offered only through a relay-supplied provider (see
//     oob.go): on stdio the spec allows only absolute https `url`
//     targets with credentials "omit", and mcphost has none of its own.
//
// Tools carrying x-mcp-file are listed only to clients that declare the
// extension in the request's _meta, per the spec's Backward
// Compatibility section; a call to one is still accepted from any
// client, because servers must not reject inline data for want of a
// declaration.

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"path"
	"sort"
	"strings"
)

const (
	// FilesExtension is the extension identifier.
	FilesExtension = "io.modelcontextprotocol/files"
	// ClientCapabilitiesMeta is the request _meta key under which a
	// client declares the extensions it supports.
	ClientCapabilitiesMeta = "io.modelcontextprotocol/clientCapabilities"
	// DefaultMaxInlineBytes is the maxInlineBytes advertised when
	// Config.MaxInlineBytes is zero.
	DefaultMaxInlineBytes int64 = 8 << 20
)

// FileInputDescriptor is the value of the x-mcp-file schema keyword.
type FileInputDescriptor struct {
	// Accept lists media types ("image/png", "image/*") or dot-prefixed
	// extensions (".pdf"). Empty accepts anything.
	Accept []string `json:"accept,omitempty"`
	// MaxSize caps each file's decoded size in bytes. Zero means no
	// per-file cap beyond the Host's maxInlineBytes.
	MaxSize int64 `json:"maxSize,omitempty"`
}

// FileDigest is a FileInput digest.
type FileDigest struct {
	Algorithm string `json:"algorithm"`
	Value     string `json:"value"` // base64url, no padding
}

// fileInput is the FileInput wire shape. Data is a RawMessage so a JSON
// null (which the spec calls undecodable) is told apart from absent.
type fileInput struct {
	ID       string          `json:"id"`
	Name     string          `json:"name,omitempty"`
	MimeType string          `json:"mimeType"`
	Size     *int64          `json:"size"`
	Digest   *FileDigest     `json:"digest,omitempty"`
	Data     json.RawMessage `json:"data,omitempty"`
}

// File is a decoded, validated inline file argument.
type File struct {
	ID       string
	Name     string // untrusted; reduce to a base name before use
	MimeType string // untrusted
	Data     []byte
}

// FileSchema returns the object schema for one x-mcp-file argument:
// the FileInput shape with the descriptor attached.
func FileSchema(desc FileInputDescriptor, description string) map[string]any {
	xf := map[string]any{}
	if len(desc.Accept) > 0 {
		xf["accept"] = desc.Accept
	}
	if desc.MaxSize > 0 {
		xf["maxSize"] = desc.MaxSize
	}
	s := map[string]any{
		"type":       "object",
		"x-mcp-file": xf,
		"properties": map[string]any{
			"id":       map[string]any{"type": "string", "minLength": 1, "maxLength": 128},
			"name":     map[string]any{"type": "string"},
			"mimeType": map[string]any{"type": "string"},
			"size":     map[string]any{"type": "integer", "minimum": 0},
			"digest": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"algorithm": map[string]any{"const": "sha-256"},
					"value":     map[string]any{"type": "string"},
				},
				"required": []string{"algorithm", "value"},
			},
			"data": map[string]any{"type": "string"},
		},
		"required": []string{"id", "mimeType", "size"},
	}
	if description != "" {
		s["description"] = description
	}
	return s
}

// fileParams finds the top-level properties of a tool's input schema
// that carry x-mcp-file. Nested and anyOf/oneOf placements are not
// supported. A descriptor that does not decode is a programming error
// in the consumer's schema, so it panics at registration rather than
// silently dropping the limits it was meant to impose.
func fileParams(schema map[string]any) map[string]FileInputDescriptor {
	props, _ := schema["properties"].(map[string]any)
	var out map[string]FileInputDescriptor
	for name, p := range props {
		pm, _ := p.(map[string]any)
		raw, ok := pm["x-mcp-file"]
		if !ok {
			continue
		}
		var d FileInputDescriptor
		b, _ := json.Marshal(raw)
		if err := json.Unmarshal(b, &d); err != nil {
			panic(fmt.Sprintf("mcphost: x-mcp-file on %q: %v", name, err))
		}
		if out == nil {
			out = map[string]FileInputDescriptor{}
		}
		out[name] = d
	}
	return out
}

// DecodeFile decodes and validates one inline FileInput argument: id
// length, mimeType, a non-negative size that matches the decoded bytes,
// and the digest when present. A size-0 file with no data is a valid
// empty file. A non-empty file without data is refused here: an
// out-of-band file reaches a ToolCall handler as Call.Files instead.
//
// The Host already ran this (plus maxSize, accept and the inline
// total) before invoking the handler; handlers call it to get the
// bytes.
func DecodeFile(raw json.RawMessage) (File, error) {
	var in fileInput
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return File{}, errors.New("file is missing")
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return File{}, fmt.Errorf("not a file input: %w", err)
	}
	if n := len(in.ID); n < 1 || n > 128 {
		return File{}, errors.New("file id must be 1 to 128 characters")
	}
	if in.MimeType == "" {
		return File{}, errors.New("file mimeType is required")
	}
	if in.Size == nil || *in.Size < 0 {
		return File{}, errors.New("file size must be a non-negative integer")
	}
	f := File{ID: in.ID, Name: in.Name, MimeType: in.MimeType}
	switch {
	case len(in.Data) == 0:
		if *in.Size > 0 {
			return File{}, errors.New("file data is missing: this server accepts files inline only")
		}
	default:
		var s string
		if err := json.Unmarshal(in.Data, &s); err != nil || bytes.Equal(in.Data, []byte("null")) {
			return File{}, errors.New("file data is not a base64 string")
		}
		d, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			return File{}, fmt.Errorf("file data is not valid base64: %w", err)
		}
		f.Data = d
	}
	if int64(len(f.Data)) != *in.Size {
		return File{}, fmt.Errorf("file size %d does not match %d decoded bytes", *in.Size, len(f.Data))
	}
	if in.Digest != nil {
		if in.Digest.Algorithm != "sha-256" {
			return File{}, fmt.Errorf("unsupported digest algorithm %q", in.Digest.Algorithm)
		}
		sum := sha256.Sum256(f.Data)
		if base64.RawURLEncoding.EncodeToString(sum[:]) != in.Digest.Value {
			return File{}, errors.New("file digest does not match its data")
		}
	}
	return f, nil
}

// accepts reports whether a file matches an accept list.
func accepts(accept []string, f File) bool {
	if len(accept) == 0 {
		return true
	}
	mt, _, _ := mime.ParseMediaType(f.MimeType)
	ext := strings.ToLower(path.Ext(f.Name))
	for _, a := range accept {
		a = strings.ToLower(strings.TrimSpace(a))
		switch {
		case a == "*/*":
			return true
		case strings.HasPrefix(a, "."):
			if ext == a {
				return true
			}
		case strings.HasSuffix(a, "/*"):
			if strings.HasPrefix(mt, strings.TrimSuffix(a, "*")) {
				return true
			}
		case a == mt:
			return true
		}
	}
	return false
}

// checkFiles validates every x-mcp-file argument of a call before the
// handler runs. An error means isError without running the tool. A
// non-empty file sent without data, on a ToolCall tool while an upload
// provider is set, is an out-of-band file: it is validated on its claims
// and returned as an UploadSpec (sorted by argument name) instead.
func (h *Host) checkFiles(t *tool, args json.RawMessage) ([]UploadSpec, error) {
	if len(t.files) == 0 {
		return nil, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(args, &m); err != nil {
		return nil, nil // the handler reports malformed arguments itself
	}
	if h.cfg.MaxInlineBytes < 0 {
		for name := range t.files {
			if _, ok := m[name]; ok {
				return nil, errors.New("this server does not accept files")
			}
		}
		return nil, nil
	}
	names := make([]string, 0, len(t.files))
	for name := range t.files {
		names = append(names, name)
	}
	sort.Strings(names)
	var total int64
	var specs []UploadSpec
	ids := map[string]bool{}
	for _, name := range names {
		desc := t.files[name]
		raw, ok := m[name]
		if !ok || bytes.Equal(raw, []byte("null")) {
			continue
		}
		f, sp, err := h.decodeArg(t, name, desc, raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if ids[f.ID] {
			return nil, fmt.Errorf("%s: duplicate file id %q", name, f.ID)
		}
		ids[f.ID] = true
		size := int64(len(f.Data))
		if sp != nil {
			size = sp.Size
		}
		if desc.MaxSize > 0 && size > desc.MaxSize {
			return nil, fmt.Errorf("%s: file is %d bytes, over the %d-byte limit", name, size, desc.MaxSize)
		}
		if !accepts(desc.Accept, f) {
			return nil, fmt.Errorf("%s: file type %q is not accepted (want %s)", name, f.MimeType, strings.Join(desc.Accept, ", "))
		}
		if sp != nil {
			specs = append(specs, *sp)
			continue
		}
		total += size
	}
	if total > h.cfg.MaxInlineBytes {
		return nil, fmt.Errorf("inline files total %d bytes, over the %d-byte limit", total, h.cfg.MaxInlineBytes)
	}
	return specs, nil
}

// decodeArg decodes one file argument: inline via DecodeFile, or, when
// it qualifies for an out-of-band upload, into an UploadSpec with File
// carrying its claimed id, name and type (no data).
func (h *Host) decodeArg(t *tool, name string, desc FileInputDescriptor, raw json.RawMessage) (File, *UploadSpec, error) {
	var in fileInput
	if h.uploads != nil && t.call && json.Unmarshal(raw, &in) == nil && len(in.Data) == 0 &&
		in.Size != nil && *in.Size > 0 && len(in.ID) >= 1 && len(in.ID) <= 128 && in.MimeType != "" {
		sp := &UploadSpec{Tool: t.name, Param: name, FileID: in.ID, Name: in.Name, MimeType: in.MimeType,
			Size: *in.Size, MaxSize: desc.MaxSize, Accept: desc.Accept}
		if in.Digest != nil {
			if in.Digest.Algorithm != "sha-256" {
				return File{}, nil, fmt.Errorf("unsupported digest algorithm %q", in.Digest.Algorithm)
			}
			sp.Digest = in.Digest.Value
		}
		return File{ID: in.ID, Name: in.Name, MimeType: in.MimeType}, sp, nil
	}
	f, err := DecodeFile(raw)
	return f, nil, err
}

// filesCapability is the extension's server settings, or nil when the
// extension is switched off.
func (h *Host) filesCapability() map[string]any {
	if h.cfg.MaxInlineBytes < 0 {
		return nil
	}
	c := map[string]any{"maxInlineBytes": h.cfg.MaxInlineBytes}
	if h.uploads != nil {
		c["upload"] = map[string]any{}
	}
	return c
}

// clientDeclaresFiles reports whether a request's params declare the
// files extension in _meta, and whether its settings include download.
// Any settings object counts as a declaration: an inline-only client
// declares empty settings.
func clientDeclaresFiles(params json.RawMessage) (declared, download bool) {
	var p struct {
		Meta map[string]struct {
			Extensions map[string]json.RawMessage `json:"extensions"`
		} `json:"_meta"`
	}
	if json.Unmarshal(params, &p) != nil {
		return false, false
	}
	s, ok := p.Meta[ClientCapabilitiesMeta].Extensions[FilesExtension]
	if !ok || bytes.Equal(s, []byte("null")) {
		return false, false
	}
	var set map[string]json.RawMessage
	_ = json.Unmarshal(s, &set)
	d, ok := set["download"]
	return true, ok && !bytes.Equal(d, []byte("null"))
}
