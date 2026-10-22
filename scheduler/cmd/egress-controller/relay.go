//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"sort"
	"strconv"
	"sync"
	"time"

	"trading-scheduler/internal/egresspolicy"
)

const (
	upstreamDialTimeout = 10 * time.Second
	statusDialTimeout   = 5 * time.Second
	resolveTimeout      = 10 * time.Second
)

type upstreamSet struct {
	mu        sync.Mutex
	addrs     []string
	resolved  time.Time
	lastError string
	conns     map[*relayConn]struct{}
	next      int
}

type relayConn struct {
	upstream string
	client   net.Conn
	server   net.Conn
	once     sync.Once
}

func (c *relayConn) close() {
	c.once.Do(func() {
		_ = c.client.Close()
		if c.server != nil {
			_ = c.server.Close()
		}
	})
}

func newUpstreamSet() *upstreamSet {
	return &upstreamSet{conns: map[*relayConn]struct{}{}}
}

func (u *upstreamSet) snapshot() ([]string, time.Time, string, int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.addrs...), u.resolved, u.lastError, len(u.conns)
}

func (u *upstreamSet) ready() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.addrs) > 0
}

func validateUpstreamIP(id egresspolicy.Identity, ip net.IP) error {
	literal := net.ParseIP(id.Host) != nil || egresspolicy.IsLoopbackHost(id.Host)
	switch {
	case ip.IsUnspecified(), ip.IsMulticast(), ip.IsLinkLocalMulticast(), ip.IsInterfaceLocalMulticast():
		return fmt.Errorf("address %s is unspecified or multicast", ip)
	case ip.IsLoopback() && !literal:
		return fmt.Errorf("the name %s resolved to loopback address %s; name a loopback proxy as localhost or by its address", id.Host, ip)
	case ip.IsLinkLocalUnicast() && !literal:
		return fmt.Errorf("the name %s resolved to link-local address %s", id.Host, ip)
	}
	return nil
}

func resolveUpstreams(ctx context.Context, id egresspolicy.Identity) ([]string, error) {
	port := strconv.Itoa(id.Port)
	if ip := net.ParseIP(id.Host); ip != nil {
		if err := validateUpstreamIP(id, ip); err != nil {
			return nil, err
		}
		return []string{net.JoinHostPort(ip.String(), port)}, nil
	}
	if egresspolicy.IsLoopbackHost(id.Host) {
		return []string{net.JoinHostPort("127.0.0.1", port), net.JoinHostPort("::1", port)}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, resolveTimeout)
	defer cancel()
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, id.Host)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %v", id.Host, err)
	}
	seen := map[string]bool{}
	var out []string
	for _, a := range ips {
		if err := validateUpstreamIP(id, a.IP); err != nil {
			return nil, err
		}
		hp := net.JoinHostPort(a.IP.String(), port)
		if !seen[hp] {
			seen[hp] = true
			out = append(out, hp)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("resolve %s: no addresses", id.Host)
	}
	sort.Strings(out)
	return out, nil
}

func (u *upstreamSet) apply(addrs []string, err error, now time.Time) (changed bool, dropped int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if err != nil {
		u.lastError = err.Error()
		return false, 0
	}
	u.lastError = ""
	u.resolved = now
	if equalStrings(u.addrs, addrs) {
		return false, 0
	}
	keep := map[string]bool{}
	for _, a := range addrs {
		keep[a] = true
	}
	u.addrs = append([]string(nil), addrs...)
	for c := range u.conns {
		if !keep[c.upstream] {
			delete(u.conns, c)
			go c.close()
			dropped++
		}
	}
	return true, dropped
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (u *upstreamSet) candidates() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	n := len(u.addrs)
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, u.addrs[(u.next+i)%n])
	}
	if n > 0 {
		u.next = (u.next + 1) % n
	}
	return out
}

func (u *upstreamSet) register(c *relayConn) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, a := range u.addrs {
		if a == c.upstream {
			u.conns[c] = struct{}{}
			return true
		}
	}
	return false
}

func (u *upstreamSet) unregister(c *relayConn) {
	u.mu.Lock()
	delete(u.conns, c)
	u.mu.Unlock()
}

func (u *upstreamSet) closeAll() {
	u.mu.Lock()
	conns := make([]*relayConn, 0, len(u.conns))
	for c := range u.conns {
		conns = append(conns, c)
	}
	u.conns = map[*relayConn]struct{}{}
	u.mu.Unlock()
	for _, c := range conns {
		c.close()
	}
}

func serveProxyRelay(ctx context.Context, ln net.Listener, ups *upstreamSet) {
	for {
		client, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) || ctx.Err() != nil {
				return
			}
			log.Printf("relay accept: %v", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		go relayToUpstream(ctx, client, ups)
	}
}

func relayToUpstream(ctx context.Context, client net.Conn, ups *upstreamSet) {
	var server net.Conn
	var upstream string
	var lastErr error
	for _, addr := range ups.candidates() {
		dctx, cancel := context.WithTimeout(ctx, upstreamDialTimeout)
		var d net.Dialer
		conn, err := d.DialContext(dctx, "tcp", addr)
		cancel()
		if err == nil {
			server, upstream = conn, addr
			break
		}
		lastErr = err
	}
	if server == nil {
		if lastErr == nil {
			lastErr = errors.New("no resolved proxy address")
		}
		log.Printf("relay: proxy unreachable: %v", lastErr)
		_ = client.Close()
		return
	}
	rc := &relayConn{upstream: upstream, client: client, server: server}
	if !ups.register(rc) {
		rc.close()
		return
	}
	defer ups.unregister(rc)
	pipe(rc.client, rc.server)
	rc.close()
}

func pipe(a, b net.Conn) {
	var wg sync.WaitGroup
	copyHalf := func(dst, src net.Conn) {
		defer wg.Done()
		_, _ = io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		} else {
			_ = dst.Close()
		}
	}
	wg.Add(2)
	go copyHalf(a, b)
	go copyHalf(b, a)
	wg.Wait()
}

func serveStatusRelay(ctx context.Context, ln net.Listener, nsPath string, targetPort int) {
	target := net.JoinHostPort("127.0.0.1", strconv.Itoa(targetPort))
	for {
		client, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) || ctx.Err() != nil {
				return
			}
			time.Sleep(100 * time.Millisecond)
			continue
		}
		go func() {
			dctx, cancel := context.WithTimeout(ctx, statusDialTimeout)
			server, err := dialInNetns(dctx, nsPath, target)
			cancel()
			if err != nil {
				_ = client.Close()
				return
			}
			pipe(client, server)
			_ = client.Close()
			_ = server.Close()
		}()
	}
}
