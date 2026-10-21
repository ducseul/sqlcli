package tunnel

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestParseProxy(t *testing.T) {
	cases := []struct {
		in   string
		want Hop
	}{
		{"sock5:10.14.5.11:8443", Hop{Kind: SOCKS5, Host: "10.14.5.11", Port: 8443}},
		{"socks5://u:p@proxy", Hop{Kind: SOCKS5, Host: "proxy", Port: 1080, User: "u", Password: "p"}},
		{"socks4:h:1081", Hop{Kind: SOCKS4, Host: "h", Port: 1081}},
		{"http://h", Hop{Kind: HTTP, Host: "h", Port: 8080}},
		{"http:[::1]:3128", Hop{Kind: HTTP, Host: "::1", Port: 3128}},
	}
	for _, c := range cases {
		got, err := ParseProxy(c.in)
		if err != nil {
			t.Fatalf("%s: %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("%s: got %+v, want %+v", c.in, got, c.want)
		}
	}
	for _, bad := range []string{"10.0.0.1", "ftp:h:21", "socks5:h:x"} {
		if _, err := ParseProxy(bad); err == nil {
			t.Errorf("%s: expected error", bad)
		}
	}
}

func TestParseSSH(t *testing.T) {
	got, err := ParseSSH("app@10.15.5.1")
	if err != nil {
		t.Fatal(err)
	}
	if got != (Hop{Kind: SSH, Host: "10.15.5.1", Port: 22, User: "app"}) {
		t.Errorf("got %+v", got)
	}
	got, err = ParseSSH("ssh://u:secret@bastion:2222")
	if err != nil {
		t.Fatal(err)
	}
	if got != (Hop{Kind: SSH, Host: "bastion", Port: 2222, User: "u", Password: "secret"}) {
		t.Errorf("got %+v", got)
	}
}

// TestChain dials an echo server through every proxy kind and an SSH tunnel,
// in both orders.
func TestChain(t *testing.T) {
	echo := listen(t, func(c net.Conn) { io.Copy(c, c) })
	socks5 := listen(t, fakeSOCKS5)
	socks4 := listen(t, fakeSOCKS4)
	httpP := listen(t, fakeHTTP)
	sshAddr := fakeSSH(t)

	hop := func(k Kind, addr string) *Hop {
		h, p, _ := net.SplitHostPort(addr)
		n, _ := strconv.Atoi(p)
		return &Hop{Kind: k, Host: h, Port: n, User: "app", Password: "pw"}
	}
	chains := map[string]Chain{
		"socks5":          {hop(SOCKS5, socks5)},
		"socks4":          {hop(SOCKS4, socks4)},
		"http":            {hop(HTTP, httpP)},
		"ssh":             {hop(SSH, sshAddr)},
		"socks5 then ssh": {hop(SOCKS5, socks5), hop(SSH, sshAddr)},
		"ssh then http":   {hop(SSH, sshAddr), hop(HTTP, httpP)},
		"all":             {hop(SOCKS4, socks4), hop(HTTP, httpP), hop(SSH, sshAddr), hop(SOCKS5, socks5)},
	}
	for name, chain := range chains {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			dial := chain.Dialer(5 * time.Second)
			for i := 0; i < 2; i++ { // second dial reuses the ssh session
				c, err := dial(ctx, "tcp", echo)
				if err != nil {
					t.Fatal(err)
				}
				msg := "hello " + name
				io.WriteString(c, msg)
				buf := make([]byte, len(msg))
				if _, err := io.ReadFull(c, buf); err != nil || string(buf) != msg {
					t.Fatalf("echo: %q, %v", buf, err)
				}
				c.Close()
			}
		})
	}
}

func listen(t *testing.T, handle func(net.Conn)) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				handle(c)
			}()
		}
	}()
	return l.Addr().String()
}

func pipe(a, b net.Conn) {
	go io.Copy(a, b)
	io.Copy(b, a)
}

func fakeSOCKS5(c net.Conn) {
	b := make([]byte, 262)
	io.ReadFull(c, b[:2])
	io.ReadFull(c, b[:b[1]])
	c.Write([]byte{5, 2})
	io.ReadFull(c, b[:2])
	io.ReadFull(c, b[:b[1]]) // user
	io.ReadFull(c, b[:1])
	io.ReadFull(c, b[:b[0]]) // password
	c.Write([]byte{1, 0})
	io.ReadFull(c, b[:4])
	var host string
	switch b[3] {
	case 1:
		io.ReadFull(c, b[:4])
		host = net.IP(b[:4]).String()
	case 3:
		io.ReadFull(c, b[:1])
		n := int(b[0])
		io.ReadFull(c, b[:n])
		host = string(b[:n])
	}
	io.ReadFull(c, b[:2])
	port := binary.BigEndian.Uint16(b[:2])
	up, err := net.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(int(port))))
	if err != nil {
		c.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	defer up.Close()
	c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
	pipe(c, up)
}

func fakeSOCKS4(c net.Conn) {
	br := bufio.NewReader(c)
	b := make([]byte, 8)
	io.ReadFull(br, b)
	br.ReadString(0) // user id
	host := net.IP(b[4:8]).String()
	if b[4] == 0 && b[5] == 0 && b[6] == 0 && b[7] != 0 {
		h, _ := br.ReadString(0)
		host = h[:len(h)-1]
	}
	up, err := net.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(b[2:4])))))
	if err != nil {
		c.Write([]byte{0, 0x5B, 0, 0, 0, 0, 0, 0})
		return
	}
	defer up.Close()
	c.Write([]byte{0, 0x5A, 0, 0, 0, 0, 0, 0})
	pipe(c, up)
}

func fakeHTTP(c net.Conn) {
	br := bufio.NewReader(c)
	req, err := http.ReadRequest(br)
	if err != nil || req.Method != http.MethodConnect || req.Header.Get("Proxy-Authorization") == "" {
		io.WriteString(c, "HTTP/1.1 407 Proxy Authentication Required\r\n\r\n")
		return
	}
	up, err := net.Dial("tcp", req.Host)
	if err != nil {
		io.WriteString(c, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
		return
	}
	defer up.Close()
	io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n")
	pipe(c, up)
}

// fakeSSH runs an SSH server that accepts password "pw" and serves
// direct-tcpip forwarding.
func fakeSSH(t *testing.T) string {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{
		PasswordCallback: func(_ ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
			if string(pw) == "pw" {
				return nil, nil
			}
			return nil, io.EOF
		},
	}
	cfg.AddHostKey(signer)
	return listen(t, func(c net.Conn) {
		_, chans, reqs, err := ssh.NewServerConn(c, cfg)
		if err != nil {
			return
		}
		go ssh.DiscardRequests(reqs)
		for nc := range chans {
			if nc.ChannelType() != "direct-tcpip" {
				nc.Reject(ssh.UnknownChannelType, "")
				continue
			}
			var p struct {
				Host     string
				Port     uint32
				OrigHost string
				OrigPort uint32
			}
			ssh.Unmarshal(nc.ExtraData(), &p)
			up, err := net.Dial("tcp", net.JoinHostPort(p.Host, strconv.Itoa(int(p.Port))))
			if err != nil {
				nc.Reject(ssh.ConnectionFailed, err.Error())
				continue
			}
			ch, creqs, _ := nc.Accept()
			go ssh.DiscardRequests(creqs)
			go func() {
				defer up.Close()
				defer ch.Close()
				go io.Copy(ch, up)
				io.Copy(up, ch)
			}()
		}
	})
}
