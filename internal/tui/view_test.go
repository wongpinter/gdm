package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/wongpinter/gdm/internal/domain"
	"github.com/wongpinter/gdm/internal/manager"
)

func TestVisibleRowsScrollsToCursor(t *testing.T) {
	m := Model{height: 12, cursor: 5, rows: make([]manager.Snapshot, 10)}
	start, end := m.visibleRows()
	if start != 3 || end != 6 {
		t.Fatalf("visible rows = (%d, %d), want (3, 6)", start, end)
	}
}

func TestSummaryShowsGroupedQueueState(t *testing.T) {
	now := time.Now()
	m := Model{rows: []manager.Snapshot{
		{Download: &domain.Download{Status: domain.StatusDownloading}},
		{Download: &domain.Download{Status: domain.StatusQueued, StartAt: now.Add(time.Hour)}},
		{Download: &domain.Download{Status: domain.StatusQueued}},
		{Download: &domain.Download{Status: domain.StatusCompleted}},
		{Download: &domain.Download{Status: domain.StatusFailed}},
	}}
	got := m.renderSummary()
	for _, want := range []string{"1 active", "1 queued", "1 scheduled", "1 done", "1 failed"} {
		if !strings.Contains(got, want) {
			t.Fatalf("summary %q missing %q", got, want)
		}
	}
}

func TestSplitDashboardOnlyWide(t *testing.T) {
	m := Model{width: 120, height: 20, rows: []manager.Snapshot{{Download: &domain.Download{ID: "id", URL: "https://example.com", Status: domain.StatusPaused}}}}
	if !m.split() || !strings.Contains(m.View(), "SELECTED") {
		t.Fatal("wide view did not render split dashboard")
	}
	m.width = 100
	if m.split() {
		t.Fatal("compact view incorrectly selected split layout")
	}
}

func TestNarrowColumnsFitTerminal(t *testing.T) {
	for width := 1; width < 76; width++ {
		columns := (Model{width: width}).narrowColumns()
		total := 0
		for _, column := range columns {
			total += column
		}
		if len(columns) > 1 {
			total += len(columns) - 1
		}
		if total > width {
			t.Fatalf("width %d: columns %v occupy %d cells", width, columns, total)
		}
	}
}
