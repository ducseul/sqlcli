package conn

import (
	"context"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"sqlcli/internal/tunnel"
)

// Traffic counts the bytes sent to / received from the database over every
// connection of the pool, as seen on the wire (after TLS, before tunnels).
type Traffic struct {
	sent, recv atomic.Int64
	opened     atomic.Int64 // connections dialed
	active     atomic.Int64 // connections currently open
	since      time.Time

	mu     sync.Mutex
	local  string // addresses of the last dialed connection
	remote string
}

// TrafficStats is a snapshot of Traffic.
type TrafficStats struct {
	Sent, Received int64
	Opened, Active int64
	Since          time.Time
	LocalAddr      string
	RemoteAddr     string
}

func newTraffic() *Traffic { return &Traffic{since: time.Now()} }

// Stats returns the current counters.
func (t *Traffic) Stats() TrafficStats {
	t.mu.Lock()
	defer t.mu.Unlock()
	return TrafficStats{
		Sent: t.sent.Load(), Received: t.recv.Load(),
		Opened: t.opened.Load(), Active: t.active.Load(),
		Since: t.since, LocalAddr: t.local, RemoteAddr: t.remote,
	}
}

// wrap makes dial count the traffic of every connection it returns.
func (t *Traffic) wrap(dial tunnel.DialFunc) tunnel.DialFunc {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := dial(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		t.opened.Add(1)
		t.active.Add(1)
		t.mu.Lock()
		if a := c.LocalAddr(); a != nil {
			t.local = a.String()
		}
		if a := c.RemoteAddr(); a != nil {
			t.remote = a.String()
		}
		t.mu.Unlock()
		return &countingConn{Conn: c, t: t}, nil
	}
}

type countingConn struct {
	net.Conn
	t      *Traffic
	closed atomic.Bool
}

func (c *countingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.t.recv.Add(int64(n))
	return n, err
}

func (c *countingConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.t.sent.Add(int64(n))
	return n, err
}

func (c *countingConn) Close() error {
	if c.closed.CompareAndSwap(false, true) {
		c.t.active.Add(-1)
	}
	return c.Conn.Close()
}

// Traffic returns the byte counters of the pool opened by Open (nil before).
func (c *Config) Traffic() *Traffic { return c.traffic }

// DialTCP opens (and closes) one raw connection to the server through the
// same proxies / tunnels as the pool, and reports how long that took. It is
// a network-level ping that does not involve the database protocol.
func (c *Config) DialTCP(ctx context.Context) (time.Duration, error) {
	dial := c.dial
	if dial == nil {
		dial = c.Via.Dialer(c.Timeout)
	}
	start := time.Now()
	nc, err := dial(ctx, "tcp", net.JoinHostPort(c.Host, strconv.Itoa(c.Port)))
	d := time.Since(start)
	if err != nil {
		return d, err
	}
	nc.Close()
	return d, nil
}
