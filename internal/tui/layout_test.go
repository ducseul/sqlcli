package tui

import (
	"testing"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"sqlcli/internal/conn"
)

// TestResizeKeys checks that Ctrl+Shift+arrows resize and plain Ctrl+arrows don't.
func TestResizeKeys(t *testing.T) {
	u := &ui{
		app:          tview.NewApplication(),
		cfg:          &conn.Config{Type: conn.Postgres},
		showSide:     true,
		sideWidth:    defaultSideWidth,
		editorWeight: defaultEditorWeight,
	}
	u.build()
	u.layout.SetRect(0, 0, 120, 30)
	press := func(k tcell.Key, mod tcell.ModMask) { u.globalKeys(tcell.NewEventKey(k, 0, mod)) }
	cs := tcell.ModCtrl | tcell.ModShift

	press(tcell.KeyRight, tcell.ModCtrl)
	press(tcell.KeyDown, tcell.ModCtrl)
	if u.sideWidth != defaultSideWidth || u.editorWeight != defaultEditorWeight {
		t.Fatalf("plain Ctrl+arrows resized: width %d weight %d", u.sideWidth, u.editorWeight)
	}
	press(tcell.KeyRight, cs)
	press(tcell.KeyDown, cs)
	if u.sideWidth != defaultSideWidth+sideWidthStep || u.editorWeight != defaultEditorWeight+1 {
		t.Fatalf("Ctrl+Shift+Right/Down: width %d weight %d", u.sideWidth, u.editorWeight)
	}
	for i := 0; i < 20; i++ {
		press(tcell.KeyLeft, cs)
		press(tcell.KeyUp, cs)
	}
	if u.sideWidth != minSideWidth || u.editorWeight != 1 {
		t.Fatalf("lower bounds: width %d weight %d", u.sideWidth, u.editorWeight)
	}
}
