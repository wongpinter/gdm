package manager

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/wongpinter/gdm/internal/domain"
)

// Dest names come from torrent metadata, state files, or
// Content-Disposition headers: nothing may resolve outside the
// configured download directory.
func TestJoinWithinBlocksTraversal(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name string
		want bool
	}{
		{"file.bin", true},
		{"sub/file.bin", true},
		{"../evil.bin", false},
		{"a/../../evil.bin", false},
		{"..", false},
		{"", false}, // resolves to dir itself — never a valid Dest
	}
	for _, tc := range cases {
		dest, ok := joinWithin(dir, tc.name)
		if ok != tc.want {
			t.Errorf("joinWithin(%q) ok = %v, want %v (dest %q)", tc.name, ok, tc.want, dest)
		}
		if tc.want && !withinDir(dir, dest) {
			t.Errorf("joinWithin(%q) = %q, not inside %q", tc.name, dest, dir)
		}
	}
}

// reserveName is the trust boundary for Content-Disposition names; a
// server must not be able to make the download land outside dir.
func TestReserveNameStripsDirectories(t *testing.T) {
	cases := map[string]string{
		"plain.bin":      "plain.bin",
		"../../evil.bin": "evil.bin",
		"/etc/evil.bin":  "evil.bin",
		"..":             "download",
		"":               "download",
	}
	for in, want := range cases {
		// fresh dir per case: reserveName dedupes existing files
		dir := t.TempDir()
		got, err := reserveName(dir, in)
		if err != nil {
			t.Fatalf("reserveName(%q): %v", in, err)
		}
		if got != want {
			t.Errorf("reserveName(%q) = %q, want %q", in, got, want)
		}
		if _, err := os.Stat(filepath.Join(dir, got)); err != nil {
			t.Errorf("reserveName(%q) did not create %s: %v", in, got, err)
		}
		if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "evil.bin")); !os.IsNotExist(err) {
			t.Errorf("reserveName(%q) escaped the download dir: %v", in, err)
		}
	}
}

// State outlives its destination file: persisted progress above what
// is on disk must be trimmed or the run resumes into a hole of zeros
// while still reporting completion.
func TestReconcileProgressClampsToBytesOnDisk(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "f.bin")
	if err := os.WriteFile(dest, make([]byte, 50), 0o644); err != nil {
		t.Fatal(err)
	}
	e := &entry{dl: &domain.Download{
		SupportsRange: true,
		Dest:          dest,
		Segments:      []domain.Segment{{Index: 0, Start: 0, End: 99, Downloaded: 100}},
	}}
	if !e.reconcileProgress() {
		t.Fatal("reconcileProgress reported no change for over-claimed progress")
	}
	if got := e.dl.Segments[0].Downloaded; got != 50 {
		t.Errorf("Downloaded = %d, want 50", got)
	}
}

// A segment whose range starts beyond the shrunken file has nothing
// left on disk and must restart from its Start.
func TestReconcileProgressResetsSegmentBeyondFile(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "f.bin")
	if err := os.WriteFile(dest, make([]byte, 128), 0o644); err != nil {
		t.Fatal(err)
	}
	e := &entry{dl: &domain.Download{
		SupportsRange: true,
		Dest:          dest,
		Segments: []domain.Segment{
			{Index: 0, Start: 0, End: 63, Downloaded: 64},
			{Index: 1, Start: 64, End: 127, Downloaded: 64},
			{Index: 2, Start: 128, End: 191, Downloaded: 64},
		},
	}}
	e.reconcileProgress()
	want := []int64{64, 64, 0}
	for i, w := range want {
		if got := e.dl.Segments[i].Downloaded; got != w {
			t.Errorf("segment %d Downloaded = %d, want %d", i, got, w)
		}
	}
}

func TestReconcileProgressResetsWhenDestMissing(t *testing.T) {
	e := &entry{dl: &domain.Download{
		SupportsRange: true,
		Dest:          filepath.Join(t.TempDir(), "gone.bin"),
		Segments:      []domain.Segment{{Index: 0, Start: 0, End: 99, Downloaded: 100}},
	}}
	if !e.reconcileProgress() {
		t.Fatal("missing destination reported no change")
	}
	if got := e.dl.Segments[0].Downloaded; got != 0 {
		t.Errorf("Downloaded = %d, want 0 after destination vanished", got)
	}
}

// Without range support the server restarts the body from byte 0, so
// resumed offsets would be written over fresh head-of-file data.
func TestReconcileProgressResetsWithoutRangeSupport(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "f.bin")
	if err := os.WriteFile(dest, make([]byte, 100), 0o644); err != nil {
		t.Fatal(err)
	}
	e := &entry{dl: &domain.Download{
		SupportsRange: false,
		Dest:          dest,
		Segments:      []domain.Segment{{Index: 0, Start: 0, End: 99, Downloaded: 40}},
	}}
	if !e.reconcileProgress() {
		t.Fatal("no-range download reported no change")
	}
	if got := e.dl.Segments[0].Downloaded; got != 0 {
		t.Errorf("Downloaded = %d, want 0 without range support", got)
	}
}
