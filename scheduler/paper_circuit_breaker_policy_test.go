package main

import (
	"strings"
	"testing"
)

func loadPaperCBPolicyConfig(t *testing.T, root map[string]interface{}, strategies ...map[string]interface{}) *Config {
	t.Helper()
	path := writeNoEdgeConfigRoot(t, CurrentConfigVersion, root, strategies...)
	cfg, err := loadConfig(path, true, false)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	return cfg
}

func paperCBPolicyStrategy(id, mode string, circuitBreaker *bool) map[string]interface{} {
	sc := noEdgeHLStrategy(id, "sma_crossover", "ETH", "1h", "--mode="+mode)
	if mode == "live" {
		sc["allow_no_edge"] = true
	}
	if circuitBreaker != nil {
		sc["circuit_breaker"] = *circuitBreaker
	}
	return sc
}

func paperCBPolicyCheckRisk(t *testing.T, sc StrategyConfig) (bool, bool) {
	t.Helper()
	st := &StrategyState{ID: sc.ID, Type: sc.Type, Platform: sc.Platform, Cash: 100}
	st.RiskState.MaxDrawdownPct = sc.MaxDrawdownPct
	st.RiskState.PeakValue = 100
	st.RiskState.ConsecutiveLosses = sc.CircuitBreakerLossStreakThreshold()
	allowed, _ := CheckRisk(&sc, st, 100, map[string]float64{}, nil, nil)
	return allowed, st.RiskState.CircuitBreaker
}

func TestPaperCircuitBreakersRootKeyLoadedConfigIntegration(t *testing.T) {
	yes, no := true, false
	root := map[string]interface{}{"paper_circuit_breakers": false}
	cfg := loadPaperCBPolicyConfig(t, root,
		paperCBPolicyStrategy("live-default", "live", nil),
		paperCBPolicyStrategy("paper-default", "paper", nil),
		paperCBPolicyStrategy("paper-on", "paper", &yes),
		paperCBPolicyStrategy("paper-off", "paper", &no),
	)
	byID := map[string]StrategyConfig{}
	for _, sc := range cfg.Strategies {
		byID[sc.ID] = sc
	}

	liveAllowed, liveLatched := paperCBPolicyCheckRisk(t, byID["live-default"])
	if liveAllowed || !liveLatched {
		t.Fatalf("live strategy under paper_circuit_breakers=false: allowed=%v latched=%v, want blocked and latched", liveAllowed, liveLatched)
	}
	paperOnAllowed, paperOnLatched := paperCBPolicyCheckRisk(t, byID["paper-on"])
	if paperOnAllowed || !paperOnLatched {
		t.Fatalf("paper strategy with circuit_breaker=true: allowed=%v latched=%v, want blocked and latched", paperOnAllowed, paperOnLatched)
	}
	paperAllowed, paperLatched := paperCBPolicyCheckRisk(t, byID["paper-default"])
	if !paperAllowed || paperLatched {
		t.Fatalf("paper strategy with no circuit_breaker key: allowed=%v latched=%v, want allowed and not latched", paperAllowed, paperLatched)
	}
	offAllowed, offLatched := paperCBPolicyCheckRisk(t, byID["paper-off"])
	if !offAllowed || offLatched {
		t.Fatalf("paper strategy with circuit_breaker=false: allowed=%v latched=%v, want allowed and not latched", offAllowed, offLatched)
	}

	defaultCfg := loadPaperCBPolicyConfig(t, nil, paperCBPolicyStrategy("paper-default", "paper", nil))
	absentAllowed, absentLatched := paperCBPolicyCheckRisk(t, defaultCfg.Strategies[0])
	if !absentAllowed || absentLatched {
		t.Fatalf("paper strategy with the root key absent: allowed=%v latched=%v, want allowed and not latched", absentAllowed, absentLatched)
	}
	onCfg := loadPaperCBPolicyConfig(t, map[string]interface{}{"paper_circuit_breakers": true}, paperCBPolicyStrategy("paper-default", "paper", nil))
	rootOnAllowed, rootOnLatched := paperCBPolicyCheckRisk(t, onCfg.Strategies[0])
	if rootOnAllowed || !rootOnLatched {
		t.Fatalf("paper strategy with paper_circuit_breakers=true: allowed=%v latched=%v, want blocked and latched", rootOnAllowed, rootOnLatched)
	}

	inspectDefault := formatStrategyInspection(defaultCfg.Strategies[0], map[string]bool{}, defaultCfg, nil)
	if !strings.Contains(inspectDefault, "off (paper_circuit_breakers is not true)") || strings.Contains(inspectDefault, "off (explicit)") {
		t.Fatalf("inspect of a root-disabled paper breaker must name the root key:\n%s", inspectDefault)
	}
	inspectOff := formatStrategyInspection(byID["paper-off"], map[string]bool{"circuit_breaker": true}, cfg, nil)
	if !strings.Contains(inspectOff, "off (explicit)") {
		t.Fatalf("inspect of an explicit circuit_breaker=false must say explicit:\n%s", inspectOff)
	}
	inspectLive := formatStrategyInspection(byID["live-default"], map[string]bool{}, cfg, nil)
	if strings.Contains(inspectLive, "circuit_breaker:     off") {
		t.Fatalf("inspect of a live strategy must show no breaker off line:\n%s", inspectLive)
	}
}

func TestPaperCircuitBreakerAlertGateLoadedConfigIntegration(t *testing.T) {
	cfg := loadPaperCBPolicyConfig(t, map[string]interface{}{"notify_paper_circuit_breakers": false},
		paperCBPolicyStrategy("live-default", "live", nil),
		paperCBPolicyStrategy("paper-default", "paper", nil),
	)
	byID := map[string]StrategyConfig{}
	for _, sc := range cfg.Strategies {
		byID[sc.ID] = sc
	}
	if !perStrategyCircuitBreakerAlertEnabled(cfg, byID["live-default"]) {
		t.Fatal("a live strategy must alert with notify_paper_circuit_breakers=false")
	}
	if perStrategyCircuitBreakerAlertEnabled(cfg, byID["paper-default"]) {
		t.Fatal("a paper strategy must not alert with notify_paper_circuit_breakers=false")
	}
	sourced := byID["paper-default"]
	sourced.PaperSource = "x"
	if perStrategyCircuitBreakerAlertEnabled(cfg, sourced) {
		t.Fatal("a paper_source strategy must not alert with the key false")
	}
	absentCfg := loadPaperCBPolicyConfig(t, nil,
		paperCBPolicyStrategy("paper-default", "paper", nil),
	)
	absentSourced := absentCfg.Strategies[0]
	absentSourced.PaperSource = "x"
	if perStrategyCircuitBreakerAlertEnabled(absentCfg, absentSourced) {
		t.Fatal("a paper_source strategy must not alert with the key absent")
	}
	if perStrategyCircuitBreakerAlertEnabled(nil, byID["paper-default"]) {
		t.Fatal("a nil config must not alert for paper")
	}
	if !perStrategyCircuitBreakerAlertEnabled(nil, byID["live-default"]) {
		t.Fatal("a nil config must still alert for live")
	}

	onCfg := loadPaperCBPolicyConfig(t, map[string]interface{}{"notify_paper_circuit_breakers": true},
		paperCBPolicyStrategy("paper-default", "paper", nil),
	)
	if !perStrategyCircuitBreakerAlertEnabled(onCfg, onCfg.Strategies[0]) {
		t.Fatal("a paper strategy must alert with notify_paper_circuit_breakers=true")
	}
	if !perStrategyCircuitBreakerAlertEnabled(onCfg, sourced) {
		t.Fatal("a paper_source strategy must alert with the key true")
	}
}
