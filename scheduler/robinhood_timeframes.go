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

var robinhoodTimeframes = map[string]string{
	"1m":  "1m",
	"2m":  "2m",
	"5m":  "5m",
	"15m": "15m",
	"30m": "30m",
	"1h":  "1h",
	"60m": "1h",
	"90m": "90m",
	"2h":  "1h",
	"4h":  "1h",
	"6h":  "1h",
	"8h":  "1h",
	"12h": "1h",
	"1d":  "1d",
	"1w":  "1wk",
	"1M":  "1mo",
	"1mo": "1mo",
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
	yahoo, ok := robinhoodTimeframes[raw]
	if !ok {
		return false
	}
	lowered, ok := robinhoodTimeframes[strings.ToLower(raw)]
	return ok && lowered != yahoo
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
				errs = append(errs, fmt.Sprintf("strategy[%s]: regime.timeframe %q is ambiguous for robinhood: it means %s on Yahoo but go-trader lowercases it to %q (%s); write %q or %q", sc.ID, raw, robinhoodTimeframes[raw], strings.ToLower(raw), robinhoodTimeframes[strings.ToLower(raw)], robinhoodTimeframes[raw], strings.ToLower(raw)))
				ambiguousReported = true
			}
			continue
		}
		if _, tf := strategyRegimeSymbolTimeframe(sc.Args, cfg.Regime); tf != "" && !robinhoodTimeframeSupported(tf) {
			errs = append(errs, fmt.Sprintf("strategy[%s]: robinhood regime.timeframe %q is not supported: Yahoo candles serve only %s", sc.ID, tf, supported))
		}
	}
	return errs
}
