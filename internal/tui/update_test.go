package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
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
