package state

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// MetaFile is the per-conversation metadata file stored in
// <StateDir>/convs/<key>/. Its content is opaque to the kit: the relay
// owns its format.
//
// It exists so a relay can rebuild its own key → conversation index
// from the conversation dirs alone, when its index file is lost. It is
// keyed by the conversation key, so it lives under the default layout
// even when a custom CwdFor sends the agent's cwd elsewhere: it is
// relay state, not agent state.
const MetaFile = ".conv.json"

// WriteMeta stores data as the metadata of key, atomically
// (write-temp + rename). It creates the conversation dir if absent.
// It does not need a Manager, so a relay can call it before the
// Manager exists.
func WriteMeta(stateDir, key string, data []byte) error {
	if err := validateKeyComponent(key); err != nil {
		return fmt.Errorf("key %q: %w", key, err)
	}
	dir := filepath.Join(stateDir, "convs", key)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir conv dir: %w", err)
	}
	tmp := filepath.Join(dir, MetaFile+".tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write meta: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, MetaFile)); err != nil {
		return fmt.Errorf("commit meta: %w", err)
	}
	return nil
}

// ReadMetas returns the metadata of every conversation dir under
// <StateDir>/convs that has one, by key. A missing convs dir is an
// empty result. A dir without a metadata file is left out of the
// result. An unreadable metadata file is an error, because a silent
// gap is the failure a restore must not hide.
func ReadMetas(stateDir string) (map[string][]byte, error) {
	out := map[string][]byte{}
	root := filepath.Join(stateDir, "convs")
	ents, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read convs dir: %w", err)
	}
	for _, e := range ents {
		if !e.IsDir() || validateKeyComponent(e.Name()) != nil {
			continue
		}
		b, err := os.ReadFile(filepath.Join(root, e.Name(), MetaFile))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read meta %s: %w", e.Name(), err)
		}
		out[e.Name()] = b
	}
	return out, nil
}
