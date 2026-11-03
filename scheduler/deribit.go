package main

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const deribitAPIBase = "https://www.deribit.com/api/v2"

type deribitTickerResult struct {
	InstrumentName  string   `json:"instrument_name"`
	MarkPrice       *float64 `json:"mark_price"`
	UnderlyingPrice *float64 `json:"underlying_price"`
	Greeks          *struct {
		Delta *float64 `json:"delta"`
		Gamma *float64 `json:"gamma"`
		Theta *float64 `json:"theta"`
		Vega  *float64 `json:"vega"`
	} `json:"greeks"`
}

type deribitRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type DeribitPricer struct {
	client  *http.Client
	baseURL string
}

const (
	deribitMarkSource             = "deribit_ticker"
	deribitSettlementSource       = "deribit_delivery_price"
	okxDeribitProxySource         = "deribit_proxy"
	deribitDeliveryPageSize       = 100
	deribitDeliveryMaxPages       = 40
	deribitDeliveryIndexUSDSuffix = "_usd"
)

func NewDeribitPricer() *DeribitPricer {
	return &DeribitPricer{
		client: egressHTTPClient(10 * time.Second),
	}
}

func (d *DeribitPricer) apiBase() string {
	if d.baseURL != "" {
		return d.baseURL
	}
	return deribitAPIBase
}

func (d *DeribitPricer) getJSON(path string, out interface{}) error {
	resp, err := d.client.Get(d.apiBase() + path)
	if err != nil {
		return fmt.Errorf("deribit API error: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("deribit API read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("deribit API status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("deribit decode error: %w", err)
	}
	return nil
}

func (d *DeribitPricer) fetchTickerFull(instrument string) (*deribitTickerResult, error) {
	var env struct {
		Result *deribitTickerResult `json:"result"`
		Error  *deribitRPCError     `json:"error"`
	}
	if err := d.getJSON("/public/ticker?instrument_name="+url.QueryEscape(instrument), &env); err != nil {
		return nil, err
	}
	if env.Error != nil {
		return nil, fmt.Errorf("deribit ticker %s: %s (code %d)", instrument, env.Error.Message, env.Error.Code)
	}
	if env.Result == nil {
		return nil, fmt.Errorf("deribit ticker %s: no result", instrument)
	}
	return env.Result, nil
}

func (d *DeribitPricer) Name() string { return "deribit" }

func (d *DeribitPricer) Refresh(reqs []optionLegRequest, refreshAt time.Time) []optionLegReply {
	return d.refreshFor("deribit", reqs, refreshAt)
}

type okxDeribitProxyPricer struct{ d *DeribitPricer }

func (d *DeribitPricer) OKXProxy() OptionPricer { return okxDeribitProxyPricer{d: d} }

func (p okxDeribitProxyPricer) Name() string { return "okx(deribit_proxy)" }

func (p okxDeribitProxyPricer) Refresh(reqs []optionLegRequest, refreshAt time.Time) []optionLegReply {
	return p.d.refreshFor("okx", reqs, refreshAt)
}

type deribitDeliveryLookup struct {
	price float64
	found bool
	err   error
}

func (d *DeribitPricer) refreshFor(platform string, reqs []optionLegRequest, refreshAt time.Time) []optionLegReply {
	markSource, settleSource := deribitMarkSource, deribitSettlementSource
	if platform == "okx" {
		markSource, settleSource = okxDeribitProxySource, okxDeribitProxySource
	}
	deliveries := map[string]deribitDeliveryLookup{}
	out := make([]optionLegReply, 0, len(reqs))
	for _, req := range reqs {
		reply := optionLegReply{Key: req.Key}
		cutoff, ready, ok, err := optionGoExpirySchedule(platform, req.Expiry)
		if err != nil || !ok {
			reply.Err = fmt.Sprintf("no %s expiry schedule for %q", platform, req.Expiry)
			if err != nil {
				reply.Err = err.Error()
			}
			out = append(out, reply)
			continue
		}
		reply.CutoffAt, reply.ReadyAt = cutoff, ready
		if err := optionPricingPolicyError(platform, req.Underlying); err != nil {
			reply.Err = err.Error()
			out = append(out, reply)
			continue
		}
		if !refreshAt.Before(cutoff) {
			index := strings.ToLower(strings.TrimSpace(req.Underlying)) + deribitDeliveryIndexUSDSuffix
			cacheKey := index + "|" + req.Expiry
			lookup, seen := deliveries[cacheKey]
			if !seen {
				lookup.price, lookup.found, lookup.err = d.deliveryPrice(index, req.Expiry)
				deliveries[cacheKey] = lookup
			}
			switch {
			case lookup.err != nil:
				reply.Err = lookup.err.Error()
			case !lookup.found:
				reply.Pending = fmt.Sprintf("%s delivery price for %s %s is not published yet", settleSource, index, req.Expiry)
			default:
				reply.Settlement = &optionLegSettlement{Price: lookup.price, Basis: optionSettlementBasisDelivery, Date: req.Expiry, Source: settleSource}
			}
			out = append(out, reply)
			continue
		}
		mark, err := d.exactMark(req, markSource)
		if err != nil {
			reply.Err = err.Error()
		} else {
			reply.Mark = mark
		}
		out = append(out, reply)
	}
	return out
}

func deribitFinite(v *float64) bool {
	return v != nil && !math.IsNaN(*v) && !math.IsInf(*v, 0)
}

func (d *DeribitPricer) exactMark(req optionLegRequest, source string) (*optionLegMark, error) {
	instrument := d.formatInstrument(req.Underlying, req.OptionType, req.Strike, req.Expiry)
	if instrument == "" {
		return nil, fmt.Errorf("invalid instrument for expiry %q", req.Expiry)
	}
	t, err := d.fetchTickerFull(instrument)
	if err != nil {
		return nil, err
	}
	if t.InstrumentName != "" && t.InstrumentName != instrument {
		return nil, fmt.Errorf("deribit ticker answered %s for %s", t.InstrumentName, instrument)
	}
	if !deribitFinite(t.MarkPrice) || *t.MarkPrice < 0 {
		return nil, fmt.Errorf("deribit ticker %s: mark_price missing or invalid", instrument)
	}
	if !deribitFinite(t.UnderlyingPrice) || *t.UnderlyingPrice <= 0 {
		return nil, fmt.Errorf("deribit ticker %s: underlying_price missing or not positive", instrument)
	}
	if t.Greeks == nil || !deribitFinite(t.Greeks.Delta) || !deribitFinite(t.Greeks.Gamma) || !deribitFinite(t.Greeks.Theta) || !deribitFinite(t.Greeks.Vega) {
		return nil, fmt.Errorf("deribit ticker %s: greeks missing or not finite", instrument)
	}
	perUnit := *t.MarkPrice * *t.UnderlyingPrice
	if math.IsNaN(perUnit) || math.IsInf(perUnit, 0) {
		return nil, fmt.Errorf("deribit ticker %s: mark value overflows", instrument)
	}
	return &optionLegMark{
		PerUnitUSD: perUnit,
		Spot:       *t.UnderlyingPrice,
		Greeks:     OptGreeks{Delta: *t.Greeks.Delta, Gamma: *t.Greeks.Gamma, Theta: *t.Greeks.Theta, Vega: *t.Greeks.Vega},
		Source:     source,
	}, nil
}

func (d *DeribitPricer) deliveryPrice(index, date string) (float64, bool, error) {
	target, err := time.Parse("2006-01-02", date)
	if err != nil {
		return 0, false, fmt.Errorf("invalid delivery date %q", date)
	}
	var last time.Time
	for page := 0; page < deribitDeliveryMaxPages; page++ {
		offset := page * deribitDeliveryPageSize
		var env struct {
			Result *struct {
				Data []struct {
					Date          string   `json:"date"`
					DeliveryPrice *float64 `json:"delivery_price"`
				} `json:"data"`
				RecordsTotal *int `json:"records_total"`
			} `json:"result"`
			Error *deribitRPCError `json:"error"`
		}
		path := fmt.Sprintf("/public/get_delivery_prices?index_name=%s&offset=%d&count=%d", url.QueryEscape(index), offset, deribitDeliveryPageSize)
		if err := d.getJSON(path, &env); err != nil {
			return 0, false, err
		}
		if env.Error != nil {
			return 0, false, fmt.Errorf("deribit delivery prices %s: %s (code %d)", index, env.Error.Message, env.Error.Code)
		}
		if env.Result == nil || env.Result.RecordsTotal == nil {
			return 0, false, fmt.Errorf("deribit delivery prices %s: response has no result or records_total", index)
		}
		rows := env.Result.Data
		for _, row := range rows {
			day, err := time.Parse("2006-01-02", row.Date)
			if err != nil {
				return 0, false, fmt.Errorf("deribit delivery prices %s: invalid date %q", index, row.Date)
			}
			if !last.IsZero() && !day.Before(last) {
				return 0, false, fmt.Errorf("deribit delivery prices %s: dates are not strictly descending at %s", index, row.Date)
			}
			last = day
			if day.Equal(target) {
				if row.DeliveryPrice == nil || !finitePositive(*row.DeliveryPrice) {
					return 0, false, fmt.Errorf("deribit delivery price %s %s is missing or not positive", index, date)
				}
				return *row.DeliveryPrice, true, nil
			}
			if day.Before(target) {
				return 0, false, nil
			}
		}
		if len(rows) == 0 || offset+len(rows) >= *env.Result.RecordsTotal {
			return 0, false, nil
		}
	}
	return 0, false, fmt.Errorf("deribit delivery prices %s: %s is older than the %d-row search limit", index, date, deribitDeliveryPageSize*deribitDeliveryMaxPages)
}

func (d *DeribitPricer) formatInstrument(underlying, optionType string, strike float64, expiry string) string {
	t, err := time.Parse("2006-01-02", expiry)
	if err != nil {
		return ""
	}

	day := t.Format("02")
	month := strings.ToUpper(t.Format("Jan"))
	year := t.Format("06")

	optType := "C"
	if strings.ToLower(optionType) == "put" {
		optType = "P"
	}

	instrument := fmt.Sprintf("%s-%s%s%s-%.0f-%s",
		strings.ToUpper(underlying),
		day,
		month,
		year,
		strike,
		optType,
	)

	return instrument
}

func applyAssignment(s *StrategyState, r optionAssignment, logger *StrategyLogger) {
	symbol := strings.ToUpper(r.AssignUnderlying)
	switch r.AssignOptionType {
	case "put":
		cost := r.AssignStrike * r.AssignQuantity
		s.Cash -= cost
		now := time.Now().UTC()
		var positionID string
		if existing, ok := s.Positions[symbol]; ok && existing.Side == "long" {
			totalQty := existing.Quantity + r.AssignQuantity
			existing.AvgCost = (existing.AvgCost*existing.Quantity + r.AssignStrike*r.AssignQuantity) / totalQty
			existing.Quantity = totalQty
			positionID = ensurePositionTradeID(s.ID, symbol, existing)
		} else {
			positionID = newTradePositionID(s.ID, symbol, now)
			s.Positions[symbol] = &Position{
				Symbol:          symbol,
				TradePositionID: positionID,
				Quantity:        r.AssignQuantity,
				InitialQuantity: r.AssignQuantity,
				AvgCost:         r.AssignStrike,
				Side:            "long",
				OpenedAt:        now,
			}
		}
		RecordTrade(s, Trade{
			Timestamp:  now,
			StrategyID: s.ID,
			Symbol:     symbol,
			PositionID: positionID,
			Side:       "buy",
			Quantity:   r.AssignQuantity,
			Price:      r.AssignStrike,
			Value:      cost,
			TradeType:  "assignment",
			Details: fmt.Sprintf("Wheel assignment: sold put expired ITM (spot=$%.2f), bought %.4f %s @ $%.0f",
				r.AssignSpotPrice, r.AssignQuantity, symbol, r.AssignStrike),
			Regime: s.Regime,
		})
		logger.Info("ASSIGNMENT: sold put %s-%.0f expired ITM (spot=$%.2f), bought %.4f %s @ $%.0f (cash debit=$%.2f)",
			r.AssignUnderlying, r.AssignStrike, r.AssignSpotPrice, r.AssignQuantity, symbol, r.AssignStrike, cost)

	case "call":
		now := time.Now().UTC()
		existing, held := s.Positions[symbol]
		if held && existing.Side != "long" {
			existing, held = nil, false
		}
		heldQty := 0.0
		if held && existing.Quantity > 0 {
			heldQty = existing.Quantity
		}
		delivered := math.Min(heldQty, r.AssignQuantity)
		uncovered := r.AssignQuantity - delivered
		if uncovered > 0 {
			intrinsic := math.Max(r.AssignSpotPrice-r.AssignStrike, 0)
			settlement := intrinsic * uncovered
			s.Cash -= settlement
			logger.Warn("UNCOVERED CALL ASSIGNMENT: sold call %s-%.0f expired ITM (spot=$%.2f) with %.4f of %.4f %s shares held; cash-settled %.4f uncovered shares for $%.2f intrinsic, no strike proceeds for missing shares",
				r.AssignUnderlying, r.AssignStrike, r.AssignSpotPrice, delivered, r.AssignQuantity, symbol, uncovered, settlement)
		}
		if delivered <= 0 {
			return
		}
		proceeds := r.AssignStrike * delivered
		s.Cash += proceeds
		pnl := 0.0
		var positionID string
		var posEntryATR, posStopLossTriggerPx float64
		if held {
			positionID = ensurePositionTradeID(s.ID, symbol, existing)
			pnl = (r.AssignStrike - existing.AvgCost) * delivered
			posEntryATR = existing.EntryATR
			posStopLossTriggerPx = existing.StopLossTriggerPx
			newQty := existing.Quantity - delivered
			if newQty <= 0 {
				recordClosedPosition(s, existing, r.AssignStrike, pnl, "assignment", now)
				delete(s.Positions, symbol)
			} else {
				existing.Quantity = newQty
			}
			RecordTradeResult(&s.RiskState, pnl)
		}
		RecordTrade(s, Trade{
			Timestamp:  now,
			StrategyID: s.ID,
			Symbol:     symbol,
			PositionID: positionID,
			Side:       "sell",
			Quantity:   delivered,
			Price:      r.AssignStrike,
			Value:      proceeds,
			TradeType:  "assignment",
			Details: fmt.Sprintf("Wheel call-away: sold call expired ITM (spot=$%.2f), sold %.4f %s @ $%.0f PnL=$%.2f",
				r.AssignSpotPrice, delivered, symbol, r.AssignStrike, pnl),
			IsClose:           true,
			RealizedPnL:       pnl,
			PnLGross:          true,
			Regime:            s.Regime,
			EntryATR:          posEntryATR,
			StopLossTriggerPx: posStopLossTriggerPx,
		})
		logger.Info("CALL-AWAY: sold call %s-%.0f expired ITM (spot=$%.2f), sold %.4f %s @ $%.0f (proceeds=$%.2f, PnL=$%.2f)",
			r.AssignUnderlying, r.AssignStrike, r.AssignSpotPrice, delivered, symbol, r.AssignStrike, proceeds, pnl)
	}
}
