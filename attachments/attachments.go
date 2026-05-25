// Package attachments stores prompt attachments in a cwd-local sandbox and
// builds ACP resource blocks that point at the stored files.
package attachments

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	acp "github.com/coder/acp-go-sdk"
)

const (
	// DefaultDirName is the attachment directory created inside a session cwd.
	DefaultDirName = ".acp-attachments"
	// DefaultMaxBytes caps a single downloaded or written attachment.
	DefaultMaxBytes int64 = 25 << 20 // 25 MiB
)

// Attachment is the transport-neutral attachment metadata the store needs.
type Attachment struct {
	URL         string
	Name        string
	ContentType string
}

// File describes one attachment stored on disk.
type File struct {
	Name        string
	Path        string
	URI         string
	ContentType string
	Size        int64
}

// Store owns the on-disk layout and byte limits for attachments.
type Store struct {
	// DirName is created inside the ACP session cwd. Empty => DefaultDirName.
	DirName string
	// MaxBytes caps one attachment. <=0 => DefaultMaxBytes.
	MaxBytes int64
	// Client is used by Download. nil => http.DefaultClient.
	Client *http.Client
}

// Download fetches a.URL and writes it under <cwd>/<DirName>/<msgID>/<name>.
// Names are opened through os.Root, so hostile names cannot escape the message
// directory. used tracks per-prompt filename collisions; nil is accepted.
func (s Store) Download(ctx context.Context, cwd, msgID string, used map[string]struct{}, a Attachment) (File, error) {
	if a.URL == "" {
		return File{}, fmt.Errorf("attachment URL is empty")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
	if err != nil {
		return File{}, err
	}
	client := s.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return File{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return File{}, fmt.Errorf("http %d", resp.StatusCode)
	}
	max := s.maxBytes()
	if resp.ContentLength > 0 && resp.ContentLength > max {
		return File{}, fmt.Errorf("declared content-length %d exceeds cap %d", resp.ContentLength, max)
	}
	return s.Write(cwd, msgID, used, a, resp.Body)
}

// Write copies r into <cwd>/<DirName>/<msgID>/<name>. It returns the final file
// path and file:// resource URI. The copy is capped by Store.MaxBytes.
func (s Store) Write(cwd, msgID string, used map[string]struct{}, a Attachment, r io.Reader) (File, error) {
	if used == nil {
		used = make(map[string]struct{})
	}
	root, err := s.openMessageDir(cwd, msgID)
	if err != nil {
		return File{}, fmt.Errorf("open message dir: %w", err)
	}
	defer root.Close()

	preferred := preferredName(a)
	finalName := uniqueName(preferred, used)
	f, err := root.OpenFile(finalName, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		// os.Root rejects traversal and absolute paths. Retry with a
		// hash-derived fallback so one hostile name cannot drop the attachment.
		finalName = uniqueName(fallbackName(a), used)
		f, err = root.OpenFile(finalName, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
		mustNot(err, "fallback OpenFile")
	}
	used[finalName] = struct{}{}

	max := s.maxBytes()
	n, copyErr := io.Copy(f, io.LimitReader(r, max+1))
	closeErr := f.Close()
	if copyErr != nil {
		_ = root.Remove(finalName)
		return File{}, copyErr
	}
	mustNot(closeErr, "close")
	if n > max {
		_ = root.Remove(finalName)
		return File{}, fmt.Errorf("attachment exceeds cap %d bytes", max)
	}

	abs := filepath.Join(cwd, s.dirName(), msgID, finalName)
	uri := (&url.URL{Scheme: "file", Path: abs}).String()
	return File{Name: finalName, Path: abs, URI: uri, ContentType: a.ContentType, Size: n}, nil
}

// ResourceLinkBlock returns a file:// ResourceLink block for f.
func (f File) ResourceLinkBlock() acp.ContentBlock {
	return ResourceLinkBlock(f.Name, f.URI, f.ContentType)
}

// FileResourceLinkBlock builds a ResourceLink for absPath using a properly escaped file:// URI.
func FileResourceLinkBlock(name, absPath, contentType string) acp.ContentBlock {
	uri := (&url.URL{Scheme: "file", Path: absPath}).String()
	return ResourceLinkBlock(name, uri, contentType)
}

// URLResourceLinkBlock builds a ResourceLink for an original remote attachment URL.
func URLResourceLinkBlock(a Attachment) acp.ContentBlock {
	name := a.Name
	if name == "" {
		name = a.URL
	}
	return ResourceLinkBlock(name, a.URL, a.ContentType)
}

// ResourceLinkBlock builds an ACP ResourceLink block and sets MimeType when known.
func ResourceLinkBlock(name, uri, contentType string) acp.ContentBlock {
	block := acp.ResourceLinkBlock(name, uri)
	if contentType != "" && block.ResourceLink != nil {
		ct := contentType
		block.ResourceLink.MimeType = &ct
	}
	return block
}

// TextResourceBlock builds an embedded text Resource block for agents that
// advertise embedded context support.
func TextResourceBlock(uri, text, mimeType string) acp.ContentBlock {
	trc := acp.TextResourceContents{Uri: uri, Text: text}
	if mimeType != "" {
		m := mimeType
		trc.MimeType = &m
	}
	return acp.ResourceBlock(acp.EmbeddedResourceResource{TextResourceContents: &trc})
}

func (s Store) openMessageDir(cwd, msgID string) (*os.Root, error) {
	if err := validateComponent(msgID); err != nil {
		return nil, fmt.Errorf("msgID %q: %w", msgID, err)
	}
	base := filepath.Join(cwd, s.dirName())
	if err := os.MkdirAll(base, 0o755); err != nil {
		return nil, err
	}
	parent, err := os.OpenRoot(base)
	mustNot(err, "OpenRoot")
	if err := parent.Mkdir(msgID, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
		_ = parent.Close()
		mustNot(err, "Mkdir within Root")
	}
	sub, err := parent.OpenRoot(msgID)
	_ = parent.Close()
	mustNot(err, "OpenRoot within Root")
	return sub, nil
}

func (s Store) dirName() string {
	if strings.TrimSpace(s.DirName) == "" {
		return DefaultDirName
	}
	return s.DirName
}

func (s Store) maxBytes() int64 {
	if s.MaxBytes <= 0 {
		return DefaultMaxBytes
	}
	return s.MaxBytes
}

func validateComponent(s string) error {
	if s == "" || s == "." || s == ".." {
		return fmt.Errorf("empty or reserved name")
	}
	if strings.ContainsAny(s, `/\\`) || strings.ContainsRune(s, 0) || s[0] == '.' {
		return fmt.Errorf("unsafe path component")
	}
	return nil
}

func preferredName(a Attachment) string {
	name := a.Name
	switch name {
	case "", ".", "..":
		return fallbackName(a)
	}
	return capName(name, 200)
}

func fallbackName(a Attachment) string {
	seed := a.URL
	if seed == "" {
		seed = a.Name + "\x00" + a.ContentType
	}
	h := sha256.Sum256([]byte(seed))
	stem := "attachment-" + hex.EncodeToString(h[:4])
	if exts, _ := mime.ExtensionsByType(a.ContentType); len(exts) > 0 {
		return stem + exts[0]
	}
	return stem + ".bin"
}

func capName(name string, max int) string {
	if len(name) <= max {
		return name
	}
	ext := filepath.Ext(name)
	if len(ext) >= max {
		return name[:max]
	}
	return name[:max-len(ext)] + ext
}

func uniqueName(name string, used map[string]struct{}) string {
	if _, taken := used[name]; !taken {
		return name
	}
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s-%d%s", stem, i, ext)
		if _, taken := used[candidate]; !taken {
			return candidate
		}
	}
}
