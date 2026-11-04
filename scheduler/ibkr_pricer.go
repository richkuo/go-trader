package main

import (
	"fmt"
	"math"
	"strings"
	"time"
)

type IBKRPricer struct {
	spotPrices map[string]float64
}

func NewIBKRPricer(spotPrices map[string]float64) *IBKRPricer {
	return &IBKRPricer{spotPrices: spotPrices}
}

func (p *IBKRPricer) Name() string { return "ibkr" }

func (p *IBKRPricer) FetchSpotPrice(underlying string) (float64, error) {
	upper := strings.ToUpper(underlying)
	for _, suffix := range []string{"/USD", "/USDT", "/USDC"} {
		if price, ok := p.spotPrices[upper+suffix]; ok && finitePositive(price) {
			return price, nil
		}
	}
	return 0, fmt.Errorf("no spot price cached for %s", underlying)
}

const (
	ibkrMarkSource       = "ibkr_black_scholes_legacy_model"
	ibkrSettlementSource = "ibkr_cycle_price_legacy_model"
	ibkrModelVolatility  = 0.80
	ibkrModelRate        = 0.05
)

func (p *IBKRPricer) Refresh(reqs []optionLegRequest, refreshAt time.Time) []optionLegReply {
	out := make([]optionLegReply, 0, len(reqs))
	for _, req := range reqs {
		reply := optionLegReply{Key: req.Key}
		cutoff, ready, ok, err := optionGoExpirySchedule("ibkr", req.Expiry)
		if err != nil || !ok {
			reply.Err = fmt.Sprintf("invalid expiry %q", req.Expiry)
			out = append(out, reply)
			continue
		}
		reply.CutoffAt, reply.ReadyAt = cutoff, ready
		spot, spotErr := p.FetchSpotPrice(req.Underlying)
		if !refreshAt.Before(cutoff) {
			if spotErr != nil {
				reply.Pending = fmt.Sprintf("%s: %v", ibkrSettlementSource, spotErr)
			} else {
				reply.Settlement = &optionLegSettlement{Price: spot, Basis: optionSettlementBasisIBKR, Date: refreshAt.UTC().Format("2006-01-02"), Source: ibkrSettlementSource}
			}
			out = append(out, reply)
			continue
		}
		if spotErr != nil {
			reply.Err = spotErr.Error()
			out = append(out, reply)
			continue
		}
		mark, err := p.markAt(req.OptionType, req.Strike, spot, cutoff.Sub(refreshAt).Hours()/24)
		if err != nil {
			reply.Err = err.Error()
		} else {
			reply.Mark = mark
		}
		out = append(out, reply)
	}
	return out
}

func (p *IBKRPricer) markAt(optionType string, strike, spot, dte float64) (*optionLegMark, error) {
	if !finitePositive(dte) {
		return nil, fmt.Errorf("days to expiry %v are not positive", dte)
	}
	price, delta, gamma, vega, theta := bsPrice(spot, strike, dte/365.0, ibkrModelRate, ibkrModelVolatility, strings.ToLower(optionType))
	for _, v := range []float64{price, delta, gamma, vega, theta} {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, fmt.Errorf("black-scholes result is not finite")
		}
	}
	if price < 0 {
		price = 0
	}
	return &optionLegMark{PerUnitUSD: price, Spot: spot, Greeks: OptGreeks{Delta: delta, Gamma: gamma, Theta: theta, Vega: vega}, Source: ibkrMarkSource}, nil
}

func bsPrice(S, K, T, r, sigma float64, optionType string) (price, delta, gamma, vega, theta float64) {
	if T <= 0 || sigma <= 0 || S <= 0 || K <= 0 {
		return 0, 0, 0, 0, 0
	}

	d1 := (math.Log(S/K) + (r+0.5*sigma*sigma)*T) / (sigma * math.Sqrt(T))
	d2 := d1 - sigma*math.Sqrt(T)

	nd1 := stdNormCDF(d1)
	nd2 := stdNormCDF(d2)
	pdfD1 := stdNormPDF(d1)

	if optionType == "call" {
		price = S*nd1 - K*math.Exp(-r*T)*nd2
		delta = nd1
		theta = (-S*pdfD1*sigma/(2*math.Sqrt(T)) - r*K*math.Exp(-r*T)*nd2) / 365
	} else {
		price = K*math.Exp(-r*T)*stdNormCDF(-d2) - S*stdNormCDF(-d1)
		delta = nd1 - 1
		theta = (-S*pdfD1*sigma/(2*math.Sqrt(T)) + r*K*math.Exp(-r*T)*stdNormCDF(-d2)) / 365
	}

	gamma = pdfD1 / (S * sigma * math.Sqrt(T))
	vega = S * pdfD1 * math.Sqrt(T)

	return
}

func stdNormCDF(x float64) float64 {
	return 0.5 * math.Erfc(-x/math.Sqrt2)
}

func stdNormPDF(x float64) float64 {
	return math.Exp(-0.5*x*x) / math.Sqrt(2*math.Pi)
}
