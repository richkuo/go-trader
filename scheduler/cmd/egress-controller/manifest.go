//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"trading-scheduler/internal/egresspolicy"
)

const (
	modeHost   = "host"
	modeDocker = "docker"

	defaultResolveSeconds = 60
	minResolveSeconds     = 10
	maxResolveSeconds     = 3600
	relayHostAddress      = "127.77.0.1"
)

type Manifest struct {
	Deployment     string                `json:"deployment"`
	Mode           string                `json:"mode"`
	Proxy          egresspolicy.Identity `json:"proxy"`
	WorkerUID      int                   `json:"worker_uid"`
	WorkerGID      int                   `json:"worker_gid"`
	ResolveSeconds int                   `json:"resolve_seconds,omitempty"`
	StatusRelays   []StatusRelay         `json:"status_relays,omitempty"`
	Host           *HostManifest         `json:"host,omitempty"`
	Docker         *DockerManifest       `json:"docker,omitempty"`
	Notify         *NotifyManifest       `json:"notify,omitempty"`

	path string
}

type StatusRelay struct {
	Listen     string `json:"listen"`
	TargetPort int    `json:"target_port"`
}

type HostManifest struct {
	AppUnit          string   `json:"app_unit"`
	AppExecutable    string   `json:"app_executable"`
	WorkingDirectory string   `json:"working_directory"`
	EnvironmentFiles []string `json:"environment_files"`
	ReadWritePaths   []string `json:"read_write_paths,omitempty"`
	LogNamespace     string   `json:"log_namespace,omitempty"`
}

type DockerManifest struct {
	CLI             string `json:"cli"`
	AnchorContainer string `json:"anchor_container"`
	NotifyContainer string `json:"notify_container,omitempty"`
}

type NotifyManifest struct {
	BrokerUID    int      `json:"broker_uid"`
	BrokerGID    int      `json:"broker_gid"`
	Destinations []string `json:"destinations,omitempty"`
}

var (
	unitNamePattern  = regexp.MustCompile(`^[A-Za-z0-9@_.:-]{1,200}\.service$`)
	containerPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)
	logNsPattern     = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

func loadManifest(path string) (*Manifest, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("manifest path %q must be absolute", path)
	}
	if err := requireRootOwned(path, false); err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("parse manifest %s: %w", path, err)
	}
	if err := m.validate(); err != nil {
		return nil, fmt.Errorf("manifest %s: %w", path, err)
	}
	return &m, nil
}

func (m *Manifest) validate() error {
	if err := egresspolicy.ValidDeployment(m.Deployment); err != nil {
		return err
	}
	m.Proxy.Scheme = strings.ToLower(m.Proxy.Scheme)
	m.Proxy.Host = strings.ToLower(m.Proxy.Host)
	if err := m.Proxy.Validate(); err != nil {
		return fmt.Errorf("proxy: %w", err)
	}
	if m.WorkerUID <= 0 || m.WorkerGID <= 0 {
		return fmt.Errorf("worker_uid and worker_gid must be positive (workers never run as root)")
	}
	if m.ResolveSeconds == 0 {
		m.ResolveSeconds = defaultResolveSeconds
	}
	if m.ResolveSeconds < minResolveSeconds || m.ResolveSeconds > maxResolveSeconds {
		return fmt.Errorf("resolve_seconds must be in %d-%d", minResolveSeconds, maxResolveSeconds)
	}
	seenListen := map[string]bool{}
	for i, r := range m.StatusRelays {
		host, port, err := net.SplitHostPort(r.Listen)
		if err != nil {
			return fmt.Errorf("status_relays[%d].listen: %v", i, err)
		}
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return fmt.Errorf("status_relays[%d].listen %q must be a host loopback address", i, r.Listen)
		}
		if p, err := strconv.Atoi(port); err != nil || p < 1 || p > 65535 {
			return fmt.Errorf("status_relays[%d].listen port is invalid", i)
		}
		if r.TargetPort < 1 || r.TargetPort > 65535 {
			return fmt.Errorf("status_relays[%d].target_port is invalid", i)
		}
		if seenListen[r.Listen] {
			return fmt.Errorf("status_relays[%d].listen %q is listed twice", i, r.Listen)
		}
		seenListen[r.Listen] = true
		if r.TargetPort == m.Proxy.Port && (relayListenHost(m.Proxy) == "127.0.0.1" || relayListenHost(m.Proxy) == "::1") {
			return fmt.Errorf("status_relays[%d].target_port %d collides with the proxy relay port", i, r.TargetPort)
		}
	}
	switch m.Mode {
	case modeHost:
		if m.Host == nil || m.Docker != nil {
			return fmt.Errorf("mode %q needs a host block and no docker block", m.Mode)
		}
		h := m.Host
		if !unitNamePattern.MatchString(h.AppUnit) {
			return fmt.Errorf("host.app_unit %q is not a service unit name", h.AppUnit)
		}
		if len(h.EnvironmentFiles) == 0 {
			return fmt.Errorf("host.environment_files must name the unit's environment files")
		}
		for _, p := range h.EnvironmentFiles {
			if !filepath.IsAbs(p) || filepath.Clean(p) != p || strings.ContainsAny(p, " \t\n\"'\\") {
				return fmt.Errorf("host.environment_files entry %q must be a clean absolute path without spaces or quotes", p)
			}
		}
		for name, p := range map[string]string{"host.app_executable": h.AppExecutable, "host.working_directory": h.WorkingDirectory} {
			if p == "" || !filepath.IsAbs(p) || filepath.Clean(p) != p {
				return fmt.Errorf("%s %q must be a clean absolute path", name, p)
			}
		}
		for _, p := range h.ReadWritePaths {
			if !filepath.IsAbs(p) || filepath.Clean(p) != p || strings.ContainsAny(p, " \t\n\"'\\") {
				return fmt.Errorf("host.read_write_paths entry %q must be a clean absolute path without spaces or quotes", p)
			}
		}
		if h.LogNamespace != "" && !logNsPattern.MatchString(h.LogNamespace) {
			return fmt.Errorf("host.log_namespace %q is invalid", h.LogNamespace)
		}
	case modeDocker:
		if m.Docker == nil || m.Host != nil {
			return fmt.Errorf("mode %q needs a docker block and no host block", m.Mode)
		}
		if !filepath.IsAbs(m.Docker.CLI) || filepath.Clean(m.Docker.CLI) != m.Docker.CLI {
			return fmt.Errorf("docker.cli %q must be a clean absolute path", m.Docker.CLI)
		}
		if !containerPattern.MatchString(m.Docker.AnchorContainer) {
			return fmt.Errorf("docker.anchor_container %q is not a container name", m.Docker.AnchorContainer)
		}
		if m.Notify != nil && !containerPattern.MatchString(m.Docker.NotifyContainer) {
			return fmt.Errorf("docker.notify_container %q is not a container name", m.Docker.NotifyContainer)
		}
		if m.Notify == nil && m.Docker.NotifyContainer != "" {
			return fmt.Errorf("docker.notify_container needs a notify block")
		}
	default:
		return fmt.Errorf("mode must be %q or %q", modeHost, modeDocker)
	}
	if n := m.Notify; n != nil {
		if n.BrokerUID <= 0 || n.BrokerGID <= 0 {
			return fmt.Errorf("notify.broker_uid and notify.broker_gid must be positive")
		}
		if n.BrokerUID == m.WorkerUID {
			return fmt.Errorf("notify.broker_uid must differ from worker_uid")
		}
		for _, d := range n.Destinations {
			if _, ok := notifyDestinations[d]; !ok {
				return fmt.Errorf("notify.destinations entry %q is not a known notification service", d)
			}
		}
	}
	return nil
}

func (m *Manifest) namespaceName() string {
	return "go-trader-egress-" + m.Deployment
}

func (m *Manifest) notifyNamespaceName() string {
	return "go-trader-notify-" + m.Deployment
}

func (m *Manifest) namespacePath() string {
	return "/run/netns/" + m.namespaceName()
}

func (m *Manifest) runDir() string {
	return filepath.Join(egresspolicy.ControlRoot, m.Deployment)
}

func relayListenHost(id egresspolicy.Identity) string {
	if ip := net.ParseIP(id.Host); ip != nil {
		return ip.String()
	}
	if egresspolicy.IsLoopbackHost(id.Host) {
		return "127.0.0.1"
	}
	return relayHostAddress
}

func requireRootOwned(path string, allowGroupWrite bool) error {
	for p := path; ; p = filepath.Dir(p) {
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("cannot read the owner of %s", p)
		}
		if st.Uid != 0 {
			return fmt.Errorf("%s is owned by uid %d, not root", p, st.Uid)
		}
		mode := fi.Mode()
		if mode&os.ModeSymlink != 0 && p == path {
			return fmt.Errorf("%s is a symbolic link", p)
		}
		if mode.Perm()&0o002 != 0 && mode&os.ModeSticky == 0 {
			return fmt.Errorf("%s is writable by every user", p)
		}
		if !allowGroupWrite && mode.Perm()&0o020 != 0 && st.Gid != 0 {
			return fmt.Errorf("%s is writable by group %d", p, st.Gid)
		}
		if p == "/" {
			return nil
		}
	}
}
