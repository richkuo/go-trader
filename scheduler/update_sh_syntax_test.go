package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
)

type shellSuiteWiring struct {
	goTest       string
	goTestFile   string
	ciStep       bool
	sudo         bool
	manualReason string
	marker       string
	requireEnv   []string
	forbidden    []string
	permitted    map[string]string
}

var shellSuiteWirings = map[string]shellSuiteWiring{
	"test_merge_paper_instance.sh": {
		goTest:     "TestMergePaperInstance",
		goTestFile: "merge_paper_instance_test.go",
		marker:     "OK: merge-paper-instance tests passed",
	},
	"test_update_helpers.sh": {
		goTest:     "TestUpdateHelpersEnvfileParsing790",
		goTestFile: "update_sh_pyintegration_test.go",
		marker:     "OK: update_helpers tests passed",
		forbidden: []string{
			"note: GO_TRADER_BIN unset; effective-cadence drift case skipped",
			"note: this git does not simulate another owner; the trust and refusal cases are skipped",
			"note: git not installed; update_git cases skipped",
			"exists; the uv-missing case is skipped",
			"note: go or git not installed; the export build case is skipped",
		},
	},
	"test_observation_replay.sh": {
		ciStep: true,
		marker: "PASS: observation replay harness (sealed payload decisions match recording replay; tampered and truncated recordings refused)",
	},
	"test_ledger_export.sh": {
		ciStep:     true,
		marker:     "PASS: ledger capture and export (active-WAL capture without source effects, version 2 export contract, every refusal leaves inputs unchanged)",
		requireEnv: []string{"LEDGER_EXPORT_REQUIRE_CAPTURE", "LEDGER_EXPORT_REQUIRE_STRACE"},
		forbidden: []string{
			"NOTE: strace is not installed; the syscall audit of capture is skipped",
			"NOTE: unshare --user is unavailable here; the shared-mount-namespace worker refusal in a new user namespace is not checked",
			"NOTE: running as root; the permission-denied refusal is not exercised",
		},
	},
	"test_merge_paper_service_fixture.sh": {
		ciStep:     true,
		sudo:       true,
		marker:     "OK: merge-paper service fixture passed (exit 79 for the second start)",
		requireEnv: []string{"MERGE_PAPER_SERVICE_FIXTURE_REQUIRE_RUN"},
	},
	"test_container_image.sh": {
		manualReason: "needs Docker and a built image; .github/workflows/container.yml runs it through scripts/run_ci_shell_suite.sh on linux/amd64 and linux/arm64",
		marker:       "PASS: container image (identity, clean layers, volume setup, bind and token policy, auth, SIGHUP reload, dashboard and crash restarts, persistence, graceful stop with a Python child, fatal holds 78/79/80, early-failure restarts, one-shot exits, backup and restore)",
		forbidden: []string{
			"NOTE: GO_TRADER_TEST_REQUIRE_ONCE=0",
		},
	},
	"test_migrate_service_layout_fixture.sh": {
		ciStep:     true,
		sudo:       true,
		marker:     "OK: migrate-service-layout fixture passed (refuse plan confirm apply latch conflict resume signal stages kill fold update newtarget)",
		requireEnv: []string{"MIGRATE_SERVICE_LAYOUT_FIXTURE_REQUIRE_RUN"},
		forbidden: []string{
			"note: this host has a system uv; the root-private uv refusals are skipped",
		},
	},
}

func assertShellSuiteOutput(t *testing.T, script string, out []byte) {
	t.Helper()
	w, ok := shellSuiteWirings[script]
	if !ok {
		t.Fatalf("scripts/%s has no declared wiring in shellSuiteWirings", script)
	}
	sawMarker := false
	var problems []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == w.marker {
			sawMarker = true
		}
		if strings.HasPrefix(line, "SKIP:") {
			problems = append(problems, "SKIP line: "+line)
		}
		for _, text := range w.forbidden {
			if strings.Contains(line, text) {
				problems = append(problems, "omission not on the permitted list: "+line)
			}
		}
		for text, reason := range w.permitted {
			if strings.Contains(line, text) {
				t.Logf("permitted omission (%s): %s", reason, line)
			}
		}
	}
	if !sawMarker {
		problems = append(problems, "missing success line: "+w.marker)
	}
	if len(problems) > 0 {
		t.Fatalf("scripts/%s did not prove its criteria:\n%s\n--- output:\n%s", script, strings.Join(problems, "\n"), out)
	}
}

func shellSuiteRepoRoot(t *testing.T) (string, string) {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	schedDir := filepath.Dir(thisFile)
	return schedDir, filepath.Join(schedDir, "..")
}

func TestUpdateShellScriptSyntax(t *testing.T) {
	t.Parallel()
	_, repoRoot := shellSuiteRepoRoot(t)
	names := map[string]struct{}{}
	for _, name := range []string{
		"update.sh", "update_helpers.sh", "create-run-sh.sh", "test_update_helpers.sh", "migrate-config-out-of-tree.sh",
		"check-live-paper-config-drift.sh", "merge-paper-instance.sh", "test_merge_paper_instance.sh", "test_merge_paper_service_fixture.sh",
		"shared-feed-convert.sh", "feed-parity.sh", "feed-source-compare.sh", "run_ci_shell_suite.sh",
	} {
		names[name] = struct{}{}
	}
	suites, err := filepath.Glob(filepath.Join(repoRoot, "scripts", "test_*.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if len(suites) == 0 {
		t.Fatal("found no scripts/test_*.sh suites")
	}
	for _, suite := range suites {
		names[filepath.Base(suite)] = struct{}{}
	}
	sorted := make([]string, 0, len(names))
	for name := range names {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)
	for _, name := range sorted {
		script := filepath.Join(repoRoot, "scripts", name)
		out, err := exec.Command("bash", "-n", script).CombinedOutput()
		if err != nil {
			t.Fatalf("bash -n scripts/%s: %v\n%s", name, err, out)
		}
	}
}

type ciWorkflowJob struct {
	raw      []string
	logical  [][]string
	problems []string
}

var (
	ciJobHeader   = regexp.MustCompile(`^  ([A-Za-z0-9_-]+):\s*$`)
	ciRunKey      = regexp.MustCompile(`^(\s*)(- )?run:\s*(.*)$`)
	ciStepIf      = regexp.MustCompile(`^\s*(- )?if:`)
	ciShellKey    = regexp.MustCompile(`^\s*(- )?(shell|defaults):`)
	ciTeeTarget   = regexp.MustCompile(`2>&1 \| tee ("[^"]+"|\S+)$`)
	ciRunPattern  = regexp.MustCompile(`-run '([^']*)'`)
	ciSuiteInvoke = regexp.MustCompile(`bash scripts/(test_[A-Za-z0-9_.-]+\.sh)`)
	ciOmissionMsg = regexp.MustCompile(`^\s*echo "((?:NOTE|note): [^"]*)"`)
	ciGateEcho    = regexp.MustCompile("^echo \"[^\"$`\\\\]*\"$")
)

func parseCIWorkflowJobs(t *testing.T, path string) map[string]*ciWorkflowJob {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	lines := strings.Split(string(data), "\n")
	jobsAt := -1
	for i, line := range lines {
		if strings.HasPrefix(line, "defaults:") {
			t.Fatalf("%s has a top-level defaults: block, which can change the shell every run: step uses", path)
		}
		if line == "jobs:" {
			if jobsAt >= 0 {
				t.Fatalf("%s has more than one top-level jobs: key", path)
			}
			jobsAt = i
		}
	}
	if jobsAt < 0 {
		t.Fatalf("%s has no top-level jobs: key", path)
	}
	jobs := map[string]*ciWorkflowJob{}
	var cur *ciWorkflowJob
	for _, line := range lines[jobsAt+1:] {
		if line != "" && line[0] != ' ' {
			break
		}
		if m := ciJobHeader.FindStringSubmatch(line); m != nil {
			if _, dup := jobs[m[1]]; dup {
				t.Fatalf("%s declares job %s twice", path, m[1])
			}
			cur = &ciWorkflowJob{}
			jobs[m[1]] = cur
			continue
		}
		if cur != nil {
			cur.raw = append(cur.raw, line)
		}
	}
	for name, job := range jobs {
		for i := 0; i < len(job.raw); i++ {
			m := ciRunKey.FindStringSubmatch(job.raw[i])
			if m == nil {
				continue
			}
			keyCol := len(m[1]) + len(m[2])
			value := strings.TrimSpace(m[3])
			var body []string
			switch value {
			case "|", "|-":
				for i+1 < len(job.raw) {
					next := job.raw[i+1]
					if strings.TrimSpace(next) != "" && len(next)-len(strings.TrimLeft(next, " ")) <= keyCol {
						break
					}
					body = append(body, strings.TrimSpace(next))
					i++
				}
			case "":
				job.problems = append(job.problems, fmt.Sprintf("job %s has an empty run: value", name))
				continue
			default:
				if strings.ContainsAny(value[:1], `|>'"`) {
					job.problems = append(job.problems, fmt.Sprintf("job %s uses a run: form this guard cannot read (%s); use a plain value or a | block", name, value))
					continue
				}
				body = []string{value}
			}
			job.logical = append(job.logical, ciLogicalLines(body))
		}
	}
	return jobs
}

func ciLogicalLines(body []string) []string {
	var out []string
	pending := ""
	for _, line := range body {
		if strings.HasSuffix(line, `\`) {
			pending += strings.TrimSuffix(line, `\`)
			continue
		}
		line = pending + line
		pending = ""
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		out = append(out, strings.Join(strings.Fields(trimmed), " "))
	}
	if strings.TrimSpace(pending) != "" {
		out = append(out, strings.Join(strings.Fields(pending), " "))
	}
	return out
}

func requireCIJob(t *testing.T, jobs map[string]*ciWorkflowJob, name string) *ciWorkflowJob {
	t.Helper()
	job, ok := jobs[name]
	if !ok {
		t.Fatalf(".github/workflows/ci.yml has no %s job", name)
	}
	for _, line := range job.raw {
		if strings.Contains(line, "continue-on-error") {
			t.Fatalf("job %s sets continue-on-error, so a failing suite would not fail CI", name)
		}
		if ciStepIf.MatchString(line) {
			t.Fatalf("job %s has a conditional (%s), so a suite step might not run", name, strings.TrimSpace(line))
		}
		if ciShellKey.MatchString(line) {
			t.Fatalf("job %s overrides the step shell or defaults (%s), so a failing command might not fail the step", name, strings.TrimSpace(line))
		}
	}
	if len(job.problems) > 0 {
		t.Fatal(strings.Join(job.problems, "\n"))
	}
	if len(job.logical) == 0 {
		t.Fatalf("job %s has no run: steps", name)
	}
	return job
}

func goTestFuncBody(t *testing.T, path, name string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	src := string(data)
	if !strings.HasPrefix(src, "//go:build pyintegration\n") {
		t.Fatalf("%s does not carry the pyintegration build tag the go-python-integration job passes", filepath.Base(path))
	}
	decl := "\nfunc " + name + "(t *testing.T) {\n"
	if strings.Count(src, decl) != 1 {
		t.Fatalf("%s does not declare %s exactly once", filepath.Base(path), name)
	}
	body := src[strings.Index(src, decl)+len(decl):]
	end := strings.Index(body, "\n}\n")
	if end < 0 {
		t.Fatalf("cannot find the end of %s in %s", name, filepath.Base(path))
	}
	return body[:end]
}

func TestShellSuiteCIWiring(t *testing.T) {
	t.Parallel()
	schedDir, repoRoot := shellSuiteRepoRoot(t)
	suites, err := filepath.Glob(filepath.Join(repoRoot, "scripts", "test_*.sh"))
	if err != nil {
		t.Fatal(err)
	}
	present := map[string]bool{}
	for _, suite := range suites {
		present[filepath.Base(suite)] = true
	}
	var names []string
	for name := range present {
		names = append(names, name)
	}
	for name := range shellSuiteWirings {
		if !present[name] {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	jobs := parseCIWorkflowJobs(t, filepath.Join(repoRoot, ".github", "workflows", "ci.yml"))
	pyJob := requireCIJob(t, jobs, "go-python-integration")
	shellJob := requireCIJob(t, jobs, "shell-suites")

	var runRegex *regexp.Regexp
	sawSkipGate := false
	for _, step := range pyJob.logical {
		pipefail := false
		teeLog := ""
		gateOpen := false
		gateExits := false
		for _, line := range step {
			matches := ciRunPattern.FindAllStringSubmatch(line, -1)
			if strings.Count(line, "-run") != len(matches) {
				t.Fatalf("go-python-integration has a -run flag this guard cannot read: %s", line)
			}
			if gateOpen {
				switch {
				case line == "fi":
					if !gateExits {
						t.Fatal("the go-python-integration SKIP gate must run exit 1 before fi, or a --- SKIP line does not fail the step")
					}
					gateOpen = false
					sawSkipGate = true
				case line == "exit 1":
					gateExits = true
				case ciGateEcho.MatchString(line):
				default:
					t.Fatalf("the go-python-integration SKIP gate may hold only plain echo lines and exit 1 before fi: %s", line)
				}
				continue
			}
			if line == "set -o pipefail" {
				pipefail = true
			}
			if teeLog != "" && strings.Contains(line, "--- SKIP") {
				want := "if grep -n -- '--- SKIP' " + teeLog + "; then"
				if line != want {
					t.Fatalf("the go-python-integration SKIP gate must be exactly %q: %s", want, line)
				}
				gateOpen = true
				gateExits = false
				continue
			}
			if len(matches) == 0 {
				continue
			}
			if runRegex != nil || len(matches) != 1 {
				t.Fatal("go-python-integration must have exactly one -run '...' argument")
			}
			if !strings.HasPrefix(line, "go -C scheduler test ") || !strings.Contains(line, " -tags pyintegration ") || !strings.Contains(line, " -v ") {
				t.Fatalf("the go-python-integration -run line must be a verbose pyintegration go test: %s", line)
			}
			if !pipefail {
				t.Fatalf("the go-python-integration step must run set -o pipefail before the piped go test, or a failing test passes: %s", line)
			}
			tee := ciTeeTarget.FindStringSubmatch(line)
			if tee == nil {
				t.Fatalf("the go-python-integration go test must end with 2>&1 | tee <log> so the SKIP gate reads its output: %s", line)
			}
			teeLog = tee[1]
			runRegex, err = regexp.Compile(matches[0][1])
			if err != nil {
				t.Fatalf("go-python-integration -run regex does not compile: %v", err)
			}
		}
		if gateOpen {
			t.Fatal("the go-python-integration SKIP gate has no closing fi")
		}
	}
	if runRegex == nil {
		t.Fatal("go-python-integration has no -run '...' argument")
	}
	if !sawSkipGate {
		t.Fatal("go-python-integration does not fail when a selected test reports --- SKIP in the log its go test writes")
	}

	invoked := map[string]int{}
	for _, step := range shellJob.logical {
		for _, line := range step {
			all := strings.Count(line, "scripts/test_")
			found := ciSuiteInvoke.FindAllStringSubmatch(line, -1)
			if all == 0 {
				continue
			}
			if all != 1 || len(found) != 1 {
				t.Fatalf("shell-suites must invoke one suite per command as bash scripts/test_<name>.sh: %s", line)
			}
			script := found[0][1]
			invoked[script]++
			w, ok := shellSuiteWirings[script]
			if !ok || !w.ciStep {
				t.Fatalf("shell-suites runs scripts/%s, which is not declared as a shell-suites step", script)
			}
			if strings.Contains(line, "#") {
				t.Fatalf("the shell-suites command for scripts/%s contains #, so part of it may be a comment: %s", script, line)
			}
			const runner = "bash scripts/run_ci_shell_suite.sh "
			if !strings.HasPrefix(line, runner) {
				t.Fatalf("shell-suites must run scripts/%s through scripts/run_ci_shell_suite.sh: %s", script, line)
			}
			sep := strings.Index(line, " -- ")
			if sep < 0 || strings.Count(line, " -- ") != 1 {
				t.Fatalf("shell-suites command for scripts/%s must have one -- separator: %s", script, line)
			}
			opts, cmd := line[len(runner):sep], line[sep+len(" -- "):]
			if !strings.HasSuffix(cmd, "bash scripts/"+script) {
				t.Fatalf("shell-suites command for scripts/%s must end with bash scripts/%s: %s", script, script, cmd)
			}
			if w.sudo != strings.HasPrefix(cmd, "sudo env ") || (!w.sudo && strings.Contains(cmd, "sudo")) {
				t.Fatalf("scripts/%s sudo=%v, but its shell-suites command is: %s", script, w.sudo, cmd)
			}
			if !strings.Contains(opts, "--marker '"+w.marker+"'") {
				t.Fatalf("shell-suites command for scripts/%s does not require its success line %q", script, w.marker)
			}
			if strings.Count(opts, "--forbid '") != len(w.forbidden) {
				t.Fatalf("shell-suites command for scripts/%s must forbid exactly its %d unpermitted omissions: %s", script, len(w.forbidden), opts)
			}
			for _, text := range w.forbidden {
				if !strings.Contains(opts, "--forbid '"+text+"'") {
					t.Fatalf("shell-suites command for scripts/%s does not forbid %q", script, text)
				}
			}
			for _, env := range w.requireEnv {
				if !strings.Contains(" "+cmd+" ", " "+env+"=1 ") {
					t.Fatalf("shell-suites command for scripts/%s does not set %s=1", script, env)
				}
			}
		}
	}

	for _, name := range names {
		w, ok := shellSuiteWirings[name]
		if !ok {
			t.Errorf("scripts/%s has no declared wiring: add it to shellSuiteWirings as a Go wrapper test, a shell-suites step, or manual-only with a reason", name)
			continue
		}
		if !present[name] {
			t.Errorf("shellSuiteWirings declares scripts/%s, which does not exist", name)
			continue
		}
		kinds := 0
		if w.goTest != "" {
			kinds++
		}
		if w.ciStep {
			kinds++
		}
		if w.manualReason != "" {
			kinds++
		}
		if kinds != 1 {
			t.Errorf("scripts/%s must declare exactly one wiring (Go wrapper, shell-suites step, or manual-only reason)", name)
			continue
		}
		if w.manualReason == "" && w.marker == "" {
			t.Errorf("scripts/%s declares no success line", name)
		}
		switch {
		case w.goTest != "":
			if !runRegex.MatchString(w.goTest) {
				t.Errorf("the go-python-integration -run regex does not select %s, so scripts/%s never runs", w.goTest, name)
			}
			body := goTestFuncBody(t, filepath.Join(schedDir, w.goTestFile), w.goTest)
			if !strings.Contains(body, `"`+name+`"`) || !strings.Contains(body, "exec.Command(") {
				t.Errorf("%s does not run scripts/%s", w.goTest, name)
			}
			if !strings.Contains(body, `assertShellSuiteOutput(t, "`+name+`", `) {
				t.Errorf("%s does not check scripts/%s output with assertShellSuiteOutput", w.goTest, name)
			}
		case w.ciStep:
			if invoked[name] != 1 {
				t.Errorf("shell-suites runs scripts/%s %d times, want exactly once", name, invoked[name])
			}
		}

		src, err := os.ReadFile(filepath.Join(repoRoot, "scripts", name))
		if err != nil {
			t.Fatal(err)
		}
		for _, env := range w.requireEnv {
			if !strings.Contains(string(src), env) {
				t.Errorf("scripts/%s does not read %s", name, env)
			}
		}
		for text, reason := range w.permitted {
			if strings.TrimSpace(reason) == "" {
				t.Errorf("scripts/%s permits omission %q without a reason", name, text)
			}
		}
		declared := append([]string{}, w.forbidden...)
		for text := range w.permitted {
			declared = append(declared, text)
		}
		used := map[string]bool{}
		for _, line := range strings.Split(string(src), "\n") {
			m := ciOmissionMsg.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			hits := 0
			for _, text := range declared {
				if strings.Contains(m[1], text) {
					hits++
					used[text] = true
				}
			}
			if hits != 1 {
				t.Errorf("scripts/%s omission line %q must match exactly one forbidden or permitted omission", name, m[1])
			}
		}
		for _, text := range declared {
			if !used[text] {
				t.Errorf("scripts/%s declares omission %q, which the script never prints", name, text)
			}
		}
	}
}

func TestRunCIShellSuiteVerdicts(t *testing.T) {
	t.Parallel()
	_, repoRoot := shellSuiteRepoRoot(t)
	runner := filepath.Join(repoRoot, "scripts", "run_ci_shell_suite.sh")
	const marker = "OK: stub suite passed"
	const forbidden = "note: stub check skipped"
	cases := []struct {
		name   string
		script string
		pass   bool
	}{
		{"marker only", `printf '%s\n' "$1"; exit 0`, true},
		{"marker and SKIP line", `printf '%s\nSKIP: x\n' "$1"; exit 0`, false},
		{"marker as a prefix only", `printf '%s extra\n' "$1"; exit 0`, false},
		{"marker, exit 3 and forbidden text", `printf '%s\n%s\n' "$1" "$2"; exit 3`, false},
		{"marker and exit 3", `printf '%s\n' "$1"; exit 3`, false},
		{"marker and forbidden text", `printf '%s\nx %s\n' "$1" "$2"; exit 0`, false},
		{"marker and SKIP not at line start", `printf '%s\nnote: SKIP: x\n' "$1"; exit 0`, true},
		{"no marker", `printf 'OK\n'; exit 0`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cmd := exec.Command("bash", runner, "--marker", marker, "--forbid", forbidden, "--", "bash", "-c", tc.script, "stub", marker, forbidden)
			cmd.Env = append(os.Environ(), "RUNNER_TEMP="+t.TempDir())
			out, err := cmd.CombinedOutput()
			if err != nil {
				if _, ok := err.(*exec.ExitError); !ok {
					t.Fatalf("run %s: %v\n%s", runner, err, out)
				}
			}
			if got := err == nil; got != tc.pass {
				t.Fatalf("run_ci_shell_suite.sh passed=%v, want %v\n%s", got, tc.pass, out)
			}
		})
	}
}
