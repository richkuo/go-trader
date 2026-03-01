package main

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// checkForUpdates uses git fetch to check for upstream changes. If new commits are found
// (and the remote hash differs from lastNotifiedHash), it sends a Discord notification with
// release notes asking the user to update, then sets *lastNotifiedHash to the new remote hash.
// Best-effort: errors are logged but never block startup or the main loop.
// Returns true if updates are available.
func checkForUpdates(cfg *Config, discord *DiscordNotifier, lastNotifiedHash *string) bool {
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

	if lastNotifiedHash != nil {
		*lastNotifiedHash = remoteHash
	}

	return true
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
