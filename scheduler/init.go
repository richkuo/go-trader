package main

import (
	"encoding/json"
	"flag"
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

// stratDef defines a strategy template with its ID and short name for config IDs.
type stratDef struct {
	ID        string // strategy arg used in script invocation
	ShortName string // abbreviated name used in config IDs
}

// knownShortNames maps strategy IDs to abbreviated config ID prefixes.
var knownShortNames = map[string]string{
	"sma_crossover":         "sma",
	"ema_crossover":         "ema",
	"momentum":              "momentum",
	"rsi":                   "rsi",
	"bollinger_bands":       "bb",
	"macd":                  "macd",
	"mean_reversion":        "mr",
	"volume_weighted":       "vw",
	"triple_ema":            "tema",
	"rsi_macd_combo":        "rmc",
	"vol_mean_reversion":    "vol",
	"momentum_options":      "mom",
	"protective_puts":       "pput",
	"covered_calls":         "ccall",
	"breakout":              "bo",
	"atr_breakout":          "atrbo",
	"stoch_rsi":             "stochrsi",
	"ichimoku_cloud":        "ichi",
	"order_blocks":          "ob",
	"vwap_reversion":        "vwap",
	"chart_pattern":         "cpat",
	"liquidity_sweeps":      "liqsw",
	"parabolic_sar":         "psar",
	"delta_neutral_funding": "dnf",
}

// deriveShortName returns a short abbreviation for a strategy ID.
// Uses knownShortNames override map; falls back to first letter of each word.
func deriveShortName(id string) string {
	if name, ok := knownShortNames[id]; ok {
		return name
	}
	parts := strings.Split(id, "_")
	var sb strings.Builder
	for _, p := range parts {
		if len(p) > 0 {
			sb.WriteByte(p[0])
		}
	}
	return sb.String()
}

// defaultSpotStrategies is the fallback list when Python discovery fails.
var defaultSpotStrategies = []stratDef{
	{ID: "sma_crossover", ShortName: "sma"},
	{ID: "ema_crossover", ShortName: "ema"},
	{ID: "momentum", ShortName: "momentum"},
	{ID: "rsi", ShortName: "rsi"},
	{ID: "bollinger_bands", ShortName: "bb"},
	{ID: "macd", ShortName: "macd"},
	{ID: "mean_reversion", ShortName: "mr"},
	{ID: "volume_weighted", ShortName: "vw"},
	{ID: "triple_ema", ShortName: "tema"},
	{ID: "rsi_macd_combo", ShortName: "rmc"},
	{ID: "stoch_rsi", ShortName: "stochrsi"},
	{ID: "ichimoku_cloud", ShortName: "ichi"},
	{ID: "order_blocks", ShortName: "ob"},
	{ID: "vwap_reversion", ShortName: "vwap"},
	{ID: "chart_pattern", ShortName: "cpat"},
	{ID: "liquidity_sweeps", ShortName: "liqsw"},
	{ID: "parabolic_sar", ShortName: "psar"},
	{ID: "delta_neutral_funding", ShortName: "dnf"},
}

var defaultOptionsStrategies = []stratDef{
	{ID: "vol_mean_reversion", ShortName: "vol"},
	{ID: "momentum_options", ShortName: "mom"},
	{ID: "protective_puts", ShortName: "pput"},
	{ID: "covered_calls", ShortName: "ccall"},
}

var defaultPerpsStrategies = []stratDef{
	{ID: "momentum", ShortName: "momentum"},
	{ID: "chart_pattern", ShortName: "cpat"},
	{ID: "liquidity_sweeps", ShortName: "liqsw"},
}

var defaultFuturesStrategies = []stratDef{
	{ID: "momentum", ShortName: "momentum"},
	{ID: "mean_reversion", ShortName: "mr"},
	{ID: "rsi", ShortName: "rsi"},
	{ID: "macd", ShortName: "macd"},
	{ID: "breakout", ShortName: "bo"},
	{ID: "stoch_rsi", ShortName: "stochrsi"},
	{ID: "ichimoku_cloud", ShortName: "ichi"},
	{ID: "order_blocks", ShortName: "ob"},
	{ID: "vwap_reversion", ShortName: "vwap"},
	{ID: "chart_pattern", ShortName: "cpat"},
	{ID: "liquidity_sweeps", ShortName: "liqsw"},
	{ID: "parabolic_sar", ShortName: "psar"},
	{ID: "delta_neutral_funding", ShortName: "dnf"},
}

// Supported CME futures symbols for the init wizard.
var supportedFuturesSymbols = []string{"ES", "NQ", "MES", "MNQ", "CL", "GC"}

// Supported stock symbols for Robinhood options.
var supportedStockSymbols = []string{"SPY", "QQQ", "AAPL", "MSFT", "AMZN", "GOOGL", "TSLA", "META"}

// Live strategy lists — populated by discoverStrategies() at startup.
// Tests set these via init() to avoid Python dependency.
var (
	spotStrategies    []stratDef
	optionsStrategies []stratDef
	perpsStrategies   []stratDef
	futuresStrategies []stratDef
)

// stratListEntry is one element from --list-json output.
type stratListEntry struct {
	ID          string `json:"id"`
	Description string `json:"description"`
}

// discoverPythonStrategies calls a Python strategy module with --list-json and parses the result.
// Returns nil on any error (caller falls back to defaults).
func discoverPythonStrategies(script string) []stratDef {
	stdout, _, err := RunPythonScript(script, []string{"--list-json"})
	if err != nil {
		return nil
	}
	var entries []stratListEntry
	if err := json.Unmarshal(stdout, &entries); err != nil {
		return nil
	}
	strats := make([]stratDef, 0, len(entries))
	for _, e := range entries {
		strats = append(strats, stratDef{
			ID:        e.ID,
			ShortName: deriveShortName(e.ID),
		})
	}
	return strats
}

// discoverStrategies populates module-level strategy lists from Python.
// Falls back to defaults on any error — safe to call at startup.
func discoverStrategies() {
	spotStrategies = defaultSpotStrategies
	optionsStrategies = defaultOptionsStrategies
	perpsStrategies = defaultPerpsStrategies

	if discovered := discoverPythonStrategies("shared_strategies/spot/strategies.py"); len(discovered) > 0 {
		var filtered []stratDef
		for _, s := range discovered {
			if s.ID != "pairs_spread" {
				filtered = append(filtered, s)
			}
		}
		if len(filtered) > 0 {
			spotStrategies = filtered
			perpsStrategies = filtered // perps supports the same set as spot
		}
	}
	if discovered := discoverPythonStrategies("shared_strategies/options/strategies.py"); len(discovered) > 0 {
		optionsStrategies = discovered
	}
	futuresStrategies = defaultFuturesStrategies
	if discovered := discoverPythonStrategies("shared_strategies/futures/strategies.py"); len(discovered) > 0 {
		futuresStrategies = discovered
	}
}

// InitOptions captures all user choices from the interactive wizard.
type InitOptions struct {
	OutputPath              string
	Assets                  []string // selected asset names, e.g. ["BTC", "ETH"]
	EnableSpot              bool
	EnableOptions           bool
	EnablePerps             bool
	OptionPlatforms         []string // "deribit", "ibkr", or both
	PerpsMode               string   // "paper" or "live"
	SpotStrategies          []string // selected spot strategy IDs
	IncludePairs            bool
	OptStrategies           []string // selected options strategy IDs
	PerpsStrategies         []string // selected perps strategy IDs (auto-populated if empty)
	SpotCapital             float64
	OptionsCapital          float64
	PerpsCapital            float64
	SpotDrawdown            float64
	OptionsDrawdown         float64
	PerpsDrawdown           float64
	EnableFutures           bool
	FuturesMode             string   // "paper" or "live"
	FuturesStrategies       []string // selected futures strategy IDs
	FuturesSymbols          []string // selected CME symbols (e.g. ["ES", "MES"])
	FuturesCapital          float64
	FuturesDrawdown         float64
	FuturesFeePerContract   float64
	EnableLuno              bool
	LunoStrategies          []string // selected spot strategy IDs for Luno
	LunoCapital             float64
	LunoDrawdown            float64
	EnableRobinhood         bool
	RobinhoodMode           string   // "paper" or "live"
	RobinhoodStrategies     []string // selected crypto strategy IDs
	RobinhoodCapital        float64
	RobinhoodDrawdown       float64
	RobinhoodOptionsSymbols []string // stock tickers for Robinhood options (e.g. ["SPY", "QQQ"])
	EnableOKX               bool
	OKXMode                 string   // "paper" or "live"
	OKXSpotStrategies       []string // selected spot strategy IDs for OKX
	OKXPerpsStrategies      []string // selected perps strategy IDs for OKX
	OKXCapital              float64
	OKXDrawdown             float64
	HTFFilter               bool // higher-timeframe trend filter for all strategies
	DiscordEnabled          bool
	DiscordOwnerID          string            // Discord user ID for DM features (upgrade prompts, config migration)
	SpotChannelID           string            // deprecated: use ChannelMap
	OptionsChannelID        string            // deprecated: use ChannelMap
	ChannelMap              map[string]string // keyed by platform/type ("spot", "hyperliquid", "deribit", etc.)
	AutoUpdate              string            // "off", "daily", "heartbeat" (default: "off")
}

// generateConfig builds a Config from InitOptions. Pure function, no I/O.
func generateConfig(opts InitOptions) *Config {
	cfg := &Config{
		ConfigVersion:   CurrentConfigVersion,
		IntervalSeconds: 3600,
		LogDir:          "logs",
		StateFile:       "scheduler/state.json",
		PortfolioRisk: &PortfolioRiskConfig{
			MaxDrawdownPct: 25,
			MaxNotionalUSD: 0,
		},
		Discord: DiscordConfig{
			Enabled:  opts.DiscordEnabled,
			OwnerID:  opts.DiscordOwnerID,
			Channels: opts.ChannelMap,
		},
		AutoUpdate: opts.AutoUpdate,
		Platforms:  make(map[string]*PlatformConfig),
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
			shortName := deriveShortName(stratID)
			for _, assetName := range opts.Assets {
				sym := assetSymbol[assetName]
				if sym == "" {
					continue
				}
				id := shortName + "-" + strings.ToLower(assetName)
				cfg.Strategies = append(cfg.Strategies, StrategyConfig{
					ID:              id,
					Type:            "spot",
					Platform:        "binanceus",
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
					Platform:        "binanceus",
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
			shortName := deriveShortName(stratID)
			for _, platform := range opts.OptionPlatforms {
				// Robinhood options use stock symbols; others use crypto assets
				var symbols []string
				if platform == "robinhood" {
					symbols = opts.RobinhoodOptionsSymbols
					if len(symbols) == 0 {
						symbols = []string{"SPY", "QQQ"}
					}
				} else {
					for _, a := range opts.Assets {
						if a != "SOL" { // options don't support SOL
							symbols = append(symbols, a)
						}
					}
				}
				for _, assetName := range symbols {
					prefix := platform
					if platform == "robinhood" {
						prefix = "rh"
					}
					id := fmt.Sprintf("%s-%s-%s", prefix, shortName, strings.ToLower(assetName))
					cfg.Strategies = append(cfg.Strategies, StrategyConfig{
						ID:              id,
						Type:            "options",
						Platform:        platform,
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
		for _, stratID := range opts.PerpsStrategies {
			shortName := deriveShortName(stratID)
			for _, assetName := range opts.Assets {
				id := fmt.Sprintf("hl-%s-%s", shortName, strings.ToLower(assetName))
				cfg.Strategies = append(cfg.Strategies, StrategyConfig{
					ID:              id,
					Type:            "perps",
					Platform:        "hyperliquid",
					Script:          "shared_scripts/check_hyperliquid.py",
					Args:            []string{stratID, assetName, "1h", fmt.Sprintf("--mode=%s", opts.PerpsMode)},
					Capital:         opts.PerpsCapital,
					MaxDrawdownPct:  opts.PerpsDrawdown,
					IntervalSeconds: 3600,
				})
			}
		}
	}

	// Futures strategies (TopStep).
	usesTopstep := false
	if opts.EnableFutures {
		usesTopstep = true
		feePerContract := opts.FuturesFeePerContract
		for _, stratID := range opts.FuturesStrategies {
			shortName := deriveShortName(stratID)
			for _, symbol := range opts.FuturesSymbols {
				id := fmt.Sprintf("ts-%s-%s", shortName, strings.ToLower(symbol))
				sc := StrategyConfig{
					ID:              id,
					Type:            "futures",
					Platform:        "topstep",
					Script:          "shared_scripts/check_topstep.py",
					Args:            []string{stratID, symbol, "1h", fmt.Sprintf("--mode=%s", opts.FuturesMode)},
					Capital:         opts.FuturesCapital,
					MaxDrawdownPct:  opts.FuturesDrawdown,
					IntervalSeconds: 3600,
				}
				if feePerContract > 0 {
					sc.FuturesConfig = &FuturesConfig{FeePerContract: feePerContract}
				}
				cfg.Strategies = append(cfg.Strategies, sc)
			}
		}
	}

	// Luno spot strategies (reuses check_strategy.py, platform=luno for fees).
	usesLuno := false
	if opts.EnableLuno {
		usesLuno = true
		for _, stratID := range opts.LunoStrategies {
			shortName := deriveShortName(stratID)
			for _, assetName := range opts.Assets {
				sym := assetSymbol[assetName]
				if sym == "" {
					continue
				}
				id := fmt.Sprintf("luno-%s-%s", shortName, strings.ToLower(assetName))
				cfg.Strategies = append(cfg.Strategies, StrategyConfig{
					ID:              id,
					Type:            "spot",
					Platform:        "luno",
					Script:          "shared_scripts/check_strategy.py",
					Args:            []string{stratID, sym, "1h"},
					Capital:         opts.LunoCapital,
					MaxDrawdownPct:  opts.LunoDrawdown,
					IntervalSeconds: 3600,
				})
			}
		}
	}

	// Robinhood crypto strategies (reuses spot strategies on Robinhood crypto).
	usesRobinhood := false
	if opts.EnableRobinhood {
		usesRobinhood = true
		for _, stratID := range opts.RobinhoodStrategies {
			shortName := deriveShortName(stratID)
			for _, assetName := range opts.Assets {
				id := fmt.Sprintf("rh-%s-%s", shortName, strings.ToLower(assetName))
				cfg.Strategies = append(cfg.Strategies, StrategyConfig{
					ID:              id,
					Type:            "spot",
					Platform:        "robinhood",
					Script:          "shared_scripts/check_robinhood.py",
					Args:            []string{stratID, assetName, "1h", fmt.Sprintf("--mode=%s", opts.RobinhoodMode)},
					Capital:         opts.RobinhoodCapital,
					MaxDrawdownPct:  opts.RobinhoodDrawdown,
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
	if usesTopstep {
		cfg.Platforms["topstep"] = &PlatformConfig{
			StateFile: "platforms/topstep/state.json",
		}
	}

	usesOKX := false
	if opts.EnableOKX {
		usesOKX = true
		okxMode := opts.OKXMode
		if okxMode == "" {
			okxMode = "paper"
		}
		// OKX spot strategies
		for _, stratID := range opts.OKXSpotStrategies {
			shortName := deriveShortName(stratID)
			for _, assetName := range opts.Assets {
				id := fmt.Sprintf("okx-%s-%s", shortName, strings.ToLower(assetName))
				cfg.Strategies = append(cfg.Strategies, StrategyConfig{
					ID:              id,
					Type:            "spot",
					Platform:        "okx",
					Script:          "shared_scripts/check_okx.py",
					Args:            []string{stratID, assetName, "1h", fmt.Sprintf("--mode=%s", okxMode), "--inst-type=spot"},
					Capital:         opts.OKXCapital,
					MaxDrawdownPct:  opts.OKXDrawdown,
					IntervalSeconds: 3600,
				})
			}
		}
		// OKX perps strategies
		for _, stratID := range opts.OKXPerpsStrategies {
			shortName := deriveShortName(stratID)
			for _, assetName := range opts.Assets {
				id := fmt.Sprintf("okx-%s-%s-perp", shortName, strings.ToLower(assetName))
				cfg.Strategies = append(cfg.Strategies, StrategyConfig{
					ID:              id,
					Type:            "perps",
					Platform:        "okx",
					Script:          "shared_scripts/check_okx.py",
					Args:            []string{stratID, assetName, "1h", fmt.Sprintf("--mode=%s", okxMode), "--inst-type=swap"},
					Capital:         opts.OKXCapital,
					MaxDrawdownPct:  opts.OKXDrawdown,
					IntervalSeconds: 3600,
				})
			}
		}
	}

	if usesRobinhood {
		cfg.Platforms["robinhood"] = &PlatformConfig{
			StateFile: "platforms/robinhood/state.json",
		}
	}

	if usesLuno {
		cfg.Platforms["luno"] = &PlatformConfig{
			StateFile: "platforms/luno/state.json",
		}
	}

	if usesOKX {
		cfg.Platforms["okx"] = &PlatformConfig{
			StateFile: "platforms/okx/state.json",
		}
	}

	// Apply HTF filter to all non-options strategies if enabled.
	if opts.HTFFilter {
		for i := range cfg.Strategies {
			if cfg.Strategies[i].Type != "options" {
				cfg.Strategies[i].HTFFilter = true
			}
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

// runInitFromJSON generates a config from a JSON blob of InitOptions. Returns exit code.
func runInitFromJSON(jsonStr string, outputPath string) int {
	discoverStrategies()

	var opts InitOptions
	if err := json.Unmarshal([]byte(jsonStr), &opts); err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing --json: %v\n", err)
		return 1
	}

	if opts.OutputPath == "" {
		if outputPath != "" {
			opts.OutputPath = outputPath
		} else {
			opts.OutputPath = "scheduler/config.json"
		}
	}

	if len(opts.Assets) == 0 {
		fmt.Fprintln(os.Stderr, "Error: at least one asset required")
		return 1
	}
	if !opts.EnableSpot && !opts.EnableOptions && !opts.EnablePerps && !opts.EnableFutures && !opts.EnableRobinhood && !opts.EnableLuno && !opts.EnableOKX {
		fmt.Fprintln(os.Stderr, "Error: at least one strategy type must be enabled")
		return 1
	}
	if opts.EnableSpot && len(opts.SpotStrategies) == 0 && !opts.IncludePairs {
		fmt.Fprintln(os.Stderr, "Error: spot enabled but no spot strategies selected")
		return 1
	}
	if opts.EnableOptions && len(opts.OptStrategies) == 0 {
		fmt.Fprintln(os.Stderr, "Error: options enabled but no options strategies selected")
		return 1
	}
	if opts.EnableOptions && len(opts.OptionPlatforms) == 0 {
		fmt.Fprintln(os.Stderr, "Error: options enabled but no option platforms selected")
		return 1
	}
	if opts.EnablePerps && opts.PerpsMode == "" {
		opts.PerpsMode = "paper"
	}

	// Auto-populate PerpsStrategies from discovered list if not specified.
	if opts.EnablePerps && len(opts.PerpsStrategies) == 0 {
		for _, s := range perpsStrategies {
			opts.PerpsStrategies = append(opts.PerpsStrategies, s.ID)
		}
	}

	// Auto-populate FuturesStrategies from discovered list if not specified.
	if opts.EnableFutures {
		if opts.FuturesMode == "" {
			opts.FuturesMode = "paper"
		}
		if len(opts.FuturesStrategies) == 0 {
			for _, s := range futuresStrategies {
				opts.FuturesStrategies = append(opts.FuturesStrategies, s.ID)
			}
		}
		if len(opts.FuturesSymbols) == 0 {
			opts.FuturesSymbols = []string{"ES", "MES"}
		}
		if opts.FuturesCapital == 0 {
			opts.FuturesCapital = 5000
		}
		if opts.FuturesDrawdown == 0 {
			opts.FuturesDrawdown = 5
		}
	}

	// Auto-populate Robinhood options symbols.
	if opts.EnableOptions {
		for _, plt := range opts.OptionPlatforms {
			if plt == "robinhood" && len(opts.RobinhoodOptionsSymbols) == 0 {
				opts.RobinhoodOptionsSymbols = []string{"SPY", "QQQ"}
			}
		}
	}

	// Auto-populate Robinhood defaults.
	if opts.EnableRobinhood {
		if opts.RobinhoodMode == "" {
			opts.RobinhoodMode = "paper"
		}
		if len(opts.RobinhoodStrategies) == 0 {
			for _, s := range spotStrategies {
				opts.RobinhoodStrategies = append(opts.RobinhoodStrategies, s.ID)
			}
		}
		if opts.RobinhoodCapital == 0 {
			opts.RobinhoodCapital = 500
		}
		if opts.RobinhoodDrawdown == 0 {
			opts.RobinhoodDrawdown = 5
		}
	}

	// Auto-populate Luno defaults.
	if opts.EnableLuno {
		if len(opts.LunoStrategies) == 0 {
			for _, s := range spotStrategies {
				opts.LunoStrategies = append(opts.LunoStrategies, s.ID)
			}
		}
		if opts.LunoCapital == 0 {
			opts.LunoCapital = 500
		}
		if opts.LunoDrawdown == 0 {
			opts.LunoDrawdown = 5
		}
	}

	// Auto-populate OKX defaults.
	if opts.EnableOKX {
		if opts.OKXMode == "" {
			opts.OKXMode = "paper"
		}
		if len(opts.OKXSpotStrategies) == 0 {
			for _, s := range spotStrategies {
				opts.OKXSpotStrategies = append(opts.OKXSpotStrategies, s.ID)
			}
		}
		if len(opts.OKXPerpsStrategies) == 0 {
			for _, s := range perpsStrategies {
				opts.OKXPerpsStrategies = append(opts.OKXPerpsStrategies, s.ID)
			}
		}
		if opts.OKXCapital == 0 {
			opts.OKXCapital = 1000
		}
		if opts.OKXDrawdown == 0 {
			opts.OKXDrawdown = 5
		}
	}

	// Migrate deprecated SpotChannelID/OptionsChannelID into ChannelMap.
	if opts.ChannelMap == nil && (opts.SpotChannelID != "" || opts.OptionsChannelID != "") {
		opts.ChannelMap = make(map[string]string)
		if opts.SpotChannelID != "" {
			opts.ChannelMap["spot"] = opts.SpotChannelID
		}
		if opts.OptionsChannelID != "" {
			opts.ChannelMap["options"] = opts.OptionsChannelID
		}
	}

	cfg := generateConfig(opts)

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error marshaling config: %v\n", err)
		return 1
	}
	if err := os.WriteFile(opts.OutputPath, data, 0600); err != nil {
		fmt.Fprintf(os.Stderr, "Error writing %s: %v\n", opts.OutputPath, err)
		return 1
	}

	fmt.Println(opts.OutputPath)
	return 0
}

// runInit executes the interactive init wizard. Returns exit code.
func runInit(args []string) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	jsonFlag := fs.String("json", "", "JSON blob of InitOptions for non-interactive config generation")
	outputFlag := fs.String("output", "scheduler/config.json", "output config file path")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing flags: %v\n", err)
		return 1
	}

	if *jsonFlag != "" {
		return runInitFromJSON(*jsonFlag, *outputFlag)
	}

	discoverStrategies()

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
	stratTypeNames := []string{"spot", "options", "perps", "futures", "robinhood", "luno", "okx"}
	stratTypeIdxs := p.MultiSelect("\nSelect strategy types:", stratTypeNames, false)
	enableSpot, enableOptions, enablePerps, enableFutures, enableRobinhood, enableLuno, enableOKX := false, false, false, false, false, false, false
	for _, idx := range stratTypeIdxs {
		switch stratTypeNames[idx] {
		case "spot":
			enableSpot = true
		case "options":
			enableOptions = true
		case "perps":
			enablePerps = true
		case "futures":
			enableFutures = true
		case "robinhood":
			enableRobinhood = true
		case "luno":
			enableLuno = true
		case "okx":
			enableOKX = true
		}
	}
	if !enableSpot && !enableOptions && !enablePerps && !enableFutures && !enableRobinhood && !enableLuno && !enableOKX {
		fmt.Println("No strategy types selected. Aborted.")
		return 1
	}

	// Step 4: Options platform.
	var optionPlatforms []string
	if enableOptions {
		platOptions := []string{"deribit", "ibkr", "robinhood", "okx", "all"}
		platIdx := p.Choice("\nOptions platform:", platOptions, 0)
		switch platOptions[platIdx] {
		case "deribit":
			optionPlatforms = []string{"deribit"}
		case "ibkr":
			optionPlatforms = []string{"ibkr"}
		case "robinhood":
			optionPlatforms = []string{"robinhood"}
		case "okx":
			optionPlatforms = []string{"okx"}
		case "all":
			optionPlatforms = []string{"deribit", "ibkr", "robinhood", "okx"}
		}
	}

	// Step 4b: Robinhood options stock symbols.
	var robinhoodOptionsSymbols []string
	for _, plt := range optionPlatforms {
		if plt == "robinhood" {
			symIdxs := p.MultiSelect("\nSelect stock symbols for Robinhood options:", supportedStockSymbols, false)
			for _, idx := range symIdxs {
				robinhoodOptionsSymbols = append(robinhoodOptionsSymbols, supportedStockSymbols[idx])
			}
			if len(robinhoodOptionsSymbols) == 0 {
				robinhoodOptionsSymbols = []string{"SPY", "QQQ"} // defaults
			}
			break
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

	// Step 5b: Futures mode and symbols.
	futuresMode := "paper"
	var futuresSymbols []string
	if enableFutures {
		modeOptions := []string{"paper (safe default)", "live (requires TOPSTEP_API_KEY)"}
		if p.Choice("\nFutures trading mode:", modeOptions, 0) == 1 {
			futuresMode = "live"
		}
		symbolIdxs := p.MultiSelect("\nSelect futures symbols:", supportedFuturesSymbols, false)
		for _, idx := range symbolIdxs {
			futuresSymbols = append(futuresSymbols, supportedFuturesSymbols[idx])
		}
		if len(futuresSymbols) == 0 {
			futuresSymbols = []string{"ES", "MES"} // defaults
		}
	}

	// Step 5c: Robinhood mode.
	robinhoodMode := "paper"
	if enableRobinhood {
		modeOptions := []string{"paper (safe default — signal only, no orders)", "live (requires ROBINHOOD_USERNAME/PASSWORD/TOTP_SECRET)"}
		if p.Choice("\nRobinhood trading mode:", modeOptions, 0) == 1 {
			robinhoodMode = "live"
		}
	}

	// Step 5d: OKX mode.
	okxMode := "paper"
	if enableOKX {
		modeOptions := []string{"paper (safe default)", "live (requires OKX_API_KEY/API_SECRET/PASSPHRASE)"}
		if p.Choice("\nOKX trading mode:", modeOptions, 0) == 1 {
			okxMode = "live"
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

	// Step 7b: Futures strategy selection.
	var selectedFuturesStrats []string
	if enableFutures {
		futNames := make([]string, len(futuresStrategies))
		for i, s := range futuresStrategies {
			futNames[i] = s.ID
		}
		futIdxs := p.MultiSelect("\nSelect futures strategies:", futNames, false)
		for _, idx := range futIdxs {
			selectedFuturesStrats = append(selectedFuturesStrats, futuresStrategies[idx].ID)
		}
	}

	// Step 7c: Luno strategy selection.
	var selectedLunoStrats []string
	if enableLuno {
		lunoNames := make([]string, len(spotStrategies))
		for i, s := range spotStrategies {
			lunoNames[i] = s.ID
		}
		lunoIdxs := p.MultiSelect("\nSelect Luno strategies:", lunoNames, false)
		for _, idx := range lunoIdxs {
			selectedLunoStrats = append(selectedLunoStrats, spotStrategies[idx].ID)
		}
	}

	// Step 7d: OKX strategy selection.
	var selectedOKXSpotStrats []string
	var selectedOKXPerpsStrats []string
	if enableOKX {
		fmt.Println("\n--- OKX Spot Strategies ---")
		okxSpotNames := make([]string, len(spotStrategies))
		for i, s := range spotStrategies {
			okxSpotNames[i] = s.ID
		}
		okxSpotIdxs := p.MultiSelect("\nSelect OKX spot strategies:", okxSpotNames, false)
		for _, idx := range okxSpotIdxs {
			selectedOKXSpotStrats = append(selectedOKXSpotStrats, spotStrategies[idx].ID)
		}

		fmt.Println("\n--- OKX Perps Strategies ---")
		okxPerpsNames := make([]string, len(perpsStrategies))
		for i, s := range perpsStrategies {
			okxPerpsNames[i] = s.ID
		}
		okxPerpsIdxs := p.MultiSelect("\nSelect OKX perps strategies:", okxPerpsNames, false)
		for _, idx := range okxPerpsIdxs {
			selectedOKXPerpsStrats = append(selectedOKXPerpsStrats, perpsStrategies[idx].ID)
		}
	}

	if len(selectedSpotStrats) == 0 && !includePairs && len(selectedOptStrats) == 0 && !enablePerps && !enableFutures && !enableRobinhood && !enableLuno && !enableOKX {
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

	robinhoodCapital := 500.0
	robinhoodDrawdown := 5.0
	if enableRobinhood {
		robinhoodCapital = p.Float("Robinhood crypto capital per strategy ($)", 500)
		robinhoodDrawdown = p.Float("Robinhood max drawdown (%)", 5)
	}

	lunoCapital := 500.0
	lunoDrawdown := 5.0
	if enableLuno {
		lunoCapital = p.Float("Luno capital per strategy ($)", 500)
		lunoDrawdown = p.Float("Luno max drawdown (%)", 5)
	}

	futuresCapital := 5000.0
	futuresDrawdown := 5.0
	futuresFeePerContract := 0.0
	if enableFutures {
		futuresCapital = p.Float("Futures capital per strategy ($)", 5000)
		futuresDrawdown = p.Float("Futures max drawdown (%)", 5)
		futuresFeePerContract = p.Float("Futures fee per contract ($)", 1.50)
	}

	okxCapital := 1000.0
	okxDrawdown := 5.0
	if enableOKX {
		okxCapital = p.Float("OKX capital per strategy ($)", 1000)
		okxDrawdown = p.Float("OKX max drawdown (%)", 5)
	}

	// Step 9: Discord.
	fmt.Println("\n--- Discord Notifications ---")
	discordEnabled := p.YesNo("Enable Discord notifications?", false)
	channelMap := make(map[string]string)
	discordOwnerID := ""
	if discordEnabled {
		if enableSpot || includePairs {
			if ch := p.String("Spot channel ID (leave blank to skip)", ""); ch != "" {
				channelMap["spot"] = ch
			}
		}
		if enableOptions {
			for _, plt := range optionPlatforms {
				if ch := p.String(fmt.Sprintf("%s channel ID (leave blank to skip)", plt), ""); ch != "" {
					channelMap[plt] = ch
				}
			}
		}
		if enablePerps {
			if ch := p.String("Hyperliquid channel ID (leave blank to skip)", ""); ch != "" {
				channelMap["hyperliquid"] = ch
			}
		}
		if enableFutures {
			if ch := p.String("TopStep channel ID (leave blank to skip)", ""); ch != "" {
				channelMap["topstep"] = ch
			}
		}
		if enableRobinhood {
			if ch := p.String("Robinhood channel ID (leave blank to skip)", ""); ch != "" {
				channelMap["robinhood"] = ch
			}
		}
		if enableLuno {
			if ch := p.String("Luno channel ID (leave blank to skip)", ""); ch != "" {
				channelMap["luno"] = ch
			}
		}
		if enableOKX {
			if ch := p.String("OKX channel ID (leave blank to skip)", ""); ch != "" {
				channelMap["okx"] = ch
			}
		}
		discordOwnerID = p.String("Your Discord user ID for DM upgrades (leave blank to skip)", "")
	}

	// Step 10: Auto-update preference.
	fmt.Println("\n--- Auto-Update ---")
	autoUpdateOptions := []string{
		"off — manual updates only (default)",
		"daily — check for updates once per day (DMs owner if configured)",
		"heartbeat — check for updates every scheduler cycle (DMs owner if configured)",
	}
	autoUpdateIdx := p.Choice("Check for new releases automatically?", autoUpdateOptions, 0)
	autoUpdateModes := []string{"off", "daily", "heartbeat"}
	autoUpdate := autoUpdateModes[autoUpdateIdx]

	// HTF trend filter.
	fmt.Println("\n--- HTF Trend Filter ---")
	htfFilter := p.YesNo("Enable higher-timeframe trend filter? (filters counter-trend signals)", true)

	// Collect all perps strategy IDs (auto-selected, no user prompt).
	perpsStratIDs := make([]string, len(perpsStrategies))
	for i, s := range perpsStrategies {
		perpsStratIDs[i] = s.ID
	}

	// Collect futures strategy IDs.
	futuresStratIDs := selectedFuturesStrats
	if enableFutures && len(futuresStratIDs) == 0 {
		for _, s := range futuresStrategies {
			futuresStratIDs = append(futuresStratIDs, s.ID)
		}
	}

	// Collect Robinhood strategy IDs (auto-selected from spot strategies).
	robinhoodStratIDs := make([]string, 0)
	if enableRobinhood {
		for _, s := range spotStrategies {
			robinhoodStratIDs = append(robinhoodStratIDs, s.ID)
		}
	}

	// Collect Luno strategy IDs.
	lunoStratIDs := selectedLunoStrats
	if enableLuno && len(lunoStratIDs) == 0 {
		for _, s := range spotStrategies {
			lunoStratIDs = append(lunoStratIDs, s.ID)
		}
	}

	// Collect OKX strategy IDs.
	okxSpotStratIDs := selectedOKXSpotStrats
	if enableOKX && len(okxSpotStratIDs) == 0 {
		for _, s := range spotStrategies {
			okxSpotStratIDs = append(okxSpotStratIDs, s.ID)
		}
	}
	okxPerpsStratIDs := selectedOKXPerpsStrats
	if enableOKX && len(okxPerpsStratIDs) == 0 {
		for _, s := range perpsStrategies {
			okxPerpsStratIDs = append(okxPerpsStratIDs, s.ID)
		}
	}

	opts := InitOptions{
		OutputPath:              outputPath,
		Assets:                  selectedAssets,
		EnableSpot:              enableSpot,
		EnableOptions:           enableOptions,
		EnablePerps:             enablePerps,
		OptionPlatforms:         optionPlatforms,
		PerpsMode:               perpsMode,
		SpotStrategies:          selectedSpotStrats,
		IncludePairs:            includePairs,
		OptStrategies:           selectedOptStrats,
		PerpsStrategies:         perpsStratIDs,
		SpotCapital:             spotCapital,
		OptionsCapital:          optionsCapital,
		PerpsCapital:            perpsCapital,
		SpotDrawdown:            spotDrawdown,
		OptionsDrawdown:         optionsDrawdown,
		PerpsDrawdown:           perpsDrawdown,
		RobinhoodOptionsSymbols: robinhoodOptionsSymbols,
		EnableRobinhood:         enableRobinhood,
		RobinhoodMode:           robinhoodMode,
		RobinhoodStrategies:     robinhoodStratIDs,
		RobinhoodCapital:        robinhoodCapital,
		RobinhoodDrawdown:       robinhoodDrawdown,
		EnableLuno:              enableLuno,
		LunoStrategies:          lunoStratIDs,
		LunoCapital:             lunoCapital,
		LunoDrawdown:            lunoDrawdown,
		EnableFutures:           enableFutures,
		FuturesMode:             futuresMode,
		FuturesStrategies:       futuresStratIDs,
		FuturesSymbols:          futuresSymbols,
		FuturesCapital:          futuresCapital,
		FuturesDrawdown:         futuresDrawdown,
		FuturesFeePerContract:   futuresFeePerContract,
		EnableOKX:               enableOKX,
		OKXMode:                 okxMode,
		OKXSpotStrategies:       okxSpotStratIDs,
		OKXPerpsStrategies:      okxPerpsStratIDs,
		OKXCapital:              okxCapital,
		OKXDrawdown:             okxDrawdown,
		HTFFilter:               htfFilter,
		DiscordEnabled:          discordEnabled,
		DiscordOwnerID:          discordOwnerID,
		ChannelMap:              channelMap,
		AutoUpdate:              autoUpdate,
	}

	cfg := generateConfig(opts)

	// Step 11: Summary + confirm.
	fmt.Println("\n--- Summary ---")
	fmt.Printf("Output:     %s\n", outputPath)
	fmt.Printf("Assets:     %s\n", strings.Join(selectedAssets, ", "))
	fmt.Printf("Auto-update: %s\n", autoUpdate)
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
	if err := os.WriteFile(outputPath, data, 0600); err != nil {
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
