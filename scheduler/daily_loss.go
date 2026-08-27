package main


import (
	"fmt"
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

var dailyLossLastAlertDate string

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

func dailyLossStatusNote(pr *PortfolioRiskConfig, states map[string]*StrategyState, strategies []StrategyConfig, now time.Time) string {
	if !dailyLossLimitConfigured(pr) {
		return ""
	}
	st := evaluateDailyLossLimit(pr, states, strategies, now)
	var note string
	switch {
	case st.Tripped:
		note = fmt.Sprintf("\n🛑 daily loss limit TRIPPED: loss $%.2f >= $%.2f — entries held until UTC rollover", st.LossUSD, st.ThresholdUSD)
	case st.ThresholdUSD > 0:
		note = fmt.Sprintf("\n🟢 daily loss limit armed: today $%.2f / threshold $%.2f", st.DailyPnL, st.ThresholdUSD)
	}
	if st.PctBasisMiss {
		note += "\n" + dailyLossPctBasisMissWarning
	}
	return note
}

const dailyLossPctBasisMissWarning = "⚠️ daily loss limit: daily_max_loss_pct is configured but no allocated strategy has initial_capital > 0 — the pct arm CANNOT evaluate and enforces nothing (keep an allocated baseline or use daily_max_loss_usd; pool members cannot set initial_capital)"

var dailyLossPctBasisMissAlertDate string

func formatDailyLossPctBasisMissDM(st DailyLossLimitStatus, now time.Time) string {
	usdNote := "No other arm is configured — the daily loss limit is fully inert."
	if st.ThresholdUSD > 0 {
		usdNote = fmt.Sprintf("The USD arm still enforces at $%.2f.", st.ThresholdUSD)
	}
	return fmt.Sprintf(
		"%s\n%s\nToday's aggregate realized PnL: $%.2f. This DM repeats once per UTC day while the gap persists. (%s UTC)",
		dailyLossPctBasisMissWarning, usdNote, st.DailyPnL, now.UTC().Format("2006-01-02 15:04"))
}
