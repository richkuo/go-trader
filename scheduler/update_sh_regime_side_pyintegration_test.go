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
