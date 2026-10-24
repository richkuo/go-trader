package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
)

const (
	basketVenueFilled   = "filled"
	basketVenuePartial  = "partial"
	basketVenueRejected = "rejected"
	basketVenueUnknown  = "unknown"

	pairVenueOpen      = "open"
	pairVenueNeverSent = "never_sent"
	pairVenueAbsent    = "absent"
)

type basketVenueOutcome struct {
	Outcome      string  `json:"outcome"`
	FilledQty    float64 `json:"filled_qty"`
	AvgPx        float64 `json:"avg_px,omitempty"`
	Fee          float64 `json:"fee,omitempty"`
	VenueOrderID string  `json:"venue_order_id,omitempty"`
	Detail       string  `json:"detail,omitempty"`
	ObservedAtMs int64   `json:"observed_at_ms"`

	ClientOrderID      string  `json:"client_order_id,omitempty"`
	AccountKey         string  `json:"account_key,omitempty"`
	Instrument         string  `json:"instrument,omitempty"`
	Side               string  `json:"side,omitempty"`
	CumQty             float64 `json:"cum_qty,omitempty"`
	CumNotional        float64 `json:"cum_notional,omitempty"`
	AcceptedAtMs       int64   `json:"accepted_at_ms,omitempty"`
	Terminal           bool    `json:"terminal,omitempty"`
	CoverageThroughMs  int64   `json:"coverage_through_ms,omitempty"`
	CoverageComplete   bool    `json:"coverage_complete,omitempty"`
	ContraryEvidence   bool    `json:"contrary_evidence,omitempty"`
	FeePresent         bool    `json:"fee_present,omitempty"`
	FeeCurrency        string  `json:"fee_currency,omitempty"`
	FeeAmount          float64 `json:"fee_amount,omitempty"`
	FeeEvidenceID      string  `json:"fee_evidence_id,omitempty"`
	FeeEvidenceVersion int64   `json:"fee_evidence_version,omitempty"`
	FeeFinal           bool    `json:"fee_final,omitempty"`

	FillCoverage []pairConfirmedFill `json:"fill_coverage,omitempty"`
	FeeCoverage  []pairFeeFill       `json:"fee_coverage,omitempty"`
	Fills        []pairVenueFill     `json:"fills,omitempty"`
}

type pairMemberValuation struct {
	Mark         float64            `json:"mark"`
	UpperPx      float64            `json:"upper_px"`
	LowerPx      float64            `json:"lower_px"`
	EvidenceMs   int64              `json:"evidence_ms"`
	MaxAgeMs     int64              `json:"max_age_ms"`
	ContractSize float64            `json:"contract_size"`
	Conversions  map[string]float64 `json:"conversions"`
	FeeRate      float64            `json:"fee_rate"`
}

func (v pairMemberValuation) conversion(currency string) (float64, bool) {
	c, ok := v.Conversions[strings.ToUpper(strings.TrimSpace(currency))]
	return c, ok && basketFinitePositive(c)
}

type pairVenue interface {
	basketVenue
	Virtual() bool
	ExecutionModel() string
	Valuation(member pairContractMember, nowMs int64) (pairMemberValuation, error)
}

type basketVenue interface {
	Name() string
	LiveShaped() bool
	Provider() string
	AccountKey(p RiskPartition) string
	NewClientOrderID() (string, error)
	Submit(a *basketAction) (basketVenueOutcome, error)
	Query(a *basketAction) (basketVenueOutcome, error)
}

type basketPaperVenue struct{}

func (basketPaperVenue) Name() string     { return "paper" }
func (basketPaperVenue) LiveShaped() bool { return false }
func (basketPaperVenue) Provider() string { return "paper" }

func (basketPaperVenue) AccountKey(p RiskPartition) string { return "paper:" + p.String() }

func (basketPaperVenue) NewClientOrderID() (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "bp-" + hex.EncodeToString(b[:]), nil
}

func (basketPaperVenue) Submit(a *basketAction) (basketVenueOutcome, error) {
	return basketVenueOutcome{Outcome: basketVenueFilled, FilledQty: a.RequestedQty, ObservedAtMs: a.UpdatedAtMs}, nil
}

func (basketPaperVenue) Query(a *basketAction) (basketVenueOutcome, error) {
	return basketVenueOutcome{Outcome: basketVenueRejected, Detail: "the paper fill was not committed before the restart, so nothing was booked"}, nil
}

var basketVenueFor = func(sc StrategyConfig) (basketVenue, error) {
	if msg := basketLiveRefusal(sc); msg != "" {
		return nil, fmt.Errorf("%s", msg)
	}
	if isLiveArgs(sc.Args) {
		return nil, fmt.Errorf("strategy[%s]: live basket dispatch is refused", sc.ID)
	}
	return basketPaperVenue{}, nil
}

type basketBooking struct {
	Qty      float64
	Px       float64
	Fee      float64
	Trades   []Trade
	Hold     string
	Stopped  bool
	PostQty  float64
	PostSide string
}

func basketPositionSigned(s *StrategyState, coin string) float64 {
	if s == nil {
		return 0
	}
	pos := s.Positions[coin]
	if pos == nil || pos.Quantity <= 0 {
		return 0
	}
	if pos.Side == entrySideShort {
		return -pos.Quantity
	}
	return pos.Quantity
}

func basketTradesSince(s *StrategyState, from int, coin string) []Trade {
	if s == nil || from < 0 || from > len(s.TradeHistory) {
		return nil
	}
	var out []Trade
	for _, t := range s.TradeHistory[from:] {
		if t.Symbol == coin && t.TradeType != TradeTypeFunding {
			out = append(out, t)
		}
	}
	return out
}

func summarizeBasketBooking(s *StrategyState, from int, coin string, wantClose bool, side string) basketBooking {
	b := basketBooking{Trades: basketTradesSince(s, from, coin)}
	var notional float64
	for _, t := range b.Trades {
		if t.IsClose == wantClose && t.Side == side && t.Quantity > 0 {
			b.Qty += t.Quantity
			notional += t.Quantity * t.Price
			b.Fee += t.ExchangeFee
		} else if t.IsClose && !wantClose {
			b.Stopped = true
		}
	}
	if b.Qty > 0 {
		b.Px = notional / b.Qty
	}
	if pos := s.Positions[coin]; pos != nil {
		b.PostQty, b.PostSide = pos.Quantity, pos.Side
	}
	return b
}

func bookBasketPaperIncrease(sc StrategyConfig, s *StrategyState, cfg *Config, coin, positionSide string, qty, mark float64, obs basketMemberObservation, logger *StrategyLogger) basketBooking {
	signal := 1
	orderSide := "buy"
	if positionSide == entrySideShort {
		signal = -1
		orderSide = "sell"
	}
	result := &HyperliquidResult{
		StrategyDecisionFields: obs.Fields,
		Strategy:               basketMemberStrategyToken,
		Symbol:                 coin,
		Signal:                 signal,
		Price:                  mark,
		Indicators:             obs.Indicators,
		Mode:                   "paper",
		Platform:               "hyperliquid",
		BasketMember:           true,
	}
	var regime *RegimeConfig
	if cfg != nil {
		regime = cfg.Regime
	}
	from := len(s.TradeHistory)
	pos := s.Positions[coin]
	switch {
	case pos == nil || pos.Quantity <= 0:
		result.BasketOpenQty = qty
		executeHyperliquidResultDeferredOpen(sc, s, result, nil, signalLabel(signal), mark, regime, cfg, HurstGateDecision{}, logger)
	case pos.Side == positionSide:
		executeHyperliquidScaleInDeferredOpen(sc, s, result, nil, signalLabel(signal), mark, qty, logger)
	default:
		return basketBooking{Hold: fmt.Sprintf("%s holds a %s position; a %s increase needs a flat or same-side book", coin, pos.Side, positionSide)}
	}
	return summarizeBasketBooking(s, from, coin, false, orderSide)
}

func bookBasketPaperReduction(sc StrategyConfig, s *StrategyState, coin string, qty, mark float64, logger *StrategyLogger) basketBooking {
	pos := s.Positions[coin]
	if pos == nil || pos.Quantity <= 0 {
		return basketBooking{Hold: fmt.Sprintf("%s has no position to reduce", coin)}
	}
	isBuy := pos.Side == entrySideShort
	signal := -1
	orderSide := "sell"
	if isBuy {
		signal = 1
		orderSide = "buy"
	}
	from := len(s.TradeHistory)
	lot := newHLPaperLotPolicy(sc, coin, mark, false)
	var exec SignalExecutionResult
	var err error
	if basketQtyEqual(qty, pos.Quantity) || qty >= pos.Quantity {
		exec, err = executePerpsSignalWithLotPolicyDeferredOpen(s, signal, coin, mark, PerpsSizing{}, 0, "", 0, EffectiveDirection(sc), 1.0, logger, lot)
	} else {
		exec, err = executePerpsSignalWithLotPolicyDeferredOpen(s, signal, coin, ApplyAdverseSlippage(mark, isBuy), PerpsSizing{}, qty, "", 0, EffectiveDirection(sc), qty/pos.Quantity, logger, lot)
	}
	if err != nil {
		return basketBooking{Hold: err.Error()}
	}
	b := summarizeBasketBooking(s, from, coin, true, orderSide)
	if b.Qty <= 0 && exec.HoldReason != "" {
		b.Hold = exec.HoldReason
	}
	return b
}

type pairBookFill struct {
	Qty float64
	Px  float64
	Fee pairConfirmedFee
	OID string
}

func bookPairFill(sc StrategyConfig, s *StrategyState, cfg *Config, a *basketAction, qty, px float64, fee pairConfirmedFee, obs basketMemberObservation, logger *StrategyLogger) basketBooking {
	if s == nil || !isPairMember(sc) || a == nil || a.StrategyID != sc.ID {
		return basketBooking{Hold: "the pair booking owner needs the action's pair member and its book"}
	}
	var b basketBooking
	var feeErr error
	if err := withPairDispatchCostScope(sc, s, func() {
		feeErr = withPairConfirmedFeeScope(s, fee, func() {
			b = bookPairFillScoped(sc, s, cfg, a, qty, px, fee, obs, logger)
		})
	}); err != nil {
		return basketBooking{Hold: "the member cost binding refused the booking before any book change: " + err.Error()}
	}
	if feeErr != nil {
		return basketBooking{Hold: "the confirmed fee was refused before any book change: " + feeErr.Error()}
	}
	return b
}

func bookPairFillScoped(sc StrategyConfig, s *StrategyState, cfg *Config, a *basketAction, qty, px float64, fee pairConfirmedFee, obs basketMemberObservation, logger *StrategyLogger) basketBooking {
	coin := a.Symbol
	pos := s.Positions[coin]
	oid := a.VenueOrderID
	if oid == "" {
		oid = a.ClientOrderID
	}
	from := len(s.TradeHistory)
	if a.Direction == basketDirectionIncrease {
		signal := 1
		if a.PositionSide == entrySideShort {
			signal = -1
		}
		if pos != nil && pos.Quantity > 0 && pos.Side != a.PositionSide {
			return basketBooking{Hold: fmt.Sprintf("%s holds a %s position; a %s increase needs a flat or same-side book", coin, pos.Side, a.PositionSide)}
		}
		switch sc.Platform {
		case "hyperliquid":
			var regime *RegimeConfig
			if cfg != nil {
				regime = cfg.Regime
			}
			result := &HyperliquidResult{
				StrategyDecisionFields: obs.Fields,
				Strategy:               pairMemberStrategyToken,
				Symbol:                 coin,
				Signal:                 signal,
				Price:                  px,
				Indicators:             obs.Indicators,
				Mode:                   hyperliquidModeFromArgs(sc.Args),
				Platform:               "hyperliquid",
				PairFill:               &pairBookFill{Qty: qty, Px: px, Fee: fee, OID: oid},
			}
			if pos == nil || pos.Quantity <= 0 {
				executeHyperliquidResultDeferredOpen(sc, s, result, nil, signalLabel(signal), px, regime, cfg, HurstGateDecision{}, logger)
			} else {
				executeHyperliquidScaleInDeferredOpen(sc, s, result, nil, signalLabel(signal), px, qty, logger)
			}
		case "okx":
			if pos == nil || pos.Quantity <= 0 {
				exec, err := ExecutePerpsSignalWithLeverageDeferredOpen(s, signal, coin, px, PerpsSizingFor(sc, px, indicatorsATRValue(obs.Indicators)), qty, oid, fee.DeltaUSD, EffectiveDirection(sc), 0, logger)
				if err != nil {
					return basketBooking{Hold: err.Error()}
				}
				stampEntryATRIfOpened(s, coin, obs.Indicators)
				if p := s.Positions[coin]; p != nil {
					recordPositionOpen(s, sc, exec.OpenTrade, p)
				}
			} else {
				trades, openTrade := applyPerpsScaleIn(s, sc, coin, px, qty, fee.DeltaUSD, oid, fee.Present, logger)
				if trades > 0 {
					if p := s.Positions[coin]; p != nil {
						recordPositionOpen(s, sc, openTrade, p)
					}
				}
			}
		default:
			return basketBooking{Hold: fmt.Sprintf("platform %s has no pair booking owner", sc.Platform)}
		}
		orderSide := "buy"
		if signal < 0 {
			orderSide = "sell"
		}
		return summarizeBasketBooking(s, from, coin, false, orderSide)
	}
	if pos == nil || pos.Quantity <= 0 {
		return basketBooking{Hold: fmt.Sprintf("%s has no position to reduce", coin)}
	}
	if pos.Side != a.PositionSide {
		return basketBooking{Hold: fmt.Sprintf("%s holds a %s position, the reduction names %s", coin, pos.Side, a.PositionSide)}
	}
	signal, orderSide := -1, "sell"
	if pos.Side == entrySideShort {
		signal, orderSide = 1, "buy"
	}
	frac := 1.0
	if !basketQtyEqual(qty, pos.Quantity) && qty < pos.Quantity {
		frac = qty / pos.Quantity
	} else {
		qty = pos.Quantity
	}
	if _, err := executePerpsSignalWithLotPolicyDeferredOpen(s, signal, coin, px, PerpsSizing{}, qty, oid, fee.DeltaUSD, EffectiveDirection(sc), frac, logger, nil); err != nil {
		return basketBooking{Hold: err.Error()}
	}
	return summarizeBasketBooking(s, from, coin, true, orderSide)
}
