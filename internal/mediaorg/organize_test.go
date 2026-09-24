package mediaorg

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPlanTVEpisodeByFilenameAndSeasonFolder(t *testing.T) {
	root := t.TempDir()
	input := filepath.Join(root, "input")
	output := filepath.Join(root, "library")
	file := filepath.Join(input, "The Simpsons", "Season 35", "S35E03.1080p.mkv")
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("video"), 0o644); err != nil {
		t.Fatal(err)
	}
	server := tvmazeServer(t)
	defer server.Close()

	items, err := Plan(context.Background(), Options{Kind: TV, Input: input, Output: output, TVMazeURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Err != nil {
		t.Fatalf("items = %+v", items)
	}
	want := filepath.Join(output, "The Simpsons", "Season 35", "The Simpsons - S35E03 - McMansion and Wife.mkv")
	if items[0].Destination != want {
		t.Fatalf("destination = %q, want %q", items[0].Destination, want)
	}
}

func TestPlanMovieWithTMDbToken(t *testing.T) {
	t.Setenv("TMDB_API_KEY", "")
	t.Setenv("TMDB_READ_ACCESS_TOKEN", "secret-test-token")
	root := t.TempDir()
	input := filepath.Join(root, "input")
	file := filepath.Join(input, "Arrival (2016).mkv")
	if err := os.MkdirAll(input, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("video"), 0o644); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret-test-token" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		if got := r.URL.Query().Get("year"); got != "2016" {
			t.Errorf("year = %q", got)
		}
		io.WriteString(w, `{"results":[{"title":"Arrival","release_date":"2016-11-10"}]}`)
	}))
	defer server.Close()

	items, err := Plan(context.Background(), Options{Kind: Movie, Input: input, Output: filepath.Join(root, "movies"), TMDBURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "movies", "Arrival (2016)", "Arrival (2016).mkv")
	if len(items) != 1 || items[0].Err != nil || items[0].Destination != want {
		t.Fatalf("items = %+v, want destination %q", items, want)
	}
}

func TestRunDryRunDoesNotWrite(t *testing.T) {
	root := t.TempDir()
	input := filepath.Join(root, "input")
	file := filepath.Join(input, "Arrival (2016).mkv")
	if err := os.MkdirAll(input, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("video"), 0o644); err != nil {
		t.Fatal(err)
	}
	server := movieServer(t)
	defer server.Close()
	output := filepath.Join(root, "movies")
	var log strings.Builder
	if err := Run(context.Background(), Options{Kind: Movie, Input: input, Output: output, TMDBURL: server.URL}, &log); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatalf("dry run created output: %v", err)
	}
	if !strings.Contains(log.String(), "[DRY-RUN]") {
		t.Fatalf("unexpected output: %s", log.String())
	}
}

func TestRunApplyCopiesAndRefusesOverwrite(t *testing.T) {
	root := t.TempDir()
	input := filepath.Join(root, "input")
	file := filepath.Join(input, "Arrival (2016).mkv")
	if err := os.MkdirAll(input, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("video"), 0o644); err != nil {
		t.Fatal(err)
	}
	server := movieServer(t)
	defer server.Close()
	output := filepath.Join(root, "movies")
	opts := Options{Kind: Movie, Input: input, Output: output, Apply: true, TMDBURL: server.URL}
	var log strings.Builder
	if err := Run(context.Background(), opts, &log); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(output, "Arrival (2016)", "Arrival (2016).mkv")
	if b, err := os.ReadFile(destination); err != nil || string(b) != "video" {
		t.Fatalf("copied file = %q, err=%v", b, err)
	}
	if err := os.WriteFile(destination, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), opts, &log); err == nil {
		t.Fatal("second copy should fail rather than overwrite")
	}
	if b, err := os.ReadFile(destination); err != nil || string(b) != "keep" {
		t.Fatalf("existing file changed: %q, err=%v", b, err)
	}
}

func TestRunApplySourceAlreadyAtDestinationDoesNotTruncate(t *testing.T) {
	t.Setenv("TMDB_READ_ACCESS_TOKEN", "test-token")
	root := t.TempDir()
	file := filepath.Join(root, "Arrival (2016)", "Arrival (2016).mkv")
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("keep source"), 0o644); err != nil {
		t.Fatal(err)
	}
	server := movieServer(t)
	defer server.Close()
	var log strings.Builder
	err := Run(context.Background(), Options{
		Kind: Movie, Input: file, Output: root, Apply: true, TMDBURL: server.URL,
	}, &log)
	if err == nil {
		t.Fatal("organizing a file already at its destination should fail")
	}
	if b, err := os.ReadFile(file); err != nil || string(b) != "keep source" {
		t.Fatalf("source changed: %q, err=%v", b, err)
	}
}

func TestDiscoveryRejectsOutputUnderFileInput(t *testing.T) {
	root := t.TempDir()
	input := filepath.Join(root, "Arrival.mkv")
	if err := os.WriteFile(input, []byte("video"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := discover(input, filepath.Join(input, "organized"))
	if err == nil {
		t.Fatal("output below input file should be rejected")
	}
}

func TestDiscoverySkipsOutputNestedInsideInput(t *testing.T) {
	root := t.TempDir()
	input := filepath.Join(root, "library")
	output := filepath.Join(input, "organized")
	for _, path := range []string{
		filepath.Join(input, "Arrival (2016).mkv"),
		filepath.Join(output, "Old Movie (2000).mkv"),
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("video"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	files, issues, err := discover(input, output)
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 0 {
		t.Fatalf("scan issues = %+v", issues)
	}
	if len(files) != 1 || filepath.Base(files[0]) != "Arrival (2016).mkv" {
		t.Fatalf("discovered files = %v", files)
	}
}

func TestMovieYearlessFilenameUsesFullTitle(t *testing.T) {
	t.Setenv("TMDB_READ_ACCESS_TOKEN", "test-token")
	root := t.TempDir()
	file := filepath.Join(root, "Arrival.mkv")
	if err := os.WriteFile(file, []byte("video"), 0o644); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("query"); got != "Arrival" {
			t.Errorf("query = %q, want Arrival", got)
		}
		io.WriteString(w, `{"results":[{"title":"Arrival","release_date":"2016-11-10"}]}`)
	}))
	defer server.Close()
	items, err := Plan(context.Background(), Options{Kind: Movie, Input: file, Output: filepath.Join(root, "movies"), TMDBURL: server.URL})
	if err != nil || len(items) != 1 || items[0].Err != nil {
		t.Fatalf("Plan = %+v, %v", items, err)
	}
	if !strings.HasSuffix(items[0].Destination, filepath.Join("Arrival (2016)", "Arrival (2016).mkv")) {
		t.Fatalf("destination = %q", items[0].Destination)
	}
}

func TestMovieRejectsInvalidTMDbMetadata(t *testing.T) {
	for _, tc := range []struct {
		name, title, releaseDate string
	}{
		{name: "empty sanitized title", title: ".", releaseDate: "2016-11-10"},
		{name: "invalid date", title: "Arrival", releaseDate: "2016-99-99"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TMDB_READ_ACCESS_TOKEN", "test-token")
			root := t.TempDir()
			file := filepath.Join(root, "Arrival.mkv")
			if err := os.WriteFile(file, []byte("video"), 0o644); err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `{"results":[{"title":%q,"release_date":%q}]}`, tc.title, tc.releaseDate)
			}))
			defer server.Close()
			items, err := Plan(context.Background(), Options{
				Kind: Movie, Input: file, Output: filepath.Join(root, "out"), TMDBURL: server.URL,
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(items) != 1 || items[0].Err == nil {
				t.Fatalf("Plan = %+v; expected invalid metadata error", items)
			}
		})
	}
}

func TestDiscoveryReportsUnreadableSubdirectory(t *testing.T) {
	root := t.TempDir()
	input := filepath.Join(root, "input")
	child := filepath.Join(input, "locked")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(input, "Arrival (2016).mkv"), []byte("video"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(child, "Hidden (2000).mkv"), []byte("video"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(child, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(child, 0o755)
	if _, err := os.ReadDir(child); err == nil {
		t.Skip("current user can read mode-000 directory")
	}

	files, issues, err := discover(input, filepath.Join(root, "out"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || len(issues) != 1 || issues[0].Source != child || issues[0].Err == nil {
		t.Fatalf("files = %v, scan issues = %+v", files, issues)
	}
}

func TestMovieRequiresTMDbCredential(t *testing.T) {
	t.Setenv("TMDB_API_KEY", "")
	t.Setenv("TMDB_READ_ACCESS_TOKEN", "")
	root := t.TempDir()
	file := filepath.Join(root, "Arrival (2016).mkv")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	items, err := Plan(context.Background(), Options{Kind: Movie, Input: file, Output: filepath.Join(root, "out"), TMDBURL: "http://127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Err == nil || !strings.Contains(items[0].Err.Error(), "TMDB_API_KEY") {
		t.Fatalf("items = %+v", items)
	}
}

func tvmazeServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/search/shows":
			io.WriteString(w, `[{"score":1,"show":{"id":42,"name":"The Simpsons"}}]`)
		case r.URL.Path == "/shows/42/episodes":
			io.WriteString(w, `[{"season":35,"number":3,"name":"McMansion and Wife"}]`)
		default:
			http.NotFound(w, r)
		}
	}))
}

func movieServer(t *testing.T) *httptest.Server {
	t.Helper()
	t.Setenv("TMDB_READ_ACCESS_TOKEN", "test-token")
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"results":[{"title":"Arrival","release_date":"2016-11-10"}]}`)
	}))
}
