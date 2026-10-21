// Package conn handles connection configuration: parsing a DSN / URL with
// automatic driver detection, or building one from discrete parameters.
package conn

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
	go_ora "github.com/sijms/go-ora/v2"

	"sqlcli/internal/tunnel"
)

// DBType is a supported database engine.
type DBType string

const (
	Postgres DBType = "postgres"
	MariaDB  DBType = "mariadb"
	Oracle   DBType = "oracle"
)

// Config holds everything needed to open a connection.
type Config struct {
	Type     DBType
	Host     string
	Port     int
	User     string
	Password string
	Database string // database name (pg/mariadb) or service name (oracle)
	Params   map[string]string
	Timeout  time.Duration
	Via      tunnel.Chain // proxies / ssh tunnels to connect through, in order
}

// DefaultPort returns the conventional port for a database type.
func DefaultPort(t DBType) int {
	switch t {
	case Postgres:
		return 5432
	case MariaDB:
		return 3306
	case Oracle:
		return 1521
	}
	return 0
}

// NormalizeType maps user-provided aliases to a canonical DBType.
func NormalizeType(s string) (DBType, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "postgres", "postgresql", "pg", "pgsql":
		return Postgres, nil
	case "mariadb", "maria", "mysql", "my":
		return MariaDB, nil
	case "oracle", "ora", "oci", "oracledb":
		return Oracle, nil
	case "":
		return "", fmt.Errorf("database type is required (postgres | mariadb | oracle)")
	}
	return "", fmt.Errorf("unsupported database type %q (supported: postgres, mariadb, oracle)", s)
}

// Parse parses a connection string and auto-detects the database type.
//
// Accepted forms:
//
//	postgres://user:pass@host:5432/dbname?sslmode=disable
//	postgresql://...
//	mysql://user:pass@host:3306/dbname
//	mariadb://user:pass@host:3306/dbname
//	oracle://user:pass@host:1521/service_name
//	user:pass@tcp(host:3306)/dbname          (go-sql-driver style, mariadb)
//	user/pass@host:1521/service              (sqlplus style, oracle)
//	host=... port=... user=... dbname=...     (libpq keyword style, postgres)
//	jdbc:oracle:thin:@host:1521:SID           (JDBC / ojdbc forms, see parseJDBC)
//	jdbc:postgresql://host:5432/db, jdbc:mysql://..., jdbc:mariadb://...
func Parse(dsn string) (*Config, error) {
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		return nil, fmt.Errorf("empty connection string")
	}

	// JDBC form.
	if strings.HasPrefix(strings.ToLower(dsn), "jdbc:") {
		return parseJDBC(dsn[len("jdbc:"):])
	}

	// URL form with scheme.
	if i := strings.Index(dsn, "://"); i > 0 {
		scheme := strings.ToLower(dsn[:i])
		t, err := NormalizeType(scheme)
		if err != nil {
			return nil, fmt.Errorf("unknown URL scheme %q: %w", scheme, err)
		}
		return parseURL(t, dsn)
	}

	// libpq keyword=value form.
	if strings.Contains(dsn, "=") && (strings.Contains(dsn, "host=") || strings.Contains(dsn, "dbname=")) {
		return parseKeyValue(dsn)
	}

	// go-sql-driver/mysql form: user:pass@tcp(host:port)/db
	if strings.Contains(dsn, "@tcp(") {
		return parseMySQLNative(dsn)
	}

	// sqlplus form: user/pass@host:port/service
	if strings.Contains(dsn, "/") && strings.Contains(dsn, "@") && strings.Index(dsn, "/") < strings.Index(dsn, "@") {
		return parseSQLPlus(dsn)
	}

	return nil, fmt.Errorf("cannot detect database type from connection string; use a URL like postgres://, mariadb:// or oracle://")
}

func parseURL(t DBType, raw string) (*Config, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid URL: %w", err)
	}
	c := &Config{Type: t, Params: map[string]string{}}
	c.Host = u.Hostname()
	if p := u.Port(); p != "" {
		c.Port, err = strconv.Atoi(p)
		if err != nil {
			return nil, fmt.Errorf("invalid port %q", p)
		}
	}
	if u.User != nil {
		c.User = u.User.Username()
		c.Password, _ = u.User.Password()
	}
	c.Database = strings.TrimPrefix(u.Path, "/")
	for k, v := range u.Query() {
		if len(v) > 0 {
			c.Params[k] = v[0]
		}
	}
	return c.fill(), nil
}

// parseJDBC parses a JDBC URL with the "jdbc:" prefix already removed.
//
//	oracle:thin:@host:port:SID
//	oracle:thin:@host:port/service
//	oracle:thin:@//host:port/service
//	oracle:thin:user/pass@host:port:SID
//	oracle:thin:@(DESCRIPTION=...)             (TNS descriptor)
//	postgresql://host:port/db?user=u&password=p
//	mysql://host:port/db, mariadb://host:port/db
func parseJDBC(rest string) (*Config, error) {
	lower := strings.ToLower(rest)
	if !strings.HasPrefix(lower, "oracle:") {
		scheme, _, ok := strings.Cut(rest, "://")
		if !ok {
			return nil, fmt.Errorf("unsupported JDBC URL %q", "jdbc:"+rest)
		}
		t, err := NormalizeType(scheme)
		if err != nil {
			return nil, fmt.Errorf("unsupported JDBC subprotocol %q: %w", scheme, err)
		}
		c, err := parseURL(t, rest)
		if err != nil {
			return nil, err
		}
		// JDBC carries credentials as query parameters.
		for _, k := range []string{"user", "password"} {
			if v, ok := c.Params[k]; ok {
				if k == "user" {
					c.User = v
				} else {
					c.Password = v
				}
				delete(c.Params, k)
			}
		}
		return c, nil
	}

	// Oracle: oracle:<driver>:[user/pass]@<target>
	c := &Config{Type: Oracle, Params: map[string]string{}}
	at := strings.LastIndex(rest, "@")
	if at < 0 {
		return nil, fmt.Errorf("invalid Oracle JDBC URL: missing '@'")
	}
	prefix, target := rest[:at], strings.TrimSpace(rest[at+1:])
	// prefix is "oracle:thin" or "oracle:thin:user/pass".
	if parts := strings.SplitN(prefix, ":", 3); len(parts) == 3 {
		c.User, c.Password, _ = strings.Cut(parts[2], "/")
	}
	if strings.HasPrefix(target, "(") {
		c.Params["connStr"] = target
		return c.fill(), nil
	}
	target = strings.TrimPrefix(target, "//")
	if addr, svc, ok := strings.Cut(target, "/"); ok {
		// host:port/service
		if err := c.setHostPort(addr); err != nil {
			return nil, err
		}
		c.Database = svc
		return c.fill(), nil
	}
	// host:port:SID or host:SID
	parts := strings.Split(target, ":")
	switch len(parts) {
	case 3:
		c.Host = parts[0]
		p, err := strconv.Atoi(parts[1])
		if err != nil {
			return nil, fmt.Errorf("invalid port %q", parts[1])
		}
		c.Port = p
		c.Params["SID"] = parts[2]
	case 2:
		c.Host = parts[0]
		c.Params["SID"] = parts[1]
	default:
		return nil, fmt.Errorf("invalid Oracle JDBC target %q", target)
	}
	return c.fill(), nil
}

func parseKeyValue(dsn string) (*Config, error) {
	c := &Config{Type: Postgres, Params: map[string]string{}}
	for _, kv := range strings.Fields(dsn) {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return nil, fmt.Errorf("invalid key=value pair %q", kv)
		}
		v = strings.Trim(v, "'\"")
		switch strings.ToLower(k) {
		case "host":
			c.Host = v
		case "port":
			p, err := strconv.Atoi(v)
			if err != nil {
				return nil, fmt.Errorf("invalid port %q", v)
			}
			c.Port = p
		case "user":
			c.User = v
		case "password":
			c.Password = v
		case "dbname":
			c.Database = v
		default:
			c.Params[k] = v
		}
	}
	return c.fill(), nil
}

func parseMySQLNative(dsn string) (*Config, error) {
	c := &Config{Type: MariaDB, Params: map[string]string{}}
	cred, rest, _ := strings.Cut(dsn, "@tcp(")
	c.User, c.Password, _ = strings.Cut(cred, ":")
	addr, rest, ok := strings.Cut(rest, ")")
	if !ok {
		return nil, fmt.Errorf("invalid mysql DSN: missing ')'")
	}
	if err := c.setHostPort(addr); err != nil {
		return nil, err
	}
	rest = strings.TrimPrefix(rest, "/")
	db, q, _ := strings.Cut(rest, "?")
	c.Database = db
	if q != "" {
		vals, err := url.ParseQuery(q)
		if err != nil {
			return nil, err
		}
		for k, v := range vals {
			if len(v) > 0 {
				c.Params[k] = v[0]
			}
		}
	}
	return c.fill(), nil
}

func parseSQLPlus(dsn string) (*Config, error) {
	c := &Config{Type: Oracle, Params: map[string]string{}}
	cred, rest, _ := strings.Cut(dsn, "@")
	c.User, c.Password, _ = strings.Cut(cred, "/")
	addr, svc, _ := strings.Cut(rest, "/")
	if err := c.setHostPort(addr); err != nil {
		return nil, err
	}
	c.Database = svc
	return c.fill(), nil
}

func (c *Config) setHostPort(addr string) error {
	if !strings.Contains(addr, ":") {
		c.Host = addr
		return nil
	}
	h, p, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid host:port %q", addr)
	}
	c.Host = h
	c.Port, err = strconv.Atoi(p)
	if err != nil {
		return fmt.Errorf("invalid port %q", p)
	}
	return nil
}

// fill applies defaults.
func (c *Config) fill() *Config {
	if c.Host == "" {
		c.Host = "localhost"
	}
	if c.Port == 0 {
		c.Port = DefaultPort(c.Type)
	}
	if c.Params == nil {
		c.Params = map[string]string{}
	}
	if c.Timeout == 0 {
		c.Timeout = 10 * time.Second
	}
	return c
}

// Validate checks that the config has the minimum required fields.
func (c *Config) Validate() error {
	if c.Type == "" {
		return fmt.Errorf("database type is required")
	}
	c.fill()
	if c.User == "" {
		return fmt.Errorf("username is required")
	}
	if c.Type == Oracle && c.Database == "" && c.Params["SID"] == "" && c.Params["connStr"] == "" {
		return fmt.Errorf("oracle requires a service name (-db) or SID")
	}
	return nil
}

// DriverName returns the database/sql driver name for this config.
func (c *Config) DriverName() string {
	switch c.Type {
	case Postgres:
		return "pgx"
	case MariaDB:
		return "mysql"
	case Oracle:
		return "oracle"
	}
	return ""
}

// DriverDSN builds the driver-specific DSN string.
func (c *Config) DriverDSN() string {
	c.fill()
	q := url.Values{}
	for k, v := range c.Params {
		q.Set(k, v)
	}
	hostPort := net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
	switch c.Type {
	case Postgres:
		if _, ok := c.Params["connect_timeout"]; !ok {
			q.Set("connect_timeout", strconv.Itoa(int(c.Timeout.Seconds())))
		}
		u := url.URL{
			Scheme:   "postgres",
			User:     url.UserPassword(c.User, c.Password),
			Host:     hostPort,
			Path:     "/" + c.Database,
			RawQuery: q.Encode(),
		}
		return u.String()
	case MariaDB:
		if _, ok := c.Params["parseTime"]; !ok {
			q.Set("parseTime", "true")
		}
		if _, ok := c.Params["timeout"]; !ok {
			q.Set("timeout", c.Timeout.String())
		}
		if _, ok := c.Params["multiStatements"]; !ok {
			q.Set("multiStatements", "true")
		}
		return fmt.Sprintf("%s:%s@tcp(%s)/%s?%s", c.User, c.Password, hostPort, c.Database, q.Encode())
	case Oracle:
		if _, ok := c.Params["TIMEOUT"]; !ok {
			q.Set("TIMEOUT", strconv.Itoa(int(c.Timeout.Seconds())))
		}
		u := url.URL{
			Scheme:   "oracle",
			User:     url.UserPassword(c.User, c.Password),
			Host:     hostPort,
			Path:     "/" + c.Database,
			RawQuery: q.Encode(),
		}
		return u.String()
	}
	return ""
}

// Redacted returns a human readable description without the password.
func (c *Config) Redacted() string {
	if c.Type == Oracle && c.Database == "" {
		if sid := c.Params["SID"]; sid != "" {
			return fmt.Sprintf("%s://%s@%s:%d (SID=%s)%s", c.Type, c.User, c.Host, c.Port, sid, c.via())
		}
	}
	return fmt.Sprintf("%s://%s@%s:%d/%s%s", c.Type, c.User, c.Host, c.Port, c.Database, c.via())
}

func (c *Config) via() string {
	if len(c.Via) == 0 {
		return ""
	}
	return " via " + c.Via.String()
}

// Open opens and pings the database.
func (c *Config) Open() (*sql.DB, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	db, err := c.openDB()
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	db.SetMaxOpenConns(2)
	db.SetConnMaxIdleTime(5 * time.Minute)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("connect to %s: %w", c.Redacted(), err)
	}
	return db, nil
}

// openDB opens the pool, routing connections through c.Via when set.
func (c *Config) openDB() (*sql.DB, error) {
	if len(c.Via) == 0 {
		return sql.Open(c.DriverName(), c.DriverDSN())
	}
	dial := c.Via.Dialer(c.Timeout)
	switch c.Type {
	case Postgres:
		pc, err := pgx.ParseConfig(c.DriverDSN())
		if err != nil {
			return nil, err
		}
		pc.DialFunc = pgconn.DialFunc(dial)
		// Leave name resolution to the far end of the tunnel.
		pc.LookupFunc = func(_ context.Context, host string) ([]string, error) { return []string{host}, nil }
		return stdlib.OpenDB(*pc), nil
	case MariaDB:
		mc, err := mysql.ParseDSN(c.DriverDSN())
		if err != nil {
			return nil, err
		}
		mc.DialFunc = dial
		connector, err := mysql.NewConnector(mc)
		if err != nil {
			return nil, err
		}
		return sql.OpenDB(connector), nil
	case Oracle:
		connector := go_ora.NewConnector(c.DriverDSN())
		connector.(*go_ora.OracleConnector).Dialer(dial)
		return sql.OpenDB(connector), nil
	}
	return nil, fmt.Errorf("unsupported database type %q", c.Type)
}
