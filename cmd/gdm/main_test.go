package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestReorderArgsFlagsAfterPositional(t *testing.T) {
	in := []string{"--headless", "magnet:?xt=urn:btih:abc", "-dir", "/x", "-state", "/s", "-interval", "500ms"}
	want := []string{"--headless", "-dir", "/x", "-state", "/s", "-interval", "500ms", "magnet:?xt=urn:btih:abc"}
	if got := reorderArgs(in); !reflect.DeepEqual(got, want) {
		t.Fatalf("reorderArgs = %q, want %q", got, want)
	}
}

func TestReorderArgsKeepsOrderAndDoubleDash(t *testing.T) {
	in := []string{"-dir", "/x", "a", "--headless", "--", "-dir", "b"}
	want := []string{"-dir", "/x", "--headless", "a", "-dir", "b"}
	if got := reorderArgs(in); !reflect.DeepEqual(got, want) {
		t.Fatalf("reorderArgs = %q, want %q", got, want)
	}
}

func TestReorderArgsEqualsForm(t *testing.T) {
	in := []string{"magnet:abc", "-dir=/x", "--interval=500ms"}
	want := []string{"-dir=/x", "--interval=500ms", "magnet:abc"}
	if got := reorderArgs(in); !reflect.DeepEqual(got, want) {
		t.Fatalf("reorderArgs = %q, want %q", got, want)
	}
}

func writeMarker(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("creating %s: %v", dir, err)
	}
	marker := filepath.Join(dir, "1.json")
	if err := os.WriteFile(marker, []byte(`{"id":"1"}`), 0o644); err != nil {
		t.Fatalf("writing %s: %v", marker, err)
	}
	return marker
}

// A legacy ~/.idm/state moves to ~/.gdm/state on first run — the
// explicit migration plan the maintainer skill requires for the rename.
func TestResolveStateDirMigratesLegacyDir(t *testing.T) {
	home := t.TempDir()
	marker := writeMarker(t, filepath.Join(home, ".idm", "state"))

	got := resolveStateDir(home)
	want := filepath.Join(home, ".gdm", "state")
	if got != want {
		t.Fatalf("resolveStateDir = %q, want %q", got, want)
	}
	if _, err := os.Stat(filepath.Join(want, "1.json")); err != nil {
		t.Fatalf("state file not migrated: %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("legacy state file still present after migration: %v", err)
	}
}

func TestResolveStateDirFreshWhenNoLegacy(t *testing.T) {
	home := t.TempDir()
	want := filepath.Join(home, ".gdm", "state")
	if got := resolveStateDir(home); got != want {
		t.Fatalf("resolveStateDir = %q, want %q", got, want)
	}
}

// Once migrated, the fresh dir wins even if a legacy dir reappears —
// migration must run exactly once, never clobbering current state.
func TestResolveStateDirKeepsMigratedDir(t *testing.T) {
	home := t.TempDir()
	writeMarker(t, filepath.Join(home, ".gdm", "state"))
	legacyMarker := writeMarker(t, filepath.Join(home, ".idm", "state"))

	want := filepath.Join(home, ".gdm", "state")
	if got := resolveStateDir(home); got != want {
		t.Fatalf("resolveStateDir = %q, want %q", got, want)
	}
	if _, err := os.Stat(legacyMarker); err != nil {
		t.Fatalf("legacy dir was touched on an already-migrated home: %v", err)
	}
}

// If the move can't happen (~/.gdm blocked by a file), fall back to the
// legacy path rather than starting over with an empty queue.
func TestResolveStateDirFallsBackWhenMoveFails(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".gdm"), []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(home, ".idm", "state")
	writeMarker(t, legacy)

	want := legacy
	if got := resolveStateDir(home); got != want {
		t.Fatalf("resolveStateDir = %q, want fallback %q", got, want)
	}
}
