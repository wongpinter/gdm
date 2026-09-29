package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/wongpinter/gdm/internal/engine"
	"github.com/wongpinter/gdm/internal/manager"
	"github.com/wongpinter/gdm/internal/store"
)

// -public-trackers is a bool flag: reorderArgs must not eat the URL
// that follows it as its "value".
func TestReorderArgsBoolFlagDoesNotSwallowPositional(t *testing.T) {
	in := []string{"-public-trackers", "magnet:?xt=urn:btih:abc", "-connections", "4"}
	want := []string{"-public-trackers", "-connections", "4", "magnet:?xt=urn:btih:abc"}
	if got := reorderArgs(in); !reflect.DeepEqual(got, want) {
		t.Fatalf("reorderArgs = %q, want %q", got, want)
	}
}

// Headless mode never resumes anything, so a paused download would
// spin forever; stalledIDs must report it instead.
func TestStalledIDsFlagsPausedDownloads(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("hello"))
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	st, err := store.New(filepath.Join(dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := manager.New(engine.New(), st, filepath.Join(dir, "downloads"), 4, 3)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { mgr.Shutdown(5 * time.Second) })

	dl, err := mgr.Add(srv.URL+"/f.bin", 1)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	// queued/probing/downloading all count as running
	if got := stalledIDs(mgr, []string{dl.ID}); len(got) != 0 {
		t.Fatalf("running download flagged stalled: %v", got)
	}

	if err := mgr.Pause(dl.ID); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		snap, _ := mgr.Get(dl.ID)
		if snap.Download.Status == "paused" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("status = %s, want paused", snap.Download.Status)
		}
		time.Sleep(15 * time.Millisecond)
	}
	got := stalledIDs(mgr, []string{dl.ID})
	if len(got) != 1 {
		t.Fatalf("stalledIDs = %v, want the paused download", got)
	}
}
