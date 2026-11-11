package mcphost

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

const declare = `"_meta":{"io.modelcontextprotocol/clientCapabilities":{"extensions":{"io.modelcontextprotocol/files":{}}}}`

func fileHost(t *testing.T, maxInline int64) (*Host, *[]File) {
	t.Helper()
	h, err := New(Config{
		BaseDir: "/tmp", DirPrefix: "mh", RedirSubcommand: "s", ServerName: "test",
		EnvSocket: "S", EnvToken: "T", RedirCommand: "/bin/true", MaxInlineBytes: maxInline,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	var got []File
	h.Tool("plain", "", map[string]any{"type": "object"}, noopHandler)
	h.Tool("up", "", map[string]any{
		"type": "object",
		"properties": map[string]any{
			"a":     FileSchema(FileInputDescriptor{Accept: []string{"text/*", ".pdf", "image/png"}, MaxSize: 10}, "first"),
			"b":     FileSchema(FileInputDescriptor{}, ""),
			"other": map[string]any{"type": "string"},
			"odd":   "not a map",
		},
	}, func(_ string, args json.RawMessage) (string, error) {
		var a struct{ A json.RawMessage }
		_ = json.Unmarshal(args, &a)
		f, err := DecodeFile(a.A)
		if err != nil {
			return "", err
		}
		got = append(got, f)
		return "ok", nil
	})
	return h, &got
}

func fi(id, name, mt string, data string) string {
	return fmt.Sprintf(`{"id":%q,"name":%q,"mimeType":%q,"size":%d,"data":%q}`,
		id, name, mt, len(data), base64.StdEncoding.EncodeToString([]byte(data)))
}

func call(args string) string {
	return `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"up","arguments":` + args + `}}`
}

func toolNames(r map[string]any) []string {
	var out []string
	for _, x := range r["result"].(map[string]any)["tools"].([]any) {
		out = append(out, x.(map[string]any)["name"].(string))
	}
	return out
}

func TestFiles_DiscoverAndList(t *testing.T) {
	h, _ := fileHost(t, 0)
	r := driveMCP(t, h,
		`{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/list","params":{`+declare+`}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/clientCapabilities":{"extensions":{"io.modelcontextprotocol/files":null}}}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/list","params":"bad"}`,
	)
	ext := r[0]["result"].(map[string]any)["capabilities"].(map[string]any)["extensions"].(map[string]any)
	if ext[FilesExtension].(map[string]any)["maxInlineBytes"].(float64) != float64(DefaultMaxInlineBytes) {
		t.Fatalf("ext=%v", ext)
	}
	if n := toolNames(r[1]); len(n) != 1 || n[0] != "plain" {
		t.Fatalf("undeclared list = %v", n)
	}
	if n := toolNames(r[2]); len(n) != 2 {
		t.Fatalf("declared list = %v", n)
	}
	if n := toolNames(r[3]); len(n) != 1 {
		t.Fatalf("null-declared list = %v", n)
	}
	if n := toolNames(r[4]); len(n) != 1 {
		t.Fatalf("bad params list = %v", n)
	}
}

func TestFiles_Disabled(t *testing.T) {
	h, got := fileHost(t, -1)
	r := driveMCP(t, h,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{`+declare+`}}`,
		call(`{"a":`+fi("1", "x.txt", "text/plain", "hi")+`}`),
		call(`{"other":"x"}`),
	)
	if _, ok := r[0]["result"].(map[string]any)["capabilities"].(map[string]any)["extensions"]; ok {
		t.Fatal("extension advertised while disabled")
	}
	if n := toolNames(r[1]); len(n) != 1 {
		t.Fatalf("list = %v", n)
	}
	if r[2]["result"].(map[string]any)["isError"] != true || len(*got) != 0 {
		t.Fatalf("file accepted while disabled: %v", r[2])
	}
	if r[3]["result"].(map[string]any)["isError"] != true {
		// no file → handler runs and fails decoding the absent file
		t.Fatalf("want handler error: %v", r[3])
	}
}

func TestFiles_CallValidation(t *testing.T) {
	h, got := fileHost(t, 12)
	sum := sha256.Sum256([]byte("hi"))
	dg := base64.RawURLEncoding.EncodeToString(sum[:])
	cases := []struct {
		args string
		ok   bool
		msg  string
	}{
		{`{"a":` + fi("1", "x.txt", "text/plain", "hi") + `}`, true, ""},
		{`{"a":` + fi("1", "x.pdf", "application/octet-stream", "hi") + `}`, true, ""},
		{`{"a":` + fi("1", "x", "image/png; q=1", "hi") + `}`, true, ""},
		{`{"a":{"id":"1","mimeType":"text/plain","size":2,"data":"aGk=","digest":{"algorithm":"sha-256","value":"` + dg + `"}}}`, true, ""},
		{`{"a":{"id":"1","mimeType":"text/plain","size":0}}`, true, ""},
		{`{"a":` + fi("1", "x.bin", "application/zip", "hi") + `}`, false, "not accepted"},
		{`{"a":` + fi("1", "x.txt", "text/plain", "01234567890") + `}`, false, "over the 10-byte"},
		{`{"a":` + fi("1", "x.txt", "text/plain", "hi") + `,"b":` + fi("1", "y", "a/b", "x") + `}`, false, "duplicate"},
		{`{"a":` + fi("1", "x.txt", "text/plain", "0123456789") + `,"b":` + fi("2", "y", "a/b", "xyz") + `}`, false, "inline files total"},
		{`{"a":{"id":"1","mimeType":"text/plain","size":2,"data":"aGk=","digest":{"algorithm":"sha-256","value":"nope"}}}`, false, "digest does not match"},
		{`{"a":{"id":"1","mimeType":"text/plain","size":2,"data":"aGk=","digest":{"algorithm":"md5","value":"x"}}}`, false, "unsupported digest"},
		{`{"a":{"id":"1","mimeType":"text/plain","size":5}}`, false, "inline only"},
		{`{"a":{"id":"1","mimeType":"text/plain","size":2,"data":null}}`, false, "not a base64 string"},
		{`{"a":{"id":"1","mimeType":"text/plain","size":2,"data":7}}`, false, "not a base64 string"},
		{`{"a":{"id":"1","mimeType":"text/plain","size":2,"data":"!!"}}`, false, "not valid base64"},
		{`{"a":{"id":"1","mimeType":"text/plain","size":3,"data":"aGk="}}`, false, "does not match"},
		{`{"a":{"id":"1","mimeType":"text/plain"}}`, false, "non-negative"},
		{`{"a":{"id":"1","mimeType":"text/plain","size":-1}}`, false, "non-negative"},
		{`{"a":{"id":"1","size":0}}`, false, "mimeType"},
		{`{"a":{"id":"","mimeType":"x","size":0}}`, false, "1 to 128"},
		{`{"a":{"id":"` + strings.Repeat("x", 129) + `","mimeType":"x","size":0}}`, false, "1 to 128"},
		{`{"a":"str"}`, false, "not a file input"},
		{`"not an object"`, false, "file is missing"}, // passes check, handler fails
		{`{"a":null}`, false, "file is missing"},      // treated as absent; handler fails
	}
	for i, c := range cases {
		r := driveMCP(t, h, call(c.args))[0]["result"].(map[string]any)
		txt := r["content"].([]any)[0].(map[string]any)["text"].(string)
		if c.ok != (r["isError"] == nil) || !strings.Contains(txt, c.msg) {
			t.Errorf("case %d: ok=%v want %v, text %q want %q", i, r["isError"] == nil, c.ok, txt, c.msg)
		}
	}
	if len(*got) != 5 || string((*got)[0].Data) != "hi" || (*got)[0].Name != "x.txt" {
		t.Fatalf("got %+v", *got)
	}
}

func TestAccepts_Any(t *testing.T) {
	if !accepts([]string{"image/png", "*/*"}, File{MimeType: "application/zip"}) {
		t.Fatal("*/* must accept anything")
	}
}

func TestFileParams_BadDescriptor(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("bad descriptor must panic")
		}
	}()
	if fileParams(map[string]any{}) != nil {
		t.Fatal("no properties")
	}
	fileParams(map[string]any{"properties": map[string]any{
		"f": map[string]any{"x-mcp-file": map[string]any{"maxSize": "big"}},
	}})
}
