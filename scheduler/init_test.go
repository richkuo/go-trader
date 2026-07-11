package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// init sets module-level strategy lists to defaults so tests don't depend on Python.
func init() {
	spotStrategies = defaultSpotStrategies
	optionsStrategies = defaultOptionsStrategies
	perpsStrategies = defaultPerpsStrategies
	futuresStrategies = defaultFuturesStrategies
}

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
		Assets:            []string{"BTC", "ETH", "SOL"},
		EnableSpot:        true,
		EnableOptions:     true,
		EnablePerps:       true,
		EnableFutures:     true,
		OptionPlatforms:   []string{"deribit"},
		PerpsMode:         "paper",
		FuturesMode:       "paper",
		SpotStrategies:    []string{"momentum"},
		IncludePairs:      true,
		OptStrategies:     []string{"vol_mean_reversion"},
		PerpsStrategies:   []string{"momentum"},
		FuturesStrategies: []string{"momentum"},
		FuturesSymbols:    []string{"ES"},
		SpotCapital:       1000,
		OptionsCapital:    5000,
		PerpsCapital:      1000,
		FuturesCapital:    5000,
		SpotDrawdown:      5,
		OptionsDrawdown:   10,
		PerpsDrawdown:     5,
		FuturesDrawdown:   5,
	}
	cfg := generateConfig(opts)

	// momentum × 3 assets = 3 spot
	// pairs: (BTC,ETH),(BTC,SOL),(ETH,SOL) = 3 pairs
	// options deribit × vol × (BTC,ETH) = 2  (SOL skipped)
	// perps momentum × 3 assets = 3
	// futures momentum × 1 symbol = 1
	// total = 12
	if len(cfg.Strategies) != 12 {
		t.Errorf("expected 12 strategies, got %d", len(cfg.Strategies))
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
	opts.PerpsStrategies = []string{"momentum"}

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

// #486: HL perps strategies default to isolated margin mode in generateConfig.
func TestGenerateConfig_PerpsDefaultsToIsolatedMargin(t *testing.T) {
	opts := baseOpts()
	opts.EnableSpot = false
	opts.EnablePerps = true
	opts.PerpsStrategies = []string{"momentum"}

	cfg := generateConfig(opts)

	perpsCount := 0
	for _, s := range cfg.Strategies {
		if s.Type != "perps" {
			continue
		}
		perpsCount++
		if s.MarginMode != "isolated" {
			t.Errorf("strategy %s: MarginMode = %q, want %q", s.ID, s.MarginMode, "isolated")
		}
	}
	if perpsCount == 0 {
		t.Fatal("expected at least one perps strategy")
	}
}

func TestGenerateConfig_PerpsDefaultPaperMode(t *testing.T) {
	opts := baseOpts()
	opts.EnableSpot = false
	opts.EnablePerps = true
	opts.PerpsMode = "paper"
	opts.PerpsStrategies = []string{"momentum"}

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
	opts.PerpsStrategies = []string{"momentum"}

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
		PerpsStrategies: []string{"momentum"},
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
	if cfg.PortfolioRisk.WarnThresholdPct != 60 {
		t.Errorf("expected WarnThresholdPct=60, got %.0f", cfg.PortfolioRisk.WarnThresholdPct)
	}
}

// #85: live-setup risk prompts feed into PortfolioRisk.* via InitOptions.
func TestGenerateConfig_PortfolioRiskOverride(t *testing.T) {
	opts := baseOpts()
	opts.PortfolioMaxDrawdownPct = 15
	opts.PortfolioWarnThresholdPct = 70

	cfg := generateConfig(opts)

	if cfg.PortfolioRisk == nil {
		t.Fatal("expected PortfolioRisk to be set")
	}
	if cfg.PortfolioRisk.MaxDrawdownPct != 15 {
		t.Errorf("expected MaxDrawdownPct=15, got %.0f", cfg.PortfolioRisk.MaxDrawdownPct)
	}
	if cfg.PortfolioRisk.WarnThresholdPct != 70 {
		t.Errorf("expected WarnThresholdPct=70, got %.0f", cfg.PortfolioRisk.WarnThresholdPct)
	}
}

// Zero values must not overwrite the safe defaults — the interactive wizard
// only prompts when a live mode is enabled, so JSON configs that omit the
// fields should still produce a valid portfolio_risk block.
func TestGenerateConfig_PortfolioRiskZeroKeepsDefaults(t *testing.T) {
	opts := baseOpts()
	opts.PortfolioMaxDrawdownPct = 0
	opts.PortfolioWarnThresholdPct = 0

	cfg := generateConfig(opts)

	if cfg.PortfolioRisk.MaxDrawdownPct != 25 || cfg.PortfolioRisk.WarnThresholdPct != 60 {
		t.Errorf("expected defaults 25/60, got %.0f/%.0f",
			cfg.PortfolioRisk.MaxDrawdownPct, cfg.PortfolioRisk.WarnThresholdPct)
	}
}

// JSON-mode consumers (OpenClaw, scripted setup) pass risk values under the
// camelCase tags; verify the unmarshal path wires them into generateConfig.
func TestInitOptions_PortfolioRiskJSONTags(t *testing.T) {
	blob := `{"portfolioMaxDrawdownPct": 18, "portfolioWarnThresholdPct": 65}`
	var opts InitOptions
	if err := json.Unmarshal([]byte(blob), &opts); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if opts.PortfolioMaxDrawdownPct != 18 {
		t.Errorf("expected 18, got %.0f", opts.PortfolioMaxDrawdownPct)
	}
	if opts.PortfolioWarnThresholdPct != 65 {
		t.Errorf("expected 65, got %.0f", opts.PortfolioWarnThresholdPct)
	}
}

func TestGenerateConfig_DiscordEnabled(t *testing.T) {
	opts := baseOpts()
	opts.DiscordEnabled = true
	opts.ChannelMap = map[string]string{
		"spot":    "111222333",
		"options": "444555666",
	}

	cfg := generateConfig(opts)

	if !cfg.Discord.Enabled {
		t.Error("expected Discord.Enabled=true")
	}
	if cfg.Discord.Channels["spot"] != "111222333" {
		t.Errorf("expected spot channel 111222333, got %s", cfg.Discord.Channels["spot"])
	}
	if cfg.Discord.Channels["options"] != "444555666" {
		t.Errorf("expected options channel 444555666, got %s", cfg.Discord.Channels["options"])
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
	if got := stratShortName(spotStrategies, "chart_pattern"); got != "cpat" {
		t.Errorf("expected cpat, got %s", got)
	}
	// tema_cross joined the quarantine roster in #1282, so it also falls
	// back to the raw ID now.
	if got := stratShortName(spotStrategies, "tema_cross"); got != "tema_cross" {
		t.Errorf("expected tema_cross, got %s", got)
	}
	// Quarantined names (#1275) are pruned from the fallback lists, so an
	// explicit legacy config falls back to the raw ID.
	if got := stratShortName(spotStrategies, "sma_crossover"); got != "sma_crossover" {
		t.Errorf("expected sma_crossover, got %s", got)
	}
	if got := stratShortName(optionsStrategies, "vol_mean_reversion"); got != "vol" {
		t.Errorf("expected vol, got %s", got)
	}
	// Unknown strategy falls back to the ID itself.
	if got := stratShortName(spotStrategies, "unknown_strat"); got != "unknown_strat" {
		t.Errorf("expected unknown_strat, got %s", got)
	}
}

func TestRunInitFromJSON_Valid(t *testing.T) {
	out := filepath.Join(t.TempDir(), "config.json")
	jsonStr := `{"assets":["BTC"],"enableSpot":true,"spotStrategies":["sma_crossover"],"spotCapital":1000,"spotDrawdown":10}`
	code := runInitFromJSON(jsonStr, out)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("expected output file to exist: %v", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if len(cfg.Strategies) == 0 {
		t.Error("expected at least one strategy in generated config")
	}
}

func TestRunInitFromJSON_EmptyUsesStarterSpotDefaults(t *testing.T) {
	out := filepath.Join(t.TempDir(), "config.json")
	jsonStr := `{}`
	code := runInitFromJSON(jsonStr, out)
	if code != 0 {
		t.Fatalf("expected exit 0 for starter defaults, got %d", code)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("expected output file to exist: %v", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if len(cfg.Strategies) != 1 {
		t.Fatalf("expected 1 starter strategy, got %d", len(cfg.Strategies))
	}
	s := cfg.Strategies[0]
	if s.ID != "cpat-btc" {
		t.Errorf("expected starter ID cpat-btc, got %s", s.ID)
	}
	if s.Type != "spot" || s.Platform != "binanceus" {
		t.Errorf("expected starter spot strategy on binanceus, got %s/%s", s.Type, s.Platform)
	}
	if s.Capital != 1000 || s.MaxDrawdownPct != 5 {
		t.Errorf("expected starter capital/drawdown 1000/5, got %.0f/%.0f", s.Capital, s.MaxDrawdownPct)
	}
}

func TestRunInitFromJSON_AssetsOnlyDefaultsToStarterSpot(t *testing.T) {
	out := filepath.Join(t.TempDir(), "config.json")
	jsonStr := `{"assets":["BTC"]}`
	code := runInitFromJSON(jsonStr, out)
	if code != 0 {
		t.Fatalf("expected exit 0 for starter defaults, got %d", code)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("expected output file to exist: %v", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if len(cfg.Strategies) != 1 || cfg.Strategies[0].ID != "cpat-btc" {
		t.Fatalf("expected starter cpat-btc config, got %+v", cfg.Strategies)
	}
}

func TestRunInitFromJSON_SpotEnabledNoStrategiesUsesStarterStrategy(t *testing.T) {
	out := filepath.Join(t.TempDir(), "config.json")
	jsonStr := `{"assets":["BTC"],"enableSpot":true}`
	code := runInitFromJSON(jsonStr, out)
	if code != 0 {
		t.Fatalf("expected exit 0 for starter defaults, got %d", code)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("expected output file to exist: %v", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if len(cfg.Strategies) != 1 || cfg.Strategies[0].ID != "cpat-btc" {
		t.Fatalf("expected starter cpat-btc config, got %+v", cfg.Strategies)
	}
}

// When the user passes includePairs=true but no assets, the starter defaulter
// populates Assets with the single starter asset. pairs_spread needs ≥2 assets,
// so IncludePairs must be cleared rather than leaving an inert flag that would
// silently generate a 1-asset config with no pair strategies.
func TestApplyMinimalStarterDefaults_IncludePairsWithoutAssetsDrops(t *testing.T) {
	opts := InitOptions{IncludePairs: true}
	applyMinimalStarterDefaults(&opts)
	if opts.IncludePairs {
		t.Errorf("expected IncludePairs cleared when assets were defaulted, still true")
	}
	if len(opts.Assets) != 1 || opts.Assets[0] != starterAssetName {
		t.Errorf("expected Assets=[%s], got %v", starterAssetName, opts.Assets)
	}
	if len(opts.SpotStrategies) != 1 || opts.SpotStrategies[0] != starterSpotStrategyID {
		t.Errorf("expected SpotStrategies=[%s], got %v", starterSpotStrategyID, opts.SpotStrategies)
	}
}

// If the user explicitly passes assets=["BTC","ETH"] and includePairs=true,
// the defaulter must leave IncludePairs alone (pairs are valid with 2+ assets).
func TestApplyMinimalStarterDefaults_IncludePairsWithMultipleAssetsPreserved(t *testing.T) {
	opts := InitOptions{Assets: []string{"BTC", "ETH"}, IncludePairs: true}
	applyMinimalStarterDefaults(&opts)
	if !opts.IncludePairs {
		t.Errorf("expected IncludePairs preserved when caller supplied 2+ assets")
	}
}

// Guard against drift between the starter constants and the option lists the
// interactive wizard uses: if `starterAssetName` is ever removed from
// `supportedAssets` or `starterSpotStrategyID` disappears from the spot
// registry, `selectionDefaults` silently falls back to index 0 — a first-run
// user would end up with some other asset/strategy without warning. Pin them
// here so the test fails loudly instead.
func TestStarterConstants_PinnedToOptionLists(t *testing.T) {
	found := false
	for _, a := range supportedAssets {
		if a.Name == starterAssetName {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("starterAssetName %q not in supportedAssets — interactive wizard would silently fall back to index 0", starterAssetName)
	}

	found = false
	for _, s := range spotStrategies {
		if s.ID == starterSpotStrategyID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("starterSpotStrategyID %q not in spotStrategies — interactive wizard would silently fall back to index 0", starterSpotStrategyID)
	}
}

func TestRunInitFromJSON_PerpsNoModeDefaultsPaper(t *testing.T) {
	out := filepath.Join(t.TempDir(), "config.json")
	jsonStr := `{"assets":["BTC"],"enablePerps":true}`
	code := runInitFromJSON(jsonStr, out)
	if code != 0 {
		t.Fatalf("expected exit 0 with perps default paper mode, got %d", code)
	}
	data, _ := os.ReadFile(out)
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	for _, s := range cfg.Strategies {
		if s.Type != "perps" {
			continue
		}
		found := false
		for _, arg := range s.Args {
			if arg == "--mode=paper" {
				found = true
			}
		}
		if !found {
			t.Errorf("expected --mode=paper in args for %s, got %v", s.ID, s.Args)
		}
	}
}

func TestDeriveShortName(t *testing.T) {
	cases := []struct {
		id   string
		want string
	}{
		{"sma_crossover", "sma"},
		{"ema_crossover", "ema"},
		{"momentum", "momentum"},
		{"bollinger_bands", "bb"},
		{"mean_reversion", "mr"},
		{"volume_weighted", "vw"},
		{"triple_ema", "tema"},
		{"triple_ema_bidir", "temab"},
		{"rsi_macd_combo", "rmc"},
		{"vol_mean_reversion", "vol"},
		{"momentum_options", "mom"},
		// unknown: first letter of each word
		{"my_new_strategy", "mns"},
		{"alpha_beta_gamma", "abg"},
	}
	for _, tc := range cases {
		if got := deriveShortName(tc.id); got != tc.want {
			t.Errorf("deriveShortName(%q) = %q, want %q", tc.id, got, tc.want)
		}
	}
}

// #328/#656 — triple_ema_bidir must generate Direction="both" in perps
// configs so ExecutePerpsSignalWithLeverage opens shorts from flat. Long-only strategies
// must keep Direction="long" so they can't silently flip into short positions.
func TestGenerateConfig_PerpsDirectionWiring(t *testing.T) {
	opts := baseOpts()
	opts.EnableSpot = false
	opts.EnablePerps = true
	opts.Assets = []string{"ETH"}
	opts.PerpsMode = "paper"
	opts.PerpsStrategies = []string{
		"triple_ema_bidir",
		"triple_ema",
		"rsi_macd_combo",
		"session_breakout",
		"donchian_breakout",
		"chart_pattern",
		"liquidity_sweeps",
	}

	cfg := generateConfig(opts)

	want := map[string]string{
		"hl-temab-eth": DirectionBoth, // bidirectional — must allow shorts
		"hl-tema-eth":  DirectionLong, // long-only — must NOT allow shorts
		"hl-rmc-eth":   DirectionLong, // long-only — must NOT allow shorts
		"hl-sbo-eth":   DirectionBoth, // bidirectional — must allow shorts (#371)
		"hl-dbo-eth":   DirectionBoth, // bidirectional — emits short on lower-channel breakdown (#649)
		"hl-cpat-eth":  DirectionBoth, // bidirectional — emits short on bearish patterns (#649)
		"hl-liqsw-eth": DirectionBoth, // bidirectional — emits short on stop-hunt wicks (#649)
	}
	seen := map[string]bool{}
	for _, s := range cfg.Strategies {
		expected, ok := want[s.ID]
		if !ok {
			continue
		}
		seen[s.ID] = true
		if s.Direction != expected {
			t.Errorf("%s Direction = %q, want %q", s.ID, s.Direction, expected)
		}
		// Defensive: never emit the legacy AllowShorts boolean from generateConfig.
		if s.AllowShorts {
			t.Errorf("%s AllowShorts = true, want false (deprecated; use Direction)", s.ID)
		}
	}
	for id := range want {
		if !seen[id] {
			t.Errorf("expected generated config for %s, not found", id)
		}
	}
}

func TestGenerateConfig_PerpsMultipleStrategies(t *testing.T) {
	opts := baseOpts()
	opts.EnableSpot = false
	opts.EnablePerps = true
	opts.Assets = []string{"BTC"}
	opts.PerpsMode = "paper"
	opts.PerpsStrategies = []string{"momentum", "rsi_macd_combo"}

	cfg := generateConfig(opts)

	// 2 strategies × 1 asset = 2 perps strategies
	if len(cfg.Strategies) != 2 {
		t.Fatalf("expected 2 perps strategies, got %d", len(cfg.Strategies))
	}
	ids := map[string]bool{}
	for _, s := range cfg.Strategies {
		ids[s.ID] = true
		if s.Type != "perps" {
			t.Errorf("expected perps type, got %s for %s", s.Type, s.ID)
		}
	}
	if !ids["hl-momentum-btc"] {
		t.Error("expected hl-momentum-btc")
	}
	if !ids["hl-rmc-btc"] {
		t.Error("expected hl-rmc-btc")
	}
}

func TestGenerateConfig_FuturesEnabled(t *testing.T) {
	opts := baseOpts()
	opts.EnableSpot = false
	opts.EnableFutures = true
	opts.FuturesMode = "paper"
	opts.FuturesStrategies = []string{"momentum"}
	opts.FuturesSymbols = []string{"ES", "MES"}
	opts.FuturesCapital = 5000
	opts.FuturesDrawdown = 5
	opts.FuturesFeePerContract = 1.50

	cfg := generateConfig(opts)

	// 1 strategy × 2 symbols = 2 futures strategies
	if len(cfg.Strategies) != 2 {
		for _, s := range cfg.Strategies {
			t.Logf("  %s (%s)", s.ID, s.Type)
		}
		t.Fatalf("expected 2 futures strategies, got %d", len(cfg.Strategies))
	}

	ids := map[string]bool{}
	for _, s := range cfg.Strategies {
		ids[s.ID] = true
		if s.Type != "futures" {
			t.Errorf("expected futures type, got %s for %s", s.Type, s.ID)
		}
		if s.Platform != "topstep" {
			t.Errorf("expected topstep platform, got %s for %s", s.Platform, s.ID)
		}
		if s.Script != "shared_scripts/check_topstep.py" {
			t.Errorf("expected check_topstep.py, got %s for %s", s.Script, s.ID)
		}
		if s.Capital != 5000 {
			t.Errorf("expected capital=5000, got %.0f for %s", s.Capital, s.ID)
		}
		if s.MaxDrawdownPct != 5 {
			t.Errorf("expected drawdown=5, got %.0f for %s", s.MaxDrawdownPct, s.ID)
		}
		if s.FuturesConfig == nil {
			t.Errorf("expected FuturesConfig to be set for %s", s.ID)
		} else if s.FuturesConfig.FeePerContract != 1.50 {
			t.Errorf("expected fee_per_contract=1.50, got %.2f for %s", s.FuturesConfig.FeePerContract, s.ID)
		}
	}

	if !ids["ts-momentum-es"] {
		t.Error("expected ts-momentum-es")
	}
	if !ids["ts-momentum-mes"] {
		t.Error("expected ts-momentum-mes")
	}
}

func TestGenerateConfig_FuturesScriptAndArgs(t *testing.T) {
	opts := baseOpts()
	opts.EnableSpot = false
	opts.EnableFutures = true
	opts.FuturesMode = "live"
	opts.FuturesStrategies = []string{"momentum"}
	opts.FuturesSymbols = []string{"ES"}
	opts.FuturesCapital = 5000
	opts.FuturesDrawdown = 5

	cfg := generateConfig(opts)

	if len(cfg.Strategies) != 1 {
		t.Fatalf("expected 1 futures strategy, got %d", len(cfg.Strategies))
	}
	s := cfg.Strategies[0]
	if s.ID != "ts-momentum-es" {
		t.Errorf("expected ts-momentum-es, got %s", s.ID)
	}
	if len(s.Args) != 4 || s.Args[0] != "momentum" || s.Args[1] != "ES" || s.Args[2] != "1h" || s.Args[3] != "--mode=live" {
		t.Errorf("unexpected futures args: %v", s.Args)
	}
}

func TestGenerateConfig_FuturesNoFeeConfig(t *testing.T) {
	opts := baseOpts()
	opts.EnableSpot = false
	opts.EnableFutures = true
	opts.FuturesMode = "paper"
	opts.FuturesStrategies = []string{"momentum"}
	opts.FuturesSymbols = []string{"ES"}
	opts.FuturesCapital = 5000
	opts.FuturesDrawdown = 5
	// No fee per contract set

	cfg := generateConfig(opts)

	s := cfg.Strategies[0]
	if s.FuturesConfig != nil {
		t.Errorf("expected nil FuturesConfig when fee is 0, got %+v", s.FuturesConfig)
	}
}

func TestRunInitFromJSON_FuturesEnabled(t *testing.T) {
	out := filepath.Join(t.TempDir(), "config.json")
	jsonStr := `{"assets":["BTC"],"enableFutures":true,"futuresSymbols":["ES","MES"],"futuresStrategies":["momentum"],"futuresCapital":5000,"futuresDrawdown":5,"futuresFeePerContract":1.50}`
	code := runInitFromJSON(jsonStr, out)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("expected output file: %v", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	futuresCount := 0
	for _, s := range cfg.Strategies {
		if s.Type == "futures" {
			futuresCount++
		}
	}
	if futuresCount != 2 {
		t.Errorf("expected 2 futures strategies, got %d", futuresCount)
	}
}

func TestGenerateConfig_CapitalPct(t *testing.T) {
	opts := baseOpts()
	opts.CapitalPct = 0.45

	cfg := generateConfig(opts)

	for _, s := range cfg.Strategies {
		if s.CapitalPct != 0.45 {
			t.Errorf("expected capital_pct=0.45 for %s, got %g", s.ID, s.CapitalPct)
		}
	}
}

func TestGenerateConfig_NoCapitalPct(t *testing.T) {
	opts := baseOpts()
	// CapitalPct defaults to 0 (not set)

	cfg := generateConfig(opts)

	for _, s := range cfg.Strategies {
		if s.CapitalPct != 0 {
			t.Errorf("expected capital_pct=0 for %s, got %g", s.ID, s.CapitalPct)
		}
	}
}

func TestConfigValidation_CapitalPctValid(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xTEST")
	cfg := &Config{
		IntervalSeconds: 600,
		Strategies: []StrategyConfig{
			{
				ID:             "test-pct",
				Type:           "spot",
				Platform:       "hyperliquid",
				Script:         "shared_scripts/check_strategy.py",
				Capital:        0,
				CapitalPct:     0.45,
				MaxDrawdownPct: 10,
			},
		},
		PortfolioRisk: &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 80},
	}
	if err := validateConfig(cfg, false); err != nil {
		t.Errorf("expected valid config with capital_pct, got error: %v", err)
	}
}

func TestConfigValidation_CapitalPctInvalid(t *testing.T) {
	cfg := &Config{
		IntervalSeconds: 600,
		Strategies: []StrategyConfig{
			{
				ID:             "test-bad-pct",
				Type:           "spot",
				Platform:       "hyperliquid",
				Script:         "shared_scripts/check_strategy.py",
				Capital:        0,
				CapitalPct:     1.5, // invalid: > 1
				MaxDrawdownPct: 10,
			},
		},
		PortfolioRisk: &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 80},
	}
	if err := validateConfig(cfg, false); err == nil {
		t.Error("expected validation error for capital_pct > 1")
	}
}

func TestConfigValidation_CapitalPctNegative(t *testing.T) {
	cfg := &Config{
		IntervalSeconds: 600,
		Strategies: []StrategyConfig{
			{
				ID:             "test-neg-pct",
				Type:           "spot",
				Platform:       "hyperliquid",
				Script:         "shared_scripts/check_strategy.py",
				Capital:        0,
				CapitalPct:     -0.5, // invalid: < 0
				MaxDrawdownPct: 10,
			},
		},
		PortfolioRisk: &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 80},
	}
	if err := validateConfig(cfg, false); err == nil {
		t.Error("expected validation error for negative capital_pct")
	}
}

func TestConfigValidation_NoCapitalNoCapitalPct(t *testing.T) {
	cfg := &Config{
		IntervalSeconds: 600,
		Strategies: []StrategyConfig{
			{
				ID:             "test-no-cap",
				Type:           "spot",
				Platform:       "hyperliquid",
				Script:         "shared_scripts/check_strategy.py",
				Capital:        0,
				CapitalPct:     0,
				MaxDrawdownPct: 10,
			},
		},
		PortfolioRisk: &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 80},
	}
	if err := validateConfig(cfg, false); err == nil {
		t.Error("expected validation error when neither capital nor capital_pct is set")
	}
}

func TestGenerateConfig_DefaultsForOptionalFields(t *testing.T) {
	// Simulates what the interactive wizard now does: only essential fields are set,
	// optional fields (notifications, auto-update) use zero-value/defaults.
	opts := InitOptions{
		Assets:         []string{"BTC"},
		EnableSpot:     true,
		SpotStrategies: []string{"sma_crossover"},
		SpotCapital:    1000,
		SpotDrawdown:   5,
		HTFFilter:      true,
	}
	cfg := generateConfig(opts)

	// Notifications should be disabled by default.
	if cfg.Discord.Enabled {
		t.Error("expected Discord.Enabled=false by default")
	}
	if cfg.Telegram.Enabled {
		t.Error("expected Telegram.Enabled=false by default")
	}
	if cfg.Discord.DMChannels != nil {
		t.Errorf("expected Discord.DMChannels=nil by default, got %v", cfg.Discord.DMChannels)
	}
	if cfg.Telegram.DMChannels != nil {
		t.Errorf("expected Telegram.DMChannels=nil by default, got %v", cfg.Telegram.DMChannels)
	}

	// Auto-update should default to empty (off).
	if cfg.AutoUpdate != "" {
		t.Errorf("expected AutoUpdate empty by default, got %q", cfg.AutoUpdate)
	}

	// HTF filter should be applied.
	for _, s := range cfg.Strategies {
		if s.Type != "options" && s.HTFFilter != true {
			t.Errorf("expected HTFFilter=true for %s", s.ID)
		}
	}

	// Strategy should exist with correct defaults.
	if len(cfg.Strategies) != 1 {
		t.Fatalf("expected 1 strategy, got %d", len(cfg.Strategies))
	}
	if cfg.Strategies[0].Capital != 1000 {
		t.Errorf("expected capital=1000, got %.0f", cfg.Strategies[0].Capital)
	}
}

func TestRunInitFromJSON_DefaultCapitalAndNotifications(t *testing.T) {
	// Verify that JSON mode with minimal input produces correct config with defaults.
	out := filepath.Join(t.TempDir(), "config.json")
	jsonStr := `{"assets":["BTC"],"enableSpot":true,"spotStrategies":["sma_crossover"],"spotCapital":1000,"spotDrawdown":5}`
	code := runInitFromJSON(jsonStr, out)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("expected output file: %v", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}

	// Notifications disabled by default.
	if cfg.Discord.Enabled {
		t.Error("expected Discord disabled by default in JSON mode")
	}
	if cfg.Telegram.Enabled {
		t.Error("expected Telegram disabled by default in JSON mode")
	}

	// Auto-update defaults to empty/off.
	if cfg.AutoUpdate != "" {
		t.Errorf("expected AutoUpdate empty by default, got %q", cfg.AutoUpdate)
	}
}

func TestGenerateConfig_OptionsRobinhood(t *testing.T) {
	opts := baseOpts()
	opts.EnableSpot = false
	opts.EnableOptions = true
	opts.OptionPlatforms = []string{"robinhood"}
	opts.OptStrategies = []string{"vol_mean_reversion"}
	// RobinhoodOptionsSymbols left empty — should default to [SPY, QQQ]

	cfg := generateConfig(opts)

	if len(cfg.Strategies) != 2 {
		t.Fatalf("expected 2 robinhood options strategies, got %d", len(cfg.Strategies))
	}
	for _, s := range cfg.Strategies {
		if s.Platform != "robinhood" {
			t.Errorf("expected platform robinhood, got %s for %s", s.Platform, s.ID)
		}
		if !strings.HasPrefix(s.ID, "rh-") {
			t.Errorf("expected rh- prefix, got %s", s.ID)
		}
		if s.Type != "options" {
			t.Errorf("expected options type, got %s for %s", s.Type, s.ID)
		}
	}
	ids := map[string]bool{}
	for _, s := range cfg.Strategies {
		ids[s.ID] = true
	}
	if !ids["rh-vol-spy"] {
		t.Error("expected rh-vol-spy strategy")
	}
	if !ids["rh-vol-qqq"] {
		t.Error("expected rh-vol-qqq strategy")
	}
}

func TestGenerateConfig_OptionsExcludesSOL(t *testing.T) {
	opts := baseOpts()
	opts.Assets = []string{"BTC", "SOL", "ETH"}
	opts.EnableSpot = false
	opts.EnableOptions = true
	opts.OptionPlatforms = []string{"deribit"}
	opts.OptStrategies = []string{"vol_mean_reversion"}

	cfg := generateConfig(opts)

	// SOL must be excluded; BTC and ETH included → 2 strategies
	if len(cfg.Strategies) != 2 {
		t.Fatalf("expected 2 options strategies (no SOL), got %d", len(cfg.Strategies))
	}
	for _, s := range cfg.Strategies {
		if strings.Contains(strings.ToLower(s.ID), "sol") {
			t.Errorf("SOL should be excluded from options, found %s", s.ID)
		}
	}
}

func TestGenerateConfig_PerpsSizingLeverageDefault(t *testing.T) {
	opts := baseOpts()
	opts.EnableSpot = false
	opts.EnablePerps = true
	opts.Assets = []string{"BTC"}
	opts.PerpsMode = "paper"
	opts.PerpsStrategies = []string{"momentum"}
	opts.PerpsLeverage = 5
	opts.PerpsSizingLeverage = 0 // omitted → should inherit PerpsLeverage

	cfg := generateConfig(opts)

	if len(cfg.Strategies) != 1 {
		t.Fatalf("expected 1 strategy, got %d", len(cfg.Strategies))
	}
	s := cfg.Strategies[0]
	if s.Leverage != 5 {
		t.Errorf("expected Leverage=5, got %g", s.Leverage)
	}
	if s.SizingLeverage != 5 {
		t.Errorf("expected SizingLeverage=5 (inherited from PerpsLeverage), got %g", s.SizingLeverage)
	}
}

func TestGenerateConfig_EnableLuno(t *testing.T) {
	opts := baseOpts()
	opts.EnableSpot = false
	opts.EnableLuno = true
	opts.Assets = []string{"BTC"}
	opts.LunoStrategies = []string{"momentum"}
	opts.LunoCapital = 500
	opts.LunoDrawdown = 5

	cfg := generateConfig(opts)

	if len(cfg.Strategies) != 1 {
		t.Fatalf("expected 1 luno strategy, got %d", len(cfg.Strategies))
	}
	s := cfg.Strategies[0]
	if s.ID != "luno-momentum-btc" {
		t.Errorf("expected luno-momentum-btc, got %s", s.ID)
	}
	if s.Platform != "luno" {
		t.Errorf("expected platform luno, got %s", s.Platform)
	}
	if s.Script != "shared_scripts/check_strategy.py" {
		t.Errorf("expected check_strategy.py, got %s", s.Script)
	}
	if s.Capital != 500 {
		t.Errorf("expected capital=500, got %g", s.Capital)
	}
}

func TestGenerateConfig_EnableRobinhood(t *testing.T) {
	opts := baseOpts()
	opts.EnableSpot = false
	opts.EnableRobinhood = true
	opts.Assets = []string{"BTC"}
	opts.RobinhoodStrategies = []string{"momentum"}
	opts.RobinhoodMode = "paper"
	opts.RobinhoodCapital = 500
	opts.RobinhoodDrawdown = 5

	cfg := generateConfig(opts)

	if len(cfg.Strategies) != 1 {
		t.Fatalf("expected 1 robinhood strategy, got %d", len(cfg.Strategies))
	}
	s := cfg.Strategies[0]
	if s.ID != "rh-momentum-btc" {
		t.Errorf("expected rh-momentum-btc, got %s", s.ID)
	}
	if s.Platform != "robinhood" {
		t.Errorf("expected platform robinhood, got %s", s.Platform)
	}
	if s.Script != "shared_scripts/check_robinhood.py" {
		t.Errorf("expected check_robinhood.py, got %s", s.Script)
	}
}

func TestGenerateConfig_EnableOKXSpot(t *testing.T) {
	opts := baseOpts()
	opts.EnableSpot = false
	opts.EnableOKX = true
	opts.Assets = []string{"BTC"}
	opts.OKXSpotStrategies = []string{"momentum"}
	opts.OKXMode = "paper"
	opts.OKXCapital = 1000
	opts.OKXDrawdown = 5

	cfg := generateConfig(opts)

	spotCount := 0
	for _, s := range cfg.Strategies {
		if s.Type == "spot" && s.Platform == "okx" {
			spotCount++
			if s.ID != "okx-momentum-btc" {
				t.Errorf("expected okx-momentum-btc, got %s", s.ID)
			}
			if s.Script != "shared_scripts/check_okx.py" {
				t.Errorf("expected check_okx.py, got %s", s.Script)
			}
			found := false
			for _, arg := range s.Args {
				if arg == "--inst-type=spot" {
					found = true
				}
			}
			if !found {
				t.Errorf("expected --inst-type=spot in args, got %v", s.Args)
			}
		}
	}
	if spotCount != 1 {
		t.Errorf("expected 1 OKX spot strategy, got %d", spotCount)
	}
}

func TestGenerateConfig_EnableOKXPerps(t *testing.T) {
	opts := baseOpts()
	opts.EnableSpot = false
	opts.EnableOKX = true
	opts.Assets = []string{"BTC"}
	opts.OKXPerpsStrategies = []string{"momentum"}
	opts.OKXMode = "paper"
	opts.OKXCapital = 1000
	opts.OKXDrawdown = 5
	opts.PerpsLeverage = 3

	cfg := generateConfig(opts)

	perpsCount := 0
	for _, s := range cfg.Strategies {
		if s.Type == "perps" && s.Platform == "okx" {
			perpsCount++
			if s.ID != "okx-momentum-btc-perp" {
				t.Errorf("expected okx-momentum-btc-perp, got %s", s.ID)
			}
			found := false
			for _, arg := range s.Args {
				if arg == "--inst-type=swap" {
					found = true
				}
			}
			if !found {
				t.Errorf("expected --inst-type=swap in args, got %v", s.Args)
			}
			if s.Leverage != 3 {
				t.Errorf("expected Leverage=3, got %g", s.Leverage)
			}
		}
	}
	if perpsCount != 1 {
		t.Errorf("expected 1 OKX perps strategy, got %d", perpsCount)
	}
}

func TestGenerateConfig_EnableManualDefaults(t *testing.T) {
	opts := baseOpts()
	opts.EnableSpot = false
	opts.EnableManual = true
	opts.ManualSymbol = "BTC"
	opts.ManualCapital = 2000
	// ManualTimeframe, ManualLeverage, ManualDrawdown left at zero → use defaults

	cfg := generateConfig(opts)

	if len(cfg.Strategies) != 1 {
		t.Fatalf("expected 1 manual strategy, got %d", len(cfg.Strategies))
	}
	s := cfg.Strategies[0]
	if s.ID != "hl-manual-btc-live" {
		t.Errorf("expected hl-manual-btc-live, got %s", s.ID)
	}
	if s.Type != "manual" {
		t.Errorf("expected type manual, got %s", s.Type)
	}
	if s.Timeframe != "1h" {
		t.Errorf("expected default Timeframe=1h, got %q", s.Timeframe)
	}
	if s.Leverage != 20 {
		t.Errorf("expected default Leverage=20, got %g", s.Leverage)
	}
	if s.MaxDrawdownPct != 20 {
		t.Errorf("expected default MaxDrawdownPct=20, got %g", s.MaxDrawdownPct)
	}
	if s.Capital != 2000 {
		t.Errorf("expected Capital=2000, got %g", s.Capital)
	}
}

func TestGenerateConfig_PortfolioRiskZeroUsesDefaults(t *testing.T) {
	opts := baseOpts()
	// PortfolioMaxDrawdownPct and PortfolioWarnThresholdPct both zero → use defaults

	cfg := generateConfig(opts)

	if cfg.PortfolioRisk == nil {
		t.Fatal("expected PortfolioRisk to be set")
	}
	if cfg.PortfolioRisk.MaxDrawdownPct != 25 {
		t.Errorf("expected MaxDrawdownPct=25 default, got %g", cfg.PortfolioRisk.MaxDrawdownPct)
	}
	if cfg.PortfolioRisk.WarnThresholdPct != 60 {
		t.Errorf("expected WarnThresholdPct=60 default, got %g", cfg.PortfolioRisk.WarnThresholdPct)
	}
}

func TestGenerateConfig_HTFFilterSkipsOptionsAndDNF(t *testing.T) {
	opts := baseOpts()
	opts.EnableSpot = true
	opts.SpotStrategies = []string{"momentum"}
	opts.EnableOptions = true
	opts.OptionPlatforms = []string{"deribit"}
	opts.OptStrategies = []string{"vol_mean_reversion"}
	opts.EnablePerps = true
	opts.PerpsMode = "paper"
	opts.PerpsStrategies = []string{"delta_neutral_funding", "triple_ema"}
	opts.Assets = []string{"BTC"}
	opts.HTFFilter = true

	cfg := generateConfig(opts)

	for _, s := range cfg.Strategies {
		switch {
		case s.Type == "options":
			if s.HTFFilter {
				t.Errorf("options strategy %s should not have HTFFilter set", s.ID)
			}
		case len(s.Args) > 0 && s.Args[0] == "delta_neutral_funding":
			if s.HTFFilter {
				t.Errorf("delta_neutral_funding strategy %s should not have HTFFilter set", s.ID)
			}
		default:
			if !s.HTFFilter {
				t.Errorf("non-options/non-DNF strategy %s should have HTFFilter=true", s.ID)
			}
		}
	}
}

func TestRunInitFromJSON_FuturesAutoPopulate(t *testing.T) {
	out := filepath.Join(t.TempDir(), "config.json")
	// Only enable futures; omit strategies/symbols/capital/drawdown — all should be auto-populated.
	jsonStr := `{"assets":["BTC"],"enableFutures":true}`
	code := runInitFromJSON(jsonStr, out)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("expected output file: %v", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	futuresCount := 0
	for _, s := range cfg.Strategies {
		if s.Type != "futures" {
			continue
		}
		futuresCount++
		if s.Capital == 0 {
			t.Errorf("expected non-zero capital on futures strategy %s", s.ID)
		}
		if s.MaxDrawdownPct == 0 {
			t.Errorf("expected non-zero drawdown on futures strategy %s", s.ID)
		}
		foundMode := false
		for _, arg := range s.Args {
			if arg == "--mode=paper" {
				foundMode = true
			}
		}
		if !foundMode {
			t.Errorf("expected --mode=paper in args for %s, got %v", s.ID, s.Args)
		}
	}
	if futuresCount == 0 {
		t.Error("expected at least one futures strategy from auto-populate")
	}
}

func TestRunInitFromJSON_RobinhoodAutoPopulate(t *testing.T) {
	out := filepath.Join(t.TempDir(), "config.json")
	jsonStr := `{"assets":["BTC"],"enableRobinhood":true}`
	code := runInitFromJSON(jsonStr, out)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
	data, _ := os.ReadFile(out)
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	rhCount := 0
	for _, s := range cfg.Strategies {
		if s.Platform == "robinhood" {
			rhCount++
		}
	}
	if rhCount == 0 {
		t.Error("expected robinhood strategies from auto-populate")
	}
}

func TestRunInitFromJSON_LunoAutoPopulate(t *testing.T) {
	out := filepath.Join(t.TempDir(), "config.json")
	jsonStr := `{"assets":["BTC"],"enableLuno":true}`
	code := runInitFromJSON(jsonStr, out)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
	data, _ := os.ReadFile(out)
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	lunoCount := 0
	for _, s := range cfg.Strategies {
		if s.Platform == "luno" {
			lunoCount++
		}
	}
	if lunoCount == 0 {
		t.Error("expected luno strategies from auto-populate")
	}
}

func TestRunInitFromJSON_OKXAutoPopulate(t *testing.T) {
	out := filepath.Join(t.TempDir(), "config.json")
	jsonStr := `{"assets":["BTC"],"enableOKX":true}`
	code := runInitFromJSON(jsonStr, out)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
	data, _ := os.ReadFile(out)
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	okxCount := 0
	for _, s := range cfg.Strategies {
		if s.Platform == "okx" {
			okxCount++
		}
	}
	if okxCount == 0 {
		t.Error("expected OKX strategies from auto-populate")
	}
}

func TestRunInitFromJSON_DeprecatedChannelMigration(t *testing.T) {
	out := filepath.Join(t.TempDir(), "config.json")
	jsonStr := `{"assets":["BTC"],"enableSpot":true,"spotStrategies":["momentum"],"spotCapital":1000,"spotDrawdown":5,"SpotChannelID":"ch-spot","OptionsChannelID":"ch-opts"}`
	code := runInitFromJSON(jsonStr, out)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
	data, _ := os.ReadFile(out)
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if cfg.Discord.Channels == nil {
		t.Fatal("expected Discord.Channels to be set from deprecated fields")
	}
	if cfg.Discord.Channels["spot"] != "ch-spot" {
		t.Errorf("expected Discord.Channels[spot]=ch-spot, got %q", cfg.Discord.Channels["spot"])
	}
	if cfg.Discord.Channels["options"] != "ch-opts" {
		t.Errorf("expected Discord.Channels[options]=ch-opts, got %q", cfg.Discord.Channels["options"])
	}
}

func TestRunInitFromJSON_PerpsSizingLeverageInherits(t *testing.T) {
	out := filepath.Join(t.TempDir(), "config.json")
	// PerpsLeverage=5, no PerpsSizingLeverage → should inherit 5
	jsonStr := `{"assets":["BTC"],"enablePerps":true,"perpsLeverage":5,"perpsStrategies":["momentum"],"perpsCapital":1000,"perpsDrawdown":5}`
	code := runInitFromJSON(jsonStr, out)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
	data, _ := os.ReadFile(out)
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	for _, s := range cfg.Strategies {
		if s.Type != "perps" {
			continue
		}
		if s.Leverage != 5 {
			t.Errorf("expected Leverage=5 for %s, got %g", s.ID, s.Leverage)
		}
		if s.SizingLeverage != 5 {
			t.Errorf("expected SizingLeverage=5 (inherited) for %s, got %g", s.ID, s.SizingLeverage)
		}
	}
}

func TestRunInitFromJSON_WriteError(t *testing.T) {
	// Pass a directory as output path → os.WriteFile should fail → exit 1
	dir := t.TempDir()
	jsonStr := `{"assets":["BTC"],"enableSpot":true,"spotStrategies":["momentum"],"spotCapital":1000,"spotDrawdown":5}`
	code := runInitFromJSON(jsonStr, dir)
	if code != 1 {
		t.Errorf("expected exit code 1 when writing to a directory, got %d", code)
	}
}

// #1048: DisableCircuitBreaker stamps circuit_breaker:false on every generated
// non-manual strategy; manual is exempt from CheckRisk so it is skipped (left
// nil). Default (false) leaves every strategy nil → enabled.
func TestGenerateConfig_DisableCircuitBreaker(t *testing.T) {
	opts := InitOptions{
		Assets:          []string{"BTC", "ETH"},
		EnableSpot:      true,
		EnablePerps:     true,
		PerpsMode:       "paper",
		SpotStrategies:  []string{"momentum"},
		PerpsStrategies: []string{"momentum"},
		SpotCapital:     1000,
		PerpsCapital:    1000,
		SpotDrawdown:    5,
		PerpsDrawdown:   5,
		// #569 manual tracking strategy — must stay nil (CB no-op for manual).
		EnableManual:    true,
		ManualSymbol:    "ETH",
		ManualTimeframe: "1h",
		ManualCapital:   1000,
		ManualDrawdown:  20,
		ManualLeverage:  20,
	}

	// Default: no stamping — every strategy stays nil → enabled.
	def := generateConfig(opts)
	for _, s := range def.Strategies {
		if s.CircuitBreaker != nil {
			t.Fatalf("default generateConfig should leave circuit_breaker nil; %s has %v", s.ID, *s.CircuitBreaker)
		}
	}

	// Opt-out: every non-manual strategy gets explicit false; manual stays nil.
	opts.DisableCircuitBreaker = true
	cfg := generateConfig(opts)
	sawNonManual := false
	sawManual := false
	for _, s := range cfg.Strategies {
		if s.Type == "manual" {
			sawManual = true
			if s.CircuitBreaker != nil {
				t.Fatalf("manual strategy %s should be skipped (CB no-op), got %v", s.ID, *s.CircuitBreaker)
			}
			continue
		}
		sawNonManual = true
		if s.CircuitBreaker == nil || *s.CircuitBreaker {
			t.Fatalf("non-manual strategy %s should have circuit_breaker:false, got %v", s.ID, s.CircuitBreaker)
		}
	}
	if !sawNonManual {
		t.Fatal("expected at least one non-manual strategy")
	}
	if !sawManual {
		t.Fatal("expected the manual tracking strategy to be generated")
	}
}

// #1273: the init --json cb_* overrides stamp every generated non-manual
// strategy; 0/omitted leaves the fields nil (the historical defaults), and the
// manual tracking strategy is always skipped (exempt from CheckRisk).
func TestGenerateConfig_CBOverrides(t *testing.T) {
	opts := InitOptions{
		Assets:          []string{"ETH"},
		EnablePerps:     true,
		PerpsMode:       "paper",
		PerpsStrategies: []string{"momentum"},
		PerpsCapital:    1000,
		PerpsDrawdown:   5,
		EnableManual:    true,
		ManualSymbol:    "ETH",
		ManualTimeframe: "1h",
		ManualCapital:   1000,
		ManualDrawdown:  20,
		ManualLeverage:  20,
	}

	// Default: no stamping — every strategy keeps nil fields.
	def := generateConfig(opts)
	for _, s := range def.Strategies {
		if s.CBDrawdownCooldownMinutes != nil || s.CBLossStreakThreshold != nil || s.CBLossStreakCooldownMinutes != nil {
			t.Fatalf("default generateConfig should leave cb_* overrides nil on %s", s.ID)
		}
	}

	opts.CBDrawdownCooldownMinutes = 720
	opts.CBLossStreakThreshold = 3
	opts.CBLossStreakCooldownMinutes = 30
	cfg := generateConfig(opts)
	sawNonManual, sawManual := false, false
	for _, s := range cfg.Strategies {
		if s.Type == "manual" {
			sawManual = true
			if s.CBDrawdownCooldownMinutes != nil || s.CBLossStreakThreshold != nil || s.CBLossStreakCooldownMinutes != nil {
				t.Fatalf("manual strategy %s should be skipped (CheckRisk-exempt)", s.ID)
			}
			continue
		}
		sawNonManual = true
		if s.CircuitBreakerDrawdownCooldown() != 12*time.Hour || s.CircuitBreakerLossStreakThreshold() != 3 || s.CircuitBreakerLossStreakCooldown() != 30*time.Minute {
			t.Fatalf("non-manual strategy %s missing stamped cb_* overrides", s.ID)
		}
	}
	if !sawNonManual || !sawManual {
		t.Fatal("expected both a non-manual and the manual tracking strategy")
	}
}
