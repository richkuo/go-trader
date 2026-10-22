package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
)

const (
	robinhoodSessionDirEnv  = "GO_TRADER_ROBINHOOD_SESSION_DIR"
	robinhoodSessionDirName = "robinhood-session"
	robinhoodLoginScript    = "shared_scripts/robinhood_login.py"
	robinhoodLoginPython    = ".venv/bin/python3"
)

func robinhoodSessionDirForConfig(configPath string) (string, error) {
	abs, err := filepath.Abs(configPath)
	if err != nil {
		return "", fmt.Errorf("resolve config path %q: %w", configPath, err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("resolve config path %q: %w", configPath, err)
	}
	return filepath.Join(filepath.Dir(resolved), robinhoodSessionDirName), nil
}

func configUsesRobinhood(cfg *Config) bool {
	if cfg == nil {
		return false
	}
	for _, sc := range cfg.Strategies {
		if sc.Platform == "robinhood" {
			return true
		}
	}
	return false
}

func exportRobinhoodSessionDir(configPath string, cfg *Config) {
	dir, err := robinhoodSessionDirForConfig(configPath)
	if err == nil {
		err = os.Setenv(robinhoodSessionDirEnv, dir)
	}
	if err != nil {
		os.Unsetenv(robinhoodSessionDirEnv)
		fmt.Fprintf(os.Stderr, "[robinhood] WARNING: cannot set %s (%v); Robinhood logins will refuse\n", robinhoodSessionDirEnv, err)
		return
	}
	if configUsesRobinhood(cfg) {
		fmt.Printf("[robinhood] session folder: %s\n", dir)
	}
}

func runRobinhoodLogin(args []string) int {
	fs := flag.NewFlagSet("robinhood-login", flag.ContinueOnError)
	configPath := fs.String("config", "scheduler/config.json", "Path to config file; the session folder is <its folder>/robinhood-session")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "robinhood-login: unexpected arguments: %v\n", fs.Args())
		return 2
	}
	dir, err := robinhoodSessionDirForConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "robinhood-login: %v\n", err)
		return 1
	}
	for _, path := range []string{robinhoodLoginPython, robinhoodLoginScript} {
		if _, err := os.Stat(path); err != nil {
			fmt.Fprintf(os.Stderr, "robinhood-login: %s not found (%v); run it from the go-trader install folder (/opt/go-trader on a host, /app in the image)\n", path, err)
			return 1
		}
	}
	fmt.Fprintf(os.Stderr, "robinhood-login: session folder %s\n", dir)

	cmd := exec.Command(robinhoodLoginPython, egressPythonArgv(robinhoodLoginScript, nil)...)
	cmd.Env = egressChildEnvironment(map[string]string{robinhoodSessionDirEnv: dir})
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	sigCh := make(chan os.Signal, 4)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigCh)
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "robinhood-login: start %s: %v\n", robinhoodLoginScript, err)
		return 1
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	for {
		select {
		case sig := <-sigCh:
			if sig == syscall.SIGINT {
				continue
			}
			if cmd.Process != nil {
				cmd.Process.Signal(sig)
			}
		case err := <-done:
			if err == nil {
				return 0
			}
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
					return 128 + int(status.Signal())
				}
				return exitErr.ExitCode()
			}
			fmt.Fprintf(os.Stderr, "robinhood-login: %v\n", err)
			return 1
		}
	}
}
