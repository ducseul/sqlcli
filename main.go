// sqlcli - a single-binary CLI/TUI client for PostgreSQL, MariaDB/MySQL and Oracle.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"

	"golang.org/x/term"

	"sqlcli/internal/conn"
	"sqlcli/internal/format"
	"sqlcli/internal/query"
	"sqlcli/internal/tui"
)

var version = "dev"

func usage() {
	fmt.Fprintf(os.Stderr, `sqlcli %s - single-binary SQL client (postgres | mariadb | oracle)

USAGE
  sqlcli [connection] [-sql "..." | -f file.sql] [options]
  sqlcli [connection]                       # opens the TUI

CONNECTION (pick one style)
  -dsn URL      auto-detect type from scheme, e.g.
                  postgres://user:pass@host:5432/dbname?sslmode=disable
                  mariadb://user:pass@host:3306/dbname
                  oracle://user:pass@host:1521/service_name
                  user/pass@host:1521/service         (sqlplus style)
                  user:pass@tcp(host:3306)/db         (mysql style)
                  host=h port=5432 user=u dbname=d    (libpq style)
                A bare first argument is treated as -dsn.
  -type T       postgres | mariadb | mysql | oracle
  -host H       default localhost
  -port N       default 5432 / 3306 / 1521 by type
  -user U
  -password P   omit to be prompted (or set SQLCLI_PASSWORD)
  -db NAME      database name (postgres/mariadb) or service name (oracle)

QUERY MODE
  -sql "..."    run statement(s) and exit
  -f FILE       run a .sql script and exit ("-" = stdin). Piped stdin is also run.
  -format F     table (default) | csv | tsv | json | vertical
  -limit N      max rows to fetch per statement (0 = all)
  -continue     keep running the script after an error
  -quiet        suppress row counts / timings

OTHER
  -timeout D    connect/query timeout (default 30s)
  -version

ENV
  SQLCLI_DSN, SQLCLI_PASSWORD
`, version)
}

func main() {
	var (
		dsn, typ, host, user, password, database string
		port                                     int
		sqlText, file, fmtName                   string
		limit                                    int
		timeout                                  time.Duration
		continueOnErr, quiet, showVersion        bool
	)
	flag.Usage = usage
	flag.StringVar(&dsn, "dsn", os.Getenv("SQLCLI_DSN"), "connection string")
	flag.StringVar(&typ, "type", "", "database type")
	flag.StringVar(&host, "host", "", "host")
	flag.IntVar(&port, "port", 0, "port")
	flag.StringVar(&user, "user", "", "username")
	flag.StringVar(&password, "password", os.Getenv("SQLCLI_PASSWORD"), "password")
	flag.StringVar(&database, "db", "", "database / service name")
	flag.StringVar(&sqlText, "sql", "", "SQL to execute")
	flag.StringVar(&file, "f", "", "SQL script file")
	flag.StringVar(&fmtName, "format", "table", "output format")
	flag.IntVar(&limit, "limit", 0, "max rows")
	flag.DurationVar(&timeout, "timeout", 30*time.Second, "timeout")
	flag.BoolVar(&continueOnErr, "continue", false, "continue on error")
	flag.BoolVar(&quiet, "quiet", false, "quiet")
	flag.BoolVar(&showVersion, "version", false, "print version")
	flag.Parse()

	// Go's flag package stops at the first positional argument, so allow the
	// DSN to appear anywhere: pull it out and keep parsing the remainder.
	var positional []string
	for flag.NArg() > 0 {
		positional = append(positional, flag.Arg(0))
		if err := flag.CommandLine.Parse(flag.Args()[1:]); err != nil {
			os.Exit(2)
		}
	}
	if showVersion {
		fmt.Println("sqlcli", version)
		return
	}
	if len(positional) > 1 {
		fmt.Fprintf(os.Stderr, "error: unexpected arguments: %v\n", positional[1:])
		os.Exit(2)
	}
	if len(positional) == 1 {
		if dsn != "" && dsn != os.Getenv("SQLCLI_DSN") {
			fmt.Fprintln(os.Stderr, "error: connection string given both as -dsn and as an argument")
			os.Exit(2)
		}
		dsn = positional[0]
	}

	cfg, err := buildConfig(dsn, typ, host, port, user, password, database)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		fmt.Fprintln(os.Stderr, "run with -h for help")
		os.Exit(2)
	}
	cfg.Timeout = timeout

	// Figure out script source before connecting so we fail fast on bad input.
	script := sqlText
	stdinPiped := !term.IsTerminal(int(os.Stdin.Fd()))
	switch {
	case file == "-" || (file == "" && script == "" && stdinPiped):
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			fatal(err)
		}
		script = string(b)
	case file != "":
		b, err := os.ReadFile(file)
		if err != nil {
			fatal(err)
		}
		script = string(b)
	}

	// Prompt for password if we have a user but no password and a terminal.
	if cfg.Password == "" && cfg.User != "" && !stdinPiped {
		fmt.Fprintf(os.Stderr, "Password for %s: ", cfg.User)
		pw, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			fatal(err)
		}
		cfg.Password = string(pw)
	}

	db, err := cfg.Open()
	if err != nil {
		fatal(err)
	}
	defer db.Close()

	if strings.TrimSpace(script) == "" {
		// Interactive TUI.
		if err := tui.Run(db, cfg, limit); err != nil {
			fatal(err)
		}
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	out := bufio.NewWriterSize(os.Stdout, 64*1024)
	defer out.Flush()

	exitCode := 0
	results := query.ExecScript(ctx, db, script, limit, continueOnErr)
	for i, r := range results {
		if i > 0 && !quiet {
			fmt.Fprintln(out)
		}
		if err := format.Write(out, r, fmtName, !quiet); err != nil {
			exitCode = 1
		}
	}
	out.Flush()
	os.Exit(exitCode)
}

func buildConfig(dsn, typ, host string, port int, user, password, database string) (*conn.Config, error) {
	var cfg *conn.Config
	if dsn != "" {
		c, err := conn.Parse(dsn)
		if err != nil {
			return nil, err
		}
		cfg = c
	} else {
		if typ == "" {
			return nil, fmt.Errorf("provide -dsn or -type with -host/-user/-db")
		}
		cfg = &conn.Config{Params: map[string]string{}}
	}
	// Discrete flags override values from the DSN.
	if typ != "" {
		t, err := conn.NormalizeType(typ)
		if err != nil {
			return nil, err
		}
		cfg.Type = t
	}
	if host != "" {
		cfg.Host = host
	}
	if port != 0 {
		cfg.Port = port
	}
	if user != "" {
		cfg.User = user
	}
	if password != "" {
		cfg.Password = password
	}
	if database != "" {
		cfg.Database = database
	}
	return cfg, nil
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
