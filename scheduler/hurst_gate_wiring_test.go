package main

import (
	"encoding/json"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

func readMainSource(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	return string(b)
}

func TestHurstGateWiredAtEveryRegimeGatedDispatchSite(t *testing.T) {
	src := readMainSource(t)
	gateSites := strings.Count(src, "applyRegimeGate(sc, storeRegime, cfg.Regime,")
	hurstSites := strings.Count(src, "advanceHurstGate(sc, storeRegime, cfg.Regime, stratState, &mu,")
	if gateSites != 6 {
		t.Fatalf("expected 6 applyRegimeGate dispatch groups, found %d — if a platform arm was added or removed, wire/unwire its Hurst arm too (#1411)", gateSites)
	}
	if hurstSites != gateSites {
		t.Fatalf("found %d applyRegimeGate sites but %d advanceHurstGate arms — every regime-gated dispatch group must carry the Hurst gate (#1411)", gateSites, hurstSites)
	}
	holds := strings.Count(src, "hurstDecision.Holds && pausedBlocksSignal(")
	if holds != gateSites {
		t.Fatalf("found %d Hurst hold arms but %d dispatch groups — every hold MUST be classified through pausedBlocksSignal so closes and reductions pass (#1411)", holds, gateSites)
	}
}

func TestHurstGateArmImmediatelyFollowsTheLabelGate(t *testing.T) {

	src := readMainSource(t)
	gateRe := regexp.MustCompile(`applyRegimeGate\(sc, storeRegime, cfg\.Regime, ([A-Za-z.]+)\)`)
	hurstRe := regexp.MustCompile(`advanceHurstGate\(sc, storeRegime, cfg\.Regime, stratState, &mu, ([A-Za-z.]+)\)`)
	gates := gateRe.FindAllStringSubmatchIndex(src, -1)
	hursts := hurstRe.FindAllStringSubmatchIndex(src, -1)
	if len(gates) != len(hursts) {
		t.Fatalf("site count mismatch: %d label gates, %d hurst arms", len(gates), len(hursts))
	}
	for i := range gates {
		if hursts[i][0] < gates[i][0] {
			t.Fatalf("site %d: the Hurst arm must come AFTER its label gate", i)
		}
		if i+1 < len(gates) && hursts[i][0] > gates[i+1][0] {
			t.Fatalf("site %d: the Hurst arm leaked past the next dispatch group's label gate", i)
		}
		gateQty := src[gates[i][2]:gates[i][3]]
		hurstQty := src[hursts[i][2]:hursts[i][3]]
		if gateQty != hurstQty {
			t.Fatalf("site %d: label gate reads posQty %q but the Hurst arm reads %q — they must observe the same position", i, gateQty, hurstQty)
		}
	}
}

func TestHurstGateNeverGatesManagementPaths(t *testing.T) {

	src := readMainSource(t)
	forbidden := []string{
		"hurstDecision.Holds && hyperliquidIsLive",
		"!hurstDecision.Holds && result.Signal == 0",
		"hurstDecision.Holds && strategyUsesTrailingTPRatchetClose",
		"hurstDecision.Holds && runHyperliquidProtectionSync",
		"hurstDecision.Holds && runHedgeSync",
	}
	for _, f := range forbidden {
		if strings.Contains(src, f) {
			t.Fatalf("the Hurst gate must never gate a management path; found %q (#1411)", f)
		}
	}

	for _, line := range strings.Split(src, "\n") {
		if strings.Contains(line, "hurstDecision.Holds") && !strings.Contains(line, "pausedBlocksSignal(") {
			t.Fatalf("unclassified use of hurstDecision.Holds: %q", strings.TrimSpace(line))
		}
	}
}

func TestHurstGateSizesTheScaleInAddQuantity(t *testing.T) {

	src := readMainSource(t)
	if !strings.Contains(src, "scaleInAddQty = q * hurstDecision.OpenSizeMult()") {
		t.Fatal("the scale-in add quantity must be scaled by the Hurst multiplier — without it a mode=size gate shrinks fresh opens but leaves adds full-size (#1411)")
	}

	if regexp.MustCompile(`defOpenNotional\s*:?=[^\n]*OpenSizeMult`).MatchString(src) {
		t.Fatal("the Hurst multiplier must scale the decided add quantity, not defOpenNotional — scaling the default would leave an explicit scale_in.add_notional_usd ungated and would move the caps off the unscaled intent (#1411)")
	}

	if !strings.Contains(src, `if result.Signal != 0 && sc.Type == "perps" && sc.AllowScaleIn {`) {
		t.Fatal("the scale-in block must stay gated on result.Signal != 0 — that is what makes the Hurst hold reach a scale-in add (#1411)")
	}
}

func TestLabelGateSemanticsUntouchedByHurstGate(t *testing.T) {

	sc := StrategyConfig{ID: "s", AllowedRegimes: []string{"trending_up"}}
	rc := hurstTestRegimeConfig(regimeClassifierComposite)
	payload := hurstPayload("medium", 0.20, true)

	labelOnly, blockedOnly := applyRegimeGate(sc, payload, rc, 0)
	sc.HurstGate = &HurstGateConfig{Enabled: true, Min: hfp(0.55)}
	labelWith, blockedWith := applyRegimeGate(sc, payload, rc, 0)
	if labelOnly != labelWith || blockedOnly != blockedWith {
		t.Fatalf("adding a hurst_gate changed the label gate: (%q,%v) -> (%q,%v)", labelOnly, blockedOnly, labelWith, blockedWith)
	}
}

func hurstValidationConfig(rc *RegimeConfig, sc StrategyConfig) *Config {
	return &Config{Regime: rc, Strategies: []StrategyConfig{sc}}
}

func assertHurstErrContains(t *testing.T, errs []string, want string) {
	t.Helper()
	for _, e := range errs {
		if strings.Contains(e, want) {
			return
		}
	}
	t.Fatalf("expected an error containing %q, got %v", want, errs)
}

func TestValidateHurstGateAcceptsAWellFormedBlock(t *testing.T) {
	cfg := hurstValidationConfig(hurstTestRegimeConfig(regimeClassifierComposite),
		hurstStrategy(&HurstGateConfig{Enabled: true, Min: hfp(0.55), DisarmMin: hfp(0.50), OnFailure: "closed"}))
	if errs := validateHurstGateConfigs(cfg); len(errs) != 0 {
		t.Fatalf("expected no errors, got %v", errs)
	}
}

func TestValidateHurstGateRejectsNonCompositeWindow(t *testing.T) {

	cfg := hurstValidationConfig(hurstTestRegimeConfig(regimeClassifierADX),
		hurstStrategy(&HurstGateConfig{Enabled: true, Min: hfp(0.55)}))
	assertHurstErrContains(t, validateHurstGateConfigs(cfg), "emitted ONLY by the \"composite\" classifier")

	rc := &RegimeConfig{Enabled: true, Windows: RegimeWindowsMap{"medium": RegimeWindowSpec{Period: 20}}}
	cfg = hurstValidationConfig(rc, hurstStrategy(&HurstGateConfig{Enabled: true, Min: hfp(0.55)}))
	assertHurstErrContains(t, validateHurstGateConfigs(cfg), "emitted ONLY by the \"composite\" classifier")

	mixed := &RegimeConfig{Enabled: true, Windows: RegimeWindowsMap{
		"medium": RegimeWindowSpec{Classifier: regimeClassifierComposite, Period: 20},
		"short":  RegimeWindowSpec{Classifier: regimeClassifierADX, Period: 10},
	}}
	ok := hurstStrategy(&HurstGateConfig{Enabled: true, Min: hfp(0.55), WindowKey: "medium"})
	if errs := validateHurstGateConfigs(hurstValidationConfig(mixed, ok)); len(errs) != 0 {
		t.Fatalf("composite window should pass, got %v", errs)
	}
	bad := hurstStrategy(&HurstGateConfig{Enabled: true, Min: hfp(0.55), WindowKey: "short"})
	assertHurstErrContains(t, validateHurstGateConfigs(hurstValidationConfig(mixed, bad)), "emitted ONLY by the \"composite\" classifier")
}

func TestValidateHurstGateRejectsMissingRegimeSurface(t *testing.T) {
	hg := &HurstGateConfig{Enabled: true, Min: hfp(0.55)}

	assertHurstErrContains(t, validateHurstGateConfigs(hurstValidationConfig(nil, hurstStrategy(hg))), "requires regime.enabled=true")

	assertHurstErrContains(t, validateHurstGateConfigs(hurstValidationConfig(&RegimeConfig{Enabled: false}, hurstStrategy(hg))), "requires regime.enabled=true")

	assertHurstErrContains(t, validateHurstGateConfigs(hurstValidationConfig(&RegimeConfig{Enabled: true, Period: 14}, hurstStrategy(hg))), "requires regime.windows")

	missing := hurstStrategy(&HurstGateConfig{Enabled: true, Min: hfp(0.55), WindowKey: "nope"})
	assertHurstErrContains(t, validateHurstGateConfigs(hurstValidationConfig(hurstTestRegimeConfig(regimeClassifierComposite), missing)), "not found in regime.windows")
}

func TestValidateHurstGateRunsEvenWhenDisabled(t *testing.T) {

	cfg := hurstValidationConfig(hurstTestRegimeConfig(regimeClassifierComposite),
		hurstStrategy(&HurstGateConfig{Enabled: false, Mode: "bogus", Min: hfp(1.4)}))
	errs := validateHurstGateConfigs(cfg)
	assertHurstErrContains(t, errs, "hurst_gate.mode must be")
	assertHurstErrContains(t, errs, "hurst_gate.min must be in (0, 1) exclusive")
}

func TestValidateHurstGateVocabulary(t *testing.T) {
	rc := hurstTestRegimeConfig(regimeClassifierComposite)

	for _, mode := range []string{"", "gate", "size", " GATE ", "Size"} {
		hg := &HurstGateConfig{Enabled: true, Mode: mode}
		if normalizeHurstGateMode(mode) == HurstGateModeSize {
			hg.SizeFloor = hfp(0.3)
		} else {
			hg.Min = hfp(0.55)
		}
		if errs := validateHurstGateConfigs(hurstValidationConfig(rc, hurstStrategy(hg))); len(errs) != 0 {
			t.Fatalf("mode %q should be valid, got %v", mode, errs)
		}
	}
	assertHurstErrContains(t, validateHurstGateConfigs(hurstValidationConfig(rc,
		hurstStrategy(&HurstGateConfig{Enabled: true, Min: hfp(0.55), OnFailure: "halt"}))), "hurst_gate.on_failure")

	badGlobal := &RegimeConfig{Enabled: true, HurstGateOnFailure: "maybe",
		Windows: RegimeWindowsMap{"medium": RegimeWindowSpec{Classifier: regimeClassifierComposite, Period: 20}}}
	assertHurstErrContains(t, validateHurstGateConfigs(&Config{Regime: badGlobal}), "regime.hurst_gate_on_failure")
}

func TestValidateHurstGateBoundOrdering(t *testing.T) {
	rc := hurstTestRegimeConfig(regimeClassifierComposite)
	check := func(hg *HurstGateConfig, want string) {
		t.Helper()
		assertHurstErrContains(t, validateHurstGateConfigs(hurstValidationConfig(rc, hurstStrategy(hg))), want)
	}
	check(&HurstGateConfig{Enabled: true}, "requires at least one of min/max")
	check(&HurstGateConfig{Enabled: true, Min: hfp(0.7), Max: hfp(0.3)}, "must be < hurst_gate.max")
	check(&HurstGateConfig{Enabled: true, DisarmMin: hfp(0.4)}, "disarm_min requires hurst_gate.min")
	check(&HurstGateConfig{Enabled: true, DisarmMax: hfp(0.6)}, "disarm_max requires hurst_gate.max")

	check(&HurstGateConfig{Enabled: true, Min: hfp(0.5), DisarmMin: hfp(0.6)}, "must be <= hurst_gate.min")
	check(&HurstGateConfig{Enabled: true, Max: hfp(0.5), DisarmMax: hfp(0.4)}, "must be >= hurst_gate.max")

	for _, hg := range []*HurstGateConfig{
		{Enabled: true, Min: hfp(0)}, {Enabled: true, Min: hfp(1)},
		{Enabled: true, Max: hfp(0)}, {Enabled: true, Max: hfp(1)},
		{Enabled: true, Min: hfp(0.5), DisarmMin: hfp(-0.1)},
		{Enabled: true, Max: hfp(0.5), DisarmMax: hfp(1.5)},
	} {
		if errs := validateHurstGateConfigs(hurstValidationConfig(rc, hurstStrategy(hg))); len(errs) == 0 {
			t.Fatalf("expected a range rejection for %+v", hg)
		}
	}

	if errs := validateHurstGateConfigs(hurstValidationConfig(rc,
		hurstStrategy(&HurstGateConfig{Enabled: true, Min: hfp(0.55), DisarmMin: hfp(0.55)}))); len(errs) != 0 {
		t.Fatalf("disarm_min == min should be legal, got %v", errs)
	}
}

func TestValidateHurstGateModeScoping(t *testing.T) {
	rc := hurstTestRegimeConfig(regimeClassifierComposite)

	assertHurstErrContains(t, validateHurstGateConfigs(hurstValidationConfig(rc,
		hurstStrategy(&HurstGateConfig{Enabled: true, Min: hfp(0.55), SizeFloor: hfp(0.5)}))), "size_floor only applies with mode=\"size\"")
	for _, v := range []float64{0, -0.2, 1.5} {
		assertHurstErrContains(t, validateHurstGateConfigs(hurstValidationConfig(rc,
			hurstStrategy(&HurstGateConfig{Enabled: true, Mode: HurstGateModeSize, SizeFloor: hfp(v)}))), "size_floor must be in (0, 1]")
	}

	assertHurstErrContains(t, validateHurstGateConfigs(hurstValidationConfig(rc,
		hurstStrategy(&HurstGateConfig{Enabled: true, Mode: HurstGateModeSize, Min: hfp(0.55)}))), "has no meaning with mode=\"size\"")

	if errs := validateHurstGateConfigs(hurstValidationConfig(rc,
		hurstStrategy(&HurstGateConfig{Enabled: true, Mode: HurstGateModeSize}))); len(errs) != 0 {
		t.Fatalf("bare mode=size should be valid, got %v", errs)
	}
}

func TestValidateHurstGateRejectsUnsupportedStrategyTypes(t *testing.T) {
	rc := hurstTestRegimeConfig(regimeClassifierComposite)
	for _, typ := range []string{"options", "manual"} {
		sc := StrategyConfig{ID: "x", Type: typ, HurstGate: &HurstGateConfig{Enabled: true, Min: hfp(0.55)}}
		assertHurstErrContains(t, validateHurstGateConfigs(hurstValidationConfig(rc, sc)), "not supported for type=")
	}
	for _, typ := range []string{"spot", "perps", "futures"} {
		sc := StrategyConfig{ID: "x", Type: typ, HurstGate: &HurstGateConfig{Enabled: true, Min: hfp(0.55)}}
		if errs := validateHurstGateConfigs(hurstValidationConfig(rc, sc)); len(errs) != 0 {
			t.Fatalf("type %q should be supported, got %v", typ, errs)
		}
	}
}

func TestHurstGateNestedUnknownKeysRejected(t *testing.T) {
	raw := []byte(`{"strategies":[{"id":"s1","hurst_gate":{"enabled":true,"min":0.55,"disarm":0.5}}]}`)
	errs := validateStrategyJSONKeys(raw)
	found := false
	for _, e := range errs {
		if strings.Contains(e, `unknown field "disarm" in hurst_gate block`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("a typo inside hurst_gate must fail loudly, got %v", errs)
	}

	if !knownStrategyConfigKeys()["hurst_gate"] {
		t.Fatal("hurst_gate must be in the reflective strategy key inventory")
	}

	known := knownHurstGateKeys()
	typ := reflect.TypeOf(HurstGateConfig{})
	for i := 0; i < typ.NumField(); i++ {
		tag := strings.SplitN(typ.Field(i).Tag.Get("json"), ",", 2)[0]
		if tag == "" || tag == "-" {
			continue
		}
		if !known[tag] {
			t.Fatalf("declared field %q missing from knownHurstGateKeys()", tag)
		}
	}

	ok := []byte(`{"strategies":[{"id":"s1","hurst_gate":{"enabled":false,"mode":"size","size_floor":0.3,"window_key":"medium","on_failure":"open","min":null,"max":null,"disarm_min":null,"disarm_max":null}}]}`)
	if errs := validateStrategyJSONKeys(ok); len(errs) != 0 {
		t.Fatalf("a fully-populated block must pass the key guard, got %v", errs)
	}
}

func TestHurstGateJSONRoundTripsThroughStrategyConfig(t *testing.T) {
	raw := `{"id":"s1","type":"perps","hurst_gate":{"enabled":true,"mode":"gate","min":0.55,"disarm_min":0.5,"window_key":"medium","on_failure":"closed"}}`
	var sc StrategyConfig
	if err := json.Unmarshal([]byte(raw), &sc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if sc.HurstGate == nil || !sc.HurstGate.Enabled || sc.HurstGate.Min == nil || *sc.HurstGate.Min != 0.55 {
		t.Fatalf("hurst_gate did not round trip: %+v", sc.HurstGate)
	}
	if sc.HurstGate.WindowKey != "medium" || sc.HurstGate.OnFailure != "closed" {
		t.Fatalf("unexpected block: %+v", sc.HurstGate)
	}
}

func TestConfigExampleShipsNoHurstThresholds(t *testing.T) {

	b, err := os.ReadFile("config.example.json")
	if err != nil {
		t.Fatalf("read config.example.json: %v", err)
	}
	if strings.Contains(string(b), "hurst") {
		t.Fatal("config.example.json must ship no hurst_gate values — the #1410 study is INCONCLUSIVE and recommends no thresholds (#1411)")
	}
}

func TestHurstGateIsHotReloadableIncludingWhileOpen(t *testing.T) {

	base := hurstStrategy(&HurstGateConfig{Enabled: true, Min: hfp(0.55)})
	edited := hurstStrategy(&HurstGateConfig{Enabled: true, Min: hfp(0.60)})
	if !reflect.DeepEqual(strategyRestartShape(base), strategyRestartShape(edited)) {
		t.Fatal("a hurst_gate edit must not be flagged restart-required")
	}

	a := &RegimeConfig{Enabled: true, HurstGateOnFailure: "open"}
	b := &RegimeConfig{Enabled: true, HurstGateOnFailure: "closed"}
	if !regimeConfigEqualIgnoringReloadableFields(a, b) {
		t.Fatal("regime.hurst_gate_on_failure must be hot-reloadable")
	}

	cfg := &Config{DBFile: "x.db", Regime: a, Strategies: []StrategyConfig{base}}
	next := &Config{DBFile: "x.db", Regime: b, Strategies: []StrategyConfig{edited}}
	if err := validateHotReloadCompatible(cfg, next); err != nil {
		t.Fatalf("hurst_gate must not be restart-required: %v", err)
	}
}

func TestCloneHurstGateConfigDeepCopies(t *testing.T) {
	src := &HurstGateConfig{Enabled: true, Min: hfp(0.55), Max: hfp(0.8), DisarmMin: hfp(0.5), DisarmMax: hfp(0.85), SizeFloor: hfp(0.3)}
	clone := cloneHurstGateConfig(src)
	if !reflect.DeepEqual(src, clone) {
		t.Fatal("clone must be value-equal")
	}
	*clone.Min = 0.9
	if *src.Min != 0.55 {
		t.Fatal("clone must not alias the source pointers")
	}
	if cloneHurstGateConfig(nil) != nil {
		t.Fatal("nil clones to nil")
	}
}

func TestApplyHotReloadConfigAdoptsHurstGateWhileOpen(t *testing.T) {
	strat := func(hg *HurstGateConfig) StrategyConfig {
		sc := hurstStrategy(hg)
		sc.Script = "x.py"
		sc.Args = []string{"a", "BTC", "1h"}
		sc.Capital = 1000
		sc.MaxDrawdownPct = 10
		sc.Leverage = 2
		sc.MarginMode = "isolated"
		return sc
	}
	build := func(hg *HurstGateConfig, globalOnFailure string) *Config {
		c := minimalReloadConfig([]StrategyConfig{strat(hg)})
		c.Regime = hurstTestRegimeConfig(regimeClassifierComposite)
		c.Regime.HurstGateOnFailure = globalOnFailure
		return c
	}

	openState := func(latch HurstGateState) *AppState {
		return &AppState{Strategies: map[string]*StrategyState{
			"s1": {
				ID: "s1", Type: "perps", Cash: 1000,
				OptionPositions: map[string]*OptionPosition{},
				Positions: map[string]*Position{"BTC": {
					Symbol: "BTC", Quantity: 0.5, AvgCost: 30000,
					Side: "long", Multiplier: 1, OwnerStrategyID: "s1",
				}},
				HurstGate: latch,
			},
		}}
	}

	t.Run("threshold edit adopted with a position open", func(t *testing.T) {
		cfg := build(&HurstGateConfig{Enabled: true, Min: hfp(0.55), DisarmMin: hfp(0.45)}, "open")
		next := build(&HurstGateConfig{Enabled: true, Min: hfp(0.60), DisarmMin: hfp(0.50)}, "closed")
		changes, err := applyHotReloadConfig(cfg, next, openState(HurstGateState{}), nil, nil)
		if err != nil {
			t.Fatalf("a hurst_gate edit must hot-reload while open, got: %v", err)
		}
		live := cfg.Strategies[0].HurstGate
		if live == nil || live.Min == nil || *live.Min != 0.60 || live.DisarmMin == nil || *live.DisarmMin != 0.50 {
			t.Fatalf("the edited hurst_gate was not adopted: %+v", live)
		}
		if normalizeRegimeGateOnFailure(cfg.Regime.HurstGateOnFailure) != HurstGateOnFailureClosed {
			t.Fatalf("regime.hurst_gate_on_failure was not adopted: %q", cfg.Regime.HurstGateOnFailure)
		}
		joined := strings.Join(changes, " | ")
		if !strings.Contains(joined, "hurst_gate:") || !strings.Contains(joined, "regime.hurst_gate_on_failure") {
			t.Fatalf("both edits must be reported to the operator, got: %v", changes)
		}

		*next.Strategies[0].HurstGate.Min = 0.99
		if *cfg.Strategies[0].HurstGate.Min != 0.60 {
			t.Fatal("the adopted block must not alias the freshly loaded config's pointers")
		}
	})

	t.Run("on_failure-only edit keeps the threshold key and the latch", func(t *testing.T) {
		cfg := build(&HurstGateConfig{Enabled: true, Min: hfp(0.55), DisarmMin: hfp(0.45)}, "")
		next := build(&HurstGateConfig{Enabled: true, Min: hfp(0.55), DisarmMin: hfp(0.45), OnFailure: "closed"}, "")
		before := hurstGateThresholdKey(cfg.Strategies[0].HurstGate, resolveHurstGateWindow(cfg.Strategies[0], cfg.Regime))
		state := openState(HurstGateState{Key: before, State: hurstGateStateArmed, LastH: 0.62, Observed: true})
		if _, err := applyHotReloadConfig(cfg, next, state, nil, nil); err != nil {
			t.Fatalf("an on_failure-only edit must hot-reload while open, got: %v", err)
		}
		adopted := cfg.Strategies[0]
		if resolveHurstGateOnFailure(adopted, cfg.Regime) != HurstGateOnFailureClosed {
			t.Fatal("the per-strategy on_failure edit was not adopted")
		}
		after := hurstGateThresholdKey(adopted.HurstGate, resolveHurstGateWindow(adopted, cfg.Regime))
		if after != before {
			t.Fatalf("on_failure must NOT be part of the threshold key: %q -> %q", before, after)
		}

		d := advanceHurstGate(adopted, hurstPayload("medium", 0.50, true), cfg.Regime, state.Strategies["s1"], nil, 0.5)
		if d.State != hurstGateStateArmed || d.Holds {
			t.Fatalf("the latch must survive an on_failure-only edit, got state=%q holds=%v", d.State, d.Holds)
		}
	})

	t.Run("threshold edit discards the stale latch on the next cycle", func(t *testing.T) {
		cfg := build(&HurstGateConfig{Enabled: true, Min: hfp(0.55), DisarmMin: hfp(0.45)}, "")
		next := build(&HurstGateConfig{Enabled: true, Min: hfp(0.60), DisarmMin: hfp(0.50)}, "")
		before := hurstGateThresholdKey(cfg.Strategies[0].HurstGate, resolveHurstGateWindow(cfg.Strategies[0], cfg.Regime))
		state := openState(HurstGateState{Key: before, State: hurstGateStateArmed, LastH: 0.62, Observed: true})
		if _, err := applyHotReloadConfig(cfg, next, state, nil, nil); err != nil {
			t.Fatalf("a threshold edit must hot-reload while open, got: %v", err)
		}
		adopted := cfg.Strategies[0]
		ss := state.Strategies["s1"]

		d := advanceHurstGate(adopted, hurstPayload("medium", 0.55, true), cfg.Regime, ss, nil, 0.5)
		if d.State != hurstGateStateDisarmed || !d.Holds {
			t.Fatalf("a threshold edit must discard the stale ARMED latch, got state=%q holds=%v", d.State, d.Holds)
		}
		if ss.HurstGate.Key == before {
			t.Fatal("the persisted latch key must be rewritten to the new threshold tuple")
		}
	})
}
