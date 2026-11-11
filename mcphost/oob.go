package mcphost

// Out-of-band transfers for the io.modelcontextprotocol/files extension.
//
// mcphost is reached over stdio through a local unix socket, so it has no
// https endpoint of its own to hand a client. A relay usually does: its
// chat platform hosts files at public https URLs (Slack's
// files.getUploadURLExternal, a Zulip temporary signed URL, a Poe
// attachment URL, or a small relay-hosted signed endpoint). The two
// providers below let the relay plug those in:
//
//   - UploadProvider (Host.SetUploads) turns a file argument the client
//     sent without data into an upload_required result: the provider
//     mints a scoped, short-lived https target per file, the Host signs a
//     requestState binding the call, and on the client's retry the Host
//     verifies that state and asks the provider to finish the upload. The
//     tool then runs with an UploadedFile handle (Call.Files) instead of
//     bytes.
//   - Result.Downloads lets a tool hand back download targets — absolute
//     https URLs the platform already serves — in the result's _meta.
//
// Targets must carry no ambient credentials: they are emitted with
// credentials "omit", and Authorization, Cookie and the other forbidden
// request headers are refused, so a provider cannot leak the relay's own
// API token to the client by accident. A platform that needs auth on
// upload must mint a one-shot target instead (see docs/files-oob.md).

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// UploadSpec describes one file the client wants to upload out of band.
// Every field except Tool, Param, MaxSize and Accept is the client's
// claim and untrusted.
type UploadSpec struct {
	Tool     string // tool being called
	Param    string // x-mcp-file argument name
	FileID   string // client-chosen FileInput id
	Name     string // untrusted file name; may be empty
	MimeType string
	Size     int64  // claimed size in bytes, > 0
	Digest   string // claimed sha-256, base64url without padding; may be empty
	MaxSize  int64  // the argument's x-mcp-file maxSize; 0 = none
	Accept   []string
}

// FormField is one multipart form field sent before the file part.
type FormField struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// UploadForm turns an upload into a multipart/form-data POST.
type UploadForm struct {
	Fields    []FormField `json:"fields,omitempty"`
	FileField string      `json:"fileField"`
}

// UploadTarget is where the client sends one file's bytes.
type UploadTarget struct {
	// URL is an absolute https URL. It is a bearer capability: make it
	// single-use and short-lived.
	URL string
	// Method is PUT (default) or POST.
	Method string
	// Headers are sent with the upload. Credential headers are refused.
	Headers map[string]string
	// Form, when set, makes the upload a multipart POST.
	Form *UploadForm
	// ExpiresAt, when non-zero, is when the target stops working.
	ExpiresAt time.Time
	// Ref is the provider's own handle for this upload (a file id, a
	// storage key). It travels inside the signed requestState and is
	// handed back to FinishUpload; at most MaxUploadRefBytes long. It is
	// readable by the client, so do not put secrets in it.
	Ref string
}

// MaxUploadRefBytes caps UploadTarget.Ref.
const MaxUploadRefBytes = 1024

// UploadedFile is a file the platform holds after an out-of-band upload.
type UploadedFile struct {
	Ref      string // the provider's handle, as returned by FinishUpload
	Name     string // untrusted; reduce to a base name before use
	MimeType string // untrusted
	Size     int64  // the size the platform actually stored
}

// UploadProvider is the relay side of out-of-band uploads. sessionKey is
// the one mcphost resolved from the connection's token.
type UploadProvider interface {
	// UploadTarget mints a target for one file.
	UploadTarget(sessionKey string, spec UploadSpec) (UploadTarget, error)
	// FinishUpload confirms the client's upload of spec to the target
	// whose Ref was ref, and reports what the platform stored. It must
	// fail when nothing was uploaded.
	FinishUpload(sessionKey string, spec UploadSpec, ref string) (UploadedFile, error)
}

// DownloadTarget is a file a tool hands back for the client to fetch.
type DownloadTarget struct {
	URL       string // absolute https URL, fetched with credentials "omit"
	Name      string // shown to the user; "file" when empty
	MimeType  string // "application/octet-stream" when empty
	Size      int64  // exact size in bytes
	Digest    string // optional sha-256, base64url without padding
	Headers   map[string]string
	ExpiresAt time.Time // zero = no expiry given
}

// Call is one tools/call as a CallHandler sees it.
type Call struct {
	SessionKey string
	Args       json.RawMessage
	// Files holds the x-mcp-file arguments uploaded out of band, by
	// argument name. Inline ones stay in Args (see DecodeFile).
	Files map[string]UploadedFile
}

// Result is a CallHandler's success value.
type Result struct {
	Text      string
	Downloads []DownloadTarget
}

// CallHandler is the richer form of Handler: it sees out-of-band
// uploads and may return download targets. Register it with ToolCall.
type CallHandler func(Call) (Result, error)

// UploadStateTTL is how long a requestState stays valid.
var UploadStateTTL = 30 * time.Minute

// SetUploads installs the upload provider and advertises `upload` in the
// files extension settings. Call it before Listen. Only tools registered
// with ToolCall accept out-of-band files; a nil p switches uploads off.
func (h *Host) SetUploads(p UploadProvider) { h.uploads = p }

// forbiddenHeaders are request headers a target may not set: fetch's
// forbidden headers plus credentials and method overrides.
var forbiddenHeaders = map[string]bool{
	"authorization": true, "proxy-authorization": true, "cookie": true, "cookie2": true,
	"set-cookie": true, "host": true, "content-length": true, "connection": true,
	"transfer-encoding": true, "te": true, "trailer": true, "upgrade": true, "expect": true,
	"keep-alive": true, "origin": true, "referer": true, "via": true, "forwarded": true,
	"x-http-method": true, "x-http-method-override": true, "x-method-override": true,
}

func checkTargetURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return errors.New("url must be an absolute https URL")
	}
	if u.User != nil {
		return errors.New("url must not carry credentials")
	}
	return nil
}

func checkHeaders(hs map[string]string, form bool) error {
	for k, v := range hs {
		lk := strings.ToLower(k)
		if forbiddenHeaders[lk] || strings.HasPrefix(lk, "sec-") || strings.HasPrefix(lk, "proxy-") ||
			(form && lk == "content-type") {
			return fmt.Errorf("header %q is not allowed", k)
		}
		if k == "" || strings.ContainsAny(k+v, "\r\n\x00") || strings.ContainsAny(k, " :") {
			return fmt.Errorf("malformed header %q", k)
		}
	}
	return nil
}

func rfc3339(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// uploadWire validates t and returns its UploadTarget wire form.
func uploadWire(fileID string, t UploadTarget) (map[string]any, error) {
	if err := checkTargetURL(t.URL); err != nil {
		return nil, err
	}
	switch t.Method {
	case "":
		t.Method = http.MethodPut
	case http.MethodPut, http.MethodPost:
	default:
		return nil, errors.New("method must be PUT or POST")
	}
	if t.Form != nil && (t.Method != http.MethodPost || t.Form.FileField == "") {
		return nil, errors.New("a form upload needs POST and a fileField")
	}
	if err := checkHeaders(t.Headers, t.Form != nil); err != nil {
		return nil, err
	}
	if len(t.Ref) > MaxUploadRefBytes {
		return nil, errors.New("ref is too long")
	}
	w := map[string]any{"fileId": fileID, "url": t.URL, "method": t.Method, "credentials": "omit"}
	if len(t.Headers) > 0 {
		w["headers"] = t.Headers
	}
	if t.Form != nil {
		w["form"] = t.Form
	}
	if !t.ExpiresAt.IsZero() {
		w["expiresAt"] = rfc3339(t.ExpiresAt)
	}
	return w, nil
}

// downloadWire validates d and returns its DownloadTarget wire form.
func downloadWire(d DownloadTarget) (map[string]any, error) {
	if err := checkTargetURL(d.URL); err != nil {
		return nil, err
	}
	if d.Size < 0 {
		return nil, errors.New("size must not be negative")
	}
	if err := checkHeaders(d.Headers, false); err != nil {
		return nil, err
	}
	if d.Name == "" {
		d.Name = "file"
	}
	if d.MimeType == "" {
		d.MimeType = "application/octet-stream"
	}
	w := map[string]any{"url": d.URL, "name": d.Name, "mimeType": d.MimeType, "size": d.Size, "credentials": "omit"}
	if d.Digest != "" {
		w["digest"] = FileDigest{Algorithm: "sha-256", Value: d.Digest}
	}
	if len(d.Headers) > 0 {
		w["headers"] = d.Headers
	}
	if !d.ExpiresAt.IsZero() {
		w["expiresAt"] = rfc3339(d.ExpiresAt)
	}
	return w, nil
}

// ---- requestState ----------------------------------------------------------

// uploadState is the signed payload of a requestState.
type uploadState struct {
	V       int         `json:"v"`
	Tool    string      `json:"t"`
	Session string      `json:"s"` // sha-256 of the session key
	Args    string      `json:"a"` // sha-256 of the canonical arguments
	Exp     int64       `json:"e"` // unix seconds
	Nonce   string      `json:"n"`
	Files   []stateFile `json:"f"`
}

type stateFile struct {
	Param string `json:"p"`
	ID    string `json:"i"`
	Ref   string `json:"r"`
}

func hash64(b []byte) string {
	s := sha256.Sum256(b)
	return base64.RawURLEncoding.EncodeToString(s[:])
}

// argsHash hashes the arguments in canonical form (object keys sorted by
// re-marshalling), so the retry matches however the client orders keys.
// Arguments that do not parse never reach here: checkFiles has run.
func argsHash(args json.RawMessage) string {
	var v any
	_ = json.Unmarshal(args, &v)
	b, _ := json.Marshal(v)
	return hash64(b)
}

func (h *Host) sign(st uploadState) string {
	b, _ := json.Marshal(st)
	p := base64.RawURLEncoding.EncodeToString(b)
	m := hmac.New(sha256.New, h.stateKey)
	m.Write([]byte(p))
	return p + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

var (
	errBadState      = errors.New("invalid or expired requestState; send the file again")
	errStateMismatch = errors.New("requestState does not match this call; send the file again")
)

// verify checks a requestState's signature and expiry, and that it was
// issued for this call, then spends it so it cannot be used again.
func (h *Host) verify(s, sessionKey string, t *tool, args json.RawMessage, specs []UploadSpec) (uploadState, error) {
	var st uploadState
	p, sig, ok := strings.Cut(s, ".")
	if !ok {
		return st, errBadState
	}
	m := hmac.New(sha256.New, h.stateKey)
	m.Write([]byte(p))
	want := base64.RawURLEncoding.EncodeToString(m.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(sig), []byte(want)) != 1 {
		return st, errBadState
	}
	b, _ := base64.RawURLEncoding.DecodeString(p) // signed by us: decodes
	_ = json.Unmarshal(b, &st)
	now := timeNow()
	if st.V != 1 || now.Unix() > st.Exp {
		return st, errBadState
	}
	if st.Tool != t.name || st.Session != hash64([]byte(sessionKey)) || st.Args != argsHash(args) || len(st.Files) != len(specs) {
		return st, errStateMismatch
	}
	for i, sp := range specs { // both sorted by argument name
		if st.Files[i].Param != sp.Param || st.Files[i].ID != sp.FileID {
			return st, errStateMismatch
		}
	}
	h.stateMu.Lock()
	defer h.stateMu.Unlock()
	for n, exp := range h.spent {
		if now.Unix() > exp {
			delete(h.spent, n)
		}
	}
	if _, used := h.spent[st.Nonce]; used {
		return st, errors.New("requestState was already used; send the file again")
	}
	h.spent[st.Nonce] = st.Exp
	return st, nil
}

// timeNow is time.Now, indirected for expiry tests.
var timeNow = time.Now

// requestUploads answers a call carrying out-of-band files with
// upload_required.
func (h *Host) requestUploads(sessionKey string, t *tool, args json.RawMessage, specs []UploadSpec) map[string]any {
	st := uploadState{V: 1, Tool: t.name, Session: hash64([]byte(sessionKey)), Args: argsHash(args),
		Exp: timeNow().Add(UploadStateTTL).Unix(), Nonce: newToken()}
	ups := make([]any, 0, len(specs))
	for _, sp := range specs {
		tgt, err := h.uploads.UploadTarget(sessionKey, sp)
		if err != nil {
			return toolError(fmt.Sprintf("%s: cannot accept this file out of band: %v", sp.Param, err))
		}
		w, err := uploadWire(sp.FileID, tgt)
		if err != nil {
			return toolError(fmt.Sprintf("%s: the relay issued a bad upload target: %v", sp.Param, err))
		}
		ups = append(ups, w)
		st.Files = append(st.Files, stateFile{Param: sp.Param, ID: sp.FileID, Ref: tgt.Ref})
	}
	return map[string]any{"resultType": "upload_required", "uploads": ups, "requestState": h.sign(st)}
}

// resume verifies a retry's requestState against the call and finishes
// every upload it names.
func (h *Host) resume(sessionKey string, t *tool, args json.RawMessage, specs []UploadSpec, state string) (map[string]UploadedFile, error) {
	st, err := h.verify(state, sessionKey, t, args, specs)
	if err != nil {
		return nil, err
	}
	files := make(map[string]UploadedFile, len(specs))
	for i, sp := range specs {
		sf := st.Files[i]
		uf, err := h.uploads.FinishUpload(sessionKey, sp, sf.Ref)
		if err != nil {
			return nil, fmt.Errorf("%s: upload not received: %w", sp.Param, err)
		}
		if uf.Size != sp.Size {
			return nil, fmt.Errorf("%s: uploaded %d bytes, the file is %d", sp.Param, uf.Size, sp.Size)
		}
		if uf.Name == "" {
			uf.Name = sp.Name
		}
		if uf.MimeType == "" {
			uf.MimeType = sp.MimeType
		}
		if uf.Ref == "" {
			uf.Ref = sf.Ref
		}
		if !accepts(sp.Accept, File{Name: uf.Name, MimeType: uf.MimeType}) {
			return nil, fmt.Errorf("%s: uploaded file type %q is not accepted", sp.Param, uf.MimeType)
		}
		files[sp.Param] = uf
	}
	return files, nil
}

// withDownloads adds res's download targets to out's _meta, or a note
// per target when the client cannot take them or one is malformed.
func withDownloads(out map[string]any, res Result, clientOK bool) map[string]any {
	if len(res.Downloads) == 0 {
		return out
	}
	var targets []any
	var notes []string
	for _, d := range res.Downloads {
		w, err := downloadWire(d)
		switch {
		case err != nil:
			notes = append(notes, fmt.Sprintf("[file %q unavailable: %v]", d.Name, err))
		case !clientOK:
			notes = append(notes, fmt.Sprintf("[file %q unavailable: this client does not accept MCP file downloads]", w["name"]))
		default:
			targets = append(targets, w)
		}
	}
	if len(notes) > 0 {
		out["content"] = append(out["content"].([]any), map[string]any{"type": "text", "text": strings.Join(notes, "\n")})
	}
	if len(targets) > 0 {
		out["_meta"] = map[string]any{FilesExtension: map[string]any{"downloads": targets}}
	}
	return out
}
