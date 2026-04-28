package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	if s.ID != "momentum-btc" {
		t.Errorf("expected starter ID momentum-btc, got %s", s.ID)
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
	if len(cfg.Strategies) != 1 || cfg.Strategies[0].ID != "momentum-btc" {
		t.Fatalf("expected starter momentum-btc config, got %+v", cfg.Strategies)
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
	if len(cfg.Strategies) != 1 || cfg.Strategies[0].ID != "momentum-btc" {
		t.Fatalf("expected starter momentum-btc config, got %+v", cfg.Strategies)
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
		{"triple_ema_bd", "temab"},
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

// #328 — triple_ema_bd must generate AllowShorts=true in perps configs so
// ExecutePerpsSignal opens shorts from flat. Long-only strategies must keep
// AllowShorts=false so they can't silently flip into short positions.
func TestGenerateConfig_PerpsAllowShortsWiring(t *testing.T) {
	opts := baseOpts()
	opts.EnableSpot = false
	opts.EnablePerps = true
	opts.Assets = []string{"ETH"}
	opts.PerpsMode = "paper"
	opts.PerpsStrategies = []string{"triple_ema_bd", "triple_ema", "rsi_macd_combo", "session_breakout"}

	cfg := generateConfig(opts)

	want := map[string]bool{
		"hl-temab-eth": true,  // bidirectional — must allow shorts
		"hl-tema-eth":  false, // long-only — must NOT allow shorts
		"hl-rmc-eth":   false, // long-only — must NOT allow shorts
		"hl-sbo-eth":   true,  // bidirectional — must allow shorts (#371)
	}
	seen := map[string]bool{}
	for _, s := range cfg.Strategies {
		expected, ok := want[s.ID]
		if !ok {
			continue
		}
		seen[s.ID] = true
		if s.AllowShorts != expected {
			t.Errorf("%s AllowShorts = %v, want %v", s.ID, s.AllowShorts, expected)
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

func TestValidateConfig_CapitalPctValid(t *testing.T) {
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
	if err := ValidateConfig(cfg); err != nil {
		t.Errorf("expected valid config with capital_pct, got error: %v", err)
	}
}

func TestValidateConfig_CapitalPctInvalid(t *testing.T) {
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
	if err := ValidateConfig(cfg); err == nil {
		t.Error("expected validation error for capital_pct > 1")
	}
}

func TestValidateConfig_CapitalPctNegative(t *testing.T) {
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
	if err := ValidateConfig(cfg); err == nil {
		t.Error("expected validation error for negative capital_pct")
	}
}

func TestValidateConfig_NoCapitalNoCapitalPct(t *testing.T) {
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
	if err := ValidateConfig(cfg); err == nil {
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
