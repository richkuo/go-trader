package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"trading-scheduler/internal/egresspolicy"
	"trading-scheduler/internal/notifybroker"
)

var egressNetworkSubcommands = map[string]bool{
	"manual-open": true, "manual-add": true, "manual-close": true, "force-close": true,
	"manual-cancel": true, "manual-update-sl": true, "manual-cancel-sl": true, "backfill": true,
	"record-observations": true, "robinhood-login": true,
}

var egressLocalSubcommands = map[string]bool{
	"init": true, "export": true, "inspect": true, "storage-inspect": true, "agent-info": true,
	"diagnostics": true, "feed-fetch": true, "basket-replay": true,
	"manual-clear-limit-row": true,
}

func subcommandConfigPath(args []string) string {
	path := "scheduler/config.json"
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--config" || a == "-config":
			if i+1 < len(args) {
				path = args[i+1]
				i++
			}
		case strings.HasPrefix(a, "--config="):
			path = strings.TrimPrefix(a, "--config=")
		case strings.HasPrefix(a, "-config="):
			path = strings.TrimPrefix(a, "-config=")
		case a == "--":
			return path
		}
	}
	return path
}

func gateSubcommandEgress(sub string, args []string) (int, bool) {
	network := egressNetworkSubcommands[sub]
	if !network && !egressLocalSubcommands[sub] && sub != "probe" {
		return 0, false
	}
	if egressLocalSubcommands[sub] && os.Getenv(egresspolicy.EnvProxy) == "" {
		return 0, false
	}
	cfg, err := peekEgressConfig(subcommandConfigPath(args))
	if err != nil {
		fmt.Fprintf(os.Stderr, "[egress] CRITICAL: refusing %s: %v\n", sub, err)
		return 1, true
	}
	if err := establishProcessEgress(context.Background(), cfg, network); err != nil {
		fmt.Fprintf(os.Stderr, "[egress] CRITICAL: refusing %s: %v\n", sub, err)
		return 1, true
	}
	if sub == "probe" && processEgress.isActive() {
		if err := probeEgressEnforcement(); err != nil {
			fmt.Fprintf(os.Stderr, "probe: %v\n", err)
			return ExitProbeFailure, true
		}
	}
	return 0, false
}

func probeEgressEnforcement() error {
	r := processEgress
	r.mu.Lock()
	deployment, id := r.deployment, r.endpoint.Identity()
	r.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), egressAttestTimeout)
	defer cancel()
	st, err := egresspolicy.QueryStatus(ctx, deployment)
	if err != nil {
		return fmt.Errorf("proxy mode is on but enforcement is unavailable: %s", r.redact(err.Error()))
	}
	if st.Proxy != id {
		return fmt.Errorf("the egress controller enforces proxy %s but %s names %s", st.Proxy, egresspolicy.EnvProxy, id)
	}
	if !st.Ready {
		return fmt.Errorf("the egress controller is not ready (rules: %q, proxy resolution: %q)", st.RulesError, st.LastResolveError)
	}
	fmt.Printf("probe: egress enforcement ready (deployment %s, generation %d, proxy %s)\n", deployment, st.Generation, st.Proxy)
	return nil
}

func startEgress(cfg *Config, configPath string, feedRole bool) int {
	if err := establishProcessEgress(context.Background(), cfg.Egress, true); err != nil {
		fmt.Fprintf(os.Stderr, "[egress] CRITICAL: refusing to start: %v\n", err)
		return 1
	}
	if processEgress.directNotifications() {
		if held := workerNotificationSecrets(cfg); len(held) > 0 {
			fmt.Fprintf(os.Stderr, "[egress] CRITICAL: refusing to start: egress.notifications_direct is true but this process holds %s; move them to the notification broker's environment file only (SKILL.md Egress Proxy Mode)\n", strings.Join(held, ", "))
			return 1
		}
	}
	fmt.Println(formatEgressStartupLine())
	if !processEgress.isActive() {
		return 0
	}
	ctx := context.Background()
	err := egressStartupCheck(ctx, egressStartupTargets(cfg, feedRole))
	if err == nil && !feedRole {
		err = egressPythonPolicyCheck(ctx, egressPythonFamilies(cfg))
	}
	dmPath := egressRefusalDMPath(configPath)
	if err != nil {
		msg := processEgress.redact(err.Error())
		reason := msg
		var se *egressStartupError
		if errors.As(err, &se) {
			reason = se.reason
		}
		fmt.Fprintf(os.Stderr, "[egress] CRITICAL: refusing to start: %s (no direct connection was attempted; the service retries)\n", msg)
		if processEgress.directNotifications() {
			interval, perr := ParseAlertThrottleInterval(cfg.AlertThrottleInterval)
			if perr != nil {
				interval = DefaultAlertThrottleInterval
			}
			now := time.Now().UTC()
			notifyEgressRefusal(cfg, dmPath, reason, msg, interval, now)
		}
		return 1
	}
	_ = os.Remove(dmPath)
	return 0
}

func notifyEgressRefusal(cfg *Config, dmPath, reason, msg string, interval time.Duration, now time.Time) bool {
	if !egressRefusalDMDue(dmPath, reason, interval, now) {
		fmt.Fprintf(os.Stderr, "[egress] the owner already has this refusal message; the next one goes out after %s or when the reason changes\n", interval)
		return false
	}
	notifier, cleanup := buildNotifierFromConfig(cfg)
	delivered := sendStartupRefusalDM(notifier, "Egress proxy", msg)
	cleanup()
	if !delivered {
		fmt.Fprintln(os.Stderr, "[egress] no owner backend accepted the refusal message; the next restart sends it again")
		return false
	}
	recordEgressRefusalDM(dmPath, reason, now)
	return true
}

const egressRefusalDMFile = ".egress-startup-refusal.json"

type egressRefusalDMRecord struct {
	Reason string    `json:"reason"`
	SentAt time.Time `json:"sent_at"`
}

func egressRefusalDMPath(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), egressRefusalDMFile)
}

func egressRefusalDMDue(path, reason string, interval time.Duration, now time.Time) bool {
	raw, err := os.ReadFile(path)
	if err != nil {
		return true
	}
	var rec egressRefusalDMRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return true
	}
	return rec.Reason != reason || now.Before(rec.SentAt) || now.Sub(rec.SentAt) >= interval
}

func recordEgressRefusalDM(path, reason string, now time.Time) {
	raw, err := json.Marshal(egressRefusalDMRecord{Reason: reason, SentAt: now})
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "[egress] could not record the refusal message time (%v); each restart sends it again\n", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		fmt.Fprintf(os.Stderr, "[egress] could not record the refusal message time (%v); each restart sends it again\n", err)
	}
}

func workerNotificationSecrets(cfg *Config) []string {
	var held []string
	if strings.TrimSpace(cfg.Discord.Token) != "" {
		held = append(held, "a Discord bot token")
	}
	if strings.TrimSpace(cfg.Telegram.BotToken) != "" {
		held = append(held, "a Telegram bot token")
	}
	if cfg.Discord.reportToken() != "" {
		held = append(held, "a GitHub token")
	}
	return held
}

func requestEgressRestart() error {
	processEgress.mu.Lock()
	deployment := processEgress.deployment
	processEgress.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), egressAttestTimeout)
	defer cancel()
	if err := egresspolicy.RequestRestart(ctx, deployment); err != nil {
		return fmt.Errorf("egress controller restart request: %w", err)
	}
	fmt.Println("[restart] the egress controller is restarting the service unit")
	return nil
}

func runEgressAnchor(args []string) int {
	fs := flag.NewFlagSet("egress-anchor", flag.ContinueOnError)
	check := fs.Bool("check", false, "exit 0 only when the egress controller has enforced this namespace")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	deployment := strings.TrimSpace(os.Getenv(egresspolicy.EnvDeployment))
	if *check {
		ctx, cancel := context.WithTimeout(context.Background(), egressAttestTimeout)
		defer cancel()
		st, err := egresspolicy.QueryStatus(ctx, deployment)
		if err != nil {
			fmt.Fprintf(os.Stderr, "egress-anchor: %v\n", err)
			return 1
		}
		own, err := egresspolicy.CurrentNetnsInode()
		if err != nil {
			fmt.Fprintf(os.Stderr, "egress-anchor: %v\n", err)
			return 1
		}
		if !st.Ready || st.NetnsInode != own {
			fmt.Fprintf(os.Stderr, "egress-anchor: not enforced yet (ready=%t, controller namespace %d, this namespace %d)\n", st.Ready, st.NetnsInode, own)
			return 1
		}
		fmt.Printf("egress-anchor: enforced (deployment %s, generation %d)\n", deployment, st.Generation)
		return 0
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	fmt.Printf("egress-anchor: holding the network namespace for deployment %s\n", deployment)
	<-ctx.Done()
	return 0
}

func runNotifyBroker(args []string) int {
	fs := flag.NewFlagSet("notify-broker", flag.ContinueOnError)
	deployment := fs.String("deployment", "", "egress deployment name")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if err := notifybroker.Run(ctx, *deployment); err != nil {
		fmt.Fprintf(os.Stderr, "[notify-broker] %v\n", err)
		return 1
	}
	return 0
}
