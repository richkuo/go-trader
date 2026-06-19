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
	for _, name := range []string{"update.sh", "update_helpers.sh", "create-run-sh.sh", "test_update_helpers.sh", "migrate-config-out-of-tree.sh"} {
		script := filepath.Join(repoRoot, "scripts", name)
		out, err := exec.Command("bash", "-n", script).CombinedOutput()
		if err != nil {
			t.Fatalf("bash -n scripts/%s: %v\n%s", name, err, out)
		}
	}
}
