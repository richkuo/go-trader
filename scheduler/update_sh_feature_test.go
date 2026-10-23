package main

import (
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
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	origTimeout := applyUpgradeTimeout
	applyUpgradeTimeout = 3 * time.Second
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
