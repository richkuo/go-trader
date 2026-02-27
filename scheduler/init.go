package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// asset represents a tradeable asset with its exchange symbol.
type asset struct {
	Name   string // e.g. "BTC"
	Symbol string // e.g. "BTC/USDT"
}

var supportedAssets = []asset{
	{Name: "BTC", Symbol: "BTC/USDT"},
	{Name: "ETH", Symbol: "ETH/USDT"},
	{Name: "SOL", Symbol: "SOL/USDT"},
}

// stratDef defines a strategy template with its ID, short name for config IDs, and supported assets.
type stratDef struct {
	ID        string   // strategy arg used in script invocation
	ShortName string   // abbreviated name used in config IDs
	Assets    []string // supported asset names (not currently filtered — handled at generation time)
}

var spotStrategies = []stratDef{
	{ID: "sma_crossover", ShortName: "sma", Assets: []string{"BTC", "ETH", "SOL"}},
	{ID: "ema_crossover", ShortName: "ema", Assets: []string{"BTC", "ETH", "SOL"}},
	{ID: "momentum", ShortName: "momentum", Assets: []string{"BTC", "ETH", "SOL"}},
	{ID: "rsi", ShortName: "rsi", Assets: []string{"BTC", "ETH", "SOL"}},
	{ID: "bollinger_bands", ShortName: "bb", Assets: []string{"BTC", "ETH", "SOL"}},
	{ID: "macd", ShortName: "macd", Assets: []string{"BTC", "ETH", "SOL"}},
	{ID: "rsi_sma", ShortName: "rsi-sma", Assets: []string{"BTC", "ETH", "SOL"}},
	{ID: "rsi_ema", ShortName: "rsi-ema", Assets: []string{"BTC", "ETH", "SOL"}},
	{ID: "ema_rsi_macd", ShortName: "erm", Assets: []string{"BTC", "ETH", "SOL"}},
	{ID: "rsi_macd_combo", ShortName: "rmc", Assets: []string{"BTC", "ETH", "SOL"}},
}

var optionsStrategies = []stratDef{
	{ID: "vol_mean_reversion", ShortName: "vol", Assets: []string{"BTC", "ETH"}},
	{ID: "momentum_options", ShortName: "mom", Assets: []string{"BTC", "ETH"}},
	{ID: "protective_puts", ShortName: "pput", Assets: []string{"BTC", "ETH"}},
	{ID: "covered_calls", ShortName: "ccall", Assets: []string{"BTC", "ETH"}},
}

var perpsStrategies = []stratDef{
	{ID: "momentum", ShortName: "momentum", Assets: []string{"BTC", "ETH", "SOL"}},
}

// InitOptions captures all user choices from the interactive wizard.
type InitOptions struct {
	OutputPath       string
	Assets           []string // selected asset names, e.g. ["BTC", "ETH"]
	EnableSpot       bool
	EnableOptions    bool
	EnablePerps      bool
	OptionPlatforms  []string // "deribit", "ibkr", or both
	PerpsMode        string   // "paper" or "live"
	SpotStrategies   []string // selected spot strategy IDs
	IncludePairs     bool
	OptStrategies    []string // selected options strategy IDs
	SpotCapital      float64
	OptionsCapital   float64
	PerpsCapital     float64
	SpotDrawdown     float64
	OptionsDrawdown  float64
	PerpsDrawdown    float64
	DiscordEnabled   bool
	SpotChannelID    string
	OptionsChannelID string
}

// generateConfig builds a Config from InitOptions. Pure function, no I/O.
func generateConfig(opts InitOptions) *Config {
	cfg := &Config{
		IntervalSeconds: 3600,
		LogDir:          "logs",
		StateFile:       "scheduler/state.json",
		PortfolioRisk: &PortfolioRiskConfig{
			MaxDrawdownPct: 25,
			MaxNotionalUSD: 0,
		},
		Discord: DiscordConfig{
			Enabled: opts.DiscordEnabled,
			Channels: DiscordChannels{
				Spot:    opts.SpotChannelID,
				Options: opts.OptionsChannelID,
			},
		},
		Platforms: make(map[string]*PlatformConfig),
	}

	// Build asset name → exchange symbol map.
	assetSymbol := make(map[string]string)
	for _, a := range supportedAssets {
		assetSymbol[a.Name] = a.Symbol
	}

	usesHyperliquid := false

	// Spot strategies.
	if opts.EnableSpot {
		for _, stratID := range opts.SpotStrategies {
			shortName := stratShortName(spotStrategies, stratID)
			for _, assetName := range opts.Assets {
				sym := assetSymbol[assetName]
				if sym == "" {
					continue
				}
				id := shortName + "-" + strings.ToLower(assetName)
				cfg.Strategies = append(cfg.Strategies, StrategyConfig{
					ID:              id,
					Type:            "spot",
					Script:          "shared_scripts/check_strategy.py",
					Args:            []string{stratID, sym, "1h"},
					Capital:         opts.SpotCapital,
					MaxDrawdownPct:  opts.SpotDrawdown,
					IntervalSeconds: 3600,
				})
			}
		}

		// Pairs spread — only available with 2+ assets.
		if opts.IncludePairs && len(opts.Assets) >= 2 {
			for _, pair := range makePairs(opts.Assets) {
				a1, a2 := pair[0], pair[1]
				id := fmt.Sprintf("pairs-%s-%s", strings.ToLower(a1), strings.ToLower(a2))
				cfg.Strategies = append(cfg.Strategies, StrategyConfig{
					ID:              id,
					Type:            "spot",
					Script:          "shared_scripts/check_strategy.py",
					Args:            []string{"pairs_spread", assetSymbol[a1], "1d", assetSymbol[a2]},
					Capital:         opts.SpotCapital,
					MaxDrawdownPct:  opts.SpotDrawdown,
					IntervalSeconds: 86400,
				})
			}
		}
	}

	// Options strategies.
	if opts.EnableOptions {
		for _, stratID := range opts.OptStrategies {
			shortName := stratShortName(optionsStrategies, stratID)
			for _, platform := range opts.OptionPlatforms {
				for _, assetName := range opts.Assets {
					if assetName == "SOL" {
						continue // options don't support SOL
					}
					id := fmt.Sprintf("%s-%s-%s", platform, shortName, strings.ToLower(assetName))
					cfg.Strategies = append(cfg.Strategies, StrategyConfig{
						ID:              id,
						Type:            "options",
						Script:          "shared_scripts/check_options.py",
						Args:            []string{stratID, assetName, fmt.Sprintf("--platform=%s", platform)},
						Capital:         opts.OptionsCapital,
						MaxDrawdownPct:  opts.OptionsDrawdown,
						IntervalSeconds: 14400,
						ThetaHarvest: &ThetaHarvestConfig{
							Enabled:         true,
							ProfitTargetPct: 60,
							StopLossPct:     200,
							MinDTEClose:     3,
						},
					})
				}
			}
		}
	}

	// Perps strategies (Hyperliquid only).
	if opts.EnablePerps {
		usesHyperliquid = true
		for _, strat := range perpsStrategies {
			for _, assetName := range opts.Assets {
				id := fmt.Sprintf("hl-%s-%s", strat.ID, strings.ToLower(assetName))
				cfg.Strategies = append(cfg.Strategies, StrategyConfig{
					ID:              id,
					Type:            "perps",
					Script:          "shared_scripts/check_hyperliquid.py",
					Args:            []string{strat.ID, assetName, "1h", fmt.Sprintf("--mode=%s", opts.PerpsMode)},
					Capital:         opts.PerpsCapital,
					MaxDrawdownPct:  opts.PerpsDrawdown,
					IntervalSeconds: 3600,
				})
			}
		}
	}

	if usesHyperliquid {
		cfg.Platforms["hyperliquid"] = &PlatformConfig{
			StateFile: "platforms/hyperliquid/state.json",
		}
	}

	return cfg
}

// stratShortName returns the ShortName for a strategy ID, falling back to the ID itself.
func stratShortName(strats []stratDef, stratID string) string {
	for _, s := range strats {
		if s.ID == stratID {
			return s.ShortName
		}
	}
	return stratID
}

// makePairs returns all ordered 2-combinations of the given asset names.
func makePairs(assets []string) [][2]string {
	var pairs [][2]string
	for i := 0; i < len(assets); i++ {
		for j := i + 1; j < len(assets); j++ {
			pairs = append(pairs, [2]string{assets[i], assets[j]})
		}
	}
	return pairs
}

// runInit executes the interactive init wizard. Returns exit code.
func runInit(_ []string) int {
	p := NewPrompter()

	fmt.Println()
	fmt.Println("=== go-trader init ===")
	fmt.Println("Interactive config setup. Press Enter to accept defaults.")
	fmt.Println()

	// Step 1: Output path.
	outputPath := p.String("Output config path", "scheduler/config.json")
	if _, err := os.Stat(outputPath); err == nil {
		if !p.YesNo(fmt.Sprintf("  %s already exists. Overwrite?", outputPath), false) {
			fmt.Println("Aborted.")
			return 0
		}
	}

	// Step 2: Asset selection.
	assetNames := make([]string, len(supportedAssets))
	for i, a := range supportedAssets {
		assetNames[i] = a.Name
	}
	assetIdxs := p.MultiSelect("\nSelect assets to trade:", assetNames, true)
	if len(assetIdxs) == 0 {
		fmt.Println("No assets selected. Aborted.")
		return 1
	}
	selectedAssets := make([]string, len(assetIdxs))
	for i, idx := range assetIdxs {
		selectedAssets[i] = supportedAssets[idx].Name
	}

	// Step 3: Strategy types.
	stratTypeNames := []string{"spot", "options", "perps"}
	stratTypeIdxs := p.MultiSelect("\nSelect strategy types:", stratTypeNames, false)
	enableSpot, enableOptions, enablePerps := false, false, false
	for _, idx := range stratTypeIdxs {
		switch stratTypeNames[idx] {
		case "spot":
			enableSpot = true
		case "options":
			enableOptions = true
		case "perps":
			enablePerps = true
		}
	}
	if !enableSpot && !enableOptions && !enablePerps {
		fmt.Println("No strategy types selected. Aborted.")
		return 1
	}

	// Step 4: Options platform.
	var optionPlatforms []string
	if enableOptions {
		platOptions := []string{"deribit", "ibkr", "both"}
		platIdx := p.Choice("\nOptions platform:", platOptions, 0)
		switch platOptions[platIdx] {
		case "deribit":
			optionPlatforms = []string{"deribit"}
		case "ibkr":
			optionPlatforms = []string{"ibkr"}
		case "both":
			optionPlatforms = []string{"deribit", "ibkr"}
		}
	}

	// Step 5: Perps mode.
	perpsMode := "paper"
	if enablePerps {
		modeOptions := []string{"paper (safe default)", "live (requires HYPERLIQUID_SECRET_KEY)"}
		if p.Choice("\nPerps trading mode:", modeOptions, 0) == 1 {
			perpsMode = "live"
		}
	}

	// Step 6: Spot strategy selection.
	var selectedSpotStrats []string
	includePairs := false
	if enableSpot {
		spotNames := make([]string, len(spotStrategies))
		for i, s := range spotStrategies {
			spotNames[i] = s.ID
		}
		hasPairsOption := len(selectedAssets) >= 2
		if hasPairsOption {
			spotNames = append(spotNames, "pairs_spread")
		}
		spotIdxs := p.MultiSelect("\nSelect spot strategies:", spotNames, false)
		for _, idx := range spotIdxs {
			if hasPairsOption && idx == len(spotStrategies) {
				includePairs = true
			} else if idx < len(spotStrategies) {
				selectedSpotStrats = append(selectedSpotStrats, spotStrategies[idx].ID)
			}
		}
	}

	// Step 7: Options strategy selection.
	var selectedOptStrats []string
	if enableOptions {
		optNames := make([]string, len(optionsStrategies))
		for i, s := range optionsStrategies {
			optNames[i] = s.ID
		}
		optIdxs := p.MultiSelect("\nSelect options strategies:", optNames, false)
		for _, idx := range optIdxs {
			selectedOptStrats = append(selectedOptStrats, optionsStrategies[idx].ID)
		}
	}

	if len(selectedSpotStrats) == 0 && !includePairs && len(selectedOptStrats) == 0 && !enablePerps {
		fmt.Println("No strategies selected. Aborted.")
		return 1
	}

	// Step 8: Capital & risk.
	fmt.Println("\n--- Capital & Risk ---")
	spotCapital := 1000.0
	optionsCapital := 5000.0
	perpsCapital := 1000.0
	spotDrawdown := 5.0
	optionsDrawdown := 10.0
	perpsDrawdown := 5.0

	if enableSpot || includePairs {
		spotCapital = p.Float("Spot/pairs capital per strategy ($)", 1000)
		spotDrawdown = p.Float("Spot max drawdown (%)", 5)
	}
	if enableOptions {
		optionsCapital = p.Float("Options capital per strategy ($)", 5000)
		optionsDrawdown = p.Float("Options max drawdown (%)", 10)
	}
	if enablePerps {
		perpsCapital = p.Float("Perps capital per strategy ($)", 1000)
		perpsDrawdown = p.Float("Perps max drawdown (%)", 5)
	}

	// Step 9: Discord.
	fmt.Println("\n--- Discord Notifications ---")
	discordEnabled := p.YesNo("Enable Discord notifications?", false)
	spotChannelID := ""
	optionsChannelID := ""
	if discordEnabled {
		spotChannelID = p.String("Spot channel ID (leave blank to skip)", "")
		optionsChannelID = p.String("Options channel ID (leave blank to skip)", "")
	}

	opts := InitOptions{
		OutputPath:       outputPath,
		Assets:           selectedAssets,
		EnableSpot:       enableSpot,
		EnableOptions:    enableOptions,
		EnablePerps:      enablePerps,
		OptionPlatforms:  optionPlatforms,
		PerpsMode:        perpsMode,
		SpotStrategies:   selectedSpotStrats,
		IncludePairs:     includePairs,
		OptStrategies:    selectedOptStrats,
		SpotCapital:      spotCapital,
		OptionsCapital:   optionsCapital,
		PerpsCapital:     perpsCapital,
		SpotDrawdown:     spotDrawdown,
		OptionsDrawdown:  optionsDrawdown,
		PerpsDrawdown:    perpsDrawdown,
		DiscordEnabled:   discordEnabled,
		SpotChannelID:    spotChannelID,
		OptionsChannelID: optionsChannelID,
	}

	cfg := generateConfig(opts)

	// Step 10: Summary + confirm.
	fmt.Println("\n--- Summary ---")
	fmt.Printf("Output:     %s\n", outputPath)
	fmt.Printf("Assets:     %s\n", strings.Join(selectedAssets, ", "))
	fmt.Printf("Strategies: %d\n", len(cfg.Strategies))
	for _, s := range cfg.Strategies {
		fmt.Printf("  - %-35s (%s, $%.0f)\n", s.ID, s.Type, s.Capital)
	}

	if !p.YesNo("\nWrite config?", true) {
		fmt.Println("Aborted.")
		return 0
	}

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error marshaling config: %v\n", err)
		return 1
	}
	if err := os.WriteFile(outputPath, data, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "Error writing %s: %v\n", outputPath, err)
		return 1
	}

	fmt.Printf("\nConfig written to %s\n", outputPath)
	fmt.Println("Next steps:")
	if discordEnabled {
		fmt.Println("  export DISCORD_BOT_TOKEN=<your-token>")
	}
	fmt.Printf("  ./go-trader --config %s --once\n", outputPath)
	return 0
}
