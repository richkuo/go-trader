package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// schedulerStarted is set once in main immediately before server.Start — the
// first spawn that can read PortfolioRisk under mu. ClearLatchedKillSwitchSharedWallet
// may only run while this is false (#1272).
var schedulerStarted atomic.Bool

// markSchedulerStarted ends the single-threaded startup phase. Call exactly
// once immediately before server.Start (or any other goroutine that reads
// AppState under mu).
func markSchedulerStarted() {
	schedulerStarted.Store(true)
}

// collectPriceSymbols returns the list of BinanceUS-format symbols to fetch
// for spot strategy valuation/notional. Only "spot" strategy types are
// included — spot positions are stored and fetched under the same key
// (e.g. "BTC/USDT"), so no aliasing is needed.
//
// Perps strategies are intentionally excluded: HL and OKX perps marks are
// now sourced from the venues they live on via fetchHyperliquidMids and
// fetchOKXPerpsMids (see collectPerpsMarkSymbols). Routing perps through
// BinanceUS spot introduced phantom PnL on shorts due to spot/perps basis
// drift — fixes issue #263 as a side effect (HL-only coins like HYPE,
// kPEPE, PURR no longer emit [WARN] Skipping zero price — fixes #262).
func collectPriceSymbols(strategies []StrategyConfig) []string {
	set := make(map[string]bool)
	for _, sc := range strategies {
		if sc.Type != "spot" {
			continue
		}
		if len(sc.Args) < 2 {
			continue
		}
		sym := sc.Args[1]
		if sym == "" {
			continue
		}
		set[sym] = true
	}
	symbols := make([]string, 0, len(set))
	for s := range set {
		symbols = append(symbols, s)
	}
	return symbols
}

// collectPerpsMarkSymbols returns two sorted slices of base-coin symbols
// for which the scheduler should fetch venue-native perps marks this cycle.
// hlCoins contains coins traded on Hyperliquid; okxCoins contains coins
// traded on OKX — each slice is deduplicated and sorted for deterministic
// iteration. Types other than "perps" and "manual" are ignored.
//
// The returned coins are used as inputs to fetchHyperliquidMids and
// fetchOKXPerpsMids respectively. This is the correct oracle for perps
// positions; see issue #263 for why BinanceUS spot is wrong.
//
// #1444: type=manual is a first-class HL perps position, so it belongs on this
// rail too. Before this it belonged to no rail at all — collectPriceSymbols
// filters on "spot" and collectFuturesMarkSymbols on "futures"+topstep — so a
// manual coin with no perps or hedge donor never entered the prices map. The
// per-cycle mark then read 0.0, and the `mark > 0` gates in the manual dispatch
// killed both the trailing stop-loss walker and the take-profit ratchet on
// every cycle, with no error and no warning.
//
// A manual strategy contributes sc.Symbol, NOT Args[1]. The manual dispatch,
// the position map and the walker all key off sc.Symbol, and loadConfig only
// derives Args from Symbol when Args was left empty — a hand-written args list
// may name a different coin, which would park the mark under a key nobody
// reads. Validation guarantees a non-empty Symbol and platform=hyperliquid for
// every manual strategy, so a manual entry on any other platform is skipped
// rather than routed to the OKX rail.
//
// collectFuturesMarkSymbols is deliberately NOT relaxed the same way: manual is
// rejected at load for any platform other than hyperliquid, and that collector
// also filters on platform=topstep, so the edit would be dead code.
//
// Named side effect: manual coins now join PortfolioValue, the exposure and
// drawdown inputs and the status page at live mids instead of a frozen
// AvgCost. That is the wanted direction, and it is the same reason the #1159
// hedge-coin block below exists.
func collectPerpsMarkSymbols(strategies []StrategyConfig) (hlCoins, okxCoins []string) {
	hlSet := make(map[string]bool)
	okxSet := make(map[string]bool)
	for _, sc := range strategies {
		var coin string
		switch sc.Type {
		case "perps":
			if len(sc.Args) < 2 {
				continue
			}
			coin = sc.Args[1]
		case "manual":
			// #1444: key off Symbol — the key the manual dispatch, the position
			// map and the trailing walker all read. Manual is hyperliquid-only
			// at load, so anything else contributes nothing (it must not fall
			// through to the OKX rail below).
			if sc.Platform != "hyperliquid" {
				continue
			}
			coin = sc.Symbol
		default:
			continue
		}
		if coin == "" {
			continue
		}
		switch sc.Platform {
		case "hyperliquid":
			hlSet[coin] = true
		case "okx":
			okxSet[coin] = true
		}
	}
	// #1159: hedge coins need a live mark for the same reasons primaries do —
	// PortfolioValue, the perps margin/drawdown inputs, exposure math and the
	// hedge reconciler's own sizing all read the marks map. Without this the
	// hedge leg would be valued at AvgCost, so its unrealized PnL would read as
	// exactly zero forever: a losing hedge would be invisible to the circuit
	// breaker and the hedge reconciler could not size a single order.
	for _, coin := range hedgeCoinsForStrategies(strategies) {
		hlSet[coin] = true
	}
	hlCoins = make([]string, 0, len(hlSet))
	for c := range hlSet {
		hlCoins = append(hlCoins, c)
	}
	sort.Strings(hlCoins)

	okxCoins = make([]string, 0, len(okxSet))
	for c := range okxSet {
		okxCoins = append(okxCoins, c)
	}
	sort.Strings(okxCoins)
	return hlCoins, okxCoins
}

// mergePerpsMarks copies non-zero perps mark prices into the shared prices
// map. An existing entry wins — a mark published by a strategy earlier in
// the cycle (ground truth for that cycle) must not be overwritten by a
// potentially staler exchange snapshot. Zero and negative marks are skipped.
//
// DO NOT remove the skip-if-exists guard: it preserves the invariant that
// strategy-published marks always win over fetcher snapshots. This mirrors
// the mergeFuturesMarks contract (scheduler/risk.go).
func mergePerpsMarks(prices map[string]float64, marks map[string]float64) {
	for sym, p := range marks {
		if p <= 0 {
			continue
		}
		if _, exists := prices[sym]; exists {
			continue
		}
		prices[sym] = p
	}
}

// missingMarkPosition names one open position that reached the end of a cycle
// with no usable live mark in the shared prices map.
//
// Live separates the two consequences of the same miss, because they need
// different operator channels. A live position also loses its mark-gated
// managers (the Hyperliquid trailing stop-loss walker, the take-profit
// ratchet), so an auto-protective mechanism is disabled and the operator must
// be told through a channel they watch. A record-only position keeps every
// management path it ever had, but its value still feeds PortfolioValue and
// therefore the portfolio kill switch's equity drawdown input, so the miss is
// still reported — as a log line only.
type missingMarkPosition struct {
	StrategyID string
	Symbol     string
	Live       bool
	// Platform and Type name the venue the position lives on, so the operator
	// alert can describe the right management surface instead of assuming
	// Hyperliquid.
	Platform string
	Type     string
	// DisabledManagers names the mark-gated auto-protective mechanisms that
	// actually exist for this position's type and venue — empty on venues that
	// run none. See markGatedManagers.
	DisabledManagers []string
}

// markGatedManagers names the Go-side mark-gated auto-protective mechanisms
// that exist for a position of this TYPE on this VENUE, i.e. the managers a
// missing mark actually stops.
//
// Scoped to Hyperliquid perps and manual because that is where those managers
// are dispatched: the trailing stop-loss walker (hyperliquid_trailing_stop.go,
// whose own effectiveTrailingStopPct returns 0 for any other platform or type)
// and the take-profit ratchet (main.go's HL perps manage path and the manual
// dispatch). The OKX perps branch runs runOKXCheck / runOKXExecuteOrder and
// neither of them; spot and TopStep futures likewise.
//
// #1445 review: the owner DM used to assert both mechanisms unconditionally,
// so a live BinanceUS spot or TopStep futures mark outage told the operator a
// stop-loss walker had stopped when no walker existed. An alert that names a
// specific protection must only name one that applies. The valuation
// consequence — PortfolioValue falling back to AvgCost inside the portfolio
// kill switch's drawdown input — is universal and is stated separately by
// formatMissingMarkDM, so a venue with no managers still carries a true claim.
//
// Deliberately keyed on type+venue rather than on individual close-strategy
// knobs: the knob-level gates are position-dependent (effectiveTrailingStopPct
// reads pos.EntryATR, and a post-TP `sl_after: trail_from_here` stamp arms the
// walker on a strategy that configures no trailing field at all), so a
// config-only test would under-report a genuinely disabled protection. Over-
// reporting inside the venue that owns the mechanism is safe; claiming it on a
// venue that has none is not.
func markGatedManagers(sc StrategyConfig) []string {
	if sc.Platform != "hyperliquid" {
		return nil
	}
	switch sc.Type {
	case "perps", "manual":
		return []string{"Trailing stop-loss walker", "Take-profit ratchet"}
	}
	return nil
}

// collectMissingMarkPositions reports every open position whose symbol carries
// no positive price in the cycle's prices map. A missing mark is silent by
// construction — a map[string]float64 miss reads 0.0, PortfolioValue falls back
// to pos.AvgCost, and every mark-gated manager (the Hyperliquid trailing
// stop-loss walker, the take-profit ratchet) returns early behind its own
// `mark > 0` guard. #1444: a whole strategy type sat outside the mark rails for
// as long as it existed and nothing said so. This guard is the regression
// tripwire, so a future type cannot repeat that silently.
//
// Pure by design (no state, no locks, no subprocess) per the CLAUDE.md testing
// rule; the caller snapshots openSymbols under mu and prints the result.
//
// openSymbols maps strategy ID to the symbols that strategy currently holds an
// open position in. Strategies absent from the map are treated as flat.
//
// One exclusion, deliberate: options strategies. Their value comes from
// OptionPositions (CurrentValueUSD, marked in Phase 5), and any spot/perps leg
// parked in Positions has no collector feeding it, so including them would
// emit a standing warning about a pre-existing valuation path rather than a
// regression.
//
// Record-only (non-live) strategies are NOT excluded. Their management paths
// are gated on liveness by design, so nothing breaks there — but
// computeSubsetPortfolioValue virtual-sums every strategy that is not folded
// into a shared-wallet dedup set, and sameAccountLiveManualMembers folds in
// LIVE manual only. A record-only manual position therefore reaches totalPV
// through PortfolioValue, where a missing key silently reverts it to AvgCost
// inside the portfolio kill switch's drawdown input — the exact silent
// fallback this tripwire exists to close. They are reported with Live=false so
// the caller logs them without raising the live-protection alarm.
//
// Output order is strategy order (config order) then symbol order, so operator
// logs stay stable across cycles.
func collectMissingMarkPositions(strategies []StrategyConfig, openSymbols map[string][]string, prices map[string]float64) []missingMarkPosition {
	if len(openSymbols) == 0 {
		return nil
	}
	var out []missingMarkPosition
	for _, sc := range strategies {
		switch sc.Type {
		case "spot", "perps", "futures", "manual":
		default:
			continue
		}
		syms := openSymbols[sc.ID]
		if len(syms) == 0 {
			continue
		}
		live := isLiveArgs(sc.Args)
		sorted := append([]string(nil), syms...)
		sort.Strings(sorted)
		for _, sym := range sorted {
			if sym == "" {
				continue
			}
			if prices[sym] > 0 {
				continue
			}
			out = append(out, missingMarkPosition{
				StrategyID:       sc.ID,
				Symbol:           sym,
				Live:             live,
				Platform:         sc.Platform,
				Type:             sc.Type,
				DisabledManagers: markGatedManagers(sc),
			})
		}
	}
	return out
}

// snapshotOpenSymbolsByStrategy builds the strategy_id -> open symbols map
// collectMissingMarkPositions consumes. The caller MUST hold mu (read or
// write) — it walks StrategyState.Positions directly.
//
// A position counts as open on Quantity > 0. Zero and negative quantities are
// the #1009 corrupt-position shape, which the force-close path clears with a
// zero-PnL leg; warning about a missing mark for one would report the wrong
// defect.
func snapshotOpenSymbolsByStrategy(state *AppState) map[string][]string {
	if state == nil {
		return nil
	}
	out := make(map[string][]string, len(state.Strategies))
	for sid, s := range state.Strategies {
		if s == nil {
			continue
		}
		for sym, pos := range s.Positions {
			if pos == nil || pos.Quantity <= 0 {
				continue
			}
			out[sid] = append(out[sid], sym)
		}
	}
	return out
}

// formatManualMarkBasisRebaselineDM is the owner-DM text for the one-shot
// #1444 valuation-basis peak migration. It states both totals so the operator
// can check the delta against their own book, and says plainly that the
// drawdown reading was not reset.
func formatManualMarkBasisRebaselineDM(priorPeak, newPeak, liveTotal, legacyTotal float64) string {
	return fmt.Sprintf("ℹ️ **Portfolio peak re-baselined once (#1444 valuation basis)**\nManual positions now value at the live mark instead of entry cost, so the stored peak was measured on the old basis.\n• Peak: $%.2f → $%.2f\n• Live-priced total: $%.2f\n• Same book on the old basis: $%.2f\n• Basis delta: $%.2f\nThe drawdown reading was NOT reset — only the units were corrected. This runs once and is recorded in the kill-switch event log.",
		priorPeak, newPeak, liveTotal, legacyTotal, liveTotal-legacyTotal)
}

// manualOnlyMarkSymbols returns the coins that reach the shared prices map
// ONLY because of the #1444 manual branch in collectPerpsMarkSymbols — the
// manual coins that no pre-#1444 rail already donated.
//
// This is the exact set whose valuation basis the #1444 change moved. A manual
// coin that a perps strategy, a hedge leg, a spot strategy or a futures
// strategy already contributed was live-marked before #1444 too, so its basis
// did not move and it must not be counted.
//
// Pure by design: the one-shot peak re-baseline (#1445 review) uses it to
// rebuild the pre-#1444 valuation and measure the basis shift exactly.
func manualOnlyMarkSymbols(strategies []StrategyConfig) []string {
	donors := make(map[string]bool)
	for _, sc := range strategies {
		switch sc.Type {
		case "perps", "spot":
			if len(sc.Args) >= 2 && sc.Args[1] != "" {
				donors[sc.Args[1]] = true
			}
		}
	}
	for _, coin := range hedgeCoinsForStrategies(strategies) {
		donors[coin] = true
	}
	for _, sym := range collectFuturesMarkSymbols(strategies) {
		donors[sym] = true
	}

	set := make(map[string]bool)
	for _, sc := range strategies {
		if sc.Type != "manual" || sc.Platform != "hyperliquid" {
			continue
		}
		if sc.Symbol == "" || donors[sc.Symbol] {
			continue
		}
		set[sc.Symbol] = true
	}
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// missingManualOnlyMarks returns the manual-only coins that are held open this
// cycle but carry no positive mark, sorted.
//
// This is the exact precondition of the one-shot #1444 valuation-basis peak
// migration, and it is deliberately NARROWER than "every open position has a
// mark". The migration measures
//
//	delta = totalPV(prices) - totalPV(pricesWithoutSymbols(prices, manualOnly))
//
// and computeSubsetPortfolioValue is additive over PortfolioValue plus
// price-independent real wallet balances, so only positions held under a
// manual-only coin can contribute to that difference. A missing mark on a
// spot / OKX-perps / futures coin is absent from BOTH maps, falls back to
// pos.AvgCost in both, and cancels out of the delta exactly.
//
// Gating the migration on those unrelated coins would defer it during a
// transient non-manual mark outage — and every deferred cycle is a cycle where
// an underwater manual position is already live-priced in totalPV while the
// peak is still on the cost basis, which is precisely the spurious first-cycle
// kill-switch fire the migration exists to prevent.
//
// The walk is over the open-position map rather than the strategy configs, so
// a manual-only coin held by a strategy that no longer declares it still
// counts: it is its VALUATION that moved, whoever holds it.
//
// Pure by design (no state, no locks, no subprocess) per the CLAUDE.md testing
// rule.
func missingManualOnlyMarks(strategies []StrategyConfig, openSymbols map[string][]string, prices map[string]float64) []string {
	manualOnly := manualOnlyMarkSymbols(strategies)
	if len(manualOnly) == 0 || len(openSymbols) == 0 {
		return nil
	}
	want := make(map[string]bool, len(manualOnly))
	for _, sym := range manualOnly {
		want[sym] = true
	}
	missing := make(map[string]bool)
	for _, syms := range openSymbols {
		for _, sym := range syms {
			if sym == "" || !want[sym] || prices[sym] > 0 {
				continue
			}
			missing[sym] = true
		}
	}
	if len(missing) == 0 {
		return nil
	}
	out := make([]string, 0, len(missing))
	for sym := range missing {
		out = append(out, sym)
	}
	sort.Strings(out)
	return out
}

// pricesWithoutSymbols returns a copy of prices with the named keys removed.
// Deleting a key (rather than zeroing it) is what reproduces the pre-#1444
// valuation exactly: PortfolioValue falls back to pos.AvgCost on a MISSING
// key, and values a position at zero on a present-but-zero one.
func pricesWithoutSymbols(prices map[string]float64, drop []string) map[string]float64 {
	if len(drop) == 0 {
		return prices
	}
	out := make(map[string]float64, len(prices))
	for k, v := range prices {
		out[k] = v
	}
	for _, sym := range drop {
		delete(out, sym)
	}
	return out
}

// manualMarkBasisPeakAdjustment returns the re-baselined portfolio peak for the
// one-shot #1444 valuation-basis migration, and whether it should be applied.
//
// #1444 moved manual positions from a frozen AvgCost valuation onto the live
// mark rail. PortfolioRisk.PeakValue is a sticky, ratchet-up-only high-water
// mark accumulated under the OLD basis, so the first post-upgrade cycle
// compares a live-priced total against a cost-priced peak. An underwater
// manual position can therefore latch the kill switch on that first cycle and
// flatten the fleet on an accounting change rather than a loss.
//
// The adjustment is the basis delta and NOTHING else:
//
//	newPeak = oldPeak + (liveTotal - legacyTotal)
//
// It deliberately does NOT zero the drawdown or set the peak to the current
// total. A real drawdown already accumulated under the old basis survives the
// migration untouched — only the units change. A blanket re-baseline would
// disarm a legitimately armed kill switch, which is the failure mode this
// helper exists to avoid.
//
// Not applied when oldPeak is not positive (a cold-start peak has no legacy
// basis to correct) or when the delta is zero (no manual position moved). The
// result is floored at a positive value so a pathological delta cannot produce
// a zero or negative peak, which CheckPortfolioRisk reads as "no peak yet".
func manualMarkBasisPeakAdjustment(oldPeak, liveTotal, legacyTotal float64) (float64, bool) {
	if oldPeak <= 0 {
		return oldPeak, false
	}
	delta := liveTotal - legacyTotal
	if delta == 0 {
		return oldPeak, false
	}
	newPeak := oldPeak + delta
	if newPeak <= 0 {
		return oldPeak, false
	}
	return newPeak, true
}

// collectFuturesMarkSymbols returns the list of CME futures contract
// symbols (e.g. "ES", "NQ", "MES", "MNQ", "CL") that need live marks to
// revalue open futures positions. Sibling to collectPriceSymbols — kept
// separate because the price-source rail is different: check_price.py
// queries BinanceUS which does not list CME futures, so the Go scheduler
// has to dispatch these symbols to fetch_futures_marks.py (TopStep
// adapter) instead.
//
// Futures strategies store positions under the bare contract symbol
// (state.Positions["ES"]) with Multiplier > 0; the strategy's Args[1] is
// the same symbol, so no normalization or alias mirroring is needed.
// Issue #261: without this, PortfolioNotional / PortfolioValue fell back
// to pos.AvgCost for futures, freezing exposure at entry cost.
//
// Platform filter: only "topstep" futures strategies are emitted.
// fetch_futures_marks.py hardcodes TopStepExchangeAdapter, so routing a
// non-TopStep futures symbol (e.g. a future IBKR futures adapter) through
// this path would either fail outright or — worse — succeed against a
// different contract on a different exchange. When a second futures
// adapter is added, this helper should be generalized to return a
// platform→symbols map (or similar) and fetch_futures_marks.py should
// gain platform-aware dispatch.
func collectFuturesMarkSymbols(strategies []StrategyConfig) []string {
	set := make(map[string]bool)
	for _, sc := range strategies {
		if sc.Type != "futures" {
			continue
		}
		if sc.Platform != "topstep" {
			continue
		}
		if len(sc.Args) < 2 {
			continue
		}
		sym := sc.Args[1]
		if sym == "" {
			continue
		}
		set[sym] = true
	}
	if len(set) == 0 {
		return nil
	}
	symbols := make([]string, 0, len(set))
	for s := range set {
		symbols = append(symbols, s)
	}
	sort.Strings(symbols)
	return symbols
}

// mergeFuturesMarks copies non-zero futures mark prices into the shared
// prices map. Existing entries win — matches mirrorPerpsPrices semantics
// so that any live mark a strategy may have already published during the
// cycle is not overwritten by a (possibly staler) fetch result.
//
// DO NOT "simplify" the skip-if-exists branch. Today the only writer
// that could pre-populate e.g. prices["ES"] is a hypothetical futures
// strategy publishing its own live exchange mark via result.Symbol
// earlier in the cycle — mirrorPerpsPrices runs first but only writes
// "/USDT"-quoted spot keys, so collisions with bare CME symbols like
// "ES" or "NQ" are not expected in the current code paths. The guard is
// still required: a strategy-published mark is ground truth for the
// cycle (observed during check), whereas fetch_futures_marks is a
// generic snapshot pulled afterwards and may be slightly stale. When
// both exist, prefer the former. Preserving this invariant matters for
// anyone adding strategy-level mark publishing later — removing the
// skip would silently regress that contract.
func mergeFuturesMarks(prices map[string]float64, marks map[string]float64) {
	for sym, p := range marks {
		if p <= 0 {
			continue
		}
		if _, exists := prices[sym]; exists {
			continue
		}
		prices[sym] = p
	}
}

const maxKillSwitchEvents = 50

// untrustedEquityLatchDeferral bounds how long the portfolio equity latch will
// wait out an over-limit reading taken from an untrusted total (#1449 review).
// It is the maximum extra exposure a genuine crash can accumulate while the
// shared-wallet balance endpoint is unreachable, and simultaneously the
// minimum outage a spurious understated substitute must sustain before it can
// flatten the book. Deliberately not configurable: it is a safety bound on an
// auto-protective mechanism, and CurrentConfigVersion stays 17.
const untrustedEquityLatchDeferral = 15 * time.Minute

// KillSwitchEvent records a kill switch lifecycle event for audit purposes.
//
// Source identifies which drawdown signal drove a "triggered" or "warning"
// event: "equity" (classic peak-relative equity drawdown) or "margin" (perps
// unrealized loss vs. deployed margin, #296). Empty for events that predate
// #296 or are signal-agnostic (e.g. "reset", "auto_reset"). DrawdownPct is the
// percentage of the signal named by Source, so tailing the event log for a
// post-incident review gives an arithmetically consistent record.
type KillSwitchEvent struct {
	Timestamp      time.Time `json:"timestamp"`
	Type           string    `json:"type"` // "triggered", "reset", "warning"
	Source         string    `json:"source,omitempty"`
	DrawdownPct    float64   `json:"drawdown_pct"`
	PortfolioValue float64   `json:"portfolio_value"`
	PeakValue      float64   `json:"peak_value"`
	Details        string    `json:"details"`
}

// PortfolioRiskState tracks aggregate portfolio-level risk (#42).
//
// CurrentDrawdownPct is pure equity drawdown ((PeakValue - totalValue) /
// PeakValue). CurrentMarginDrawdownPct is the #296 perps-margin drawdown
// (perps unrealized loss / deployed margin). Keeping them as separate fields
// preserves the arithmetic invariant that (PeakValue, CurrentDrawdownPct) is
// reconstructable, while still exposing the margin signal for operators and
// the kill switch. #1448: the portfolio latch is owned by CurrentDrawdownPct
// whenever the equity guard can measure (equityAvailable && PeakValue > 0);
// CurrentMarginDrawdownPct warns, and trips the latch only when the equity
// guard cannot measure.
//
// #1449 review: that reconstructability invariant has EXACTLY ONE exception,
// and DrawdownReadingSubstituted is the marker for it. On an untrusted cycle
// (a substituted or one-generation-stale total) CurrentDrawdownPct carries the
// floored reading the kill switch actually decided on rather than the raw
// quotient, so the persisted number always matches the number that governed
// the cycle. DrawdownReadingSubstituted is true on exactly those cycles, and
// every operator surface that prints CurrentDrawdownPct next to PeakValue must
// label the reading when it is set — otherwise the percentage and the dollar
// figures beside it silently stop reconciling. A trusted cycle always writes
// the exact measurement and clears the marker.
// WarningSent is retained for persisted status visibility and is true while
// either drawdown signal is in the warning band; notifications are emitted on
// every cycle in that band.
type PortfolioRiskState struct {
	PeakValue                float64 `json:"peak_value"`
	CurrentDrawdownPct       float64 `json:"current_drawdown_pct"`
	CurrentMarginDrawdownPct float64 `json:"current_margin_drawdown_pct,omitempty"`
	// DrawdownReadingSubstituted marks CurrentDrawdownPct as the floored
	// decision value of an untrusted cycle instead of a direct measurement.
	DrawdownReadingSubstituted bool `json:"drawdown_reading_substituted,omitempty"`
	// UntrustedOverLimitSince stamps the start of an unbroken run of cycles on
	// which an UNTRUSTED equity total (substituted or one-generation-stale)
	// measured a drawdown above MaxDrawdownPct. While it is set the portfolio
	// equity latch is DEFERRED, not disarmed: an untrusted measurement must
	// not flatten the whole book on its own, but it also must not disarm the
	// only full-book protection indefinitely, so the latch escalates once the
	// run exceeds untrustedEquityLatchDeferral. Any cycle that does not
	// qualify — trusted, at or below the limit, or equity guard unarmed —
	// clears it, and so does the firing latch and every reset path. Persisted
	// so a crash-restart loop cannot keep restarting the window.
	UntrustedOverLimitSince time.Time         `json:"untrusted_over_limit_since,omitempty"`
	KillSwitchActive        bool              `json:"kill_switch_active"`
	KillSwitchAt            time.Time         `json:"kill_switch_at,omitempty"`
	WarningSent             bool              `json:"warning_sent,omitempty"`
	WarnBandEnteredAt       time.Time         `json:"warn_band_entered_at,omitempty"`
	LastWarningEquityDDPct  float64           `json:"last_warning_equity_dd_pct,omitempty"`
	LastWarningMarginDDPct  float64           `json:"last_warning_margin_dd_pct,omitempty"`
	WarningEquityDeltaPct   float64           `json:"warning_equity_delta_pct,omitempty"`
	WarningMarginDeltaPct   float64           `json:"warning_margin_delta_pct,omitempty"`
	Events                  []KillSwitchEvent `json:"events,omitempty"`

	// ManualMarkBasisRebaselined latches the one-shot #1444 valuation-basis
	// peak migration (see manualMarkBasisPeakAdjustment). Persisted, so a
	// restart cannot re-run it and move the peak a second time. Set on the
	// first cycle that can measure the shift — every open position has a
	// positive mark — regardless of whether an adjustment was actually
	// needed, so a later cycle never revisits a basis that has already
	// converged.
	ManualMarkBasisRebaselined bool `json:"manual_mark_basis_rebaselined,omitempty"`
}

// SharedWalletBalanceFetcher returns the real on-chain balance for a given
// platform. Implementations are expected to encapsulate any address/credential
// lookup (e.g. environment variables) and return a non-nil error on any
// network or configuration failure.
type SharedWalletBalanceFetcher func(platform string) (float64, error)

// detectSharedWalletPlatforms returns actually detected live shared-wallet
// platforms where every automated/manual risk-path member uses a legacy
// percentage allocation that could have produced the inflated persisted peak
// #244 repairs. One fixed-capital or zero-baseline pool member excludes the
// entire wallet: its kill-switch latch may reflect a real loss, and a process
// restart must never grant that account a fresh drawdown budget.
func detectSharedWalletPlatforms(strategies []StrategyConfig) []string {
	byID := make(map[string]StrategyConfig, len(strategies))
	walletMembers := make(map[SharedWalletKey][]string)
	for _, sc := range strategies {
		byID[sc.ID] = sc
		if key, ok := walletKeyFor(sc); ok && hasSharedWalletBalanceFetcher(key.Platform) {
			walletMembers[key] = append(walletMembers[key], sc.ID)
		}
	}
	platformSet := make(map[string]bool)
	for key, memberIDs := range walletMembers {
		memberIDs = riskPathWalletMemberIDs(key, memberIDs, strategies)
		if len(memberIDs) < 2 {
			continue
		}
		allLegacyPct := true
		for _, id := range memberIDs {
			if byID[id].CapitalPct <= 0 {
				allLegacyPct = false
				break
			}
		}
		if allLegacyPct {
			platformSet[key.Platform] = true
		}
	}
	var platforms []string
	for platform := range platformSet {
		platforms = append(platforms, platform)
	}
	sort.Strings(platforms)
	return platforms
}

// ClearLatchedKillSwitchSharedWallet auto-clears a latched portfolio kill
// switch on startup when a shared wallet is in use AND the real on-chain
// balance can be successfully fetched for every shared-wallet platform. This
// protects against legacy state where an inflated PortfolioRisk.PeakValue
// (e.g. from earlier shared-wallet double-counting) would otherwise leave the
// kill switch latched forever across restarts. See issue #244.
//
// Guards (all must hold):
//   - the kill switch must currently be active (otherwise no-op)
//   - at least one platform must host an account-detected shared wallet whose
//     2+ risk-path members all use legacy capital_pct (the #244 fake-peak mode)
//   - fetcher must successfully return a real balance for EVERY shared-wallet
//     platform — any network/config failure preserves the kill switch so the
//     re-baselined peak reflects the full portfolio-wide truth rather than a
//     partial slice that would under-baseline PeakValue
//
// On success, PortfolioRisk.PeakValue is re-baselined to the verified total
// balance (and drawdown fields zeroed) so the very next CheckPortfolioRisk
// call cannot immediately re-latch the kill switch using a stale inflated
// peak — the original root cause from #244.
//
// CONCURRENCY: This function mutates state.PortfolioRisk without holding any
// lock. It is only safe during the single-threaded startup phase — before
// markSchedulerStarted(). Calling it after that panics (#1272); do not hold
// mu across the balance fetcher I/O inside this helper.
//
// Returns true iff the kill switch was cleared.
func ClearLatchedKillSwitchSharedWallet(state *AppState, strategies []StrategyConfig, fetcher SharedWalletBalanceFetcher) bool {
	if schedulerStarted.Load() {
		panic("ClearLatchedKillSwitchSharedWallet called after scheduler started")
	}
	if state == nil || !state.PortfolioRisk.KillSwitchActive {
		return false
	}

	sharedPlatforms := detectSharedWalletPlatforms(strategies)
	if len(sharedPlatforms) == 0 {
		return false
	}

	// Fetch every shared-wallet platform up front. Any failure aborts the
	// clear so we never re-baseline PeakValue from an incomplete picture.
	totalBalance := 0.0
	for _, plat := range sharedPlatforms {
		balance, err := fetcher(plat)
		if err != nil {
			fmt.Printf("[INFO] Shared wallet (%s): kill switch NOT cleared — balance fetch failed: %v\n", plat, err)
			return false
		}
		totalBalance += balance
	}

	latchedAt := state.PortfolioRisk.KillSwitchAt.Format("2006-01-02 15:04 UTC")
	fmt.Printf("[INFO] Shared wallet (%v): clearing kill switch (was latched at %s, real total balance=$%.2f, prior peak=$%.2f)\n",
		sharedPlatforms, latchedAt, totalBalance, state.PortfolioRisk.PeakValue)

	state.PortfolioRisk.KillSwitchActive = false
	state.PortfolioRisk.KillSwitchAt = time.Time{}
	state.PortfolioRisk.WarningSent = false
	state.PortfolioRisk.WarnBandEnteredAt = time.Time{}
	state.PortfolioRisk.LastWarningEquityDDPct = 0
	state.PortfolioRisk.LastWarningMarginDDPct = 0
	state.PortfolioRisk.WarningEquityDeltaPct = 0
	state.PortfolioRisk.WarningMarginDeltaPct = 0
	// Re-baseline peak to the verified on-chain total so CheckPortfolioRisk
	// does not immediately re-latch on the first tick using the stale
	// (potentially double-counted) peak.
	state.PortfolioRisk.PeakValue = totalBalance
	state.PortfolioRisk.CurrentDrawdownPct = 0
	state.PortfolioRisk.CurrentMarginDrawdownPct = 0
	state.PortfolioRisk.DrawdownReadingSubstituted = false
	state.PortfolioRisk.UntrustedOverLimitSince = time.Time{}
	addKillSwitchEvent(&state.PortfolioRisk, "auto_reset", "",
		0, totalBalance, totalBalance,
		fmt.Sprintf("startup auto-clear: shared wallets %v reachable, total balance=$%.2f (peak re-baselined)",
			sharedPlatforms, totalBalance))
	return true
}

// portfolioPeakRebaselineAvailable returns true only when the cycle's portfolio
// total was computed without any missing-balance or stale-snapshot substitution.
func portfolioPeakRebaselineAvailable(usedPVFallback, usedStaleRiskBalance, pooledEquityComplete bool) bool {
	return !usedPVFallback && !usedStaleRiskBalance && pooledEquityComplete
}

// AutoResetConfirmedFlatKillSwitch clears a portfolio kill-switch latch after
// live close planning has confirmed all automated venues are flat. This is used
// only when no DM-capable owner is configured; owner-backed deployments keep the
// existing human-in-the-loop reset path.
//
// rebaselineValue is the best available estimate for post-close portfolio
// value. The hot loop typically passes the pre-close mark-to-market totalPV,
// which closely approximates post-close cash apart from fees and slippage.
// rebaselineAvailable must be false when that value includes a missing-balance
// fallback or stale pooled-wallet snapshot. The latch still clears in that
// case, but the prior real-equity peak is retained.
//
// Note: callers should suppress this auto-reset when the close plan has
// operator-required gaps such as OKX spot or Robinhood options. Those venues do
// not block OnChainConfirmedFlat because there is no safe automated close path,
// but resuming trading without a human reset would hide remaining live exposure.
//
// CONCURRENCY: lock-free body — the caller must hold mu while invoking this
// (hot-loop site in main does). Unlike ClearLatchedKillSwitchSharedWallet,
// this helper is intended for post-startup use under the state lock.
func AutoResetConfirmedFlatKillSwitch(
	prs *PortfolioRiskState,
	rebaselineValue float64,
	rebaselineAvailable bool,
	details string,
) bool {
	if prs == nil || !prs.KillSwitchActive {
		return false
	}

	prevEquityDrawdownPct := prs.CurrentDrawdownPct
	prevMarginDrawdownPct := prs.CurrentMarginDrawdownPct
	if details != "" {
		details = fmt.Sprintf("%s (previous equity drawdown=%.2f%%, previous margin drawdown=%.2f%%)",
			details, prevEquityDrawdownPct, prevMarginDrawdownPct)
	}
	if !rebaselineAvailable {
		details = fmt.Sprintf("%s (portfolio peak retained at $%.2f because current equity is not trustworthy)",
			details, prs.PeakValue)
	}

	prs.KillSwitchActive = false
	prs.KillSwitchAt = time.Time{}
	prs.WarningSent = false
	prs.WarnBandEnteredAt = time.Time{}
	prs.LastWarningEquityDDPct = 0
	prs.LastWarningMarginDDPct = 0
	prs.WarningEquityDeltaPct = 0
	prs.WarningMarginDeltaPct = 0
	if rebaselineAvailable {
		prs.PeakValue = rebaselineValue
	}
	prs.CurrentDrawdownPct = 0
	prs.CurrentMarginDrawdownPct = 0
	prs.DrawdownReadingSubstituted = false
	prs.UntrustedOverLimitSince = time.Time{}
	addKillSwitchEvent(prs, "auto_reset", "", 0, rebaselineValue, prs.PeakValue, details)
	return true
}

// ResetPortfolioKillSwitchManual clears a portfolio kill-switch latch on the
// operator's explicit instruction (the #1368 owner-DM reset). It returns the
// drawdown reading recorded at the moment of the reset so the caller can put it
// on the audit event before it is cleared.
//
// #1449 review: this exists so the manual reset clears the same drawdown
// readings both auto-reset paths clear (ClearLatchedKillSwitchSharedWallet,
// AutoResetConfirmedFlatKillSwitch). It previously cleared only the latch
// flags, which left CurrentDrawdownPct holding the OVER-LIMIT measurement the
// latching cycle persists before the latch check. That stale reading then fed
// the untrusted-cycle floor and could re-latch the whole book off a number
// nothing measured this cycle. The floor is clamped in the risk check as the
// structural guard; this removes the stale value at the source, so the warn
// band and the operator surfaces also stop reporting a pre-reset drawdown.
//
// Unlike the auto-reset paths this does NOT re-baseline PeakValue: a manual
// reset makes no claim about the portfolio being flat or the total being
// verified, so the real high-water mark is retained and the next cycle
// re-measures against it.
//
// CONCURRENCY: lock-free body — the caller must hold mu.
func ResetPortfolioKillSwitchManual(prs *PortfolioRiskState) float64 {
	if prs == nil {
		return 0
	}
	priorDrawdownPct := prs.CurrentDrawdownPct
	prs.KillSwitchActive = false
	prs.KillSwitchAt = time.Time{}
	prs.CurrentDrawdownPct = 0
	prs.CurrentMarginDrawdownPct = 0
	prs.DrawdownReadingSubstituted = false
	prs.UntrustedOverLimitSince = time.Time{}
	return priorDrawdownPct
}

// addKillSwitchEvent appends an event and trims to maxKillSwitchEvents.
//
// source identifies which drawdown signal drove the event: "equity", "margin",
// or "" (unknown / signal-agnostic). For "triggered" and "warning" events it
// must be set; for "reset" / "auto_reset" it is typically empty. DrawdownPct
// is interpreted as a pct of the signal named by source — do not pass
// max(equity, margin) here; pass the value for the specific source, otherwise
// the event log becomes arithmetically inconsistent.
func addKillSwitchEvent(prs *PortfolioRiskState, eventType, source string, drawdownPct, portfolioValue, peakValue float64, details string) {
	prs.Events = append(prs.Events, KillSwitchEvent{
		Timestamp:      time.Now().UTC(),
		Type:           eventType,
		Source:         source,
		DrawdownPct:    drawdownPct,
		PortfolioValue: portfolioValue,
		PeakValue:      peakValue,
		Details:        details,
	})
	if len(prs.Events) > maxKillSwitchEvents {
		prs.Events = prs.Events[len(prs.Events)-maxKillSwitchEvents:]
	}
}

// AggregatePerpsMarginInputs sums unrealized loss and deployed margin across
// every perps strategy in the portfolio. It returns the numerator and
// denominator inputs of the drawdown ratio (not a ratio itself) — matches the
// per-strategy counterpart perpsMarginDrawdownInputs (#292), aggregated to the
// portfolio level for the kill switch (#296).
//
// Only strategies with Type == "perps" contribute. configs maps strategy ID
// to StrategyConfig — used to source exchange sc.Leverage so the margin
// denominator matches the actual exchange leverage rather than the
// sizing_leverage order multiplier (#497). Strategies whose config is missing or has Leverage <= 0 are
// skipped; they don't contribute to the perps margin signal and the kill
// switch falls back to equity drawdown for them.
//
// Returns (0, 0) when no perps margin is deployed — the caller treats a zero
// margin as "no perps signal this cycle" and falls back to pure equity
// drawdown. This preserves existing behavior for all-spot / all-options
// portfolios.
func AggregatePerpsMarginInputs(strategies map[string]*StrategyState, configs []StrategyConfig, prices map[string]float64) (unrealizedLoss, margin float64) {
	leverageByID := make(map[string]float64, len(configs))
	for _, sc := range configs {
		leverageByID[sc.ID] = sc.Leverage
	}
	for id, s := range strategies {
		if s.Type != "perps" {
			continue
		}
		lev := leverageByID[id]
		if lev <= 0 {
			continue
		}
		loss, m := perpsMarginDrawdownInputs(s, lev, prices)
		unrealizedLoss += loss
		margin += m
	}
	return unrealizedLoss, margin
}

// CheckPortfolioRisk evaluates aggregate portfolio risk.
// Returns (allowed, notionalBlocked, warning, reason).
// allowed=false means the kill switch has fired or is latched; notionalBlocked=true
// means position-INCREASING opens must be held (#1344 — per-signal via
// pausedBlocksSignal at the dispatch sites; never a whole-strategy cycle skip)
// while closes/reductions and SL/TP maintenance keep running; warning=true
// means drawdown is approaching the kill switch threshold.
//
// Two independent drawdown signals are computed:
//
//  1. Equity drawdown — (peak - totalValue) / peak. Captures spot/options
//     PnL and overall cash erosion. Persisted as CurrentDrawdownPct.
//  2. Perps margin drawdown (#296) — perpsUnrealizedLoss / perpsMargin.
//     Captures leveraged-position losses against deployed margin, which a
//     pure equity view understates for all-perps accounts: a 50% loss on 10x
//     margin shows up as ~5% of total account value. Persisted as
//     CurrentMarginDrawdownPct.
//
// The two signals live on separate fields so (PeakValue, CurrentDrawdownPct)
// remains an arithmetically consistent equity tuple for post-incident review.
//
// #1448 — which signal owns the PORTFOLIO latch:
//
// The portfolio latch force-closes every position and blocks the whole book
// until an operator resets it, so its cost is the entire book. The margin
// signal and the equity signal shared one limit before #1448, and they
// diverge exactly when deployed margin is a SMALL share of the book. In that
// regime the loss a margin trip can avert is bounded by that small margin,
// while the latch still costs the full book, including manual and spot
// positions that contribute nothing to the margin ratio. A live incident
// (2026-08-22) latched the fleet at 65.3% margin drawdown on $48.42 of
// deployed margin while equity drawdown was 9.8% against a 30% limit.
//
// So the latch is owned by the signal that measures real book loss whenever
// that signal can measure at all:
//
//   - equity guard armed (equityAvailable && PeakValue > 0): the latch trips
//     on equity drawdown ONLY. Margin drawdown stays a warning lens.
//   - equity guard NOT armed (pooled shared wallet with no trustworthy
//     balance, or no positive valuation has EVER been recorded): margin
//     drawdown trips the latch, because it is then the only signal that can
//     protect the account.
//
// equityTrusted (#1449 review) is a THIRD state, distinct from both: the
// equity total exists but this cycle substituted or aged it —
// computeSubsetPortfolioValue replaced a failed shared-wallet balance fetch
// with sum(member PV), or resolveSharedWalletRiskBalances reused a
// one-generation-old balance. Both leave equityAvailable true. The latch
// STAYS with equity on those cycles on purpose: sum(member PV) prices every
// position at the same live marks, and a one-generation-old balance is a real
// balance, so the equity signal still measures real book loss — and handing
// the whole book to the margin signal over a transient balance-fetch blip is
// exactly the failure #1448 exists to remove. What an untrusted total must
// never do is make a drawdown DISAPPEAR, so two things are gated on
// equityTrusted instead: the peak ratchet does not run (the caller also rolls
// the peak back, so the two agree instead of leaving CurrentDrawdownPct
// written off a peak that was undone), and the measured equity drawdown is
// floored at the last recorded reading. The floor can never itself latch —
// any stored reading came from a cycle that did not latch, so it is at or
// below the limit — it can only stop an overstated substitute total from
// masking a loss that was already visible.
//
// Per-POSITION margin protection is unaffected and lives where it belongs:
// the #292 per-strategy circuit breaker measures the same margin-drawdown
// ratio and force-closes that one strategy on a cooldown, without latching
// the book (see CheckRisk).
//
// CAVEAT — circuit_breaker:false (#1449 review): that per-strategy breaker is
// opt-outable (#1048), and an explicit false suppresses BOTH of its firing
// arms. A perps strategy with the breaker disabled therefore has no automatic
// margin protection at any level once the portfolio latch belongs to equity,
// and where the breaker IS enabled its perps max_drawdown_pct default is 50
// against the portfolio default of 25, so the handover is to a LOOSER limit.
// That gap is made loud rather than silent: recordCircuitBreakerSuppression
// raises a throttled owner DM (not only a log line) on the cycle a disabled
// breaker crosses a halt threshold.
//
// The emitted KillSwitchEvent.Source records whether equity or margin drove
// the fire/warning so operators can tell at a glance which lever tripped.
func CheckPortfolioRisk(prs *PortfolioRiskState, cfg *PortfolioRiskConfig, totalValue, totalNotional, perpsUnrealizedLoss, perpsMargin float64) (allowed, notionalBlocked, warning bool, reason string) {
	return checkPortfolioRiskWithEquityAvailability(prs, cfg, totalValue, totalNotional, perpsUnrealizedLoss, perpsMargin, true, true)
}

// checkPortfolioRiskWithEquityAvailability allows the shared-wallet risk path
// to suppress the equity-drawdown signal when a pooled wallet has neither a
// current nor one-generation-old real balance. On that path the equity guard
// is not armed, so the perps margin signal remains the TRIP for the portfolio
// latch (#1448) rather than only a warning; the notional cap is unaffected.
// Existing callers use CheckPortfolioRisk and therefore retain the historical
// equity-available behavior.
//
// equityTrusted (#1449 review) is a separate, weaker signal: totalValue is
// usable but this cycle SUBSTITUTED or AGED it (sum(member PV) after a failed
// shared-wallet balance fetch, or a one-generation-old cached balance). The
// equity guard stays armed and keeps the latch; the peak ratchet is skipped
// and the measured drawdown is floored at the last recorded reading so an
// overstated substitute cannot mask a loss. See the #1448 block above for why
// the latch does not fall back to margin here. Pass equityTrusted=false
// whenever equityAvailable is false — an unavailable total is never trusted.
func checkPortfolioRiskWithEquityAvailability(prs *PortfolioRiskState, cfg *PortfolioRiskConfig, totalValue, totalNotional, perpsUnrealizedLoss, perpsMargin float64, equityAvailable, equityTrusted bool) (allowed, notionalBlocked, warning bool, reason string) {
	if prs.KillSwitchActive {
		return false, false, false, fmt.Sprintf("portfolio kill switch is latched (triggered at %s, manual reset required)",
			prs.KillSwitchAt.Format("2006-01-02 15:04:05 UTC"))
	}

	// #1449 review: the last recorded equity drawdown, read BEFORE anything
	// this cycle overwrites it. It is the floor applied on an untrusted cycle.
	//
	// It is clamped to cfg.MaxDrawdownPct because the stored reading is NOT
	// guaranteed to be below the limit: the latching cycle writes its
	// over-limit measurement to CurrentDrawdownPct before the latch check, and
	// the owner-DM reset path clears only KillSwitchActive/KillSwitchAt. An
	// unclamped floor would therefore re-latch the whole book on the first
	// untrusted cycle after that reset, off a reading nothing measured this
	// cycle. The clamp keeps the floor at or below the limit while the latch
	// test is a strict >, so a carried value can raise the reported reading
	// but can never fire the kill switch on its own. A genuine this-cycle
	// measurement above the limit is unaffected — it is not clamped.
	//
	// The clamp is applied at READ time, not stored, so a later
	// MaxDrawdownPct change (SIGHUP) re-derives it instead of permanently
	// truncating the recorded reading. When MaxDrawdownPct is non-positive the
	// clamp yields a non-positive floor, which cannot raise a drawdown that is
	// already floored at 0 — the degenerate config keeps its existing meaning.
	priorEquityDD := prs.CurrentDrawdownPct
	if priorEquityDD > cfg.MaxDrawdownPct {
		priorEquityDD = cfg.MaxDrawdownPct
	}

	// Ratchet peak high-water mark upward only. equityTrusted gates it so a
	// substituted or one-generation-stale total cannot move the high-water
	// mark. The caller also rolls an untrusted ratchet back; doing it here too
	// means equityDD below is computed against the peak that actually
	// survives, instead of against one the caller is about to undo.
	if equityAvailable && equityTrusted && totalValue > prs.PeakValue {
		prs.PeakValue = totalValue
	}

	// Compute both drawdown signals independently. Each is persisted to its
	// own field so (PeakValue, CurrentDrawdownPct) stays internally consistent
	// and operators can see both lenses at once.
	var equityDD, marginDD float64
	if equityAvailable && prs.PeakValue > 0 {
		equityDD = (prs.PeakValue - totalValue) / prs.PeakValue * 100
		if equityDD < 0 {
			equityDD = 0
		}
		// #1449 review: an untrusted total may raise the measured drawdown but
		// never lower it. Monotone across a run of untrusted cycles; the first
		// trusted cycle overwrites it with a real measurement. This closes the
		// fail-open direction (a substitute that overstates equity understates
		// the drawdown), while the clamp on priorEquityDD above keeps the floor
		// from firing the latch by itself.
		//
		// The floored value — not the raw quotient — is what gets persisted,
		// so CurrentDrawdownPct always equals the number this cycle's latch and
		// warn band decided on. DrawdownReadingSubstituted marks it as such for
		// the operator surfaces (see the PortfolioRiskState doc comment).
		substituted := false
		if !equityTrusted && equityDD < priorEquityDD {
			equityDD = priorEquityDD
			substituted = true
		}
		prs.CurrentDrawdownPct = equityDD
		prs.DrawdownReadingSubstituted = substituted
	}
	if perpsMargin > 0 && perpsUnrealizedLoss > 0 {
		marginDD = perpsUnrealizedLoss / perpsMargin * 100
	}
	prs.CurrentMarginDrawdownPct = marginDD

	// Kill switch (#1448): the equity guard owns the portfolio latch whenever
	// it can measure; the margin signal trips only when it cannot. The two
	// arms are mutually exclusive, so exactly one signal can latch the book on
	// any given cycle and the reason always names it.
	//
	// PeakValue == 0 is treated as "cannot measure" rather than as a separate
	// case. The peak ratchet above runs BEFORE this check, so any cycle with a
	// positive totalValue arms the guard in the same call. PeakValue == 0 here
	// therefore means no equity snapshot has ever been recorded (totalValue
	// has been non-positive on every cycle so far), which is the same
	// condition as equityAvailable == false: the equity guard is not
	// operative, and margin is the only signal that can protect the account.
	//
	// #1449 review — the exact reach of that clause: it is NOT "a cold-start
	// account that blows up margin on bar 1". A first cycle carrying any
	// positive total arms the guard through the ratchet above, so from that
	// cycle on the latch belongs to equity and margin can no longer fire it.
	// The margin arm on the equityAvailable path is reachable only while
	// totalValue has been non-positive on EVERY cycle, which is what
	// TestCheckPortfolioRisk_PeakZero_MarginCanStillFire pins with
	// totalValue = 0. Margin's real standing backstop is the equityAvailable
	// == false path (pooled wallet with no trustworthy balance), plus the #292
	// per-strategy circuit breaker on every path.
	equityGuardArmed := equityAvailable && prs.PeakValue > 0

	// #1449 review round 3 — the UPWARD direction of the untrusted reading.
	//
	// The floor above closes the fail-open direction: an untrusted total that
	// OVERSTATES equity can no longer mask a loss. The opposite direction was
	// still open. An untrusted total that UNDERSTATES equity inflates equityDD,
	// and nothing stopped that inflated number from tripping the latch and
	// flattening the whole book — manual and spot included — off a total the
	// same cycle already flagged as substituted or aged. That is the mirror of
	// the guarantee the floor already gives ("the floor can never itself
	// latch"), and the equity arm now gets it too: an untrusted measurement
	// alone does not flatten the book on the cycle it appears.
	//
	// It is a DEFERRAL, not a veto, and the difference is load-bearing. A
	// straight "only a trusted measurement may latch" rule opens a worse hole
	// than it closes, because the two untrusted sources have different
	// lifetimes:
	//
	//   - usedStaleRiskBalance is structurally one cycle. The stale branch in
	//     resolveSharedWalletRiskBalances requires Generation == generation-1
	//     and does NOT refresh the cache, so a second consecutive miss fails
	//     that test, equityComplete goes false, and the cycle lands on the
	//     equityAvailable == false path where margin owns the latch.
	//   - usedPVFallback has no such bound. computeTotalPortfolioValue
	//     substitutes sum(member PV) for as long as the balance fetch keeps
	//     failing, and equityAvailable stays true throughout. A pure veto would
	//     therefore leave the portfolio latch disarmed indefinitely during a
	//     balance-endpoint outage — and an outage is exactly when a genuine
	//     crash is least likely to be noticed. The loss that hole permits is
	//     unbounded; the loss a spurious flatten costs is slippage, fees and a
	//     manual reset. The unbounded error governs.
	//
	// So the latch waits out untrustedEquityLatchDeferral of CONTINUOUS
	// over-limit untrusted cycles and then fires. A transient blip never
	// reaches it; a persistent substituted total that keeps reading over the
	// limit is no longer credible as an artifact and is treated as the loss it
	// reports. UntrustedOverLimitSince is persisted so a restart loop cannot
	// keep resetting the window, and every non-qualifying cycle clears it, so
	// the window only ever measures an unbroken run.
	//
	// A trusted measurement over the limit is untouched and latches at once.
	//
	// cfg.MaxDrawdownPct <= 0 is excluded, matching the priorEquityDD clamp
	// above: a non-positive limit already means "latch on any drawdown", and
	// deferring that would silently redefine the degenerate config instead of
	// leaving its existing meaning alone.
	equityLatchDeferred := false
	if equityGuardArmed && !equityTrusted && cfg.MaxDrawdownPct > 0 && equityDD > cfg.MaxDrawdownPct {
		now := time.Now().UTC()
		if prs.UntrustedOverLimitSince.IsZero() {
			prs.UntrustedOverLimitSince = now
			addKillSwitchEvent(prs, "latch_deferred", "equity", equityDD, totalValue, prs.PeakValue,
				fmt.Sprintf("equity drawdown %.1f%% exceeds limit %.1f%% on an untrusted total (substituted or one-generation-stale); portfolio latch deferred up to %s pending a trusted measurement",
					equityDD, cfg.MaxDrawdownPct, formatWarningDuration(untrustedEquityLatchDeferral)))
		}
		equityLatchDeferred = now.Sub(prs.UntrustedOverLimitSince) < untrustedEquityLatchDeferral
	} else {
		prs.UntrustedOverLimitSince = time.Time{}
	}

	if (equityGuardArmed && !equityLatchDeferred && equityDD > cfg.MaxDrawdownPct) || (!equityGuardArmed && marginDD > cfg.MaxDrawdownPct) {
		prs.KillSwitchActive = true
		prs.KillSwitchAt = time.Now().UTC()
		prs.WarningSent = false
		prs.WarnBandEnteredAt = time.Time{}
		prs.LastWarningEquityDDPct = 0
		prs.LastWarningMarginDDPct = 0
		prs.WarningEquityDeltaPct = 0
		prs.WarningMarginDeltaPct = 0
		var r, source string
		var dd float64
		// #1448: the arms above are mutually exclusive, so the source falls
		// out of which guard is armed. No tie-break is possible or needed.
		if !equityGuardArmed {
			source = "margin"
			dd = marginDD
			if equityAvailable {
				r = fmt.Sprintf("portfolio perps margin drawdown %.1f%% exceeds limit %.1f%% (unrealized loss=$%.2f, margin=$%.2f, value=$%.2f, peak=$%.2f)",
					marginDD, cfg.MaxDrawdownPct, perpsUnrealizedLoss, perpsMargin, totalValue, prs.PeakValue)
			} else {
				r = fmt.Sprintf("portfolio perps margin drawdown %.1f%% exceeds limit %.1f%% (unrealized loss=$%.2f, margin=$%.2f; equity unavailable)",
					marginDD, cfg.MaxDrawdownPct, perpsUnrealizedLoss, perpsMargin)
			}
		} else {
			source = "equity"
			dd = equityDD
			// An escalated latch (the deferral window ran out while every
			// cycle in it read over the limit on an untrusted total) says so
			// in the reason. The operator's first question on a flattened book
			// is which number did it, and "substituted for N minutes" is a
			// different post-mortem from a clean measurement.
			if !prs.UntrustedOverLimitSince.IsZero() {
				r = fmt.Sprintf("portfolio drawdown %.1f%% exceeds limit %.1f%% (value=$%.2f, peak=$%.2f); measurement is UNTRUSTED (substituted or stale total) and has read over the limit continuously since %s — latch escalated after %s",
					equityDD, cfg.MaxDrawdownPct, totalValue, prs.PeakValue,
					prs.UntrustedOverLimitSince.Format("2006-01-02 15:04 UTC"),
					formatWarningDuration(untrustedEquityLatchDeferral))
			} else {
				r = fmt.Sprintf("portfolio drawdown %.1f%% exceeds limit %.1f%% (value=$%.2f, peak=$%.2f)",
					equityDD, cfg.MaxDrawdownPct, totalValue, prs.PeakValue)
			}
		}
		// The window has done its job once the latch fires; leaving it set
		// would make the reading look deferred to every surface that reads it
		// while the book is already flat.
		prs.UntrustedOverLimitSince = time.Time{}
		addKillSwitchEvent(prs, "triggered", source, dd, totalValue, prs.PeakValue, r)
		return false, false, false, r
	}

	// Warning check: approaching kill switch threshold on either signal.
	if cfg.MaxDrawdownPct > 0 {
		warnDrawdownPct := cfg.MaxDrawdownPct * cfg.WarnThresholdPct / 100
		// Both persisted fields are already written above, so the shared
		// helper sees exactly the values this cycle measured. The caller reads
		// the same helper for the #1449 warn-DM throttle — one implementation,
		// so the throttle's idea of "which signal is in band" can never drift
		// from the one that produced the warning.
		equityWarn, marginWarn := portfolioWarnBandSignals(cfg, prs, equityAvailable)
		if equityWarn || marginWarn {
			now := time.Now().UTC()
			if !prs.WarningSent {
				prs.WarnBandEnteredAt = now
				prs.WarningEquityDeltaPct = 0
				prs.WarningMarginDeltaPct = 0
			} else {
				if equityAvailable {
					prs.WarningEquityDeltaPct = equityDD - prs.LastWarningEquityDDPct
				} else {
					prs.WarningEquityDeltaPct = 0
				}
				prs.WarningMarginDeltaPct = marginDD - prs.LastWarningMarginDDPct
			}
			if equityAvailable {
				prs.LastWarningEquityDDPct = equityDD
			}
			prs.LastWarningMarginDDPct = marginDD
			prs.WarningSent = true
			warning = true
			// #1448: margin drawdown can now sit ABOVE cfg.MaxDrawdownPct
			// without latching (only reachable with the equity guard armed —
			// otherwise the margin arm above already tripped). Saying it is
			// "approaching" the limit would be false, so that case gets its
			// own wording naming who actually owns the latch and where
			// per-position margin protection lives.
			marginOverLimit := marginDD > cfg.MaxDrawdownPct
			switch {
			case equityLatchDeferred:
				// #1449 review round 3: equity is already OVER the limit, so
				// "approaching" would be false — and unlike the margin
				// over-limit cases the latch is not owned by another arm, it
				// is being held back deliberately. Say so, name the deadline,
				// and name what is protecting the book in the meantime, so an
				// operator reading this during a balance outage knows whether
				// to intervene by hand.
				reason = fmt.Sprintf("portfolio equity drawdown %.1f%% exceeds limit %.1f%% (value=$%.2f, peak=$%.2f) but the total is UNTRUSTED (substituted or one-generation-stale) — full-book latch DEFERRED since %s, escalates at %s unless a trusted measurement lands first; per-strategy circuit breakers (#292) remain active",
					equityDD, cfg.MaxDrawdownPct, totalValue, prs.PeakValue,
					prs.UntrustedOverLimitSince.Format("2006-01-02 15:04 UTC"),
					prs.UntrustedOverLimitSince.Add(untrustedEquityLatchDeferral).Format("2006-01-02 15:04 UTC"))
				if marginWarn {
					reason += fmt.Sprintf("; perps margin=%.1f%% (unrealized loss=$%.2f, margin=$%.2f)",
						marginDD, perpsUnrealizedLoss, perpsMargin)
				}
			case equityWarn && marginWarn && marginOverLimit:
				reason = fmt.Sprintf("portfolio drawdown warning: equity=%.1f%% (value=$%.2f, peak=$%.2f); perps margin=%.1f%% exceeds limit %.1f%% (unrealized loss=$%.2f, margin=$%.2f); portfolio latch governed by equity drawdown (limit %.1f%%); per-strategy circuit breakers own margin protection (#1448)",
					equityDD, totalValue, prs.PeakValue, marginDD, cfg.MaxDrawdownPct, perpsUnrealizedLoss, perpsMargin, cfg.MaxDrawdownPct)
			case equityWarn && marginWarn:
				// Both breached — surface both in the reason so a
				// correlated move is visible to the operator.
				reason = fmt.Sprintf("portfolio drawdown approaching kill switch limit %.1f%% (warn at %.1f%%): equity=%.1f%% (value=$%.2f, peak=$%.2f); perps margin=%.1f%% (unrealized loss=$%.2f, margin=$%.2f)",
					cfg.MaxDrawdownPct, warnDrawdownPct, equityDD, totalValue, prs.PeakValue, marginDD, perpsUnrealizedLoss, perpsMargin)
			case marginWarn && marginOverLimit:
				reason = fmt.Sprintf("portfolio perps margin drawdown %.1f%% exceeds limit %.1f%% (unrealized loss=$%.2f, margin=$%.2f); portfolio latch governed by equity drawdown %.1f%% (limit %.1f%%); per-strategy circuit breakers own margin protection (#1448)",
					marginDD, cfg.MaxDrawdownPct, perpsUnrealizedLoss, perpsMargin, equityDD, cfg.MaxDrawdownPct)
			case marginWarn:
				reason = fmt.Sprintf("portfolio perps margin drawdown %.1f%% approaching kill switch limit %.1f%% (warn at %.1f%%, unrealized loss=$%.2f, margin=$%.2f)",
					marginDD, cfg.MaxDrawdownPct, warnDrawdownPct, perpsUnrealizedLoss, perpsMargin)
			default:
				reason = fmt.Sprintf("portfolio drawdown %.1f%% approaching kill switch limit %.1f%% (warn at %.1f%%, value=$%.2f, peak=$%.2f)",
					equityDD, cfg.MaxDrawdownPct, warnDrawdownPct, totalValue, prs.PeakValue)
			}
		} else if equityAvailable {
			// Recovered below warning threshold — no active warning band.
			prs.WarningSent = false
			prs.WarnBandEnteredAt = time.Time{}
			prs.LastWarningEquityDDPct = 0
			prs.LastWarningMarginDDPct = 0
			prs.WarningEquityDeltaPct = 0
			prs.WarningMarginDeltaPct = 0
		}
	}

	// Check notional cap — entry hold only (#1344); does not force-close and
	// must not skip the strategy cycle (closes / SL/TP maintenance keep running).
	if cfg.MaxNotionalUSD > 0 && totalNotional > cfg.MaxNotionalUSD {
		return true, true, warning, notionalCapHoldDetail(totalNotional, cfg.MaxNotionalUSD)
	}

	return true, false, warning, reason
}

// portfolioWarnBandSignals reports which drawdown lenses sit in the portfolio
// warn band, reading the same persisted fields the kill-switch path just wrote.
//
// It is the SINGLE definition of warn-band membership: the warn-reason switch
// inside checkPortfolioRiskWithEquityAvailability and the #1449 warn-DM
// throttle in the main loop both call it, so a signal that "newly entered the
// band" means the same thing on both sides. equityAvailable must be the same
// value passed to the risk check for the cycle; the PeakValue > 0 guard
// mirrors the drawdown computation, which leaves CurrentDrawdownPct untouched
// (possibly holding a stale non-zero reading) when no peak exists.
func portfolioWarnBandSignals(cfg *PortfolioRiskConfig, prs *PortfolioRiskState, equityAvailable bool) (equityInBand, marginInBand bool) {
	if cfg == nil || prs == nil || cfg.MaxDrawdownPct <= 0 {
		return false, false
	}
	warnDrawdownPct := cfg.MaxDrawdownPct * cfg.WarnThresholdPct / 100
	equityInBand = equityAvailable && prs.PeakValue > 0 && prs.CurrentDrawdownPct > warnDrawdownPct
	marginInBand = prs.CurrentMarginDrawdownPct > warnDrawdownPct
	return equityInBand, marginInBand
}

// PortfolioNotional computes gross market exposure across all strategies.
// Spot: quantity * price. Options sold: strike * quantity (max obligation).
// Options bought: CurrentValueUSD if positive.
func PortfolioNotional(strategies map[string]*StrategyState, prices map[string]float64) float64 {
	total := 0.0
	for _, s := range strategies {
		for sym, pos := range s.Positions {
			price, ok := prices[sym]
			if !ok {
				price = pos.AvgCost
			}
			if pos.Multiplier > 0 {
				total += pos.Quantity * pos.Multiplier * price
			} else {
				total += pos.Quantity * price
			}
		}
		for _, opt := range s.OptionPositions {
			if opt.Action == "sell" {
				total += opt.Strike * opt.Quantity
			} else if opt.CurrentValueUSD > 0 {
				total += opt.CurrentValueUSD
			}
		}
	}
	return total
}

// RiskState tracks risk metrics for a strategy.
type RiskState struct {
	PeakValue           float64   `json:"peak_value"`
	MaxDrawdownPct      float64   `json:"max_drawdown_pct"`
	CurrentDrawdownPct  float64   `json:"current_drawdown_pct"`
	DailyPnL            float64   `json:"daily_pnl"`
	DailyPnLDate        string    `json:"daily_pnl_date"`
	ConsecutiveLosses   int       `json:"consecutive_losses"`
	CircuitBreaker      bool      `json:"circuit_breaker"`
	CircuitBreakerUntil time.Time `json:"circuit_breaker_until"`
	// PendingCircuitCloses holds venue-appropriate reduce-only / flatten close
	// requests queued by per-strategy circuit breakers, keyed by platform string.
	// The key MUST match StrategyConfig.Platform ("hyperliquid", "okx",
	// "topstep", "robinhood") — not the strategy-ID prefix (hl-/ts-/rh-/okx-)
	// and not an ad-hoc label — so the drain runners can correlate pending
	// entries with live strategies by platform. Use the PlatformPendingClose*
	// constants when setting or reading entries. Serialized to SQLite as
	// risk_pending_circuit_closes_json. Drained out-of-lock by platform-specific
	// runners (e.g. runPendingHyperliquidCircuitCloses for "hyperliquid").
	//
	// Generalized from the HL-specific PendingHyperliquidCircuitClose field in
	// #359 phase 1b. The per-platform drain code interprets the symbol/size
	// pairs according to its API; HL uses coin name + base-unit size, other
	// venues will use their own identifier conventions (phases 2-4).
	PendingCircuitCloses map[string]*PendingCircuitClose `json:"pending_circuit_closes,omitempty"`
}

// PlatformPendingCloseHyperliquid is the map key in RiskState.PendingCircuitCloses
// for Hyperliquid perps closes. Other platform constants land alongside their
// phase PRs (#360 OKX, #361 RH, #362 TS).
const PlatformPendingCloseHyperliquid = "hyperliquid"

// PlatformPendingCloseOKX is the map key in RiskState.PendingCircuitCloses for
// OKX perpetual swap reduce-only closes (#360 phase 2 of #357).
const PlatformPendingCloseOKX = "okx"

// PlatformPendingCloseRobinhood is the map key in RiskState.PendingCircuitCloses
// for Robinhood crypto closes (#361 phase 3). Robinhood crypto has no
// reduce-only primitive — the drain submits a full market_sell of the coin's
// on-account balance, gated on sole-ownership (only one live configured RH
// crypto strategy trading that coin on the account). Shared-coin setups
// cannot CB-close safely and are surfaced to the owner via DM instead.
const PlatformPendingCloseRobinhood = "robinhood"

// PlatformPendingCloseTopStep is the map key in RiskState.PendingCircuitCloses
// for TopStep futures closes. Size entries are integer contract counts encoded
// as float64 (PendingCircuitCloseSymbol.Size is float64 across all venues for
// storage uniformity; the TopStep drain logs the live on-account count at
// drain time — market_close has no size argument and flattens the full position).
const PlatformPendingCloseTopStep = "topstep"

// PlatformPendingCloseOKXSpot and PlatformPendingCloseRobinhoodOptions are map
// keys for per-strategy circuit-breaker closes the scheduler CANNOT auto-close
// safely (#363 phase 5, mirrors the portfolio-kill gaps from #345 / #346).
//
// OKX spot: no reduce-only semantic for asset balances; a net-close would wipe
// holdings that other strategies or the operator's manual positions rely on.
//
// Robinhood options: stock options close semantics (sell-to-close vs
// buy-to-close per leg, multi-leg spreads) are non-trivial to automate and the
// failure mode is high-risk.
//
// Pending entries under these keys carry OperatorRequired=true. The drain does
// NOT submit orders — it emits a CRITICAL warning once per cycle and leaves
// the pending intact until the operator intervenes (or the CB naturally
// resets). Deliberately distinct from "okx" / "robinhood" portfolio-kill keys
// so the auto-close drains never dequeue an operator-required entry.
const (
	PlatformPendingCloseOKXSpot          = "okx_spot"
	PlatformPendingCloseRobinhoodOptions = "robinhood_options"
)

// PendingCircuitClose is a queued request to close one or more positions on a
// single venue after a per-strategy circuit breaker fired. The drain runner
// for that venue (platform key in RiskState.PendingCircuitCloses) translates
// the symbol/size legs into venue-specific orders.
//
// When OperatorRequired is true the scheduler will not attempt an automated
// close — the venue lacks a safe reduce-only primitive or the close semantics
// are leg-aware enough that automation is unsafe (OKX spot, Robinhood options;
// #363). The drain emits a CRITICAL warning each cycle instead and leaves the
// pending populated so /status, Discord, and Telegram all surface the gap
// continuously until the operator clears it manually.
//
// ConsecutiveFailures and LastNotifiedAt track consecutive close-attempt
// failures (without any partial progress) for the throttled owner-DM alert
// added in #427. The drain increments ConsecutiveFailures on each hard error
// and resets it to 0 on any partial fill progress. The DM fires on the first
// failure, every 10th consecutive failure, or once per hour — whichever fires
// first. The counter is discarded together with the entry when the close
// fully succeeds.
type PendingCircuitClose struct {
	Symbols             []PendingCircuitCloseSymbol `json:"symbols"`
	OperatorRequired    bool                        `json:"operator_required,omitempty"`
	ConsecutiveFailures int                         `json:"consecutive_failures,omitempty"`
	LastNotifiedAt      time.Time                   `json:"last_notified_at,omitempty"`
}

// PendingCircuitCloseSymbol is one position leg of a pending close. Symbol is
// venue-specific (e.g. HL coin "ETH", OKX inst_id "BTC-USDT-SWAP", TS
// contract "ESM25"). Size is a positive magnitude; units are venue-specific
// (coin units for HL, contracts for TS, quote-currency amount for OKX).
type PendingCircuitCloseSymbol struct {
	Symbol string  `json:"symbol"`
	Size   float64 `json:"size"`
}

// PlatformRiskAssist carries pre-fetched venue state that
// setCircuitBreakerPending helpers need to size per-strategy on-chain closes
// when a CB fires. Nil fields disable pending enqueue for that platform; the
// drain runner's stuck-CB recovery path then re-enqueues once the fetch
// succeeds on a later cycle (#356).
//
// HL (#356), OKX (#360), Robinhood (#361), and TopStep (#362) fields are all
// populated today. RH fields are left unpopulated at the CheckRisk call site —
// see setRobinhoodCircuitBreakerPending for why the RH enqueue is driven
// exclusively by the drain's stuck-CB recovery path rather than at CB-fire time.
type PlatformRiskAssist struct {
	HLPositions  []HLPosition
	HLLiveAll    []StrategyConfig
	OKXPositions []OKXPosition
	OKXLiveAll   []StrategyConfig
	// RHPositions is reserved for a future main.go wiring that fetches live
	// Robinhood crypto balances once per cycle. It is intentionally left nil
	// at the CheckRisk call site today (see setRobinhoodCircuitBreakerPending
	// doc for rationale — fetching per cycle would cost a TOTP round-trip
	// even when no CB fires).
	RHPositions []RobinhoodPosition
	// RHLiveAll mirrors HLLiveAll/OKXLiveAll: every live configured Robinhood
	// crypto (Type=="spot") strategy. Left nil at the CheckRisk call site today
	// — see setRobinhoodCircuitBreakerPending.
	RHLiveAll []StrategyConfig
	// TSPositions is the pre-fetched live TopStep futures position snapshot
	// for the configured account. Populated in main.go from a once-per-cycle
	// fetch_topstep_positions.py call (#362). Empty slice with TSLiveAll set
	// is a successful fetch that found no open positions; nil slice signals
	// a fetch failure (stuck-CB path will retry).
	TSPositions []TopStepPosition
	// TSLiveAll mirrors HLLiveAll — every configured live TopStep futures
	// strategy on this scheduler. Needed by the sole-vs-shared-peer branch
	// in computeTopStepCircuitCloseQty.
	TSLiveAll []StrategyConfig
}

// MarshalPendingCircuitClosesJSON returns a DB-safe JSON blob for the pending
// field. A marshal error is logged loudly rather than silently swallowed: the
// map-of-struct payload is essentially unreachable for json.Marshal failures,
// but silently returning "" would persist a blank column that wipes pending
// closes on reload. Logging gives operators a chance to notice (#356 review).
func (r *RiskState) MarshalPendingCircuitClosesJSON() string {
	if r == nil || len(r.PendingCircuitCloses) == 0 {
		return ""
	}
	// Drop platforms whose pending payload has no legs — persisting
	// {"hyperliquid":{"symbols":[]}} is noise and makes reload ambiguous.
	filtered := make(map[string]*PendingCircuitClose, len(r.PendingCircuitCloses))
	for k, v := range r.PendingCircuitCloses {
		if v == nil || len(v.Symbols) == 0 {
			continue
		}
		filtered[k] = v
	}
	if len(filtered) == 0 {
		return ""
	}
	b, err := json.Marshal(filtered)
	if err != nil {
		fmt.Printf("[CRITICAL] MarshalPendingCircuitClosesJSON: refusing to persist pending circuit closes — json.Marshal failed: %v (pending=%+v)\n",
			err, filtered)
		return ""
	}
	return string(b)
}

// UnmarshalPendingCircuitClosesJSON restores PendingCircuitCloses from DB.
//
// Accepts two JSON shapes for backwards-compatibility with rows written by
// pre-#359 (#356) builds:
//
//  1. New map shape: {"hyperliquid":{"symbols":[{"symbol":"ETH","size":0.1}]}}
//  2. Legacy HL-only shape: {"coins":[{"coin":"ETH","sz":0.1}]} — transparently
//     converted to {"hyperliquid":{"symbols":[...]}} on first load. Subsequent
//     saves write the new shape, so the DB self-heals within one cycle.
func (r *RiskState) UnmarshalPendingCircuitClosesJSON(raw string) {
	if r == nil {
		return
	}
	if raw == "" {
		r.PendingCircuitCloses = nil
		return
	}

	// Try new map shape first.
	var asMap map[string]*PendingCircuitClose
	if err := json.Unmarshal([]byte(raw), &asMap); err == nil {
		filtered := make(map[string]*PendingCircuitClose, len(asMap))
		for k, v := range asMap {
			if v == nil || len(v.Symbols) == 0 {
				continue
			}
			filtered[k] = v
		}
		if len(filtered) > 0 {
			r.PendingCircuitCloses = filtered
			return
		}
	}

	// Legacy shape fallback: {"coins":[{"coin":"ETH","sz":0.1}]} from #356.
	// json.Unmarshal into map[string]*PendingCircuitClose errors out on the
	// legacy payload (the "coins" value is an array, which cannot decode into
	// a *PendingCircuitClose), so the new-shape attempt above returns non-nil
	// err and execution falls through here.
	var legacy struct {
		Coins []struct {
			Coin string  `json:"coin"`
			Sz   float64 `json:"sz"`
		} `json:"coins"`
	}
	if err := json.Unmarshal([]byte(raw), &legacy); err != nil || len(legacy.Coins) == 0 {
		r.PendingCircuitCloses = nil
		return
	}
	symbols := make([]PendingCircuitCloseSymbol, 0, len(legacy.Coins))
	for _, c := range legacy.Coins {
		symbols = append(symbols, PendingCircuitCloseSymbol{Symbol: c.Coin, Size: c.Sz})
	}
	r.PendingCircuitCloses = map[string]*PendingCircuitClose{
		PlatformPendingCloseHyperliquid: {Symbols: symbols},
	}
}

// setPendingCircuitClose stores a pending close for the given platform,
// creating the map on first use. Passing nil or an empty-symbols close deletes
// the platform entry instead of storing an empty shell.
func (r *RiskState) setPendingCircuitClose(platform string, pending *PendingCircuitClose) {
	if r == nil {
		return
	}
	if pending == nil || len(pending.Symbols) == 0 {
		delete(r.PendingCircuitCloses, platform)
		if len(r.PendingCircuitCloses) == 0 {
			r.PendingCircuitCloses = nil
		}
		return
	}
	if r.PendingCircuitCloses == nil {
		r.PendingCircuitCloses = make(map[string]*PendingCircuitClose)
	}
	r.PendingCircuitCloses[platform] = pending
}

// clearPendingCircuitClose removes the pending entry for a platform, if any.
func (r *RiskState) clearPendingCircuitClose(platform string) {
	if r == nil {
		return
	}
	delete(r.PendingCircuitCloses, platform)
	if len(r.PendingCircuitCloses) == 0 {
		r.PendingCircuitCloses = nil
	}
}

// getPendingCircuitClose returns the pending entry for a platform, or nil if
// none is queued.
func (r *RiskState) getPendingCircuitClose(platform string) *PendingCircuitClose {
	if r == nil {
		return nil
	}
	return r.PendingCircuitCloses[platform]
}

// setTopStepCircuitBreakerPending enqueues a reduce-only flatten request for
// the firing strategy's TopStep futures contract (#362). Sole-peer strategies
// enqueue the full on-account contract count; multi-peer shared contracts are
// skipped because TopStepX's market_close only flattens the entire on-account
// size — no safe partial-close primitive exists for whole-contract futures.
// The operator is notified via the virtual force-close (CheckRisk still calls
// forceCloseAllPositions), and manual intervention is required to split a
// shared contract.
//
// A nil or empty assist bails — same stuck-CB semantics as the HL helper:
// a fetch failure at CB fire time leaves pending nil, and the drain's
// stuck-CB recovery phase reconstructs the pending once TS is reachable.
func setTopStepCircuitBreakerPending(sc *StrategyConfig, s *StrategyState, assist *PlatformRiskAssist) {
	if sc == nil || assist == nil || len(assist.TSPositions) == 0 {
		return
	}
	if sc.Platform != "topstep" || sc.Type != "futures" || !topstepIsLive(sc.Args) {
		return
	}
	sym := topstepSymbol(sc.Args)
	if sym == "" {
		return
	}
	if _, ok := s.Positions[sym]; !ok {
		return
	}
	qty, ok := computeTopStepCircuitCloseQty(sym, s.ID, assist.TSPositions, assist.TSLiveAll)
	if !ok || qty <= 0 {
		return
	}
	s.RiskState.setPendingCircuitClose(PlatformPendingCloseTopStep, &PendingCircuitClose{
		Symbols: []PendingCircuitCloseSymbol{{Symbol: sym, Size: float64(qty)}},
	})
}

func setHyperliquidCircuitBreakerPending(sc *StrategyConfig, s *StrategyState, assist *PlatformRiskAssist) {
	if sc == nil || assist == nil || len(assist.HLPositions) == 0 {
		return
	}
	if sc.Platform != "hyperliquid" || sc.Type != "perps" || !hyperliquidIsLive(sc.Args) {
		return
	}
	sym := hyperliquidSymbol(sc.Args)
	if sym == "" {
		return
	}
	if hyperliquidCircuitBreakerHasSharedCoin(sc, assist) {
		return
	}
	if _, ok := s.Positions[sym]; !ok {
		return
	}
	qty, ok := computeHyperliquidCircuitCloseQty(sym, s.ID, assist.HLPositions, assist.HLLiveAll)
	if !ok || qty <= 0 {
		return
	}
	symbols := []PendingCircuitCloseSymbol{{Symbol: sym, Size: qty}}
	// #1159: a circuit breaker that flattens the primary but leaves the hedge
	// running converts a market-neutral pair into a naked INVERSE directional
	// position — the exact opposite of the strategy's thesis — at the precise
	// moment the auto-protective machinery decided the strategy should stop
	// trading. The hedge leg goes with it. Sole ownership (validateHedgeConfigs)
	// makes the peers>1 guard inside computeHyperliquidCircuitCloseQty pass
	// vacuously for the hedge coin.
	if hCoin := heldHedgeCoin(*sc, s); hCoin != "" {
		if hQty, hok := computeHyperliquidCircuitCloseQty(hCoin, s.ID, assist.HLPositions, assist.HLLiveAll); hok && hQty > 0 {
			symbols = append(symbols, PendingCircuitCloseSymbol{Symbol: hCoin, Size: hQty})
		}
	}
	s.RiskState.setPendingCircuitClose(PlatformPendingCloseHyperliquid, &PendingCircuitClose{
		Symbols: symbols,
	})
}

func hyperliquidCircuitBreakerHasSharedCoin(sc *StrategyConfig, assist *PlatformRiskAssist) bool {
	if sc == nil || assist == nil || sc.Platform != "hyperliquid" || sc.Type != "perps" || !hyperliquidIsLive(sc.Args) {
		return false
	}
	sym := hyperliquidSymbol(sc.Args)
	if sym == "" {
		return false
	}
	return len(hlLiveStrategiesForCoin(sym, assist.HLLiveAll)) > 1
}

func shouldForceCloseAllPositionsOnCircuitBreaker(sc *StrategyConfig, assist *PlatformRiskAssist) bool {
	return !hyperliquidCircuitBreakerHasSharedCoin(sc, assist)
}

// setOperatorRequiredCircuitBreakerPending enqueues an OperatorRequired=true
// pending close for OKX spot and Robinhood options strategies, the two live
// venues the scheduler has no safe auto-close path for (#345 / #346 / #363).
//
// Unlike setHyperliquidCircuitBreakerPending, this helper does not size the
// close — no subprocess round-trip is ever attempted, so a notional size is
// unnecessary. Size is set to the strategy's virtual position quantity (or 0
// when no virtual position exists, e.g. options strategies whose positions
// live in OptionPositions rather than Positions) purely for operator-facing
// context in the warning message.
//
// No-op when the strategy is not live, or when the strategy is not one of the
// two covered operator-gap configurations (call sites can invoke it broadly;
// the guard keeps it cheap).
func setOperatorRequiredCircuitBreakerPending(sc *StrategyConfig, s *StrategyState) {
	if sc == nil || s == nil {
		return
	}
	switch {
	case sc.Platform == "okx" && sc.Type == "spot" && okxIsLive(sc.Args):
		sym := okxSymbol(sc.Args)
		if sym == "" {
			return
		}
		var size float64
		if pos, ok := s.Positions[sym]; ok {
			size = pos.Quantity
		}
		s.RiskState.setPendingCircuitClose(PlatformPendingCloseOKXSpot, &PendingCircuitClose{
			Symbols:          []PendingCircuitCloseSymbol{{Symbol: sym, Size: size}},
			OperatorRequired: true,
		})
	case sc.Platform == "robinhood" && sc.Type == "options" && robinhoodIsLive(sc.Args):
		// Options positions live in s.OptionPositions keyed by option ID, not
		// a single underlier. Collect every open leg's ID so the operator sees
		// exactly which positions need manual close (not just the underlier).
		symbols := make([]PendingCircuitCloseSymbol, 0, len(s.OptionPositions))
		for id, op := range s.OptionPositions {
			if op == nil {
				continue
			}
			symbols = append(symbols, PendingCircuitCloseSymbol{Symbol: id, Size: op.Quantity})
		}
		if len(symbols) == 0 {
			// No open option legs — emit a single marker entry with the
			// underlier so the operator still sees the strategy-level CB fire
			// on /status and in notifications.
			sym := robinhoodSymbol(sc.Args)
			if sym == "" {
				return
			}
			symbols = append(symbols, PendingCircuitCloseSymbol{Symbol: sym, Size: 0})
		}
		sort.Slice(symbols, func(i, j int) bool { return symbols[i].Symbol < symbols[j].Symbol })
		s.RiskState.setPendingCircuitClose(PlatformPendingCloseRobinhoodOptions, &PendingCircuitClose{
			Symbols:          symbols,
			OperatorRequired: true,
		})
	}
}

// setRobinhoodCircuitBreakerPending enqueues a pending full-close for a live
// Robinhood crypto strategy whose per-strategy circuit breaker fired (#361
// phase 3). Robinhood crypto has no reduce-only primitive: market_sell
// consumes the entire on-account balance for the coin. We still enqueue
// unconditionally when an on-account position exists — the sole-ownership
// gate lives in the drain (runPendingRobinhoodCircuitCloses) so that shared-
// coin setups DM the owner exactly once per fire cycle rather than silently
// stalling forever.
//
// Wiring note (important): under the current main.go wiring, `assist` is
// built from HL and OKX pre-fetches only — `assist.RHPositions` is always
// nil when CheckRisk calls this setter (see scheduler/main.go where the
// riskAssist literal sets HLPositions/HLLiveAll/OKXPositions/OKXLiveAll but
// leaves RH fields unset). This function therefore no-ops on the CB-fire
// cycle itself and relies on the drain's stuck-CB recovery path
// (runPendingRobinhoodCircuitCloses) to reconstruct the pending leg on the
// next cycle once the drain's lazy RH positions fetch succeeds. The trade-
// off is deliberate: wiring RH into CheckRisk would require a live TOTP
// round-trip every cycle (including cycles where no RH CB fires), which is
// the exact cost we are avoiding. Do not "fix" this by populating
// assist.RHPositions at the CheckRisk call site without revisiting the
// lazy-fetch design, or every cycle will pay a TOTP round-trip for an RH
// CB that fires maybe once per month.
//
// No-op also when assist is nil (defensive — same code path as the design
// above, mirroring the HL pattern).
func setRobinhoodCircuitBreakerPending(sc *StrategyConfig, s *StrategyState, assist *PlatformRiskAssist) {
	if sc == nil || assist == nil || len(assist.RHPositions) == 0 {
		return
	}
	if sc.Platform != "robinhood" || sc.Type != "spot" || !robinhoodIsLive(sc.Args) {
		return
	}
	coin := robinhoodSymbol(sc.Args)
	if coin == "" {
		return
	}
	if _, ok := s.Positions[coin]; !ok {
		return
	}
	qty := robinhoodOnAccountSize(coin, assist.RHPositions)
	if qty <= 0 {
		return
	}
	s.RiskState.setPendingCircuitClose(PlatformPendingCloseRobinhood, &PendingCircuitClose{
		Symbols: []PendingCircuitCloseSymbol{{Symbol: coin, Size: qty}},
	})
}

// robinhoodOnAccountSize returns the unsigned on-account size of a coin,
// or 0 if not found. Robinhood crypto is spot so Size is always >= 0.
func robinhoodOnAccountSize(coin string, positions []RobinhoodPosition) float64 {
	for i := range positions {
		if positions[i].Coin == coin {
			if positions[i].Size > 0 {
				return positions[i].Size
			}
			return 0
		}
	}
	return 0
}

// setOKXCircuitBreakerPending mirrors setHyperliquidCircuitBreakerPending for
// OKX perps (#360 phase 2 of #357). Bails on any nil dependency or missing
// fetched assist so the stuck-CB recovery path in runPendingOKXCircuitCloses
// can reconstruct the pending on a later cycle once OKX is reachable again.
func setOKXCircuitBreakerPending(sc *StrategyConfig, s *StrategyState, assist *PlatformRiskAssist) {
	if sc == nil || assist == nil || len(assist.OKXPositions) == 0 {
		return
	}
	if sc.Platform != "okx" || sc.Type != "perps" || !okxIsLive(sc.Args) {
		return
	}
	sym := okxSymbol(sc.Args)
	if sym == "" {
		return
	}
	if _, ok := s.Positions[sym]; !ok {
		return
	}
	qty, ok := computeOKXCircuitCloseQty(sym, s.ID, assist.OKXPositions, assist.OKXLiveAll)
	if !ok || qty <= 0 {
		return
	}
	s.RiskState.setPendingCircuitClose(PlatformPendingCloseOKX, &PendingCircuitClose{
		Symbols: []PendingCircuitCloseSymbol{{Symbol: sym, Size: qty}},
	})
}

// rolloverDailyPnL resets DailyPnL to zero whenever the UTC date has advanced
// past DailyPnLDate. Calling this at both risk-check time and trade-record time
// ensures the reset is applied regardless of which code path runs first after
// midnight — fixing issue #27 where a skipped or late risk check could cause
// trades to be counted against the wrong day.
func rolloverDailyPnL(r *RiskState) {
	today := time.Now().UTC().Format("2006-01-02")
	if r.DailyPnLDate != today {
		r.DailyPnL = 0
		r.DailyPnLDate = today
	}
}

// forceCloseKillSwitchPositions clears virtual positions after a confirmed
// portfolio kill-switch close. `hlFills` carries the realized Hyperliquid
// close fills (price/size/fee) so HL strategies record accurate Trade and
// ClosedPosition rows; `hlVirtualQty` is the pre-close peer snapshot used to
// split shared-coin fills by virtual quantity. Pass nil for non-HL or when no
// fill data is available.
func forceCloseKillSwitchPositions(s *StrategyState, sc StrategyConfig, prices map[string]float64, hlFills map[string]HyperliquidCloseFill, hlLiveAll []StrategyConfig, hlVirtualQty hlVirtualQuantitySnapshot, logger *StrategyLogger) {
	// Live HL portfolio-kill closes can carry real exchange fills. Apply them
	// first so Trade and ClosedPosition rows use realized fill price/fee; the
	// generic pass below remains the cleanup path for non-HL strategies,
	// missing-fill fallbacks, options, and any residual virtual positions.
	applyHyperliquidKillSwitchCloseFill(s, sc, hlFills, hlLiveAll, hlVirtualQty)
	// #1159: the hedge leg lives on its own coin with its own fill in the
	// report. Book it before the generic sweep so it records the real fill
	// price/fee rather than the model-only reconciliation adjustment
	// forceCloseAllPositions would otherwise write.
	applyHyperliquidKillSwitchHedgeFill(s, sc, hlFills)
	forceCloseAllPositions(s, prices, logger)
}

// classifyPositionTradeType maps a position to the correct trade_type label
// for circuit-breaker / kill-switch close records. HL perps and OKX perps
// carry pos.Multiplier=1 (#254/#497 perps PnL valuation convention — NOT a
// contract multiplier), so the legacy "Multiplier>0 → futures" classifier
// mislabels every perps force-close as "futures". This is an operator-facing
// label fix only: tradeLedgerDeltaSQL (trade_pnl.go) keys on
// is_close/pnl_gross/realized_pnl/exchange_fee and never reads trade_type, so
// the label does NOT affect any ledger sum — relabeling here changes what an
// operator sees (Discord/leaderboard/audit), not the #954 ledger math.
// TopStep/CME futures keep pos.Multiplier as the real contract multiplier;
// that is the only branch where "futures" is correct.
func classifyPositionTradeType(s *StrategyState, pos *Position) string {
	if pos == nil {
		return "spot"
	}
	// #1159: a hedge leg keeps its own label on EVERY leg, including the
	// circuit-breaker / kill-switch virtual sweep. tradeStatsExcludedTypesSQL
	// filters lifetime #T / W-L on this value, so a force-closed hedge
	// mislabeled "perps" would count as a real round trip.
	if pos.isHedgeLeg() {
		return hedgeTradeType
	}
	if pos.Multiplier > 0 {
		if s != nil {
			switch {
			case s.Platform == "hyperliquid" && (s.Type == "perps" || s.Type == "manual"):
				return "perps"
			case s.Platform == "okx" && s.Type == "perps":
				return "perps"
			}
		}
		return "futures"
	}
	return "spot"
}

// forceCloseAllPositions liquidates all open positions at current prices.
// Called when any circuit breaker fires.
func forceCloseAllPositions(s *StrategyState, prices map[string]float64, logger *StrategyLogger) {
	now := time.Now().UTC()

	for symbol, pos := range s.Positions {
		price, ok := prices[symbol]
		if !ok {
			price = pos.AvgCost
		}
		var pnl, value float64
		// PnL branch is the same for perps (Multiplier=1) and futures
		// (Multiplier=contract size) — qty*multiplier*price_delta. Only the
		// trade_type LABEL differs by venue, classified via
		// classifyPositionTradeType so perps force-closes carry an accurate
		// operator-facing label. The label does not feed any ledger sum
		// (tradeLedgerDeltaSQL ignores trade_type); it is display-only.
		tradeType := classifyPositionTradeType(s, pos)
		reason := "circuit_breaker"
		details := ""
		// #1009: a force-close must never book PnL off a structurally-corrupt
		// position. A non-positive quantity (the negative residual a mis-sized
		// direction reversal used to leave) or a non-positive avg cost (a zeroed
		// entry that books the full notional as PnL — the ~4884x overstatement
		// folded in from PR #1008) makes qty*(price-avgCost) meaningless. Clear
		// it with a zero-PnL leg and leave cash untouched so the booked
		// realized_pnl reconciles with the closed_positions row.
		if closePositionIsCorrupt(pos) {
			reason = "circuit_breaker_corrupt"
			details = fmt.Sprintf("Circuit breaker close %s (corrupt qty=%.6f avg_cost=%.4f) — zero PnL booked", pos.Side, pos.Quantity, pos.AvgCost)
			if logger != nil {
				logger.Warn("Circuit breaker: corrupt %s position %s (qty=%.6f avg_cost=%.4f) — booking zero realized PnL, not qty*(price-avgCost)", pos.Side, symbol, pos.Quantity, pos.AvgCost)
			}
		} else if pos.Multiplier > 0 {
			// Futures/perps: PnL-based (contracts * multiplier * price delta)
			if pos.Side == "long" {
				pnl = pos.Quantity * pos.Multiplier * (price - pos.AvgCost)
			} else {
				pnl = pos.Quantity * pos.Multiplier * (pos.AvgCost - price)
			}
			s.Cash += pnl
			value = pos.Quantity * pos.Multiplier * price
		} else if pos.Side == "long" {
			proceeds := pos.Quantity * price
			pnl = proceeds - pos.Quantity*pos.AvgCost
			s.Cash += proceeds
			value = proceeds
		} else {
			pnl = pos.Quantity * (pos.AvgCost - price)
			s.Cash += pos.Quantity*pos.AvgCost - pos.Quantity*price
			value = pos.Quantity * price
		}
		if details == "" {
			details = fmt.Sprintf("Circuit breaker close %s, PnL: $%.2f (model-only reconciliation adjustment; no exchange fill)", pos.Side, pnl)
		}
		if logger != nil {
			logger.Warn("Circuit breaker: force-closing %s %s @ $%.2f (PnL: $%.2f)", pos.Side, symbol, price, pnl)
		}
		positionID := ensurePositionTradeID(s.ID, symbol, pos)
		trade := Trade{
			Timestamp:         now,
			StrategyID:        s.ID,
			Symbol:            symbol,
			PositionID:        positionID,
			Side:              closeTradeSide(pos.Side),
			Quantity:          absQty(pos.Quantity),
			Price:             price,
			Value:             value,
			TradeType:         tradeType,
			Details:           details,
			IsClose:           true,
			RealizedPnL:       pnl,
			PnLGross:          true, // model-only adjustment has no exchange fee: gross == net
			FeeSource:         FeeSourceReconcileAdjustment,
			Regime:            s.Regime,
			EntryATR:          pos.EntryATR,
			StopLossTriggerPx: pos.StopLossTriggerPx,
			StopLossATRMult:   pos.StopLossATRMult,
			TPTiersJSON:       pos.TPTiersJSON,
		}
		RecordTrade(s, trade)
		// #1159: a hedge leg's PnL never feeds the loss streak.
		recordPositionTradeResult(s, pos, pnl)
		recordClosedPosition(s, pos, price, pnl, reason, now)
		delete(s.Positions, symbol)
		clearATRMultMissingEntryATRWarningOnHLPerpsClose(s, symbol)
	}

	for id, pos := range s.OptionPositions {
		var pnl, closePrice float64
		if pos.Action == "buy" {
			pnl = pos.CurrentValueUSD - pos.EntryPremiumUSD
			s.Cash += pos.CurrentValueUSD
			closePrice = pos.CurrentValueUSD
		} else {
			buybackCost := -pos.CurrentValueUSD
			pnl = pos.EntryPremiumUSD - buybackCost
			s.Cash -= buybackCost
			closePrice = buybackCost
		}
		if logger != nil {
			logger.Warn("Circuit breaker: force-closing %s %s @ $%.2f (PnL: $%.2f)", pos.Action, id, closePrice, pnl)
		}
		positionID := ensureOptionTradeID(s.ID, pos)
		trade := Trade{
			Timestamp:   now,
			StrategyID:  s.ID,
			Symbol:      id,
			PositionID:  positionID,
			Side:        optionCloseTradeSide(pos.Action),
			Quantity:    pos.Quantity,
			Price:       closePrice,
			Value:       closePrice,
			TradeType:   "options",
			Details:     fmt.Sprintf("Circuit breaker force-close, PnL: $%.2f", pnl),
			IsClose:     true,
			RealizedPnL: pnl,
			PnLGross:    true, // model-only adjustment has no exchange fee: gross == net
			FeeSource:   FeeSourceReconcileAdjustment,
			Regime:      s.Regime,
		}
		RecordTrade(s, trade)
		RecordTradeResult(&s.RiskState, pnl)
		recordClosedOptionPosition(s, pos, closePrice, pnl, "circuit_breaker", now)
		delete(s.OptionPositions, id)
	}
}

// perpsMarginDrawdownInputs iterates open perps positions and returns the sum
// of unrealized losses (positive number; gains clamp to zero) and the sum of
// deployed margin (notional / leverage). These are the numerator and
// denominator of the perps-specific drawdown ratio introduced in #292.
//
// configLeverage is the strategy-config exchange leverage (sc.Leverage), not
// sc.SizingLeverage and not pos.Leverage. This lets operators size small
// positions with sizing_leverage while calculating margin drawdown against the
// leverage actually configured at the exchange (#497).
//
// Positions are filtered by Multiplier > 0 (perps marker). The outer
// s.Type == "perps" check at the call site is the primary guard. configLeverage
// must be > 0 — when zero, the function returns (0, 0) and the caller falls
// back to peak-relative drawdown.
//
// The unrealized-loss numerator (rather than peakValue - portfolioValue) keeps
// the drawdown ratio referenced to the currently-open position: prior realized
// losses that already live in Cash below the high-water mark do NOT inflate
// drawdown against a fresh small position's margin. See #292 code review.
//
// Mark price falls back to AvgCost when missing or non-positive so numerator
// and denominator share the same basis as PortfolioValue's valuation.
//
// Returns (0, 0) when no perps positions are open; the caller uses a zero
// margin as the signal to fall back to peak-relative drawdown.
func perpsMarginDrawdownInputs(s *StrategyState, configLeverage float64, prices map[string]float64) (unrealizedLoss, margin float64) {
	if configLeverage <= 0 {
		return 0, 0
	}
	for sym, pos := range s.Positions {
		if pos.Multiplier <= 0 {
			continue
		}
		price, ok := prices[sym]
		if !ok || price <= 0 {
			price = pos.AvgCost
		}
		if price <= 0 {
			continue
		}
		notional := pos.Quantity * price
		if notional <= 0 {
			continue
		}
		margin += notional / configLeverage

		var pnl float64
		if pos.Side == "long" {
			pnl = pos.Quantity * pos.Multiplier * (price - pos.AvgCost)
		} else {
			pnl = pos.Quantity * pos.Multiplier * (pos.AvgCost - price)
		}
		if pnl < 0 {
			unrealizedLoss += -pnl
		}
	}
	return unrealizedLoss, margin
}

// Shared reason-string prefixes for CheckRisk return values. Consumers
// (main.go notification dispatch, circuit_breaker_alert.go classification,
// tests) must reference these constants rather than re-typing literals so
// reason-string tweaks stay safe under refactor.
//
// RiskReasonConsecutiveLosses is deliberately threshold-free (#1273): the
// loss-streak threshold is per-strategy-configurable, so the fire site appends
// the actual count/threshold after the prefix and every consumer matches with
// strings.HasPrefix — a non-default threshold can never break classification.
const (
	RiskReasonCircuitBreakerActive = "circuit breaker active"
	RiskReasonMaxDrawdownExceeded  = "max drawdown exceeded"
	RiskReasonConsecutiveLosses    = "consecutive losses"
)

// circuitBreakerPermitsManagement reports whether a CheckRisk block should still
// run existing-position management (trailing SL ratchet, TP ratchet, protection
// sync) for an open position instead of skipping the strategy outright. A
// per-strategy circuit breaker exists to block NEW entries; it must not freeze
// the stop-loss on a position that is already open — e.g. a shared-coin residual
// the CB cannot force-close (shouldForceCloseAllPositionsOnCircuitBreaker is
// false when the coin is shared), which then sits with a stale trailing SL for
// the whole latch window and fails to lock in favorable movement (#1046).
//
// Scoped to the latched reason and to HL perps: only that path runs the
// trailing-SL/TP-ratchet walker. Manual strategies are exempt from CheckRisk
// entirely (returns allowed early), and other platforms/types have no equivalent
// in-loop SL ratchet, so they keep the plain skip. The first-fire cycle (reason
// "max drawdown exceeded" / "consecutive losses") is deliberately excluded:
// that is the cycle that force-closes / enqueues the reduce-only drain, so the
// position state is mid-transition; management resumes on the next (latched)
// cycle, which is ~the entire latch window.
func circuitBreakerPermitsManagement(reason, platform, stratType string, posQty float64) bool {
	return reason == RiskReasonCircuitBreakerActive &&
		platform == "hyperliquid" && stratType == "perps" && posQty > 0
}

// CheckRisk evaluates risk state and returns whether trading is allowed.
// sc is the strategy config for this state (nil in some tests — platform
// pending logic is skipped). assist carries pre-fetched per-platform state
// (HL clearinghouse positions today; OKX/TS/RH in later phases) so live
// strategies can enqueue on-chain closes on circuit breaker (#356 / #359).
func CheckRisk(sc *StrategyConfig, s *StrategyState, portfolioValue float64, prices map[string]float64, logger *StrategyLogger, assist *PlatformRiskAssist) (bool, string) {
	// #574: manual strategies are operator-controlled and start with capital=0
	// funded ad-hoc, so peak-relative drawdown is meaningless.
	if sc != nil && sc.Type == "manual" {
		return true, ""
	}
	r := &s.RiskState
	now := time.Now().UTC()

	rolloverDailyPnL(r)

	// Check circuit breaker
	if r.CircuitBreaker {
		if now.Before(r.CircuitBreakerUntil) {
			return false, RiskReasonCircuitBreakerActive
		}
		r.CircuitBreaker = false
		r.ConsecutiveLosses = 0
	}

	// #1048: per-strategy circuit-breaker opt-out. When explicitly disabled, both
	// firing arms below are suppressed so the strategy never latches a NEW circuit
	// break. The gate sits BELOW the latch check and the drawdown computation on
	// purpose: an already-latched CB still blocks/drains (the latch check above is
	// ungated), and CurrentDrawdownPct/peak still update for the status UI. Manual
	// is already exempt via the early return at the top of CheckRisk.
	cbEnabled := sc.CircuitBreakerEnabled()
	if cbEnabled {
		// Clear any sticky suppression-warning throttle eagerly: while the
		// breaker is enabled the warning never applies, and an enabled+breached
		// cycle fires (returning before recordCircuitBreakerSuppression below),
		// so this is the only place a re-enable reliably resets the throttle —
		// ensuring a later re-disable warns afresh. (#1048)
		circuitBreakerSuppressedWarned.Delete(s.ID)
	}

	poolBudget := sc != nil && usesSharedWalletPoolBudget(*sc)

	// A pooled strategy has no per-strategy equity baseline: its cash book is
	// an attribution ledger, while solvency belongs to the account-level
	// portfolio risk check. Do not manufacture a peak from that ledger.
	if !poolBudget && portfolioValue > r.PeakValue {
		r.PeakValue = portfolioValue
	}

	// Check drawdown.
	//
	// For perps strategies with open leveraged positions, drawdown is measured
	// as unrealized loss on currently-open positions divided by deployed margin
	// (capital at risk). A 20x leveraged position only puts ~5% of notional at
	// risk as margin; using the full portfolio as denominator with peak-relative
	// numerator under-states near-100% margin losses as a few-percent drawdown,
	// so the circuit breaker would only fire after the position had already been
	// liquidated. See #292.
	//
	// Referencing the numerator to unrealized PnL on *currently-open* positions
	// (rather than peak - portfolioValue, which is cumulative from the
	// high-water mark) keeps prior realized losses from inflating drawdown
	// against a freshly opened position's margin. A strategy that has taken
	// past losses but just opened a small untouched position should not fire.
	//
	// When the strategy has no perps margin deployed (all positions closed,
	// leverage unset, or non-perps type), we fall back to the classic
	// peak-relative drawdown so strategies without leverage behave identically
	// to before.
	loss := 0.0
	denom := 0.0
	denomLabel := "peak"
	if s.Type == "perps" {
		var configLev float64
		if sc != nil {
			configLev = sc.Leverage
		}
		if pnlLoss, margin := perpsMarginDrawdownInputs(s, configLev, prices); margin > 0 {
			loss = pnlLoss
			denom = margin
			denomLabel = "margin"
		}
	}
	if denom <= 0 && !poolBudget && r.PeakValue > 0 {
		loss = r.PeakValue - portfolioValue
		denom = r.PeakValue
	}
	if denom > 0 {
		if loss < 0 {
			loss = 0
		}
		r.CurrentDrawdownPct = (loss / denom) * 100
		if r.CurrentDrawdownPct > r.MaxDrawdownPct && cbEnabled {
			r.CircuitBreaker = true
			r.CircuitBreakerUntil = now.Add(sc.CircuitBreakerDrawdownCooldown())
			setHyperliquidCircuitBreakerPending(sc, s, assist)
			setOKXCircuitBreakerPending(sc, s, assist)
			setRobinhoodCircuitBreakerPending(sc, s, assist)
			setTopStepCircuitBreakerPending(sc, s, assist)
			setOperatorRequiredCircuitBreakerPending(sc, s)
			if shouldForceCloseAllPositionsOnCircuitBreaker(sc, assist) {
				forceCloseAllPositions(s, prices, logger)
			}
			return false, fmt.Sprintf("%s (%.1f%% > %.1f%%, portfolio=$%.2f peak=$%.2f, denom=%s=$%.2f)",
				RiskReasonMaxDrawdownExceeded, r.CurrentDrawdownPct, r.MaxDrawdownPct, portfolioValue, r.PeakValue, denomLabel, denom)
		}
	} else {
		r.CurrentDrawdownPct = 0
	}

	// Consecutive losses circuit breaker (default 5 in a row → pause 1h, close
	// positions; threshold and cooldown per-strategy-tunable, #1273). The
	// reason string keeps RiskReasonConsecutiveLosses as its prefix — every
	// classifier matches on that prefix, so the appended count/threshold is
	// operator display only.
	lossStreakThreshold := sc.CircuitBreakerLossStreakThreshold()
	if r.ConsecutiveLosses >= lossStreakThreshold && cbEnabled {
		r.CircuitBreaker = true
		r.CircuitBreakerUntil = now.Add(sc.CircuitBreakerLossStreakCooldown())
		setHyperliquidCircuitBreakerPending(sc, s, assist)
		setOKXCircuitBreakerPending(sc, s, assist)
		setRobinhoodCircuitBreakerPending(sc, s, assist)
		setTopStepCircuitBreakerPending(sc, s, assist)
		setOperatorRequiredCircuitBreakerPending(sc, s)
		if shouldForceCloseAllPositionsOnCircuitBreaker(sc, assist) {
			forceCloseAllPositions(s, prices, logger)
		}
		return false, fmt.Sprintf("%s (%d in a row, threshold %d)", RiskReasonConsecutiveLosses, r.ConsecutiveLosses, lossStreakThreshold)
	}

	// #1048: if the circuit breaker is disabled and a halt threshold was just
	// crossed, the two arms above fell through silently. Leave a runtime trace
	// (a WARNING, not a halt) so the missing auto-protection is observable in
	// logs at the cycle it matters — not only at startup / on-demand inspect.
	recordCircuitBreakerSuppression(s, cbEnabled, lossStreakThreshold, logger)

	return true, ""
}

// circuitBreakerSuppressedWarned throttles the "circuit breaker disabled but a
// halt threshold was crossed" warning to once per strategy per suppression
// episode. The key is cleared by recordCircuitBreakerSuppression when the
// breaker is re-enabled or the breach clears, so a fresh crossing — or a later
// re-disable — warns again. (#1048)
var circuitBreakerSuppressedWarned sync.Map

// recordCircuitBreakerSuppression emits a one-shot WARNING when a strategy with
// the circuit breaker explicitly disabled (circuit_breaker:false) crosses a
// halt threshold that WOULD have fired. It makes the absence of the
// auto-protective halt observable at the cycle it matters, not only via the
// startup summary / inspect surfaces. It is a warning, never a halt: nothing is
// closed and trading continues. The notice is deduped to once per suppression
// episode and cleared when the breaker is re-enabled or all thresholds clear —
// so a later genuine fire (once re-enabled) still alerts through the normal
// circuit-breaker path, and a subsequent re-disable warns afresh. (#1048)
// lossStreakThreshold is the caller's resolved CircuitBreakerLossStreakThreshold()
// so the warning fires at exactly the same streak length as the firing arm,
// including per-strategy overrides (#1273).
func recordCircuitBreakerSuppression(s *StrategyState, cbEnabled bool, lossStreakThreshold int, logger *StrategyLogger) {
	if s == nil {
		return
	}
	r := &s.RiskState
	// Mirror the drawdown arm's condition exactly (risk.go ~1470) so the warning
	// stays in sync if that gate is later edited — the PeakValue>0 guard is
	// implicit there (CurrentDrawdownPct is 0 when PeakValue is 0). (#1048)
	drawdownBreached := r.CurrentDrawdownPct > r.MaxDrawdownPct
	lossBreached := r.ConsecutiveLosses >= lossStreakThreshold
	if cbEnabled || (!drawdownBreached && !lossBreached) {
		circuitBreakerSuppressedWarned.Delete(s.ID)
		return
	}
	if _, loaded := circuitBreakerSuppressedWarned.LoadOrStore(s.ID, struct{}{}); loaded {
		return // already warned this episode — do not repeat every cycle
	}
	var reasons []string
	if drawdownBreached {
		reasons = append(reasons, fmt.Sprintf("drawdown %.1f%% > %.1f%%", r.CurrentDrawdownPct, r.MaxDrawdownPct))
	}
	if lossBreached {
		reasons = append(reasons, fmt.Sprintf("%d consecutive losses", r.ConsecutiveLosses))
	}
	if logger != nil {
		logger.Warn("WARNING: circuit breaker is DISABLED (circuit_breaker:false) and a halt threshold was crossed (%s) — NO circuit breaker fired. This strategy is trading WITHOUT the drawdown/consecutive-loss auto-halt and positions are NOT being auto-closed on this condition. This is a warning only (nothing was closed); re-enable circuit_breaker to restore protection.",
			strings.Join(reasons, "; "))
	}
	// #1449 review: escalate to the owner, not only to the strategy log. Since
	// #1448 this breaker owns margin protection whenever the portfolio latch
	// belongs to equity, so a disabled one crossing a halt threshold is an
	// absent auto-protective mechanism. Queued rather than sent because
	// CheckRisk holds mu (#880); the main loop drains it after mu.Unlock().
	// The LoadOrStore above already made this once per suppression episode.
	queueCircuitBreakerSuppressionAlert(s.ID, reasons)
}

// RecordTradeResult updates risk state with realized PnL for daily limits and
// consecutive-loss circuit breakers. Lifetime trade stats come from SQLite.
func RecordTradeResult(r *RiskState, pnl float64) {
	rolloverDailyPnL(r)
	r.DailyPnL += pnl
	if pnl >= 0 {
		r.ConsecutiveLosses = 0
	} else {
		r.ConsecutiveLosses++
	}
}

// RecordHedgeTradeResult books a #1159 hedge leg's realized PnL into the daily
// aggregate WITHOUT touching the consecutive-loss streak.
//
// The daily PnL must include the hedge: it is real cash on the same wallet, and
// the #1269 daily-loss limit measures the operator's actual exposure for the
// day. Omitting it would let a hedge bleed unlimited cash past the limit.
//
// The loss STREAK must not. A correlated hedge loses by construction whenever
// the primary wins — that is the entire point of holding it. Feeding it into
// ConsecutiveLosses would count one thesis twice with opposite signs: a
// perfectly profitable strategy alternating primary-win/hedge-loss legs would
// have its streak reset and re-armed on every round trip, and a strategy on a
// genuine losing run would have its streak spuriously RESET by the hedge's
// offsetting win — disarming the loss-streak circuit-breaker arm exactly when
// it is most needed. Neither direction of that error is acceptable in an
// auto-protective path, so hedge legs stay out of the streak entirely.
func RecordHedgeTradeResult(r *RiskState, pnl float64) {
	if r == nil {
		return
	}
	rolloverDailyPnL(r)
	r.DailyPnL += pnl
}

// recordPositionTradeResult routes a realized-PnL booking to the correct risk
// accumulator for the position that produced it. Every perps close path books
// through here so a hedge leg can never reach the loss streak by accident: a
// new close site gets the routing for free instead of having to remember it.
func recordPositionTradeResult(s *StrategyState, pos *Position, pnl float64) {
	if s == nil {
		return
	}
	if pos.isHedgeLeg() {
		RecordHedgeTradeResult(&s.RiskState, pnl)
		return
	}
	RecordTradeResult(&s.RiskState, pnl)
}
