package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type sweepAbsorptionParityCase struct {
	Name          string                 `json:"name"`
	Params        map[string]interface{} `json:"params"`
	Support       map[string]int64       `json:"support"`
	ErrorContains string                 `json:"error_contains"`
}

func TestSweepAbsorptionSupportEqualsTheSharedParityFixture(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "sweep_absorption_support_parity.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Schema string                      `json:"schema"`
		Cases  []sweepAbsorptionParityCase `json:"cases"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Schema != "sweep_absorption_support_parity/1" || len(doc.Cases) < 10 {
		t.Fatalf("unexpected parity fixture %q with %d cases", doc.Schema, len(doc.Cases))
	}
	for _, c := range doc.Cases {
		t.Run(c.Name, func(t *testing.T) {
			got, err := sweepAbsorptionSupportFor(c.Params)
			if c.ErrorContains != "" {
				if err == nil || !strings.Contains(err.Error(), c.ErrorContains) {
					t.Fatalf("want an error containing %q, got %+v, %v", c.ErrorContains, got, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]int64{"session_ms": got.SessionMs, "session_bars": got.SessionBars, "prelude_bars": got.PreludeBars,
				"post_bars": got.PostBars, "decision_bars": got.DecisionBars, "flow_window_ms": got.FlowWindowMs, "htf_rows": got.HTFRows}
			for k, v := range c.Support {
				if want[k] != v {
					t.Fatalf("%s: Go %d != Python %d", k, want[k], v)
				}
			}
			if len(c.Support) != len(want) {
				t.Fatalf("fixture fields %v do not cover %v", c.Support, want)
			}
		})
	}
}

func sweepAbsorptionConfig(t *testing.T, feed string, mutate func(sc map[string]interface{}), extra ...map[string]interface{}) (*Config, error) {
	t.Helper()
	sc := researchFixtureArgs(researchScalarFixture(t, sweepAbsorptionStrategyName), sweepAbsorptionStrategyName, []string{"--mode=paper"})
	if mutate != nil {
		mutate(sc)
	}
	strategies := append([]map[string]interface{}{sc}, extra...)
	path := writeNoEdgeConfigRoot(t, CurrentConfigVersion, map[string]interface{}{"market_feed": feed}, strategies...)
	return LoadConfig(path)
}

func TestSweepAbsorptionFeedRequirementsAndAdmission(t *testing.T) {
	setNoEdgeLiveCredentials(t)
	cfg, err := sweepAbsorptionConfig(t, marketFeedWebsocket, nil)
	if err != nil {
		t.Fatalf("explicit paper on the websocket route with closed-bar decisions must load: %v", err)
	}
	sc := cfg.Strategies[0]
	if sc.HTFFilter || !sc.ClosedBarDecisions {
		t.Fatalf("the add-command row must enable closed-bar decisions and leave the optional EMA filter off: %+v", sc)
	}
	req, err := deriveFeedRequirements(cfg)
	if err != nil {
		t.Fatal(err)
	}
	entry := req.Strategies[sc.ID]
	support, err := sweepAbsorptionSupportFor(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !entry.TradeFlow || entry.TradeWindowMs != support.FlowWindowMs || req.Observations[tradeFlowKeyFor("BTC")] != support.FlowWindowMs {
		t.Fatalf("trade-flow need = %v %d (observations %v), want %d", entry.TradeFlow, entry.TradeWindowMs, req.Observations, support.FlowWindowMs)
	}
	if entry.HasHTF {
		t.Fatal("the optional EMA filter is off, so its HTF key must stay off")
	}
	htfKey := feedKeyFor("BTC", sweepAbsorptionHTFTimeframe)
	if !entry.HasResearchHTF || entry.ResearchHTF != htfKey || entry.ResearchHTFRow != int(support.HTFRows) || req.Keys[htfKey] != int(support.HTFRows) {
		t.Fatalf("research HTF need = %+v, keys %v", entry, req.Keys)
	}

	cvd := researchFixtureArgs(researchScalarFixture(t, cumulativeVolumeDeltaStrategyName), cumulativeVolumeDeltaStrategyName, []string{"--mode=paper"})
	cvd["id"] = "hl-cvddiv-btc-mixed"
	mixed, err := sweepAbsorptionConfig(t, marketFeedWebsocket, nil, cvd)
	if err != nil {
		t.Fatal(err)
	}
	mixedReq, err := deriveFeedRequirements(mixed)
	if err != nil {
		t.Fatal(err)
	}
	cvdWindow := mixedReq.Strategies["hl-cvddiv-btc-mixed"].TradeWindowMs
	if cvdWindow != 4*3_600_000+300_000+feedTradeIntervalMs {
		t.Fatalf("the CVD window must stay unchanged, got %d", cvdWindow)
	}
	if mixedReq.Observations[tradeFlowKeyFor("BTC")] != support.FlowWindowMs {
		t.Fatalf("mixed needs must take the larger window, got %d", mixedReq.Observations[tradeFlowKeyFor("BTC")])
	}

	cfg.MarketFeed = marketFeedREST
	if err := validateMarketFeedConfig(cfg); err == nil || !strings.Contains(err.Error(), "cannot supply trade-flow observations") || !strings.Contains(err.Error(), sweepAbsorptionStrategyName) {
		t.Fatalf("the REST feed must refuse the trade-flow candidate, got %v", err)
	}

	if _, err := sweepAbsorptionConfig(t, marketFeedWebsocket, func(sc map[string]interface{}) { delete(sc, "closed_bar_decisions") }); err == nil || !strings.Contains(err.Error(), "closed_bar_decisions") {
		t.Fatalf("the candidate must refuse a config without closed-bar decisions, got %v", err)
	}
	_, err = sweepAbsorptionConfig(t, marketFeedWebsocket, func(sc map[string]interface{}) {
		sc["open_strategy"] = map[string]interface{}{"name": sweepAbsorptionStrategyName, "params": map[string]interface{}{"sweep_window": 57, "setup_expiry": 36}}
	})
	if err == nil || !strings.Contains(err.Error(), "decision window") {
		t.Fatalf("an unsupported support horizon must be refused before evaluation, got %v", err)
	}
	_, err = sweepAbsorptionConfig(t, marketFeedWebsocket, func(sc map[string]interface{}) {
		sc["args"] = []string{sweepAbsorptionStrategyName, "BTC", "15m", "--mode=paper"}
		sc["interval_seconds"] = 900
	})
	if err == nil || !strings.Contains(err.Error(), "expected_interval_ms") {
		t.Fatalf("a 15m run with the 5m registry default must be refused, got %v", err)
	}
}

func sweepAbsorptionSnapshot(withHTF bool) *marketSnapshot {
	sealedAt := time.UnixMilli(1_767_312_000_000 + 9*3_600_000 + 5_000).UTC()
	snap := &marketSnapshot{
		Version:      marketSnapshotVersion,
		EvaluationID: fmt.Sprintf("sai/%d", sealedAt.UnixMilli()),
		SealedAt:     sealedAt,
		Deadline:     sealedAt,
		keys:         make(map[marketFeedKey]*marketSnapshotKey),
		mids:         make(map[string]feedMid),
		funding:      make(map[string]feedFunding),
		observations: make(map[feedObservationKey]*marketSnapshotObservation),
	}
	add := func(tf string, interval int64, total int) {
		key := feedKeyFor("BTC", tf)
		entry := &marketSnapshotKey{Key: key, IntervalMs: interval}
		forming := sealedAt.UnixMilli() - sealedAt.UnixMilli()%interval
		start := forming - int64(total-1)*interval
		for i := 0; i < total; i++ {
			open := start + int64(i)*interval
			entry.Bars = append(entry.Bars, feedBar{OpenMs: open, CloseMs: open + interval - 1, HasClose: i < total-1, Open: 100, High: 101, Low: 99, Close: 100, Volume: 10, RecvAt: sealedAt})
		}
		entry.Readiness = feedKeyReadiness{Key: key, Bars: total, Required: total, Ready: true, Status: feedStatusReady, Source: "test", LastRecvAt: sealedAt}
		snap.keys[key] = entry
	}
	add("5m", 300_000, 202)
	if withHTF {
		add("1h", 3_600_000, 40)
	}
	snap.mids["BTC"] = feedMid{Px: 100, RecvAt: sealedAt, Source: "test"}
	return snap
}

func TestSweepAbsorptionHTFFrameIsOptionalEntryEvidence(t *testing.T) {
	setNoEdgeLiveCredentials(t)
	cfg, err := sweepAbsorptionConfig(t, marketFeedWebsocket, nil)
	if err != nil {
		t.Fatal(err)
	}
	req, err := deriveFeedRequirements(cfg)
	if err != nil {
		t.Fatal(err)
	}
	sc := cfg.Strategies[0]
	htfID := feedKeyFor("BTC", sweepAbsorptionHTFTimeframe).PayloadID()
	for _, withHTF := range []bool{true, false} {
		ctx := &marketFeedContext{Enabled: true, Requirements: req, Snapshot: sweepAbsorptionSnapshot(withHTF)}
		if held, why := ctx.feedHoldsSignal(sc); held {
			t.Fatalf("withHTF=%v: a missing research HTF frame must not hold closes and protection: %s", withHTF, why)
		}
		blob, err := ctx.singleCheckPayload(sc)
		if err != nil {
			t.Fatalf("withHTF=%v: single payload: %v", withHTF, err)
		}
		var single struct {
			Market marketPayload `json:"market"`
		}
		if err := json.Unmarshal(blob, &single); err != nil {
			t.Fatal(err)
		}
		frame, ok := single.Market.Frames[htfID]
		if ok != withHTF {
			t.Fatalf("withHTF=%v: research HTF frame present=%v", withHTF, ok)
		}
		if ok && (frame.Timing == nil || len(frame.Rows) != int(req.Strategies[sc.ID].ResearchHTFRow)) {
			t.Fatalf("the research HTF frame needs timing and its derived rows, got %d rows timing=%v", len(frame.Rows), frame.Timing != nil)
		}
		if single.Market.TradeFlows == nil {
			t.Fatalf("withHTF=%v: the payload must still carry the trade-flow section", withHTF)
		}
		batch, err := ctx.batchPayload(hlBatchKey{Symbol: "BTC", Timeframe: "5m", OhlcvLimit: 200}, []StrategyConfig{sc})
		if err != nil {
			t.Fatalf("withHTF=%v: batch payload: %v", withHTF, err)
		}
		if _, ok := batch.Frames[htfID]; ok != withHTF {
			t.Fatalf("withHTF=%v: batch research HTF frame present=%v", withHTF, ok)
		}
	}
}
