package tui

import (
	"fmt"
	"strings"

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
	var b strings.Builder

	b.WriteString(titleStyle.Render("gdm — Go Download Manager"))
	b.WriteString("\n")
	b.WriteString(m.renderSummary())
	b.WriteString("\n\n")

	if m.mode == modeAdd {
		b.WriteString(inputBoxStyle.Render(m.input.View()))
		b.WriteString("\n\n")
	}

	if m.mode == modeConfirmRemove {
		b.WriteString(errStyle.Render(fmt.Sprintf("Remove %q and delete its file? (y/N)", m.selectedName())))
		b.WriteString("\n\n")
	}

	b.WriteString(m.renderHeader())
	b.WriteString("\n")

	if len(m.rows) == 0 {
		b.WriteString(helpStyle.Render("  No downloads yet — press 'a' to add one."))
		b.WriteString("\n")
	}
	for i, snap := range m.rows {
		b.WriteString(m.renderRow(i, snap))
		b.WriteString("\n")
	}

	if detail := m.renderSelectedDetail(); detail != "" {
		b.WriteString("\n")
		b.WriteString(detail)
		b.WriteString("\n")
	}

	b.WriteString("\n")
	if m.errMsg != "" {
		b.WriteString(errStyle.Render(m.errMsg))
		b.WriteString("\n")
	} else if m.statusMsg != "" {
		b.WriteString(helpStyle.Render(m.statusMsg))
		b.WriteString("\n")
	}

	b.WriteString(helpStyle.Render("a add · p pause · r resume · x remove · ↑/↓ select · q quit"))
	return b.String()
}

func (m Model) renderHeader() string {
	if m.narrow() {
		return headerStyle.Render(strings.Join([]string{
			pad("NAME", colName),
			pad("STATUS", colStatus),
			pad("PROGRESS", colBar+colPct+1),
		}, " "))
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
	name := d.Filename
	if name == "" {
		name = d.URL
	}
	if d.EffectiveKind() == domain.KindTorrent {
		name = "\u21bb " + name // ⇃ swarm marker, distinguishes torrents at a glance
	}
	progress := d.Progress()
	remaining := d.TotalSize - d.BytesDownloaded()
	peers := "-"
	if d.EffectiveKind() == domain.KindTorrent {
		peers = fmt.Sprintf("%d", snap.Peers)
	}

	cols := []string{
		pad(truncate(name, colName), colName),
		statusStyle(string(d.Status)).Render(pad(string(d.Status), colStatus)),
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

func (m Model) renderSummary() string {
	active, completed := 0, 0
	var aggregateSpeed float64
	for _, snap := range m.rows {
		aggregateSpeed += snap.SpeedBps
		switch snap.Download.Status {
		case domain.StatusQueued, domain.StatusProbing, domain.StatusDownloading:
			active++
		case domain.StatusCompleted:
			completed++
		}
	}
	return summaryStyle.Render(fmt.Sprintf(
		"%d total · %d active · %d completed · %s aggregate",
		len(m.rows), active, completed, formatSpeed(aggregateSpeed),
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

func detailLine(label, value string, width int) string {
	return detailLabelStyle.Render(label+":") + " " + detailValueStyle.Render(truncate(value, width))
}

func (m Model) narrow() bool {
	return m.width > 0 && m.width < 76
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
	if len(s) >= width {
		return s[:width]
	}
	return s + strings.Repeat(" ", width-len(s))
}

func truncate(s string, width int) string {
	if len(s) <= width {
		return s
	}
	if width <= 1 {
		return s[:width]
	}
	return s[:width-1] + "…"
}
