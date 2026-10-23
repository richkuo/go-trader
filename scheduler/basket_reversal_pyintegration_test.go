//go:build pyintegration

package main

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

func xsrevConfig(t *testing.T, edit func(b map[string]interface{})) *Config {
	t.Helper()
	return xsmomConfig(t, func(b map[string]interface{}) {
		b["ranker"] = "cross_sectional_reversal"
		if edit != nil {
			edit(b)
		}
	})
}

func xsrevDeadlineClose(t *testing.T, snap *marketSnapshot, coin string) float64 {
	t.Helper()
	entry := snap.keys[feedKeyFor(coin, "1h")]
	for i := len(entry.Bars) - 1; i >= 0; i-- {
		if entry.Bars[i].HasClose && entry.Bars[i].CloseMs+1 == snap.Deadline.UnixMilli() {
			return entry.Bars[i].Close
		}
	}
	t.Fatalf("%s: no closed bar ends at the deadline", coin)
	return 0
}

func xsrevExpectedWeights(series map[string]xsmomSeries, longs, shorts, middle []string, capFraction float64) map[string]float64 {
	pick := func(coins []string) map[string]float64 {
		out := map[string]float64{}
		for _, c := range coins {
			out[c] = 2 * series[c].Swing
		}
		return out
	}
	want := map[string]float64{}
	for c, w := range xsmomWaterFill(pick(longs), capFraction) {
		want[c] = w
	}
	for c, w := range xsmomWaterFill(pick(shorts), capFraction) {
		want[c] = -w
	}
	for _, c := range middle {
		want[c] = 0
	}
	return want
}

func TestBasketRealRankerReversalFreezesBottomLongTopShortTargets(t *testing.T) {
	root := basketPyRoot(t)
	useXSMomMembers(t)
	deadline := basketPyDeadline()
	delisted := deadline.Add(-time.Hour).UTC().Format(time.RFC3339)
	bottomTie := func(s map[string]xsmomSeries) { v := s["LINK"]; v.Drift = -0.002; s["LINK"] = v }
	cases := []struct {
		name    string
		edit    func(b map[string]interface{})
		series  map[string]xsmomSeries
		longs   []string
		shorts  []string
		middle  []string
		exclude string
	}{
		{name: "a top-boundary tie, middle members and cap redistribution", series: xsmomSeriesWith(nil),
			longs: []string{"AVAX", "DOGE"}, shorts: []string{"BTC", "ETH"}, middle: []string{"LINK", "SOL"}},
		{name: "ties at both tail boundaries", series: xsmomSeriesWith(bottomTie),
			longs: []string{"DOGE", "LINK"}, shorts: []string{"BTC", "ETH"}, middle: []string{"AVAX", "SOL"}},
		{name: "a very small positive net bound", edit: func(b map[string]interface{}) { b["max_transition_net_usd"] = 0.01 }, series: xsmomSeriesWith(nil),
			longs: []string{"AVAX", "DOGE"}, shorts: []string{"BTC", "ETH"}, middle: []string{"LINK", "SOL"}},
		{name: "a listing-excluded member with no frame", edit: func(b map[string]interface{}) {
			xsmomListing(b, "LINK", map[string]interface{}{"listed_at": "2020-01-01T00:00:00Z", "known_at": delisted, "delisted_at": delisted})
		}, series: xsmomSeriesWith(func(s map[string]xsmomSeries) { delete(s, "LINK") }),
			longs: []string{"AVAX", "DOGE"}, shorts: []string{"BTC", "ETH"}, middle: []string{"SOL"}, exclude: "LINK"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := xsrevConfig(t, tc.edit)
			t.Chdir(root)
			snap := xsmomSnapshot(deadline, tc.series, 5, 1.0)
			_, rb := xsmomRun(t, cfg, snap, nil)
			if rb == nil || rb.HoldReason != "" || (rb.Status != basketRebalanceFrozen && rb.Status != basketRebalanceExecuting && rb.Status != basketRebalanceCompleted) {
				t.Fatalf("the coordinator must freeze the reversal targets without a cap, gross or net hold: %+v", rb)
			}
			targets, err := rb.Targets()
			if err != nil || targets == nil {
				t.Fatalf("targets: %v", err)
			}
			bc := cfg.Baskets[0]
			want := xsrevExpectedWeights(tc.series, tc.longs, tc.shorts, tc.middle, bc.MaxCoinNotionalUSD/bc.GrossBudgetUSD)
			var long, short, net, absolute float64
			for _, m := range basketTestMembers {
				mt := targets.Members[m.ID]
				if m.Coin == tc.exclude {
					if mt.Excluded == "" || mt.Weight != 0 || mt.TargetQty != 0 {
						t.Fatalf("%s must stay listing-excluded with no allocation: %+v", m.Coin, mt)
					}
					continue
				}
				if math.Abs(mt.Weight-want[m.Coin]) > 1e-10 {
					t.Fatalf("%s weight %.17g, independently calculated %.17g", m.Coin, mt.Weight, want[m.Coin])
				}
				wantScore := math.Exp(24*tc.series[m.Coin].Drift) - 1
				if math.Abs(mt.Score-wantScore) > 1e-9*math.Max(1, math.Abs(wantScore)) {
					t.Fatalf("%s score %.17g, independently calculated %.17g: scores stay in trailing-return units", m.Coin, mt.Score, wantScore)
				}
				if math.Abs(mt.Weight)*bc.GrossBudgetUSD > bc.MaxCoinNotionalUSD*(1+1e-9) {
					t.Fatalf("%s weight %.17g breaks the per-coin cap", m.Coin, mt.Weight)
				}
				if mt.Weight > 0 {
					long += mt.Weight
				} else {
					short += mt.Weight
				}
				net += mt.Weight
				absolute += math.Abs(mt.Weight)
			}
			if long != 0.5 || short != -0.5 || net != 0 || absolute > 1 {
				t.Fatalf("the side sums are %.17g and %.17g with net %.17g and gross %.17g; want exactly +0.5, -0.5, 0 and at most 1", long, short, net, absolute)
			}
			for _, m := range basketTestMembers {
				mt := targets.Members[m.ID]
				if m.Coin == tc.exclude {
					continue
				}
				px := xsrevDeadlineClose(t, snap, m.Coin)
				step := math.Pow(10, -float64(xsmomLots[m.Coin]))
				exact := math.Abs(mt.Weight) * bc.GrossBudgetUSD / px
				if mt.Weight == 0 && mt.TargetQty != 0 || mt.Weight*mt.TargetQty < 0 {
					t.Fatalf("%s target quantity %.10g disagrees with weight %.17g", m.Coin, mt.TargetQty, mt.Weight)
				}
				if q := math.Abs(mt.TargetQty); q > exact*(1+1e-9) || q < exact-step-1e-9 {
					t.Fatalf("%s target quantity %.10g is not the lot floor of %.10g (step %g)", m.Coin, mt.TargetQty, exact, step)
				}
			}
			if !strings.Contains(rb.EvidenceJSON, `"status":"eligible"`) || len(rb.InputFP) != 64 {
				t.Fatalf("the frozen evidence is missing: fp=%q evidence=%s", rb.InputFP, rb.EvidenceJSON)
			}
		})
	}
}

func TestBasketRealRankerReversalHoldsTheWholeSetOnBadInputs(t *testing.T) {
	root := basketPyRoot(t)
	useXSMomMembers(t)
	deadline := basketPyDeadline()
	recent := deadline.Add(-20 * time.Hour).UTC().Format(time.RFC3339)
	nonPositive := xsmomCloses(3000, xsmomBaseSeries["SOL"], 60)
	nonPositive[50] = -1
	flat := make([]float64, 60)
	for i := range flat {
		flat[i] = 15
	}
	explosive := make([]float64, 60)
	for i := range explosive {
		explosive[i] = 1e-300
	}
	explosive[59] = 1e300
	overflow := func(req map[string]interface{}) {
		rows := req["market"].(map[string]interface{})["frames"].(map[string]interface{})["DOGE|1h"].(map[string]interface{})["rows"].([]interface{})
		rows[len(rows)-5].([]interface{})[4] = json.Number("1e400")
	}
	cases := []struct {
		name   string
		edit   func(b map[string]interface{})
		series map[string]xsmomSeries
		mutate func(req map[string]interface{})
		want   string
	}{
		{"infeasible side under the cap", func(b map[string]interface{}) { b["max_coin_notional_usd"] = 900 }, xsmomSeriesWith(nil), nil, "cannot reach 0.5"},
		{"short eligible history on a middle member", func(b map[string]interface{}) {
			b["eligibility"].(map[string]interface{})["min_history_bars"] = 10
			xsmomListing(b, "LINK", map[string]interface{}{"listed_at": recent, "known_at": recent})
		}, xsmomSeriesWith(func(s map[string]xsmomSeries) { v := s["LINK"]; v.Bars = 20; s["LINK"] = v }), nil, "LINK: the frame carries 20 closed bars, cross_sectional_reversal needs 25"},
		{"non-positive price on a middle member", nil, xsmomSeriesWith(func(s map[string]xsmomSeries) { v := s["SOL"]; v.Closes = nonPositive; s["SOL"] = v }), nil, "non-positive price"},
		{"non-finite price on a selected member", nil, xsmomSeriesWith(nil), overflow, "non-finite"},
		{"zero volatility on a middle member", nil, xsmomSeriesWith(func(s map[string]xsmomSeries) { v := s["LINK"]; v.Closes = flat; s["LINK"] = v }), nil, "LINK: the trailing volatility 0.0 is zero or not finite"},
		{"zero volatility on a selected member", nil, xsmomSeriesWith(func(s map[string]xsmomSeries) {
			v := s["AVAX"]
			v.Closes = make([]float64, 60)
			for i := range v.Closes {
				v.Closes[i] = 30
			}
			s["AVAX"] = v
		}), nil, "AVAX: the trailing volatility 0.0 is zero or not finite"},
		{"overflowed trailing return on a selected member", nil, xsmomSeriesWith(func(s map[string]xsmomSeries) { v := s["DOGE"]; v.Closes = explosive; s["DOGE"] = v }), nil, "DOGE: the trailing return is not finite"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := xsrevConfig(t, tc.edit)
			t.Chdir(root)
			var stdout []byte
			env, rb := xsmomRun(t, cfg, xsmomSnapshot(deadline, tc.series, 5, 1.0), xsmomRecordingRanker(tc.mutate, &stdout))
			if rb == nil || rb.Status != basketRebalanceHeld || rb.TargetsJSON != "" {
				t.Fatalf("want a whole-set hold with no frozen targets, got %+v", rb)
			}
			if !strings.Contains(rb.HoldReason, tc.want) {
				t.Fatalf("the hold must name %q, got %q", tc.want, rb.HoldReason)
			}
			var res basketRankResult
			if err := json.Unmarshal(stdout, &res); err != nil {
				t.Fatalf("the real ranker printed no JSON: %v: %s", err, stdout)
			}
			if res.Status != basketRankStatusHold || len(res.Scores) != 0 || len(res.Weights) != 0 || len(res.Exclusions) != 0 {
				t.Fatalf("a data hold carries no scores, weights or exclusions: %+v", res)
			}
			xsmomAssertNoTrades(t, env)
		})
	}
}

func TestBasketRealRankerReversalRefusesInvalidConfigurationBeforeMemberData(t *testing.T) {
	root := basketPyRoot(t)
	useXSMomMembers(t)
	deadline := basketPyDeadline()
	delisted := deadline.Add(-time.Hour).UTC().Format(time.RFC3339)
	configs := []struct {
		name    string
		edit    func(b map[string]interface{})
		invalid string
	}{
		{"valid configuration", nil, ""},
		{"lookback_bars too small for the lookbacks", func(b map[string]interface{}) {
			b["lookback_bars"] = 20
			b["eligibility"].(map[string]interface{})["min_history_bars"] = 20
		}, "lookback_bars 20 cannot supply"},
		{"volatility lookback above the configured history", func(b map[string]interface{}) {
			b["params"] = map[string]interface{}{"quantile": 0.4, "volatility_lookback_bars": 60}
		}, "the ranker needs 61 closed bars"},
		{"invalid quantile", func(b map[string]interface{}) { b["params"] = map[string]interface{}{"quantile": 0.6} }, "constraint 'quantile < 0.5' violated"},
	}
	data := []struct {
		name     string
		edit     func(b map[string]interface{})
		series   map[string]xsmomSeries
		mutate   func(req map[string]interface{})
		dataHold string
	}{
		{"short frame", nil, xsmomSeriesWith(func(s map[string]xsmomSeries) { v := s["BTC"]; v.Bars = 5; s["BTC"] = v }), nil, "BTC: the sealed frame carries"},
		{"missing frame", nil, xsmomSeriesWith(nil), func(req map[string]interface{}) {
			delete(req["market"].(map[string]interface{})["frames"].(map[string]interface{}), "ETH|1h")
		}, "no frame for ETH|1h"},
		{"stale frame", nil, xsmomSeriesWith(func(s map[string]xsmomSeries) { v := s["SOL"]; v.Stale = true; s["SOL"] = v }), nil, "SOL: the sealed frame is stale"},
		{"eligible population below the minimum", func(b map[string]interface{}) {
			b["min_eligible_members"] = 6
			xsmomListing(b, "LINK", map[string]interface{}{"listed_at": "2020-01-01T00:00:00Z", "known_at": delisted, "delisted_at": delisted})
		}, xsmomSeriesWith(func(s map[string]xsmomSeries) { delete(s, "LINK") }), nil, "the eligible population 5 is below the minimum 6"},
	}
	for _, c := range configs {
		for _, d := range data {
			t.Run(c.name+"/"+d.name, func(t *testing.T) {
				cfg := xsrevConfig(t, func(b map[string]interface{}) {
					if d.edit != nil {
						d.edit(b)
					}
					if c.edit != nil {
						c.edit(b)
					}
				})
				t.Chdir(root)
				var stdout []byte
				env, rb := xsmomRun(t, cfg, xsmomSnapshot(deadline, d.series, 5, 1.0), xsmomRecordingRanker(d.mutate, &stdout))
				var res basketRankResult
				if err := json.Unmarshal(stdout, &res); err != nil {
					t.Fatalf("the real ranker printed no JSON: %v: %s", err, stdout)
				}
				if rb == nil || rb.Status != basketRebalanceHeld || rb.TargetsJSON != "" {
					t.Fatalf("want a held deadline with no targets, got %+v", rb)
				}
				if c.invalid != "" {
					if res.Status != basketRankStatusInvalid || !strings.Contains(res.Error, c.invalid) {
						t.Fatalf("the configuration error must return invalid before the member data is read, got status %q error %q hold %q", res.Status, res.Error, res.HoldReason)
					}
					if !strings.Contains(rb.HoldReason, "the ranker refused its output") {
						t.Fatalf("the coordinator must hold through the failed-process path, got %q", rb.HoldReason)
					}
				} else if res.Status != basketRankStatusHold || res.V != basketRankRequestVersion || !strings.Contains(res.HoldReason, d.dataHold) {
					t.Fatalf("a valid configuration must hold on the member data %q, got status %q v %d hold %q error %q", d.dataHold, res.Status, res.V, res.HoldReason, res.Error)
				}
				xsmomAssertNoTrades(t, env)
			})
		}
	}
}

func TestBasketRealRankerReversalIgnoresBarsAfterTheDeadline(t *testing.T) {
	root := basketPyRoot(t)
	useXSMomMembers(t)
	deadline := basketPyDeadline()
	type frozen struct {
		fp      string
		weights map[string]float64
		scores  map[string]float64
	}
	run := func(forming float64) frozen {
		cfg := xsrevConfig(t, nil)
		t.Chdir(root)
		env, rb := xsmomRun(t, cfg, xsmomSnapshot(deadline, xsmomSeriesWith(nil), 9, forming), nil)
		if rb == nil || rb.TargetsJSON == "" {
			t.Fatalf("forming bar x%v: no frozen targets: %+v", forming, rb)
		}
		env.restartFromDisk(basketTestRanker(nil, "a recorded deadline must not be reranked"), nil)
		saved := env.rt().Rebalances[deadline.UnixMilli()]
		targets, err := saved.Targets()
		if saved == nil || err != nil || targets == nil || saved.InputFP != rb.InputFP {
			t.Fatalf("forming bar x%v: the owning store did not return the frozen targets: %+v (%v)", forming, saved, err)
		}
		out := frozen{fp: saved.InputFP, weights: map[string]float64{}, scores: map[string]float64{}}
		for id, mt := range targets.Members {
			out.weights[id], out.scores[id] = mt.Weight, mt.Score
		}
		return out
	}
	base := run(1.0)
	for _, forming := range []float64{1.5, 0.4} {
		got := run(forming)
		if got.fp != base.fp || len(got.fp) != 64 {
			t.Fatalf("a post-deadline bar change moved the persisted fingerprint: %q -> %q", base.fp, got.fp)
		}
		for id, w := range base.weights {
			if got.weights[id] != w || got.scores[id] != base.scores[id] {
				t.Fatalf("a post-deadline bar change moved %s: weight %.17g -> %.17g, score %.17g -> %.17g", id, w, got.weights[id], base.scores[id], got.scores[id])
			}
		}
	}
}

type xsrevAction struct {
	Seq          int
	Strategy     string
	Direction    string
	OrderSide    string
	PositionSide string
	Status       string
}

func xsrevActions(t *testing.T, env *basketTestEnv, deadline time.Time) []xsrevAction {
	t.Helper()
	rows, err := env.store.primary().db.Query(`SELECT seq, strategy_id, direction, order_side, position_side, status FROM basket_member_actions WHERE deadline_ms = ? ORDER BY seq, rowid`, deadline.UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []xsrevAction
	for rows.Next() {
		var a xsrevAction
		if err := rows.Scan(&a.Seq, &a.Strategy, &a.Direction, &a.OrderSide, &a.PositionSide, &a.Status); err != nil {
			t.Fatal(err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestBasketRealRankerReversalSecondDeadlineExitsMiddleMembersAndFlattensBeforeReversing(t *testing.T) {
	root := basketPyRoot(t)
	useXSMomMembers(t)
	first := basketPyDeadline()
	second := first.Add(2 * time.Hour)
	cfg := xsrevConfig(t, func(b map[string]interface{}) { b["max_transition_net_usd"] = 4000 })
	t.Chdir(root)
	env := newBasketTestEnv(t, cfg, nil, nil)

	firstSnap := xsmomSnapshot(first, xsmomSeriesWith(nil), 5, 1.0)
	env.cycle(first.Add(time.Minute), first, firstSnap, true)
	rb := env.rt().Rebalances[first.UnixMilli()]
	if rb == nil || rb.Status != basketRebalanceCompleted {
		t.Fatalf("the first deadline must complete under feasible bounds: %+v", rb)
	}
	wantFirst := map[string]float64{"AVAX": 1, "DOGE": 1, "BTC": -1, "ETH": -1, "LINK": 0, "SOL": 0}
	for _, m := range basketTestMembers {
		if got := env.signedQty(m.ID, m.Coin); math.Copysign(1, got)*wantFirst[m.Coin] < 0 || (got == 0) != (wantFirst[m.Coin] == 0) {
			t.Fatalf("after the first deadline %s holds %.10g, want side %v", m.Coin, got, wantFirst[m.Coin])
		}
	}

	secondSeries := xsmomSeriesWith(func(s map[string]xsmomSeries) {
		for coin, drift := range map[string]float64{"DOGE": 0.005, "ETH": 0.003, "SOL": 0.003, "AVAX": 0.0, "LINK": -0.003, "BTC": -0.004} {
			v := s[coin]
			v.Drift = drift
			s[coin] = v
		}
	})
	secondSnap := xsmomSnapshot(second, secondSeries, 6, 1.0)
	env.cycle(second.Add(time.Minute), second, secondSnap, true)
	rb = env.rt().Rebalances[second.UnixMilli()]
	if rb == nil || rb.Status != basketRebalanceCompleted {
		t.Fatalf("the second deadline must complete under feasible bounds: %+v", rb)
	}
	targets, err := rb.Targets()
	if err != nil || targets == nil {
		t.Fatalf("targets: %v", err)
	}
	wantSecond := map[string]float64{"BTC": 1, "LINK": 1, "DOGE": -1, "ETH": -1, "AVAX": 0, "SOL": 0}
	for _, m := range basketTestMembers {
		mt := targets.Members[m.ID]
		got := env.signedQty(m.ID, m.Coin)
		if math.Copysign(1, mt.Weight)*wantSecond[m.Coin] < 0 || (mt.Weight == 0) != (wantSecond[m.Coin] == 0) {
			t.Fatalf("%s second-deadline weight %.17g, want side %v", m.Coin, mt.Weight, wantSecond[m.Coin])
		}
		if !basketQtyEqual(got, mt.TargetQty) {
			t.Fatalf("%s holds %.10g after the second deadline, target %.10g", m.Coin, got, mt.TargetQty)
		}
	}
	actions := xsrevActions(t, env, second)
	lastReduce, firstIncrease := 0, 0
	byMember := map[string][]xsrevAction{}
	for _, a := range actions {
		if a.Status != "committed" {
			t.Fatalf("second-deadline action %+v is not committed", a)
		}
		byMember[a.Strategy] = append(byMember[a.Strategy], a)
		if a.Direction == "reduce" {
			lastReduce = a.Seq
		} else if firstIncrease == 0 {
			firstIncrease = a.Seq
		}
	}
	if lastReduce == 0 || firstIncrease == 0 || lastReduce > firstIncrease {
		t.Fatalf("every reduction must run before any increase: %+v", actions)
	}
	for id, want := range map[string][]xsrevAction{
		"hl-bsk-btc":  {{Direction: "reduce", OrderSide: "buy", PositionSide: "short"}, {Direction: "increase", OrderSide: "buy", PositionSide: "long"}},
		"hl-bsk-doge": {{Direction: "reduce", OrderSide: "sell", PositionSide: "long"}, {Direction: "increase", OrderSide: "sell", PositionSide: "short"}},
		"hl-bsk-avax": {{Direction: "reduce", OrderSide: "sell", PositionSide: "long"}},
		"hl-bsk-link": {{Direction: "increase", OrderSide: "buy", PositionSide: "long"}},
	} {
		got := byMember[id]
		if len(got) != len(want) {
			t.Fatalf("%s actions %+v, want %+v", id, got, want)
		}
		for i := range want {
			if got[i].Direction != want[i].Direction || got[i].OrderSide != want[i].OrderSide || got[i].PositionSide != want[i].PositionSide {
				t.Fatalf("%s action %d is %+v, want %+v", id, i, got[i], want[i])
			}
		}
	}
	for _, id := range []string{"hl-bsk-btc", "hl-bsk-doge", "hl-bsk-avax"} {
		closes := 0
		for _, tr := range env.state.Strategies[id].TradeHistory {
			if tr.IsClose {
				closes++
			}
		}
		if closes != 1 {
			t.Fatalf("%s booked %d closes at the second deadline, want exactly one flatten", id, closes)
		}
	}
	if q := env.signedQty("hl-bsk-avax", "AVAX"); q != 0 {
		t.Fatalf("the middle member AVAX must exit flat, holds %.10g", q)
	}
}
