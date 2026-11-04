package tui

import (
	"fmt"
	"strings"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"sqlcli/internal/schema"
)

const (
	popupMaxRows  = 8
	popupMaxWidth = 60
)

// completion is the state of the autocomplete dropdown.
type completion struct {
	visible    bool
	items      []schema.Item
	sel, top   int
	start, end int    // byte range in the editor text an accepted item replaces
	word       string // text currently in [start, end)
}

// updateCompletion recomputes suggestions for the editor cursor. With force
// (Ctrl+Space) it also suggests when nothing has been typed.
func (u *ui) updateCompletion(force bool) {
	_, start, end := u.editor.GetSelection()
	if start != end {
		u.hideCompletion()
		return
	}
	full := u.editor.GetText()
	wstart, items := schema.Complete(full, start, u.schema, force)
	if len(items) == 0 {
		u.hideCompletion()
		return
	}
	prev := ""
	if u.comp.visible && u.comp.sel < len(u.comp.items) {
		prev = u.comp.items[u.comp.sel].Label
	}
	u.comp = completion{visible: true, items: items, start: wstart, end: start, word: full[wstart:start]}
	// Keep the highlighted entry if it is still offered.
	for i, it := range items {
		if it.Label == prev {
			u.comp.sel = i
			break
		}
	}
	u.scrollCompletion()
}

func (u *ui) hideCompletion() {
	u.comp = completion{}
}

func (u *ui) scrollCompletion() {
	c := &u.comp
	if c.sel < c.top {
		c.top = c.sel
	}
	if c.sel >= c.top+popupMaxRows {
		c.top = c.sel - popupMaxRows + 1
	}
}

func (u *ui) moveCompletion(delta int) {
	n := len(u.comp.items)
	if n == 0 {
		return
	}
	u.comp.sel = ((u.comp.sel+delta)%n + n) % n
	u.scrollCompletion()
}

// acceptCompletion replaces the word being typed with the selected item.
func (u *ui) acceptCompletion() {
	c := u.comp
	u.hideCompletion()
	if c.sel >= len(c.items) {
		return
	}
	u.editor.Replace(c.start, c.end, c.items[c.sel].Text)
}

// completionKeys handles keys while the dropdown is open. It returns true when
// the key was consumed.
func (u *ui) completionKeys(ev *tcell.EventKey) bool {
	if !u.comp.visible {
		return false
	}
	if u.app.GetFocus() != u.editor {
		u.hideCompletion()
		return false
	}
	if ev.Modifiers()&(tcell.ModCtrl|tcell.ModAlt) != 0 {
		if ev.Key() != tcell.KeyRune {
			u.hideCompletion()
		}
		return false
	}
	switch ev.Key() {
	case tcell.KeyUp:
		u.moveCompletion(-1)
	case tcell.KeyDown:
		u.moveCompletion(1)
	case tcell.KeyPgUp:
		u.moveCompletion(-popupMaxRows)
	case tcell.KeyPgDn:
		u.moveCompletion(popupMaxRows)
	case tcell.KeyTab:
		u.acceptCompletion()
	case tcell.KeyEnter:
		// Enter on a word that is already complete is a plain newline.
		if it := u.comp.items[u.comp.sel]; it.Text == u.comp.word {
			u.hideCompletion()
			return false
		}
		u.acceptCompletion()
	case tcell.KeyEsc:
		u.hideCompletion()
	case tcell.KeyLeft, tcell.KeyRight, tcell.KeyHome, tcell.KeyEnd:
		u.hideCompletion()
		return false
	default:
		return false
	}
	return true
}

// drawCompletion paints the dropdown under (or above) the editor cursor. It
// runs after the normal draw pass.
func (u *ui) drawCompletion(screen tcell.Screen) {
	c := &u.comp
	if !c.visible || len(c.items) == 0 {
		return
	}
	if name, _ := u.root.GetFrontPage(); name != "main" || !u.editor.HasFocus() {
		return
	}
	ex, ey, ew, eh := u.editor.GetInnerRect()
	_, _, row, col := u.editor.GetCursor()
	offRow, _ := u.editor.GetOffset() // the editor wraps, so there is no column offset
	cx, cy := ex+col, ey+row-offRow
	if cy < ey || cy >= ey+eh || cx > ex+ew {
		return
	}

	// Size: label + two spaces + detail.
	labelW, detailW := 0, 0
	for _, it := range c.items {
		labelW = max(labelW, tview.TaggedStringWidth(tview.Escape(it.Label)))
		detailW = max(detailW, len(detailText(it)))
	}
	width := min(labelW+detailW+4, popupMaxWidth)
	rows := min(len(c.items), popupMaxRows)
	sw, sh := screen.Size()
	width = min(width, sw)

	x := cx - tview.TaggedStringWidth(tview.Escape(c.word))
	x = max(0, min(x, sw-width))
	y := cy + 1
	if y+rows > sh {
		y = cy - rows
	}
	if y < 0 {
		y = 0
	}

	base := tcell.StyleDefault.Background(tcell.ColorDarkSlateGray).Foreground(tcell.ColorWhite)
	selected := tcell.StyleDefault.Background(tcell.ColorSteelBlue).Foreground(tcell.ColorWhite)
	for i := 0; i < rows; i++ {
		idx := c.top + i
		if idx >= len(c.items) {
			break
		}
		it := c.items[idx]
		st := base
		if idx == c.sel {
			st = selected
		}
		for j := 0; j < width; j++ {
			screen.SetContent(x+j, y+i, ' ', nil, st)
		}
		labelColor := tcell.ColorWhite
		switch it.Kind {
		case schema.KindTable:
			labelColor = tcell.ColorAqua
		case schema.KindColumn:
			labelColor = tcell.ColorLightGreen
		case schema.KindKeyword:
			labelColor = tcell.ColorYellow
		}
		tview.Print(screen, tview.Escape(it.Label), x+1, y+i, width-2, tview.AlignLeft, labelColor)
		if d := detailText(it); d != "" && labelW+3 < width {
			tview.Print(screen, tview.Escape(d), x+labelW+3, y+i, width-labelW-4, tview.AlignLeft, tcell.ColorSilver)
		}
	}
	// Scroll hint when not everything fits.
	if len(c.items) > rows {
		hint := fmt.Sprintf(" %d/%d ", c.sel+1, len(c.items))
		tview.Print(screen, hint, x+width-len(hint), y+rows-1, len(hint), tview.AlignLeft, tcell.ColorSilver)
	}
}

func detailText(it schema.Item) string {
	if it.Detail != "" {
		return strings.TrimPrefix(it.Detail, " · ")
	}
	return it.Kind.String()
}
