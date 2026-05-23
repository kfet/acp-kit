// Package skills loads fir-style skill catalogs from embedded bundles and host dirs.
package skills

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	stdlog "log"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const hashPrefixLen = 12

// Test seams. Production sets these to the real os.* / filepath.* helpers.
var (
	osMkdirAll  = os.MkdirAll
	osWriteFile = os.WriteFile
	osReadFile  = os.ReadFile
	osReadDir   = os.ReadDir
	filepathAbs = filepath.Abs
)

// Skill is one entry in a fir-style skills catalog.
type Skill struct {
	Name        string
	Description string
	// Path is the absolute on-disk path to SKILL.md.
	Path string
}

// LoadBuiltin walks an embedded bundle rooted at "bundle", selects skills whose
// frontmatter declares `builtin: true`, extracts them to
// $TMPDIR/<appPrefix>-<contentHash>/skills, and returns a deterministic catalog.
//
// appPrefix must be non-empty and app-specific (for example "poe-acp") so two
// relays with different embedded bundles never collide in the same tmp dir.
func LoadBuiltin(bundle fs.FS, appPrefix string) ([]Skill, error) {
	appPrefix = strings.TrimSpace(appPrefix)
	if appPrefix == "" {
		return nil, fmt.Errorf("skills: empty appPrefix")
	}
	hash, err := bundleHashFS(bundle)
	if err != nil {
		return nil, fmt.Errorf("hash bundle: %w", err)
	}
	root := filepath.Join(os.TempDir(), appPrefix+"-"+hash[:hashPrefixLen], "skills")

	var skills []Skill
	err = fs.WalkDir(bundle, "bundle", func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() || filepath.Base(p) != "SKILL.md" {
			return nil
		}
		body, rerr := fs.ReadFile(bundle, p)
		if rerr != nil {
			return rerr
		}
		name, desc, builtin := parseFrontmatter(body)
		if !builtin {
			return nil
		}
		rel := strings.TrimPrefix(p, "bundle/")
		dst := filepath.Join(root, rel)
		if err := osMkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if cur, rerr := osReadFile(dst); rerr != nil || string(cur) != string(body) {
			if err := osWriteFile(dst, body, 0o644); err != nil {
				return err
			}
		}
		if name == "" {
			name = filepath.Base(filepath.Dir(rel))
		}
		skills = append(skills, Skill{Name: name, Description: desc, Path: dst})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(skills, func(i, j int) bool { return skills[i].Name < skills[j].Name })
	return skills, nil
}

// LoadDir walks <path>/*/SKILL.md and returns a deterministic catalog. A missing
// directory is not an error. Skills missing description are skipped.
func LoadDir(path string) ([]Skill, error) {
	if path == "" {
		return nil, nil
	}
	abs, err := filepathAbs(path)
	if err != nil {
		return nil, err
	}
	entries, err := osReadDir(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var skills []Skill
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p := filepath.Join(abs, e.Name(), "SKILL.md")
		body, rerr := osReadFile(p)
		if rerr != nil {
			if !os.IsNotExist(rerr) {
				stdlog.Printf("skills: %s: %v, skipping", p, rerr)
			}
			continue
		}
		name, desc, _ := parseFrontmatter(body)
		if name == "" {
			name = e.Name()
		}
		if desc == "" {
			stdlog.Printf("skills: %s: missing description, skipping", p)
			continue
		}
		skills = append(skills, Skill{Name: name, Description: desc, Path: p})
	}
	sort.Slice(skills, func(i, j int) bool { return skills[i].Name < skills[j].Name })
	return skills, nil
}

// Merge layers skill lists with last-wins-by-name semantics, drops names listed
// in disable, and returns a slice sorted by name.
func Merge(layers [][]Skill, disable []string) []Skill {
	disabled := make(map[string]struct{}, len(disable))
	for _, d := range disable {
		disabled[d] = struct{}{}
	}
	by := make(map[string]Skill)
	for _, layer := range layers {
		for _, s := range layer {
			by[s.Name] = s
		}
	}
	out := make([]Skill, 0, len(by))
	for name, s := range by {
		if _, drop := disabled[name]; drop {
			continue
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// FormatCatalog renders a fir-style <available_skills> block ready for system-prompt injection.
func FormatCatalog(skills []Skill) string {
	if len(skills) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("The following skills provide specialized instructions for specific tasks.\n")
	b.WriteString("Use your read tool to load a skill's SKILL.md when the task matches its description.\n")
	b.WriteString("Skill body paths are absolute and stable for the lifetime of this session.\n\n")
	b.WriteString("<available_skills>\n")
	for _, s := range skills {
		b.WriteString("  <skill>\n")
		b.WriteString("    <name>")
		b.WriteString(escapeXML(s.Name))
		b.WriteString("</name>\n")
		b.WriteString("    <description>")
		b.WriteString(escapeXML(s.Description))
		b.WriteString("</description>\n")
		b.WriteString("    <location>")
		b.WriteString(escapeXML(s.Path))
		b.WriteString("</location>\n")
		b.WriteString("  </skill>\n")
	}
	b.WriteString("</available_skills>\n")
	return b.String()
}

func parseFrontmatter(body []byte) (name, desc string, builtin bool) {
	s := string(body)
	if !strings.HasPrefix(s, "---\n") {
		return "", "", false
	}
	end := strings.Index(s[4:], "\n---")
	if end < 0 {
		return "", "", false
	}
	for _, line := range strings.Split(s[4:4+end], "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		switch k {
		case "name":
			name = v
		case "description":
			desc = v
		case "builtin":
			builtin = v == "true"
		}
	}
	return name, desc, builtin
}

func bundleHashFS(fsys fs.FS) (string, error) {
	h := sha256.New()
	err := fs.WalkDir(fsys, "bundle", func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		b, rerr := fs.ReadFile(fsys, p)
		if rerr != nil {
			return rerr
		}
		_, _ = h.Write([]byte(p))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write(b)
		_, _ = h.Write([]byte{0})
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func escapeXML(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}
