package main

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
)

const (
	replayExitOK      = 0
	replayExitError   = 1
	replayExitUsage   = 2
	replayExitRefused = 3
	replayExitFault   = 75
)

const (
	replayModeNew    = "new"
	replayModeResume = "resume"
)

const (
	replayStatusComplete   = "complete"
	replayStatusIncomplete = "incomplete"
	replayStatusStopped    = "stopped"
	replayStatusRefused    = "refused"
	replayStatusError      = "error"
)

var replayRunIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

var replayScrubbedEnvWords = []string{"SECRET", "TOKEN", "PRIVATE", "PASSWORD", "API_KEY", "WALLET", "MNEMONIC", "WEBHOOK"}

var replayDeployedPrefixes = []string{"/var/lib/go-trader", "/opt/go-trader", "/etc/go-trader", "/etc/systemd"}

type replayRunSpec struct {
	RunID    string            `json:"run_id"`
	StateDir string            `json:"state_dir"`
	Source   map[string]string `json:"source"`
}

type replayRefusal struct {
	Reason string   `json:"reason"`
	Detail []string `json:"detail"`
}

func (r *replayRefusal) Error() string {
	return r.Reason + ": " + strings.Join(r.Detail, "; ")
}

func refuseReplay(reason string, detail ...string) *replayRefusal {
	return &replayRefusal{Reason: reason, Detail: detail}
}

func replaySourceFingerprint(source map[string]string) string {
	blob, _ := json.Marshal(struct {
		Source       map[string]string `json:"source"`
		Version      string            `json:"adapter_version"`
		SourceCommit string            `json:"adapter_source_commit"`
	}{source, Version, SourceCommit})
	return sha256Hex(blob)
}

func replayPathAllowed(dir string) error {
	for _, p := range replayDeployedPrefixes {
		if dir == p || strings.HasPrefix(dir, p+string(os.PathSeparator)) {
			return fmt.Errorf("run.state_dir %q is inside the deployed path %s; a replay never opens deployed state", dir, p)
		}
	}
	return nil
}
