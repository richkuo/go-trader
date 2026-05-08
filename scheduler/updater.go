package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// checkForUpdates uses git fetch to check for upstream changes. If new commits are found
// (and the remote hash differs from lastNotifiedHash), it sends a Discord channel notification
// and, if OwnerID is configured, a DM offering to auto-upgrade.
// Best-effort: errors are logged but never block startup or the main loop.
// Returns true if updates are available.
func checkForUpdates(cfg *Config, notifier *MultiNotifier, lastNotifiedHash *string, mu *sync.RWMutex, state *AppState, stateDB *StateDB) bool {
	// Must be a git repo.
	if err := gitCheck(); err != nil {
		fmt.Printf("[update] Not a git repo or git unavailable: %v\n", err)
		return false
	}

	localHash, err := gitRevParse("HEAD")
	if err != nil {
		fmt.Printf("[update] Failed to get local HEAD: %v\n", err)
		return false
	}

	if err := runGitFetch(); err != nil {
		fmt.Printf("[update] git fetch failed: %v\n", err)
		return false
	}

	remoteHash, err := gitRevParse("origin/main")
	if err != nil {
		fmt.Printf("[update] Failed to get origin/main: %v\n", err)
		return false
	}

	if localHash == remoteHash {
		fmt.Println("[update] Already up to date")
		return false
	}

	// Skip notification if we already notified about this exact remote hash.
	if lastNotifiedHash != nil && *lastNotifiedHash == remoteHash {
		fmt.Printf("[update] Update available (%s), already notified\n", remoteHash[:8])
		return true
	}

	fmt.Printf("[update] Update available: %s → %s\n", localHash[:8], remoteHash[:8])

	commitLog, _ := gitLog("HEAD", "origin/main")
	newTag, _ := gitDescribeExact("origin/main")

	msg := formatUpdateMessage(localHash, remoteHash, commitLog, newTag)

	if notifier != nil && notifier.HasBackends() {
		notifier.SendToAllChannels(msg)
	}

	// DM the owner offering auto-upgrade (non-blocking goroutine).
	if notifier != nil && notifier.HasOwner() {
		go func() {
			dmMsg := fmt.Sprintf("**Update available**: `%s` → `%s`\nWould you like me to upgrade automatically? (yes/no)\n_This will: git pull, rebuild, and restart._",
				localHash[:8], remoteHash[:8])
			resp, err := notifier.AskOwnerDM(dmMsg, 30*time.Minute)
			if err != nil || strings.ToLower(strings.TrimSpace(resp)) != "yes" {
				notifier.SendOwnerDM("Upgrade skipped.")
				return
			}
			applyUpgrade(notifier, mu, state, cfg, stateDB)
		}()
	}

	if lastNotifiedHash != nil {
		*lastNotifiedHash = remoteHash
	}

	return true
}

// applyUpgrade runs scripts/update.sh (atomic git pull + uv sync + go build) and
// then saves state and restarts. The script is invoked without --restart so this
// function retains control of the state-save + restartSelf() ordering.
func applyUpgrade(notifier *MultiNotifier, mu *sync.RWMutex, state *AppState, cfg *Config, stateDB *StateDB) {
	notifier.SendOwnerDM("Starting upgrade...")

	// 5min covers a cold uv sync + go build on a slow VPS; killing mid-build
	// leaves the worktree at the new SHA but no rebuilt binary, which the
	// startup probe then catches on next start.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "scripts/update.sh")
	out, err := cmd.CombinedOutput()
	if err != nil {
		notifier.SendOwnerDM(fmt.Sprintf("**update.sh failed**:\n```\n%s\n```\n%v", tailForDM(string(out), 1500), err))
		return
	}
	notifier.SendOwnerDM(fmt.Sprintf("update.sh OK:\n```\n%s\n```\nSaving state and restarting...", tailForDM(string(out), 1500)))

	// Step 3: save state safely
	mu.Lock()
	if err := SaveStateWithDB(state, cfg, stateDB); err != nil {
		fmt.Printf("[upgrade] Failed to save state: %v\n", err)
	}
	mu.Unlock()

	// Step 4: close notifier connections
	notifier.Close()

	// Step 5: restart the process
	if err := restartSelf(); err != nil {
		fmt.Printf("[upgrade] Restart failed: %v\n", err)
	}
}

// tailForDM trims s to the last max bytes (with a leading "...truncated..."
// marker) so update output stays inside Discord's 2000-char DM limit. uv sync
// can be chatty on first install; without trimming the DM gets rejected.
func tailForDM(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return "...truncated...\n" + s[len(s)-max:]
}

// restartSelf attempts to restart the process via systemctl, then falls back to syscall.Exec.
func restartSelf() error {
	// Try systemctl first (Linux/systemd deployments).
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "systemctl", "restart", "go-trader").Run(); err == nil {
		// systemd will SIGTERM this process; give it time to do so.
		time.Sleep(30 * time.Second)
	}

	// Fallback: re-exec our own binary in-place.
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("get executable: %w", err)
	}
	return syscall.Exec(exe, os.Args, os.Environ())
}

// gitCheck verifies that git is available and the working directory is a git repo.
func gitCheck() error {
	_, err := exec.Command("git", "rev-parse", "--git-dir").Output()
	return err
}

// gitRevParse returns the full commit hash for the given ref.
func gitRevParse(ref string) (string, error) {
	out, err := exec.Command("git", "rev-parse", ref).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// runGitFetch fetches from origin with a 15-second timeout.
func runGitFetch() error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, err := exec.CommandContext(ctx, "git", "fetch", "origin").CombinedOutput()
	return err
}

// gitLog returns a short one-line log of commits in the range from..to.
func gitLog(from, to string) (string, error) {
	out, err := exec.Command("git", "log", "--oneline", from+".."+to).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// gitDescribeExact returns the tag name if the given ref is exactly tagged, empty string otherwise.
func gitDescribeExact(ref string) (string, error) {
	out, err := exec.Command("git", "describe", "--tags", "--exact-match", ref).Output()
	if err != nil {
		return "", nil // not tagged — normal, not an error
	}
	return strings.TrimSpace(string(out)), nil
}

// formatUpdateMessage builds the Discord notification message for an available update.
func formatUpdateMessage(localHash, remoteHash, commitLog, newTag string) string {
	var sb strings.Builder

	if newTag != "" {
		sb.WriteString(fmt.Sprintf("**New Release: %s**\n", newTag))
	} else {
		sb.WriteString("**go-trader Update Available**\n")
	}

	sb.WriteString(fmt.Sprintf("`%s` → `%s`\n", localHash[:8], remoteHash[:8]))

	if commitLog != "" {
		lines := strings.Split(commitLog, "\n")
		if len(lines) > 10 {
			extra := len(lines) - 10
			lines = lines[:10]
			lines = append(lines, fmt.Sprintf("... and %d more", extra))
		}
		sb.WriteString("```\n")
		sb.WriteString(strings.Join(lines, "\n"))
		sb.WriteString("\n```\n")
	}

	sb.WriteString("To update:\n```\n")
	sb.WriteString("cd /path/to/go-trader && bash scripts/update.sh --restart\n")
	sb.WriteString("```")

	return sb.String()
}
