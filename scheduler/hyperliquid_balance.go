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
func reconcileHyperliquidPositions(stratState *StrategyState, sym string, positions []HLPosition, logger *StrategyLogger) bool {
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
		if statePos.StopLossOID > 0 && statePos.StopLossTriggerPx > 0 {
			if recordPerpsStopLossClose(stratState, sym, statePos.StopLossTriggerPx, "stop_loss", logger) {
				return true
			}
		}
		// Close price is unknown — the fill happened off-scheduler between
		// reconcile cycles. Record 0 in both fields; downstream analytics
		// that compute avg close price / slippage must filter
		// close_reason != 'hl_sync_external' to avoid biased aggregates.
		recordClosedPosition(stratState, statePos, 0, 0, "hl_sync_external", time.Now().UTC())
		delete(stratState.Positions, sym)
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

	// Self-contained entry: due and all are the same list.
	return reconcileHyperliquidAccountPositions(hlStrategies, hlStrategies, state, mu, logMgr, positions)
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
// Must be called WITHOUT holding any lock; acquires Lock internally.
func reconcileHyperliquidAccountPositions(dueStrategies, allStrategies []StrategyConfig, state *AppState, mu *sync.RWMutex, logMgr *LogManager, positions []HLPosition) bool {
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
		if reconcileHyperliquidPositions(ss, sym, positions, logger) {
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
	// AlreadyFlat is set from two sources: the pre-submit szi==0 short-circuit
	// in forceCloseHyperliquidLive (defense-in-depth — FetchHyperliquidPositions
	// pre-filters szi≠0, so this branch should not fire in production) AND the
	// adapter-side already_flat envelope flag, which IS production-reachable
	// when the eventual-consistency window between the Go-side fetch and the
	// SDK submit lets a position close out from under us (#350).
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
// doesn't leave orphan triggers consuming HL's 10/day account-wide cap
// (#421). nil/empty disables the cancel; the closer is otherwise unchanged.
func forceCloseHyperliquidLive(ctx context.Context, positions []HLPosition, hlLiveAll []StrategyConfig, closer HyperliquidLiveCloser, stopLossOIDsByCoin map[string][]int64) HyperliquidLiveCloseReport {
	report := HyperliquidLiveCloseReport{Errors: make(map[string]error)}

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
		report.ClosedCoins = append(report.ClosedCoins, p.Coin)
	}

	return report
}

func hlLiveStrategiesForCoin(coin string, hlLiveAll []StrategyConfig) []StrategyConfig {
	var out []StrategyConfig
	for _, sc := range hlLiveAll {
		if hyperliquidSymbol(sc.Args) == coin {
			out = append(out, sc)
		}
	}
	return out
}

func hlStrategyCapitalWeight(sc StrategyConfig) float64 {
	if sc.CapitalPct > 0 {
		return sc.CapitalPct
	}
	if sc.Capital > 0 {
		return sc.Capital
	}
	return 1.0
}

// hlStrategyCapitalWeights returns per-peer weights for proportional close
// sizing on a shared coin. When peers mix units (one declares CapitalPct as a
// fraction, another declares raw Capital in dollars), their sum is nonsensical
// (e.g. 0.5 + 1000 ≈ 1000.5) and the CapitalPct-only peer's share collapses to
// ~0, producing a no-op close. Detect the mismatch and fall back to equal
// weights (1.0 each) so the firing strategy still gets a meaningful share.
// When all peers use the same field, behavior matches hlStrategyCapitalWeight
// (#356 review).
func hlStrategyCapitalWeights(peers []StrategyConfig) []float64 {
	hasPct := false
	hasAbs := false
	for _, p := range peers {
		switch {
		case p.CapitalPct > 0:
			hasPct = true
		case p.Capital > 0:
			hasAbs = true
		}
	}
	mixed := hasPct && hasAbs
	out := make([]float64, len(peers))
	for i, p := range peers {
		if mixed {
			out[i] = 1.0
			continue
		}
		out[i] = hlStrategyCapitalWeight(p)
	}
	return out
}

// computeHyperliquidCircuitCloseQty returns the unsigned coin quantity for a
// reduce-only market_close when strategyID's per-strategy circuit breaker fires
// (#356). For a coin traded by multiple live HL strategies on the same wallet,
// the close size is proportional to capital_pct (or capital) weights. For a
// sole configured trader of that coin, the full on-chain absolute size is used.
// ok is false when there is no non-zero on-chain position for the coin.
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
	if len(peers) <= 1 {
		return absSzi, true
	}
	weights := hlStrategyCapitalWeights(peers)
	sumW := 0.0
	var wFiring float64
	foundFiring := false
	for i, p := range peers {
		sumW += weights[i]
		if p.ID == strategyID {
			wFiring = weights[i]
			foundFiring = true
		}
	}
	if !foundFiring || sumW <= 0 {
		return absSzi, true
	}
	q := absSzi * (wFiring / sumW)
	if q > absSzi {
		q = absSzi
	}
	if q < 1e-12 {
		return 0, false
	}
	return q, true
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

	// Build the live HL perps roster from strategies — needed for both the
	// stuck-CB recovery path and the shared-coin weight computation.
	var hlLiveAll []StrategyConfig
	for _, sc := range strategies {
		if sc.Platform == "hyperliquid" && sc.Type == "perps" && hyperliquidIsLive(sc.Args) {
			hlLiveAll = append(hlLiveAll, sc)
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
			sym := hyperliquidSymbol(sc.Args)
			if sym == "" {
				continue
			}
			qty, ok := computeHyperliquidCircuitCloseQty(sym, sc.ID, positions, hlLiveAll)
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
	// burn one of HL's 10/day account-wide trigger slots per CB fire and
	// silently degrade the safety feature for every other strategy on the
	// same wallet (#421 review point 1).
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
			if pos, ok := ss.Positions[c.Symbol]; ok && pos != nil && pos.StopLossOID > 0 {
				slOIDs[c.Symbol] = []int64{pos.StopLossOID}
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

			// Clear the StopLossOID under Lock when the cancel went
			// through, so a follow-up cycle doesn't try to cancel the
			// already-cancelled trigger.
			cancelOID := firstPositiveStopLossOID(cancelOIDs)
			if cancelOID > 0 && result != nil && result.CancelStopLossSucceeded {
				mu.Lock()
				if ss := state.Strategies[j.stratID]; ss != nil {
					if pos, ok := ss.Positions[c.Symbol]; ok && pos != nil && pos.StopLossOID == cancelOID {
						pos.StopLossOID = 0
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
		// partial progress and reset the counter so the next hard error re-fires
		// the first-failure alert.
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
			Timestamp:  now,
			StrategyID: s.ID,
			Symbol:     symbol,
			Side:       closeSide,
			Quantity:   fillSz,
			Price:      fillPx,
			Value:      fillSz * fillPx,
			TradeType:  "perps",
			Details:    fmt.Sprintf("Circuit breaker on-chain close (no virtual position), fill=%.6f fee=$%.4f", fillSz, fillFee),
		})
		return
	}

	qtyClosed := fillSz
	if qtyClosed > pos.Quantity {
		qtyClosed = pos.Quantity
	}
	side := pos.Side
	avgCost := pos.AvgCost
	closeSide := "sell"
	if side == "short" {
		closeSide = "buy"
	}
	var pnl float64
	if side == "long" {
		pnl = qtyClosed * (fillPx - avgCost)
	} else {
		pnl = qtyClosed * (avgCost - fillPx)
	}
	pnl -= fillFee
	s.Cash += pnl

	RecordTrade(s, Trade{
		Timestamp:  now,
		StrategyID: s.ID,
		Symbol:     symbol,
		Side:       closeSide,
		Quantity:   qtyClosed,
		Price:      fillPx,
		Value:      qtyClosed * fillPx,
		TradeType:  "perps",
		Details:    fmt.Sprintf("Circuit breaker on-chain close, PnL: $%.2f (fee $%.4f)", pnl, fillFee),
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
