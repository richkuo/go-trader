//go:build pyintegration

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"testing"
	"time"
)

var xsmomMembers = []basketTestMember{
	{ID: "hl-bsk-avax", Coin: "AVAX", Mark: 30},
	{ID: "hl-bsk-btc", Coin: "BTC", Mark: 60000},
	{ID: "hl-bsk-doge", Coin: "DOGE", Mark: 0.2},
	{ID: "hl-bsk-eth", Coin: "ETH", Mark: 3000},
	{ID: "hl-bsk-link", Coin: "LINK", Mark: 15},
	{ID: "hl-bsk-sol", Coin: "SOL", Mark: 3000},
}

var xsmomLots = map[string]int{"AVAX": 2, "BTC": 5, "DOGE": 0, "ETH": 4, "LINK": 1, "SOL": 2}

type xsmomSeries struct {
	Drift  float64
	Swing  float64
	Bars   int
	Closes []float64
	Stale  bool
}

var xsmomBaseSeries = map[string]xsmomSeries{
	"BTC":  {Drift: 0.004, Swing: 0.005},
	"ETH":  {Drift: 0.003, Swing: 0.015},
	"SOL":  {Drift: 0.003, Swing: 0.004},
	"LINK": {Drift: 0.0, Swing: 0.008},
	"AVAX": {Drift: -0.002, Swing: 0.01},
	"DOGE": {Drift: -0.003, Swing: 0.01},
}

func useXSMomMembers(t *testing.T) {
	t.Helper()
	prevMembers, prevLots := basketTestMembers, basketTestLots
	basketTestMembers, basketTestLots = xsmomMembers, xsmomLots
	t.Cleanup(func() { basketTestMembers, basketTestLots = prevMembers, prevLots })
}

func xsmomSeriesWith(edit func(map[string]xsmomSeries)) map[string]xsmomSeries {
	out := make(map[string]xsmomSeries, len(xsmomBaseSeries))
	for k, v := range xsmomBaseSeries {
		out[k] = v
	}
	if edit != nil {
		edit(out)
	}
	return out
}

func xsmomCloses(mark float64, s xsmomSeries, bars int) []float64 {
	if s.Closes != nil {
		return s.Closes
	}
	out := make([]float64, bars)
	lnMark := math.Log(mark)
	for i := 0; i < bars; i++ {
		back := bars - 1 - i
		sign := 1.0
		if back%2 == 1 {
			sign = -1.0
		}
		out[i] = math.Exp(lnMark - float64(back)*s.Drift + s.Swing*(sign-1))
	}
	return out
}

func xsmomSnapshot(deadline time.Time, series map[string]xsmomSeries, generation uint64, forming float64) *marketSnapshot {
	sealedAt := deadline.Add(30 * time.Second)
	snap := &marketSnapshot{
		Version:          marketSnapshotVersion,
		EvaluationID:     fmt.Sprintf("test/%d", deadline.Unix()),
		ConfigGeneration: generation,
		SealedAt:         sealedAt,
		Deadline:         deadline,
		keys:             make(map[marketFeedKey]*marketSnapshotKey),
		mids:             make(map[string]feedMid),
		funding:          make(map[string]feedFunding),
		observations:     make(map[feedObservationKey]*marketSnapshotObservation),
	}
	const interval = int64(3_600_000)
	for _, m := range xsmomMembers {
		s, ok := series[m.Coin]
		if !ok {
			continue
		}
		bars := s.Bars
		if bars == 0 {
			bars = 60
		}
		closes := xsmomCloses(m.Mark, s, bars)
		key := feedKeyFor(m.Coin, "1h")
		entry := &marketSnapshotKey{Key: key, IntervalMs: interval}
		start := deadline.UnixMilli() - int64(len(closes))*interval
		for i, c := range closes {
			open := start + int64(i)*interval
			prev := c
			if i > 0 {
				prev = closes[i-1]
			}
			entry.Bars = append(entry.Bars, feedBar{OpenMs: open, CloseMs: open + interval - 1, HasClose: true, Open: prev, High: math.Max(prev, c) * 1.001, Low: math.Min(prev, c) * 0.999, Close: c, Volume: 100 + float64(i)})
		}
		last := closes[len(closes)-1]
		px := last * forming
		entry.Bars = append(entry.Bars, feedBar{OpenMs: deadline.UnixMilli(), CloseMs: deadline.UnixMilli() + interval - 1, Open: last, High: math.Max(last, px) * 1.002, Low: math.Min(last, px) * 0.998, Close: px, Volume: 50})
		entry.Readiness = feedKeyReadiness{Key: key, Bars: len(entry.Bars), Required: len(entry.Bars), Ready: true, Status: feedStatusReady, Source: "test", Stale: s.Stale}
		if s.Stale {
			entry.Readiness.StaleReason = "recorded gap"
		}
		snap.keys[key] = entry
		snap.mids[m.Coin] = feedMid{Px: last, RecvAt: sealedAt, Source: "test"}
	}
	return snap
}

func xsmomConfig(t *testing.T, edit func(b map[string]interface{})) *Config {
	t.Helper()
	return loadBasketTestConfig(t, func(root map[string]interface{}) {
		b := basketTestBasket(root)
		b["ranker"] = "cross_sectional_momentum"
		b["params"] = map[string]interface{}{"quantile": 0.4}
		b["rebalance_interval_seconds"] = 7200
		b["evaluation_window_seconds"] = 7140
		b["gross_budget_usd"] = 4000
		b["max_coin_notional_usd"] = 1200
		b["max_transition_net_usd"] = 1100
		b["min_eligible_members"] = 4
		if edit != nil {
			edit(b)
		}
	})
}

func xsmomListing(b map[string]interface{}, coin string, fact map[string]interface{}) {
	b["eligibility"].(map[string]interface{})["listings"].(map[string]interface{})[coin] = fact
}

func xsmomWaterFill(vols map[string]float64, capFraction float64) map[string]float64 {
	out := make(map[string]float64, len(vols))
	free := make([]string, 0, len(vols))
	for sym := range vols {
		free = append(free, sym)
	}
	sort.Strings(free)
	budget := 0.5
	for len(free) > 0 {
		var total float64
		for _, sym := range free {
			total += 1 / vols[sym]
		}
		var keep, over []string
		for _, sym := range free {
			if budget*(1/vols[sym])/total > capFraction {
				over = append(over, sym)
			} else {
				keep = append(keep, sym)
			}
		}
		if len(over) == 0 {
			for _, sym := range free {
				out[sym] = budget * (1 / vols[sym]) / total
			}
			break
		}
		for _, sym := range over {
			out[sym] = capFraction
		}
		budget -= capFraction * float64(len(over))
		free = keep
	}
	return out
}

func xsmomRun(t *testing.T, cfg *Config, snap *marketSnapshot, rank basketRankRunner) (*basketTestEnv, *basketRebalance) {
	t.Helper()
	env := newBasketTestEnv(t, cfg, rank, nil)
	deadline := snap.Deadline
	env.cycle(deadline.Add(time.Minute), deadline, snap, true)
	return env, env.rt().Rebalances[deadline.UnixMilli()]
}

func xsmomAssertNoTrades(t *testing.T, env *basketTestEnv) {
	t.Helper()
	for _, m := range basketTestMembers {
		if q := env.signedQty(m.ID, m.Coin); q != 0 {
			t.Fatalf("%s traded %.10g during a whole-set hold", m.ID, q)
		}
	}
	if n := env.dbCount(`SELECT COUNT(*) FROM basket_member_actions`); n != 0 {
		t.Fatalf("member actions %d, want 0 during a whole-set hold", n)
	}
}

func TestBasketRealRankerMomentumFreezesCappedInverseVolatilityTargets(t *testing.T) {
	root := basketPyRoot(t)
	useXSMomMembers(t)
	deadline := basketPyDeadline()
	delisted := deadline.Add(-time.Hour).UTC().Format(time.RFC3339)
	cases := []struct {
		name    string
		edit    func(b map[string]interface{})
		series  map[string]xsmomSeries
		longs   []string
		shorts  []string
		middle  []string
		exclude string
	}{
		{name: "ties, middle members and cap redistribution", series: xsmomSeriesWith(nil),
			longs: []string{"BTC", "ETH"}, shorts: []string{"AVAX", "DOGE"}, middle: []string{"LINK", "SOL"}},
		{name: "a very small positive net bound", edit: func(b map[string]interface{}) { b["max_transition_net_usd"] = 0.01 }, series: xsmomSeriesWith(nil),
			longs: []string{"BTC", "ETH"}, shorts: []string{"AVAX", "DOGE"}, middle: []string{"LINK", "SOL"}},
		{name: "a listing-excluded member with no frame", edit: func(b map[string]interface{}) {
			xsmomListing(b, "LINK", map[string]interface{}{"listed_at": "2020-01-01T00:00:00Z", "known_at": delisted, "delisted_at": delisted})
		}, series: xsmomSeriesWith(func(s map[string]xsmomSeries) { delete(s, "LINK") }),
			longs: []string{"BTC", "ETH"}, shorts: []string{"AVAX", "DOGE"}, middle: []string{"SOL"}, exclude: "LINK"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := xsmomConfig(t, tc.edit)
			t.Chdir(root)
			_, rb := xsmomRun(t, cfg, xsmomSnapshot(deadline, tc.series, 5, 1.0), nil)
			if rb == nil || rb.HoldReason != "" || (rb.Status != basketRebalanceFrozen && rb.Status != basketRebalanceExecuting && rb.Status != basketRebalanceCompleted) {
				t.Fatalf("the coordinator must freeze the momentum targets without a cap or net hold: %+v", rb)
			}
			targets, err := rb.Targets()
			if err != nil || targets == nil {
				t.Fatalf("targets: %v", err)
			}
			capFraction := cfg.Baskets[0].MaxCoinNotionalUSD / cfg.Baskets[0].GrossBudgetUSD
			vols := map[string]float64{}
			for coin, s := range tc.series {
				vols[coin] = 2 * s.Swing
			}
			pick := func(coins []string) map[string]float64 {
				out := map[string]float64{}
				for _, c := range coins {
					out[c] = vols[c]
				}
				return out
			}
			want := map[string]float64{}
			for c, w := range xsmomWaterFill(pick(tc.longs), capFraction) {
				want[c] = w
			}
			for c, w := range xsmomWaterFill(pick(tc.shorts), capFraction) {
				want[c] = -w
			}
			for _, c := range tc.middle {
				want[c] = 0
			}
			var long, short, net float64
			for _, m := range basketTestMembers {
				mt := targets.Members[m.ID]
				if m.Coin == tc.exclude {
					if mt.Excluded == "" || mt.Weight != 0 {
						t.Fatalf("%s must stay listing-excluded with no allocation: %+v", m.Coin, mt)
					}
					continue
				}
				if math.Abs(mt.Weight-want[m.Coin]) > 1e-10 {
					t.Fatalf("%s weight %.17g, independently calculated %.17g", m.Coin, mt.Weight, want[m.Coin])
				}
				if math.Abs(mt.Weight)*cfg.Baskets[0].GrossBudgetUSD > cfg.Baskets[0].MaxCoinNotionalUSD*(1+1e-9) {
					t.Fatalf("%s weight %.17g breaks the per-coin cap", m.Coin, mt.Weight)
				}
				wantScore := math.Exp(24*tc.series[m.Coin].Drift) - 1
				if math.Abs(mt.Score-wantScore) > 1e-9*math.Max(1, math.Abs(wantScore)) {
					t.Fatalf("%s score %.17g, independently calculated %.17g", m.Coin, mt.Score, wantScore)
				}
				if mt.Weight > 0 {
					long += mt.Weight
				} else {
					short += mt.Weight
				}
				net += mt.Weight
			}
			if long != 0.5 || short != -0.5 || net != 0 {
				t.Fatalf("the side sums are %.17g and %.17g with net %.17g; want exactly +0.5, -0.5 and 0", long, short, net)
			}
			if targets.Members["hl-bsk-sol"].Score != targets.Members["hl-bsk-eth"].Score {
				t.Fatalf("setup: ETH and SOL must tie on score, got %.17g and %.17g", targets.Members["hl-bsk-eth"].Score, targets.Members["hl-bsk-sol"].Score)
			}
			if !strings.Contains(rb.EvidenceJSON, `"status":"eligible"`) || len(rb.InputFP) != 64 {
				t.Fatalf("the frozen evidence is missing: fp=%q evidence=%s", rb.InputFP, rb.EvidenceJSON)
			}
		})
	}
}

func TestBasketRealRankerMomentumHoldsTheWholeSetOnBadInputs(t *testing.T) {
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
	cases := []struct {
		name   string
		edit   func(b map[string]interface{})
		series map[string]xsmomSeries
		want   string
	}{
		{"infeasible side under the cap", func(b map[string]interface{}) { b["max_coin_notional_usd"] = 900 }, xsmomSeriesWith(nil), "cannot reach 0.5"},
		{"short history on a middle member", func(b map[string]interface{}) {
			b["eligibility"].(map[string]interface{})["min_history_bars"] = 10
			xsmomListing(b, "LINK", map[string]interface{}{"listed_at": recent, "known_at": recent})
		}, xsmomSeriesWith(func(s map[string]xsmomSeries) { v := s["LINK"]; v.Bars = 20; s["LINK"] = v }), "LINK: the frame carries 20 closed bars, cross_sectional_momentum needs 25"},
		{"non-positive price", nil, xsmomSeriesWith(func(s map[string]xsmomSeries) { v := s["SOL"]; v.Closes = nonPositive; s["SOL"] = v }), "non-positive price"},
		{"zero volatility on a middle member", nil, xsmomSeriesWith(func(s map[string]xsmomSeries) { v := s["LINK"]; v.Closes = flat; s["LINK"] = v }), "LINK: the trailing volatility 0.0 is zero or not finite"},
		{"non-finite trailing return on a selected member", nil, xsmomSeriesWith(func(s map[string]xsmomSeries) { v := s["DOGE"]; v.Closes = explosive; s["DOGE"] = v }), "DOGE: the trailing return is not finite"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := xsmomConfig(t, tc.edit)
			t.Chdir(root)
			env, rb := xsmomRun(t, cfg, xsmomSnapshot(deadline, tc.series, 5, 1.0), nil)
			if rb == nil || rb.Status != basketRebalanceHeld || rb.TargetsJSON != "" {
				t.Fatalf("want a whole-set hold with no frozen targets, got %+v", rb)
			}
			if !strings.Contains(rb.HoldReason, tc.want) {
				t.Fatalf("the hold must name %q, got %q", tc.want, rb.HoldReason)
			}
			xsmomAssertNoTrades(t, env)
		})
	}
}

func xsmomRecordingRanker(edit func(req map[string]interface{}), out *[]byte) basketRankRunner {
	return func(ctx context.Context, stdin []byte, timeout time.Duration) ([]byte, []byte, error) {
		if edit != nil {
			var req map[string]interface{}
			if err := json.Unmarshal(stdin, &req); err != nil {
				return nil, nil, err
			}
			edit(req)
			b, err := json.Marshal(req)
			if err != nil {
				return nil, nil, err
			}
			stdin = b
		}
		stdout, stderr, err := runBasketRankerProcess(ctx, stdin, timeout)
		*out = append([]byte(nil), stdout...)
		return stdout, stderr, err
	}
}

func TestBasketRealRankerMomentumRefusesInvalidConfigurationBeforeMemberData(t *testing.T) {
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
				cfg := xsmomConfig(t, func(b map[string]interface{}) {
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

func TestBasketRealRankerMomentumIgnoresBarsAfterTheDeadline(t *testing.T) {
	root := basketPyRoot(t)
	useXSMomMembers(t)
	deadline := basketPyDeadline()
	type frozen struct {
		fp      string
		weights map[string]float64
		scores  map[string]float64
	}
	run := func(forming float64) frozen {
		cfg := xsmomConfig(t, nil)
		t.Chdir(root)
		_, rb := xsmomRun(t, cfg, xsmomSnapshot(deadline, xsmomSeriesWith(nil), 9, forming), nil)
		targets, err := rb.Targets()
		if rb == nil || err != nil || targets == nil {
			t.Fatalf("forming bar x%v: no frozen targets: %+v (%v)", forming, rb, err)
		}
		out := frozen{fp: rb.InputFP, weights: map[string]float64{}, scores: map[string]float64{}}
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
