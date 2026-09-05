package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestNewStrategyState(t *testing.T) {
	cases := []struct {
		name        string
		cfg         StrategyConfig
		wantCash    float64
		wantInitial float64
	}{
		{
			name:     "capital seeds cash and initial capital",
			cfg:      StrategyConfig{ID: "test-spot-btc", Type: "spot", Platform: "binanceus", Capital: 1000, MaxDrawdownPct: 60},
			wantCash: 1000, wantInitial: 1000,
		},
		{
			name:     "config initial_capital overrides capital",
			cfg:      StrategyConfig{ID: "hl-sma-btc", Type: "perps", Platform: "hyperliquid", Capital: 600, InitialCapital: 505, MaxDrawdownPct: 10},
			wantCash: 600, wantInitial: 505,
		},
		{
			name:     "no config initial_capital falls back to capital",
			cfg:      StrategyConfig{ID: "hl-sma-btc", Type: "perps", Platform: "hyperliquid", Capital: 600, MaxDrawdownPct: 10},
			wantCash: 600, wantInitial: 600,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := NewStrategyState(tc.cfg)
			if s.ID != tc.cfg.ID || s.Type != tc.cfg.Type || s.Platform != tc.cfg.Platform {
				t.Errorf("identity = (%q, %q, %q), want (%q, %q, %q)", s.ID, s.Type, s.Platform, tc.cfg.ID, tc.cfg.Type, tc.cfg.Platform)
			}
			if s.Cash != tc.wantCash {
				t.Errorf("Cash = %g, want %g", s.Cash, tc.wantCash)
			}
			if s.InitialCapital != tc.wantInitial {
				t.Errorf("InitialCapital = %g, want %g", s.InitialCapital, tc.wantInitial)
			}
			if s.Positions == nil || s.OptionPositions == nil || s.TradeHistory == nil {
				t.Errorf("maps and trade history must be initialized: %+v", s)
			}
			if s.RiskState.PeakValue != tc.cfg.Capital {
				t.Errorf("RiskState.PeakValue = %g, want %g", s.RiskState.PeakValue, tc.cfg.Capital)
			}
			if s.RiskState.MaxDrawdownPct != tc.cfg.MaxDrawdownPct {
				t.Errorf("RiskState.MaxDrawdownPct = %g, want %g", s.RiskState.MaxDrawdownPct, tc.cfg.MaxDrawdownPct)
			}
		})
	}
}

func TestApplySharedWalletPoolStateModeClearsLegacyAllocation(t *testing.T) {
	sc := StrategyConfig{sharedWalletPoolBudget: true}
	s := &StrategyState{
		Cash:           500,
		InitialCapital: 500,
		RiskState:      RiskState{PeakValue: 500, CurrentDrawdownPct: 12},
	}
	transition, err := applySharedWalletPoolStateMode(sc, s)
	if err != nil || transition != sharedWalletPoolStateEntered {
		t.Fatalf("expected pool entry, got transition=%q err=%v", transition, err)
	}
	if s.Cash != 0 || s.InitialCapital != 0 || s.RiskState.PeakValue != 0 || s.RiskState.CurrentDrawdownPct != 0 {
		t.Fatalf("pooled state not reset: %+v", s)
	}
	if !s.SharedWalletPerformanceOnly || !s.SharedWalletPoolBudget {
		t.Fatal("pooled state must be marked performance-only")
	}
}

func TestApplySharedWalletPoolStateModeLeavingReseedsOnce(t *testing.T) {
	s := &StrategyState{
		ID: "hl-a", Type: "perps",
		Cash:                        -100,
		InitialCapital:              0,
		SharedWalletPoolBudget:      true,
		SharedWalletPerformanceOnly: true,
		Positions:                   map[string]*Position{},
	}
	allocated := StrategyConfig{ID: "hl-a", Type: "perps", Capital: 1000}
	transition, err := applySharedWalletPoolStateMode(allocated, s)
	if err != nil || transition != sharedWalletPoolStateLeft {
		t.Fatalf("expected pool exit, got transition=%q err=%v", transition, err)
	}
	if s.Cash != 900 || s.InitialCapital != 1000 {
		t.Fatalf("pool exit must add allocation while preserving losses: cash=%v initial=%v", s.Cash, s.InitialCapital)
	}
	if s.SharedWalletPoolBudget || s.SharedWalletPerformanceOnly {
		t.Fatal("pool exit must clear pool markers")
	}
	if s.RiskState.PeakValue != 900 {
		t.Fatalf("flat pool exit peak=%v, want preserved net book 900", s.RiskState.PeakValue)
	}

	transition, err = applySharedWalletPoolStateMode(allocated, s)
	if err != nil || transition != sharedWalletPoolStateUnchanged || s.Cash != 900 {
		t.Fatalf("allocated restart must not reseed twice: transition=%q cash=%v err=%v", transition, s.Cash, err)
	}

	pooled := allocated
	pooled.Capital = 0
	pooled.sharedWalletPoolBudget = true
	transition, err = applySharedWalletPoolStateMode(pooled, s)
	if err != nil || transition != sharedWalletPoolStateEntered || s.Cash != 0 || s.InitialCapital != 0 {
		t.Fatalf("round trip back to pool failed: transition=%q state=%+v err=%v", transition, s, err)
	}
}

func TestApplySharedWalletPoolStateModeDefersUnresolvedCapitalPctManageOnly(t *testing.T) {
	s := &StrategyState{
		ID:                          "hl-a",
		Cash:                        -75,
		InitialCapital:              0,
		SharedWalletPoolBudget:      true,
		SharedWalletPerformanceOnly: true,
		Positions:                   map[string]*Position{},
	}
	sc := StrategyConfig{ID: "hl-a", Type: "perps", CapitalPct: 0.5}

	transition, err := applySharedWalletPoolStateMode(sc, s)
	if err == nil || transition != sharedWalletPoolStateUnchanged {
		t.Fatalf("unresolved capital_pct exit: transition=%q err=%v", transition, err)
	}
	if !s.SharedWalletPoolBudget || !s.SharedWalletPerformanceOnly || s.Cash != -75 {
		t.Fatalf("failed transition must preserve durable pool book: %+v", s)
	}

	msg := deferSharedWalletPoolTransition(&sc, err)
	if msg == "" || !sc.sharedWalletModeDeferred || !sc.Paused {
		t.Fatalf("deferred transition must latch manage-only mode: sc=%+v msg=%q", sc, msg)
	}
	if shouldSkipZeroCapital(sc) {
		t.Fatal("deferred pool exit must stay scheduled so protection management runs")
	}
	if !pausedBlocksSignal(-1, 0, 1, "long", true, true) {
		t.Fatal("manage-only deferred exit must block a bidirectional flip")
	}
	if pausedBlocksSignal(-1, 1, 1, "long", true, true) {
		t.Fatal("manage-only deferred exit must allow a close-registry exit")
	}

	retry := StrategyConfig{ID: "hl-a", Type: "perps", CapitalPct: 0.5, Capital: 500}
	transition, err = applySharedWalletPoolStateMode(retry, s)
	if err != nil || transition != sharedWalletPoolStateLeft {
		t.Fatalf("later resolved restart must complete transition: transition=%q err=%v", transition, err)
	}
	if s.SharedWalletPoolBudget || s.Cash != 425 || s.InitialCapital != 500 {
		t.Fatalf("resolved retry must reseed once while preserving loss: %+v", s)
	}
}

func TestApplySharedWalletPoolStateModeDefersBothDirectionsWhileOpen(t *testing.T) {
	tests := []struct {
		name             string
		sc               StrategyConfig
		state            *StrategyState
		wantPool         bool
		wantPerformance  bool
		increasingSignal int
	}{
		{
			name: "pool to allocated blocks scale-in",
			sc:   StrategyConfig{ID: "hl-a", Type: "perps", Capital: 1000},
			state: &StrategyState{
				ID: "hl-a", Type: "perps", Cash: -100,
				SharedWalletPoolBudget: true, SharedWalletPerformanceOnly: true,
				Positions: map[string]*Position{
					"BTC": {Symbol: "BTC", Quantity: 1, AvgCost: 200, Side: "long"},
				},
			},
			wantPool: true, wantPerformance: true, increasingSignal: 1,
		},
		{
			name: "allocated to pool blocks flip",
			sc: StrategyConfig{
				ID: "hl-a", Type: "perps", sharedWalletPoolBudget: true,
			},
			state: &StrategyState{
				ID: "hl-a", Type: "perps", Cash: 900, InitialCapital: 1000,
				Positions: map[string]*Position{
					"BTC": {Symbol: "BTC", Quantity: 1, AvgCost: 200, Side: "long"},
				},
			},
			wantPool: false, wantPerformance: false, increasingSignal: -1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			beforeCash, beforeInitial := tt.state.Cash, tt.state.InitialCapital
			transition, err := applySharedWalletPoolStateMode(tt.sc, tt.state)
			if err == nil || transition != sharedWalletPoolStateUnchanged {
				t.Fatalf("open transition must defer: transition=%q err=%v", transition, err)
			}
			if tt.state.SharedWalletPoolBudget != tt.wantPool ||
				tt.state.SharedWalletPerformanceOnly != tt.wantPerformance ||
				tt.state.Cash != beforeCash || tt.state.InitialCapital != beforeInitial {
				t.Fatalf("deferred transition changed the durable book: %+v", tt.state)
			}

			msg := deferSharedWalletPoolTransition(&tt.sc, err)
			if msg == "" || !tt.sc.Paused || !tt.sc.sharedWalletModeDeferred {
				t.Fatalf("deferred transition must enter manage-only: sc=%+v msg=%q", tt.sc, msg)
			}
			if got := effectiveSharedWalletPoolBook(tt.sc, tt.state); got != tt.wantPool {
				t.Fatalf("deferred display/reconciliation mode=%t, want durable mode %t", got, tt.wantPool)
			}
			if !pausedBlocksSignal(tt.increasingSignal, 0, 1, "long", true, tt.sc.Paused) {
				t.Fatal("manage-only transition must block scale-ins and flips")
			}
			if pausedBlocksSignal(-1, 1, 1, "long", true, tt.sc.Paused) {
				t.Fatal("manage-only transition must preserve explicit closes")
			}
		})
	}
}

func TestSharedWalletPoolTransitionBlockersAreWalletAtomic(t *testing.T) {
	margin := 100.0
	strategies := []StrategyConfig{
		{
			ID: "hl-open", Platform: "hyperliquid", Type: "perps",
			Args:              []string{"sma", "BTC", "1h", "--mode=live"},
			MarginPerTradeUSD: &margin, sharedWalletPoolBudget: true,
		},
		{
			ID: "hl-flat", Platform: "hyperliquid", Type: "perps",
			Args:              []string{"rsi", "ETH", "1h", "--mode=live"},
			MarginPerTradeUSD: &margin, sharedWalletPoolBudget: true,
		},
	}
	state := &AppState{Strategies: map[string]*StrategyState{
		"hl-open": {
			SharedWalletPoolBudget: false,
			Positions: map[string]*Position{
				"BTC": {Symbol: "BTC", Quantity: 1, Side: "long"},
			},
		},
		"hl-flat": {SharedWalletPoolBudget: false, Positions: map[string]*Position{}},
	}}

	blocked := sharedWalletPoolTransitionBlockers(strategies, state)
	for _, id := range []string{"hl-open", "hl-flat"} {
		if blocked[id] == nil {
			t.Fatalf("wallet peer %s must be blocked until the whole wallet is flat: %v", id, blocked)
		}
	}

	state.Strategies["hl-open"].Positions = map[string]*Position{}
	if got := sharedWalletPoolTransitionBlockers(strategies, state); len(got) != 0 {
		t.Fatalf("flat wallet transition must remain unchanged: %v", got)
	}
}

func TestValidateState(t *testing.T) {
	state := NewAppState()
	state.Strategies["s1"] = &StrategyState{
		ID:             "s1",
		InitialCapital: -100,
		Cash:           -50,
		Positions: map[string]*Position{
			"BTC/USDT": {Quantity: 0.01, Side: "long"},
			"ETH/USDT": {Quantity: 0, Side: "long"},
			"SOL/USDT": {Quantity: -1, Side: "short"},
		},
		OptionPositions: map[string]*OptionPosition{
			"valid":   {Action: "buy", OptionType: "call", Quantity: 1},
			"badact":  {Action: "invalid", OptionType: "call", Quantity: 1},
			"badtype": {Action: "sell", OptionType: "invalid", Quantity: 1},
			"badqty":  {Action: "buy", OptionType: "put", Quantity: 0},
		},
		TradeHistory: []Trade{},
	}
	state.Strategies["pool"] = &StrategyState{
		ID:              "pool",
		InitialCapital:  0,
		Cash:            -25,
		Positions:       map[string]*Position{},
		OptionPositions: map[string]*OptionPosition{},
	}

	ValidateState(state, []StrategyConfig{{ID: "pool", sharedWalletPoolBudget: true}})

	s := state.Strategies["s1"]
	if s.InitialCapital != 0 {
		t.Errorf("InitialCapital should be reset to 0, got %g", s.InitialCapital)
	}
	if s.Cash != 0 {
		t.Errorf("Cash should be clamped to 0, got %g", s.Cash)
	}
	if got := state.Strategies["pool"].Cash; got != -25 {
		t.Errorf("configured pool performance cash should survive validation, got %g", got)
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

func makeRegimeDirectionalPolicyForValidation() *RegimeDirectionalPolicy {
	return &RegimeDirectionalPolicy{TrendRegime: map[string]RegimeDirectionalEntry{
		"trending_up":   {Direction: DirectionLong, InvertSignal: false},
		"trending_down": {Direction: DirectionShort, InvertSignal: true},
		"ranging":       {Direction: DirectionLong, InvertSignal: false},
	}}
}

func TestValidatePerpsDirectionConfig(t *testing.T) {
	perpsPos := func(sym string, qty, avg float64, side string) *Position {
		return &Position{Symbol: sym, Quantity: qty, AvgCost: avg, Side: side, Multiplier: 1, Leverage: 1}
	}
	certified := map[string]string{"trending_up": DirectionLong, "trending_down": DirectionShort, "ranging": DirectionLong}
	stamped := func(regime string) *Position {
		p := perpsPos("HYPE", 1, 0, "short")
		p.Regime = regime
		p.DirectionCertifiedAtOpen = true
		p.DirectionCertifiedStatesAtOpen = certified
		return p
	}
	cases := []struct {
		name           string
		strategies     map[string]*StrategyState
		cfg            []StrategyConfig
		wantCount      int
		wantJoined     []string
		wantPerWarning [][]string
	}{
		{
			name: "long and short direction conflicts",
			strategies: map[string]*StrategyState{
				"hl-triple-ema-eth": {ID: "hl-triple-ema-eth", Type: "perps", Positions: map[string]*Position{"ETH": perpsPos("ETH", 0.5, 2000, "short")}},
				"hl-bidir-btc":      {ID: "hl-bidir-btc", Type: "perps", Positions: map[string]*Position{"BTC": perpsPos("BTC", 0.1, 60000, "short")}},
				"hl-bear-sol":       {ID: "hl-bear-sol", Type: "perps", Positions: map[string]*Position{"SOL": perpsPos("SOL", 1.0, 200, "long")}},
				"bn-sma-btc":        {ID: "bn-sma-btc", Type: "spot", Positions: map[string]*Position{"BTC/USDT": {Symbol: "BTC/USDT", Quantity: 0.01, AvgCost: 60000, Side: "long"}}},
			},
			cfg: []StrategyConfig{
				{ID: "hl-triple-ema-eth", Type: "perps", Platform: "hyperliquid", Direction: DirectionLong},
				{ID: "hl-bidir-btc", Type: "perps", Platform: "hyperliquid", Direction: DirectionBoth},
				{ID: "hl-bear-sol", Type: "perps", Platform: "hyperliquid", Direction: DirectionShort},
				{ID: "bn-sma-btc", Type: "spot", Platform: "binanceus"},
			},
			wantCount:  2,
			wantJoined: []string{"hl-triple-ema-eth", "hl-bear-sol", "direction="},
		},
		{
			name: "no conflicts when sides match direction",
			strategies: map[string]*StrategyState{
				"hl-triple-ema-eth": {ID: "hl-triple-ema-eth", Type: "perps", Positions: map[string]*Position{"ETH": perpsPos("ETH", 0.5, 2000, "long")}},
				"hl-bear-sol":       {ID: "hl-bear-sol", Type: "perps", Positions: map[string]*Position{"SOL": perpsPos("SOL", 1.0, 200, "short")}},
			},
			cfg: []StrategyConfig{
				{ID: "hl-triple-ema-eth", Type: "perps", Platform: "hyperliquid", Direction: DirectionLong},
				{ID: "hl-bear-sol", Type: "perps", Platform: "hyperliquid", Direction: DirectionShort},
			},
			wantCount: 0,
		},
		{
			name: "orphan state without config produces no warning",
			strategies: map[string]*StrategyState{
				"gone": {ID: "gone", Type: "perps", Positions: map[string]*Position{"ETH": perpsPos("ETH", 0.5, 0, "short")}},
			},
			cfg:       []StrategyConfig{},
			wantCount: 0,
		},
		{
			name: "legacy allow_shorts=false maps to direction=long",
			strategies: map[string]*StrategyState{
				"hl-legacy-eth": {ID: "hl-legacy-eth", Type: "perps", Positions: map[string]*Position{"ETH": perpsPos("ETH", 0.5, 2000, "short")}},
			},
			cfg:       []StrategyConfig{{ID: "hl-legacy-eth", Type: "perps", Platform: "hyperliquid", AllowShorts: false}},
			wantCount: 1,
		},
		{
			name: "regime policy stamped trending_down short does not warn",
			strategies: map[string]*StrategyState{
				"hl-mr-hype": {ID: "hl-mr-hype", Type: "perps", Positions: map[string]*Position{"HYPE": stamped("trending_down")}},
			},
			cfg: []StrategyConfig{{
				ID: "hl-mr-hype", Type: "perps", Platform: "hyperliquid", Direction: DirectionLong,
				RegimeDirectionalPolicy: makeRegimeDirectionalPolicyForValidation(),
			}},
			wantCount: 0,
		},
		{
			name: "regime policy stamped trending_up short conflicts",
			strategies: map[string]*StrategyState{
				"hl-mr-hype": {ID: "hl-mr-hype", Type: "perps", Positions: map[string]*Position{"HYPE": stamped("trending_up")}},
			},
			cfg: []StrategyConfig{{
				ID: "hl-mr-hype", Type: "perps", Platform: "hyperliquid", Direction: DirectionLong,
				RegimeDirectionalPolicy: makeRegimeDirectionalPolicyForValidation(),
			}},
			wantCount:      1,
			wantPerWarning: [][]string{{"stamped regime=\"trending_up\""}},
		},
		{
			name: "regime policy uncertified legacy short warns for #1085 migration",
			strategies: map[string]*StrategyState{
				"hl-mr-hype": {ID: "hl-mr-hype", Type: "perps", Positions: map[string]*Position{"HYPE": perpsPos("HYPE", 1, 0, "short")}},
			},
			cfg: []StrategyConfig{{
				ID: "hl-mr-hype", Type: "perps", Platform: "hyperliquid", Direction: DirectionLong,
				RegimeDirectionalPolicy: makeRegimeDirectionalPolicyForValidation(),
			}},
			wantCount:      1,
			wantPerWarning: [][]string{{"DEFAULT-OFF", "#1085"}},
		},
		{
			name: "warning order sorted by symbol",
			strategies: map[string]*StrategyState{
				"hl-multi": {ID: "hl-multi", Type: "perps", Positions: map[string]*Position{
					"ETH": perpsPos("ETH", 1, 0, "short"),
					"BTC": perpsPos("BTC", 0.1, 0, "short"),
				}},
			},
			cfg:            []StrategyConfig{{ID: "hl-multi", Type: "perps", Platform: "hyperliquid", Direction: DirectionLong}},
			wantCount:      2,
			wantPerWarning: [][]string{{" BTC "}, {" ETH "}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state := NewAppState()
			for id, s := range tc.strategies {
				state.Strategies[id] = s
			}
			warnings := ValidatePerpsDirectionConfig(state, &Config{Strategies: tc.cfg})
			if len(warnings) != tc.wantCount {
				t.Fatalf("want %d warnings, got %d: %v", tc.wantCount, len(warnings), warnings)
			}
			joined := strings.Join(warnings, "\n")
			for _, want := range tc.wantJoined {
				if !strings.Contains(joined, want) {
					t.Errorf("warnings should contain %q, got: %s", want, joined)
				}
			}
			for i, wants := range tc.wantPerWarning {
				for _, want := range wants {
					if !strings.Contains(warnings[i], want) {
						t.Errorf("warnings[%d] should contain %q, got: %s", i, want, warnings[i])
					}
				}
			}
		})
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

func TestLoadStateWithDB_MigratesLegacyPerpsMultiplier(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")

	db, err := OpenStateDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	original := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-eth": {
				ID:             "hl-eth",
				Type:           "perps",
				Platform:       "hyperliquid",
				Cash:           500,
				InitialCapital: 500,
				Positions: map[string]*Position{
					"ETH": {Symbol: "ETH", Quantity: 0.25, AvgCost: 2200, Side: "long", Multiplier: 0},
				},
				OptionPositions: make(map[string]*OptionPosition),
				TradeHistory:    []Trade{},
			},
			"spot-btc": {
				ID:             "spot-btc",
				Type:           "spot",
				Platform:       "binanceus",
				Cash:           500,
				InitialCapital: 500,
				Positions: map[string]*Position{
					"BTC/USDT": {Symbol: "BTC/USDT", Quantity: 0.01, AvgCost: 50000, Side: "long", Multiplier: 0},
				},
				OptionPositions: make(map[string]*OptionPosition),
				TradeHistory:    []Trade{},
			},
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

	if got := loaded.Strategies["hl-eth"].Positions["ETH"].Multiplier; got != 1 {
		t.Errorf("perps multiplier = %g, want 1 after legacy migration", got)
	}
	if got := loaded.Strategies["spot-btc"].Positions["BTC/USDT"].Multiplier; got != 0 {
		t.Errorf("spot multiplier = %g, want 0 (spot position must not migrate)", got)
	}
}

func TestSaveStateWithDB(t *testing.T) {
	t.Run("persists cycle count", func(t *testing.T) {
		db, err := OpenStateDB(filepath.Join(t.TempDir(), "state.db"))
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
		if err := SaveStateWithDB(state, &Config{}, db); err != nil {
			t.Fatal(err)
		}
		dbState, err := db.LoadState()
		if err != nil {
			t.Fatal(err)
		}
		if dbState.CycleCount != 3 {
			t.Errorf("SQLite CycleCount = %d, want 3", dbState.CycleCount)
		}
	})
	t.Run("closed db returns error", func(t *testing.T) {
		db, err := OpenStateDB(filepath.Join(t.TempDir(), "state.db"))
		if err != nil {
			t.Fatal(err)
		}
		db.Close()

		state := &AppState{CycleCount: 5, Strategies: make(map[string]*StrategyState)}
		if err := SaveStateWithDB(state, &Config{}, db); err == nil {
			t.Error("expected error when SQLite is closed")
		}
	})
}

func TestReconcileConfigInitialCapital(t *testing.T) {
	t.Run("config change updates memory and db, untouched strategy stays", func(t *testing.T) {
		db, err := OpenStateDB(filepath.Join(t.TempDir(), "state.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		resetInitialCapitalGuardDedup(t)

		state := &AppState{
			Strategies: map[string]*StrategyState{
				"hl-tema-eth": {
					ID: "hl-tema-eth", Type: "perps", Platform: "hyperliquid",
					Cash: 505, InitialCapital: 505,
					Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{},
				},
				"silent": {
					ID: "silent", Type: "spot", Cash: 200, InitialCapital: 200,
					Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{},
				},
			},
		}
		if err := db.SaveState(state); err != nil {
			t.Fatal(err)
		}

		cfg := &Config{
			Strategies: []StrategyConfig{
				{ID: "hl-tema-eth", Type: "perps", Platform: "hyperliquid", Capital: 1000, InitialCapital: 1000},
				{ID: "silent", Type: "spot", Capital: 200},
			},
		}

		infos, errs := ReconcileConfigInitialCapital(cfg, state, openTestStore(t, db))
		if len(infos) != 1 {
			t.Fatalf("infos = %d, want 1 (only hl-tema-eth changed)", len(infos))
		}
		if len(errs) != 0 {
			t.Fatalf("errs = %v, want none", errs)
		}

		if got := state.Strategies["hl-tema-eth"].InitialCapital; got != 1000 {
			t.Errorf("in-memory InitialCapital = %g, want 1000", got)
		}
		if got := state.Strategies["silent"].InitialCapital; got != 200 {
			t.Errorf("untouched strategy InitialCapital = %g, want 200", got)
		}

		if err := db.SaveState(state); err != nil {
			t.Fatalf("SaveState after reconcile: %v", err)
		}
		loaded, err := db.LoadState()
		if err != nil {
			t.Fatal(err)
		}
		if got := loaded.Strategies["hl-tema-eth"].InitialCapital; got != 1000 {
			t.Errorf("persisted InitialCapital = %g, want 1000", got)
		}
	})
	t.Run("no-op when config matches db", func(t *testing.T) {
		db, err := OpenStateDB(filepath.Join(t.TempDir(), "state.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		resetInitialCapitalGuardDedup(t)

		state := &AppState{
			Strategies: map[string]*StrategyState{
				"s": {ID: "s", Type: "spot", Cash: 1000, InitialCapital: 1000,
					Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{}},
			},
		}
		if err := db.SaveState(state); err != nil {
			t.Fatal(err)
		}
		cfg := &Config{Strategies: []StrategyConfig{
			{ID: "s", Type: "spot", Capital: 1000, InitialCapital: 1000},
		}}
		if infos, errs := ReconcileConfigInitialCapital(cfg, state, openTestStore(t, db)); len(infos) != 0 || len(errs) != 0 {
			t.Errorf("infos=%v errs=%v, want none when config matches DB", infos, errs)
		}
	})
}
