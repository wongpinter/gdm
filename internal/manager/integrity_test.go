package manager_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/wongpinter/gdm/internal/domain"
	"github.com/wongpinter/gdm/internal/engine"
	"github.com/wongpinter/gdm/internal/manager"
	"github.com/wongpinter/gdm/internal/store"
)

// Regression: a server that answers without Content-Length (or that
// does not support ranges) used to be marked completed with a 0-byte
// file — the unbounded segment reported Done before any byte arrived.
func TestUnknownSizeServerCompletesWithContent(t *testing.T) {
	data := make([]byte, 51)
	for i := range data {
		data[i] = byte('a' + i%26)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush() // no Content-Length: chunked body
		_, _ = w.Write(data)
	}))
	t.Cleanup(srv.Close)

	mgr := newTestManager(t, engine.New())
	dl, err := mgr.Add(srv.URL+"/file.bin", 1)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	waitForStatus(t, mgr, dl.ID, "completed", "failed")
	assertContent(t, mgr, dl.ID, data)
}

// Known size but no range support: the whole body must be fetched in
// one pass instead of being skipped as "already complete".
func TestNoRangeServerCompletesWithContent(t *testing.T) {
	data := make([]byte, 96)
	for i := range data {
		data[i] = byte(i)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		_, _ = w.Write(data)
	}))
	t.Cleanup(srv.Close)

	mgr := newTestManager(t, engine.New())
	dl, err := mgr.Add(srv.URL+"/file.bin", 4)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	snap := waitForStatus(t, mgr, dl.ID, "completed", "failed")
	if snap.Download.Status != domain.StatusCompleted {
		t.Fatalf("status = %s, want completed", snap.Download.Status)
	}
	if snap.Download.TotalSize != int64(len(data)) {
		t.Errorf("TotalSize = %d, want %d", snap.Download.TotalSize, len(data))
	}
	assertContent(t, mgr, dl.ID, data)
}

// A partial file shrunk behind the manager's back (manual delete,
// cleanup job, partial copy): persisted progress above what is on disk
// must be re-downloaded, or the file "completes" with zero holes.
func TestResumeAfterDestinationTruncated(t *testing.T) {
	data := make([]byte, 512<<10)
	for i := range data {
		data[i] = byte(i)
	}
	srv := newRangeServer(t, data, 20*time.Millisecond)
	t.Cleanup(srv.Close)

	mgr := newTestManager(t, engine.New())
	dl, err := mgr.Add(srv.URL+"/file.bin", 4)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	waitForStatus(t, mgr, dl.ID, "downloading")
	waitForProgress(t, mgr, dl.ID)

	if err := mgr.Pause(dl.ID); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	waitForStatus(t, mgr, dl.ID, "paused")

	snap, _ := mgr.Get(dl.ID)
	fi, err := os.Stat(snap.Download.Dest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(snap.Download.Dest, fi.Size()/2); err != nil {
		t.Fatal(err)
	}

	if err := mgr.Resume(dl.ID); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	waitForStatus(t, mgr, dl.ID, "completed", "failed")
	assertContent(t, mgr, dl.ID, data)
}

// A state file whose Dest escapes the download directory (hand-edited
// or written by an older build) must not take the file with it when
// the download is removed.
func TestRemoveRefusesToDeleteOutsideDownloadDir(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	downloadDir := filepath.Join(dir, "downloads")
	victim := filepath.Join(dir, "victim.txt")
	if err := os.WriteFile(victim, []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}

	st, err := store.New(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	evil := &domain.Download{
		ID:     "evil-1",
		URL:    "http://example.com/f.bin",
		Dest:   victim,
		Status: domain.StatusCompleted,
	}
	state, err := json.Marshal(evil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "evil-1.json"), state, 0o644); err != nil {
		t.Fatal(err)
	}

	mgr, err := manager.New(engine.New(), st, downloadDir, 4, 3)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { mgr.Shutdown(5 * time.Second) })
	if _, ok := mgr.Get("evil-1"); !ok {
		t.Fatal("state file not loaded")
	}
	if err := mgr.Remove("evil-1", true); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, ok := mgr.Get("evil-1"); ok {
		t.Fatal("entry still present after Remove")
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("file outside the download dir was deleted: %v", err)
	}
}

func assertContent(t *testing.T, mgr *manager.Manager, id string, want []byte) {
	t.Helper()
	snap, ok := mgr.Get(id)
	if !ok {
		t.Fatalf("download %s not found", id)
	}
	if snap.Download.Status != domain.StatusCompleted {
		t.Fatalf("status = %s, want completed", snap.Download.Status)
	}
	got, err := os.ReadFile(snap.Download.Dest)
	if err != nil {
		t.Fatalf("reading %s: %v", snap.Download.Dest, err)
	}
	if len(got) != len(want) {
		t.Fatalf("content is %d bytes, want %d", len(got), len(want))
	}
	if !bytes.Equal(got, want) {
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("content differs at byte %d: got %d, want %d (zero hole?)", i, got[i], want[i])
			}
		}
	}
}
