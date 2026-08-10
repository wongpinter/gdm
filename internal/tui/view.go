package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/wongpinter/gdm/internal/domain"
	"github.com/wongpinter/gdm/internal/manager"
)

const (
	colName   = 26
	colStatus = 12
	colBar    = 20
	colPct    = 5
	colSpeed  = 10
	colSize   = 10
	colPeers  = 6
	colETA    = 8
)

func (m Model) View() string {
	if m.mode == modeHelp {
		return m.renderHelp()
	}
	var b strings.Builder

	b.WriteString(titleStyle.Render("gdm — Go Download Manager"))
	b.WriteString("\n")
	b.WriteString(m.renderSummary())
	b.WriteString("\n\n")

	if m.mode == modeAdd {
		b.WriteString(inputBoxStyle.Render(strings.Join([]string{
			"ADD DOWNLOAD",
			m.input.View(),
			m.scheduleInput.View(),
			"Tab next field · Enter add · Esc cancel",
		}, "\n")))
		b.WriteString("\n\n")
	}

	if m.mode == modeConfirmRemove {
		b.WriteString(errStyle.Render(fmt.Sprintf("Remove %q and delete its file? (y/N)", m.selectedName())))
		b.WriteString("\n\n")
	}

	if m.split() {
		b.WriteString(m.renderSplitDashboard())
	} else {
		b.WriteString(m.renderQueue())
		if detail := m.renderSelectedDetail(); detail != "" && m.detailVisible() {
			b.WriteString("\n")
			b.WriteString(detail)
			b.WriteString("\n")
		}
	}

	b.WriteString("\n")
	if m.errMsg != "" {
		b.WriteString(errStyle.Render(m.errMsg))
		b.WriteString("\n")
	} else if m.statusMsg != "" {
		b.WriteString(helpStyle.Render(m.statusMsg))
		b.WriteString("\n")
	}

	b.WriteString(helpStyle.Render("a add · p pause · r resume · x remove · ↑/↓ select · ? help · q quit"))
	return b.String()
}

func (m Model) split() bool { return m.width >= 120 && m.height >= 14 }

func (m Model) renderQueue() string {
	var b strings.Builder
	b.WriteString(detailLabelStyle.Render("QUEUE"))
	b.WriteString("\n")
	b.WriteString(m.renderHeader())
	b.WriteString("\n")
	if len(m.rows) == 0 {
		b.WriteString(helpStyle.Render("  No downloads yet — press 'a' to add one."))
		b.WriteString("\n")
	}
	start, end := m.visibleRows()
	for i := start; i < end; i++ {
		b.WriteString(m.renderRow(i, m.rows[i]))
		b.WriteString("\n")
	}
	if end < len(m.rows) {
		b.WriteString(helpStyle.Render(fmt.Sprintf("  showing %d-%d of %d", start+1, end, len(m.rows))))
		b.WriteString("\n")
	}
	return b.String()
}

func (m Model) renderSplitDashboard() string {
	left := m.renderQueue()
	right := m.renderSelectedDetail()
	if right == "" {
		right = detailStyle.Render("SELECTED\nNo download selected")
	}
	leftLines := strings.Split(left, "\n")
	rightLines := strings.Split(right, "\n")
	rows := max(len(leftLines), len(rightLines))
	leftWidth := max(m.width/2-3, 20)
	var b strings.Builder
	b.WriteString(detailLabelStyle.Render("QUEUE"))
	b.WriteString(strings.Repeat(" ", max(leftWidth-5, 1)))
	b.WriteString(detailLabelStyle.Render("SELECTED"))
	b.WriteString("\n")
	for i := 0; i < rows; i++ {
		l, r := "", ""
		if i < len(leftLines) {
			l = leftLines[i]
		}
		if i < len(rightLines) {
			r = rightLines[i]
		}
		b.WriteString(pad(truncate(l, leftWidth), leftWidth))
		b.WriteString(" │ ")
		b.WriteString(truncate(r, max(m.width-leftWidth-3, 12)))
		b.WriteString("\n")
	}
	return b.String()
}

func (m Model) renderHeader() string {
	if m.narrow() {
		cols := m.narrowColumns()
		headers := []string{pad("NAME", cols[0])}
		if len(cols) > 1 {
			headers = append(headers, pad("STATUS", cols[1]))
		}
		if len(cols) > 2 {
			headers = append(headers, pad("PROGRESS", cols[2]))
		}
		return headerStyle.Render(strings.Join(headers, " "))
	}
	if m.compact() {
		return headerStyle.Render(strings.Join([]string{
			pad("NAME", colName),
			pad("STATUS", colStatus),
			pad("PROGRESS", colBar+colPct+1),
			pad("SPEED", colSpeed),
		}, " "))
	}
	cols := []string{
		pad("NAME", colName),
		pad("STATUS", colStatus),
		pad("PROGRESS", colBar+colPct+1),
		pad("SPEED", colSpeed),
		pad("SIZE", colSize),
		pad("PEERS", colPeers),
		pad("ETA", colETA),
	}
	return headerStyle.Render(strings.Join(cols, " "))
}

func (m Model) renderRow(i int, snap manager.Snapshot) string {
	d := snap.Download
	statusIcon := map[string]string{
		"queued": "○", "probing": "…", "downloading": "●", "paused": "Ⅱ", "completed": "✓", "failed": "!",
	}[string(d.Status)]
	name := d.Filename
	if name == "" {
		name = d.URL
	}
	if d.EffectiveKind() == domain.KindTorrent {
		name = "↻ " + name
	}
	if statusIcon != "" {
		name = statusIcon + " " + name
	}
	progress := d.Progress()
	remaining := d.TotalSize - d.BytesDownloaded()
	status := string(d.Status)
	if d.Status == domain.StatusQueued && !d.StartAt.IsZero() && time.Now().Before(d.StartAt) {
		status = "scheduled"
	}
	peers := "-"
	if d.EffectiveKind() == domain.KindTorrent {
		peers = fmt.Sprintf("%d", snap.Peers)
	}

	if m.narrow() {
		columns := m.narrowColumns()
		cols := []string{pad(truncate(name, columns[0]), columns[0])}
		if len(columns) > 1 {
			cols = append(cols, statusStyle(status).Render(pad(status, columns[1])))
		}
		if len(columns) > 2 {
			barWidth := max(columns[2]-colPct-1, 4)
			cols = append(cols, renderBar(d.Status, progress, barWidth)+" "+pad(fmt.Sprintf("%3.0f%%", progress*100), colPct))
		}
		line := strings.Join(cols, " ")
		if i == m.cursor {
			return selectedRowStyle.Render(line)
		}
		return rowStyle.Render(line)
	}

	cols := []string{
		pad(truncate(name, colName), colName),
		statusStyle(status).Render(pad(status, colStatus)),
		renderBar(d.Status, progress, colBar) + " " + pad(fmt.Sprintf("%3.0f%%", progress*100), colPct),
	}
	if !m.narrow() {
		cols = append(cols, pad(formatSpeed(snap.SpeedBps), colSpeed))
	}
	if !m.compact() {
		cols = append(cols,
			pad(formatBytes(d.TotalSize), colSize),
			pad(peers, colPeers),
			pad(formatETA(remaining, snap.SpeedBps), colETA),
		)
	}

	line := strings.Join(cols, " ")
	if i == m.cursor {
		return selectedRowStyle.Render(line)
	}
	return rowStyle.Render(line)
}

func (m Model) renderHelp() string {
	return detailStyle.Render(strings.Join([]string{
		titleStyle.Render("GDM HELP"),
		"↑/↓ or j/k   select download",
		"a             add download",
		"p / r         pause / resume",
		"x             remove and delete file",
		"Enter         confirm or open action",
		"Esc or ?      close this help",
		"q             quit",
	}, "\n"))
}

func (m Model) renderSummary() string {
	active, queued, scheduled, completed, failed := 0, 0, 0, 0, 0
	var aggregateSpeed float64
	for _, snap := range m.rows {
		aggregateSpeed += snap.SpeedBps
		switch snap.Download.Status {
		case domain.StatusProbing, domain.StatusDownloading:
			active++
		case domain.StatusQueued:
			if !snap.Download.StartAt.IsZero() && time.Now().Before(snap.Download.StartAt) {
				scheduled++
			} else {
				queued++
			}
		case domain.StatusCompleted:
			completed++
		case domain.StatusFailed:
			failed++
		}
	}
	return summaryStyle.Render(fmt.Sprintf(
		"%d total · %d active · %d queued · %d scheduled · %d done · %d failed · %s aggregate",
		len(m.rows), active, queued, scheduled, completed, failed, formatSpeed(aggregateSpeed),
	))
}

func (m Model) renderSelectedDetail() string {
	if m.cursor < 0 || m.cursor >= len(m.rows) {
		return ""
	}
	d := m.rows[m.cursor].Download
	lines := []string{
		detailLabelStyle.Render("SELECTED DOWNLOAD"),
		detailLine("URL", d.URL, m.detailWidth()),
		detailLine("DESTINATION", d.Dest, m.detailWidth()),
		detailLine("STATUS", string(d.Status), m.detailWidth()),
	}
	if !d.StartAt.IsZero() && d.Status == domain.StatusQueued {
		lines = append(lines, detailLine("STARTS", d.StartAt.Local().Format("2006-01-02 15:04"), m.detailWidth()))
	}
	if d.Error != "" {
		lines = append(lines, detailLine("ERROR", d.Error, m.detailWidth()))
	}
	return detailStyle.Render(strings.Join(lines, "\n"))
}

func (m Model) detailWidth() int {
	if m.width <= 0 {
		return 80
	}
	return max(m.width-16, 12)
}

func (m Model) visibleRows() (int, int) {
	if len(m.rows) == 0 {
		return 0, 0
	}
	start := 0
	if m.height > 0 {
		available := m.height - 9
		if available < 1 {
			available = 1
		}
		if m.cursor >= available {
			start = m.cursor - available + 1
		}
	}
	end := len(m.rows)
	if m.height > 0 && end > start+max(m.height-9, 1) {
		end = start + max(m.height-9, 1)
	}
	return start, end
}

func (m Model) detailVisible() bool {
	return m.height <= 0 || m.height >= 14
}

func detailLine(label, value string, width int) string {
	return detailLabelStyle.Render(label+":") + " " + detailValueStyle.Render(truncate(value, width))
}

func (m Model) narrow() bool {
	return m.width > 0 && m.width < 76
}

func (m Model) narrowColumns() []int {
	available := max(m.width, 1)
	progressWidth := colBar + colPct + 1
	if available < 1+colStatus+progressWidth+2 {
		return []int{available}
	}
	nameWidth := min(colName, available-colStatus-progressWidth-2)
	return []int{nameWidth, colStatus, progressWidth}
}

func (m Model) compact() bool {
	return m.width > 0 && m.width < 110
}

func (m Model) selectedName() string {
	if m.cursor < 0 || m.cursor >= len(m.rows) {
		return ""
	}
	d := m.rows[m.cursor].Download
	if d.Filename != "" {
		return d.Filename
	}
	return d.URL
}

func pad(s string, width int) string {
	if width <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) >= width {
		return string(runes[:width])
	}
	return s + strings.Repeat(" ", width-len(runes))
}

func truncate(s string, width int) string {
	if width <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= width {
		return s
	}
	if width == 1 {
		return string(runes[:1])
	}
	return string(runes[:width-1]) + "…"
}
