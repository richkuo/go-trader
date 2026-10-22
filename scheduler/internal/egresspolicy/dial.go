package egresspolicy

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

const DefaultHandshakeTimeout = 15 * time.Second

var ErrProxyTransport = errors.New("egress proxy transport failure")

type ProxyError struct {
	Proxy  string
	Target string
	Stage  string
	Reason string
}

func (e *ProxyError) Error() string {
	if e.Target == "" {
		return fmt.Sprintf("egress proxy %s: %s: %s", e.Proxy, e.Stage, e.Reason)
	}
	return fmt.Sprintf("egress proxy %s: %s to %s: %s", e.Proxy, e.Stage, e.Target, e.Reason)
}

func (e *ProxyError) Is(target error) bool {
	return target == ErrProxyTransport
}

type Dialer struct {
	Endpoint         Endpoint
	Redactor         *Redactor
	HandshakeTimeout time.Duration
	TLSConfig        *tls.Config
	OnResult         func(target string, err error)
	Base             net.Dialer
}

func NewDialer(ep Endpoint) *Dialer {
	return &Dialer{
		Endpoint:         ep,
		Redactor:         NewRedactor(ep),
		HandshakeTimeout: DefaultHandshakeTimeout,
	}
}

func IsLoopbackHost(host string) bool {
	h := strings.TrimSuffix(strings.ToLower(host), ".")
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	if ip := net.ParseIP(strings.Trim(h, "[]")); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

func (d *Dialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	switch network {
	case "unix", "unixpacket":
		return d.Base.DialContext(ctx, network, addr)
	case "tcp", "tcp4", "tcp6":
	default:
		return nil, d.fail(addr, "dial", fmt.Sprintf("network %q is not allowed through the egress proxy", network))
	}
	host, portText, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, d.fail(addr, "dial", "the target address has no port")
	}
	if IsLoopbackHost(host) {
		return d.Base.DialContext(ctx, network, addr)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return nil, d.fail(addr, "dial", "the target port is not a number in 1-65535")
	}
	conn, err := d.tunnel(ctx, host, port)
	if d.OnResult != nil {
		d.OnResult(addr, err)
	}
	return conn, err
}

func (d *Dialer) fail(target, stage, reason string) error {
	return &ProxyError{Proxy: d.Endpoint.Redacted(), Target: target, Stage: stage, Reason: d.Redactor.Redact(reason)}
}

func (d *Dialer) failErr(target, stage string, err error) error {
	reason := err.Error()
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		reason = "timed out: " + reason
	}
	return d.fail(target, stage, reason)
}

func (d *Dialer) tunnel(ctx context.Context, host string, port int) (net.Conn, error) {
	target := net.JoinHostPort(host, strconv.Itoa(port))
	if err := ctx.Err(); err != nil {
		return nil, d.failErr(target, "connect", err)
	}
	conn, err := d.Base.DialContext(ctx, "tcp", d.Endpoint.HostPort())
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, d.failErr(target, "connect", ctxErr)
		}
		return nil, d.failErr(target, "connect", err)
	}
	deadline, hasDeadline := ctx.Deadline()
	timeout := d.HandshakeTimeout
	if timeout <= 0 {
		timeout = DefaultHandshakeTimeout
	}
	if limit := time.Now().Add(timeout); !hasDeadline || limit.Before(deadline) {
		deadline = limit
	}
	_ = conn.SetDeadline(deadline)
	cancelled := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		close(cancelled)
		_ = conn.SetDeadline(time.Unix(1, 0))
	})
	tunneled, err := d.handshake(ctx, conn, host, port, target)
	if !stop() {
		<-cancelled
		if err == nil {
			err = d.failErr(target, "handshake", ctx.Err())
		}
	}
	if err != nil {
		_ = conn.Close()
		if ctxErr := ctx.Err(); ctxErr != nil {
			var pe *ProxyError
			if errors.As(err, &pe) && !strings.Contains(pe.Reason, ctxErr.Error()) {
				pe.Reason = ctxErr.Error() + ": " + pe.Reason
			}
		}
		return nil, err
	}
	_ = tunneled.SetDeadline(time.Time{})
	return tunneled, nil
}

func (d *Dialer) handshake(ctx context.Context, conn net.Conn, host string, port int, target string) (net.Conn, error) {
	stream := conn
	if d.Endpoint.Scheme == SchemeHTTPS {
		cfg := &tls.Config{MinVersion: tls.VersionTLS12}
		if d.TLSConfig != nil {
			cfg = d.TLSConfig.Clone()
		}
		cfg.ServerName = d.Endpoint.Host
		cfg.NextProtos = []string{"http/1.1"}
		tlsConn := tls.Client(conn, cfg)
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			return nil, d.failErr(target, "proxy TLS handshake", err)
		}
		stream = tlsConn
	}
	switch d.Endpoint.Scheme {
	case SchemeHTTP, SchemeHTTPS:
		return d.httpConnect(stream, target)
	default:
		return d.socks5Connect(stream, host, port, target)
	}
}

func (d *Dialer) httpConnect(conn net.Conn, target string) (net.Conn, error) {
	var b strings.Builder
	b.WriteString("CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n")
	if d.Endpoint.HasAuth {
		token := base64.StdEncoding.EncodeToString([]byte(d.Endpoint.Username + ":" + d.Endpoint.Password))
		b.WriteString("Proxy-Authorization: Basic " + token + "\r\n")
	}
	b.WriteString("\r\n")
	if _, err := io.WriteString(conn, b.String()); err != nil {
		return nil, d.failErr(target, "CONNECT", err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		return nil, d.failErr(target, "CONNECT", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, d.fail(target, "CONNECT", "the proxy answered "+resp.Status)
	}
	if br.Buffered() > 0 {
		return &bufferedConn{Conn: conn, r: br}, nil
	}
	return conn, nil
}

type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) {
	if c.r.Buffered() > 0 {
		return c.r.Read(p)
	}
	return c.Conn.Read(p)
}

var socksReplies = map[byte]string{
	1: "general SOCKS server failure",
	2: "connection not allowed by ruleset",
	3: "network unreachable",
	4: "host unreachable",
	5: "connection refused",
	6: "TTL expired",
	7: "command not supported",
	8: "address type not supported",
}

func (d *Dialer) socks5Connect(conn net.Conn, host string, port int, target string) (net.Conn, error) {
	methods := []byte{0x00}
	if d.Endpoint.HasAuth {
		methods = []byte{0x02}
	}
	greeting := append([]byte{0x05, byte(len(methods))}, methods...)
	if _, err := conn.Write(greeting); err != nil {
		return nil, d.failErr(target, "SOCKS5 greeting", err)
	}
	reply := make([]byte, 2)
	if _, err := io.ReadFull(conn, reply); err != nil {
		return nil, d.failErr(target, "SOCKS5 greeting", err)
	}
	if reply[0] != 0x05 {
		return nil, d.fail(target, "SOCKS5 greeting", "the proxy is not a SOCKS5 server")
	}
	switch reply[1] {
	case 0x00:
		if d.Endpoint.HasAuth {
			return nil, d.fail(target, "SOCKS5 greeting", "the proxy did not accept the user name and password method")
		}
	case 0x02:
		if !d.Endpoint.HasAuth {
			return nil, d.fail(target, "SOCKS5 greeting", "the proxy requires credentials")
		}
		auth := []byte{0x01, byte(len(d.Endpoint.Username))}
		auth = append(auth, d.Endpoint.Username...)
		auth = append(auth, byte(len(d.Endpoint.Password)))
		auth = append(auth, d.Endpoint.Password...)
		if _, err := conn.Write(auth); err != nil {
			return nil, d.failErr(target, "SOCKS5 authentication", err)
		}
		if _, err := io.ReadFull(conn, reply); err != nil {
			return nil, d.failErr(target, "SOCKS5 authentication", err)
		}
		if reply[1] != 0x00 {
			return nil, d.fail(target, "SOCKS5 authentication", "the proxy rejected the credentials")
		}
	default:
		return nil, d.fail(target, "SOCKS5 greeting", "the proxy offered no acceptable authentication method")
	}
	req := []byte{0x05, 0x01, 0x00}
	if ip := net.ParseIP(host); ip != nil {
		if v4 := ip.To4(); v4 != nil {
			req = append(req, 0x01)
			req = append(req, v4...)
		} else {
			req = append(req, 0x04)
			req = append(req, ip.To16()...)
		}
	} else {
		if len(host) > 255 {
			return nil, d.fail(target, "SOCKS5 connect", "the target host name is longer than 255 bytes")
		}
		req = append(req, 0x03, byte(len(host)))
		req = append(req, host...)
	}
	req = binary.BigEndian.AppendUint16(req, uint16(port))
	if _, err := conn.Write(req); err != nil {
		return nil, d.failErr(target, "SOCKS5 connect", err)
	}
	head := make([]byte, 4)
	if _, err := io.ReadFull(conn, head); err != nil {
		return nil, d.failErr(target, "SOCKS5 connect", err)
	}
	if head[0] != 0x05 {
		return nil, d.fail(target, "SOCKS5 connect", "malformed SOCKS5 reply")
	}
	if head[1] != 0x00 {
		reason, ok := socksReplies[head[1]]
		if !ok {
			reason = fmt.Sprintf("SOCKS5 reply code %d", head[1])
		}
		return nil, d.fail(target, "SOCKS5 connect", "the proxy refused: "+reason)
	}
	var skip int
	switch head[3] {
	case 0x01:
		skip = 4
	case 0x04:
		skip = 16
	case 0x03:
		l := make([]byte, 1)
		if _, err := io.ReadFull(conn, l); err != nil {
			return nil, d.failErr(target, "SOCKS5 connect", err)
		}
		skip = int(l[0])
	default:
		return nil, d.fail(target, "SOCKS5 connect", "malformed SOCKS5 bound address")
	}
	if _, err := io.ReadFull(conn, make([]byte, skip+2)); err != nil {
		return nil, d.failErr(target, "SOCKS5 connect", err)
	}
	return conn, nil
}

func (d *Dialer) Transport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil
	t.DialContext = d.DialContext
	t.DialTLSContext = nil
	t.ForceAttemptHTTP2 = true
	return t
}

func (d *Dialer) WebsocketDialer(handshakeTimeout time.Duration) *websocket.Dialer {
	return &websocket.Dialer{
		Proxy:            nil,
		NetDialContext:   d.DialContext,
		HandshakeTimeout: handshakeTimeout,
	}
}
