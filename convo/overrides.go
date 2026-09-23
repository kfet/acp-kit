package convo

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	acp "github.com/coder/acp-go-sdk"
	"github.com/kfet/acp-kit/client"
)

// Store persists per-conversation model overrides across restarts. It is
// optional: a nil Store keeps overrides in memory only, which is what a
// relay wants when a model choice is session-shaped and a restart drops
// the sessions it applied to anyway.
type Store interface {
	// Load returns every persisted override, keyed by conversation.
	Load() (map[string]string, error)
	// Save records conv's override. An empty id deletes it.
	Save(conv, id string) error
}

// ErrUnknownModel is wrapped by ValidateModel's error.
var ErrUnknownModel = errors.New("unknown model")

// ValidateModel checks id against the agent's advertised models. An empty
// catalogue accepts anything: the relay cannot tell a bad id from a list
// it has not learned yet, and refusing would lock the user out of the one
// command that can repair a broken model.
func ValidateModel(models []client.ModelInfo, id string) error {
	if len(models) == 0 {
		return nil
	}
	for _, m := range models {
		if m.ID == id {
			return nil
		}
	}
	return fmt.Errorf("%w %q", ErrUnknownModel, id)
}

type choice struct {
	id      string
	applied acp.SessionId
}

// Overrides holds the sticky per-conversation model chosen with `!model`.
//
// Each choice also records the ACP session it was last pushed to, so a
// relay that applies overrides lazily (at the start of the next turn)
// re-applies after idle GC or a reset hands the conversation a new
// session: the id mismatch is the signal. See Pending and MarkApplied.
type Overrides struct {
	store Store

	mu sync.Mutex
	m  map[string]choice
}

// NewOverrides returns an Overrides backed by store (nil = memory only),
// preloaded with whatever store holds.
func NewOverrides(store Store) (*Overrides, error) {
	o := &Overrides{store: store, m: map[string]choice{}}
	if store == nil {
		return o, nil
	}
	saved, err := store.Load()
	if err != nil {
		return nil, fmt.Errorf("convo: load overrides: %w", err)
	}
	for conv, id := range saved {
		if id != "" {
			o.m[conv] = choice{id: id}
		}
	}
	return o, nil
}

// Get returns conv's override, if any.
func (o *Overrides) Get(conv string) (string, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	c, ok := o.m[conv]
	return c.id, ok
}

// Set records id as conv's override (clearing any applied marker) and
// persists it. An empty id clears the override.
func (o *Overrides) Set(conv, id string) error {
	o.mu.Lock()
	if id == "" {
		delete(o.m, conv)
	} else {
		o.m[conv] = choice{id: id}
	}
	o.mu.Unlock()
	return o.save(conv, id)
}

// Carry moves from's override to to, dropping the applied marker so the
// new conversation's session gets its own push. Used when a relay
// re-keys a conversation (zulip-acp's `!new` allocates a fresh id): the
// model choice is the user's, not the conversation's.
func (o *Overrides) Carry(from, to string) error {
	o.mu.Lock()
	c, ok := o.m[from]
	if ok {
		delete(o.m, from)
		o.m[to] = choice{id: c.id}
	}
	o.mu.Unlock()
	if !ok {
		return nil
	}
	return errors.Join(o.save(from, ""), o.save(to, c.id))
}

// Pending reports conv's override when it has not yet been applied to
// session sid.
func (o *Overrides) Pending(conv string, sid acp.SessionId) (string, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	c, ok := o.m[conv]
	if !ok || c.applied == sid {
		return "", false
	}
	return c.id, true
}

// MarkApplied records that id was pushed to sid for conv. A choice
// changed in the meantime is left untouched so it is applied next turn.
func (o *Overrides) MarkApplied(conv, id string, sid acp.SessionId) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if c, ok := o.m[conv]; ok && c.id == id {
		o.m[conv] = choice{id: id, applied: sid}
	}
}

// Len returns the number of conversations with an override.
func (o *Overrides) Len() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.m)
}

func (o *Overrides) save(conv, id string) error {
	if o.store == nil {
		return nil
	}
	return o.store.Save(conv, id)
}

// FileStore is a Store kept in one JSON object file.
type FileStore struct {
	Path string

	mu sync.Mutex
}

// Load reads the file. A missing file is an empty store.
func (f *FileStore) Load() (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.load()
}

func (f *FileStore) load() (map[string]string, error) {
	b, err := os.ReadFile(f.Path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", f.Path, err)
	}
	return m, nil
}

// Save rewrites the file atomically with conv updated.
func (f *FileStore) Save(conv, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, err := f.load()
	if err != nil {
		return err
	}
	if id == "" {
		delete(m, conv)
	} else {
		m[conv] = id
	}
	if err := os.MkdirAll(filepath.Dir(f.Path), 0o755); err != nil {
		return err
	}
	tmp := f.Path + ".tmp"
	if err := os.WriteFile(tmp, mustJSON(m), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, f.Path)
}
