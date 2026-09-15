package main

import (
	"fmt"
	"strings"
	"time"
)

type DailyLossLimitStatus struct {
	Configured   bool
	Tripped      bool
	DailyPnL     float64
	LossUSD      float64
	CapitalBasis float64
	ThresholdUSD float64
	PctBasisMiss bool
}

func dailyLossLimitConfigured(pr *PortfolioRiskConfig) bool {
	return pr != nil && (pr.DailyMaxLossUSD > 0 || pr.DailyMaxLossPct > 0)
}

func evaluateDailyLossLimit(pr *PortfolioRiskConfig, states map[string]*StrategyState, strategies []StrategyConfig, now time.Time) DailyLossLimitStatus {
	st := DailyLossLimitStatus{Configured: dailyLossLimitConfigured(pr)}
	pooledIDs := make(map[string]bool)
	for _, sc := range strategies {
		if usesSharedWalletPoolBudget(sc) {
			pooledIDs[sc.ID] = true
		}
	}
	today := now.UTC().Format("2006-01-02")
	for id, ss := range states {
		if ss == nil {
			continue
		}
		if ss.RiskState.DailyPnLDate == today {
			st.DailyPnL += ss.RiskState.DailyPnL
		}
		if !pooledIDs[id] && ss.InitialCapital > 0 {
			st.CapitalBasis += ss.InitialCapital
		}
	}
	if st.DailyPnL < 0 {
		st.LossUSD = -st.DailyPnL
	}
	if !st.Configured {
		return st
	}
	if pr.DailyMaxLossUSD > 0 {
		st.ThresholdUSD = pr.DailyMaxLossUSD
	}
	if pr.DailyMaxLossPct > 0 {
		if st.CapitalBasis > 0 {
			pctUSD := st.CapitalBasis * pr.DailyMaxLossPct / 100
			if st.ThresholdUSD == 0 || pctUSD < st.ThresholdUSD {
				st.ThresholdUSD = pctUSD
			}
		} else {
			st.PctBasisMiss = true
		}
	}
	st.Tripped = st.ThresholdUSD > 0 && st.LossUSD >= st.ThresholdUSD
	return st
}

func dailyLossHoldDetail(st DailyLossLimitStatus) string {
	return fmt.Sprintf("daily loss limit tripped: today's realized loss $%.2f >= threshold $%.2f (pre-fee; basis=$%.2f initial capital)",
		st.LossUSD, st.ThresholdUSD, st.CapitalBasis)
}

func formatDailyLossTripDM(st DailyLossLimitStatus, now time.Time) string {
	return fmt.Sprintf(
		"🛑 **Daily loss limit tripped** (%s UTC)\n"+
			"Today's aggregate realized PnL: $%.2f (pre-fee, across all strategies)\n"+
			"Threshold: $%.2f (capital basis $%.2f)\n"+
			"All fresh opens, scale-in adds, and flips are held for the rest of the UTC day — including manual-open/manual-add. "+
			"Open positions keep being managed (closes, trailing SL, ratchet, protection sync) and nothing is force-closed. "+
			"Entries resume automatically at the next UTC rollover.",
		now.UTC().Format("2006-01-02 15:04"), st.DailyPnL, st.ThresholdUSD, st.CapitalBasis)
}

func dailyLossAlertDue(tripped bool, lastAlertDate, today string) bool {
	return tripped && lastAlertDate != today
}

var dailyLossLastAlertDate = map[RiskPartition]string{}

func dailyLossStartupSummaryLine(pr *PortfolioRiskConfig) string {
	if !dailyLossLimitConfigured(pr) {
		return ""
	}
	parts := ""
	if pr.DailyMaxLossUSD > 0 {
		parts = fmt.Sprintf("usd=$%.2f", pr.DailyMaxLossUSD)
	}
	if pr.DailyMaxLossPct > 0 {
		if parts != "" {
			parts += " "
		}
		parts += fmt.Sprintf("pct=%.2f%% of initial capital", pr.DailyMaxLossPct)
	}
	return fmt.Sprintf("[config] portfolio: daily_max_loss %s (pre-fee realized; blocks new entries for the rest of the UTC day when tripped)", parts)
}

// dailyLossPaperStartupSummaryLines prints one line per paper partition whose
// effective limits differ from the root, so a folded source's own limits are
// visible at boot instead of hidden behind the default paper line.
func dailyLossPaperStartupSummaryLines(cfg *Config) []string {
	if cfg == nil || cfg.PortfolioRisk == nil {
		return nil
	}
	var out []string
	for _, p := range activePartitions(cfg.Strategies) {
		if p.Scope != ScopePaper {
			continue
		}
		pr := partitionRiskConfig(cfg, p)
		if pr == nil {
			continue
		}
		if pr.DailyMaxLossUSD == cfg.PortfolioRisk.DailyMaxLossUSD && pr.DailyMaxLossPct == cfg.PortfolioRisk.DailyMaxLossPct {
			continue
		}
		line := dailyLossStartupSummaryLine(pr)
		if line == "" {
			continue
		}
		out = append(out, strings.Replace(line, "[config] portfolio:",
			fmt.Sprintf("[config] portfolio (%s scope):", partitionLabel(p)), 1))
	}
	return out
}

func dailyLossStatusNote(cfg *Config, states map[string]*StrategyState, now time.Time) string {
	if cfg == nil {
		return ""
	}
	parts := activePartitions(cfg.Strategies)
	var note string
	for _, part := range parts {
		pr := partitionRiskConfig(cfg, part)
		if !dailyLossLimitConfigured(pr) {
			continue
		}
		st := evaluateDailyLossLimit(pr, filterStatesByPartition(states, cfg.Strategies, part), strategiesInPartition(cfg.Strategies, part), now)
		prefix := ""
		if len(parts) > 1 {
			prefix = "[" + partitionLabel(part) + "] "
		}
		switch {
		case st.Tripped:
			note += fmt.Sprintf("\n🛑 %sdaily loss limit TRIPPED: loss $%.2f >= $%.2f — entries held until UTC rollover", prefix, st.LossUSD, st.ThresholdUSD)
		case st.ThresholdUSD > 0:
			note += fmt.Sprintf("\n🟢 %sdaily loss limit armed: today $%.2f / threshold $%.2f", prefix, st.DailyPnL, st.ThresholdUSD)
		}
		if st.PctBasisMiss {
			note += "\n" + dailyLossPctBasisMissWarning
		}
	}
	return note
}

const dailyLossPctBasisMissWarning = "⚠️ daily loss limit: daily_max_loss_pct is configured but no allocated strategy has initial_capital > 0 — the pct arm CANNOT evaluate and enforces nothing (keep an allocated baseline or use daily_max_loss_usd; pool members cannot set initial_capital)"

var dailyLossPctBasisMissAlertDate = map[RiskPartition]string{}

func formatDailyLossPctBasisMissDM(st DailyLossLimitStatus, now time.Time) string {
	usdNote := "No other arm is configured — the daily loss limit is fully inert."
	if st.ThresholdUSD > 0 {
		usdNote = fmt.Sprintf("The USD arm still enforces at $%.2f.", st.ThresholdUSD)
	}
	return fmt.Sprintf(
		"%s\n%s\nToday's aggregate realized PnL: $%.2f. This DM repeats once per UTC day while the gap persists. (%s UTC)",
		dailyLossPctBasisMissWarning, usdNote, st.DailyPnL, now.UTC().Format("2006-01-02 15:04"))
}
