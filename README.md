# sqlcli

Single-binary SQL client (CLI + TUI) for **PostgreSQL**, **MariaDB/MySQL** and **Oracle**.
Written in Go, no runtime, no Oracle Instant Client, no cgo. One `.exe` / ELF file per platform.

## Why Go (vs Rust / Java 8)

| | Go | Rust | Java 8 |
|---|---|---|---|
| Single static binary | Yes (`CGO_ENABLED=0`) | Yes | No. Needs a JRE; GraalVM native-image dropped Java 8 |
| Oracle driver without Instant Client | Yes, `sijms/go-ora` (pure Go) | No. `oracle`/`sibyl` crates need ODPI-C + Instant Client DLLs | Yes, ojdbc thin (but see above) |
| PostgreSQL / MariaDB pure drivers | `pgx`, `go-sql-driver/mysql` | `sqlx`, `tokio-postgres` | JDBC |
| TUI library maturity | `tview`, `bubbletea` | `ratatui` | `lanterna` |
| Cross-compile | `GOOS=linux go build` | needs target toolchains | n/a |

Oracle is the deciding factor: Go is the only one of the three with a pure driver, so the binary stays self-contained.

## Build

```bash
go build -trimpath -ldflags "-s -w" -o sqlcli .
```

Cross-platform release into `dist/` (Windows, Linux, macOS; amd64 + arm64):

```powershell
.\build.ps1 -Version 0.1.0
```

or `make release` on Linux/macOS.

## Connect

Connection can be given as one connection string (type auto-detected from the scheme) or as discrete flags.

```bash
# URL, type detected from scheme
sqlcli "postgres://user:pass@host:5432/dbname?sslmode=disable"
sqlcli "mariadb://user:pass@host:3306/dbname"
sqlcli "oracle://user:pass@host:1521/SERVICE_NAME"

# Other recognised shapes
sqlcli "user/pass@host:1521/SERVICE"            # sqlplus style  -> oracle
sqlcli "user:pass@tcp(host:3306)/dbname"        # go-mysql style -> mariadb
sqlcli "host=h port=5432 user=u dbname=d"       # libpq style    -> postgres

# JDBC / ojdbc URLs
sqlcli "jdbc:oracle:thin:@host:1521:SID"         # SID
sqlcli "jdbc:oracle:thin:@//host:1521/SERVICE"   # service name (also @host:1521/SERVICE)
sqlcli "jdbc:oracle:thin:@(DESCRIPTION=...)"     # TNS descriptor
sqlcli "jdbc:postgresql://host:5432/db?user=u&password=p"   # also jdbc:mysql://, jdbc:mariadb://

# Discrete flags
sqlcli -type oracle -host 10.0.0.5 -port 1521 -user scott -db ORCLPDB1
sqlcli -type mariadb -host db.local -user root -password secret -db shop
sqlcli -type mariadb -host db.local -u root -p secret -db shop   # -u / -p are short for -user / -password
```

`-type` accepts `postgres|postgresql|pg`, `mariadb|mysql`, `oracle|ora`.
Flags override values parsed from the connection string.
If no password is given you are prompted (hidden input). `SQLCLI_DSN` and `SQLCLI_PASSWORD` env vars are honoured.

## Proxy and SSH tunnel

`-proxy` and `-tun` can be repeated and mixed; the hops are chained in the order they appear on the command line.
Each hop is reached through the ones before it and the database through the last one. Host names are resolved at the far end of the chain.

```bash
# through a SOCKS5 proxy
sqlcli -proxy socks5:10.14.5.11:8443 "postgres://u:p@db.internal/app"

# through an SSH bastion (like ssh -L)
sqlcli -tun app@10.15.5.1 "oracle://u:p@10.20.0.7:1521/SVC"

# SSH reached via the proxy, then the database via SSH
sqlcli -proxy socks5:10.14.5.11:8443 -tun app@10.15.5.1 "mariadb://u:p@10.20.0.8/shop"
```

| Flag | Meaning |
|---|---|
| `-proxy P` | `socks5:host:port`, `socks4:host:port` (SOCKS4a for host names) or `http:host:port` (HTTP CONNECT); also `scheme://[user:pass@]host:port`. Default port 1080 for SOCKS, 8080 for HTTP |
| `-tun T`, `-tunnel T` | SSH tunnel `[user[:pass]@]host[:port]`, default port 22, default user the OS user |
| `-tun-key FILE` | private key for SSH tunnels (passphrase is prompted) |

SSH auth tries ssh-agent (`SSH_AUTH_SOCK`), `-tun-key`, `~/.ssh/id_ed25519|id_ecdsa|id_rsa`, then the password (prompted if not given).
Host keys are checked against `~/.ssh/known_hosts`: a changed key is refused, an unknown host is accepted with a warning showing its fingerprint.

## Quick query mode

Any of `-sql`, `-f`, or piped stdin runs the statements and exits (exit code 1 on SQL error, 2 on usage error).

```bash
sqlcli "postgres://u:p@h/db" -sql "select * from users where id = 1"
sqlcli "oracle://u:p@h/SVC" -sql "select sysdate from dual" -format json
sqlcli "mariadb://u:p@h/db" -f migrate.sql -continue
cat report.sql | sqlcli "postgres://u:p@h/db" -format csv -quiet > report.csv
```

| Flag | Meaning |
|---|---|
| `-sql "..."` | statements to run; `;` separates them, quotes and comments are respected |
| `-f FILE` | run a script (`-` = stdin) |
| `-format` | `auto` (default: table, or vertical when the table is wider than the terminal), `table`, `csv`, `tsv`, `json`, `vertical` |
| `-nopager` | don't page output; by default output taller than the terminal is shown page by page (Enter/Space: next page, j/Down: next line, Esc/q/Ctrl+Q: quit) |
| `-limit N` | fetch at most N rows per statement (0 = all) |
| `-continue` | keep going after an error |
| `-quiet` | no row counts / timings |
| `-timeout` | connect and query timeout, default `30s` |

Oracle PL/SQL blocks (`BEGIN ... END;`, `CREATE OR REPLACE PROCEDURE ...`) are kept whole and terminated by a line containing only `/`, same as sqlplus.

## TUI mode

Run with a connection and nothing to execute:

```bash
sqlcli "oracle://user:pass@host:1521/SVC"
```

```
┌ Tables 12/12 ───────┐╔ SQL ═══════════════════════════════════════╗
│/                    │║select * from orders where status = 'NEW'   ║
│CUSTOMERS            │║                                            ║
│ORDERS               │╚════════════════════════════════════════════╝
│PRODUCTS             │┌ Results (37 rows x 6 cols) ────────────────┐
│...                  ││ID  │CUSTOMER_ID│STATUS│AMOUNT│CREATED      │
│                     ││1001│7          │NEW   │120.5 │2026-10-18 … │
└─────────────────────┘└────────────────────────────────────────────┘
 OK 37 row(s) in 12ms      F1 help  F5 run  F2 tables  F3 history  F8 export  Tab focus  Esc cancel  Ctrl+Q quit
```

| Key | Action |
|---|---|
| `F1` | help popup with every shortcut |
| `F5` / `Ctrl+E` | run editor contents (or only the selected text) |
| `Ctrl+Enter` | run only the statement under the cursor (or the selected text) |
| `Ctrl+Shift+Left` / `Ctrl+Shift+Right` | narrow / widen the Tables sidebar |
| `Ctrl+Shift+Up` / `Ctrl+Shift+Down` | move the SQL / Results divider up / down |
| `Ctrl+Space` | open the autocomplete dropdown |
| `Esc` | cancel a running query, or close a popup |
| `F2` | show/hide the table sidebar |
| `F3` | history popup (`Enter` loads a statement into the editor) |
| `F8` | export the current result to CSV |
| `Ctrl+L` | clear the editor |
| `Tab` / `Shift+Tab` | cycle focus: tables → editor → results |
| `Ctrl+Q` | quit |

In the table sidebar: `/` filters, `Enter` runs `SELECT * FROM <table>` (first 100 rows), `d` describes columns, `i` inserts the name into the editor, `r` re-introspects tables and columns.
In the results grid: arrow keys move, `Enter` on a cell opens the full value (long text, multi-line).

Queries run in the background; the UI stays responsive. Results are capped at `-limit` rows (default 1000 in the TUI) and the status bar says when the result was truncated.
History is saved to `~/.sqlcli_history`.

### Autocomplete

On connect the status bar shows `introspecting schema/tables...` while the schema is read in the background. The table list fills the sidebar as soon as it arrives; columns (which can take longer on big schemas) are loaded after that and reach autocomplete whenever they are done. The result is cached in `./cache/<type>_<user>@<host>_<port>_<db>.json` (relative to the working directory, no password in it). On the next start the cache fills the sidebar and suggestions immediately, then gets refreshed.

While typing in the SQL editor a dropdown suggests tables (after `FROM`/`JOIN`/`INTO`/`UPDATE`), columns of the tables in the current statement, and keywords. After `alias.`, `table.` or `schema.` only that table's columns (or that schema's tables) are offered. `Up`/`Down` pick, `Tab`/`Enter` insert, `Esc` closes; `Enter` on a word that is already complete still inserts a newline. Nothing pops up inside strings or comments.

## Tests

```bash
go test ./...                                                   # unit tests (DSN parsing, statement splitting)
SQLCLI_TEST_DSN="postgres://u:p@localhost:5432/db" go test ./... # + TUI smoke test on a simulated terminal
```

Local throwaway databases for testing:

```bash
docker run -d --name pg    -e POSTGRES_PASSWORD=pass -e POSTGRES_DB=testdb -p 15432:5432 postgres:16-alpine
docker run -d --name maria -e MARIADB_ROOT_PASSWORD=pass -e MARIADB_DATABASE=testdb -p 13306:3306 mariadb:11
docker run -d --name ora   -e ORACLE_PASSWORD=pass -e APP_USER=test -e APP_USER_PASSWORD=pass -p 11521:1521 gvenzl/oracle-free:slim-faststart
```

## Layout

```
main.go              flags, quick mode, dispatch to TUI
internal/conn        DSN parsing + auto-detect, driver DSN building, Open()
internal/query       statement splitter, Exec/ExecScript, value formatting, ListTables/DescribeTable
internal/tunnel      socks4/socks5/http proxy + ssh tunnel dialer chain
internal/format      table / csv / tsv / json / vertical renderers
internal/schema      table/column introspection, ./cache files, autocomplete suggestions
internal/tui         tview-based interactive UI
```
