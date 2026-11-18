//go:build pyintegration

package main

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func updateShellScriptPath(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(thisFile), "..", "scripts", "update.sh")
}

func updateShellBash(t *testing.T) string {
	t.Helper()
	candidates := []string{}
	if path, err := exec.LookPath("bash"); err == nil {
		candidates = append(candidates, path)
	}
	candidates = append(candidates, "/opt/homebrew/bin/bash", "/usr/local/bin/bash", "/opt/local/bin/bash")
	seen := make(map[string]struct{}, len(candidates))
	versions := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		if _, ok := seen[candidate]; ok {
			continue
		}
		seen[candidate] = struct{}{}
		versionOutput, versionErr := exec.Command(candidate, "--version").CombinedOutput()
		if versionErr != nil {
			continue
		}
		version := strings.TrimSpace(strings.SplitN(string(versionOutput), "\n", 2)[0])
		versions = append(versions, version)
		if err := exec.Command(candidate, "-c", `test "${BASH_VERSINFO[0]}" -ge 4`).Run(); err == nil {
			return candidate
		}
	}
	t.Skipf("update.sh --all tests require Bash >= 4 for declare -A; available Bash runtimes: %s", strings.Join(versions, "; "))
	return ""
}

func TestUpdateShellRejectsMissingRsyncFromDir790(t *testing.T) {
	t.Parallel()
	script := updateShellScriptPath(t)
	out, err := exec.Command("bash", script, "--rsync-from", "/nonexistent-go-trader-rsync-src").CombinedOutput()
	if err == nil {
		t.Fatalf("expected non-zero exit for missing --rsync-from dir\n%s", out)
	}
	if !strings.Contains(string(out), "requires an existing source directory") {
		t.Fatalf("unexpected error output:\n%s", out)
	}
}
