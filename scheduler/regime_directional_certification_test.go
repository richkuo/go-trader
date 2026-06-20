package main

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestNormalizeCertAsset(t *testing.T) {
	cases := map[string]string{
		"BTC/USDT": "BTC",
		"btc":      "BTC",
		"BTC-PERP": "BTC",
		"ETH/USD":  "ETH",
		"SOL_USDT": "SOL",
		"BTC:USDC": "BTC",
		"  xrp  ":  "XRP",
		"":         "",
	}
	for in, want := range cases {
		if got := normalizeCertAsset(in); got != want {
			t.Errorf("normalizeCertAsset(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseEmptyCertSetCertifiesNothing(t *testing.T) {
	data := []byte(`{"schema_version":1,"certified":[]}`)
	set, err := parseDirectionalCertSet(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, ok := set.Certified("BTC", "1h", "composite", time.Now()); ok {
		t.Fatal("empty set must certify nothing")
	}
	if st := set.Status("BTC", "1h", "composite", time.Now()); st != CertNever {
		t.Fatalf("status = %v, want CertNever", st)
	}
}

func TestCertifiedActiveAndExpired(t *testing.T) {
	now := time.Date(2026, 6, 19, 0, 0, 0, 0, time.UTC)
	future := now.Add(48 * time.Hour).Format(time.RFC3339)
	past := now.Add(-48 * time.Hour).Format(time.RFC3339)

	active := []byte(`{"schema_version":1,"certified":[
		{"asset":"BTC/USDT","timeframe":"1h","classifier":"composite","expires_at":"` + future + `",
		 "states":{"trending_up":"long","trending_down":"short"}}]}`)
	set, err := parseDirectionalCertSet(active)
	if err != nil {
		t.Fatalf("parse active: %v", err)
	}
	// Live HL args use "BTC"; artifact uses "BTC/USDT" — must reconcile.
	states, ok := set.Certified("BTC", "1h", "composite", now)
	if !ok {
		t.Fatal("expected active certification for BTC/1h/composite")
	}
	if states["trending_up"] != DirectionLong || states["trending_down"] != DirectionShort {
		t.Fatalf("unexpected states: %v", states)
	}
	if st := set.Status("btc", "1h", "composite", now); st != CertActive {
		t.Fatalf("status = %v, want CertActive", st)
	}
	// Wrong classifier / timeframe / asset must all miss.
	if _, ok := set.Certified("BTC", "1h", "adx", now); ok {
		t.Fatal("classifier mismatch must not certify")
	}
	if _, ok := set.Certified("BTC", "4h", "composite", now); ok {
		t.Fatal("timeframe mismatch must not certify")
	}
	if _, ok := set.Certified("ETH", "1h", "composite", now); ok {
		t.Fatal("asset mismatch must not certify")
	}

	expired := []byte(`{"schema_version":1,"certified":[
		{"asset":"BTC","timeframe":"1h","classifier":"composite","expires_at":"` + past + `",
		 "states":{"trending_up":"long"}}]}`)
	eset, err := parseDirectionalCertSet(expired)
	if err != nil {
		t.Fatalf("parse expired: %v", err)
	}
	if _, ok := eset.Certified("BTC", "1h", "composite", now); ok {
		t.Fatal("expired certification must not certify")
	}
	if st := eset.Status("BTC", "1h", "composite", now); st != CertExpired {
		t.Fatalf("status = %v, want CertExpired", st)
	}
}

func TestParseRejectsBadDirectionAndSchema(t *testing.T) {
	bad := []byte(`{"schema_version":1,"certified":[
		{"asset":"BTC","timeframe":"1h","classifier":"composite","states":{"trending_up":"sideways"}}]}`)
	if _, err := parseDirectionalCertSet(bad); err == nil {
		t.Fatal("expected error for invalid direction")
	}
	wrongSchema := []byte(`{"schema_version":2,"certified":[]}`)
	if _, err := parseDirectionalCertSet(wrongSchema); err == nil {
		t.Fatal("expected error for unsupported schema_version")
	}
	malformed := []byte(`{not json`)
	if _, err := parseDirectionalCertSet(malformed); err == nil {
		t.Fatal("expected error for malformed JSON")
	}
}

func TestLoadFailClosedOnMissingAndMalformed(t *testing.T) {
	// Missing file -> empty set, no warn, no error.
	set, err := LoadDirectionalCertSet("/nonexistent/regime_directional_certifications.json")
	if err != nil {
		t.Fatalf("missing file should not error, got %v", err)
	}
	if len(set.byKey) != 0 {
		t.Fatal("missing file should yield empty set")
	}

	// Malformed -> fail-closed wrapper returns empty + warns.
	dir := t.TempDir()
	path := dir + "/cert.json"
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	warned := false
	got := LoadDirectionalCertSetFailClosed(path, func(string, ...interface{}) { warned = true })
	if !warned {
		t.Fatal("malformed artifact must warn")
	}
	if _, ok := got.Certified("BTC", "1h", "composite", time.Now()); ok {
		t.Fatal("fail-closed set must certify nothing")
	}
}

func TestDirectionalCertIdentity(t *testing.T) {
	sc := StrategyConfig{
		Type: "perps",
		Args: []string{"hold", "BTC", "1h"},
	}
	asset, tf, classifier, ok := directionalCertIdentity(sc, nil)
	if !ok {
		t.Fatal("expected resolvable identity")
	}
	if asset != "BTC" || tf != "1h" || classifier != regimeClassifierADX {
		t.Fatalf("identity = (%q,%q,%q), want (BTC,1h,adx)", asset, tf, classifier)
	}
	// No symbol/timeframe -> not resolvable.
	if _, _, _, ok := directionalCertIdentity(StrategyConfig{Args: []string{"hold"}}, nil); ok {
		t.Fatal("expected unresolvable identity for short args")
	}
}

func TestApplyRegimeDirectionalPolicyDefaultOffWhenUncertified(t *testing.T) {
	sc := StrategyConfig{Direction: DirectionLong, InvertSignal: false, RegimeDirectionalPolicy: &RegimeDirectionalPolicy{
		TrendRegime: map[string]RegimeDirectionalEntry{
			"trending_down": {Direction: DirectionShort, InvertSignal: true},
			"trending_up":   {Direction: DirectionLong},
			"ranging":       {Direction: DirectionLong},
		}}}
	// certified=false → default-off: policy not applied, base config preserved.
	_, applied, _ := applyRegimeDirectionalPolicy(&sc, "trending_down", "", 0, false)
	if applied {
		t.Fatal("uncertified policy must not apply (default-off)")
	}
	if sc.Direction != DirectionLong || sc.InvertSignal {
		t.Fatalf("sc must stay at base, got dir=%q invert=%t", sc.Direction, sc.InvertSignal)
	}
	// certified=true → applied, sc mutated to the regime's direction.
	entry, applied, _ := applyRegimeDirectionalPolicy(&sc, "trending_down", "", 0, true)
	if !applied || entry.Direction != DirectionShort || sc.Direction != DirectionShort {
		t.Fatalf("certified policy must apply short, got applied=%t entry=%+v dir=%q", applied, entry, sc.Direction)
	}
}

func TestStrategyDirectionalCertifiedUsesStore(t *testing.T) {
	now := time.Date(2026, 6, 19, 0, 0, 0, 0, time.UTC)
	future := now.Add(24 * time.Hour).Format(time.RFC3339)
	set, err := parseDirectionalCertSet([]byte(`{"schema_version":1,"certified":[
		{"asset":"BTC","timeframe":"1h","classifier":"adx","expires_at":"` + future + `",
		 "states":{"trending_up":"long","trending_down":"short","ranging":"long"}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	prev := getDirectionalCertStore()
	setDirectionalCertStore(set)
	defer setDirectionalCertStore(prev)

	// rc=nil → classifier defaults to adx, matching the entry.
	sc := StrategyConfig{Type: "perps", Args: []string{"vwap", "BTC", "1h"}}
	if _, ok := strategyDirectionalCertified(sc, nil, now); !ok {
		t.Fatal("BTC/1h/adx should be certified via store")
	}
	if st := strategyDirectionalCertStatus(sc, nil, now); st != CertActive {
		t.Fatalf("status = %v, want CertActive", st)
	}
	scEth := StrategyConfig{Type: "perps", Args: []string{"vwap", "ETH", "1h"}}
	if _, ok := strategyDirectionalCertified(scEth, nil, now); ok {
		t.Fatal("ETH should not be certified")
	}
}

func TestDirectionalCertStartupSummary(t *testing.T) {
	prev := getDirectionalCertStore()
	setDirectionalCertStore(emptyDirectionalCertSet())
	defer setDirectionalCertStore(prev)

	cfg := &Config{Strategies: []StrategyConfig{
		{ID: "plain", Type: "perps", Args: []string{"vwap", "BTC", "1h"}}, // no policy → no line
		{ID: "dir", Type: "perps", Args: []string{"vwap", "BTC", "1h"}, RegimeDirectionalPolicy: &RegimeDirectionalPolicy{
			TrendRegime: map[string]RegimeDirectionalEntry{"trending_up": {Direction: DirectionLong}},
		}},
	}}
	lines := directionalCertStartupSummary(cfg)
	if len(lines) != 1 {
		t.Fatalf("want one summary line for the configured strategy, got %d: %v", len(lines), lines)
	}
	if !strings.Contains(lines[0], "dir") || !strings.Contains(lines[0], "DEFAULT-OFF") {
		t.Fatalf("uncertified strategy line should be default-off, got: %q", lines[0])
	}
}

func TestDirectionalCertSignMismatches(t *testing.T) {
	sc := StrategyConfig{}
	pol := &RegimeDirectionalPolicy{TrendRegime: map[string]RegimeDirectionalEntry{
		"trending_up":   {Direction: DirectionShort}, // contradicts certified long
		"trending_down": {Direction: DirectionShort}, // matches
		"ranging":       {Direction: DirectionBoth},  // never contradicts
	}}
	sc.RegimeDirectionalPolicy = pol
	certStates := map[string]string{
		"trending_up":   DirectionLong,
		"trending_down": DirectionShort,
		"ranging":       DirectionLong,
	}
	got := directionalCertSignMismatches(sc, certStates)
	if len(got) != 1 {
		t.Fatalf("expected 1 mismatch, got %v", got)
	}
	if got[0] != "trending_up: config=short certified=long" {
		t.Fatalf("unexpected mismatch text: %q", got[0])
	}
}
