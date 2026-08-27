package main

import (
	"encoding/json"
	"fmt"
	"os"
)

type balanceResult struct {
	Balance float64 `json:"balance"`
	Error   string  `json:"error,omitempty"`
}

func FetchPlatformBalance(platform string) (float64, error) {
	switch platform {
	case "hyperliquid":
		addr := os.Getenv("HYPERLIQUID_ACCOUNT_ADDRESS")
		if addr == "" {
			return 0, fmt.Errorf("HYPERLIQUID_ACCOUNT_ADDRESS env var not set")
		}
		return fetchHyperliquidBalance(addr)
	default:
		return fetchPythonBalance(platform)
	}
}

func fetchPythonBalance(platform string) (float64, error) {
	args := []string{fmt.Sprintf("--platform=%s", platform)}
	stdout, stderr, err := RunPythonScript("shared_scripts/check_balance.py", args)
	if err != nil {
		return 0, fmt.Errorf("check_balance.py %s: %w (stderr: %s)", platform, err, string(stderr))
	}

	var result balanceResult
	if err := json.Unmarshal(stdout, &result); err != nil {
		return 0, fmt.Errorf("parse balance output for %s: %w (stdout: %s)", platform, err, string(stdout))
	}
	if result.Error != "" {
		return 0, fmt.Errorf("balance check %s: %s", platform, result.Error)
	}
	return result.Balance, nil
}

func resolveCapitalPct(strategies []StrategyConfig) {
	needsBalance := make(map[string]bool)
	for _, sc := range strategies {
		if sc.CapitalPct > 0 {
			needsBalance[sc.Platform] = true
		}
	}
	if len(needsBalance) == 0 {
		return
	}

	balances := make(map[string]float64)
	for platform := range needsBalance {
		balance, err := FetchPlatformBalance(platform)
		if err != nil {
			fmt.Printf("[WARN] capital_pct: failed to fetch %s balance: %v\n", platform, err)
			continue
		}
		if balance <= 0 {
			fmt.Printf("[WARN] capital_pct: %s balance is $%.2f — wallet may be unfunded\n", platform, balance)
			continue
		}
		balances[platform] = balance
	}

	for i := range strategies {
		sc := &strategies[i]
		if sc.CapitalPct <= 0 {
			continue
		}
		balance, ok := balances[sc.Platform]
		if !ok {
			if sc.Capital > 0 {
				fmt.Printf("[WARN] capital_pct: no balance for %s, using fallback capital=$%.2f\n", sc.ID, sc.Capital)
			} else {
				fmt.Printf("[WARN] capital_pct: no balance for %s and no fallback capital — skipping\n", sc.ID)
			}
			continue
		}
		newCapital := balance * sc.CapitalPct
		fmt.Printf("[INFO] capital_pct: %s → $%.2f (%.0f%% of $%.2f %s balance)\n",
			sc.ID, newCapital, sc.CapitalPct*100, balance, sc.Platform)
		sc.Capital = newCapital
	}
}
