package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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

// A finished headless run must say where the payload landed, not just
// that everything completed.
func TestHeadlessResultPrintsSavedPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, filepath.Join("testdata", "body.txt"))
	}))
	if srv == nil {
		t.Fatal("no server")
	}
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

	dl, err := mgr.Add(srv.URL+"/payload.bin", 1)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		snap, ok := mgr.Get(dl.ID)
		if ok && snap.Download.Finished() {
			if snap.Download.Error != "" {
				t.Fatalf("download failed: %s", snap.Download.Error)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("download never finished")
		}
		time.Sleep(15 * time.Millisecond)
	}

	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	resErr := headlessResult(mgr, []string{dl.ID})
	_ = w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)

	if resErr != nil {
		t.Fatalf("headlessResult: %v", resErr)
	}
	snap, _ := mgr.Get(dl.ID)
	if want := "gdm: saved " + snap.Download.Dest; !strings.Contains(string(out), want) {
		t.Fatalf("output %q missing %q", out, want)
	}
	if !strings.Contains(string(out), "gdm: all downloads finished") {
		t.Fatalf("output %q missing completion line", out)
	}
}
