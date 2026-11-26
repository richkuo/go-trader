package main

import (
	"fmt"
	"math/big"
	"path/filepath"
	"sort"
	"strings"
)

const (
	okxStopOwnerPct   = "stop_loss_pct"
	okxStopOwnerATR   = "stop_loss_atr_mult"
	okxStopOwnerTrail = "trailing_stop_pct"
)

const okxStopContract = "okx_stop_v1"

func okxPerpsStrategy(sc StrategyConfig) bool {
	return sc.Platform == "okx" && sc.Type == "perps"
}

func okxExplicitStopOwner(sc StrategyConfig) (string, error) {
	if !okxPerpsStrategy(sc) {
		return "", nil
	}
	trail, atr, pct := 0.0, 0.0, 0.0
	if sc.TrailingStopPct != nil {
		trail = *sc.TrailingStopPct
	}
	if sc.StopLossATRMult != nil {
		atr = *sc.StopLossATRMult
	}
	if sc.StopLossPct != nil {
		pct = *sc.StopLossPct
	}
	switch {
	case trail > 0 && (atr > 0 || pct > 0):
		return "", fmt.Errorf("trailing_stop_pct is mutually exclusive with stop_loss_pct and stop_loss_atr_mult")
	case atr > 0 && pct > 0:
		return "", fmt.Errorf("stop_loss_atr_mult is mutually exclusive with stop_loss_pct")
	case trail > 0:
		return okxStopOwnerTrail, nil
	case atr > 0:
		return okxStopOwnerATR, nil
	case pct > 0:
		return okxStopOwnerPct, nil
	default:
		return "", nil
	}
}

func okxStopWanted(sc StrategyConfig) bool {
	owner, err := okxExplicitStopOwner(sc)
	return err == nil && owner != ""
}

func okxStopScriptOK(sc StrategyConfig) bool {
	return filepath.Base(sc.Script) == "check_okx.py"
}

type okxStopPolicy struct {
	Owner      string
	Pct        string
	ATRMult    string
	MinMovePct string
}

func okxDecimalField(v float64) string {
	return new(big.Rat).SetFloat64(v).FloatString(8)
}

func okxStopPolicyOf(sc StrategyConfig) (okxStopPolicy, bool, error) {
	owner, err := okxExplicitStopOwner(sc)
	if err != nil || owner == "" {
		return okxStopPolicy{}, false, err
	}
	pol := okxStopPolicy{Owner: owner}
	switch owner {
	case okxStopOwnerTrail:
		pol.Pct = okxDecimalField(*sc.TrailingStopPct)
		minMove := defaultTrailingStopMinMovePct
		if sc.TrailingStopMinMovePct != nil {
			minMove = *sc.TrailingStopMinMovePct
		}
		pol.MinMovePct = okxDecimalField(minMove)
	case okxStopOwnerATR:
		pol.ATRMult = okxDecimalField(*sc.StopLossATRMult)
	case okxStopOwnerPct:
		pol.Pct = okxDecimalField(*sc.StopLossPct)
	}
	return pol, true, nil
}

func okxStopPolicyEqual(a, b StrategyConfig) bool {
	pa, aOK, aErr := okxStopPolicyOf(a)
	pb, bOK, bErr := okxStopPolicyOf(b)
	if aErr != nil || bErr != nil || aOK != bOK {
		return false
	}
	if !aOK {
		return true
	}
	return pa == pb
}

type okxStopGeometryInput struct {
	Side            string
	Owner           string
	Entry           string
	Mark            string
	EntryATR        string
	HighWater       string
	PreviousTrigger string
	Pct             string
	ATRMult         string
	MinMovePct      string
	Tick            string
	Equity          string
	Contracts       string
	CtVal           string
	MMR             string
	Fee             string
	VenueLiq        string
	VenueLiqKnown   bool
	RequireVenueLiq bool
}

type okxStopGeometryResult struct {
	Trigger   string
	HighWater string
	Moved     bool
	LiqBound  string
	Clamped   bool
}

func okxRat(value, label string) (*big.Rat, error) {
	text := strings.TrimSpace(value)
	if text == "" {
		return nil, fmt.Errorf("%s is missing", label)
	}
	r, ok := new(big.Rat).SetString(text)
	if !ok {
		return nil, fmt.Errorf("%s is not a number", label)
	}
	return r, nil
}

func okxPositive(value, label string) (*big.Rat, error) {
	r, err := okxRat(value, label)
	if err != nil {
		return nil, err
	}
	if r.Sign() <= 0 {
		return nil, fmt.Errorf("%s must be positive", label)
	}
	return r, nil
}

func okxTickDecimals(tick string) (int, error) {
	if strings.ContainsAny(tick, "eE+") {
		return 0, fmt.Errorf("tick must be a plain decimal string")
	}
	i := strings.IndexByte(tick, '.')
	if i < 0 {
		return 0, nil
	}
	return len(tick) - i - 1, nil
}

func okxFormatRat(v *big.Rat, tick string) (string, error) {
	places, err := okxTickDecimals(tick)
	if err != nil {
		return "", err
	}
	return new(big.Float).SetPrec(256).SetMode(big.ToNearestAway).SetRat(v).Text('f', places), nil
}

func okxRoundTowardRat(side string, px *big.Rat, tick string) (*big.Rat, error) {
	step, err := okxPositive(tick, "tick")
	if err != nil {
		return nil, err
	}
	if side != "long" && side != "short" {
		return nil, fmt.Errorf("side must be long or short")
	}
	if px == nil || px.Sign() <= 0 {
		return nil, fmt.Errorf("trigger must be positive")
	}
	steps := new(big.Rat).Quo(px, step)
	q, r := new(big.Int).QuoRem(new(big.Int).Set(steps.Num()), new(big.Int).Set(steps.Denom()), new(big.Int))
	if r.Sign() < 0 {
		return nil, fmt.Errorf("trigger must be positive")
	}
	if r.Sign() > 0 && side == "long" {
		q.Add(q, big.NewInt(1))
	}
	out := new(big.Rat).SetInt(q)
	out.Mul(out, step)
	if out.Sign() <= 0 {
		return nil, fmt.Errorf("rounded trigger is not positive")
	}
	return out, nil
}

func okxCrossLiquidation(side, equity, contracts, ctVal, entry, mmr, fee string) (*big.Rat, error) {
	margin, err := okxPositive(equity, "equity")
	if err != nil {
		return nil, err
	}
	count, err := okxPositive(contracts, "contracts")
	if err != nil {
		return nil, err
	}
	face, err := okxPositive(ctVal, "ctVal")
	if err != nil {
		return nil, err
	}
	avg, err := okxPositive(entry, "entry")
	if err != nil {
		return nil, err
	}
	maintenance, err := okxPositive(mmr, "mmr")
	if err != nil {
		return nil, err
	}
	feeRate, err := okxRat(fee, "fee")
	if err != nil {
		return nil, err
	}
	one := big.NewRat(1, 1)
	if maintenance.Cmp(one) >= 0 || feeRate.Sign() < 0 || feeRate.Cmp(one) >= 0 {
		return nil, fmt.Errorf("mmr and fee are not a usable OKX margin ratio")
	}
	notional := new(big.Rat).Mul(new(big.Rat).Mul(face, count), avg)
	var num, den *big.Rat
	if side == "long" {
		num = new(big.Rat).Sub(margin, notional)
		den = new(big.Rat).Mul(new(big.Rat).Mul(face, count), new(big.Rat).Sub(new(big.Rat).Add(maintenance, feeRate), one))
	} else if side == "short" {
		num = new(big.Rat).Add(margin, notional)
		den = new(big.Rat).Mul(new(big.Rat).Mul(face, count), new(big.Rat).Add(new(big.Rat).Add(maintenance, feeRate), one))
	} else {
		return nil, fmt.Errorf("side must be long or short")
	}
	if den.Sign() == 0 {
		return nil, fmt.Errorf("liquidation denominator is zero")
	}
	return new(big.Rat).Quo(num, den), nil
}

func okxTighterLiq(side string, formula, venue *big.Rat) *big.Rat {
	var bounds []*big.Rat
	if formula != nil && formula.Sign() > 0 {
		bounds = append(bounds, formula)
	}
	if venue != nil && venue.Sign() > 0 {
		bounds = append(bounds, venue)
	}
	if len(bounds) == 0 {
		return nil
	}
	best := new(big.Rat).Set(bounds[0])
	for _, b := range bounds[1:] {
		if side == "long" && b.Cmp(best) > 0 {
			best.Set(b)
		}
		if side == "short" && b.Cmp(best) < 0 {
			best.Set(b)
		}
	}
	return best
}

func okxClampInside(side string, trigger, liquidation *big.Rat) (*big.Rat, bool) {
	if liquidation == nil || liquidation.Sign() <= 0 {
		return trigger, false
	}
	if side == "long" && trigger.Cmp(liquidation) <= 0 {
		return new(big.Rat).Mul(liquidation, big.NewRat(1005, 1000)), true
	}
	if side == "short" && trigger.Cmp(liquidation) >= 0 {
		return new(big.Rat).Mul(liquidation, big.NewRat(995, 1000)), true
	}
	return trigger, false
}

func okxOnMarkSide(side string, trigger, mark *big.Rat) bool {
	if side == "long" {
		return trigger.Cmp(mark) < 0
	}
	return trigger.Cmp(mark) > 0
}

func okxMinMoveReached(next, previous, minMove *big.Rat) bool {
	if previous == nil || previous.Sign() <= 0 {
		return true
	}
	move := new(big.Rat).Abs(new(big.Rat).Sub(next, previous))
	move.Quo(move, previous)
	move.Mul(move, big.NewRat(100, 1))
	return move.Cmp(minMove) >= 0
}

func okxRawTrigger(in okxStopGeometryInput) (raw, high *big.Rat, err error) {
	entry, err := okxPositive(in.Entry, "entry")
	if err != nil {
		return nil, nil, err
	}
	hundred := big.NewRat(100, 1)
	switch in.Owner {
	case okxStopOwnerPct:
		pct, err := okxPositive(in.Pct, "pct")
		if err != nil {
			return nil, nil, err
		}
		if pct.Cmp(big.NewRat(50, 1)) > 0 {
			return nil, nil, fmt.Errorf("pct must be <= 50")
		}
		frac := new(big.Rat).Quo(pct, hundred)
		if in.Side == "long" {
			return new(big.Rat).Mul(entry, new(big.Rat).Sub(big.NewRat(1, 1), frac)), nil, nil
		}
		return new(big.Rat).Mul(entry, new(big.Rat).Add(big.NewRat(1, 1), frac)), nil, nil
	case okxStopOwnerATR:
		mult, err := okxPositive(in.ATRMult, "atr_mult")
		if err != nil {
			return nil, nil, err
		}
		atr, err := okxPositive(in.EntryATR, "entry_atr")
		if err != nil {
			return nil, nil, err
		}
		delta := new(big.Rat).Mul(mult, atr)
		if in.Side == "long" {
			return new(big.Rat).Sub(entry, delta), nil, nil
		}
		return new(big.Rat).Add(entry, delta), nil, nil
	case okxStopOwnerTrail:
		pct, err := okxPositive(in.Pct, "pct")
		if err != nil {
			return nil, nil, err
		}
		if pct.Cmp(big.NewRat(50, 1)) > 0 {
			return nil, nil, fmt.Errorf("pct must be <= 50")
		}
		mark, err := okxPositive(in.Mark, "mark")
		if err != nil {
			return nil, nil, err
		}
		hwText := in.HighWater
		if strings.TrimSpace(hwText) == "" {
			hwText = in.Mark
		}
		high, err = okxRat(hwText, "high_water")
		if err != nil {
			return nil, nil, err
		}
		if high.Sign() <= 0 {
			high = mark
		}
		if in.Side == "long" && mark.Cmp(high) > 0 {
			high = mark
		}
		if in.Side == "short" && (high.Sign() <= 0 || mark.Cmp(high) < 0) {
			high = mark
		}
		frac := new(big.Rat).Quo(pct, hundred)
		if in.Side == "long" {
			return new(big.Rat).Mul(high, new(big.Rat).Sub(big.NewRat(1, 1), frac)), high, nil
		}
		return new(big.Rat).Mul(high, new(big.Rat).Add(big.NewRat(1, 1), frac)), high, nil
	default:
		return nil, nil, fmt.Errorf("owner is not an OKX stop")
	}
}

func okxFinalizeStop(in okxStopGeometryInput) (okxStopGeometryResult, error) {
	if in.Side != "long" && in.Side != "short" {
		return okxStopGeometryResult{}, fmt.Errorf("side must be long or short")
	}
	mark, err := okxPositive(in.Mark, "mark")
	if err != nil {
		return okxStopGeometryResult{}, err
	}
	if _, err := okxPositive(in.Tick, "tick"); err != nil {
		return okxStopGeometryResult{}, err
	}
	raw, high, err := okxRawTrigger(in)
	if err != nil {
		return okxStopGeometryResult{}, err
	}
	if raw.Sign() <= 0 {
		return okxStopGeometryResult{}, fmt.Errorf("raw trigger is not positive")
	}
	rounded, err := okxRoundTowardRat(in.Side, raw, in.Tick)
	if err != nil {
		return okxStopGeometryResult{}, err
	}
	var previous *big.Rat
	if strings.TrimSpace(in.PreviousTrigger) != "" {
		previous, err = okxPositive(in.PreviousTrigger, "previous_trigger")
		if err != nil {
			return okxStopGeometryResult{}, err
		}
	}
	minMove, err := okxRat(orOKX(in.MinMovePct, "0"), "min_move_pct")
	if err != nil {
		return okxStopGeometryResult{}, err
	}
	if minMove.Sign() < 0 {
		return okxStopGeometryResult{}, fmt.Errorf("min move must be >= 0")
	}
	if in.Owner == okxStopOwnerTrail && previous != nil {
		tighter := (in.Side == "long" && rounded.Cmp(previous) > 0) || (in.Side == "short" && rounded.Cmp(previous) < 0)
		if !tighter || !okxMinMoveReached(rounded, previous, minMove) {
			prevText, err := okxFormatRat(previous, in.Tick)
			if err != nil {
				return okxStopGeometryResult{}, err
			}
			hwText := ""
			if high != nil {
				hwText, err = okxFormatRat(high, in.Tick)
				if err != nil {
					return okxStopGeometryResult{}, err
				}
			}
			return okxStopGeometryResult{Trigger: prevText, HighWater: hwText, Moved: false}, nil
		}
	}
	var formula *big.Rat
	if in.Equity != "" && in.Contracts != "" && in.CtVal != "" && in.MMR != "" && in.Fee != "" && in.Entry != "" {
		formula, err = okxCrossLiquidation(in.Side, in.Equity, in.Contracts, in.CtVal, in.Entry, in.MMR, in.Fee)
		if err != nil {
			return okxStopGeometryResult{}, err
		}
	}
	if in.RequireVenueLiq && !in.VenueLiqKnown {
		return okxStopGeometryResult{}, fmt.Errorf("position liquidation evidence is missing")
	}
	var venue *big.Rat
	if strings.TrimSpace(in.VenueLiq) != "" {
		venue, err = okxPositive(in.VenueLiq, "venue_liq")
		if err != nil {
			return okxStopGeometryResult{}, err
		}
	}
	formulaBound := formula
	if formula != nil && formula.Sign() <= 0 {
		formulaBound = nil
	}
	if in.RequireVenueLiq && venue == nil && formulaBound != nil {
		return okxStopGeometryResult{}, fmt.Errorf("venue liquidation price is missing while the cross bound is positive")
	}
	bound := okxTighterLiq(in.Side, formulaBound, venue)
	clamped, didClamp := okxClampInside(in.Side, rounded, bound)
	if didClamp {
		clamped, err = okxRoundTowardRat(in.Side, clamped, in.Tick)
		if err != nil {
			return okxStopGeometryResult{}, err
		}
		if bound != nil {
			if in.Side == "long" && clamped.Cmp(bound) <= 0 {
				return okxStopGeometryResult{}, fmt.Errorf("stop cannot sit inside the liquidation bound")
			}
			if in.Side == "short" && clamped.Cmp(bound) >= 0 {
				return okxStopGeometryResult{}, fmt.Errorf("stop cannot sit inside the liquidation bound")
			}
		}
	}
	if !okxOnMarkSide(in.Side, clamped, mark) {
		return okxStopGeometryResult{}, fmt.Errorf("stop is not on the executable side of the mark")
	}
	trigger, err := okxFormatRat(clamped, in.Tick)
	if err != nil {
		return okxStopGeometryResult{}, err
	}
	hwText := ""
	if high != nil {
		hwText, err = okxFormatRat(high, in.Tick)
		if err != nil {
			return okxStopGeometryResult{}, err
		}
	}
	liqText := ""
	if bound != nil && bound.Sign() > 0 {
		liqText = okxBoundString(bound)
	}
	return okxStopGeometryResult{Trigger: trigger, HighWater: hwText, Moved: true, LiqBound: liqText, Clamped: didClamp}, nil
}

func okxBoundString(v *big.Rat) string {
	if v.Denom().Cmp(big.NewInt(1)) == 0 {
		return v.Num().String()
	}
	text := v.FloatString(24)
	text = strings.TrimRight(text, "0")
	return strings.TrimRight(text, ".")
}

func orOKX(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

func okxPerpsStopOverlapErrors(strategies []StrategyConfig) []string {
	type peer struct {
		id   string
		stop bool
	}
	groups := map[string][]peer{}
	for _, sc := range strategies {
		if !okxPerpsStrategy(sc) || !okxIsLive(sc.Args) {
			continue
		}
		sym := strings.ToUpper(strings.TrimSpace(okxSymbol(sc.Args)))
		if sym == "" {
			continue
		}
		owner, _ := okxExplicitStopOwner(sc)
		groups[sym] = append(groups[sym], peer{id: sc.ID, stop: owner != ""})
	}
	var syms []string
	for sym := range groups {
		syms = append(syms, sym)
	}
	sort.Strings(syms)
	var errs []string
	for _, sym := range syms {
		peers := groups[sym]
		if len(peers) < 2 {
			continue
		}
		anyStop := false
		ids := make([]string, 0, len(peers))
		for _, p := range peers {
			ids = append(ids, p.id)
			if p.stop {
				anyStop = true
			}
		}
		if !anyStop {
			continue
		}
		sort.Strings(ids)
		errs = append(errs, fmt.Sprintf("okx perps %s is shared by %s and one of them arms an exchange stop; one live stop owner per instrument", sym, strings.Join(ids, ", ")))
	}
	return errs
}
