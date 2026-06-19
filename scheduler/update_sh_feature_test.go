package main

import (
	"os"
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

func TestUpdateShellHelpDocumentsRsyncFrom790(t *testing.T) {
	t.Parallel()
	script := updateShellScriptPath(t)
	out, err := exec.Command("bash", script, "--help").CombinedOutput()
	if err != nil {
		t.Fatalf("bash %s --help: %v\n%s", script, err, out)
	}
	text := string(out)
	for _, want := range []string{
		"--rsync-from",
		"hardcoded exclusions",
		".env",
		"state DB",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("help missing %q", want)
		}
	}
}

func TestUpdateHelpersEnvfileParsing790(t *testing.T) {
	t.Parallel()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	script := filepath.Join(filepath.Dir(thisFile), "..", "scripts", "test_update_helpers.sh")
	out, err := exec.Command("bash", script).CombinedOutput()
	if err != nil {
		t.Fatalf("bash %s: %v\n%s", script, err, out)
	}
	if !strings.Contains(string(out), "OK:") {
		t.Fatalf("unexpected output:\n%s", out)
	}
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

// #1055: --all must not silently no-op. When every discovered dir is skipped
// (none has scheduler/config.json), each skip is reported with a reason and the
// run fails loudly rather than printing "all instances OK" having updated nothing.
// GO_TRADER_UPDATE_ALL_ROOT pins the glob path, so this is deterministic on any
// host regardless of whether systemctl is present.
func TestUpdateShellAllReportsSkippedAndFailsOnZeroUpdate1055(t *testing.T) {
	t.Parallel()
	script := updateShellScriptPath(t)
	root := t.TempDir()
	// Two glob-matching dirs, neither a real deployment (no scheduler/config.json),
	// plus one dir that the glob ignores entirely.
	for _, d := range []string{"go-trader-live", "go-trader-paper", "unrelated"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("bash", script, "--all", "--restart")
	cmd.Env = append(os.Environ(), "GO_TRADER_UPDATE_ALL_ROOT="+root)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected non-zero exit when --all updates zero deployments\n%s", out)
	}
	text := string(out)
	for _, want := range []string{
		"2 deployment dir(s) via glob discovery", // count reflects glob matches
		"skipping",                               // each skipped dir reported
		"no scheduler/config.json",               // with a reason
		"updated 0 deployments",                  // loud zero-update failure
	} {
		if !strings.Contains(text, want) {
			t.Errorf("--all output missing %q\n%s", want, text)
		}
	}
	// The glob must not have pulled in the unrelated dir.
	if strings.Contains(text, "unrelated") {
		t.Errorf("--all glob should not match 'unrelated'\n%s", text)
	}
}
