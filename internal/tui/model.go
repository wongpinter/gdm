// Package tui is the inbound adapter that lets a person drive the
// Manager from a terminal dashboard. It only ever calls the Manager's
// public methods — it has no idea how downloads actually happen.
package tui

import (
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/wongpinter/gdm/internal/manager"
)

const tickInterval = 500 * time.Millisecond

type mode int

const (
	modeList mode = iota
	modeAdd
	modeConfirmRemove
	modeHelp
)

// Model is bubbletea's immutable-per-frame state. Mutating methods on
// it (per convention) return a new Model rather than modifying in place.
type Model struct {
	mgr *manager.Manager

	rows   []manager.Snapshot
	cursor int
	mode   mode

	input         textinput.Model
	scheduleInput textinput.Model
	addField      int

	statusMsg string
	errMsg    string

	width, height int
}

// New wires a Model to an already-running Manager.
func New(mgr *manager.Manager) Model {
	ti := textinput.New()
	ti.Placeholder = "https://example.com/file.zip or magnet:?xt=..."
	ti.CharLimit = 2048
	ti.Prompt = "URL › "
	si := textinput.New()
	si.Placeholder = "blank = now; RFC3339 e.g. 2026-08-10T22:30:00Z"
	si.Prompt = "Start › "

	return Model{
		mgr:           mgr,
		input:         ti,
		scheduleInput: si,
		rows:          mgr.List(),
	}
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(tickCmd(), m.refresh())
}

type tickMsg time.Time

func tickCmd() tea.Cmd {
	return tea.Tick(tickInterval, func(t time.Time) tea.Msg { return tickMsg(t) })
}

type refreshMsg []manager.Snapshot

// refresh re-reads the manager's live state. This is cheap: List()
// only clones lightweight snapshots, never touches the network.
func (m Model) refresh() tea.Cmd {
	return func() tea.Msg { return refreshMsg(m.mgr.List()) }
}

type actionResultMsg struct {
	action string
	err    error
}
