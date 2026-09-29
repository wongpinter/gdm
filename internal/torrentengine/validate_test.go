package torrentengine

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateFilesChecksumManifest(t *testing.T) {
	dir := t.TempDir()
	payload := []byte("hello manifest payload")
	if err := os.WriteFile(filepath.Join(dir, "audio file.m4b"), payload, 0o644); err != nil {
		t.Fatalf("writing payload: %v", err)
	}
	sum := sha256.Sum256(payload)
	hexSum := hex.EncodeToString(sum[:])

	write := func(name, content string) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("writing manifest: %v", err)
		}
		return p
	}

	for _, tc := range []struct {
		name    string
		line    string
		wantErr string
	}{
		{"text mode with spaces", hexSum + "  audio file.m4b\n", ""},
		{"binary mode asterisk", hexSum + " *audio file.m4b\n", ""},
		{"hash mismatch", strings.Repeat("0", 64) + "  audio file.m4b\n", "SHA-256 mismatch"},
		{"path escape", hexSum + "  ../outside.m4b\n", "escapes download dir"},
		{"malformed line ignored", "not a checksum line\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateFiles(dir, nil, write("manifest.txt", tc.line), false)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}
