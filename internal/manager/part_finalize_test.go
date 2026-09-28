package manager_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wongpinter/gdm/internal/domain"
	"github.com/wongpinter/gdm/internal/engine"
	"github.com/wongpinter/gdm/internal/manager"
	"github.com/wongpinter/gdm/internal/store"
)

// TestNewDemotesCompletedTorrentWithPartFiles pins startup recovery for
// the "says finished, files still .part" state: a torrent persisted as
// completed whose payload never left .part must load as Paused with an
// explanatory error, so the user can resume and let the engine repair
// it. Finalized torrents keep their completed status.
func TestNewDemotesCompletedTorrentWithPartFiles(t *testing.T) {
	dir := t.TempDir()
	st, err := store.New(filepath.Join(dir, "state"))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	downloads := filepath.Join(dir, "downloads")
	if err := os.MkdirAll(downloads, 0o755); err != nil {
		t.Fatalf("mkdir downloads: %v", err)
	}
	now := time.Now()

	// Single-file torrent: .part sits beside Dest.
	singleDest := filepath.Join(downloads, "movie.mkv")
	if err := os.WriteFile(singleDest+".part", []byte("payload"), 0o644); err != nil {
		t.Fatalf("writing single .part: %v", err)
	}

	// Multi-file torrent: Dest is a folder holding a nested .part.
	multiDest := filepath.Join(downloads, "album")
	if err := os.MkdirAll(multiDest, 0o755); err != nil {
		t.Fatalf("mkdir multi: %v", err)
	}
	if err := os.WriteFile(filepath.Join(multiDest, "track01.flac.part"), []byte("payload"), 0o644); err != nil {
		t.Fatalf("writing nested .part: %v", err)
	}

	// Control: completed torrent with everything finalized on disk.
	doneDest := filepath.Join(downloads, "done.mkv")
	if err := os.WriteFile(doneDest, []byte("payload"), 0o644); err != nil {
		t.Fatalf("writing control file: %v", err)
	}

	for _, tc := range []struct{ id, dest string }{
		{"single", singleDest},
		{"multi", multiDest},
		{"done", doneDest},
	} {
		if err := st.Save(&domain.Download{
			ID:        tc.id,
			URL:       "magnet:?xt=urn:btih:774253cc2983a7479a3d5b2ff91386027a006ebd",
			Kind:      domain.KindTorrent,
			Status:    domain.StatusCompleted,
			Dest:      tc.dest,
			Filename:  filepath.Base(tc.dest),
			CreatedAt: now,
			UpdatedAt: now,
		}); err != nil {
			t.Fatalf("saving %s: %v", tc.id, err)
		}
	}

	mgr, err := manager.New(engine.New(), st, downloads, 4, 3)
	if err != nil {
		t.Fatalf("manager.New: %v", err)
	}
	t.Cleanup(func() { mgr.Shutdown(5 * time.Second) })

	for _, id := range []string{"single", "multi"} {
		snap, ok := mgr.Get(id)
		if !ok {
			t.Fatalf("%s missing from manager", id)
		}
		if snap.Download.Status != domain.StatusPaused {
			t.Errorf("%s status = %q, want paused", id, snap.Download.Status)
		}
		if !strings.Contains(snap.Download.Error, ".part") {
			t.Errorf("%s error = %q, want a .part mention", id, snap.Download.Error)
		}
	}
	snap, ok := mgr.Get("done")
	if !ok {
		t.Fatal("control download missing")
	}
	if snap.Download.Status != domain.StatusCompleted {
		t.Errorf("control status = %q, want completed (no .part left)", snap.Download.Status)
	}
}
