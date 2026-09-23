package main

import (
	"fmt"
	"math"
	"strings"
)

type hlCloseAction int

const (
	hlCloseSend hlCloseAction = iota
	hlCloseSkip
	hlCloseDefer
)

func (a hlCloseAction) String() string {
	switch a {
	case hlCloseSkip:
		return "skip"
	case hlCloseDefer:
		return "defer"
	default:
		return "send"
	}
}

type hlCloseOrderPlan struct {
	Action hlCloseAction
	Mode   hlCloseMode
	Size   float64
	Capped bool
	Reason string
}

type hlCloseContext struct {
	PeerSameQty float64
	PeerOppQty  float64
	OnChain     hlOnChainCoinView
	Refetch     func() (hlOnChainCoinView, error)
}

func hlSideSign(side string) float64 {
	if side == "short" {
		return -1
	}
	return 1
}

func hlPeerBooksOnCoin(strategies map[string]*StrategyState, hlLiveAll []StrategyConfig, coin, selfID, selfSide string) (sameQty, oppQty float64) {
	target := strings.ToUpper(strings.TrimSpace(coin))
	if target == "" {
		return 0, 0
	}
	selfSign := hlSideSign(selfSide)
	add := func(pos *Position) {
		if pos == nil || pos.Quantity <= 0 {
			return
		}
		if hlSideSign(pos.Side) == selfSign {
			sameQty += pos.Quantity
		} else {
			oppQty += pos.Quantity
		}
	}
	for _, sc := range hlLiveAll {
		if sc.ID == selfID {
			continue
		}
		ss := strategies[sc.ID]
		if ss == nil {
			continue
		}
		if raw := hyperliquidRawCoin(sc); raw != "" && strings.ToUpper(strings.TrimSpace(raw)) == target {
			add(hlVirtualPositionFor(ss, sc, raw))
		}
		if hCoin := hedgeCoin(sc); hCoin != "" && strings.ToUpper(strings.TrimSpace(hCoin)) == target {
			if hPos := ss.Positions[hCoin]; hPos.isHedgeLeg() {
				add(hPos)
			}
		}
	}
	return sameQty, oppQty
}

func hlOnChainSignedQty(view hlOnChainCoinView, symbol string) (float64, bool) {
	coin := strings.TrimSpace(symbol)
	key := coin
	abs, found := view.AbsQty[key]
	if !found {
		for k, v := range view.AbsQty {
			if strings.EqualFold(strings.TrimSpace(k), coin) {
				key, abs, found = k, v, true
				break
			}
		}
	}
	if !found || abs <= 1e-9 {
		return 0, true
	}
	if math.IsNaN(abs) || math.IsInf(abs, 0) {
		return 0, false
	}
	switch view.NetSide[key] {
	case "long":
		return abs, true
	case "short":
		return -abs, true
	default:
		return 0, false
	}
}

func finitePositive(v float64) bool {
	return v > 0 && !math.IsNaN(v) && !math.IsInf(v, 0)
}

func planHLCloseOrder(symbol, posSide string, posQty, closeQty, peerSameQty, peerOppQty float64, onChain hlOnChainCoinView) hlCloseOrderPlan {
	tol := hlSharedCloseQtyTolerance
	if posSide != "long" && posSide != "short" {
		return hlCloseOrderPlan{Action: hlCloseDefer, Reason: fmt.Sprintf("the %s position side %q is neither long nor short, so the close direction is unknown", symbol, posSide)}
	}
	if !finitePositive(posQty) || !finitePositive(closeQty) || closeQty > posQty+tol {
		return hlCloseOrderPlan{Action: hlCloseDefer, Reason: fmt.Sprintf("the %s close quantity %.6f is not a valid part of the book quantity %.6f", symbol, closeQty, posQty)}
	}
	if closeQty > posQty {
		closeQty = posQty
	}
	if peerSameQty < 0 || peerOppQty < 0 || math.IsNaN(peerSameQty) || math.IsNaN(peerOppQty) || math.IsInf(peerSameQty, 0) || math.IsInf(peerOppQty, 0) {
		return hlCloseOrderPlan{Action: hlCloseDefer, Reason: fmt.Sprintf("the peer book quantities on %s are not usable", symbol)}
	}
	if !onChain.Known {
		if peerOppQty > tol {
			return hlCloseOrderPlan{Action: hlCloseDefer, Reason: fmt.Sprintf("the on-chain %s position is unknown and an opposite-side peer holds %.6f in its book, so neither a reduce-only nor a netted close can be sized safely", symbol, peerOppQty)}
		}
		return hlCloseOrderPlan{Action: hlCloseSend, Mode: hlCloseModeReduceOnly, Size: closeQty, Reason: fmt.Sprintf("the on-chain %s position is unknown and no opposite-side peer holds a book, so the close is sent reduce-only at the book size", symbol)}
	}
	netSigned, ok := hlOnChainSignedQty(onChain, symbol)
	if !ok {
		return hlCloseOrderPlan{Action: hlCloseDefer, Reason: fmt.Sprintf("the on-chain %s position has no readable side", symbol)}
	}
	s := hlSideSign(posSide)
	target := s*(posQty-closeQty) + s*peerSameQty - s*peerOppQty
	requested := s * (netSigned - target)
	if requested <= tol {
		return hlCloseOrderPlan{Action: hlCloseSkip, Reason: fmt.Sprintf("the on-chain %s net %.6f is already at or past the %.6f the books state after this close (%s book %.6f, close %.6f, same-side peers %.6f, opposite-side peers %.6f); no close order sent, the reconciler owns the book", symbol, netSigned, target, posSide, posQty, closeQty, peerSameQty, peerOppQty)}
	}
	plan := hlCloseOrderPlan{Action: hlCloseSend, Size: closeQty}
	if requested < closeQty-tol {
		plan.Size = requested
		plan.Capped = true
	}
	if s*netSigned > tol && s*target >= -tol {
		plan.Mode = hlCloseModeReduceOnly
		plan.Reason = fmt.Sprintf("the %s close keeps the on-chain net on the %s side (net %.6f, books after close %.6f)", symbol, posSide, netSigned, target)
	} else {
		plan.Mode = hlCloseModeCross
		plan.Reason = fmt.Sprintf("the %s close crosses zero or starts from the other side (net %.6f, books after close %.6f), so it is sent as the netted order", symbol, netSigned, target)
	}
	return plan
}

func resolveHLCloseOrder(symbol, posSide string, posQty, closeQty float64, ctx hlCloseContext) hlCloseOrderPlan {
	plan := planHLCloseOrder(symbol, posSide, posQty, closeQty, ctx.PeerSameQty, ctx.PeerOppQty, ctx.OnChain)
	if plan.Action != hlCloseSend || plan.Mode != hlCloseModeCross {
		return plan
	}
	if ctx.Refetch == nil {
		return hlCloseOrderPlan{Action: hlCloseDefer, Reason: fmt.Sprintf("the %s close needs the netted order, but no account refetch is available to confirm the on-chain position", symbol)}
	}
	fresh, err := ctx.Refetch()
	if err != nil || !fresh.Known {
		detail := "the account state is not readable"
		if err != nil {
			detail = err.Error()
		}
		return hlCloseOrderPlan{Action: hlCloseDefer, Reason: fmt.Sprintf("the %s close needs the netted order, but the account refetch failed (%s)", symbol, detail)}
	}
	return planHLCloseOrder(symbol, posSide, posQty, closeQty, ctx.PeerSameQty, ctx.PeerOppQty, fresh)
}

func hlOnChainRefetcher(accountAddress string) func() (hlOnChainCoinView, error) {
	return func() (hlOnChainCoinView, error) {
		_, fresh, err := fetchHyperliquidStateFn(accountAddress)
		if err != nil {
			return hlOnChainCoinView{}, err
		}
		return hlOnChainCoinViewFromPositions(fresh), nil
	}
}

func manualCloseFillAttribution(posQty float64, fill *HyperliquidFill) (bookedQty, fee float64, fullClose bool) {
	if fill == nil {
		return 0, 0, false
	}
	bookedQty = fill.TotalSz
	fee = fill.Fee
	if bookedQty > posQty+1e-9 {
		if fill.TotalSz > 0 {
			fee *= posQty / fill.TotalSz
		}
		bookedQty = posQty
	}
	fullClose = posQty-bookedQty <= 0.0001
	return bookedQty, fee, fullClose
}
