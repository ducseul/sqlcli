package tui

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"

	"sqlcli/internal/conn"
)

func TestFmtBytes(t *testing.T) {
	cases := map[float64]string{0: "0 B", 1023: "1023 B", 1536: "1.5 KiB", 5 << 20: "5.0 MiB"}
	for n, want := range cases {
		if got := fmtBytes(n); got != want {
			t.Errorf("fmtBytes(%v) = %q, want %q", n, got, want)
		}
	}
}

// TestConnStatus measures a live database; set SQLCLI_TEST_DSN to run it.
func TestConnStatus(t *testing.T) {
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
	screen.SetSize(140, 45)
	u := newTestUI()
	u.db, u.cfg = db, cfg
	u.app.SetScreen(screen)
	done := make(chan error, 1)
	go func() { done <- u.app.Run() }()
	defer func() {
		u.app.Stop()
		<-done
	}()

	u.app.QueueUpdateDraw(u.showConnStatus)
	deadline := time.Now().Add(20 * time.Second)
	// The popup scrolls, so read its text rather than the screen.
	text := func() string {
		ch := make(chan string, 1)
		u.app.QueueUpdate(func() {
			if u.cs == nil {
				ch <- ""
				return
			}
			ch <- u.cs.tv.GetText(true)
		})
		return <-ch
	}
	for time.Now().Before(deadline) {
		s := text()
		if strings.Contains(s, "done; r measures again") {
			for _, want := range []string{"10 sent, 10 received", "0% loss", "Round trip", "Jitter", "TCP connect", "Clock skew", "Upload", "Download", "SERVER", "User / database"} {
				if !strings.Contains(s, want) {
					t.Errorf("missing %q", want)
				}
			}
			t.Log("\n" + s)
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("measurement did not finish:\n%s", text())
}
