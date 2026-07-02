package main

import (
	"strings"
	"testing"
)

func TestFormatClosingStrategiesResponseSortedOrder(t *testing.T) {
	entries := []closeRegistryEntry{
		{Name: "zscore_target", Description: "Z target", DefaultParams: map[string]interface{}{"z_target": 0.0}, Platforms: []string{"spot"}},
		{Name: "atr_stop", Description: "ATR stop", DefaultParams: map[string]interface{}{"atr_mult": 0.0}, Platforms: []string{"spot"}},
		{Name: "avwap_stop", Description: "AVWAP stop", DefaultParams: map[string]interface{}{"buffer_atr_mult": 0.25}, Platforms: []string{"spot", "futures"}},
	}
	pages := formatClosingStrategiesResponse(&Config{}, entries)
	if len(pages) != 1 {
		t.Fatalf("expected 1 page, got %d", len(pages))
	}
	body := pages[0]
	iAtr := strings.Index(body, "**atr_stop**")
	iAvwap := strings.Index(body, "**avwap_stop**")
	iZ := strings.Index(body, "**zscore_target**")
	if iAtr < 0 || iAvwap < 0 || iZ < 0 {
		t.Fatalf("missing expected entries in output: %s", body)
	}
	if !(iAtr < iAvwap && iAvwap < iZ) {
		t.Fatalf("entries not sorted by name: atr_stop=%d avwap_stop=%d zscore_target=%d", iAtr, iAvwap, iZ)
	}
	if !strings.Contains(body, "3 registered") {
		t.Fatalf("expected header to report 3 registered evaluators, got: %s", body)
	}
}

func TestFormatClosingStrategiesResponseParamsAndPlatforms(t *testing.T) {
	entries := []closeRegistryEntry{
		{
			Name:          "avwap_stop",
			Description:   "AVWAP loss-of-line exit",
			DefaultParams: map[string]interface{}{"buffer_atr_mult": 0.25, "atr_source": "live"},
			Platforms:     []string{"futures", "spot"},
		},
	}
	body := formatClosingStrategiesResponse(&Config{}, entries)[0]
	if !strings.Contains(body, "platforms: futures, spot") {
		t.Fatalf("expected sorted platform list, got: %s", body)
	}
	if !strings.Contains(body, "atr_source=\"live\"") {
		t.Fatalf("expected atr_source param rendered, got: %s", body)
	}
	if !strings.Contains(body, "buffer_atr_mult=0.25") {
		t.Fatalf("expected buffer_atr_mult param rendered, got: %s", body)
	}
}

func TestFormatClosingStrategiesResponseNoParams(t *testing.T) {
	entries := []closeRegistryEntry{
		{Name: "trailing_tp_ratchet_regime", Description: "Regime ratchet", DefaultParams: map[string]interface{}{}, Platforms: []string{"spot"}},
	}
	body := formatClosingStrategiesResponse(&Config{}, entries)[0]
	if !strings.Contains(body, "params: (none)") {
		t.Fatalf("expected explicit (none) marker for empty default_params, got: %s", body)
	}
}

func TestFormatClosingStrategiesResponseOverrideSurfacesWhenRegistryDefaultIsEmpty(t *testing.T) {
	// trailing_tp_ratchet_regime (and any override-eligible evaluator with
	// empty registry default_params) must still show the operator's
	// configured tp_tiers — that's the only value that ever runs for it.
	cfg := &Config{
		UserDefaults: &UserDefaultsConfig{
			Close: CloseDefaultsMap{
				"trailing_tp_ratchet_regime": {"tp_tiers": []interface{}{
					map[string]interface{}{"atr_multiple": 1.0, "trailing_stop_mult_after": 0.5},
				}},
			},
		},
	}
	entries := []closeRegistryEntry{
		{Name: "trailing_tp_ratchet_regime", Description: "Regime ratchet", DefaultParams: map[string]interface{}{}, Platforms: []string{"spot"}},
	}
	body := formatClosingStrategiesResponse(cfg, entries)[0]
	if strings.Contains(body, "params: (none)") {
		t.Fatalf("configured override must not be hidden behind the empty-registry-default (none) marker, got: %s", body)
	}
	if !strings.Contains(body, "tp_tiers=") || !strings.Contains(body, "user_defaults.close override") {
		t.Fatalf("expected tp_tiers override to be surfaced, got: %s", body)
	}
}

func TestFormatClosingStrategiesResponseRatchetRegimeSLOverride(t *testing.T) {
	// trailing_tp_ratchet_regime is the one evaluator whose user_defaults.close
	// entry may carry a coupled trailing_stop_atr_regime SL owner in addition to
	// tp_tiers (close_defaults.go validateUserCloseDefaults / applyUserClose
	// DefaultRatchetRegimeTrail). Both effective override keys must be surfaced
	// and each marked as an override — the catalog exists so an operator can see
	// what actually runs without reading source.
	entries := []closeRegistryEntry{
		{Name: "trailing_tp_ratchet_regime", Description: "Regime ratchet", DefaultParams: map[string]interface{}{}, Platforms: []string{"spot"}},
	}

	// Case 1: both tp_tiers and trailing_stop_atr_regime configured — both shown,
	// both marked as overrides.
	cfgBoth := &Config{
		UserDefaults: &UserDefaultsConfig{
			Close: CloseDefaultsMap{
				"trailing_tp_ratchet_regime": {
					"tp_tiers": []interface{}{
						map[string]interface{}{"atr_multiple": 1.0, "trailing_stop_mult_after": 0.5},
					},
					"trailing_stop_atr_regime": map[string]interface{}{
						"use_defaults": true,
					},
				},
			},
		},
	}
	body := formatClosingStrategiesResponse(cfgBoth, entries)[0]
	if !strings.Contains(body, "tp_tiers=") || !strings.Contains(body, "trailing_stop_atr_regime=") {
		t.Fatalf("expected both tp_tiers and trailing_stop_atr_regime surfaced, got: %s", body)
	}
	if strings.Count(body, "user_defaults.close override") != 2 {
		t.Fatalf("expected both override keys marked as overrides, got: %s", body)
	}
	if strings.Contains(body, "params: (none)") {
		t.Fatalf("configured overrides must not be hidden behind the (none) marker, got: %s", body)
	}

	// Case 2: tp_tiers only — no spurious trailing_stop_atr_regime line.
	cfgTPOnly := &Config{
		UserDefaults: &UserDefaultsConfig{
			Close: CloseDefaultsMap{
				"trailing_tp_ratchet_regime": {
					"tp_tiers": []interface{}{
						map[string]interface{}{"atr_multiple": 1.0, "trailing_stop_mult_after": 0.5},
					},
				},
			},
		},
	}
	body = formatClosingStrategiesResponse(cfgTPOnly, entries)[0]
	if strings.Contains(body, "trailing_stop_atr_regime") {
		t.Fatalf("no trailing_stop_atr_regime configured — must not emit a spurious SL line, got: %s", body)
	}
	if !strings.Contains(body, "tp_tiers=") || !strings.Contains(body, "user_defaults.close override") {
		t.Fatalf("expected tp_tiers override surfaced, got: %s", body)
	}

	// Case 3: no user_defaults.close entry at all — no override note anywhere.
	body = formatClosingStrategiesResponse(&Config{}, entries)[0]
	if strings.Contains(body, "override") || strings.Contains(body, "trailing_stop_atr_regime") {
		t.Fatalf("no user_defaults.close entry exists — must not claim any override, got: %s", body)
	}
}

func TestFormatClosingStrategiesResponseUserDefaultsOverride(t *testing.T) {
	cfg := &Config{
		UserDefaults: &UserDefaultsConfig{
			Close: CloseDefaultsMap{
				"tiered_tp_atr_live": {"tp_tiers": []interface{}{
					map[string]interface{}{"atr_multiple": 1.0, "close_fraction": 1.0},
				}},
			},
		},
	}
	entries := []closeRegistryEntry{
		{
			Name:        "tiered_tp_atr_live",
			Description: "Tiered TP",
			DefaultParams: map[string]interface{}{
				"tp_tiers":   []interface{}{map[string]interface{}{"atr_multiple": 1.5, "close_fraction": 0.4}},
				"atr_source": "live",
			},
			Platforms: []string{"spot"},
		},
	}
	body := formatClosingStrategiesResponse(cfg, entries)[0]
	if !strings.Contains(body, "user_defaults.close override") {
		t.Fatalf("expected override marker, got: %s", body)
	}
	if strings.Contains(body, "atr_multiple\":1.5") {
		t.Fatalf("expected the registry default tp_tiers to be replaced by the override, got: %s", body)
	}
	if !strings.Contains(body, "atr_multiple\":1") {
		t.Fatalf("expected the overriding tp_tiers value to be shown, got: %s", body)
	}
	// atr_source has no user_defaults.close story (only tp_tiers can be
	// overridden) so it must still show the plain registry default.
	if strings.Contains(body, "atr_source=\"live\" (user_defaults.close override)") {
		t.Fatalf("atr_source must not be marked as an override, got: %s", body)
	}
}

func TestFormatClosingStrategiesResponseNoOverrideWhenUnconfigured(t *testing.T) {
	entries := []closeRegistryEntry{
		{
			Name:          "tiered_tp_atr_live",
			Description:   "Tiered TP",
			DefaultParams: map[string]interface{}{"tp_tiers": []interface{}{map[string]interface{}{"atr_multiple": 1.5}}},
			Platforms:     []string{"spot"},
		},
	}
	body := formatClosingStrategiesResponse(&Config{}, entries)[0]
	if strings.Contains(body, "override") {
		t.Fatalf("no user_defaults.close entry exists — must not claim an override: %s", body)
	}
}

func TestFormatClosingStrategiesResponseEmptyCatalog(t *testing.T) {
	pages := formatClosingStrategiesResponse(&Config{}, nil)
	if len(pages) != 1 || pages[0] != "No close evaluators registered." {
		t.Fatalf("expected the empty-catalog message, got: %v", pages)
	}
}

func TestFormatClosingStrategiesResponseChunksAcrossMessages(t *testing.T) {
	// Build enough synthetic evaluators with long descriptions to force the
	// output past discordCharLimit and confirm it splits into multiple pages,
	// each individually under the limit.
	var entries []closeRegistryEntry
	longDesc := strings.Repeat("x", 300)
	for i := 0; i < 20; i++ {
		entries = append(entries, closeRegistryEntry{
			Name:          "evaluator_" + string(rune('a'+i)),
			Description:   longDesc,
			DefaultParams: map[string]interface{}{"param": 1.0},
			Platforms:     []string{"spot"},
		})
	}
	pages := formatClosingStrategiesResponse(&Config{}, entries)
	if len(pages) < 2 {
		t.Fatalf("expected multiple pages for oversized catalog, got %d", len(pages))
	}
	for idx, p := range pages {
		if len(p) > discordCharLimit {
			t.Fatalf("page %d exceeds discordCharLimit (%d): len=%d", idx, discordCharLimit, len(p))
		}
	}
	if !strings.Contains(pages[1], "cont'd") {
		t.Fatalf("expected continuation header on page 2, got: %s", pages[1])
	}
}

func TestPackTextBlocksSingleOversizedBlock(t *testing.T) {
	block := strings.Repeat("y", 100)
	pages := packTextBlocks([]string{block}, 10)
	if len(pages) != 1 || pages[0] != block {
		t.Fatalf("a lone oversized block should still be returned whole, got: %v", pages)
	}
}
