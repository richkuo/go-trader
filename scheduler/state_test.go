package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestNewAppState(t *testing.T) {
	state := NewAppState()
	if state.CycleCount != 0 {
		t.Errorf("CycleCount = %d, want 0", state.CycleCount)
	}
	if state.Strategies == nil {
		t.Error("Strategies map should not be nil")
	}
	if len(state.Strategies) != 0 {
		t.Errorf("Strategies should be empty, got %d", len(state.Strategies))
	}
}

func TestNewStrategyState(t *testing.T) {
	cfg := StrategyConfig{
		ID:             "test-spot-btc",
		Type:           "spot",
		Platform:       "binanceus",
		Capital:        1000,
		MaxDrawdownPct: 60,
	}
	s := NewStrategyState(cfg)
	if s.ID != "test-spot-btc" {
		t.Errorf("ID = %q, want %q", s.ID, "test-spot-btc")
	}
	if s.Type != "spot" {
		t.Errorf("Type = %q, want %q", s.Type, "spot")
	}
	if s.Platform != "binanceus" {
		t.Errorf("Platform = %q, want %q", s.Platform, "binanceus")
	}
	if s.Cash != 1000 {
		t.Errorf("Cash = %g, want 1000", s.Cash)
	}
	if s.InitialCapital != 1000 {
		t.Errorf("InitialCapital = %g, want 1000", s.InitialCapital)
	}
	if s.Positions == nil {
		t.Error("Positions should not be nil")
	}
	if s.OptionPositions == nil {
		t.Error("OptionPositions should not be nil")
	}
	if s.TradeHistory == nil {
		t.Error("TradeHistory should not be nil")
	}
	if s.RiskState.PeakValue != 1000 {
		t.Errorf("RiskState.PeakValue = %g, want 1000", s.RiskState.PeakValue)
	}
	if s.RiskState.MaxDrawdownPct != 60 {
		t.Errorf("RiskState.MaxDrawdownPct = %g, want 60", s.RiskState.MaxDrawdownPct)
	}
}

func TestValidateState(t *testing.T) {
	state := NewAppState()
	state.Strategies["s1"] = &StrategyState{
		ID:             "s1",
		InitialCapital: -100, // invalid
		Cash:           -50,  // negative
		Positions: map[string]*Position{
			"BTC/USDT": {Quantity: 0.01, Side: "long"},
			"ETH/USDT": {Quantity: 0, Side: "long"},   // invalid: zero
			"SOL/USDT": {Quantity: -1, Side: "short"}, // invalid: negative
		},
		OptionPositions: map[string]*OptionPosition{
			"valid":   {Action: "buy", OptionType: "call", Quantity: 1},
			"badact":  {Action: "invalid", OptionType: "call", Quantity: 1},
			"badtype": {Action: "sell", OptionType: "invalid", Quantity: 1},
			"badqty":  {Action: "buy", OptionType: "put", Quantity: 0},
		},
		TradeHistory: []Trade{},
	}

	ValidateState(state)

	s := state.Strategies["s1"]
	if s.InitialCapital != 0 {
		t.Errorf("InitialCapital should be reset to 0, got %g", s.InitialCapital)
	}
	if s.Cash != 0 {
		t.Errorf("Cash should be clamped to 0, got %g", s.Cash)
	}
	if _, ok := s.Positions["BTC/USDT"]; !ok {
		t.Error("valid position BTC/USDT should remain")
	}
	if _, ok := s.Positions["ETH/USDT"]; ok {
		t.Error("zero-quantity position should be removed")
	}
	if _, ok := s.Positions["SOL/USDT"]; ok {
		t.Error("negative-quantity position should be removed")
	}
	if _, ok := s.OptionPositions["valid"]; !ok {
		t.Error("valid option should remain")
	}
	if _, ok := s.OptionPositions["badact"]; ok {
		t.Error("invalid-action option should be removed")
	}
	if _, ok := s.OptionPositions["badtype"]; ok {
		t.Error("invalid-type option should be removed")
	}
	if _, ok := s.OptionPositions["badqty"]; ok {
		t.Error("zero-quantity option should be removed")
	}
}

// TestValidatePerpsAllowShortsConfig exercises the #336 startup check: a short
// position seeded into SQLite (via migration, paper→live handoff, or operator
// edit) under a perps strategy whose config has AllowShorts=false must be
// flagged so the operator sees it before the executor's flip path quietly
// desyncs virtual state from the exchange on the next buy signal.
func TestValidatePerpsAllowShortsConfig(t *testing.T) {
	state := NewAppState()
	state.Strategies["hl-triple-ema-eth"] = &StrategyState{
		ID:   "hl-triple-ema-eth",
		Type: "perps",
		Positions: map[string]*Position{
			// The gap case: short under AllowShorts=false.
			"ETH": {Symbol: "ETH", Quantity: 0.5, AvgCost: 2000, Side: "short", Multiplier: 1, Leverage: 1},
		},
	}
	state.Strategies["hl-bidir-btc"] = &StrategyState{
		ID:   "hl-bidir-btc",
		Type: "perps",
		Positions: map[string]*Position{
			// Allowed short: AllowShorts=true in config below, must not warn.
			"BTC": {Symbol: "BTC", Quantity: 0.1, AvgCost: 60000, Side: "short", Multiplier: 1, Leverage: 1},
		},
	}
	state.Strategies["bn-sma-btc"] = &StrategyState{
		ID:   "bn-sma-btc",
		Type: "spot",
		Positions: map[string]*Position{
			// Non-perps: AllowShorts is meaningless, must not warn.
			"BTC/USDT": {Symbol: "BTC/USDT", Quantity: 0.01, AvgCost: 60000, Side: "long"},
		},
	}

	cfg := &Config{
		Strategies: []StrategyConfig{
			{ID: "hl-triple-ema-eth", Type: "perps", Platform: "hyperliquid", AllowShorts: false},
			{ID: "hl-bidir-btc", Type: "perps", Platform: "hyperliquid", AllowShorts: true},
			{ID: "bn-sma-btc", Type: "spot", Platform: "binanceus", AllowShorts: false},
		},
	}

	warnings := ValidatePerpsAllowShortsConfig(state, cfg)
	if len(warnings) != 1 {
		t.Fatalf("want 1 warning, got %d: %v", len(warnings), warnings)
	}
	w := warnings[0]
	if !strings.Contains(w, "hl-triple-ema-eth") {
		t.Errorf("warning should name the offending strategy, got: %s", w)
	}
	if !strings.Contains(w, "ETH") {
		t.Errorf("warning should name the symbol, got: %s", w)
	}
	if !strings.Contains(w, "AllowShorts=false") {
		t.Errorf("warning should cite the config gap, got: %s", w)
	}
}

// TestValidatePerpsAllowShortsConfig_NoShorts confirms the validator is silent
// when all perps positions match their strategy's AllowShorts setting.
func TestValidatePerpsAllowShortsConfig_NoShorts(t *testing.T) {
	state := NewAppState()
	state.Strategies["hl-triple-ema-eth"] = &StrategyState{
		ID:   "hl-triple-ema-eth",
		Type: "perps",
		Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 0.5, AvgCost: 2000, Side: "long", Multiplier: 1, Leverage: 1},
		},
	}
	cfg := &Config{
		Strategies: []StrategyConfig{
			{ID: "hl-triple-ema-eth", Type: "perps", Platform: "hyperliquid", AllowShorts: false},
		},
	}
	if warnings := ValidatePerpsAllowShortsConfig(state, cfg); len(warnings) != 0 {
		t.Errorf("want no warnings for a long-only state, got: %v", warnings)
	}
}

// TestValidatePerpsAllowShortsConfig_OrphanState verifies we don't crash when
// state has a strategy that's been removed from config (pruning happens
// separately in main.go but validators must tolerate the intermediate state).
func TestValidatePerpsAllowShortsConfig_OrphanState(t *testing.T) {
	state := NewAppState()
	state.Strategies["gone"] = &StrategyState{
		ID:   "gone",
		Type: "perps",
		Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 0.5, Side: "short", Multiplier: 1, Leverage: 1},
		},
	}
	cfg := &Config{Strategies: []StrategyConfig{}}
	if warnings := ValidatePerpsAllowShortsConfig(state, cfg); len(warnings) != 0 {
		t.Errorf("orphan state should not produce warnings, got: %v", warnings)
	}
}

func TestLoadStateWithDB_SQLitePrimary(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")

	db, err := OpenStateDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	original := &AppState{
		CycleCount: 10,
		Strategies: map[string]*StrategyState{
			"test": {ID: "test", Type: "spot", Cash: 500, InitialCapital: 1000,
				Positions: make(map[string]*Position), OptionPositions: make(map[string]*OptionPosition), TradeHistory: []Trade{}},
		},
	}
	if err := db.SaveState(original); err != nil {
		t.Fatal(err)
	}

	cfg := &Config{DBFile: dbPath}
	loaded, err := LoadStateWithDB(cfg, db)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.CycleCount != 10 {
		t.Errorf("CycleCount = %d, want 10", loaded.CycleCount)
	}
}

func TestLoadStateWithDB_EmptyDB(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")

	db, err := OpenStateDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	cfg := &Config{DBFile: dbPath}
	loaded, err := LoadStateWithDB(cfg, db)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.CycleCount != 0 {
		t.Errorf("CycleCount = %d, want 0 (fresh start)", loaded.CycleCount)
	}
	if len(loaded.Strategies) != 0 {
		t.Errorf("strategies = %d, want 0", len(loaded.Strategies))
	}
}

func TestSaveStateWithDB(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")

	db, err := OpenStateDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	state := &AppState{
		CycleCount: 3,
		Strategies: map[string]*StrategyState{
			"test": {ID: "test", Type: "spot", Cash: 800, InitialCapital: 1000,
				Positions: make(map[string]*Position), OptionPositions: make(map[string]*OptionPosition), TradeHistory: []Trade{}},
		},
	}

	cfg := &Config{}
	if err := SaveStateWithDB(state, cfg, db); err != nil {
		t.Fatal(err)
	}

	dbState, err := db.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if dbState.CycleCount != 3 {
		t.Errorf("SQLite CycleCount = %d, want 3", dbState.CycleCount)
	}
}

func TestSaveStateWithDB_Error(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")

	// Create a broken StateDB by closing it before use.
	db, err := OpenStateDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()

	state := &AppState{CycleCount: 5, Strategies: make(map[string]*StrategyState)}
	cfg := &Config{}
	err = SaveStateWithDB(state, cfg, db)
	if err == nil {
		t.Error("expected error when SQLite is closed")
	}
}

func TestNewStrategyState_ConfigInitialCapital(t *testing.T) {
	// When config has InitialCapital set, it should be used instead of Capital.
	cfg := StrategyConfig{
		ID:             "hl-sma-btc",
		Type:           "perps",
		Platform:       "hyperliquid",
		Capital:        600,
		InitialCapital: 505,
		MaxDrawdownPct: 10,
	}
	s := NewStrategyState(cfg)
	if s.InitialCapital != 505 {
		t.Errorf("InitialCapital = %g, want 505 (from config)", s.InitialCapital)
	}
	if s.Cash != 600 {
		t.Errorf("Cash = %g, want 600 (from Capital)", s.Cash)
	}
}

func TestNewStrategyState_NoConfigInitialCapital(t *testing.T) {
	// When config has no InitialCapital, it should fall back to Capital.
	cfg := StrategyConfig{
		ID:             "hl-sma-btc",
		Type:           "perps",
		Platform:       "hyperliquid",
		Capital:        600,
		MaxDrawdownPct: 10,
	}
	s := NewStrategyState(cfg)
	if s.InitialCapital != 600 {
		t.Errorf("InitialCapital = %g, want 600 (from Capital fallback)", s.InitialCapital)
	}
}
