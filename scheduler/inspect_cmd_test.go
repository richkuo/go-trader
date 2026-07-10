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

func TestResolveTPMatchesTieredCloseRef(t *testing.T) {
	sc := StrategyConfig{
		Platform: "hyperliquid",
		Type:     "perps",
		CloseStrategy: &StrategyRef{Name: "tiered_tp_atr", Params: map[string]interface{}{
			"tp_tiers": []interface{}{
				map[string]interface{}{"atr_multiple": 1.0, "close_fraction": 0.5},
				map[string]interface{}{"atr_multiple": 2.0, "close_fraction": 1.0},
			},
		}},
	}
	res := resolveTP(sc, nil)
	if !res.OK {
		t.Fatal("expected TP resolution to succeed")
	}
	if res.CloseIndex != 0 || res.CloseName != "tiered_tp_atr" {
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
		Platform:      "hyperliquid",
		Type:          "perps",
		CloseStrategy: &StrategyRef{Name: "trailing_stop_atr"},
	}
	res := resolveTP(sc, nil)
	if res.OK {
		t.Errorf("expected TP resolution to be missing")
	}
}

func TestFormatStrategyInspectionShowsResolvedTPSource(t *testing.T) {
	sc := StrategyConfig{
		ID:             "hl-rmc-eth-live",
		Type:           "perps",
		Platform:       "hyperliquid",
		Script:         "shared_scripts/check_hyperliquid.py",
		Args:           []string{"range_mean_revert", "ETH", "1h"},
		OpenStrategy:   StrategyRef{Name: "range_mean_revert"},
		CloseStrategy:  &StrategyRef{Name: "tiered_tp_atr"},
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
		"open_strategy": true, "close_strategy": true,
		"capital": true, "leverage": true, "sizing_leverage": true,
		"margin_mode": true, "max_drawdown_pct": true, "stop_loss_atr_mult": true,
	}
	out := formatStrategyInspection(sc, explicit, &Config{IntervalSeconds: 600}, nil)

	for _, want := range []string{
		"strategy hl-rmc-eth-live",
		"close_strategy:      [tiered_tp_atr]",
		"stop_loss:",
		"source:            stop_loss_atr_mult (explicit)",
		"take_profit:",
		"source:            close_strategy tiered_tp_atr",
		"tiers:             [1.5× ATR @ 40%, 3× ATR @ 80%, 5× ATR @ 100%]",
		"default (canonical [1.5×@40%, 3×@80%, 5×@100%])",
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
		CloseStrategy:   &StrategyRef{Name: "tiered_tp_atr"},
		StopLossATRMult: &mult,
	}
	line := formatStrategySummaryLine(sc, map[string]bool{"stop_loss_atr_mult": true}, nil)
	for _, want := range []string{
		"[config] hl-rmc-eth-live:",
		"type=perps",
		"open=range_mean_revert",
		"close=tiered_tp_atr",
		"sl=stop_loss_atr_mult (explicit)",
		"tp=tiered_tp_atr[3-tier]",
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
	line := formatStrategySummaryLine(sc, nil, nil)
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
		CloseStrategy:   &StrategyRef{Name: "tiered_tp_atr"},
		Leverage:        5,
		SizingLeverage:  5,
		MarginMode:      "isolated",
		MaxDrawdownPct:  50,
		StopLossATRMult: &mult,
	}
	out := buildStrategyInspectionJSON(sc, map[string]bool{
		"open_strategy": true, "close_strategy": true, "stop_loss_atr_mult": true,
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
		CloseStrategy:   &StrategyRef{Name: "tiered_tp_atr_regime", Params: map[string]interface{}{"use_defaults": true}},
		Leverage:        3,
		StopLossATRMult: &mult,
		MaxDrawdownPct:  50,
	}
	explicit := map[string]bool{
		"id": true, "type": true, "platform": true, "script": true,
		"close_strategy": true, "leverage": true, "max_drawdown_pct": true,
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
		CloseStrategy:   &StrategyRef{Name: "tiered_tp_atr_regime", Params: map[string]interface{}{"use_defaults": true}},
		StopLossATRMult: &mult,
	}
	line := formatStrategySummaryLine(sc, nil, nil)
	// #870: fleet default is ragged per group (2/3/4 tiers); the summary reports
	// the fleet maximum (clean group = 4 tiers).
	if !strings.Contains(line, "tp=tiered_tp_atr_regime[4-tier]") {
		t.Errorf("expected 4-tier regime TP summary, got: %s", line)
	}
}

func TestFormatStrategyInspectionLegacyDirectionLabel(t *testing.T) {
	sc := StrategyConfig{ID: "hl-plain", Type: "perps", Platform: "hyperliquid", Direction: DirectionLong}
	out := formatStrategyInspection(sc, map[string]bool{"direction": true}, nil, nil)
	if !strings.Contains(out, "  direction:           long") {
		t.Errorf("strategies without regime_directional_policy should use legacy direction: label:\n%s", out)
	}
	if strings.Contains(out, "  base_direction:") {
		t.Errorf("should not emit base_direction without policy:\n%s", out)
	}
}

func TestFormatStrategyInspectionBaseDirectionWithPolicy(t *testing.T) {
	sc := StrategyConfig{
		ID: "hl-policy", Type: "perps", Platform: "hyperliquid", Direction: DirectionLong,
		RegimeDirectionalPolicy: &RegimeDirectionalPolicy{TrendRegime: map[string]RegimeDirectionalEntry{
			"trending_up": {Direction: DirectionLong}, "trending_down": {Direction: DirectionShort}, "ranging": {Direction: DirectionLong},
		}},
	}
	out := formatStrategyInspection(sc, map[string]bool{"direction": true}, nil, nil)
	if !strings.Contains(out, "  base_direction:      long") {
		t.Errorf("policy strategies should use base_direction: label:\n%s", out)
	}
	if !strings.Contains(out, "  regime_directional_policy:") {
		t.Errorf("missing policy table:\n%s", out)
	}
}

func TestFormatStrategyInspectionPositionOrderDeterministic(t *testing.T) {
	sc := StrategyConfig{ID: "hl-multi", Type: "perps", Platform: "hyperliquid", Direction: DirectionLong}
	state := NewAppState()
	state.Strategies["hl-multi"] = &StrategyState{
		ID:   "hl-multi",
		Type: "perps",
		Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 1, Side: "long", Multiplier: 1, Leverage: 1},
			"BTC": {Symbol: "BTC", Quantity: 0.1, Side: "short", Regime: "trending_down", Multiplier: 1, Leverage: 1},
		},
	}
	out := formatStrategyInspection(sc, nil, nil, state)
	btc := strings.Index(out, "position BTC:")
	eth := strings.Index(out, "position ETH:")
	if btc < 0 || eth < 0 {
		t.Fatalf("missing position lines:\n%s", out)
	}
	if btc > eth {
		t.Errorf("positions should be sorted by symbol (BTC before ETH), got BTC@%d ETH@%d", btc, eth)
	}
}

// #1048: a strategy with the circuit breaker explicitly disabled surfaces
// "cb=off" in the startup summary so it isn't silently unprotected; an enabled
// (default) strategy and a manual strategy (CB no-op) show nothing.
func TestFormatStrategySummaryLineShowsCircuitBreakerOff(t *testing.T) {
	off := false
	disabled := StrategyConfig{
		ID: "hl-eth", Type: "perps", Platform: "hyperliquid",
		OpenStrategy: StrategyRef{Name: "momentum"}, CircuitBreaker: &off,
	}
	if line := formatStrategySummaryLine(disabled, nil, nil); !strings.Contains(line, "cb=off") {
		t.Errorf("disabled CB should show cb=off: %s", line)
	}

	enabled := StrategyConfig{
		ID: "hl-eth", Type: "perps", Platform: "hyperliquid",
		OpenStrategy: StrategyRef{Name: "momentum"},
	}
	if line := formatStrategySummaryLine(enabled, nil, nil); strings.Contains(line, "cb=") {
		t.Errorf("default CB should not mention cb=: %s", line)
	}

	// Manual is exempt from CheckRisk — the flag is a no-op, so don't surface it.
	manual := StrategyConfig{
		ID: "manual-eth", Type: "manual", Platform: "hyperliquid",
		OpenStrategy: StrategyRef{Name: "hold"}, CircuitBreaker: &off,
	}
	if line := formatStrategySummaryLine(manual, nil, nil); strings.Contains(line, "cb=") {
		t.Errorf("manual strategy should not mention cb=: %s", line)
	}
}

// #1273: non-default cb_* timing/threshold overrides on an enabled breaker
// surface in the startup summary line, the inspect text, and the inspect JSON;
// pure defaults show nothing extra.
func TestStrategySurfacesShowCBOverrides(t *testing.T) {
	dd, th, lc := 720, 3, 30
	tuned := StrategyConfig{
		ID: "spot-btc", Type: "spot", Platform: "binanceus",
		OpenStrategy:                StrategyRef{Name: "momentum"},
		MaxDrawdownPct:              20,
		CBDrawdownCooldownMinutes:   &dd,
		CBLossStreakThreshold:       &th,
		CBLossStreakCooldownMinutes: &lc,
	}

	line := formatStrategySummaryLine(tuned, nil, nil)
	if !strings.Contains(line, "cb[losses>=3, loss_cooldown=30m, dd_cooldown=12h0m]") {
		t.Errorf("summary line should carry the tuned CB parameters: %s", line)
	}

	text := formatStrategyInspection(tuned, nil, nil, nil)
	if !strings.Contains(text, "circuit_breaker:     on — losses>=3, loss_cooldown=30m, dd_cooldown=12h0m") {
		t.Errorf("inspect text should carry the tuned CB parameters:\n%s", text)
	}

	out := buildStrategyInspectionJSON(tuned, map[string]bool{
		"cb_drawdown_cooldown_minutes": true, "cb_loss_streak_threshold": true, "cb_loss_streak_cooldown_minutes": true,
	}, nil, nil)
	if out["cb_drawdown_cooldown_minutes"] != 720 || out["cb_loss_streak_threshold"] != 3 || out["cb_loss_streak_cooldown_minutes"] != 30 {
		t.Errorf("inspect JSON should carry the effective tuned values, got %v %v %v",
			out["cb_drawdown_cooldown_minutes"], out["cb_loss_streak_threshold"], out["cb_loss_streak_cooldown_minutes"])
	}
	if out["cb_loss_streak_threshold_explicit"] != true {
		t.Errorf("explicit provenance flag lost: %v", out["cb_loss_streak_threshold_explicit"])
	}

	// Defaults: no override marker anywhere, JSON reports the built-in values.
	plain := StrategyConfig{
		ID: "spot-btc", Type: "spot", Platform: "binanceus",
		OpenStrategy: StrategyRef{Name: "momentum"}, MaxDrawdownPct: 20,
	}
	if line := formatStrategySummaryLine(plain, nil, nil); strings.Contains(line, "cb[") {
		t.Errorf("default CB parameters should not surface in summary: %s", line)
	}
	if text := formatStrategyInspection(plain, nil, nil, nil); strings.Contains(text, "circuit_breaker:") {
		t.Errorf("default CB parameters should not surface in inspect text:\n%s", text)
	}
	out = buildStrategyInspectionJSON(plain, nil, nil, nil)
	if out["cb_drawdown_cooldown_minutes"] != 1440 || out["cb_loss_streak_threshold"] != 5 || out["cb_loss_streak_cooldown_minutes"] != 60 {
		t.Errorf("inspect JSON should report the built-in defaults, got %v %v %v",
			out["cb_drawdown_cooldown_minutes"], out["cb_loss_streak_threshold"], out["cb_loss_streak_cooldown_minutes"])
	}

	// Manual is exempt from CheckRisk — no CB keys at all.
	manual := StrategyConfig{ID: "manual-eth", Type: "manual", Platform: "hyperliquid"}
	out = buildStrategyInspectionJSON(manual, nil, nil, nil)
	if _, ok := out["cb_loss_streak_threshold"]; ok {
		t.Error("manual strategy should not emit cb_* keys in inspect JSON")
	}
}
