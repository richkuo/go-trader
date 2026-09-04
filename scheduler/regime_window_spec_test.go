package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRegimeWindowsMap_UnmarshalBareInt(t *testing.T) {
	var m RegimeWindowsMap
	if err := json.Unmarshal([]byte(`{"fast":14,"macro":720}`), &m); err != nil {
		t.Fatal(err)
	}
	if m["fast"].effectiveClassifier() != regimeClassifierADX || m["fast"].Period != 14 {
		t.Fatalf("fast = %+v", m["fast"])
	}
}

func TestRegimeWindowsMap_UnmarshalCompositeSpec(t *testing.T) {
	raw := `{"macro":{"classifier":"composite","period":720,"thresholds":{"return_eff":0.05,"range_eff":0.03,"adx":25}}}`
	var m RegimeWindowsMap
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	if m["macro"].effectiveClassifier() != regimeClassifierComposite {
		t.Fatalf("classifier = %q", m["macro"].Classifier)
	}
}

func TestRegimeWindowsMap_CompositeThresholdAliases(t *testing.T) {
	raw := `{"macro":{"classifier":"composite","period":720,"thresholds":{"return_pct":0.90,"range_pct":0.90,"return_eff":0.05,"range_eff":0.03,"adx":25}}}`
	var m RegimeWindowsMap
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	th := m["macro"].compositeThresholds()
	if th.ReturnEff != 0.05 || th.RangeEff != 0.03 {
		t.Fatalf("thresholds = %+v, want canonical values", th)
	}
}

func TestRegimeWindowsMap_CompositeThresholdUnknownKey(t *testing.T) {
	raw := `{"macro":{"classifier":"composite","period":720,"thresholds":{"return_price_pct":0.05}}}`
	var m RegimeWindowsMap
	err := json.Unmarshal([]byte(raw), &m)
	if err == nil || !strings.Contains(err.Error(), `unknown key "return_price_pct"`) {
		t.Fatalf("expected unknown threshold key error, got: %v", err)
	}
}

func TestValidateStrategyRegimeVocabulary_CompositeGate(t *testing.T) {
	cfg := &Config{
		Regime: &RegimeConfig{
			Enabled: true,
			Windows: RegimeWindowsMap{
				"macro": {Classifier: regimeClassifierComposite, Period: 720},
			},
		},
		Strategies: []StrategyConfig{{
			ID:               "hl-test",
			RegimeGateWindow: "macro",
			AllowedRegimes:   []string{"trending_up"},
		}},
	}
	errs := validateStrategyRegimeVocabulary(cfg)
	if len(errs) == 0 {
		t.Fatal("expected allowed_regimes vocabulary error")
	}
}

func TestValidateHotReloadStateCompatible_BlocksRemovedRegimeWindow(t *testing.T) {
	old := &Config{
		Regime: &RegimeConfig{
			Enabled: true,
			Windows: RegimeWindowsMap{
				"macro": {Classifier: regimeClassifierComposite, Period: 720},
			},
		},
		Strategies: []StrategyConfig{{ID: "hl-test"}},
	}
	next := &Config{
		Regime: &RegimeConfig{
			Enabled: true,
			Windows: RegimeWindowsMap{
				"fast": {Period: 14},
			},
		},
		Strategies: []StrategyConfig{{ID: "hl-test"}},
	}
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-test": {
				Positions: map[string]*Position{
					"ETH": {
						Quantity:      1,
						Regime:        "trending_down_choppy",
						RegimeWindows: map[string]string{"macro": "trending_down_choppy"},
					},
				},
			},
		},
	}
	err := validateHotReloadStateCompatible(old, next, state)
	if err == nil || !strings.Contains(err.Error(), `regime.windows["macro"] removed`) {
		t.Fatalf("expected window removal error, got: %v", err)
	}
}

func TestValidateStrategyRegimeVocabulary_PolicyShapeWhenRegimeDisabled(t *testing.T) {
	cfg := &Config{
		Regime: &RegimeConfig{Enabled: false},
		Strategies: []StrategyConfig{{
			ID: "hl-test",
			RegimeDirectionalPolicy: policyPtr(mustParseRegimeDirectionalPolicy(t, `{
				"trend_regime": {
					"trending_up": {"direction": "sideways"}
				}
			}`)),
		}},
	}
	errs := validateStrategyRegimeVocabulary(cfg)
	if len(errs) == 0 {
		t.Fatal("expected policy shape error when regime disabled")
	}
}

func TestRegimeWindowsSpecJSON_LegacyDefault(t *testing.T) {
	rc := &RegimeConfig{Enabled: true, Period: 28, ADXThreshold: 22}
	blob := regimeWindowsSpecJSON(rc)
	if blob == "" || !strings.Contains(blob, `"period":28`) || !strings.Contains(blob, `"adx_threshold":22`) {
		t.Fatalf("blob = %s", blob)
	}
}

func policyPtr(p RegimeDirectionalPolicy) *RegimeDirectionalPolicy {
	return &p
}

func regimeDisplayTestConfig() (*RegimeConfig, *StrategyState) {
	rc := &RegimeConfig{
		Enabled: true,
		Windows: RegimeWindowsMap{
			"long":             {Period: 2160},
			"medium":           {Period: 720},
			"short":            {Period: 168},
			"composite_long":   {Classifier: regimeClassifierComposite, Period: 2160},
			"composite_medium": {Classifier: regimeClassifierComposite, Period: 720},
			"composite_short":  {Classifier: regimeClassifierComposite, Period: 168},
		},
	}
	ss := &StrategyState{
		Regime: "ranging",
		RegimeWindows: map[string]string{
			"long":             "ranging",
			"medium":           "ranging",
			"short":            "trending_down",
			"composite_long":   "ranging_directional",
			"composite_medium": "ranging_directional",
			"composite_short":  "trending_down_choppy",
		},
	}
	return rc, ss
}

func TestFormatStrategyRegimeDisplay_WindowSelection(t *testing.T) {
	cases := []struct {
		name        string
		display     []string
		dropWindows []string
		want        []string
		hidden      []string
		notContains []string
		exact       string
	}{
		{
			name: "unset renders every window without classifier suffix",
			want: []string{
				"long=", "medium=", "short=",
				"composite_long=ranging_directional", "composite_medium=", "composite_short=",
			},
			notContains: []string{"[adx]", "[composite]", "["},
		},
		{
			name:    "composite only hides scalar windows",
			display: []string{"composite_long", "composite_medium", "composite_short"},
			want:    []string{"composite_long=", "composite_medium=", "composite_short="},
			hidden:  []string{"long", "medium", "short"},
		},
		{
			name:        "case and space insensitive match",
			display:     []string{"  COMPOSITE_LONG  "},
			want:        []string{"composite_long="},
			hidden:      []string{"long"},
			notContains: []string{"composite_medium="},
		},
		{
			name:        "all selected windows unpopulated falls back to primary",
			display:     []string{"composite_long", "composite_medium", "composite_short"},
			dropWindows: []string{"composite_long", "composite_medium", "composite_short"},
			exact:       "ranging",
		},
		{
			name:    "blank entries treated as unset",
			display: []string{"", "   "},
			want:    []string{"long=", "composite_long="},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rc, ss := regimeDisplayTestConfig()
			rc.DisplayWindows = tc.display
			for _, w := range tc.dropWindows {
				delete(ss.RegimeWindows, w)
			}
			got := formatStrategyRegimeDisplay(ss, rc)
			if tc.exact != "" && got != tc.exact {
				t.Fatalf("display = %q, want %q", got, tc.exact)
			}
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Fatalf("missing %q in: %s", want, got)
				}
			}
			for _, name := range tc.hidden {
				if regimeDisplayHasBareWindow(got, name) {
					t.Fatalf("window %q should be hidden; got: %s", name, got)
				}
			}
			for _, bad := range tc.notContains {
				if strings.Contains(got, bad) {
					t.Fatalf("unexpected %q in: %s", bad, got)
				}
			}
		})
	}
}

func TestValidateRegimeWindowsConfig_DisplayWindows(t *testing.T) {
	baseWindows := func() RegimeWindowsMap {
		return RegimeWindowsMap{
			"long":           {Period: 2160},
			"composite_long": {Classifier: regimeClassifierComposite, Period: 2160},
		}
	}
	hasErrContaining := func(errs []string, sub string) bool {
		for _, e := range errs {
			if strings.Contains(e, sub) {
				return true
			}
		}
		return false
	}

	t.Run("valid names pass", func(t *testing.T) {
		cfg := &Config{Regime: &RegimeConfig{Enabled: true, Windows: baseWindows(),
			DisplayWindows: []string{"composite_long", "LONG"}}}
		if errs := validateRegimeWindowsConfig(cfg); len(errs) != 0 {
			t.Fatalf("valid display_windows should pass; got: %v", errs)
		}
	})

	t.Run("single typo errors", func(t *testing.T) {
		cfg := &Config{Regime: &RegimeConfig{Enabled: true, Windows: baseWindows(),
			DisplayWindows: []string{"composit_long"}}}
		errs := validateRegimeWindowsConfig(cfg)
		if !hasErrContaining(errs, `"composit_long" not found`) {
			t.Fatalf("typo should error; got: %v", errs)
		}
	})

	t.Run("mixed list errors only on the typo", func(t *testing.T) {
		cfg := &Config{Regime: &RegimeConfig{Enabled: true, Windows: baseWindows(),
			DisplayWindows: []string{"composite_long", "shrot"}}}
		errs := validateRegimeWindowsConfig(cfg)
		if !hasErrContaining(errs, `"shrot" not found`) {
			t.Fatalf("typo should error; got: %v", errs)
		}
		if hasErrContaining(errs, `"composite_long" not found`) {
			t.Fatalf("valid name must not error; got: %v", errs)
		}
	})

	t.Run("blank entries ignored", func(t *testing.T) {
		cfg := &Config{Regime: &RegimeConfig{Enabled: true, Windows: baseWindows(),
			DisplayWindows: []string{"", "   "}}}
		if errs := validateRegimeWindowsConfig(cfg); len(errs) != 0 {
			t.Fatalf("blank entries should be ignored, not errored; got: %v", errs)
		}
	})

	t.Run("requires windows configured", func(t *testing.T) {
		cfg := &Config{Regime: &RegimeConfig{Enabled: true,
			DisplayWindows: []string{"composite_long"}}}
		errs := validateRegimeWindowsConfig(cfg)
		if !hasErrContaining(errs, "regime.display_windows requires regime.windows") {
			t.Fatalf("display_windows without windows should error; got: %v", errs)
		}
	})
}

func regimeDisplayHasBareWindow(out, name string) bool {
	needle := name + "="
	for _, part := range strings.Split(out, "; ") {
		if strings.HasPrefix(strings.TrimSpace(part), needle) {
			return true
		}
	}
	return false
}

func mustParseRegimeDirectionalPolicy(t *testing.T, raw string) RegimeDirectionalPolicy {
	t.Helper()
	var p RegimeDirectionalPolicy
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatalf("unmarshal policy: %v", err)
	}
	return p
}
