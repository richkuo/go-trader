package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

const (
	basketVenueFilled   = "filled"
	basketVenuePartial  = "partial"
	basketVenueRejected = "rejected"
	basketVenueUnknown  = "unknown"
)

type basketVenueOutcome struct {
	Outcome      string  `json:"outcome"`
	FilledQty    float64 `json:"filled_qty"`
	AvgPx        float64 `json:"avg_px,omitempty"`
	Fee          float64 `json:"fee,omitempty"`
	VenueOrderID string  `json:"venue_order_id,omitempty"`
	Detail       string  `json:"detail,omitempty"`
	ObservedAtMs int64   `json:"observed_at_ms"`
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
	return basketVenueOutcome{Outcome: basketVenueFilled, FilledQty: a.RequestedQty, ObservedAtMs: time.Now().UnixMilli()}, nil
}

func (basketPaperVenue) Query(a *basketAction) (basketVenueOutcome, error) {
	return basketVenueOutcome{Outcome: basketVenueRejected, Detail: "the paper fill was not committed before the restart, so nothing was booked", ObservedAtMs: time.Now().UnixMilli()}, nil
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
