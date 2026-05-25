package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadStrategyExplicitKeysCapturesPresence(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "config.json")
	body := `{
		"strategies": [
			{
				"id": "hl-rmc-eth-live",
				"type": "perps",
				"platform": "hyperliquid",
				"script": "shared_scripts/check_hyperliquid.py",
				"args": ["range_mean_revert", "ETH", "1h"],
				"close_strategies": [{"name": "tiered_tp_atr"}],
				"capital": 1000,
				"leverage": 5
			},
			{
				"id": "hl-tema-eth-live",
				"type": "perps",
				"platform": "hyperliquid",
				"script": "shared_scripts/check_hyperliquid.py",
				"args": ["triple_ema", "ETH", "1h"],
				"capital": 1000
			}
		]
	}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	keys, err := loadStrategyExplicitKeys(path)
	if err != nil {
		t.Fatal(err)
	}
	if !keys["hl-rmc-eth-live"]["close_strategies"] {
		t.Errorf("expected close_strategies marked explicit on hl-rmc-eth-live")
	}
	if !keys["hl-rmc-eth-live"]["leverage"] {
		t.Errorf("expected leverage marked explicit on hl-rmc-eth-live")
	}
	if keys["hl-rmc-eth-live"]["stop_loss_atr_mult"] {
		t.Errorf("stop_loss_atr_mult should not be marked explicit when omitted")
	}
	if keys["hl-tema-eth-live"]["close_strategies"] {
		t.Errorf("close_strategies should not be explicit when omitted")
	}
}

func TestResolveStopLossPicksHighestPriorityField(t *testing.T) {
	mult := 1.5
	sc := StrategyConfig{
		Type:            "perps",
		Platform:        "hyperliquid",
		StopLossATRMult: &mult,
		Leverage:        5,
	}
	res := resolveStopLoss(sc, map[string]bool{"stop_loss_atr_mult": true})
	if res.Source != "stop_loss_atr_mult" {
		t.Errorf("source = %q, want stop_loss_atr_mult", res.Source)
	}
	if !res.Explicit {
		t.Errorf("explicit should be true")
	}
	if !strings.Contains(res.Value, "1.5× ATR") {
		t.Errorf("value missing mult: %q", res.Value)
	}
}

func TestResolveStopLossTrailingATRWinsOverFixed(t *testing.T) {
	fixed := 1.5
	trail := 2.0
	sc := StrategyConfig{
		Type:                "perps",
		Platform:            "hyperliquid",
		StopLossATRMult:     &fixed,
		TrailingStopATRMult: &trail,
	}
	res := resolveStopLoss(sc, nil)
	if res.Source != "trailing_stop_atr_mult" {
		t.Errorf("source = %q, want trailing_stop_atr_mult (higher priority)", res.Source)
	}
}

func TestResolveStopLossMarginPctDerivesPriceFromLeverage(t *testing.T) {
	m := 10.0
	sc := StrategyConfig{
		Type:              "perps",
		Platform:          "hyperliquid",
		StopLossMarginPct: &m,
		Leverage:          5,
	}
	res := resolveStopLoss(sc, map[string]bool{"stop_loss_margin_pct": true})
	if res.Source != "stop_loss_margin_pct" {
		t.Errorf("source = %q", res.Source)
	}
	if res.PriceTag != 2.0 {
		t.Errorf("price tag = %g, want 2.0 (10/5)", res.PriceTag)
	}
}

func TestResolveStopLossNonHLReturnsNA(t *testing.T) {
	sc := StrategyConfig{Type: "spot", Platform: "binanceus"}
	res := resolveStopLoss(sc, nil)
	if res.Source != "n/a" {
		t.Errorf("source = %q, want n/a for non-HL", res.Source)
	}
}

func TestResolveTPMatchesFirstTieredCloseRef(t *testing.T) {
	sc := StrategyConfig{
		Platform: "hyperliquid",
		Type:     "perps",
		CloseStrategies: []StrategyRef{
			{Name: "trailing_stop_atr"}, // unrelated
			{Name: "tiered_tp_atr", Params: map[string]interface{}{
				"tiers": []interface{}{
					map[string]interface{}{"atr_multiple": 1.0, "close_fraction": 0.5},
					map[string]interface{}{"atr_multiple": 2.0, "close_fraction": 1.0},
				},
			}},
		},
	}
	res := resolveTP(sc, nil)
	if !res.OK {
		t.Fatal("expected TP resolution to succeed")
	}
	if res.CloseIndex != 1 || res.CloseName != "tiered_tp_atr" {
		t.Errorf("index=%d name=%q", res.CloseIndex, res.CloseName)
	}
	if !strings.Contains(res.TiersFrom, "explicit") {
		t.Errorf("tiers source = %q, want explicit", res.TiersFrom)
	}
	if len(res.Tiers) != 2 {
		t.Errorf("len(tiers) = %d, want 2", len(res.Tiers))
	}
}

func TestResolveTPNoTieredCloseRefReturnsNotOK(t *testing.T) {
	sc := StrategyConfig{
		Platform:        "hyperliquid",
		Type:            "perps",
		CloseStrategies: []StrategyRef{{Name: "trailing_stop_atr"}},
	}
	res := resolveTP(sc, nil)
	if res.OK {
		t.Errorf("expected TP resolution to be missing")
	}
}

func TestFormatStrategyInspectionShowsResolvedTPSource(t *testing.T) {
	sc := StrategyConfig{
		ID:           "hl-rmc-eth-live",
		Type:         "perps",
		Platform:     "hyperliquid",
		Script:       "shared_scripts/check_hyperliquid.py",
		Args:         []string{"range_mean_revert", "ETH", "1h"},
		OpenStrategy: StrategyRef{Name: "range_mean_revert"},
		CloseStrategies: []StrategyRef{
			{Name: "tiered_tp_atr"},
		},
		Capital:        1000,
		Leverage:       5,
		SizingLeverage: 5,
		MarginMode:     "isolated",
		MaxDrawdownPct: 50,
	}
	mult := 1.5
	sc.StopLossATRMult = &mult
	explicit := map[string]bool{
		"id": true, "type": true, "platform": true, "script": true, "args": true,
		"open_strategy": true, "close_strategies": true,
		"capital": true, "leverage": true, "sizing_leverage": true,
		"margin_mode": true, "max_drawdown_pct": true, "stop_loss_atr_mult": true,
	}
	out := formatStrategyInspection(sc, explicit, &Config{IntervalSeconds: 600}, nil)

	for _, want := range []string{
		"strategy hl-rmc-eth-live",
		"close_strategies:    [tiered_tp_atr]",
		"stop_loss:",
		"source:            stop_loss_atr_mult (explicit)",
		"take_profit:",
		"source:            close_strategies[0] tiered_tp_atr",
		"tiers:             [1× ATR @ 50%, 2× ATR @ 100%]",
		"default (canonical [1×@50%, 2×@100%])",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q.\nfull:\n%s", want, out)
		}
	}
	// Verify default markers do NOT appear for explicit fields.
	if strings.Contains(out, "platform:            hyperliquid (default)") {
		t.Errorf("platform was explicit but rendered as default")
	}
}

func TestFormatStrategyInspectionMarksDefaultedFields(t *testing.T) {
	mult := 1.0 // applied by LoadConfig default
	sc := StrategyConfig{
		ID:              "hl-tema-eth-live",
		Type:            "perps",
		Platform:        "hyperliquid",
		Script:          "shared_scripts/check_hyperliquid.py",
		Args:            []string{"triple_ema", "ETH", "1h"},
		OpenStrategy:    StrategyRef{Name: "triple_ema"},
		Leverage:        1, // LoadConfig default
		SizingLeverage:  1, // inherited from leverage
		MarginMode:      "isolated",
		MaxDrawdownPct:  50,
		StopLossATRMult: &mult,
	}
	// Operator only set id/type/platform/script/args/open_strategy.
	explicit := map[string]bool{"id": true, "type": true, "platform": true, "script": true, "args": true, "open_strategy": true}
	out := formatStrategyInspection(sc, explicit, &Config{IntervalSeconds: 600}, nil)

	if !strings.Contains(out, "stop_loss_atr_mult (default)") {
		t.Errorf("SL default marker missing.\nfull:\n%s", out)
	}
	if !strings.Contains(out, "leverage:            1 (default)") {
		t.Errorf("leverage default marker missing.\nfull:\n%s", out)
	}
	if !strings.Contains(out, "sizing_leverage:     1 (default)") {
		t.Errorf("sizing_leverage default marker missing.\nfull:\n%s", out)
	}
	if !strings.Contains(out, "margin_mode:         isolated (default)") {
		t.Errorf("margin_mode default marker missing.\nfull:\n%s", out)
	}
}

func TestFormatStrategySummaryLineCompressesEverything(t *testing.T) {
	mult := 1.5
	sc := StrategyConfig{
		ID:              "hl-rmc-eth-live",
		Type:            "perps",
		Platform:        "hyperliquid",
		OpenStrategy:    StrategyRef{Name: "range_mean_revert"},
		CloseStrategies: []StrategyRef{{Name: "tiered_tp_atr"}},
		StopLossATRMult: &mult,
	}
	line := formatStrategySummaryLine(sc, map[string]bool{"stop_loss_atr_mult": true})
	for _, want := range []string{
		"[config] hl-rmc-eth-live:",
		"type=perps",
		"open=range_mean_revert",
		"close=[tiered_tp_atr]",
		"sl=stop_loss_atr_mult (explicit)",
		"tp=tiered_tp_atr[2-tier]",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("summary line missing %q: %s", want, line)
		}
	}
}

func TestFormatStrategySummaryLineSpotOmitsHLFields(t *testing.T) {
	sc := StrategyConfig{
		ID:           "momentum-btc",
		Type:         "spot",
		Platform:     "binanceus",
		OpenStrategy: StrategyRef{Name: "momentum"},
	}
	line := formatStrategySummaryLine(sc, nil)
	if strings.Contains(line, "sl=") || strings.Contains(line, "tp=") {
		t.Errorf("spot summary should not include sl/tp fields: %s", line)
	}
	if !strings.Contains(line, "close=open-as-close") {
		t.Errorf("spot summary should show open-as-close: %s", line)
	}
}

func TestBuildStrategyInspectionJSONStableShape(t *testing.T) {
	mult := 1.5
	sc := StrategyConfig{
		ID:              "hl-rmc-eth-live",
		Type:            "perps",
		Platform:        "hyperliquid",
		OpenStrategy:    StrategyRef{Name: "range_mean_revert"},
		CloseStrategies: []StrategyRef{{Name: "tiered_tp_atr"}},
		Leverage:        5,
		SizingLeverage:  5,
		MarginMode:      "isolated",
		MaxDrawdownPct:  50,
		StopLossATRMult: &mult,
	}
	out := buildStrategyInspectionJSON(sc, map[string]bool{
		"open_strategy": true, "close_strategies": true, "stop_loss_atr_mult": true,
	}, &Config{IntervalSeconds: 600}, nil)

	bs, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	s := string(bs)
	if !strings.Contains(s, `"id":"hl-rmc-eth-live"`) {
		t.Errorf("json missing id: %s", s)
	}
	sl, ok := out["stop_loss"].(map[string]interface{})
	if !ok {
		t.Fatalf("stop_loss not a map: %v", out["stop_loss"])
	}
	if sl["source"] != "stop_loss_atr_mult" {
		t.Errorf("stop_loss.source = %v", sl["source"])
	}
	if sl["explicit"] != true {
		t.Errorf("stop_loss.explicit = %v", sl["explicit"])
	}
	tp, ok := out["take_profit"].(map[string]interface{})
	if !ok {
		t.Fatalf("take_profit not a map: %v", out["take_profit"])
	}
	if tp["configured"] != true {
		t.Errorf("take_profit.configured = %v", tp["configured"])
	}
}

func TestResolveStopLossRegimeFixedInspectDetail(t *testing.T) {
	sc := StrategyConfig{
		Type:     "perps",
		Platform: "hyperliquid",
		StopLossATRRegime: &RegimeATRBlock{
			UseDefaults: true,
			TrendRegime: map[string]RegimeATREntry{
				"trending_up":   {ATR: 2.0},
				"trending_down": {ATR: 2.0},
				"ranging":       {ATR: 1.5},
			},
		},
	}
	res := resolveStopLoss(sc, map[string]bool{"stop_loss_atr_regime": true})
	if res.Source != "stop_loss_atr_regime" {
		t.Fatalf("source=%q", res.Source)
	}
	if len(res.Detail) != 1 || !strings.Contains(res.Detail[0], "use_defaults baseline") {
		t.Fatalf("unexpected detail: %v", res.Detail)
	}
	if !strings.Contains(res.Detail[0], "trend_regime") {
		t.Errorf("classifier missing: %q", res.Detail[0])
	}
}

func TestFormatStrategyInspectionRegimeTPUseDefaults(t *testing.T) {
	mult := 1.0
	sc := StrategyConfig{
		ID:              "hl-reg-tp",
		Type:            "perps",
		Platform:        "hyperliquid",
		Script:          "shared_scripts/check_hyperliquid.py",
		CloseStrategies: []StrategyRef{{Name: "tiered_tp_atr_regime", Params: map[string]interface{}{"use_defaults": true}}},
		Leverage:        3,
		StopLossATRMult: &mult,
		MaxDrawdownPct:  50,
	}
	explicit := map[string]bool{
		"id": true, "type": true, "platform": true, "script": true,
		"close_strategies": true, "leverage": true, "max_drawdown_pct": true,
	}
	out := formatStrategyInspection(sc, explicit, &Config{IntervalSeconds: 60}, nil)
	if !strings.Contains(out, "tiered_tp_atr_regime tier[0]:") {
		t.Errorf("missing tier[0] provenance line:\n%s", out)
	}
	if !strings.Contains(out, "use_defaults baseline") {
		t.Errorf("missing use_defaults provenance:\n%s", out)
	}
	if !strings.Contains(out, "tiers (example: trend_regime=trending_up):") {
		t.Errorf("missing example tier line:\n%s", out)
	}
}

func TestFormatStrategySummaryLineRegimeTPTierCount(t *testing.T) {
	mult := 1.0
	sc := StrategyConfig{
		ID:              "hl-reg-tp",
		Type:            "perps",
		Platform:        "hyperliquid",
		CloseStrategies: []StrategyRef{{Name: "tiered_tp_atr_regime", Params: map[string]interface{}{"use_defaults": true}}},
		StopLossATRMult: &mult,
	}
	line := formatStrategySummaryLine(sc, nil)
	if !strings.Contains(line, "tp=tiered_tp_atr_regime[2-tier]") {
		t.Errorf("expected 2-tier regime TP summary, got: %s", line)
	}
}
