package main

import (
	"fmt"
	"path/filepath"
	"regexp"
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
	"AAVE":    "AAVE-USD",
	"ADA":     "ADA-USD",
	"ALGO":    "ALGO-USD",
	"ATOM":    "ATOM-USD",
	"AVAX":    "AVAX-USD",
	"AVNT":    "AVNT-USD",
	"AXS":     "AXS-USD",
	"BAT":     "BAT-USD",
	"BCH":     "BCH-USD",
	"BNB":     "BNB-USD",
	"BONK":    "BONK-USD",
	"BTC":     "BTC-USD",
	"CASHCAT": "CASHCAT-USD",
	"DOGE":    "DOGE-USD",
	"DOT":     "DOT-USD",
	"ENA":     "ENA-USD",
	"ETC":     "ETC-USD",
	"ETH":     "ETH-USD",
	"FLR":     "FLR-USD",
	"HBAR":    "HBAR-USD",
	"HYPE":    "HYPE32196-USD",
	"INJ":     "INJ-USD",
	"JTO":     "JTO-USD",
	"LDO":     "LDO-USD",
	"LINK":    "LINK-USD",
	"LTC":     "LTC-USD",
	"MEW":     "MEW30126-USD",
	"MORPHO":  "MORPHO34104-USD",
	"NEAR":    "NEAR-USD",
	"ONDO":    "ONDO-USD",
	"OP":      "OP-USD",
	"ORCA":    "ORCA-USD",
	"PAXG":    "PAXG-USD",
	"PEPE":    "PEPE24478-USD",
	"PNUT":    "PNUT-USD",
	"PYTH":    "PYTH-USD",
	"QNT":     "QNT-USD",
	"RAY":     "RAY-USD",
	"RE":      "RE-USD",
	"RENDER":  "RENDER-USD",
	"SEI":     "SEI-USD",
	"SHIB":    "SHIB-USD",
	"SNX":     "SNX-USD",
	"SOL":     "SOL-USD",
	"SUI":     "SUI20947-USD",
	"SYRUP":   "SYRUP-USD",
	"UNI":     "UNI7083-USD",
	"USDC":    "USDC-USD",
	"VIRTUAL": "VIRTUAL-USD",
	"W":       "W-USD",
	"WIF":     "WIF-USD",
	"WLD":     "WLD-USD",
	"WLFI":    "WLFI33251-USD",
	"XDC":     "XDC-USD",
	"XPL":     "XPL-USD",
	"XRP":     "XRP-USD",
	"XTZ":     "XTZ-USD",
	"ZEC":     "ZEC-USD",
	"ZORA":    "ZORA35931-USD",
}

const (
	robinhoodAssetClassCrypto = "crypto"
	robinhoodAssetClassStock  = "stock"
)

func robinhoodSupportedCryptoCoins() string {
	out := make([]string, 0, len(robinhoodYahooCryptoMap))
	for sym := range robinhoodYahooCryptoMap {
		out = append(out, sym)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

func robinhoodCryptoCoinSupported(symbol string) bool {
	_, ok := robinhoodYahooCryptoMap[symbol]
	return ok
}

var (
	robinhoodStockTickerRe  = regexp.MustCompile(`^[A-Z][A-Z0-9]{0,5}(?:[.-][A-Z]{1,2})?$`)
	robinhoodCurrencyPairRe = regexp.MustCompile(`^.+-(?:USD|USDT|USDC)$`)
)

func robinhoodStockUnderlyingError(raw string) string {
	text := strings.ToUpper(strings.TrimSpace(raw))
	if strings.Contains(text, "/") || robinhoodCurrencyPairRe.MatchString(text) {
		return fmt.Sprintf("robinhood options underlying %q is a currency pair: Robinhood options price only listed stock and ETF underlyings", raw)
	}
	if !robinhoodStockTickerRe.MatchString(text) {
		return fmt.Sprintf("robinhood options underlying %q is not a stock or ETF ticker (1 to 6 letters or digits, starting with a letter, with an optional .X or -X share class)", raw)
	}
	return ""
}

func robinhoodSpotCoinError(id string, args []string) string {
	if len(args) < 2 {
		return fmt.Sprintf("strategy[%s]: robinhood spot coin args[1] is missing: args must be <strategy> <coin> <timeframe>; supported coins: %s; a stock or ETF underlying runs only as a robinhood type \"options\" strategy", id, robinhoodSupportedCryptoCoins())
	}
	if robinhoodCryptoCoinSupported(args[1]) {
		return ""
	}
	return fmt.Sprintf("strategy[%s]: robinhood spot coin %q is not a supported crypto coin: write one bare coin code from the verified table (%s); a stock or ETF underlying runs only as a robinhood type \"options\" strategy", id, args[1], robinhoodSupportedCryptoCoins())
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
		if why := robinhoodSpotCoinError(sc.ID, sc.Args); why != "" {
			errs = append(errs, why)
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
				errs = append(errs, fmt.Sprintf("strategy[%s]: regime.timeframe %q is ambiguous for robinhood: it means %s on Yahoo but go-trader lowercases it to %q (%s); write %q or %q", sc.ID, raw, robinhoodTimeframes[raw].Yahoo, strings.ToLower(raw), robinhoodTimeframes[strings.ToLower(raw)].Yahoo, robinhoodTimeframes[raw].Yahoo, strings.ToLower(raw)))
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
