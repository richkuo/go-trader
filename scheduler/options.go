package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

type OptionPosition struct {
	ID                 string    `json:"id"`
	TradePositionID    string    `json:"position_id,omitempty"`
	Underlying         string    `json:"underlying"`
	OptionType         string    `json:"option_type"`
	Strike             float64   `json:"strike"`
	Expiry             string    `json:"expiry"`
	DTE                float64   `json:"dte"`
	Action             string    `json:"action"`
	Quantity           float64   `json:"quantity"`
	ContractMultiplier float64   `json:"contract_multiplier,omitempty"`
	EntryPremium       float64   `json:"entry_premium"`
	EntryPremiumUSD    float64   `json:"entry_premium_usd"`
	CurrentValueUSD    float64   `json:"current_value_usd"`
	Greeks             OptGreeks `json:"greeks"`
	OpenedAt           time.Time `json:"opened_at"`
	MarkUpdatedAt      time.Time `json:"mark_updated_at,omitzero"`
	MarkError          string    `json:"mark_error,omitempty"`
	MarkSource         string    `json:"mark_source,omitempty"`
	ExpiryCutoffAt     time.Time `json:"expiry_cutoff_at,omitzero"`
	SettlementReadyAt  time.Time `json:"settlement_ready_at,omitzero"`
	markVerifiedAt     time.Time
}

type OptGreeks struct {
	Delta float64 `json:"delta"`
	Gamma float64 `json:"gamma"`
	Theta float64 `json:"theta"`
	Vega  float64 `json:"vega"`
}

type OptionsAction struct {
	Action     string    `json:"action"`
	OptionType string    `json:"option_type"`
	Strike     float64   `json:"strike"`
	Expiry     string    `json:"expiry"`
	DTE        float64   `json:"dte"`
	Premium    float64   `json:"premium"`
	PremiumUSD float64   `json:"premium_usd"`
	Quantity   float64   `json:"quantity,omitempty"`
	Greeks     OptGreeks `json:"greeks"`

	StructureID      string `json:"structure_id,omitempty"`
	StructureLegs    int    `json:"structure_legs,omitempty"`
	structureMetaErr string
}

type OptionsResult struct {
	Strategy          string          `json:"strategy"`
	Underlying        string          `json:"underlying"`
	Platform          string          `json:"platform"`
	PremiumUnit       string          `json:"premium_unit"`
	StructureContract string          `json:"structure_contract,omitempty"`
	Signal            int             `json:"signal"`
	SpotPrice         float64         `json:"spot_price"`
	Actions           []OptionsAction `json:"actions"`
	IVRank            *float64        `json:"iv_rank"`
	VolMetricsError   string          `json:"vol_metrics_error,omitempty"`
	Regime            string          `json:"regime,omitempty"`
	Timestamp         string          `json:"timestamp"`
	Error             string          `json:"error,omitempty"`
}

func formatOptionsIVRank(result *OptionsResult) string {
	if result == nil || result.VolMetricsError != "" || result.IVRank == nil {
		return "unavailable"
	}
	return fmt.Sprintf("%.1f", *result.IVRank)
}

func ExecuteOptionsSignal(s *StrategyState, result *OptionsResult, logger *StrategyLogger) (int, error) {
	if result.Signal == 0 || len(result.Actions) == 0 {
		return 0, nil
	}
	if err := optionsResultContractError(s.Platform, result); err != nil {
		return 0, fmt.Errorf("%w; refusing %d option action(s)", err, len(result.Actions))
	}
	check := checkOptionsStructures(result)
	for _, why := range check.Refusals {
		logger.Warn("Options structure refused: %s", why)
	}

	refuseEntries := false
	spec, err := lookupOptionContractSpec(s.Platform, result.Underlying)
	if err != nil {
		return 0, err
	}
	if spec.RequireCoveredCalls {
		needed := 0.0
		var firstShortCall *OptionsAction
		for i := range check.Actions {
			action := &check.Actions[i]
			if action.Action != "sell" || action.OptionType != "call" {
				continue
			}
			qty := action.Quantity
			if qty <= 0 {
				qty = 1
			}
			shares, err := optionContractValueUSD(1, spec.Multiplier, qty)
			if err != nil {
				logger.Error("Option signal cover check failed: %v", err)
				refuseEntries = true
				break
			}
			needed += shares
			if firstShortCall == nil {
				firstShortCall = action
			}
		}
		if firstShortCall != nil {
			held := heldLongUnderlyingShares(s, result.Underlying)
			reservedShares, err := reservedShortCallShares(s, result.Underlying)
			if err != nil {
				logger.Error("Option signal cover check failed: %v", err)
				refuseEntries = true
			} else if needed > held-reservedShares {
				logger.Info("Uncovered short call refused %s strike=%s qty=%g: required %g free shares, available %g (held %g - reserved by open short calls %g); refusing every entry action in this signal",
					result.Underlying, optionStrikeText(firstShortCall.Strike), max(firstShortCall.Quantity, 1), needed, held-reservedShares, held, reservedShares)
				refuseEntries = true
			}
		}
	}

	tradesExecuted := 0
	for i := range check.Closes {
		trades, err := executeOptionClose(s, result, &check.Closes[i], logger)
		if err != nil {
			logger.Error("Option close failed: %v", err)
			continue
		}
		tradesExecuted += trades
	}
	if refuseEntries {
		return tradesExecuted, nil
	}
	for _, st := range check.Structures {
		plan, why := planOptionStructure(s, result, spec, st, time.Now().UTC())
		if plan == nil {
			logger.Info("%s", why)
			continue
		}
		tradesExecuted += applyOptionStructurePlan(s, plan, logger)
	}
	return tradesExecuted, nil
}

func optionActionUnitPremiumUSD(result *OptionsResult, action *OptionsAction) float64 {
	unit := action.PremiumUSD
	if unit <= 0 {
		unit = action.Premium * result.SpotPrice
	}
	return unit
}

type optionStructurePlan struct {
	StructureID string
	Cash        float64
	Legs        []*OptionPosition
	Trades      []Trade
	Logs        []string
}

func sameOptionContract(a, b *OptionPosition) bool {
	return strings.EqualFold(a.Underlying, b.Underlying) && a.OptionType == b.OptionType && a.Action == b.Action && a.Strike == b.Strike && a.Expiry == b.Expiry
}

func finiteOptionGreeks(g OptGreeks) bool {
	return finiteAmount(g.Delta) && finiteAmount(g.Gamma) && finiteAmount(g.Theta) && finiteAmount(g.Vega)
}

func planOptionStructure(s *StrategyState, result *OptionsResult, spec optionContractSpec, st optionStructure, now time.Time) (*optionStructurePlan, string) {
	n := len(st.Legs)
	refuseLeg := func(i int, a OptionsAction, format string, args ...any) (*optionStructurePlan, string) {
		return nil, fmt.Sprintf(format, args...) + fmt.Sprintf("; structure %s leg %d/%d (%s) refused, so none of its %d leg(s) book",
			st.ID, i+1, n, optionContractKey(result.Underlying, a.OptionType, a.Action, a.Strike, a.Expiry), n)
	}
	refuseStructure := func(format string, args ...any) (*optionStructurePlan, string) {
		return nil, fmt.Sprintf(format, args...) + fmt.Sprintf("; structure %s refused, so none of its %d leg(s) book", st.ID, n)
	}
	if n == 0 {
		return refuseStructure("Option structure %s has no entry legs", st.ID)
	}

	type legPlan struct {
		a          OptionsAction
		key        string
		qty, value float64
		fee        float64
		pos        *OptionPosition
	}
	planned := make([]legPlan, 0, n)
	for i, a := range st.Legs {
		strike := optionStrikeText(a.Strike)
		if !isOptionEntryAction(a.Action) {
			return refuseLeg(i, a, "Option entry action %q is not buy or sell", a.Action)
		}
		if a.OptionType != "call" && a.OptionType != "put" {
			return refuseLeg(i, a, "Option entry option_type %q is not call or put", a.OptionType)
		}
		if _, err := time.Parse("2006-01-02", a.Expiry); err != nil {
			return refuseLeg(i, a, "Option %s %s %s strike=%s expiry %q is not a YYYY-MM-DD date", a.Action, result.Underlying, a.OptionType, strike, a.Expiry)
		}
		if !finitePositive(a.Strike) {
			return refuseLeg(i, a, "Option %s %s %s strike %v is not a finite positive number", a.Action, result.Underlying, a.OptionType, a.Strike)
		}
		if !finiteAmount(a.Quantity) {
			return refuseLeg(i, a, "Option %s %s %s strike=%s quantity %v is not finite", a.Action, result.Underlying, a.OptionType, strike, a.Quantity)
		}
		if !finiteAmount(a.Premium) || !finiteAmount(a.PremiumUSD) || !finiteAmount(a.DTE) || !finiteOptionGreeks(a.Greeks) {
			return refuseLeg(i, a, "Option %s %s %s strike=%s has a non-finite premium, DTE or greek", a.Action, result.Underlying, a.OptionType, strike)
		}
		qty := a.Quantity
		if qty <= 0 {
			qty = 1
		}
		unit := optionActionUnitPremiumUSD(result, &a)
		if !finitePositive(unit) {
			return refuseLeg(i, a, "Zero premium, skipping option %s %s %s strike=%s (per-unit premium $%v)", a.Action, result.Underlying, a.OptionType, strike, unit)
		}
		value, err := optionContractValueUSD(unit, spec.Multiplier, qty)
		if err != nil {
			return refuseLeg(i, a, "Option %s %s %s strike=%s value refused: %v", a.Action, result.Underlying, a.OptionType, strike, err)
		}
		if !finitePositive(value) {
			return refuseLeg(i, a, "Zero premium, skipping option %s %s %s strike=%s", a.Action, result.Underlying, a.OptionType, strike)
		}
		fee := CalculateOptionFee(s.Platform, value, qty)
		if !finiteAmount(fee) || fee < 0 {
			return refuseLeg(i, a, "Option %s %s %s strike=%s fee %v is not a finite non-negative amount", a.Action, result.Underlying, a.OptionType, strike, fee)
		}
		key := optionContractKey(result.Underlying, a.OptionType, a.Action, a.Strike, a.Expiry)
		pos := &OptionPosition{
			ID:                 key,
			TradePositionID:    newTradePositionID(s.ID, key, now),
			Underlying:         result.Underlying,
			OptionType:         a.OptionType,
			Strike:             a.Strike,
			Expiry:             a.Expiry,
			DTE:                a.DTE,
			Action:             a.Action,
			Quantity:           qty,
			ContractMultiplier: spec.Multiplier,
			EntryPremium:       a.Premium,
			Greeks:             a.Greeks,
			OpenedAt:           now,
		}
		for j := range planned {
			if sameOptionContract(planned[j].pos, pos) {
				return refuseLeg(i, a, "Option %s %s %s strike=%s exp=%s repeats leg %d of the same structure", a.Action, result.Underlying, a.OptionType, strike, a.Expiry, j+1)
			}
		}
		for _, existingKey := range sortedOptionLegKeys(s.OptionPositions) {
			if existing := s.OptionPositions[existingKey]; existing != nil && sameOptionContract(existing, pos) {
				return refuseLeg(i, a, "Option %s %s %s strike=%s exp=%s repeats open option leg %s", a.Action, result.Underlying, a.OptionType, strike, a.Expiry, existingKey)
			}
		}
		if _, taken := s.OptionPositions[key]; taken {
			return refuseLeg(i, a, "Option %s %s %s strike=%s exp=%s destination key %s is already held by another option row", a.Action, result.Underlying, a.OptionType, strike, a.Expiry, key)
		}
		planned = append(planned, legPlan{a: a, key: key, qty: qty, value: value, fee: fee, pos: pos})
	}

	if open := len(sortedOptionKeys(s.OptionPositions)); open+n > maxOptionPositionsPerStrategy {
		return refuseStructure("Option structure %s (%d leg(s)) would hold %d option rows with the %d open, above the %d-row cap per strategy", st.ID, n, open+n, open, maxOptionPositionsPerStrategy)
	}

	work := &StrategyState{ID: s.ID, Type: s.Type, Platform: s.Platform, Cash: s.Cash, Positions: s.Positions,
		OptionPositions: make(map[string]*OptionPosition, len(s.OptionPositions)+n)}
	for k, v := range s.OptionPositions {
		work.OptionPositions[k] = v
	}
	reserve, err := reservedShortPutCollateralUSD(work)
	if err != nil {
		return refuseStructure("Option structure %s reserve check failed: %v", st.ID, err)
	}
	buyCount, buyPremium, buyFees := 0, 0.0, 0.0
	lastBuy := -1
	for i, lp := range planned {
		if lp.a.Action == "buy" {
			buyCount++
			buyPremium += lp.value
			buyFees += lp.fee
			lastBuy = i
		}
	}
	if available := work.Cash - reserve; buyCount > 0 && buyPremium+buyFees > available {
		if buyCount == 1 {
			lp := planned[lastBuy]
			return refuseLeg(lastBuy, lp.a, "Insufficient free cash for option buy %s %s strike=%s: required $%.2f (premium $%.2f + fee $%.2f), available $%.2f (cash $%.2f - reserved put collateral $%.2f)",
				result.Underlying, lp.a.OptionType, optionStrikeText(lp.a.Strike), lp.value+lp.fee, lp.value, lp.fee, available, work.Cash, reserve)
		}
		lp := planned[lastBuy]
		return refuseLeg(lastBuy, lp.a, "Insufficient free cash for the %d buys of option structure %s: required $%.2f (premiums $%.2f + fees $%.2f), available $%.2f (cash $%.2f - reserved put collateral $%.2f) before any premium from its sells",
			buyCount, st.ID, buyPremium+buyFees, buyPremium, buyFees, available, work.Cash, reserve)
	}

	plan := &optionStructurePlan{StructureID: st.ID}
	for i, lp := range planned {
		a := lp.a
		strike := optionStrikeText(a.Strike)
		reserve, err := reservedShortPutCollateralUSD(work)
		if err != nil {
			return refuseLeg(i, a, "Option structure %s reserve check failed: %v", st.ID, err)
		}
		pos := lp.pos
		var trade Trade
		if a.Action == "buy" {
			totalCost := lp.value + lp.fee
			available := work.Cash - reserve
			if totalCost > available {
				return refuseLeg(i, a, "Insufficient free cash for option buy %s %s strike=%s: required $%.2f (premium $%.2f + fee $%.2f), available $%.2f (cash $%.2f - reserved put collateral $%.2f)",
					result.Underlying, a.OptionType, strike, totalCost, lp.value, lp.fee, available, work.Cash, reserve)
			}
			work.Cash -= totalCost
			pos.EntryPremiumUSD = lp.value
			pos.CurrentValueUSD = lp.value
			trade = Trade{Side: "buy", Price: lp.value, Value: totalCost,
				Details: fmt.Sprintf("Buy %s %s strike=%s exp=%s qty=%g premium=$%.2f fee=$%.2f", result.Underlying, a.OptionType, strike, a.Expiry, lp.qty, lp.value, lp.fee)}
			plan.Logs = append(plan.Logs, fmt.Sprintf("BUY OPTION %s %s strike=%s exp=%s qty=%g | $%.2f (fee $%.2f)", result.Underlying, a.OptionType, strike, a.Expiry, lp.qty, lp.value, lp.fee))
		} else {
			newCollateral := 0.0
			if a.OptionType == "put" {
				newCollateral, err = optionContractValueUSD(a.Strike, spec.Multiplier, lp.qty)
				if err != nil {
					return refuseLeg(i, a, "Option sell %s put strike=%s collateral refused: %v", result.Underlying, strike, err)
				}
				if reserve+newCollateral > work.Cash {
					return refuseLeg(i, a, "Insufficient collateral for short put %s strike=%s qty=%g: required $%.2f (new $%.2f + reserved $%.2f), available cash $%.2f",
						result.Underlying, strike, lp.qty, reserve+newCollateral, newCollateral, reserve, work.Cash)
				}
			}
			if a.OptionType == "call" && spec.RequireCoveredCalls {
				need, err := optionContractValueUSD(1, spec.Multiplier, lp.qty)
				if err != nil {
					return refuseLeg(i, a, "Option sell %s call strike=%s cover refused: %v", result.Underlying, strike, err)
				}
				held := heldLongUnderlyingShares(work, result.Underlying)
				reservedShares, err := reservedShortCallShares(work, result.Underlying)
				if err != nil {
					return refuseLeg(i, a, "Option sell %s call strike=%s cover check failed: %v", result.Underlying, strike, err)
				}
				if free := held - reservedShares; need > free {
					return refuseLeg(i, a, "Uncovered short call refused %s strike=%s qty=%g: required %g free shares, available %g (held %g - reserved by open short calls %g)",
						result.Underlying, strike, lp.qty, need, free, held, reservedShares)
				}
			}
			netPremium := lp.value - lp.fee
			resultingCash := work.Cash + netPremium
			resultingReserve := reserve + newCollateral
			if resultingCash < resultingReserve {
				return refuseLeg(i, a, "Option sell %s %s strike=%s would leave cash below reserved put collateral: required $%.2f, available $%.2f after premium $%.2f and fee $%.2f",
					result.Underlying, a.OptionType, strike, resultingReserve, resultingCash, lp.value, lp.fee)
			}
			work.Cash = resultingCash
			pos.EntryPremiumUSD = netPremium
			pos.CurrentValueUSD = -netPremium
			trade = Trade{Side: "sell", Price: lp.value, Value: netPremium,
				Details: fmt.Sprintf("Sell %s %s strike=%s exp=%s qty=%g premium=$%.2f fee=$%.2f", result.Underlying, a.OptionType, strike, a.Expiry, lp.qty, lp.value, lp.fee)}
			plan.Logs = append(plan.Logs, fmt.Sprintf("SELL OPTION %s %s strike=%s exp=%s qty=%g | +$%.2f (fee $%.2f)", result.Underlying, a.OptionType, strike, a.Expiry, lp.qty, lp.value, lp.fee))
		}
		if !finiteAmount(work.Cash) || !finiteAmount(pos.EntryPremiumUSD) {
			return refuseLeg(i, a, "Option %s %s %s strike=%s leaves non-finite cash or premium", a.Action, result.Underlying, a.OptionType, strike)
		}
		trade.Timestamp = now
		trade.StrategyID = s.ID
		trade.Symbol = fmt.Sprintf("%s-%s-%s-%s", result.Underlying, a.OptionType, strike, a.Expiry)
		trade.PositionID = pos.TradePositionID
		trade.Quantity = lp.qty
		trade.TradeType = "options"
		work.OptionPositions[lp.key] = pos
		plan.Legs = append(plan.Legs, pos)
		plan.Trades = append(plan.Trades, trade)
	}
	plan.Cash = work.Cash
	return plan, ""
}

func applyOptionStructurePlan(s *StrategyState, plan *optionStructurePlan, logger *StrategyLogger) int {
	prevDefer := s.deferTradePersist
	s.deferTradePersist = true
	defer func() { s.deferTradePersist = prevDefer }()
	if s.OptionPositions == nil {
		s.OptionPositions = make(map[string]*OptionPosition, len(plan.Legs))
	}
	s.Cash = plan.Cash
	for _, leg := range plan.Legs {
		s.OptionPositions[leg.ID] = leg
	}
	for _, trade := range plan.Trades {
		trade.Regime = s.Regime
		RecordTrade(s, trade)
	}
	for _, line := range plan.Logs {
		logger.Info("%s", line)
	}
	return len(plan.Legs)
}

func executeOptionClose(s *StrategyState, result *OptionsResult, action *OptionsAction, logger *StrategyLogger) (int, error) {
	closed := 0
	for _, id := range sortedOptionLegKeys(s.OptionPositions) {
		pos := s.OptionPositions[id]
		if pos.Underlying == result.Underlying && pos.Strike == action.Strike && pos.OptionType == action.OptionType {
			if optionLegAwaitingSettlement(s.Platform, pos, tradingNow().UTC()) {
				logger.Warn("Option close skipped for %s: at or past its expiry cutoff; settlement books it", id)
				continue
			}
			pnl := 0.0
			var closePriceUSD float64
			if pos.Action == "buy" {
				pnl = pos.CurrentValueUSD - pos.EntryPremiumUSD
				s.Cash += pos.CurrentValueUSD
				closePriceUSD = pos.CurrentValueUSD
			} else {
				m, err := strategyOptionMultiplier(s, id, pos)
				if err != nil {
					logger.Error("Option close skipped: %v", err)
					continue
				}
				unit := optionActionUnitPremiumUSD(result, action)
				if unit < 0 {
					unit = 0
				}
				debit, err := optionContractValueUSD(unit, m, pos.Quantity)
				if err != nil {
					logger.Error("Option close skipped for %s: %v", id, err)
					continue
				}
				pnl = pos.EntryPremiumUSD - debit
				s.Cash -= debit
				closePriceUSD = debit
			}
			now := time.Now().UTC()
			positionID := ensureOptionTradeID(s.ID, pos)
			trade := Trade{
				Timestamp:   now,
				StrategyID:  s.ID,
				Symbol:      pos.ID,
				PositionID:  positionID,
				Side:        optionCloseTradeSide(pos.Action),
				Quantity:    pos.Quantity,
				Price:       closePriceUSD,
				Value:       closePriceUSD,
				TradeType:   "options",
				Details:     fmt.Sprintf("Close %s PnL=$%.2f", pos.ID, pnl),
				IsClose:     true,
				RealizedPnL: pnl,
				PnLGross:    true,
			}
			trade.Regime = s.Regime
			RecordTrade(s, trade)
			RecordTradeResult(&s.RiskState, pnl)
			recordClosedOptionPosition(s, pos, closePriceUSD, pnl, "signal", now)
			logger.Info("CLOSE OPTION %s | PnL: $%.2f", pos.ID, pnl)
			delete(s.OptionPositions, id)
			closed++
		}
	}
	return closed, nil
}

func optionUnitEntryPremiumUSD(platform, key string, p *OptionPosition) (float64, error) {
	m, err := optionPositionMultiplier(platform, p)
	if err != nil {
		return 0, fmt.Errorf("option leg %s: %w", key, err)
	}
	if !finitePositive(p.Quantity) {
		return 0, fmt.Errorf("option leg %s: quantity %v is not positive", key, p.Quantity)
	}
	return p.EntryPremiumUSD / (p.Quantity * m), nil
}

type encodedOptionPosition struct {
	OptionType string  `json:"option_type"`
	Strike     float64 `json:"strike"`
	Expiry     string  `json:"expiry"`
	DTE        float64 `json:"dte"`
	Action     string  `json:"action"`
	Premium    float64 `json:"entry_premium_usd"`
	Delta      float64 `json:"delta"`
	Gamma      float64 `json:"gamma"`
	Theta      float64 `json:"theta"`
	Vega       float64 `json:"vega"`
}

func encodeOptionPosition(platform, key string, p *OptionPosition) (encodedOptionPosition, error) {
	unit, err := optionUnitEntryPremiumUSD(platform, key, p)
	if err != nil {
		return encodedOptionPosition{}, err
	}
	return encodedOptionPosition{
		OptionType: p.OptionType,
		Strike:     p.Strike,
		Expiry:     p.Expiry,
		DTE:        p.DTE,
		Action:     p.Action,
		Premium:    unit,
		Delta:      p.Greeks.Delta,
		Gamma:      p.Greeks.Gamma,
		Theta:      p.Greeks.Theta,
		Vega:       p.Greeks.Vega,
	}, nil
}

func EncodePositionsJSON(platform string, positions map[string]*OptionPosition) (string, error) {
	if len(positions) == 0 {
		return "[]", nil
	}
	var out []encodedOptionPosition
	for _, key := range sortedOptionLegKeys(positions) {
		e, err := encodeOptionPosition(platform, key, positions[key])
		if err != nil {
			return "", fmt.Errorf("options positions encoding failed: %w", err)
		}
		out = append(out, e)
	}
	b, err := json.Marshal(out)
	if err != nil {
		return "", fmt.Errorf("options positions encoding failed: %w", err)
	}
	return string(b), nil
}

func EncodeAllPositionsJSON(platform string, optPos map[string]*OptionPosition, spotPos map[string]*Position) (string, error) {
	type spotEntry struct {
		PositionType string  `json:"position_type"`
		Symbol       string  `json:"symbol"`
		Quantity     float64 `json:"quantity"`
		AvgCost      float64 `json:"avg_cost"`
		Side         string  `json:"side"`
	}

	var out []interface{}
	for _, key := range sortedOptionLegKeys(optPos) {
		e, err := encodeOptionPosition(platform, key, optPos[key])
		if err != nil {
			return "", fmt.Errorf("options positions encoding failed: %w", err)
		}
		out = append(out, e)
	}
	spotKeys := make([]string, 0, len(spotPos))
	for k := range spotPos {
		spotKeys = append(spotKeys, k)
	}
	sort.Strings(spotKeys)
	for _, k := range spotKeys {
		p := spotPos[k]
		out = append(out, spotEntry{
			PositionType: "spot",
			Symbol:       p.Symbol,
			Quantity:     p.Quantity,
			AvgCost:      p.AvgCost,
			Side:         p.Side,
		})
	}
	if len(out) == 0 {
		return "[]", nil
	}
	b, err := json.Marshal(out)
	if err != nil {
		return "", fmt.Errorf("options positions encoding failed: %w", err)
	}
	return string(b), nil
}

type optionsDispatch struct {
	SC                    StrategyConfig
	GlobalIntervalSeconds int
	Now                   time.Time
	State                 *StrategyState
	PositionsJSON         string
	PositionsErr          error
	Risk                  *scopeCycleRisk
	PersistenceHold       bool
	Scope                 PortfolioScope
	Regime                *RegimeConfig
	Mu                    *sync.RWMutex
	Notifier              *MultiNotifier
	Logger                *StrategyLogger
}

func runOptionsDispatch(d optionsDispatch) (int, []string) {
	sc, logger := d.SC, d.Logger
	var result *OptionsResult
	var signalStr string
	if d.PositionsErr != nil {
		logger.Error("Options check skipped: %v", d.PositionsErr)
		notifyScriptFailure(d.Notifier, sc, scriptFailureError, d.PositionsErr.Error())
	} else if r, sig, ok := runOptionsCheck(sc, d.PositionsJSON, d.Notifier, logger); ok {
		result, signalStr = r, sig
		filterOptionsEntryActions(d, result)
	}
	optionsRegime := globalRegimeStore.PayloadForStrategy(sc, d.Regime)
	d.Mu.Lock()
	defer d.Mu.Unlock()
	d.State.Regime = optionsRegime.PrimaryLabel(nil)
	now := d.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	return executeOptionsCycleLocked(sc, d.State, result, signalStr, newOptionHarvestClock(sc, d.GlobalIntervalSeconds, now), logger)
}

func filterOptionsEntryActions(d optionsDispatch, result *OptionsResult) {
	sc, logger, sr := d.SC, d.Logger, d.Risk
	if sc.Paused {
		kept, dropped := pausedOptionsActions(result.Actions)
		if dropped > 0 {
			logger.Info("Paused: %d option open action(s) dropped — close actions still execute (#1150)", dropped)
		}
		result.Actions = kept
	}
	if sr.DailyLossEntriesHeld {
		kept, dropped := pausedOptionsActions(result.Actions)
		if dropped > 0 {
			logger.Info("Daily loss limit: %d option open action(s) dropped — entries held until UTC rollover (#1269)", dropped)
		}
		result.Actions = kept
	}
	if sr.NotionalBlocked {
		kept, dropped := pausedOptionsActions(result.Actions)
		if dropped > 0 {
			logger.Warn("Notional cap: %d option open action(s) dropped — new opens blocked, exits continue (#1344)", dropped)
		}
		result.Actions = kept
	}
	if d.PersistenceHold {
		kept, dropped := pausedOptionsActions(result.Actions)
		if dropped > 0 {
			logger.Warn("Persistence hold: %d option open action(s) dropped — the %s scope has an unacknowledged state save failure, so no new exposure is taken; closes continue (#1523)", dropped, scopeLabel(d.Scope))
		}
		result.Actions = kept
	}
	if kept, dropped, structures, capWhy := exposureCapOptionsActions(sr.ExposureCapStatus, extractAsset(sc), result.Actions); dropped > 0 {
		logger.Warn("Exposure cap: %d option open action(s) in %d structure(s) dropped whole — %s (#1270)", dropped, structures, capWhy)
		result.Actions = kept
	}
}

func executeOptionsCycleLocked(sc StrategyConfig, s *StrategyState, result *OptionsResult, signalStr string, clock optionHarvestClock, logger *StrategyLogger) (int, []string) {
	trades := 0
	var lines []string
	if result != nil {
		entryTrades, err := ExecuteOptionsSignal(s, result, logger)
		if err != nil {
			logger.Error("Options execution failed: %v", err)
		} else if entryTrades > 0 {
			trades += entryTrades
			lines = append(lines, fmt.Sprintf("[%s] %s %s spot=$%s IV=%s", sc.ID, signalStr, result.Underlying, formatSignalPrice(result.SpotPrice), formatOptionsIVRank(result)))
		}
	}
	if sc.ThetaHarvest != nil {
		harvestTrades, harvestDetails := CheckThetaHarvest(s, sc.ThetaHarvest, clock, logger)
		trades += harvestTrades
		lines = append(lines, harvestDetails...)
	}
	return trades, lines
}

func CheckThetaHarvest(s *StrategyState, cfg *ThetaHarvestConfig, clock optionHarvestClock, logger *StrategyLogger) (int, []string) {
	if cfg == nil || !cfg.Enabled {
		return 0, nil
	}

	trades := 0
	var details []string

	type closeAction struct {
		id     string
		reason string
	}
	var toClose []closeAction

	for _, id := range sortedOptionLegKeys(s.OptionPositions) {
		pos := s.OptionPositions[id]
		if pos == nil || pos.Action != "sell" {
			continue
		}
		dte, why := optionHarvestEligibility(pos, clock)
		if why != "" {
			logger.InfoOnChange("harvest-eligibility:"+id, why, "Theta harvest holds %s: %s", id, why)
			continue
		}
		logger.Changed("harvest-eligibility:"+id, "")

		entryPremium := pos.EntryPremiumUSD
		if entryPremium <= 0 {
			continue
		}

		currentCost := -pos.CurrentValueUSD
		if currentCost < 0 {
			currentCost = 0
		}

		profitUSD := entryPremium - currentCost
		profitPct := (profitUSD / entryPremium) * 100

		if cfg.ProfitTargetPct > 0 && profitPct >= cfg.ProfitTargetPct {
			toClose = append(toClose, closeAction{
				id:     id,
				reason: fmt.Sprintf("🎯 Theta harvest: %.0f%% profit captured ($%.2f of $%.2f premium)", profitPct, profitUSD, entryPremium),
			})
			continue
		}

		if cfg.StopLossPct > 0 && profitPct < 0 {
			lossPct := -profitPct
			if lossPct >= cfg.StopLossPct {
				toClose = append(toClose, closeAction{
					id:     id,
					reason: fmt.Sprintf("🛑 Stop loss: %.0f%% loss on sold option ($%.2f)", lossPct, -profitUSD),
				})
				continue
			}
		}

		if cfg.MinDTEClose > 0 && dte > 0 && dte <= cfg.MinDTEClose {
			toClose = append(toClose, closeAction{
				id:     id,
				reason: fmt.Sprintf("⏰ DTE exit: %.1f days to expiry (min: %.0f)", dte, cfg.MinDTEClose),
			})
			continue
		}
	}

	for _, c := range toClose {
		pos := s.OptionPositions[c.id]
		if pos == nil {
			continue
		}

		buybackCost := -pos.CurrentValueUSD
		if buybackCost < 0 {
			buybackCost = 0
		}
		pnl := pos.EntryPremiumUSD - buybackCost

		s.Cash -= buybackCost

		now := time.Now().UTC()
		positionID := ensureOptionTradeID(s.ID, pos)
		trade := Trade{
			Timestamp:   now,
			StrategyID:  s.ID,
			Symbol:      pos.ID,
			PositionID:  positionID,
			Side:        optionCloseTradeSide(pos.Action),
			Quantity:    pos.Quantity,
			Price:       buybackCost,
			Value:       buybackCost,
			TradeType:   "options",
			Details:     fmt.Sprintf("Theta harvest close %s PnL=$%.2f", pos.ID, pnl),
			IsClose:     true,
			RealizedPnL: pnl,
			PnLGross:    true,
		}
		trade.Regime = s.Regime
		RecordTrade(s, trade)
		RecordTradeResult(&s.RiskState, pnl)
		recordClosedOptionPosition(s, pos, buybackCost, pnl, "theta_harvest", now)

		logger.Info("%s | %s | PnL: $%.2f", c.reason, pos.ID, pnl)
		detail := fmt.Sprintf("[%s] CLOSE %s — %s (PnL: $%.2f)", s.ID, pos.ID, c.reason, pnl)
		details = append(details, detail)

		delete(s.OptionPositions, c.id)
		trades++
	}

	return trades, details
}
