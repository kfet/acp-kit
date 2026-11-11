package command

import (
	"testing"

	"github.com/kfet/acp-kit/mcphost"
)

type upFake struct {
	conv   string
	posted mcphost.UploadedFile
}

func (u *upFake) UploadTargetFor(string, mcphost.UploadSpec) (mcphost.UploadTarget, error) {
	return mcphost.UploadTarget{}, nil
}
func (u *upFake) FinishUploadFor(string, mcphost.UploadSpec, string) (mcphost.UploadedFile, error) {
	return mcphost.UploadedFile{}, nil
}
func (u *upFake) PostUploadedTo(conv string, f mcphost.UploadedFile, _ string) ([]string, error) {
	u.conv, u.posted = conv, f
	return []string{"1"}, nil
}

type linkFake struct{}

func (linkFake) AttachmentsIn(string) ([]InboundAttachment, error) { return nil, nil }
func (linkFake) DownloadLinkFor(string, string) (mcphost.DownloadTarget, error) {
	return mcphost.DownloadTarget{}, nil
}

type upCtrl struct {
	fakeCtrl
	upFake
}

type upPoster struct {
	upFake
	linkFake
}

func (upPoster) PostTo(string, string) error { return nil }

func TestOOBCapabilities(t *testing.T) {
	b := withCtrl(&fakeCtrl{})
	if _, ok := b.UploadTargeter(); ok {
		t.Fatal("UploadTargeter without one")
	}
	if _, ok := b.DownloadLinker(); ok {
		t.Fatal("DownloadLinker without one")
	}
	if _, err := b.PostUploaded("c", mcphost.UploadedFile{}, ""); err == nil {
		t.Fatal("want error")
	}
	c := &upCtrl{}
	b = withCtrl(c)
	if _, err := b.PostUploaded("c", mcphost.UploadedFile{Name: "a/../b.txt", MimeType: "a b;"}, ""); err != nil {
		t.Fatal(err)
	}
	if c.conv != "c" || c.posted.Name != "b.txt" || c.posted.MimeType != "application/octet-stream" {
		t.Fatalf("%+v", c.posted)
	}
	p := &upPoster{}
	b.SetPoster(p)
	if u, _ := b.UploadTargeter(); u != p {
		t.Fatal("Poster's UploadTargeter should win")
	}
	if d, _ := b.DownloadLinker(); d != p {
		t.Fatal("Poster's DownloadLinker should win")
	}
	u, l := &upFake{}, linkFake{}
	b.SetUploadTargeter(u)
	b.SetDownloadLinker(l)
	if got, _ := b.UploadTargeter(); got != u {
		t.Fatal("explicit UploadTargeter should win")
	}
	if got, _ := b.DownloadLinker(); got != l {
		t.Fatal("explicit DownloadLinker should win")
	}
}
