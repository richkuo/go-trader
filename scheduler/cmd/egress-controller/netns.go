//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"trading-scheduler/internal/egresspolicy"
)

const (
	nftTable      = "go_trader_egress"
	toolTimeout   = 20 * time.Second
	commentPrefix = "go-trader-egress"
)

var toolSearchDirs = []string{"/usr/sbin", "/sbin", "/usr/bin", "/bin"}

type tools struct {
	ip  string
	nft string
}

func findTool(name string) (string, error) {
	for _, dir := range toolSearchDirs {
		p := filepath.Join(dir, name)
		resolved, err := filepath.EvalSymlinks(p)
		if err != nil {
			continue
		}
		fi, err := os.Stat(resolved)
		if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 {
			continue
		}
		if err := requireRootOwned(resolved, false); err != nil {
			return "", fmt.Errorf("%s: %v", resolved, err)
		}
		return resolved, nil
	}
	return "", fmt.Errorf("%s is not installed in %s", name, strings.Join(toolSearchDirs, ", "))
}

func loadTools() (tools, error) {
	ip, err := findTool("ip")
	if err != nil {
		return tools{}, fmt.Errorf("iproute2: %w", err)
	}
	nft, err := findTool("nft")
	if err != nil {
		return tools{}, fmt.Errorf("nftables: %w", err)
	}
	return tools{ip: ip, nft: nft}, nil
}

func runTool(stdin []byte, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), toolTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL=C"}
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), fmt.Errorf("%s %s: %v: %s", filepath.Base(name), strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

func (t tools) inNetns(ns string, stdin []byte, name string, args ...string) ([]byte, error) {
	full := append([]string{"netns", "exec", ns, name}, args...)
	return runTool(stdin, t.ip, full...)
}

func netnsExists(ns string) bool {
	_, err := os.Stat("/run/netns/" + ns)
	return err == nil
}

func (t tools) createNetns(ns string) error {
	if netnsExists(ns) {
		return nil
	}
	_, err := runTool(nil, t.ip, "netns", "add", ns)
	return err
}

func (t tools) attachNetns(ns string, pid int) error {
	want, err := egresspolicy.ProcessNetnsInode(pid)
	if err != nil {
		return err
	}
	if netnsExists(ns) {
		have, err := egresspolicy.NamespaceInode("/run/netns/" + ns)
		if err == nil && have == want {
			return nil
		}
		if _, err := runTool(nil, t.ip, "netns", "delete", ns); err != nil {
			return err
		}
	}
	_, err = runTool(nil, t.ip, "netns", "attach", ns, strconv.Itoa(pid))
	return err
}

type linkInfo struct {
	IfName string   `json:"ifname"`
	Flags  []string `json:"flags"`
}

func (t tools) upLinks(ns string) ([]string, error) {
	out, err := runTool(nil, t.ip, "-n", ns, "-j", "link", "show")
	if err != nil {
		return nil, err
	}
	var links []linkInfo
	if err := json.Unmarshal(out, &links); err != nil {
		return nil, fmt.Errorf("parse ip link output: %v", err)
	}
	var names []string
	for _, l := range links {
		if l.IfName == "lo" {
			continue
		}
		for _, f := range l.Flags {
			if f == "UP" {
				names = append(names, l.IfName)
				break
			}
		}
	}
	return names, nil
}

func (t tools) prepareLoopback(ns string, extra net.IP) error {
	if _, err := runTool(nil, t.ip, "-n", ns, "link", "set", "lo", "up"); err != nil {
		return err
	}
	if extra == nil || extra.IsLoopback() {
		return nil
	}
	prefix := "/32"
	if extra.To4() == nil {
		prefix = "/128"
	}
	out, err := runTool(nil, t.ip, "-n", ns, "-j", "addr", "show", "dev", "lo")
	if err != nil {
		return err
	}
	if bytes.Contains(out, []byte(`"local":"`+extra.String()+`"`)) {
		return nil
	}
	_, err = runTool(nil, t.ip, "-n", ns, "addr", "add", extra.String()+prefix, "dev", "lo")
	return err
}

func rulesetText(m *Manifest, generation uint64) string {
	comment := fmt.Sprintf("%s deployment=%s generation=%d", commentPrefix, m.Deployment, generation)
	var b strings.Builder
	fmt.Fprintf(&b, "table inet %s\n", nftTable)
	fmt.Fprintf(&b, "delete table inet %s\n", nftTable)
	fmt.Fprintf(&b, "table inet %s {\n", nftTable)
	fmt.Fprintf(&b, "\tcomment %q\n", comment)
	fmt.Fprintf(&b, "\tchain output {\n\t\ttype filter hook output priority filter; policy drop;\n\t\toifname \"lo\" accept comment %q\n\t}\n", comment)
	b.WriteString("\tchain input {\n\t\ttype filter hook input priority filter; policy drop;\n\t\tiifname \"lo\" accept\n\t}\n")
	b.WriteString("\tchain forward {\n\t\ttype filter hook forward priority filter; policy drop;\n\t}\n")
	b.WriteString("}\n")
	return b.String()
}

func (t tools) installRules(ns string, m *Manifest, generation uint64) error {
	_, err := t.inNetns(ns, []byte(rulesetText(m, generation)), t.nft, "-f", "-")
	return err
}

type nftDoc struct {
	Nftables []map[string]json.RawMessage `json:"nftables"`
}

type nftTableObj struct {
	Family  string `json:"family"`
	Name    string `json:"name"`
	Comment string `json:"comment"`
}

type nftChainObj struct {
	Family string `json:"family"`
	Table  string `json:"table"`
	Name   string `json:"name"`
	Type   string `json:"type"`
	Hook   string `json:"hook"`
	Policy string `json:"policy"`
}

type nftRuleObj struct {
	Family  string            `json:"family"`
	Table   string            `json:"table"`
	Chain   string            `json:"chain"`
	Comment string            `json:"comment"`
	Expr    []json.RawMessage `json:"expr"`
}

var expectedRules = map[string]string{
	"output": `[{"match":{"op":"==","left":{"meta":{"key":"oifname"}},"right":"lo"}},{"accept":null}]`,
	"input":  `[{"match":{"op":"==","left":{"meta":{"key":"iifname"}},"right":"lo"}},{"accept":null}]`,
}

func canonicalJSON(raw []json.RawMessage) (string, error) {
	var v any
	data, err := json.Marshal(raw)
	if err != nil {
		return "", err
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return "", err
	}
	out, err := json.Marshal(v)
	return string(out), err
}

func canonicalString(s string) string {
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return s
	}
	out, _ := json.Marshal(v)
	return string(out)
}

func (t tools) verifyRules(ns string, m *Manifest) (uint64, error) {
	out, err := t.inNetns(ns, nil, t.nft, "-j", "list", "table", "inet", nftTable)
	if err != nil {
		return 0, fmt.Errorf("the %s table is not installed: %v", nftTable, err)
	}
	var doc nftDoc
	if err := json.Unmarshal(out, &doc); err != nil {
		return 0, fmt.Errorf("parse nft output: %v", err)
	}
	var generation uint64
	chains := map[string]nftChainObj{}
	rules := map[string][]string{}
	for _, obj := range doc.Nftables {
		if raw, ok := obj["table"]; ok {
			var tbl nftTableObj
			if err := json.Unmarshal(raw, &tbl); err != nil {
				return 0, err
			}
			if tbl.Family != "inet" || tbl.Name != nftTable {
				return 0, fmt.Errorf("unexpected table %s %s", tbl.Family, tbl.Name)
			}
		}
		if raw, ok := obj["chain"]; ok {
			var c nftChainObj
			if err := json.Unmarshal(raw, &c); err != nil {
				return 0, err
			}
			chains[c.Name] = c
		}
		if raw, ok := obj["rule"]; ok {
			var r nftRuleObj
			if err := json.Unmarshal(raw, &r); err != nil {
				return 0, err
			}
			expr, err := canonicalJSON(r.Expr)
			if err != nil {
				return 0, err
			}
			rules[r.Chain] = append(rules[r.Chain], expr)
			if r.Chain == "output" {
				want := fmt.Sprintf("%s deployment=%s generation=", commentPrefix, m.Deployment)
				if !strings.HasPrefix(r.Comment, want) {
					return 0, fmt.Errorf("the %s table belongs to another deployment or has no generation", nftTable)
				}
				generation, err = strconv.ParseUint(strings.TrimPrefix(r.Comment, want), 10, 64)
				if err != nil {
					return 0, fmt.Errorf("the %s table generation does not parse", nftTable)
				}
			}
		}
		for key := range obj {
			switch key {
			case "metainfo", "table", "chain", "rule":
			default:
				return 0, fmt.Errorf("the %s table holds an unexpected %s object", nftTable, key)
			}
		}
	}
	if generation == 0 {
		return 0, fmt.Errorf("the %s table has no generation", nftTable)
	}
	if len(chains) != 3 {
		return 0, fmt.Errorf("the %s table has %d chains, want 3", nftTable, len(chains))
	}
	for _, name := range []string{"output", "input", "forward"} {
		c, ok := chains[name]
		if !ok || c.Type != "filter" || c.Hook != name || c.Policy != "drop" {
			return 0, fmt.Errorf("chain %s is missing or does not drop by default", name)
		}
		want := expectedRules[name]
		got := rules[name]
		if want == "" {
			if len(got) != 0 {
				return 0, fmt.Errorf("chain %s has %d rules, want none", name, len(got))
			}
			continue
		}
		if len(got) != 1 || got[0] != canonicalString(want) {
			return 0, fmt.Errorf("chain %s does not hold exactly the loopback rule", name)
		}
	}
	up, err := t.upLinks(ns)
	if err != nil {
		return 0, err
	}
	if len(up) > 0 {
		return 0, fmt.Errorf("the namespace has links %v up; only lo may be up", up)
	}
	return generation, nil
}

func (t tools) deleteNetns(ns string) error {
	if !netnsExists(ns) {
		return nil
	}
	_, err := runTool(nil, t.ip, "netns", "delete", ns)
	return err
}

func processesInNetns(nsPath string) ([]int, error) {
	inode, err := egresspolicy.NamespaceInode(nsPath)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var pids []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		ino, err := egresspolicy.ProcessNetnsInode(pid)
		if err == nil && ino == inode {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}

func withNetns(nsPath string, fn func() error) error {
	target, err := os.Open(nsPath)
	if err != nil {
		return err
	}
	defer target.Close()
	errc := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		orig, err := os.Open("/proc/thread-self/ns/net")
		if err != nil {
			errc <- err
			return
		}
		defer orig.Close()
		if err := unix.Setns(int(target.Fd()), unix.CLONE_NEWNET); err != nil {
			runtime.UnlockOSThread()
			errc <- fmt.Errorf("enter network namespace %s: %v", nsPath, err)
			return
		}
		ferr := fn()
		if err := unix.Setns(int(orig.Fd()), unix.CLONE_NEWNET); err != nil {
			errc <- errors.Join(ferr, fmt.Errorf("leave network namespace %s: %v", nsPath, err))
			return
		}
		runtime.UnlockOSThread()
		errc <- ferr
	}()
	return <-errc
}

func listenInNetns(nsPath, addr string) (net.Listener, error) {
	var ln net.Listener
	err := withNetns(nsPath, func() error {
		var err error
		ln, err = net.Listen("tcp", addr)
		return err
	})
	return ln, err
}

func dialInNetns(ctx context.Context, nsPath, addr string) (net.Conn, error) {
	var conn net.Conn
	err := withNetns(nsPath, func() error {
		var d net.Dialer
		var err error
		conn, err = d.DialContext(ctx, "tcp", addr)
		return err
	})
	return conn, err
}

func accountProcessesOutside(uid int, nsInode uint64) []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var pids []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		ps, err := egresspolicy.InspectProcess(pid)
		if err != nil || ps.UID != uid {
			continue
		}
		ino, err := egresspolicy.ProcessNetnsInode(pid)
		if err != nil || ino == nsInode {
			continue
		}
		pids = append(pids, pid)
	}
	return pids
}
