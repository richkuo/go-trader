package main

import (
	"path/filepath"
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
