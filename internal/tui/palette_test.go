package tui

import (
	"testing"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

func TestFilterCommands(t *testing.T) {
	cmds := []paletteCmd{
		{name: "Run statement at cursor", key: "Ctrl+Enter"},
		{name: "Explain statement at cursor", key: "F9"},
		{name: "Clear editor", key: "Ctrl+L"},
	}
	cases := map[string]int{
		"":           3,
		"stat":       2,
		"EXPLAIN":    1,
		"run cursor": 1,
		"f9":         1,
		"ctrl":       2,
		"nothing":    0,
	}
	for q, want := range cases {
		if got := len(filterCommands(cmds, q)); got != want {
			t.Errorf("filter %q: %d commands, want %d", q, got, want)
		}
	}
}

func TestPalette(t *testing.T) {
	u := newTestUI()
	key := func(k tcell.Key) *tcell.EventKey { return tcell.NewEventKey(k, 0, tcell.ModNone) }
	front := func() string { name, _ := u.root.GetFrontPage(); return name }
	// send passes a key through the global handler and then to the focused widget.
	send := func(k tcell.Key) {
		if ev := u.globalKeys(key(k)); ev != nil {
			u.app.GetFocus().InputHandler()(ev, func(p tview.Primitive) { u.app.SetFocus(p) })
		}
	}

	u.app.SetFocus(u.results)
	send(tcell.KeyCtrlP)
	if front() != "palette" {
		t.Fatalf("Ctrl+P: front page %q", front())
	}
	send(tcell.KeyCtrlP)
	if front() != "main" || u.app.GetFocus() != u.results {
		t.Fatalf("second Ctrl+P: front %q, focus restored %v", front(), u.app.GetFocus() == u.results)
	}

	// Esc closes and restores focus.
	send(tcell.KeyCtrlP)
	send(tcell.KeyEsc)
	if front() != "main" || u.app.GetFocus() != u.results {
		t.Fatalf("Esc: front %q", front())
	}

	// Filter + Enter runs the command.
	u.editor.SetText("select 1", false)
	send(tcell.KeyCtrlP)
	in, ok := u.app.GetFocus().(*tview.InputField)
	if !ok {
		t.Fatal("palette input not focused")
	}
	in.SetText("clear edit")
	send(tcell.KeyEnter)
	if front() != "main" || u.editor.GetText() != "" {
		t.Fatalf("clear editor via palette: front %q, text %q", front(), u.editor.GetText())
	}

	// A command can open another popup.
	send(tcell.KeyCtrlP)
	u.app.GetFocus().(*tview.InputField).SetText("help")
	send(tcell.KeyEnter)
	if front() != "help" {
		t.Fatalf("help via palette: front %q", front())
	}
	// Ctrl+P is ignored over other popups.
	send(tcell.KeyCtrlP)
	if front() != "help" {
		t.Fatalf("Ctrl+P over help: front %q", front())
	}
}
