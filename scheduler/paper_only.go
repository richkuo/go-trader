package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const defaultCgroupCPUWeight = 100

func paperOnlyLiveStrategyErrors(cfg *Config) []string {
	if cfg == nil || !cfg.PaperOnly {
		return nil
	}
	var errs []string
	for i, sc := range cfg.Strategies {
		if portfolioScopeFor(effectiveIdentityStrategy(sc)) != ScopeLive {
			continue
		}
		label := sc.ID
		if label == "" {
			label = fmt.Sprintf("%d", i)
		}
		errs = append(errs, fmt.Sprintf("strategy[%s]: paper_only is true, so a live strategy (--mode=live) is refused; remove the strategy, or remove paper_only and restart", label))
	}
	return errs
}

func liveProcessCPUWeightWarning(cfg *Config) string {
	if cfg == nil || cfg.PaperOnly || !HasLiveStrategy(cfg.Strategies) {
		return ""
	}
	cgroupPath, weight, ok := ownCgroupCPUWeight()
	if !ok || weight >= defaultCgroupCPUWeight {
		return ""
	}
	return fmt.Sprintf("[WARN] cpu: this process runs live strategies, but its cgroup %s has cpu.weight=%d (below the default %d), so live cycles get a reduced CPU share. A paper CPU weight drop-in (40-go-trader-cpu.conf) may remain from a paper_only config: run bash scripts/update.sh --restart for this instance, or remove the drop-in and run systemctl daemon-reload, then restart", cgroupPath, weight, defaultCgroupCPUWeight)
}

func ownCgroupCPUWeight() (string, int, bool) {
	raw, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", 0, false
	}
	for _, line := range strings.Split(string(raw), "\n") {
		rel, found := strings.CutPrefix(line, "0::")
		if !found || !strings.HasPrefix(rel, "/") {
			continue
		}
		data, err := os.ReadFile(filepath.Join("/sys/fs/cgroup", filepath.Clean(rel), "cpu.weight"))
		if err != nil {
			return "", 0, false
		}
		weight, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil || weight <= 0 {
			return "", 0, false
		}
		return rel, weight, true
	}
	return "", 0, false
}
