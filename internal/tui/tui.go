// Package tui implements the interactive terminal UI.
package tui

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"sqlcli/internal/conn"
	"sqlcli/internal/format"
	"sqlcli/internal/query"
	"sqlcli/internal/schema"
)

// cacheDir is where introspected schemas are stored, relative to the working
// directory.
const cacheDir = "cache"

const (
	defaultLimit = 1000
	cellMaxWidth = 40

	// Sidebar width (columns) and its Ctrl+Shift+Left/Right step and bounds.
	defaultSideWidth = 32
	sideWidthStep    = 4
	minSideWidth     = 12
	minMainWidth     = 20

	// SQL editor vs results height, as editorWeight : (heightUnits - editorWeight).
	heightUnits         = 12
	defaultEditorWeight = 4
)

type ui struct {
	app    *tview.Application
	db     *sql.DB
	cfg    *conn.Config
	limit  int
	ctx    context.Context
	cancel context.CancelFunc

	root     *tview.Pages
	layout   *tview.Flex
	sidebar  *tview.Flex
	filter   *tview.InputField
	tables   *tview.List
	editor   *tview.TextArea
	results  *tview.Table
	status   *tview.TextView
	hints    *tview.TextView
	screen   tcell.Screen // last drawn screen (for the OSC 52 clipboard fallback)
	right    *tview.Flex
	showSide bool

	sideWidth    int // sidebar width in columns
	editorWeight int // editor share of heightUnits

	mu        sync.Mutex
	running   bool
	last      *query.Result
	history   []string
	histFile  string
	allTables []string

	schema    *schema.Schema // tables + columns for autocomplete (nil until loaded)
	cacheFile string         // where schema is cached ("" = no cache)
	comp      completion     // autocomplete dropdown state
	typing    bool           // the current key edits the editor text
}

// Run starts the TUI and blocks until the user quits.
func Run(db *sql.DB, cfg *conn.Config, limit int) error {
	u := newUI(db, cfg, limit, historyPath(), schema.CachePath(cacheDir, cfg))
	// Ctrl+C is "copy" in the TUI. The terminal is in raw mode so it arrives
	// as a key, but ignore SIGINT too in case the shell or console still
	// delivers one (e.g. Ctrl+C before raw mode is set, or a parent's signal).
	signal.Ignore(os.Interrupt)
	defer signal.Reset(os.Interrupt)
	return u.app.EnableMouse(true).EnablePaste(true).Run()
}

func newUI(db *sql.DB, cfg *conn.Config, limit int, histFile, cacheFile string) *ui {
	if limit <= 0 {
		limit = defaultLimit
	}
	u := &ui{
		app:       tview.NewApplication(),
		db:        db,
		cfg:       cfg,
		limit:     limit,
		showSide:  true,
		histFile:  histFile,
		cacheFile: cacheFile,

		sideWidth:    defaultSideWidth,
		editorWeight: defaultEditorWeight,
	}
	u.loadHistory()
	u.build()
	u.loadCachedSchema()
	u.loadTables()
	u.app.SetRoot(u.root, true)
	return u
}

func (u *ui) build() {
	// Editor
	u.editor = tview.NewTextArea().SetPlaceholder("Type SQL here. F5 = run all, Ctrl+Enter = run statement at cursor, F1 = help, Ctrl+Q = quit")
	u.editor.SetBorder(true).SetTitle(" SQL ").SetTitleAlign(tview.AlignLeft)
	u.editor.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		switch ev.Key() {
		case tcell.KeyRune, tcell.KeyBackspace, tcell.KeyBackspace2, tcell.KeyDelete:
			u.typing = ev.Modifiers()&(tcell.ModCtrl|tcell.ModAlt) == 0
		}
		return ev
	})
	u.editor.SetChangedFunc(func() {
		if u.typing {
			u.updateCompletion(false)
		} else {
			u.hideCompletion()
		}
	})
	u.editor.SetMovedFunc(func() {
		if !u.typing {
			u.hideCompletion()
		}
	})

	// Results
	u.results = tview.NewTable().SetBorders(false).SetSeparator(tview.Borders.Vertical).
		SetFixed(1, 0).SetSelectable(true, true)
	u.results.SetBorder(true).SetTitle(" Results ").SetTitleAlign(tview.AlignLeft)
	u.results.SetSelectedFunc(func(row, col int) {
		if row == 0 || u.last == nil || row-1 >= len(u.last.Rows) {
			return
		}
		u.showText(fmt.Sprintf(" %s (row %d) ", u.last.Columns[col], row), u.last.Rows[row-1][col])
	})

	// Status + hints
	u.status = tview.NewTextView().SetDynamicColors(true).SetWrap(false)
	u.hints = tview.NewTextView().SetDynamicColors(true).SetWrap(false).SetTextAlign(tview.AlignRight)
	u.hints.SetText("[gray]F1[-] help  [gray]F5[-] run  [gray]Ctrl+Q[-] quit")
	bar := tview.NewFlex().AddItem(u.status, 0, 2, false).AddItem(u.hints, 0, 3, false)

	// Sidebar: filter + table list
	u.filter = tview.NewInputField().SetLabel("/ ").SetFieldBackgroundColor(tcell.ColorDefault)
	u.filter.SetChangedFunc(func(string) { u.renderTables() })
	u.filter.SetDoneFunc(func(key tcell.Key) {
		if key == tcell.KeyEnter || key == tcell.KeyTab {
			u.app.SetFocus(u.tables)
		}
	})
	u.tables = tview.NewList().ShowSecondaryText(false).SetHighlightFullLine(true)
	u.tables.SetSelectedFunc(func(i int, name, _ string, _ rune) {
		u.editor.SetText(u.selectStmt(name), true)
		u.runSQL(u.editor.GetText())
	})
	u.tables.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		switch ev.Rune() {
		case 'd', 'D':
			if i := u.tables.GetCurrentItem(); i >= 0 && u.tables.GetItemCount() > 0 {
				name, _ := u.tables.GetItemText(i)
				u.describe(name)
			}
			return nil
		case 'i', 'I':
			if i := u.tables.GetCurrentItem(); i >= 0 && u.tables.GetItemCount() > 0 {
				name, _ := u.tables.GetItemText(i)
				u.editor.SetText(u.editor.GetText()+name, true)
				u.app.SetFocus(u.editor)
			}
			return nil
		case '/':
			u.app.SetFocus(u.filter)
			return nil
		case 'r', 'R':
			u.loadTables()
			return nil
		}
		return ev
	})
	u.sidebar = tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(u.filter, 1, 0, false).
		AddItem(u.tables, 0, 1, false)
	u.sidebar.SetBorder(true).SetTitle(" Tables (Enter=select, d=describe, i=insert) ").SetTitleAlign(tview.AlignLeft)

	u.right = tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(u.editor, 0, u.editorWeight, true).
		AddItem(u.results, 0, heightUnits-u.editorWeight, false)

	u.layout = tview.NewFlex().AddItem(u.sidebar, u.sideWidth, 0, false).AddItem(u.right, 0, 1, true)

	main := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(u.layout, 0, 1, true).
		AddItem(bar, 1, 0, false)

	u.root = tview.NewPages().AddPage("main", main, true, true)

	u.app.SetInputCapture(u.globalKeys)
	u.app.SetAfterDrawFunc(u.drawCompletion)
	u.app.SetBeforeDrawFunc(u.rememberScreen)
	// Ctrl+X (cut) inside the editor uses the system clipboard too.
	u.editor.SetClipboard(func(s string) { _ = u.writeClipboard(s) }, func() string { return "" })
	u.setStatus(fmt.Sprintf("[green]connected[-] %s  (limit %d rows)", u.cfg.Redacted(), u.limit))
}

func (u *ui) globalKeys(ev *tcell.EventKey) *tcell.EventKey {
	u.typing = false
	// Ctrl+C / Ctrl+V are always ours: copy / paste, never "quit" (tview would
	// otherwise stop the app on Ctrl+C).
	switch ev.Key() {
	case tcell.KeyCtrlC:
		u.copyFocused()
		return nil
	case tcell.KeyCtrlV:
		u.pasteClipboard()
		return nil
	}
	// Don't steal keys from modals.
	if name, _ := u.root.GetFrontPage(); name != "main" {
		u.hideCompletion()
		if ev.Key() == tcell.KeyEsc {
			u.root.RemovePage(name)
			u.app.SetFocus(u.editor)
			return nil
		}
		return ev
	}
	// Ctrl+Space: open the autocomplete dropdown.
	if ev.Key() == tcell.KeyCtrlSpace || (ev.Key() == tcell.KeyRune && ev.Rune() == ' ' && ev.Modifiers()&tcell.ModCtrl != 0) {
		if u.app.GetFocus() == u.editor {
			u.updateCompletion(true)
		}
		return nil
	}
	if u.completionKeys(ev) {
		return nil
	}
	// Ctrl+Enter: most terminals send it as LF (Ctrl+J); the Windows console
	// and terminals with extended key reporting send Enter + Ctrl.
	if (ev.Key() == tcell.KeyEnter && ev.Modifiers()&tcell.ModCtrl != 0) || ev.Key() == tcell.KeyCtrlJ {
		u.runStatementAtCursor()
		return nil
	}
	// Ctrl+Shift+arrows resize the panes (plain Ctrl+arrows stay word jumps).
	if ev.Modifiers()&(tcell.ModCtrl|tcell.ModShift|tcell.ModAlt) == tcell.ModCtrl|tcell.ModShift {
		switch ev.Key() {
		case tcell.KeyLeft:
			u.resizeSidebar(-sideWidthStep)
			return nil
		case tcell.KeyRight:
			u.resizeSidebar(sideWidthStep)
			return nil
		case tcell.KeyUp:
			u.resizeEditor(-1)
			return nil
		case tcell.KeyDown:
			u.resizeEditor(1)
			return nil
		}
	}
	switch ev.Key() {
	case tcell.KeyCtrlQ:
		u.app.Stop()
		return nil
	case tcell.KeyF1:
		u.showHelp()
		return nil
	case tcell.KeyF5, tcell.KeyCtrlE:
		text, start, end := u.editor.GetSelection()
		if start == end {
			text = u.editor.GetText()
		}
		u.runSQL(text)
		return nil
	case tcell.KeyF9, tcell.KeyF21:
		// Shift+F9 arrives as F9+Shift, or as F21 on terminals that encode it.
		u.explainAtCursor(ev.Key() == tcell.KeyF21 || ev.Modifiers()&tcell.ModShift != 0)
		return nil
	case tcell.KeyF2:
		u.toggleSidebar()
		return nil
	case tcell.KeyF3:
		u.showHistory()
		return nil
	case tcell.KeyF8:
		u.exportCSV()
		return nil
	case tcell.KeyCtrlL:
		u.editor.SetText("", false)
		return nil
	case tcell.KeyEsc:
		u.mu.Lock()
		if u.running && u.cancel != nil {
			u.cancel()
		}
		u.mu.Unlock()
		return nil
	case tcell.KeyTab, tcell.KeyBacktab:
		u.cycleFocus(ev.Key() == tcell.KeyBacktab)
		return nil
	}
	return ev
}

func (u *ui) cycleFocus(back bool) {
	order := []tview.Primitive{u.editor, u.results}
	if u.showSide {
		order = append([]tview.Primitive{u.tables}, order...)
	}
	cur := u.app.GetFocus()
	idx := -1
	for i, p := range order {
		if p == cur {
			idx = i
		}
	}
	if back {
		idx = (idx - 1 + len(order)) % len(order)
	} else {
		idx = (idx + 1) % len(order)
	}
	u.app.SetFocus(order[idx])
}

// runStatementAtCursor runs the selection if there is one, otherwise only the
// statement the editor cursor is in.
func (u *ui) runStatementAtCursor() {
	text, start, end := u.editor.GetSelection()
	if start == end {
		text = query.StatementAt(u.editor.GetText(), start)
	}
	if strings.TrimSpace(text) == "" {
		u.setStatus("[yellow]no statement at cursor[-]")
		return
	}
	u.runSQL(text)
}

// resizeSidebar changes the Tables panel width by delta columns.
func (u *ui) resizeSidebar(delta int) {
	if !u.showSide {
		u.toggleSidebar()
		return
	}
	maxW := 200
	if _, _, w, _ := u.layout.GetRect(); w > 0 {
		maxW = max(minSideWidth, w-minMainWidth)
	}
	u.sideWidth = max(minSideWidth, min(u.sideWidth+delta, maxW))
	u.layout.ResizeItem(u.sidebar, u.sideWidth, 0)
	u.setStatus(fmt.Sprintf("tables width %d", u.sideWidth))
}

// resizeEditor moves the SQL/results divider by delta units (negative = up).
func (u *ui) resizeEditor(delta int) {
	u.editorWeight = max(1, min(u.editorWeight+delta, heightUnits-1))
	u.right.ResizeItem(u.editor, 0, u.editorWeight)
	u.right.ResizeItem(u.results, 0, heightUnits-u.editorWeight)
	u.setStatus(fmt.Sprintf("SQL / results height %d:%d", u.editorWeight, heightUnits-u.editorWeight))
}

func (u *ui) toggleSidebar() {
	u.showSide = !u.showSide
	if u.showSide {
		u.layout.ResizeItem(u.sidebar, u.sideWidth, 0)
	} else {
		u.layout.ResizeItem(u.sidebar, 0, 0)
		if u.app.GetFocus() == u.tables || u.app.GetFocus() == u.filter {
			u.app.SetFocus(u.editor)
		}
	}
}

func (u *ui) setStatus(msg string) {
	u.status.SetText(" " + msg)
}

// ---- query execution -------------------------------------------------------

func (u *ui) runSQL(text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	if u.runAsync("running...", func(ctx context.Context) func() {
		results := query.ExecScript(ctx, u.db, text, u.limit, false)
		return func() { u.showResults(results) }
	}) {
		u.addHistory(text)
	}
}

// runAsync runs work in the background as the single cancellable job and
// applies the UI update it returns on the draw thread. It reports false when
// another job is already running.
func (u *ui) runAsync(label string, work func(ctx context.Context) func()) bool {
	u.mu.Lock()
	if u.running {
		u.mu.Unlock()
		u.setStatus("[yellow]a query is already running (Esc to cancel)[-]")
		return false
	}
	u.running = true
	u.ctx, u.cancel = context.WithCancel(context.Background())
	ctx := u.ctx
	u.mu.Unlock()

	u.setStatus("[yellow]" + label + "[-] (Esc to cancel)")
	go func() {
		apply := work(ctx)
		u.mu.Lock()
		u.running = false
		u.cancel = nil
		u.mu.Unlock()
		u.app.QueueUpdateDraw(apply)
	}()
	return true
}

// explainAtCursor shows the plan of the selection, or of the statement under
// the cursor. raw selects the engine's full (JSON / PLAN_TABLE) output.
func (u *ui) explainAtCursor(raw bool) {
	text, start, end := u.editor.GetSelection()
	if start == end {
		text = query.StatementAt(u.editor.GetText(), start)
	}
	if strings.TrimSpace(text) == "" {
		u.setStatus("[yellow]no statement at cursor[-]")
		return
	}
	kind := "explain"
	if raw {
		kind = "raw explain"
	}
	u.runAsync(kind+"...", func(ctx context.Context) func() {
		p, err := query.Explain(ctx, u.db, u.cfg.Type, text, raw)
		return func() {
			if err != nil {
				u.setStatus(fmt.Sprintf("[red]ERROR:[-] %s", tview.Escape(err.Error())))
				u.showText(" Error ", fmt.Sprintf("%s\n\n--- statement ---\n%s", err.Error(), text))
				return
			}
			u.setStatus(fmt.Sprintf("[green]OK[-] %s in %s", kind, p.Duration.Round(time.Millisecond)))
			if p.Result != nil {
				u.renderResult(p.Result)
				u.results.SetTitle(fmt.Sprintf(" %s (%d rows) ", strings.ToUpper(kind[:1])+kind[1:], len(p.Result.Rows)))
				return
			}
			// No wrapping: plan trees rely on indentation.
			tv := tview.NewTextView().SetText(tview.Escape(p.Text)).SetScrollable(true).SetWrap(false)
			tv.SetBorder(true).SetTitle(" " + strings.ToUpper(kind[:1]) + kind[1:] + " (Esc=close) ").SetTitleAlign(tview.AlignLeft)
			u.root.AddPage("text", center(tv, 0.9, 0.8), true, true)
			u.app.SetFocus(tv)
		}
	})
}

func (u *ui) showResults(results []*query.Result) {
	if len(results) == 0 {
		return
	}
	var total time.Duration
	var lastQuery *query.Result
	affected := int64(0)
	nonQuery := 0
	for _, r := range results {
		total += r.Duration
		if r.Err != nil {
			u.setStatus(fmt.Sprintf("[red]ERROR:[-] %s", tview.Escape(r.Err.Error())))
			u.showText(" Error ", fmt.Sprintf("%s\n\n--- statement ---\n%s", r.Err.Error(), r.Statement))
			return
		}
		if r.IsQuery {
			lastQuery = r
		} else {
			nonQuery++
			affected += r.RowsAffected
		}
	}
	if lastQuery != nil {
		u.renderResult(lastQuery)
		extra := ""
		if nonQuery > 0 {
			extra = fmt.Sprintf(", %d other stmt(s), %d row(s) affected", nonQuery, affected)
		}
		trunc := ""
		if len(lastQuery.Rows) >= u.limit {
			trunc = fmt.Sprintf(" [yellow](truncated at %d)[-]", u.limit)
		}
		u.setStatus(fmt.Sprintf("[green]OK[-] %d row(s)%s in %s%s", len(lastQuery.Rows), trunc, total.Round(time.Millisecond), extra))
	} else {
		u.results.Clear()
		u.last = nil
		u.setStatus(fmt.Sprintf("[green]OK[-] %d statement(s), %d row(s) affected in %s", nonQuery, affected, total.Round(time.Millisecond)))
	}
}

func (u *ui) renderResult(r *query.Result) {
	u.last = r
	u.results.Clear()
	for c, name := range r.Columns {
		u.results.SetCell(0, c, tview.NewTableCell(tview.Escape(name)).
			SetTextColor(tcell.ColorYellow).SetAttributes(tcell.AttrBold).
			SetSelectable(false).SetMaxWidth(cellMaxWidth))
	}
	for ri, row := range r.Rows {
		for c, v := range row {
			cell := tview.NewTableCell(tview.Escape(oneLine(v))).SetMaxWidth(cellMaxWidth)
			if v == "NULL" {
				cell.SetTextColor(tcell.ColorGray)
			}
			u.results.SetCell(ri+1, c, cell)
		}
	}
	u.results.ScrollToBeginning()
	u.results.Select(1, 0)
	u.results.SetTitle(fmt.Sprintf(" Results (%d rows x %d cols) ", len(r.Rows), len(r.Columns)))
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}

// ---- tables sidebar --------------------------------------------------------

// loadCachedSchema fills the sidebar and autocomplete from the on-disk cache
// so they work right away, before introspection finishes.
func (u *ui) loadCachedSchema() {
	if u.cacheFile == "" {
		return
	}
	s, err := schema.Load(u.cacheFile)
	if err != nil {
		return
	}
	u.applySchema(s)
}

func (u *ui) applySchema(s *schema.Schema) {
	u.schema = s
	u.allTables = s.TableNames()
	u.renderTables()
}

// loadTables introspects tables and columns in the background and rewrites
// the cache. The sidebar is filled as soon as the table list arrives;
// autocomplete gets the columns whenever they are done.
func (u *ui) loadTables() {
	msg := "[yellow]introspecting schema/tables...[-]"
	if u.schema != nil {
		msg += fmt.Sprintf(" (using cache: %d tables)", len(u.schema.Tables))
	}
	u.setStatus(msg)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		started := time.Now()
		s, err := schema.Introspect(ctx, u.db, u.cfg.Type, func(names []string) {
			u.app.QueueUpdateDraw(func() {
				u.allTables = names
				u.renderTables()
				// Without a cache, table names alone already help autocomplete.
				if u.schema == nil {
					u.schema = schema.FromTables(u.cfg.Type, names)
				}
				if !u.isRunning() {
					u.setStatus(fmt.Sprintf("[green]connected[-] %s  %d tables  [yellow]introspecting columns...[-]",
						u.cfg.Redacted(), len(names)))
				}
			})
		})
		var saveErr error
		// A failed column query returns tables only; don't let that replace a
		// complete cache.
		if s != nil && err == nil && u.cacheFile != "" {
			saveErr = s.Save(u.cacheFile)
		}
		u.app.QueueUpdateDraw(func() {
			if s == nil {
				u.setStatus(fmt.Sprintf("[red]introspect:[-] %s", tview.Escape(err.Error())))
				return
			}
			if err != nil && u.schema != nil && u.schema.ColumnCount() > 0 {
				u.allTables = s.TableNames() // keep the cached columns for autocomplete
				u.renderTables()
			} else {
				u.applySchema(s)
			}
			if u.isRunning() {
				return // don't hide the running query's status
			}
			msg := fmt.Sprintf("[green]connected[-] %s  (limit %d rows)  schema: %d tables, %d columns in %s",
				u.cfg.Redacted(), u.limit, len(s.Tables), s.ColumnCount(), time.Since(started).Round(time.Millisecond))
			switch {
			case err != nil:
				msg += "  [yellow]" + tview.Escape(err.Error()) + "[-]"
			case saveErr != nil:
				msg += "  [yellow]cache: " + tview.Escape(saveErr.Error()) + "[-]"
			case u.cacheFile != "":
				msg += "  [gray]cached " + tview.Escape(u.cacheFile) + "[-]"
			}
			u.setStatus(msg)
		})
	}()
}

func (u *ui) isRunning() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.running
}

func (u *ui) renderTables() {
	f := strings.ToLower(u.filter.GetText())
	u.tables.Clear()
	for _, n := range u.allTables {
		if f == "" || strings.Contains(strings.ToLower(n), f) {
			u.tables.AddItem(n, "", 0, nil)
		}
	}
	u.sidebar.SetTitle(fmt.Sprintf(" Tables %d/%d ", u.tables.GetItemCount(), len(u.allTables)))
}

func (u *ui) selectStmt(table string) string {
	switch u.cfg.Type {
	case conn.Oracle:
		return fmt.Sprintf("SELECT * FROM %s FETCH FIRST 100 ROWS ONLY", table)
	default:
		return fmt.Sprintf("SELECT * FROM %s LIMIT 100", table)
	}
}

func (u *ui) describe(table string) {
	u.setStatus("[yellow]describing " + tview.Escape(table) + "...[-]")
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		r := query.DescribeTable(ctx, u.db, u.cfg.Type, table)
		u.app.QueueUpdateDraw(func() {
			if r.Err != nil {
				u.setStatus(fmt.Sprintf("[red]describe:[-] %s", tview.Escape(r.Err.Error())))
				return
			}
			u.renderResult(r)
			u.results.SetTitle(fmt.Sprintf(" Columns of %s ", table))
			u.setStatus(fmt.Sprintf("[green]OK[-] %d column(s)", len(r.Rows)))
		})
	}()
}

// ---- history ---------------------------------------------------------------

func historyPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".sqlcli_history")
}

func (u *ui) loadHistory() {
	if u.histFile == "" {
		return
	}
	b, err := os.ReadFile(u.histFile)
	if err != nil {
		return
	}
	for _, e := range strings.Split(string(b), "\n\x00\n") {
		if s := strings.TrimSpace(e); s != "" {
			u.history = append(u.history, s)
		}
	}
	if len(u.history) > 500 {
		u.history = u.history[len(u.history)-500:]
	}
}

func (u *ui) addHistory(stmt string) {
	if n := len(u.history); n > 0 && u.history[n-1] == stmt {
		return
	}
	u.history = append(u.history, stmt)
	if u.histFile == "" {
		return
	}
	f, err := os.OpenFile(u.histFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s\n\x00\n", stmt)
}

func (u *ui) showHistory() {
	list := tview.NewList().ShowSecondaryText(false).SetHighlightFullLine(true)
	for i := len(u.history) - 1; i >= 0; i-- {
		stmt := u.history[i]
		list.AddItem(oneLine(stmt), "", 0, func() {
			u.editor.SetText(stmt, true)
			u.root.RemovePage("history")
			u.app.SetFocus(u.editor)
		})
	}
	list.SetBorder(true).SetTitle(" History (Enter=load, Esc=close) ").SetTitleAlign(tview.AlignLeft)
	u.root.AddPage("history", center(list, 0.8, 0.7), true, true)
	u.app.SetFocus(list)
}

// ---- export ----------------------------------------------------------------

func (u *ui) exportCSV() {
	if u.last == nil {
		u.setStatus("[yellow]nothing to export[-]")
		return
	}
	in := tview.NewInputField().SetLabel("Save CSV as: ").
		SetText(fmt.Sprintf("result_%s.csv", time.Now().Format("20060102_150405")))
	in.SetDoneFunc(func(key tcell.Key) {
		defer func() {
			u.root.RemovePage("export")
			u.app.SetFocus(u.results)
		}()
		if key != tcell.KeyEnter {
			return
		}
		path := in.GetText()
		f, err := os.Create(path)
		if err != nil {
			u.setStatus("[red]export:[-] " + tview.Escape(err.Error()))
			return
		}
		defer f.Close()
		if err := format.Write(f, u.last, "csv", false); err != nil {
			u.setStatus("[red]export:[-] " + tview.Escape(err.Error()))
			return
		}
		u.setStatus(fmt.Sprintf("[green]saved[-] %d row(s) to %s", len(u.last.Rows), tview.Escape(path)))
	})
	in.SetBorder(true)
	u.root.AddPage("export", center(in, 0.6, 0), true, true)
	u.app.SetFocus(in)
}

// ---- help ------------------------------------------------------------------

const helpText = `[yellow::b]GLOBAL[-::-]
  [green]F1[-]              this help
  [green]F5[-] / [green]Ctrl+E[-]     run the editor contents (or only the selected text)
  [green]Ctrl+Enter[-]      run only the statement under the cursor (or the selection)
  [green]Ctrl+Shift+Left/Right[-]  narrow / widen the Tables sidebar
  [green]Ctrl+Shift+Up/Down[-]     move the SQL / Results divider up / down
  [green]Ctrl+Space[-]      open the autocomplete dropdown
  [green]Esc[-]             cancel the running query / close a popup
  [green]F2[-]              show / hide the Tables sidebar
  [green]F3[-]              history popup (Enter loads a statement into the editor)
  [green]F8[-]              export the current result to a CSV file
  [green]F9[-]              explain the statement under the cursor (or the selection)
  [green]Shift+F9[-]        raw explain: JSON plan (PG / MariaDB), PLAN_TABLE rows (Oracle)
  [green]Ctrl+C[-]          copy: selected SQL text / selected result cell / table name
  [green]Ctrl+V[-]          paste the clipboard into the SQL editor
  [green]Ctrl+L[-]          clear the editor
  [green]Tab[-] / [green]Shift+Tab[-]  cycle focus: Tables -> SQL -> Results
  [green]Ctrl+Q[-]          quit

[yellow::b]SQL EDITOR[-::-]
  Type freely; several statements separated by ';' run in order.
  Oracle PL/SQL blocks end with a line containing only '/'.
  Select text with Shift+Arrows and press F5 to run just that part.
  Autocomplete pops up while typing (tables, columns, keywords):
    [green]Up/Down[-] pick, [green]Tab[-] / [green]Enter[-] insert, [green]Esc[-] close.
    After "alias." or "table." only that table's columns are offered.
  The schema is introspected on connect and cached under ./cache, so the
  next start has suggestions immediately; [green]r[-] in the sidebar re-introspects.

[yellow::b]TABLES SIDEBAR[-::-]
  [green]/[-]               filter the list (Enter or Tab returns to the list)
  [green]Enter[-]           run  SELECT * FROM <table>  (first 100 rows)
  [green]d[-]               describe the table (columns, types, nullable)
  [green]i[-]               insert the table name into the editor
  [green]r[-]               reload tables + columns (refreshes the cache)

[yellow::b]RESULTS[-::-]
  Arrow keys / PgUp / PgDn move; [green]Enter[-] on a cell shows its full value.
  Cells are clipped to 40 chars; NULL is shown in gray.
  Rows are capped at the -limit value (default 1000); the status bar says
  "truncated" when the cap was hit.

[yellow::b]COMMAND LINE[-::-]
  sqlcli -h                       full flag reference
  sqlcli <dsn> -sql "select 1"    quick query mode, no TUI
  sqlcli <dsn> -f script.sql      run a script
  History file: ~/.sqlcli_history`

func (u *ui) showHelp() {
	tv := tview.NewTextView().SetDynamicColors(true).SetScrollable(true).SetWrap(false).SetText(helpText)
	tv.SetBorder(true).SetTitle(" Help (Esc=close) ").SetTitleAlign(tview.AlignLeft)
	u.root.AddPage("help", center(tv, 0.8, 0.85), true, true)
	u.app.SetFocus(tv)
}

// ---- helpers ---------------------------------------------------------------

func (u *ui) showText(title, body string) {
	tv := tview.NewTextView().SetText(tview.Escape(body)).SetScrollable(true).SetWrap(true)
	tv.SetBorder(true).SetTitle(title + "(Esc=close) ").SetTitleAlign(tview.AlignLeft)
	u.root.AddPage("text", center(tv, 0.8, 0.7), true, true)
	u.app.SetFocus(tv)
}

// center wraps p in a flex so it is centered with the given fractional size.
// hFrac 0 means "fixed 3 rows" (for single input fields).
func center(p tview.Primitive, wFrac, hFrac float64) tview.Primitive {
	wProp := int(wFrac * 100)
	row := tview.NewFlex().
		AddItem(nil, 0, (100-wProp)/2, false).
		AddItem(p, 0, wProp, true).
		AddItem(nil, 0, (100-wProp)/2, false)
	col := tview.NewFlex().SetDirection(tview.FlexRow)
	if hFrac == 0 {
		col.AddItem(nil, 0, 1, false).AddItem(row, 3, 0, true).AddItem(nil, 0, 1, false)
	} else {
		hProp := int(hFrac * 100)
		col.AddItem(nil, 0, (100-hProp)/2, false).AddItem(row, 0, hProp, true).AddItem(nil, 0, (100-hProp)/2, false)
	}
	return col
}
