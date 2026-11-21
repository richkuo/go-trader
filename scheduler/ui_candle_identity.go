package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"strings"
)

const (
	candleNetworkMainnet = "mainnet"
	candleNetworkTestnet = "testnet"
	candleNetworkSandbox = "sandbox"
	candleNetworkLive    = "live"

	candleClassPerps   = "perps"
	candleClassSpot    = "spot"
	candleClassCrypto  = "crypto"
	candleClassStock   = "stock"
	candleClassFutures = "futures"

	candleVenueHyperliquid = "hyperliquid"
	candleVenueOKX         = "okx"
	candleVenueRobinhood   = "robinhood"
	candleVenueTopStep     = "topstep"
)

type candleIdentity struct {
	Venue           string `json:"venue"`
	Network         string `json:"network"`
	InstrumentClass string `json:"instrument_class"`
	Symbol          string `json:"symbol"`
	Timeframe       string `json:"timeframe"`
}

func (id candleIdentity) String() string {
	return id.Venue + "|" + id.Network + "|" + id.InstrumentClass + "|" + id.Symbol + "|" + id.Timeframe
}

func (id candleIdentity) withTimeframe(tf string) candleIdentity {
	id.Timeframe = tf
	return id
}

func (id candleIdentity) series() candleIdentity {
	id.Timeframe = ""
	return id
}

func (id candleIdentity) digest() string {
	sum := sha256.Sum256([]byte(id.String()))
	return hex.EncodeToString(sum[:8])
}

func (id candleIdentity) valid() bool {
	for _, v := range []string{id.Venue, id.Network, id.InstrumentClass, id.Symbol, id.Timeframe} {
		if strings.TrimSpace(v) == "" || v != strings.TrimSpace(v) || strings.ContainsAny(v, "|\x00\n\r\t") {
			return false
		}
	}
	return true
}

var candleGridSeconds = map[string]int64{
	"1m": 60, "3m": 180, "5m": 300, "15m": 900, "30m": 1800,
	"1h": 3600, "2h": 7200, "4h": 14400, "6h": 21600, "8h": 28800, "12h": 43200,
	"1d": 86400, "3d": 259200, "1w": 604800,
}

const candleDisplayOnlyMonth = "1M"

func candleGridInterval(tf string) (int64, bool) {
	sec, ok := candleGridSeconds[tf]
	return sec, ok
}

func candleEpochAligned(tf string) bool {
	_, ok := tradeReviewIntervals[tf]
	return ok
}

func candleTimeframeKnown(tf string) bool {
	if tf == candleDisplayOnlyMonth {
		return true
	}
	_, ok := candleGridSeconds[tf]
	return ok
}

func sortCandleTimeframes(tfs []string) {
	sort.SliceStable(tfs, func(i, j int) bool {
		return candleTimeframeOrder(tfs[i]) < candleTimeframeOrder(tfs[j])
	})
}

func candleTimeframeOrder(tf string) int64 {
	if tf == candleDisplayOnlyMonth {
		return 2_678_400
	}
	if sec, ok := candleGridSeconds[tf]; ok {
		return sec
	}
	return 1 << 62
}

func candleIdentityFor(sc StrategyConfig, symbol, timeframe string) (candleIdentity, error) {
	symbol = strings.TrimSpace(symbol)
	timeframe = strings.TrimSpace(timeframe)
	if symbol == "" {
		return candleIdentity{}, fmt.Errorf("strategy %s has no symbol", sc.ID)
	}
	if timeframe == "" {
		return candleIdentity{}, fmt.Errorf("strategy %s has no timeframe", sc.ID)
	}
	id := candleIdentity{Symbol: symbol, Timeframe: timeframe}
	switch sc.Platform {
	case "hyperliquid":
		id.Venue = candleVenueHyperliquid
		id.Network = candleHyperliquidNetwork(hlInfoEndpoint())
		switch sc.Type {
		case "perps", "manual":
			id.InstrumentClass = candleClassPerps
		default:
			return candleIdentity{}, fmt.Errorf("hyperliquid type %q has no chart identity", sc.Type)
		}
	case "okx":
		id.Venue = candleVenueOKX
		id.Network = candleNetworkLive
		if os.Getenv("OKX_SANDBOX") == "1" {
			id.Network = candleNetworkSandbox
		}
		if sc.Type == "perps" {
			id.InstrumentClass = candleClassPerps
		} else {
			id.InstrumentClass = candleClassSpot
		}
	case "robinhood":
		id.Venue = candleVenueRobinhood
		id.Network = candleNetworkLive
		switch sc.Type {
		case "spot":
			id.InstrumentClass = candleClassCrypto
		case "options":
			id.InstrumentClass = candleClassStock
		default:
			return candleIdentity{}, fmt.Errorf("robinhood type %q has no chart identity", sc.Type)
		}
	case "topstep":
		id.Venue = candleVenueTopStep
		id.Network = candleNetworkLive
		id.InstrumentClass = candleClassFutures
	case "manual":
		return candleIdentity{}, fmt.Errorf("platform %q has no historical candle source", sc.Platform)
	default:
		exchange := sc.Platform
		if exchange == "" {
			exchange = "binanceus"
		}
		id.Venue = exchange
		id.Network = candleNetworkLive
		id.InstrumentClass = candleClassSpot
		if !strings.Contains(symbol, "/") && (sc.Type == "perps" || sc.Type == "manual") {
			id.Symbol = symbol + "/USDT"
		}
	}
	if !id.valid() {
		return candleIdentity{}, fmt.Errorf("strategy %s has no valid chart identity", sc.ID)
	}
	return id, nil
}

func candleHyperliquidNetwork(host string) string {
	switch strings.TrimRight(host, "/") {
	case strings.TrimRight(hlTestnetURL, "/"):
		return candleNetworkTestnet
	case strings.TrimRight(hlMainnetURL, "/"):
		return candleNetworkMainnet
	}
	return ""
}

func candleIdentityForFeedKey(key marketFeedKey) (candleIdentity, bool) {
	if key.Namespace != feedNamespacePerps {
		return candleIdentity{}, false
	}
	network := candleHyperliquidNetwork(key.Host)
	if network == "" {
		return candleIdentity{}, false
	}
	id := candleIdentity{Venue: candleVenueHyperliquid, Network: network, InstrumentClass: candleClassPerps, Symbol: key.Symbol, Timeframe: key.Timeframe}
	if !id.valid() || !candleTimeframeKnown(id.Timeframe) {
		return candleIdentity{}, false
	}
	return id, true
}
