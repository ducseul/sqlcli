package conn

import (
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	cases := []struct {
		in       string
		typ      DBType
		host     string
		port     int
		user     string
		pass     string
		database string
	}{
		{"postgres://u:p@h:5433/d?sslmode=disable", Postgres, "h", 5433, "u", "p", "d"},
		{"postgresql://u:p@h/d", Postgres, "h", 5432, "u", "p", "d"},
		{"mysql://u:p@h:3307/d", MariaDB, "h", 3307, "u", "p", "d"},
		{"mariadb://u:p@h/d", MariaDB, "h", 3306, "u", "p", "d"},
		{"oracle://u:p@h:1522/svc", Oracle, "h", 1522, "u", "p", "svc"},
		{"u:p@tcp(h:3306)/d?charset=utf8", MariaDB, "h", 3306, "u", "p", "d"},
		{"u/p@h:1521/svc", Oracle, "h", 1521, "u", "p", "svc"},
		{"host=h port=5432 user=u password=p dbname=d", Postgres, "h", 5432, "u", "p", "d"},
	}
	for _, tc := range cases {
		c, err := Parse(tc.in)
		if err != nil {
			t.Fatalf("%s: %v", tc.in, err)
		}
		if c.Type != tc.typ || c.Host != tc.host || c.Port != tc.port || c.User != tc.user || c.Password != tc.pass || c.Database != tc.database {
			t.Errorf("%s: got %+v", tc.in, c)
		}
	}
	if _, err := Parse("foo://x"); err == nil {
		t.Error("expected error for unknown scheme")
	}
}

func TestDriverDSN(t *testing.T) {
	c, _ := Parse("mariadb://u:p@h/d")
	if got := c.DriverDSN(); !strings.HasPrefix(got, "u:p@tcp(h:3306)/d?") {
		t.Errorf("unexpected mariadb dsn %q", got)
	}
	c, _ = Parse("oracle://u:p@h/svc")
	if got := c.DriverDSN(); !strings.HasPrefix(got, "oracle://u:p@h:1521/svc") {
		t.Errorf("unexpected oracle dsn %q", got)
	}
	c, _ = Parse("postgres://u:p@h/d")
	if got := c.DriverDSN(); !strings.HasPrefix(got, "postgres://u:p@h:5432/d?") {
		t.Errorf("unexpected postgres dsn %q", got)
	}
}
