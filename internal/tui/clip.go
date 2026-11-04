package tui

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"sqlcli/internal/clipboard"
)

// copyFocused copies what the focused widget points at:
//   - SQL editor: the selected text
//   - Results: the full value of the selected cell
//   - Tables: the table name under the cursor
//   - a text popup / input field / list: its text or the current item
func (u *ui) copyFocused() {
	var text, what string
	switch f := u.app.GetFocus().(type) {
	case *tview.TextArea:
		sel, start, end := f.GetSelection()
		if start == end {
			u.setStatus("[yellow]nothing selected[-] (Shift+Arrows to select)")
			return
		}
		text, what = sel, "selection"
	case *tview.Table:
		row, col := f.GetSelection()
		if f != u.results || u.last == nil || row < 1 || row-1 >= len(u.last.Rows) || col >= len(u.last.Rows[row-1]) {
			u.setStatus("[yellow]no cell selected[-]")
			return
		}
		text, what = u.last.Rows[row-1][col], "cell "+u.last.Columns[col]
	case *tview.List:
		i := f.GetCurrentItem()
		if f.GetItemCount() == 0 || i < 0 {
			return
		}
		text, _ = f.GetItemText(i)
		what = "item"
	case *tview.TextView:
		text, what = f.GetText(true), "text"
	case *tview.InputField:
		text, what = f.GetText(), "field"
	default:
		return
	}
	if err := u.writeClipboard(text); err != nil {
		u.setStatus(fmt.Sprintf("[red]copy failed:[-] %s", tview.Escape(err.Error())))
		return
	}
	u.setStatus(fmt.Sprintf("[green]copied[-] %s (%d chars)", tview.Escape(what), utf8.RuneCountInString(text)))
}

// pasteClipboard inserts the clipboard text into the focused input field, or
// otherwise into the SQL editor (replacing its selection) and focuses it.
func (u *ui) pasteClipboard() {
	text, err := clipboard.Read()
	if err != nil {
		u.setStatus(fmt.Sprintf("[red]paste failed:[-] %s", tview.Escape(err.Error())))
		return
	}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	if text == "" {
		return
	}
	if f, ok := u.app.GetFocus().(*tview.InputField); ok {
		f.SetText(f.GetText() + strings.ReplaceAll(text, "\n", " "))
		return
	}
	if name, _ := u.root.GetFrontPage(); name != "main" {
		return // read-only popup
	}
	u.hideCompletion()
	_, start, end := u.editor.GetSelection()
	u.editor.Replace(start, end, text)
	u.app.SetFocus(u.editor)
}

// writeClipboard uses the system clipboard and falls back to the terminal's
// OSC 52 clipboard (works over SSH in most modern terminals).
func (u *ui) writeClipboard(text string) error {
	err := clipboard.Write(text)
	if err == nil {
		return nil
	}
	if errors.Is(err, clipboard.ErrUnavailable) && u.screen != nil {
		u.screen.SetClipboard([]byte(text))
		return nil
	}
	return err
}

// rememberScreen keeps the screen for the OSC 52 fallback.
func (u *ui) rememberScreen(s tcell.Screen) bool {
	u.screen = s
	return false
}
