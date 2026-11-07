//go:build pyintegration

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestUpdateShellProbeReadsUnitDirectionalCertPath1906(t *testing.T) {
	t.Parallel()
	bash, deploy, work, recRoot, baseEnv := setupUpdateShellTestRepo(t)
	binDir := filepath.Join(filepath.Dir(recRoot), "bin")
	record := filepath.Join(t.TempDir(), "probe-cert-path")
	certFile := filepath.Join(t.TempDir(), "unit-certs.json")
	envFile := filepath.Join(t.TempDir(), "unit.env")
	if err := os.WriteFile(envFile, []byte("OTHER=1\nGO_TRADER_DIRECTIONAL_CERT_PATH="+certFile+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	src, err := os.ReadFile(filepath.Join(filepath.Dir(updateShellScriptPath(t)), "migrate-service-layout.py"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "scripts", "migrate-service-layout.py"), src, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"-C", work, "add", "."}, {"-C", work, "commit", "-m", "v3"}, {"-C", work, "push", "origin", "HEAD:main"}} {
		cmd := exec.Command("git", args...)
		cmd.Env = baseEnv
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	goStub := "#!/usr/bin/env bash\n" +
		"if [[ \"$1\" == \"version\" ]]; then echo 'go version go1.27.3'; exit 0; fi\n" +
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
		"printf '#!/usr/bin/env bash\\nif [[ \"$1\" == \"version\" ]]; then echo \"%s\"; exit 0; fi\\nif [[ \"$1\" == \"probe\" ]]; then printf \"%%s\" \"${GO_TRADER_DIRECTIONAL_CERT_PATH-unset}\" > \"%s\"; exit 0; fi\\nif [[ \"$1\" == \"agent-info\" ]]; then exit 0; fi\\nexec sleep 60\\n' \"$ver\" \"" + record + "\" > \"$out\"\n" +
		"chmod +x \"$out\"\n"
	if err := os.WriteFile(filepath.Join(binDir, "go"), []byte(goStub), 0o755); err != nil {
		t.Fatal(err)
	}
	systemctlStub := "#!/usr/bin/env bash\n" +
		"case \"$*\" in\n" +
		"  --version) echo 'systemd 255' ;;\n" +
		"  *is-active*) exit 0 ;;\n" +
		"  *MainPID*) echo $$ ;;\n" +
		"  *EnvironmentFiles*) echo '" + envFile + " (ignore_errors=no)' ;;\n" +
		"  *'-p Environment '*) echo 'GO_TRADER_DIRECTIONAL_CERT_PATH=/inline/ignored.json' ;;\n" +
		"  *) exit 0 ;;\n" +
		"esac\n"
	if err := os.WriteFile(filepath.Join(binDir, "systemctl"), []byte(systemctlStub), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bash, filepath.Join(deploy, "scripts", "update.sh"))
	cmd.Dir = deploy
	cmd.Env = append(append([]string{}, baseEnv...), "GO_TRADER_DIRECTIONAL_CERT_PATH=/update/shell/value.json")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("update.sh failed: %v\n%s", err, out)
	}
	got, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("the probe never ran: %v\n%s", err, out)
	}
	if string(got) != certFile {
		t.Fatalf("probe read GO_TRADER_DIRECTIONAL_CERT_PATH=%q, want the unit's EnvironmentFile value %q\n%s", got, certFile, out)
	}
	if !strings.Contains(string(out), "GO_TRADER_DIRECTIONAL_CERT_PATH="+certFile) {
		t.Fatalf("update.sh did not report the resolved artifact path\n%s", out)
	}
}

func TestUpdateShellRecoveryProbeSkipsRegimeSideAdmission1906(t *testing.T) {
	t.Parallel()
	bash, deploy, _, recRoot, baseEnv := setupUpdateShellTestRepo(t)
	binDir := filepath.Join(filepath.Dir(recRoot), "bin")
	newRecord := filepath.Join(t.TempDir(), "new-probe-admission")
	oldRecord := filepath.Join(t.TempDir(), "old-probe-admission")

	goStub := "#!/usr/bin/env bash\n" +
		"if [[ \"$1\" == \"version\" ]]; then echo 'go version go1.27.3'; exit 0; fi\n" +
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
		"printf '#!/usr/bin/env bash\\nif [[ \"$1\" == \"version\" ]]; then echo \"%s\"; exit 0; fi\\nif [[ \"$1\" == \"probe\" ]]; then printf \"%%s\" \"${GO_TRADER_PROBE_REGIME_SIDE_ADMISSION-unset}\" > \"%s\"; exit 0; fi\\nif [[ \"$1\" == \"agent-info\" ]]; then exit 0; fi\\nexec sleep 60\\n' \"$ver\" \"" + newRecord + "\" > \"$out\"\n" +
		"chmod +x \"$out\"\n"
	if err := os.WriteFile(filepath.Join(binDir, "go"), []byte(goStub), 0o755); err != nil {
		t.Fatal(err)
	}
	stopped := filepath.Join(t.TempDir(), "stopped")
	systemctlStub := "#!/usr/bin/env bash\n" +
		"case \"$*\" in\n" +
		"  --version) echo 'systemd 255' ;;\n" +
		"  stop\\ *) touch \"" + stopped + "\" ;;\n" +
		"  start\\ *|restart\\ *) rm -f \"" + stopped + "\" ;;\n" +
		"  *is-active*) exit 0 ;;\n" +
		"  *MainPID*) if [[ -f \"" + stopped + "\" ]]; then echo 0; else echo $$; fi ;;\n" +
		"  *) exit 0 ;;\n" +
		"esac\n"
	if err := os.WriteFile(filepath.Join(binDir, "systemctl"), []byte(systemctlStub), 0o755); err != nil {
		t.Fatal(err)
	}
	oldBin := "#!/usr/bin/env bash\nif [[ \"$1\" == \"version\" ]]; then echo v1; exit 0; fi\nif [[ \"$1\" == \"probe\" ]]; then printf '%s' \"${GO_TRADER_PROBE_REGIME_SIDE_ADMISSION-unset}\" > \"" + oldRecord + "\"; exit 0; fi\nif [[ \"$1\" == \"agent-info\" ]]; then exit 0; fi\nexec sleep 60\n"
	if err := os.WriteFile(filepath.Join(deploy, "go-trader"), []byte(oldBin), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bash, filepath.Join(deploy, "scripts", "update.sh"), "--restart")
	cmd.Dir = deploy
	cmd.Env = append(append([]string{}, baseEnv...), "GO_TRADER_UPDATE_FAIL_AT=verify_health", "GO_TRADER_PROBE_REGIME_SIDE_ADMISSION=off")
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "recovery: complete") {
		t.Fatalf("expected a forced verify_health failure to end in recovery: complete, err=%v\n%s", err, out)
	}
	gotNew, err := os.ReadFile(newRecord)
	if err != nil {
		t.Fatalf("the update probe never ran: %v\n%s", err, out)
	}
	if string(gotNew) != "unset" {
		t.Fatalf("the update probe must enforce regime_side admission whatever the shell sets, got GO_TRADER_PROBE_REGIME_SIDE_ADMISSION=%q\n%s", gotNew, out)
	}
	gotOld, err := os.ReadFile(oldRecord)
	if err != nil {
		t.Fatalf("the recovery probe never ran: %v\n%s", err, out)
	}
	if string(gotOld) != "off" {
		t.Fatalf("the recovery probe must skip regime_side admission, got GO_TRADER_PROBE_REGIME_SIDE_ADMISSION=%q\n%s", gotOld, out)
	}
}
