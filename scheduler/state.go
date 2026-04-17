package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// maxTradeHistory is the maximum number of trades to retain per strategy.
const maxTradeHistory = 1000

// ReconciliationGap tracks the drift between virtual per-strategy positions and
// the actual on-chain position for a coin that is traded by multiple strategies
// on the same shared wallet (#258). When two strategies trade the same coin,
// per-strategy reconciliation is skipped (to prevent phantom circuit breakers)
// and this gap is computed instead.
type ReconciliationGap struct {
	Coin       string    `json:"coin"`
	OnChainQty float64   `json:"on_chain_qty"` // signed: positive = long, negative = short
	VirtualQty float64   `json:"virtual_qty"`  // sum of all strategies' positions (signed)
	DeltaQty   float64   `json:"delta_qty"`    // computed: VirtualQty - OnChainQty
	Strategies []string  `json:"strategies"`   // strategy IDs configured to trade this coin
	UpdatedAt  time.Time `json:"updated_at"`
}

// AppState holds all persistent state across restarts.
type AppState struct {
	CycleCount          int                       `json:"cycle_count"`
	LastCycle           time.Time                 `json:"last_cycle"`
	Strategies          map[string]*StrategyState `json:"strategies"`
	PortfolioRisk       PortfolioRiskState        `json:"portfolio_risk"`
	CorrelationSnapshot *CorrelationSnapshot      `json:"correlation_snapshot,omitempty"`
	// ReconciliationGaps is ephemeral — recomputed each sync cycle, not persisted to SQLite.
	ReconciliationGaps      map[string]*ReconciliationGap `json:"reconciliation_gaps,omitempty"`
	LastTop10Summary        time.Time                     `json:"last_top10_summary,omitempty"`
	LastLeaderboardPostDate string                        `json:"last_leaderboard_post_date,omitempty"`
}

// StrategyState is the per-strategy persistent state.
type StrategyState struct {
	ID              string                     `json:"id"`
	Type            string                     `json:"type"`
	Platform        string                     `json:"platform,omitempty"`
	Cash            float64                    `json:"cash"`
	InitialCapital  float64                    `json:"initial_capital"`
	Positions       map[string]*Position       `json:"positions"`
	OptionPositions map[string]*OptionPosition `json:"option_positions"`
	TradeHistory    []Trade                    `json:"trade_history"`
	RiskState       RiskState                  `json:"risk_state"`
	// ClosedPositions is an in-memory buffer of positions closed during the
	// current cycle. SaveState appends these to the closed_positions table and
	// clears the buffer on successful commit. Not serialized to JSON state
	// files — history lives exclusively in SQLite. (#288)
	ClosedPositions []ClosedPosition `json:"-"`
}

func NewStrategyState(cfg StrategyConfig) *StrategyState {
	initCap := cfg.Capital
	if cfg.InitialCapital > 0 {
		initCap = cfg.InitialCapital
	}
	return &StrategyState{
		ID:              cfg.ID,
		Type:            cfg.Type,
		Platform:        cfg.Platform,
		Cash:            cfg.Capital,
		InitialCapital:  initCap,
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
	if state.ReconciliationGaps == nil {
		state.ReconciliationGaps = make(map[string]*ReconciliationGap)
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
				continue
			}
			// Migrate legacy positions: stamp ownership if missing.
			if pos.OwnerStrategyID == "" {
				pos.OwnerStrategyID = id
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

// loadJSONPlatformStates loads state from legacy JSON files for one-time migration to SQLite.
// Falls back to cfg.StateFile when no platforms are configured.
func loadJSONPlatformStates(cfg *Config) (*AppState, error) {
	if len(cfg.Platforms) == 0 {
		return LoadState(cfg.StateFile)
	}

	merged := NewAppState()
	for name, pc := range cfg.Platforms {
		stateFile := pc.StateFile
		if stateFile == "" {
			stateFile = fmt.Sprintf("platforms/%s/state.json", name)
		}
		s, err := LoadState(stateFile)
		if err != nil {
			return nil, fmt.Errorf("platform %s: %w", name, err)
		}
		for _, stratState := range s.Strategies {
			if stratState.Platform == "" {
				stratState.Platform = name
			}
			if _, dup := merged.Strategies[stratState.ID]; dup {
				fmt.Printf("[state] skipping duplicate strategy %s from %s\n", stratState.ID, stateFile)
				continue
			}
			merged.Strategies[stratState.ID] = stratState
		}
		if merged.CycleCount < s.CycleCount {
			merged.CycleCount = s.CycleCount
		}
		if s.LastCycle.After(merged.LastCycle) {
			merged.LastCycle = s.LastCycle
		}
		if s.PortfolioRisk.PeakValue > merged.PortfolioRisk.PeakValue {
			merged.PortfolioRisk.PeakValue = s.PortfolioRisk.PeakValue
		}
	}
	return merged, nil
}

// LoadStateWithDB loads state from SQLite. If SQLite is empty, attempts one-time
// migration from legacy JSON state files.
func LoadStateWithDB(cfg *Config, sdb *StateDB) (*AppState, error) {
	state, err := sdb.LoadState()
	if err != nil {
		return nil, fmt.Errorf("sqlite load: %w", err)
	}
	if state != nil {
		fmt.Println("[state] Loaded from SQLite")
		return state, nil
	}

	// SQLite empty — try legacy JSON migration.
	state, err = loadJSONPlatformStates(cfg)
	if err != nil {
		return nil, err
	}

	// One-time migration: persist JSON data into SQLite.
	if state.CycleCount > 0 || len(state.Strategies) > 0 {
		fmt.Println("[state] Migrating JSON → SQLite (one-time)")
		if err := sdb.SaveState(state); err != nil {
			fmt.Printf("[WARN] SQLite migration failed: %v\n", err)
		}
	}

	return state, nil
}

// SaveStateWithDB saves state to SQLite.
func SaveStateWithDB(state *AppState, cfg *Config, sdb *StateDB) error {
	return sdb.SaveState(state)
}
