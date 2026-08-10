package manager_test

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wongpinter/gdm/internal/engine"
	"github.com/wongpinter/gdm/internal/manager"
	"github.com/wongpinter/gdm/internal/store"
)

// slowReader throttles ServeContent's reads (it drives Range requests
// via Seek+Read, not ReadAt) so a test has a reliable window to pause
// a download mid-flight instead of racing a fast in-memory copy.
type slowReader struct {
	io.ReadSeeker
	delay time.Duration
}

func (s slowReader) Read(p []byte) (int, error) {
	time.Sleep(s.delay)
	return s.ReadSeeker.Read(p)
}

func newRangeServer(t *testing.T, data []byte, delay time.Duration) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rdr := slowReader{ReadSeeker: bytes.NewReader(data), delay: delay}
		http.ServeContent(w, r, "testfile.bin", time.Now(), rdr)
	}))
}

func newTestManager(t *testing.T, eng manager.Engine) *manager.Manager {
	t.Helper()
	dir := t.TempDir()
	st, err := store.New(filepath.Join(dir, "state"))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	mgr, err := manager.New(eng, st, filepath.Join(dir, "downloads"), 4, 3)
	if err != nil {
		t.Fatalf("manager.New: %v", err)
	}
	return mgr
}

func waitForStatus(t *testing.T, mgr *manager.Manager, id string, want ...string) manager.Snapshot {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		snap, ok := mgr.Get(id)
		if ok {
			for _, w := range want {
				if string(snap.Download.Status) == w {
					return snap
				}
			}
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for status %v", want)
	return manager.Snapshot{}
}

// waitForProgress blocks until at least one byte has landed on disk,
// giving a test a reliable moment to pause without racing a fixed sleep
// against however long the first HTTP round-trip happens to take.
func waitForProgress(t *testing.T, mgr *manager.Manager, id string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if snap, ok := mgr.Get(id); ok && snap.Download.BytesDownloaded() > 0 {
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatal("timed out waiting for download progress")
}

func TestScheduledDownloadWaitsUntilStart(t *testing.T) {
	data := []byte("scheduled download")
	srv := newRangeServer(t, data, 0)
	defer srv.Close()

	mgr := newTestManager(t, engine.New())
	startAt := time.Now().Add(300 * time.Millisecond)
	dl, err := mgr.AddAt(srv.URL+"/testfile.bin", 1, startAt)
	if err != nil {
		t.Fatalf("AddAt: %v", err)
	}
	time.Sleep(75 * time.Millisecond)
	if snap, ok := mgr.Get(dl.ID); !ok || snap.Download.Status != "queued" {
		t.Fatalf("status before StartAt = %v, want queued", snap.Download.Status)
	}
	if snap := waitForStatus(t, mgr, dl.ID, "completed", "failed"); snap.Download.Status != "completed" {
		t.Fatalf("download ended in status %q, error: %s", snap.Download.Status, snap.Download.Error)
	}
}

func TestFullDownload(t *testing.T) {
	data := make([]byte, 500_000)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("generating test data: %v", err)
	}
	wantSum := sha256.Sum256(data)

	srv := newRangeServer(t, data, 0)
	defer srv.Close()

	mgr := newTestManager(t, engine.New())
	dl, err := mgr.Add(srv.URL+"/testfile.bin", 4)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	snap := waitForStatus(t, mgr, dl.ID, "completed", "failed")
	if snap.Download.Status != "completed" {
		t.Fatalf("download ended in status %q, error: %s", snap.Download.Status, snap.Download.Error)
	}
	if snap.Download.TotalSize != int64(len(data)) {
		t.Errorf("TotalSize = %d, want %d", snap.Download.TotalSize, len(data))
	}

	got, err := os.ReadFile(snap.Download.Dest)
	if err != nil {
		t.Fatalf("reading downloaded file: %v", err)
	}
	gotSum := sha256.Sum256(got)
	if gotSum != wantSum {
		t.Errorf("downloaded content checksum mismatch (got %d bytes, want %d)", len(got), len(data))
	}
}

func TestPauseResume(t *testing.T) {
	data := make([]byte, 300_000)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("generating test data: %v", err)
	}
	wantSum := sha256.Sum256(data)

	// A per-read delay gives the test a reliable window to pause before
	// the (small, in-memory) transfer finishes on its own — each 32KB
	// read costs 150ms, so a 75KB segment takes well over 300ms.
	srv := newRangeServer(t, data, 150*time.Millisecond)
	defer srv.Close()

	mgr := newTestManager(t, engine.New())
	dl, err := mgr.Add(srv.URL+"/testfile.bin", 4)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	waitForStatus(t, mgr, dl.ID, "downloading")
	waitForProgress(t, mgr, dl.ID)

	if err := mgr.Pause(dl.ID); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	snap := waitForStatus(t, mgr, dl.ID, "paused")
	if snap.Download.BytesDownloaded() == 0 {
		t.Fatal("expected some bytes downloaded before pause, got 0")
	}
	if snap.Download.BytesDownloaded() >= snap.Download.TotalSize {
		t.Fatal("download finished before pause could take effect; test is not exercising pause")
	}

	if err := mgr.Resume(dl.ID); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	snap = waitForStatus(t, mgr, dl.ID, "completed", "failed")
	if snap.Download.Status != "completed" {
		t.Fatalf("download ended in status %q, error: %s", snap.Download.Status, snap.Download.Error)
	}

	got, err := os.ReadFile(snap.Download.Dest)
	if err != nil {
		t.Fatalf("reading downloaded file: %v", err)
	}
	gotSum := sha256.Sum256(got)
	if gotSum != wantSum {
		t.Errorf("downloaded content checksum mismatch after resume (got %d bytes, want %d)", len(got), len(data))
	}
}

func TestRestartRequeuesPausedDownloads(t *testing.T) {
	data := make([]byte, 200_000)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("generating test data: %v", err)
	}

	srv := newRangeServer(t, data, 150*time.Millisecond)
	defer srv.Close()

	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	downloadDir := filepath.Join(dir, "downloads")

	st1, err := store.New(stateDir)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	mgr1, err := manager.New(engine.New(), st1, downloadDir, 4, 3)
	if err != nil {
		t.Fatalf("manager.New: %v", err)
	}
	dl, err := mgr1.Add(srv.URL+"/testfile.bin", 4)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	waitForStatus(t, mgr1, dl.ID, "downloading")
	waitForProgress(t, mgr1, dl.ID)
	mgr1.Shutdown(5 * time.Second) // simulates the process exiting mid-download

	// A fresh manager over the same state/download dirs should find the
	// interrupted download and mark it Paused, not silently resume it.
	st2, err := store.New(stateDir)
	if err != nil {
		t.Fatalf("store.New (reload): %v", err)
	}
	mgr2, err := manager.New(engine.New(), st2, downloadDir, 4, 3)
	if err != nil {
		t.Fatalf("manager.New (reload): %v", err)
	}
	snap, ok := mgr2.Get(dl.ID)
	if !ok {
		t.Fatalf("download %s not found after reload", dl.ID)
	}
	if snap.Download.Status != "paused" {
		t.Fatalf("status after reload = %q, want %q", snap.Download.Status, "paused")
	}

	if err := mgr2.Resume(dl.ID); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	final := waitForStatus(t, mgr2, dl.ID, "completed", "failed")
	if final.Download.Status != "completed" {
		t.Fatalf("download ended in status %q, error: %s", final.Download.Status, final.Download.Error)
	}
	got, err := os.ReadFile(final.Download.Dest)
	if err != nil {
		t.Fatalf("reading downloaded file: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("content mismatch after reload+resume (got %d bytes, want %d)", len(got), len(data))
	}
	_ = io.Discard
}
