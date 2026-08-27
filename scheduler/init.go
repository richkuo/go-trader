package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
)

type asset struct {
	Name   string
	Symbol string
}

var supportedAssets = []asset{
	{Name: "BTC", Symbol: "BTC/USDT"},
	{Name: "ETH", Symbol: "ETH/USDT"},
	{Name: "SOL", Symbol: "SOL/USDT"},
}

const (
	starterAssetName = "BTC"

	starterSpotStrategyID = "chart_pattern"
	starterSpotCapital    = 1000.0
	starterSpotDrawdown   = 5.0
)

type stratDef struct {
	ID        string
	ShortName string
}

var knownShortNames = map[string]string{
	"sma_crossover":           "sma",
	"ema_crossover":           "ema",
	"momentum":                "momentum",
	"rsi":                     "rsi",
	"bollinger_bands":         "bb",
	"macd":                    "macd",
	"mean_reversion":          "mr",
	"volume_weighted":         "vw",
	"triple_ema":              "tema",
	"triple_ema_bidir":        "temab",
	"tema_cross":              "temac",
	"tema_cross_bd":           "temacb",
	"rsi_macd_combo":          "rmc",
	"vol_mean_reversion":      "vol",
	"momentum_options":        "mom",
	"protective_puts":         "pput",
	"covered_calls":           "ccall",
	"breakout":                "bo",
	"atr_breakout":            "atrbo",
	"stoch_rsi":               "stochrsi",
	"ichimoku_cloud":          "ichi",
	"order_blocks":            "ob",
	"vwap_reversion":          "vwap",
	"anchored_vwap":           "avwap",
	"anchored_vwap_channel":   "avwapch",
	"anchored_vwap_reversion": "avwaprev",
	"chart_pattern":           "cpat",
	"liquidity_sweeps":        "liqsw",
	"parabolic_sar":           "psar",
	"delta_neutral_funding":   "dnf",
	"funding_skew":            "fskew",
	"supertrend":              "st",
	"squeeze_momentum":        "sqm",
	"heikin_ashi_ema":         "hae",
	"range_scalper":           "rs",
	"sweep_squeeze_combo":     "ssc",
	"adx_trend":               "adxt",
	"donchian_breakout":       "dbo",
	"session_breakout":        "sbo",
	"bear_pullback_st":        "bps",
	"vwap_rejection_st":       "vrs",
	"momentum_pro":            "mompro",
	"mean_reversion_pro":      "mrpro",
	"rsi_bb_combo":            "rsibb",
	"consolidation_range":     "cr",
	"atr_band_revert":         "abr",
	"mtf_confluence":          "mtfc",
	"vol_momentum":            "volmom",
	"regime_adaptive":         "regad",
	"regime_adaptive_htf":     "rahtf",
}

var bidirectionalPerpsStrategies = map[string]bool{
	"triple_ema_bidir":        true,
	"tema_cross_bd":           true,
	"session_breakout":        true,
	"donchian_breakout":       true,
	"chart_pattern":           true,
	"liquidity_sweeps":        true,
	"bear_pullback_st":        true,
	"vwap_rejection_st":       true,
	"anchored_vwap":           true,
	"anchored_vwap_channel":   true,
	"anchored_vwap_reversion": true,
	"momentum_pro":            true,
	"mean_reversion_pro":      true,
	"rsi_bb_combo":            true,
	"consolidation_range":     true,
	"atr_band_revert":         true,
	"mtf_confluence":          true,
	"vol_momentum":            true,
	"funding_skew":            true,
	"regime_adaptive":         true,
}

func isBidirectionalPerpsStrategy(id string) bool {
	return bidirectionalPerpsStrategies[id]
}

var strategiesDefaultingToCompositeRangingGate = map[string][]string{
	"atr_band_revert": {"ranging_quiet", "ranging_volatile"},

	"anchored_vwap_channel": {"ranging_quiet", "ranging_volatile"},

	"anchored_vwap_reversion": {"ranging_quiet", "ranging_volatile"},

	"rsi_bb_combo": {"ranging_quiet", "ranging_volatile"},
}

func defaultCompositeRangingGate(stratID string) []string {
	labels, ok := strategiesDefaultingToCompositeRangingGate[stratID]
	if !ok {
		return nil
	}
	out := make([]string, len(labels))
	copy(out, labels)
	return out
}

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

var defaultSpotStrategies = []stratDef{
	{ID: "anchored_vwap", ShortName: "avwap"},
	{ID: "anchored_vwap_channel", ShortName: "avwapch"},
	{ID: "anchored_vwap_reversion", ShortName: "avwaprev"},
	{ID: "chart_pattern", ShortName: "cpat"},
	{ID: "liquidity_sweeps", ShortName: "liqsw"},
	{ID: "momentum_pro", ShortName: "mompro"},
	{ID: "mean_reversion_pro", ShortName: "mrpro"},
	{ID: "atr_band_revert", ShortName: "abr"},
	{ID: "regime_adaptive_htf", ShortName: "rahtf"},
}

var defaultOptionsStrategies = []stratDef{
	{ID: "vol_mean_reversion", ShortName: "vol"},
	{ID: "momentum_options", ShortName: "mom"},
	{ID: "protective_puts", ShortName: "pput"},
	{ID: "covered_calls", ShortName: "ccall"},
}

var defaultPerpsStrategies = []stratDef{
	{ID: "chart_pattern", ShortName: "cpat"},
	{ID: "liquidity_sweeps", ShortName: "liqsw"},
	{ID: "anchored_vwap", ShortName: "avwap"},
	{ID: "anchored_vwap_channel", ShortName: "avwapch"},
	{ID: "anchored_vwap_reversion", ShortName: "avwaprev"},
	{ID: "delta_neutral_funding", ShortName: "dnf"},
	{ID: "momentum_pro", ShortName: "mompro"},
	{ID: "mean_reversion_pro", ShortName: "mrpro"},
	{ID: "atr_band_revert", ShortName: "abr"},
	{ID: "regime_adaptive_htf", ShortName: "rahtf"},
}

var defaultFuturesStrategies = []stratDef{
	{ID: "breakout", ShortName: "bo"},
	{ID: "anchored_vwap", ShortName: "avwap"},
	{ID: "anchored_vwap_channel", ShortName: "avwapch"},
	{ID: "anchored_vwap_reversion", ShortName: "avwaprev"},
	{ID: "chart_pattern", ShortName: "cpat"},
	{ID: "liquidity_sweeps", ShortName: "liqsw"},
	{ID: "delta_neutral_funding", ShortName: "dnf"},
	{ID: "momentum_pro", ShortName: "mompro"},
	{ID: "mean_reversion_pro", ShortName: "mrpro"},
	{ID: "atr_band_revert", ShortName: "abr"},
	{ID: "regime_adaptive_htf", ShortName: "rahtf"},
}

var supportedFuturesSymbols = []string{"ES", "NQ", "MES", "MNQ", "CL", "GC"}

var supportedStockSymbols = []string{"SPY", "QQQ", "AAPL", "MSFT", "AMZN", "GOOGL", "TSLA", "META"}

var (
	spotStrategies    []stratDef
	optionsStrategies []stratDef
	perpsStrategies   []stratDef
	futuresStrategies []stratDef
)

type stratListEntry struct {
	ID          string `json:"id"`
	Description string `json:"description"`
}

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

func discoverStrategies() {
	spotStrategies = defaultSpotStrategies
	optionsStrategies = defaultOptionsStrategies
	perpsStrategies = defaultPerpsStrategies

	if discovered := discoverPythonStrategies("shared_strategies/open/spot/strategies.py"); len(discovered) > 0 {
		var filtered []stratDef
		for _, s := range discovered {
			if s.ID != "pairs_spread" {
				filtered = append(filtered, s)
			}
		}
		if len(filtered) > 0 {
			spotStrategies = filtered
		}
	}
	if discovered := discoverPythonStrategies("shared_strategies/options/strategies.py"); len(discovered) > 0 {
		optionsStrategies = discovered
	}
	futuresStrategies = defaultFuturesStrategies
	if discovered := discoverPythonStrategies("shared_strategies/open/futures/strategies.py"); len(discovered) > 0 {
		futuresStrategies = discovered

		perpsStrategies = discovered
	}
}

func hasAnyEnabledStrategyType(opts InitOptions) bool {
	return opts.EnableSpot || opts.EnableOptions || opts.EnablePerps || opts.EnableFutures || opts.EnableRobinhood || opts.EnableLuno || opts.EnableOKX || opts.EnableManual
}

func applyMinimalStarterDefaults(opts *InitOptions) {
	if !opts.EnableSpot && hasAnyEnabledStrategyType(*opts) {
		return
	}
	if !opts.EnableSpot {
		opts.EnableSpot = true
	}
	if len(opts.Assets) == 0 {
		opts.Assets = []string{starterAssetName}

		opts.IncludePairs = false
	}
	if len(opts.SpotStrategies) == 0 && (!opts.IncludePairs || len(opts.Assets) < 2) {
		opts.SpotStrategies = []string{starterSpotStrategyID}
	}
	if opts.SpotCapital <= 0 {
		opts.SpotCapital = starterSpotCapital
	}
	if opts.SpotDrawdown <= 0 {
		opts.SpotDrawdown = starterSpotDrawdown
	}
}

func selectionDefaults(options []string, preferred []string, fallbackFirst bool) []int {
	indexByValue := make(map[string]int, len(options))
	for i, option := range options {
		indexByValue[option] = i
	}
	result := make([]int, 0, len(preferred))
	for _, want := range preferred {
		if idx, ok := indexByValue[want]; ok {
			result = append(result, idx)
		}
	}
	if len(result) == 0 && fallbackFirst && len(options) > 0 {
		return []int{0}
	}
	return result
}

type InitOptions struct {
	OutputPath              string
	Assets                  []string
	EnableSpot              bool
	EnableOptions           bool
	EnablePerps             bool
	OptionPlatforms         []string
	PerpsMode               string
	SpotStrategies          []string
	IncludePairs            bool
	OptStrategies           []string
	PerpsStrategies         []string
	SpotCapital             float64
	OptionsCapital          float64
	PerpsCapital            float64
	PerpsLeverage           float64
	PerpsSizingLeverage     float64
	PerpsRiskPerTradePct    float64
	HLStopLossPct           *float64
	HLStopLossMarginPct     *float64
	HLTrailingStopPct       *float64
	SpotDrawdown            float64
	OptionsDrawdown         float64
	PerpsDrawdown           float64
	EnableFutures           bool
	FuturesMode             string
	FuturesStrategies       []string
	FuturesSymbols          []string
	FuturesCapital          float64
	FuturesDrawdown         float64
	FuturesFeePerContract   float64
	EnableLuno              bool
	LunoStrategies          []string
	LunoCapital             float64
	LunoDrawdown            float64
	EnableRobinhood         bool
	RobinhoodMode           string
	RobinhoodStrategies     []string
	RobinhoodCapital        float64
	RobinhoodDrawdown       float64
	RobinhoodOptionsSymbols []string
	EnableOKX               bool
	OKXMode                 string
	OKXSpotStrategies       []string
	OKXPerpsStrategies      []string
	OKXCapital              float64
	OKXDrawdown             float64
	CapitalPct              float64 `json:"capitalPct,omitempty"`
	HTFFilter               bool
	DisableCircuitBreaker   bool   `json:"disableCircuitBreaker,omitempty"`
	ATRMethod               string `json:"atrMethod,omitempty"`

	CBDrawdownCooldownMinutes   int `json:"cbDrawdownCooldownMinutes,omitempty"`
	CBLossStreakThreshold       int `json:"cbLossStreakThreshold,omitempty"`
	CBLossStreakCooldownMinutes int `json:"cbLossStreakCooldownMinutes,omitempty"`

	PortfolioMaxDrawdownPct   float64 `json:"portfolioMaxDrawdownPct,omitempty"`
	PortfolioWarnThresholdPct float64 `json:"portfolioWarnThresholdPct,omitempty"`
	DiscordEnabled            bool
	DiscordOwnerID            string
	SpotChannelID             string
	OptionsChannelID          string
	ChannelMap                map[string]string
	TelegramEnabled           bool
	TelegramOwnerChatID       string
	TelegramChannelMap        map[string]string
	AutoUpdate                string

	EnableManual    bool
	ManualSymbol    string
	ManualTimeframe string
	ManualCapital   float64
	ManualDrawdown  float64
	ManualLeverage  float64
}

func generateConfig(opts InitOptions) *Config {
	portfolioMaxDD := opts.PortfolioMaxDrawdownPct
	if portfolioMaxDD <= 0 {
		portfolioMaxDD = 25
	}
	portfolioWarn := opts.PortfolioWarnThresholdPct
	if portfolioWarn <= 0 {
		portfolioWarn = 60
	}
	defaultStopLossATRMult := DefaultStopLossATRMult
	cfg := &Config{
		ConfigVersion:          CurrentConfigVersion,
		IntervalSeconds:        3600,
		LogDir:                 "logs",
		DBFile:                 "scheduler/state.db",
		DefaultStopLossATRMult: &defaultStopLossATRMult,
		PortfolioRisk: &PortfolioRiskConfig{
			MaxDrawdownPct:   portfolioMaxDD,
			MaxNotionalUSD:   0,
			WarnThresholdPct: portfolioWarn,
		},
		Discord: DiscordConfig{
			Enabled:  opts.DiscordEnabled,
			OwnerID:  opts.DiscordOwnerID,
			Channels: opts.ChannelMap,
		},
		Telegram: TelegramConfig{
			Enabled:     opts.TelegramEnabled,
			OwnerChatID: opts.TelegramOwnerChatID,
			Channels:    opts.TelegramChannelMap,
		},
		AutoUpdate: opts.AutoUpdate,
		ATRMethod:  normalizeATRMethod(opts.ATRMethod),
	}

	assetSymbol := make(map[string]string)
	for _, a := range supportedAssets {
		assetSymbol[a.Name] = a.Symbol
	}

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

	if opts.EnableOptions {
		for _, stratID := range opts.OptStrategies {
			shortName := deriveShortName(stratID)
			for _, platform := range opts.OptionPlatforms {

				var symbols []string
				if platform == "robinhood" {
					symbols = opts.RobinhoodOptionsSymbols
					if len(symbols) == 0 {
						symbols = []string{"SPY", "QQQ"}
					}
				} else {
					for _, a := range opts.Assets {
						if a != "SOL" {
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

	if opts.EnablePerps {
		perpsLeverage := opts.PerpsLeverage
		if perpsLeverage <= 0 {
			perpsLeverage = 1
		}
		perpsSizingLeverage := opts.PerpsSizingLeverage
		if perpsSizingLeverage <= 0 {
			perpsSizingLeverage = perpsLeverage
		}

		var perpsRiskPerTradePct *float64
		if opts.PerpsRiskPerTradePct > 0 {
			v := opts.PerpsRiskPerTradePct
			perpsRiskPerTradePct = &v
			perpsSizingLeverage = 0
		}
		for _, stratID := range opts.PerpsStrategies {
			shortName := deriveShortName(stratID)

			direction := DirectionLong
			if isBidirectionalPerpsStrategy(stratID) {
				direction = DirectionBoth
			}
			for _, assetName := range opts.Assets {
				id := fmt.Sprintf("hl-%s-%s", shortName, strings.ToLower(assetName))
				cfg.Strategies = append(cfg.Strategies, StrategyConfig{
					ID:                id,
					Type:              "perps",
					Platform:          "hyperliquid",
					Script:            "shared_scripts/check_hyperliquid.py",
					Args:              []string{stratID, assetName, "1h", fmt.Sprintf("--mode=%s", opts.PerpsMode)},
					Capital:           opts.PerpsCapital,
					MaxDrawdownPct:    opts.PerpsDrawdown,
					IntervalSeconds:   3600,
					Leverage:          perpsLeverage,
					SizingLeverage:    perpsSizingLeverage,
					RiskPerTradePct:   perpsRiskPerTradePct,
					Direction:         direction,
					StopLossPct:       opts.HLStopLossPct,
					StopLossMarginPct: opts.HLStopLossMarginPct,
					TrailingStopPct:   opts.HLTrailingStopPct,
					MarginMode:        "isolated",
				})
			}
		}
	}

	if opts.EnableFutures {
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

	if opts.EnableLuno {
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

	if opts.EnableRobinhood {
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

	if opts.EnableOKX {
		okxMode := opts.OKXMode
		if okxMode == "" {
			okxMode = "paper"
		}

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

		okxPerpsLeverage := opts.PerpsLeverage
		if okxPerpsLeverage <= 0 {
			okxPerpsLeverage = 1
		}
		okxPerpsSizingLeverage := opts.PerpsSizingLeverage
		if okxPerpsSizingLeverage <= 0 {
			okxPerpsSizingLeverage = okxPerpsLeverage
		}
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
					Leverage:        okxPerpsLeverage,
					SizingLeverage:  okxPerpsSizingLeverage,
				})
			}
		}
	}

	if opts.EnableManual && opts.ManualSymbol != "" {
		tf := opts.ManualTimeframe
		if tf == "" {
			tf = "1h"
		}
		lev := opts.ManualLeverage
		if lev <= 0 {
			lev = 20
		}
		dd := opts.ManualDrawdown
		if dd <= 0 {
			dd = 20
		}
		id := fmt.Sprintf("hl-manual-%s-live", strings.ToLower(opts.ManualSymbol))
		cfg.Strategies = append(cfg.Strategies, StrategyConfig{
			ID:             id,
			Type:           "manual",
			Platform:       "hyperliquid",
			Symbol:         opts.ManualSymbol,
			Timeframe:      tf,
			Capital:        opts.ManualCapital,
			MaxDrawdownPct: dd,
			Leverage:       lev,
		})
	}

	if opts.HTFFilter {
		for i := range cfg.Strategies {
			if cfg.Strategies[i].Type != "options" && (len(cfg.Strategies[i].Args) == 0 || cfg.Strategies[i].Args[0] != "delta_neutral_funding") {
				cfg.Strategies[i].HTFFilter = true
			}
		}
	}

	if opts.DisableCircuitBreaker {
		cbOff := false
		for i := range cfg.Strategies {
			if cfg.Strategies[i].Type == "manual" {
				continue
			}
			cfg.Strategies[i].CircuitBreaker = &cbOff
		}
	}

	stampCBOverride := func(v int, set func(sc *StrategyConfig, p *int)) {
		if v <= 0 {
			return
		}
		for i := range cfg.Strategies {
			if cfg.Strategies[i].Type == "manual" {
				continue
			}
			val := v
			set(&cfg.Strategies[i], &val)
		}
	}
	stampCBOverride(opts.CBDrawdownCooldownMinutes, func(sc *StrategyConfig, p *int) { sc.CBDrawdownCooldownMinutes = p })
	stampCBOverride(opts.CBLossStreakThreshold, func(sc *StrategyConfig, p *int) { sc.CBLossStreakThreshold = p })
	stampCBOverride(opts.CBLossStreakCooldownMinutes, func(sc *StrategyConfig, p *int) { sc.CBLossStreakCooldownMinutes = p })

	if opts.CapitalPct > 0 {
		for i := range cfg.Strategies {
			cfg.Strategies[i].CapitalPct = opts.CapitalPct
		}
	}

	needsCompositeRangingRegime := false
	for i := range cfg.Strategies {
		sc := &cfg.Strategies[i]
		if sc.Type == "options" || len(sc.Args) == 0 || len(sc.AllowedRegimes) > 0 {
			continue
		}
		if gate := defaultCompositeRangingGate(sc.Args[0]); gate != nil {
			sc.AllowedRegimes = gate

			sc.RegimeGateOnFailure = RegimeGateOnFailureClosed
			needsCompositeRangingRegime = true
		}
	}
	if needsCompositeRangingRegime && cfg.Regime == nil {
		cfg.Regime = &RegimeConfig{
			Enabled:      true,
			Period:       14,
			ADXThreshold: 20.0,
			Windows: RegimeWindowsMap{
				"medium": {Classifier: regimeClassifierComposite, Period: 20},
			},
		}
	}

	return cfg
}

func stratShortName(strats []stratDef, stratID string) string {
	for _, s := range strats {
		if s.ID == stratID {
			return s.ShortName
		}
	}
	return stratID
}

func makePairs(assets []string) [][2]string {
	var pairs [][2]string
	for i := 0; i < len(assets); i++ {
		for j := i + 1; j < len(assets); j++ {
			pairs = append(pairs, [2]string{assets[i], assets[j]})
		}
	}
	return pairs
}

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

	applyMinimalStarterDefaults(&opts)

	if len(opts.Assets) == 0 {
		fmt.Fprintln(os.Stderr, "Error: at least one asset required")
		return 1
	}
	if !hasAnyEnabledStrategyType(opts) {
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

	if opts.EnablePerps && opts.PerpsLeverage <= 0 {
		opts.PerpsLeverage = 1
	}

	if !validATRMethodValue(opts.ATRMethod) {
		fmt.Fprintf(os.Stderr, "Error: atrMethod must be %q or %q, got %q\n", ATRMethodSimple, ATRMethodWilder, opts.ATRMethod)
		return 1
	}

	if opts.EnablePerps && opts.PerpsRiskPerTradePct > 0 {
		if opts.PerpsSizingLeverage > 0 {
			fmt.Fprintln(os.Stderr, "Error: perpsRiskPerTradePct and perpsSizingLeverage are mutually exclusive — pick one sizing mode")
			return 1
		}
		if opts.PerpsRiskPerTradePct > 10 {
			fmt.Fprintf(os.Stderr, "Error: perpsRiskPerTradePct must be in (0, 10], got %g\n", opts.PerpsRiskPerTradePct)
			return 1
		}
	} else if opts.EnablePerps && opts.PerpsSizingLeverage <= 0 {
		opts.PerpsSizingLeverage = opts.PerpsLeverage
	}

	if opts.EnablePerps && len(opts.PerpsStrategies) == 0 {
		for _, s := range perpsStrategies {
			opts.PerpsStrategies = append(opts.PerpsStrategies, s.ID)
		}
	}

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

	if opts.EnableOptions {
		for _, plt := range opts.OptionPlatforms {
			if plt == "robinhood" && len(opts.RobinhoodOptionsSymbols) == 0 {
				opts.RobinhoodOptionsSymbols = []string{"SPY", "QQQ"}
			}
		}
	}

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

	outputPath := p.String("Output config path", "scheduler/config.json")
	if _, err := os.Stat(outputPath); err == nil {
		if !p.YesNo(fmt.Sprintf("  %s already exists. Overwrite?", outputPath), false) {
			fmt.Println("Aborted.")
			return 0
		}
	}

	assetNames := make([]string, len(supportedAssets))
	for i, a := range supportedAssets {
		assetNames[i] = a.Name
	}
	assetIdxs := p.MultiSelectWithDefaults("\nSelect assets to trade:", assetNames, selectionDefaults(assetNames, []string{starterAssetName}, true))
	if len(assetIdxs) == 0 {
		fmt.Println("No assets selected. Aborted.")
		return 1
	}
	selectedAssets := make([]string, len(assetIdxs))
	for i, idx := range assetIdxs {
		selectedAssets[i] = supportedAssets[idx].Name
	}

	stratTypeNames := []string{"spot", "options", "perps", "futures", "robinhood", "luno", "okx"}
	stratTypeIdxs := p.MultiSelectWithDefaults("\nSelect strategy types:", stratTypeNames, selectionDefaults(stratTypeNames, []string{"spot"}, true))
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

	var robinhoodOptionsSymbols []string
	for _, plt := range optionPlatforms {
		if plt == "robinhood" {
			symIdxs := p.MultiSelect("\nSelect stock symbols for Robinhood options:", supportedStockSymbols, false)
			for _, idx := range symIdxs {
				robinhoodOptionsSymbols = append(robinhoodOptionsSymbols, supportedStockSymbols[idx])
			}
			if len(robinhoodOptionsSymbols) == 0 {
				robinhoodOptionsSymbols = []string{"SPY", "QQQ"}
			}
			break
		}
	}

	perpsMode := "paper"
	if enablePerps {
		modeOptions := []string{"paper (safe default)", "live (requires HYPERLIQUID_SECRET_KEY)"}
		if p.Choice("\nPerps trading mode:", modeOptions, 0) == 1 {
			perpsMode = "live"
		}
	}

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
			futuresSymbols = []string{"ES", "MES"}
		}
	}

	robinhoodMode := "paper"
	if enableRobinhood {
		modeOptions := []string{"paper (safe default — signal only, no orders)", "live (requires ROBINHOOD_USERNAME/PASSWORD/TOTP_SECRET)"}
		if p.Choice("\nRobinhood trading mode:", modeOptions, 0) == 1 {
			robinhoodMode = "live"
		}
	}

	okxMode := "paper"
	if enableOKX {
		modeOptions := []string{"paper (safe default)", "live (requires OKX_API_KEY/API_SECRET/PASSPHRASE)"}
		if p.Choice("\nOKX trading mode:", modeOptions, 0) == 1 {
			okxMode = "live"
		}
	}

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
		spotIdxs := p.MultiSelectWithDefaults("\nSelect spot strategies:", spotNames, selectionDefaults(spotNames, []string{starterSpotStrategyID}, true))
		for _, idx := range spotIdxs {
			if hasPairsOption && idx == len(spotStrategies) {
				includePairs = true
			} else if idx < len(spotStrategies) {
				selectedSpotStrats = append(selectedSpotStrats, spotStrategies[idx].ID)
			}
		}
	}

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

	spotCapital := 1000.0
	optionsCapital := 5000.0
	perpsCapital := 1000.0
	spotDrawdown := 5.0
	optionsDrawdown := 10.0
	perpsDrawdown := 5.0
	perpsLeverage := 1.0
	var hlStopLossPct *float64
	var hlStopLossMarginPct *float64
	var hlTrailingStopPct *float64
	robinhoodCapital := 500.0
	robinhoodDrawdown := 5.0
	lunoCapital := 500.0
	lunoDrawdown := 5.0
	futuresCapital := 5000.0
	futuresDrawdown := 5.0
	futuresFeePerContract := 1.50
	okxCapital := 1000.0
	okxDrawdown := 5.0

	portfolioMaxDD := 25.0
	portfolioWarnPct := 60.0

	anyLive := perpsMode == "live" || futuresMode == "live" || robinhoodMode == "live" || okxMode == "live"
	if anyLive {
		fmt.Println("\n--- Risk settings (live trading) ---")
		fmt.Println("These guard real capital. Press Enter to accept defaults.")

		perStrategyDD := p.FloatRange("Per-strategy max drawdown % (applied to all strategies)", 5, 0, 100)

		spotDrawdown = perStrategyDD
		optionsDrawdown = perStrategyDD
		perpsDrawdown = perStrategyDD
		robinhoodDrawdown = perStrategyDD
		lunoDrawdown = perStrategyDD
		futuresDrawdown = perStrategyDD
		okxDrawdown = perStrategyDD

		portfolioMaxDD = p.FloatRange("Portfolio kill-switch max drawdown %", 25, 0, 100)
		portfolioWarnPct = p.FloatRange("Portfolio warn threshold % (of kill switch)", 60, 0, 100)
	}

	if enablePerps {
		slOptions := []string{
			"Auto (derive from per-strategy max_drawdown_pct)",
			"Price % from entry (e.g. 1.0 = trigger on 1% adverse move)",
			"% of deployed margin (leverage-aware; e.g. 20 = trigger on 20% margin loss)",
			"Trailing % from best mark (e.g. long entry 100, mark 110, 3% trail -> SL 106.70)",
			"Explicitly disabled (no exchange-side stop-loss)",
		}
		switch p.Choice("HL perps per-trade stop-loss framing", slOptions, 0) {
		case 1:
			v := p.FloatRange("HL perps per-trade stop-loss % from entry", 1, 0, 50)
			hlStopLossPct = &v
		case 2:
			v := p.FloatRange("HL perps per-trade stop-loss % of deployed margin", 20, 0, 100)
			hlStopLossMarginPct = &v
		case 3:
			v := p.FloatRange("HL perps trailing stop distance % from best mark", 3, 0, 50)
			hlTrailingStopPct = &v
		case 4:
			zero := 0.0
			hlStopLossPct = &zero
		}
	}

	enableManual := false
	manualSymbol := ""
	manualTimeframe := "1h"
	manualCapital := 1000.0
	manualDrawdown := 20.0
	manualLeverage := 20.0
	if p.YesNo("Do you plan to do any manual trading on Hyperliquid?", false) {
		enableManual = true
		manualSymbol = strings.TrimSpace(p.String("Symbol for manual trades (e.g. ETH)", "ETH"))
		manualTimeframe = strings.TrimSpace(p.String("Timeframe for TP evaluation", "1h"))
		manualCapital = p.FloatRange("Capital budget (USD)", 1000, 1, 1e9)
		manualLeverage = p.FloatRange("Leverage", 20, 1, 100)
		manualDrawdown = p.FloatRange("Max drawdown %", 20, 1, 100)
	}

	discordEnabled := false
	channelMap := make(map[string]string)
	discordOwnerID := ""
	telegramEnabled := false
	telegramChannelMap := make(map[string]string)
	telegramOwnerChatID := ""

	autoUpdate := "off"
	htfFilter := true

	perpsStratIDs := make([]string, len(perpsStrategies))
	for i, s := range perpsStrategies {
		perpsStratIDs[i] = s.ID
	}

	futuresStratIDs := selectedFuturesStrats
	if enableFutures && len(futuresStratIDs) == 0 {
		for _, s := range futuresStrategies {
			futuresStratIDs = append(futuresStratIDs, s.ID)
		}
	}

	robinhoodStratIDs := make([]string, 0)
	if enableRobinhood {
		for _, s := range spotStrategies {
			robinhoodStratIDs = append(robinhoodStratIDs, s.ID)
		}
	}

	lunoStratIDs := selectedLunoStrats
	if enableLuno && len(lunoStratIDs) == 0 {
		for _, s := range spotStrategies {
			lunoStratIDs = append(lunoStratIDs, s.ID)
		}
	}

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
		OutputPath:                outputPath,
		Assets:                    selectedAssets,
		EnableSpot:                enableSpot,
		EnableOptions:             enableOptions,
		EnablePerps:               enablePerps,
		OptionPlatforms:           optionPlatforms,
		PerpsMode:                 perpsMode,
		SpotStrategies:            selectedSpotStrats,
		IncludePairs:              includePairs,
		OptStrategies:             selectedOptStrats,
		PerpsStrategies:           perpsStratIDs,
		SpotCapital:               spotCapital,
		OptionsCapital:            optionsCapital,
		PerpsCapital:              perpsCapital,
		PerpsLeverage:             perpsLeverage,
		HLStopLossPct:             hlStopLossPct,
		HLStopLossMarginPct:       hlStopLossMarginPct,
		HLTrailingStopPct:         hlTrailingStopPct,
		SpotDrawdown:              spotDrawdown,
		OptionsDrawdown:           optionsDrawdown,
		PerpsDrawdown:             perpsDrawdown,
		RobinhoodOptionsSymbols:   robinhoodOptionsSymbols,
		EnableRobinhood:           enableRobinhood,
		RobinhoodMode:             robinhoodMode,
		RobinhoodStrategies:       robinhoodStratIDs,
		RobinhoodCapital:          robinhoodCapital,
		RobinhoodDrawdown:         robinhoodDrawdown,
		EnableLuno:                enableLuno,
		LunoStrategies:            lunoStratIDs,
		LunoCapital:               lunoCapital,
		LunoDrawdown:              lunoDrawdown,
		EnableFutures:             enableFutures,
		FuturesMode:               futuresMode,
		FuturesStrategies:         futuresStratIDs,
		FuturesSymbols:            futuresSymbols,
		FuturesCapital:            futuresCapital,
		FuturesDrawdown:           futuresDrawdown,
		FuturesFeePerContract:     futuresFeePerContract,
		EnableOKX:                 enableOKX,
		OKXMode:                   okxMode,
		OKXSpotStrategies:         okxSpotStratIDs,
		OKXPerpsStrategies:        okxPerpsStratIDs,
		OKXCapital:                okxCapital,
		OKXDrawdown:               okxDrawdown,
		HTFFilter:                 htfFilter,
		EnableManual:              enableManual,
		ManualSymbol:              manualSymbol,
		ManualTimeframe:           manualTimeframe,
		ManualCapital:             manualCapital,
		ManualDrawdown:            manualDrawdown,
		ManualLeverage:            manualLeverage,
		PortfolioMaxDrawdownPct:   portfolioMaxDD,
		PortfolioWarnThresholdPct: portfolioWarnPct,
		DiscordEnabled:            discordEnabled,
		DiscordOwnerID:            discordOwnerID,
		ChannelMap:                channelMap,
		TelegramEnabled:           telegramEnabled,
		TelegramOwnerChatID:       telegramOwnerChatID,
		TelegramChannelMap:        telegramChannelMap,
		AutoUpdate:                autoUpdate,
	}

	cfg := generateConfig(opts)

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
	if err := os.WriteFile(outputPath, data, 0600); err != nil {
		fmt.Fprintf(os.Stderr, "Error writing %s: %v\n", outputPath, err)
		return 1
	}

	fmt.Printf("\nConfig written to %s\n", outputPath)
	fmt.Println("Next steps:")
	fmt.Println("  To enable Discord/Telegram notifications, edit the config or ask OpenClaw.")
	fmt.Printf("  ./go-trader --config %s --once\n", outputPath)
	return 0
}
