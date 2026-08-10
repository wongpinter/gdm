package tui

import (
	"strings"

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
		m.input.SetValue("")
		m.input.Focus()
		return m, nil

	case "p":
		return m, m.dispatch("paused", func() error { return m.mgr.Pause(m.selectedID()) })

	case "r":
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
	switch msg.Type {
	case tea.KeyEsc:
		m.mode = modeList
		m.input.Blur()
		return m, nil

	case tea.KeyEnter:
		src := m.input.Value()
		m.mode = modeList
		m.input.Blur()
		if src == "" {
			return m, nil
		}
		if isTorrentSource(src) {
			return m, m.dispatch("added", func() error {
				_, err := m.mgr.AddTorrent(src)
				return err
			})
		}
		return m, m.dispatch("added", func() error {
			_, err := m.mgr.Add(src, 0)
			return err
		})
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
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
