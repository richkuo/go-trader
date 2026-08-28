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

const defaultGoTraderSystemdUnit = "go-trader"

func checkForUpdates(cfg *Config, notifier *MultiNotifier, lastNotifiedHash *string, mu *sync.RWMutex, state *AppState, stateDB *StateDB) bool {
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

func applyUpgrade(notifier *MultiNotifier, mu *sync.RWMutex, state *AppState, cfg *Config, stateDB *StateDB) {
	notifier.SendOwnerDM("Starting upgrade...")

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "scripts/update.sh")
	out, err := cmd.CombinedOutput()
	if err != nil {
		notifier.SendOwnerDM(fmt.Sprintf("**update.sh failed**:\n```\n%s\n```\n%v", tailForDM(string(out), 1500), err))
		return
	}
	notifier.SendOwnerDM(fmt.Sprintf("update.sh OK:\n```\n%s\n```\nSaving state and restarting...", tailForDM(string(out), 1500)))

	mu.Lock()
	if err := SaveStateWithDB(state, cfg, stateDB); err != nil {
		fmt.Printf("[upgrade] Failed to save state: %v\n", err)
	}
	mu.Unlock()

	notifier.Close()

	if err := restartSelf(); err != nil {
		fmt.Printf("[upgrade] Restart failed: %v\n", err)
	}
}

func tailForDM(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return "...truncated...\n" + s[len(s)-max:]
}

func restartSelf() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "systemctl", "restart", updateSystemdUnitName()).Run(); err == nil {
		time.Sleep(30 * time.Second)
	}

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("get executable: %w", err)
	}
	return syscall.Exec(exe, os.Args, os.Environ())
}

func updateSystemdUnitName() string {
	unit := strings.TrimSpace(os.Getenv("GO_TRADER_SERVICE"))
	if unit == "" {
		return defaultGoTraderSystemdUnit
	}
	return unit
}

func gitCheck() error {
	_, err := exec.Command("git", "rev-parse", "--git-dir").Output()
	return err
}

func gitRevParse(ref string) (string, error) {
	out, err := exec.Command("git", "rev-parse", ref).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func runGitFetch() error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, err := exec.CommandContext(ctx, "git", "fetch", "origin").CombinedOutput()
	return err
}

func gitLog(from, to string) (string, error) {
	out, err := exec.Command("git", "log", "--oneline", from+".."+to).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func gitDescribeExact(ref string) (string, error) {
	out, err := exec.Command("git", "describe", "--tags", "--exact-match", ref).Output()
	if err != nil {
		return "", nil
	}
	return strings.TrimSpace(string(out)), nil
}

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
	sb.WriteString("# custom unit: GO_TRADER_SERVICE=go-trader-live.service bash scripts/update.sh --restart\n")
	sb.WriteString("```")

	return sb.String()
}
