package tui

import (
	"fmt"
	"strings"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// paletteCmd is one entry of the Ctrl+P command palette.
type paletteCmd struct {
	name string // what it does, matched against the filter
	key  string // its usual shortcut, shown (and matched) next to the name
	run  func()
}

// paletteCommands lists every action the palette can trigger.
func (u *ui) paletteCommands() []paletteCmd {
	return []paletteCmd{
		{"Run editor contents (or selection)", "F5", u.runEditor},
		{"Run statement at cursor", "Ctrl+Enter", u.runStatementAtCursor},
		{"Explain statement at cursor", "F9", func() { u.explainAtCursor(false) }},
		{"Raw explain statement at cursor", "Shift+F9", func() { u.explainAtCursor(true) }},
		{"Cancel running query", "Esc", u.cancelRunning},
		{"History", "F3", u.showHistory},
		{"Export result to CSV", "F8", u.exportCSV},
		{"Copy selection / cell / table name", "Ctrl+C", u.copyFocused},
		{"Paste into editor", "Ctrl+V", u.pasteClipboard},
		{"Clear editor", "Ctrl+L", func() { u.editor.SetText("", false) }},
		{"Autocomplete", "Ctrl+Space", func() {
			u.app.SetFocus(u.editor)
			u.updateCompletion(true)
		}},
		{"Toggle tables sidebar", "F2", u.toggleSidebar},
		{"Filter tables", "/", func() {
			if !u.showSide {
				u.toggleSidebar()
			}
			u.app.SetFocus(u.filter)
		}},
		{"Connection status (ping, loss, jitter, traffic, server)", "", u.showConnStatus},
		{"Reload tables and columns", "r", u.loadTables},
		{"Describe selected table", "d", func() {
			if i := u.tables.GetCurrentItem(); i >= 0 && u.tables.GetItemCount() > 0 {
				name, _ := u.tables.GetItemText(i)
				u.describe(name)
				return
			}
			u.setStatus("[yellow]no table selected[-]")
		}},
		{"Focus SQL editor", "", func() { u.app.SetFocus(u.editor) }},
		{"Focus results", "", func() { u.app.SetFocus(u.results) }},
		{"Focus tables", "", func() {
			if !u.showSide {
				u.toggleSidebar()
			}
			u.app.SetFocus(u.tables)
		}},
		{"Widen tables sidebar", "Ctrl+Shift+Right", func() { u.resizeSidebar(sideWidthStep) }},
		{"Narrow tables sidebar", "Ctrl+Shift+Left", func() { u.resizeSidebar(-sideWidthStep) }},
		{"Grow SQL editor", "Ctrl+Shift+Down", func() { u.resizeEditor(1) }},
		{"Shrink SQL editor", "Ctrl+Shift+Up", func() { u.resizeEditor(-1) }},
		{"Help", "F1", u.showHelp},
		{"Version (build, author, build date)", "", func() { u.showAbout(true) }},
		{"Author", "", func() { u.showAbout(false) }},
		{"Quit", "Ctrl+Q", u.app.Stop},
	}
}

// filterCommands keeps the commands whose name or shortcut contains every
// space-separated word of q (case-insensitive), in their original order.
func filterCommands(cmds []paletteCmd, q string) []paletteCmd {
	words := strings.Fields(strings.ToLower(q))
	var out []paletteCmd
	for _, c := range cmds {
		hay := strings.ToLower(c.name + " " + c.key)
		ok := true
		for _, w := range words {
			if !strings.Contains(hay, w) {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, c)
		}
	}
	return out
}

// showPalette opens the command palette: type to filter, Up/Down to pick,
// Enter to run, Esc to close.
func (u *ui) showPalette() {
	u.hideCompletion()
	u.palPrev = u.app.GetFocus()
	cmds := u.paletteCommands()
	var shown []paletteCmd

	in := tview.NewInputField().SetLabel("> ").SetFieldBackgroundColor(tcell.ColorDefault).
		SetPlaceholder("type a command...")
	list := tview.NewList().ShowSecondaryText(false).SetHighlightFullLine(true)

	width := 0
	for _, c := range cmds {
		width = max(width, len(c.name))
	}
	render := func(q string) {
		shown = filterCommands(cmds, q)
		list.Clear()
		for _, c := range shown {
			list.AddItem(fmt.Sprintf("%-*s  [gray]%s[-]", width, c.name, tview.Escape(c.key)), "", 0, nil)
		}
		if len(shown) == 0 {
			list.AddItem("[gray]no matching command[-]", "", 0, nil)
		}
	}
	run := func(i int) {
		if i < 0 || i >= len(shown) {
			return
		}
		cmd := shown[i]
		u.closePalette()
		cmd.run()
	}
	render("")

	in.SetChangedFunc(render)
	list.SetSelectedFunc(func(i int, _, _ string, _ rune) { run(i) })
	in.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		n := list.GetItemCount()
		cur := list.GetCurrentItem()
		switch ev.Key() {
		case tcell.KeyUp:
			list.SetCurrentItem((cur - 1 + n) % n)
		case tcell.KeyDown, tcell.KeyTab:
			list.SetCurrentItem((cur + 1) % n)
		case tcell.KeyBacktab:
			list.SetCurrentItem((cur - 1 + n) % n)
		case tcell.KeyPgUp:
			list.SetCurrentItem(max(0, cur-10))
		case tcell.KeyPgDn:
			list.SetCurrentItem(min(n-1, cur+10))
		case tcell.KeyEnter:
			run(cur)
		default:
			return ev
		}
		return nil
	})

	box := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(in, 1, 0, true).
		AddItem(list, 0, 1, false)
	box.SetBorder(true).SetTitle(" Commands (Enter=run, Esc=close) ").SetTitleAlign(tview.AlignLeft)
	u.root.AddPage("palette", center(box, 0.6, 0.6), true, true)
	u.app.SetFocus(in)
}

// closePalette removes the palette and gives focus back to where it was.
func (u *ui) closePalette() {
	u.root.RemovePage("palette")
	prev := u.palPrev
	u.palPrev = nil
	if prev == nil {
		prev = u.editor
	}
	u.app.SetFocus(prev)
}
