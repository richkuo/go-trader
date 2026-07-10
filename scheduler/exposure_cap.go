package main

// #1270: portfolio-wide same-direction exposure cap.
//
// The gross notional cap (#42) treats a long and an equal short as the same
// risk, so a fully correlated all-long crypto book passes every check until
// the kill switch fires. This gate measures SIGNED exposure instead: per-asset
// net delta (reusing the #1270-extracted computeAssetDeltas that also feeds
// ComputeCorrelation) is bucketed by sign into a long sum and a short sum for
// the phase-1 "crypto" bucket (every spot/perps/manual position plus
// delta-weighted options — everything ComputeCorrelation measures; CME futures
// are not crypto and stay outside the bucket). When a bucket exceeds
// portfolio_risk.max_same_direction_notional_usd, position-INCREASING signals
// in that direction are held at the crypto dispatch sites via the #1150
// pausedBlocksSignal predicate — the opposite direction and every
// position-reducing action (closes, trailing SL/ratchet, protection sync,
// reduce-only exits) pass through untouched. The optional
// max_asset_concentration_pct arm blocks per-asset: an asset whose |net delta|
// exceeds the configured percent of portfolio value holds new opens in its net
// direction for strategies trading that asset only.
//
// Blocking-only, mirroring the #42 notional cap contract: nothing is ever
// force-closed, reduced, or mutated. Manual open/add/limit-open refuse next to
// their kill-switch / pending-CB / daily-loss guards — BOTH arms: the CLI path
// has no live marks, so positions value at AvgCost and the concentration basis
// is derived by manualExposureCapStatus (a configured protective arm is never
// silently bypassed on an entry path it governs).
//
// The gate is UNLATCHED: it is recomputed once per cycle from live positions
// and prices, so it clears itself as soon as exposure falls back under the
// cap. Positions with no usable price (no live mark AND no positive AvgCost)
// are excluded from the sums and surfaced — a pricing gap must never block
// everything or silently gate nothing. Both fields are SIGHUP hot-reloadable
// (threshold-only, blocking-only); max_notional_usd deliberately keeps its
// restart-required behavior.

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// ExposureCapAssetStat describes one over-concentrated asset: the direction of
// its net delta, its |net| as a percent of portfolio value, and the net USD.
type ExposureCapAssetStat struct {
	Direction string  // "long" / "short" (sign of the asset's net delta)
	Pct       float64 // |net| / portfolio value * 100
	NetUSD    float64 // signed net delta USD
}

// ExposureCapStatus is the once-per-cycle evaluation of the #1270 exposure
// cap. Pure data — computed under mu.RLock, consumed lock-free by the
// dispatch gates and operator surfaces.
type ExposureCapStatus struct {
	Configured       bool                            // at least one arm is set
	CapUSD           float64                         // max_same_direction_notional_usd (0 = arm disabled)
	LongUSD          float64                         // sum of positive per-asset net deltas
	ShortUSD         float64                         // sum of |negative| per-asset net deltas
	LongBlocked      bool                            // long bucket exceeds CapUSD
	ShortBlocked     bool                            // short bucket exceeds CapUSD
	ConcentrationPct float64                         // max_asset_concentration_pct (0 = arm disabled)
	PortfolioValue   float64                         // concentration basis (live portfolio value)
	PVBasisMiss      bool                            // concentration arm configured but basis <= 0 — it cannot evaluate
	OverConcentrated map[string]ExposureCapAssetStat // asset -> stat for assets over the concentration arm
	SkippedPositions []string                        // sorted "strategy/symbol: why" entries excluded from the sums
}

// exposureCapConfigured reports whether either exposure-cap arm is set.
func exposureCapConfigured(pr *PortfolioRiskConfig) bool {
	return pr != nil && (pr.MaxSameDirectionNotionalUSD > 0 || pr.MaxAssetConcentrationPct > 0)
}

// evaluateExposureCap aggregates signed per-asset exposure across every
// strategy and compares the same-direction bucket sums (and per-asset
// concentration) against the configured caps. Pure read — never mutates state;
// safe under mu.RLock. prices may be nil (manual CLI path): positions then
// value at AvgCost via computeAssetDeltas' fallback. portfolioValue is the
// concentration basis; pass 0 when unavailable (the concentration arm then
// flags PVBasisMiss instead of guessing).
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
	sort.Strings(names) // deterministic float summation + map assembly
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

// manualExposureCapStatus evaluates the exposure cap for the manual
// open/add/limit-open guards. The manual paths have no live price feed, so
// positions value at AvgCost (computeAssetDeltas' nil-prices fallback) and the
// concentration basis is the sum of displayStrategyValue at the same AvgCost
// fallback — the basis exposureCapStatusNote already uses for /status — so a
// concentration-only config is enforced on manual entries instead of silently
// inert. In the daemon-embedded dashboard path displayStrategyValue picks up
// the shared-wallet-reconciled value; the standalone CLI falls back to the
// modeled per-strategy sum (shared-wallet members virtual-sum there, which can
// overstate the basis and make the concentration arm slightly permissive —
// never the bucket arm, whose sums are basis-free).
func manualExposureCapStatus(cfg *Config, state *AppState) ExposureCapStatus {
	if cfg == nil || state == nil || !exposureCapConfigured(cfg.PortfolioRisk) {
		return ExposureCapStatus{}
	}
	ids := make([]string, 0, len(state.Strategies))
	for id := range state.Strategies {
		ids = append(ids, id)
	}
	sort.Strings(ids) // deterministic float summation
	var pv float64
	for _, id := range ids {
		pv += displayStrategyValue(state.Strategies[id], nil)
	}
	return evaluateExposureCap(cfg.PortfolioRisk, state.Strategies, cfg.Strategies, nil, pv)
}

// exposureCapManualEntryBlock reports whether a manual position-increasing
// entry (open/add/limit-open) for asset in direction dir must refuse, with the
// operator detail line. Bucket arm first, then the concentration arm for this
// asset — the same order exposureCapBlocksSignal applies at the dispatch
// sites. dir is "long"/"short".
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

// exposureCapBlocksSignal reports whether a check signal must be forced to
// hold (0) because it would increase exposure in a capped direction. It
// reuses the #1150 pausedBlocksSignal classifier to decide whether the signal
// is position-increasing at all — closes, pure-close directional exits, and
// signal==0 manage cycles always pass. For any position-increasing signal the
// direction of the NEW exposure equals the sign of the signal (fresh open,
// same-side add, and the flip/legacy edge all open in the signal's direction),
// so a short entry passes while only the long bucket is capped and vice versa.
// asset scopes the concentration arm: only strategies trading an
// over-concentrated asset are held, and only in that asset's net direction.
func exposureCapBlocksSignal(st ExposureCapStatus, asset string, signal int, closeFraction, posQty float64, posSide string, allowsLong, allowsShort bool) (bool, string) {
	if !st.Configured || signal == 0 {
		return false, ""
	}
	if !pausedBlocksSignal(signal, closeFraction, posQty, posSide, allowsLong, allowsShort) {
		return false, "" // position-reducing — always passes
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

// optionsActionDirection returns the coarse delta direction ("long"/"short")
// a new option leg would add, or "" for non-open actions. Uses the emitted
// greeks when present (sign of action-sign x delta), else the same coarse
// call/put fallback computeAssetDeltas applies to unmarked positions.
func optionsActionDirection(a OptionsAction) string {
	var actionSign float64
	switch a.Action {
	case "buy":
		actionSign = 1
	case "sell":
		actionSign = -1
	default:
		return "" // "close" reduces exposure; unknown actions are not opens
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

// exposureCapOptionsActions filters an options result's action list, dropping
// open actions whose coarse delta direction is capped (bucket arm, or the
// concentration arm for this underlying). Close actions always survive.
// Returns the surviving actions, the drop count, and the reason for the first
// drop (for the cycle log).
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

// exposureCapHoldDetail is the one-line operator explanation for the blocked
// bucket arm(s), used by the manual open/add/limit-open refusals.
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

// sortedOverConcentrated returns the over-concentrated asset names sorted for
// deterministic operator output (Go map iteration is randomized).
func sortedOverConcentrated(st ExposureCapStatus) []string {
	names := make([]string, 0, len(st.OverConcentrated))
	for a := range st.OverConcentrated {
		names = append(names, a)
	}
	sort.Strings(names)
	return names
}

// exposureCapCycleWarning renders the once-per-cycle [WARN] line while any
// arm is actively blocking. Empty when nothing is blocked.
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

// exposureCapSkippedWarning renders the once-per-cycle [WARN] line listing
// positions excluded from the sums for lack of a usable price. Empty when
// the cap is not configured or nothing was skipped.
func exposureCapSkippedWarning(st ExposureCapStatus) string {
	if !st.Configured || len(st.SkippedPositions) == 0 {
		return ""
	}
	return fmt.Sprintf("exposure cap: %d position(s) excluded from the same-direction sums (fail-safe — they neither block nor count): %s",
		len(st.SkippedPositions), strings.Join(st.SkippedPositions, "; "))
}

// exposureCapPVBasisMissWarning is the shared operator text for a configured
// concentration arm that cannot evaluate. Used verbatim by /status, the
// per-cycle [WARN], and the owner DM so log greps and operator reports match.
const exposureCapPVBasisMissWarning = "⚠️ exposure cap: max_asset_concentration_pct is configured but portfolio value is unavailable (<= 0) — the concentration arm CANNOT evaluate and enforces nothing this cycle"

// exposureCapAlertState tracks which blocks have already been DM'd so the
// owner is alerted once on the transition INTO blocked (per direction, per
// asset) rather than every cycle. Re-arms when a block clears. Written only
// from the main trading loop (single goroutine).
type exposureCapAlertState struct {
	LongAlerted        bool
	ShortAlerted       bool
	ConcAlerted        map[string]string // asset -> direction already alerted
	PVBasisMissAlerted bool
}

// exposureCapAlerts is the live alert-throttle state for the #1270 owner DM.
var exposureCapAlerts exposureCapAlertState

// exposureCapAlertMessage diffs the current status against the previous alert
// state and returns the owner DM to send ("" when nothing newly blocked) plus
// the next alert state. Pure function — the caller owns the state variable.
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

// exposureCapStartupSummaryLine is the one-line [config] summary printed at
// startup when an exposure cap is configured. Empty when disabled.
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

// exposureCapStatusNote renders the /status line(s) for the exposure cap.
// Empty when not configured. Callers hold mu.RLock; prices come from the
// caller's live-mark fetch. The concentration basis here is the display
// portfolio value (sum of displayStrategyValue) — the same figure the /status
// header shows — which can differ slightly from the trading loop's
// shared-wallet-reconciled basis; utilization of the bucket arm is exact.
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

// blockedDirectionsLabel names the capped bucket arm(s) for operator output.
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
