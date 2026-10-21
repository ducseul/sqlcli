package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"

	"sqlcli/internal/conn"
)

// TestTUISmoke drives the TUI through a simulated terminal. It needs a live
// database; set SQLCLI_TEST_DSN (e.g. postgres://u:p@localhost:5432/db) to run.
func TestTUISmoke(t *testing.T) {
	dsn := os.Getenv("SQLCLI_TEST_DSN")
	if dsn == "" {
		t.Skip("SQLCLI_TEST_DSN not set")
	}
	cfg, err := conn.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	db, err := cfg.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	screen := tcell.NewSimulationScreen("UTF-8")
	if err := screen.Init(); err != nil {
		t.Fatal(err)
	}
	screen.SetSize(140, 40)

	u := newUI(db, cfg, 50, filepath.Join(t.TempDir(), "hist"))
	u.app.SetScreen(screen)
	done := make(chan error, 1)
	go func() { done <- u.app.Run() }()
	defer func() {
		u.app.Stop()
		<-done
	}()

	waitFor := func(what string, subs ...string) {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			s := dump(screen)
			ok := true
			for _, sub := range subs {
				if !strings.Contains(s, sub) {
					ok = false
				}
			}
			if ok {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("%s: screen never showed %q\n%s", what, subs, dump(screen))
	}
	typeText := func(s string) {
		for _, r := range s {
			screen.InjectKey(tcell.KeyRune, r, tcell.ModNone)
		}
	}
	key := func(k tcell.Key) { screen.InjectKey(k, 0, tcell.ModNone) }

	waitFor("startup", "connected", "SQL", "Results")

	// Run a query with F5.
	typeText("select 42 as answer, 'héllo' as txt")
	key(tcell.KeyF5)
	waitFor("query result", "answer", "txt", "42", "héllo", "OK", "1 row(s)")
	t.Logf("screen after query:\n%s", dump(screen))

	// Error shows a modal; Esc closes it.
	key(tcell.KeyCtrlL)
	typeText("select * from table_that_does_not_exist")
	key(tcell.KeyF5)
	waitFor("error modal", "Error", "table_that_does_not_exist")
	key(tcell.KeyEsc)
	time.Sleep(100 * time.Millisecond)
	if s := dump(screen); strings.Contains(s, "(Esc=close)") {
		t.Fatalf("error modal still open:\n%s", s)
	}

	// Help popup opens with F1 and closes with Esc.
	key(tcell.KeyF1)
	waitFor("help", "Help", "TABLES SIDEBAR", "Ctrl+Q")
	key(tcell.KeyEsc)
	time.Sleep(100 * time.Millisecond)
	if s := dump(screen); strings.Contains(s, "TABLES SIDEBAR") {
		t.Fatalf("help still open:\n%s", s)
	}

	// History modal lists the previous statements.
	key(tcell.KeyF3)
	waitFor("history", "History", "select 42 as answer")
	key(tcell.KeyEsc)

	// Sidebar toggles off with F2.
	waitFor("sidebar", "Tables")
	key(tcell.KeyF2)
	time.Sleep(150 * time.Millisecond)
	if s := dump(screen); strings.Contains(s, "Tables") {
		t.Fatalf("sidebar still visible after F2:\n%s", s)
	}
	key(tcell.KeyF2)
	waitFor("sidebar back", "Tables")

	// Quit.
	key(tcell.KeyCtrlQ)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Ctrl+Q did not stop the app")
	}
	done <- nil // keep the deferred receive happy
}

// dump renders the simulation screen as plain text.
func dump(s tcell.SimulationScreen) string {
	cells, w, h := s.GetContents()
	var b strings.Builder
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := cells[y*w+x]
			if len(c.Runes) == 0 {
				b.WriteRune(' ')
				continue
			}
			b.WriteString(string(c.Runes))
		}
		b.WriteRune('\n')
	}
	return b.String()
}
