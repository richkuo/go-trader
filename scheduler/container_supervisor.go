package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"
)

const defaultHealthcheckURL = "http://127.0.0.1:8099/health"

func heldDaemonExitReason(code int) (string, bool) {
	switch code {
	case ExitProbeFailure:
		return "check-script probe failure", true
	case ExitSingletonLock:
		return "another process owns the state files", true
	case ExitStorageOwnership:
		return "rejected storage layout", true
	}
	return "", false
}

func runSupervise(args []string) int {
	if !inContainerRuntime() {
		fmt.Fprintf(os.Stderr, "supervise runs only in the container image (%s=%s); start the daemon directly on a host\n", runtimeEnvVar, runtimeContainer)
		return 2
	}
	for _, a := range args {
		for _, sub := range knownSubcommands {
			if a == sub {
				fmt.Fprintf(os.Stderr, "supervise starts only the scheduler daemon; run %q directly (docker compose run --rm cli %s ...)\n", sub, sub)
				return 2
			}
		}
	}

	sigCh := make(chan os.Signal, 8)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)

	if issues := containerVolumeIssues(); len(issues) > 0 {
		for _, issue := range issues[1:] {
			fmt.Fprintf(os.Stderr, "[supervisor] %s\n", issue)
		}
		fmt.Fprintf(os.Stderr, "[supervisor] CRITICAL: refusing to start the daemon: %s. Holding without a restart; the container stays unhealthy until the volume is fixed.\n", issues[0])
		return holdSupervisor(sigCh, 1)
	}

	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[supervisor] cannot resolve the go-trader binary: %v\n", err)
		return 1
	}
	cmd := exec.Command(exe, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "[supervisor] cannot start the daemon: %v\n", err)
		return 1
	}
	pid := cmd.Process.Pid
	fmt.Printf("[supervisor] started daemon pid %d\n", pid)

	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()

	terminating := false
	for {
		select {
		case sig := <-sigCh:
			if sig == syscall.SIGTERM || sig == syscall.SIGINT {
				terminating = true
			}
			fmt.Printf("[supervisor] forwarding %s to daemon pid %d\n", sig, pid)
			if err := cmd.Process.Signal(sig); err != nil && !errors.Is(err, os.ErrProcessDone) {
				fmt.Fprintf(os.Stderr, "[supervisor] forwarding %s failed: %v\n", sig, err)
			}
		case <-done:
			code := processExitCode(cmd.ProcessState)
			if reason, held := heldDaemonExitReason(code); held && !terminating {
				fmt.Fprintf(os.Stderr, "[supervisor] CRITICAL: daemon pid %d exited %d (%s). Holding without a restart; the container stays unhealthy. Read the daemon lines above, fix the cause, then run: %s\n", pid, code, reason, containerRestartAdvice)
				return holdSupervisor(sigCh, code)
			}
			fmt.Printf("[supervisor] daemon pid %d exited %d\n", pid, code)
			return code
		}
	}
}

func processExitCode(ps *os.ProcessState) int {
	if ps == nil {
		return 1
	}
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ps.ExitCode()
}

func holdSupervisor(sigCh <-chan os.Signal, code int) int {
	for sig := range sigCh {
		if sig == syscall.SIGTERM || sig == syscall.SIGINT {
			fmt.Printf("[supervisor] received %s while held; exiting %d\n", sig, code)
			return code
		}
		fmt.Printf("[supervisor] ignoring %s while held: no daemon is running\n", sig)
	}
	return code
}

func runHealthcheck(args []string) int {
	fs := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	url := fs.String("url", defaultHealthcheckURL, "health endpoint to query")
	timeout := fs.Duration("timeout", 5*time.Second, "request timeout")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "healthcheck: unexpected arguments: %v\n", fs.Args())
		return 2
	}
	client := &http.Client{Timeout: *timeout}
	resp, err := client.Get(*url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		fmt.Fprintf(os.Stderr, "healthcheck: %s returned HTTP %d: %s\n", *url, resp.StatusCode, body)
		return 1
	}
	fmt.Printf("healthcheck: %s HTTP %d\n", *url, resp.StatusCode)
	return 0
}
