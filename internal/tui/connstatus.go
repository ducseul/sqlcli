package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"sqlcli/internal/conn"
	"sqlcli/internal/query"
)

const (
	statusPings        = 10
	statusPingInterval = 200 * time.Millisecond
	statusPingTimeout  = 2 * time.Second
	statusTCPProbes    = 3
	statusQueryTimeout = 3 * time.Second
)

// connStatus is one measurement shown in the Connection status popup. Its
// fields are only touched on the draw thread.
type connStatus struct {
	tv     *tview.TextView
	cancel context.CancelFunc
	phase  string // what is being measured now ("" when done)

	ping     query.PingResult
	pingDone bool

	tcp     []time.Duration
	tcpErr  error
	tcpDone bool

	skew     time.Duration
	skewErr  error
	skewDone bool

	info     []query.InfoItem
	infoDone bool

	// traffic rate, sampled by the refresh ticker
	lastAt             time.Time
	lastSent, lastRecv int64
	upRate, downRate   float64 // bytes per second
}

// showConnStatus opens the Connection status popup and starts measuring.
func (u *ui) showConnStatus() {
	if u.db == nil {
		u.setStatus("[yellow]not connected[-]")
		return
	}
	tv := tview.NewTextView().SetDynamicColors(true).SetScrollable(true).SetWrap(false)
	tv.SetBorder(true).SetTitle(" Connection status (r=measure again, Esc=close) ").SetTitleAlign(tview.AlignLeft)
	tv.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		if ev.Key() == tcell.KeyRune && (ev.Rune() == 'r' || ev.Rune() == 'R') {
			u.measureConn(tv)
			return nil
		}
		return ev
	})
	u.root.AddPage("connstatus", center(tv, 0.9, 0.9), true, true)
	u.app.SetFocus(tv)
	u.measureConn(tv)
}

// stopConnStatus cancels a running measurement (the popup is closing).
func (u *ui) stopConnStatus() {
	if u.cs != nil {
		u.cs.cancel()
		u.cs = nil
	}
}

// measureConn (re)starts the measurement shown in tv.
func (u *ui) measureConn(tv *tview.TextView) {
	u.stopConnStatus()
	ctx, cancel := context.WithCancel(context.Background())
	cs := &connStatus{tv: tv, cancel: cancel, phase: "pinging"}
	if t := u.cfg.Traffic(); t != nil {
		st := t.Stats()
		cs.lastAt, cs.lastSent, cs.lastRecv = time.Now(), st.Sent, st.Received
	}
	u.cs = cs
	u.renderConnStatus(cs)

	// update applies f to cs on the draw thread, unless cs was replaced.
	update := func(f func()) {
		u.app.QueueUpdateDraw(func() {
			if u.cs != cs {
				return
			}
			f()
			u.renderConnStatus(cs)
		})
	}
	go func() {
		ping := query.Ping(ctx, u.db, statusPings, statusPingInterval, statusPingTimeout, func(p query.PingResult) {
			update(func() { cs.ping = p })
		})
		update(func() { cs.ping, cs.pingDone, cs.phase = ping, true, "TCP connect" })

		var tcp []time.Duration
		var tcpErr error
		if u.cfg.Host != "" {
			for i := 0; i < statusTCPProbes && ctx.Err() == nil; i++ {
				dctx, dcancel := context.WithTimeout(ctx, statusPingTimeout)
				d, err := u.cfg.DialTCP(dctx)
				dcancel()
				if err != nil {
					tcpErr = err
					break
				}
				tcp = append(tcp, d)
			}
		}
		update(func() { cs.tcp, cs.tcpErr, cs.tcpDone, cs.phase = tcp, tcpErr, true, "clock" })

		skew, skewErr := query.ClockSkew(ctx, u.db, u.cfg.Type, statusQueryTimeout)
		update(func() { cs.skew, cs.skewErr, cs.skewDone, cs.phase = skew, skewErr, true, "server info" })

		info := query.ServerInfo(ctx, u.db, u.cfg.Type, statusQueryTimeout)
		update(func() { cs.info, cs.infoDone, cs.phase = info, true, "" })
	}()
	go func() {
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				update(func() {})
			}
		}
	}()
}

// renderConnStatus redraws the popup from cs and the live counters.
func (u *ui) renderConnStatus(cs *connStatus) {
	var b strings.Builder
	sec := func(name string) { fmt.Fprintf(&b, "\n[yellow::b]%s[-::-]\n", name) }
	row := func(label, format string, args ...any) {
		fmt.Fprintf(&b, "  [green]%-16s[-] %s\n", label, fmt.Sprintf(format, args...))
	}
	pending := "[gray]measuring...[-]"

	sec("CONNECTION")
	row("Target", "%s", tview.Escape(u.cfg.Redacted()))
	if len(u.cfg.Via) > 0 {
		row("Route", "%s", tview.Escape(u.cfg.Via.String()))
	} else {
		row("Route", "direct")
	}
	var traffic *conn.TrafficStats
	if t := u.cfg.Traffic(); t != nil {
		st := t.Stats()
		traffic = &st
		if st.LocalAddr != "" {
			row("Local -> remote", "%s -> %s", tview.Escape(st.LocalAddr), tview.Escape(st.RemoteAddr))
		}
		row("Connected for", "%s", query.FormatDuration(time.Since(st.Since)))
	}

	sec("LATENCY")
	p := cs.ping
	if p.Sent == 0 {
		row("DB ping", "%s", pending)
	} else {
		more := ""
		if !cs.pingDone {
			more = fmt.Sprintf("  [gray](%d/%d...)[-]", p.Sent, statusPings)
		}
		lossColor := "green"
		if p.Lost > 0 {
			lossColor = "red"
		}
		row("DB ping", "%d sent, %d received, [%s]%.0f%% loss[-]%s", p.Sent, p.Sent-p.Lost, lossColor, p.Loss()*100, more)
		if len(p.RTTs) > 0 {
			lo, avg, hi, jit := p.Summary()
			row("Round trip", "min %s  avg [%s]%s[-]  max %s", ms(lo), latencyColor(avg), ms(avg), ms(hi))
			row("Jitter", "%s", ms(jit))
		}
		if p.LastErr != nil {
			row("Last error", "[red]%s[-]", tview.Escape(oneLine(p.LastErr.Error())))
		}
	}
	switch {
	case u.cfg.Host == "":
		row("TCP connect", "[gray]n/a (no host)[-]")
	case !cs.tcpDone:
		row("TCP connect", "%s", pendingIf(cs.phase == "TCP connect", pending))
	case len(cs.tcp) > 0:
		var lo, hi, sum time.Duration = cs.tcp[0], cs.tcp[0], 0
		for _, d := range cs.tcp {
			lo, hi, sum = min(lo, d), max(hi, d), sum+d
		}
		via := ""
		if len(u.cfg.Via) > 0 {
			via = " [gray](through the route)[-]"
		}
		row("TCP connect", "min %s  avg %s  max %s  (%d tries)%s", ms(lo), ms(sum/time.Duration(len(cs.tcp))), ms(hi), len(cs.tcp), via)
	default:
		row("TCP connect", "[red]%s[-]", tview.Escape(oneLine(errText(cs.tcpErr))))
	}
	switch {
	case !cs.skewDone:
		row("Clock skew", "%s", pendingIf(cs.phase == "clock", pending))
	case cs.skewErr != nil:
		row("Clock skew", "[gray]n/a (%s)[-]", tview.Escape(oneLine(cs.skewErr.Error())))
	default:
		dir := "ahead of"
		if cs.skew < 0 {
			dir = "behind"
		}
		color := "green"
		if cs.skew.Abs() > 2*time.Second {
			color = "yellow"
		}
		row("Clock skew", "server is [%s]%s[-] %s this machine", color, ms(cs.skew.Abs()), dir)
	}

	sec("TRAFFIC (this session)")
	if traffic != nil {
		now := time.Now()
		if dt := now.Sub(cs.lastAt).Seconds(); dt >= 0.5 {
			cs.upRate = float64(traffic.Sent-cs.lastSent) / dt
			cs.downRate = float64(traffic.Received-cs.lastRecv) / dt
			cs.lastAt, cs.lastSent, cs.lastRecv = now, traffic.Sent, traffic.Received
		}
		up := max(time.Since(traffic.Since).Seconds(), 1)
		row("Upload", "%-12s now %-12s avg %s", fmtBytes(float64(traffic.Sent)), fmtBytes(cs.upRate)+"/s", fmtBytes(float64(traffic.Sent)/up)+"/s")
		row("Download", "%-12s now %-12s avg %s", fmtBytes(float64(traffic.Received)), fmtBytes(cs.downRate)+"/s", fmtBytes(float64(traffic.Received)/up)+"/s")
		row("Total", "%s", fmtBytes(float64(traffic.Sent+traffic.Received)))
		row("Connections", "%d open, %d dialed since start", traffic.Active, traffic.Opened)
	} else {
		row("Upload / download", "[gray]n/a[-]")
	}
	ps := u.db.Stats()
	row("Pool", "%d open (max %d), %d in use, %d idle, waited %d times (%s)",
		ps.OpenConnections, ps.MaxOpenConnections, ps.InUse, ps.Idle, ps.WaitCount, ps.WaitDuration.Round(time.Millisecond))

	sec("SERVER")
	switch {
	case !cs.infoDone:
		row("Info", "%s", pendingIf(cs.phase == "server info", pending))
	case len(cs.info) == 0:
		row("Info", "[gray]n/a[-]")
	default:
		for _, it := range cs.info {
			row(it.Label, "%s", tview.Escape(oneLine(it.Value)))
		}
	}

	if cs.phase != "" {
		fmt.Fprintf(&b, "\n[gray]measuring %s...[-]", cs.phase)
	} else {
		fmt.Fprintf(&b, "\n[gray]done; r measures again[-]")
	}
	cs.tv.SetText(b.String())
}

func pendingIf(active bool, pending string) string {
	if active {
		return pending
	}
	return "[gray]waiting...[-]"
}

func errText(err error) string {
	if err == nil {
		return "failed"
	}
	return err.Error()
}

// ms renders a duration in milliseconds with a precision that fits it.
func ms(d time.Duration) string {
	v := float64(d) / float64(time.Millisecond)
	switch {
	case v < 10:
		return fmt.Sprintf("%.2f ms", v)
	case v < 100:
		return fmt.Sprintf("%.1f ms", v)
	}
	return fmt.Sprintf("%.0f ms", v)
}

func latencyColor(d time.Duration) string {
	switch {
	case d < 50*time.Millisecond:
		return "green"
	case d < 150*time.Millisecond:
		return "yellow"
	}
	return "red"
}

// fmtBytes renders n bytes with a binary unit.
func fmtBytes(n float64) string {
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	i := 0
	for n >= 1024 && i < len(units)-1 {
		n /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%.0f B", n)
	}
	return fmt.Sprintf("%.1f %s", n, units[i])
}
