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
func checkForUpdates(cfg *Config, discord *DiscordNotifier, lastNotifiedHash *string, mu *sync.RWMutex, state *AppState) bool {
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
	goChanged, _ := gitDiffHasFiles("HEAD", "origin/main", "scheduler/")

	msg := formatUpdateMessage(localHash, remoteHash, commitLog, newTag, goChanged)

	if discord != nil && len(cfg.Discord.Channels) > 0 {
		seen := make(map[string]bool)
		for _, ch := range cfg.Discord.Channels {
			if ch != "" && !seen[ch] {
				seen[ch] = true
				if err := discord.SendMessage(ch, msg); err != nil {
					fmt.Printf("[update] Discord notify failed: %v\n", err)
				}
			}
		}
	}

	// DM the owner offering auto-upgrade (non-blocking goroutine).
	if discord != nil && cfg.Discord.OwnerID != "" {
		ownerID := cfg.Discord.OwnerID
		go func() {
			dmMsg := fmt.Sprintf("**Update available**: `%s` → `%s`\nWould you like me to upgrade automatically? (yes/no)\n_This will: git pull, rebuild, and restart._",
				localHash[:8], remoteHash[:8])
			resp, err := discord.AskDM(ownerID, dmMsg, 30*time.Minute)
			if err != nil || strings.ToLower(strings.TrimSpace(resp)) != "yes" {
				_ = discord.SendDM(ownerID, "Upgrade skipped.")
				return
			}
			applyUpgrade(discord, ownerID, mu, state, cfg)
		}()
	}

	if lastNotifiedHash != nil {
		*lastNotifiedHash = remoteHash
	}

	return true
}

// applyUpgrade performs git pull, rebuild, and restart after user confirmation.
func applyUpgrade(discord *DiscordNotifier, ownerID string, mu *sync.RWMutex, state *AppState, cfg *Config) {
	_ = discord.SendDM(ownerID, "Starting upgrade...")

	// Step 1: git pull --ff-only
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "pull", "--ff-only").CombinedOutput()
	if err != nil {
		_ = discord.SendDM(ownerID, fmt.Sprintf("**git pull failed**:\n```\n%s\n```\n%v", strings.TrimSpace(string(out)), err))
		return
	}
	_ = discord.SendDM(ownerID, fmt.Sprintf("git pull OK:\n```\n%s\n```", strings.TrimSpace(string(out))))

	// Step 2: rebuild
	goBinary, err := exec.LookPath("go")
	if err != nil {
		goBinary = "/opt/homebrew/bin/go"
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel2()
	buildCmd := exec.CommandContext(ctx2, goBinary, "build", "-o", "../go-trader", ".")
	buildCmd.Dir = "scheduler"
	buildOut, buildErr := buildCmd.CombinedOutput()
	if buildErr != nil {
		_ = discord.SendDM(ownerID, fmt.Sprintf("**Build failed**:\n```\n%s\n```\n%v", strings.TrimSpace(string(buildOut)), buildErr))
		return
	}
	_ = discord.SendDM(ownerID, "Build OK. Saving state and restarting...")

	// Step 3: save state safely
	mu.Lock()
	if err := SavePlatformStates(state, cfg); err != nil {
		fmt.Printf("[upgrade] Failed to save state: %v\n", err)
	}
	mu.Unlock()

	// Step 4: close Discord connection
	discord.Close()

	// Step 5: restart the process
	if err := restartSelf(); err != nil {
		fmt.Printf("[upgrade] Restart failed: %v\n", err)
	}
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

// gitDiffHasFiles returns true if any files under the given path prefix changed between from and to.
func gitDiffHasFiles(from, to, prefix string) (bool, error) {
	out, err := exec.Command("git", "diff", "--name-only", from, to, "--", prefix).Output()
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(out)) != "", nil
}

// formatUpdateMessage builds the Discord notification message for an available update.
func formatUpdateMessage(localHash, remoteHash, commitLog, newTag string, goChanged bool) string {
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
	sb.WriteString("cd /path/to/go-trader && git pull --ff-only\n")
	if goChanged {
		sb.WriteString("cd scheduler && go build -o ../go-trader . && cd ..\n")
	}
	sb.WriteString("systemctl restart go-trader\n```")

	return sb.String()
}
