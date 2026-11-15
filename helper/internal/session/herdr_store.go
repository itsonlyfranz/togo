package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"github.com/gettogo/togo/helper/internal/state"
)

// herdrEntry is one session in the Herdr backend's store: where it runs
// and the values togo keeps with it.
type herdrEntry struct {
	ID        string `json:"id"`
	Workspace string `json:"workspace"`
	Pane      string `json:"pane"`
	// Terminal tells a pane apart from one with the same id on a server
	// that restarted.
	Terminal string            `json:"terminal"`
	Dir      string            `json:"dir"`
	Created  time.Time         `json:"created"`
	Values   map[string]string `json:"values"`
}

type herdrStore struct {
	Sessions []herdrEntry `json:"sessions"`
}

// update rewrites the store with what fn returns, under the store lock.
// The file is written only when fn succeeds.
func (b *HerdrBackend) update(ctx context.Context, fn func([]herdrEntry) ([]herdrEntry, error)) error {
	return withLock(ctx, b.LockFile, func() error {
		entries, err := b.load()
		if err != nil {
			return err
		}
		before, _ := json.Marshal(entries)
		entries, err = fn(entries)
		if err != nil {
			return err
		}
		after, _ := json.Marshal(entries)
		if string(before) == string(after) {
			return nil
		}
		b2, err := json.MarshalIndent(herdrStore{Sessions: entries}, "", "  ")
		if err != nil {
			return err
		}
		return state.WriteFile(b.Store, b2)
	})
}

// load reads the store; a missing file is an empty one.
func (b *HerdrBackend) load() ([]herdrEntry, error) {
	raw, err := os.ReadFile(b.Store)
	if errors.Is(err, fs.ErrNotExist) {
		return []herdrEntry{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("herdr sessions: %w", err)
	}
	var st herdrStore
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, fmt.Errorf("herdr sessions %s: %w", b.Store, err)
	}
	if st.Sessions == nil {
		st.Sessions = []herdrEntry{}
	}
	return st.Sessions, nil
}

// entry returns the stored session id, or ErrNotFound.
func (b *HerdrBackend) entry(ctx context.Context, id string) (herdrEntry, error) {
	var found *herdrEntry
	err := withLock(ctx, b.LockFile, func() error {
		entries, err := b.load()
		for i := range entries {
			if entries[i].ID == id {
				found = &entries[i]
			}
		}
		return err
	})
	if err != nil {
		return herdrEntry{}, err
	}
	if found == nil {
		return herdrEntry{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return *found, nil
}

// remove drops session id from the store.
func (b *HerdrBackend) remove(ctx context.Context, id string) error {
	return b.update(ctx, func(entries []herdrEntry) ([]herdrEntry, error) {
		kept := entries[:0]
		for _, e := range entries {
			if e.ID != id {
				kept = append(kept, e)
			}
		}
		return kept, nil
	})
}
