package main

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestUpdateShellScriptSyntax(t *testing.T) {
	t.Parallel()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	schedDir := filepath.Dir(thisFile)
	repoRoot := filepath.Join(schedDir, "..")
	script := filepath.Join(repoRoot, "scripts", "update.sh")
	out, err := exec.Command("bash", "-n", script).CombinedOutput()
	if err != nil {
		t.Fatalf("bash -n scripts/update.sh: %v\n%s", err, out)
	}
}
