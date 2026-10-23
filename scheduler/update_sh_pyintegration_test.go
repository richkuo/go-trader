//go:build pyintegration

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
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
	cmd.Env = append(gitEnv,
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"GO_TRADER_UPDATE_RECOVERY_DIR="+filepath.Join(parent, "rec"),
	)
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
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("git not available: %v", err)
	}
	if _, err := exec.LookPath("rsync"); err != nil {
		t.Skipf("rsync not available: %v", err)
	}
	scriptsDir := filepath.Dir(updateShellScriptPath(t))

	runCase := func(mode string, recSleepSec string) {
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
			cmd := exec.Command(realGit, args...)
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
		if err := os.WriteFile(filepath.Join(deploy, "scheduler", "main.go"), []byte("package main\nfunc main() {}\n"), 0o644); err != nil {
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
		childPidFile := filepath.Join(parent, "child.pid")
		switch mode {
		case "uv":
			uvStub := "#!/usr/bin/env bash\n" +
				"if [[ \"$1\" == \"--version\" ]]; then echo 'uv 0.0.0'; exit 0; fi\n" +
				"echo $$ > " + childPidFile + "\n" +
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
		case "git_pull":
			gitStub := "#!/bin/sh\n" +
				"case \" $* \" in\n" +
				"  *\" pull \"*)\n" +
				"    echo $$ > " + childPidFile + "\n" +
				"    sleep 30\n" +
				"    exit 1\n" +
				"    ;;\n" +
				"esac\n" +
				"exec " + realGit + " \"$@\"\n"
			if err := os.WriteFile(filepath.Join(binDir, "git"), []byte(gitStub), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(binDir, "uv"), []byte("#!/usr/bin/env bash\nexit 0\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(binDir, "go"), []byte("#!/usr/bin/env bash\nexit 0\n"), 0o755); err != nil {
				t.Fatal(err)
			}
		case "go_build":
			goStub := "#!/usr/bin/env bash\n" +
				"if [[ \"$1\" == \"version\" || \"$1\" == \"--version\" ]]; then echo 'go version go1.26.2'; exit 0; fi\n" +
				"echo $$ > " + childPidFile + "\n" +
				"sleep 30\n" +
				"exit 1\n"
			if err := os.WriteFile(filepath.Join(binDir, "go"), []byte(goStub), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(binDir, "uv"), []byte("#!/usr/bin/env bash\nexit 0\n"), 0o755); err != nil {
				t.Fatal(err)
			}
		}

		for _, warm := range []string{
			filepath.Join(deploy, "go-trader"),
			filepath.Join(binDir, "uv"),
			filepath.Join(binDir, "go"),
			filepath.Join(binDir, "git"),
		} {
			if _, err := os.Stat(warm); err == nil {
				_ = exec.Command(warm, "--version").Run()
			}
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
		t.Setenv("GO_TRADER_UPDATE_RECOVERY_SLEEP_SEC", recSleepSec)
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
			t.Fatalf("[%s] expected Starting upgrade and failure DMs, got %v", mode, rec.dms)
		}
		lastDM := rec.dms[len(rec.dms)-1]
		for _, want := range []string{"**update.sh failed**", "interrupted by SIGTERM", "recovery: complete"} {
			if !strings.Contains(lastDM, want) {
				t.Fatalf("[%s] expected DM to contain %q, got:\n%s", mode, want, lastDM)
			}
		}
		readme, err := os.ReadFile(filepath.Join(deploy, "README.md"))
		if err != nil {
			t.Fatal(err)
		}
		if string(readme) != "baseline\n" {
			t.Fatalf("[%s] expected README.md restored to baseline, got %q", mode, string(readme))
		}
		if _, statErr := os.Stat(filepath.Join(deploy, ".venv")); !os.IsNotExist(statErr) {
			t.Fatalf("[%s] expected .venv absence restored, stat err=%v", mode, statErr)
		}
		if pidBytes, err := os.ReadFile(childPidFile); err == nil {
			pidStr := strings.TrimSpace(string(pidBytes))
			if exec.Command("kill", "-0", pidStr).Run() == nil {
				t.Fatalf("[%s] child pid %s is still alive after applyUpgrade timeout recovery", mode, pidStr)
			}
		}
	}

	runCase("uv", "45")
	runCase("git_pull", "")
	runCase("go_build", "")
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
	procSelf := filepath.Join(parent, "proc", "self")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(procSelf, 0o755); err != nil {
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
		"printf '#!/usr/bin/env bash\\nif [[ \"$1\" == \"version\" ]]; then echo \"%s\"; exit 0; fi\\nif [[ \"$1\" == \"probe\" || \"$1\" == \"agent-info\" ]]; then exit 0; fi\\nexec sleep 60\\n' \"$ver\" > \"$out\"\n" +
		"chmod +x \"$out\"\n"
	if err := os.WriteFile(filepath.Join(binDir, "go"), []byte(goStub), 0o755); err != nil {
		t.Fatal(err)
	}
	systemctlStub := "#!/usr/bin/env bash\n" +
		"if [[ \"${GO_TRADER_TEST_SYSTEMCTL_INACTIVE:-0}\" == \"1\" ]]; then\n" +
		"  case \"$*\" in\n" +
		"    *is-active*) echo inactive; exit 3 ;;\n" +
		"    *MainPID*) echo 0; exit 0 ;;\n" +
		"    *) exit 0 ;;\n" +
		"  esac\n" +
		"fi\n" +
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
	setsidStub := "#!/usr/bin/env python3\nimport os, sys\ntry:\n    os.setsid()\nexcept OSError:\n    pass\nos.execvp(sys.argv[1], sys.argv[1:])\n"
	if err := os.WriteFile(filepath.Join(binDir, "setsid"), []byte(setsidStub), 0o755); err != nil {
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
		"GO_TRADER_PROC_ROOT="+filepath.Join(parent, "proc"),
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
	if err := os.WriteFile(filepath.Join(deploy, ".gitignore"), []byte("go-trader\ngo-trader.*\nrun.sh\nwrapper.pid\nscheduler/config.json\nscheduler/tuning_runs/\nscheduler/logs/\nstate/\n.venv/\n"), 0o644); err != nil {
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
	oldBin := "#!/usr/bin/env bash\nif [[ \"$1\" == \"version\" ]]; then echo v1; exit 0; fi\nif [[ \"$1\" == \"probe\" || \"$1\" == \"agent-info\" ]]; then exit 0; fi\nexec sleep 60\n"
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
	userRecRoot := filepath.Join(userHome, ".local", "state", "go-trader-update")
	if err := os.MkdirAll(userHome, 0o755); err != nil {
		t.Fatal(err)
	}

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

	singleRun := exec.Command(bash, script)
	singleRun.Dir = deploy
	singleRun.Env = append(append([]string{}, cleanEnv...),
		"HOME="+userHome,
		"GO_TRADER_UPDATE_SYSTEM_RECOVERY_DIR="+sysRecRoot,
	)
	if out, err := singleRun.CombinedOutput(); err != nil || !strings.Contains(string(out), "build OK at") {
		t.Fatalf("expected single run without contention to acquire lock and succeed: err=%v\n%s", err, out)
	}
	resetGit := exec.Command("git", "-C", deploy, "reset", "--hard", "HEAD~1")
	resetGit.Env = cleanEnv
	if out, err := resetGit.CombinedOutput(); err != nil {
		t.Fatalf("git reset: %v\n%s", err, out)
	}

	pauseBeforeMkdir := filepath.Join(t.TempDir(), "pause-before-mkdir")
	pauseOwnerSnap := filepath.Join(t.TempDir(), "pause-owner-snap")
	rootRace := exec.Command(bash, script)
	rootRace.Dir = deploy
	rootRace.Env = append(append([]string{}, cleanEnv...),
		"HOME="+userHome,
		"GO_TRADER_UPDATE_RECOVERY_DIR="+sysRecRoot,
		"GO_TRADER_UPDATE_SYSTEM_RECOVERY_DIR="+sysRecRoot,
		"GO_TRADER_UPDATE_PAUSE_AT=lock_before_mkdir:"+pauseBeforeMkdir,
	)
	var rootRaceOut strings.Builder
	rootRace.Stdout = &rootRaceOut
	rootRace.Stderr = &rootRaceOut
	if err := rootRace.Start(); err != nil {
		t.Fatal(err)
	}
	waitForPauseReached(t, pauseBeforeMkdir)

	ownerRace := exec.Command(bash, script)
	ownerRace.Dir = deploy
	ownerRace.Env = append(append([]string{}, cleanEnv...),
		"HOME="+userHome,
		"GO_TRADER_UPDATE_SYSTEM_RECOVERY_DIR="+sysRecRoot,
		"GO_TRADER_UPDATE_PAUSE_AT=snapshot_done:"+pauseOwnerSnap,
		"GO_TRADER_UPDATE_FAIL_AT=build",
	)
	var ownerRaceOut strings.Builder
	ownerRace.Stdout = &ownerRaceOut
	ownerRace.Stderr = &ownerRaceOut
	if err := ownerRace.Start(); err != nil {
		t.Fatal(err)
	}
	ownerPID := waitForPauseReached(t, pauseOwnerSnap)

	_ = os.WriteFile(pauseBeforeMkdir, []byte("go\n"), 0o644)
	if err := rootRace.Wait(); err == nil {
		t.Fatalf("expected root run to be refused by post-mkdir recheck\n%s", rootRaceOut.String())
	}
	if strings.Contains(rootRaceOut.String(), "phase: snapshot") {
		t.Fatalf("root run reached phase: snapshot despite owner run holding lock:\n%s", rootRaceOut.String())
	}
	sysEntries, _ := os.ReadDir(sysRecRoot)
	for _, e := range sysEntries {
		if strings.HasSuffix(e.Name(), ".lock") {
			t.Fatalf("refused root run did not remove its own lock directory %s", e.Name())
		}
	}
	userEntries, err := os.ReadDir(userRecRoot)
	if err != nil {
		t.Fatalf("read userRecRoot: %v", err)
	}
	foundOwnerLock := false
	for _, e := range userEntries {
		if strings.HasSuffix(e.Name(), ".lock") {
			pidBytes, err := os.ReadFile(filepath.Join(userRecRoot, e.Name(), "pid"))
			if err == nil && strings.TrimSpace(string(pidBytes)) == ownerPID {
				foundOwnerLock = true
			}
		}
	}
	if !foundOwnerLock {
		t.Fatalf("owner run lock directory in %s was missing or modified by refused root run", userRecRoot)
	}
	_ = os.WriteFile(pauseOwnerSnap, []byte("go\n"), 0o644)
	_ = ownerRace.Wait()

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

	nextAfterB := exec.Command(bash, script)
	nextAfterB.Dir = deploy
	nextAfterB.Env = append(append([]string{}, baseEnv...), "GO_TRADER_UPDATE_FAIL_AT=build")
	outAfterB, _ := nextAfterB.CombinedOutput()
	if !strings.Contains(string(outAfterB), "recovery: complete") {
		t.Fatalf("expected run after pre-COMPLETE SIGKILL to proceed normally and recover on build fail, got:\n%s", outAfterB)
	}

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

	tuningFileA := filepath.Join(deploy, "scheduler", "tuning_runs", "run-1", "results.json")
	if err := os.MkdirAll(filepath.Dir(tuningFileA), 0o755); err != nil {
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

func TestUpdateShellSignalModePreservesWrapperAndRecoversOnHealthFail(t *testing.T) {
	t.Parallel()
	bash, deploy, _, _, baseEnv := setupUpdateShellTestRepo(t)
	script := filepath.Join(deploy, "scripts", "update.sh")
	sigEnv := append(append([]string{}, baseEnv...), "GO_TRADER_TEST_SYSTEMCTL_INACTIVE=1")

	startDummyTrader := func() *exec.Cmd {
		t.Helper()
		cmd := exec.Command("sleep", "60")
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		pidStr := fmt.Sprintf("%d\n", cmd.Process.Pid)
		go func() { _ = cmd.Wait() }()
		if err := os.WriteFile(filepath.Join(deploy, "go-trader.pid"), []byte(pidStr), 0o644); err != nil {
			t.Fatal(err)
		}
		return cmd
	}
	killIfAlive := func(pidStr string) {
		pidStr = strings.TrimSpace(pidStr)
		if pidStr != "" {
			_ = exec.Command("kill", "-KILL", pidStr).Run()
		}
	}

	execRunSh := "#!/usr/bin/env bash\necho $$ > ./go-trader.pid\nexec ./go-trader --config scheduler/config.json\n"
	if err := os.WriteFile(filepath.Join(deploy, "run.sh"), []byte(execRunSh), 0o755); err != nil {
		t.Fatal(err)
	}
	oldCmdA := startDummyTrader()
	defer func() { _ = oldCmdA.Process.Kill() }()

	updA := exec.Command(bash, script, "--restart", "--restart-mode", "signal")
	updA.Dir = deploy
	updA.Env = sigEnv
	if out, err := updA.CombinedOutput(); err != nil {
		t.Fatalf("signal mode update (a) failed: %v\n%s", err, out)
	}
	pidBytesA, err := os.ReadFile(filepath.Join(deploy, "go-trader.pid"))
	if err != nil {
		t.Fatal(err)
	}
	newPidA := strings.TrimSpace(string(pidBytesA))
	defer killIfAlive(newPidA)
	time.Sleep(10 * time.Second)
	if err := exec.Command("kill", "-0", newPidA).Run(); err != nil {
		t.Fatalf("exec-style wrapper pid %s was killed after signal-mode update exit: %v", newPidA, err)
	}
	killIfAlive(newPidA)

	resetCmd := exec.Command("git", "-C", deploy, "reset", "--hard", "HEAD~1")
	resetCmd.Env = sigEnv
	if out, err := resetCmd.CombinedOutput(); err != nil {
		t.Fatalf("git reset: %v\n%s", err, out)
	}
	oldBin := "#!/usr/bin/env bash\nif [[ \"$1\" == \"version\" ]]; then echo v1; exit 0; fi\nif [[ \"$1\" == \"probe\" || \"$1\" == \"agent-info\" ]]; then exit 0; fi\nexec sleep 60\n"
	if err := os.WriteFile(filepath.Join(deploy, "go-trader"), []byte(oldBin), 0o755); err != nil {
		t.Fatal(err)
	}

	loopRunSh := "#!/usr/bin/env bash\necho $$ > ./wrapper.pid\nwhile true; do\n  ./go-trader --config scheduler/config.json &\n  c=$!\n  echo $c > ./go-trader.pid\n  wait $c || exit 0\ndone\n"
	if err := os.WriteFile(filepath.Join(deploy, "run.sh"), []byte(loopRunSh), 0o755); err != nil {
		t.Fatal(err)
	}
	oldCmdB := startDummyTrader()
	defer func() { _ = oldCmdB.Process.Kill() }()

	updB := exec.Command(bash, script, "--restart", "--restart-mode", "signal")
	updB.Dir = deploy
	updB.Env = sigEnv
	if out, err := updB.CombinedOutput(); err != nil {
		t.Fatalf("signal mode update (b) failed: %v\n%s", err, out)
	}
	wrapBytesB, err := os.ReadFile(filepath.Join(deploy, "wrapper.pid"))
	if err != nil {
		t.Fatal(err)
	}
	wrapPidB := strings.TrimSpace(string(wrapBytesB))
	pidBytesB, err := os.ReadFile(filepath.Join(deploy, "go-trader.pid"))
	if err != nil {
		t.Fatal(err)
	}
	traderPidB := strings.TrimSpace(string(pidBytesB))
	defer killIfAlive(wrapPidB)
	defer killIfAlive(traderPidB)
	time.Sleep(3 * time.Second)
	if err := exec.Command("kill", "-0", wrapPidB).Run(); err != nil {
		t.Fatalf("loop wrapper pid %s was killed after signal-mode update exit: %v", wrapPidB, err)
	}
	killIfAlive(wrapPidB)
	killIfAlive(traderPidB)

	resetCmdC := exec.Command("git", "-C", deploy, "reset", "--hard", "HEAD~1")
	resetCmdC.Env = sigEnv
	if out, err := resetCmdC.CombinedOutput(); err != nil {
		t.Fatalf("git reset: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(deploy, "go-trader"), []byte(oldBin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deploy, "run.sh"), []byte(execRunSh), 0o755); err != nil {
		t.Fatal(err)
	}

	oldCmdC := startDummyTrader()
	defer func() { _ = oldCmdC.Process.Kill() }()

	updC := exec.Command(bash, script, "--restart", "--restart-mode", "signal")
	updC.Dir = deploy
	updC.Env = append(append([]string{}, sigEnv...), "GO_TRADER_UPDATE_FAIL_AT=verify_health")
	outC, errC := updC.CombinedOutput()
	if errC == nil {
		t.Fatalf("expected verify_health failure to exit non-zero\n%s", outC)
	}
	if !strings.Contains(string(outC), "recovery: complete") {
		t.Fatalf("expected signal-mode verify_health failure to end in recovery: complete, got:\n%s", outC)
	}
	verOut, err := exec.Command(filepath.Join(deploy, "go-trader"), "version").CombinedOutput()
	if err != nil || strings.TrimSpace(string(verOut)) != "v1" {
		t.Fatalf("expected restored binary version v1, got err=%v out=%q", err, verOut)
	}
	pidBytesC, err := os.ReadFile(filepath.Join(deploy, "go-trader.pid"))
	if err != nil {
		t.Fatal(err)
	}
	recPidC := strings.TrimSpace(string(pidBytesC))
	defer killIfAlive(recPidC)
	time.Sleep(3 * time.Second)
	if err := exec.Command("kill", "-0", recPidC).Run(); err != nil {
		t.Fatalf("recovered signal-mode trader pid %s is not alive: %v", recPidC, err)
	}
	killIfAlive(recPidC)
}

func TestUpdateShellInodeOnlyStaleMarkerAndChangedDeviceRefusal(t *testing.T) {
	t.Parallel()
	bash, deploy, _, recRoot, baseEnv := setupUpdateShellTestRepo(t)
	script := filepath.Join(deploy, "scripts", "update.sh")

	pauseSync := filepath.Join(t.TempDir(), "pause-dev-sync")
	cmd := exec.Command(bash, script)
	cmd.Dir = deploy
	cmd.Env = append(append([]string{}, baseEnv...), "GO_TRADER_UPDATE_PAUSE_AT=sync:"+pauseSync)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitForPauseReached(t, pauseSync)
	_ = cmd.Process.Kill()
	_ = cmd.Wait()

	st, err := os.Stat(deploy)
	if err != nil {
		t.Fatal(err)
	}
	sysStat := st.Sys().(*syscall.Stat_t)
	realDevIno := fmt.Sprintf("%d:%d", sysStat.Dev, sysStat.Ino)
	fakeDevSameIno := fmt.Sprintf("%d:%d", sysStat.Dev+99999, sysStat.Ino)

	entries, err := os.ReadDir(recRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		p := filepath.Join(recRoot, e.Name())
		if e.IsDir() && !strings.HasSuffix(e.Name(), ".lock") {
			mf := filepath.Join(p, "manifest.env")
			if data, err := os.ReadFile(mf); err == nil {
				_ = os.WriteFile(mf, []byte(strings.ReplaceAll(string(data), realDevIno, fakeDevSameIno)), 0o600)
			}
		} else if strings.HasSuffix(e.Name(), ".state") {
			if data, err := os.ReadFile(p); err == nil {
				_ = os.WriteFile(p, []byte(strings.ReplaceAll(string(data), realDevIno, fakeDevSameIno)), 0o644)
			}
		}
	}

	runSameIno := exec.Command(bash, script)
	runSameIno.Dir = deploy
	runSameIno.Env = baseEnv
	outA, errA := runSameIno.CombinedOutput()
	if errA == nil {
		t.Fatalf("expected killed-run snapshot with changed st_dev and same inode to refuse\n%s", outA)
	}
	if !strings.Contains(string(outA), "incomplete recovery marker") {
		t.Fatalf("expected refusal naming incomplete recovery marker, got:\n%s", outA)
	}

	origin := filepath.Join(filepath.Dir(deploy), "origin.git")
	movedAside := deploy + ".old-inode"
	if err := os.Rename(deploy, movedAside); err != nil {
		t.Fatal(err)
	}
	cloneCmd := exec.Command("git", "clone", origin, deploy)
	cloneCmd.Env = baseEnv
	if out, err := cloneCmd.CombinedOutput(); err != nil {
		t.Fatalf("git clone: %v\n%s", err, out)
	}
	resetV1 := exec.Command("git", "-C", deploy, "reset", "--hard", "HEAD~1")
	resetV1.Env = baseEnv
	if out, err := resetV1.CombinedOutput(); err != nil {
		t.Fatalf("git reset: %v\n%s", err, out)
	}
	_ = os.RemoveAll(movedAside)
	if err := os.WriteFile(filepath.Join(deploy, "scheduler", "config.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deploy, "go-trader"), []byte("#!/usr/bin/env bash\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	runNewIno := exec.Command(bash, filepath.Join(deploy, "scripts", "update.sh"))
	runNewIno.Dir = deploy
	runNewIno.Env = baseEnv
	outB, errB := runNewIno.CombinedOutput()
	if errB != nil || !strings.Contains(string(outB), "build OK at") {
		t.Fatalf("expected marker from old inode not to block re-cloned directory: err=%v\n%s", errB, outB)
	}
}

func TestUpdateShellFreeSpaceCheckIgnoresExcludedLogs(t *testing.T) {
	t.Parallel()
	bash, deploy, _, _, baseEnv := setupUpdateShellTestRepo(t)
	script := filepath.Join(deploy, "scripts", "update.sh")

	sigLog := filepath.Join(deploy, "go-trader-signal.log")
	fA, err := os.Create(sigLog)
	if err != nil {
		t.Fatal(err)
	}
	if err := fA.Truncate(10 * 1024 * 1024 * 1024); err != nil {
		_ = fA.Close()
		t.Fatal(err)
	}
	_ = fA.Close()

	cmdA := exec.Command(bash, script)
	cmdA.Dir = deploy
	cmdA.Env = append(append([]string{}, baseEnv...), "GO_TRADER_UPDATE_FAKE_FREE_BYTES=50000000")
	outA, errA := cmdA.CombinedOutput()
	if errA != nil || !strings.Contains(string(outA), "build OK at") {
		t.Fatalf("expected 10 GB sparse go-trader-signal.log to be ignored by snapshot space check: err=%v\n%s", errA, outA)
	}
	_ = os.Remove(sigLog)

	resetCmd := exec.Command("git", "-C", deploy, "reset", "--hard", "HEAD~1")
	resetCmd.Env = baseEnv
	if out, err := resetCmd.CombinedOutput(); err != nil {
		t.Fatalf("git reset: %v\n%s", err, out)
	}

	if err := os.WriteFile(filepath.Join(deploy, "scheduler", "config.json"), []byte(`{"log_dir":"scheduler/logs"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	customLogsDir := filepath.Join(deploy, "scheduler", "logs")
	if err := os.MkdirAll(customLogsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fB, err := os.Create(filepath.Join(customLogsDir, "big.log"))
	if err != nil {
		t.Fatal(err)
	}
	if err := fB.Truncate(10 * 1024 * 1024 * 1024); err != nil {
		_ = fB.Close()
		t.Fatal(err)
	}
	_ = fB.Close()

	cmdB := exec.Command(bash, script)
	cmdB.Dir = deploy
	cmdB.Env = append(append([]string{}, baseEnv...), "GO_TRADER_UPDATE_FAKE_FREE_BYTES=50000000")
	outB, errB := cmdB.CombinedOutput()
	if errB != nil || !strings.Contains(string(outB), "build OK at") {
		t.Fatalf("expected 10 GB sparse custom log_dir file to be ignored by snapshot space check: err=%v\n%s", errB, outB)
	}
}

func TestUpdateShellForeignRecoveryRootModePreservationAndPreRebootLock(t *testing.T) {
	t.Parallel()
	bash, deploy, _, _, baseEnv := setupUpdateShellTestRepo(t)
	script := filepath.Join(deploy, "scripts", "update.sh")

	ownerHome := filepath.Join(t.TempDir(), "owner-home")
	ownerRec := filepath.Join(ownerHome, ".local", "state", "go-trader-update")
	sysRec := filepath.Join(t.TempDir(), "sys-rec")
	if err := os.MkdirAll(ownerRec, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(ownerRec, 0o700); err != nil {
		t.Fatal(err)
	}

	var cleanEnv []string
	for _, kv := range baseEnv {
		if strings.HasPrefix(kv, "GO_TRADER_UPDATE_RECOVERY_DIR=") ||
			strings.HasPrefix(kv, "XDG_STATE_HOME=") ||
			strings.HasPrefix(kv, "HOME=") {
			continue
		}
		cleanEnv = append(cleanEnv, kv)
	}

	pauseKill := filepath.Join(t.TempDir(), "pause-owner-kill")
	ownerKilled := exec.Command(bash, script)
	ownerKilled.Dir = deploy
	ownerKilled.Env = append(append([]string{}, cleanEnv...),
		"HOME="+ownerHome,
		"GO_TRADER_UPDATE_PAUSE_AT=sync:"+pauseKill,
	)
	if err := ownerKilled.Start(); err != nil {
		t.Fatal(err)
	}
	waitForPauseReached(t, pauseKill)
	_ = ownerKilled.Process.Kill()
	_ = ownerKilled.Wait()

	fakeUidBin := filepath.Join(t.TempDir(), "fakeuid-bin")
	if err := os.MkdirAll(fakeUidBin, 0o755); err != nil {
		t.Fatal(err)
	}
	realStat, err := exec.LookPath("stat")
	if err != nil {
		t.Fatal(err)
	}
	statWrap := "#!/usr/bin/env bash\n" +
		"if [[ \"$1 $2\" == \"-c %u\" || \"$1 $2\" == \"-f %u\" ]]; then\n" +
		"  if [[ \"$3\" == \"" + ownerRec + "\" ]]; then echo 99999; exit 0; fi\n" +
		"fi\n" +
		"exec " + realStat + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(fakeUidBin, "stat"), []byte(statWrap), 0o755); err != nil {
		t.Fatal(err)
	}
	envWithFakeUid := append([]string{"PATH=" + fakeUidBin + string(os.PathListSeparator) + os.Getenv("PATH")}, cleanEnv...)
	for i, kv := range envWithFakeUid {
		if i > 0 && strings.HasPrefix(kv, "PATH=") {
			envWithFakeUid[0] = "PATH=" + fakeUidBin + string(os.PathListSeparator) + strings.TrimPrefix(kv, "PATH=")
		}
	}

	rootDetect := exec.Command(bash, script)
	rootDetect.Dir = deploy
	rootDetect.Env = append(append([]string{}, envWithFakeUid...),
		"HOME="+ownerHome,
		"GO_TRADER_UPDATE_RECOVERY_DIR="+sysRec,
		"GO_TRADER_UPDATE_SYSTEM_RECOVERY_DIR="+sysRec,
	)
	detectOut, detectErr := rootDetect.CombinedOutput()
	if detectErr == nil || !strings.Contains(string(detectOut), "incomplete recovery marker") {
		t.Fatalf("expected root run to refuse killed owner snapshot: err=%v\n%s", detectErr, detectOut)
	}
	infoAfterDetect, err := os.Stat(ownerRec)
	if err != nil {
		t.Fatal(err)
	}
	if infoAfterDetect.Mode().Perm() != 0o700 {
		t.Fatalf("root killed-run detection changed owner recovery root mode from 0700 to %04o", infoAfterDetect.Mode().Perm())
	}

	rootClear := exec.Command(bash, script, "--clear-recovery")
	rootClear.Dir = deploy
	rootClear.Env = append(append([]string{}, envWithFakeUid...),
		"HOME="+ownerHome,
		"GO_TRADER_UPDATE_RECOVERY_DIR="+sysRec,
		"GO_TRADER_UPDATE_SYSTEM_RECOVERY_DIR="+sysRec,
	)
	if out, err := rootClear.CombinedOutput(); err != nil {
		t.Fatalf("root --clear-recovery failed: %v\n%s", err, out)
	}
	infoAfterClear, err := os.Stat(ownerRec)
	if err != nil {
		t.Fatal(err)
	}
	if infoAfterClear.Mode().Perm() != 0o700 {
		t.Fatalf("root --clear-recovery changed owner recovery root mode from 0700 to %04o", infoAfterClear.Mode().Perm())
	}

	resetV1 := exec.Command("git", "-C", deploy, "reset", "--hard", "HEAD~1")
	resetV1.Env = cleanEnv
	if out, err := resetV1.CombinedOutput(); err != nil {
		t.Fatalf("git reset: %v\n%s", err, out)
	}

	ownerInc := exec.Command(bash, script)
	ownerInc.Dir = deploy
	ownerInc.Env = append(append([]string{}, cleanEnv...),
		"HOME="+ownerHome,
		"GO_TRADER_UPDATE_FAIL_AT=build,restore_probe",
	)
	if out, err := ownerInc.CombinedOutput(); err == nil || !strings.Contains(string(out), "recovery: INCOMPLETE") {
		t.Fatalf("expected owner INCOMPLETE run, got err=%v\n%s", err, out)
	}
	if err := os.Chmod(ownerRec, 0o700); err != nil {
		t.Fatal(err)
	}
	rootClearInc := exec.Command(bash, script, "--clear-recovery")
	rootClearInc.Dir = deploy
	rootClearInc.Env = append(append([]string{}, envWithFakeUid...),
		"HOME="+ownerHome,
		"GO_TRADER_UPDATE_RECOVERY_DIR="+sysRec,
		"GO_TRADER_UPDATE_SYSTEM_RECOVERY_DIR="+sysRec,
	)
	if out, err := rootClearInc.CombinedOutput(); err != nil {
		t.Fatalf("root --clear-recovery after owner INCOMPLETE failed: %v\n%s", err, out)
	}
	infoAfterIncClear, err := os.Stat(ownerRec)
	if err != nil {
		t.Fatal(err)
	}
	if infoAfterIncClear.Mode().Perm() != 0o700 {
		t.Fatalf("root --clear-recovery after owner INCOMPLETE changed mode from 0700 to %04o", infoAfterIncClear.Mode().Perm())
	}

	ownerFollowup := exec.Command(bash, script)
	ownerFollowup.Dir = deploy
	ownerFollowup.Env = append(append([]string{}, cleanEnv...),
		"HOME="+ownerHome,
		"GO_TRADER_UPDATE_FAIL_AT=build",
	)
	followOut, _ := ownerFollowup.CombinedOutput()
	if !strings.Contains(string(followOut), "phase: snapshot") || !strings.Contains(string(followOut), "recovery: complete") {
		t.Fatalf("expected owner update after root --clear-recovery to reach phase: snapshot and complete recovery:\n%s", followOut)
	}

	sleepCmd := exec.Command("sleep", "60")
	if err := sleepCmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sleepCmd.Process.Kill() }()
	go func() { _ = sleepCmd.Wait() }()
	sleepPid := fmt.Sprintf("%d", sleepCmd.Process.Pid)

	pauseBoot := filepath.Join(t.TempDir(), "pause-boot-kill")
	killedForBoot := exec.Command(bash, script)
	killedForBoot.Dir = deploy
	killedForBoot.Env = append(append([]string{}, cleanEnv...),
		"HOME="+ownerHome,
		"GO_TRADER_UPDATE_BOOT_ID=boot-old",
		"GO_TRADER_UPDATE_PAUSE_AT=sync:"+pauseBoot,
	)
	if err := killedForBoot.Start(); err != nil {
		t.Fatal(err)
	}
	waitForPauseReached(t, pauseBoot)
	_ = killedForBoot.Process.Kill()
	_ = killedForBoot.Wait()

	recEntries, err := os.ReadDir(ownerRec)
	if err != nil {
		t.Fatal(err)
	}
	var lockDir string
	for _, e := range recEntries {
		if strings.HasSuffix(e.Name(), ".lock") {
			lockDir = filepath.Join(ownerRec, e.Name())
		}
	}
	if lockDir == "" {
		t.Fatalf("expected lock directory in %s after SIGKILL", ownerRec)
	}
	if err := os.WriteFile(filepath.Join(lockDir, "pid"), []byte(sleepPid+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lockDir, "boot_id"), []byte("boot-old\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	sameBootRun := exec.Command(bash, script)
	sameBootRun.Dir = deploy
	sameBootRun.Env = append(append([]string{}, cleanEnv...),
		"HOME="+ownerHome,
		"GO_TRADER_UPDATE_BOOT_ID=boot-old",
	)
	sameBootOut, sameBootErr := sameBootRun.CombinedOutput()
	if sameBootErr == nil || !strings.Contains(string(sameBootOut), "held by pid "+sleepPid) {
		t.Fatalf("expected same-boot live holder to refuse with pid %s, got err=%v:\n%s", sleepPid, sameBootErr, sameBootOut)
	}

	newBootRun := exec.Command(bash, script)
	newBootRun.Dir = deploy
	newBootRun.Env = append(append([]string{}, cleanEnv...),
		"HOME="+ownerHome,
		"GO_TRADER_UPDATE_BOOT_ID=boot-new",
	)
	newBootOut, newBootErr := newBootRun.CombinedOutput()
	if newBootErr == nil {
		t.Fatalf("expected new-boot run to hit killed-run incomplete marker refusal\n%s", newBootOut)
	}
	if !strings.Contains(string(newBootOut), "removing stale lock") {
		t.Fatalf("expected new-boot run to remove stale pre-reboot lock, got:\n%s", newBootOut)
	}
	if !strings.Contains(string(newBootOut), "incomplete recovery marker") {
		t.Fatalf("expected new-boot run to proceed to killed-run check and refuse on incomplete marker, got:\n%s", newBootOut)
	}
}
