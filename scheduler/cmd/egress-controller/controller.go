//go:build linux

package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"trading-scheduler/internal/egresspolicy"
)

const (
	verifyInterval    = 15 * time.Second
	anchorPollTimeout = 2 * time.Second
	controlIOTimeout  = 10 * time.Second
	statusRelayRetry  = 2 * time.Second
)

var errPeerNamespace = errors.New("namespace mismatch")

type controller struct {
	m *Manifest
	t tools

	mu              sync.Mutex
	generation      uint64
	netnsInode      uint64
	rulesVerifiedAt time.Time
	rulesErr        string
	anchorPID       int
	relayLn         net.Listener
	relayAddr       string
	statusRelayErrs map[string]string

	ups    *upstreamSet
	notify *notifyRuntime

	ctx context.Context
}

func runServe(m *Manifest) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("the egress controller must run as root")
	}
	t, err := loadTools()
	if err != nil {
		return err
	}
	ctx, cancel := signalContext()
	defer cancel()
	c := &controller{m: m, t: t, ups: newUpstreamSet(), ctx: ctx}
	if err := c.prepareRunDir(); err != nil {
		return err
	}
	if m.Mode == modeHost {
		if err := writeResolverFiles(m, c.hostResolverDir()); err != nil {
			return err
		}
	}
	if err := c.ensureNamespace(); err != nil {
		if m.Mode == modeHost {
			return err
		}
		log.Printf("waiting for the anchor container: %v", err)
	}
	if m.Notify != nil {
		c.notify = newNotifyRuntime(c)
		if err := c.notify.setup(); err != nil {
			if m.Mode == modeHost {
				return err
			}
			log.Printf("waiting for the notification broker container: %v", err)
		}
	}
	c.refreshUpstreams()
	ctl, err := c.listenControl()
	if err != nil {
		return err
	}
	defer ctl.Close()
	go c.serveControl(ctl)
	for _, sr := range m.StatusRelays {
		go c.runStatusRelay(sr)
	}
	log.Printf("egress controller serving deployment %s (%s mode, proxy %s)", m.Deployment, m.Mode, m.Proxy)
	resolveTick := time.NewTicker(time.Duration(m.ResolveSeconds) * time.Second)
	defer resolveTick.Stop()
	verifyTick := time.NewTicker(verifyInterval)
	defer verifyTick.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Printf("egress controller stopping; the namespace rules stay installed and the relays close")
			c.mu.Lock()
			if c.relayLn != nil {
				_ = c.relayLn.Close()
			}
			c.mu.Unlock()
			c.ups.closeAll()
			if c.notify != nil {
				c.notify.close()
			}
			return nil
		case <-resolveTick.C:
			c.refreshUpstreams()
		case <-verifyTick.C:
			if err := c.ensureNamespace(); err != nil {
				log.Printf("namespace check: %v", err)
			}
			if c.notify != nil {
				if err := c.notify.setup(); err != nil {
					log.Printf("notification namespace check: %v", err)
				}
			}
		}
	}
}

func (c *controller) setStatusRelayErr(listen, msg string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.statusRelayErrs == nil {
		c.statusRelayErrs = map[string]string{}
	}
	if c.statusRelayErrs[listen] == msg {
		return false
	}
	if msg == "" {
		delete(c.statusRelayErrs, listen)
	} else {
		c.statusRelayErrs[listen] = msg
	}
	return true
}

func (c *controller) runStatusRelay(sr StatusRelay) {
	for {
		ln, err := net.Listen("tcp", sr.Listen)
		if err == nil {
			c.setStatusRelayErr(sr.Listen, "")
			log.Printf("status relay %s -> namespace 127.0.0.1:%d", sr.Listen, sr.TargetPort)
			go func() {
				<-c.ctx.Done()
				_ = ln.Close()
			}()
			serveStatusRelay(c.ctx, ln, c.m.namespacePath(), sr.TargetPort)
			return
		}
		msg := fmt.Sprintf("status relay %s: %v (retrying every %s; the proxy relay and attestation are unaffected)", sr.Listen, err, statusRelayRetry)
		if c.setStatusRelayErr(sr.Listen, msg) {
			log.Print(msg)
		}
		select {
		case <-c.ctx.Done():
			return
		case <-time.After(statusRelayRetry):
		}
	}
}

func (c *controller) hostResolverDir() string {
	return filepath.Join(c.m.runDir(), "resolver")
}

func (c *controller) prepareRunDir() error {
	if err := os.MkdirAll(egresspolicy.ControlRoot, 0o755); err != nil {
		return err
	}
	if err := os.Chmod(egresspolicy.ControlRoot, 0o755); err != nil {
		return err
	}
	dir := c.m.runDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.Chown(dir, 0, 0); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		return err
	}
	return requireRootOwned(dir, false)
}

func (c *controller) refreshUpstreams() {
	addrs, err := resolveUpstreams(c.ctx, c.m.Proxy)
	changed, dropped := c.ups.apply(addrs, err, time.Now().UTC())
	switch {
	case err != nil:
		log.Printf("proxy resolution failed; keeping the last address set: %v", err)
	case changed:
		log.Printf("proxy addresses now %s (closed %d relayed connections to removed addresses)", strings.Join(addrs, ", "), dropped)
	}
}

func (c *controller) ensureNamespace() error {
	ns := c.m.namespaceName()
	switch c.m.Mode {
	case modeHost:
		if err := c.t.createNetns(ns); err != nil {
			return err
		}
	case modeDocker:
		info, err := inspectContainer(c.m.Docker.CLI, c.m.Docker.AnchorContainer, c.m.Deployment, "anchor")
		if err != nil {
			c.markUnverified(err)
			return err
		}
		c.mu.Lock()
		moved := c.anchorPID != 0 && c.anchorPID != info.PID
		c.mu.Unlock()
		if moved {
			c.closeRelay()
		}
		if err := c.t.attachNetns(ns, info.PID); err != nil {
			c.markUnverified(err)
			return err
		}
		if err := writeDockerResolverFiles(c.m, info); err != nil {
			c.markUnverified(err)
			return err
		}
		c.mu.Lock()
		c.anchorPID = info.PID
		c.mu.Unlock()
	}
	return c.enforce(ns)
}

func (c *controller) markUnverified(err error) {
	c.mu.Lock()
	c.rulesErr = err.Error()
	c.rulesVerifiedAt = time.Time{}
	c.mu.Unlock()
}

func (c *controller) enforce(ns string) error {
	nsPath := "/run/netns/" + ns
	inode, err := egresspolicy.NamespaceInode(nsPath)
	if err != nil {
		c.markUnverified(err)
		return err
	}
	c.mu.Lock()
	prevInode := c.netnsInode
	c.mu.Unlock()
	gen, verr := c.t.verifyRules(ns, c.m)
	if verr != nil || inode != prevInode {
		if verr != nil {
			log.Printf("namespace %s rules need installing: %v", ns, verr)
		}
		next := gen + 1
		c.mu.Lock()
		if c.generation >= next {
			next = c.generation + 1
		}
		c.mu.Unlock()
		if err := c.t.installRules(ns, c.m, next); err != nil {
			c.markUnverified(err)
			return err
		}
		if gen, verr = c.t.verifyRules(ns, c.m); verr != nil {
			c.markUnverified(verr)
			return verr
		}
		log.Printf("namespace %s: rules generation %d installed and verified", ns, gen)
	}
	var proxyIP net.IP
	if ip := net.ParseIP(c.m.Proxy.Host); ip != nil {
		proxyIP = ip
	}
	if err := c.t.prepareLoopback(ns, proxyIP); err != nil {
		c.markUnverified(err)
		return err
	}
	if err := c.ensureRelay(nsPath, inode); err != nil {
		c.markUnverified(err)
		return err
	}
	c.mu.Lock()
	c.generation = gen
	c.netnsInode = inode
	c.rulesVerifiedAt = time.Now().UTC()
	c.rulesErr = ""
	c.mu.Unlock()
	return nil
}

func (c *controller) closeRelay() {
	c.mu.Lock()
	ln := c.relayLn
	c.relayLn = nil
	c.mu.Unlock()
	if ln != nil {
		_ = ln.Close()
	}
	c.ups.closeAll()
}

func (c *controller) ensureRelay(nsPath string, inode uint64) error {
	c.mu.Lock()
	sameNs := c.relayLn != nil && c.netnsInode == inode
	c.mu.Unlock()
	if sameNs {
		return nil
	}
	c.closeRelay()
	addr := net.JoinHostPort(relayListenHost(c.m.Proxy), strconv.Itoa(c.m.Proxy.Port))
	ln, err := listenInNetns(nsPath, addr)
	if err != nil {
		return fmt.Errorf("proxy relay listener %s in the namespace: %w", addr, err)
	}
	c.mu.Lock()
	c.relayLn = ln
	c.relayAddr = addr
	c.mu.Unlock()
	go serveProxyRelay(c.ctx, ln, c.ups)
	log.Printf("proxy relay listening at %s inside the namespace", addr)
	return nil
}

func (c *controller) status() *egresspolicy.ControllerStatus {
	addrs, resolved, resolveErr, conns := c.ups.snapshot()
	c.mu.Lock()
	defer c.mu.Unlock()
	var relayErrs []string
	for _, msg := range c.statusRelayErrs {
		relayErrs = append(relayErrs, msg)
	}
	sort.Strings(relayErrs)
	return &egresspolicy.ControllerStatus{
		StatusRelayErrors: relayErrs,
		Deployment:        c.m.Deployment,
		Mode:              c.m.Mode,
		Ready:             c.readyLocked(len(addrs) > 0),
		Generation:        c.generation,
		NetnsInode:        c.netnsInode,
		Proxy:             c.m.Proxy,
		RelayAddress:      c.relayAddr,
		Upstreams:         addrs,
		LastResolveAt:     resolved,
		LastResolveError:  resolveErr,
		RelayConnections:  conns,
		RulesVerifiedAt:   c.rulesVerifiedAt,
		RulesError:        c.rulesErr,
		ControllerVersion: controllerVersion(),
	}
}

func (c *controller) readyLocked(haveUpstreams bool) bool {
	return haveUpstreams && c.relayLn != nil && c.generation > 0 && c.rulesErr == "" && !c.rulesVerifiedAt.IsZero()
}

func (c *controller) listenControl() (net.Listener, error) {
	path := egresspolicy.ControlSocketPath(c.m.Deployment)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o666); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

func (c *controller) knownPeer(uid int) bool {
	return uid == 0 || uid == c.m.WorkerUID || (c.m.Notify != nil && uid == c.m.Notify.BrokerUID)
}

const maxControlConnections = 32

func (c *controller) serveControl(ln net.Listener) {
	slots := make(chan struct{}, maxControlConnections)
	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			time.Sleep(100 * time.Millisecond)
			continue
		}
		select {
		case slots <- struct{}{}:
			go func() {
				defer func() { <-slots }()
				c.handleControl(conn)
			}()
		default:
			_ = conn.Close()
		}
	}
}

func (c *controller) handleControl(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(controlIOTimeout))
	resp := egresspolicy.Response{Version: egresspolicy.ProtocolVersion}
	reply := func() { _ = egresspolicy.WriteMessage(conn, resp) }
	cred, err := egresspolicy.PeerCredentials(conn)
	if err != nil {
		resp.Error = "peer credentials unavailable"
		reply()
		return
	}
	if !c.knownPeer(cred.UID) {
		resp.Error = "this account may not use the egress control socket"
		reply()
		return
	}
	var req egresspolicy.Request
	if err := egresspolicy.ReadMessage(bufio.NewReaderSize(conn, egresspolicy.MaxMessageBytes), &req); err != nil {
		resp.Error = "malformed request"
		reply()
		return
	}
	if req.Version != egresspolicy.ProtocolVersion {
		resp.Error = fmt.Sprintf("protocol %d is not supported; this controller speaks protocol %d", req.Version, egresspolicy.ProtocolVersion)
		reply()
		return
	}
	if req.Deployment != c.m.Deployment {
		resp.Error = fmt.Sprintf("this controller serves deployment %q", c.m.Deployment)
		reply()
		return
	}
	switch req.Op {
	case egresspolicy.OpAttest:
		role := req.Role
		if role == "" {
			role = egresspolicy.RoleWorker
		}
		a, err := c.attest(cred, role)
		if err != nil {
			log.Printf("attestation refused for pid %d uid %d role %s: %v", cred.PID, cred.UID, role, err)
			resp.Error = err.Error()
			if errors.Is(err, errPeerNamespace) {
				resp.Code = egresspolicy.CodeNamespaceMismatch
			}
			reply()
			return
		}
		resp.OK = true
		resp.Attestation = a
	case egresspolicy.OpStatus:
		if cred.UID != 0 && cred.UID != c.m.WorkerUID && (c.m.Notify == nil || cred.UID != c.m.Notify.BrokerUID) {
			resp.Error = "status is limited to root, the worker and the notification broker"
			reply()
			return
		}
		resp.OK = true
		resp.Status = c.status()
	case egresspolicy.OpRestartApp:
		if _, err := c.attest(cred, egresspolicy.RoleWorker); err != nil {
			resp.Error = "restart refused: " + err.Error()
			reply()
			return
		}
		if err := c.restartApp(); err != nil {
			resp.Error = err.Error()
			reply()
			return
		}
		resp.OK = true
	default:
		resp.Error = fmt.Sprintf("unknown operation %q", req.Op)
	}
	reply()
}

func (c *controller) attest(cred egresspolicy.PeerCred, role string) (*egresspolicy.Attestation, error) {
	var wantUID int
	var nsName string
	switch role {
	case egresspolicy.RoleWorker:
		wantUID = c.m.WorkerUID
		nsName = c.m.namespaceName()
	case egresspolicy.RoleNotify:
		if c.notify == nil {
			return nil, fmt.Errorf("this deployment has no notification broker")
		}
		wantUID = c.m.Notify.BrokerUID
		nsName = c.notify.namespaceName()
	default:
		return nil, fmt.Errorf("unknown role %q", role)
	}
	if cred.UID != wantUID {
		return nil, fmt.Errorf("uid %d is not the %s uid", cred.UID, role)
	}
	if cred.PID <= 0 {
		return nil, fmt.Errorf("the peer pid is unknown")
	}
	before, err := egresspolicy.InspectProcess(cred.PID)
	if err != nil {
		return nil, fmt.Errorf("inspect the peer process: %v", err)
	}
	peerNs, err := egresspolicy.ProcessNetnsInode(cred.PID)
	if err != nil {
		return nil, fmt.Errorf("read the peer network namespace: %v", err)
	}
	after, err := egresspolicy.InspectProcess(cred.PID)
	if err != nil || after.StartTime != before.StartTime {
		return nil, fmt.Errorf("the peer process changed during inspection")
	}
	if before.UID != wantUID {
		return nil, fmt.Errorf("the peer process runs as uid %d", before.UID)
	}
	if before.CapEff != 0 || before.CapPrm != 0 || before.CapAmb != 0 {
		return nil, fmt.Errorf("the peer process holds capabilities (effective %x, permitted %x, ambient %x)", before.CapEff, before.CapPrm, before.CapAmb)
	}
	if !before.NoNewPrivs {
		return nil, fmt.Errorf("the peer process can gain privileges (no_new_privs is off)")
	}
	nsPath := "/run/netns/" + nsName
	nsInode, err := egresspolicy.NamespaceInode(nsPath)
	if err != nil {
		return nil, fmt.Errorf("the managed namespace is missing: %v", err)
	}
	if peerNs != nsInode {
		return nil, fmt.Errorf("%w: the peer is in network namespace %d, not the managed namespace %d", errPeerNamespace, peerNs, nsInode)
	}
	if c.m.Mode == modeHost {
		if pids := c.t.accountProcessesUnenforced(wantUID, nsInode); len(pids) > 0 {
			return nil, fmt.Errorf("the %s account also runs processes %v outside every enforced egress namespace; every process of the account must run in an enforced namespace so no worker can reach an unconfined process", role, pids)
		}
	}
	gen, err := c.t.verifyRules(nsName, c.m)
	if err != nil {
		c.markUnverified(err)
		return nil, fmt.Errorf("the namespace rules do not verify: %v", err)
	}
	a := &egresspolicy.Attestation{
		Deployment:        c.m.Deployment,
		Generation:        gen,
		NetnsInode:        nsInode,
		Proxy:             c.m.Proxy,
		ControllerVersion: controllerVersion(),
		IssuedAt:          time.Now().UTC(),
		Role:              role,
		WorkerUID:         c.m.WorkerUID,
	}
	if role == egresspolicy.RoleWorker {
		addrs, _, _, _ := c.ups.snapshot()
		c.mu.Lock()
		ready := c.readyLocked(len(addrs) > 0) && c.netnsInode == nsInode
		a.RelayAddress = c.relayAddr
		c.mu.Unlock()
		if !ready {
			return nil, fmt.Errorf("the egress controller is not ready (proxy addresses %d, see the controller status)", len(addrs))
		}
		a.UpstreamAddresses = len(addrs)
	} else if !c.notify.ready() {
		return nil, fmt.Errorf("the notification namespace is not ready")
	}
	if c.notify != nil {
		a.Notify = c.notify.attestation()
	}
	return a, nil
}

func (c *controller) restartApp() error {
	if c.m.Mode != modeHost {
		return fmt.Errorf("restart is a host-mode operation; container deployments restart through Docker")
	}
	systemctl, err := findTool("systemctl")
	if err != nil {
		return err
	}
	_, err = runTool(nil, systemctl, "restart", "--no-block", c.m.Host.AppUnit)
	return err
}
