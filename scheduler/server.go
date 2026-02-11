package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
)

// StatusServer provides an HTTP endpoint for portfolio status.
type StatusServer struct {
	state *AppState
	mu    *sync.RWMutex
}

func NewStatusServer(state *AppState, mu *sync.RWMutex) *StatusServer {
	return &StatusServer{state: state, mu: mu}
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
	w.Write([]byte(`{"status":"ok"}`))
}

func (ss *StatusServer) handleStatus(w http.ResponseWriter, r *http.Request) {
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
		RiskState       RiskState                  `json:"risk_state"`
	}

	type StatusResp struct {
		CycleCount int                    `json:"cycle_count"`
		Strategies map[string]StratStatus `json:"strategies"`
	}

	resp := StatusResp{
		CycleCount: ss.state.CycleCount,
		Strategies: make(map[string]StratStatus),
	}

	for id, s := range ss.state.Strategies {
		resp.Strategies[id] = StratStatus{
			ID:              s.ID,
			Type:            s.Type,
			Cash:            s.Cash,
			InitialCapital:  s.InitialCapital,
			Positions:       s.Positions,
			OptionPositions: s.OptionPositions,
			TradeCount:      len(s.TradeHistory),
			RiskState:       s.RiskState,
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}
