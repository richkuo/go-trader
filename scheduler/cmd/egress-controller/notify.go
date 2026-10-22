//go:build linux

package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"trading-scheduler/internal/egresspolicy"
)

const (
	notifyProxyAddress = "127.77.0.2:3128"
	notifyDirName      = "notify"
	notifySocketName   = "broker.sock"
	notifyHeaderLimit  = 8 << 10
)

type hostRule struct {
	exact  string
	suffix string
}

var notifyDestinations = map[string][]hostRule{
	"discord":  {{exact: "discord.com"}, {exact: "gateway.discord.gg"}, {suffix: ".discord.gg"}},
	"telegram": {{exact: "api.telegram.org"}},
	"github":   {{exact: "api.github.com"}},
}

type notifyRuntime struct {
	c *controller

	mu         sync.Mutex
	ln         net.Listener
	nsInode    uint64
	generation uint64
	verified   bool
	lastErr    string
	brokerPID  int
}

func newNotifyRuntime(c *controller) *notifyRuntime {
	return &notifyRuntime{c: c}
}

func (n *notifyRuntime) namespaceName() string {
	return n.c.m.notifyNamespaceName()
}

func (n *notifyRuntime) destinations() []string {
	d := n.c.m.Notify.Destinations
	if len(d) == 0 {
		for k := range notifyDestinations {
			d = append(d, k)
		}
	}
	out := append([]string(nil), d...)
	sort.Strings(out)
	return out
}

func (n *notifyRuntime) socketDir() string {
	return filepath.Join(n.c.m.runDir(), notifyDirName)
}

func (n *notifyRuntime) attestation() *egresspolicy.NotifyAttestation {
	return &egresspolicy.NotifyAttestation{
		BrokerUID:    n.c.m.Notify.BrokerUID,
		Socket:       filepath.Join(n.socketDir(), notifySocketName),
		ProxyAddress: notifyProxyAddress,
		Destinations: n.destinations(),
	}
}

func (n *notifyRuntime) ready() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.verified && n.ln != nil
}

func (n *notifyRuntime) fail(err error) error {
	n.mu.Lock()
	n.verified = false
	n.lastErr = err.Error()
	n.mu.Unlock()
	return err
}

func (n *notifyRuntime) setup() error {
	m := n.c.m
	dir := n.socketDir()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return n.fail(err)
	}
	if err := os.Chown(dir, m.Notify.BrokerUID, m.WorkerGID); err != nil {
		return n.fail(err)
	}
	if err := os.Chmod(dir, 0o750); err != nil {
		return n.fail(err)
	}
	ns := n.namespaceName()
	switch m.Mode {
	case modeHost:
		if err := n.c.t.createNetns(ns); err != nil {
			return n.fail(err)
		}
	case modeDocker:
		info, err := inspectContainer(m.Docker.CLI, m.Docker.NotifyContainer, m.Deployment, "notify")
		if err != nil {
			return n.fail(err)
		}
		n.mu.Lock()
		moved := n.brokerPID != 0 && n.brokerPID != info.PID
		n.mu.Unlock()
		if moved {
			n.closeListener()
		}
		if err := n.c.t.attachNetns(ns, info.PID); err != nil {
			return n.fail(err)
		}
		n.mu.Lock()
		n.brokerPID = info.PID
		n.mu.Unlock()
	}
	nsPath := "/run/netns/" + ns
	inode, err := egresspolicy.NamespaceInode(nsPath)
	if err != nil {
		return n.fail(err)
	}
	gen, verr := n.c.t.verifyRules(ns, m)
	n.mu.Lock()
	prevInode := n.nsInode
	n.mu.Unlock()
	if verr != nil || inode != prevInode {
		next := gen + 1
		if err := n.c.t.installRules(ns, m, next); err != nil {
			return n.fail(err)
		}
		if gen, verr = n.c.t.verifyRules(ns, m); verr != nil {
			return n.fail(verr)
		}
		log.Printf("notification namespace %s: rules generation %d installed and verified", ns, gen)
	}
	if err := n.c.t.prepareLoopback(ns, nil); err != nil {
		return n.fail(err)
	}
	n.mu.Lock()
	sameNs := n.ln != nil && n.nsInode == inode
	n.mu.Unlock()
	if !sameNs {
		n.closeListener()
		ln, err := listenInNetns(nsPath, notifyProxyAddress)
		if err != nil {
			return n.fail(fmt.Errorf("notification proxy listener: %w", err))
		}
		n.mu.Lock()
		n.ln = ln
		n.mu.Unlock()
		go n.serve(ln)
		log.Printf("notification proxy listening at %s inside %s for %s", notifyProxyAddress, ns, strings.Join(n.destinations(), ", "))
	}
	n.mu.Lock()
	n.nsInode = inode
	n.generation = gen
	n.verified = true
	n.lastErr = ""
	n.mu.Unlock()
	return nil
}

func (n *notifyRuntime) closeListener() {
	n.mu.Lock()
	ln := n.ln
	n.ln = nil
	n.mu.Unlock()
	if ln != nil {
		_ = ln.Close()
	}
}

func (n *notifyRuntime) close() {
	n.closeListener()
}

func (n *notifyRuntime) allowed(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if net.ParseIP(host) != nil {
		return false
	}
	for _, d := range n.destinations() {
		for _, r := range notifyDestinations[d] {
			if r.exact != "" && host == r.exact {
				return true
			}
			if r.suffix != "" && strings.HasSuffix(host, r.suffix) && len(host) > len(r.suffix) {
				return true
			}
		}
	}
	return false
}

func publicAddress(ip net.IP) bool {
	return ip.IsGlobalUnicast() && !ip.IsPrivate() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() && !cgnat.Contains(ip)
}

var cgnat = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

func (n *notifyRuntime) serve(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			time.Sleep(100 * time.Millisecond)
			continue
		}
		go n.handle(conn)
	}
}

func (n *notifyRuntime) handle(client net.Conn) {
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(controlIOTimeout))
	br := bufio.NewReaderSize(io.LimitReader(client, notifyHeaderLimit), notifyHeaderLimit)
	req, err := http.ReadRequest(br)
	if err != nil {
		writeProxyStatus(client, http.StatusBadRequest)
		return
	}
	if req.Method != http.MethodConnect {
		writeProxyStatus(client, http.StatusMethodNotAllowed)
		return
	}
	host, port, err := net.SplitHostPort(req.Host)
	if err != nil || port != "443" || !n.allowed(host) {
		log.Printf("notification proxy refused CONNECT %q", req.Host)
		writeProxyStatus(client, http.StatusForbidden)
		return
	}
	ctx, cancel := context.WithTimeout(n.c.ctx, upstreamDialTimeout)
	defer cancel()
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		writeProxyStatus(client, http.StatusBadGateway)
		return
	}
	var server net.Conn
	for _, a := range ips {
		if !publicAddress(a.IP) {
			continue
		}
		var d net.Dialer
		conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(a.IP.String(), "443"))
		if err == nil {
			server = conn
			break
		}
	}
	if server == nil {
		writeProxyStatus(client, http.StatusBadGateway)
		return
	}
	defer server.Close()
	if _, err := io.WriteString(client, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		return
	}
	_ = client.SetDeadline(time.Time{})
	if br.Buffered() > 0 {
		buffered, _ := br.Peek(br.Buffered())
		if _, err := server.Write(buffered); err != nil {
			return
		}
	}
	pipe(client, server)
}

func writeProxyStatus(w io.Writer, code int) {
	_, _ = fmt.Fprintf(w, "HTTP/1.1 %d %s\r\nContent-Length: 0\r\nConnection: close\r\n\r\n", code, http.StatusText(code))
}
