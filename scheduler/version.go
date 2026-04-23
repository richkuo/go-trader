package main

import (
	"os/exec"
	"strings"
)

// Version is set at build time via -ldflags "-X main.Version=vX.Y.Z".
// Falls back to "dev" for local builds.
var Version = "dev"

// resolveBuildVersion returns a version string for stamping a rebuilt binary
// during in-app upgrade. Uses `git describe --tags --always --dirty` so the
// auto-upgrade path preserves the version label it was meant to surface
// (otherwise every /upgrade reverts Version to "dev"). Falls back to "dev"
// when git is unavailable or the repo has no reachable commits.
func resolveBuildVersion() string {
	out, err := exec.Command("git", "describe", "--tags", "--always", "--dirty").Output()
	if err != nil {
		return "dev"
	}
	v := strings.TrimSpace(string(out))
	if v == "" {
		return "dev"
	}
	return v
}
