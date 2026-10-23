package main

import (
	"fmt"
	"math"
	"math/big"
	"regexp"
	"sort"
	"strings"
)

const (
	quotePolicyFormPair     = "pair"
	quotePolicyFormReducing = "reducing"
	quotePolicyFormExit     = "exit"
	quotePolicyFormNone     = "none"
)

type quotePolicyParams struct {
	QuoteNotionalUSD        float64 `json:"quote_notional_usd"`
	TargetInventoryUSD      float64 `json:"target_inventory_usd"`
	RiskAversion            float64 `json:"risk_aversion"`
	VolatilityMultiplier    float64 `json:"volatility_multiplier"`
	VolatilityWindowSeconds int     `json:"volatility_window_seconds"`
	MinSpreadBps            float64 `json:"min_spread_bps"`
	MaxSpreadBps            float64 `json:"max_spread_bps"`
	MaxSkewBps              float64 `json:"max_skew_bps"`
	TaperBandPct            float64 `json:"taper_band_pct"`
	QuoteLifetimeSeconds    int     `json:"quote_lifetime_seconds"`
	MaxInputAgeMs           int     `json:"max_input_age_ms"`
	RefreshThresholdBps     float64 `json:"refresh_threshold_bps"`
	MaxInventoryAgeSeconds  int     `json:"max_inventory_age_seconds"`
	RoundTripReserveBps     float64 `json:"round_trip_reserve_bps"`
	MakerFeeBps             float64 `json:"maker_fee_bps"`
	MinOrderValueUSD        float64 `json:"min_order_value_usd"`
}

func parseQuotePolicyParams(params map[string]interface{}) (quotePolicyParams, error) {
	if err := validateInventoryMarketMakingParams(params); err != nil {
		return quotePolicyParams{}, err
	}
	values, err := quoteSpecResolve(quotePolicySpec.Params, params)
	if err != nil {
		return quotePolicyParams{}, err
	}
	f := func(name string) float64 { return quoteRatFloat(values[name]) }
	i := func(name string) int { return int(values[name].Num().Int64()) }
	return quotePolicyParams{
		QuoteNotionalUSD:        f("quote_notional_usd"),
		TargetInventoryUSD:      f("target_inventory_usd"),
		RiskAversion:            f("risk_aversion"),
		VolatilityMultiplier:    f("volatility_multiplier"),
		VolatilityWindowSeconds: i("volatility_window_seconds"),
		MinSpreadBps:            f("min_spread_bps"),
		MaxSpreadBps:            f("max_spread_bps"),
		MaxSkewBps:              f("max_skew_bps"),
		TaperBandPct:            f("taper_band_pct"),
		QuoteLifetimeSeconds:    i("quote_lifetime_seconds"),
		MaxInputAgeMs:           i("max_input_age_ms"),
		RefreshThresholdBps:     f("refresh_threshold_bps"),
		MaxInventoryAgeSeconds:  i("max_inventory_age_seconds"),
		RoundTripReserveBps:     f("round_trip_reserve_bps"),
		MakerFeeBps:             f("maker_fee_bps"),
		MinOrderValueUSD:        f("min_order_value_usd"),
	}, nil
}

type quotePolicyBook struct {
	EventMs  int64  `json:"event_ms"`
	RecvMs   int64  `json:"recv_ms"`
	Identity string `json:"identity"`
	BestBid  string `json:"best_bid"`
	BestAsk  string `json:"best_ask"`
}

type quotePolicySample struct {
	SecondMs int64  `json:"second_ms"`
	Mid      string `json:"mid,omitempty"`
	RecvMs   int64  `json:"recv_ms,omitempty"`
	Identity string `json:"identity,omitempty"`
	Missing  string `json:"missing,omitempty"`
}

type quotePolicyEvidence struct {
	DeadlineMs int64               `json:"deadline_ms"`
	Book       *quotePolicyBook    `json:"book,omitempty"`
	BookIssue  string              `json:"book_issue,omitempty"`
	Samples    []quotePolicySample `json:"samples"`
	Healthy    bool                `json:"healthy"`
	Reasons    []string            `json:"reasons,omitempty"`
}

type quotePolicyContext struct {
	Params            quotePolicyParams
	Coin              string
	CapUSD            float64
	MaxOrderQty       float64
	CycleSeconds      int
	Prices            quotePriceRule
	PolicyFingerprint string
}

type quotePolicyValues struct {
	Mid                 string   `json:"mid,omitempty"`
	MidFloat            *float64 `json:"mid_float,omitempty"`
	InventoryFloat      float64  `json:"inventory_float"`
	NormalizedInventory *float64 `json:"normalized_inventory,omitempty"`
	WarmupSamples       int      `json:"warmup_samples"`
	Variance            *float64 `json:"variance_per_second,omitempty"`
	HorizonSeconds      int      `json:"horizon_seconds"`
	Sigma               *float64 `json:"sigma,omitempty"`
	Skew                *float64 `json:"skew,omitempty"`
	Reservation         *float64 `json:"reservation,omitempty"`
	HalfSpread          *float64 `json:"half_spread,omitempty"`
	FeeFloor            float64  `json:"fee_floor"`
	RawBid              *float64 `json:"raw_bid,omitempty"`
	RawAsk              *float64 `json:"raw_ask,omitempty"`
	Bid                 string   `json:"bid,omitempty"`
	Ask                 string   `json:"ask,omitempty"`
	BaseSize            string   `json:"base_size,omitempty"`
	Taper               *float64 `json:"taper,omitempty"`
	IncreasingSize      string   `json:"increasing_size,omitempty"`
	ReducingSize        string   `json:"reducing_size,omitempty"`
	MarkedInventoryUSD  *float64 `json:"marked_inventory_usd,omitempty"`
	AboveCap            bool     `json:"above_cap"`
	InventoryAgeMs      int64    `json:"inventory_age_ms"`
}

type quotePolicyOutcome struct {
	Decision      quoteDecision     `json:"-"`
	Form          string            `json:"form"`
	Desired       quoteDesiredPair  `json:"desired"`
	Exit          quoteExitRequest  `json:"exit"`
	Values        quotePolicyValues `json:"values"`
	PolicyHolds   []string          `json:"policy_holds,omitempty"`
	PredictedHold []string          `json:"predicted_holds,omitempty"`
	Refresh       map[string]string `json:"refresh,omitempty"`
	CancelReasons []string          `json:"cancel_reasons,omitempty"`
}

var quoteHoldExact = map[string]bool{
	"input verdict missing":                                   true,
	"input verdict configuration fingerprint is incompatible": true,
	"input verdict policy fingerprint is incompatible":        true,
	"input verdict names no source":                           true,
	"strategy paused":                                         true,
	"circuit breaker active":                                  true,
	"portfolio kill switch latched":                           true,
	"the partition state file has not saved":                  true,
	"the deadline mark is missing or stale":                   true,
	"nonzero inventory has no confirmed resting stop":         true,
	"shutdown in progress":                                    true,
}

var quoteHoldPrefixes = []string{
	"input verdict \"",
	"input verdict names owner ",
	"input verdict is for deadline ",
	"settlement held: ",
	"daily loss limit tripped: ",
	"operator hold: ",
	"evaluation failed: ",
}

var quoteHoldUnknownOrder = regexp.MustCompile(`^(bid|ask|stop|close) order [0-9]+ has an unknown outcome$`)

func quoteIncreaseHoldMatches(holds []string) []string {
	var out []string
	for _, h := range holds {
		if quoteHoldExact[h] || quoteHoldUnknownOrder.MatchString(h) {
			out = append(out, h)
			continue
		}
		for _, p := range quoteHoldPrefixes {
			if strings.HasPrefix(h, p) {
				out = append(out, h)
				break
			}
		}
	}
	return out
}

func quoteFloatPtr(v float64) *float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return nil
	}
	return &v
}

func quoteRatFromFloatOK(v float64) (*big.Rat, bool) {
	r, err := quoteRatFromFloatDecimal(v)
	if err != nil {
		return nil, false
	}
	return r, true
}

func quotePolicyTick(rule quotePriceRule, px *big.Rat) *big.Rat {
	return pow10Rat(-rule.pxDecimals(px))
}

func quotePolicyMid(b *quotePolicyBook) (*big.Rat, error) {
	bid, err := parseQuoteDecimal(b.BestBid)
	if err != nil {
		return nil, err
	}
	ask, err := parseQuoteDecimal(b.BestAsk)
	if err != nil {
		return nil, err
	}
	if bid.Sign() <= 0 || ask.Cmp(bid) <= 0 {
		return nil, fmt.Errorf("book bid %s and ask %s are not a positive uncrossed pair", b.BestBid, b.BestAsk)
	}
	return quoteRatQuo(quoteRatAdd(bid, ask), big.NewRat(2, 1)), nil
}

func quotePolicySideOf(snap quoteSnapshot, role string) *quoteSnapshotSide {
	for i := range snap.Sides {
		s := &snap.Sides[i]
		if s.Role == role && s.Generation == snap.Generation {
			return s
		}
	}
	return nil
}

func quoteSnapshotSideRests(s *quoteSnapshotSide) bool {
	return s != nil && s.Status == quoteSubmitResting && s.CancelState == quoteCancelNone
}

func decideQuotePolicy(ctx quotePolicyContext, ev quotePolicyEvidence, in quoteDecisionInput) quotePolicyOutcome {
	p := ctx.Params
	snap := in.Snapshot
	out := quotePolicyOutcome{Form: quotePolicyFormNone, Refresh: map[string]string{}}
	out.Desired.PolicyFingerprint = ctx.PolicyFingerprint
	v := &out.Values
	inv, err := parseQuoteRat(snap.Inventory)
	if err != nil {
		inv = quoteRatZero()
		out.PolicyHolds = append(out.PolicyHolds, fmt.Sprintf("snapshot inventory %q is unreadable", snap.Inventory))
	}
	v.InventoryFloat = quoteRatFloat(inv)
	v.HorizonSeconds = p.QuoteLifetimeSeconds
	if ctx.CycleSeconds > v.HorizonSeconds {
		v.HorizonSeconds = ctx.CycleSeconds
	}
	v.FeeFloor = (2*math.Max(p.MakerFeeBps, 0) + p.RoundTripReserveBps) / 20000
	if inv.Sign() != 0 && snap.FirstOpenMs > 0 {
		v.InventoryAgeMs = ev.DeadlineMs - snap.FirstOpenMs
	}

	reducingRole, increasingRoles := "", []string{quoteRoleBid, quoteRoleAsk}
	posSide := ""
	switch inv.Sign() {
	case 1:
		reducingRole, increasingRoles, posSide = quoteRoleAsk, []string{quoteRoleBid}, "long"
	case -1:
		reducingRole, increasingRoles, posSide = quoteRoleBid, []string{quoteRoleAsk}, "short"
	}

	if inv.Sign() != 0 && snap.FirstOpenMs > 0 && ev.DeadlineMs >= snap.FirstOpenMs+int64(p.MaxInventoryAgeSeconds)*1000 {
		out.Form = quotePolicyFormExit
		out.Exit = quoteExitRequest{Kind: quoteExitKindExit, ExitReason: quoteExitReasonAge}
		out.CancelReasons = append(out.CancelReasons, fmt.Sprintf("inventory age %dms reached the %ds threshold", v.InventoryAgeMs, p.MaxInventoryAgeSeconds))
		return quoteFinishOutcome(out)
	}

	var mid *big.Rat
	pricing := true
	if !ev.Healthy {
		pricing = false
		out.PolicyHolds = append(out.PolicyHolds, "input evidence is unhealthy: "+strings.Join(ev.Reasons, "; "))
	}
	if ev.Book == nil {
		pricing = false
		reason := ev.BookIssue
		if reason == "" {
			reason = "no eligible book at the deadline"
		}
		out.PolicyHolds = append(out.PolicyHolds, reason)
	} else if m, err := quotePolicyMid(ev.Book); err != nil {
		pricing = false
		out.PolicyHolds = append(out.PolicyHolds, err.Error())
	} else {
		mid = m
		v.Mid = quoteDecimalText(m)
		v.MidFloat = quoteFloatPtr(quoteRatFloat(m))
	}

	var sigma float64
	if pricing {
		want := p.VolatilityWindowSeconds + 1
		var mids []float64
		for _, s := range ev.Samples {
			if s.Missing != "" {
				break
			}
			r, err := parseQuoteDecimal(s.Mid)
			if err != nil || r.Sign() <= 0 {
				break
			}
			mids = append(mids, quoteRatFloat(r))
		}
		v.WarmupSamples = len(mids)
		if len(ev.Samples) != want || len(mids) != want {
			pricing = false
			out.PolicyHolds = append(out.PolicyHolds, fmt.Sprintf("volatility warmup has %d of %d continuous eligible one-second samples", len(mids), want))
		} else {
			sum := 0.0
			for i := 1; i < len(mids); i++ {
				l := math.Log(mids[i] / mids[i-1])
				sum += l * l
			}
			variance := sum / float64(p.VolatilityWindowSeconds)
			sigma = math.Sqrt(variance * float64(v.HorizonSeconds))
			v.Variance = quoteFloatPtr(variance)
			v.Sigma = quoteFloatPtr(sigma)
			if v.Variance == nil || v.Sigma == nil {
				pricing = false
				out.PolicyHolds = append(out.PolicyHolds, "the volatility estimate is not finite")
			}
		}
	}

	predicted := quoteIncreaseHoldMatches(snap.Holds)
	if in.SettlementHeld {
		predicted = append(predicted, "settlement held: "+strings.Join(in.SettlementHolds, "; "))
	}
	if snap.OperatorHold {
		predicted = append(predicted, "operator hold: "+snap.OperatorHoldReason)
	}
	if ev.Book == nil || !ev.Healthy {
		predicted = append(predicted, "the deadline evidence carries no fresh mark")
	}
	sort.Strings(predicted)
	out.PredictedHold = dedupeStrings(predicted)
	reducingForm := len(out.PredictedHold) > 0

	var bidPx, askPx *big.Rat
	var baseSize *big.Rat
	wide := false
	if pricing && !in.SettlementHeld {
		m := quoteRatFloat(mid)
		x := (v.InventoryFloat*m - p.TargetInventoryUSD) / ctx.CapUSD
		x = math.Max(-1, math.Min(1, x))
		v.NormalizedInventory = quoteFloatPtr(x)
		marked := math.Abs(v.InventoryFloat) * m
		v.MarkedInventoryUSD = quoteFloatPtr(marked)
		v.AboveCap = marked > ctx.CapUSD
		maxSkew := p.MaxSkewBps / 10000
		a := math.Max(-maxSkew, math.Min(maxSkew, p.RiskAversion*x*sigma))
		r := m * (1 - a)
		h := math.Max(p.MinSpreadBps/20000, math.Max(v.FeeFloor, p.VolatilityMultiplier*sigma))
		v.Skew, v.Reservation, v.HalfSpread = quoteFloatPtr(a), quoteFloatPtr(r), quoteFloatPtr(h)
		if h > p.MaxSpreadBps/20000 {
			wide = true
			out.PolicyHolds = append(out.PolicyHolds, fmt.Sprintf("half-spread %.6g is above half the %.6g bps maximum spread", h, p.MaxSpreadBps))
		}
		rawBid, rawAsk := r-m*h, r+m*h
		v.RawBid, v.RawAsk = quoteFloatPtr(rawBid), quoteFloatPtr(rawAsk)
		bestBid, _ := parseQuoteDecimal(ev.Book.BestBid)
		bestAsk, _ := parseQuoteDecimal(ev.Book.BestAsk)
		if rb, ok := quoteRatFromFloatOK(rawBid); ok && rb.Sign() > 0 {
			if b, err := ctx.Prices.roundQuotePx("buy", rb); err == nil {
				if b.Cmp(bestAsk) >= 0 {
					if b2, err := ctx.Prices.roundQuotePx("buy", quoteRatSub(bestAsk, quotePolicyTick(ctx.Prices, bestAsk))); err == nil {
						b = b2
					}
				}
				if b.Sign() > 0 && b.Cmp(bestAsk) < 0 {
					bidPx = b
				}
			} else {
				out.PolicyHolds = append(out.PolicyHolds, "bid rounding: "+err.Error())
			}
		}
		if ra, ok := quoteRatFromFloatOK(rawAsk); ok && ra.Sign() > 0 {
			if a2, err := ctx.Prices.roundQuotePx("sell", ra); err == nil {
				if a2.Cmp(bestBid) <= 0 {
					if a3, err := ctx.Prices.roundQuotePx("sell", quoteRatAdd(bestBid, quotePolicyTick(ctx.Prices, bestBid))); err == nil {
						a2 = a3
					}
				}
				if a2.Cmp(bestBid) > 0 {
					askPx = a2
				}
			} else {
				out.PolicyHolds = append(out.PolicyHolds, "ask rounding: "+err.Error())
			}
		}
		if bidPx == nil {
			out.PolicyHolds = append(out.PolicyHolds, "no valid bid price strictly below the current ask")
		}
		if askPx == nil {
			out.PolicyHolds = append(out.PolicyHolds, "no valid ask price strictly above the current bid")
		}
		if bidPx != nil && askPx != nil {
			v.Bid, v.Ask = quoteDecimalText(bidPx), quoteDecimalText(askPx)
			if askPx.Cmp(bidPx) <= 0 {
				out.PolicyHolds = append(out.PolicyHolds, "the rounded bid is not below the rounded ask")
				bidPx, askPx = nil, nil
			} else {
				maxFull, _ := quoteRatFromFloatOK(p.MaxSpreadBps / 10000)
				if maxFull != nil && quoteRatSub(askPx, bidPx).Cmp(quoteRatMul(mid, maxFull)) > 0 {
					wide = true
					out.PolicyHolds = append(out.PolicyHolds, "the rounded full spread is above the maximum spread")
				}
			}
		}
		if n, ok := quoteRatFromFloatOK(p.QuoteNotionalUSD); ok {
			sz := quoteRatQuo(n, mid)
			if mq, ok := quoteRatFromFloatOK(ctx.MaxOrderQty); ok && sz.Cmp(mq) > 0 {
				sz = mq
			}
			if f, err := ctx.Prices.floorQuoteSize(sz); err == nil {
				baseSize = f
				v.BaseSize = quoteDecimalText(f)
			} else {
				out.PolicyHolds = append(out.PolicyHolds, "size flooring: "+err.Error())
			}
		}
	}

	sides := map[string]*quoteSideRequest{}
	pxFor := func(role string) *big.Rat {
		if role == quoteRoleBid {
			return bidPx
		}
		return askPx
	}
	minValue, _ := quoteRatFromFloatOK(p.MinOrderValueUSD)
	if baseSize != nil && !wide && !reducingForm {
		taper := 1.0
		if ctx.CapUSD > 0 && p.TaperBandPct > 0 {
			marked := math.Abs(v.InventoryFloat) * quoteRatFloat(mid)
			taper = math.Min(1, math.Max(0, (ctx.CapUSD-marked)/(p.TaperBandPct/100*ctx.CapUSD)))
		}
		v.Taper = quoteFloatPtr(taper)
		var incSize *big.Rat
		if tr, ok := quoteRatFromFloatOK(taper); ok {
			if f, err := ctx.Prices.floorQuoteSize(quoteRatMul(baseSize, tr)); err == nil {
				incSize = f
				v.IncreasingSize = quoteDecimalText(f)
			}
		}
		for _, role := range increasingRoles {
			px := pxFor(role)
			switch {
			case px == nil:
			case incSize == nil || incSize.Sign() <= 0:
				out.PolicyHolds = append(out.PolicyHolds, fmt.Sprintf("the %s side floors to zero on the lot grid", role))
			case minValue != nil && quoteRatMul(incSize, px).Cmp(minValue) < 0:
				out.PolicyHolds = append(out.PolicyHolds, fmt.Sprintf("the %s side value %s is below the %s minimum order value", role, quoteDecimalText(quoteRatMul(incSize, px)), quoteDecimalText(minValue)))
			default:
				sides[role] = &quoteSideRequest{Px: quoteDecimalText(px), Size: quoteDecimalText(incSize)}
			}
		}
	}

	var reducing *quoteSideRequest
	if reducingRole != "" {
		absInv := quoteRatAbs(inv)
		cur := quotePolicySideOf(snap, reducingRole)
		if baseSize != nil && pxFor(reducingRole) != nil && !in.SettlementHeld {
			want, err := ctx.Prices.floorQuoteSize(quoteRatMin(baseSize, absInv))
			if err == nil && want.Sign() > 0 {
				plan := planHLCloseOrder(ctx.Coin, posSide, quoteRatFloat(absInv), quoteRatFloat(want), 0, 0, hlOnChainCoinView{Known: false})
				if plan.Action == hlCloseSend {
					planned, _ := quoteRatFromFloatOK(plan.Size)
					if planned != nil {
						size, err := ctx.Prices.floorQuoteSize(quoteRatMin(planned, want))
						if err == nil && size.Sign() > 0 && size.Cmp(absInv) <= 0 {
							reducing = &quoteSideRequest{Px: quoteDecimalText(pxFor(reducingRole)), Size: quoteDecimalText(size), ReduceOnly: true}
							v.ReducingSize = quoteDecimalText(size)
						}
					}
				} else {
					out.PolicyHolds = append(out.PolicyHolds, "the close planner holds the reducing side: "+plan.Reason)
				}
			} else {
				out.PolicyHolds = append(out.PolicyHolds, "the reducing side floors to zero on the lot grid")
			}
		} else if quoteSnapshotSideRests(cur) && cur.Request.ReduceOnly {
			if sz, err := parseQuoteDecimal(cur.Request.Size); err == nil && sz.Cmp(absInv) <= 0 {
				reducing = &quoteSideRequest{Px: cur.Request.Px, Size: cur.Request.Size, ReduceOnly: true}
				out.Refresh[reducingRole] = "repeated the confirmed resting reducing side; no fresh pricing evidence"
			}
		}
	}

	refresh := func(role string, want *quoteSideRequest) *quoteSideRequest {
		cur := quotePolicySideOf(snap, role)
		if !quoteSnapshotSideRests(cur) {
			return want
		}
		if cur.AcceptedAtMs > 0 && ev.DeadlineMs >= cur.AcceptedAtMs+int64(p.QuoteLifetimeSeconds)*1000 {
			out.Refresh[role] = fmt.Sprintf("expired: accepted at %d, lifetime %ds", cur.AcceptedAtMs, p.QuoteLifetimeSeconds)
			return nil
		}
		if want == nil {
			out.Refresh[role] = "the side is no longer desired"
			return nil
		}
		curSide := &quoteSideRequest{Px: cur.Request.Px, Size: cur.Request.Size, ReduceOnly: cur.Request.ReduceOnly}
		if curSide.same(want) {
			out.Refresh[role] = "kept: identical terms"
			return curSide
		}
		cp, e1 := parseQuoteDecimal(cur.Request.Px)
		wp, e2 := parseQuoteDecimal(want.Px)
		cs, e3 := parseQuoteDecimal(cur.Request.Size)
		ws, e4 := parseQuoteDecimal(want.Size)
		if e1 != nil || e2 != nil || e3 != nil || e4 != nil || mid == nil {
			out.Refresh[role] = "refresh: terms unreadable"
			return want
		}
		if cur.Request.ReduceOnly != want.ReduceOnly || cs.Cmp(ws) != 0 {
			out.Refresh[role] = "refresh: size or reduce-only terms changed"
			return want
		}
		threshold, _ := quoteRatFromFloatOK(p.RefreshThresholdBps / 10000)
		move := quoteRatAbs(quoteRatSub(wp, cp))
		if threshold != nil && move.Cmp(quoteRatMul(mid, threshold)) < 0 {
			out.Refresh[role] = "kept: price movement below the refresh threshold"
			return curSide
		}
		out.Refresh[role] = "refresh: price movement reached the threshold"
		return want
	}

	if reducingForm {
		out.Form = quotePolicyFormReducing
		out.Exit = quoteExitRequest{Kind: quoteExitKindReducing}
		if reducing != nil {
			kept := refresh(reducingRole, reducing)
			if kept != nil {
				kept.ReduceOnly = true
			}
			if reducingRole == quoteRoleBid {
				out.Exit.ReducingBid = kept
			} else {
				out.Exit.ReducingAsk = kept
			}
		}
		for _, role := range []string{quoteRoleBid, quoteRoleAsk} {
			if role != reducingRole {
				if cur := quotePolicySideOf(snap, role); quoteSnapshotSideRests(cur) {
					out.CancelReasons = append(out.CancelReasons, fmt.Sprintf("the resting %s side is cancelled while increases are held", role))
				}
			}
		}
		return quoteFinishOutcome(out)
	}

	if reducing != nil {
		sides[reducingRole] = reducing
	}
	out.Form = quotePolicyFormPair
	out.Desired.Bid = refresh(quoteRoleBid, sides[quoteRoleBid])
	out.Desired.Ask = refresh(quoteRoleAsk, sides[quoteRoleAsk])
	if out.Desired.Bid == nil && out.Desired.Ask == nil {
		out.Form = quotePolicyFormNone
	}
	return quoteFinishOutcome(out)
}

func quoteFinishOutcome(out quotePolicyOutcome) quotePolicyOutcome {
	sort.Strings(out.PolicyHolds)
	out.PolicyHolds = dedupeStrings(out.PolicyHolds)
	sort.Strings(out.CancelReasons)
	if len(out.Refresh) == 0 {
		out.Refresh = nil
	}
	out.Decision = quoteDecision{Desired: out.Desired, Exit: out.Exit}
	return out
}
