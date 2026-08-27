package main

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

type ExposureCapAssetStat struct {
	Direction string
	Pct       float64
	NetUSD    float64
}

type ExposureCapStatus struct {
	Configured       bool
	CapUSD           float64
	LongUSD          float64
	ShortUSD         float64
	LongBlocked      bool
	ShortBlocked     bool
	ConcentrationPct float64
	PortfolioValue   float64
	PVBasisMiss      bool
	OverConcentrated map[string]ExposureCapAssetStat
	SkippedPositions []string
}

func exposureCapConfigured(pr *PortfolioRiskConfig) bool {
	return pr != nil && (pr.MaxSameDirectionNotionalUSD > 0 || pr.MaxAssetConcentrationPct > 0)
}

func evaluateExposureCap(pr *PortfolioRiskConfig, states map[string]*StrategyState, cfgStrategies []StrategyConfig, prices map[string]float64, portfolioValue float64) ExposureCapStatus {
	st := ExposureCapStatus{Configured: exposureCapConfigured(pr)}
	if !st.Configured {
		return st
	}
	st.CapUSD = pr.MaxSameDirectionNotionalUSD
	st.ConcentrationPct = pr.MaxAssetConcentrationPct
	st.PortfolioValue = portfolioValue
	st.PVBasisMiss = st.ConcentrationPct > 0 && portfolioValue <= 0

	assets, skipped := computeAssetDeltas(states, cfgStrategies, prices)
	st.SkippedPositions = skipped

	names := make([]string, 0, len(assets))
	for a := range assets {
		names = append(names, a)
	}
	sort.Strings(names)
	for _, a := range names {
		net := assets[a].NetDeltaUSD
		if net > 0 {
			st.LongUSD += net
		} else {
			st.ShortUSD += -net
		}
		if st.ConcentrationPct > 0 && portfolioValue > 0 {
			pct := net / portfolioValue * 100
			dir := "long"
			if net < 0 {
				pct = -pct
				dir = "short"
			}
			if pct > st.ConcentrationPct {
				if st.OverConcentrated == nil {
					st.OverConcentrated = make(map[string]ExposureCapAssetStat)
				}
				st.OverConcentrated[a] = ExposureCapAssetStat{Direction: dir, Pct: pct, NetUSD: net}
			}
		}
	}
	st.LongBlocked = st.CapUSD > 0 && st.LongUSD > st.CapUSD
	st.ShortBlocked = st.CapUSD > 0 && st.ShortUSD > st.CapUSD
	return st
}

func manualExposureCapStatus(cfg *Config, state *AppState) ExposureCapStatus {
	if cfg == nil || state == nil || !exposureCapConfigured(cfg.PortfolioRisk) {
		return ExposureCapStatus{}
	}
	ids := make([]string, 0, len(state.Strategies))
	for id := range state.Strategies {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var pv float64
	for _, id := range ids {
		pv += displayStrategyValue(state.Strategies[id], nil)
	}
	return evaluateExposureCap(cfg.PortfolioRisk, state.Strategies, cfg.Strategies, nil, pv)
}

func exposureCapManualEntryBlock(st ExposureCapStatus, asset, dir string) (bool, string) {
	if !st.Configured {
		return false, ""
	}
	if (dir == "long" && st.LongBlocked) || (dir == "short" && st.ShortBlocked) {
		return true, exposureCapHoldDetail(st)
	}
	if stat, ok := st.OverConcentrated[asset]; ok && stat.Direction == dir {
		return true, fmt.Sprintf("%s net %s exposure $%.2f is %.1f%% of portfolio value $%.2f (cap %.1f%%)",
			asset, dir, stat.NetUSD, stat.Pct, st.PortfolioValue, st.ConcentrationPct)
	}
	return false, ""
}

func exposureCapBlocksSignal(st ExposureCapStatus, asset string, signal int, closeFraction, posQty float64, posSide string, allowsLong, allowsShort bool) (bool, string) {
	if !st.Configured || signal == 0 {
		return false, ""
	}
	if !pausedBlocksSignal(signal, closeFraction, posQty, posSide, allowsLong, allowsShort) {
		return false, ""
	}
	dir := "long"
	dirBlocked := st.LongBlocked
	bucketUSD := st.LongUSD
	if signal < 0 {
		dir = "short"
		dirBlocked = st.ShortBlocked
		bucketUSD = st.ShortUSD
	}
	if dirBlocked {
		return true, fmt.Sprintf("same-direction crypto exposure $%.2f exceeds cap $%.2f — new %s opens blocked", bucketUSD, st.CapUSD, dir)
	}
	if stat, ok := st.OverConcentrated[asset]; ok && stat.Direction == dir {
		return true, fmt.Sprintf("%s net %s exposure $%.2f is %.1f%% of portfolio value $%.2f (cap %.1f%%) — new %s %ss blocked",
			asset, dir, stat.NetUSD, stat.Pct, st.PortfolioValue, st.ConcentrationPct, asset, dir)
	}
	return false, ""
}

func optionsActionDirection(a OptionsAction) string {
	var actionSign float64
	switch a.Action {
	case "buy":
		actionSign = 1
	case "sell":
		actionSign = -1
	default:
		return ""
	}
	delta := a.Greeks.Delta
	if delta == 0 {
		delta = 1
		if a.OptionType == "put" {
			delta = -1
		}
	}
	if actionSign*delta < 0 {
		return "short"
	}
	return "long"
}

func exposureCapOptionsActions(st ExposureCapStatus, asset string, actions []OptionsAction) (kept []OptionsAction, dropped int, reason string) {
	if !st.Configured {
		return actions, 0, ""
	}
	for _, a := range actions {
		dir := optionsActionDirection(a)
		blocked := false
		switch dir {
		case "long":
			blocked = st.LongBlocked
		case "short":
			blocked = st.ShortBlocked
		}
		if !blocked && dir != "" {
			if stat, ok := st.OverConcentrated[asset]; ok && stat.Direction == dir {
				blocked = true
			}
		}
		if blocked {
			dropped++
			if reason == "" {
				bucketUSD := st.LongUSD
				if dir == "short" {
					bucketUSD = st.ShortUSD
				}
				if (dir == "long" && st.LongBlocked) || (dir == "short" && st.ShortBlocked) {
					reason = fmt.Sprintf("same-direction crypto exposure $%.2f exceeds cap $%.2f — new %s-delta option opens blocked", bucketUSD, st.CapUSD, dir)
				} else {
					stat := st.OverConcentrated[asset]
					reason = fmt.Sprintf("%s net %s exposure is %.1f%% of portfolio value (cap %.1f%%) — new %s-delta option opens blocked", asset, dir, stat.Pct, st.ConcentrationPct, dir)
				}
			}
			continue
		}
		kept = append(kept, a)
	}
	return kept, dropped, reason
}

func exposureCapHoldDetail(st ExposureCapStatus) string {
	var parts []string
	if st.LongBlocked {
		parts = append(parts, fmt.Sprintf("long $%.2f", st.LongUSD))
	}
	if st.ShortBlocked {
		parts = append(parts, fmt.Sprintf("short $%.2f", st.ShortUSD))
	}
	if len(parts) == 0 {
		return ""
	}
	return fmt.Sprintf("same-direction crypto exposure (%s) exceeds cap $%.2f", strings.Join(parts, ", "), st.CapUSD)
}

func sortedOverConcentrated(st ExposureCapStatus) []string {
	names := make([]string, 0, len(st.OverConcentrated))
	for a := range st.OverConcentrated {
		names = append(names, a)
	}
	sort.Strings(names)
	return names
}

func exposureCapCycleWarning(st ExposureCapStatus) string {
	if !st.Configured {
		return ""
	}
	var parts []string
	if st.LongBlocked {
		parts = append(parts, fmt.Sprintf("long bucket $%.2f > cap $%.2f — new long opens blocked", st.LongUSD, st.CapUSD))
	}
	if st.ShortBlocked {
		parts = append(parts, fmt.Sprintf("short bucket $%.2f > cap $%.2f — new short opens blocked", st.ShortUSD, st.CapUSD))
	}
	for _, a := range sortedOverConcentrated(st) {
		stat := st.OverConcentrated[a]
		parts = append(parts, fmt.Sprintf("%s net %s %.1f%% of portfolio value $%.2f > cap %.1f%% — new %s %ss blocked",
			a, stat.Direction, stat.Pct, st.PortfolioValue, st.ConcentrationPct, a, stat.Direction))
	}
	if len(parts) == 0 {
		return ""
	}
	return "exposure cap: " + strings.Join(parts, "; ")
}

func exposureCapSkippedWarning(st ExposureCapStatus) string {
	if !st.Configured || len(st.SkippedPositions) == 0 {
		return ""
	}
	return fmt.Sprintf("exposure cap: %d position(s) excluded from the same-direction sums (fail-safe — they neither block nor count): %s",
		len(st.SkippedPositions), strings.Join(st.SkippedPositions, "; "))
}

const exposureCapPVBasisMissWarning = "⚠️ exposure cap: max_asset_concentration_pct is configured but portfolio value is unavailable (<= 0) — the concentration arm CANNOT evaluate and enforces nothing this cycle"

type exposureCapAlertState struct {
	LongAlerted        bool
	ShortAlerted       bool
	ConcAlerted        map[string]string
	PVBasisMissAlerted bool
}

var exposureCapAlerts exposureCapAlertState

func exposureCapAlertMessage(st ExposureCapStatus, prev exposureCapAlertState, now time.Time) (string, exposureCapAlertState) {
	next := exposureCapAlertState{
		LongAlerted:        st.LongBlocked,
		ShortAlerted:       st.ShortBlocked,
		PVBasisMissAlerted: st.PVBasisMiss,
	}
	var lines []string
	if st.LongBlocked && !prev.LongAlerted {
		lines = append(lines, fmt.Sprintf("🛑 Long bucket $%.2f exceeds cap $%.2f — new long opens blocked (short entries unaffected)", st.LongUSD, st.CapUSD))
	}
	if st.ShortBlocked && !prev.ShortAlerted {
		lines = append(lines, fmt.Sprintf("🛑 Short bucket $%.2f exceeds cap $%.2f — new short opens blocked (long entries unaffected)", st.ShortUSD, st.CapUSD))
	}
	for _, a := range sortedOverConcentrated(st) {
		stat := st.OverConcentrated[a]
		if next.ConcAlerted == nil {
			next.ConcAlerted = make(map[string]string)
		}
		next.ConcAlerted[a] = stat.Direction
		if prev.ConcAlerted[a] != stat.Direction {
			lines = append(lines, fmt.Sprintf("🛑 %s net %s $%.2f is %.1f%% of portfolio value $%.2f (cap %.1f%%) — new %s %ss blocked",
				a, stat.Direction, stat.NetUSD, stat.Pct, st.PortfolioValue, st.ConcentrationPct, a, stat.Direction))
		}
	}
	if st.PVBasisMiss && !prev.PVBasisMissAlerted {
		lines = append(lines, exposureCapPVBasisMissWarning)
	}
	if len(lines) == 0 {
		return "", next
	}
	msg := fmt.Sprintf("⚠️ **Same-direction exposure cap** (%s UTC)\n%s\n"+
		"Blocking-only: closes, trailing SL/ratchet, and protection sync keep running and nothing is force-closed. "+
		"Blocks lift automatically when exposure falls back under the cap.",
		now.UTC().Format("2006-01-02 15:04"), strings.Join(lines, "\n"))
	return msg, next
}

func exposureCapStartupSummaryLine(pr *PortfolioRiskConfig) string {
	if !exposureCapConfigured(pr) {
		return ""
	}
	var parts []string
	if pr.MaxSameDirectionNotionalUSD > 0 {
		parts = append(parts, fmt.Sprintf("same_direction=$%.2f (crypto bucket)", pr.MaxSameDirectionNotionalUSD))
	}
	if pr.MaxAssetConcentrationPct > 0 {
		parts = append(parts, fmt.Sprintf("asset_concentration=%.1f%% of portfolio value", pr.MaxAssetConcentrationPct))
	}
	return fmt.Sprintf("[config] portfolio: exposure cap %s (blocks capped-direction opens only; closes and SL/TP management unaffected)", strings.Join(parts, " "))
}

func exposureCapStatusNote(pr *PortfolioRiskConfig, state *AppState, cfgStrategies []StrategyConfig, prices map[string]float64) string {
	if !exposureCapConfigured(pr) {
		return ""
	}
	var pv float64
	for _, ss := range state.Strategies {
		pv += displayStrategyValue(ss, prices)
	}
	st := evaluateExposureCap(pr, state.Strategies, cfgStrategies, prices, pv)
	var note string
	if st.CapUSD > 0 {
		if st.LongBlocked || st.ShortBlocked {
			note += fmt.Sprintf("\n🛑 exposure cap: long $%.2f / short $%.2f vs cap $%.2f — %s", st.LongUSD, st.ShortUSD, st.CapUSD, blockedDirectionsLabel(st))
		} else {
			note += fmt.Sprintf("\n🟢 exposure cap armed: long $%.2f / short $%.2f / cap $%.2f", st.LongUSD, st.ShortUSD, st.CapUSD)
		}
	}
	for _, a := range sortedOverConcentrated(st) {
		stat := st.OverConcentrated[a]
		note += fmt.Sprintf("\n🛑 exposure cap: %s net %s %.1f%% of portfolio value (cap %.1f%%) — new %s %ss blocked",
			a, stat.Direction, stat.Pct, st.ConcentrationPct, a, stat.Direction)
	}
	if st.PVBasisMiss {
		note += "\n" + exposureCapPVBasisMissWarning
	}
	return note
}

func blockedDirectionsLabel(st ExposureCapStatus) string {
	switch {
	case st.LongBlocked && st.ShortBlocked:
		return "new long AND short opens blocked"
	case st.LongBlocked:
		return "new long opens blocked"
	case st.ShortBlocked:
		return "new short opens blocked"
	}
	return ""
}
