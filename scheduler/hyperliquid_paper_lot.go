package main

import (
	"fmt"
	"math/big"
	"strconv"
	"strings"
)

const (
	hlPaperHoldLotUnknown  = "lot_size_unknown"
	hlPaperHoldBelowLot    = "below_lot"
	hlPaperHoldBelowMin    = "below_min_notional"
	hlPaperHoldInvalidQty  = "invalid_quantity"
	hlPaperHoldInvalidPx   = "invalid_price"
	hlPyDecimalContextPrec = 28
)

type hlPaperLotDecision struct {
	RequestedQty float64
	Qty          float64
	Notional     float64
	Hold         string
	Detail       string
}

type hlPaperLotPolicy struct {
	coin          string
	cache         *hlLotMetadataCache
	decisionPrice float64
	looked        bool
	lot           hlLotLookup
}

func hlPaperSyntheticFill(sc StrategyConfig, liveFill bool) bool {
	return !liveFill && sc.Platform == "hyperliquid" && sc.Type == "perps" && !hyperliquidIsLive(sc.Args)
}

func newHLPaperLotPolicy(sc StrategyConfig, coin string, decisionPrice float64, liveFill bool) *hlPaperLotPolicy {
	if !hlPaperSyntheticFill(sc, liveFill) {
		return nil
	}
	return &hlPaperLotPolicy{coin: coin, cache: hlLotMetadata, decisionPrice: decisionPrice}
}

func (p *hlPaperLotPolicy) lookup() hlLotLookup {
	if !p.looked {
		p.looked = true
		if p.cache == nil {
			p.lot = hlLotLookup{Coin: p.coin, Reason: "no venue metadata source"}
		} else {
			p.lot = p.cache.Lookup(p.coin)
		}
	}
	return p.lot
}

func (p *hlPaperLotPolicy) entry(requestedQty, execPrice float64) hlPaperLotDecision {
	return hlVenueEntryDecision(requestedQty, execPrice, p.lookup())
}

func (p *hlPaperLotPolicy) partialClose(requestedQty float64) hlPaperLotDecision {
	return hlVenuePartialCloseDecision(requestedQty, p.decisionPrice, p.lookup())
}

func hlVenueEntryDecision(requestedQty, execPrice float64, lot hlLotLookup) hlPaperLotDecision {
	d := hlPaperLotDecision{RequestedQty: requestedQty}
	if !lot.Known {
		d.Hold, d.Detail = hlPaperHoldLotUnknown, lot.Reason
		return d
	}
	if !finitePositive(requestedQty) {
		d.Hold, d.Detail = hlPaperHoldInvalidQty, fmt.Sprintf("requested quantity %v is not a finite positive number", requestedQty)
		return d
	}
	if !finitePositive(execPrice) {
		d.Hold, d.Detail = hlPaperHoldInvalidPx, fmt.Sprintf("price %v is not a finite positive number", execPrice)
		return d
	}
	d.Qty = hlFloorLotSize(requestedQty, lot.SzDecimals)
	d.Notional = d.Qty * execPrice
	return hlVenueLotGate(d, lot.SzDecimals)
}

func hlVenuePartialCloseDecision(requestedQty, decisionPrice float64, lot hlLotLookup) hlPaperLotDecision {
	d := hlPaperLotDecision{RequestedQty: requestedQty}
	if !lot.Known {
		d.Hold, d.Detail = hlPaperHoldLotUnknown, lot.Reason
		return d
	}
	if !finitePositive(requestedQty) {
		d.Hold, d.Detail = hlPaperHoldInvalidQty, fmt.Sprintf("requested quantity %v is not a finite positive number", requestedQty)
		return d
	}
	if !finitePositive(decisionPrice) {
		d.Hold, d.Detail = hlPaperHoldInvalidPx, fmt.Sprintf("decision price %v is not a finite positive number", decisionPrice)
		return d
	}
	d.Qty = hlFloorLotSize(requestedQty, lot.SzDecimals)
	d.Notional = d.Qty * decisionPrice
	return hlVenueLotGate(d, lot.SzDecimals)
}

func hlVenueLotGate(d hlPaperLotDecision, szDecimals int) hlPaperLotDecision {
	threshold := hlVenueCloseGateThresholdUSD()
	switch {
	case d.Qty <= 0:
		d.Hold = hlPaperHoldBelowLot
		d.Detail = fmt.Sprintf("requested %.10g floors to 0 at szDecimals=%d", d.RequestedQty, szDecimals)
	case d.Notional < threshold:
		d.Hold = hlPaperHoldBelowMin
		d.Detail = fmt.Sprintf("floored %.10g is $%.4f, below the $%.2f venue minimum ($%.2f + %.0f%% margin)", d.Qty, d.Notional, threshold, hlVenueMinOrderNotionalUSD, hlVenueMinOrderNotionalMargin*100)
	}
	return d
}

func hlFloorLotSize(sz float64, szDecimals int) float64 {
	if !finitePositive(sz) {
		return 0
	}
	decimals := szDecimals
	if decimals < 0 {
		decimals = 0
	}
	coef, exp := hlShortestDecimal(sz)
	slackExp := -9 - decimals
	minExp := exp
	if slackExp < minExp {
		minExp = slackExp
	}
	sum := new(big.Int).Mul(coef, pow10Big(exp-minExp))
	sum.Add(sum, pow10Big(slackExp-minExp))
	sum, minExp = pyDecimalContextRound(sum, minExp, hlPyDecimalContextPrec)
	quantExp := -decimals
	var units *big.Int
	if minExp >= quantExp {
		units = new(big.Int).Mul(sum, pow10Big(minExp-quantExp))
	} else {
		units = new(big.Int).Quo(sum, pow10Big(quantExp-minExp))
	}
	out, err := strconv.ParseFloat(units.String()+"e"+strconv.Itoa(quantExp), 64)
	if err != nil {
		return 0
	}
	return out
}

func hlShortestDecimal(v float64) (*big.Int, int) {
	s := strconv.FormatFloat(v, 'e', -1, 64)
	mant, expPart, _ := strings.Cut(s, "e")
	exp, _ := strconv.Atoi(expPart)
	intPart, fracPart, _ := strings.Cut(mant, ".")
	digits := intPart + fracPart
	coef, _ := new(big.Int).SetString(digits, 10)
	return coef, exp - len(fracPart)
}

func pow10Big(n int) *big.Int {
	if n <= 0 {
		return big.NewInt(1)
	}
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil)
}

func pyDecimalContextRound(coef *big.Int, exp, prec int) (*big.Int, int) {
	digits := len(coef.String())
	if digits <= prec {
		return coef, exp
	}
	drop := digits - prec
	div := pow10Big(drop)
	q, r := new(big.Int).QuoRem(coef, div, new(big.Int))
	twice := new(big.Int).Mul(r, big.NewInt(2))
	switch twice.Cmp(div) {
	case 1:
		q.Add(q, big.NewInt(1))
	case 0:
		if q.Bit(0) == 1 {
			q.Add(q, big.NewInt(1))
		}
	}
	return q, exp + drop
}
