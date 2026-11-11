package interop

// Shape checks against the draft specs: SEP-2575 DiscoverResult and the
// files extension's upload_required result and download targets. The
// golden transcripts catch any drift; these say which drift is off-spec.

import (
	"encoding/json"
	"fmt"
	"slices"
	"testing"

	"github.com/kfet/acp-kit/mcphost"
)

func shape(t *testing.T, what string, raw json.RawMessage, check func(map[string]any) error) {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("%s: not a JSON object: %v (%s)", what, err, raw)
	}
	if err := check(v); err != nil {
		t.Fatalf("%s off the draft spec shape: %v\n%s", what, err, raw)
	}
}

func keysWithin(where string, m map[string]any, allowed ...string) error {
	for k := range m {
		if !slices.Contains(allowed, k) {
			return fmt.Errorf("%s: unexpected member %q", where, k)
		}
	}
	return nil
}

func requireKeys(where string, m map[string]any, keys ...string) error {
	for _, k := range keys {
		if _, ok := m[k]; !ok {
			return fmt.Errorf("%s: missing required member %q", where, k)
		}
	}
	return nil
}

func checkDiscover(v map[string]any) error {
	if err := keysWithin("discover", v, "supportedVersions", "capabilities", "instructions", "_meta", "resultType", "ttl", "cacheScope"); err != nil {
		return err
	}
	if err := requireKeys("discover", v, "supportedVersions", "capabilities"); err != nil {
		return err
	}
	if vs, _ := v["supportedVersions"].([]any); !slices.Contains(vs, any(latest())) {
		return fmt.Errorf("supportedVersions %v lacks %s", vs, latest())
	}
	meta, _ := v["_meta"].(map[string]any)
	si, _ := meta["io.modelcontextprotocol/serverInfo"].(map[string]any)
	if err := requireKeys("serverInfo", si, "name", "version"); err != nil {
		return err
	}
	caps, _ := v["capabilities"].(map[string]any)
	ext, _ := caps["extensions"].(map[string]any)
	fx, ok := ext[mcphost.FilesExtension].(map[string]any)
	if !ok {
		return fmt.Errorf("capabilities.extensions[%s] missing", mcphost.FilesExtension)
	}
	if err := keysWithin("files settings", fx, "maxInlineBytes", "upload", "revision"); err != nil {
		return err
	}
	if _, ok := fx["maxInlineBytes"].(float64); !ok {
		return fmt.Errorf("maxInlineBytes not a number: %v", fx["maxInlineBytes"])
	}
	return nil
}

var (
	uploadTargetKeys   = []string{"fileId", "url", "path", "method", "form", "headers", "credentials", "expiresAt", "_meta"}
	downloadTargetKeys = []string{"name", "mimeType", "size", "digest", "url", "path", "headers", "credentials", "expiresAt", "_meta"}
)

func checkUploadRequired(v map[string]any) error {
	if err := keysWithin("upload_required", v, "resultType", "uploads", "requestState", "_meta"); err != nil {
		return err
	}
	if v["resultType"] != "upload_required" {
		return fmt.Errorf("resultType = %v", v["resultType"])
	}
	if s, _ := v["requestState"].(string); s == "" {
		return fmt.Errorf("requestState missing")
	}
	ups, _ := v["uploads"].([]any)
	if len(ups) == 0 {
		return fmt.Errorf("uploads empty")
	}
	for _, u := range ups {
		um, _ := u.(map[string]any)
		if err := keysWithin("upload target", um, uploadTargetKeys...); err != nil {
			return err
		}
		if err := requireKeys("upload target", um, "fileId", "url"); err != nil {
			return err
		}
		if c, ok := um["credentials"]; ok && c != "omit" {
			return fmt.Errorf("credentials = %v on a url target", c)
		}
	}
	return nil
}

func checkDownloads(v map[string]any) error {
	meta, _ := v["_meta"].(map[string]any)
	fx, _ := meta[mcphost.FilesExtension].(map[string]any)
	if err := keysWithin("_meta files", fx, "downloads"); err != nil {
		return err
	}
	ds, _ := fx["downloads"].([]any)
	if len(ds) == 0 {
		return fmt.Errorf("downloads empty")
	}
	for _, d := range ds {
		dm, _ := d.(map[string]any)
		if err := keysWithin("download target", dm, downloadTargetKeys...); err != nil {
			return err
		}
		if err := requireKeys("download target", dm, "name", "mimeType", "size", "url"); err != nil {
			return err
		}
		if dg, ok := dm["digest"].(map[string]any); ok {
			if err := keysWithin("digest", dg, "algorithm", "value"); err != nil {
				return err
			}
		}
	}
	return nil
}

// TestDiscoverShape checks the discover reply the real client got.
func TestDiscoverShape(t *testing.T) {
	_, tp := connect(t, plainHost(t), firCaps(), "")
	shape(t, "server/discover", tp.lastResult("server/discover"), checkDiscover)
}
