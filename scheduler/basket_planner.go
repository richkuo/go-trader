package main

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

type basketBounds struct {
	GrossUSD   float64
	NetUSD     float64
	PerCoinUSD float64
}

func basketBoundsFor(c basketContract) basketBounds {
	return basketBounds{GrossUSD: c.GrossBudgetUSD, NetUSD: c.MaxTransitionNetUSD, PerCoinUSD: c.MaxCoinNotionalUSD}
}

type basketExposure struct {
	Qty  map[string]float64
	Mark map[string]float64
}

func (e basketExposure) clone() basketExposure {
	out := basketExposure{Qty: make(map[string]float64, len(e.Qty)), Mark: e.Mark}
	for k, v := range e.Qty {
		out.Qty[k] = v
	}
	return out
}

func (e basketExposure) coins() []string {
	out := make([]string, 0, len(e.Mark))
	for c := range e.Mark {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

func (e basketExposure) notional(coin string) float64 {
	return e.Qty[coin] * e.Mark[coin]
}

func (e basketExposure) gross() float64 {
	var g float64
	for _, c := range e.coins() {
		g += math.Abs(e.notional(c))
	}
	return g
}

func (e basketExposure) net() float64 {
	var n float64
	for _, c := range e.coins() {
		n += e.notional(c)
	}
	return n
}

type basketBoundReading struct {
	Gross   float64
	Net     float64
	PerCoin map[string]float64
}

func (e basketExposure) reading() basketBoundReading {
	r := basketBoundReading{Gross: e.gross(), Net: math.Abs(e.net()), PerCoin: make(map[string]float64, len(e.Mark))}
	for _, c := range e.coins() {
		r.PerCoin[c] = math.Abs(e.notional(c))
	}
	return r
}

const basketBoundSlack = 1e-9

func basketWithin(v, limit float64) bool {
	return v <= limit*(1+basketBoundSlack)+basketBoundSlack
}

func (r basketBoundReading) violations(b basketBounds) []string {
	var out []string
	if !basketWithin(r.Gross, b.GrossUSD) {
		out = append(out, fmt.Sprintf("gross $%.2f > budget $%.2f", r.Gross, b.GrossUSD))
	}
	if !basketWithin(r.Net, b.NetUSD) {
		out = append(out, fmt.Sprintf("net $%.2f > max_transition_net_usd $%.2f", r.Net, b.NetUSD))
	}
	coins := make([]string, 0, len(r.PerCoin))
	for c := range r.PerCoin {
		coins = append(coins, c)
	}
	sort.Strings(coins)
	for _, c := range coins {
		if !basketWithin(r.PerCoin[c], b.PerCoinUSD) {
			out = append(out, fmt.Sprintf("%s $%.2f > per-coin cap $%.2f", c, r.PerCoin[c], b.PerCoinUSD))
		}
	}
	return out
}

func basketNoWorse(after, before, limit float64) bool {
	return basketWithin(after, limit) || after <= before*(1+basketBoundSlack)+basketBoundSlack
}

func basketEndpointLegal(start, end basketBoundReading, b basketBounds, increase bool) (bool, string) {
	if increase {
		if v := start.violations(b); len(v) > 0 {
			return false, "the current basket exposure is outside the elective limits: " + strings.Join(v, "; ")
		}
		if v := end.violations(b); len(v) > 0 {
			return false, "a full fill would leave " + strings.Join(v, "; ")
		}
		return true, ""
	}
	var bad []string
	if !basketNoWorse(end.Gross, start.Gross, b.GrossUSD) {
		bad = append(bad, fmt.Sprintf("gross $%.2f", end.Gross))
	}
	if !basketNoWorse(end.Net, start.Net, b.NetUSD) {
		bad = append(bad, fmt.Sprintf("net $%.2f (from $%.2f, limit $%.2f)", end.Net, start.Net, b.NetUSD))
	}
	coins := make([]string, 0, len(end.PerCoin))
	for c := range end.PerCoin {
		coins = append(coins, c)
	}
	sort.Strings(coins)
	for _, c := range coins {
		if !basketNoWorse(end.PerCoin[c], start.PerCoin[c], b.PerCoinUSD) {
			bad = append(bad, fmt.Sprintf("%s $%.2f", c, end.PerCoin[c]))
		}
	}
	if len(bad) > 0 {
		return false, "a full fill would worsen " + strings.Join(bad, "; ")
	}
	return true, ""
}

func basketSegmentLegal(start basketExposure, coin string, delta float64, b basketBounds, increase bool) (bool, string) {
	end := start.clone()
	end.Qty[coin] += delta
	return basketEndpointLegal(start.reading(), end.reading(), b, increase)
}

type basketCandidate struct {
	StrategyID   string
	Coin         string
	Direction    string
	PositionSide string
	OrderSide    string
	FullQty      float64
	CurrentQty   float64
	TargetQty    float64
}

func (c basketCandidate) closesFully() bool {
	return c.Direction == basketDirectionReduce && basketQtyEqual(c.FullQty, math.Abs(c.CurrentQty))
}

func (c basketCandidate) signedDelta(qty float64) float64 {
	if c.OrderSide == "buy" {
		return qty
	}
	return -qty
}

func basketCandidates(t *basketTargets, exposure basketExposure, members []basketContractMember, minQty func(coin string) float64) (reductions, increases []basketCandidate) {
	for _, m := range members {
		mt, ok := t.Members[m.StrategyID]
		if !ok {
			continue
		}
		q := exposure.Qty[m.Coin]
		target := mt.TargetQty
		dust := minQty(m.Coin)
		switch {
		case q > 0 && (target < 0 || target < q):
			goal := 0.0
			if target > 0 {
				goal = target
			}
			if qty := q - goal; qty > dust || goal == 0 {
				reductions = append(reductions, basketCandidate{StrategyID: m.StrategyID, Coin: m.Coin, Direction: basketDirectionReduce, PositionSide: entrySideLong, OrderSide: "sell", FullQty: qty, CurrentQty: q, TargetQty: target})
			}
		case q < 0 && (target > 0 || target > q):
			goal := 0.0
			if target < 0 {
				goal = target
			}
			if qty := goal - q; qty > dust || goal == 0 {
				reductions = append(reductions, basketCandidate{StrategyID: m.StrategyID, Coin: m.Coin, Direction: basketDirectionReduce, PositionSide: entrySideShort, OrderSide: "buy", FullQty: qty, CurrentQty: q, TargetQty: target})
			}
		}
		switch {
		case target > 0 && q >= 0 && target-q > dust:
			increases = append(increases, basketCandidate{StrategyID: m.StrategyID, Coin: m.Coin, Direction: basketDirectionIncrease, PositionSide: entrySideLong, OrderSide: "buy", FullQty: target - q, CurrentQty: q, TargetQty: target})
		case target < 0 && q <= 0 && q-target > dust:
			increases = append(increases, basketCandidate{StrategyID: m.StrategyID, Coin: m.Coin, Direction: basketDirectionIncrease, PositionSide: entrySideShort, OrderSide: "sell", FullQty: q - target, CurrentQty: q, TargetQty: target})
		}
	}
	sortCandidates := func(cs []basketCandidate) {
		sort.SliceStable(cs, func(i, j int) bool {
			ni := cs[i].FullQty * exposure.Mark[cs[i].Coin]
			nj := cs[j].FullQty * exposure.Mark[cs[j].Coin]
			if ni != nj {
				return ni > nj
			}
			return cs[i].StrategyID < cs[j].StrategyID
		})
	}
	sortCandidates(reductions)
	sortCandidates(increases)
	return reductions, increases
}

func basketOrderIncreases(cs []basketCandidate, exposure basketExposure) []basketCandidate {
	out := append([]basketCandidate(nil), cs...)
	net := exposure.net()
	sort.SliceStable(out, func(i, j int) bool {
		ri := basketNetRelief(out[i], net)
		rj := basketNetRelief(out[j], net)
		if ri != rj {
			return ri
		}
		return false
	})
	return out
}

func basketNetRelief(c basketCandidate, net float64) bool {
	if net > 0 {
		return c.OrderSide == "sell"
	}
	if net < 0 {
		return c.OrderSide == "buy"
	}
	return false
}

type basketTranche struct {
	Qty    float64
	Detail string
}

func basketLegalTranche(start basketExposure, c basketCandidate, b basketBounds, increase bool, floor func(coin string, qty float64) float64) basketTranche {
	full := c.FullQty
	if !c.closesFully() {
		full = floor(c.Coin, c.FullQty)
	}
	if !(full > 0) {
		return basketTranche{Detail: fmt.Sprintf("%s %s %.10g floors to no tradable lot", c.StrategyID, c.Direction, c.FullQty)}
	}
	if ok, _ := basketSegmentLegal(start, c.Coin, c.signedDelta(full), b, increase); ok {
		return basketTranche{Qty: full}
	}
	if increase {
		if v := start.reading().violations(b); len(v) > 0 {
			return basketTranche{Detail: "the current basket exposure is outside the elective limits: " + strings.Join(v, "; ")}
		}
	}
	lo, hi := 0.0, 1.0
	for i := 0; i < 60; i++ {
		mid := (lo + hi) / 2
		if ok, _ := basketSegmentLegal(start, c.Coin, c.signedDelta(c.FullQty*mid), b, increase); ok {
			lo = mid
		} else {
			hi = mid
		}
	}
	qty := floor(c.Coin, c.FullQty*lo)
	if !(qty > 0) {
		_, why := basketSegmentLegal(start, c.Coin, c.signedDelta(full), b, increase)
		return basketTranche{Detail: fmt.Sprintf("%s %s: no legal lot-sized tranche (%s)", c.StrategyID, c.Direction, why)}
	}
	if ok, why := basketSegmentLegal(start, c.Coin, c.signedDelta(qty), b, increase); !ok {
		return basketTranche{Detail: fmt.Sprintf("%s %s: the floored tranche %.10g is not legal (%s)", c.StrategyID, c.Direction, qty, why)}
	}
	return basketTranche{Qty: qty}
}
