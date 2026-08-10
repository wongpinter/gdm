package tui

import "testing"

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
