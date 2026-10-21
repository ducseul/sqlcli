// Package tunnel builds a chain of proxies (SOCKS4/4a, SOCKS5, HTTP CONNECT)
// and SSH tunnels that database connections are dialed through. Hops are
// applied in the order they were declared: each hop is reached through the
// hops before it, and the database is reached through the last one.
package tunnel

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

// DialFunc dials a network address. Its DialContext method lets it be used
// wherever a dialer interface is expected (go-ora).
type DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

func (f DialFunc) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	return f(ctx, network, addr)
}

// Prompt reads a secret from the user (hidden input). It is nil when there is
// no terminal, in which case SSH passwords and key passphrases must be given
// up front.
var Prompt func(prompt string) (string, error)

// Kind is the protocol of a hop.
type Kind string

const (
	SOCKS4 Kind = "socks4" // falls back to SOCKS4a for host names
	SOCKS5 Kind = "socks5"
	HTTP   Kind = "http"
	SSH    Kind = "ssh"
)

// Hop is one step of the chain.
type Hop struct {
	Kind     Kind
	Host     string
	Port     int
	User     string
	Password string
	KeyFile  string // ssh only: private key to try before the defaults
}

func (h Hop) addr() string { return net.JoinHostPort(h.Host, strconv.Itoa(h.Port)) }

func (h Hop) String() string {
	u := ""
	if h.User != "" {
		u = h.User + "@"
	}
	return fmt.Sprintf("%s://%s%s", h.Kind, u, h.addr())
}

// ParseProxy parses a -proxy value:
//
//	socks5://[user:pass@]host[:port]   (also socks5h, sock5, socks)
//	socks4://[user@]host[:port]        (also socks4a, sock4)
//	http://[user:pass@]host[:port]
//	socks5:host:port                   (scheme without //)
func ParseProxy(s string) (Hop, error) {
	s = strings.TrimSpace(s)
	scheme, rest, ok := strings.Cut(s, "://")
	if !ok {
		scheme, rest, ok = strings.Cut(s, ":")
		if !ok {
			return Hop{}, fmt.Errorf("invalid proxy %q: expected scheme:host:port (socks4, socks5 or http)", s)
		}
	}
	var h Hop
	switch strings.ToLower(scheme) {
	case "socks5", "socks5h", "sock5", "socks":
		h.Kind, h.Port = SOCKS5, 1080
	case "socks4", "socks4a", "sock4", "sock4a":
		h.Kind, h.Port = SOCKS4, 1080
	case "http":
		h.Kind, h.Port = HTTP, 8080
	default:
		return Hop{}, fmt.Errorf("unsupported proxy type %q (supported: socks4, socks5, http)", scheme)
	}
	if err := h.parseTarget(rest); err != nil {
		return Hop{}, fmt.Errorf("invalid proxy %q: %w", s, err)
	}
	return h, nil
}

// ParseSSH parses a -tun value: [ssh://][user[:pass]@]host[:port].
// The user defaults to the current OS user, the port to 22.
func ParseSSH(s string) (Hop, error) {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "://"); i >= 0 {
		if !strings.EqualFold(s[:i], "ssh") {
			return Hop{}, fmt.Errorf("invalid tunnel %q: only ssh is supported", s)
		}
		s = s[i+3:]
	}
	h := Hop{Kind: SSH, Port: 22}
	if err := h.parseTarget(s); err != nil {
		return Hop{}, fmt.Errorf("invalid tunnel %q: %w", s, err)
	}
	if h.User == "" {
		if u, err := user.Current(); err == nil {
			h.User = u.Username
			// Windows reports DOMAIN\user.
			if i := strings.LastIndex(h.User, `\`); i >= 0 {
				h.User = h.User[i+1:]
			}
		}
	}
	return h, nil
}

// parseTarget fills user, password, host and port from [user[:pass]@]host[:port].
func (h *Hop) parseTarget(s string) error {
	s = strings.TrimSuffix(s, "/")
	u, err := url.Parse("//" + s)
	if err != nil {
		return err
	}
	if u.Hostname() == "" {
		return fmt.Errorf("missing host")
	}
	h.Host = u.Hostname()
	if p := u.Port(); p != "" {
		if h.Port, err = strconv.Atoi(p); err != nil {
			return fmt.Errorf("invalid port %q", p)
		}
	}
	if u.User != nil {
		h.User = u.User.Username()
		h.Password, _ = u.User.Password()
	}
	return nil
}

// Chain is an ordered list of hops.
type Chain []*Hop

func (c Chain) String() string {
	parts := make([]string, len(c))
	for i, h := range c {
		parts[i] = h.String()
	}
	return strings.Join(parts, " -> ")
}

// Dialer returns a dial function that goes through every hop in order. SSH
// sessions are opened on first use and shared by all connections after that.
func (c Chain) Dialer(timeout time.Duration) DialFunc {
	nd := &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}
	d := DialFunc(nd.DialContext)
	for _, h := range c {
		prev := d
		switch h.Kind {
		case SOCKS4:
			d = proxyDialer(prev, h, socks4Connect)
		case SOCKS5:
			d = proxyDialer(prev, h, socks5Connect)
		case HTTP:
			d = proxyDialer(prev, h, httpConnect)
		case SSH:
			d = (&sshTunnel{hop: h, prev: prev, timeout: timeout}).dial
		}
	}
	return d
}

type handshake func(conn net.Conn, h *Hop, target string) (net.Conn, error)

func proxyDialer(prev DialFunc, h *Hop, connect handshake) DialFunc {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := prev(ctx, "tcp", h.addr())
		if err != nil {
			return nil, fmt.Errorf("%s: %w", h, err)
		}
		if dl, ok := ctx.Deadline(); ok {
			conn.SetDeadline(dl)
		}
		c, err := connect(conn, h, addr)
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("%s: %w", h, err)
		}
		conn.SetDeadline(time.Time{})
		return c, nil
	}
}

func splitTarget(target string) (string, uint16, error) {
	host, ps, err := net.SplitHostPort(target)
	if err != nil {
		return "", 0, err
	}
	p, err := strconv.ParseUint(ps, 10, 16)
	if err != nil {
		return "", 0, fmt.Errorf("invalid port %q", ps)
	}
	return host, uint16(p), nil
}

func socks4Connect(conn net.Conn, h *Hop, target string) (net.Conn, error) {
	host, port, err := splitTarget(target)
	if err != nil {
		return nil, err
	}
	req := []byte{4, 1, 0, 0}
	binary.BigEndian.PutUint16(req[2:], port)
	ip := net.ParseIP(host).To4()
	if ip == nil {
		// SOCKS4a: 0.0.0.x tells the proxy to resolve the name that follows the user id.
		ip = net.IPv4(0, 0, 0, 1).To4()
	}
	req = append(req, ip...)
	req = append(req, h.User...)
	req = append(req, 0)
	if net.ParseIP(host).To4() == nil {
		req = append(req, host...)
		req = append(req, 0)
	}
	if _, err := conn.Write(req); err != nil {
		return nil, err
	}
	resp := make([]byte, 8)
	if _, err := io.ReadFull(conn, resp); err != nil {
		return nil, fmt.Errorf("socks4 handshake: %w", err)
	}
	if resp[1] != 0x5A {
		return nil, fmt.Errorf("socks4 connect to %s rejected (code 0x%02X)", target, resp[1])
	}
	return conn, nil
}

func socks5Connect(conn net.Conn, h *Hop, target string) (net.Conn, error) {
	host, port, err := splitTarget(target)
	if err != nil {
		return nil, err
	}
	methods := []byte{0}
	if h.User != "" {
		methods = append(methods, 2)
	}
	if _, err := conn.Write(append([]byte{5, byte(len(methods))}, methods...)); err != nil {
		return nil, err
	}
	resp := make([]byte, 2)
	if _, err := io.ReadFull(conn, resp); err != nil {
		return nil, fmt.Errorf("socks5 handshake: %w", err)
	}
	if resp[0] != 5 {
		return nil, fmt.Errorf("not a socks5 proxy")
	}
	switch resp[1] {
	case 0:
	case 2:
		if len(h.User) > 255 || len(h.Password) > 255 {
			return nil, fmt.Errorf("socks5 username/password too long")
		}
		auth := []byte{1, byte(len(h.User))}
		auth = append(auth, h.User...)
		auth = append(auth, byte(len(h.Password)))
		auth = append(auth, h.Password...)
		if _, err := conn.Write(auth); err != nil {
			return nil, err
		}
		if _, err := io.ReadFull(conn, resp); err != nil {
			return nil, fmt.Errorf("socks5 auth: %w", err)
		}
		if resp[1] != 0 {
			return nil, fmt.Errorf("socks5 authentication failed")
		}
	default:
		return nil, fmt.Errorf("socks5 proxy requires an unsupported auth method (give user:pass@ if it needs a password)")
	}

	req := []byte{5, 1, 0}
	if ip := net.ParseIP(host); ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			req = append(append(req, 1), ip4...)
		} else {
			req = append(append(req, 4), ip.To16()...)
		}
	} else {
		if len(host) > 255 {
			return nil, fmt.Errorf("host name too long")
		}
		req = append(append(req, 3, byte(len(host))), host...)
	}
	req = binary.BigEndian.AppendUint16(req, port)
	if _, err := conn.Write(req); err != nil {
		return nil, err
	}
	hdr := make([]byte, 4)
	if _, err := io.ReadFull(conn, hdr); err != nil {
		return nil, fmt.Errorf("socks5 connect: %w", err)
	}
	if hdr[1] != 0 {
		return nil, fmt.Errorf("socks5 connect to %s failed: %s", target, socks5Reply(hdr[1]))
	}
	// Skip the bound address.
	var n int
	switch hdr[3] {
	case 1:
		n = 4
	case 4:
		n = 16
	case 3:
		l := make([]byte, 1)
		if _, err := io.ReadFull(conn, l); err != nil {
			return nil, err
		}
		n = int(l[0])
	default:
		return nil, fmt.Errorf("socks5: bad address type %d", hdr[3])
	}
	if _, err := io.ReadFull(conn, make([]byte, n+2)); err != nil {
		return nil, err
	}
	return conn, nil
}

func socks5Reply(code byte) string {
	switch code {
	case 1:
		return "general failure"
	case 2:
		return "not allowed by ruleset"
	case 3:
		return "network unreachable"
	case 4:
		return "host unreachable"
	case 5:
		return "connection refused"
	case 6:
		return "TTL expired"
	case 7:
		return "command not supported"
	case 8:
		return "address type not supported"
	}
	return fmt.Sprintf("code %d", code)
}

func httpConnect(conn net.Conn, h *Hop, target string) (net.Conn, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n", target, target)
	if h.User != "" {
		cred := base64.StdEncoding.EncodeToString([]byte(h.User + ":" + h.Password))
		fmt.Fprintf(&b, "Proxy-Authorization: Basic %s\r\n", cred)
	}
	b.WriteString("\r\n")
	if _, err := io.WriteString(conn, b.String()); err != nil {
		return nil, err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		return nil, fmt.Errorf("http CONNECT: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http CONNECT to %s: %s", target, resp.Status)
	}
	// Servers that speak first (MySQL) may already have sent bytes that the
	// reader buffered along with the response headers.
	if br.Buffered() > 0 {
		return &bufferedConn{Conn: conn, r: br}, nil
	}
	return conn, nil
}

type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// sshTunnel holds one SSH session that all connections are forwarded through.
type sshTunnel struct {
	hop     *Hop
	prev    DialFunc
	timeout time.Duration

	mu        sync.Mutex
	client    *ssh.Client
	triedAuth bool // a password was sent, so a handshake failure means bad credentials
}

func (t *sshTunnel) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	client, err := t.get(ctx)
	if err != nil {
		return nil, err
	}
	conn, err := client.DialContext(ctx, network, addr)
	if err != nil {
		return nil, fmt.Errorf("%s: forward to %s: %w", t.hop, addr, err)
	}
	return withDeadlines(conn), nil
}

// withDeadlines bridges an SSH channel, which rejects SetDeadline, through a
// net.Pipe, which supports it (go-ora and the proxy handshakes rely on it).
func withDeadlines(ch net.Conn) net.Conn {
	local, remote := net.Pipe()
	go func() {
		io.Copy(remote, ch)
		remote.Close()
	}()
	go func() {
		io.Copy(ch, remote)
		ch.Close()
	}()
	return &pipeConn{Conn: local, ch: ch}
}

type pipeConn struct {
	net.Conn
	ch net.Conn
}

func (c *pipeConn) Close() error {
	c.ch.Close()
	return c.Conn.Close()
}

func (c *pipeConn) LocalAddr() net.Addr  { return c.ch.LocalAddr() }
func (c *pipeConn) RemoteAddr() net.Addr { return c.ch.RemoteAddr() }

func (t *sshTunnel) get(ctx context.Context) (*ssh.Client, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.client != nil {
		return t.client, nil
	}
	client, err := t.connect(ctx)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", t.hop, err)
	}
	t.client = client
	go t.keepalive(client)
	return client, nil
}

// keepalive pings the server so idle sessions (e.g. a TUI left open) survive
// NAT/firewall timeouts, and drops the client once the session dies so the
// next dial reconnects.
func (t *sshTunnel) keepalive(c *ssh.Client) {
	done := make(chan struct{})
	go func() {
		c.Wait()
		close(done)
	}()
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-done:
			t.mu.Lock()
			if t.client == c {
				t.client = nil
			}
			t.mu.Unlock()
			return
		case <-tick.C:
			if _, _, err := c.SendRequest("keepalive@openssh.com", true, nil); err != nil {
				c.Close()
			}
		}
	}
}

func (t *sshTunnel) connect(ctx context.Context) (*ssh.Client, error) {
	h := t.hop
	cfg := &ssh.ClientConfig{
		User:            h.User,
		Auth:            t.authMethods(),
		HostKeyCallback: hostKeyCallback(),
		Timeout:         t.timeout,
	}
	conn, err := t.prev(ctx, "tcp", h.addr())
	if err != nil {
		return nil, err
	}
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	} else if t.timeout > 0 {
		conn.SetDeadline(time.Now().Add(t.timeout))
	}
	c, chans, reqs, err := ssh.NewClientConn(conn, h.addr(), cfg)
	if err != nil {
		conn.Close()
		// x/crypto reports a rejected password as an odd protocol error
		// when the server then refuses keyboard-interactive too.
		if t.triedAuth || strings.Contains(err.Error(), "unable to authenticate") {
			return nil, fmt.Errorf("authentication failed for %s@%s (wrong password, or key not accepted)", h.User, h.Host)
		}
		return nil, err
	}
	conn.SetDeadline(time.Time{})
	return ssh.NewClient(c, chans, reqs), nil
}

func (t *sshTunnel) authMethods() []ssh.AuthMethod {
	h := t.hop
	var methods []ssh.AuthMethod
	var signers []ssh.Signer
	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		if ac, err := net.Dial("unix", sock); err == nil {
			if s, err := agent.NewClient(ac).Signers(); err == nil {
				signers = append(signers, s...)
			}
		}
	}
	if h.KeyFile != "" {
		if s, err := loadKey(h.KeyFile, true); err != nil {
			fmt.Fprintf(os.Stderr, "warning: ssh key %s: %v\n", h.KeyFile, err)
		} else {
			signers = append(signers, s)
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		for _, name := range []string{"id_ed25519", "id_ecdsa", "id_rsa"} {
			p := filepath.Join(home, ".ssh", name)
			if p == h.KeyFile {
				continue
			}
			// Default keys are tried silently; encrypted ones are skipped.
			if s, err := loadKey(p, false); err == nil {
				signers = append(signers, s)
			}
		}
	}
	if len(signers) > 0 {
		methods = append(methods, ssh.PublicKeys(signers...))
	}

	// The password is asked at most once and reused for reconnects.
	var once sync.Once
	var pwErr error
	password := func() (string, error) {
		once.Do(func() {
			if h.Password != "" {
				return
			}
			if Prompt == nil {
				pwErr = errors.New("no ssh password given and no terminal to ask for one")
				return
			}
			h.Password, pwErr = Prompt(fmt.Sprintf("Password for %s@%s: ", h.User, h.Host))
		})
		t.triedAuth = pwErr == nil
		return h.Password, pwErr
	}
	methods = append(methods,
		ssh.PasswordCallback(password),
		ssh.KeyboardInteractive(func(_, _ string, questions []string, echos []bool) ([]string, error) {
			answers := make([]string, len(questions))
			for i := range questions {
				pw, err := password()
				if err != nil {
					return nil, err
				}
				answers[i] = pw
			}
			return answers, nil
		}),
	)
	return methods
}

func loadKey(path string, ask bool) (ssh.Signer, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	s, err := ssh.ParsePrivateKey(b)
	var missing *ssh.PassphraseMissingError
	if errors.As(err, &missing) && ask && Prompt != nil {
		pass, perr := Prompt(fmt.Sprintf("Passphrase for %s: ", path))
		if perr != nil {
			return nil, perr
		}
		return ssh.ParsePrivateKeyWithPassphrase(b, []byte(pass))
	}
	return s, err
}

var warnedHosts sync.Map

// hostKeyCallback checks ~/.ssh/known_hosts. A changed key is refused; a host
// that is not listed is accepted with a warning showing its fingerprint.
func hostKeyCallback() ssh.HostKeyCallback {
	var known ssh.HostKeyCallback
	if home, err := os.UserHomeDir(); err == nil {
		if cb, err := knownhosts.New(filepath.Join(home, ".ssh", "known_hosts")); err == nil {
			known = cb
		}
	}
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		if known != nil {
			err := known(hostname, remote, key)
			if err == nil {
				return nil
			}
			var ke *knownhosts.KeyError
			if !errors.As(err, &ke) {
				return err
			}
			if len(ke.Want) > 0 {
				return fmt.Errorf("host key for %s does not match line %d of %s (possible MITM)",
					hostname, ke.Want[0].Line, ke.Want[0].Filename)
			}
		}
		fp := ssh.FingerprintSHA256(key)
		if _, seen := warnedHosts.LoadOrStore(hostname+" "+fp, true); !seen {
			fmt.Fprintf(os.Stderr, "warning: %s is not in ~/.ssh/known_hosts, accepting %s key %s\n",
				hostname, key.Type(), fp)
		}
		return nil
	}
}
