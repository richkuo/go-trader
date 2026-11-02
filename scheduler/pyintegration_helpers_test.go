//go:build pyintegration

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

var (
	pyintegrationBinOnce sync.Once
	pyintegrationBinPath string
	pyintegrationBinErr  error
)

func pyintegrationBinary(t *testing.T) string {
	t.Helper()
	pyintegrationBinOnce.Do(func() {
		_, thisFile, _, ok := runtime.Caller(0)
		if !ok {
			pyintegrationBinErr = fmt.Errorf("runtime.Caller failed")
			return
		}
		dir, err := os.MkdirTemp("", "pyintegration-bin-")
		if err != nil {
			pyintegrationBinErr = err
			return
		}
		pyintegrationBinPath = filepath.Join(dir, "go-trader")
		args := []string{"build", "-tags", "pyintegration", "-o", pyintegrationBinPath, "."}
		if raceBuildEnabled {
			args = []string{"build", "-race", "-tags", "pyintegration", "-o", pyintegrationBinPath, "."}
		}
		cmd := exec.Command("go", args...)
		cmd.Dir = filepath.Dir(thisFile)
		out, err := cmd.CombinedOutput()
		if err != nil {
			pyintegrationBinErr = fmt.Errorf("go build: %v: %s", err, out)
		}
	})
	if pyintegrationBinErr != nil {
		t.Fatal(pyintegrationBinErr)
	}
	return pyintegrationBinPath
}

func pyRepoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, ".venv", "bin", "python3")); err != nil {
		t.Skipf("repository virtualenv missing (%v); run uv sync", err)
	}
	return root
}
