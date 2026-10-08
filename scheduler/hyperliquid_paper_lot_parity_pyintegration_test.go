//go:build pyintegration

package main

import (
	"bytes"
	"encoding/json"
	"math"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

const hlLotParityDriver = `import importlib.util
import json
import os
import sys

root = sys.argv[1]
for sub in ("backtest", "shared_tools", "shared_scripts", os.path.join("platforms", "hyperliquid")):
    sys.path.insert(0, os.path.join(root, sub))

import numpy as np
import pandas as pd

spec = importlib.util.spec_from_file_location("_lot_parity_check_hl", os.path.join(root, "shared_scripts", "check_hyperliquid.py"))
check = importlib.util.module_from_spec(spec)
spec.loader.exec_module(check)
from adapter import MIN_ORDER_NOTIONAL_SAFETY_MARGIN, MIN_ORDER_NOTIONAL_USD, floor_lot_size
from backtester import FILL_COST_MODEL_VERSION, PAPER_FILL_COST_MODELS, Backtester


def execution_spec(d, taker=0.0):
    return {"taker_fee_pct": taker, "maker_fee_pct": 0.0, "half_spread_pct": 0.0, "slippage_pct": 0.0,
            "size_decimals": d, "min_notional_usd": MIN_ORDER_NOTIONAL_USD,
            "min_notional_margin": MIN_ORDER_NOTIONAL_SAFETY_MARGIN}


def frame(price, side, fractions):
    n = 2 * len(fractions) + 4
    px = np.full(n, float(price))
    df = pd.DataFrame({"open": px, "high": px, "low": px, "close": px, "volume": np.ones(n)},
                      index=pd.date_range("2026-01-01", periods=n, freq="1h"))
    df["open_action"] = [side] + ["none"] * (n - 1)
    col = [0.0] * n
    for i, f in enumerate(fractions):
        col[2 + 2 * i] = float(f)
    df["close_fraction"] = col
    return df


req = json.load(sys.stdin)
out = {"floors": [floor_lot_size(q, d) for q, d in req["floors"]], "closes": [], "entries": [], "scale_in_error": ""}

for c in req["closes"]:
    bt = Backtester(initial_capital=c["capital"], platform="hyperliquid", execution_spec=execution_spec(c["d"]))
    res = bt.run(frame(c["price"], c["side"], c["fractions"]), strategy_name="lot-parity", symbol="ETH/USDT",
                 timeframe="1h", save=False)
    trades = res["trades"]
    ex = res["execution"]
    entry_qty = sum(t["shares"] for t in trades)
    booked = [t["shares"] for t in trades if t.get("exit_reason") != "end_of_data"]
    skipped = [{"gate": s["gate"], "floored": s["floored_qty"]} for s in ex["skipped_partial_closes"]]
    live = []
    cur = entry_qty
    for f in c["fractions"]:
        decision = {"close_fraction": f, "open_action": c.get("open_action", "none"),
                    "signal": -1 if c["side"] == "long" else 1}
        gated = check.apply_venue_close_gate(decision, {"current_quantity": cur}, c["price"], c["d"],
                                             MIN_ORDER_NOTIONAL_USD, c["side"],
                                             min_notional_margin=MIN_ORDER_NOTIONAL_SAFETY_MARGIN)
        if gated is decision:
            qty = cur if f >= 1 else floor_lot_size(cur * f, c["d"])
            live.append({"kind": "booked", "qty": qty, "signal": decision["signal"]})
            cur -= qty
        else:
            detail = gated["close_gate_detail"]
            live.append({"kind": "below_lot" if detail["floored_qty"] <= 0 else "below_min_notional",
                         "qty": detail["floored_qty"], "signal": gated["signal"]})
    out["closes"].append({"entry_qty": entry_qty, "bt_booked": booked, "bt_skipped": skipped, "live": live,
                          "bt_entry_rejected": ex["rejected_entries"]})

for e in req["entries"]:
    bt = Backtester(initial_capital=e["capital"], platform="hyperliquid",
                    execution_spec=execution_spec(e["d"], e["taker"]))
    res = bt.run(frame(e["price"], "long", []), strategy_name="lot-parity", symbol="ETH/USDT", timeframe="1h",
                 save=False)
    rejected = res["execution"]["rejected_entries"]
    out["entries"].append({
        "bt_qty": res["trades"][0]["shares"] if res["trades"] else 0.0,
        "bt_rejected": rejected[0]["reason"] if rejected else "",
        "live_round": round(e["capital"] / e["price"], e["d"]),
    })

paper_model = PAPER_FILL_COST_MODELS[FILL_COST_MODEL_VERSION]
paper_spec = {"taker_fee_pct": paper_model["taker_fee_pct"], "maker_fee_pct": paper_model["tier_fee_pct"],
              "half_spread_pct": 0.0, "slippage_pct": paper_model["slippage_pct"], "size_decimals": 4,
              "min_notional_usd": MIN_ORDER_NOTIONAL_USD, "min_notional_margin": MIN_ORDER_NOTIONAL_SAFETY_MARGIN}
out["costs"] = []
for c in req["costs"]:
    n = 40
    o = np.full(n, float(c["price"]))
    h, l, cl = o + 0.5, o - 0.5, o.copy()
    kwargs = {}
    if c["kind"] == "stop":
        o[30] = h[30] = l[30] = cl[30] = float(c["move_to"])
        kwargs["stop_loss_pct"] = c["stop_frac"]
    elif c["kind"] == "tier":
        h[30] = float(c["move_to"]) + 10.0
        cl[30] = float(c["move_to"])
        kwargs["close_strategies"] = [{"name": "tiered_tp_atr", "params": {"tp_tiers": [
            {"atr_multiple": c["atr_mult"], "close_fraction": 0.5}, {"atr_multiple": 50, "close_fraction": 1.0}]}}]
    df = pd.DataFrame({"open": o, "high": h, "low": l, "close": cl, "volume": np.ones(n)},
                      index=pd.date_range("2026-01-01", periods=n, freq="1h"))
    df["open_action"] = ["none"] * n
    df.loc[df.index[25], "open_action"] = c["side"]
    df["close_fraction"] = 0.0
    if c["kind"] == "tier":
        df["atr"] = float(c["atr"])
    if c.get("no_spec"):
        bt = Backtester(initial_capital=c["capital"], platform="hyperliquid", direction="both", **kwargs)
    else:
        bt = Backtester(initial_capital=c["capital"], platform="hyperliquid", execution_spec=paper_spec,
                        direction="both", **kwargs)
    res = bt.run(df, strategy_name="cost-parity", symbol="ETH/USDT", timeframe="1h", save=False, record_events=True)
    evs = [{k: float(e[k]) if k in ("quantity", "raw_price", "effective_price", "fee_charged") else e[k]
            for k in ("kind", "timing", "quantity", "raw_price", "effective_price", "fee_charged", "reason")}
           for e in res["ledger_events"]["events"] if e["kind"] in ("open", "close")]
    out["costs"].append({"events": evs})

try:
    Backtester(initial_capital=1000.0, platform="hyperliquid", execution_spec=execution_spec(4),
               allow_scale_in=True).run(frame(100.0, "long", []), strategy_name="lot-parity", symbol="ETH/USDT",
                                        timeframe="1h", save=False)
except ValueError as exc:
    out["scale_in_error"] = str(exc)

json.dump(out, sys.stdout)
`

type hlLotParityClose struct {
	Capital    float64   `json:"capital"`
	Price      float64   `json:"price"`
	D          int       `json:"d"`
	Side       string    `json:"side"`
	OpenAction string    `json:"open_action,omitempty"`
	Fractions  []float64 `json:"fractions"`
}

type hlLotParityEntry struct {
	Capital float64 `json:"capital"`
	Price   float64 `json:"price"`
	D       int     `json:"d"`
	Taker   float64 `json:"taker"`
}

type hlLotParityStep struct {
	Kind   string  `json:"kind"`
	Qty    float64 `json:"qty"`
	Signal int     `json:"signal"`
}

type hlLotParitySkip struct {
	Gate    string  `json:"gate"`
	Floored float64 `json:"floored"`
}

type hlLotParityOutput struct {
	Floors []float64 `json:"floors"`
	Closes []struct {
		EntryQty        float64           `json:"entry_qty"`
		BTBooked        []float64         `json:"bt_booked"`
		BTSkipped       []hlLotParitySkip `json:"bt_skipped"`
		Live            []hlLotParityStep `json:"live"`
		BTEntryRejected []any             `json:"bt_entry_rejected"`
	} `json:"closes"`
	Entries []struct {
		BTQty      float64 `json:"bt_qty"`
		BTRejected string  `json:"bt_rejected"`
		LiveRound  float64 `json:"live_round"`
	} `json:"entries"`
	ScaleInError string `json:"scale_in_error"`
	Costs        []struct {
		Events []hlCostParityEvent `json:"events"`
	} `json:"costs"`
}

type hlCostParityCase struct {
	Kind     string  `json:"kind"`
	Side     string  `json:"side"`
	Capital  float64 `json:"capital"`
	Price    float64 `json:"price"`
	MoveTo   float64 `json:"move_to,omitempty"`
	StopFrac float64 `json:"stop_frac,omitempty"`
	ATR      float64 `json:"atr,omitempty"`
	ATRMult  float64 `json:"atr_mult,omitempty"`
	NoSpec   bool    `json:"no_spec,omitempty"`
}

type hlCostParityEvent struct {
	Kind           string  `json:"kind"`
	Timing         string  `json:"timing"`
	Quantity       float64 `json:"quantity"`
	RawPrice       float64 `json:"raw_price"`
	EffectivePrice float64 `json:"effective_price"`
	FeeCharged     float64 `json:"fee_charged"`
	Reason         string  `json:"reason"`
}

func hlCostParityRel(a, b float64) float64 {
	if b == 0 {
		return math.Abs(a)
	}
	return math.Abs(a-b) / math.Abs(b)
}

func hlLotParityFloorInputs() [][2]float64 {
	rng := rand.New(rand.NewSource(1716))
	var out [][2]float64
	for i := 0; i < 1500; i++ {
		d := rng.Intn(7)
		mag := math.Pow(10, float64(rng.Intn(13)-6))
		out = append(out, [2]float64{rng.Float64() * mag, float64(d)})
	}
	for d := 0; d <= 6; d++ {
		step := math.Pow(10, -float64(d))
		for k := 1; k <= 40; k++ {
			base := float64(k) * step * float64(1+k%7)
			for _, eps := range []float64{0, 1e-15, -1e-15, 1e-12, -1e-12, 1e-10, -1e-10, 1e-9, -1e-9} {
				out = append(out, [2]float64{base + eps*step, float64(d)})
			}
		}
	}
	return append(out,
		[2]float64{0.420714573875666, 4}, [2]float64{0.1682858295502664, 4}, [2]float64{0.25242874432539963, 4},
		[2]float64{0.29999999999, 4}, [2]float64{0.29999999999999, 4}, [2]float64{2.9999999999, 0},
		[2]float64{123456.78901234567, 2}, [2]float64{1e-05, 4}, [2]float64{0.00009999999999999, 4},
	)
}

func runHLLotParityDriver(t *testing.T, req any) hlLotParityOutput {
	t.Helper()
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatalf("repo root: %v", err)
	}
	python := filepath.Join(root, ".venv", "bin", "python3")
	if _, err := os.Stat(python); err != nil {
		t.Skipf("repository virtualenv missing (%v); run uv sync", err)
	}
	dir := t.TempDir()
	driver := filepath.Join(dir, "lot_parity_driver.py")
	if err := os.WriteFile(driver, []byte(hlLotParityDriver), 0o600); err != nil {
		t.Fatalf("write driver: %v", err)
	}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	cmd := exec.Command(python, "-I", driver, root)
	cmd.Dir = dir
	cmd.Stdin = bytes.NewReader(body)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("parity driver: %v\n%s", err, stderr.String())
	}
	var out hlLotParityOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("parse driver output: %v\n%s", err, stdout.String())
	}
	return out
}

func TestHLPaperLotParityWithLiveGateAndBacktester(t *testing.T) {
	prev := tradeRecorder
	tradeRecorder = nil
	t.Cleanup(func() { tradeRecorder = prev })
	floors := hlLotParityFloorInputs()
	closes := []hlLotParityClose{
		{Capital: 1000, Price: 2000, D: 4, Side: "long", Fractions: []float64{0.4, 0.4, 0.05, 0.5}},
		{Capital: 30, Price: 2000, D: 4, Side: "short", Fractions: []float64{0.5, 0.5, 1.0}},
		{Capital: 200, Price: 7.3, D: 0, Side: "long", Fractions: []float64{0.5, 0.05, 0.1, 0.75}},
		{Capital: 200, Price: 7.3, D: 0, Side: "short", Fractions: []float64{0.5, 0.05, 0.1, 0.75}},
		{Capital: 103, Price: 10.3, D: 0, Side: "long", Fractions: []float64{0.1, 0.1}},
		{Capital: 1000, Price: 2000, D: 4, Side: "long", Fractions: []float64{0.59999999998, 0.4999999999999, 0.333}},
		{Capital: 1000, Price: 2000, D: 4, Side: "short", Fractions: []float64{0.59999999999998, 0.25}},
		{Capital: 40, Price: 2000, D: 4, Side: "long", OpenAction: "short", Fractions: []float64{0.2, 0.5}},
	}
	entries := []hlLotParityEntry{
		{Capital: 1000, Price: 100, D: 2, Taker: 0.00045},
		{Capital: 841.58, Price: 2000, D: 4, Taker: 0},
	}
	costs := []hlCostParityCase{
		{Kind: "open", Side: "long", Capital: 1000, Price: 2000},
		{Kind: "open", Side: "short", Capital: 1000, Price: 2000},
		{Kind: "stop", Side: "long", Capital: 1000, Price: 100, MoveTo: 90, StopFrac: 0.03},
		{Kind: "stop", Side: "short", Capital: 1000, Price: 100, MoveTo: 110, StopFrac: 0.03},
		{Kind: "tier", Side: "long", Capital: 1000, Price: 100, MoveTo: 110, ATR: 2, ATRMult: 2},
		{Kind: "tier", Side: "long", Capital: 1000, Price: 100, MoveTo: 110, ATR: 2, ATRMult: 2, NoSpec: true},
	}
	out := runHLLotParityDriver(t, map[string]any{"floors": floors, "closes": closes, "entries": entries, "costs": costs})

	if len(out.Floors) != len(floors) {
		t.Fatalf("driver returned %d floors, want %d", len(out.Floors), len(floors))
	}
	for i, in := range floors {
		if got := hlFloorLotSize(in[0], int(in[1])); got != out.Floors[i] {
			t.Fatalf("floor(%v, %v): Go %v, adapter floor_lot_size %v", in[0], in[1], got, out.Floors[i])
		}
	}

	for i, c := range closes {
		res := out.Closes[i]
		if len(res.BTEntryRejected) != 0 || res.EntryQty <= 0 {
			t.Fatalf("case %d: backtester entry rejected %v", i, res.BTEntryRejected)
		}
		useHLLotMetadataForTest(t, map[string]int{"ETH": c.D})
		sc := StrategyConfig{ID: "hl-lot-parity", Type: "perps", Platform: "hyperliquid", Args: []string{"sma", "ETH", "1h", "--mode=paper"}, Direction: DirectionBoth, Leverage: 1, SizingLeverage: 1}
		s := &StrategyState{ID: sc.ID, Type: "perps", Platform: "hyperliquid", Cash: c.Capital, Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Side: c.Side, Quantity: res.EntryQty, InitialQuantity: res.EntryQty, AvgCost: c.Price, Multiplier: 1, Leverage: 1, OwnerStrategyID: sc.ID},
		}}
		closeSig := -1
		if c.Side == "short" {
			closeSig = 1
		}
		var paperBooked []float64
		var paperSkipped []hlLotParitySkip
		var paperSteps []hlLotParityStep
		for _, f := range c.Fractions {
			before := s.Positions["ETH"].Quantity
			d := hlVenuePartialCloseDecision(before*f, c.Price, hlLotLookup{Known: true, SzDecimals: c.D})
			n := paperDispatch(t, sc, s, hlLotTestResult("ETH", closeSig, c.Price, f, 0), c.Price)
			switch {
			case n == 1:
				q := lastTrade(t, s).Quantity
				paperBooked = append(paperBooked, q)
				paperSteps = append(paperSteps, hlLotParityStep{Kind: "booked", Qty: q, Signal: closeSig})
				if f < 1 && (d.Hold != "" || d.Qty != q) {
					t.Fatalf("case %d: executor booked %v, decision %+v", i, q, d)
				}
			case n == 0 && f < 1 && d.Hold != "":
				if pos := s.Positions["ETH"]; pos == nil || pos.Side != c.Side || pos.Quantity != before {
					t.Fatalf("case %d: held close changed the book to %+v", i, pos)
				}
				paperSkipped = append(paperSkipped, hlLotParitySkip{Gate: d.Hold, Floored: d.Qty})
				paperSteps = append(paperSteps, hlLotParityStep{Kind: d.Hold, Qty: d.Qty})
			default:
				t.Fatalf("case %d: fraction %v booked %d trades with decision %+v", i, f, n, d)
			}
		}
		if !reflect.DeepEqual(paperBooked, nilIfEmpty(res.BTBooked)) {
			t.Fatalf("case %d: paper booked %v, backtester booked %v", i, paperBooked, res.BTBooked)
		}
		if !reflect.DeepEqual(paperSkipped, skipsOrNil(res.BTSkipped)) {
			t.Fatalf("case %d: paper held %+v, backtester skipped %+v", i, paperSkipped, res.BTSkipped)
		}
		if len(res.Live) != len(paperSteps) {
			t.Fatalf("case %d: live gate steps %+v, paper steps %+v", i, res.Live, paperSteps)
		}
		for j := range paperSteps {
			p, l := paperSteps[j], res.Live[j]
			if p.Kind != l.Kind || p.Qty != l.Qty {
				t.Fatalf("case %d step %d: paper %+v, live gate %+v", i, j, p, l)
			}
			if l.Kind != "booked" && l.Signal != 0 {
				t.Fatalf("case %d step %d: the live gate recomposed a held close into signal %d", i, j, l.Signal)
			}
		}
		if c.OpenAction != "" {
			if pos := s.Positions["ETH"]; pos == nil || pos.Side != c.Side {
				t.Fatalf("case %d: a held close with open intent %q left %+v, want the %s book kept", i, c.OpenAction, pos, c.Side)
			}
		}
	}

	if len(out.Costs) != len(costs) {
		t.Fatalf("driver returned %d cost cases, want %d", len(out.Costs), len(costs))
	}
	for i, c := range costs {
		evs := out.Costs[i].Events
		if len(evs) < 1 || evs[0].Kind != "open" {
			t.Fatalf("cost case %d (%s %s): backtester events %+v, want an open first", i, c.Kind, c.Side, evs)
		}
		btOpen := evs[0]
		useHLLotMetadataForTest(t, map[string]int{"ETH": 4})
		pf := func(v float64) *float64 { return &v }
		sc := StrategyConfig{ID: "hl-cost-parity", Type: "perps", Platform: "hyperliquid", Args: []string{"sma", "ETH", "1h", "--mode=paper"}, Direction: DirectionBoth, Leverage: 1, SizingLeverage: 1}
		openSig := 1
		if c.Side == "short" {
			openSig = -1
		}
		switch c.Kind {
		case "open":
			s := &StrategyState{ID: sc.ID, Type: "perps", Platform: "hyperliquid", Cash: c.Capital, Positions: map[string]*Position{}}
			if n := paperDispatch(t, sc, s, hlLotTestResult("ETH", openSig, c.Price, 0, 0), c.Price); n != 1 {
				t.Fatalf("cost case %d: paper open booked %d trades", i, n)
			}
			open := lastTrade(t, s)
			if hlCostParityRel(open.Price, btOpen.EffectivePrice) > 1e-9 || hlCostParityRel(open.ExchangeFee/open.Quantity, btOpen.FeeCharged/btOpen.Quantity) > 1e-9 {
				t.Fatalf("cost case %d (%s open): paper price %v fee/qty %v, backtester price %v fee/qty %v", i, c.Side, open.Price, open.ExchangeFee/open.Quantity, btOpen.EffectivePrice, btOpen.FeeCharged/btOpen.Quantity)
			}
		case "stop":
			if len(evs) != 2 || evs[1].Kind != "close" || evs[1].Reason != "sl" {
				t.Fatalf("cost case %d: backtester events %+v, want an open and a stop close", i, evs)
			}
			btStop := evs[1]
			sc.StopLossPct = pf(c.StopFrac * 100)
			trigger := btOpen.EffectivePrice * (1 - c.StopFrac)
			if c.Side == "short" {
				trigger = btOpen.EffectivePrice * (1 + c.StopFrac)
			}
			s := &StrategyState{ID: sc.ID, Type: "perps", Platform: "hyperliquid", Cash: c.Capital, Positions: map[string]*Position{
				"ETH": {Symbol: "ETH", Side: c.Side, Quantity: btStop.Quantity, InitialQuantity: btStop.Quantity, AvgCost: btOpen.EffectivePrice, Multiplier: 1, Leverage: 1, OwnerStrategyID: sc.ID, StopLossTriggerPx: trigger},
			}}
			var mu sync.RWMutex
			if n, _ := applyPaperStopLossBreach(sc, s, "ETH", c.Side, btStop.RawPrice, &mu, silentStrategyLogger(sc.ID)); n != 1 {
				t.Fatalf("cost case %d: paper stop booked %d trades", i, n)
			}
			stop := lastTrade(t, s)
			if stop.Quantity != btStop.Quantity || hlCostParityRel(stop.Price, btStop.EffectivePrice) > 1e-9 || hlCostParityRel(stop.ExchangeFee, btStop.FeeCharged) > 1e-9 {
				t.Fatalf("cost case %d (%s stop): paper qty %v price %v fee %v, backtester qty %v price %v fee %v", i, c.Side, stop.Quantity, stop.Price, stop.ExchangeFee, btStop.Quantity, btStop.EffectivePrice, btStop.FeeCharged)
			}
		case "tier":
			if len(evs) < 2 || evs[1].Kind != "close" || evs[1].Timing != "intrabar_trigger_fill" || evs[1].RawPrice != evs[1].EffectivePrice {
				t.Fatalf("cost case %d: backtester events %+v, want a resting tier fill at the tier price", i, evs)
			}
			btTier := evs[1]
			s := &StrategyState{ID: sc.ID, Type: "perps", Platform: "hyperliquid", Cash: c.Capital, Positions: map[string]*Position{
				"ETH": {Symbol: "ETH", Side: c.Side, Quantity: btOpen.Quantity, InitialQuantity: btOpen.Quantity, AvgCost: btOpen.EffectivePrice, Multiplier: 1, Leverage: 1, OwnerStrategyID: sc.ID},
			}}
			if n := paperDispatch(t, sc, s, hlLotTestResult("ETH", -openSig, c.MoveTo, 0.5, btTier.RawPrice), c.MoveTo); n != 1 {
				t.Fatalf("cost case %d: paper tier fill booked %d trades", i, n)
			}
			tier := lastTrade(t, s)
			if c.NoSpec {
				paperRate := tier.ExchangeFee / (tier.Quantity * tier.Price)
				btRate := btTier.FeeCharged / (btTier.Quantity * btTier.EffectivePrice)
				if tier.Price != btTier.EffectivePrice || hlCostParityRel(paperRate, HyperliquidTakerFeePct) > 1e-9 || hlCostParityRel(btRate, HyperliquidTakerFeePct) > 1e-9 {
					t.Fatalf("cost case %d (non-spec tier fill): paper price %v fee rate %v, backtester price %v fee rate %v, want both at the taker rate %v", i, tier.Price, paperRate, btTier.EffectivePrice, btRate, HyperliquidTakerFeePct)
				}
				continue
			}
			if tier.Quantity != btTier.Quantity || tier.Price != btTier.EffectivePrice || hlCostParityRel(tier.ExchangeFee, btTier.FeeCharged) > 1e-9 {
				t.Fatalf("cost case %d (tier fill): paper qty %v price %v fee %v, backtester qty %v price %v fee %v", i, tier.Quantity, tier.Price, tier.ExchangeFee, btTier.Quantity, btTier.EffectivePrice, btTier.FeeCharged)
			}
		}
	}

	feeReserve := out.Entries[0]
	paperFeeEntry := hlVenueEntryDecision(entries[0].Capital/entries[0].Price, entries[0].Price, hlLotLookup{Known: true, SzDecimals: entries[0].D})
	if feeReserve.BTQty >= paperFeeEntry.Qty || paperFeeEntry.Qty != 10 || feeReserve.BTQty != 9.99 {
		t.Fatalf("known difference changed: backtester reserves the taker fee before flooring (bt %v, paper %v)", feeReserve.BTQty, paperFeeEntry.Qty)
	}
	rounding := out.Entries[1]
	paperRoundEntry := hlVenueEntryDecision(entries[1].Capital/entries[1].Price, entries[1].Price, hlLotLookup{Known: true, SzDecimals: entries[1].D})
	if rounding.LiveRound != 0.4208 || paperRoundEntry.Qty != 0.4207 || rounding.BTQty != paperRoundEntry.Qty {
		t.Fatalf("known difference changed: live opens round to nearest (live %v, paper %v, bt %v)", rounding.LiveRound, paperRoundEntry.Qty, rounding.BTQty)
	}
	if !strings.Contains(out.ScaleInError, "does not model scale-in") {
		t.Fatalf("known difference changed: the execution-spec backtester accepted scale-in (%q)", out.ScaleInError)
	}
	t.Logf("known differences: live opens round to nearest (%v vs paper floor %v); the backtester reserves the taker fee before it floors (%v vs paper %v); full closes have no lot or minimum gate on any path; the execution-spec backtester rejects scale-in (%s); paper tier fills book at the tier price while the gate reads the decision price",
		rounding.LiveRound, paperRoundEntry.Qty, feeReserve.BTQty, paperFeeEntry.Qty, out.ScaleInError)
}

func nilIfEmpty(v []float64) []float64 {
	if len(v) == 0 {
		return nil
	}
	return v
}

func skipsOrNil(v []hlLotParitySkip) []hlLotParitySkip {
	if len(v) == 0 {
		return nil
	}
	return v
}
