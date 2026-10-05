//go:build pyintegration

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestUpdateHelpersEnvfileParsing790(t *testing.T) {
	t.Parallel()
	bash := updateShellBash(t)
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	schedDir := filepath.Dir(thisFile)
	bin := filepath.Join(t.TempDir(), "go-trader")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = schedDir
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	script := filepath.Join(schedDir, "..", "scripts", "test_update_helpers.sh")
	cmd := exec.Command(bash, script)
	cmd.Env = append(os.Environ(), "GO_TRADER_BIN="+bin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bash %s: %v\n%s", script, err, out)
	}
	assertShellSuiteOutput(t, "test_update_helpers.sh", out)
}
