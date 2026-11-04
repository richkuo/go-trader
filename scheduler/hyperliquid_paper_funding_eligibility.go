package main

import (
	"sort"
	"strings"
)

func paperFundingEligible(sc StrategyConfig) bool {
	if sc.Platform != "hyperliquid" {
		return false
	}
	if sc.Type != "perps" && sc.Type != "manual" {
		return false
	}
	return !partitionFor(sc).IsLive()
}

func paperFundingPrimaryCoin(sc StrategyConfig) string {
	switch sc.Type {
	case "perps":
		if len(sc.Args) < 2 {
			return ""
		}
		return strings.TrimSpace(sc.Args[1])
	case "manual":
		return strings.TrimSpace(sc.Symbol)
	}
	return ""
}

func paperFundingCoins(sc StrategyConfig) []string {
	if !paperFundingEligible(sc) {
		return nil
	}
	set := make(map[string]bool, 2)
	if coin := paperFundingPrimaryCoin(sc); coin != "" {
		set[coin] = true
	}
	if coin := hedgeCoin(sc); coin != "" {
		set[coin] = true
	}
	out := make([]string, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

func paperFundingAccountingCoins(cfgs []StrategyConfig) []string {
	set := make(map[string]bool)
	for _, sc := range cfgs {
		for _, coin := range paperFundingCoins(sc) {
			set[coin] = true
		}
	}
	out := make([]string, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}
