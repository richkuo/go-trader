package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

type updateDMRecorder struct {
	dms []string
}

func (r *updateDMRecorder) SendMessage(string, string) error { return nil }
func (r *updateDMRecorder) SendDM(_ string, content string) error {
	r.dms = append(r.dms, content)
	return nil
}
func (r *updateDMRecorder) AskDM(string, string, time.Duration) (string, error) { return "", nil }
func (r *updateDMRecorder) Close()                                              {}

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

func TestUpdateShellAllReportsSkippedAndFailsOnZeroUpdate1055(t *testing.T) {
	t.Parallel()
	script := updateShellScriptPath(t)
	bash := updateShellBash(t)
	root := t.TempDir()
	for _, d := range []string{"go-trader-live", "go-trader-paper", "unrelated"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(bash, script, "--all", "--restart")
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
	bash := updateShellBash(t)
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

	cmd := exec.Command(bash, script, "--all", "--restart")
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
	bash := updateShellBash(t)
	repo, _, env := allUnionTestEnv(t)

	cmd := exec.Command(bash, script, "--all", "--restart")
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
	bash := updateShellBash(t)
	repo, _, env := allUnionTestEnv(t)
	parent := filepath.Dir(repo)

	cmd := exec.Command(bash, script, "--all", "--restart", "--update-all-root", parent)
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

func TestUpdateShellJournalSyncFailureLeavesBinaryUnswapped(t *testing.T) {
	t.Parallel()
	bash := updateShellBash(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not available: %v", err)
	}
	scriptsDir := filepath.Dir(updateShellScriptPath(t))
	parent := t.TempDir()
	origin := filepath.Join(parent, "origin.git")
	deploy := filepath.Join(parent, "deploy")
	gitEnv := append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
	)
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Env = gitEnv
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	runGit("init", "--bare", "-b", "main", origin)
	runGit("clone", origin, deploy)
	for _, name := range []string{"update.sh", "update_helpers.sh"} {
		body, err := os.ReadFile(filepath.Join(scriptsDir, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(deploy, "scripts"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(deploy, "scripts", name), body, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	unit := "[Service]\nExecStart=" + deploy + "/go-trader\nLogNamespace=go-trader\n"
	if err := os.WriteFile(filepath.Join(deploy, "go-trader.service"), []byte(unit), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deploy, ".gitignore"), []byte("go-trader\ngo-trader.*\nscheduler/config.json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(deploy, "scheduler"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deploy, "scheduler", "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("-C", deploy, "add", ".")
	runGit("-C", deploy, "commit", "-m", "init")
	runGit("-C", deploy, "push", "-u", "origin", "HEAD:main")

	if err := os.MkdirAll(filepath.Join(deploy, "scheduler"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deploy, "scheduler", "config.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldBinary := []byte("#!/usr/bin/env bash\necho old-binary\n")
	if err := os.WriteFile(filepath.Join(deploy, "go-trader"), oldBinary, 0o755); err != nil {
		t.Fatal(err)
	}

	binDir := t.TempDir()
	stubs := map[string]string{
		"uv": "#!/usr/bin/env bash\nexit 0\n",
		"go": "#!/usr/bin/env bash\n" +
			"dir=. out=\n" +
			"while [[ $# -gt 0 ]]; do case \"$1\" in -C) dir=\"$2\"; shift 2 ;; -o) out=\"$2\"; shift 2 ;; *) shift ;; esac; done\n" +
			"case \"$out\" in /*) ;; *) out=\"$dir/$out\" ;; esac\n" +
			"printf '#!/usr/bin/env bash\\nexit 0\\n' > \"$out\"\n" +
			"chmod +x \"$out\"\n",
		"systemctl": "#!/usr/bin/env bash\n" +
			"case \"$*\" in\n" +
			"  --version) echo 'systemd 255 (255.4-1ubuntu8)' ;;\n" +
			"  *FragmentPath*) echo /etc/systemd/system/go-trader.service ;;\n" +
			"  *MainPID*) echo 0 ;;\n" +
			"esac\n",
		"sudo": "#!/usr/bin/env bash\n\"$@\"\n",
	}
	for name, body := range stubs {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	cmd := exec.Command(bash, filepath.Join(deploy, "scripts", "update.sh"), "--restart")
	cmd.Dir = deploy
	cmd.Env = append(gitEnv, "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	text := string(out)
	if err == nil {
		t.Fatalf("expected update.sh to fail when the journald namespace config is missing\n%s", text)
	}
	if !strings.Contains(text, "FAIL phase=journal") {
		t.Fatalf("expected the failure in the journal phase, before the swap\n%s", text)
	}
	got, readErr := os.ReadFile(filepath.Join(deploy, "go-trader"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != string(oldBinary) {
		t.Fatalf("./go-trader was swapped although the journald sync failed; got %q\n%s", got, text)
	}
	for _, leftover := range []string{"go-trader.new", "go-trader.prev"} {
		if _, statErr := os.Stat(filepath.Join(deploy, leftover)); !os.IsNotExist(statErr) {
			t.Fatalf("%s must not exist after a journal-phase failure (stat err: %v)\n%s", leftover, statErr, text)
		}
	}
}

func TestApplyUpgradeTimeoutSendsSIGTERMAndRecoversWithoutOrphans(t *testing.T) {
	bash := updateShellBash(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not available: %v", err)
	}
	if _, err := exec.LookPath("rsync"); err != nil {
		t.Skipf("rsync not available: %v", err)
	}
	scriptsDir := filepath.Dir(updateShellScriptPath(t))
	parent := t.TempDir()
	origin := filepath.Join(parent, "origin.git")
	deploy := filepath.Join(parent, "deploy")
	recRoot := filepath.Join(parent, "recovery")
	gitEnv := append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
	)
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Env = gitEnv
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	runGit("init", "--bare", "-b", "main", origin)
	runGit("clone", origin, deploy)
	for _, name := range []string{"update.sh", "update_helpers.sh"} {
		body, err := os.ReadFile(filepath.Join(scriptsDir, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(deploy, "scripts"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(deploy, "scripts", name), body, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(deploy, "scheduler"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deploy, "README.md"), []byte("baseline\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deploy, ".gitignore"), []byte("go-trader\ngo-trader.*\nscheduler/config.json\n.venv/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("-C", deploy, "add", ".")
	runGit("-C", deploy, "commit", "-m", "v1")
	runGit("-C", deploy, "push", "-u", "origin", "HEAD:main")

	work := filepath.Join(parent, "work")
	runGit("clone", origin, work)
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("updated\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("-C", work, "add", ".")
	runGit("-C", work, "commit", "-m", "v2")
	runGit("-C", work, "push", "origin", "HEAD:main")

	if err := os.WriteFile(filepath.Join(deploy, "scheduler", "config.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deploy, "go-trader"), []byte("#!/usr/bin/env bash\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	binDir := t.TempDir()
	uvPidFile := filepath.Join(parent, "uv.pid")
	uvStub := "#!/usr/bin/env bash\n" +
		"if [[ \"$1\" == \"--version\" ]]; then echo 'uv 0.0.0'; exit 0; fi\n" +
		"echo $$ > " + uvPidFile + "\n" +
		"mkdir -p .venv/bin\n" +
		"echo mutated > .venv/mutated.txt\n" +
		"sleep 30 &\n" +
		"spid=$!\n" +
		"trap 'kill -TERM $spid 2>/dev/null || true; exit 143' TERM\n" +
		"wait $spid\n"
	if err := os.WriteFile(filepath.Join(binDir, "uv"), []byte(uvStub), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "go"), []byte("#!/usr/bin/env bash\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	bashDir := filepath.Dir(bash)

	origWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(deploy); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(origWD) }()

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+bashDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GO_TRADER_UPDATE_RECOVERY_DIR", recRoot)
	t.Setenv("GO_TRADER_UPDATE_RECOVERY_SLEEP_SEC", "45")
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	origTimeout := applyUpgradeTimeout
	applyUpgradeTimeout = 1 * time.Second
	defer func() { applyUpgradeTimeout = origTimeout }()

	rec := &updateDMRecorder{}
	mn := NewMultiNotifier(notifierBackend{notifier: rec, ownerID: "owner-1"})
	var mu sync.RWMutex
	applyUpgrade(mn, &mu, nil, nil, nil)

	if len(rec.dms) < 2 {
		t.Fatalf("expected Starting upgrade and failure DMs, got %v", rec.dms)
	}
	lastDM := rec.dms[len(rec.dms)-1]
	for _, want := range []string{"**update.sh failed**", "interrupted by SIGTERM", "recovery: complete"} {
		if !strings.Contains(lastDM, want) {
			t.Fatalf("expected DM to contain %q, got:\n%s", want, lastDM)
		}
	}
	readme, err := os.ReadFile(filepath.Join(deploy, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(readme) != "baseline\n" {
		t.Fatalf("expected README.md restored to baseline, got %q", string(readme))
	}
	if _, statErr := os.Stat(filepath.Join(deploy, ".venv")); !os.IsNotExist(statErr) {
		t.Fatalf("expected .venv absence restored, stat err=%v", statErr)
	}
	if pidBytes, err := os.ReadFile(uvPidFile); err == nil {
		pidStr := strings.TrimSpace(string(pidBytes))
		if exec.Command("kill", "-0", pidStr).Run() == nil {
			t.Fatalf("uv child pid %s is still alive after applyUpgrade timeout recovery", pidStr)
		}
	}
}

func TestUpdateShellGitModeRefusesMissingRsyncBeforeWrite(t *testing.T) {
	t.Parallel()
	bash := updateShellBash(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not available: %v", err)
	}
	scriptsDir := filepath.Dir(updateShellScriptPath(t))
	parent := t.TempDir()
	origin := filepath.Join(parent, "origin.git")
	deploy := filepath.Join(parent, "deploy")
	gitEnv := append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
	)
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Env = gitEnv
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	runGit("init", "--bare", "-b", "main", origin)
	runGit("clone", origin, deploy)
	for _, name := range []string{"update.sh", "update_helpers.sh"} {
		body, err := os.ReadFile(filepath.Join(scriptsDir, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(deploy, "scripts"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(deploy, "scripts", name), body, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(deploy, "scheduler"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deploy, "README.md"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("-C", deploy, "add", ".")
	runGit("-C", deploy, "commit", "-m", "v1")
	runGit("-C", deploy, "push", "-u", "origin", "HEAD:main")
	if err := os.WriteFile(filepath.Join(deploy, "scheduler", "config.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	binDir := t.TempDir()
	for _, tool := range []string{"git", "python3", "tr", "dirname", "basename", "id", "stat", "mktemp", "rm", "mkdir"} {
		src, err := exec.LookPath(tool)
		if err != nil {
			t.Skipf("required tool %q not found: %v", tool, err)
		}
		if err := os.Symlink(src, filepath.Join(binDir, tool)); err != nil {
			t.Fatal(err)
		}
	}
	for _, stub := range []string{"uv", "go"} {
		if err := os.WriteFile(filepath.Join(binDir, stub), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	cmd := exec.Command(bash, filepath.Join(deploy, "scripts", "update.sh"))
	cmd.Dir = deploy
	cmd.Env = []string{
		"PATH=" + binDir,
		"HOME=" + t.TempDir(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
	}
	out, err := cmd.CombinedOutput()
	text := string(out)
	if err == nil {
		t.Fatalf("expected non-zero exit when rsync is missing in git mode\n%s", text)
	}
	if !strings.Contains(text, "rsync preflight failed") {
		t.Fatalf("expected rsync preflight failure message, got:\n%s", text)
	}
	if strings.Contains(text, "phase: pull") {
		t.Fatalf("must refuse before pull phase when rsync is missing\n%s", text)
	}
}

func setupUpdateShellTestRepo(t *testing.T) (string, string, string, string, []string) {
	t.Helper()
	bash := updateShellBash(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not available: %v", err)
	}
	if _, err := exec.LookPath("rsync"); err != nil {
		t.Skipf("rsync not available: %v", err)
	}
	scriptsDir := filepath.Dir(updateShellScriptPath(t))
	parent := t.TempDir()
	origin := filepath.Join(parent, "origin.git")
	deploy := filepath.Join(parent, "go-trader-deploy")
	work := filepath.Join(parent, "work")
	recRoot := filepath.Join(parent, "recovery")
	binDir := filepath.Join(parent, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	uvStub := "#!/usr/bin/env bash\n" +
		"if [[ \"$1\" == \"--version\" ]]; then echo 'uv 0.0.0'; exit 0; fi\n" +
		"mkdir -p .venv/bin\n" +
		"printf '#!/bin/sh\\necho Python 3.12.9\\n' > .venv/bin/python3\n" +
		"chmod +x .venv/bin/python3\n" +
		"echo synced > .venv/stamp\n"
	if err := os.WriteFile(filepath.Join(binDir, "uv"), []byte(uvStub), 0o755); err != nil {
		t.Fatal(err)
	}
	goStub := "#!/usr/bin/env bash\n" +
		"if [[ \"$1\" == \"version\" ]]; then echo 'go version go1.26.2'; exit 0; fi\n" +
		"dir=. out= ver=dev\n" +
		"while [[ $# -gt 0 ]]; do\n" +
		"  case \"$1\" in\n" +
		"    -C) dir=\"$2\"; shift 2 ;;\n" +
		"    -o) out=\"$2\"; shift 2 ;;\n" +
		"    -ldflags) ver=\"${2#*main.Version=}\"; ver=\"${ver%% *}\"; shift 2 ;;\n" +
		"    *) shift ;;\n" +
		"  esac\n" +
		"done\n" +
		"case \"$out\" in /*) ;; *) out=\"$dir/$out\" ;; esac\n" +
		"printf '#!/usr/bin/env bash\\nif [[ \"$1\" == \"version\" ]]; then echo \"%s\"; fi\\nexit 0\\n' \"$ver\" > \"$out\"\n" +
		"chmod +x \"$out\"\n"
	if err := os.WriteFile(filepath.Join(binDir, "go"), []byte(goStub), 0o755); err != nil {
		t.Fatal(err)
	}
	systemctlStub := "#!/usr/bin/env bash\n" +
		"case \"$*\" in\n" +
		"  --version) echo 'systemd 255' ;;\n" +
		"  *is-active*) exit 0 ;;\n" +
		"  *MainPID*) echo $$ ;;\n" +
		"  *) exit 0 ;;\n" +
		"esac\n"
	if err := os.WriteFile(filepath.Join(binDir, "systemctl"), []byte(systemctlStub), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "sudo"), []byte("#!/usr/bin/env bash\n\"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	curlStub := "#!/usr/bin/env bash\n" +
		"ver=$(./go-trader version 2>/dev/null || echo dev)\n" +
		"printf '{\"version\":\"%s\",\"pid\":99999}\\n' \"$ver\"\n"
	if err := os.WriteFile(filepath.Join(binDir, "curl"), []byte(curlStub), 0o755); err != nil {
		t.Fatal(err)
	}

	baseEnv := append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+filepath.Dir(bash)+string(os.PathListSeparator)+os.Getenv("PATH"),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GO_TRADER_UPDATE_RECOVERY_DIR="+recRoot,
	)
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Env = baseEnv
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	runGit("init", "--bare", "-b", "main", origin)
	runGit("clone", origin, deploy)
	for _, name := range []string{"update.sh", "update_helpers.sh"} {
		body, err := os.ReadFile(filepath.Join(scriptsDir, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(deploy, "scripts"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(deploy, "scripts", name), body, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(deploy, "scheduler"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deploy, "scheduler", "main.go"), []byte("package main\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deploy, "README.md"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deploy, ".gitignore"), []byte("go-trader\ngo-trader.*\nscheduler/config.json\nscheduler/tuning_runs/\nstate/\n.venv/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("-C", deploy, "add", ".")
	runGit("-C", deploy, "commit", "-m", "v1")
	runGit("-C", deploy, "push", "-u", "origin", "HEAD:main")

	runGit("clone", origin, work)
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "added_v2.txt"), []byte("new-in-v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("-C", work, "add", ".")
	runGit("-C", work, "commit", "-m", "v2")
	runGit("-C", work, "push", "origin", "HEAD:main")

	if err := os.WriteFile(filepath.Join(deploy, "scheduler", "config.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldBin := "#!/usr/bin/env bash\nif [[ \"$1\" == \"version\" ]]; then echo v1; fi\nexit 0\n"
	if err := os.WriteFile(filepath.Join(deploy, "go-trader"), []byte(oldBin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(deploy, ".venv", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deploy, ".venv", "bin", "python3"), []byte("#!/bin/sh\necho Python 3.12.9\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deploy, ".venv", "baseline.txt"), []byte("venv-v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return bash, deploy, work, recRoot, baseEnv
}

func waitForPauseReached(t *testing.T, sentinel string) string {
	t.Helper()
	reached := sentinel + ".reached"
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(reached); err == nil && len(strings.TrimSpace(string(data))) > 0 {
			return strings.TrimSpace(string(data))
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for pause sentinel %s", reached)
	return ""
}

func TestUpdateShellRefusedSecondUpdatePreservesHolderSnapshotAndNewBinary(t *testing.T) {
	t.Parallel()
	bash, deploy, _, recRoot, baseEnv := setupUpdateShellTestRepo(t)
	script := filepath.Join(deploy, "scripts", "update.sh")

	// Case (a) and (c): Pause holder after update_take_snapshot (snapshot_done).
	// A second refused update and a refused --all child must leave $recovery_dir/COMPLETE and managed/ intact,
	// and a forced holder build failure must end in recovery: complete.
	pauseSnap := filepath.Join(t.TempDir(), "pause-snap")
	holderCmd := exec.Command(bash, script)
	holderCmd.Dir = deploy
	holderCmd.Env = append(append([]string{}, baseEnv...),
		"GO_TRADER_UPDATE_PAUSE_AT=snapshot_done:"+pauseSnap,
		"GO_TRADER_UPDATE_FAIL_AT=build",
	)
	var holderOut strings.Builder
	holderCmd.Stdout = &holderOut
	holderCmd.Stderr = &holderOut
	if err := holderCmd.Start(); err != nil {
		t.Fatal(err)
	}
	holderPID := waitForPauseReached(t, pauseSnap)

	secondCmd := exec.Command(bash, script)
	secondCmd.Dir = deploy
	secondCmd.Env = baseEnv
	secondOut, secondErr := secondCmd.CombinedOutput()
	if secondErr == nil {
		t.Fatalf("expected second update to be refused by lock\n%s", secondOut)
	}
	if !strings.Contains(string(secondOut), "held by pid "+holderPID) {
		t.Fatalf("expected refused message to name holder pid %s, got:\n%s", holderPID, secondOut)
	}

	allCmd := exec.Command(bash, script, "--all", "--restart", "--update-all-root", filepath.Dir(deploy))
	allCmd.Dir = deploy
	allCmd.Env = baseEnv
	allOut, allErr := allCmd.CombinedOutput()
	if allErr == nil {
		t.Fatalf("expected --all child to be refused by holder lock\n%s", allOut)
	}
	if !strings.Contains(string(allOut), "another update is already") {
		t.Fatalf("expected --all output to report lock contention, got:\n%s", allOut)
	}

	entries, err := os.ReadDir(recRoot)
	if err != nil {
		t.Fatal(err)
	}
	var snapDir string
	for _, e := range entries {
		if e.IsDir() && !strings.HasSuffix(e.Name(), ".lock") {
			snapDir = filepath.Join(recRoot, e.Name())
		}
	}
	if snapDir == "" {
		t.Fatalf("holder snapshot directory was deleted by refused update")
	}
	for _, sub := range []string{"COMPLETE", "managed"} {
		if _, statErr := os.Stat(filepath.Join(snapDir, sub)); statErr != nil {
			t.Fatalf("holder snapshot %s missing %s after refused updates: %v", snapDir, sub, statErr)
		}
	}

	if err := os.WriteFile(pauseSnap, []byte("go\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = holderCmd.Wait()
	if !strings.Contains(holderOut.String(), "recovery: complete") {
		t.Fatalf("expected holder build failure to finish with recovery: complete, got:\n%s", holderOut.String())
	}

	// Case (b): Pause holder at probe after go-trader.new is built.
	// A refused second run leaves go-trader.new in place and the holder's swap succeeds.
	pauseProbe := filepath.Join(t.TempDir(), "pause-probe")
	probeHolder := exec.Command(bash, script)
	probeHolder.Dir = deploy
	probeHolder.Env = append(append([]string{}, baseEnv...),
		"GO_TRADER_UPDATE_PAUSE_AT=probe:"+pauseProbe,
	)
	var probeHolderOut strings.Builder
	probeHolder.Stdout = &probeHolderOut
	probeHolder.Stderr = &probeHolderOut
	if err := probeHolder.Start(); err != nil {
		t.Fatal(err)
	}
	waitForPauseReached(t, pauseProbe)
	if _, statErr := os.Stat(filepath.Join(deploy, "go-trader.new")); statErr != nil {
		t.Fatalf("expected go-trader.new to exist at probe pause: %v", statErr)
	}

	refusedAtProbe := exec.Command(bash, script)
	refusedAtProbe.Dir = deploy
	refusedAtProbe.Env = baseEnv
	if out, err := refusedAtProbe.CombinedOutput(); err == nil {
		t.Fatalf("expected second run during probe pause to be refused\n%s", out)
	}
	if _, statErr := os.Stat(filepath.Join(deploy, "go-trader.new")); statErr != nil {
		t.Fatalf("refused second run deleted holder's go-trader.new: %v", statErr)
	}
	if err := os.WriteFile(pauseProbe, []byte("go\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := probeHolder.Wait(); err != nil {
		t.Fatalf("expected probe holder to succeed after release: %v\n%s", err, probeHolderOut.String())
	}
	if !strings.Contains(probeHolderOut.String(), "build OK at") {
		t.Fatalf("expected holder swap and build OK, got:\n%s", probeHolderOut.String())
	}
}

func TestUpdateShellCrossAccountLockAndIncompleteMarker(t *testing.T) {
	t.Parallel()
	bash, deploy, _, _, baseEnv := setupUpdateShellTestRepo(t)
	script := filepath.Join(deploy, "scripts", "update.sh")

	sysRecRoot := filepath.Join(t.TempDir(), "sys-rec")
	userHome := filepath.Join(t.TempDir(), "user-home")
	if err := os.MkdirAll(userHome, 0o755); err != nil {
		t.Fatal(err)
	}

	// Recommended Optional 1(a): Non-root, HOME=/nonexistent, no XDG_STATE_HOME:
	// message names the recovery root it cannot create.
	var cleanEnv []string
	for _, kv := range baseEnv {
		if strings.HasPrefix(kv, "GO_TRADER_UPDATE_RECOVERY_DIR=") ||
			strings.HasPrefix(kv, "XDG_STATE_HOME=") ||
			strings.HasPrefix(kv, "HOME=") {
			continue
		}
		cleanEnv = append(cleanEnv, kv)
	}
	badHomeCmd := exec.Command(bash, script)
	badHomeCmd.Dir = deploy
	badHomeCmd.Env = append(append([]string{}, cleanEnv...), "HOME=/nonexistent")
	badHomeOut, badHomeErr := badHomeCmd.CombinedOutput()
	if badHomeErr == nil {
		t.Fatalf("expected HOME=/nonexistent update to fail\n%s", badHomeOut)
	}
	if !strings.Contains(string(badHomeOut), "/nonexistent/.local/state/go-trader-update") {
		t.Fatalf("expected failure to name /nonexistent/.local/state/go-trader-update, got:\n%s", badHomeOut)
	}
	if strings.Contains(string(badHomeOut), "another update is already") {
		t.Fatalf("unwritable recovery root must not be misreported as lock contention:\n%s", badHomeOut)
	}

	// Finding 2(a): Pause a system-root run after its snapshot; an owner-account run (using its own HOME) is refused.
	pauseSnap := filepath.Join(t.TempDir(), "pause-sys-snap")
	sysHolder := exec.Command(bash, script)
	sysHolder.Dir = deploy
	sysHolder.Env = append(append([]string{}, cleanEnv...),
		"GO_TRADER_UPDATE_RECOVERY_DIR="+sysRecRoot,
		"GO_TRADER_UPDATE_PAUSE_AT=snapshot_done:"+pauseSnap,
		"GO_TRADER_UPDATE_FAIL_AT=build",
	)
	if err := sysHolder.Start(); err != nil {
		t.Fatal(err)
	}
	waitForPauseReached(t, pauseSnap)

	ownerRun := exec.Command(bash, script)
	ownerRun.Dir = deploy
	ownerRun.Env = append(append([]string{}, cleanEnv...),
		"HOME="+userHome,
		"GO_TRADER_UPDATE_SYSTEM_RECOVERY_DIR="+sysRecRoot,
	)
	ownerOut, ownerErr := ownerRun.CombinedOutput()
	if ownerErr == nil {
		t.Fatalf("expected owner-account run to be refused while system-root run holds lock\n%s", ownerOut)
	}
	if !strings.Contains(string(ownerOut), "another update is already") {
		t.Fatalf("expected owner-account run to report active lock, got:\n%s", ownerOut)
	}
	_ = os.WriteFile(pauseSnap, []byte("go\n"), 0o644)
	_ = sysHolder.Wait()

	// Finding 2(b) and 2(c): A root run ends INCOMPLETE; a later owner-account run is refused,
	// and a non-root --clear-recovery does not report success while the root marker remains.
	incRun := exec.Command(bash, script)
	incRun.Dir = deploy
	incRun.Env = append(append([]string{}, cleanEnv...),
		"GO_TRADER_UPDATE_RECOVERY_DIR="+sysRecRoot,
		"GO_TRADER_UPDATE_FAIL_AT=build,restore_probe",
	)
	incOut, incErr := incRun.CombinedOutput()
	if incErr == nil || !strings.Contains(string(incOut), "recovery: INCOMPLETE") {
		t.Fatalf("expected INCOMPLETE recovery, got err=%v:\n%s", incErr, incOut)
	}
	entries, err := os.ReadDir(sysRecRoot)
	if err != nil {
		t.Fatal(err)
	}
	var markerPath string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".incomplete") {
			markerPath = filepath.Join(sysRecRoot, e.Name())
		}
	}
	if markerPath == "" {
		t.Fatalf("expected .incomplete marker in %s", sysRecRoot)
	}
	markerBytes, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate root ownership of the marker (owner_uid=0 and unwritable sysRecRoot).
	rootMarker := strings.ReplaceAll(string(markerBytes), fmt.Sprintf("owner_uid=%d", os.Geteuid()), "owner_uid=0")
	if err := os.WriteFile(markerPath, []byte(rootMarker), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sysRecRoot, 0o555); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(sysRecRoot, 0o755) }()

	ownerAfterInc := exec.Command(bash, script)
	ownerAfterInc.Dir = deploy
	ownerAfterInc.Env = append(append([]string{}, cleanEnv...),
		"HOME="+userHome,
		"GO_TRADER_UPDATE_SYSTEM_RECOVERY_DIR="+sysRecRoot,
	)
	ownerIncOut, ownerIncErr := ownerAfterInc.CombinedOutput()
	if ownerIncErr == nil || !strings.Contains(string(ownerIncOut), "incomplete recovery marker") {
		t.Fatalf("expected owner run to be refused by root incomplete marker, got err=%v:\n%s", ownerIncErr, ownerIncOut)
	}

	ownerClear := exec.Command(bash, script, "--clear-recovery")
	ownerClear.Dir = deploy
	ownerClear.Env = append(append([]string{}, cleanEnv...),
		"HOME="+userHome,
		"GO_TRADER_UPDATE_SYSTEM_RECOVERY_DIR="+sysRecRoot,
	)
	clearOut, clearErr := ownerClear.CombinedOutput()
	if clearErr == nil {
		t.Fatalf("expected non-root --clear-recovery to fail when root marker remains\n%s", clearOut)
	}
	if strings.Contains(string(clearOut), "cleared incomplete-recovery marker") {
		t.Fatalf("non-root --clear-recovery falsely reported success:\n%s", clearOut)
	}
	if !strings.Contains(string(clearOut), "sudo bash scripts/update.sh --clear-recovery") {
		t.Fatalf("expected non-root --clear-recovery to instruct sudo, got:\n%s", clearOut)
	}
	if _, statErr := os.Stat(markerPath); statErr != nil {
		t.Fatalf("root marker was removed by non-root --clear-recovery: %v", statErr)
	}
}

func TestUpdateShellKilledRunSnapshotProtection(t *testing.T) {
	t.Parallel()
	bash, deploy, _, recRoot, baseEnv := setupUpdateShellTestRepo(t)
	script := filepath.Join(deploy, "scripts", "update.sh")

	// Case (b): SIGKILL during snapshot, before COMPLETE exists. The next run proceeds normally.
	pauseBeforeComplete := filepath.Join(t.TempDir(), "pause-before-complete")
	cmdB := exec.Command(bash, script)
	cmdB.Dir = deploy
	cmdB.Env = append(append([]string{}, baseEnv...),
		"GO_TRADER_UPDATE_PAUSE_AT=snapshot_before_complete:"+pauseBeforeComplete,
	)
	if err := cmdB.Start(); err != nil {
		t.Fatal(err)
	}
	waitForPauseReached(t, pauseBeforeComplete)
	_ = cmdB.Process.Kill()
	_ = cmdB.Wait()

	// Next run after pre-COMPLETE SIGKILL (failing at build just to verify normal execution & recovery).
	nextAfterB := exec.Command(bash, script)
	nextAfterB.Dir = deploy
	nextAfterB.Env = append(append([]string{}, baseEnv...), "GO_TRADER_UPDATE_FAIL_AT=build")
	outAfterB, _ := nextAfterB.CombinedOutput()
	if !strings.Contains(string(outAfterB), "recovery: complete") {
		t.Fatalf("expected run after pre-COMPLETE SIGKILL to proceed normally and recover on build fail, got:\n%s", outAfterB)
	}

	// Case (a): SIGKILL during sync, after intents.log contains sync.
	// The next run refuses, and the old snapshot is byte-identical.
	pauseSync := filepath.Join(t.TempDir(), "pause-sync")
	cmdA := exec.Command(bash, script)
	cmdA.Dir = deploy
	cmdA.Env = append(append([]string{}, baseEnv...),
		"GO_TRADER_UPDATE_PAUSE_AT=sync:"+pauseSync,
	)
	if err := cmdA.Start(); err != nil {
		t.Fatal(err)
	}
	waitForPauseReached(t, pauseSync)
	_ = cmdA.Process.Kill()
	_ = cmdA.Wait()

	entries, err := os.ReadDir(recRoot)
	if err != nil {
		t.Fatal(err)
	}
	var snapDir string
	for _, e := range entries {
		if e.IsDir() && !strings.HasSuffix(e.Name(), ".lock") {
			snapDir = filepath.Join(recRoot, e.Name())
		}
	}
	if snapDir == "" {
		t.Fatalf("expected snapshot dir after SIGKILL during sync")
	}
	snapManifestBefore := computeDirDigest(t, snapDir)

	nextAfterA := exec.Command(bash, script)
	nextAfterA.Dir = deploy
	nextAfterA.Env = baseEnv
	outAfterA, errAfterA := nextAfterA.CombinedOutput()
	if errAfterA == nil {
		t.Fatalf("expected next run after SIGKILL during sync to refuse\n%s", outAfterA)
	}
	if !strings.Contains(string(outAfterA), "incomplete recovery marker") {
		t.Fatalf("expected refusal naming incomplete recovery marker, got:\n%s", outAfterA)
	}
	snapManifestAfter := computeDirDigest(t, snapDir)
	if snapManifestBefore != snapManifestAfter {
		t.Fatalf("snapshot directory changed after refused run:\nbefore:\n%s\nafter:\n%s", snapManifestBefore, snapManifestAfter)
	}

	// Clear recovery and restore README to v1 before testing Case (c).
	clearCmd := exec.Command(bash, script, "--clear-recovery")
	clearCmd.Dir = deploy
	clearCmd.Env = baseEnv
	if out, err := clearCmd.CombinedOutput(); err != nil {
		t.Fatalf("clear-recovery failed: %v\n%s", err, out)
	}
	gitReset := exec.Command("git", "-C", deploy, "reset", "--hard", "HEAD~1")
	gitReset.Env = baseEnv
	if out, err := gitReset.CombinedOutput(); err != nil {
		t.Fatalf("git reset: %v\n%s", err, out)
	}

	// Case (c): SIGKILL during recovery, after .venv was moved to .venv.restore-old. The next run refuses.
	pauseVenvOld := filepath.Join(t.TempDir(), "pause-venv-old")
	cmdC := exec.Command(bash, script)
	cmdC.Dir = deploy
	cmdC.Env = append(append([]string{}, baseEnv...),
		"GO_TRADER_UPDATE_FAIL_AT=build",
		"GO_TRADER_UPDATE_PAUSE_AT=restore_venv_moved_old:"+pauseVenvOld,
	)
	if err := cmdC.Start(); err != nil {
		t.Fatal(err)
	}
	waitForPauseReached(t, pauseVenvOld)
	_ = cmdC.Process.Kill()
	_ = cmdC.Wait()

	if _, statErr := os.Stat(filepath.Join(deploy, ".venv.restore-old")); statErr != nil {
		t.Fatalf("expected .venv.restore-old to exist after SIGKILL at restore_venv_moved_old: %v", statErr)
	}

	nextAfterC := exec.Command(bash, script)
	nextAfterC.Dir = deploy
	nextAfterC.Env = baseEnv
	outAfterC, errAfterC := nextAfterC.CombinedOutput()
	if errAfterC == nil {
		t.Fatalf("expected next run after SIGKILL during recovery to refuse\n%s", outAfterC)
	}
	if !strings.Contains(string(outAfterC), "incomplete recovery marker") {
		t.Fatalf("expected refusal naming incomplete recovery marker after recovery SIGKILL, got:\n%s", outAfterC)
	}
}

func computeDirDigest(t *testing.T, root string) string {
	t.Helper()
	var lines []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, _ := os.Readlink(path)
			lines = append(lines, "L:"+rel+"->"+target)
		} else if info.IsDir() {
			lines = append(lines, "D:"+rel)
		} else {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			lines = append(lines, fmt.Sprintf("F:%s:%d:%x", rel, len(data), data))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(lines, "\n")
}

func TestUpdateShellTuningRunsExcludedAcrossRsyncAndRecovery(t *testing.T) {
	t.Parallel()
	bash, deploy, work, _, baseEnv := setupUpdateShellTestRepo(t)
	script := filepath.Join(deploy, "scripts", "update.sh")

	// Case (a): Successful --rsync-from update with an existing scheduler/tuning_runs/<id>/results.json.
	tuningFileA := filepath.Join(deploy, "scheduler", "tuning_runs", "run-1", "results.json")
	if err := os.MkdirAll(filepath.Join(filepath.Dir(tuningFileA)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tuningFileA, []byte(`{"ok":true}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rsyncCmd := exec.Command(bash, script, "--rsync-from", work)
	rsyncCmd.Dir = deploy
	rsyncCmd.Env = baseEnv
	if out, err := rsyncCmd.CombinedOutput(); err != nil {
		t.Fatalf("rsync update failed: %v\n%s", err, out)
	}
	if got, err := os.ReadFile(tuningFileA); err != nil || string(got) != "{\"ok\":true}\n" {
		t.Fatalf("scheduler/tuning_runs/run-1/results.json did not survive --rsync-from: err=%v got=%q", err, got)
	}
	_ = os.Remove(filepath.Join(deploy, "added_v2.txt"))
	_ = os.WriteFile(filepath.Join(deploy, "README.md"), []byte("v1\n"), 0o644)

	// Case (b): Git-mode forced build failure, where a new scheduler/tuning_runs/<id>/ and a changed promotions.json
	// are written between the snapshot and the failure. Both survive recovery.
	promoFile := filepath.Join(deploy, "scheduler", "tuning_runs", "promotions.json")
	if err := os.WriteFile(promoFile, []byte(`{"v":1}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pauseSync := filepath.Join(t.TempDir(), "pause-tuning-sync")
	gitFailCmd := exec.Command(bash, script)
	gitFailCmd.Dir = deploy
	gitFailCmd.Env = append(append([]string{}, baseEnv...),
		"GO_TRADER_UPDATE_PAUSE_AT=sync:"+pauseSync,
		"GO_TRADER_UPDATE_FAIL_AT=build",
	)
	if err := gitFailCmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitForPauseReached(t, pauseSync)
	tuningFileB := filepath.Join(deploy, "scheduler", "tuning_runs", "run-2", "results.json")
	if err := os.MkdirAll(filepath.Dir(tuningFileB), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tuningFileB, []byte(`{"run":2}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(promoFile, []byte(`{"v":2}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(pauseSync, []byte("go\n"), 0o644)
	_ = gitFailCmd.Wait()

	if got, err := os.ReadFile(tuningFileB); err != nil || string(got) != "{\"run\":2}\n" {
		t.Fatalf("new tuning run did not survive recovery: err=%v got=%q", err, got)
	}
	if got, err := os.ReadFile(promoFile); err != nil || string(got) != "{\"v\":2}\n" {
		t.Fatalf("updated promotions.json was rolled back by recovery: err=%v got=%q", err, got)
	}

	// Case (c): scheduler/config.json is a symlink to another in-tree path, state/config.json.
	// state/tuning_runs/ is protected across forward rsync and recovery.
	stateDir := filepath.Join(deploy, "state")
	if err := os.MkdirAll(filepath.Join(stateDir, "tuning_runs", "run-3"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "config.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stateTuningFile := filepath.Join(stateDir, "tuning_runs", "run-3", "results.json")
	if err := os.WriteFile(stateTuningFile, []byte(`{"state":3}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(filepath.Join(deploy, "scheduler", "config.json"))
	if err := os.Symlink(filepath.Join(stateDir, "config.json"), filepath.Join(deploy, "scheduler", "config.json")); err != nil {
		t.Fatal(err)
	}
	rsyncStateCmd := exec.Command(bash, script, "--rsync-from", work)
	rsyncStateCmd.Dir = deploy
	rsyncStateCmd.Env = baseEnv
	if out, err := rsyncStateCmd.CombinedOutput(); err != nil {
		t.Fatalf("rsync with state/config.json symlink failed: %v\n%s", err, out)
	}
	if got, err := os.ReadFile(stateTuningFile); err != nil || string(got) != "{\"state\":3}\n" {
		t.Fatalf("state/tuning_runs/run-3/results.json did not survive --rsync-from: err=%v got=%q", err, got)
	}
}

func TestUpdateShellAllForwardsSIGINTAndTerminalCtrlCToChildRecovery(t *testing.T) {
	t.Parallel()
	bash, deploy, _, _, baseEnv := setupUpdateShellTestRepo(t)
	script := filepath.Join(deploy, "scripts", "update.sh")
	fleetRoot := filepath.Dir(deploy)

	// Case (a): --all with the child paused at sync: SIGINT to the parent ends the child with recovery: complete.
	pauseSyncA := filepath.Join(t.TempDir(), "pause-all-int")
	allCmd := exec.Command(bash, script, "--all", "--restart", "--update-all-root", fleetRoot)
	allCmd.Dir = deploy
	allCmd.Env = append(append([]string{}, baseEnv...),
		"GO_TRADER_UPDATE_PAUSE_AT=sync:"+pauseSyncA,
	)
	var allOut strings.Builder
	allCmd.Stdout = &allOut
	allCmd.Stderr = &allOut
	if err := allCmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitForPauseReached(t, pauseSyncA)
	if err := allCmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	_ = allCmd.Wait()
	if !strings.Contains(allOut.String(), "recovery: complete") {
		t.Fatalf("expected SIGINT to --all parent to trigger child recovery: complete, got:\n%s", allOut.String())
	}

	// Case (b): Ctrl-C at the terminal during --all (via a real pseudo-terminal sending \x03).
	pauseSyncB := filepath.Join(t.TempDir(), "pause-all-pty")
	pyPty := `
import os, pty, select, subprocess, sys, time
bash_bin, script_path, deploy_dir, fleet_root, pause_file = sys.argv[1:6]
master, slave = pty.openpty()
env = os.environ.copy()
env["GO_TRADER_UPDATE_PAUSE_AT"] = "sync:" + pause_file
proc = subprocess.Popen(
    [bash_bin, script_path, "--all", "--restart", "--update-all-root", fleet_root],
    cwd=deploy_dir, env=env, stdin=slave, stdout=slave, stderr=slave, close_fds=True, start_new_session=True
)
os.close(slave)
buf = b""
deadline = time.time() + 15
while not os.path.exists(pause_file + ".reached") and time.time() < deadline:
    r, _, _ = select.select([master], [], [], 0.1)
    if r:
        try:
            buf += os.read(master, 4096)
        except OSError:
            break
os.write(master, b"\x03")
while True:
    r, _, _ = select.select([master], [], [], 5)
    if not r:
        break
    try:
        chunk = os.read(master, 4096)
        if not chunk:
            break
        buf += chunk
    except OSError:
        break
rc = proc.wait(timeout=15)
os.close(master)
sys.stdout.write(buf.decode("utf-8", "replace"))
sys.exit(0 if rc != 0 else 1)
`
	ptyCmd := exec.Command("python3", "-c", pyPty, bash, script, deploy, fleetRoot, pauseSyncB)
	ptyCmd.Env = baseEnv
	ptyOut, ptyErr := ptyCmd.CombinedOutput()
	if ptyErr != nil {
		t.Fatalf("pty Ctrl-C during --all failed: %v\n%s", ptyErr, ptyOut)
	}
	if !strings.Contains(string(ptyOut), "recovery: complete") {
		t.Fatalf("expected terminal Ctrl-C during --all to finish with recovery: complete, got:\n%s", ptyOut)
	}
}
