package tui

import (
	"testing"

	"github.com/wongpinter/gdm/internal/manager"
)

func TestVisibleRowsScrollsToCursor(t *testing.T) {
	m := Model{height: 12, cursor: 5, rows: make([]manager.Snapshot, 10)}
	start, end := m.visibleRows()
	if start != 3 || end != 6 {
		t.Fatalf("visible rows = (%d, %d), want (3, 6)", start, end)
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
