package tui

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"sqlcli/internal/clipboard"
	"sqlcli/internal/conn"
	"sqlcli/internal/query"
)

func newTestUI() *ui {
	u := &ui{
		app:          tview.NewApplication(),
		cfg:          &conn.Config{Type: conn.Postgres},
		showSide:     true,
		sideWidth:    defaultSideWidth,
		editorWeight: defaultEditorWeight,
	}
	u.build()
	u.app.SetRoot(u.root, true)
	return u
}

// TestCtrlCDoesNotQuit runs the app and checks it survives Ctrl+C.
func TestCtrlCDoesNotQuit(t *testing.T) {
	screen := tcell.NewSimulationScreen("UTF-8")
	if err := screen.Init(); err != nil {
		t.Fatal(err)
	}
	screen.SetSize(120, 30)
	u := newTestUI()
	u.app.SetScreen(screen)
	done := make(chan error, 1)
	go func() { done <- u.app.Run() }()
	defer func() {
		u.app.Stop()
		<-done
	}()

	time.Sleep(100 * time.Millisecond)
	screen.InjectKey(tcell.KeyCtrlC, 0, tcell.ModNone) // nothing selected: no clipboard write
	select {
	case <-done:
		t.Fatal("Ctrl+C stopped the application")
	case <-time.After(300 * time.Millisecond):
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(dump(screen), "nothing selected") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no 'nothing selected' status:\n%s", dump(screen))
}

// TestCopyPaste uses the real clipboard, so it only runs on request.
func TestCopyPaste(t *testing.T) {
	if os.Getenv("SQLCLI_TEST_CLIPBOARD") == "" {
		t.Skip("SQLCLI_TEST_CLIPBOARD not set")
	}
	u := newTestUI()
	press := func(k tcell.Key) { u.globalKeys(tcell.NewEventKey(k, 0, tcell.ModNone)) }
	clip := func() string {
		s, err := clipboard.Read()
		if err != nil {
			t.Fatal(err)
		}
		return s
	}

	// Editor selection.
	u.app.SetFocus(u.editor)
	u.editor.SetText("select 1;\nselect 'héllo' from t;", false)
	u.editor.Select(10, 10+len("select 'héllo' from t"))
	press(tcell.KeyCtrlC)
	if got := clip(); got != "select 'héllo' from t" {
		t.Fatalf("editor copy: %q", got)
	}

	// Results cell: the full value, not the clipped display text.
	long := strings.Repeat("x", 100) + "\nline2"
	u.renderResult(&query.Result{Columns: []string{"a", "b"}, Rows: [][]string{{"1", long}}, IsQuery: true})
	u.app.SetFocus(u.results)
	u.results.Select(1, 1)
	press(tcell.KeyCtrlC)
	if got := clip(); got != long {
		t.Fatalf("cell copy: %q", got)
	}

	// Paste from the results pane goes into the editor at the cursor.
	if err := clipboard.Write("NOW()\r\n"); err != nil {
		t.Fatal(err)
	}
	u.editor.SetText("select ", true)
	press(tcell.KeyCtrlV)
	if got := u.editor.GetText(); got != "select NOW()\n" {
		t.Fatalf("paste: %q", got)
	}
	if u.app.GetFocus() != u.editor {
		t.Fatal("paste did not focus the editor")
	}
}
