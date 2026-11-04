package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"sqlcli/internal/conn"
	"sqlcli/internal/schema"
)

// TestAutocompleteDropdown drives the editor through a simulated screen with
// a fixed schema (no database needed).
func TestAutocompleteDropdown(t *testing.T) {
	screen := tcell.NewSimulationScreen("UTF-8")
	if err := screen.Init(); err != nil {
		t.Fatal(err)
	}
	screen.SetSize(120, 30)

	u := &ui{
		app:          tview.NewApplication(),
		cfg:          &conn.Config{Type: conn.Postgres, Host: "h", Port: 5432, User: "u", Database: "d"},
		limit:        10,
		showSide:     true,
		sideWidth:    defaultSideWidth,
		editorWeight: defaultEditorWeight,
	}
	u.build()
	u.applySchema(&schema.Schema{Tables: []schema.Table{
		{Name: "public.users", Columns: []schema.Column{{Name: "id", Type: "int4"}, {Name: "user_name", Type: "text"}}},
		{Name: "public.user_roles", Columns: []schema.Column{{Name: "role", Type: "text"}}},
	}})
	u.app.SetRoot(u.root, true).SetScreen(screen)
	u.app.SetFocus(u.editor)
	done := make(chan error, 1)
	go func() { done <- u.app.Run() }()
	defer func() {
		u.app.Stop()
		<-done
	}()

	sync := func() {
		ch := make(chan struct{}, 1)
		u.app.QueueUpdateDraw(func() {})
		u.app.QueueUpdate(func() { close(ch) })
		<-ch
		time.Sleep(20 * time.Millisecond)
	}
	typeText := func(s string) {
		for _, r := range s {
			screen.InjectKey(tcell.KeyRune, r, tcell.ModNone)
		}
		sync()
	}
	key := func(k tcell.Key) { screen.InjectKey(k, 0, tcell.ModNone); sync() }
	text := func() string {
		ch := make(chan string, 1)
		u.app.QueueUpdate(func() { ch <- u.editor.GetText() })
		return <-ch
	}
	visible := func() bool {
		ch := make(chan bool, 1)
		u.app.QueueUpdate(func() { ch <- u.comp.visible })
		return <-ch
	}

	typeText("select * from us")
	if !visible() {
		t.Fatalf("dropdown not shown\n%s", dump(screen))
	}
	if s := dump(screen); !strings.Contains(s, "public.users") || !strings.Contains(s, "public.user_roles") {
		t.Fatalf("dropdown items not drawn\n%s", s)
	}
	t.Logf("dropdown:\n%s", dump(screen))
	key(tcell.KeyDown)
	key(tcell.KeyUp)
	key(tcell.KeyTab)
	if got := text(); got != "select * from user_roles" && got != "select * from users" {
		t.Fatalf("after Tab: %q", got)
	}
	if visible() {
		t.Fatal("dropdown still visible after accept")
	}

	// Alias columns, accepted with Enter after moving down.
	key(tcell.KeyCtrlL)
	typeText("select x.")
	if visible() {
		t.Fatal("unknown alias should not suggest")
	}
	key(tcell.KeyCtrlL)
	typeText("select * from users x where x.")
	if !visible() {
		t.Fatalf("no column dropdown\n%s", dump(screen))
	}
	key(tcell.KeyDown)
	key(tcell.KeyEnter)
	if got := text(); got != "select * from users x where x.user_name" {
		t.Fatalf("after Enter: %q", got)
	}

	// Esc closes without changing the text; arrows then move the cursor.
	typeText(" an")
	if !visible() {
		t.Fatal("keyword dropdown not shown")
	}
	key(tcell.KeyEsc)
	if visible() || text() != "select * from users x where x.user_name an" {
		t.Fatalf("Esc: visible=%v text=%q", visible(), text())
	}

	// Enter on a word that is already complete inserts a newline.
	key(tcell.KeyCtrlL)
	typeText("select * from users where id")
	if visible() {
		key(tcell.KeyEnter)
		if got := text(); !strings.HasSuffix(got, "id\n") {
			t.Fatalf("Enter on complete word: %q", got)
		}
	}
}
