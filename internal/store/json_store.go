// Package store is the outbound adapter for persistence. It keeps one
// JSON file per download so a crash or restart loses at most the
// in-flight chunk of each segment, not the whole queue.
package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/wongpinter/gdm/internal/domain"
	"github.com/wongpinter/gdm/internal/manager"
)

// JSONStore persists downloads as <dir>/<id>.json.
type JSONStore struct {
	dir string
}

var _ manager.Store = (*JSONStore)(nil)

// New creates (if needed) dir and returns a store rooted there.
func New(dir string) (*JSONStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("creating state dir: %w", err)
	}
	return &JSONStore{dir: dir}, nil
}

func (s *JSONStore) path(id string) string {
	return filepath.Join(s.dir, id+".json")
}

// Save writes d atomically: it writes to a temp file then renames,
// so a crash mid-write never leaves a corrupt state file behind.
func (s *JSONStore) Save(d *domain.Download) error {
	data, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding %s: %w", d.ID, err)
	}
	tmp := s.path(d.ID) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", d.ID, err)
	}
	return os.Rename(tmp, s.path(d.ID))
}

// LoadAll reads every persisted download. Malformed files are skipped
// rather than failing the whole load, so one bad record can't block
// the manager from starting.
func (s *JSONStore) LoadAll() ([]*domain.Download, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("reading state dir: %w", err)
	}
	downloads := make([]*domain.Download, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.dir, entry.Name()))
		if err != nil {
			continue
		}
		var d domain.Download
		if err := json.Unmarshal(data, &d); err != nil {
			continue
		}
		downloads = append(downloads, &d)
	}
	return downloads, nil
}

// Delete removes a download's persisted state, if present.
func (s *JSONStore) Delete(id string) error {
	err := os.Remove(s.path(id))
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("deleting %s: %w", id, err)
	}
	return nil
}
