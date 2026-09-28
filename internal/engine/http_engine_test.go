package engine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wongpinter/gdm/internal/domain"
	"github.com/wongpinter/gdm/internal/manager"
)

func TestContentRangeStart(t *testing.T) {
	cases := []struct {
		header string
		want   int64
		ok     bool
	}{
		{"bytes 100-199/2000", 100, true},
		{"bytes 0-0/*", 0, true},
		{"bytes 5-9", 5, true},
		{"", 0, false},
		{"items 100-199/2000", 0, false},
		{"bytes -199/2000", 0, false},
		{"bytes abc-199/2000", 0, false},
		{"bytes 100", 0, false},
	}
	for _, tc := range cases {
		got, ok := contentRangeStart(tc.header)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("contentRangeStart(%q) = (%d, %v), want (%d, %v)", tc.header, got, ok, tc.want, tc.ok)
		}
	}
}

// A proxy or CDN that strips Range answers 200 with the whole body.
// Writing that at a nonzero offset corrupts the file, so the engine
// must fail instead of accepting the response.
func TestRunFailsWhenServerIgnoresRange(t *testing.T) {
	data := make([]byte, 40)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(data) // no 206, no Content-Range, whole body
	}))
	t.Cleanup(srv.Close)

	dest := filepath.Join(t.TempDir(), "out.bin")
	d := &domain.Download{
		ID:            "t",
		URL:           srv.URL,
		Dest:          dest,
		TotalSize:     int64(len(data)),
		SupportsRange: true,
		Segments: []domain.Segment{
			{Index: 0, Start: 0, End: 9},
			{Index: 1, Start: 10, End: 19},
			{Index: 2, Start: 20, End: 29},
			{Index: 3, Start: 30, End: 39},
		},
	}
	err := New().Run(context.Background(), d, make(chan manager.ProgressEvent, 64))
	if err == nil {
		t.Fatal("Run succeeded although the server ignored the Range request")
	}
	if !strings.Contains(err.Error(), "Range") {
		t.Errorf("error = %v, want it to mention the Range request", err)
	}
}

// A body that ends before the segment's range does would otherwise
// leave the remainder of the range as zeros in a "completed" file.
func TestRunFailsOnShortBody(t *testing.T) {
	body := make([]byte, 10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	dest := filepath.Join(t.TempDir(), "out.bin")
	d := &domain.Download{
		ID:            "t",
		URL:           srv.URL,
		Dest:          dest,
		TotalSize:     100,
		SupportsRange: false,
		Segments:      []domain.Segment{{Index: 0, Start: 0, End: 99}},
	}
	err := New().Run(context.Background(), d, make(chan manager.ProgressEvent, 64))
	if err == nil {
		t.Fatal("Run succeeded although the server sent 10 of 100 bytes")
	}
	if !strings.Contains(err.Error(), "10 of 100 bytes") {
		t.Errorf("error = %v, want it to report 10 of 100 bytes", err)
	}
}

// Unknown-size downloads use one unbounded segment; the transport is
// the only layer that can declare them finished (at EOF).
func TestRunDownloadsUnknownSizeBody(t *testing.T) {
	payload := []byte("chunked body of unknown length")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush() // force chunked: no Content-Length
		_, _ = w.Write(payload)
	}))
	t.Cleanup(srv.Close)

	dest := filepath.Join(t.TempDir(), "out.bin")
	d := &domain.Download{
		ID:            "t",
		URL:           srv.URL,
		Dest:          dest,
		TotalSize:     0,
		SupportsRange: false,
		Segments:      []domain.Segment{{Index: 0, Start: 0, End: -1}},
	}
	if err := New().Run(context.Background(), d, make(chan manager.ProgressEvent, 64)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Fatalf("content = %q, want %q", got, payload)
	}
}
