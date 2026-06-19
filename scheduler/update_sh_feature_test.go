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

// #1055: the --all coordinator must reach discovery/dispatch on a host that has
// only what fan-out needs (git + coreutils), WITHOUT uv/go — those gate the
// per-deployment children's build, not the parent. Regression for the CI go-job
// (no uv) that the first cut of this feature broke by running the uv/go preflight
// before the --all dispatcher. Deterministic on any host: we build a curated PATH
// that deliberately omits uv and go.
func TestUpdateShellAllDispatchesWithoutBuildToolchain1055(t *testing.T) {
	t.Parallel()
	script := updateShellScriptPath(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "go-trader-x"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Curated bin with only the externals the --all glob path invokes; uv and go
	// are intentionally absent. (bash itself is resolved from the parent PATH by
	// exec.Command, not from cmd.Env, so it need not be symlinked here.)
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

	// PATH is exactly binDir (no uv/go), so a pass proves the dispatcher ran before
	// the uv/go preflight rather than the toolchain merely happening to be present.
	cmd := exec.Command("bash", script, "--all", "--restart")
	cmd.Env = []string{
		"PATH=" + binDir,
		"GO_TRADER_UPDATE_ALL_ROOT=" + root,
		"HOME=" + t.TempDir(),
	}
	out, err := cmd.CombinedOutput()
	text := string(out)
	// Coordinator must NOT abort in the build-toolchain preflight.
	if strings.Contains(text, "uv not on PATH") || strings.Contains(text, "go not on PATH") {
		t.Fatalf("--all aborted in build-toolchain preflight without uv/go (must dispatch first)\n%s", text)
	}
	// It must reach discovery; the lone dir has no config.json, so it skips it and
	// fails with the zero-update error (non-zero exit) — that proves it got past
	// preflight all the way to the dispatch/skip logic.
	if err == nil {
		t.Fatalf("expected non-zero exit (zero deployments updated)\n%s", text)
	}
	for _, want := range []string{"via glob discovery", "updated 0 deployments"} {
		if !strings.Contains(text, want) {
			t.Errorf("--all without uv/go did not reach dispatch: missing %q\n%s", want, text)
		}
	}
}

// allUnionTestEnv builds a controllable --all environment for the union/suppress
// tests (#1055 review): a real temp git repo (so `git rev-parse --show-toplevel`
// resolves), a sibling glob-root deployment dir, a scattered deployment dir, and a
// fake `systemctl` on PATH that reports the scattered unit as ACTIVE. Neither
// deployment carries scheduler/config.json, so the run skips both and fails with
// the zero-update error — but each skip line names its dir, which is what lets the
// tests prove which source(s) the dir came from. Returns (repoDir, scatteredDir, env).
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
	// Sibling of the repo under the default scan_root (= parent): glob-only source.
	if err := os.MkdirAll(filepath.Join(parent, "go-trader-globonly"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Scattered deployment outside the glob root: systemd-only source.
	scattered := filepath.Join(t.TempDir(), "go-trader-scattered")
	if err := os.MkdirAll(scattered, 0o755); err != nil {
		t.Fatal(err)
	}

	binDir := t.TempDir()
	fake := "#!/usr/bin/env bash\n" +
		"case \"$1\" in\n" +
		"  list-units) printf '%s\\n' \"go-trader-scattered.service loaded active running scattered\" ;;\n" +
		"  show) printf '%s\\n' \"${GO_TRADER_TEST_SCATTERED:-}\" ;;\n" + // show <unit> -p WorkingDirectory --value
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

// #1055 review (finding 1): the --all batch must be the UNION of active systemd
// units and the glob root, so neither a scattered systemd deployment nor a
// glob-root-only (signal-mode / unloaded) deployment silently leaves the batch.
func TestUpdateShellAllUnionsSystemdAndGlob1055(t *testing.T) {
	t.Parallel()
	script := updateShellScriptPath(t)
	repo, _, env := allUnionTestEnv(t)

	cmd := exec.Command("bash", script, "--all", "--restart")
	cmd.Dir = repo // so scan_root defaults to parent (non-explicit -> systemd enabled)
	cmd.Env = env
	out, _ := cmd.CombinedOutput()
	text := string(out)

	for _, want := range []string{
		"2 deployment dir(s) via systemd+glob discovery", // both sources contributed
		"go-trader-scattered",                            // systemd-only dir present
		"go-trader-globonly",                             // glob-only dir present
		"updated 0 deployments",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("union --all missing %q\n%s", want, text)
		}
	}
}

// #1055 (AC): an explicit --update-all-root pins the glob root and SUPPRESSES
// systemd discovery, so the scattered active unit must NOT be pulled into the
// batch — keeping the documented --update-all-root flow unchanged.
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
		"1 deployment dir(s) via glob discovery", // glob root only
		"go-trader-globonly",
		"updated 0 deployments",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("explicit-root --all missing %q\n%s", want, text)
		}
	}
}
