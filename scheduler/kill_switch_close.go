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

	// OKXLiveAllPerps: every live OKX perps strategy configured (used to
	// decide which coins to close and to detect "unconfigured" positions).
	OKXLiveAllPerps []StrategyConfig
	// OKXLiveAllSpot: every live OKX spot strategy configured. The kill
	// switch cannot close spot positions safely (#345) — these are surfaced
	// in the Discord message as a known gap so the operator intervenes.
	OKXLiveAllSpot []StrategyConfig
	OKXCloser      OKXLiveCloser
	OKXFetcher     OKXPositionsFetcher

	PortfolioReason string
	CloseTimeout    time.Duration
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

	// DiscordMessage is the formatted notification string; empty when no
	// Discord message should be sent. Caller checks notifier.HasBackends()
	// before delivering.
	DiscordMessage string

	// LogLines are the stderr lines to print ([CRITICAL]/[INFO]). Built here
	// rather than printed directly so tests can assert messaging.
	LogLines []string
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
	if !hlStateFetched && in.HLAddr != "" && in.HLFetcher != nil {
		pos, err := in.HLFetcher(in.HLAddr)
		if err != nil {
			plan.LogLines = append(plan.LogLines,
				fmt.Sprintf("[CRITICAL] hl-close: kill switch unable to fetch HL state: %v — cannot confirm on-chain flat", err))
			plan.OnChainConfirmedFlat = false
		} else {
			hlPositions = pos
			hlStateFetched = true
		}
	}

	switch {
	case hlStateFetched && len(in.HLLiveAll) > 0:
		ctx, cancel := context.WithTimeout(context.Background(), in.CloseTimeout)
		plan.CloseReport = forceCloseHyperliquidLive(ctx, hlPositions, in.HLLiveAll, in.HLCloser)
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
	if len(in.OKXLiveAllPerps) > 0 && in.OKXFetcher != nil {
		okxPositions, err := in.OKXFetcher()
		if err != nil {
			plan.LogLines = append(plan.LogLines,
				fmt.Sprintf("[CRITICAL] okx-close: kill switch unable to fetch OKX positions: %v — cannot confirm on-chain flat", err))
			plan.OnChainConfirmedFlat = false
		} else {
			ctx, cancel := context.WithTimeout(context.Background(), in.CloseTimeout)
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

			// Unconfigured OKX positions: traded coins vs. on-chain. Same
			// semantic as HL: kill switch refuses to unilaterally liquidate
			// positions for coins it isn't configured to trade.
			tradedCoins := make(map[string]bool)
			for _, sc := range in.OKXLiveAllPerps {
				if sc.Type != "perps" {
					continue
				}
				if sym := okxSymbol(sc.Args); sym != "" {
					tradedCoins[sym] = true
				}
			}
			for _, p := range okxPositions {
				if !tradedCoins[p.Coin] && p.Size != 0 {
					plan.OKXUnconfigured = append(plan.OKXUnconfigured, p)
				}
			}
			if len(plan.OKXUnconfigured) > 0 {
				plan.OnChainConfirmedFlat = false
				for _, p := range plan.OKXUnconfigured {
					plan.LogLines = append(plan.LogLines,
						fmt.Sprintf("[CRITICAL] okx-close: on-chain position for unconfigured coin %s (size=%.6f) — manual intervention required, kill switch will retry next cycle", p.Coin, p.Size))
				}
			}
		}
	}

	plan.DiscordMessage = formatKillSwitchMessage(in.HLAddr, plan, in.PortfolioReason)
	return plan
}

// formatKillSwitchMessage builds the Discord notification string from a plan.
// Split out so tests can call it directly and so main.go delivery stays a
// one-liner. Returns two distinct shapes: "PORTFOLIO KILL SWITCH" on
// confirmed-flat, "PORTFOLIO KILL SWITCH (LATCHED, RETRYING)" otherwise.
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
		if plan.OKXSpotPresent {
			parts = append(parts, "OKX spot strategies present — verify manually (kill switch cannot auto-close spot)")
		}
		summary := strings.Join(parts, "; ")
		return fmt.Sprintf("**PORTFOLIO KILL SWITCH**\n%s\n%s. Virtual state cleared. Manual reset required.", portfolioReason, summary)
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
	if plan.OKXSpotPresent {
		segments = append(segments, "OKX spot strategies present — verify manually (kill switch cannot auto-close spot)")
	}
	if len(segments) == 0 {
		// Fallback: HL fetch failure path doesn't populate Errors/Unconfigured.
		segments = append(segments, "Could not fetch on-chain state to confirm flat")
	}

	return fmt.Sprintf("**PORTFOLIO KILL SWITCH (LATCHED, RETRYING)**\n%s\n%s. Virtual state preserved. Next cycle will retry.", portfolioReason, strings.Join(segments, " | "))
}
