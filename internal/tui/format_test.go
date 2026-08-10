package tui

import "testing"

func TestPadPreservesRuneBoundaries(t *testing.T) {
	if got, want := pad("日本語", 2), "日本"; got != want {
		t.Fatalf("pad = %q, want %q", got, want)
	}
	if got, want := pad("日", 3), "日  "; got != want {
		t.Fatalf("pad = %q, want %q", got, want)
	}
}

func TestTruncatePreservesRuneBoundaries(t *testing.T) {
	if got, want := truncate("日本語ファイル", 5), "日本語フ…"; got != want {
		t.Fatalf("truncate = %q, want %q", got, want)
	}
	if got, want := truncate("日本", 1), "日"; got != want {
		t.Fatalf("truncate width 1 = %q, want %q", got, want)
	}
	if got := truncate("日本", 0); got != "" {
		t.Fatalf("truncate width 0 = %q, want empty", got)
	}
}
