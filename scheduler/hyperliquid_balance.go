package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// HLPosition represents an on-chain Hyperliquid perps position.
type HLPosition struct {
	Coin       string
	Size       float64 // signed: positive = long, negative = short
	EntryPrice float64
	Leverage   float64 // on-chain leverage value (#254)
}

// hlReconcileSLFillConfirmed mirrors the #685 sole-owner vanish gate for
// shared-coin Detectors 1 and 2: attribute hl_sync_stop_loss only when
// userFills confirms this exact SL OID filled with positive size (#756).
func hlReconcileSLFillConfirmed(lookup HLFillLookup, useFillFee bool, stopLossOID int64) bool {
	return useFillFee && lookup.OID == stopLossOID && lookup.FilledQty > 1e-9
}

var hlMainnetURL = "https://api.hyperliquid.xyz"

// hyperliquidLiveCloseScript is the path to the Python close helper. Exposed as
// a var so tests can substitute. Path is repo-relative because the scheduler is
// invoked from the repo root (same convention as other shared_scripts paths).
var hyperliquidLiveCloseScript = "shared_scripts/close_hyperliquid_position.py"

// HyperliquidLiveCloser submits a reduce-only market close for a single coin
// and returns the parsed result. Exposed as a function variable so tests can
// inject a fake without spawning a real Python subprocess. Production
// implementation is defaultHyperliquidLiveCloser, which shells out to
// close_hyperliquid_position.py via RunHyperliquidClose.
// When partialSz is nil, the full on-chain position is closed (#341). When
// non-nil, submits a partial close for that coin quantity (#356). When
// cancelStopLossOIDs is non-empty, the script also cancels those resting
// trigger orders before the close so per-strategy SL slots are freed (#421).
type HyperliquidLiveCloser func(symbol string, partialSz *float64, cancelStopLossOIDs []int64) (*HyperliquidCloseResult, error)

// defaultHyperliquidLiveCloser is the production close implementation. Writes
// stderr to os.Stderr rather than a per-strategy logger — kill switch is a
// system-level event, not strategy-scoped. Relies on RunHyperliquidClose's
// uniform error contract: any non-nil err means the close was not confirmed
// by the SDK and the kill switch must stay latched.
func defaultHyperliquidLiveCloser(symbol string, partialSz *float64, cancelStopLossOIDs []int64) (*HyperliquidCloseResult, error) {
	result, stderr, err := RunHyperliquidClose(hyperliquidLiveCloseScript, symbol, partialSz, cancelStopLossOIDs)
	if stderr != "" {
		fmt.Fprintf(os.Stderr, "[hl-close] %s stderr: %s\n", symbol, stderr)
	}
	return result, err
}

// fetchHyperliquidBalance fetches the live USDC balance (accountValue) from
// the Hyperliquid clearinghouseState endpoint for a given address.
// Returns 0 and a non-nil error if the request fails or the response is unexpected.
func fetchHyperliquidBalance(accountAddress string) (float64, error) {
	payload := map[string]string{
		"type": "clearinghouseState",
		"user": accountAddress,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, fmt.Errorf("marshal request: %w", err)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(hlMainnetURL+"/info", "application/json", bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("http %d from %s", resp.StatusCode, hlMainnetURL)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, fmt.Errorf("read response: %w", err)
	}

	var result struct {
		MarginSummary struct {
			AccountValue string `json:"accountValue"`
		} `json:"marginSummary"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return 0, fmt.Errorf("parse response: %w", err)
	}

	val, err := strconv.ParseFloat(result.MarginSummary.AccountValue, 64)
	if err != nil {
		return 0, fmt.Errorf("parse accountValue %q: %w", result.MarginSummary.AccountValue, err)
	}
	return val, nil
}

// okxBalanceScript is the path to the Python balance fetcher. Exposed as a
// var so tests can substitute.
var okxBalanceScript = "shared_scripts/fetch_okx_balance.py"

// defaultSharedWalletBalance dispatches a real on-chain balance lookup by
// platform name for use with ClearLatchedKillSwitchSharedWallet (#244).
// Returns an error for any platform that does not (yet) expose a real
// balance endpoint, so callers preserve the kill switch on uncertainty.
func defaultSharedWalletBalance(platform string) (float64, error) {
	switch platform {
	case "hyperliquid":
		addr := os.Getenv("HYPERLIQUID_ACCOUNT_ADDRESS")
		if addr == "" {
			return 0, fmt.Errorf("HYPERLIQUID_ACCOUNT_ADDRESS not set")
		}
		return fetchHyperliquidBalance(addr)
	case "okx":
		// #360 phase 2 of #357: unlocks multi-strategy OKX portfolio value
		// correctness. fetch_okx_balance.py reads the CCXT-unified USDT
		// total for the configured API key account.
		if os.Getenv("OKX_API_KEY") == "" {
			return 0, fmt.Errorf("OKX_API_KEY not set")
		}
		result, stderr, err := RunOKXFetchBalance(okxBalanceScript)
		if stderr != "" {
			fmt.Fprintf(os.Stderr, "[okx-balance] stderr: %s\n", stderr)
		}
		if err != nil {
			return 0, err
		}
		return result.Balance, nil
	}
	return 0, fmt.Errorf("no shared-wallet balance fetcher for platform %q", platform)
}

// syncHyperliquidLiveCapital is a no-op kept for backward compatibility.
// Capital is now managed per-strategy via config (Capital field) or capital_pct.
// With multiple strategies on one account, overriding each strategy's capital
// with the full wallet balance would double-count funds.
func syncHyperliquidLiveCapital(sc *StrategyConfig) {
	// Intentionally empty — capital is set from config or resolveCapitalPct.
}

// fetchHyperliquidState fetches the account value and open positions from the
// Hyperliquid clearinghouseState endpoint in a single API call.
func fetchHyperliquidState(accountAddress string) (float64, []HLPosition, error) {
	payload := map[string]string{
		"type": "clearinghouseState",
		"user": accountAddress,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, nil, fmt.Errorf("marshal request: %w", err)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(hlMainnetURL+"/info", "application/json", bytes.NewReader(body))
	if err != nil {
		return 0, nil, fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, nil, fmt.Errorf("http %d from %s", resp.StatusCode, hlMainnetURL)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, fmt.Errorf("read response: %w", err)
	}

	var result struct {
		MarginSummary struct {
			AccountValue string `json:"accountValue"`
		} `json:"marginSummary"`
		AssetPositions []struct {
			Position struct {
				Coin     string `json:"coin"`
				Szi      string `json:"szi"`
				EntryPx  string `json:"entryPx"`
				Leverage struct {
					Type  string      `json:"type"`
					Value json.Number `json:"value"`
				} `json:"leverage"`
			} `json:"position"`
		} `json:"assetPositions"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return 0, nil, fmt.Errorf("parse response: %w", err)
	}

	balance, err := strconv.ParseFloat(result.MarginSummary.AccountValue, 64)
	if err != nil {
		return 0, nil, fmt.Errorf("parse accountValue %q: %w", result.MarginSummary.AccountValue, err)
	}

	var positions []HLPosition
	for _, ap := range result.AssetPositions {
		szi, err := strconv.ParseFloat(ap.Position.Szi, 64)
		if err != nil || szi == 0 {
			continue
		}
		entryPx, err := strconv.ParseFloat(ap.Position.EntryPx, 64)
		if err != nil {
			fmt.Printf("[WARN] hl-sync: failed to parse entryPx %q for %s: %v\n", ap.Position.EntryPx, ap.Position.Coin, err)
		}
		// #254: HL per-position leverage from clearinghouseState. Value is a
		// number in the API but tolerated as string; default 1 on parse error.
		lev := 1.0
		if lvStr := ap.Position.Leverage.Value.String(); lvStr != "" {
			if parsed, lerr := strconv.ParseFloat(lvStr, 64); lerr == nil && parsed > 0 {
				lev = parsed
			}
		}
		positions = append(positions, HLPosition{
			Coin:       ap.Position.Coin,
			Size:       szi,
			EntryPrice: entryPx,
			Leverage:   lev,
		})
	}

	return balance, positions, nil
}

// reconcileHyperliquidPositions applies on-chain position data to a single StrategyState.
// It updates or removes the position for the given symbol based on ownership.
// Does NOT sync cash (each strategy manages its own virtual cash).
// Returns true if any state was changed. Must be called under Lock.
//
// accountAddress is used to resolve the on-chain fill fee via userFills when
// the reconciler detects an external close (#588). Empty disables the
// lookup — the close still books with the modeled fee. This entry point
// performs the lookup synchronously; production reconcile paths build a
// cached resolver outside the lock and call
// reconcileHyperliquidPositionsWithResolver.
func reconcileHyperliquidPositions(stratState *StrategyState, sym string, positions []HLPosition, accountAddress string, logger *StrategyLogger) bool {
	return reconcileHyperliquidPositionsWithResolver(stratState, sym, positions, directHyperliquidReconcileFillResolver(accountAddress), logger, nil)
}

// reconcileHyperliquidPositionsForStrategy is the production entry point with
// strategy-config awareness. It first attempts to attribute partial / full
// closes to a cleared TP tier (sole-owner mirror of shared-coin Detector 3 —
// ambiguity is moot here because exactly one strategy owns sym), books the
// close at the configured TP price (or the userFills px when available), and
// only falls through to the legacy quantity-resync / SL-fallback path when no
// TP attribution is found.
//
// Without this hook, partial TP fills on a sole-owner perps strategy are
// silently absorbed by the legacy reconciler — qty resync hides the close,
// no Trade row is written, s.Cash drifts from the on-chain account value, and
// the operator never sees a DM. Full closes attributable to the final TP tier
// were also booked at the SL trigger price (when SLOID was still set) instead
// of the TP price (#670).
//
// pendingAlerts (when non-nil) collects ProtectionFillAlert entries for owner
// DM emission after mu.Unlock — same pattern shared-coin detectors use so HTTP
// notifier calls don't extend the locked critical section.
func reconcileHyperliquidPositionsForStrategy(
	sc StrategyConfig,
	stratState *StrategyState,
	sym string,
	positions []HLPosition,
	resolveFee hlReconcileFillResolver,
	logger *StrategyLogger,
	pendingAlerts *[]ProtectionFillAlert,
) bool {
	if stratState == nil || sym == "" {
		return false
	}

	if booked := tryBookSoleOwnerTPFill(sc, stratState, sym, positions, resolveFee, logger, pendingAlerts); booked {
		// pos.Quantity has been shrunk (partial) or the position removed (full)
		// by the booker; the legacy reconciler's qty/side/avgCost resync will
		// no-op because virtual now matches on-chain. Continue through it for
		// idempotent housekeeping (multiplier migration, leverage seed).
		reconcileHyperliquidPositionsWithResolver(stratState, sym, positions, resolveFee, logger, pendingAlerts)
		return true
	}

	return reconcileHyperliquidPositionsWithResolver(stratState, sym, positions, resolveFee, logger, pendingAlerts)
}

// stampSoleOwnerRecoveryTierConsumed mirrors post-protection-sync state for a
// tier that tryBookSoleOwnerTPFill attributed via the cycle-ordering recovery
// path (#758). Without this, pos.TPOIDs[i] stays positive until Python
// protection-sync runs, and hlAttemptCloseFromTPFills on a later vanish
// snapshot can book the same TP OID again.
func stampSoleOwnerRecoveryTierConsumed(pos *Position, tierIdx int) {
	if pos == nil || tierIdx < 0 {
		return
	}
	n := len(pos.TPOIDs)
	if tierIdx >= n {
		return
	}
	if len(pos.TPArmedTiers) < n {
		ext := make([]bool, n)
		copy(ext, pos.TPArmedTiers)
		pos.TPArmedTiers = ext
	}
	pos.TPOIDs[tierIdx] = 0
	pos.TPArmedTiers[tierIdx] = true
}

// tryBookSoleOwnerTPFill is the sole-owner TP attribution helper. Returns true
// when a TP-tier fill was detected and booked via
// recordPerpsExternalPartialCloseWithFillFee — covers both the partial-drop
// case (on-chain qty < virtual qty, same direction) and the full-close case
// (on-chain flat, ALL TP tiers cleared). When no TP attribution applies,
// returns false so the caller falls through to the legacy reconciler.
//
// Two attribution paths handle the cycle-ordering interaction with
// applyHyperliquidProtectionSync (which runs in the per-strategy phase, AFTER
// this pre-phase reconcile):
//
//  1. Cleared-tier path — pos.TPOIDs[i]==0, set by applyHyperliquidProtectionSync
//     after Python observes the userFills entry for that OID. Reliable signal
//     but lags the fill by one cycle.
//  2. Cycle-ordering recovery path — pos.TPOIDs[i] still positive but the
//     userFills resolver returns a matched fill whose OID equals one of the
//     configured TPOIDs. Closes the (protection-sync, next-reconcile) window
//     where legacy resync would otherwise wipe the drift signal before
//     protection-sync zeros the TPOID. Restricted to the partial path:
//     full-close attribution still requires all-tiers-cleared per finding #1.
//
// Precision: the recovery path's OID match against pos.TPOIDs is exact, so SL
// fills (lookup.OID == pos.StopLossOID) and operator/CB closes (different OID)
// don't mis-attribute. The booker shrinks pos.Quantity to match on-chain so a
// later same-cycle protection-sync sees the fill, zeros TPOIDs[i] normally,
// and the next cycle's reconcile finds no drift.
//
// Cycle-ordering recovery additionally stamps the consumed tier immediately
// (#758) so a later vanish reconcile cannot re-book the same TP OID in
// hlAttemptCloseFromTPFills while pos.TPOIDs[i] would otherwise still be
// positive until applyHyperliquidProtectionSync runs.
func tryBookSoleOwnerTPFill(
	sc StrategyConfig,
	stratState *StrategyState,
	sym string,
	positions []HLPosition,
	resolveFee hlReconcileFillResolver,
	logger *StrategyLogger,
	pendingAlerts *[]ProtectionFillAlert,
) bool {
	statePos := stratState.Positions[sym]
	if statePos == nil || statePos.Quantity <= 0 {
		return false
	}
	if statePos.AvgCost <= 0 || statePos.EntryATR <= 0 {
		// TP price computation needs AvgCost + EntryATR; without them we
		// can't attribute. Fall back to the legacy reconciler.
		return false
	}

	var onChainPos *HLPosition
	for i := range positions {
		if positions[i].Coin == sym {
			onChainPos = &positions[i]
			break
		}
	}

	var closeQty float64
	if onChainPos == nil {
		// Full-close path: only attribute to a TP tier when ALL configured TP
		// OIDs are zero (i.e. final tier flatten — the "all tiers gone"
		// branch of hyperliquidClearedTPTier). If any tier is still active, a
		// later SL fire / operator close / kill-switch on the residual after a
		// prior partial TP fill (state TPOIDs=[0, 222], Quantity=residual)
		// would otherwise be mis-attributed to the already-booked tier — wrong
		// price on the trade record AND wrong TP{n} label on the DM alert.
		// Defer those cases to the legacy SL-owner branch in
		// reconcileHyperliquidPositionsWithResolver.
		tiers := strategyTPTiersForRegime(sc, statePos.Regime)
		tpOIDs := tpOIDsForTierCount(statePos.TPOIDs, len(tiers))
		for _, oid := range tpOIDs {
			if oid > 0 {
				return false
			}
		}
		closeQty = statePos.Quantity
	} else {
		onChainAbs := math.Abs(onChainPos.Size)
		sameDirection := (onChainPos.Size > 0 && statePos.Side == "long") ||
			(onChainPos.Size < 0 && statePos.Side == "short")
		if !sameDirection {
			return false
		}
		if onChainAbs+1e-9 >= statePos.Quantity {
			return false
		}
		closeQty = statePos.Quantity - onChainAbs
	}
	if closeQty <= 1e-9 {
		return false
	}

	lookup, useFillFee := resolveFee(sym, 0, closeQty)
	logHyperliquidReconcileFillLookup(logger, sym, 0, closeQty, lookup, useFillFee)

	var soleOwnerRecoveryBook bool
	tierIdx, hasCleared := hyperliquidClearedTPTier(sc, statePos, closeQty)
	if !hasCleared {
		// Cycle-ordering recovery (#672): protection-sync hasn't yet zeroed
		// pos.TPOIDs[i] for the freshly-filled tier. Cross-check the userFills
		// lookup — if the matched fill's OID equals one of the configured
		// TPOIDs, we know which tier fired without waiting for protection-sync.
		// Restricted to the partial path: full-close attribution still requires
		// all-tiers-cleared per finding #1.
		if onChainPos == nil || !useFillFee || lookup.OID <= 0 {
			return false
		}
		tiers := strategyTPTiersForRegime(sc, statePos.Regime)
		if len(tiers) == 0 {
			return false
		}
		tpOIDs := tpOIDsForTierCount(statePos.TPOIDs, len(tiers))
		matched := -1
		for i, oid := range tpOIDs {
			if oid > 0 && oid == lookup.OID {
				matched = i
				break
			}
		}
		if matched < 0 {
			return false
		}
		tierIdx = matched
		soleOwnerRecoveryBook = true
	}

	tpPrices := tieredTPATRPricesForRegime(sc, statePos.Side, statePos.AvgCost, statePos.EntryATR, statePos.Regime)
	tpPrice := 0.0
	if tierIdx >= 0 && tierIdx < len(tpPrices) {
		tpPrice = tpPrices[tierIdx]
	}

	closePx := tpPrice
	if useFillFee && lookup.Px > 0 {
		closePx = lookup.Px
	}
	if closePx <= 0 {
		return false
	}

	exchangeOrderID := ""
	if useFillFee && lookup.OID > 0 {
		exchangeOrderID = strconv.FormatInt(lookup.OID, 10)
	}

	alertSide := statePos.Side
	if !recordPerpsExternalPartialCloseWithFillFee(
		stratState, sym, closeQty, closePx, lookup.Fee, useFillFee,
		exchangeOrderID, "hl_sync_external_partial", logger,
	) {
		return false
	}
	posAfter := stratState.Positions[sym]
	if soleOwnerRecoveryBook && posAfter != nil {
		stampSoleOwnerRecoveryTierConsumed(posAfter, tierIdx)
	}

	remaining := 0.0
	if posAfter != nil {
		remaining = posAfter.Quantity
	}
	if pendingAlerts != nil {
		// lastBookedTradePnL relies on the just-completed RecordTrade inside
		// the booker; do not insert another RecordTrade between here and the
		// booker call.
		*pendingAlerts = append(*pendingAlerts, ProtectionFillAlert{
			StrategyID:      sc.ID,
			Symbol:          sym,
			Side:            alertSide,
			FillType:        tpTierLabel(tierIdx),
			IsPartial:       remaining > 1e-9,
			FillPrice:       closePx,
			CloseQty:        closeQty,
			RemainingQty:    remaining,
			RealizedPnL:     lastBookedTradePnL(stratState),
			HasPnL:          true,
			ExchangeOrderID: exchangeOrderID,
		})
	}
	return true
}

// reconcileHyperliquidPositionsWithResolver is the resolver-aware variant. The
// resolver is expected to do pure in-memory cache reads when called under
// mu.Lock() (see buildCachedHyperliquidReconcileFillResolver) — never make
// HTTP calls.
//
// When pendingAlerts is non-nil, hlAttemptCloseFromTPFills appends TP fill
// alerts here (same contract as tryBookSoleOwnerTPFill) for owner DM flush
// after mu.Unlock (#757 re-review).
func reconcileHyperliquidPositionsWithResolver(stratState *StrategyState, sym string, positions []HLPosition, resolveFee hlReconcileFillResolver, logger *StrategyLogger, pendingAlerts *[]ProtectionFillAlert) bool {
	changed := false

	// Find the on-chain position for this strategy's symbol.
	var onChainPos *HLPosition
	for i := range positions {
		if positions[i].Coin == sym {
			onChainPos = &positions[i]
			break
		}
	}

	statePos := stratState.Positions[sym]

	if onChainPos != nil && statePos != nil {
		// Both exist — reconcile quantity/side if they differ.
		qty := math.Abs(onChainPos.Size)
		side := "long"
		if onChainPos.Size < 0 {
			side = "short"
		}
		if statePos.Quantity != qty || statePos.Side != side {
			logger.Info("hl-sync: reconciled %s: state=%.6f %s → on-chain=%.6f %s @ $%.2f",
				sym, statePos.Quantity, statePos.Side, qty, side, onChainPos.EntryPrice)
			statePos.Quantity = qty
			statePos.Side = side
			statePos.AvgCost = onChainPos.EntryPrice
			changed = true
		}
		// #254: always pull the current on-chain leverage and ensure Multiplier=1
		// so PortfolioValue uses the PnL branch. Also migrates legacy positions
		// that were stored with Multiplier=0 (treated as spot/full-notional).
		if statePos.Multiplier != 1 {
			logger.Info("hl-sync: %s migrate multiplier %v → 1 (perps PnL valuation) (#254)", sym, statePos.Multiplier)
			statePos.Multiplier = 1
			changed = true
		}
		// #418: only seed leverage from on-chain when the virtual position has
		// none yet (Leverage==0 → legacy/uninitialised). The entry path sets
		// Leverage from sc.Leverage (config); the exchange's account-wide
		// margin tier can differ (e.g. HL allows up to 20x while the trader
		// sized at 2x) and unconditionally overwriting it inflates the
		// perpsMarginDrawdownInputs denominator and can re-fire the circuit
		// breaker spuriously. Defense in depth — risk math also reads
		// sc.Leverage now, so this is belt-and-suspenders against any future
		// consumer that reads pos.Leverage directly.
		if onChainPos.Leverage > 0 && statePos.Leverage == 0 {
			logger.Info("hl-sync: %s leverage init → %v (from on-chain, legacy/zero-value position)", sym, onChainPos.Leverage)
			statePos.Leverage = onChainPos.Leverage
			changed = true
		}
	} else if onChainPos == nil && statePos != nil {
		// Position in state but not on-chain — closed externally.
		logger.Info("hl-sync: %s position (%.6f %s) no longer on-chain, removing",
			sym, statePos.Quantity, statePos.Side)
		// #673: When the position has TP OIDs and the SL OID has no fills,
		// the position was flattened by TPs (HL auto-cancels the resting
		// reduce-only SL once flat). Book each TP fill at its actual price
		// rather than mis-attributing to the SL trigger price.
		if hlAttemptCloseFromTPFills(stratState, sym, statePos, resolveFee, logger, pendingAlerts) {
			return true
		}
		if statePos.StopLossOID > 0 && statePos.StopLossTriggerPx > 0 {
			lookup, useFillFee := resolveFee(sym, statePos.StopLossOID, statePos.Quantity)
			oidStr := strconv.FormatInt(statePos.StopLossOID, 10)
			logHyperliquidReconcileFillLookup(logger, sym, statePos.StopLossOID, statePos.Quantity, lookup, useFillFee)
			// #685: Only book as SL when userFills confirms the SL OID actually
			// filled. Without this gate, a TP-fired close whose TPOIDs have all
			// been zeroed by a prior applyHyperliquidProtectionSync cycle (so
			// hlAttemptCloseFromTPFills above returns false) lands here and gets
			// mis-attributed to the cancelled SL at its trigger price. Match must
			// be by exact OID; the coin+size fallback can spuriously hit a TP
			// fill of the same size.
			slConfirmed := hlReconcileSLFillConfirmed(lookup, useFillFee, statePos.StopLossOID)
			if slConfirmed {
				// #621: When userFills returned a real fill qty smaller than the virtual
				// position (e.g. SL was placed at the on-chain size after a manual TP
				// reduced the position), use the actual fill qty so PnL/cash are correct.
				if lookup.FilledQty < statePos.Quantity-1e-9 {
					logger.Info("hl-sync: %s SL close qty adjusted %.6f → %.6f (actual fill from userFills)", sym, statePos.Quantity, lookup.FilledQty)
					statePos.Quantity = lookup.FilledQty
				}
				if recordPerpsStopLossCloseWithFillFee(stratState, sym, statePos.StopLossTriggerPx, lookup.Fee, useFillFee, oidStr, "stop_loss", logger) {
					return true
				}
			} else if useFillFee {
				// #685 log clarity: the lookup hit (logged above) matched
				// something other than this SL OID (e.g. a coin+size fallback
				// onto a TP fill), so this close is NOT being booked as SL.
				logger.Info("hl-sync: %s SL OID %s unfilled — routing to hl_sync_external (matched oid=%d qty=%.6f)", sym, oidStr, lookup.OID, lookup.FilledQty)
			}
		}
		// Close price is unknown — the fill happened off-scheduler between
		// reconcile cycles. Record 0 in both fields; downstream analytics
		// that compute avg close price / slippage must filter
		// close_reason != 'hl_sync_external' to avoid biased aggregates.
		recordClosedPosition(stratState, statePos, 0, 0, "hl_sync_external", time.Now().UTC())
		delete(stratState.Positions, sym)
		clearATRMultMissingEntryATRWarningOnHLPerpsClose(stratState, sym)
		changed = true
	}
	// If on-chain exists but NOT in this strategy's state, we skip it —
	// it either belongs to another strategy or is an unowned manual trade.

	return changed
}

// syncHyperliquidAccountPositions fetches on-chain positions once and reconciles
// them across all live HL strategies using ownership tracking. Positions are only
// assigned to the strategy that opened them (via OwnerStrategyID).
// Unowned on-chain positions are logged as warnings but not assigned.
// Must be called WITHOUT holding any lock; acquires Lock internally.
//
// This is the self-contained entry point that fetches its own state. When the
// scheduler has already fetched clearinghouseState earlier in the cycle (e.g.
// for shared-wallet balance), use reconcileHyperliquidAccountPositions instead
// to avoid a second round-trip to the HL API.
//
// hlStrategies must include ALL live HL strategies (not a subset) for shared-coin
// detection to work correctly. It is passed as both dueStrategies and allStrategies.
func syncHyperliquidAccountPositions(hlStrategies []StrategyConfig, state *AppState, mu *sync.RWMutex, logMgr *LogManager) bool {
	accountAddr := os.Getenv("HYPERLIQUID_ACCOUNT_ADDRESS")
	if accountAddr == "" {
		return false
	}

	// Fetch on-chain state once (no lock — I/O).
	_, positions, err := fetchHyperliquidState(accountAddr)
	if err != nil {
		fmt.Printf("[WARN] hl-sync: failed to fetch on-chain state: %v\n", err)
		return false
	}

	// Self-contained entry: due and all are the same list. Prices are
	// unavailable in this path (caller did not pre-fetch); external-close
	// PnL falls back to zero (legacy behavior pre-#584).
	// This entry point is used by --once and tests; alerts are suppressed
	// since no notifier is plumbed through here.
	return reconcileHyperliquidAccountPositions(hlStrategies, hlStrategies, state, mu, logMgr, positions, nil, accountAddr, nil, false)
}

// reconcileHyperliquidAccountPositions reconciles pre-fetched on-chain positions
// against strategy state. Use this when the caller has already fetched
// clearinghouseState earlier in the cycle (e.g. main.go fetches once for the
// shared-wallet balance and reuses the positions here to avoid a duplicate
// HTTP round-trip — see #243 review feedback).
//
// dueStrategies are the strategies to reconcile this cycle (subset of allStrategies).
// allStrategies includes every live HL strategy in the config — needed to detect
// shared coins (#258) even when only some strategies are due.
//
// prices supplies the current mark for each coin (keyed by HL coin symbol such
// as "BTC"). When an external close is detected for a non-SL-owner peer, the
// mark is used as the approximate close price so realized PnL can be credited
// to s.Cash (#584). Pass nil to fall back to the legacy zero-PnL recording.
//
// accountAddress is the HL account whose userFills are queried for real
// exchange fees on closes detected by the reconciler (#588). Pass an empty
// string to skip the lookup — closes still book correctly using the
// modeled fee.
//
// notifier and notifyTPSLFills control owner DMs emitted on TP/SL fill
// detection (#661). Pass a nil notifier to suppress alerts; pass false for
// notifyTPSLFills when the operator has explicitly opted out via
// `notify_tp_sl_fills: false`.
//
// Must be called WITHOUT holding any lock; acquires Lock internally.
func reconcileHyperliquidAccountPositions(dueStrategies, allStrategies []StrategyConfig, state *AppState, mu *sync.RWMutex, logMgr *LogManager, positions []HLPosition, prices map[string]float64, accountAddress string, notifier *MultiNotifier, notifyTPSLFills bool) bool {
	// Resolve userFills BEFORE taking mu.Lock(): each lookup can sleep up
	// to ~1.5s on indexer-lag retries, and holding the write lock blocks
	// every reader of state (/status, /health, per-strategy phase RLocks).
	// The resolver itself is a pure map read inside the locked region.
	resolveFee := buildCachedHyperliquidReconcileFillResolver(accountAddress, allStrategies, state, mu, positions)

	// pendingAlerts is populated under mu.Lock() at the three protection-fill
	// detection sites and drained AFTER mu.Unlock() so SendOwnerDM's blocking
	// HTTP calls don't extend the critical section. Defer ordering: the flush
	// closure is registered first, so it fires LAST — after defer mu.Unlock()
	// has already released the lock (defer runs LIFO).
	var pendingAlerts []ProtectionFillAlert
	defer func() {
		for _, a := range pendingAlerts {
			notifyProtectionFill(notifier, notifyTPSLFills, a)
		}
	}()

	mu.Lock()
	defer mu.Unlock()

	changed := false

	// Build coin → strategy IDs from ALL strategies (not just due) to detect
	// shared coins. A coin is "shared" when 2+ strategies are configured to
	// trade it on the same wallet. For shared coins, per-strategy reconciliation
	// is skipped to prevent the phantom drawdown described in #258: one strategy
	// selling causes the other's position to be removed by sync, collapsing its
	// portfolio value and tripping the circuit breaker.
	coinStrategies := make(map[string][]string)
	for _, sc := range allStrategies {
		sym := hyperliquidSymbol(sc.Args)
		if sym == "" {
			continue
		}
		coinStrategies[sym] = append(coinStrategies[sym], sc.ID)
	}
	strategyByID := make(map[string]StrategyConfig, len(allStrategies))
	for _, sc := range allStrategies {
		strategyByID[sc.ID] = sc
	}
	sharedCoins := make(map[string]bool)
	for coin, ids := range coinStrategies {
		if len(ids) > 1 {
			sharedCoins[coin] = true
		}
	}

	// Reconcile non-shared coins normally for due strategies.
	for _, sc := range dueStrategies {
		ss := state.Strategies[sc.ID]
		if ss == nil {
			continue
		}
		sym := hyperliquidSymbol(sc.Args)
		if sym == "" {
			continue
		}
		if sharedCoins[sym] {
			continue // handled below
		}
		logger, err := logMgr.GetStrategyLogger(sc.ID)
		if err != nil {
			fmt.Printf("[ERROR] hl-sync: logger for %s: %v\n", sc.ID, err)
			continue
		}
		if reconcileHyperliquidPositionsForStrategy(sc, ss, sym, positions, resolveFee, logger, &pendingAlerts) {
			changed = true
		}
	}

	// For shared coins: apply non-destructive updates (multiplier migration,
	// leverage sync) but do NOT modify quantities or remove positions. Compute
	// reconciliation gaps so the user can see drift via /status.
	now := time.Now().UTC()
	if state.ReconciliationGaps == nil {
		state.ReconciliationGaps = make(map[string]*ReconciliationGap)
	}
	for coin, stratIDs := range coinStrategies {
		if !sharedCoins[coin] {
			continue
		}

		// Find on-chain position for this coin.
		var onChainPos *HLPosition
		for i := range positions {
			if positions[i].Coin == coin {
				onChainPos = &positions[i]
				break
			}
		}

		virtualQty := 0.0
		for _, id := range stratIDs {
			ss := state.Strategies[id]
			if ss == nil {
				continue
			}
			pos := ss.Positions[coin]
			if pos == nil {
				continue
			}
			// Sum signed virtual qty.
			if pos.Side == "long" {
				virtualQty += pos.Quantity
			} else if pos.Side == "short" {
				virtualQty -= pos.Quantity
			} else {
				fmt.Printf("[WARN] hl-sync: strategy %s coin %s has unexpected side=%q, skipping in virtual qty\n", id, coin, pos.Side)
			}
			// Non-destructive updates applied to ALL strategies (not just due) since
			// multiplier migration and leverage sync are idempotent corrections that
			// should not wait for the strategy's next scheduled cycle.
			if pos.Multiplier != 1 {
				logger, err := logMgr.GetStrategyLogger(id)
				if err != nil {
					fmt.Printf("[ERROR] hl-sync: logger for %s: %v\n", id, err)
				} else {
					logger.Info("hl-sync: %s migrate multiplier %v → 1 (shared coin) (#254)", coin, pos.Multiplier)
				}
				pos.Multiplier = 1
				changed = true
			}
			// #418: same write-path guard as reconcileHyperliquidPositions —
			// only seed leverage from on-chain when virtual is zero-value.
			if onChainPos != nil && onChainPos.Leverage > 0 && pos.Leverage == 0 {
				logger, err := logMgr.GetStrategyLogger(id)
				if err != nil {
					fmt.Printf("[ERROR] hl-sync: logger for %s: %v\n", id, err)
				} else {
					logger.Info("hl-sync: %s leverage init → %v (shared coin, from on-chain)", coin, onChainPos.Leverage)
				}
				pos.Leverage = onChainPos.Leverage
				changed = true
			}
		}

		// Compute reconciliation gap.
		onChainQty := 0.0
		if onChainPos != nil {
			onChainQty = onChainPos.Size
		}
		delta := virtualQty - onChainQty

		// Detect and reconcile unambiguous shared-coin closes (#565).
		//
		// Detector 1 — full external close: on-chain is flat but virtual is not.
		// Covers stop-loss sweep of the aggregate position, manual close on HL UI,
		// and kill-switch closes that finish between scheduler cycles.
		//
		// Detector 2 — SL owner partial close: exactly one peer holds a resting
		// trigger (StopLossOID) and the on-chain residual matches the signed sum of
		// all non-owner peers' virtual qty. HL trigger orders are sized to the
		// owner's qty at arm time, so when the trigger fires the non-owner peers'
		// portion remains on-chain untouched.
		//
		// Detector 3 — TP partial fill: on-chain qty is a same-direction nonzero
		// subset of virtual qty, and exactly one same-side strategy has a cleared
		// on-chain TP tier. Book the virtual/on-chain delta as an external partial
		// close for that strategy, then shrink its virtual qty so the next
		// protection-sync cycle sizes SL/TP orders from the true residual (#609).
		//
		// All other qty mismatches (ambiguous gaps that #258/#515 protect) fall
		// through to the gap-recording block unchanged.
		if math.Abs(onChainQty) < 1e-6 && math.Abs(virtualQty) > 1e-6 {
			// Detector 1: everything gone on-chain — close all peers.
			//
			// Fee-lookup caveat for the multi-peer case: a single aggregated
			// UI close produces one userFills row sized at the *aggregate*
			// quantity, while each non-owner peer here queries with its own
			// per-strategy pos.Quantity. The hlReconcileFillSizeTolerance
			// (1e-4) won't accept that mismatch, so peers fall back to the
			// modeled fee. SL attribution additionally requires an OID-keyed
			// userFills hit matching Position.StopLossOID (#756); otherwise the
			// peer is closed as hl_sync_external (mark or zero PnL). Per-peer fee
			// accuracy on external closes is only achievable when each peer's qty
			// happens to equal the aggregate close size.
			for _, id := range stratIDs {
				ss := state.Strategies[id]
				if ss == nil {
					continue
				}
				pos := ss.Positions[coin]
				if pos == nil {
					continue
				}
				logger, logErr := logMgr.GetStrategyLogger(id)
				if logErr != nil {
					fmt.Printf("[ERROR] hl-sync: logger for %s: %v\n", id, logErr)
				}
				if pos.StopLossOID > 0 && pos.StopLossTriggerPx > 0 {
					lookup, useFillFee := resolveFee(coin, pos.StopLossOID, pos.Quantity)
					oidStr := strconv.FormatInt(pos.StopLossOID, 10)
					logHyperliquidReconcileFillLookup(logger, coin, pos.StopLossOID, pos.Quantity, lookup, useFillFee)
					slConfirmed := hlReconcileSLFillConfirmed(lookup, useFillFee, pos.StopLossOID)
					if slConfirmed {
						if lookup.FilledQty < pos.Quantity-1e-9 {
							if logger != nil {
								logger.Info("hl-sync: %s SL close qty adjusted %.6f → %.6f (actual fill from userFills)", coin, pos.Quantity, lookup.FilledQty)
							}
							pos.Quantity = lookup.FilledQty
						}
						// Snapshot alertSide/alertQty/alertTriggerPx before
						// recordPerpsStopLossCloseWithFillFee mutates state.
						// lastBookedTradePnL relies on the just-completed RecordTrade
						// inside the booker; do not insert another RecordTrade between
						// here and pendingAlerts append.
						alertSide := pos.Side
						alertQty := pos.Quantity
						alertTriggerPx := pos.StopLossTriggerPx
						if recordPerpsStopLossCloseWithFillFee(ss, coin, pos.StopLossTriggerPx, lookup.Fee, useFillFee, oidStr, "hl_sync_stop_loss", logger) {
							changed = true
							pendingAlerts = append(pendingAlerts, ProtectionFillAlert{
								StrategyID:      id,
								Symbol:          coin,
								Side:            alertSide,
								FillType:        "SL",
								IsPartial:       false,
								FillPrice:       alertTriggerPx,
								CloseQty:        alertQty,
								RemainingQty:    0,
								RealizedPnL:     lastBookedTradePnL(ss),
								HasPnL:          true,
								ExchangeOrderID: oidStr,
							})
						}
					} else {
						if useFillFee && logger != nil {
							logger.Info("hl-sync: %s Detector 1 SL OID %s unfilled — routing external (matched oid=%d qty=%.6f)", coin, oidStr, lookup.OID, lookup.FilledQty)
						} else if logger != nil {
							logger.Info("hl-sync: %s Detector 1 SL OID %s unfilled — routing external (userFills miss)", coin, oidStr)
						}
						if mark, ok := prices[coin]; ok && mark > 0 {
							lookupExt, useFillFeeExt := resolveFee(coin, 0, pos.Quantity)
							logHyperliquidReconcileFillLookup(logger, coin, 0, pos.Quantity, lookupExt, useFillFeeExt)
							if recordPerpsExternalCloseWithFillFee(ss, coin, mark, lookupExt.Fee, useFillFeeExt, "", "hl_sync_external", logger) {
								changed = true
							}
						} else {
							recordClosedPosition(ss, pos, 0, 0, "hl_sync_external", now)
							delete(ss.Positions, coin)
							clearATRMultMissingEntryATRWarningOnHLPerpsClose(ss, coin)
							if logger != nil {
								logger.Info("hl-sync: %s position (%.6f %s) no longer on-chain, removing (Detector 1 external close, no mark price)", coin, pos.Quantity, pos.Side)
							}
							changed = true
						}
					}
				} else if mark, ok := prices[coin]; ok && mark > 0 {
					// #584: credit s.Cash with mark-based PnL so the per-strategy
					// PortfolioValue (and the summary TOTAL) match the real HL
					// account after an external close. The mark is fetched at
					// cycle start, so cp.RealizedPnL is an *approximation* — it
					// will drift from the true on-chain fill price (which can be
					// minutes earlier). Do not treat the resulting Trade /
					// ClosedPosition rows as authoritative for tax or reporting;
					// they exist to keep cash bookkeeping in sync.
					lookup, useFillFee := resolveFee(coin, 0, pos.Quantity)
					logHyperliquidReconcileFillLookup(logger, coin, 0, pos.Quantity, lookup, useFillFee)
					if recordPerpsExternalCloseWithFillFee(ss, coin, mark, lookup.Fee, useFillFee, "", "hl_sync_external", logger) {
						changed = true
					}
				} else {
					// No mark price available — fall back to recording the
					// close with zero PnL. s.Cash will be stale until the
					// strategy reopens; tracked for follow-up if it matters.
					recordClosedPosition(ss, pos, 0, 0, "hl_sync_external", now)
					delete(ss.Positions, coin)
					clearATRMultMissingEntryATRWarningOnHLPerpsClose(ss, coin)
					if logger != nil {
						logger.Info("hl-sync: %s position (%.6f %s) no longer on-chain, removing (external close, no mark price)", coin, pos.Quantity, pos.Side)
					}
					changed = true
				}
			}
			virtualQty = 0.0
			delta = 0.0
		} else if math.Abs(delta) > 1e-6 {
			// Detector 2: partial drop — find the sole SL owner and check whether
			// the on-chain residual matches the expected post-fire remainder.
			var slOwnerID string
			var slOwnerPos *Position
			for _, id := range stratIDs {
				ss := state.Strategies[id]
				if ss == nil {
					continue
				}
				pos := ss.Positions[coin]
				if pos == nil {
					continue
				}
				if pos.StopLossOID > 0 && pos.StopLossTriggerPx > 0 {
					if slOwnerID != "" {
						// Multiple SL owners — ambiguous, skip both detectors.
						slOwnerID, slOwnerPos = "", nil
						break
					}
					slOwnerID, slOwnerPos = id, pos
				}
			}
			if slOwnerID != "" && slOwnerPos != nil {
				// Expected residual = signed virtual qty minus the owner's signed qty.
				expectedResidual := virtualQty
				if slOwnerPos.Side == "long" {
					expectedResidual -= slOwnerPos.Quantity
				} else {
					expectedResidual += slOwnerPos.Quantity
				}
				if math.Abs(onChainQty-expectedResidual) < 1e-6 {
					ownerSS := state.Strategies[slOwnerID]
					if ownerSS != nil {
						logger, logErr := logMgr.GetStrategyLogger(slOwnerID)
						if logErr != nil {
							fmt.Printf("[ERROR] hl-sync: logger for %s: %v\n", slOwnerID, logErr)
						}
						lookup, useFillFee := resolveFee(coin, slOwnerPos.StopLossOID, slOwnerPos.Quantity)
						oidStr := strconv.FormatInt(slOwnerPos.StopLossOID, 10)
						logHyperliquidReconcileFillLookup(logger, coin, slOwnerPos.StopLossOID, slOwnerPos.Quantity, lookup, useFillFee)
						slConfirmed := hlReconcileSLFillConfirmed(lookup, useFillFee, slOwnerPos.StopLossOID)
						if slConfirmed {
							if lookup.FilledQty < slOwnerPos.Quantity-1e-9 {
								if logger != nil {
									logger.Info("hl-sync: %s SL close qty adjusted %.6f → %.6f (actual fill from userFills)", coin, slOwnerPos.Quantity, lookup.FilledQty)
								}
								slOwnerPos.Quantity = lookup.FilledQty
							}
							// Snapshot alertSide/alertQty/alertTriggerPx before
							// recordPerpsStopLossCloseWithFillFee mutates state.
							// lastBookedTradePnL relies on the just-completed RecordTrade
							// inside the booker; do not insert another RecordTrade between
							// here and pendingAlerts append.
							alertSide := slOwnerPos.Side
							alertQty := slOwnerPos.Quantity
							alertTriggerPx := slOwnerPos.StopLossTriggerPx
							if recordPerpsStopLossCloseWithFillFee(ownerSS, coin, slOwnerPos.StopLossTriggerPx, lookup.Fee, useFillFee, oidStr, "hl_sync_stop_loss", logger) {
								changed = true
								virtualQty = expectedResidual
								delta = virtualQty - onChainQty
								pendingAlerts = append(pendingAlerts, ProtectionFillAlert{
									StrategyID:      slOwnerID,
									Symbol:          coin,
									Side:            alertSide,
									FillType:        "SL",
									IsPartial:       false,
									FillPrice:       alertTriggerPx,
									CloseQty:        alertQty,
									RemainingQty:    0,
									RealizedPnL:     lastBookedTradePnL(ownerSS),
									HasPnL:          true,
									ExchangeOrderID: oidStr,
								})
							}
						} else {
							if useFillFee && logger != nil {
								logger.Info("hl-sync: %s Detector 2 SL OID %s unfilled — routing external (matched oid=%d qty=%.6f)", coin, oidStr, lookup.OID, lookup.FilledQty)
							} else if logger != nil {
								logger.Info("hl-sync: %s Detector 2 SL OID %s unfilled — routing external (userFills miss)", coin, oidStr)
							}
							if mark, ok := prices[coin]; ok && mark > 0 {
								lookupExt, useFillFeeExt := resolveFee(coin, 0, slOwnerPos.Quantity)
								logHyperliquidReconcileFillLookup(logger, coin, 0, slOwnerPos.Quantity, lookupExt, useFillFeeExt)
								if recordPerpsExternalCloseWithFillFee(ownerSS, coin, mark, lookupExt.Fee, useFillFeeExt, "", "hl_sync_external", logger) {
									changed = true
									virtualQty = expectedResidual
									delta = virtualQty - onChainQty
								}
							} else {
								recordClosedPosition(ownerSS, slOwnerPos, 0, 0, "hl_sync_external", now)
								delete(ownerSS.Positions, coin)
								clearATRMultMissingEntryATRWarningOnHLPerpsClose(ownerSS, coin)
								if logger != nil {
									logger.Info("hl-sync: %s position (%.6f %s) no longer on-chain, removing (Detector 2 external close, no mark price)", coin, slOwnerPos.Quantity, slOwnerPos.Side)
								}
								changed = true
								virtualQty = expectedResidual
								delta = virtualQty - onChainQty
							}
						}
					}
				}
			}
			if math.Abs(delta) > 1e-6 {
				if closeSide, closeQty, ok := hyperliquidSharedPartialCloseDrift(virtualQty, onChainQty); ok {
					var candidateID string
					var candidateSS *StrategyState
					var candidatePos *Position
					var candidateTierIdx int
					for _, id := range stratIDs {
						ss := state.Strategies[id]
						if ss == nil {
							continue
						}
						pos := ss.Positions[coin]
						if pos == nil || pos.Side != closeSide {
							continue
						}
						sc, ok := strategyByID[id]
						if !ok {
							continue
						}
						tierIdx, hasCleared := hyperliquidClearedTPTier(sc, pos, closeQty)
						if !hasCleared {
							continue
						}
						if candidateID != "" {
							// Multiple TP owners changed in the same window; leave the
							// aggregate gap visible rather than guessing the allocation.
							candidateID, candidateSS, candidatePos = "", nil, nil
							break
						}
						candidateID, candidateSS, candidatePos, candidateTierIdx = id, ss, pos, tierIdx
					}
					if candidateID != "" && candidateSS != nil && candidatePos != nil && closeQty <= candidatePos.Quantity+1e-6 {
						if mark, ok := prices[coin]; ok && mark > 0 {
							logger, logErr := logMgr.GetStrategyLogger(candidateID)
							if logErr != nil {
								fmt.Printf("[ERROR] hl-sync: logger for %s: %v\n", candidateID, logErr)
							}
							lookup, useFillFee := resolveFee(coin, 0, closeQty)
							logHyperliquidReconcileFillLookup(logger, coin, 0, closeQty, lookup, useFillFee)
							detector3OID := ""
							if useFillFee && lookup.OID > 0 {
								detector3OID = strconv.FormatInt(lookup.OID, 10)
							}
							if recordPerpsExternalPartialCloseWithFillFee(candidateSS, coin, closeQty, mark, lookup.Fee, useFillFee, detector3OID, "hl_sync_external_partial", logger) {
								changed = true
								if closeSide == "long" {
									virtualQty -= closeQty
								} else {
									virtualQty += closeQty
								}
								delta = virtualQty - onChainQty
								// candidatePos.Quantity is decremented in-place by
								// the partial-close booker (or the position is
								// deleted when fully drained); read it back for
								// the DM remaining-qty line.
								remaining := 0.0
								if posAfter := candidateSS.Positions[coin]; posAfter != nil {
									remaining = posAfter.Quantity
								}
								// lastBookedTradePnL relies on the just-completed
								// RecordTrade inside the booker; do not insert another
								// RecordTrade between here and the booker call.
								pendingAlerts = append(pendingAlerts, ProtectionFillAlert{
									StrategyID:      candidateID,
									Symbol:          coin,
									Side:            closeSide,
									FillType:        tpTierLabel(candidateTierIdx),
									IsPartial:       true,
									FillPrice:       mark,
									CloseQty:        closeQty,
									RemainingQty:    remaining,
									RealizedPnL:     lastBookedTradePnL(candidateSS),
									HasPnL:          true,
									ExchangeOrderID: detector3OID,
								})
							}
						} else {
							fmt.Printf("[WARN] hl-sync: shared coin %s TP partial drift detected for %s but no mark price is available; leaving virtual qty unchanged\n", coin, candidateID)
						}
					}
				}
			}
		}

		state.ReconciliationGaps[coin] = &ReconciliationGap{
			Coin:       coin,
			OnChainQty: onChainQty,
			VirtualQty: virtualQty,
			DeltaQty:   delta,
			Strategies: stratIDs,
			UpdatedAt:  now,
		}

		if math.Abs(delta) > 0.000001 {
			fmt.Printf("[WARN] hl-sync: shared coin %s reconciliation gap: virtual=%.6f on-chain=%.6f delta=%.6f (strategies: %v)\n",
				coin, virtualQty, onChainQty, delta, stratIDs)
		}
	}

	// Clean up gaps for coins that are no longer shared.
	for coin := range state.ReconciliationGaps {
		if !sharedCoins[coin] {
			delete(state.ReconciliationGaps, coin)
		}
	}

	// Warn about unowned on-chain positions (not traded by any strategy).
	tradedCoins := make(map[string]bool)
	for coin := range coinStrategies {
		tradedCoins[coin] = true
	}
	for _, p := range positions {
		if !tradedCoins[p.Coin] {
			qty := math.Abs(p.Size)
			side := "long"
			if p.Size < 0 {
				side = "short"
			}
			fmt.Printf("[WARN] hl-sync: unowned on-chain position: %s %.6f %s @ $%.2f (no strategy claims it)\n",
				side, qty, p.Coin, p.EntryPrice)
		}
	}

	return changed
}

func hyperliquidSharedPartialCloseDrift(virtualQty, onChainQty float64) (string, float64, bool) {
	const tol = 1e-6
	if virtualQty > tol && onChainQty > tol && onChainQty < virtualQty-tol {
		return "long", virtualQty - onChainQty, true
	}
	if virtualQty < -tol && onChainQty < -tol && onChainQty > virtualQty+tol {
		return "short", onChainQty - virtualQty, true
	}
	return "", 0, false
}

// hyperliquidClearedTPTier reports whether sc/pos shows a cleared TP tier
// attributable to closeQty, and which tier index (0-based) cleared. Used by
// reconciler Detector 3 to attribute partial closes and by the TP-fill DM
// alert to label the tier (#661).
//
// Returns (clearedIdx, true) when at least one TP OID is zero AND either:
//   - some other tier is still active (the cleared one is the freshest fill), or
//   - all tiers are zero AND closeQty matches pos.Quantity (sole-peer final close,
//     attributed to the last tier).
//
// Caveat: when multiple tiers have already cleared but none is yet booked,
// this returns the FIRST cleared index. Detector 3 is expected to fire once
// per fill (each cycle's drift detection books exactly one tier), so the
// "earliest cleared" answer matches the unbooked fill in practice. If a future
// caller batches multiple un-booked fills, revisit this assumption.
func hyperliquidClearedTPTier(sc StrategyConfig, pos *Position, closeQty float64) (int, bool) {
	if pos == nil || len(pos.TPOIDs) == 0 {
		return 0, false
	}
	tiers := strategyTPTiersForRegime(sc, pos.Regime)
	if len(tiers) == 0 {
		return 0, false
	}
	if len(pos.TPOIDs) < len(tiers) {
		return 0, false
	}
	tpOIDs := tpOIDsForTierCount(pos.TPOIDs, len(tiers))
	clearedIdx := -1
	hasActive := false
	for i, oid := range tpOIDs {
		if oid > 0 {
			hasActive = true
		} else if clearedIdx < 0 {
			clearedIdx = i
		}
	}
	if clearedIdx < 0 {
		return 0, false
	}
	if hasActive {
		return clearedIdx, true
	}
	// All TP tiers gone usually means the final tier filled. Treat that as
	// attributable only when the observed drift can fully close this strategy;
	// otherwise an all-zero, never-placed TP list would make ambiguous gaps look
	// actionable.
	if math.Abs(pos.Quantity-closeQty) <= 1e-6 {
		return len(tpOIDs) - 1, true
	}
	return 0, false
}

// hyperliquidHasClearedTPTier is the bool-returning shim retained for callers
// that don't need the tier index.
func hyperliquidHasClearedTPTier(sc StrategyConfig, pos *Position, closeQty float64) bool {
	_, ok := hyperliquidClearedTPTier(sc, pos, closeQty)
	return ok
}

// hlAttemptCloseFromTPFills books a flat-on-chain perps position from
// userFills-confirmed TP OID fills, returning true on success. Fixes #673:
// when TPs flatten the position, HL auto-cancels the resting reduce-only SL
// — without this check, the reconciler books the close at the SL trigger
// price, producing a fictitious loss.
//
// Triggers when (a) pos has at least one TP OID, (b) the SL OID has NO
// userFills entries (so SL didn't fire), and (c) at least one TP OID does.
// Each filled TP OID is booked as a partial close at its actual VWAP fill
// price + fee. Any residual after all TP fills is finalized at zero PnL —
// that catches the rare "userFills indexer missed an SL partial" race.
//
// Returns false (no mutation) when no TP attribution is possible; the caller
// then falls through to the legacy SL-trigger-price path.
//
// When pendingAlerts is non-nil, each successful TP partial book appends a
// ProtectionFillAlert (same ordering contract as tryBookSoleOwnerTPFill) so
// sole-owner cycle-ordering races still emit owner TP fill DMs (#757).
func hlAttemptCloseFromTPFills(s *StrategyState, sym string, pos *Position, resolveFee hlReconcileFillResolver, logger *StrategyLogger, pendingAlerts *[]ProtectionFillAlert) bool {
	if s == nil || pos == nil || len(pos.TPOIDs) == 0 || resolveFee == nil {
		return false
	}
	// If the SL OID has fills, leave attribution to the existing SL path —
	// it knows how to handle the #621 "SL fired on the post-TP residual"
	// case and we don't want to double-book by also crediting TPs here.
	// #685: require OID equality so a coin+size fallback hit on a TP fill of
	// the same size (lookup.OID != StopLossOID) doesn't masquerade as an SL
	// fill and starve TP attribution.
	if pos.StopLossOID > 0 {
		if lookup, slFilled := resolveFee(sym, pos.StopLossOID, pos.Quantity); hlReconcileSLFillConfirmed(lookup, slFilled, pos.StopLossOID) {
			return false
		}
	}
	type tpFill struct {
		oid     int64
		tierIdx int
		lookup  HLFillLookup
	}
	var fills []tpFill
	for i, oid := range pos.TPOIDs {
		if oid <= 0 {
			continue
		}
		lookup, ok := resolveFee(sym, oid, pos.Quantity)
		if !ok || lookup.FilledQty <= 0 || lookup.Px <= 0 {
			continue
		}
		fills = append(fills, tpFill{oid: oid, tierIdx: i, lookup: lookup})
	}
	if len(fills) == 0 {
		return false
	}
	for _, f := range fills {
		curBefore := s.Positions[sym]
		if curBefore == nil {
			break
		}
		alertSide := curBefore.Side
		oidStr := strconv.FormatInt(f.oid, 10)
		logHyperliquidReconcileFillLookup(logger, sym, f.oid, f.lookup.FilledQty, f.lookup, true)
		reason := fmt.Sprintf("hl_sync_tp%d_fill", f.tierIdx+1)
		detailsPrefix := fmt.Sprintf("TP%d fill close", f.tierIdx+1)
		logPrefix := fmt.Sprintf("TP%d fill reconciled", f.tierIdx+1)
		if !bookPerpsPartialCloseWithFillFee(s, sym, f.lookup.FilledQty, f.lookup.Px, f.lookup.Fee, true, oidStr, reason, detailsPrefix, logPrefix, logger) {
			break
		}
		if pendingAlerts != nil {
			remaining := 0.0
			if posAfter := s.Positions[sym]; posAfter != nil {
				remaining = posAfter.Quantity
			}
			*pendingAlerts = append(*pendingAlerts, ProtectionFillAlert{
				StrategyID:      s.ID,
				Symbol:          sym,
				Side:            alertSide,
				FillType:        tpTierLabel(f.tierIdx),
				IsPartial:       remaining > 1e-9,
				FillPrice:       f.lookup.Px,
				CloseQty:        f.lookup.FilledQty,
				RemainingQty:    remaining,
				RealizedPnL:     lastBookedTradePnL(s),
				HasPnL:          true,
				ExchangeOrderID: oidStr,
			})
		}
	}
	// Finalize any residual at zero PnL. Hits when cumulative TP fills under-
	// shoot pos.Quantity — typically an SL fill the indexer hasn't surfaced
	// yet. Better to clear virtual state than to leave a phantom position.
	if residual := s.Positions[sym]; residual != nil {
		if logger != nil {
			logger.Warn("hl-sync: %s residual %.6f after TP fill attribution; finalizing at zero PnL", sym, residual.Quantity)
		}
		recordClosedPosition(s, residual, 0, 0, "hl_sync_external", time.Now().UTC())
		delete(s.Positions, sym)
	}
	clearATRMultMissingEntryATRWarningOnHLPerpsClose(s, sym)
	return true
}

// HyperliquidLiveCloseReport summarizes a forceCloseHyperliquidLive run.
// Each configured live HL coin lands in exactly one of ClosedCoins (SDK
// accepted the reduce-only close), AlreadyFlat (defensive: szi==0 short-
// circuited before submit), or Errors (close not confirmed). The producer
// (forceCloseHyperliquidLive) is the single writer and maintains the
// partition via mutually-exclusive control flow.
//
// Errors is the load-bearing kill-switch correctness signal: only when it's
// empty does the caller mutate virtual state. Any error keeps the kill
// switch latched so the next cycle re-fetches on-chain state and retries
// (#341). Use ConfirmedFlat() rather than `len(Errors) == 0` at call sites
// so future readers see the predicate spelled out.
type HyperliquidLiveCloseReport struct {
	ClosedCoins []string
	// Fills carries the real exchange fill for coins in ClosedCoins when the
	// adapter returned one. Kill-switch state clearing uses this to book
	// realized PnL from the close fill instead of the pre-close mark (#454).
	Fills map[string]HyperliquidCloseFill
	// AlreadyFlat is set from two sources: the pre-submit szi==0 short-circuit
	// in forceCloseHyperliquidLive (defense-in-depth — FetchHyperliquidPositions
	// pre-filters szi≠0, so this branch should not fire in production) AND the
	// adapter-side already_flat envelope flag, which IS production-reachable
	// when the eventual-consistency window between the Go-side fetch and the
	// SDK submit lets a position close out from under us (#350), or when a
	// post-close verification fetch proves a coin is flat after the close
	// subprocess returned an error (#452).
	AlreadyFlat []string
	// Errors is non-nil so coin-keyed writes don't panic; len() works on nil maps too.
	Errors map[string]error
}

// ConfirmedFlat reports whether every configured live HL coin reached a
// terminal closed/flat state without errors. The kill-switch path uses this
// to gate virtual state mutation.
func (r HyperliquidLiveCloseReport) ConfirmedFlat() bool {
	return len(r.Errors) == 0
}

// SortedErrorCoins returns Errors keys in deterministic order for stable
// log/Discord output. Map iteration is randomized in Go, so two identical
// kill-switch fires would otherwise produce different messages — confusing
// for operator triage.
func (r HyperliquidLiveCloseReport) SortedErrorCoins() []string {
	coins := make([]string, 0, len(r.Errors))
	for c := range r.Errors {
		coins = append(coins, c)
	}
	sort.Strings(coins)
	return coins
}

// forceCloseHyperliquidLive submits reduce-only market closes for every
// non-zero on-chain HL position belonging to a coin a configured live HL
// strategy trades on this account. Closes the on-chain quantity directly,
// regardless of which strategy "owns" it — required because shared coins
// have per-strategy reconciliation that deliberately does not overwrite
// virtual quantities (#258), so virtual state can diverge from the on-chain
// net (#341). HL SDK's market_close passes reduce_only=True (verified at
// hyperliquid.exchange.Exchange.market_close), so overshooting cannot
// accidentally flip the position.
//
// Pure / no state mutation. Caller is responsible for mutating virtual state
// only when report.ConfirmedFlat() is true.
//
// The Size==0 branch is defense-in-depth: fetchHyperliquidState upstream
// already filters zero-szi entries out of HLPosition (see hyperliquid_balance.go's
// szi parser), so this path is unreachable in production. Kept so a future
// loosening of the upstream filter (e.g. surfacing legacy positions for
// reconciliation) cannot accidentally submit a zero-size order that the HL
// API would reject and the kill switch would treat as a fatal error.
//
// The ctx argument bounds the OVERALL close loop. Each individual closer call
// also has its own subprocess timeout (see RunPythonScript). Once ctx expires,
// remaining unprocessed coins are added to Errors so the kill switch stays
// latched and retries next cycle. Pass context.Background() to disable the
// overall bound.
//
// stopLossOIDsByCoin carries any resting per-trade SL trigger OIDs that
// should be cancelled before the close fires, so kill-switch flattening
// doesn't leave orphan triggers consuming HL's open-order cap (#421, #479).
// nil/empty disables the cancel; the closer is otherwise unchanged.
func forceCloseHyperliquidLive(ctx context.Context, positions []HLPosition, hlLiveAll []StrategyConfig, closer HyperliquidLiveCloser, stopLossOIDsByCoin map[string][]int64) HyperliquidLiveCloseReport {
	report := HyperliquidLiveCloseReport{
		Fills:  make(map[string]HyperliquidCloseFill),
		Errors: make(map[string]error),
	}

	tradedCoins := make(map[string]bool)
	for _, sc := range hlLiveAll {
		sym := hyperliquidSymbol(sc.Args)
		if sym != "" {
			tradedCoins[sym] = true
		}
	}

	for _, p := range positions {
		if !tradedCoins[p.Coin] {
			// Unowned position — kill switch only acts on coins this scheduler
			// is configured to trade. An on-chain leftover from a different
			// system (manual trade, another bot) is the operator's problem to
			// reconcile, not the scheduler's to liquidate.
			continue
		}
		if p.Size == 0 {
			report.AlreadyFlat = append(report.AlreadyFlat, p.Coin)
			continue
		}
		// Bail out before submitting if the overall budget expired so we
		// don't queue another N×30s of subprocess time on top of a deadline
		// the scheduler has already missed.
		if err := ctx.Err(); err != nil {
			report.Errors[p.Coin] = fmt.Errorf("close budget exhausted before submit: %w", err)
			continue
		}
		var slOIDs []int64
		if stopLossOIDsByCoin != nil {
			slOIDs = stopLossOIDsByCoin[p.Coin]
		}
		result, err := closer(p.Coin, nil, slOIDs)
		if err != nil {
			report.Errors[p.Coin] = err
			continue
		}
		// Adapter may report already_flat when its own pre-submit position
		// check finds nothing to close (eventual-consistency window between
		// the Go-side fetch and the close submit). Route through AlreadyFlat
		// so operator messaging accurately distinguishes "we sent a close
		// order" from "nothing to close" (#350).
		if result != nil && result.Close != nil && result.Close.AlreadyFlat {
			report.AlreadyFlat = append(report.AlreadyFlat, p.Coin)
			continue
		}
		if result != nil && result.Close != nil && result.Close.Fill != nil {
			report.Fills[p.Coin] = *result.Close.Fill
		}
		report.ClosedCoins = append(report.ClosedCoins, p.Coin)
	}

	return report
}

func hlLiveStrategiesForCoin(coin string, hlLiveAll []StrategyConfig) []StrategyConfig {
	target := strings.ToUpper(strings.TrimSpace(coin))
	var out []StrategyConfig
	for _, sc := range hlLiveAll {
		if hyperliquidConfiguredCoin(sc) == target {
			out = append(out, sc)
		}
	}
	return out
}

// hyperliquidConfiguredCoin returns the coin ticker a HL strategy targets,
// normalized to upper-case + trimmed so peer detection survives operator
// typos like `symbol: "eth"` against `args: [..., "ETH", ...]`. HL coin
// tickers are uppercase by convention and the Python adapter rejects unknown
// casings on its own, so normalizing here only affects Go-side peer matching.
func hyperliquidConfiguredCoin(sc StrategyConfig) string {
	if sc.Platform != "hyperliquid" {
		return ""
	}
	var raw string
	if sc.Type == "manual" {
		raw = sc.Symbol
	} else {
		raw = hyperliquidSymbol(sc.Args)
	}
	return strings.ToUpper(strings.TrimSpace(raw))
}

type hlVirtualQuantitySnapshot map[string]map[string]float64

// snapshotHyperliquidVirtualQuantities captures the per-strategy virtual
// quantities that exist before a portfolio kill-switch close mutates state.
func snapshotHyperliquidVirtualQuantities(strategies map[string]*StrategyState, hlLiveAll []StrategyConfig) hlVirtualQuantitySnapshot {
	if len(strategies) == 0 || len(hlLiveAll) == 0 {
		return nil
	}
	out := make(hlVirtualQuantitySnapshot)
	for _, sc := range hlLiveAll {
		coin := hyperliquidSymbol(sc.Args)
		if coin == "" {
			continue
		}
		ss := strategies[sc.ID]
		if ss == nil {
			continue
		}
		pos := ss.Positions[coin]
		if pos == nil || pos.Quantity <= 0 {
			continue
		}
		if out[coin] == nil {
			out[coin] = make(map[string]float64)
		}
		out[coin][sc.ID] = pos.Quantity
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// computeHyperliquidCircuitCloseQty returns the unsigned coin quantity for a
// reduce-only market_close when strategyID's per-strategy circuit breaker fires
// (#356). Shared-coin peers are deliberately skipped: Hyperliquid aggregates a
// coin into one exchange-side position per wallet, so even a partial close can
// disturb other strategies' live exposure (#512). For a sole configured trader
// of that coin, the full on-chain absolute size is used. ok is false when there
// is no non-zero on-chain position or the coin is shared by multiple live peers.
func computeHyperliquidCircuitCloseQty(coin, strategyID string, hlPositions []HLPosition, hlLiveAll []StrategyConfig) (qty float64, ok bool) {
	var onChain float64
	found := false
	for i := range hlPositions {
		if hlPositions[i].Coin == coin {
			onChain = hlPositions[i].Size
			found = true
			break
		}
	}
	if !found || onChain == 0 {
		return 0, false
	}
	absSzi := math.Abs(onChain)
	peers := hlLiveStrategiesForCoin(coin, hlLiveAll)
	if len(peers) > 1 {
		return 0, false
	}
	return absSzi, true
}

func hyperliquidKillSwitchFillShare(sc StrategyConfig, coin string, fillSz, fillFee float64, hlLiveAll []StrategyConfig, virtualQty hlVirtualQuantitySnapshot) (float64, float64) {
	peers := hlLiveStrategiesForCoin(coin, hlLiveAll)
	if len(peers) <= 1 {
		return fillSz, fillFee
	}
	qtyByStrategy := virtualQty[coin]
	sumQty := 0.0
	var selfQty float64
	foundSelf := false
	for _, p := range peers {
		if p.ID == sc.ID {
			foundSelf = true
		}
		qty := qtyByStrategy[p.ID]
		if qty <= 0 {
			continue
		}
		sumQty += qty
		if p.ID == sc.ID {
			selfQty = qty
		}
	}
	if !foundSelf || sumQty <= 0 || selfQty <= 0 {
		// Fail closed: a misconfigured caller passing an `sc` that isn't among
		// peers must not cause a single strategy to claim the entire portfolio
		// fill. The generic fallback in forceCloseAllPositions will then close
		// any residual virtual position at mark price.
		return 0, 0
	}
	ratio := selfQty / sumQty
	if ratio < 0 {
		ratio = 0
	} else if ratio > 1 {
		ratio = 1
	}
	return fillSz * ratio, fillFee * ratio
}

// applyHyperliquidKillSwitchCloseFill applies one strategy's virtual-quantity
// share of the portfolio kill-switch fill before generic virtual-state cleanup
// runs.
func applyHyperliquidKillSwitchCloseFill(s *StrategyState, sc StrategyConfig, fills map[string]HyperliquidCloseFill, hlLiveAll []StrategyConfig, virtualQty hlVirtualQuantitySnapshot) bool {
	if s == nil || sc.Platform != "hyperliquid" || sc.Type != "perps" || !hyperliquidIsLive(sc.Args) {
		return false
	}
	coin := hyperliquidSymbol(sc.Args)
	if coin == "" {
		return false
	}
	fill, ok := fills[coin]
	if !ok || fill.TotalSz <= 1e-15 || fill.AvgPx <= 0 {
		return false
	}
	fillSz, fillFee := hyperliquidKillSwitchFillShare(sc, coin, fill.TotalSz, fill.Fee, hlLiveAll, virtualQty)
	if fillSz <= 1e-15 {
		return false
	}
	applyHyperliquidCircuitCloseFill(s, coin, fillSz, fill.AvgPx, fillFee, 0)
	return true
}

func lookupStrategyConfig(strategies []StrategyConfig, id string) *StrategyConfig {
	for i := range strategies {
		if strategies[i].ID == id {
			return &strategies[i]
		}
	}
	return nil
}

// runPendingHyperliquidCircuitCloses drains the hyperliquid entry of
// RiskState.PendingCircuitCloses for every strategy, submitting reduce-only HL
// closes outside the state mutex. Retries next scheduler cycle on failure
// (#356 / #359).
//
// Also recovers "stuck CB" strategies: if a per-strategy circuit breaker fires
// on a cycle where the HL clearinghouse fetch failed, setHyperliquidCircuitBreakerPending
// bails on the nil assist and the pending close is never set. Subsequent
// CheckRisk calls early-return with "circuit breaker active" without re-enqueuing.
// This drain detects the case (live HL perps strategy with CircuitBreaker=true
// but no pending HL entry AND a matching non-zero on-chain position) and
// reconstructs the pending so the reduce-only close eventually fires once HL
// is reachable again (#356 review finding 1).
func runPendingHyperliquidCircuitCloses(
	ctx context.Context,
	state *AppState,
	strategies []StrategyConfig,
	hlAddr string,
	hlPositions []HLPosition,
	hlStateFetched bool,
	hlFetcher HLStateFetcher,
	closer HyperliquidLiveCloser,
	totalBudget time.Duration,
	mu *sync.RWMutex,
	ownerDM func(string),
) {
	if hlAddr == "" || closer == nil || state == nil {
		return
	}

	// Build the live HL perps roster from strategies — needed for actual
	// circuit-breaker close work. Build a wider perps+manual peer scope for
	// shared-coin safety checks so a perps CB never closes a manual peer's
	// wallet exposure (#620).
	var hlLiveAll []StrategyConfig
	var hlCircuitPeerAll []StrategyConfig
	for _, sc := range strategies {
		if sc.Platform == "hyperliquid" && sc.Type == "perps" && hyperliquidIsLive(sc.Args) {
			hlLiveAll = append(hlLiveAll, sc)
		}
		if isHLLiveReconcilable(sc) {
			hlCircuitPeerAll = append(hlCircuitPeerAll, sc)
		}
	}

	// Phase 1: snapshot — detect pending jobs AND stuck-CB strategies that
	// need their pending reconstructed.
	mu.RLock()
	hasPending := false
	hasStuckCB := false
	for _, ss := range state.Strategies {
		if ss == nil {
			continue
		}
		if ss.RiskState.getPendingCircuitClose(PlatformPendingCloseHyperliquid) != nil {
			hasPending = true
		}
	}
	for _, sc := range hlLiveAll {
		ss := state.Strategies[sc.ID]
		if ss == nil {
			continue
		}
		sym := hyperliquidConfiguredCoin(sc)
		if sym == "" || len(hlLiveStrategiesForCoin(sym, hlCircuitPeerAll)) > 1 {
			continue
		}
		if ss.RiskState.getPendingCircuitClose(PlatformPendingCloseHyperliquid) == nil && ss.RiskState.CircuitBreaker {
			hasStuckCB = true
			break
		}
	}
	mu.RUnlock()

	if !hasPending && !hasStuckCB {
		return
	}

	ctxOverall, cancelOverall := context.WithTimeout(ctx, totalBudget)
	defer cancelOverall()

	positions := hlPositions
	if !hlStateFetched && hlFetcher != nil {
		pos, err := hlFetcher(hlAddr)
		if err != nil {
			fmt.Printf("[CRITICAL] hl-circuit-close: cannot fetch HL positions: %v — will retry next cycle\n", err)
			return
		}
		positions = pos
	}

	// Phase 2: reconstruct pending for stuck-CB strategies.
	if hasStuckCB {
		// Sort hlLiveAll for deterministic recovery-log order.
		recoverOrder := make([]StrategyConfig, len(hlLiveAll))
		copy(recoverOrder, hlLiveAll)
		sort.Slice(recoverOrder, func(i, j int) bool { return recoverOrder[i].ID < recoverOrder[j].ID })
		mu.Lock()
		for _, sc := range recoverOrder {
			ss := state.Strategies[sc.ID]
			if ss == nil {
				continue
			}
			if ss.RiskState.getPendingCircuitClose(PlatformPendingCloseHyperliquid) != nil {
				continue
			}
			if !ss.RiskState.CircuitBreaker {
				continue
			}
			sym := hyperliquidConfiguredCoin(sc)
			if sym == "" {
				continue
			}
			qty, ok := computeHyperliquidCircuitCloseQty(sym, sc.ID, positions, hlCircuitPeerAll)
			if !ok || qty <= 0 {
				continue
			}
			ss.RiskState.setPendingCircuitClose(PlatformPendingCloseHyperliquid, &PendingCircuitClose{
				Symbols: []PendingCircuitCloseSymbol{{Symbol: sym, Size: qty}},
			})
			fmt.Printf("[CRITICAL] hl-circuit-close: recovered pending for strategy %s coin %s sz=%.6f (CB latched, HL fetch had failed at fire time)\n",
				sc.ID, sym, qty)
		}
		mu.Unlock()
	}

	// Phase 3: re-snapshot jobs (may now include recovered entries).
	// Also snapshot per-symbol StopLossOID so the closer can cancel any
	// resting SL trigger before flattening — leaving them orphaned would
	// burn one of HL's open-order cap slots per CB fire and silently
	// degrade the safety feature for every other strategy on the same
	// wallet (#421 review point 1, #479).
	type job struct {
		stratID string
		pending PendingCircuitClose
		slOIDs  map[string][]int64
	}
	var jobs []job
	mu.RLock()
	for id, ss := range state.Strategies {
		if ss == nil {
			continue
		}
		p := ss.RiskState.getPendingCircuitClose(PlatformPendingCloseHyperliquid)
		if p == nil || len(p.Symbols) == 0 {
			continue
		}
		slOIDs := make(map[string][]int64, len(p.Symbols))
		for _, c := range p.Symbols {
			if pos, ok := ss.Positions[c.Symbol]; ok && pos != nil {
				slOIDs[c.Symbol] = appendUniquePositiveStopLossOID(slOIDs[c.Symbol], pos.StopLossOID)
				for _, tpOID := range pos.TPOIDs {
					slOIDs[c.Symbol] = appendUniquePositiveStopLossOID(slOIDs[c.Symbol], tpOID)
				}
			}
		}
		jobs = append(jobs, job{id, *p, slOIDs})
	}
	mu.RUnlock()

	if len(jobs) == 0 {
		return
	}

	// Deterministic drain order — operator-facing logs at lines below iterate
	// this slice, and map iteration above would otherwise randomize which
	// subset of strategies get serviced when the budget is partially exhausted
	// (#356 review finding 2; CLAUDE.md "Sort map keys before formatting any
	// operator-facing output").
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].stratID < jobs[j].stratID })

	for _, j := range jobs {
		if err := ctxOverall.Err(); err != nil {
			fmt.Printf("[CRITICAL] hl-circuit-close: budget exhausted: %v\n", err)
			return
		}
		sc := lookupStrategyConfig(strategies, j.stratID)
		if sc == nil || sc.Platform != "hyperliquid" || sc.Type != "perps" || !hyperliquidIsLive(sc.Args) {
			mu.Lock()
			if ss := state.Strategies[j.stratID]; ss != nil {
				ss.RiskState.clearPendingCircuitClose(PlatformPendingCloseHyperliquid)
			}
			mu.Unlock()
			continue
		}
		if sym := hyperliquidConfiguredCoin(*sc); sym != "" && len(hlLiveStrategiesForCoin(sym, hlCircuitPeerAll)) > 1 {
			fmt.Printf("[INFO] hl-circuit-close: strategy %s coin %s shares the wallet position with peers — clearing pending close and leaving exchange position untouched\n",
				j.stratID, sym)
			mu.Lock()
			if ss := state.Strategies[j.stratID]; ss != nil {
				ss.RiskState.clearPendingCircuitClose(PlatformPendingCloseHyperliquid)
			}
			mu.Unlock()
			continue
		}

		allOK := true
		drainError := false // set on closer() error; not set for under-fills (partial progress)
		var drainErrSym string
		var drainErrSz float64
		var drainErrMsg string
		for _, c := range j.pending.Symbols {
			if err := ctxOverall.Err(); err != nil {
				allOK = false
				break
			}
			sz := c.Size
			var onChainSigned float64
			for _, p := range positions {
				if p.Coin != c.Symbol {
					continue
				}
				onChainSigned = p.Size
				absOC := math.Abs(p.Size)
				if absOC <= 1e-15 {
					sz = 0
					break
				}
				if sz > absOC {
					sz = absOC
				}
				break
			}
			if sz <= 1e-15 {
				continue
			}
			partial := sz
			cancelOIDs := j.slOIDs[c.Symbol]
			result, err := closer(c.Symbol, &partial, cancelOIDs)
			if err != nil {
				fmt.Printf("[CRITICAL] hl-circuit-close: strategy %s coin %s sz=%.6f failed: %v\n", j.stratID, c.Symbol, sz, err)
				allOK = false
				drainError = true
				drainErrSym = c.Symbol
				drainErrSz = sz
				drainErrMsg = err.Error()
				break
			}

			// #418: extract actual fill metadata. Previously the drain logged
			// the *requested* sz and cleared pending regardless of how much
			// actually filled, so a partial fill (slippage cap, market depth,
			// market_close slippage param) was indistinguishable from a full
			// close in operator logs and the residual on-chain position was
			// silently abandoned until the next cycle's reconcile (which for
			// shared-wallet coins never overwrites virtual quantity).
			var (
				fillSz, fillPx, fillFee float64
				alreadyFlat             bool
			)
			if result != nil && result.Close != nil {
				alreadyFlat = result.Close.AlreadyFlat
				if result.Close.Fill != nil {
					fillSz = result.Close.Fill.TotalSz
					fillPx = result.Close.Fill.AvgPx
					fillFee = result.Close.Fill.Fee
				}
			}

			// Apply whatever did fill against virtual state (#418 Fix 2). For
			// shared-wallet coins reconcileHyperliquidPositions deliberately
			// does NOT overwrite quantities (#258), so without this decrement
			// the firing strategy's virtual position would stay at 100% while
			// on-chain dropped to its weighted share — the inflated virtual
			// notional then re-fires the CB next cycle.
			if !alreadyFlat && fillSz > 1e-15 {
				mu.Lock()
				if ss := state.Strategies[j.stratID]; ss != nil {
					applyHyperliquidCircuitCloseFill(ss, c.Symbol, fillSz, fillPx, fillFee, onChainSigned)
				}
				mu.Unlock()
			}

			// Detect partial fill: the closer reported a fill smaller than
			// requested. 0.99 tolerance accounts for HL lot-size rounding
			// (the SDK rounds to the asset's stepSz). On under-fill, leave
			// pending intact so the next cycle retries the residual. Note
			// the `fillSz > 0` clause is intentionally absent: a closer that
			// returns success with no fill (nil/zero-TotalSz) is treated as
			// under-fill so a permissive future adapter can't silently clear
			// pending without flattening anything (#418 review observation 1).
			underFill := !alreadyFlat && fillSz < sz*0.99
			if underFill {
				slCancelled := firstPositiveStopLossOID(cancelOIDs) > 0 && result != nil && result.CancelStopLossSucceeded
				slNote := ""
				if slCancelled {
					slNote = " — stop-loss was cancelled, residual is unprotected until retry"
				}
				fmt.Printf("[CRITICAL] hl-circuit-close: strategy %s coin %s PARTIAL fill %.6f/%.6f — leaving pending for retry%s\n",
					j.stratID, c.Symbol, fillSz, sz, slNote)
				allOK = false
			} else {
				fmt.Printf("[INFO] hl-circuit-close: strategy %s coin %s closed sz=%.6f (filled %.6f)\n", j.stratID, c.Symbol, sz, fillSz)
			}

			// Clear protection OIDs under Lock when the cancel went
			// through, so a follow-up cycle doesn't try to cancel the
			// already-cancelled orders.
			if len(cancelOIDs) > 0 && result != nil && result.CancelStopLossSucceeded {
				mu.Lock()
				if ss := state.Strategies[j.stratID]; ss != nil {
					if pos, ok := ss.Positions[c.Symbol]; ok && pos != nil {
						for _, cancelOID := range cancelOIDs {
							if cancelOID > 0 && pos.StopLossOID == cancelOID {
								pos.StopLossOID = 0
							}
							for idx, tpOID := range pos.TPOIDs {
								if cancelOID > 0 && tpOID == cancelOID {
									pos.TPOIDs[idx] = 0
								}
							}
						}
					}
				}
				mu.Unlock()
			}

			// Other symbols in this strategy's pending list are independent
			// positions (e.g. ETH partial + BTC + SOL) — under-fill on one
			// symbol must not defer the others. Use continue, not break, so
			// each symbol gets its own attempt this cycle (#418 review
			// observation 2).
			if underFill {
				continue
			}
		}

		// Post-loop: update ConsecutiveFailures counter and fire owner DM.
		// drainError = true only on a hard closer() error; under-fills are
		// partial progress that reset the counter to 0 — but ONLY when the
		// cycle had no hard error at all. In a multi-symbol pending list where
		// one leg under-fills and another hard-errors, drainError wins (we
		// still increment) so the operator is alerted to the failed leg.
		var failCount int
		var shouldAlert bool
		now := time.Now().UTC()
		mu.Lock()
		if ss := state.Strategies[j.stratID]; ss != nil {
			if allOK {
				ss.RiskState.clearPendingCircuitClose(PlatformPendingCloseHyperliquid)
			} else if p := ss.RiskState.getPendingCircuitClose(PlatformPendingCloseHyperliquid); p != nil {
				if drainError {
					p.ConsecutiveFailures++
					failCount = p.ConsecutiveFailures
					if shouldNotifyDrainFailure(p.ConsecutiveFailures, p.LastNotifiedAt, now) {
						p.LastNotifiedAt = now
						shouldAlert = true
					}
				} else {
					// Under-fill only — partial progress. Reset so the next
					// hard error re-notifies as a fresh first failure.
					p.ConsecutiveFailures = 0
				}
			}
		}
		mu.Unlock()

		if shouldAlert && ownerDM != nil {
			ownerDM(formatDrainFailureAlert("hyperliquid", j.stratID, drainErrSym, drainErrSz, drainErrMsg, failCount))
		}
	}
}

// applyHyperliquidCircuitCloseFill applies a reduce-only close fill against
// the strategy's virtual position (#418 Fix 2). Decrements pos.Quantity by
// the actual filled amount, books realized PnL net of the on-chain fee, and
// records a Trade so the close fill lands in trade history just like a normal
// signal-driven close. AvgCost is preserved (standard partial-close
// semantics) — only Quantity is reduced.
//
// When the post-fill quantity drops to ~0 the position is fully closed and
// removed from s.Positions via recordClosedPosition (consistent with the
// signal-driven close path).
//
// When no virtual position exists (or has zero quantity) we still record a
// defensive Trade so the on-chain close lives in audit history; PnL is
// skipped because we have no AvgCost basis. onChainSigned is the signed
// on-chain position size at submit time (positive = long, negative = short)
// so the trade-history Side is inferred from what we actually closed rather
// than hard-coded as "sell" — matters when reconciling a stranded short
// (#418 review observation 4). Pass 0 if the on-chain side is unknown; the
// trade then falls back to "sell".
//
// Caller must hold mu.Lock(). Reason is fixed to "circuit_breaker" for
// clarity in trade history and closed-position rows.
func applyHyperliquidCircuitCloseFill(s *StrategyState, symbol string, fillSz, fillPx, fillFee, onChainSigned float64) {
	if s == nil || fillSz <= 0 || fillPx <= 0 {
		return
	}
	now := time.Now().UTC()
	pos, ok := s.Positions[symbol]
	if !ok || pos == nil || pos.Quantity <= 0 {
		// No virtual position to decrement — record defensive Trade with no
		// PnL accounting (no AvgCost basis available). Closing a short is a
		// buy; closing a long is a sell. Default to "sell" when the on-chain
		// side is unknown (legacy callers, no positions snapshot).
		closeSide := "sell"
		if onChainSigned < 0 {
			closeSide = "buy"
		}
		RecordTrade(s, Trade{
			Timestamp:   now,
			StrategyID:  s.ID,
			Symbol:      symbol,
			Side:        closeSide,
			Quantity:    fillSz,
			Price:       fillPx,
			Value:       fillSz * fillPx,
			TradeType:   "perps",
			Details:     fmt.Sprintf("Circuit breaker on-chain close (no virtual position), fill=%.6f fee=$%.4f", fillSz, fillFee),
			ExchangeFee: exchangeFeeForTrade(fillFee, true),
			// No virtual position to derive PnL from. Still mark as a close
			// leg so the lifetime round-trip count (#455) reflects that the
			// exchange-side position was reduced, but leave RealizedPnL=0
			// (no AvgCost basis available). With strict #471 W/L semantics,
			// this breakeven close counts as neither win nor loss.
			IsClose: true,
			Regime:  s.Regime,
		})
		return
	}

	qtyClosed := fillSz
	if qtyClosed > pos.Quantity {
		qtyClosed = pos.Quantity
	}
	side := pos.Side
	avgCost := pos.AvgCost
	var pnl float64
	if side == "long" {
		pnl = qtyClosed * (fillPx - avgCost)
	} else {
		pnl = qtyClosed * (avgCost - fillPx)
	}
	pnl -= fillFee
	s.Cash += pnl
	positionID := ensurePositionTradeID(s.ID, symbol, pos)

	RecordTrade(s, Trade{
		Timestamp:         now,
		StrategyID:        s.ID,
		Symbol:            symbol,
		PositionID:        positionID,
		Side:              closeTradeSide(side),
		Quantity:          qtyClosed,
		Price:             fillPx,
		Value:             qtyClosed * fillPx,
		TradeType:         "perps",
		Details:           fmt.Sprintf("Circuit breaker on-chain close, PnL: $%.2f (fee $%.4f)", pnl, fillFee),
		ExchangeFee:       exchangeFeeForTrade(fillFee, true),
		IsClose:           true,
		RealizedPnL:       pnl,
		Regime:            s.Regime,
		EntryATR:          pos.EntryATR,
		StopLossTriggerPx: pos.StopLossTriggerPx,
		StopLossATRMult:   pos.StopLossATRMult,
		TPTiersJSON:       pos.TPTiersJSON,
	})
	RecordTradeResult(&s.RiskState, pnl)

	remaining := pos.Quantity - qtyClosed
	if remaining <= 1e-9 {
		// Position fully closed — pos.Quantity is still the original value at
		// this point (we never wrote qtyClosed back into it). Since
		// remaining ≈ 0, the original ≈ qtyClosed, so recordClosedPosition's
		// snapshot of pos.Quantity into ClosedPosition.Quantity captures the
		// right amount. delete() runs after the snapshot.
		recordClosedPosition(s, pos, fillPx, pnl, "circuit_breaker", now)
		delete(s.Positions, symbol)
		clearATRMultMissingEntryATRWarningOnHLPerpsClose(s, symbol)
	} else {
		pos.Quantity = remaining
	}
}

func firstPositiveStopLossOID(oids []int64) int64 {
	for _, oid := range oids {
		if oid > 0 {
			return oid
		}
	}
	return 0
}

func appendUniquePositiveStopLossOID(oids []int64, oid int64) []int64 {
	if oid <= 0 {
		return oids
	}
	for _, existing := range oids {
		if existing == oid {
			return oids
		}
	}
	return append(oids, oid)
}
