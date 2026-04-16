package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
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
		if onChainPos.Leverage > 0 && statePos.Leverage != onChainPos.Leverage {
			logger.Info("hl-sync: %s leverage %v → %v", sym, statePos.Leverage, onChainPos.Leverage)
			statePos.Leverage = onChainPos.Leverage
			changed = true
		}
	} else if onChainPos == nil && statePos != nil {
		// Position in state but not on-chain — closed externally.
		logger.Info("hl-sync: %s position (%.6f %s) no longer on-chain, removing",
			sym, statePos.Quantity, statePos.Side)
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
			} else {
				virtualQty -= pos.Quantity
			}
			// Non-destructive: migrate multiplier (#254).
			if pos.Multiplier != 1 {
				logger, _ := logMgr.GetStrategyLogger(id)
				if logger != nil {
					logger.Info("hl-sync: %s migrate multiplier %v → 1 (shared coin) (#254)", coin, pos.Multiplier)
				}
				pos.Multiplier = 1
				changed = true
			}
			// Non-destructive: sync leverage from on-chain.
			if onChainPos != nil && onChainPos.Leverage > 0 && pos.Leverage != onChainPos.Leverage {
				logger, _ := logMgr.GetStrategyLogger(id)
				if logger != nil {
					logger.Info("hl-sync: %s leverage %v → %v (shared coin)", coin, pos.Leverage, onChainPos.Leverage)
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
