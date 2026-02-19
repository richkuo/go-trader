package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// StatusServer provides an HTTP endpoint for portfolio status.
type StatusServer struct {
	state        *AppState
	mu           *sync.RWMutex
	statusToken  string   // if non-empty, /status requires Authorization: Bearer <token>
	priceSymbols []string // symbols to always fetch prices for
}

func NewStatusServer(state *AppState, mu *sync.RWMutex, statusToken string, strategies []StrategyConfig) *StatusServer {
	// Extract all traded symbols from strategy configs so prices are always fetched,
	// even when no positions are open.
	symbolSet := make(map[string]bool)
	for _, sc := range strategies {
		if sc.Type == "spot" && len(sc.Args) >= 2 {
			symbolSet[sc.Args[1]] = true // e.g., "BTC/USDT"
		}
	}
	symbols := make([]string, 0, len(symbolSet))
	for s := range symbolSet {
		symbols = append(symbols, s)
	}
	return &StatusServer{state: state, mu: mu, statusToken: statusToken, priceSymbols: symbols}
}

func (ss *StatusServer) Start(port int) {
	mux := http.NewServeMux()
	mux.HandleFunc("/status", ss.handleStatus)
	mux.HandleFunc("/health", ss.handleHealth)

	addr := fmt.Sprintf("localhost:%d", port)
	go func() {
		fmt.Printf("[server] Status endpoint at http://%s/status\n", addr)
		if err := http.ListenAndServe(addr, mux); err != nil {
			fmt.Printf("[server] HTTP server error: %v\n", err)
		}
	}()
}

func (ss *StatusServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	ss.mu.RLock()
	lastCycle := ss.state.LastCycle
	ss.mu.RUnlock()

	// Stale if main loop hasn't completed a cycle in the last 30 minutes
	if !lastCycle.IsZero() && time.Since(lastCycle) > 30*time.Minute {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"status":"unhealthy","reason":"main loop stale"}`))
		return
	}
	w.Write([]byte(`{"status":"ok"}`))
}

func (ss *StatusServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	// #38: Optional bearer token auth for /status.
	if ss.statusToken != "" {
		if r.Header.Get("Authorization") != "Bearer "+ss.statusToken {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}
	}

	// Always fetch prices for all configured symbols + any with open positions.
	symbolSet := make(map[string]bool)
	for _, sym := range ss.priceSymbols {
		symbolSet[sym] = true
	}
	// Also include symbols from open positions (in case config changed).
	ss.mu.RLock()
	for _, s := range ss.state.Strategies {
		for sym := range s.Positions {
			symbolSet[sym] = true
		}
	}
	ss.mu.RUnlock()

	symbols := make([]string, 0, len(symbolSet))
	for s := range symbolSet {
		symbols = append(symbols, s)
	}

	// Fetch live prices WITHOUT holding the lock
	prices := make(map[string]float64)
	if len(symbols) > 0 {
		p, err := FetchPrices(symbols)
		if err == nil {
			prices = p
		}
	}

	// Re-acquire read lock to build the response
	ss.mu.RLock()
	defer ss.mu.RUnlock()

	type StratStatus struct {
		ID              string                     `json:"id"`
		Type            string                     `json:"type"`
		Cash            float64                    `json:"cash"`
		InitialCapital  float64                    `json:"initial_capital"`
		Positions       map[string]*Position       `json:"positions"`
		OptionPositions map[string]*OptionPosition `json:"option_positions"`
		TradeCount      int                        `json:"trade_count"`
		PortfolioValue  float64                    `json:"portfolio_value"`
		PnL             float64                    `json:"pnl"`
		PnLPct          float64                    `json:"pnl_pct"`
		RiskState       RiskState                  `json:"risk_state"`
	}

	type StatusResp struct {
		CycleCount int                    `json:"cycle_count"`
		Prices     map[string]float64     `json:"prices"`
		Strategies map[string]StratStatus `json:"strategies"`
	}

	resp := StatusResp{
		CycleCount: ss.state.CycleCount,
		Prices:     prices,
		Strategies: make(map[string]StratStatus),
	}

	for id, s := range ss.state.Strategies {
		pv := PortfolioValue(s, prices)
		pnl := pv - s.InitialCapital
		pnlPct := 0.0
		if s.InitialCapital > 0 {
			pnlPct = (pnl / s.InitialCapital) * 100
		}
		resp.Strategies[id] = StratStatus{
			ID:              s.ID,
			Type:            s.Type,
			Cash:            s.Cash,
			InitialCapital:  s.InitialCapital,
			Positions:       s.Positions,
			OptionPositions: s.OptionPositions,
			TradeCount:      len(s.TradeHistory),
			PortfolioValue:  pv,
			PnL:             pnl,
			PnLPct:          pnlPct,
			RiskState:       s.RiskState,
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}
