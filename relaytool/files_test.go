package relaytool

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/kfet/acp-kit/command"
	"github.com/kfet/acp-kit/mcphost"
)

// oobCtrl adds UploadTargeter and DownloadLinker.
type oobCtrl struct {
	ctrl
	conv      string
	postedUp  []mcphost.UploadedFile
	postUpErr error
	atts      []command.InboundAttachment
	attsErr   error
	link      mcphost.DownloadTarget
	linkErr   error
}

func (c *oobCtrl) UploadTargetFor(conv string, s mcphost.UploadSpec) (mcphost.UploadTarget, error) {
	c.conv = conv
	return mcphost.UploadTarget{URL: "https://up.example/" + s.FileID, Ref: "R"}, nil
}
func (c *oobCtrl) FinishUploadFor(conv string, s mcphost.UploadSpec, ref string) (mcphost.UploadedFile, error) {
	return mcphost.UploadedFile{Ref: ref, Size: s.Size}, nil
}
func (c *oobCtrl) PostUploadedTo(conv string, f mcphost.UploadedFile, caption string) ([]string, error) {
	c.postedUp = append(c.postedUp, f)
	return []string{"m1"}, c.postUpErr
}
func (c *oobCtrl) AttachmentsIn(string) ([]command.InboundAttachment, error) {
	return c.atts, c.attsErr
}
func (c *oobCtrl) DownloadLinkFor(_, id string) (mcphost.DownloadTarget, error) {
	return c.link, c.linkErr
}

func callFull(t *testing.T, tools *Tools, name string, c mcphost.Call) (mcphost.Result, error) {
	t.Helper()
	for _, x := range tools.Tools() {
		if x.Name == name {
			return x.Call(c)
		}
	}
	t.Fatalf("tool %q is not registered", name)
	return mcphost.Result{}, nil
}

func TestOOBToolsGating(t *testing.T) {
	n := names(newTools(t, &oobCtrl{}, nil))
	for _, w := range []string{ToolSendFile, ToolListAttachments, ToolFetchAttachment} {
		if !has(n, w) {
			t.Fatalf("%s missing from %v", w, n)
		}
	}
	if n := names(newTools(t, &ctrl{}, nil)); has(n, ToolListAttachments) || has(n, ToolSendFile) {
		t.Fatalf("ungated: %v", n)
	}
}

func TestSendFileUploaded(t *testing.T) {
	c := &oobCtrl{}
	tools := newTools(t, c, nil)
	up := mcphost.UploadedFile{Ref: "R", Name: "d/big.bin", MimeType: "application/x", Size: 99}
	r, err := callFull(t, tools, ToolSendFile, mcphost.Call{SessionKey: "k", Args: json.RawMessage(`{"caption":"c"}`),
		Files: map[string]mcphost.UploadedFile{"file": up}})
	if err != nil || r.Text != "Sent big.bin (99 bytes) to this conversation as message id(s): m1." {
		t.Fatalf("%q %v", r.Text, err)
	}
	c.postUpErr = errors.New("boom")
	if _, err := callFull(t, tools, ToolSendFile, mcphost.Call{SessionKey: "k", Args: json.RawMessage(`{}`),
		Files: map[string]mcphost.UploadedFile{"file": up}}); err == nil {
		t.Fatal("want error")
	}
	// Inline with no FilePoster fails honestly.
	if _, err := call(t, tools, ToolSendFile, "k", `{"file":{"id":"1","mimeType":"a/b","size":1,"data":"YQ=="}}`); err == nil {
		t.Fatal("inline without FilePoster accepted")
	}
	inactive := newTools(t, c, func(cfg *Config) { cfg.ConvToken = func(string) (string, bool) { return "", false } })
	if _, err := callFull(t, inactive, ToolSendFile, mcphost.Call{}); err == nil {
		t.Fatal("inactive conversation accepted")
	}
}

func TestUploadProvider(t *testing.T) {
	c := &oobCtrl{}
	p := uploadProvider{t: newTools(t, c, nil), u: c}
	tg, err := p.UploadTarget("k", mcphost.UploadSpec{FileID: "f"})
	if err != nil || tg.URL != "https://up.example/f" || c.conv != "k" {
		t.Fatalf("%+v %v", tg, err)
	}
	if f, err := p.FinishUpload("k", mcphost.UploadSpec{Size: 3}, "R"); err != nil || f.Size != 3 {
		t.Fatalf("%+v %v", f, err)
	}
	dead := uploadProvider{t: newTools(t, c, func(cfg *Config) { cfg.ConvToken = func(string) (string, bool) { return "", false } }), u: c}
	if _, err := dead.UploadTarget("k", mcphost.UploadSpec{}); err == nil {
		t.Fatal("want error")
	}
	if _, err := dead.FinishUpload("k", mcphost.UploadSpec{}, ""); err == nil {
		t.Fatal("want error")
	}
}

func TestAttachmentTools(t *testing.T) {
	c := &oobCtrl{}
	tools := newTools(t, c, nil)
	if got, _ := call(t, tools, ToolListAttachments, "k", ``); got != "No attachments in this conversation." {
		t.Fatal(got)
	}
	c.atts = []command.InboundAttachment{{ID: "a1", Name: "x/r.pdf", MimeType: "application/pdf", Size: 10}, {ID: "a2", Name: "n", MimeType: "a/b", Size: -1}}
	got, _ := call(t, tools, ToolListAttachments, "k", ``)
	if !strings.Contains(got, "- a1: r.pdf (application/pdf, 10 bytes)") || !strings.Contains(got, "unknown size") {
		t.Fatal(got)
	}
	c.attsErr = errors.New("nope")
	if _, err := call(t, tools, ToolListAttachments, "k", ``); err == nil {
		t.Fatal("want error")
	}
	c.link = mcphost.DownloadTarget{URL: "https://dl/x", Name: "../r.pdf", Size: 10}
	r, err := callFull(t, tools, ToolFetchAttachment, mcphost.Call{SessionKey: "k", Args: json.RawMessage(`{"id":"a1"}`)})
	if err != nil || len(r.Downloads) != 1 || r.Downloads[0].Name != "r.pdf" {
		t.Fatalf("%+v %v", r, err)
	}
	for _, bad := range []string{`{}`, `[`} {
		if _, err := call(t, tools, ToolFetchAttachment, "k", bad); err == nil {
			t.Fatalf("%s accepted", bad)
		}
	}
	c.linkErr = errors.New("gone")
	if _, err := call(t, tools, ToolFetchAttachment, "k", `{"id":"a1"}`); err == nil {
		t.Fatal("want error")
	}
}

func TestRegisterWithUploads(t *testing.T) {
	h, err := mcphost.New(mcphost.Config{BaseDir: t.TempDir(), RedirCommand: "/bin/true", RedirSubcommand: "s",
		ServerName: "relay", EnvSocket: "S", EnvToken: "T"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h.Close() }()
	newTools(t, &oobCtrl{}, nil).Register(h)
}
