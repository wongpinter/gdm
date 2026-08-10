package tui

import (
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// isTorrentSource reports whether s names a torrent (a magnet URI or a
// path to a local .torrent file) rather than an HTTP(S) URL.
func isTorrentSource(s string) bool {
	return strings.HasPrefix(s, "magnet:") || strings.HasSuffix(strings.ToLower(s), ".torrent")
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case tickMsg:
		return m, tea.Batch(tickCmd(), m.refresh())

	case refreshMsg:
		m.rows = msg
		if m.cursor >= len(m.rows) {
			m.cursor = max(len(m.rows)-1, 0)
		}
		return m, nil

	case actionResultMsg:
		if msg.err != nil {
			m.errMsg = msg.action + ": " + msg.err.Error()
			m.statusMsg = ""
		} else {
			m.statusMsg = msg.action
			m.errMsg = ""
		}
		return m, m.refresh()

	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch m.mode {
	case modeAdd:
		return m.handleAddKey(msg)
	case modeConfirmRemove:
		return m.handleConfirmKey(msg)
	case modeHelp:
		if msg.String() == "?" || msg.Type == tea.KeyEsc {
			m.mode = modeList
		}
		return m, nil
	default:
		return m.handleListKey(msg)
	}
}

func (m Model) handleListKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c", "q":
		return m, tea.Quit

	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
		return m, nil

	case "down", "j":
		if m.cursor < len(m.rows)-1 {
			m.cursor++
		}
		return m, nil

	case "a":
		m.mode = modeAdd
		m.errMsg = ""
		m.statusMsg = ""
		m.addField = 0
		m.input.SetValue("")
		m.scheduleInput.SetValue("")
		m.input.Focus()
		m.scheduleInput.Blur()
		return m, nil

	case "?":
		m.mode = modeHelp
		return m, nil

	case "p":
		if m.selectedID() == "" {
			return m, nil
		}
		return m, m.dispatch("paused", func() error { return m.mgr.Pause(m.selectedID()) })

	case "r":
		if m.selectedID() == "" {
			return m, nil
		}
		return m, m.dispatch("resumed", func() error { return m.mgr.Resume(m.selectedID()) })

	case "x":
		if m.selectedID() == "" {
			return m, nil
		}
		m.mode = modeConfirmRemove
		return m, nil
	}
	return m, nil
}

func (m Model) handleAddKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.Type == tea.KeyEsc {
		m.mode = modeList
		m.input.Blur()
		m.scheduleInput.Blur()
		return m, nil
	}
	if msg.Type == tea.KeyTab {
		m.addField = 1 - m.addField
		if m.addField == 0 {
			m.input.Focus()
			m.scheduleInput.Blur()
			return m, nil
		}
		m.input.Blur()
		m.scheduleInput.Focus()
		return m, nil
	}
	if msg.Type == tea.KeyEnter {
		src := strings.TrimSpace(m.input.Value())
		startText := strings.TrimSpace(m.scheduleInput.Value())
		if src == "" {
			m.errMsg = "add: source is empty"
			return m, nil
		}
		var startAt time.Time
		if startText != "" {
			parsed, err := time.Parse(time.RFC3339, startText)
			if err != nil {
				m.errMsg = "add: start must be RFC3339 (or blank for now)"
				return m, nil
			}
			startAt = parsed
		}
		m.mode = modeList
		m.input.Blur()
		m.scheduleInput.Blur()
		if isTorrentSource(src) {
			return m, m.dispatch("added", func() error {
				_, err := m.mgr.AddTorrentAt(src, startAt)
				return err
			})
		}
		return m, m.dispatch("added", func() error {
			_, err := m.mgr.AddAt(src, 0, startAt)
			return err
		})
	}
	var cmd tea.Cmd
	if m.addField == 0 {
		m.input, cmd = m.input.Update(msg)
	} else {
		m.scheduleInput, cmd = m.scheduleInput.Update(msg)
	}
	return m, cmd
}

func (m Model) handleConfirmKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y", "enter":
		m.mode = modeList
		id := m.selectedID()
		return m, m.dispatch("removed", func() error { return m.mgr.Remove(id, true) })
	default:
		m.mode = modeList
		return m, nil
	}
}

func (m Model) selectedID() string {
	if m.cursor < 0 || m.cursor >= len(m.rows) {
		return ""
	}
	return m.rows[m.cursor].Download.ID
}

// dispatch runs a manager call off the update loop's return path (it's
// still synchronous work, but wrapping it as a tea.Cmd keeps Update
// itself non-blocking and lets bubbletea schedule the result message).
func (m Model) dispatch(label string, fn func() error) tea.Cmd {
	return func() tea.Msg {
		return actionResultMsg{action: label, err: fn()}
	}
}
