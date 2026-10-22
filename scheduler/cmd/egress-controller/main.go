//go:build linux

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"trading-scheduler/internal/egresspolicy"
	"trading-scheduler/internal/notifybroker"
)

var Version = "dev"

func controllerVersion() string {
	return Version
}

func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
}

const usage = `usage: go-trader-egress-controller <command> [flags]

commands:
  serve        --manifest PATH   run the controller (root)
  preflight    --manifest PATH   check kernel, tools and resolution support (root)
  status       --manifest PATH   print the controller status
  launch       --manifest PATH -- ARGS...   run one go-trader command inside the namespace (root, host mode)
  render-units --manifest PATH   print the systemd drop-in and broker unit for this deployment
  install-units --manifest PATH  write the drop-in and broker unit (root, host mode)
  remove-units --manifest PATH   remove the drop-in and broker unit (root, host mode)
  identity ENV_FILE              print the nonsecret proxy identity from an environment file
  extract-notify-env APP_ENV OUT GID   copy only the notification keys into the broker environment file
  teardown     --manifest PATH   remove the namespaces once no process uses them (root)
  notify-broker --deployment NAME   run the notification broker (broker account)
  version                         print the build and protocol versions`

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	if cmd == "version" {
		fmt.Printf("go-trader-egress-controller %s protocol %d\n", Version, egresspolicy.ProtocolVersion)
		return
	}
	if cmd == "identity" || cmd == "extract-notify-env" {
		var err error
		if cmd == "identity" {
			err = runIdentity(args)
		} else {
			err = runExtractNotifyEnv(args)
		}
		if err != nil {
			log.Fatalf("%s: %v", cmd, err)
		}
		return
	}
	if cmd == "notify-broker" {
		fs := flag.NewFlagSet(cmd, flag.ExitOnError)
		deployment := fs.String("deployment", "", "deployment name")
		_ = fs.Parse(args)
		ctx, cancel := signalContext()
		defer cancel()
		if err := notifybroker.Run(ctx, *deployment); err != nil {
			log.Fatalf("notification broker: %v", err)
		}
		return
	}
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	manifestPath := fs.String("manifest", "", "absolute path of the root-owned deployment manifest")
	_ = fs.Parse(args)
	if *manifestPath == "" {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	m, err := loadManifest(*manifestPath)
	if err != nil {
		log.Fatalf("%v", err)
	}
	switch cmd {
	case "serve":
		err = runServe(m)
	case "preflight":
		err = runPreflight(m)
	case "status":
		err = runStatus(m)
	case "launch":
		var code int
		code, err = runLaunch(m, fs.Args())
		if err == nil {
			os.Exit(code)
		}
	case "render-units":
		err = runRenderUnits(m)
	case "install-units":
		err = runInstallUnits(m)
	case "remove-units":
		err = runRemoveUnits(m)
	case "teardown":
		err = runTeardown(m)
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		log.Fatalf("%s: %v", cmd, err)
	}
}

func runStatus(m *Manifest) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err := egresspolicy.QueryStatus(ctx, m.Deployment)
	if err != nil {
		return err
	}
	out, _ := json.MarshalIndent(st, "", "  ")
	fmt.Println(string(out))
	if !st.Ready {
		os.Exit(1)
	}
	return nil
}

func runPreflight(m *Manifest) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("preflight must run as root")
	}
	t, err := loadTools()
	if err != nil {
		return err
	}
	fmt.Printf("ok: iproute2 %s, nftables %s\n", t.ip, t.nft)
	var buf [4]byte
	_, _ = rand.Read(buf[:])
	ns := "go-trader-egress-preflight-" + hex.EncodeToString(buf[:])
	if err := t.createNetns(ns); err != nil {
		return fmt.Errorf("create a network namespace: %w", err)
	}
	defer t.deleteNetns(ns)
	if err := t.installRules(ns, m, 1); err != nil {
		return fmt.Errorf("install nftables rules in a namespace: %w", err)
	}
	if _, err := t.verifyRules(ns, m); err != nil {
		return fmt.Errorf("verify nftables rules: %w", err)
	}
	fmt.Println("ok: namespace rules install and verify")
	ctx, cancel := context.WithTimeout(context.Background(), resolveTimeout)
	defer cancel()
	addrs, err := resolveUpstreams(ctx, m.Proxy)
	if err != nil {
		return fmt.Errorf("resolve the proxy: %w", err)
	}
	fmt.Printf("ok: proxy %s resolves to %s\n", m.Proxy, strings.Join(addrs, ", "))
	if m.Mode == modeHost {
		if _, err := findTool("systemd-run"); err != nil {
			return err
		}
		systemctl, err := findTool("systemctl")
		if err != nil {
			return err
		}
		out, err := runTool(nil, systemctl, "--version")
		if err != nil {
			return err
		}
		var ver int
		fmt.Sscanf(strings.TrimPrefix(strings.SplitN(string(out), "\n", 2)[0], "systemd "), "%d", &ver)
		if ver < 247 {
			return fmt.Errorf("systemd %d is too old; NetworkNamespacePath and ProtectProc need systemd 247 or newer", ver)
		}
		fmt.Printf("ok: systemd %d\n", ver)
	} else {
		if _, err := os.Stat(m.Docker.CLI); err != nil {
			return fmt.Errorf("docker cli: %w", err)
		}
		fmt.Printf("ok: docker cli %s\n", m.Docker.CLI)
	}
	fmt.Println("preflight passed")
	return nil
}

func runTeardown(m *Manifest) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("teardown must run as root")
	}
	t, err := loadTools()
	if err != nil {
		return err
	}
	names := []string{m.namespaceName()}
	if m.Notify != nil {
		names = append(names, m.notifyNamespaceName())
	}
	for _, ns := range names {
		if !netnsExists(ns) {
			continue
		}
		if m.Mode == modeHost {
			pids, err := processesInNetns("/run/netns/" + ns)
			if err != nil {
				return err
			}
			if len(pids) > 0 {
				return fmt.Errorf("namespace %s still holds processes %v; stop the services first", ns, pids)
			}
		}
		if err := t.deleteNetns(ns); err != nil {
			return err
		}
		fmt.Printf("removed namespace %s\n", ns)
	}
	if err := os.RemoveAll(m.runDir()); err != nil {
		return err
	}
	fmt.Printf("removed %s\n", m.runDir())
	return nil
}

var launchSubcommands = map[string]bool{
	"manual-open": true, "manual-add": true, "manual-close": true, "force-close": true,
	"manual-cancel": true, "manual-clear-limit-row": true, "manual-update-sl": true, "manual-cancel-sl": true,
	"backfill": true, "probe": true, "record-observations": true, "robinhood-login": true,
}

var launchDaemonFlags = map[string]bool{
	"--once": true, "-once": true, "--summary": true, "-summary": true, "--leaderboard": true, "-leaderboard": true,
}

func validateLaunchArgs(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("name the go-trader command to run after --")
	}
	first := args[0]
	if !launchSubcommands[first] {
		ok := false
		for _, a := range args {
			name := strings.SplitN(a, "=", 2)[0]
			if launchDaemonFlags[name] {
				ok = true
			}
		}
		if !ok {
			return fmt.Errorf("%q is not a network command the launcher runs; allowed: a manual or backfill subcommand, probe, or the daemon with --once, --summary or --leaderboard", first)
		}
	}
	for _, a := range args {
		if strings.ContainsAny(a, "\x00\n\r") {
			return fmt.Errorf("arguments must not contain NUL or line breaks")
		}
	}
	return nil
}

func runLaunch(m *Manifest, args []string) (int, error) {
	if os.Geteuid() != 0 {
		return 0, fmt.Errorf("launch must run as root")
	}
	if m.Mode != modeHost {
		return 0, fmt.Errorf("container deployments run one-shot commands with docker compose run (the cli service joins the enforced namespace)")
	}
	if err := validateLaunchArgs(args); err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err := egresspolicy.QueryStatus(ctx, m.Deployment)
	if err != nil {
		return 0, err
	}
	if !st.Ready {
		return 0, fmt.Errorf("the egress controller is not ready: rules=%q resolve=%q", st.RulesError, st.LastResolveError)
	}
	systemdRun, err := findTool("systemd-run")
	if err != nil {
		return 0, err
	}
	if fi, err := os.Stat(m.Host.AppExecutable); err != nil || !fi.Mode().IsRegular() {
		return 0, fmt.Errorf("app executable %s is not a regular file", m.Host.AppExecutable)
	}
	var buf [6]byte
	_, _ = rand.Read(buf[:])
	unit := fmt.Sprintf("go-trader-egress-%s-cmd-%s", m.Deployment, hex.EncodeToString(buf[:]))
	full := []string{"--wait", "--pipe", "--collect", "--quiet", "--service-type=exec", "--unit=" + unit,
		"--uid=" + fmt.Sprint(m.WorkerUID), "--gid=" + fmt.Sprint(m.WorkerGID),
		"-p", "WorkingDirectory=" + m.Host.WorkingDirectory,
		"-p", "ProtectSystem=strict", "-p", "PrivateTmp=yes"}
	for _, f := range m.Host.EnvironmentFiles {
		full = append(full, "-p", "EnvironmentFile=-"+f)
	}
	if len(m.Host.ReadWritePaths) > 0 {
		full = append(full, "-p", "ReadWritePaths="+strings.Join(m.Host.ReadWritePaths, " "))
	}
	for _, p := range workerSandboxProperties(m) {
		full = append(full, "-p", p)
	}
	full = append(full, "--", m.Host.AppExecutable)
	full = append(full, args...)
	proc, err := os.StartProcess(systemdRun, append([]string{systemdRun}, full...), &os.ProcAttr{
		Env:   []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL=C"},
		Files: []*os.File{os.Stdin, os.Stdout, os.Stderr},
	})
	if err != nil {
		return 0, err
	}
	state, err := proc.Wait()
	if err != nil {
		return 0, err
	}
	return state.ExitCode(), nil
}

var inaccessibleHostPaths = []string{
	"/run/dbus", "/run/systemd/resolve", "/run/systemd/private", "/run/systemd/io.systemd.Resolve",
	"/run/nscd", "/run/avahi-daemon", "/run/docker.sock", "/run/docker", "/run/containerd",
	"/run/podman", "/run/NetworkManager", "/run/systemd/netif", "/run/user",
}

func workerSandboxProperties(m *Manifest) []string {
	dir := filepath.Join(m.runDir(), "resolver")
	inaccessible := make([]string, 0, len(inaccessibleHostPaths))
	for _, p := range inaccessibleHostPaths {
		inaccessible = append(inaccessible, "-"+p)
	}
	props := []string{
		"NetworkNamespacePath=" + m.namespacePath(),
		"Environment=" + egresspolicy.EnvDeployment + "=" + m.Deployment,
		"NoNewPrivileges=yes",
		"CapabilityBoundingSet=",
		"AmbientCapabilities=",
		"RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6 AF_NETLINK",
		"ProtectProc=invisible",
		"SystemCallFilter=~@debug",
		"BindReadOnlyPaths=" + dir + "/hosts:/etc/hosts " + dir + "/resolv.conf:/etc/resolv.conf " + dir + "/nsswitch.conf:/etc/nsswitch.conf",
		"InaccessiblePaths=" + strings.Join(inaccessible, " "),
	}
	if m.Notify != nil {
		props = append(props, "InaccessiblePaths=-/etc/go-trader/notify-"+m.Deployment+".env")
	}
	return props
}

func renderDropIn(m *Manifest) string {
	var b strings.Builder
	b.WriteString("[Unit]\n")
	fmt.Fprintf(&b, "Requires=go-trader-egress@%s.service\nAfter=go-trader-egress@%s.service\n", m.Deployment, m.Deployment)
	if m.Notify != nil {
		fmt.Fprintf(&b, "Wants=go-trader-notify@%s.service\nAfter=go-trader-notify@%s.service\n", m.Deployment, m.Deployment)
	}
	b.WriteString("\n[Service]\n")
	for _, p := range workerSandboxProperties(m) {
		b.WriteString(p + "\n")
	}
	return b.String()
}

func renderBrokerUnit(m *Manifest, exe string) string {
	var b strings.Builder
	b.WriteString("[Unit]\n")
	fmt.Fprintf(&b, "Description=go-trader notification broker (%s)\n", m.Deployment)
	fmt.Fprintf(&b, "Requires=go-trader-egress@%s.service\nAfter=go-trader-egress@%s.service\n", m.Deployment, m.Deployment)
	b.WriteString("\n[Service]\nType=simple\n")
	fmt.Fprintf(&b, "User=%d\nGroup=%d\n", m.Notify.BrokerUID, m.Notify.BrokerGID)
	fmt.Fprintf(&b, "ExecStart=%s notify-broker --deployment %s\n", exe, m.Deployment)
	fmt.Fprintf(&b, "EnvironmentFile=/etc/go-trader/notify-%s.env\n", m.Deployment)
	b.WriteString("WorkingDirectory=/\n")
	fmt.Fprintf(&b, "NetworkNamespacePath=/run/netns/%s\n", m.notifyNamespaceName())
	b.WriteString("Restart=always\nRestartSec=5\n")
	b.WriteString("NoNewPrivileges=yes\nCapabilityBoundingSet=\nAmbientCapabilities=\nProtectSystem=strict\nProtectHome=yes\nPrivateTmp=yes\nPrivateDevices=yes\nProtectProc=invisible\nRestrictAddressFamilies=AF_UNIX AF_INET AF_INET6\n")
	fmt.Fprintf(&b, "ReadWritePaths=%s\n", filepath.Join(m.runDir(), notifyDirName))
	inaccessible := []string{"-" + m.Host.WorkingDirectory, "-/var/lib/go-trader"}
	for _, f := range m.Host.EnvironmentFiles {
		inaccessible = append(inaccessible, "-"+f)
	}
	for _, p := range m.Host.ReadWritePaths {
		inaccessible = append(inaccessible, "-"+p)
	}
	for _, p := range inaccessibleHostPaths {
		inaccessible = append(inaccessible, "-"+p)
	}
	fmt.Fprintf(&b, "InaccessiblePaths=%s\n", strings.Join(inaccessible, " "))
	b.WriteString("\n[Install]\nWantedBy=multi-user.target\n")
	return b.String()
}

func unitPaths(m *Manifest) (string, string) {
	return filepath.Join("/etc/systemd/system", m.Host.AppUnit+".d", "50-egress.conf"),
		filepath.Join("/etc/systemd/system", "go-trader-notify@"+m.Deployment+".service")
}

func controllerExecutable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}

func runRenderUnits(m *Manifest) error {
	if m.Mode != modeHost {
		return fmt.Errorf("render-units is for host mode; container deployments use docker/compose.egress.yaml")
	}
	exe, err := controllerExecutable()
	if err != nil {
		return err
	}
	dropIn, broker := unitPaths(m)
	fmt.Printf("# %s\n%s", dropIn, renderDropIn(m))
	if m.Notify != nil {
		fmt.Printf("\n# %s\n%s", broker, renderBrokerUnit(m, exe))
	}
	return nil
}

func runInstallUnits(m *Manifest) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("install-units must run as root")
	}
	if m.Mode != modeHost {
		return fmt.Errorf("install-units is for host mode")
	}
	exe, err := controllerExecutable()
	if err != nil {
		return err
	}
	if err := requireRootOwned(exe, false); err != nil {
		return fmt.Errorf("the controller executable must be root-owned before units reference it: %w", err)
	}
	dropIn, broker := unitPaths(m)
	if err := os.MkdirAll(filepath.Dir(dropIn), 0o755); err != nil {
		return err
	}
	if err := writeFileAtomic(dropIn, []byte(renderDropIn(m)), 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %s\n", dropIn)
	if m.Notify != nil {
		if err := writeFileAtomic(broker, []byte(renderBrokerUnit(m, exe)), 0o644); err != nil {
			return err
		}
		fmt.Printf("wrote %s\n", broker)
	} else if err := os.Remove(broker); err == nil {
		fmt.Printf("removed %s\n", broker)
	}
	return nil
}

func runRemoveUnits(m *Manifest) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("remove-units must run as root")
	}
	if m.Mode != modeHost {
		return fmt.Errorf("remove-units is for host mode")
	}
	dropIn, broker := unitPaths(m)
	for _, p := range []string{dropIn, broker} {
		if err := os.Remove(p); err == nil {
			fmt.Printf("removed %s\n", p)
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}
