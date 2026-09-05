package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func feedTestConfig(strategies ...StrategyConfig) *Config {
	return &Config{IntervalSeconds: 300, MarketFeed: marketFeedWebsocket, Strategies: strategies}
}

func TestHTFTableMatchesPython(t *testing.T) {
	blob, err := os.ReadFile(filepath.Join("..", "shared_tools", "testdata", "htf_map.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var spec struct {
		Map      map[string]string `json:"map"`
		Default  string            `json:"default"`
		Lookback int               `json:"lookback"`
	}
	if err := json.Unmarshal(blob, &spec); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	if len(spec.Map) != len(hlFeedHTFMap) {
		t.Fatalf("table size: Go has %d entries, the fixture has %d", len(hlFeedHTFMap), len(spec.Map))
	}
	for tf, want := range spec.Map {
		if got := hlFeedHTFTimeframe(tf); got != want {
			t.Fatalf("%s: Go maps to %q, the fixture says %q", tf, got, want)
		}
	}
	if got := hlFeedHTFTimeframe("7m"); got != spec.Default {
		t.Fatalf("default: got %q want %q", got, spec.Default)
	}
	if hlFeedHTFLookback != spec.Lookback {
		t.Fatalf("lookback: Go uses %d, the fixture says %d", hlFeedHTFLookback, spec.Lookback)
	}
}

func TestFeedRequirementsCoverEveryConsumer(t *testing.T) {
	rc := &RegimeConfig{Enabled: true, Timeframe: "4h", Period: 14, ADXThreshold: 20}
	cfg := feedTestConfig(
		StrategyConfig{
			ID: "hl-live", Type: "perps", Platform: "hyperliquid", Script: hyperliquidCheckScript,
			Args: []string{"momentum", "BTC", "1h", "--mode=live"}, HTFFilter: true,
		},
		StrategyConfig{
			ID: "hl-paper", Type: "perps", Platform: "hyperliquid", Script: hyperliquidCheckScript,
			Args: []string{"momentum", "BTC", "1h", "--mode=paper"},
		},
		StrategyConfig{
			ID: "hl-manual", Type: "manual", Platform: "hyperliquid", Script: hyperliquidCheckScript,
			Symbol: "ETH", Args: []string{"hold", "ETH", "1h", "--mode=live"},
		},
		StrategyConfig{
			ID: "hl-funding", Type: "perps", Platform: "hyperliquid", Script: hyperliquidCheckScript,
			Args: []string{"delta_neutral_funding", "SOL", "1h", "--mode=paper"},
		},
		StrategyConfig{
			ID: "hl-skew", Type: "perps", Platform: "hyperliquid", Script: hyperliquidCheckScript,
			Args: []string{"funding_skew", "SOL", "1h", "--mode=paper"},
		},
		StrategyConfig{
			ID: "okx-out-of-scope", Type: "perps", Platform: "okx", Script: "shared_scripts/check_okx.py",
			Args: []string{"momentum", "BTC", "1h", "--mode=paper"},
		},
		StrategyConfig{
			ID: "hl-hedged", Type: "perps", Platform: "hyperliquid", Script: hyperliquidCheckScript,
			Args:  []string{"momentum", "AVAX", "1h", "--mode=live"},
			Hedge: &HedgeConfig{Enabled: true, Symbol: "BTC"},
		},
	)
	cfg.Regime = rc

	req, err := deriveFeedRequirements(cfg)
	if err != nil {
		t.Fatalf("derive: %v", err)
	}

	wantKeys := map[string]int{
		"BTC|1h":  feedSignalLookback(rc),
		"BTC|4h":  hlFeedHTFLookback,
		"ETH|1h":  feedSignalLookback(rc),
		"SOL|1h":  feedSignalLookback(rc),
		"AVAX|1h": feedSignalLookback(rc),
		"ETH|4h":  regimeRequiredOhlcvLimit(rc),
		"SOL|4h":  regimeRequiredOhlcvLimit(rc),
		"AVAX|4h": regimeRequiredOhlcvLimit(rc),
	}
	got := map[string]int{}
	for key, lookback := range req.Keys {
		got[key.PayloadID()] = lookback
	}
	if _, ok := got["BTC|4h"]; !ok {
		t.Fatalf("BTC 4h must be required by both the HTF filter and the regime timeframe: %v", got)
	}
	for id, want := range wantKeys {
		have, ok := got[id]
		if !ok {
			t.Fatalf("missing key %s (got %v)", id, got)
		}
		if id == "BTC|4h" {
			if have < want {
				t.Fatalf("%s lookback %d is below the highest consumer requirement %d", id, have, want)
			}
			continue
		}
		if have != want {
			t.Fatalf("%s lookback: got %d want %d", id, have, want)
		}
	}
	for id := range got {
		if _, ok := wantKeys[id]; !ok {
			t.Fatalf("unexpected feed key %s (out-of-scope strategies must not create keys)", id)
		}
	}

	if _, ok := req.Strategies["okx-out-of-scope"]; ok {
		t.Fatalf("an OKX strategy must stay outside the feed contract")
	}
	live := req.Strategies["hl-live"]
	paper := req.Strategies["hl-paper"]
	if live.Signal != paper.Signal {
		t.Fatalf("live and paper twins must share one signal key: %s vs %s", live.Signal, paper.Signal)
	}
	if !live.HasHTF || live.HTF.Timeframe != "4h" {
		t.Fatalf("the higher-timeframe filter key is missing: %+v", live)
	}
	if paper.HasHTF {
		t.Fatalf("a strategy without htf_filter must not claim a higher-timeframe key")
	}
	if !live.HasRegime || live.Regime.Timeframe != "4h" {
		t.Fatalf("the regime timeframe override must produce its own key: %+v", live)
	}
	if req.Funding["SOL"].Scalar != true || req.Funding["SOL"].Records != true {
		t.Fatalf("SOL needs both funding shapes: %+v", req.Funding["SOL"])
	}
	if _, ok := req.Funding["BTC"]; ok {
		t.Fatalf("only funding-aware strategies request funding: %+v", req.Funding)
	}

	midSet := map[string]bool{}
	for _, coin := range req.MidCoins {
		midSet[coin] = true
	}
	for _, coin := range []string{"BTC", "ETH", "SOL", "AVAX"} {
		if !midSet[coin] {
			t.Fatalf("mid coin %s missing: %v", coin, req.MidCoins)
		}
	}
}

func TestFeedRequirementsRejectUnsupportedTimeframes(t *testing.T) {
	tests := []struct {
		name string
		cfg  *Config
	}{
		{
			name: "signal timeframe outside the venue interval set",
			cfg: feedTestConfig(StrategyConfig{
				ID: "hl-a", Type: "perps", Platform: "hyperliquid", Script: hyperliquidCheckScript,
				Args: []string{"momentum", "BTC", "90m", "--mode=paper"},
			}),
		},
		{
			name: "regime timeframe outside the venue interval set",
			cfg: func() *Config {
				c := feedTestConfig(StrategyConfig{
					ID: "hl-a", Type: "perps", Platform: "hyperliquid", Script: hyperliquidCheckScript,
					Args: []string{"momentum", "BTC", "1h", "--mode=paper"},
				})
				c.Regime = &RegimeConfig{Enabled: true, Timeframe: "1mo", Period: 14, ADXThreshold: 20}
				return c
			}(),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := deriveFeedRequirements(tc.cfg); err == nil {
				t.Fatalf("expected the config to be rejected under market_feed=websocket")
			}
			if err := validateMarketFeedConfig(tc.cfg); err == nil {
				t.Fatalf("validateMarketFeedConfig must refuse the same config")
			}
			restCfg := *tc.cfg
			restCfg.MarketFeed = ""
			if err := validateMarketFeedConfig(&restCfg); err != nil {
				t.Fatalf("rest mode must keep accepting the config: %v", err)
			}
		})
	}
}

func TestMarketFeedConfigModes(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		mode    string
		wantErr bool
	}{
		{name: "omitted defaults to rest", raw: "", mode: marketFeedREST},
		{name: "explicit rest", raw: "rest", mode: marketFeedREST},
		{name: "websocket", raw: "websocket", mode: marketFeedWebsocket},
		{name: "unknown value fails", raw: "grpc", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{MarketFeed: tc.raw}
			err := validateMarketFeedConfig(cfg)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error for market_feed=%q", tc.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got := cfg.marketFeedMode(); got != tc.mode {
				t.Fatalf("mode: got %q want %q", got, tc.mode)
			}
			if want := tc.mode == marketFeedWebsocket; cfg.marketFeedWebsocketEnabled() != want {
				t.Fatalf("websocket enabled: got %v want %v", cfg.marketFeedWebsocketEnabled(), want)
			}
		})
	}
}

func TestMarketFeedModeChangeIsRestartRequired(t *testing.T) {
	cur := &Config{MarketFeed: ""}
	next := &Config{MarketFeed: marketFeedWebsocket}
	err := validateHotReloadCompatible(cur, next)
	if err == nil {
		t.Fatalf("changing market_feed must be refused on SIGHUP")
	}
	if got := err.Error(); !strings.Contains(got, "market_feed") || !strings.Contains(got, "restart required") {
		t.Fatalf("reload refusal must name the field and the remedy: %s", got)
	}
	if err := validateHotReloadCompatible(&Config{MarketFeed: ""}, &Config{MarketFeed: marketFeedREST}); err != nil {
		t.Fatalf("an omitted value and an explicit rest value are the same mode: %v", err)
	}
}
