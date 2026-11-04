// sqlcli - a single-binary CLI/TUI client for PostgreSQL, MariaDB/MySQL and Oracle.
package main

import (
	"bufio"
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"strings"
	"time"

	"golang.org/x/term"

	"sqlcli/internal/conn"
	"sqlcli/internal/format"
	"sqlcli/internal/pager"
	"sqlcli/internal/query"
	"sqlcli/internal/tui"
	"sqlcli/internal/tunnel"
)

var (
	version   = "dev"
	buildDate = "" // set with -ldflags "-X main.buildDate=..."; falls back to the commit time
)

const author = "ducseul"

// builtAt is the build date, or the VCS commit time for plain `go build`.
func builtAt() string {
	if buildDate != "" {
		return buildDate
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == "vcs.time" {
				return s.Value
			}
		}
	}
	return "unknown"
}

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
                  jdbc:oracle:thin:@host:1521:SID     (ojdbc, also @host:1521/service,
                                                       @//host:1521/service, @(DESCRIPTION=...))
                  jdbc:postgresql://host:5432/db      (also jdbc:mysql://, jdbc:mariadb://)
                A bare first argument is treated as -dsn.
  -type T       postgres | mariadb | mysql | oracle
  -host H       default localhost
  -port N       default 5432 / 3306 / 1521 by type
  -user, -u U
  -password, -p P   omit to be prompted (or set SQLCLI_PASSWORD)
  -db NAME      database name (postgres/mariadb) or service name (oracle)

PROXY / TUNNEL (repeatable; hops are chained in the order given)
  -proxy P      socks5:host:port | socks4:host:port | http:host:port
                (also scheme://[user:pass@]host:port; default port 1080 / 8080)
  -tun, -tunnel T   ssh tunnel [user[:pass]@]host[:port] (default port 22)
                auth: ssh-agent, -tun-key, ~/.ssh/id_*, then password (prompted)
  -tun-key FILE private key for ssh tunnels
                e.g. -proxy socks5:10.14.5.11:8443 -tun app@10.15.5.1
                     reaches the ssh server through the proxy, the database through ssh

QUERY MODE
  -sql "..."    run statement(s) and exit
  -f FILE       run a .sql script and exit ("-" = stdin). Piped stdin is also run.
  -format F     auto (default: table, or vertical when wider than the terminal)
                | table | csv | tsv | json | vertical
  -o, -output FILE  write results to FILE as UTF-8 (use this instead of '>' in
                PowerShell, which corrupts non-ASCII text; .csv gets a BOM for Excel on Windows only)
  -nopager      don't page output taller than the terminal
  -limit N      max rows to fetch per statement (0 = all)
  -continue     keep running the script after an error
  -quiet        suppress row counts / timings

OTHER
  -timeout D    connect/query timeout (default 30s)
  -version, -v  print version and author
  -h, help      show this help

ENV
  SQLCLI_DSN, SQLCLI_PASSWORD

Author: %s
`, version, author)
}

func main() {
	var (
		dsn, typ, host, user, password, database   string
		port                                       int
		sqlText, file, fmtName, outFile            string
		limit                                      int
		timeout                                    time.Duration
		continueOnErr, quiet, showVersion, noPager bool
		via                                        tunnel.Chain
		tunKey                                     string
	)
	flag.Usage = usage
	if len(os.Args) == 2 {
		switch strings.ToLower(os.Args[1]) {
		case "help", "-h", "--help", "-help", "/?", "/h":
			usage()
			return
		}
	}
	flag.StringVar(&dsn, "dsn", os.Getenv("SQLCLI_DSN"), "connection string")
	flag.StringVar(&typ, "type", "", "database type")
	flag.StringVar(&host, "host", "", "host")
	flag.IntVar(&port, "port", 0, "port")
	flag.StringVar(&user, "user", "", "username")
	flag.StringVar(&user, "u", "", "username (short for -user)")
	flag.StringVar(&password, "password", os.Getenv("SQLCLI_PASSWORD"), "password")
	flag.StringVar(&password, "p", os.Getenv("SQLCLI_PASSWORD"), "password (short for -password)")
	flag.StringVar(&database, "db", "", "database / service name")
	flag.Var(hopFlag{&via, tunnel.ParseProxy}, "proxy", "proxy hop (socks4|socks5|http)")
	flag.Var(hopFlag{&via, tunnel.ParseSSH}, "tun", "ssh tunnel hop")
	flag.Var(hopFlag{&via, tunnel.ParseSSH}, "tunnel", "ssh tunnel hop (same as -tun)")
	flag.StringVar(&tunKey, "tun-key", "", "private key for ssh tunnels")
	flag.StringVar(&sqlText, "sql", "", "SQL to execute")
	flag.StringVar(&file, "f", "", "SQL script file")
	flag.StringVar(&fmtName, "format", "auto", "output format")
	flag.StringVar(&outFile, "o", "", "write output to FILE (UTF-8)")
	flag.StringVar(&outFile, "output", "", "write output to FILE (UTF-8)")
	flag.BoolVar(&noPager, "nopager", false, "disable pager")
	flag.IntVar(&limit, "limit", 0, "max rows")
	flag.DurationVar(&timeout, "timeout", 30*time.Second, "timeout")
	flag.BoolVar(&continueOnErr, "continue", false, "continue on error")
	flag.BoolVar(&quiet, "quiet", false, "quiet")
	flag.BoolVar(&showVersion, "version", false, "print version")
	flag.BoolVar(&showVersion, "v", false, "print version (short for -version)")
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
		fmt.Printf("sqlcli %s by %s (built %s)\n", version, author, builtAt())
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
	for _, h := range via {
		if h.Kind == tunnel.SSH {
			h.KeyFile = tunKey
		}
	}
	cfg.Via = via

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

	if !stdinPiped {
		tunnel.Prompt = readSecret
	}

	// Prompt for password if we have a user but no password and a terminal.
	if cfg.Password == "" && cfg.User != "" && !stdinPiped {
		pw, err := readSecret(fmt.Sprintf("Password for %s: ", cfg.User))
		if err != nil {
			fatal(err)
		}
		cfg.Password = pw
	}

	db, err := cfg.Open()
	if err != nil {
		fatal(err)
	}
	defer db.Close()

	if strings.TrimSpace(script) == "" {
		// Interactive TUI.
		about := tui.About{Version: version, Author: author, BuildDate: builtAt()}
		if err := tui.Run(db, cfg, limit, about); err != nil {
			fatal(err)
		}
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	// On an interactive terminal fit tables to its width and page long output.
	width, height := 0, 0
	if term.IsTerminal(int(os.Stdout.Fd())) {
		width, height, _ = term.GetSize(int(os.Stdout.Fd()))
	}
	paging := !noPager && !stdinPiped && height > 0

	var buf bytes.Buffer
	var out io.Writer = &buf
	var sink io.Writer = os.Stdout
	if outFile != "" {
		// Write the file ourselves: shell redirection on Windows PowerShell
		// re-encodes our UTF-8 through the console codepage and mangles it.
		f, err := os.Create(outFile)
		if err != nil {
			fatal(err)
		}
		defer f.Close()
		// BOM so Excel detects UTF-8; Unix tools would read it as part of the first header.
		if runtime.GOOS == "windows" && strings.HasSuffix(strings.ToLower(outFile), ".csv") {
			f.WriteString("\xEF\xBB\xBF")
		}
		sink = f
		width, paging = 0, false
	}
	bw := bufio.NewWriterSize(sink, 64*1024)
	if !paging {
		out = bw
	}

	exitCode := 0
	results := query.ExecScript(ctx, db, script, limit, continueOnErr)
	for i, r := range results {
		if i > 0 && !quiet {
			fmt.Fprintln(out)
		}
		if err := format.WriteWidth(out, r, fmtName, !quiet, width); err != nil {
			exitCode = 1
		}
	}
	bw.Flush()
	if outFile != "" {
		fmt.Fprintf(os.Stderr, "wrote %s\n", outFile)
	}
	if paging {
		if err := pager.Page(os.Stdout, os.Stdin, buf.String(), width, height); err != nil {
			fatal(err)
		}
	}
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

// hopFlag appends a parsed proxy / tunnel hop to a shared chain, so -proxy and
// -tun keep their relative order on the command line.
type hopFlag struct {
	chain *tunnel.Chain
	parse func(string) (tunnel.Hop, error)
}

func (f hopFlag) String() string {
	if f.chain == nil {
		return ""
	}
	return f.chain.String()
}

func (f hopFlag) Set(s string) error {
	h, err := f.parse(s)
	if err != nil {
		return err
	}
	*f.chain = append(*f.chain, &h)
	return nil
}

func readSecret(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	return string(b), err
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
