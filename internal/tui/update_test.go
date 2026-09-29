package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/wongpinter/gdm/internal/domain"
	"github.com/wongpinter/gdm/internal/manager"
)

func TestHandleListKeyOpensAndClosesHelp(t *testing.T) {
	m := Model{}
	got, cmd := m.handleListKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("?")})
	if cmd != nil || got.(Model).mode != modeHelp {
		t.Fatal("? did not open help")
	}
	got, cmd = got.(Model).handleKey(tea.KeyMsg{Type: tea.KeyEsc})
	if cmd != nil || got.(Model).mode != modeList {
		t.Fatal("Esc did not close help")
	}
}

func TestHandleListKeyIgnoresPauseResumeWithoutSelection(t *testing.T) {
	m := Model{}
	for _, key := range []string{"p", "r"} {
		got, cmd := m.handleListKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)})
		if cmd != nil {
			t.Fatalf("key %q returned command for empty queue", key)
		}
		gotModel, ok := got.(Model)
		if !ok {
			t.Fatalf("key %q returned unexpected model type %T", key, got)
		}
		if gotModel.cursor != m.cursor || gotModel.mode != m.mode {
			t.Fatalf("key %q changed model unexpectedly: got cursor=%d mode=%d", key, gotModel.cursor, gotModel.mode)
		}
	}
}

func TestRefreshMsgAnnouncesSavedPathOnCompletion(t *testing.T) {
	dl := &domain.Download{ID: "1", Status: domain.StatusDownloading, Dest: "/tmp/file.mkv"}
	m := Model{rows: []manager.Snapshot{{Download: dl}}}

	// First refresh adopts the state without announcing anything.
	got, cmd := m.Update(refreshMsg{{Download: dl}})
	if cmd != nil {
		t.Fatal("refresh returned unexpected command")
	}
	m = got.(Model)
	if m.statusMsg != "" {
		t.Fatalf("baseline refresh announced %q", m.statusMsg)
	}

	dl.Status = domain.StatusCompleted
	got, cmd = m.Update(refreshMsg{{Download: dl}})
	if cmd != nil {
		t.Fatal("refresh returned unexpected command")
	}
	m = got.(Model)
	if want := "saved /tmp/file.mkv"; m.statusMsg != want {
		t.Fatalf("statusMsg = %q, want %q", m.statusMsg, want)
	}

	// A later refresh with the same completed row must not re-announce.
	m.statusMsg = ""
	got, _ = m.Update(refreshMsg{{Download: dl}})
	if got.(Model).statusMsg != "" {
		t.Fatalf("completion re-announced on steady state: %q", got.(Model).statusMsg)
	}
}
