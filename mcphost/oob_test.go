package mcphost

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

type fakeUploads struct {
	target    UploadTarget
	targetErr error
	finish    func(spec UploadSpec, ref string) (UploadedFile, error)
	specs     []UploadSpec
}

func (f *fakeUploads) UploadTarget(_ string, s UploadSpec) (UploadTarget, error) {
	f.specs = append(f.specs, s)
	t := f.target
	t.Ref = "ref-" + s.FileID
	return t, f.targetErr
}

func (f *fakeUploads) FinishUpload(_ string, s UploadSpec, ref string) (UploadedFile, error) {
	if f.finish != nil {
		return f.finish(s, ref)
	}
	return UploadedFile{Size: s.Size}, nil
}

func oobHost(t *testing.T) (*Host, *fakeUploads, *[]Call) {
	t.Helper()
	h, _ := fileHost(t, 0)
	up := &fakeUploads{target: UploadTarget{URL: "https://files.example/u/1", ExpiresAt: time.Unix(2000000000, 0)}}
	h.SetUploads(up)
	var calls []Call
	h.ToolCall("send", "", map[string]any{"type": "object", "properties": map[string]any{
		"a": FileSchema(FileInputDescriptor{Accept: []string{"text/*"}, MaxSize: 100}, ""),
		"b": FileSchema(FileInputDescriptor{}, ""),
	}}, func(c Call) (Result, error) {
		calls = append(calls, c)
		return Result{Text: "done"}, nil
	})
	return h, up, &calls
}

func oobArg(id, name, mt string, size int) string {
	return fmt.Sprintf(`{"id":%q,"name":%q,"mimeType":%q,"size":%d}`, id, name, mt, size)
}

func sendParams(args, state string) json.RawMessage {
	p := map[string]any{"name": "send", "arguments": json.RawMessage(args)}
	if state != "" {
		p["requestState"] = state
	}
	b, _ := json.Marshal(p)
	return b
}

func errText(t *testing.T, r map[string]any) string {
	t.Helper()
	if r["isError"] != true {
		t.Fatalf("want isError, got %v", r)
	}
	return r["content"].([]any)[0].(map[string]any)["text"].(string)
}

func TestOOB_Capability(t *testing.T) {
	h, _, _ := oobHost(t)
	if _, ok := h.filesCapability()["upload"]; !ok {
		t.Fatal("upload not advertised")
	}
	r := driveMCP(t, h, `{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{}}`)
	res := r[0]["result"].(map[string]any)
	if res["supportedVersions"].([]any)[0] != statelessProtocolVersion || res["_meta"] == nil {
		t.Fatalf("discover=%v", res)
	}
}

func TestOOB_RoundTrip(t *testing.T) {
	h, up, calls := oobHost(t)
	args := `{"a":` + oobArg("f1", "r.txt", "text/plain", 50) + `,"b":` + fi("f2", "x.bin", "application/x", "hi") + `}`
	r := h.handleToolCall(sendParams(args, ""), "k")
	if r["resultType"] != "upload_required" {
		t.Fatalf("got %v", r)
	}
	ups := r["uploads"].([]any)
	u := ups[0].(map[string]any)
	if len(ups) != 1 || u["fileId"] != "f1" || u["method"] != "PUT" || u["credentials"] != "omit" || u["expiresAt"] != "2033-05-18T03:33:20Z" {
		t.Fatalf("uploads=%v", ups)
	}
	if up.specs[0].Size != 50 || up.specs[0].Param != "a" || up.specs[0].Tool != "send" {
		t.Fatalf("spec=%+v", up.specs[0])
	}
	state := r["requestState"].(string)
	// Key order does not matter: the args hash is canonical.
	reordered := `{"b":` + fi("f2", "x.bin", "application/x", "hi") + `,"a":` + oobArg("f1", "r.txt", "text/plain", 50) + `}`
	r = h.handleToolCall(sendParams(reordered, state), "k")
	if r["isError"] == true || len(*calls) != 1 {
		t.Fatalf("retry: %v", r)
	}
	f := (*calls)[0].Files["a"]
	if f.Ref != "ref-f1" || f.Name != "r.txt" || f.MimeType != "text/plain" || f.Size != 50 {
		t.Fatalf("file=%+v", f)
	}
	// Replay is refused.
	if s := errText(t, h.handleToolCall(sendParams(args, state), "k")); !strings.Contains(s, "already used") {
		t.Fatal(s)
	}
}

func TestOOB_StateMismatch(t *testing.T) {
	h, _, _ := oobHost(t)
	args := `{"a":` + oobArg("f1", "r.txt", "text/plain", 50) + `}`
	state := h.handleToolCall(sendParams(args, ""), "k")["requestState"].(string)
	for name, tc := range map[string]struct{ args, state, sk string }{
		"session": {args, state, "other"},
		"args":    {`{"a":` + oobArg("f1", "r.txt", "text/plain", 51) + `}`, state, "k"},
		"files":   {`{"a":` + oobArg("f1", "r.txt", "text/plain", 50) + `,"b":` + oobArg("f9", "y", "a/b", 3) + `}`, state, "k"},
	} {
		h2, _, _ := oobHost(t)
		h2.stateKey = h.stateKey
		if s := errText(t, h2.handleToolCall(sendParams(tc.args, tc.state), tc.sk)); !strings.Contains(s, "does not match") {
			t.Fatalf("%s: %s", name, s)
		}
	}
	// Same param count but a different file id.
	h3, _, _ := oobHost(t)
	h3.stateKey = h.stateKey
	b, _ := base64.RawURLEncoding.DecodeString(strings.Split(state, ".")[0])
	var st uploadState
	_ = json.Unmarshal(b, &st)
	st.Files[0].ID = "zz"
	if s := errText(t, h3.handleToolCall(sendParams(args, h3.sign(st)), "k")); !strings.Contains(s, "does not match") {
		t.Fatal(s)
	}
	for _, bad := range []string{"nodot", "a.b", state + "x"} {
		if s := errText(t, h.handleToolCall(sendParams(args, bad), "k")); !strings.Contains(s, "invalid or expired") {
			t.Fatal(s)
		}
	}
	// Expired.
	old := timeNow
	timeNow = func() time.Time { return old().Add(2 * UploadStateTTL) }
	s := errText(t, h.handleToolCall(sendParams(args, state), "k"))
	timeNow = old
	if !strings.Contains(s, "invalid or expired") {
		t.Fatal(s)
	}
	// requestState with no out-of-band file.
	if s := errText(t, h.handleToolCall(sendParams(`{}`, state), "k")); !strings.Contains(s, "unexpected requestState") {
		t.Fatal(s)
	}
	// None of the refusals spent the state.
	errText(t, h.handleToolCall(sendParams(args, state), "other"))
	if r := h.handleToolCall(sendParams(args, state), "k"); r["isError"] == true {
		t.Fatalf("state was spent by a refused call: %v", r)
	}
}

func TestOOB_SpentNoncesExpire(t *testing.T) {
	h, _, _ := oobHost(t)
	h.spent["gone"] = 1
	args := `{"a":` + oobArg("f1", "r.txt", "text/plain", 5) + `}`
	state := h.handleToolCall(sendParams(args, ""), "k")["requestState"].(string)
	h.handleToolCall(sendParams(args, state), "k")
	if _, ok := h.spent["gone"]; ok {
		t.Fatal("expired nonce kept")
	}
}

func TestOOB_FinishFailures(t *testing.T) {
	cases := map[string]struct {
		uf   UploadedFile
		err  error
		want string
	}{
		"err":    {err: errors.New("nothing"), want: "upload not received: nothing"},
		"size":   {uf: UploadedFile{Size: 4}, want: "uploaded 4 bytes"},
		"accept": {uf: UploadedFile{Size: 5, MimeType: "image/png"}, want: "not accepted"},
	}
	for name, tc := range cases {
		h, up, _ := oobHost(t)
		up.finish = func(UploadSpec, string) (UploadedFile, error) { return tc.uf, tc.err }
		args := `{"a":` + oobArg("f1", "r.txt", "text/plain", 5) + `}`
		state := h.handleToolCall(sendParams(args, ""), "k")["requestState"].(string)
		if s := errText(t, h.handleToolCall(sendParams(args, state), "k")); !strings.Contains(s, tc.want) {
			t.Fatalf("%s: %s", name, s)
		}
	}
	// A provider-reported ref, name and type win.
	h, up, calls := oobHost(t)
	up.finish = func(UploadSpec, string) (UploadedFile, error) {
		return UploadedFile{Ref: "R", Name: "n.txt", MimeType: "text/csv", Size: 5}, nil
	}
	args := `{"a":` + oobArg("f1", "r.txt", "text/plain", 5) + `}`
	state := h.handleToolCall(sendParams(args, ""), "k")["requestState"].(string)
	h.handleToolCall(sendParams(args, state), "k")
	if f := (*calls)[0].Files["a"]; f.Ref != "R" || f.Name != "n.txt" || f.MimeType != "text/csv" {
		t.Fatalf("%+v", f)
	}
}

func TestOOB_ClaimValidation(t *testing.T) {
	h, _, _ := oobHost(t)
	for args, want := range map[string]string{
		`{"a":` + oobArg("f1", "r.txt", "text/plain", 500) + `}`:                                       "over the 100-byte limit",
		`{"a":` + oobArg("f1", "r.png", "image/png", 5) + `}`:                                          "not accepted",
		`{"a":` + oobArg("f1", "r.txt", "text/plain", 5) + `,"b":` + oobArg("f1", "y", "a/b", 3) + `}`: "duplicate file id",
		`{"a":{"id":"f1","mimeType":"text/plain","size":5,"digest":{"algorithm":"md5","value":"x"}}}`:  "unsupported digest",
	} {
		if s := errText(t, h.handleToolCall(sendParams(args, ""), "k")); !strings.Contains(s, want) {
			t.Fatalf("%s: %s", args, s)
		}
	}
	// A digest is passed through.
	h2, up, _ := oobHost(t)
	h2.handleToolCall(sendParams(`{"b":{"id":"f1","mimeType":"a/b","size":5,"digest":{"algorithm":"sha-256","value":"DG"}}}`, ""), "k")
	if up.specs[0].Digest != "DG" {
		t.Fatalf("%+v", up.specs)
	}
	// A plain Tool never gets out-of-band files.
	r := driveMCP(t, h, call(`{"b":`+oobArg("f1", "y", "a/b", 3)+`}`))
	if s := errText(t, r[0]["result"].(map[string]any)); !strings.Contains(s, "data is missing") {
		t.Fatal(s)
	}
}

func TestOOB_TargetErrors(t *testing.T) {
	args := `{"b":` + oobArg("f1", "y", "a/b", 3) + `}`
	h, up, _ := oobHost(t)
	up.targetErr = errors.New("quota")
	if s := errText(t, h.handleToolCall(sendParams(args, ""), "k")); !strings.Contains(s, "quota") {
		t.Fatal(s)
	}
	up.targetErr = nil
	up.target = UploadTarget{URL: "http://x"}
	if s := errText(t, h.handleToolCall(sendParams(args, ""), "k")); !strings.Contains(s, "bad upload target") {
		t.Fatal(s)
	}
}

func TestUploadWire(t *testing.T) {
	form := &UploadForm{Fields: []FormField{{"k", "v"}}, FileField: "file"}
	ok := []UploadTarget{
		{URL: "https://h/p", Method: "POST", Form: form, Headers: map[string]string{"X-A": "1"}},
		{URL: "https://h/p", Method: "PUT"},
	}
	for _, tg := range ok {
		w, err := uploadWire("id", tg)
		if err != nil || w["fileId"] != "id" {
			t.Fatalf("%+v: %v %v", tg, w, err)
		}
	}
	bad := []UploadTarget{
		{URL: "://"},
		{URL: "https://"},
		{URL: "https://u:p@h/"},
		{URL: "https://h/", Method: "GET"},
		{URL: "https://h/", Form: form},
		{URL: "https://h/", Method: "POST", Form: &UploadForm{}},
		{URL: "https://h/", Headers: map[string]string{"Authorization": "x"}},
		{URL: "https://h/", Headers: map[string]string{"Sec-Fetch": "x"}},
		{URL: "https://h/", Headers: map[string]string{"Proxy-X": "x"}},
		{URL: "https://h/", Method: "POST", Form: form, Headers: map[string]string{"content-type": "x"}},
		{URL: "https://h/", Headers: map[string]string{"X": "a\r\nb"}},
		{URL: "https://h/", Headers: map[string]string{"": "a"}},
		{URL: "https://h/", Headers: map[string]string{"A B": "a"}},
		{URL: "https://h/", Ref: strings.Repeat("r", MaxUploadRefBytes+1)},
	}
	for _, tg := range bad {
		if _, err := uploadWire("id", tg); err == nil {
			t.Fatalf("accepted %+v", tg)
		}
	}
}

func TestDownloads(t *testing.T) {
	h, _ := fileHost(t, 0)
	targets := []DownloadTarget{
		{URL: "https://h/a", Name: "a.txt", MimeType: "text/plain", Size: 3, Digest: "D",
			Headers: map[string]string{"X-K": "v"}, ExpiresAt: time.Unix(0, 0)},
		{URL: "https://h/b", Size: 1},
		{URL: "http://h/c", Name: "c"},
		{URL: "https://h/d", Name: "d", Size: -1},
		{URL: "https://h/e", Name: "e", Headers: map[string]string{"Cookie": "x"}},
	}
	h.ToolCall("dl", "", map[string]any{"type": "object"}, func(Call) (Result, error) {
		return Result{Text: "here", Downloads: targets}, nil
	})
	h.ToolCall("none", "", map[string]any{"type": "object"}, func(Call) (Result, error) {
		return Result{Text: "x"}, nil
	})
	h.ToolCall("fail", "", map[string]any{"type": "object"}, func(Call) (Result, error) {
		return Result{}, errors.New("boom")
	})
	dl := `"_meta":{"io.modelcontextprotocol/clientCapabilities":{"extensions":{"io.modelcontextprotocol/files":{"download":{}}}}}`
	r := driveMCP(t, h,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"dl",`+dl+`}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"dl",`+declare+`}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"none"}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"fail"}}`,
	)
	res := r[0]["result"].(map[string]any)
	ds := res["_meta"].(map[string]any)[FilesExtension].(map[string]any)["downloads"].([]any)
	if len(ds) != 2 {
		t.Fatalf("downloads=%v", ds)
	}
	a, b := ds[0].(map[string]any), ds[1].(map[string]any)
	if a["credentials"] != "omit" || a["digest"].(map[string]any)["algorithm"] != "sha-256" || a["expiresAt"] != "1970-01-01T00:00:00Z" || a["headers"] == nil {
		t.Fatalf("a=%v", a)
	}
	if b["name"] != "file" || b["mimeType"] != "application/octet-stream" {
		t.Fatalf("b=%v", b)
	}
	if c := res["content"].([]any); len(c) != 2 || strings.Count(c[1].(map[string]any)["text"].(string), "unavailable") != 3 {
		t.Fatalf("content=%v", c)
	}
	res = r[1]["result"].(map[string]any)
	if res["_meta"] != nil || !strings.Contains(res["content"].([]any)[1].(map[string]any)["text"].(string), "does not accept MCP file downloads") {
		t.Fatalf("undeclared=%v", res)
	}
	if r[2]["result"].(map[string]any)["_meta"] != nil {
		t.Fatal("meta without downloads")
	}
	errText(t, r[3]["result"].(map[string]any))
}

func TestToolCallReplace(t *testing.T) {
	h, _ := fileHost(t, 0)
	h.ToolCall("plain", "v2", map[string]any{"type": "object"}, func(Call) (Result, error) { return Result{}, nil })
	if h.toolsIn[0].description != "v2" || len(h.toolsIn) != 2 {
		t.Fatalf("%+v", h.toolsIn)
	}
}

func TestStateKeySurvivesExport(t *testing.T) {
	h, _, _ := oobHost(t)
	args := `{"a":` + oobArg("f1", "r.txt", "text/plain", 5) + `}`
	state := h.handleToolCall(sendParams(args, ""), "k")["requestState"].(string)
	h.spent["used"] = time.Now().Add(time.Hour).Unix()
	blob := h.ExportTokens()
	h2, _, calls := oobHost(t)
	if err := h2.SeedTokens(blob); err != nil {
		t.Fatal(err)
	}
	if _, ok := h2.spent["used"]; !ok {
		t.Fatal("spent not carried")
	}
	h2.handleToolCall(sendParams(args, state), "k")
	if len(*calls) != 1 {
		t.Fatal("state from predecessor rejected")
	}
}
