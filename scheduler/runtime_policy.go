package main

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	runtimeEnvVar          = "GO_TRADER_RUNTIME"
	runtimeContainer       = "container"
	statusBindEnvVar       = "GO_TRADER_STATUS_BIND"
	defaultStatusBindHost  = "localhost"
	containerDataDir       = "/data"
	containerConfigPath    = "/data/config.json"
	containerStateDBPath   = "/data/state.db"
	containerLogDir        = "/app/logs"
	containerRuntimeUID    = 10001
	containerRuntimeGID    = 10001
	containerRestartAdvice = "docker compose restart go-trader"
	containerUpgradeAdvice = "docker compose pull && docker compose up -d"
)

type runtimePolicy struct {
	container bool
}

var processRuntime runtimePolicy

func resolveRuntimePolicy(value string) (runtimePolicy, error) {
	switch value {
	case "":
		return runtimePolicy{}, nil
	case runtimeContainer:
		return runtimePolicy{container: true}, nil
	}
	return runtimePolicy{}, fmt.Errorf("%s=%q is not supported: unset it for a host deployment, or set it to %q in the container image", runtimeEnvVar, value, runtimeContainer)
}

func inContainerRuntime() bool {
	return processRuntime.container
}

func validateContainerRuntimeConfig(cfg *Config) error {
	if !inContainerRuntime() || cfg == nil {
		return nil
	}
	var errs []string
	if cfg.Role == configRoleFeed {
		errs = append(errs, "role \"feed\" (a shared market feed service) is not supported in the container deployment")
	}
	if cfg.AutoUpdate != "off" {
		errs = append(errs, fmt.Sprintf("auto_update is %q, but a container deployment updates only by pulling a new image (%s); set auto_update to \"off\" or remove it", cfg.AutoUpdate, containerUpgradeAdvice))
	}
	if cfg.marketFeedSharedEnabled() {
		errs = append(errs, "market_feed \"shared\" needs a separate feed service and is not supported in the container deployment")
	}
	if cfg.Role != configRoleFeed {
		check := func(label, path string) {
			if issue := containerPersistentPathIssue(path); issue != "" {
				errs = append(errs, fmt.Sprintf("%s %q %s", label, path, issue))
			}
		}
		check("db_file", cfg.DBFile)
		if cfg.PaperDBFile != "" {
			check("paper_db_file", cfg.PaperDBFile)
		}
		for _, src := range sortedPaperSources(cfg.PaperSources) {
			if src.DBFile != "" {
				check(fmt.Sprintf("paper_sources[%s].db_file", src.ID), src.DBFile)
			}
		}
		if p := strings.TrimSpace(cfg.ReplayLogPath); p != "" {
			check("replay_log_path", p)
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("container runtime policy (%s=%s):\n  %s", runtimeEnvVar, runtimeContainer, strings.Join(errs, "\n  "))
}

func containerPersistentPathIssue(path string) string {
	if isInMemoryDBPath(path) {
		return fmt.Sprintf("is an in-memory database; every database must be a file in the data volume at %s", containerDataDir)
	}
	if issue := containerDataPathIssue(path); issue != "" {
		return fmt.Sprintf("%s; use a path under %s (for example %s) so the database survives a container replacement", issue, containerDataDir, containerStateDBPath)
	}
	return ""
}

func containerDataPathIssue(path string) string {
	canonical, err := canonicalStoragePath(path)
	if err != nil {
		return fmt.Sprintf("cannot be resolved: %v", err)
	}
	root, err := filepath.EvalSymlinks(containerDataDir)
	if err != nil {
		return fmt.Sprintf("cannot be checked because the data volume %s cannot be resolved: %v", containerDataDir, err)
	}
	rel, err := filepath.Rel(root, canonical)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Sprintf("resolves to %s, outside the data volume %s", canonical, root)
	}
	return ""
}

func containerMountIssue(dir, purpose string) string {
	mounted, err := isMountPoint(dir)
	if err != nil {
		return fmt.Sprintf("%s (%s) cannot be checked: %v", dir, purpose, err)
	}
	if !mounted {
		return fmt.Sprintf("%s (%s) is not a mounted volume, so its contents would live in the disposable container layer. Mount the named volume at %s (docker/compose.yaml does this) and start again", dir, purpose, dir)
	}
	probe, err := os.CreateTemp(dir, ".go-trader-write-check-*")
	if err != nil {
		return fmt.Sprintf("%s (%s) is not writable by uid %d gid %d: %v. The volume must be owned by uid %d and used only by go-trader. Recovery: docs/DOCKER.md § Volume permissions", dir, purpose, os.Getuid(), os.Getgid(), err, containerRuntimeUID)
	}
	name := probe.Name()
	_ = probe.Close()
	if err := os.Remove(name); err != nil {
		return fmt.Sprintf("%s (%s) write check could not remove %s: %v", dir, purpose, name, err)
	}
	return ""
}

func containerVolumeIssues() []string {
	var issues []string
	if issue := containerMountIssue(containerDataDir, "config and state databases"); issue != "" {
		issues = append(issues, issue)
	}
	if issue := containerMountIssue(containerLogDir, "strategy logs"); issue != "" {
		issues = append(issues, issue)
	}
	return issues
}

func isMountPoint(dir string) (bool, error) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return false, err
	}
	defer f.Close()
	target := filepath.Clean(dir)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 5 {
			continue
		}
		if unescapeMountInfoPath(fields[4]) == target {
			return true, nil
		}
	}
	return false, sc.Err()
}

func unescapeMountInfoPath(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func resolveStatusBindHost(flagValue string) string {
	if v := strings.TrimSpace(flagValue); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv(statusBindEnvVar)); v != "" {
		return v
	}
	return defaultStatusBindHost
}

func normalizeBindHost(host string) string {
	return strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(host), "["), "]")
}

func isLoopbackBindHost(host string) bool {
	h := normalizeBindHost(host)
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

func isWildcardBindHost(host string) bool {
	h := normalizeBindHost(host)
	return h == "0.0.0.0" || h == "::"
}

func validateStatusBind(host, statusToken string) error {
	if isLoopbackBindHost(host) {
		return nil
	}
	if !inContainerRuntime() {
		return fmt.Errorf("status bind %q is not a loopback address; outside the container runtime the status server binds only localhost, 127.0.0.0/8 or ::1", host)
	}
	if !isWildcardBindHost(host) {
		return fmt.Errorf("status bind %q is not supported in the container runtime; use a loopback address or the wildcard address 0.0.0.0 (or ::)", host)
	}
	if statusToken == "" {
		return fmt.Errorf("status bind %q exposes the status API on the container network, but STATUS_AUTH_TOKEN is empty; set STATUS_AUTH_TOKEN in docker/go-trader.env (docs/DOCKER.md § Secrets) and start again", host)
	}
	return nil
}

func statusListenAddr(host string, port int) string {
	return net.JoinHostPort(normalizeBindHost(host), strconv.Itoa(port))
}

var containerRestartCh = make(chan struct{}, 1)

func requestContainerRestart() error {
	select {
	case containerRestartCh <- struct{}{}:
	default:
	}
	fmt.Println("[restart] container restart requested: draining and saving state, then exiting so Docker starts the container again")
	return nil
}
