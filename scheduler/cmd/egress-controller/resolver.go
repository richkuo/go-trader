//go:build linux

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
)

func hostsFileContent(m *Manifest) []byte {
	var b strings.Builder
	b.WriteString("127.0.0.1\tlocalhost\n")
	b.WriteString("::1\tlocalhost ip6-localhost ip6-loopback\n")
	if net.ParseIP(m.Proxy.Host) == nil && !strings.EqualFold(m.Proxy.Host, "localhost") {
		fmt.Fprintf(&b, "%s\t%s\n", relayListenHost(m.Proxy), m.Proxy.Host)
	}
	return []byte(b.String())
}

const resolvConfContent = "options timeout:1 attempts:1\nnameserver 127.0.0.1\n"

func nsswitchContent(base []byte) []byte {
	var out bytes.Buffer
	sawHosts := false
	sc := bufio.NewScanner(bytes.NewReader(base))
	for sc.Scan() {
		line := sc.Text()
		key := strings.TrimSpace(line)
		if strings.HasPrefix(key, "hosts:") || strings.HasPrefix(key, "networks:") {
			if strings.HasPrefix(key, "hosts:") {
				sawHosts = true
				out.WriteString("hosts: files\n")
			} else {
				out.WriteString("networks: files\n")
			}
			continue
		}
		out.WriteString(line + "\n")
	}
	if !sawHosts {
		out.WriteString("hosts: files\n")
	}
	return out.Bytes()
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func writeInPlace(path string, data []byte) error {
	current, err := os.ReadFile(path)
	if err == nil && bytes.Equal(current, data) {
		return nil
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func writeResolverFiles(m *Manifest, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		return err
	}
	base, err := os.ReadFile("/etc/nsswitch.conf")
	if err != nil {
		base = nil
	}
	files := map[string][]byte{
		"hosts":         hostsFileContent(m),
		"resolv.conf":   []byte(resolvConfContent),
		"nsswitch.conf": nsswitchContent(base),
	}
	for name, data := range files {
		path := filepath.Join(dir, name)
		if _, err := os.Stat(path); err == nil {
			if err := writeInPlace(path, data); err != nil {
				return err
			}
			continue
		}
		if err := writeFileAtomic(path, data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func writeDockerResolverFiles(m *Manifest, info *containerInfo) error {
	for _, p := range []string{info.HostsPath, info.ResolvConfPath} {
		if p == "" || !filepath.IsAbs(p) {
			return fmt.Errorf("the anchor container has no hosts or resolv.conf path")
		}
		if err := requireRootOwned(p, false); err != nil {
			return fmt.Errorf("anchor resolver file: %w", err)
		}
	}
	if err := writeInPlace(info.HostsPath, hostsFileContent(m)); err != nil {
		return err
	}
	return writeInPlace(info.ResolvConfPath, []byte(resolvConfContent))
}

type containerInfo struct {
	PID            int
	HostsPath      string
	ResolvConfPath string
	Revision       string
}

type dockerInspect struct {
	State struct {
		Running bool `json:"Running"`
		Pid     int  `json:"Pid"`
	} `json:"State"`
	HostsPath      string `json:"HostsPath"`
	ResolvConfPath string `json:"ResolvConfPath"`
	HostConfig     struct {
		NetworkMode string   `json:"NetworkMode"`
		Privileged  bool     `json:"Privileged"`
		CapAdd      []string `json:"CapAdd"`
		PidMode     string   `json:"PidMode"`
	} `json:"HostConfig"`
	Config struct {
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
}

func inspectContainer(cli, name, deployment, role string) (*containerInfo, error) {
	if err := requireRootOwned(cli, false); err != nil {
		return nil, fmt.Errorf("docker cli: %w", err)
	}
	out, err := runTool(nil, cli, "inspect", "--type", "container", "--format", "{{json .}}", "--", name)
	if err != nil {
		return nil, err
	}
	var d dockerInspect
	if err := json.Unmarshal(bytes.TrimSpace(out), &d); err != nil {
		return nil, fmt.Errorf("parse docker inspect: %v", err)
	}
	if !d.State.Running || d.State.Pid <= 0 {
		return nil, fmt.Errorf("container %s is not running", name)
	}
	if d.HostConfig.NetworkMode != "none" {
		return nil, fmt.Errorf("container %s uses network mode %q; the %s needs network_mode: none", name, d.HostConfig.NetworkMode, role)
	}
	if d.HostConfig.Privileged || len(d.HostConfig.CapAdd) > 0 {
		return nil, fmt.Errorf("container %s is privileged or adds capabilities", name)
	}
	if d.HostConfig.PidMode == "host" {
		return nil, fmt.Errorf("container %s shares the host process namespace", name)
	}
	label := "io.github.richkuo.go-trader.egress." + role
	if d.Config.Labels[label] != deployment {
		return nil, fmt.Errorf("container %s has label %s=%q, want %q", name, label, d.Config.Labels[label], deployment)
	}
	return &containerInfo{
		PID:            d.State.Pid,
		HostsPath:      d.HostsPath,
		ResolvConfPath: d.ResolvConfPath,
		Revision:       d.Config.Labels["org.opencontainers.image.revision"],
	}, nil
}
