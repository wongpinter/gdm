// Package store persists download state as JSON files.
package store

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/wongpinter/gdm/internal/domain"
)

// JSONStore persists each download as <id>.json in a directory.
// All operations are safe for concurrent use.
type JSONStore struct {
	dir string

	// mu serializes writes to the directory so a Save and a Delete for
	// the same id cannot interleave, and guards dead.
	mu sync.Mutex
	// dead tombstones ids that were deleted in this process: a persist
	// already in flight when Remove ran must not recreate the file and
	// resurrect the entry.
	dead map[string]bool
}

// New creates a JSONStore rooted at dir.
func New(dir string) (*JSONStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("creating state dir %s: %w", dir, err)
	}
	return &JSONStore{dir: dir, dead: make(map[string]bool)}, nil
}

// Save serializes one download to its state file, replacing it atomically.
func (s *JSONStore) Save(d *domain.Download) error {
	if d == nil {
		return fmt.Errorf("encoding nil download")
	}
	if err := validID(d.ID); err != nil {
		return fmt.Errorf("saving download: %w", err)
	}
	data, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding download %s: %w", d.ID, err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dead[d.ID] {
		return nil // deleted while this save was queued; drop it
	}

	// A unique temp name per write: two saves of the same id (the
	// worker's periodic persist racing a user action) used to share one
	// <id>.json.tmp and could interleave into a torn file. Rename makes
	// the swap atomic for readers either way.
	tmp, err := os.CreateTemp(s.dir, d.ID+"-*.tmp")
	if err != nil {
		return fmt.Errorf("creating temp state file for %s: %w", d.ID, err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("writing state file for %s: %w", d.ID, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("closing state file for %s: %w", d.ID, err)
	}
	if err := os.Rename(tmpName, s.path(d.ID)); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("committing state file for %s: %w", d.ID, err)
	}
	return nil
}

// Load reads one download by id.
func (s *JSONStore) Load(id string) (*domain.Download, error) {
	if err := validID(id); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(s.path(id))
	if err != nil {
		return nil, err
	}
	var d domain.Download
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, fmt.Errorf("decoding %s: %w", s.path(id), err)
	}
	return &d, nil
}

// LoadAll lists every stored download, skipping files that fail to
// decode (e.g. truncated by a killed process).
func (s *JSONStore) LoadAll() ([]*domain.Download, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("reading state dir: %w", err)
	}
	out := make([]*domain.Download, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		if err := validID(id); err != nil {
			continue
		}
		d, err := s.Load(id)
		if err != nil {
			continue
		}
		out = append(out, d)
	}
	return out, nil
}

// Delete removes one download's state file. It also tombstones the id,
// so a Save that was already in flight cannot bring the file back.
func (s *JSONStore) Delete(id string) error {
	if err := validID(id); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dead[id] = true
	err := os.Remove(s.path(id))
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("deleting state file for %s: %w", id, err)
	}
	return nil
}

// Ping implements manager.Store; a ready directory is a ready store.
func (s *JSONStore) Ping(context.Context) error {
	fi, err := os.Stat(s.dir)
	if err != nil {
		return fmt.Errorf("state dir %s not ready: %w", s.dir, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("state path %s is not a directory", s.dir)
	}
	return nil
}

func validID(id string) error {
	if id == "" || id == "." || id == ".." || strings.ContainsAny(id, `/\\`) {
		return fmt.Errorf("invalid download id %q", id)
	}
	return nil
}

func (s *JSONStore) path(id string) string {
	return filepath.Join(s.dir, id+".json")
}
