package main

import (
	"strings"
	"testing"
)

// hlHedgeTestStrategy returns a minimal valid HL perps strategy config on the
// ETH coin, with the given hedge block attached.
func hlHedgeTestStrategy(id string, hedge *HedgeConfig) StrategyConfig {
	return StrategyConfig{
		ID:       id,
		Type:     "perps",
		Platform: "hyperliquid",
		Script:   "shared_scripts/check_hyperliquid.py",
		Args:     []string{"sma_crossover", "ETH", "1h"},
		Capital:  1000,
		Hedge:    hedge,
	}
}

func TestHedgeAccessors_Defaults(t *testing.T) {
	sc := hlHedgeTestStrategy("hl-eth", nil)
	if HedgeEnabled(sc) {
		t.Error("HedgeEnabled should be false with no hedge block")
	}
	if got := hedgeCoin(sc); got != "" {
		t.Errorf("hedgeCoin = %q, want empty without a block", got)
	}
	if got := HedgeRatio(sc); got != 1.0 {
		t.Errorf("HedgeRatio = %g, want 1.0 default without a block", got)
	}
	if got := hedgeLeverage(sc); got != 1.0 {
		t.Errorf("hedgeLeverage = %g, want 1.0 default without a block", got)
	}
	if got := hedgeMarginMode(sc); got != "isolated" {
		t.Errorf("hedgeMarginMode = %q, want isolated default", got)
	}

	sc.Hedge = &HedgeConfig{Enabled: true, Symbol: "BTC"}
	if !HedgeEnabled(sc) {
		t.Error("HedgeEnabled should be true with enabled block")
	}
	if got := HedgeRatio(sc); got != 1.0 {
		t.Errorf("HedgeRatio = %g, want 1.0 for zero value", got)
	}
	if got := hedgeLeverage(sc); got != 1.0 {
		t.Errorf("hedgeLeverage = %g, want 1.0 for zero value", got)
	}

	sc.Hedge.Ratio = 0.5
	sc.Hedge.Leverage = 3
	sc.Hedge.MarginMode = "cross"
	if got := HedgeRatio(sc); got != 0.5 {
		t.Errorf("HedgeRatio = %g, want 0.5", got)
	}
	if got := hedgeLeverage(sc); got != 3 {
		t.Errorf("hedgeLeverage = %g, want 3", got)
	}
	if got := hedgeMarginMode(sc); got != "cross" {
		t.Errorf("hedgeMarginMode = %q, want cross", got)
	}
}

func TestHedgeCoinNormalization(t *testing.T) {
	cases := []struct {
		symbol string
		want   string
	}{
		{"BTC", "BTC"},
		{"btc", "BTC"},
		{"  btc  ", "BTC"},
		{"BTC/USDC:USDC", "BTC"},
		{"btc/usdc:usdc", "BTC"},
		{"kPEPE", "KPEPE"},
		{"", ""},
		{"/USDC:USDC", ""},
	}
	for _, tc := range cases {
		sc := hlHedgeTestStrategy("hl-eth", &HedgeConfig{Enabled: true, Symbol: tc.symbol})
		if got := hedgeCoin(sc); got != tc.want {
			t.Errorf("hedgeCoin(%q) = %q, want %q", tc.symbol, got, tc.want)
		}
	}
}

func TestValidateHedgeConfigs_Valid(t *testing.T) {
	cfg := &Config{Strategies: []StrategyConfig{
		hlHedgeTestStrategy("hl-eth", &HedgeConfig{Enabled: true, Symbol: "BTC"}),
	}}
	if errs := validateHedgeConfigs(cfg); len(errs) != 0 {
		t.Fatalf("unexpected errors for minimal valid hedge: %v", errs)
	}

	cfg = &Config{Strategies: []StrategyConfig{
		hlHedgeTestStrategy("hl-eth", &HedgeConfig{
			Enabled: true, Symbol: "BTC/USDC:USDC", Side: "inverse",
			Ratio: 0.75, Platform: "hyperliquid", Type: "perps",
			MarginMode: "cross", Leverage: 3,
		}),
	}}
	if errs := validateHedgeConfigs(cfg); len(errs) != 0 {
		t.Fatalf("unexpected errors for fully-specified valid hedge: %v", errs)
	}

	// Disabled blocks skip symbol requirement and collision checks.
	cfg = &Config{Strategies: []StrategyConfig{
		hlHedgeTestStrategy("hl-eth", &HedgeConfig{Enabled: false}),
	}}
	if errs := validateHedgeConfigs(cfg); len(errs) != 0 {
		t.Fatalf("unexpected errors for disabled hedge block: %v", errs)
	}
}

func TestValidateHedgeConfigs_ShapeVocabulary(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(h *HedgeConfig)
		wantSub string
	}{
		{"bad side", func(h *HedgeConfig) { h.Side = "same" }, `hedge.side`},
		{"negative ratio", func(h *HedgeConfig) { h.Ratio = -0.5 }, `hedge.ratio`},
		{"ratio above cap", func(h *HedgeConfig) { h.Ratio = 10.01 }, `hedge.ratio`},
		{"negative leverage", func(h *HedgeConfig) { h.Leverage = -1 }, `hedge.leverage`},
		{"leverage above cap", func(h *HedgeConfig) { h.Leverage = 100.01 }, `hedge.leverage`},
		{"bad margin mode", func(h *HedgeConfig) { h.MarginMode = "portfolio" }, `hedge.margin_mode`},
		{"bad platform", func(h *HedgeConfig) { h.Platform = "binanceus" }, `hedge.platform`},
		{"bad type", func(h *HedgeConfig) { h.Type = "spot" }, `hedge.type`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &HedgeConfig{Enabled: true, Symbol: "BTC"}
			tc.mutate(h)
			cfg := &Config{Strategies: []StrategyConfig{hlHedgeTestStrategy("hl-eth", h)}}
			errs := validateHedgeConfigs(cfg)
			if len(errs) == 0 {
				t.Fatalf("want error containing %q, got none", tc.wantSub)
			}
			joined := strings.Join(errs, "\n")
			if !strings.Contains(joined, tc.wantSub) {
				t.Errorf("errors should mention %s, got: %s", tc.wantSub, joined)
			}
			if !strings.Contains(joined, "strategy[hl-eth]") {
				t.Errorf("errors should name the strategy, got: %s", joined)
			}
		})
	}

	// Shape typos reject even on a DISABLED block (a typo must not silently
	// default); only symbol/collision checks are skipped when disabled.
	h := &HedgeConfig{Enabled: false, Side: "inverse-ish"}
	cfg := &Config{Strategies: []StrategyConfig{hlHedgeTestStrategy("hl-eth", h)}}
	if errs := validateHedgeConfigs(cfg); len(errs) == 0 {
		t.Fatal("disabled block with a bad side should still reject")
	}
}

func TestValidateHedgeConfigs_ScopeRejects(t *testing.T) {
	hedge := &HedgeConfig{Enabled: true, Symbol: "BTC"}
	cases := []struct {
		name string
		sc   StrategyConfig
	}{
		{"spot", StrategyConfig{ID: "s-spot", Type: "spot", Platform: "binanceus", Hedge: hedge}},
		{"options", StrategyConfig{ID: "s-opt", Type: "options", Platform: "deribit", Hedge: hedge}},
		{"futures", StrategyConfig{ID: "s-fut", Type: "futures", Platform: "topstep", Hedge: hedge}},
		{"manual", StrategyConfig{ID: "s-man", Type: "manual", Platform: "hyperliquid", Symbol: "ETH", Hedge: hedge}},
		{"perps wrong platform", StrategyConfig{ID: "s-perps", Type: "perps", Platform: "okx", Hedge: hedge}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{Strategies: []StrategyConfig{tc.sc}}
			errs := validateHedgeConfigs(cfg)
			if len(errs) == 0 {
				t.Fatal("want scope rejection for non-HL-perps hedge")
			}
			if !strings.Contains(strings.Join(errs, "\n"), "only supported on platform=hyperliquid type=perps") {
				t.Errorf("unexpected error message: %v", errs)
			}
		})
	}
}

func TestValidateHedgeConfigs_SymbolRequired(t *testing.T) {
	for _, symbol := range []string{"", "   ", "/USDC:USDC"} {
		cfg := &Config{Strategies: []StrategyConfig{
			hlHedgeTestStrategy("hl-eth", &HedgeConfig{Enabled: true, Symbol: symbol}),
		}}
		errs := validateHedgeConfigs(cfg)
		if len(errs) == 0 {
			t.Errorf("symbol %q: want empty-symbol rejection", symbol)
			continue
		}
		if !strings.Contains(strings.Join(errs, "\n"), "hedge.symbol is required") {
			t.Errorf("symbol %q: unexpected message: %v", symbol, errs)
		}
	}
}

func TestValidateHedgeConfigs_OwnCoinRejected(t *testing.T) {
	for _, symbol := range []string{"ETH", "eth", "ETH/USDC:USDC"} {
		cfg := &Config{Strategies: []StrategyConfig{
			hlHedgeTestStrategy("hl-eth", &HedgeConfig{Enabled: true, Symbol: symbol}),
		}}
		errs := validateHedgeConfigs(cfg)
		if len(errs) == 0 {
			t.Errorf("symbol %q: want own-coin rejection", symbol)
			continue
		}
		if !strings.Contains(strings.Join(errs, "\n"), "own coin") {
			t.Errorf("symbol %q: unexpected message: %v", symbol, errs)
		}
	}
}

func TestValidateHedgeConfigs_PeerCoinCollisions(t *testing.T) {
	hedger := func() StrategyConfig {
		return hlHedgeTestStrategy("hl-eth", &HedgeConfig{Enabled: true, Symbol: "BTC"})
	}
	cases := []struct {
		name  string
		peers []StrategyConfig
	}{
		{"live perps peer", []StrategyConfig{
			{ID: "hl-btc", Type: "perps", Platform: "hyperliquid", Args: []string{"trend", "BTC", "1h"}},
		}},
		{"paper perps peer", []StrategyConfig{
			{ID: "hl-btc-paper", Type: "perps", Platform: "hyperliquid", Args: []string{"trend", "BTC", "1h", "--mode=paper"}},
		}},
		{"manual peer", []StrategyConfig{
			{ID: "hl-manual-btc", Type: "manual", Platform: "hyperliquid", Symbol: "BTC"},
		}},
		{"lowercase manual peer", []StrategyConfig{
			{ID: "hl-manual-btc", Type: "manual", Platform: "hyperliquid", Symbol: "btc"},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{Strategies: append([]StrategyConfig{hedger()}, tc.peers...)}
			errs := validateHedgeConfigs(cfg)
			if len(errs) == 0 {
				t.Fatal("want peer-coin collision rejection")
			}
			joined := strings.Join(errs, "\n")
			if !strings.Contains(joined, "collides with strategy[") {
				t.Errorf("unexpected message: %s", joined)
			}
			if !strings.Contains(joined, "strategy[hl-eth]") {
				t.Errorf("error should name the hedging strategy: %s", joined)
			}
		})
	}

	// Non-HL strategies on other platforms never collide (no shared wallet).
	cfg := &Config{Strategies: []StrategyConfig{
		hedger(),
		{ID: "bn-btc", Type: "spot", Platform: "binanceus", Args: []string{"sma", "BTC/USDT", "1h"}},
	}}
	if errs := validateHedgeConfigs(cfg); len(errs) != 0 {
		t.Fatalf("non-HL strategy must not collide, got: %v", errs)
	}
}

func TestValidateHedgeConfigs_TwoHedgesShareCoin(t *testing.T) {
	a := hlHedgeTestStrategy("hl-eth", &HedgeConfig{Enabled: true, Symbol: "BTC"})
	b := hlHedgeTestStrategy("hl-sol", &HedgeConfig{Enabled: true, Symbol: "btc"})
	b.Args = []string{"sma_crossover", "SOL", "1h"}
	cfg := &Config{Strategies: []StrategyConfig{a, b}}
	errs := validateHedgeConfigs(cfg)
	if len(errs) != 1 {
		t.Fatalf("want exactly one shared-hedge-coin error, got: %v", errs)
	}
	if !strings.Contains(errs[0], "strategy[hl-sol]") || !strings.Contains(errs[0], "strategy[hl-eth]") {
		t.Errorf("error should name both strategies, got: %s", errs[0])
	}
	if !strings.Contains(errs[0], "BTC") {
		t.Errorf("error should name the normalized coin, got: %s", errs[0])
	}
}

func TestValidateHedgeConfigs_DirectionBothRejected(t *testing.T) {
	// Explicit direction="both".
	sc := hlHedgeTestStrategy("hl-eth", &HedgeConfig{Enabled: true, Symbol: "BTC"})
	sc.Direction = DirectionBoth
	cfg := &Config{Strategies: []StrategyConfig{sc}}
	errs := validateHedgeConfigs(cfg)
	if len(errs) == 0 || !strings.Contains(strings.Join(errs, "\n"), `direction="both"`) {
		t.Fatalf("want direction=both rejection, got: %v", errs)
	}

	// Legacy allow_shorts=true resolves to "both" via EffectiveDirection —
	// same rejection (flips change the hedge side mid-flight either way).
	sc = hlHedgeTestStrategy("hl-eth", &HedgeConfig{Enabled: true, Symbol: "BTC"})
	sc.AllowShorts = true
	cfg = &Config{Strategies: []StrategyConfig{sc}}
	if errs := validateHedgeConfigs(cfg); len(errs) == 0 {
		t.Fatal("want rejection for legacy allow_shorts=true + hedge")
	}

	// direction="short" and direction="long" are fine.
	for _, dir := range []string{DirectionLong, DirectionShort} {
		sc = hlHedgeTestStrategy("hl-eth", &HedgeConfig{Enabled: true, Symbol: "BTC"})
		sc.Direction = dir
		cfg = &Config{Strategies: []StrategyConfig{sc}}
		if errs := validateHedgeConfigs(cfg); len(errs) != 0 {
			t.Errorf("direction=%q should be allowed with hedge, got: %v", dir, errs)
		}
	}
}

func TestValidateHedgeConfigs_NilConfigAndNoBlocks(t *testing.T) {
	if errs := validateHedgeConfigs(nil); len(errs) != 0 {
		t.Errorf("nil config should produce no errors, got: %v", errs)
	}
	cfg := &Config{Strategies: []StrategyConfig{
		hlHedgeTestStrategy("hl-eth", nil),
		{ID: "hl-btc", Type: "perps", Platform: "hyperliquid", Args: []string{"trend", "BTC", "1h"}},
	}}
	if errs := validateHedgeConfigs(cfg); len(errs) != 0 {
		t.Errorf("no hedge blocks should produce no errors, got: %v", errs)
	}
}

func TestLoadConfig_HedgeBlockParses(t *testing.T) {
	dir := t.TempDir()
	cfgJSON := `{
		"strategies": [{
			"id": "hl-eth",
			"type": "perps",
			"platform": "hyperliquid",
			"script": "shared_scripts/check_hyperliquid.py",
			"args": ["sma_crossover", "ETH", "1h", "--mode=paper"],
			"capital": 1000,
			"max_drawdown_pct": 10,
			"hedge": {
				"enabled": true,
				"symbol": "BTC/USDC:USDC",
				"ratio": 0.5,
				"margin_mode": "cross",
				"leverage": 2
			}
		}]
	}`
	path := writeTestConfig(t, dir, cfgJSON)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	sc := cfg.Strategies[0]
	if !HedgeEnabled(sc) {
		t.Fatal("hedge block should parse as enabled")
	}
	if got := hedgeCoin(sc); got != "BTC" {
		t.Errorf("hedgeCoin = %q, want BTC", got)
	}
	if got := HedgeRatio(sc); got != 0.5 {
		t.Errorf("HedgeRatio = %g, want 0.5", got)
	}
	if got := hedgeMarginMode(sc); got != "cross" {
		t.Errorf("hedgeMarginMode = %q, want cross", got)
	}
	if got := hedgeLeverage(sc); got != 2 {
		t.Errorf("hedgeLeverage = %g, want 2", got)
	}
}

func TestLoadConfig_HedgeUnknownKeyRejected(t *testing.T) {
	dir := t.TempDir()
	cfgJSON := `{
		"strategies": [{
			"id": "hl-eth",
			"type": "perps",
			"platform": "hyperliquid",
			"script": "shared_scripts/check_hyperliquid.py",
			"args": ["sma_crossover", "ETH", "1h", "--mode=paper"],
			"capital": 1000,
			"max_drawdown_pct": 10,
			"hedge": {
				"enabled": true,
				"symbol": "BTC",
				"ration": 2
			}
		}]
	}`
	path := writeTestConfig(t, dir, cfgJSON)
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("typo'd hedge key \"ration\" must reject, not silently default")
	}
	if !strings.Contains(err.Error(), `strategy[hl-eth].hedge: unknown field "ration"`) {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestLoadConfig_HedgeCollisionRejectedAtLoad(t *testing.T) {
	dir := t.TempDir()
	cfgJSON := `{
		"strategies": [
			{
				"id": "hl-eth",
				"type": "perps",
				"platform": "hyperliquid",
				"script": "shared_scripts/check_hyperliquid.py",
				"args": ["sma_crossover", "ETH", "1h", "--mode=paper"],
				"capital": 1000,
				"max_drawdown_pct": 10,
				"hedge": {"enabled": true, "symbol": "BTC"}
			},
			{
				"id": "hl-btc",
				"type": "perps",
				"platform": "hyperliquid",
				"script": "shared_scripts/check_hyperliquid.py",
				"args": ["sma_crossover", "BTC", "1h", "--mode=paper"],
				"capital": 1000,
				"max_drawdown_pct": 10
			}
		]
	}`
	path := writeTestConfig(t, dir, cfgJSON)
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("hedge coin colliding with a configured strategy coin must reject at load")
	}
	if !strings.Contains(err.Error(), "collides with strategy[hl-btc]") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestHotReload_HedgeFlatOnly pins the phase-I stance (issue constraint 7):
// the hedge block is hot-reloadable when the strategy is fully flat, and
// blocked while ANY leg is open — including a residual hedge leg held with
// the primary flat. Supersedes the phase-A conservative pin
// (restart-required for every hedge edit).
func TestHotReload_HedgeFlatOnly(t *testing.T) {
	plain := hlHedgeTestStrategy("hl-eth", nil)
	hedged := hlHedgeTestStrategy("hl-eth", &HedgeConfig{Enabled: true, Symbol: "BTC"})
	hedged2x := hlHedgeTestStrategy("hl-eth", &HedgeConfig{Enabled: true, Symbol: "BTC", Ratio: 2})
	flatState := NewAppState()

	t.Run("shape mask: hedge edits are not restart-required", func(t *testing.T) {
		// nil -> block.
		cur := &Config{Strategies: []StrategyConfig{plain}}
		next := &Config{Strategies: []StrategyConfig{hedged}}
		if err := validateHotReloadCompatible(cur, next); err != nil {
			t.Errorf("adding a hedge block should not require restart, got: %v", err)
		}
		// block -> changed params.
		cur = &Config{Strategies: []StrategyConfig{hedged}}
		next = &Config{Strategies: []StrategyConfig{hedged2x}}
		if err := validateHotReloadCompatible(cur, next); err != nil {
			t.Errorf("hedge param edit should not require restart, got: %v", err)
		}
		// block -> nil (removal).
		next = &Config{Strategies: []StrategyConfig{plain}}
		if err := validateHotReloadCompatible(cur, next); err != nil {
			t.Errorf("removing a hedge block should not require restart, got: %v", err)
		}
	})

	t.Run("state-compat: blocked while the primary is open", func(t *testing.T) {
		cur := &Config{Strategies: []StrategyConfig{hedged}}
		next := &Config{Strategies: []StrategyConfig{hedged2x}}
		state := NewAppState()
		s := hedgeTestStrategyState()
		delete(s.Positions, "BTC") // primary open only
		state.Strategies["hl-eth"] = s
		err := validateHotReloadStateCompatible(cur, next, state)
		if err == nil || !strings.Contains(err.Error(), "hedge block changed with open positions") {
			t.Fatalf("hedge edit with an open primary must be blocked, got: %v", err)
		}
	})

	t.Run("state-compat: blocked with a residual hedge leg only", func(t *testing.T) {
		cur := &Config{Strategies: []StrategyConfig{hedged}}
		next := &Config{Strategies: []StrategyConfig{hedged2x}}
		state := NewAppState()
		s := hedgeTestStrategyState()
		delete(s.Positions, "ETH") // hedge leg open, primary flat
		state.Strategies["hl-eth"] = s
		err := validateHotReloadStateCompatible(cur, next, state)
		if err == nil || !strings.Contains(err.Error(), "hedge block changed with open positions") {
			t.Fatalf("hedge edit with a residual hedge leg must be blocked, got: %v", err)
		}
	})

	t.Run("state-compat: identical blocks pass while open", func(t *testing.T) {
		cur := &Config{Strategies: []StrategyConfig{hedged}}
		next := &Config{Strategies: []StrategyConfig{hedged}}
		state := NewAppState()
		state.Strategies["hl-eth"] = hedgeTestStrategyState()
		if err := validateHotReloadStateCompatible(cur, next, state); err != nil {
			t.Fatalf("identical hedge block should pass state-compat, got: %v", err)
		}
	})

	t.Run("apply: flat reload deep-copies the new block", func(t *testing.T) {
		cur := &Config{Strategies: []StrategyConfig{hedged}}
		next := &Config{Strategies: []StrategyConfig{hedged2x}}
		changes, err := applyHotReloadConfig(cur, next, flatState, nil, nil)
		if err != nil {
			t.Fatalf("applyHotReloadConfig: %v", err)
		}
		got := cur.Strategies[0].Hedge
		if got == nil || got.Ratio != 2 {
			t.Fatalf("hedge block not applied: %+v", got)
		}
		if got == next.Strategies[0].Hedge {
			t.Error("applied hedge block must be a deep copy, not an alias into next")
		}
		joined := strings.Join(changes, "\n")
		if !strings.Contains(joined, "strategy[hl-eth].hedge: shape updated") {
			t.Errorf("change log missing hedge line: %v", changes)
		}
	})

	t.Run("apply: flat reload can add and remove the block", func(t *testing.T) {
		cur := &Config{Strategies: []StrategyConfig{plain}}
		next := &Config{Strategies: []StrategyConfig{hedged}}
		if _, err := applyHotReloadConfig(cur, next, flatState, nil, nil); err != nil {
			t.Fatalf("add: %v", err)
		}
		if cur.Strategies[0].Hedge == nil || !HedgeEnabled(cur.Strategies[0]) {
			t.Fatalf("hedge block not added: %+v", cur.Strategies[0].Hedge)
		}
		// Removal.
		next2 := &Config{Strategies: []StrategyConfig{hlHedgeTestStrategy("hl-eth", nil)}}
		if _, err := applyHotReloadConfig(cur, next2, flatState, nil, nil); err != nil {
			t.Fatalf("remove: %v", err)
		}
		if cur.Strategies[0].Hedge != nil {
			t.Errorf("hedge block not removed: %+v", cur.Strategies[0].Hedge)
		}
	})

	t.Run("reload cannot introduce a hedge collision", func(t *testing.T) {
		ethPlain := hlHedgeTestStrategy("hl-eth", nil)
		btcPlain := hlHedgeTestStrategy("hl-btc", nil)
		btcPlain.Args = []string{"sma_crossover", "BTC", "1h"}
		cur := &Config{Strategies: []StrategyConfig{ethPlain, btcPlain}}

		ethHedged := hlHedgeTestStrategy("hl-eth", &HedgeConfig{Enabled: true, Symbol: "BTC"})
		next := &Config{Strategies: []StrategyConfig{ethHedged, btcPlain}}
		err := validateHotReloadCompatible(cur, next)
		if err == nil || !strings.Contains(err.Error(), "collides with strategy[hl-btc]") {
			t.Fatalf("reload introducing a hedge-coin collision must reject, got: %v", err)
		}
	})
}
