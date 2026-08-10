package tui

import "github.com/charmbracelet/lipgloss"

var (
	colorAccent = lipgloss.Color("39")  // blue
	colorGood   = lipgloss.Color("42")  // green
	colorWarn   = lipgloss.Color("214") // amber
	colorBad    = lipgloss.Color("203") // red
	colorMuted  = lipgloss.Color("244") // gray
	colorText   = lipgloss.Color("252")

	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("0")).
			Background(colorAccent).
			Padding(0, 1)

	headerStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(colorMuted)

	selectedRowStyle = lipgloss.NewStyle().
				Background(lipgloss.Color("236")).
				Foreground(colorText)

	rowStyle = lipgloss.NewStyle().Foreground(colorText)

	helpStyle = lipgloss.NewStyle().Foreground(colorMuted)

	summaryStyle = lipgloss.NewStyle().Foreground(colorText)

	detailStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colorMuted).
			Padding(0, 1)

	detailLabelStyle = lipgloss.NewStyle().Foreground(colorAccent).Bold(true)
	detailValueStyle = lipgloss.NewStyle().Foreground(colorText)

	errStyle = lipgloss.NewStyle().Foreground(colorBad).Bold(true)

	barEmptyStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("238"))

	inputBoxStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colorAccent).
			Padding(0, 1)
)

func statusStyle(status string) lipgloss.Style {
	switch status {
	case "downloading":
		return lipgloss.NewStyle().Foreground(colorAccent).Bold(true)
	case "completed":
		return lipgloss.NewStyle().Foreground(colorGood).Bold(true)
	case "paused":
		return lipgloss.NewStyle().Foreground(colorWarn)
	case "failed":
		return lipgloss.NewStyle().Foreground(colorBad).Bold(true)
	case "queued", "probing", "scheduled":
		return lipgloss.NewStyle().Foreground(colorMuted)
	default:
		return rowStyle
	}
}

func barFilledStyle(status string) lipgloss.Style {
	switch status {
	case "completed":
		return lipgloss.NewStyle().Foreground(colorGood)
	case "failed":
		return lipgloss.NewStyle().Foreground(colorBad)
	case "paused":
		return lipgloss.NewStyle().Foreground(colorWarn)
	default:
		return lipgloss.NewStyle().Foreground(colorAccent)
	}
}
