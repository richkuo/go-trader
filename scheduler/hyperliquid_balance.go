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
				Coin    string `json:"coin"`
				Szi     string `json:"szi"`
				EntryPx string `json:"entryPx"`
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
		positions = append(positions, HLPosition{
			Coin:       ap.Position.Coin,
			Size:       szi,
			EntryPrice: entryPx,
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

	mu.Lock()
	defer mu.Unlock()

	changed := false

	// Build ownership index: coin → strategyID from existing state positions.
	owned := make(map[string]string) // coin → strategy ID
	for _, sc := range hlStrategies {
		ss := state.Strategies[sc.ID]
		if ss == nil {
			continue
		}
		sym := hyperliquidSymbol(sc.Args)
		if sym == "" {
			continue
		}
		if pos, ok := ss.Positions[sym]; ok && pos.OwnerStrategyID != "" {
			if existing, dup := owned[sym]; dup {
				fmt.Printf("[WARN] hl-sync: coin %s claimed by both %s and %s — skipping duplicate\n", sym, existing, sc.ID)
				continue
			}
			owned[sym] = sc.ID
		}
	}

	// Reconcile each strategy's position against on-chain data.
	for _, sc := range hlStrategies {
		ss := state.Strategies[sc.ID]
		if ss == nil {
			continue
		}
		sym := hyperliquidSymbol(sc.Args)
		if sym == "" {
			continue
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

	// Warn about unowned on-chain positions.
	for _, p := range positions {
		if _, ok := owned[p.Coin]; !ok {
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
