//go:build pyintegration

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestUpdateCanonicalDBPathMatchesScheduler(t *testing.T) {
	bash := updateShellBash(t)
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	helpers := filepath.Join(filepath.Dir(thisFile), "..", "scripts", "update_helpers.sh")
	tmp := t.TempDir()
	real := filepath.Join(tmp, "real", "dir")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(tmp, "link")
	if err := os.Symlink(filepath.Join(tmp, "real"), link); err != nil {
		t.Fatal(err)
	}
	absent := filepath.Join(link, "dir", "state.db")
	present := filepath.Join(link, "dir", "present.db")
	if err := os.WriteFile(filepath.Join(real, "present.db"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	dangling := filepath.Join(real, "dangling.db")
	if err := os.Symlink(filepath.Join(tmp, "missing-target"), dangling); err != nil {
		t.Fatal(err)
	}
	cases := []struct{ name, path string }{
		{"absent file behind a symlinked parent", absent},
		{"present file behind a symlinked parent", present},
		{"dangling symlink", dangling},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(bash, "-c", `source "$1" && update_canonical_db_path "$2"`, "bash", helpers, tc.path)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("update_canonical_db_path: %v\n%s", err, out)
			}
			got := strings.TrimSpace(string(out))
			want := canonicalDBPath(tc.path)
			if got != want {
				t.Fatalf("shell helper=%q scheduler canonicalDBPath=%q; the handoff would lock a different file than the scheduler", got, want)
			}
		})
	}
}
