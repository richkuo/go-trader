package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestInvertOpenSignalEchoAndResolver(t *testing.T) {
	orig := runHyperliquidCheckFn
	t.Cleanup(func() { runHyperliquidCheckFn = orig })

	sc := hlBatchStrategy("hl-inv", "breakout", "ETH", "1h", func(sc *StrategyConfig) {
		sc.InvertSignal = true
		sc.Direction = DirectionShort
	})
	var refs string
	runHyperliquidCheckFn = func(script string, args []string) (*HyperliquidResult, string, error) {
		for i, arg := range args {
			if arg == "--strategy-refs" && i+1 < len(args) {
				refs = args[i+1]
			}
		}
		return &HyperliquidResult{
			Strategy: "breakout", Symbol: "ETH", Timeframe: "1h", Signal: -1, Price: 2000, Mode: "paper",
			StrategyDecisionFields: StrategyDecisionFields{OpenSignalInverted: true},
		}, "", nil
	}
	result, _, _, ok := runHyperliquidCheck(&sc, map[string]float64{"ETH": 2000}, PositionCtx{}, nil, "simple", nil, hlBatchTestLogger(), nil, nil)
	if !ok || result.Signal != -1 {
		t.Fatalf("echoed signal = %+v ok=%t, want -1", result, ok)
	}
	if !strings.Contains(refs, `"invert_open_signal":true`) {
		t.Fatalf("refs %s missing invert_open_signal", refs)
	}

	runHyperliquidCheckFn = func(string, []string) (*HyperliquidResult, string, error) {
		return &HyperliquidResult{
			Strategy: "breakout", Symbol: "ETH", Timeframe: "1h", Signal: -1, Price: 2000, Mode: "paper",
		}, "", nil
	}
	if _, _, _, ok := runHyperliquidCheck(&sc, map[string]float64{"ETH": 2000}, PositionCtx{}, nil, "simple", nil, hlBatchTestLogger(), nil, nil); ok {
		t.Fatal("missing echo was accepted")
	}

	plain := hlBatchStrategy("hl-plain", "breakout", "ETH", "1h")
	runHyperliquidCheckFn = func(string, []string) (*HyperliquidResult, string, error) {
		return &HyperliquidResult{
			Strategy: "breakout", Symbol: "ETH", Timeframe: "1h", Signal: 1, Price: 2000, Mode: "paper",
			StrategyDecisionFields: StrategyDecisionFields{OpenSignalInverted: true},
		}, "", nil
	}
	if _, _, _, ok := runHyperliquidCheck(&plain, map[string]float64{"ETH": 2000}, PositionCtx{}, nil, "simple", nil, hlBatchTestLogger(), nil, nil); ok {
		t.Fatal("unrequested echo was accepted")
	}

	degraded := degradedHyperliquidResult(sc, "ETH", "paper", "feed down", 2000)
	got, _, _, ok := finishHyperliquidCheck(&sc, map[string]float64{"ETH": 2000}, PositionCtx{}, nil, nil, hlBatchTestLogger(), degraded, "", "", scriptFailureError, true)
	if !ok || got.Signal != 0 || got.Degraded == "" {
		t.Fatalf("degraded result = %+v ok=%t, want signal 0", got, ok)
	}

	posCtx := PositionCtx{Side: "short", Quantity: 1, AvgCost: 2000, DirectionalRegime: "trending_down", DirectionCertifiedStatesAtOpen: map[string]string{"trending_down": DirectionShort}}
	policy := &RegimeDirectionalPolicy{TrendRegime: map[string]RegimeDirectionalEntry{
		"trending_down": {Direction: DirectionShort, InvertSignal: true},
	}}
	openSC := hlBatchStrategy("hl-open", "breakout", "ETH", "1h", func(sc *StrategyConfig) {
		sc.RegimeDirectionalPolicy = policy
	})
	if !hlInvertOpenSignalForCheck(openSC, posCtx, &RegimeConfig{Enabled: true}) {
		t.Fatal("open policy invert resolved false")
	}
	base := hlBatchStrategy("hl-base", "breakout", "ETH", "1h", func(sc *StrategyConfig) { sc.InvertSignal = true })
	if !hlInvertOpenSignalForCheck(base, PositionCtx{}, nil) {
		t.Fatal("base invert resolved false")
	}

	divSC := hlBatchStrategy("hl-div", "breakout", "ETH", "1h", func(sc *StrategyConfig) {
		sc.InvertSignal = true
		sc.RegimeWindowDivergence = &RegimeWindowDivergence{ShortWindow: "micro", MediumWindow: "macro", OnDivergence: onDivergenceTrustShort}
	})
	copySC := divSC
	applyCheckDirectionalOverrides(&copySC, RegimePayload{
		MultiMode: true,
		Windows: map[string]RegimeSnapshot{
			"micro": {Regime: "trending_up"},
			"macro": {Regime: "trending_down"},
		},
	}, PositionCtx{}, &RegimeConfig{Enabled: true})
	if copySC.InvertSignal {
		t.Fatal("flat divergence left invert on")
	}

	prev := getDirectionalCertStore()
	setDirectionalCertStore(&DirectionalCertSet{byKey: map[string]DirectionalCertEntry{
		certKey("ETH", "1h", "adx"): {States: map[string]string{"trending_down": DirectionShort, "ranging": DirectionLong}},
	}})
	t.Cleanup(func() { setDirectionalCertStore(prev) })
	regime := &RegimeConfig{Enabled: true, Period: 14, ADXThreshold: 20}
	flat := hlBatchStrategy("hl-flat", "breakout", "ETH", "1h", func(sc *StrategyConfig) {
		sc.RegimeDirectionalPolicy = policy
		sc.Direction = DirectionLong
	})
	gen := globalRegimeStore.resetForCycle(time.Now())
	t.Cleanup(func() { globalRegimeStore.resetForCycle(time.Now()) })
	req, okReq := strategyRegimeBundleRequest(flat, regime)
	if !okReq {
		t.Fatal("regime bundle request")
	}
	globalRegimeStore.set(&RegimeBundle{Key: req.Key, Payload: RegimePayload{Legacy: "ranging"}}, gen)
	if hlInvertOpenSignalForCheck(flat, PositionCtx{}, regime) {
		t.Fatal("store label ranging resolved invert")
	}
	mismatch := &HyperliquidResult{
		Strategy: "breakout", Symbol: "ETH", Timeframe: "1h", Signal: 1, Price: 2000, Mode: "paper",
		StrategyDecisionFields: StrategyDecisionFields{CloseFraction: 1, Regime: &RegimePayload{Legacy: "trending_down"}},
	}
	held, _, _, ok := finishHyperliquidCheck(&flat, map[string]float64{"ETH": 2000}, PositionCtx{}, regime, nil, hlBatchTestLogger(), mismatch, "", "", scriptFailureCrash, false)
	if !ok || held.Signal != 0 || held.CloseFraction != 0 {
		t.Fatalf("mismatch hold = %+v ok=%t", held, ok)
	}

	batchSC := hlBatchStrategy("hl-batch-inv", "breakout", "ETH", "1h", func(sc *StrategyConfig) { sc.InvertSignal = true })
	slot, err := buildHyperliquidBatchSlot(batchSC, PositionCtx{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	single, err := buildStrategyRefsArg(batchSC, "", true)
	if err != nil {
		t.Fatal(err)
	}
	var slotRefs map[string]any
	if err := json.Unmarshal(slot.StrategyRefs, &slotRefs); err != nil {
		t.Fatal(err)
	}
	var singleRefs map[string]any
	if err := json.Unmarshal([]byte(single[1]), &singleRefs); err != nil {
		t.Fatal(err)
	}
	if slotRefs["invert_open_signal"] != true || singleRefs["invert_open_signal"] != true {
		t.Fatalf("batch refs %s single refs %s", slot.StrategyRefs, single[1])
	}
}

func TestInvertedCloseReachesLiveAndPaper(t *testing.T) {
	origCheck := runHyperliquidCheckFn
	origExec := runHyperliquidExecuteFn
	t.Cleanup(func() {
		runHyperliquidCheckFn = origCheck
		runHyperliquidExecuteFn = origExec
	})

	cases := []struct {
		name      string
		direction string
		live      bool
		tiered    bool
	}{
		{name: "short live atr_stop", direction: DirectionShort, live: true},
		{name: "both live atr_stop", direction: DirectionBoth, live: true},
		{name: "short live unplaceable tier", direction: DirectionShort, live: true, tiered: true},
		{name: "short paper atr_stop", direction: DirectionShort},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mode := "--mode=paper"
			if tc.live {
				mode = "--mode=live"
			}
			sc := hlBatchStrategy("hl-close", "breakout", "ETH", "1h", func(sc *StrategyConfig) {
				sc.InvertSignal = true
				sc.Direction = tc.direction
				sc.Args = []string{"breakout", "ETH", "1h", mode}
				sc.CloseStrategy = &StrategyRef{Name: "atr_stop"}
				if tc.tiered {
					sc.CloseStrategy = &StrategyRef{Name: "tiered_tp_atr"}
				}
			})
			posCtx := PositionCtx{Side: "short", Quantity: 1, AvgCost: 2100, EntryATR: 10}
			if tc.tiered {
				posCtx.OnChainTPBlocked = hlOnChainTPBlockedTiersUnplaced
			}
			var refs string
			runHyperliquidCheckFn = func(script string, args []string) (*HyperliquidResult, string, error) {
				for i, arg := range args {
					if arg == "--strategy-refs" && i+1 < len(args) {
						refs = args[i+1]
					}
				}
				return &HyperliquidResult{
					Strategy: "breakout", Symbol: "ETH", Timeframe: "1h", Signal: 1, Price: 2000, Mode: strings.TrimPrefix(mode, "--mode="),
					StrategyDecisionFields: StrategyDecisionFields{CloseFraction: 1, CloseStrategy: sc.CloseStrategy.Name, OpenSignalInverted: true},
				}, "", nil
			}
			result, _, price, ok := runHyperliquidCheck(&sc, map[string]float64{"ETH": 2000}, posCtx, nil, "simple", nil, hlBatchTestLogger(), nil, nil)
			if !ok || result.Signal != 1 || result.CloseFraction != 1 {
				t.Fatalf("check result = %+v ok=%t", result, ok)
			}
			if !strings.Contains(refs, `"invert_open_signal":true`) {
				t.Fatalf("refs missing invert key: %s", refs)
			}
			if tc.tiered && strings.Contains(refs, "close_owner") {
				t.Fatalf("unplaceable tier sent close_owner: %s", refs)
			}
			if tc.live {
				var side string
				runHyperliquidExecuteFn = func(script, symbol, gotSide string, size, stopLossPct float64, cancelOID int64, prevPosQty float64, marginMode string, leverage float64, closeMode hlCloseMode, snapshot hlExecuteSnapshot, extraCancelOIDs ...int64) (*HyperliquidExecuteResult, string, error) {
					side = gotSide
					return &HyperliquidExecuteResult{Execution: &HyperliquidExecution{Action: "close", Symbol: symbol, Size: size, Fill: &HyperliquidFill{AvgPx: 2000, TotalSz: size}}}, "", nil
				}
				if _, ok := runHyperliquidExecuteOrder(sc, result, price, 1000, false, posCtx.Quantity, posCtx.Side, posCtx.AvgCost, 1, 0, nil, nil, hlExecuteSnapshot{}, hlCloseContext{}, HurstGateDecision{}, nil, silentStrategyLogger(sc.ID)); !ok || side != "buy" {
					t.Fatalf("live close side=%q ok=%t", side, ok)
				}
				return
			}
			state := &StrategyState{
				ID: sc.ID, Platform: "hyperliquid", Type: "perps", Cash: 1000, InitialCapital: 1000,
				Positions:       map[string]*Position{"ETH": {Side: "short", Quantity: 1, AvgCost: 2100, InitialQuantity: 1}},
				OptionPositions: map[string]*OptionPosition{}, TradeHistory: []Trade{},
			}
			trades, _ := executeHyperliquidResult(sc, state, result, nil, "BUY", price, nil, &Config{}, HurstGateDecision{}, silentStrategyLogger(sc.ID))
			if trades != 1 || state.Positions["ETH"] != nil && state.Positions["ETH"].Quantity != 0 {
				pos := state.Positions["ETH"]
				t.Fatalf("paper close trades=%d position=%+v", trades, pos)
			}
		})
	}
}

func TestInvertedScaleInCloseIsNotAnAdd(t *testing.T) {
	origCheck := runHyperliquidCheckFn
	t.Cleanup(func() { runHyperliquidCheckFn = origCheck })
	sc := hlBatchStrategy("hl-scale", "breakout", "ETH", "1h", func(sc *StrategyConfig) {
		sc.InvertSignal = true
		sc.AllowScaleIn = true
		sc.Direction = DirectionShort
		sc.Args = []string{"breakout", "ETH", "1h", "--mode=live"}
	})
	runHyperliquidCheckFn = func(string, []string) (*HyperliquidResult, string, error) {
		return &HyperliquidResult{
			Strategy: "breakout", Symbol: "ETH", Timeframe: "1h", Signal: 1, Price: 2000, Mode: "live",
			StrategyDecisionFields: StrategyDecisionFields{CloseFraction: 1, OpenSignalInverted: true},
		}, "", nil
	}
	result, _, _, ok := runHyperliquidCheck(&sc, map[string]float64{"ETH": 2000}, PositionCtx{Side: "short", Quantity: 1, AvgCost: 2100}, nil, "simple", nil, hlBatchTestLogger(), nil, nil)
	if !ok || result.Signal != 1 {
		t.Fatalf("check result = %+v ok=%t", result, ok)
	}
	snap := scaleInSnapshot{Side: "short", Quantity: 1, AvgCost: 2100, EntryATR: 10}
	addQty, ok, reason := perpsScaleInDecision(sc, snap, result.Signal, 2000, 1000)
	if ok || addQty != 0 || reason != "not a same-direction add" {
		t.Fatalf("scale-in = %v %v %q", addQty, ok, reason)
	}
	orig := runHyperliquidExecuteFn
	t.Cleanup(func() { runHyperliquidExecuteFn = orig })
	var side string
	runHyperliquidExecuteFn = func(script, symbol, gotSide string, size, stopLossPct float64, cancelOID int64, prevPosQty float64, marginMode string, leverage float64, closeMode hlCloseMode, snapshot hlExecuteSnapshot, extraCancelOIDs ...int64) (*HyperliquidExecuteResult, string, error) {
		side = gotSide
		return &HyperliquidExecuteResult{Execution: &HyperliquidExecution{Action: "close", Symbol: symbol, Size: size, Fill: &HyperliquidFill{AvgPx: 2000, TotalSz: size}}}, "", nil
	}
	if _, ok := runHyperliquidExecuteOrder(sc, result, 2000, 1000, false, 1, "short", 2100, 1, 0, nil, nil, hlExecuteSnapshot{}, hlCloseContext{}, HurstGateDecision{}, nil, silentStrategyLogger(sc.ID)); !ok || side != "buy" {
		t.Fatalf("scale-in close side=%q ok=%t", side, ok)
	}
}

func TestSameSideCloseIsZeroedOnce(t *testing.T) {
	notifier, mock := confirmationNotifier()
	sc := hlBatchStrategy("hl-guard", "breakout", "ETH", "1h")
	logger := silentStrategyLogger(sc.ID)
	guardSameSideClose(sc, &HyperliquidResult{Symbol: "ETH"}, "short", 0, notifier, logger)
	same := &HyperliquidResult{Symbol: "ETH", Signal: -1, StrategyDecisionFields: StrategyDecisionFields{CloseFraction: 1}}
	guardSameSideClose(sc, same, "short", 1, notifier, logger)
	if same.Signal != 0 || same.CloseFraction != 0 {
		t.Fatalf("same-side close survived: %+v", same)
	}
	guardSameSideClose(sc, &HyperliquidResult{Symbol: "ETH", Signal: -1, StrategyDecisionFields: StrategyDecisionFields{CloseFraction: 1}}, "short", 1, notifier, logger)
	if len(mock.dms) != 1 {
		t.Fatalf("owner alerts = %d, want 1", len(mock.dms))
	}
	closing := &HyperliquidResult{Symbol: "ETH", Signal: 1, StrategyDecisionFields: StrategyDecisionFields{CloseFraction: 1}}
	guardSameSideClose(sc, closing, "short", 1, notifier, logger)
	if closing.Signal != 1 || closing.CloseFraction != 1 {
		t.Fatalf("closing-side result changed: %+v", closing)
	}
	guardSameSideClose(sc, &HyperliquidResult{Symbol: "ETH"}, "short", 0, notifier, logger)
}

func TestManualInvertDoesNotChangeCloseEval(t *testing.T) {
	orig := runHyperliquidCheckFn
	t.Cleanup(func() { runHyperliquidCheckFn = orig })
	sc := StrategyConfig{
		ID: "manual-inv", Type: "manual", Platform: "hyperliquid", Symbol: "ETH",
		Script: hyperliquidCheckScript, Args: []string{"hold", "ETH", "1h", "--mode=live"},
		InvertSignal: true, CloseStrategy: &StrategyRef{Name: "atr_stop"},
	}
	var refs string
	runHyperliquidCheckFn = func(script string, args []string) (*HyperliquidResult, string, error) {
		for i, arg := range args {
			if arg == "--strategy-refs" && i+1 < len(args) {
				refs = args[i+1]
			}
		}
		return &HyperliquidResult{
			Strategy: "hold", Symbol: "ETH", Timeframe: "1h", Signal: -1, Price: 2000, Mode: "live",
			StrategyDecisionFields: StrategyDecisionFields{CloseFraction: 0.4, CloseStrategy: "atr_stop"},
		}, "", nil
	}
	ss := &StrategyState{Positions: map[string]*Position{"ETH": {Side: "long", Quantity: 1, AvgCost: 1900}}}
	fraction, _, ok := runManualCloseEval(sc, ss, &Config{}, nil, silentStrategyLogger(sc.ID), nil)
	if !ok || fraction != 0.4 {
		t.Fatalf("manual close = %v ok=%t", fraction, ok)
	}
	if strings.Contains(refs, "invert_open_signal") {
		t.Fatalf("manual refs carried invert key: %s", refs)
	}
}
