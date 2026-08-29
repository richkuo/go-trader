package main

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type HLNoFillRecoverer func(since time.Time) (*HLUserFillsResult, error)

const (
	hlKillSwitchNoFillRecoveryLookback = 10 * time.Minute
	hlKillSwitchNoFillRecoveryTimeout  = 20 * time.Second
)

type KillSwitchCloseInputs struct {
	HLAddr            string
	HLStateFetched    bool
	HLPositions       []HLPosition
	HLLiveAll         []StrategyConfig
	HLHedgeCoins      map[string]bool
	HLCloser          HyperliquidLiveCloser
	HLFetcher         HLStateFetcher
	HLNoFillRecoverer HLNoFillRecoverer
	HLStopLossOIDs    map[string][]int64

	HLLimitOrderLoader  func() ([]PendingLimitOrder, error)
	HLLimitOrderRoster  []StrategyConfig
	HLLimitOrderDeps    killSwitchLimitOrderDeps
	HLLimitOrderTimeout time.Duration

	OKXLiveAllPerps []StrategyConfig
	OKXLiveAllSpot  []StrategyConfig
	OKXCloser       OKXLiveCloser
	OKXFetcher      OKXPositionsFetcher

	RHLiveCrypto  []StrategyConfig
	RHLiveOptions []StrategyConfig
	RHCloser      RobinhoodLiveCloser
	RHFetcher     RobinhoodPositionsFetcher

	TSLiveAll []StrategyConfig
	TSCloser  TopStepLiveCloser
	TSFetcher TopStepPositionsFetcher

	PortfolioReason string

	CloseTimeout time.Duration

	HLCloseTimeout  time.Duration
	OKXCloseTimeout time.Duration
	RHCloseTimeout  time.Duration
	TSCloseTimeout  time.Duration
}

func (in KillSwitchCloseInputs) platformCloseBudget(override time.Duration) time.Duration {
	if override > 0 {
		return override
	}
	return in.CloseTimeout
}

func defaultHLKillSwitchNoFillRecoverer(since time.Time) (*HLUserFillsResult, error) {
	return runFetchHLUserFillsWithTimeout(since, hlKillSwitchNoFillRecoveryTimeout)
}

type KillSwitchClosePlan struct {
	OnChainConfirmedFlat bool

	LimitOrderReport killSwitchLimitOrderReport

	CloseReport HyperliquidLiveCloseReport

	OKXCloseReport OKXLiveCloseReport

	Unconfigured []HLPosition

	OKXUnconfigured []OKXPosition

	OKXSpotPresent bool

	RHCloseReport RobinhoodLiveCloseReport

	RHUnconfigured []RobinhoodPosition

	RHOptionsPresent bool

	TSCloseReport TopStepLiveCloseReport

	TSUnconfigured []TopStepPosition

	DiscordMessage string

	LogLines []string
}

func (p KillSwitchClosePlan) CanAutoResetWithoutOwner() bool {
	return p.OnChainConfirmedFlat && p.LimitOrderReport.ConfirmedClear() && !p.OKXSpotPresent && !p.RHOptionsPresent
}

const killSwitchManualResetLine = "Virtual state cleared. Manual reset required."
const killSwitchAutoResetLine = "Virtual state cleared. Kill switch auto-reset; trading will resume next cycle."

func formatKillSwitchAutoResetMessage(msg string) string {
	return strings.Replace(msg, killSwitchManualResetLine, killSwitchAutoResetLine, 1)
}

type HLStateFetcher func(accountAddress string) ([]HLPosition, error)

func defaultHLStateFetcher(addr string) ([]HLPosition, error) {
	_, pos, err := fetchHyperliquidState(addr)
	return pos, err
}

func clearVerifiedFlatHLErrors(report *HyperliquidLiveCloseReport, positions []HLPosition) []string {
	if report == nil || len(report.Errors) == 0 {
		return nil
	}

	open := make(map[string]bool)
	for _, p := range positions {
		if p.Size != 0 {
			open[p.Coin] = true
		}
	}

	var verified []string
	for _, coin := range report.SortedErrorCoins() {
		if open[coin] {
			continue
		}
		delete(report.Errors, coin)
		report.AlreadyFlat = append(report.AlreadyFlat, coin)
		verified = append(verified, coin)
	}
	return verified
}

func recoverHyperliquidAlreadyFlatFills(report *HyperliquidLiveCloseReport, positions []HLPosition, recoverer HLNoFillRecoverer, since time.Time) []string {
	if report == nil || len(report.AlreadyFlat) == 0 || recoverer == nil {
		return nil
	}
	type expectedFill struct {
		qty float64
	}
	expectedByCoin := make(map[string]expectedFill)
	for _, p := range positions {
		qty := math.Abs(p.Size)
		if qty <= 0 {
			continue
		}
		coin := normalizeHLFillCoin(p.Coin)
		if coin == "" {
			continue
		}
		expectedByCoin[coin] = expectedFill{qty: qty}
	}
	eligibleByRaw := make(map[string]expectedFill)
	for _, rawCoin := range report.AlreadyFlat {
		norm := normalizeHLFillCoin(rawCoin)
		if norm == "" {
			continue
		}
		if report.Fills != nil {
			if _, ok := report.Fills[rawCoin]; ok {
				continue
			}
			if _, ok := report.Fills[norm]; ok {
				continue
			}
		}
		expected, ok := expectedByCoin[norm]
		if !ok || expected.qty <= 0 {
			continue
		}
		eligibleByRaw[rawCoin] = expected
	}
	if len(eligibleByRaw) == 0 {
		return nil
	}

	result, err := recoverer(since)
	if err != nil {
		return []string{fmt.Sprintf("[WARN] hl-close: unable to recover already-flat fill from userFills: %v", err)}
	}
	if result == nil {
		return []string{"[WARN] hl-close: unable to recover already-flat fill from userFills: empty result"}
	}
	if strings.TrimSpace(result.Error) != "" {
		return []string{fmt.Sprintf("[WARN] hl-close: unable to recover already-flat fill from userFills: %s", result.Error)}
	}
	if report.Fills == nil {
		report.Fills = make(map[string]HyperliquidCloseFill)
	}

	raws := make([]string, 0, len(eligibleByRaw))
	for r := range eligibleByRaw {
		raws = append(raws, r)
	}
	sort.Strings(raws)
	candidates := make(map[string]HLFillSummary, len(result.ByOID))
	for oid, summary := range result.ByOID {
		if summary.ClosedPnLGross == 0 {
			continue
		}
		if t := hlFillSummaryEventTime(summary); !t.IsZero() && t.Before(since) {
			continue
		}
		candidates[oid] = summary
	}
	var lines []string
	for _, rawCoin := range raws {
		expected := eligibleByRaw[rawCoin]
		match, ok, ambiguous := findUniqueHLFillByCoinQty(candidates, rawCoin, expected.qty, true, time.Time{}, 0)
		switch {
		case ok:
			report.Fills[rawCoin] = HyperliquidCloseFill{
				AvgPx:   match.Summary.Px,
				TotalSz: expected.qty,
				OID:     match.OIDInt,
				Fee:     match.Summary.Fee,
			}
			lines = append(lines,
				fmt.Sprintf("[INFO] hl-close: recovered already-flat fill for %s from userFills oid=%s qty=%.6f px=%.6f fee=%.6f", rawCoin, match.OID, expected.qty, match.Summary.Px, match.Summary.Fee))
		case ambiguous:
			lines = append(lines,
				fmt.Sprintf("[WARN] hl-close: multiple userFills candidates for already-flat %s qty=%.6f; falling back to model-only cleanup", rawCoin, expected.qty))
		default:
			lines = append(lines,
				fmt.Sprintf("[WARN] hl-close: no userFills match for already-flat %s qty=%.6f; falling back to model-only cleanup", rawCoin, expected.qty))
		}
	}
	return lines
}

func settledKillSwitchSymbols(plan *KillSwitchClosePlan) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	add := func(platform string, coins []string) {
		if len(coins) == 0 {
			return
		}
		set := out[platform]
		if set == nil {
			set = map[string]bool{}
			out[platform] = set
		}
		for _, c := range coins {
			if c != "" {
				set[c] = true
			}
		}
	}
	add("okx", plan.OKXCloseReport.ClosedCoins)
	add("robinhood", plan.RHCloseReport.ClosedCoins)
	add("topstep", plan.TSCloseReport.ClosedCoins)
	return out
}

func applyKillSwitchSettledLegsWhileLatched(strategies map[string]*StrategyState, cfgs []StrategyConfig, plan *KillSwitchClosePlan, hlRoster []StrategyConfig, virtualQty hlVirtualQuantitySnapshot, prices map[string]float64, logger *StrategyLogger) {
	if plan == nil || len(cfgs) == 0 {
		return
	}
	settled := settledKillSwitchSymbols(plan)
	hasHLFills := len(plan.CloseReport.Fills) > 0
	for _, sc := range cfgs {
		s, ok := strategies[sc.ID]
		if !ok || s == nil {
			continue
		}
		if hasHLFills {
			applyHyperliquidKillSwitchCloseFill(s, sc, plan.CloseReport.Fills, hlRoster, virtualQty)
			applyHyperliquidKillSwitchHedgeFill(s, sc, plan.CloseReport.Fills)
		}
		forceCloseSettledPositions(s, sc, prices, settled, logger)
	}
}

func planKillSwitchClose(in KillSwitchCloseInputs) KillSwitchClosePlan {
	plan := KillSwitchClosePlan{OnChainConfirmedFlat: true}

	plan.LimitOrderReport = cancelKillSwitchRestingLimitOrders(
		in.HLLimitOrderLoader, in.HLLimitOrderRoster, in.HLLimitOrderDeps,
		in.platformCloseBudget(in.HLLimitOrderTimeout))
	plan.LogLines = append(plan.LogLines, plan.LimitOrderReport.LogLines...)
	if !plan.LimitOrderReport.ConfirmedClear() {
		plan.OnChainConfirmedFlat = false
	}

	hlPositions := in.HLPositions
	hlStateFetched := in.HLStateFetched

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
			plan.LogLines = append(plan.LogLines,
				"[CRITICAL] hl-close: HLAddr configured but HLFetcher unwired — cannot confirm on-chain flat (kill switch will retry next cycle)")
			plan.OnChainConfirmedFlat = false
		}
	}

	switch {
	case hlStateFetched && len(in.HLLiveAll) > 0:
		recoverSince := time.Now().UTC().Add(-hlKillSwitchNoFillRecoveryLookback)
		ctx, cancel := context.WithTimeout(context.Background(), in.platformCloseBudget(in.HLCloseTimeout))
		plan.CloseReport = forceCloseHyperliquidLive(ctx, hlPositions, in.HLLiveAll, in.HLHedgeCoins, in.HLCloser, in.HLStopLossOIDs)
		cancel()
		if !plan.CloseReport.ConfirmedFlat() {
			if in.HLAddr != "" && in.HLFetcher != nil {
				postClosePositions, err := in.HLFetcher(in.HLAddr)
				if err != nil {
					plan.LogLines = append(plan.LogLines,
						fmt.Sprintf("[CRITICAL] hl-close: unable to verify HL state after close error: %v", err))
				} else if verified := clearVerifiedFlatHLErrors(&plan.CloseReport, postClosePositions); len(verified) > 0 {
					plan.LogLines = append(plan.LogLines,
						fmt.Sprintf("[INFO] hl-close: verified flat after close error: %v", verified))
				}
			}
		}
		plan.LogLines = append(plan.LogLines,
			recoverHyperliquidAlreadyFlatFills(&plan.CloseReport, hlPositions, in.HLNoFillRecoverer, recoverSince)...)
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
		if len(plan.CloseReport.Unconfigured) > 0 {
			plan.Unconfigured = append(plan.Unconfigured, plan.CloseReport.Unconfigured...)
			sort.Slice(plan.Unconfigured, func(i, j int) bool { return plan.Unconfigured[i].Coin < plan.Unconfigured[j].Coin })
			plan.OnChainConfirmedFlat = false
			for _, p := range plan.Unconfigured {
				plan.LogLines = append(plan.LogLines,
					fmt.Sprintf("[CRITICAL] hl-close: on-chain position for unconfigured coin %s (szi=%.6f) — manual intervention required, kill switch will retry next cycle", p.Coin, p.Size))
			}
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

	plan.OKXSpotPresent = len(in.OKXLiveAllSpot) > 0
	if plan.OKXSpotPresent {
		plan.LogLines = append(plan.LogLines,
			fmt.Sprintf("[CRITICAL] okx-close: %d live OKX spot strategies configured — kill switch cannot auto-close spot (no reduce-only); operator must verify manually (#345)", len(in.OKXLiveAllSpot)))
	}

	switch {
	case len(in.OKXLiveAllPerps) > 0 && in.OKXFetcher == nil:
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

	plan.RHOptionsPresent = len(in.RHLiveOptions) > 0
	if plan.RHOptionsPresent {
		plan.LogLines = append(plan.LogLines,
			fmt.Sprintf("[CRITICAL] rh-close: %d live Robinhood options strategies configured — kill switch cannot auto-close options (sell-to-close vs buy-to-close semantics); operator must verify manually (#346)", len(in.RHLiveOptions)))
	}

	switch {
	case len(in.RHLiveCrypto) > 0 && in.RHFetcher == nil:
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

	switch {
	case len(in.TSLiveAll) > 0 && in.TSFetcher == nil:
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

func collectHLKillSwitchStopOIDs(strategies map[string]*StrategyState, roster []StrategyConfig) map[string][]int64 {
	out := map[string][]int64{}
	for _, sc := range roster {
		sym := hyperliquidRawCoin(sc)
		if sym == "" {
			continue
		}
		ss, ok := strategies[sc.ID]
		if !ok || ss == nil {
			continue
		}
		pos := hlVirtualPositionFor(ss, sc, sym)
		if pos == nil {
			continue
		}
		out[sym] = appendUniquePositiveStopLossOID(out[sym], pos.StopLossOID)
		for _, tpOID := range pos.TPOIDs {
			out[sym] = appendUniquePositiveStopLossOID(out[sym], tpOID)
		}
	}
	return out
}

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
		if len(plan.LimitOrderReport.Cancelled) > 0 {
			parts = append(parts, fmt.Sprintf("cancelled resting limit orders: %s", strings.Join(plan.LimitOrderReport.Cancelled, ", ")))
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

	if len(plan.LimitOrderReport.Unresolved) > 0 {
		segments = append(segments, "Resting limit orders NOT confirmed cancelled (they can still fill and re-enter) — "+strings.Join(plan.LimitOrderReport.Unresolved, "; "))
	}
	if len(plan.LimitOrderReport.Cancelled) > 0 {
		segments = append(segments, "Cancelled resting limit orders — "+strings.Join(plan.LimitOrderReport.Cancelled, ", "))
	}
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
		segments = append(segments, "Could not fetch on-chain state to confirm flat")
	}

	return fmt.Sprintf("**PORTFOLIO KILL SWITCH (LATCHED, RETRYING)**\n%s\n%s. Virtual state preserved. Next cycle will retry.", portfolioReason, strings.Join(segments, " | "))
}

func killSwitchInstanceLabel(configPath string) string {
	dir := filepath.Base(filepath.Dir(configPath))
	if dir != "" && dir != "." && dir != string(filepath.Separator) {
		return dir
	}
	if host, err := os.Hostname(); err == nil && host != "" {
		return host
	}
	return "go-trader"
}

func formatKillSwitchResetPrompt(instanceLabel, hlAddr string, plan KillSwitchClosePlan) string {
	identity := instanceLabel
	if hlAddr != "" {
		identity = fmt.Sprintf("%s (Hyperliquid %s)", identity, hlAddr)
	}
	resetNote := "Replying 'reset' only clears the kill switch latch so trading can resume next cycle — it does not itself close or protect any position."
	if !plan.OnChainConfirmedFlat {
		resetNote += " On-chain close is still retrying and resting stop-losses may already be cancelled ahead of the flatten attempt — verify positions manually before assuming they're protected."
	}
	return fmt.Sprintf("[KILL SWITCH] %s\n%s\n\n%s\nReply 'reset' to proceed.", identity, plan.DiscordMessage, resetNote)
}
