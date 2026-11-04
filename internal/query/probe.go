package query

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"sqlcli/internal/conn"
)

// PingResult accumulates database round trips (driver-level pings).
type PingResult struct {
	Sent    int
	Lost    int
	RTTs    []time.Duration // successful round trips, in order
	LastErr error
}

// Loss is the share of lost pings, 0..1.
func (p PingResult) Loss() float64 {
	if p.Sent == 0 {
		return 0
	}
	return float64(p.Lost) / float64(p.Sent)
}

// Summary returns min / avg / max of the round trips and the jitter: the
// mean difference between consecutive round trips (as ping tools report it).
func (p PingResult) Summary() (lo, avg, hi, jitter time.Duration) {
	if len(p.RTTs) == 0 {
		return
	}
	lo, hi = p.RTTs[0], p.RTTs[0]
	var sum, diff time.Duration
	for i, d := range p.RTTs {
		lo, hi = min(lo, d), max(hi, d)
		sum += d
		if i > 0 {
			diff += (d - p.RTTs[i-1]).Abs()
		}
	}
	avg = sum / time.Duration(len(p.RTTs))
	if len(p.RTTs) > 1 {
		jitter = diff / time.Duration(len(p.RTTs)-1)
	}
	return
}

// Ping sends n driver pings over one pooled connection, interval apart. A
// ping that fails or takes longer than timeout counts as lost; the
// connection is then replaced. each is called after every ping.
func Ping(ctx context.Context, db *sql.DB, n int, interval, timeout time.Duration, each func(PingResult)) PingResult {
	var res PingResult
	var c *sql.Conn
	defer func() {
		if c != nil {
			c.Close()
		}
	}()
	for i := 0; i < n && ctx.Err() == nil; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return res
			case <-time.After(interval):
			}
		}
		res.Sent++
		pctx, cancel := context.WithTimeout(ctx, timeout)
		var err error
		if c == nil {
			c, err = db.Conn(pctx)
		}
		var d time.Duration
		if err == nil {
			start := time.Now()
			err = c.PingContext(pctx)
			d = time.Since(start)
		}
		cancel()
		if ctx.Err() != nil {
			res.Sent-- // cancelled by the caller, not lost
			return res
		}
		if err != nil {
			res.Lost++
			res.LastErr = err
			if c != nil {
				c.Raw(func(any) error { return driver.ErrBadConn }) // drop it from the pool
				c.Close()
				c = nil
			}
		} else {
			res.RTTs = append(res.RTTs, d)
		}
		if each != nil {
			each(res)
		}
	}
	return res
}

// InfoItem is one line of server information.
type InfoItem struct {
	Label, Value string
}

type probe struct {
	label string
	sql   string
	conv  func(string) string // optional value formatting
}

// probes are best-effort: a query that fails (old version, no privilege) is
// skipped, and a later probe with the same label is tried instead.
var probes = map[conn.DBType][]probe{
	conn.Postgres: {
		{"Server", "SELECT 'PostgreSQL ' || current_setting('server_version')", nil},
		{"User / database", "SELECT current_user || ' / ' || current_database()", nil},
		{"Server address", "SELECT coalesce(host(inet_server_addr()) || ':' || inet_server_port(), 'unix socket')", nil},
		{"Session", "SELECT 'pid ' || pg_backend_pid()", nil},
		{"Encryption", "SELECT CASE WHEN ssl THEN version || ' ' || cipher ELSE 'none' END FROM pg_stat_ssl WHERE pid = pg_backend_pid()", nil},
		{"Encoding", "SELECT current_setting('server_encoding') || ' (client ' || current_setting('client_encoding') || ')'", nil},
		{"Time zone", "SELECT current_setting('TimeZone')", nil},
		{"Server uptime", "SELECT extract(epoch FROM now() - pg_postmaster_start_time())::bigint::text", fmtSeconds},
		{"Connections", "SELECT count(*) || ' / max ' || current_setting('max_connections') FROM pg_stat_activity", nil},
	},
	conn.MariaDB: {
		{"Server", "SELECT CONCAT(@@version_comment, ' ', version())", nil},
		{"Server", "SELECT version()", nil},
		{"User / database", "SELECT CONCAT(current_user(), ' / ', COALESCE(database(), '-'))", nil},
		{"Server address", "SELECT CONCAT(@@hostname, ':', @@port)", nil},
		{"Session", "SELECT CONCAT('id ', connection_id())", nil},
		{"Encryption", "SHOW SESSION STATUS LIKE 'Ssl_cipher'", noneIfEmpty},
		{"Encoding", "SELECT CONCAT(@@character_set_server, ' (client ', @@character_set_client, ')')", nil},
		{"Time zone", "SELECT CONCAT(@@time_zone, ' (system ', @@system_time_zone, ')')", nil},
		{"Server uptime", "SHOW GLOBAL STATUS LIKE 'Uptime'", fmtSeconds},
		{"Connections", "SELECT CONCAT(VARIABLE_VALUE, ' / max ', @@max_connections) FROM information_schema.GLOBAL_STATUS WHERE VARIABLE_NAME = 'THREADS_CONNECTED'", nil},
		{"Connections", "SELECT CONCAT(VARIABLE_VALUE, ' / max ', @@max_connections) FROM performance_schema.global_status WHERE VARIABLE_NAME = 'Threads_connected'", nil},
	},
	conn.Oracle: {
		{"Server", "SELECT banner FROM v$version WHERE ROWNUM = 1", nil},
		{"Server", "SELECT product || ' ' || version FROM product_component_version WHERE ROWNUM = 1", nil},
		{"User / database", "SELECT USER || ' / ' || SYS_CONTEXT('USERENV', 'DB_NAME') FROM DUAL", nil},
		{"Service @ host", "SELECT SYS_CONTEXT('USERENV', 'SERVICE_NAME') || ' @ ' || SYS_CONTEXT('USERENV', 'SERVER_HOST') FROM DUAL", nil},
		{"Session", "SELECT 'sid ' || SYS_CONTEXT('USERENV', 'SID') FROM DUAL", nil},
		{"Protocol", "SELECT SYS_CONTEXT('USERENV', 'NETWORK_PROTOCOL') FROM DUAL", nil},
		{"Encoding", "SELECT value FROM nls_database_parameters WHERE parameter = 'NLS_CHARACTERSET'", nil},
		{"Time zone", "SELECT DBTIMEZONE || ' (session ' || SESSIONTIMEZONE || ')' FROM DUAL", nil},
		{"Server uptime", "SELECT TO_CHAR(ROUND((SYSDATE - startup_time) * 86400)) FROM v$instance", fmtSeconds},
	},
}

// clockQueries return the server's current time as Unix seconds.
var clockQueries = map[conn.DBType]string{
	conn.Postgres: "SELECT extract(epoch FROM clock_timestamp())::float8",
	conn.MariaDB:  "SELECT unix_timestamp(now(6))",
	conn.Oracle: "SELECT (CAST(SYS_EXTRACT_UTC(SYSTIMESTAMP) AS DATE) - DATE '1970-01-01') * 86400" +
		" + MOD(EXTRACT(SECOND FROM SYS_EXTRACT_UTC(SYSTIMESTAMP)), 1) FROM DUAL",
}

// ServerInfo collects what the server tells about itself. Each query gets
// timeout; failures are skipped.
func ServerInfo(ctx context.Context, db *sql.DB, t conn.DBType, timeout time.Duration) []InfoItem {
	var items []InfoItem
	done := map[string]bool{}
	for _, p := range probes[t] {
		if done[p.label] || ctx.Err() != nil {
			continue
		}
		v, err := queryString(ctx, db, p.sql, timeout)
		if err != nil {
			continue
		}
		if p.conv != nil {
			v = p.conv(v)
		}
		done[p.label] = true
		items = append(items, InfoItem{p.label, v})
	}
	return items
}

// ClockSkew reports how far the server clock is ahead of the local one
// (negative: behind), using the middle of the round trip as local time.
func ClockSkew(ctx context.Context, db *sql.DB, t conn.DBType, timeout time.Duration) (time.Duration, error) {
	q, ok := clockQueries[t]
	if !ok {
		return 0, fmt.Errorf("unsupported")
	}
	start := time.Now()
	v, err := queryString(ctx, db, q, timeout)
	end := time.Now()
	if err != nil {
		return 0, err
	}
	secs, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil {
		return 0, err
	}
	whole, frac := math.Modf(secs)
	server := time.Unix(int64(whole), int64(frac*1e9))
	local := start.Add(end.Sub(start) / 2)
	return server.Sub(local), nil
}

// queryString runs q and returns the last column of its first row.
func queryString(ctx context.Context, db *sql.DB, q string, timeout time.Duration) (string, error) {
	qctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	rows, err := db.QueryContext(qctx, q)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return "", err
	}
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return "", err
		}
		return "", sql.ErrNoRows
	}
	vals := make([]sql.NullString, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		return "", err
	}
	last := vals[len(vals)-1]
	if !last.Valid {
		return "", nil
	}
	return last.String, nil
}

func noneIfEmpty(s string) string {
	if strings.TrimSpace(s) == "" {
		return "none"
	}
	return s
}

// fmtSeconds turns a number of seconds into "3d 4h 5m".
func fmtSeconds(s string) string {
	n, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return s
	}
	return FormatDuration(time.Duration(n) * time.Second)
}

// FormatDuration renders d as "3d 4h 5m 6s", dropping leading zero units.
func FormatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	days := d / (24 * time.Hour)
	d -= days * 24 * time.Hour
	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	s := (d - m*time.Minute) / time.Second
	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh %dm", days, h, m)
	case h > 0:
		return fmt.Sprintf("%dh %dm %ds", h, m, s)
	case m > 0:
		return fmt.Sprintf("%dm %ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}
