package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"trading-scheduler/internal/egresspolicy"
)

type record struct {
	Time     string `json:"time"`
	Kind     string `json:"kind"`
	Scheme   string `json:"scheme,omitempty"`
	Target   string `json:"target,omitempty"`
	Domain   bool   `json:"domain,omitempty"`
	AuthOK   bool   `json:"auth_ok"`
	Remote   string `json:"remote,omitempty"`
	Outcome  string `json:"outcome,omitempty"`
	Protocol string `json:"protocol,omitempty"`
}

type recorder struct {
	mu sync.Mutex
	f  *os.File
}

func (r *recorder) write(rec record) {
	rec.Time = time.Now().UTC().Format(time.RFC3339Nano)
	data, _ := json.Marshal(rec)
	r.mu.Lock()
	defer r.mu.Unlock()
	_, _ = r.f.Write(append(data, '\n'))
	_ = r.f.Sync()
}

func openRecorder(path string) *recorder {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	return &recorder{f: f}
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: egress-fixture proxy|listen|udp-listen|connect|attest|dial-proxy ...")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "proxy":
		runProxy(os.Args[2:])
	case "listen":
		runListen(os.Args[2:])
	case "udp-listen":
		runUDPListen(os.Args[2:])
	case "connect":
		os.Exit(runConnect(os.Args[2:]))
	case "attest":
		os.Exit(runAttest(os.Args[2:]))
	case "dial-proxy":
		os.Exit(runDialProxy(os.Args[2:]))
	case "hold-tunnel":
		os.Exit(runHoldTunnel(os.Args[2:]))
	default:
		fmt.Fprintln(os.Stderr, "unknown command")
		os.Exit(2)
	}
}

func waitSignal() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	<-ctx.Done()
}

func runProxy(args []string) {
	fs := flag.NewFlagSet("proxy", flag.ExitOnError)
	listen := fs.String("listen", "127.0.0.1:0", "listen address")
	scheme := fs.String("scheme", "http", "http, https or socks5")
	user := fs.String("user", "", "required user name")
	pass := fs.String("pass", "", "required password")
	certFile := fs.String("cert", "", "TLS certificate for https")
	keyFile := fs.String("key", "", "TLS key for https")
	logPath := fs.String("log", "", "JSON lines record file")
	forward := fs.Bool("forward", false, "connect accepted tunnels to the target")
	forwardTo := fs.String("forward-host", "", "connect every accepted tunnel to this host at the requested port")
	portFile := fs.String("port-file", "", "write the bound port here")
	_ = fs.Parse(args)
	rec := openRecorder(*logPath)
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if *scheme == "https" {
		cert, err := tls.LoadX509KeyPair(*certFile, *keyFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		ln = tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{cert}})
	}
	if *portFile != "" {
		_ = os.WriteFile(*portFile, []byte(strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)), 0o644)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
				if *scheme == "socks5" {
					serveSOCKS(conn, *user, *pass, *forward, *forwardTo, rec)
				} else {
					serveCONNECT(conn, *scheme, *user, *pass, *forward, *forwardTo, rec)
				}
			}()
		}
	}()
	waitSignal()
}

func tunnel(client net.Conn, target, forwardTo string) string {
	if forwardTo != "" {
		if _, port, err := net.SplitHostPort(target); err == nil {
			target = net.JoinHostPort(forwardTo, port)
		}
	}
	server, err := net.DialTimeout("tcp", target, 5*time.Second)
	if err != nil {
		return "forward failed: " + err.Error()
	}
	defer server.Close()
	_ = client.SetDeadline(time.Time{})
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(server, client); done <- struct{}{} }()
	go func() { _, _ = io.Copy(client, server); done <- struct{}{} }()
	<-done
	return "forwarded"
}

func serveCONNECT(conn net.Conn, scheme, user, pass string, forward bool, forwardTo string, rec *recorder) {
	br := bufio.NewReader(conn)
	req, err := http.ReadRequest(br)
	if err != nil {
		rec.write(record{Kind: "proxy", Scheme: scheme, Outcome: "bad request: " + err.Error(), Remote: conn.RemoteAddr().String()})
		return
	}
	r := record{Kind: "proxy", Scheme: scheme, Target: req.Host, Remote: conn.RemoteAddr().String()}
	host, _, _ := net.SplitHostPort(req.Host)
	r.Domain = net.ParseIP(host) == nil
	authOK := user == ""
	if user != "" {
		want := "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
		authOK = req.Header.Get("Proxy-Authorization") == want
	}
	r.AuthOK = authOK
	if req.Method != http.MethodConnect && authOK && forward && req.URL.IsAbs() {
		target := req.URL.Host
		if req.URL.Port() == "" {
			target = net.JoinHostPort(req.URL.Hostname(), "80")
		}
		r.Target = target
		r.Domain = net.ParseIP(req.URL.Hostname()) == nil
		dial := target
		if forwardTo != "" {
			_, port, _ := net.SplitHostPort(target)
			dial = net.JoinHostPort(forwardTo, port)
		}
		upstream, err := net.DialTimeout("tcp", dial, 5*time.Second)
		if err != nil {
			r.Outcome = "forward failed: " + err.Error()
			rec.write(r)
			_, _ = io.WriteString(conn, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
			return
		}
		defer upstream.Close()
		req.Header.Del("Proxy-Authorization")
		req.RequestURI = ""
		req.Close = true
		_ = req.Write(upstream)
		_, _ = io.Copy(conn, upstream)
		r.Outcome = "forwarded plain request"
		rec.write(r)
		return
	}
	if req.Method != http.MethodConnect {
		r.Outcome = "not CONNECT: " + req.Method
		rec.write(r)
		_, _ = io.WriteString(conn, "HTTP/1.1 405 Method Not Allowed\r\nContent-Length: 0\r\n\r\n")
		return
	}
	if !authOK {
		r.Outcome = "auth refused"
		rec.write(r)
		_, _ = io.WriteString(conn, "HTTP/1.1 407 Proxy Authentication Required\r\nProxy-Authenticate: Basic realm=\"fixture\"\r\nContent-Length: 0\r\n\r\n")
		return
	}
	if !forward {
		r.Outcome = "recorded"
		rec.write(r)
		_, _ = io.WriteString(conn, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
		return
	}
	_, _ = io.WriteString(conn, "HTTP/1.1 200 Connection established\r\n\r\n")
	r.Outcome = tunnel(&prefixConn{Conn: conn, r: br}, req.Host, forwardTo)
	rec.write(r)
}

type prefixConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *prefixConn) Read(p []byte) (int, error) {
	if c.r.Buffered() > 0 {
		return c.r.Read(p)
	}
	return c.Conn.Read(p)
}

func serveSOCKS(conn net.Conn, user, pass string, forward bool, forwardTo string, rec *recorder) {
	r := record{Kind: "proxy", Scheme: "socks5", Remote: conn.RemoteAddr().String()}
	head := make([]byte, 2)
	if _, err := io.ReadFull(conn, head); err != nil || head[0] != 5 {
		return
	}
	methods := make([]byte, head[1])
	if _, err := io.ReadFull(conn, methods); err != nil {
		return
	}
	want := byte(0)
	if user != "" {
		want = 2
	}
	offered := false
	for _, m := range methods {
		if m == want {
			offered = true
		}
	}
	if !offered {
		_, _ = conn.Write([]byte{5, 0xff})
		r.Outcome = "no acceptable method"
		rec.write(r)
		return
	}
	_, _ = conn.Write([]byte{5, want})
	r.AuthOK = true
	if want == 2 {
		vh := make([]byte, 2)
		if _, err := io.ReadFull(conn, vh); err != nil {
			return
		}
		u := make([]byte, vh[1])
		_, _ = io.ReadFull(conn, u)
		pl := make([]byte, 1)
		_, _ = io.ReadFull(conn, pl)
		p := make([]byte, pl[0])
		_, _ = io.ReadFull(conn, p)
		if string(u) != user || string(p) != pass {
			_, _ = conn.Write([]byte{1, 1})
			r.AuthOK = false
			r.Outcome = "auth refused"
			rec.write(r)
			return
		}
		_, _ = conn.Write([]byte{1, 0})
	}
	req := make([]byte, 4)
	if _, err := io.ReadFull(conn, req); err != nil {
		return
	}
	var host string
	switch req[3] {
	case 1:
		b := make([]byte, 4)
		_, _ = io.ReadFull(conn, b)
		host = net.IP(b).String()
	case 4:
		b := make([]byte, 16)
		_, _ = io.ReadFull(conn, b)
		host = net.IP(b).String()
	case 3:
		l := make([]byte, 1)
		_, _ = io.ReadFull(conn, l)
		b := make([]byte, l[0])
		_, _ = io.ReadFull(conn, b)
		host = string(b)
		r.Domain = true
	}
	pb := make([]byte, 2)
	_, _ = io.ReadFull(conn, pb)
	r.Target = net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(pb))))
	if !forward {
		_, _ = conn.Write([]byte{5, 2, 0, 1, 0, 0, 0, 0, 0, 0})
		r.Outcome = "recorded"
		rec.write(r)
		return
	}
	_, _ = conn.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
	r.Outcome = tunnel(conn, r.Target, forwardTo)
	rec.write(r)
}

func runListen(args []string) {
	fs := flag.NewFlagSet("listen", flag.ExitOnError)
	addr := fs.String("addr", "", "listen address")
	logPath := fs.String("log", "", "record file")
	reply := fs.String("reply", "", "write this HTTP body to each connection")
	hold := fs.Bool("hold", false, "keep each connection open until the peer closes it")
	certFile := fs.String("cert", "", "serve TLS with this certificate")
	keyFile := fs.String("key", "", "TLS key")
	_ = fs.Parse(args)
	rec := openRecorder(*logPath)
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if *certFile != "" {
		cert, err := tls.LoadX509KeyPair(*certFile, *keyFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		ln = tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{cert}})
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			rec.write(record{Kind: "direct", Protocol: "tcp", Target: *addr, Remote: conn.RemoteAddr().String()})
			go func() {
				defer conn.Close()
				if *hold {
					_ = conn.SetDeadline(time.Now().Add(5 * time.Minute))
					_, _ = io.Copy(io.Discard, conn)
					return
				}
				if *reply != "" {
					_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
					br := bufio.NewReader(conn)
					if _, err := http.ReadRequest(br); err == nil {
						fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(*reply), *reply)
					}
				}
			}()
		}
	}()
	waitSignal()
}

func runUDPListen(args []string) {
	fs := flag.NewFlagSet("udp-listen", flag.ExitOnError)
	addr := fs.String("addr", "", "listen address")
	logPath := fs.String("log", "", "record file")
	_ = fs.Parse(args)
	rec := openRecorder(*logPath)
	pc, err := net.ListenPacket("udp", *addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	go func() {
		buf := make([]byte, 2048)
		for {
			_, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			rec.write(record{Kind: "direct", Protocol: "udp", Target: *addr, Remote: from.String()})
		}
	}()
	waitSignal()
}

func runConnect(args []string) int {
	fs := flag.NewFlagSet("connect", flag.ExitOnError)
	network := fs.String("network", "tcp", "tcp, tcp4, tcp6 or udp")
	addr := fs.String("addr", "", "target")
	timeout := fs.Duration("timeout", 3*time.Second, "timeout")
	_ = fs.Parse(args)
	conn, err := net.DialTimeout(*network, *addr, *timeout)
	if err != nil {
		fmt.Printf("blocked: %v\n", err)
		return 3
	}
	defer conn.Close()
	if strings.HasPrefix(*network, "udp") {
		_, err = conn.Write([]byte("probe"))
		if err != nil {
			fmt.Printf("blocked: %v\n", err)
			return 3
		}
		fmt.Println("sent")
		return 0
	}
	fmt.Println("connected")
	return 0
}

func runAttest(args []string) int {
	fs := flag.NewFlagSet("attest", flag.ExitOnError)
	deployment := fs.String("deployment", "", "deployment")
	role := fs.String("role", egresspolicy.RoleWorker, "role")
	proxy := fs.String("proxy", "", "proxy URL")
	_ = fs.Parse(args)
	var want egresspolicy.Identity
	if *proxy != "" {
		ep, err := egresspolicy.ParseProxyURL(*proxy)
		if err != nil {
			fmt.Println("refused:", err)
			return 3
		}
		want = ep.Identity()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a, err := egresspolicy.Attest(ctx, *deployment, *role, want)
	if err != nil {
		fmt.Println("refused:", err)
		return 3
	}
	data, _ := json.Marshal(a)
	fmt.Println(string(data))
	return 0
}

func runDialProxy(args []string) int {
	fs := flag.NewFlagSet("dial-proxy", flag.ExitOnError)
	proxy := fs.String("proxy", "", "proxy URL")
	target := fs.String("target", "", "host:port")
	path := fs.String("path", "/", "HTTP path to GET through the tunnel")
	timeout := fs.Duration("timeout", 10*time.Second, "timeout")
	caFile := fs.String("proxy-ca", "", "trust this CA for an https proxy")
	_ = fs.Parse(args)
	ep, err := egresspolicy.ParseProxyURL(*proxy)
	if err != nil {
		fmt.Println("refused:", err)
		return 3
	}
	d := egresspolicy.NewDialer(ep)
	if *caFile != "" {
		pool, err := loadPool(*caFile)
		if err != nil {
			fmt.Println("failed:", err)
			return 3
		}
		d.TLSConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	conn, err := d.DialContext(ctx, "tcp", *target)
	if err != nil {
		fmt.Println("failed:", err)
		return 3
	}
	defer conn.Close()
	host, _, _ := net.SplitHostPort(*target)
	fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", *path, host)
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		if errors.Is(err, io.EOF) {
			fmt.Println("failed: tunnel closed")
		} else {
			fmt.Println("failed:", err)
		}
		return 3
	}
	body, _ := io.ReadAll(resp.Body)
	fmt.Printf("status=%d body=%s\n", resp.StatusCode, strings.TrimSpace(string(body)))
	return 0
}

func loadPool(path string) (*x509.CertPool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(data) {
		return nil, fmt.Errorf("no certificates in %s", path)
	}
	return pool, nil
}

func runHoldTunnel(args []string) int {
	fs := flag.NewFlagSet("hold-tunnel", flag.ExitOnError)
	proxy := fs.String("proxy", "", "proxy URL")
	target := fs.String("target", "", "host:port")
	maxWait := fs.Duration("max", 60*time.Second, "give up after")
	_ = fs.Parse(args)
	ep, err := egresspolicy.ParseProxyURL(*proxy)
	if err != nil {
		fmt.Println("refused:", err)
		return 3
	}
	d := egresspolicy.NewDialer(ep)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	conn, err := d.DialContext(ctx, "tcp", *target)
	cancel()
	if err != nil {
		fmt.Println("failed:", err)
		return 3
	}
	fmt.Println("open")
	_ = conn.SetReadDeadline(time.Now().Add(*maxWait))
	start := time.Now()
	_, err = conn.Read(make([]byte, 1))
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		fmt.Println("still-open")
		return 4
	}
	fmt.Printf("closed after %s\n", time.Since(start).Round(time.Second))
	return 0
}
