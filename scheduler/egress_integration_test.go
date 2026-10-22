package main

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/gorilla/websocket"

	"trading-scheduler/internal/egresspolicy"
)

type egressTestCA struct {
	pool *x509.CertPool
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

func newEgressTestCA(t *testing.T) *egressTestCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "egress test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &egressTestCA{pool: pool, cert: cert, key: key}
}

func egressPEM(ca *egressTestCA) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.cert.Raw})
}

func (ca *egressTestCA) leaf(t *testing.T, names ...string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: names[0]},
		DNSNames:     names,
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

type egressTestProxy struct {
	scheme  string
	user    string
	pass    string
	forward string
	stall   atomic.Bool
	ln      net.Listener

	mu      sync.Mutex
	targets []egressProxyRecord
}

type egressProxyRecord struct {
	Target string
	Domain bool
	AuthOK bool
}

func startEgressTestProxy(t *testing.T, scheme, user, pass, forward string, cert *tls.Certificate) *egressTestProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if cert != nil {
		ln = tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{*cert}})
	}
	p := &egressTestProxy{scheme: scheme, user: user, pass: pass, forward: forward, ln: ln}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go p.serve(conn)
		}
	}()
	return p
}

func (p *egressTestProxy) url(host string) string {
	_, port, _ := net.SplitHostPort(p.ln.Addr().String())
	auth := ""
	if p.user != "" {
		auth = p.user + ":" + p.pass + "@"
	}
	return p.scheme + "://" + auth + net.JoinHostPort(host, port)
}

func (p *egressTestProxy) record(r egressProxyRecord) {
	p.mu.Lock()
	p.targets = append(p.targets, r)
	p.mu.Unlock()
}

func (p *egressTestProxy) records() []egressProxyRecord {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]egressProxyRecord(nil), p.targets...)
}

func (p *egressTestProxy) serve(conn net.Conn) {
	defer conn.Close()
	if p.stall.Load() {
		_, _ = io.Copy(io.Discard, conn)
		return
	}
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if strings.HasPrefix(p.scheme, "socks5") {
		p.serveSOCKS(conn)
		return
	}
	br := bufio.NewReader(conn)
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	authOK := p.user == "" || req.Header.Get("Proxy-Authorization") == "Basic "+base64.StdEncoding.EncodeToString([]byte(p.user+":"+p.pass))
	host, _, _ := net.SplitHostPort(req.Host)
	p.record(egressProxyRecord{Target: req.Host, Domain: net.ParseIP(host) == nil, AuthOK: authOK})
	if req.Method != http.MethodConnect || !authOK {
		_, _ = io.WriteString(conn, "HTTP/1.1 407 Proxy Authentication Required\r\nContent-Length: 0\r\n\r\n")
		return
	}
	upstream, err := net.Dial("tcp", p.forward)
	if err != nil {
		_, _ = io.WriteString(conn, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
		return
	}
	defer upstream.Close()
	_, _ = io.WriteString(conn, "HTTP/1.1 200 Connection established\r\n\r\n")
	egressTestSplice(&prefixedConn{Conn: conn, r: br}, upstream)
}

type prefixedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *prefixedConn) Read(b []byte) (int, error) {
	if c.r.Buffered() > 0 {
		return c.r.Read(b)
	}
	return c.Conn.Read(b)
}

func egressTestSplice(a, b net.Conn) {
	_ = a.SetDeadline(time.Time{})
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(a, b); done <- struct{}{} }()
	go func() { _, _ = io.Copy(b, a); done <- struct{}{} }()
	<-done
}

func (p *egressTestProxy) serveSOCKS(conn net.Conn) {
	head := make([]byte, 2)
	if _, err := io.ReadFull(conn, head); err != nil {
		return
	}
	methods := make([]byte, head[1])
	_, _ = io.ReadFull(conn, methods)
	want := byte(0)
	if p.user != "" {
		want = 2
	}
	_, _ = conn.Write([]byte{5, want})
	authOK := true
	if want == 2 {
		vh := make([]byte, 2)
		_, _ = io.ReadFull(conn, vh)
		u := make([]byte, vh[1])
		_, _ = io.ReadFull(conn, u)
		pl := make([]byte, 1)
		_, _ = io.ReadFull(conn, pl)
		pw := make([]byte, pl[0])
		_, _ = io.ReadFull(conn, pw)
		authOK = string(u) == p.user && string(pw) == p.pass
		if !authOK {
			p.record(egressProxyRecord{AuthOK: false})
			_, _ = conn.Write([]byte{1, 1})
			return
		}
		_, _ = conn.Write([]byte{1, 0})
	}
	req := make([]byte, 4)
	if _, err := io.ReadFull(conn, req); err != nil {
		return
	}
	var host string
	domain := false
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
		host, domain = string(b), true
	}
	pb := make([]byte, 2)
	_, _ = io.ReadFull(conn, pb)
	p.record(egressProxyRecord{Target: net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(pb)))), Domain: domain, AuthOK: authOK})
	upstream, err := net.Dial("tcp", p.forward)
	if err != nil {
		_, _ = conn.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	defer upstream.Close()
	_, _ = conn.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
	egressTestSplice(conn, upstream)
}

func installTestEgress(t *testing.T, proxyURL string, roots *x509.CertPool, proxyRoots *x509.CertPool) {
	t.Helper()
	prevTransport := http.DefaultTransport
	prevClientTransport := http.DefaultClient.Transport
	prevWS := websocket.DefaultDialer
	prevRuntime := processEgress
	processEgress = &egressRuntime{}
	t.Setenv(egresspolicy.EnvProxy, proxyURL)
	if err := establishProcessEgress(context.Background(), nil, false); err != nil {
		t.Fatalf("establish egress: %v", err)
	}
	processEgress.mu.Lock()
	processEgress.transport.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	if proxyRoots != nil {
		processEgress.dialer.TLSConfig = &tls.Config{RootCAs: proxyRoots, MinVersion: tls.VersionTLS12}
	}
	processEgress.dialer.HandshakeTimeout = 3 * time.Second
	processEgress.mu.Unlock()
	t.Cleanup(func() {
		processEgress = prevRuntime
		http.DefaultTransport = prevTransport
		http.DefaultClient.Transport = prevClientTransport
		websocket.DefaultDialer = prevWS
	})
}

type egressFakeVenue struct {
	addr string
	mu   sync.Mutex
	hits []string
}

func (v *egressFakeVenue) seen() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]string(nil), v.hits...)
}

func startEgressFakeVenue(t *testing.T, ca *egressTestCA) *egressFakeVenue {
	t.Helper()
	v := &egressFakeVenue{}
	upgrader := websocket.Upgrader{}
	mux := http.NewServeMux()
	mux.HandleFunc("/info", func(w http.ResponseWriter, r *http.Request) {
		v.mu.Lock()
		v.hits = append(v.hits, r.Host+r.URL.Path)
		v.mu.Unlock()
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch req["type"] {
		case "clearinghouseState":
			_, _ = io.WriteString(w, `{"marginSummary":{"accountValue":"1234.5"}}`)
		case "allMids":
			_, _ = io.WriteString(w, `{"BTC":"65000.5"}`)
		default:
			_, _ = io.WriteString(w, `{}`)
		}
	})
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		v.mu.Lock()
		v.hits = append(v.hits, r.Host+r.URL.Path)
		v.mu.Unlock()
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		_ = c.WriteMessage(websocket.TextMessage, []byte(`{"channel":"pong"}`))
		_ = c.Close()
	})
	mux.HandleFunc("/api/v9/users/@me", func(w http.ResponseWriter, r *http.Request) {
		v.mu.Lock()
		v.hits = append(v.hits, r.Host+r.URL.Path)
		v.mu.Unlock()
		_, _ = io.WriteString(w, `{"id":"42","username":"fixture"}`)
	})
	srv := httptest.NewUnstartedServer(mux)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{ca.leaf(t, "api.hyperliquid.xyz", "discord.com")}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	v.addr = srv.Listener.Addr().String()
	return v
}

const egressTestUser = "fixtureuser"

func egressSentinel(t *testing.T) string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "sntl" + base64.RawURLEncoding.EncodeToString(b)
}

func TestEgressProxyRoutesRealVenueClientsForEveryScheme(t *testing.T) {
	ca := newEgressTestCA(t)
	venue := startEgressFakeVenue(t, ca)
	proxyCert := ca.leaf(t, "localhost")
	for _, scheme := range []string{"http", "https", "socks5", "socks5h"} {
		t.Run(scheme, func(t *testing.T) {
			sentinel := egressSentinel(t)
			var cert *tls.Certificate
			if scheme == "https" {
				cert = &proxyCert
			}
			proxy := startEgressTestProxy(t, scheme, egressTestUser, sentinel, venue.addr, cert)
			installTestEgress(t, proxy.url("localhost"), ca.pool, ca.pool)
			before := len(venue.seen())

			bal, err := fetchHyperliquidBalance("0xabc")
			if err != nil || bal != 1234.5 {
				t.Fatalf("balance through the proxy: %v %v", bal, err)
			}
			mids, err := fetchHyperliquidMids([]string{"BTC"})
			if err != nil || mids["BTC"] != 65000.5 {
				t.Fatalf("mids through the proxy: %v %v", mids, err)
			}

			d := egressWebsocketDialer(5 * time.Second)
			d.TLSClientConfig = &tls.Config{RootCAs: ca.pool}
			ws, _, err := d.Dial("wss://api.hyperliquid.xyz/ws", nil)
			if err != nil {
				t.Fatalf("feed websocket through the proxy: %v", err)
			}
			_, msg, err := ws.ReadMessage()
			ws.Close()
			if err != nil || !strings.Contains(string(msg), "pong") {
				t.Fatalf("websocket read: %q %v", msg, err)
			}

			session, err := discordgo.New("Bot fixture-token")
			if err != nil {
				t.Fatal(err)
			}
			pinDiscordSession(session)
			session.Client.Transport.(*http.Transport).TLSClientConfig = &tls.Config{RootCAs: ca.pool}
			u, err := session.User("@me")
			if err != nil || u.ID != "42" {
				t.Fatalf("discord REST through the proxy: %v %v", u, err)
			}
			if session.Dialer.Proxy != nil || session.Dialer.NetDialContext == nil {
				t.Fatalf("discord websocket dialer is not pinned to the policy")
			}

			recs := proxy.records()
			wantTargets := map[string]bool{"api.hyperliquid.xyz:443": false, "discord.com:443": false}
			for _, r := range recs {
				if !r.AuthOK {
					t.Fatalf("proxy saw failed auth: %+v", r)
				}
				if !r.Domain {
					t.Fatalf("proxy saw an IP target, not the venue host name: %+v", r)
				}
				if _, ok := wantTargets[r.Target]; ok {
					wantTargets[r.Target] = true
				}
			}
			for target, seen := range wantTargets {
				if !seen {
					t.Fatalf("proxy never saw %s; records %+v", target, recs)
				}
			}
			if got := len(venue.seen()) - before; got < 4 {
				t.Fatalf("venue saw %d requests, want 4", got)
			}
			st := processEgress.status()
			if st.Mode != egressModeProxy || st.Tunnels == 0 || st.LastSuccessAt == nil {
				t.Fatalf("status did not record successful tunnels: %+v", st)
			}
			data, _ := json.Marshal(st)
			if strings.Contains(string(data), sentinel) {
				t.Fatalf("status holds the credential: %s", data)
			}
		})
	}
}

func TestEgressProxyFailuresNeverFallBackAndStayRedacted(t *testing.T) {
	ca := newEgressTestCA(t)
	venue := startEgressFakeVenue(t, ca)
	sentinel := egressSentinel(t)
	untrusted := newEgressTestCA(t)
	untrustedCert := untrusted.leaf(t, "localhost")

	cases := []struct {
		name   string
		build  func(t *testing.T) string
		expect string
	}{
		{"http wrong credentials", func(t *testing.T) string {
			p := startEgressTestProxy(t, "http", egressTestUser, "right-"+sentinel, venue.addr, nil)
			_, port, _ := net.SplitHostPort(p.ln.Addr().String())
			return "http://" + egressTestUser + ":" + sentinel + "@localhost:" + port
		}, "407"},
		{"socks5h wrong credentials", func(t *testing.T) string {
			p := startEgressTestProxy(t, "socks5", egressTestUser, "right-"+sentinel, venue.addr, nil)
			_, port, _ := net.SplitHostPort(p.ln.Addr().String())
			return "socks5h://" + egressTestUser + ":" + sentinel + "@localhost:" + port
		}, "rejected the credentials"},
		{"https proxy with an untrusted certificate", func(t *testing.T) string {
			p := startEgressTestProxy(t, "http", egressTestUser, sentinel, venue.addr, &untrustedCert)
			_, port, _ := net.SplitHostPort(p.ln.Addr().String())
			return "https://" + egressTestUser + ":" + sentinel + "@localhost:" + port
		}, "proxy TLS handshake"},
		{"refused proxy", func(t *testing.T) string {
			ln, _ := net.Listen("tcp", "127.0.0.1:0")
			_, port, _ := net.SplitHostPort(ln.Addr().String())
			ln.Close()
			return "http://" + egressTestUser + ":" + sentinel + "@localhost:" + port
		}, "connect"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			installTestEgress(t, tc.build(t), ca.pool, ca.pool)
			before := len(venue.seen())
			_, err := fetchHyperliquidBalance("0xabc")
			if err == nil {
				t.Fatal("request succeeded through a failing proxy")
			}
			if !errors.Is(err, egresspolicy.ErrProxyTransport) {
				t.Fatalf("error is not a proxy transport failure: %v", err)
			}
			if !strings.Contains(err.Error(), tc.expect) {
				t.Fatalf("error %q does not name %q", err, tc.expect)
			}
			if strings.Contains(err.Error(), sentinel) {
				t.Fatalf("error holds the credential: %v", err)
			}
			if got := len(venue.seen()) - before; got != 0 {
				t.Fatalf("the venue was reached %d times despite the proxy failure", got)
			}
			st := processEgress.status()
			data, _ := json.Marshal(st)
			if st.ConsecutiveFailures == 0 || st.LastError == "" || strings.Contains(string(data), sentinel) {
				t.Fatalf("status after failure: %s", data)
			}
		})
	}
}

func TestEgressProxyHandshakeHonorsCancellation(t *testing.T) {
	ca := newEgressTestCA(t)
	venue := startEgressFakeVenue(t, ca)
	for _, scheme := range []string{"http", "socks5h"} {
		t.Run(scheme, func(t *testing.T) {
			p := startEgressTestProxy(t, strings.TrimSuffix(scheme, "h"), "", "", venue.addr, nil)
			p.stall.Store(true)
			_, port, _ := net.SplitHostPort(p.ln.Addr().String())
			installTestEgress(t, scheme+"://localhost:"+port, ca.pool, nil)
			processEgress.mu.Lock()
			processEgress.dialer.HandshakeTimeout = time.Minute
			d := processEgress.dialer
			processEgress.mu.Unlock()
			ctx, cancel := context.WithCancel(context.Background())
			go func() {
				time.Sleep(200 * time.Millisecond)
				cancel()
			}()
			start := time.Now()
			_, err := d.DialContext(ctx, "tcp", "api.hyperliquid.xyz:443")
			if err == nil {
				t.Fatal("dial succeeded through a stalled proxy")
			}
			if elapsed := time.Since(start); elapsed > 5*time.Second {
				t.Fatalf("cancellation took %s", elapsed)
			}
			if !strings.Contains(err.Error(), "context canceled") {
				t.Fatalf("error does not report the cancellation: %v", err)
			}
			dctx, dcancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer dcancel()
			start = time.Now()
			if _, err := d.DialContext(dctx, "tcp", "api.hyperliquid.xyz:443"); err == nil || time.Since(start) > 5*time.Second {
				t.Fatalf("deadline not honored: %v after %s", err, time.Since(start))
			}
		})
	}
}

func TestEgressLoopbackStaysDirect(t *testing.T) {
	ca := newEgressTestCA(t)
	venue := startEgressFakeVenue(t, ca)
	p := startEgressTestProxy(t, "http", "", "", venue.addr, nil)
	installTestEgress(t, p.url("localhost"), ca.pool, nil)
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "local-ok")
	}))
	defer local.Close()
	resp, err := egressHTTPClient(5 * time.Second).Get(local.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "local-ok" {
		t.Fatalf("loopback body %q", body)
	}
	if len(p.records()) != 0 {
		t.Fatalf("loopback traffic went to the proxy: %+v", p.records())
	}
	sock := t.TempDir() + "/s.sock"
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			c.Close()
		}
	}()
	processEgress.mu.Lock()
	d := processEgress.dialer
	processEgress.mu.Unlock()
	c, err := d.DialContext(context.Background(), "unix", sock)
	if err != nil {
		t.Fatalf("unix socket dial: %v", err)
	}
	c.Close()
}

func TestEgressHostProxyVariablesCannotChangeRouting(t *testing.T) {
	ca := newEgressTestCA(t)
	venue := startEgressFakeVenue(t, ca)
	decoy := startEgressTestProxy(t, "http", "", "", venue.addr, nil)
	for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		t.Setenv(k, decoy.url("127.0.0.1"))
	}
	t.Setenv("NO_PROXY", "*")
	t.Setenv("no_proxy", "*")
	p := startEgressTestProxy(t, "http", "", "", venue.addr, nil)
	installTestEgress(t, p.url("localhost"), ca.pool, nil)
	if _, err := fetchHyperliquidBalance("0xabc"); err != nil {
		t.Fatal(err)
	}
	if len(decoy.records()) != 0 {
		t.Fatalf("host proxy variables routed traffic: %+v", decoy.records())
	}
	if len(p.records()) == 0 {
		t.Fatal("the policy proxy saw nothing")
	}
	env := egressChildEnvironment(map[string]string{"HTTP_PROXY": "http://evil:1", "NO_PROXY": "*", egresspolicy.EnvProxy: "http://evil:1", "KEEP": "yes"})
	got := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		got[k] = v
	}
	want := p.url("localhost")
	if got["HTTP_PROXY"] != want || got["https_proxy"] != want || got["ALL_PROXY"] != want || got[egresspolicy.EnvProxy] != want {
		t.Fatalf("child proxy variables were not restored to the policy: %v", got)
	}
	if got["NO_PROXY"] != egressNoProxy || got["no_proxy"] != egressNoProxy || got[egresspolicy.EnvMode] != egresspolicy.ModeProxy || got["KEEP"] != "yes" {
		t.Fatalf("child environment: %v", got)
	}
}

func TestEgressConfigBlockIsStrictAndFrozen(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := dir + "/" + name
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	base := `{"config_version": 20, "interval_seconds": 60, "db_file": "` + dir + `/s.db", "strategies": [], %s}`
	unknown := write("unknown.json", strings.Replace(base, "%s", `"egress": {"proxy_required": true, "proxy_url": "http://x:1"}`, 1))
	if _, err := LoadConfigReadOnly(unknown); err == nil || !strings.Contains(err.Error(), "proxy_url") {
		t.Fatalf("unknown egress key accepted: %v", err)
	}
	wrongType := write("type.json", strings.Replace(base, "%s", `"egress": {"proxy_required": "yes"}`, 1))
	if _, err := LoadConfigReadOnly(wrongType); err == nil {
		t.Fatal("non-boolean egress value accepted")
	}
	if _, err := LoadConfigForProbe(wrongType); err == nil {
		t.Fatal("probe config path accepted a non-boolean egress value")
	}
	on := write("on.json", strings.Replace(base, "%s", `"egress": {"proxy_required": true}`, 1))
	off := write("off.json", strings.Replace(base, "%s", `"log_dir": "logs"`, 1))
	cur, err := LoadConfigReadOnly(off)
	if err != nil {
		t.Fatal(err)
	}
	next, err := LoadConfigReadOnly(on)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateHotReloadCompatible(cur, next); err == nil || !strings.Contains(err.Error(), "egress changed") {
		t.Fatalf("scheduler reload accepted an egress change: %v", err)
	}
	sockDir, err := os.MkdirTemp("/tmp", "eg")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	feedBase := `{"role": "feed", "status_port": 9301, "feed": {"source": "websocket", "socket_path": "` + sockDir + `/f.sock", "consumer_configs": ["` + off + `"]}%s}`
	feedUnknown := write("feed-unknown.json", strings.Replace(feedBase, "%s", `, "egress": {"notify": true}`, 1))
	if _, err := loadFeedRoleConfig(feedUnknown); err == nil || !strings.Contains(err.Error(), "egress") {
		t.Fatalf("feed accepted an unknown egress key: %v", err)
	}
	feedOn := write("feed-on.json", strings.Replace(feedBase, "%s", `, "egress": {"proxy_required": true}`, 1))
	feedOff := write("feed-off.json", strings.Replace(feedBase, "%s", ``, 1))
	fOn, errOn := loadFeedRoleConfig(feedOn)
	fOff, errOff := loadFeedRoleConfig(feedOff)
	if errOn != nil || errOff != nil {
		t.Fatalf("feed configs: %v %v", errOn, errOff)
	}
	if errs := egressReloadErrors(fOff, fOn); len(errs) == 0 {
		t.Fatal("feed reload accepted an egress change")
	}
	peeked, err := peekEgressConfig(feedOn)
	if err != nil || peeked == nil || !peeked.ProxyRequired {
		t.Fatalf("peek: %+v %v", peeked, err)
	}
}

func TestEgressEntryRefusesBeforeExternalAccess(t *testing.T) {
	sentinel := egressSentinel(t)
	cases := []struct {
		name string
		cfg  *EgressConfig
		env  string
		want string
	}{
		{"required and unset", &EgressConfig{ProxyRequired: true}, "", "proxy_required is true"},
		{"direct notifications without proxy mode", &EgressConfig{NotificationsDirect: true}, "", "notifications_direct"},
		{"malformed with proxy optional", nil, "http://" + egressTestUser + ":" + sentinel + "@proxy.example/path", "must not have a path"},
		{"unsupported scheme", nil, "ftp://" + egressTestUser + ":" + sentinel + "@proxy.example:21", "unsupported or missing scheme"},
		{"missing port", nil, "http://" + egressTestUser + ":" + sentinel + "@proxy.example", "explicit port"},
		{"scheme-less credentials", nil, egressTestUser + ":" + sentinel + "@proxy.example:1080", "scheme"},
		{"valid URL without enforcement", nil, "http://" + egressTestUser + ":" + sentinel + "@proxy.example:3128", "enforcement is unavailable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prev := processEgress
			processEgress = &egressRuntime{}
			t.Cleanup(func() { processEgress = prev })
			t.Setenv(egresspolicy.EnvProxy, tc.env)
			t.Setenv(egresspolicy.EnvDeployment, "nosuchdeployment")
			err := establishProcessEgress(context.Background(), tc.cfg, true)
			if err == nil {
				t.Fatal("entry accepted the policy")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not name %q", err, tc.want)
			}
			if strings.Contains(err.Error(), sentinel) || strings.Contains(err.Error(), egressTestUser) {
				t.Fatalf("error leaks credentials: %v", err)
			}
			if processEgress.isActive() {
				t.Fatal("a refused policy left proxy mode active")
			}
		})
	}
}
