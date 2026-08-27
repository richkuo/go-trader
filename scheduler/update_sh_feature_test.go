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

func TestUpdateShellAllReportsSkippedAndFailsOnZeroUpdate1055(t *testing.T) {
	t.Parallel()
	script := updateShellScriptPath(t)
	root := t.TempDir()
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
		"2 deployment dir(s) via glob discovery",
		"skipping",
		"no scheduler/config.json",
		"updated 0 deployments",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("--all output missing %q\n%s", want, text)
		}
	}
	if strings.Contains(text, "unrelated") {
		t.Errorf("--all glob should not match 'unrelated'\n%s", text)
	}
}

func TestUpdateShellAllDispatchesWithoutBuildToolchain1055(t *testing.T) {
	t.Parallel()
	script := updateShellScriptPath(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "go-trader-x"), 0o755); err != nil {
		t.Fatal(err)
	}

	binDir := t.TempDir()
	for _, tool := range []string{"git", "sort", "tr", "dirname", "basename"} {
		src, err := exec.LookPath(tool)
		if err != nil {
			t.Skipf("required tool %q not found on host: %v", tool, err)
		}
		if err := os.Symlink(src, filepath.Join(binDir, tool)); err != nil {
			t.Fatal(err)
		}
	}

	cmd := exec.Command("bash", script, "--all", "--restart")
	cmd.Env = []string{
		"PATH=" + binDir,
		"GO_TRADER_UPDATE_ALL_ROOT=" + root,
		"HOME=" + t.TempDir(),
	}
	out, err := cmd.CombinedOutput()
	text := string(out)
	if strings.Contains(text, "uv not on PATH") || strings.Contains(text, "go not on PATH") {
		t.Fatalf("--all aborted in build-toolchain preflight without uv/go (must dispatch first)\n%s", text)
	}
	if err == nil {
		t.Fatalf("expected non-zero exit (zero deployments updated)\n%s", text)
	}
	for _, want := range []string{"via glob discovery", "updated 0 deployments"} {
		if !strings.Contains(text, want) {
			t.Errorf("--all without uv/go did not reach dispatch: missing %q\n%s", want, text)
		}
	}
}

func allUnionTestEnv(t *testing.T) (string, string, []string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not available: %v", err)
	}
	parent := t.TempDir()
	repo := filepath.Join(parent, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", repo, "init").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	if err := os.MkdirAll(filepath.Join(parent, "go-trader-globonly"), 0o755); err != nil {
		t.Fatal(err)
	}
	scattered := filepath.Join(t.TempDir(), "go-trader-scattered")
	if err := os.MkdirAll(scattered, 0o755); err != nil {
		t.Fatal(err)
	}

	binDir := t.TempDir()
	fake := "#!/usr/bin/env bash\n" +
		"case \"$1\" in\n" +
		"  list-units) printf '%s\\n' \"go-trader-scattered.service loaded active running scattered\" ;;\n" +
		"  show) printf '%s\\n' \"${GO_TRADER_TEST_SCATTERED:-}\" ;;\n" +
		"esac\n"
	if err := os.WriteFile(filepath.Join(binDir, "systemctl"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"GO_TRADER_TEST_SCATTERED="+scattered,
	)
	return repo, scattered, env
}

func TestUpdateShellAllUnionsSystemdAndGlob1055(t *testing.T) {
	t.Parallel()
	script := updateShellScriptPath(t)
	repo, _, env := allUnionTestEnv(t)

	cmd := exec.Command("bash", script, "--all", "--restart")
	cmd.Dir = repo
	cmd.Env = env
	out, _ := cmd.CombinedOutput()
	text := string(out)

	for _, want := range []string{
		"2 deployment dir(s) via systemd+glob discovery",
		"go-trader-scattered",
		"go-trader-globonly",
		"updated 0 deployments",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("union --all missing %q\n%s", want, text)
		}
	}
}

func TestUpdateShellAllExplicitRootSuppressesSystemd1055(t *testing.T) {
	t.Parallel()
	script := updateShellScriptPath(t)
	repo, _, env := allUnionTestEnv(t)
	parent := filepath.Dir(repo)

	cmd := exec.Command("bash", script, "--all", "--restart", "--update-all-root", parent)
	cmd.Dir = repo
	cmd.Env = env
	out, _ := cmd.CombinedOutput()
	text := string(out)

	if strings.Contains(text, "go-trader-scattered") {
		t.Errorf("explicit --update-all-root must suppress systemd discovery, but scattered unit appeared\n%s", text)
	}
	for _, want := range []string{
		"1 deployment dir(s) via glob discovery",
		"go-trader-globonly",
		"updated 0 deployments",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("explicit-root --all missing %q\n%s", want, text)
		}
	}
}

func TestUpdateShellAllDedupesCanonicalAliases1055(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not available: %v", err)
	}
	script := updateShellScriptPath(t)
	parent := t.TempDir()
	repo := filepath.Join(parent, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", repo, "init").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	aliased := filepath.Join(parent, "go-trader-aliased")
	if err := os.MkdirAll(aliased, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "go-trader-link")
	if err := os.Symlink(aliased, link); err != nil {
		t.Fatal(err)
	}

	binDir := t.TempDir()
	fake := "#!/usr/bin/env bash\n" +
		"case \"$1\" in\n" +
		"  list-units) printf '%s\\n' \"go-trader-aliased.service loaded active running aliased\" ;;\n" +
		"  show) printf '%s\\n' \"${GO_TRADER_TEST_SCATTERED:-}\" ;;\n" +
		"esac\n"
	if err := os.WriteFile(filepath.Join(binDir, "systemctl"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("bash", script, "--all", "--restart")
	cmd.Dir = repo
	cmd.Env = append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"GO_TRADER_TEST_SCATTERED="+link,
	)
	out, _ := cmd.CombinedOutput()
	text := string(out)

	if strings.Contains(text, "2 deployment dir(s)") {
		t.Errorf("symlinked WorkingDirectory aliasing a glob dir was not de-duped (counted twice)\n%s", text)
	}
	if !strings.Contains(text, "1 deployment dir(s) via systemd+glob discovery") {
		t.Errorf("expected a single canonicalized entry from both sources\n%s", text)
	}
}
