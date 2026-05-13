package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
)

// uvBinaryEnv names an optional absolute path to the uv executable. Use when
// uv is not on systemd's default PATH (e.g. install under a custom prefix); set
// in /opt/go-trader/.env or a systemd drop-in.
const uvBinaryEnv = "GO_TRADER_UV"

// newPythonCommand builds the canonical Python subprocess invocation.
// Runtime and startup probes must share this argv shape so the probe validates
// the same contract the scheduler uses during normal operation.
func newPythonCommand(ctx context.Context, script string, args ...string) (*exec.Cmd, error) {
	uvPath := os.Getenv(uvBinaryEnv)
	if uvPath == "" {
		var err error
		uvPath, err = exec.LookPath("uv")
		if err != nil {
			return nil, fmt.Errorf("uv not found on PATH: %w", err)
		}
	}

	cmdArgs := append([]string{"run", "--no-sync", "python", script}, args...)
	return exec.CommandContext(ctx, uvPath, cmdArgs...), nil
}
