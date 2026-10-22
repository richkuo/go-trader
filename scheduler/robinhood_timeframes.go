package main

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

const robinhoodCheckScript = "shared_scripts/check_robinhood.py"

const robinhoodLiveClosedBarRefusal = "live Robinhood fills settle through robinhood_order_attempts and the closed-bar entry ledger has no Robinhood provider, so one entry per decision candle and side cannot be enforced; use it on a --mode=paper Robinhood strategy"

var robinhoodClosedBarRefusedTimeframes = map[string]string{
	"1m": "Yahoo's newest closed 1m row was a flat zero-volume placeholder (open = high = low = close) in 6 of 8 sampled BTC-USD and ETH-USD minutes until the next minute's row appeared, 10 to 20 seconds after the close (#1822), so the settle hold would cover a third of every interval",
}

type robinhoodTimeframe struct {
	Yahoo   string
	Session string
}

var robinhoodTimeframes = map[string]robinhoodTimeframe{
	"1m":  {"1m", "1m"},
	"2m":  {"2m", "2m"},
	"5m":  {"5m", "5m"},
	"15m": {"15m", "15m"},
	"30m": {"30m", "30m"},
	"1h":  {"1h", "1h"},
	"60m": {"1h", "1h"},
	"90m": {"90m", "90m"},
	"2h":  {"1h", ""},
	"4h":  {"1h", "4h"},
	"6h":  {"1h", ""},
	"8h":  {"1h", ""},
	"12h": {"1h", ""},
	"1d":  {"1d", "1d"},
	"1w":  {"1wk", "1wk"},
	"1M":  {"1mo", "1mo"},
	"1mo": {"1mo", "1mo"},
}

var robinhoodYahooCryptoMap = map[string]string{
	"BTC":   "BTC-USD",
	"ETH":   "ETH-USD",
	"SOL":   "SOL-USD",
	"DOGE":  "DOGE-USD",
	"AVAX":  "AVAX-USD",
	"LINK":  "LINK-USD",
	"ADA":   "ADA-USD",
	"DOT":   "DOT-USD",
	"MATIC": "MATIC-USD",
	"SHIB":  "SHIB-USD",
}

func robinhoodYahooSymbol(symbol string) string {
	upper := strings.ToUpper(symbol)
	if yahoo, ok := robinhoodYahooCryptoMap[upper]; ok {
		return yahoo
	}
	return upper
}

func robinhoodYahooCryptoPair(symbol string) bool {
	return strings.HasSuffix(robinhoodYahooSymbol(symbol), "-USD")
}

func robinhoodYahooCryptoPairs() string {
	out := make([]string, 0, len(robinhoodYahooCryptoMap))
	for sym := range robinhoodYahooCryptoMap {
		out = append(out, sym)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

func robinhoodTimeframeServesSymbol(tf, symbol string) bool {
	entry, ok := robinhoodTimeframes[tf]
	return ok && (entry.Session != "" || robinhoodYahooCryptoPair(symbol))
}

func robinhoodCryptoOnlyTimeframeError(id, role, tf, symbol string) string {
	return fmt.Sprintf("strategy[%s]: robinhood %s %q is built from Yahoo 1h rows only for crypto pairs, and symbol %q resolves to Yahoo %q, which is not one (Yahoo crypto pairs: %s, or a symbol ending in -USD)", id, role, tf, symbol, robinhoodYahooSymbol(symbol), robinhoodYahooCryptoPairs())
}

func robinhoodSessionClosedBarRefusal(symbol string) string {
	return fmt.Sprintf("symbol %q resolves to Yahoo %q, which is not a Yahoo crypto pair: its Yahoo rows follow exchange trading sessions (no rows outside session hours, and hourly and longer rows open at session times off the UTC interval grid), so the newest closed bar at the cutoff cannot be proven (Yahoo crypto pairs: %s, or a symbol ending in -USD)", symbol, robinhoodYahooSymbol(symbol), robinhoodYahooCryptoPairs())
}

func robinhoodSupportedTimeframes() string {
	out := make([]string, 0, len(robinhoodTimeframes))
	for tf := range robinhoodTimeframes {
		out = append(out, tf)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

func robinhoodTimeframeSupported(tf string) bool {
	_, ok := robinhoodTimeframes[tf]
	return ok
}

func robinhoodCandleCheckStrategy(sc StrategyConfig) bool {
	return sc.Platform == "robinhood" && sc.Type == "spot" && filepath.Clean(strings.TrimSpace(sc.Script)) == robinhoodCheckScript
}

func robinhoodRegimeTimeframeAmbiguous(raw string) bool {
	entry, ok := robinhoodTimeframes[raw]
	if !ok {
		return false
	}
	lowered, ok := robinhoodTimeframes[strings.ToLower(raw)]
	return ok && lowered.Yahoo != entry.Yahoo
}

func robinhoodTimeframeConfigErrors(cfg *Config) []string {
	if cfg == nil {
		return nil
	}
	supported := robinhoodSupportedTimeframes()
	regimeOn := cfg.Regime != nil && cfg.Regime.Enabled
	var errs []string
	ambiguousReported := false
	for _, sc := range cfg.Strategies {
		if sc.Platform != "robinhood" || sc.Type != "spot" {
			continue
		}
		if robinhoodCandleCheckStrategy(sc) {
			if len(sc.Args) < 3 {
				errs = append(errs, fmt.Sprintf("strategy[%s]: robinhood args timeframe is missing: args must be <strategy> <symbol> <timeframe>; Yahoo candles serve only %s", sc.ID, supported))
			} else if tf := sc.Args[2]; !robinhoodTimeframeSupported(tf) {
				errs = append(errs, fmt.Sprintf("strategy[%s]: robinhood args timeframe %q is not supported: Yahoo candles serve only %s", sc.ID, tf, supported))
			} else if !robinhoodTimeframeServesSymbol(tf, sc.Args[1]) {
				errs = append(errs, robinhoodCryptoOnlyTimeframeError(sc.ID, "args timeframe", tf, sc.Args[1]))
			} else if sc.HTFFilter && effectiveOpenStrategy(sc) != fundingScalarStrategyName {
				if htf := hlFeedHTFTimeframe(tf); !robinhoodTimeframeSupported(htf) {
					errs = append(errs, fmt.Sprintf("strategy[%s]: robinhood higher-timeframe filter interval %q for timeframe %q is not supported: Yahoo candles serve only %s", sc.ID, htf, tf, supported))
				}
			}
		}
		if !regimeOn {
			continue
		}
		if raw := cfg.regimeTimeframeRaw; robinhoodRegimeTimeframeAmbiguous(raw) {
			if !ambiguousReported {
				errs = append(errs, fmt.Sprintf("strategy[%s]: regime.timeframe %q is ambiguous for robinhood: it means %s on Yahoo but go-trader lowercases it to %q (%s); write %q or %q", sc.ID, raw, robinhoodTimeframes[raw].Yahoo, strings.ToLower(raw), robinhoodTimeframes[strings.ToLower(raw)].Yahoo, robinhoodTimeframes[raw].Yahoo, strings.ToLower(raw)))
				ambiguousReported = true
			}
			continue
		}
		if symbol, tf := strategyRegimeSymbolTimeframe(sc.Args, cfg.Regime); tf != "" && !robinhoodTimeframeSupported(tf) {
			errs = append(errs, fmt.Sprintf("strategy[%s]: robinhood regime.timeframe %q is not supported: Yahoo candles serve only %s", sc.ID, tf, supported))
		} else if tf != "" && !robinhoodTimeframeServesSymbol(tf, symbol) {
			errs = append(errs, robinhoodCryptoOnlyTimeframeError(sc.ID, "regime.timeframe", tf, symbol))
		}
	}
	return errs
}
