package main

import (
	"strings"
	"testing"
)

// baseOpts returns an InitOptions suitable as a starting point for tests.
func baseOpts() InitOptions {
	return InitOptions{
		Assets:          []string{"BTC", "ETH"},
		EnableSpot:      true,
		EnableOptions:   false,
		EnablePerps:     false,
		OptionPlatforms: []string{"deribit"},
		PerpsMode:       "paper",
		SpotStrategies:  []string{"momentum"},
		IncludePairs:    false,
		OptStrategies:   []string{},
		SpotCapital:     1000,
		OptionsCapital:  5000,
		PerpsCapital:    1000,
		SpotDrawdown:    5,
		OptionsDrawdown: 10,
		PerpsDrawdown:   5,
	}
}

func TestGenerateConfig_AllTypes(t *testing.T) {
	opts := InitOptions{
		Assets:          []string{"BTC", "ETH", "SOL"},
		EnableSpot:      true,
		EnableOptions:   true,
		EnablePerps:     true,
		OptionPlatforms: []string{"deribit"},
		PerpsMode:       "paper",
		SpotStrategies:  []string{"momentum"},
		IncludePairs:    true,
		OptStrategies:   []string{"vol_mean_reversion"},
		SpotCapital:     1000,
		OptionsCapital:  5000,
		PerpsCapital:    1000,
		SpotDrawdown:    5,
		OptionsDrawdown: 10,
		PerpsDrawdown:   5,
	}
	cfg := generateConfig(opts)

	// momentum × 3 assets = 3 spot
	// pairs: (BTC,ETH),(BTC,SOL),(ETH,SOL) = 3 pairs
	// options deribit × vol × (BTC,ETH) = 2  (SOL skipped)
	// perps momentum × 3 assets = 3
	// total = 11
	if len(cfg.Strategies) != 11 {
		t.Errorf("expected 11 strategies, got %d", len(cfg.Strategies))
		for _, s := range cfg.Strategies {
			t.Logf("  %s (%s)", s.ID, s.Type)
		}
	}
}

func TestGenerateConfig_SingleAsset_NoPairs(t *testing.T) {
	opts := baseOpts()
	opts.Assets = []string{"BTC"}
	opts.IncludePairs = true // should be ignored: < 2 assets

	cfg := generateConfig(opts)

	// momentum × BTC = 1, no pairs
	if len(cfg.Strategies) != 1 {
		t.Errorf("expected 1 strategy for single asset, got %d", len(cfg.Strategies))
	}
	if cfg.Strategies[0].ID != "momentum-btc" {
		t.Errorf("expected id momentum-btc, got %s", cfg.Strategies[0].ID)
	}
}

func TestGenerateConfig_SpotOnly(t *testing.T) {
	opts := baseOpts()
	cfg := generateConfig(opts)

	for _, s := range cfg.Strategies {
		if s.Type != "spot" {
			t.Errorf("expected only spot strategies, got %s (%s)", s.ID, s.Type)
		}
	}
}

func TestGenerateConfig_OptionsSinglePlatformDeribit(t *testing.T) {
	opts := baseOpts()
	opts.EnableSpot = false
	opts.EnableOptions = true
	opts.OptStrategies = []string{"vol_mean_reversion"}
	opts.OptionPlatforms = []string{"deribit"}

	cfg := generateConfig(opts)

	// deribit × vol × (BTC, ETH) = 2
	if len(cfg.Strategies) != 2 {
		t.Errorf("expected 2 options strategies, got %d", len(cfg.Strategies))
	}
	for _, s := range cfg.Strategies {
		if s.Type != "options" {
			t.Errorf("expected options type, got %s", s.Type)
		}
		if s.Script != "shared_scripts/check_options.py" {
			t.Errorf("expected check_options.py script, got %s", s.Script)
		}
		if !strings.HasPrefix(s.ID, "deribit-") {
			t.Errorf("expected deribit- prefix, got %s", s.ID)
		}
	}
}

func TestGenerateConfig_OptionsBothPlatforms(t *testing.T) {
	opts := baseOpts()
	opts.EnableSpot = false
	opts.EnableOptions = true
	opts.OptStrategies = []string{"vol_mean_reversion"}
	opts.OptionPlatforms = []string{"deribit", "ibkr"}

	cfg := generateConfig(opts)

	// 2 platforms × vol × (BTC, ETH) = 4
	if len(cfg.Strategies) != 4 {
		t.Errorf("expected 4 options strategies (both platforms), got %d", len(cfg.Strategies))
	}
}

func TestGenerateConfig_PerpsLiveMode(t *testing.T) {
	opts := baseOpts()
	opts.EnableSpot = false
	opts.EnablePerps = true
	opts.PerpsMode = "live"

	cfg := generateConfig(opts)

	for _, s := range cfg.Strategies {
		if s.Type != "perps" {
			continue
		}
		found := false
		for _, arg := range s.Args {
			if arg == "--mode=live" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected --mode=live in args for %s, got %v", s.ID, s.Args)
		}
	}
}

func TestGenerateConfig_PerpsDefaultPaperMode(t *testing.T) {
	opts := baseOpts()
	opts.EnableSpot = false
	opts.EnablePerps = true
	opts.PerpsMode = "paper"

	cfg := generateConfig(opts)

	for _, s := range cfg.Strategies {
		if s.Type != "perps" {
			continue
		}
		found := false
		for _, arg := range s.Args {
			if arg == "--mode=paper" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected --mode=paper in args for %s, got %v", s.ID, s.Args)
		}
	}
}

func TestGenerateConfig_ThreeAssets_ThreePairs(t *testing.T) {
	opts := baseOpts()
	opts.Assets = []string{"BTC", "ETH", "SOL"}
	opts.SpotStrategies = []string{} // no regular spot
	opts.IncludePairs = true

	cfg := generateConfig(opts)

	// pairs: (BTC,ETH),(BTC,SOL),(ETH,SOL) = 3
	if len(cfg.Strategies) != 3 {
		t.Errorf("expected 3 pairs for 3 assets, got %d", len(cfg.Strategies))
	}
	for _, s := range cfg.Strategies {
		if s.IntervalSeconds != 86400 {
			t.Errorf("expected pairs interval 86400, got %d for %s", s.IntervalSeconds, s.ID)
		}
	}
}

func TestGenerateConfig_TwoAssets_OnePair(t *testing.T) {
	opts := baseOpts()
	opts.SpotStrategies = []string{}
	opts.IncludePairs = true

	cfg := generateConfig(opts)

	// pairs: (BTC,ETH) = 1
	if len(cfg.Strategies) != 1 {
		t.Errorf("expected 1 pair for 2 assets, got %d", len(cfg.Strategies))
	}
	if cfg.Strategies[0].ID != "pairs-btc-eth" {
		t.Errorf("expected pairs-btc-eth, got %s", cfg.Strategies[0].ID)
	}
	if cfg.Strategies[0].Args[0] != "pairs_spread" {
		t.Errorf("expected pairs_spread arg, got %s", cfg.Strategies[0].Args[0])
	}
}

func TestGenerateConfig_CustomCapital(t *testing.T) {
	opts := baseOpts()
	opts.SpotCapital = 2500
	opts.SpotDrawdown = 15

	cfg := generateConfig(opts)

	for _, s := range cfg.Strategies {
		if s.Capital != 2500 {
			t.Errorf("expected capital=2500 for %s, got %.0f", s.ID, s.Capital)
		}
		if s.MaxDrawdownPct != 15 {
			t.Errorf("expected max_drawdown_pct=15 for %s, got %.0f", s.ID, s.MaxDrawdownPct)
		}
	}
}

func TestGenerateConfig_IDFormat(t *testing.T) {
	opts := baseOpts()
	opts.Assets = []string{"BTC"}

	cfg := generateConfig(opts)

	if cfg.Strategies[0].ID != "momentum-btc" {
		t.Errorf("expected momentum-btc, got %s", cfg.Strategies[0].ID)
	}
}

func TestGenerateConfig_SpotScriptAndArgs(t *testing.T) {
	opts := baseOpts()
	opts.Assets = []string{"BTC"}

	cfg := generateConfig(opts)

	s := cfg.Strategies[0]
	if s.Script != "shared_scripts/check_strategy.py" {
		t.Errorf("expected check_strategy.py, got %s", s.Script)
	}
	if len(s.Args) != 3 || s.Args[0] != "momentum" || s.Args[1] != "BTC/USDT" || s.Args[2] != "1h" {
		t.Errorf("unexpected spot args: %v", s.Args)
	}
}

func TestGenerateConfig_HyperliquidPlatformAdded(t *testing.T) {
	opts := baseOpts()
	opts.EnablePerps = true

	cfg := generateConfig(opts)

	if _, ok := cfg.Platforms["hyperliquid"]; !ok {
		t.Error("expected hyperliquid platform config when perps enabled")
	}
}

func TestGenerateConfig_NoHyperliquidWithoutPerps(t *testing.T) {
	opts := baseOpts()
	opts.EnablePerps = false

	cfg := generateConfig(opts)

	if _, ok := cfg.Platforms["hyperliquid"]; ok {
		t.Error("expected no hyperliquid platform config when perps disabled")
	}
}

func TestGenerateConfig_SOLSkippedForOptions(t *testing.T) {
	opts := baseOpts()
	opts.Assets = []string{"BTC", "ETH", "SOL"}
	opts.EnableSpot = false
	opts.EnableOptions = true
	opts.OptStrategies = []string{"vol_mean_reversion"}
	opts.OptionPlatforms = []string{"deribit"}

	cfg := generateConfig(opts)

	// Only BTC and ETH — SOL skipped
	if len(cfg.Strategies) != 2 {
		t.Errorf("expected 2 options strategies (SOL skipped), got %d", len(cfg.Strategies))
	}
	for _, s := range cfg.Strategies {
		if strings.Contains(s.ID, "sol") {
			t.Errorf("SOL should be skipped for options, got %s", s.ID)
		}
		for _, arg := range s.Args {
			if arg == "SOL" {
				t.Errorf("SOL should not appear in options args: %v", s.Args)
			}
		}
	}
}

func TestGenerateConfig_OptionsThetaHarvest(t *testing.T) {
	opts := baseOpts()
	opts.EnableSpot = false
	opts.EnableOptions = true
	opts.OptStrategies = []string{"vol_mean_reversion"}

	cfg := generateConfig(opts)

	for _, s := range cfg.Strategies {
		if s.Type != "options" {
			continue
		}
		if s.ThetaHarvest == nil {
			t.Errorf("expected ThetaHarvest to be set for %s", s.ID)
			continue
		}
		if !s.ThetaHarvest.Enabled {
			t.Errorf("expected ThetaHarvest.Enabled=true for %s", s.ID)
		}
		if s.ThetaHarvest.ProfitTargetPct != 60 {
			t.Errorf("expected ProfitTargetPct=60 for %s, got %.0f", s.ID, s.ThetaHarvest.ProfitTargetPct)
		}
	}
}

func TestGenerateConfig_PerpsScriptAndArgs(t *testing.T) {
	opts := baseOpts()
	opts.EnableSpot = false
	opts.EnablePerps = true
	opts.Assets = []string{"BTC"}
	opts.PerpsMode = "paper"

	cfg := generateConfig(opts)

	if len(cfg.Strategies) != 1 {
		t.Fatalf("expected 1 perps strategy, got %d", len(cfg.Strategies))
	}
	s := cfg.Strategies[0]
	if s.ID != "hl-momentum-btc" {
		t.Errorf("expected hl-momentum-btc, got %s", s.ID)
	}
	if s.Script != "shared_scripts/check_hyperliquid.py" {
		t.Errorf("expected check_hyperliquid.py, got %s", s.Script)
	}
	if len(s.Args) != 4 || s.Args[0] != "momentum" || s.Args[1] != "BTC" || s.Args[2] != "1h" || s.Args[3] != "--mode=paper" {
		t.Errorf("unexpected perps args: %v", s.Args)
	}
}

func TestGenerateConfig_IntervalDefaults(t *testing.T) {
	opts := InitOptions{
		Assets:          []string{"BTC", "ETH"},
		EnableSpot:      true,
		EnableOptions:   true,
		EnablePerps:     true,
		OptionPlatforms: []string{"deribit"},
		PerpsMode:       "paper",
		SpotStrategies:  []string{"momentum"},
		IncludePairs:    true,
		OptStrategies:   []string{"vol_mean_reversion"},
		SpotCapital:     1000,
		OptionsCapital:  5000,
		PerpsCapital:    1000,
		SpotDrawdown:    5,
		OptionsDrawdown: 10,
		PerpsDrawdown:   5,
	}
	cfg := generateConfig(opts)

	for _, s := range cfg.Strategies {
		switch s.Type {
		case "spot":
			if strings.HasPrefix(s.ID, "pairs-") {
				if s.IntervalSeconds != 86400 {
					t.Errorf("expected pairs interval 86400, got %d for %s", s.IntervalSeconds, s.ID)
				}
			} else {
				if s.IntervalSeconds != 3600 {
					t.Errorf("expected spot interval 3600, got %d for %s", s.IntervalSeconds, s.ID)
				}
			}
		case "options":
			if s.IntervalSeconds != 14400 {
				t.Errorf("expected options interval 14400, got %d for %s", s.IntervalSeconds, s.ID)
			}
		case "perps":
			if s.IntervalSeconds != 3600 {
				t.Errorf("expected perps interval 3600, got %d for %s", s.IntervalSeconds, s.ID)
			}
		}
	}
}

func TestGenerateConfig_PortfolioRiskDefaults(t *testing.T) {
	cfg := generateConfig(baseOpts())

	if cfg.PortfolioRisk == nil {
		t.Fatal("expected PortfolioRisk to be set")
	}
	if cfg.PortfolioRisk.MaxDrawdownPct != 25 {
		t.Errorf("expected MaxDrawdownPct=25, got %.0f", cfg.PortfolioRisk.MaxDrawdownPct)
	}
}

func TestGenerateConfig_DiscordEnabled(t *testing.T) {
	opts := baseOpts()
	opts.DiscordEnabled = true
	opts.SpotChannelID = "111222333"
	opts.OptionsChannelID = "444555666"

	cfg := generateConfig(opts)

	if !cfg.Discord.Enabled {
		t.Error("expected Discord.Enabled=true")
	}
	if cfg.Discord.Channels.Spot != "111222333" {
		t.Errorf("expected spot channel 111222333, got %s", cfg.Discord.Channels.Spot)
	}
	if cfg.Discord.Channels.Options != "444555666" {
		t.Errorf("expected options channel 444555666, got %s", cfg.Discord.Channels.Options)
	}
}

func TestMakePairs(t *testing.T) {
	pairs := makePairs([]string{"BTC", "ETH", "SOL"})
	if len(pairs) != 3 {
		t.Errorf("expected 3 pairs, got %d", len(pairs))
	}
	// Verify ordering: (BTC,ETH), (BTC,SOL), (ETH,SOL)
	expected := [][2]string{{"BTC", "ETH"}, {"BTC", "SOL"}, {"ETH", "SOL"}}
	for i, pair := range pairs {
		if pair != expected[i] {
			t.Errorf("pair[%d]: expected %v, got %v", i, expected[i], pair)
		}
	}
}

func TestMakePairs_TwoAssets(t *testing.T) {
	pairs := makePairs([]string{"BTC", "ETH"})
	if len(pairs) != 1 {
		t.Errorf("expected 1 pair, got %d", len(pairs))
	}
}

func TestStratShortName(t *testing.T) {
	if got := stratShortName(spotStrategies, "momentum"); got != "momentum" {
		t.Errorf("expected momentum, got %s", got)
	}
	if got := stratShortName(spotStrategies, "sma_crossover"); got != "sma" {
		t.Errorf("expected sma, got %s", got)
	}
	if got := stratShortName(optionsStrategies, "vol_mean_reversion"); got != "vol" {
		t.Errorf("expected vol, got %s", got)
	}
	// Unknown strategy falls back to the ID itself.
	if got := stratShortName(spotStrategies, "unknown_strat"); got != "unknown_strat" {
		t.Errorf("expected unknown_strat, got %s", got)
	}
}
