package mediaorg

import (
	"os"
	"path/filepath"
	"testing"
)

// When the output directory contains the input directory (organizing an
// inbox folder into its parent), every directory walked is "inside" the
// output — a containment check there skipped them all and discovery
// returned nothing.
func TestDiscoveryFindsFilesWhenOutputContainsInput(t *testing.T) {
	root := t.TempDir()
	output := filepath.Join(root, "library")
	input := filepath.Join(output, "inbox")
	file := filepath.Join(input, "Show", "Season 1", "Show - S01E01.mkv")
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("video"), 0o644); err != nil {
		t.Fatal(err)
	}

	files, issues, err := discover(input, output)
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 0 {
		t.Fatalf("scan issues = %+v", issues)
	}
	if len(files) != 1 || files[0] != file {
		t.Fatalf("discovered files = %v, want [%s]", files, file)
	}
}
