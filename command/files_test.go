package command

import (
	"errors"
	"testing"
)

type filePosterFake struct {
	got  []OutFile
	conv string
	ids  []string
	err  error
}

func (p *filePosterFake) PostFileTo(conv string, f OutFile) ([]string, error) {
	p.conv = conv
	p.got = append(p.got, f)
	return p.ids, p.err
}

// filePostCtrl is a Controller that is also a FilePoster.
type filePostCtrl struct {
	fakeCtrl
	filePosterFake
}

// postAndFile is a Poster that is also a FilePoster.
type postAndFile struct {
	filePosterFake
}

func (postAndFile) PostTo(string, string) error { return nil }

func TestPostFile_NoCapability(t *testing.T) {
	b := withCtrl(&fakeCtrl{})
	if b.CanPostFile() {
		t.Fatal("CanPostFile with no FilePoster")
	}
	if _, err := b.PostFile("c", OutFile{Data: []byte("x")}); err == nil {
		t.Fatal("want error")
	}
	if New(newFake()).CanPostFile() {
		t.Fatal("CanPostFile with no controller")
	}
}

func TestPostFile_Lookup(t *testing.T) {
	c := &filePostCtrl{}
	b := withCtrl(c)
	if !b.CanPostFile() {
		t.Fatal("controller FilePoster not found")
	}
	pf := &postAndFile{}
	b.SetPoster(pf)
	if _, err := b.PostFile("c", OutFile{Name: "a", Data: []byte("x")}); err != nil {
		t.Fatal(err)
	}
	if len(pf.got) != 1 || len(c.got) != 0 {
		t.Fatal("Poster's FilePoster should win over the controller's")
	}
	explicit := &filePosterFake{ids: []string{"7"}}
	b.SetFilePoster(explicit)
	ids, err := b.PostFile("conv", OutFile{Name: "../x/rep.pdf", Data: []byte("x"), Caption: "hi"})
	if err != nil || len(ids) != 1 || ids[0] != "7" {
		t.Fatalf("ids=%v err=%v", ids, err)
	}
	got := explicit.got[0]
	if explicit.conv != "conv" || got.Name != "rep.pdf" || got.MimeType != "application/octet-stream" || got.Caption != "hi" {
		t.Fatalf("got %+v", got)
	}
}

func TestPostFile_EmptyAndError(t *testing.T) {
	p := &filePosterFake{err: errors.New("boom")}
	b := New(newFake())
	b.SetFilePoster(p)
	if _, err := b.PostFile("c", OutFile{Name: "a"}); err == nil {
		t.Fatal("empty file accepted")
	}
	if _, err := b.PostFile("c", OutFile{Name: "a", MimeType: "text/plain", Data: []byte("x")}); err == nil || err.Error() != "boom" {
		t.Fatalf("err=%v", err)
	}
	if p.got[0].MimeType != "text/plain" {
		t.Fatal("mime type overwritten")
	}
}

func TestPostFile_MediaType(t *testing.T) {
	p := &filePosterFake{}
	b := New(newFake())
	b.SetFilePoster(p)
	for in, want := range map[string]string{
		"Text/Plain; charset=UTF-8": "text/plain; charset=UTF-8",
		"text/plain\r\nX-Evil: 1":   "application/octet-stream",
		"":                          "application/octet-stream",
	} {
		if _, err := b.PostFile("c", OutFile{Name: "a", MimeType: in, Data: []byte("x")}); err != nil {
			t.Fatal(err)
		}
		if got := p.got[len(p.got)-1].MimeType; got != want {
			t.Errorf("%q -> %q want %q", in, got, want)
		}
	}
}

func TestSafeFileName(t *testing.T) {
	for in, want := range map[string]string{
		"":              "file",
		".":             "file",
		"..":            "file",
		"/":             "file",
		"a/b/c.txt":     "c.txt",
		`C:\x\y.png`:    "y.png",
		"bad\x00\nname": "badname",
		"  sp.txt ":     "sp.txt",
		"dir/":          "dir",
	} {
		if got := SafeFileName(in); got != want {
			t.Errorf("SafeFileName(%q)=%q want %q", in, got, want)
		}
	}
}
