//go:build pyintegration

package main

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestUpdateHelpersEnvfileParsing790(t *testing.T) {
	t.Parallel()
	bash := updateShellBash(t)
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	script := filepath.Join(filepath.Dir(thisFile), "..", "scripts", "test_update_helpers.sh")
	out, err := exec.Command(bash, script).CombinedOutput()
	if err != nil {
		t.Fatalf("bash %s: %v\n%s", script, err, out)
	}
	if !strings.Contains(string(out), "OK:") {
		t.Fatalf("unexpected output:\n%s", out)
	}
}
