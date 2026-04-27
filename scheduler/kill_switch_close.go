package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// KillSwitchCloseInputs bundles every platform-specific bit the kill-switch
// plan builder needs. Grouped into a struct so that adding a new platform
// (Robinhood, TopStep, etc.) is an additive change rather than a signature
// widening that cascades through every call site and test.
//
// HLStateFetched / HLPositions capture whether main.go already fetched HL
// clearinghouseState earlier in the cycle (for shared-wallet balance or
// due reconcile). When HLStateFetched is false but HLAddr is set, the
// plan builder does an opportunistic fetch via HLFetcher so a configured
// account with no due strategies still gets verified (false-reassurance
// guard from #341 review).
//
// OKX has no equivalent public position endpoint — we always call
// OKXFetcher when there's at least one live OKX perps strategy, rather
// than trying to reuse a pre-fetch. Spot OKX strategies are surfaced via
// OKXSpotLive so the plan builder can warn in the Discord message without
// attempting an unsafe automated close (see forceCloseOKXLive doc).
type KillSwitchCloseInputs struct {
	HLAddr         string
	HLStateFetched bool
	HLPositions    []HLPosition
	HLLiveAll      []StrategyConfig
	HLCloser       HyperliquidLiveCloser
	HLFetcher      HLStateFetcher
	// HLStopLossOIDs maps coin → resting per-trade SL trigger OIDs so the
	// kill-switch close path can cancel them before flattening. Without
	// this, kill-switch wipes virtual state but the on-chain triggers sit
	// resting and burn HL's 10/day account-wide cap (#421 review point 1).
	// nil/empty disables; coins with no resting SL are simply absent.
	HLStopLossOIDs map[string][]int64

	// OKXLiveAllPerps: every live OKX perps strategy configured (used to
	// decide which coins to close and to detect "unconfigured" positions).
	OKXLiveAllPerps []StrategyConfig
	// OKXLiveAllSpot: every live OKX spot strategy configured. The kill
	// switch cannot close spot positions safely (#345) — these are surfaced
	// in the Discord message as a known gap so the operator intervenes.
	OKXLiveAllSpot []StrategyConfig
	OKXCloser      OKXLiveCloser
	OKXFetcher     OKXPositionsFetcher

	// RHLiveCrypto: every live Robinhood crypto (Type=="spot") strategy
	// configured. Used to decide which coins to close and to detect
	// "unconfigured" crypto balances. Robinhood has no public unauthenticated
	// position endpoint — we always call RHFetcher when there's at least
	// one live Robinhood crypto strategy, rather than trying to reuse a
	// pre-fetch. (#346)
	RHLiveCrypto []StrategyConfig
	// RHLiveOptions: every live Robinhood options strategy configured.
	// Surfaced in the Discord message as a known gap — stock options close
	// semantics (sell-to-close vs buy-to-close per leg) require dispatch
	// that the kill switch doesn't yet handle (#346 follow-up). Does NOT
	// block OnChainConfirmedFlat; matches the OKXSpotPresent semantic.
	RHLiveOptions []StrategyConfig
	RHCloser      RobinhoodLiveCloser
	RHFetcher     RobinhoodPositionsFetcher

	// TSLiveAll: every live TopStep futures strategy configured. Used to
	// decide which symbols to close and to detect "unconfigured" positions.
	// TopStep has no public unauthenticated endpoint — we always call
	// TSFetcher when there's at least one live TopStep futures strategy,
	// rather than trying to reuse a pre-fetch. (#347)
	TSLiveAll []StrategyConfig
	TSCloser  TopStepLiveCloser
	TSFetcher TopStepPositionsFetcher

	PortfolioReason string

	// CloseTimeout is the default per-platform close-budget when a
	// platform-specific override is unset (zero). Each platform gets its
	// OWN context.WithTimeout — they do not share a single budget — but a
	// single tunable here was insufficient for platforms with very different
	// per-call costs (Robinhood adds TOTP login overhead per submit). The
	// per-platform fields below let the caller widen RH without giving HL
	// extra headroom.
	CloseTimeout time.Duration

	// Per-platform overrides. Zero means "use CloseTimeout". Each platform's
	// context is independent so one slow platform's budget cannot starve
	// the others.
	HLCloseTimeout  time.Duration
	OKXCloseTimeout time.Duration
	RHCloseTimeout  time.Duration
	TSCloseTimeout  time.Duration
}

// platformCloseBudget returns the effective close-budget for a platform,
// preferring the per-platform override and falling back to CloseTimeout.
// Centralized so a future "minimum 30s per platform" floor lives in one
// place rather than four switch arms.
func (in KillSwitchCloseInputs) platformCloseBudget(override time.Duration) time.Duration {
	if override > 0 {
		return override
	}
	return in.CloseTimeout
}

// KillSwitchClosePlan is the output of planKillSwitchClose — everything the
// main loop needs to apply virtual-state mutation and send notifications.
// The plan is pure data (no goroutines, no I/O callbacks), so the main loop
// can gate virtual state mutation on OnChainConfirmedFlat under its own
// mutex without re-running any logic.
type KillSwitchClosePlan struct {
	// OnChainConfirmedFlat is the load-bearing correctness signal. True means
	// the caller MAY clear virtual state. False means at least one live
	// exposure (on any platform) could not be confirmed closed — caller
	// MUST leave virtual state intact and let the next cycle retry.
	OnChainConfirmedFlat bool

	// CloseReport is the HL per-coin outcome. Zero value when no HL close
	// was attempted.
	CloseReport HyperliquidLiveCloseReport

	// OKXCloseReport is the OKX per-coin outcome. Zero value when no OKX
	// close was attempted.
	OKXCloseReport OKXLiveCloseReport

	// Unconfigured lists HL on-chain positions for coins no configured live
	// HL strategy trades. Kept as HLPosition for backward compat with #341
	// tests; OKX equivalent is in OKXUnconfigured.
	Unconfigured []HLPosition

	// OKXUnconfigured lists OKX on-chain positions for coins no configured
	// live OKX strategy trades. Same manual-intervention semantic as
	// Unconfigured.
	OKXUnconfigured []OKXPosition

	// OKXSpotPresent is true when there is at least one live OKX spot
	// strategy configured. Signals that the kill switch left an unhandled
	// gap — surfaced in the Discord message but does NOT block
	// OnChainConfirmedFlat (the scheduler has no reliable way to check
	// whether there is actual spot exposure, and blocking would latch
	// forever).
	OKXSpotPresent bool

	// RHCloseReport is the Robinhood per-coin outcome. Zero value when no
	// Robinhood crypto close was attempted.
	RHCloseReport RobinhoodLiveCloseReport

	// RHUnconfigured lists live Robinhood crypto balances for coins no
	// configured live Robinhood crypto strategy trades. Same manual-
	// intervention semantic as HL Unconfigured / OKX Unconfigured.
	RHUnconfigured []RobinhoodPosition

	// RHOptionsPresent is true when there is at least one live Robinhood
	// options strategy configured. Signals an unhandled gap — surfaced in
	// the Discord message but does NOT block OnChainConfirmedFlat
	// (hard-latch would freeze the scheduler for any Robinhood options
	// user with no available close path). Mirrors OKXSpotPresent.
	RHOptionsPresent bool

	// TSCloseReport is the TopStep per-symbol outcome. Zero value when no
	// TopStep close was attempted.
	TSCloseReport TopStepLiveCloseReport

	// TSUnconfigured lists live TopStep positions for symbols no configured
	// live TopStep futures strategy trades. Same manual-intervention
	// semantic as HL/OKX/Robinhood Unconfigured.
	TSUnconfigured []TopStepPosition

	// DiscordMessage is the formatted notification string; empty when no
	// Discord message should be sent. Caller checks notifier.HasBackends()
	// before delivering.
	DiscordMessage string

	// LogLines are the stderr lines to print ([CRITICAL]/[INFO]). Built here
	// rather than printed directly so tests can assert messaging.
	LogLines []string
}

func (p KillSwitchClosePlan) CanAutoResetWithoutOwner() bool {
	return p.OnChainConfirmedFlat && !p.OKXSpotPresent && !p.RHOptionsPresent
}

const killSwitchManualResetLine = "Virtual state cleared. Manual reset required."
const killSwitchAutoResetLine = "Virtual state cleared. Kill switch auto-reset; trading will resume next cycle."

func formatKillSwitchAutoResetMessage(msg string) string {
	return strings.Replace(msg, killSwitchManualResetLine, killSwitchAutoResetLine, 1)
}

// HLStateFetcher re-fetches Hyperliquid on-chain positions for the kill-switch
// opportunistic-fetch path. Exposed as a function type so tests can stub the
// HTTP call. The default wraps fetchHyperliquidState.
type HLStateFetcher func(accountAddress string) ([]HLPosition, error)

// defaultHLStateFetcher wraps fetchHyperliquidState for production use. The
// kill-switch path discards the balance field — only positions are needed.
func defaultHLStateFetcher(addr string) ([]HLPosition, error) {
	_, pos, err := fetchHyperliquidState(addr)
	return pos, err
}

// planKillSwitchClose runs the kill-switch close logic without touching any
// mutable state — no locks, no virtual state mutation, no Discord delivery.
// The caller applies mutations based on the returned plan.
//
// Extracted from main.go so the latch-until-flat flow (the actual #341 fix)
// can be unit-tested with fake closers + fake fetchers. Without this seam,
// the load-bearing `if killSwitchFired && OnChainConfirmedFlat` gate around
// forceCloseAllPositions would regress silently — exactly the kind of bug
// #341 was.
//
// Platform handling is independent: either platform (HL or OKX) being
// un-confirmed-flat flips OnChainConfirmedFlat to false and latches the
// switch. Messages combine both platforms' status.
func planKillSwitchClose(in KillSwitchCloseInputs) KillSwitchClosePlan {
	plan := KillSwitchClosePlan{OnChainConfirmedFlat: true}

	// ── Hyperliquid ─────────────────────────────────────────────────
	hlPositions := in.HLPositions
	hlStateFetched := in.HLStateFetched

	// Opportunistic HL fetch: operator could have removed all HL strategies
	// from config while the wallet still holds positions from a previous
	// deploy or manual trade. Kill switch must not report "no exposure"
	// without actually checking (#341 review, false-reassurance case).
	if !hlStateFetched && in.HLAddr != "" {
		switch {
		case in.HLFetcher != nil:
			pos, err := in.HLFetcher(in.HLAddr)
			if err != nil {
				plan.LogLines = append(plan.LogLines,
					fmt.Sprintf("[CRITICAL] hl-close: kill switch unable to fetch HL state: %v — cannot confirm on-chain flat", err))
				plan.OnChainConfirmedFlat = false
			} else {
				hlPositions = pos
				hlStateFetched = true
			}
		default:
			// Defense-in-depth: production wires HLFetcher in main.go, but a
			// future regression that drops the assignment would otherwise
			// silently bypass the kill switch (false-reassurance, latch
			// stays clear). Latch and log instead. (#350)
			plan.LogLines = append(plan.LogLines,
				"[CRITICAL] hl-close: HLAddr configured but HLFetcher unwired — cannot confirm on-chain flat (kill switch will retry next cycle)")
			plan.OnChainConfirmedFlat = false
		}
	}

	switch {
	case hlStateFetched && len(in.HLLiveAll) > 0:
		ctx, cancel := context.WithTimeout(context.Background(), in.platformCloseBudget(in.HLCloseTimeout))
		plan.CloseReport = forceCloseHyperliquidLive(ctx, hlPositions, in.HLLiveAll, in.HLCloser, in.HLStopLossOIDs)
		cancel()
		if !plan.CloseReport.ConfirmedFlat() {
			plan.OnChainConfirmedFlat = false
		}
		if len(plan.CloseReport.ClosedCoins) > 0 {
			plan.LogLines = append(plan.LogLines,
				fmt.Sprintf("[CRITICAL] hl-close: confirmed close for %v", plan.CloseReport.ClosedCoins))
		}
		if len(plan.CloseReport.AlreadyFlat) > 0 {
			plan.LogLines = append(plan.LogLines,
				fmt.Sprintf("[INFO] hl-close: already flat on-chain: %v", plan.CloseReport.AlreadyFlat))
		}
		for _, coin := range plan.CloseReport.SortedErrorCoins() {
			plan.LogLines = append(plan.LogLines,
				fmt.Sprintf("[CRITICAL] hl-close: %s failed: %v (kill switch will retry next cycle)", coin, plan.CloseReport.Errors[coin]))
		}

	case hlStateFetched && len(in.HLLiveAll) == 0:
		for _, p := range hlPositions {
			if p.Size != 0 {
				plan.Unconfigured = append(plan.Unconfigured, p)
			}
		}
		if len(plan.Unconfigured) > 0 {
			plan.OnChainConfirmedFlat = false
			for _, p := range plan.Unconfigured {
				plan.LogLines = append(plan.LogLines,
					fmt.Sprintf("[CRITICAL] hl-close: on-chain position for unconfigured coin %s (szi=%.6f) — manual intervention required, kill switch will retry next cycle", p.Coin, p.Size))
			}
		}
	}

	// ── OKX ─────────────────────────────────────────────────────────
	// OKX spot is surfaced as a known gap but does not block flat — we
	// cannot fetch nor safely auto-close spot balances (#345).
	plan.OKXSpotPresent = len(in.OKXLiveAllSpot) > 0
	if plan.OKXSpotPresent {
		plan.LogLines = append(plan.LogLines,
			fmt.Sprintf("[CRITICAL] okx-close: %d live OKX spot strategies configured — kill switch cannot auto-close spot (no reduce-only); operator must verify manually (#345)", len(in.OKXLiveAllSpot)))
	}

	// Perps: always attempt fetch when there's a perps strategy or fetcher
	// — mirrors the HL opportunistic-fetch guard. Unlike HL there is no
	// pre-fetch to reuse; OKX always requires a subprocess round-trip.
	switch {
	case len(in.OKXLiveAllPerps) > 0 && in.OKXFetcher == nil:
		// Defense-in-depth (#350): a future main.go regression that drops
		// OKXFetcher would otherwise silently skip OKX and clear virtual
		// state with on-chain exposure live. Latch and log.
		plan.LogLines = append(plan.LogLines,
			"[CRITICAL] okx-close: OKX live perps strategies configured but OKXFetcher unwired — cannot confirm on-chain flat (kill switch will retry next cycle)")
		plan.OnChainConfirmedFlat = false
	case len(in.OKXLiveAllPerps) > 0:
		okxPositions, err := in.OKXFetcher()
		if err != nil {
			plan.LogLines = append(plan.LogLines,
				fmt.Sprintf("[CRITICAL] okx-close: kill switch unable to fetch OKX positions: %v — cannot confirm on-chain flat", err))
			plan.OnChainConfirmedFlat = false
		} else {
			ctx, cancel := context.WithTimeout(context.Background(), in.platformCloseBudget(in.OKXCloseTimeout))
			plan.OKXCloseReport = forceCloseOKXLive(ctx, okxPositions, in.OKXLiveAllPerps, in.OKXCloser)
			cancel()
			if !plan.OKXCloseReport.ConfirmedFlat() {
				plan.OnChainConfirmedFlat = false
			}
			if len(plan.OKXCloseReport.ClosedCoins) > 0 {
				plan.LogLines = append(plan.LogLines,
					fmt.Sprintf("[CRITICAL] okx-close: confirmed close for %v", plan.OKXCloseReport.ClosedCoins))
			}
			if len(plan.OKXCloseReport.AlreadyFlat) > 0 {
				plan.LogLines = append(plan.LogLines,
					fmt.Sprintf("[INFO] okx-close: already flat on-chain: %v", plan.OKXCloseReport.AlreadyFlat))
			}
			for _, coin := range plan.OKXCloseReport.SortedErrorCoins() {
				plan.LogLines = append(plan.LogLines,
					fmt.Sprintf("[CRITICAL] okx-close: %s failed: %v (kill switch will retry next cycle)", coin, plan.OKXCloseReport.Errors[coin]))
			}

			// Unconfigured OKX positions are detected in forceCloseOKXLive
			// so the traded-coins partition has a single source of truth.
			// Semantic matches HL Unconfigured: kill switch refuses to
			// unilaterally liquidate positions for coins it isn't
			// configured to trade.
			plan.OKXUnconfigured = plan.OKXCloseReport.Unconfigured
			if len(plan.OKXUnconfigured) > 0 {
				plan.OnChainConfirmedFlat = false
				for _, p := range plan.OKXUnconfigured {
					plan.LogLines = append(plan.LogLines,
						fmt.Sprintf("[CRITICAL] okx-close: on-chain position for unconfigured coin %s (size=%.6f) — manual intervention required, kill switch will retry next cycle", p.Coin, p.Size))
				}
			}
		}
	}

	// ── Robinhood ───────────────────────────────────────────────────
	// Options are surfaced as a known gap (like OKX spot) — stock options
	// close semantics are complex enough that the kill switch cannot safely
	// auto-close them. Crypto (spot) is handled below.
	plan.RHOptionsPresent = len(in.RHLiveOptions) > 0
	if plan.RHOptionsPresent {
		plan.LogLines = append(plan.LogLines,
			fmt.Sprintf("[CRITICAL] rh-close: %d live Robinhood options strategies configured — kill switch cannot auto-close options (sell-to-close vs buy-to-close semantics); operator must verify manually (#346)", len(in.RHLiveOptions)))
	}

	// Crypto: always attempt fetch when there's a configured crypto strategy
	// — mirrors the OKX opportunistic-fetch guard. Robinhood has no
	// pre-fetch to reuse; every cycle requires a subprocess round-trip
	// (TOTP-authenticated).
	switch {
	case len(in.RHLiveCrypto) > 0 && in.RHFetcher == nil:
		// Defense-in-depth (#350): a future main.go regression that drops
		// RHFetcher would otherwise silently skip Robinhood and clear
		// virtual state with on-account exposure live. Latch and log.
		plan.LogLines = append(plan.LogLines,
			"[CRITICAL] rh-close: Robinhood live crypto strategies configured but RHFetcher unwired — cannot confirm flat (kill switch will retry next cycle)")
		plan.OnChainConfirmedFlat = false
	case len(in.RHLiveCrypto) > 0:
		rhPositions, err := in.RHFetcher()
		if err != nil {
			plan.LogLines = append(plan.LogLines,
				fmt.Sprintf("[CRITICAL] rh-close: kill switch unable to fetch Robinhood positions: %v — cannot confirm flat", err))
			plan.OnChainConfirmedFlat = false
		} else {
			ctx, cancel := context.WithTimeout(context.Background(), in.platformCloseBudget(in.RHCloseTimeout))
			plan.RHCloseReport = forceCloseRobinhoodLive(ctx, rhPositions, in.RHLiveCrypto, in.RHCloser)
			cancel()
			if !plan.RHCloseReport.ConfirmedFlat() {
				plan.OnChainConfirmedFlat = false
			}
			if len(plan.RHCloseReport.ClosedCoins) > 0 {
				plan.LogLines = append(plan.LogLines,
					fmt.Sprintf("[CRITICAL] rh-close: confirmed close for %v", plan.RHCloseReport.ClosedCoins))
			}
			if len(plan.RHCloseReport.AlreadyFlat) > 0 {
				plan.LogLines = append(plan.LogLines,
					fmt.Sprintf("[INFO] rh-close: already flat: %v", plan.RHCloseReport.AlreadyFlat))
			}
			for _, coin := range plan.RHCloseReport.SortedErrorCoins() {
				plan.LogLines = append(plan.LogLines,
					fmt.Sprintf("[CRITICAL] rh-close: %s failed: %v (kill switch will retry next cycle)", coin, plan.RHCloseReport.Errors[coin]))
			}

			plan.RHUnconfigured = plan.RHCloseReport.Unconfigured
			if len(plan.RHUnconfigured) > 0 {
				plan.OnChainConfirmedFlat = false
				for _, p := range plan.RHUnconfigured {
					plan.LogLines = append(plan.LogLines,
						fmt.Sprintf("[CRITICAL] rh-close: live balance for unconfigured coin %s (size=%.6f) — manual intervention required, kill switch will retry next cycle", p.Coin, p.Size))
				}
			}
		}
	}

	// ── TopStep ─────────────────────────────────────────────────────
	// Futures: always attempt fetch when there's a configured futures
	// strategy — mirrors the OKX / Robinhood opportunistic-fetch guard.
	// TopStep has no pre-fetch to reuse; every cycle requires a subprocess
	// round-trip (TopStepX REST, authenticated).
	//
	// CME trading-hour restriction: fires outside RTH will surface a venue
	// error here, latching the kill switch until the next in-hours cycle.
	// This is the correct behavior — do not attempt to bypass the venue.
	switch {
	case len(in.TSLiveAll) > 0 && in.TSFetcher == nil:
		// Defense-in-depth (#350): a future main.go regression that drops
		// TSFetcher would otherwise silently skip TopStep and clear virtual
		// state with on-account exposure live. Latch and log.
		plan.LogLines = append(plan.LogLines,
			"[CRITICAL] ts-close: TopStep live futures strategies configured but TSFetcher unwired — cannot confirm flat (kill switch will retry next cycle)")
		plan.OnChainConfirmedFlat = false
	case len(in.TSLiveAll) > 0:
		tsPositions, err := in.TSFetcher()
		if err != nil {
			plan.LogLines = append(plan.LogLines,
				fmt.Sprintf("[CRITICAL] ts-close: kill switch unable to fetch TopStep positions: %v — cannot confirm flat", err))
			plan.OnChainConfirmedFlat = false
		} else {
			ctx, cancel := context.WithTimeout(context.Background(), in.platformCloseBudget(in.TSCloseTimeout))
			plan.TSCloseReport = forceCloseTopStepLive(ctx, tsPositions, in.TSLiveAll, in.TSCloser)
			cancel()
			if !plan.TSCloseReport.ConfirmedFlat() {
				plan.OnChainConfirmedFlat = false
			}
			if len(plan.TSCloseReport.ClosedCoins) > 0 {
				plan.LogLines = append(plan.LogLines,
					fmt.Sprintf("[CRITICAL] ts-close: confirmed close for %v", plan.TSCloseReport.ClosedCoins))
			}
			if len(plan.TSCloseReport.AlreadyFlat) > 0 {
				plan.LogLines = append(plan.LogLines,
					fmt.Sprintf("[INFO] ts-close: already flat: %v", plan.TSCloseReport.AlreadyFlat))
			}
			for _, coin := range plan.TSCloseReport.SortedErrorCoins() {
				plan.LogLines = append(plan.LogLines,
					fmt.Sprintf("[CRITICAL] ts-close: %s failed: %v (kill switch will retry next cycle)", coin, plan.TSCloseReport.Errors[coin]))
			}

			plan.TSUnconfigured = plan.TSCloseReport.Unconfigured
			if len(plan.TSUnconfigured) > 0 {
				plan.OnChainConfirmedFlat = false
				for _, p := range plan.TSUnconfigured {
					plan.LogLines = append(plan.LogLines,
						fmt.Sprintf("[CRITICAL] ts-close: live position for unconfigured symbol %s (size=%d) — manual intervention required, kill switch will retry next cycle", p.Coin, p.Size))
				}
			}
		}
	}

	plan.DiscordMessage = formatKillSwitchMessage(in.HLAddr, plan, in.PortfolioReason)
	return plan
}

// formatKillSwitchMessage builds the Discord notification string from a plan.
// Split out so tests can call it directly and so main.go delivery stays a
// one-liner. Returns three distinct shapes:
//   - "PORTFOLIO KILL SWITCH" — confirmed-flat, no spot gap.
//   - "PORTFOLIO KILL SWITCH (GAPS — VERIFY MANUALLY)" — confirmed-flat
//     for closable platforms, but at least one unhandled exposure class
//     (OKX spot #345, Robinhood options #346) is configured and the
//     scheduler has no safe auto-close path. Header is distinct so an
//     operator skimming does not read "Virtual state cleared" as
//     "everything is closed."
//   - "PORTFOLIO KILL SWITCH (LATCHED, RETRYING)" — some on-chain
//     exposure could not be confirmed closed; retry next cycle.
func formatKillSwitchMessage(hlAddr string, plan KillSwitchClosePlan, portfolioReason string) string {
	if plan.OnChainConfirmedFlat {
		var parts []string
		if len(plan.CloseReport.ClosedCoins) > 0 {
			parts = append(parts, fmt.Sprintf("HL closes: %v", plan.CloseReport.ClosedCoins))
		} else if hlAddr == "" {
			parts = append(parts, "HL not configured")
		} else {
			parts = append(parts, "no live HL exposure")
		}
		if len(plan.OKXCloseReport.ClosedCoins) > 0 {
			parts = append(parts, fmt.Sprintf("OKX closes: %v", plan.OKXCloseReport.ClosedCoins))
		}
		if len(plan.RHCloseReport.ClosedCoins) > 0 {
			parts = append(parts, fmt.Sprintf("Robinhood closes: %v", plan.RHCloseReport.ClosedCoins))
		}
		if len(plan.TSCloseReport.ClosedCoins) > 0 {
			parts = append(parts, fmt.Sprintf("TopStep closes: %v", plan.TSCloseReport.ClosedCoins))
		}
		header := "**PORTFOLIO KILL SWITCH**"
		gapNotes := []string{}
		if plan.OKXSpotPresent {
			gapNotes = append(gapNotes, "OKX spot strategies present — kill switch cannot auto-close spot, verify balances manually")
		}
		if plan.RHOptionsPresent {
			gapNotes = append(gapNotes, "Robinhood options strategies present — kill switch cannot auto-close options, verify manually")
		}
		if len(gapNotes) > 0 {
			header = "**PORTFOLIO KILL SWITCH (GAPS — VERIFY MANUALLY)**"
			parts = append(parts, gapNotes...)
		}
		summary := strings.Join(parts, "; ")
		return fmt.Sprintf("%s\n%s\n%s. %s", header, portfolioReason, summary, killSwitchManualResetLine)
	}

	var segments []string

	if len(plan.CloseReport.Errors) > 0 {
		parts := make([]string, 0, len(plan.CloseReport.Errors))
		for _, coin := range plan.CloseReport.SortedErrorCoins() {
			parts = append(parts, fmt.Sprintf("%s: %v", coin, plan.CloseReport.Errors[coin]))
		}
		segments = append(segments, "HL live close errors — "+strings.Join(parts, "; "))
	}
	if len(plan.OKXCloseReport.Errors) > 0 {
		parts := make([]string, 0, len(plan.OKXCloseReport.Errors))
		for _, coin := range plan.OKXCloseReport.SortedErrorCoins() {
			parts = append(parts, fmt.Sprintf("%s: %v", coin, plan.OKXCloseReport.Errors[coin]))
		}
		segments = append(segments, "OKX live close errors — "+strings.Join(parts, "; "))
	}
	if len(plan.Unconfigured) > 0 {
		names := make([]string, 0, len(plan.Unconfigured))
		for _, p := range plan.Unconfigured {
			names = append(names, fmt.Sprintf("%s szi=%.6f", p.Coin, p.Size))
		}
		sort.Strings(names)
		segments = append(segments, "On-chain HL positions for unconfigured coins (manual intervention required) — "+strings.Join(names, "; "))
	}
	if len(plan.OKXUnconfigured) > 0 {
		names := make([]string, 0, len(plan.OKXUnconfigured))
		for _, p := range plan.OKXUnconfigured {
			names = append(names, fmt.Sprintf("%s size=%.6f", p.Coin, p.Size))
		}
		sort.Strings(names)
		segments = append(segments, "On-chain OKX positions for unconfigured coins (manual intervention required) — "+strings.Join(names, "; "))
	}
	if len(plan.RHCloseReport.Errors) > 0 {
		parts := make([]string, 0, len(plan.RHCloseReport.Errors))
		for _, coin := range plan.RHCloseReport.SortedErrorCoins() {
			parts = append(parts, fmt.Sprintf("%s: %v", coin, plan.RHCloseReport.Errors[coin]))
		}
		segments = append(segments, "Robinhood live close errors — "+strings.Join(parts, "; "))
	}
	if len(plan.RHUnconfigured) > 0 {
		names := make([]string, 0, len(plan.RHUnconfigured))
		for _, p := range plan.RHUnconfigured {
			names = append(names, fmt.Sprintf("%s size=%.6f", p.Coin, p.Size))
		}
		sort.Strings(names)
		segments = append(segments, "Live Robinhood balances for unconfigured coins (manual intervention required) — "+strings.Join(names, "; "))
	}
	if len(plan.TSCloseReport.Errors) > 0 {
		parts := make([]string, 0, len(plan.TSCloseReport.Errors))
		for _, coin := range plan.TSCloseReport.SortedErrorCoins() {
			parts = append(parts, fmt.Sprintf("%s: %v", coin, plan.TSCloseReport.Errors[coin]))
		}
		segments = append(segments, "TopStep live close errors — "+strings.Join(parts, "; "))
	}
	if len(plan.TSUnconfigured) > 0 {
		names := make([]string, 0, len(plan.TSUnconfigured))
		for _, p := range plan.TSUnconfigured {
			names = append(names, fmt.Sprintf("%s size=%d", p.Coin, p.Size))
		}
		sort.Strings(names)
		segments = append(segments, "Live TopStep positions for unconfigured symbols (manual intervention required) — "+strings.Join(names, "; "))
	}
	if plan.OKXSpotPresent {
		segments = append(segments, "OKX spot strategies present — verify manually (kill switch cannot auto-close spot)")
	}
	if plan.RHOptionsPresent {
		segments = append(segments, "Robinhood options strategies present — verify manually (kill switch cannot auto-close options)")
	}
	if len(segments) == 0 {
		// Fallback: HL fetch failure path doesn't populate Errors/Unconfigured.
		segments = append(segments, "Could not fetch on-chain state to confirm flat")
	}

	return fmt.Sprintf("**PORTFOLIO KILL SWITCH (LATCHED, RETRYING)**\n%s\n%s. Virtual state preserved. Next cycle will retry.", portfolioReason, strings.Join(segments, " | "))
}
