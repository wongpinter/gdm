package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/wongpinter/gdm/internal/domain"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	st, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	want := &domain.Download{ID: "dl-1", URL: "http://example.com/f.bin", Status: domain.StatusPaused}
	if err := st.Save(want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := st.Load("dl-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.URL != want.URL || got.Status != want.Status {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}
}

// Two saves of the same id used to share one fixed <id>.json.tmp and
// could interleave into a torn file on disk.
func TestConcurrentSavesOfSameIDStayValid(t *testing.T) {
	st, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const id = "dl-race"
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := st.Save(&domain.Download{ID: id, URL: "http://example.com/f.bin"}); err != nil {
				t.Errorf("Save: %v", err)
			}
		}()
	}
	wg.Wait()

	data, err := os.ReadFile(st.path(id))
	if err != nil {
		t.Fatal(err)
	}
	var d domain.Download
	if err := json.Unmarshal(data, &d); err != nil {
		t.Fatalf("torn state file: %v", err)
	}
	if d.ID != id {
		t.Fatalf("id = %q, want %q", d.ID, id)
	}
}

// A persist already in flight when Delete runs must not resurrect the
// state file: the manager would reload a removed download on restart.
func TestSaveAfterDeleteDoesNotResurrectFile(t *testing.T) {
	st, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	d := &domain.Download{ID: "dl-gone"}
	if err := st.Save(d); err != nil {
		t.Fatal(err)
	}
	if err := st.Delete("dl-gone"); err != nil {
		t.Fatal(err)
	}
	if err := st.Save(d); err != nil {
		t.Fatalf("Save after Delete: %v", err)
	}
	if _, err := os.Stat(st.path("dl-gone")); !os.IsNotExist(err) {
		t.Fatalf("state file recreated after Delete (stat err = %v)", err)
	}
}

func TestStoreRejectsPathTraversalIDs(t *testing.T) {
	st, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bad := []string{"", ".", "..", "../escape", `nested\\id`}
	for _, id := range bad {
		if err := st.Save(&domain.Download{ID: id}); err == nil {
			t.Errorf("Save(%q) accepted invalid id", id)
		}
		if _, err := st.Load(id); err == nil {
			t.Errorf("Load(%q) accepted invalid id", id)
		}
		if err := st.Delete(id); err == nil {
			t.Errorf("Delete(%q) accepted invalid id", id)
		}
	}
}

func TestLoadAllSkipsTempLeftovers(t *testing.T) {
	dir := t.TempDir()
	st, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Save(&domain.Download{ID: "keep"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "keep-999.tmp"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	all, err := st.LoadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].ID != "keep" {
		t.Fatalf("LoadAll = %+v, want just the keep entry", all)
	}
}
