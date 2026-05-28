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
	raw := `{"macro":{"classifier":"composite","period":720,"thresholds":{"return_pct":0.05,"range_pct":0.03,"adx":25}}}`
	var m RegimeWindowsMap
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	if m["macro"].effectiveClassifier() != regimeClassifierComposite {
		t.Fatalf("classifier = %q", m["macro"].Classifier)
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
			AllowedRegimes:   []string{"trending_up"}, // ADX label on composite window
		}},
	}
	errs := validateStrategyRegimeVocabulary(cfg)
	if len(errs) == 0 {
		t.Fatal("expected allowed_regimes vocabulary error")
	}
}

func TestRegimeWindowsSpecJSON_LegacyDefault(t *testing.T) {
	rc := &RegimeConfig{Enabled: true, Period: 28, ADXThreshold: 22}
	blob := regimeWindowsSpecJSON(rc)
	if blob == "" || !strings.Contains(blob, `"period":28`) || !strings.Contains(blob, `"adx_threshold":22`) {
		t.Fatalf("blob = %s", blob)
	}
}
