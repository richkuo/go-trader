package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// maxTradeHistory is the maximum number of trades to retain per strategy.
const maxTradeHistory = 1000

// AppState holds all persistent state across restarts.
type AppState struct {
	CycleCount int                       `json:"cycle_count"`
	LastCycle  time.Time                 `json:"last_cycle"`
	Strategies map[string]*StrategyState `json:"strategies"`
}

// StrategyState is the per-strategy persistent state.
type StrategyState struct {
	ID              string                     `json:"id"`
	Type            string                     `json:"type"`
	Cash            float64                    `json:"cash"`
	InitialCapital  float64                    `json:"initial_capital"`
	Positions       map[string]*Position       `json:"positions"`
	OptionPositions map[string]*OptionPosition `json:"option_positions"`
	TradeHistory    []Trade                    `json:"trade_history"`
	RiskState       RiskState                  `json:"risk_state"`
}

func NewStrategyState(cfg StrategyConfig) *StrategyState {
	return &StrategyState{
		ID:              cfg.ID,
		Type:            cfg.Type,
		Cash:            cfg.Capital,
		InitialCapital:  cfg.Capital,
		Positions:       make(map[string]*Position),
		OptionPositions: make(map[string]*OptionPosition),
		TradeHistory:    []Trade{},
		RiskState: RiskState{
			PeakValue:      cfg.Capital,
			MaxDrawdownPct: cfg.MaxDrawdownPct,
		},
	}
}

func NewAppState() *AppState {
	return &AppState{
		CycleCount: 0,
		Strategies: make(map[string]*StrategyState),
	}
}

func LoadState(path string) (*AppState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return NewAppState(), nil
		}
		return nil, fmt.Errorf("read state: %w", err)
	}
	var state AppState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("parse state: %w", err)
	}
	if state.Strategies == nil {
		state.Strategies = make(map[string]*StrategyState)
	}
	// Fix nil maps
	for _, s := range state.Strategies {
		if s.Positions == nil {
			s.Positions = make(map[string]*Position)
		}
		if s.OptionPositions == nil {
			s.OptionPositions = make(map[string]*OptionPosition)
		}
		if s.TradeHistory == nil {
			s.TradeHistory = []Trade{}
		}
	}
	return &state, nil
}

// ValidateState checks loaded state for invalid entries and removes or clamps them (#39).
// Logs warnings for each corrected field rather than refusing to start.
func ValidateState(state *AppState) {
	for id, s := range state.Strategies {
		if s.InitialCapital <= 0 {
			fmt.Printf("[WARN] state: strategy %s has invalid initial_capital=%g, resetting to 0\n", id, s.InitialCapital)
			s.InitialCapital = 0
		}
		if s.Cash < 0 {
			fmt.Printf("[WARN] state: strategy %s has negative cash=%g, clamping to 0\n", id, s.Cash)
			s.Cash = 0
		}
		for sym, pos := range s.Positions {
			if pos.Quantity <= 0 {
				fmt.Printf("[WARN] state: strategy %s position %s has invalid quantity=%g, removing\n", id, sym, pos.Quantity)
				delete(s.Positions, sym)
			}
		}
		for key, op := range s.OptionPositions {
			valid := true
			if op.Action != "buy" && op.Action != "sell" {
				fmt.Printf("[WARN] state: strategy %s option %s has invalid action=%q, removing\n", id, key, op.Action)
				valid = false
			}
			if op.OptionType != "call" && op.OptionType != "put" {
				fmt.Printf("[WARN] state: strategy %s option %s has invalid option_type=%q, removing\n", id, key, op.OptionType)
				valid = false
			}
			if op.Quantity <= 0 {
				fmt.Printf("[WARN] state: strategy %s option %s has invalid quantity=%g, removing\n", id, key, op.Quantity)
				valid = false
			}
			if !valid {
				delete(s.OptionPositions, key)
			}
		}
	}
}

func SaveState(path string, state *AppState) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}

	// Trim trade history to prevent unbounded growth
	for _, s := range state.Strategies {
		if len(s.TradeHistory) > maxTradeHistory {
			s.TradeHistory = s.TradeHistory[len(s.TradeHistory)-maxTradeHistory:]
		}
	}

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0600); err != nil {
		return fmt.Errorf("write state: %w", err)
	}
	return os.Rename(tmpPath, path)
}
