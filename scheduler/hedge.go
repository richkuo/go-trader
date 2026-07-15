package main

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	hedgeActionNone           = "none"
	hedgeActionOpen           = "open"
	hedgeActionReduce         = "reduce"
	hedgeActionClose          = "close"
	hedgeActionMismatch       = "mismatch"
	hedgeActionPrimaryFailure = "primary_failure"
	hedgeActionHold           = "hold"
	hedgeQuantityEpsilon      = 1e-9
)

// hedgePositionSnapshot is the small, lock-free input used by the hedge
// decision core. Keeping the core independent of StrategyState makes the
// lifecycle policy testable and prevents accidental state mutation while the
// scheduler is in its no-lock execution phase.
type hedgePositionSnapshot struct {
	Quantity             float64
	Side                 string
	HedgePrimaryQtyBasis float64
}

type hedgeDecision struct {
	Action                  string
	Side                    string
	Quantity                float64
	PrimaryQtyBasis         float64
	PreviousPrimaryQtyBasis float64
	Reason                  string
}

func normalizeHedgeCoin(symbol string) string {
	s := strings.ToUpper(strings.TrimSpace(symbol))
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[:i]
	}
	if i := strings.IndexByte(s, ':'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

func cloneHedgeConfig(h *HedgeConfig) *HedgeConfig {
	if h == nil {
		return nil
	}
	cp := *h
	return &cp
}

func normalizeHedgeConfig(h *HedgeConfig) {
	if h == nil || !h.Enabled {
		return
	}
	if h.Side == "" {
		h.Side = "inverse"
	}
	if h.Ratio == 0 {
		h.Ratio = 1
	}
	if h.Platform == "" {
		h.Platform = "hyperliquid"
	}
	if h.Type == "" {
		h.Type = "perps"
	}
	if h.MarginMode == "" {
		h.MarginMode = "isolated"
	}
	if h.Leverage == 0 {
		h.Leverage = 1
	}
}

func hedgeEnabled(sc StrategyConfig) bool {
	return sc.Hedge != nil && sc.Hedge.Enabled
}

func hedgeCoin(sc StrategyConfig) string {
	if sc.Hedge == nil || !sc.Hedge.Enabled {
		return ""
	}
	return normalizeHedgeCoin(sc.Hedge.Symbol)
}

func hedgeRatio(sc StrategyConfig) float64 {
	if sc.Hedge == nil || sc.Hedge.Ratio <= 0 {
		return 0
	}
	return sc.Hedge.Ratio
}

func hedgeDesiredSide(primarySide string) string {
	switch strings.ToLower(strings.TrimSpace(primarySide)) {
	case "long":
		return "short"
	case "short":
		return "long"
	default:
		return ""
	}
}

func hedgeTargetQuantity(primaryQty, primaryPrice, hedgePrice, ratio float64) (float64, error) {
	if primaryQty <= hedgeQuantityEpsilon {
		return 0, nil
	}
	if !usableHedgeMark(primaryPrice) || !usableHedgeMark(hedgePrice) {
		return 0, fmt.Errorf("unusable hedge price (primary=%.12g hedge=%.12g)", primaryPrice, hedgePrice)
	}
	if ratio <= hedgeQuantityEpsilon {
		return 0, fmt.Errorf("hedge ratio must be positive, got %.12g", ratio)
	}
	return primaryQty * primaryPrice * ratio / hedgePrice, nil
}

func usableHedgeMark(price float64) bool {
	return price > hedgeQuantityEpsilon && !math.IsNaN(price) && !math.IsInf(price, 0)
}

func decideHedge(primary, hedge hedgePositionSnapshot, primaryPrice, hedgePrice, ratio float64, freshPrimaryOpen bool) hedgeDecision {
	if primary.Quantity <= hedgeQuantityEpsilon {
		if hedge.Quantity > hedgeQuantityEpsilon {
			return hedgeDecision{Action: hedgeActionClose, Side: hedge.Side, Quantity: hedge.Quantity, Reason: "primary is flat"}
		}
		return hedgeDecision{Action: hedgeActionNone}
	}

	desiredSide := hedgeDesiredSide(primary.Side)
	if desiredSide == "" {
		return hedgeDecision{Action: hedgeActionPrimaryFailure, Reason: "primary side is invalid"}
	}
	if hedge.Quantity > hedgeQuantityEpsilon && hedge.Side != desiredSide {
		return hedgeDecision{Action: hedgeActionMismatch, Side: hedge.Side, Quantity: hedge.Quantity, Reason: "hedge side does not oppose primary"}
	}
	if !usableHedgeMark(primaryPrice) || !usableHedgeMark(hedgePrice) {
		return hedgeDecision{Action: hedgeActionHold, Reason: fmt.Sprintf("missing usable mark (primary=%.12g hedge=%.12g)", primaryPrice, hedgePrice)}
	}

	targetQty, err := hedgeTargetQuantity(primary.Quantity, primaryPrice, hedgePrice, ratio)
	if err != nil {
		return hedgeDecision{Action: hedgeActionPrimaryFailure, Reason: err.Error()}
	}
	if hedge.Quantity <= hedgeQuantityEpsilon {
		if !freshPrimaryOpen {
			return hedgeDecision{Action: hedgeActionPrimaryFailure, Reason: "primary is open but hedge is absent"}
		}
		return hedgeDecision{Action: hedgeActionOpen, Side: desiredSide, Quantity: targetQty, PrimaryQtyBasis: primary.Quantity, Reason: "fresh primary open"}
	}

	basis := hedge.HedgePrimaryQtyBasis
	if basis < 0 {
		basis = 0
	}
	if basis <= hedgeQuantityEpsilon {
		// A legacy or manually seeded hedge can predate the watermark. Infer the
		// covered primary quantity once, but never treat it as permission to
		// churn on a mark-price-only change.
		if primaryPrice > hedgeQuantityEpsilon && ratio > hedgeQuantityEpsilon {
			basis = hedge.Quantity * hedgePrice / (primaryPrice * ratio)
			if basis > primary.Quantity {
				basis = primary.Quantity
			}
		}
	}
	if primary.Quantity > basis+hedgeQuantityEpsilon {
		delta := targetQty - hedge.Quantity
		if delta <= hedgeQuantityEpsilon {
			return hedgeDecision{Action: hedgeActionNone}
		}
		return hedgeDecision{Action: hedgeActionOpen, Side: desiredSide, Quantity: delta, PrimaryQtyBasis: primary.Quantity, Reason: "primary quantity increased"}
	}
	if primary.Quantity < basis-hedgeQuantityEpsilon {
		closeQty := hedge.Quantity * (basis - primary.Quantity) / basis
		if closeQty > hedge.Quantity {
			closeQty = hedge.Quantity
		}
		if closeQty <= hedgeQuantityEpsilon {
			return hedgeDecision{Action: hedgeActionNone}
		}
		return hedgeDecision{Action: hedgeActionReduce, Side: hedge.Side, Quantity: closeQty, PrimaryQtyBasis: primary.Quantity, PreviousPrimaryQtyBasis: basis, Reason: "primary quantity decreased"}
	}
	return hedgeDecision{Action: hedgeActionNone}
}

// validateHedgeConfigs enforces the account-level uniqueness rules that make
// a reduce-only hedge safe: a hedge coin must not overlap any configured HL
// coin and only one strategy may own a given hedge coin.
func validateHedgeConfigs(cfg *Config) []string {
	if cfg == nil {
		return nil
	}
	primaryOwners := make(map[string][]string)
	for _, sc := range cfg.Strategies {
		if sc.Platform != "hyperliquid" || (sc.Type != "perps" && sc.Type != "manual") {
			continue
		}
		if coin := hyperliquidConfiguredCoin(sc); coin != "" {
			primaryOwners[coin] = append(primaryOwners[coin], sc.ID)
		}
	}

	var errs []string
	hedgeOwners := make(map[string][]string)
	for _, sc := range cfg.Strategies {
		if sc.Hedge == nil {
			continue
		}
		prefix := fmt.Sprintf("strategy[%s].hedge", sc.ID)
		h := sc.Hedge
		if !h.Enabled {
			continue
		}
		if sc.Platform != "hyperliquid" || sc.Type != "perps" {
			errs = append(errs, fmt.Sprintf("%s is only supported on hyperliquid perps strategies", prefix))
		}
		if h.Platform != "hyperliquid" || h.Type != "perps" {
			errs = append(errs, fmt.Sprintf("%s platform/type must be hyperliquid/perps, got %q/%q", prefix, h.Platform, h.Type))
		}
		if h.Side != "inverse" {
			errs = append(errs, fmt.Sprintf("%s.side must be \"inverse\", got %q", prefix, h.Side))
		}
		if h.Ratio <= 0 {
			errs = append(errs, fmt.Sprintf("%s.ratio must be > 0, got %g", prefix, h.Ratio))
		}
		if h.MarginMode != "isolated" && h.MarginMode != "cross" {
			errs = append(errs, fmt.Sprintf("%s.margin_mode must be \"isolated\" or \"cross\", got %q", prefix, h.MarginMode))
		}
		if h.Leverage <= 0 {
			errs = append(errs, fmt.Sprintf("%s.leverage must be > 0, got %g", prefix, h.Leverage))
		}
		coin := normalizeHedgeCoin(h.Symbol)
		if coin == "" {
			errs = append(errs, fmt.Sprintf("%s.symbol is required", prefix))
			continue
		}
		primary := hyperliquidConfiguredCoin(sc)
		if coin == primary {
			errs = append(errs, fmt.Sprintf("%s.symbol %q equals the primary coin", prefix, coin))
		}
		if owners := primaryOwners[coin]; len(owners) > 0 {
			errs = append(errs, fmt.Sprintf("%s.symbol %q collides with configured HL coin(s) %s", prefix, coin, strings.Join(sortedStrings(owners), ", ")))
		}
		hedgeOwners[coin] = append(hedgeOwners[coin], sc.ID)
	}
	coins := make([]string, 0, len(hedgeOwners))
	for coin := range hedgeOwners {
		coins = append(coins, coin)
	}
	sort.Strings(coins)
	for _, coin := range coins {
		owners := hedgeOwners[coin]
		if len(owners) > 1 {
			errs = append(errs, fmt.Sprintf("hedge coin %q is configured by multiple strategies: %s", coin, strings.Join(sortedStrings(owners), ", ")))
		}
	}
	sort.Strings(errs)
	return errs
}

func sortedStrings(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func validateHedgeStateConsistency(state *AppState, cfg *Config) []string {
	if state == nil || cfg == nil {
		return nil
	}
	byID := make(map[string]StrategyConfig, len(cfg.Strategies))
	for _, sc := range cfg.Strategies {
		byID[sc.ID] = sc
	}
	var warnings []string
	for _, ss := range state.Strategies {
		if ss == nil {
			continue
		}
		for _, pos := range ss.Positions {
			if pos == nil || pos.HedgeFor == "" {
				continue
			}
			sc, ok := byID[ss.ID]
			if !ok || !hedgeEnabled(sc) || normalizeHedgeCoin(pos.Symbol) != hedgeCoin(sc) || normalizeHedgeCoin(pos.HedgeFor) != hyperliquidConfiguredCoin(sc) {
				warnings = append(warnings, fmt.Sprintf("[state] %s/%s is an unmanaged hedge leg (hedge_for=%q)", ss.ID, pos.Symbol, pos.HedgeFor))
			}
		}
	}
	sort.Strings(warnings)
	return warnings
}

func hedgePositionFor(ss *StrategyState, coin string) (string, *Position) {
	if ss == nil || coin == "" {
		return "", nil
	}
	if pos := ss.Positions[coin]; pos != nil {
		return coin, pos
	}
	for symbol, pos := range ss.Positions {
		if pos != nil && normalizeHedgeCoin(symbol) == coin {
			return symbol, pos
		}
	}
	return "", nil
}

func hedgeSnapshotFromPosition(pos *Position) hedgePositionSnapshot {
	if pos == nil {
		return hedgePositionSnapshot{}
	}
	return hedgePositionSnapshot{
		Quantity:             pos.Quantity,
		Side:                 pos.Side,
		HedgePrimaryQtyBasis: pos.HedgePrimaryQtyBasis,
	}
}

func hedgePrice(prices map[string]float64, coin string) float64 {
	if prices == nil || coin == "" {
		return 0
	}
	if p := prices[coin]; p > 0 {
		return p
	}
	for symbol, p := range prices {
		if p > 0 && normalizeHedgeCoin(symbol) == normalizeHedgeCoin(coin) {
			return p
		}
	}
	return 0
}

func hedgeOnChainQty(positions []HLPosition, coin string) float64 {
	for _, p := range positions {
		if normalizeHedgeCoin(p.Coin) == normalizeHedgeCoin(coin) {
			return mathAbs(p.Size)
		}
	}
	return 0
}

func mathAbs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

// syncHedgeForStrategy is the single lifecycle coordinator for #1159. It is
// called after a primary dispatch and on the pre-dispatch coherence sweep. It
// snapshots under RLock, submits any side effect without the lock, then applies
// only a confirmed fill under Lock. Hedge positions never run a check script,
// close evaluator, or protection sync.
func syncHedgeForStrategy(sc StrategyConfig, state *AppState, mu *sync.RWMutex, prices map[string]float64, hlPositions []HLPosition, primaryFreshOpen bool, notifier *MultiNotifier, logger *StrategyLogger) {
	if !hedgeEnabled(sc) || sc.Platform != "hyperliquid" || sc.Type != "perps" || state == nil || mu == nil {
		return
	}
	primaryCoin := hyperliquidConfiguredCoin(sc)
	hCoin := hedgeCoin(sc)
	if primaryCoin == "" || hCoin == "" {
		return
	}

	mu.RLock()
	ss := state.Strategies[sc.ID]
	primarySymbol, primaryPos := hedgePositionFor(ss, primaryCoin)
	hedgeSymbol, hedgePos := hedgePositionFor(ss, hCoin)
	primary := hedgeSnapshotFromPosition(primaryPos)
	hedge := hedgeSnapshotFromPosition(hedgePos)
	mu.RUnlock()
	if primarySymbol == "" {
		primarySymbol = primaryCoin
	}
	if hedgeSymbol == "" {
		hedgeSymbol = hCoin
	}

	primaryPx := hedgePrice(prices, primaryCoin)
	hedgePx := hedgePrice(prices, hCoin)
	decision := decideHedge(primary, hedge, primaryPx, hedgePx, hedgeRatio(sc), primaryFreshOpen)
	if decision.Action == hedgeActionNone {
		return
	}
	if decision.Action == hedgeActionHold {
		holdAlert := fmt.Sprintf("**HEDGE HOLD** [%s] %s; retrying without changing the primary or hedge.", sc.ID, decision.Reason)
		hedgeOwnerAlert(notifier, holdAlert)
		if logger != nil {
			logger.Warn("hedge hold: %s", decision.Reason)
		}
		return
	}
	if decision.Action == hedgeActionPrimaryFailure || decision.Action == hedgeActionMismatch {
		if decision.Action == hedgeActionMismatch && hedgePos != nil {
			if !closeHedgeLeg(sc, state, mu, hedgeSymbol, hedgePos, hlPositions, notifier, logger) {
				hedgeOwnerAlert(notifier, fmt.Sprintf("**HEDGE FAIL-CLOSED** [%s] mismatched hedge %s could not be closed; reducing primary %s to flat.", sc.ID, hedgeSymbol, primarySymbol))
				if primaryPos != nil {
					failClosedPrimary(sc, state, mu, primarySymbol, primaryPos, primaryPx, hlPositions, notifier, logger, decision.Reason)
				}
				return
			}
			// A confirmed side flip can be repaired in place: the old inverse
			// leg is flat, so submit the new inverse leg against the same primary.
			targetQty, err := hedgeTargetQuantity(primary.Quantity, primaryPx, hedgePx, hedgeRatio(sc))
			if err != nil {
				hedgeOwnerAlert(notifier, fmt.Sprintf("**HEDGE FAIL-CLOSED** [%s] %s; reducing primary %s to flat.", sc.ID, err, primarySymbol))
				failClosedPrimary(sc, state, mu, primarySymbol, primaryPos, primaryPx, hlPositions, notifier, logger, err.Error())
				return
			}
			freshPositions := make([]HLPosition, len(hlPositions))
			copy(freshPositions, hlPositions)
			for i := range freshPositions {
				if normalizeHedgeCoin(freshPositions[i].Coin) == hCoin {
					freshPositions[i].Size = 0
				}
			}
			openHedgeLeg(sc, state, mu, primaryCoin, hCoin, hedgeSymbol, hedgePositionSnapshot{}, hedgeDecision{
				Action: hedgeActionOpen, Side: hedgeDesiredSide(primary.Side), Quantity: targetQty,
				PrimaryQtyBasis: primary.Quantity, Reason: "primary side flipped",
			}, primaryPx, hedgePx, freshPositions, notifier, logger)
			return
		}
		if primaryPos != nil {
			hedgeOwnerAlert(notifier, fmt.Sprintf("**HEDGE FAIL-CLOSED** [%s] %s; reducing primary %s to flat.", sc.ID, decision.Reason, primarySymbol))
			failClosedPrimary(sc, state, mu, primarySymbol, primaryPos, primaryPx, hlPositions, notifier, logger, decision.Reason)
		}
		return
	}

	if decision.Quantity <= hedgeQuantityEpsilon {
		return
	}
	if decision.Action == hedgeActionOpen {
		openHedgeLeg(sc, state, mu, primaryCoin, hCoin, hedgeSymbol, hedge, decision, primaryPx, hedgePx, hlPositions, notifier, logger)
		return
	}
	closeHedgeLegSized(sc, state, mu, hedgeSymbol, hedgePos, decision.Quantity, decision.Action == hedgeActionClose, decision.PreviousPrimaryQtyBasis, hlPositions, notifier, logger)
}

func hedgeOwnerAlert(notifier *MultiNotifier, msg string) {
	if notifier != nil {
		notifier.SendOwnerDM(msg)
	}
}

func openHedgeLeg(sc StrategyConfig, state *AppState, mu *sync.RWMutex, primaryCoin, hedgeCoinName, hedgeSymbol string, hedge hedgePositionSnapshot, decision hedgeDecision, primaryPx, hedgePx float64, hlPositions []HLPosition, notifier *MultiNotifier, logger *StrategyLogger) {
	if !usableHedgeMark(primaryPx) || !usableHedgeMark(hedgePx) {
		hedgeOwnerAlert(notifier, fmt.Sprintf("**HEDGE BLOCKED** [%s] missing usable mark for primary %s or hedge %s; no hedge order submitted.", sc.ID, primaryCoin, hedgeCoinName))
		return
	}
	qty := decision.Quantity
	fillPx := hedgePx
	fillFee := CalculatePlatformSpotFee("hyperliquid", qty*fillPx)
	fillOID := ""
	confirmedQty := qty
	isLive := hyperliquidIsLive(sc.Args)
	if isLive {
		mode := "isolated"
		leverage := 1.0
		if sc.Hedge != nil {
			mode = sc.Hedge.MarginMode
			leverage = sc.Hedge.Leverage
		}
		prevQty := hedgeOnChainQty(hlPositions, hedgeCoinName)
		result, stderr, err := RunHyperliquidExecute(sc.Script, sc.Hedge.Symbol, decision.Side, qty, 0, 0, prevQty, mode, leverage, false, hlExecuteSnapshotForCoin(hlPositions, hedgeCoinName))
		if logger != nil && stderr != "" {
			logger.Info("hedge execute stderr: %s", stderr)
		}
		if err != nil || result == nil || result.Error != "" || result.Execution == nil || result.Execution.Fill == nil || result.Execution.Fill.TotalSz <= hedgeQuantityEpsilon || result.Execution.Fill.AvgPx <= 0 {
			msg := fmt.Sprintf("hedge %s %s %.6f failed: err=%v", sc.ID, hedgeCoinName, qty, err)
			if result != nil && result.Error != "" {
				msg += "; " + result.Error
			}
			hedgeOwnerAlert(notifier, "**HEDGE EXECUTION FAILURE** ["+sc.ID+"] "+msg+". Primary is being reduced to flat.")
			failClosedPrimary(sc, state, mu, primaryCoin, nil, primaryPx, hlPositions, notifier, logger, "hedge open was not fill-confirmed")
			return
		}
		fill := result.Execution.Fill
		confirmedQty = fill.TotalSz
		fillPx = fill.AvgPx
		fillFee = fill.Fee
		if fill.OID != 0 {
			fillOID = fmt.Sprintf("%d", fill.OID)
		}
	}

	mu.Lock()
	ss := state.Strategies[sc.ID]
	if ss == nil {
		mu.Unlock()
		return
	}
	_, primaryPos := hedgePositionFor(ss, primaryCoin)
	_, existing := hedgePositionFor(ss, hedgeCoinName)
	if primaryPos == nil || primaryPos.Quantity <= hedgeQuantityEpsilon {
		mu.Unlock()
		return
	}
	coveredBasis := existingBasis(existing)
	if decision.Action == hedgeActionOpen {
		remainingPrimary := primaryPos.Quantity - coveredBasis
		if remainingPrimary < 0 {
			remainingPrimary = 0
		}
		coveredBasis += remainingPrimary * minRatio(confirmedQty, qty)
		if coveredBasis > primaryPos.Quantity {
			coveredBasis = primaryPos.Quantity
		}
	}
	applyHedgeOpen(ss, sc, primaryCoin, hedgeCoinName, existing, decision.Side, confirmedQty, fillPx, fillFee, isLive, fillOID, coveredBasis)
	mu.Unlock()
}

func minRatio(actual, requested float64) float64 {
	if requested <= hedgeQuantityEpsilon {
		return 0
	}
	r := actual / requested
	if r < 0 {
		return 0
	}
	if r > 1 {
		return 1
	}
	return r
}

func existingBasis(pos *Position) float64 {
	if pos == nil || pos.HedgePrimaryQtyBasis <= 0 {
		return 0
	}
	return pos.HedgePrimaryQtyBasis
}

func applyHedgeOpen(ss *StrategyState, sc StrategyConfig, primaryCoin, hedgeCoinName string, existing *Position, side string, qty, price, fillFee float64, live bool, fillOID string, primaryBasis float64) {
	if ss == nil || qty <= hedgeQuantityEpsilon || price <= 0 {
		return
	}
	fee := CalculatePlatformSpotFee("hyperliquid", qty*price)
	feeSource := FeeSourceModeled
	if live {
		fee = fillFee
		feeSource = FeeSourceUserFills
	}
	ss.Cash -= fee
	now := time.Now().UTC()
	positionID := ""
	if existing != nil {
		positionID = ensurePositionTradeID(ss.ID, hedgeCoinName, existing)
		total := existing.Quantity + qty
		if total > hedgeQuantityEpsilon {
			existing.AvgCost = (existing.AvgCost*existing.Quantity + price*qty) / total
		}
		existing.Quantity = total
		if existing.InitialQuantity <= 0 {
			existing.InitialQuantity = total
		}
		existing.HedgePrimaryQtyBasis = primaryBasis
	} else {
		positionID = newTradePositionID(ss.ID, hedgeCoinName, now)
		ss.Positions[hedgeCoinName] = &Position{
			Symbol: hedgeCoinName, TradePositionID: positionID, Quantity: qty, InitialQuantity: qty,
			AvgCost: price, Side: side, Multiplier: 1, Leverage: sc.Hedge.Leverage,
			OwnerStrategyID: ss.ID, HedgeFor: primaryCoin, HedgePrimaryQtyBasis: primaryBasis, OpenedAt: now,
		}
	}
	trade := Trade{
		Timestamp: now, StrategyID: ss.ID, Symbol: hedgeCoinName, PositionID: positionID,
		Side: hedgeOpenTradeSide(side), Quantity: qty, Price: price, Value: qty * price,
		TradeType: TradeTypeHedge, Details: fmt.Sprintf("Hedge %s %.6f %s @ $%.4f (ratio-following, fee $%.2f)", side, qty, hedgeCoinName, price, fee),
		ExchangeOrderID: fillOID, ExchangeFee: fee, FeeSource: feeSource, PnLGross: true,
	}
	RecordTrade(ss, trade)
}

func hedgeOpenTradeSide(positionSide string) string {
	if positionSide == "short" {
		return "sell"
	}
	return "buy"
}

func closeHedgeLeg(sc StrategyConfig, state *AppState, mu *sync.RWMutex, symbol string, pos *Position, hlPositions []HLPosition, notifier *MultiNotifier, logger *StrategyLogger) bool {
	if pos == nil {
		return true
	}
	return closeHedgeLegSized(sc, state, mu, symbol, pos, pos.Quantity, true, 0, hlPositions, notifier, logger)
}

func closeHedgeLegSized(sc StrategyConfig, state *AppState, mu *sync.RWMutex, symbol string, pos *Position, qty float64, full bool, previousBasis float64, hlPositions []HLPosition, notifier *MultiNotifier, logger *StrategyLogger) bool {
	if pos == nil || qty <= hedgeQuantityEpsilon {
		return true
	}
	if qty > pos.Quantity {
		qty = pos.Quantity
	}
	requestedQty := qty
	if previousBasis <= hedgeQuantityEpsilon {
		previousBasis = pos.HedgePrimaryQtyBasis
	}
	isLive := hyperliquidIsLive(sc.Args)
	fillPx := pos.AvgCost
	fillFee := 0.0
	fillOID := ""
	if isLive {
		partial := qty
		result, stderr, err := RunHyperliquidClose(sc.Script, symbol, &partial, nil)
		if logger != nil && stderr != "" {
			logger.Info("hedge close stderr: %s", stderr)
		}
		if err != nil || result == nil || result.Error != "" || result.Close == nil || result.Close.Fill == nil || result.Close.Fill.TotalSz <= hedgeQuantityEpsilon || result.Close.Fill.AvgPx <= 0 {
			hedgeOwnerAlert(notifier, fmt.Sprintf("**HEDGE CLOSE FAILURE** [%s] %s %.6f was not fill-confirmed; virtual hedge remains protected for retry.", sc.ID, symbol, qty))
			return false
		}
		fill := result.Close.Fill
		qty = fill.TotalSz
		fillPx = fill.AvgPx
		fillFee = fill.Fee
		if fill.OID != 0 {
			fillOID = fmt.Sprintf("%d", fill.OID)
		}
	}
	mu.Lock()
	ss := state.Strategies[sc.ID]
	if ss == nil {
		mu.Unlock()
		return false
	}
	current := ss.Positions[symbol]
	if current == nil {
		mu.Unlock()
		return true
	}
	bookedFull := qty >= current.Quantity-hedgeQuantityEpsilon
	if full && !bookedFull && logger != nil {
		logger.Warn("hedge full close underfilled: requested %.6f, confirmed %.6f; preserving the residual hedge for retry", requestedQty, qty)
	}
	if bookedFull {
		bookPerpsCloseWithFillFee(ss, symbol, fillPx, fillFee, isLive, fillOID, "hedge_sync", "Hedge close", "Hedge close", logger)
	} else {
		bookPerpsPartialCloseWithFillFee(ss, symbol, qty, fillPx, fillFee, isLive, fillOID, "hedge_sync", "Hedge reduce", "Hedge reduce", logger)
	}
	if remaining := ss.Positions[symbol]; remaining != nil {
		primaryCoin := current.HedgeFor
		if primaryCoin != "" {
			primaryQty := 0.0
			if _, primary := hedgePositionFor(ss, normalizeHedgeCoin(primaryCoin)); primary != nil {
				primaryQty = primary.Quantity
			}
			remaining.HedgePrimaryQtyBasis = primaryQty
			if !bookedFull {
				remaining.HedgePrimaryQtyBasis = hedgeBasisAfterReduction(previousBasis, primaryQty, requestedQty, qty)
			}
		}
	}
	mu.Unlock()
	return true
}

func hedgeBasisAfterReduction(previousBasis, currentPrimaryQty, requestedHedgeQty, filledHedgeQty float64) float64 {
	if filledHedgeQty <= hedgeQuantityEpsilon {
		return previousBasis
	}
	if currentPrimaryQty <= hedgeQuantityEpsilon {
		return 0
	}
	if previousBasis <= hedgeQuantityEpsilon || requestedHedgeQty <= hedgeQuantityEpsilon {
		return currentPrimaryQty
	}
	primaryReduction := previousBasis - currentPrimaryQty
	if primaryReduction <= hedgeQuantityEpsilon {
		return currentPrimaryQty
	}
	fillFraction := filledHedgeQty / requestedHedgeQty
	if fillFraction < 0 {
		fillFraction = 0
	}
	if fillFraction > 1 {
		fillFraction = 1
	}
	basis := previousBasis - primaryReduction*fillFraction
	if basis < currentPrimaryQty {
		basis = currentPrimaryQty
	}
	return basis
}

func failClosedPrimary(sc StrategyConfig, state *AppState, mu *sync.RWMutex, symbol string, pos *Position, mark float64, hlPositions []HLPosition, notifier *MultiNotifier, logger *StrategyLogger, reason string) {
	if pos == nil {
		mu.RLock()
		ss := state.Strategies[sc.ID]
		_, pos = hedgePositionFor(ss, normalizeHedgeCoin(symbol))
		mu.RUnlock()
	}
	if pos == nil {
		return
	}
	if mark <= 0 {
		mark = pos.AvgCost
	}
	if hyperliquidIsLive(sc.Args) {
		partial := pos.Quantity
		result, _, err := RunHyperliquidClose(sc.Script, symbol, &partial, appendPositionProtectionOIDs(pos))
		if err != nil || result == nil || result.Error != "" || result.Close == nil || result.Close.Fill == nil || result.Close.Fill.TotalSz <= hedgeQuantityEpsilon || result.Close.Fill.AvgPx <= 0 {
			hedgeOwnerAlert(notifier, fmt.Sprintf("**HEDGE FAIL-CLOSED** [%s] primary %s could not be reduced after %s. Manual intervention required.", sc.ID, symbol, reason))
			return
		}
		mu.Lock()
		if ss := state.Strategies[sc.ID]; ss != nil {
			fill := result.Close.Fill
			fillOID := ""
			if fill.OID != 0 {
				fillOID = fmt.Sprintf("%d", fill.OID)
			}
			bookPerpsCloseWithFillFee(ss, symbol, fill.AvgPx, fill.Fee, true, fillOID, "hedge_fail_closed", "Hedge fail-closed primary", "Hedge fail-closed primary", logger)
		}
		mu.Unlock()
		return
	}
	mu.Lock()
	if ss := state.Strategies[sc.ID]; ss != nil {
		bookPerpsClose(ss, symbol, mark, "hedge_fail_closed", "Hedge fail-closed primary", "Hedge fail-closed primary", logger)
	}
	mu.Unlock()
}

func appendPositionProtectionOIDs(pos *Position) []int64 {
	if pos == nil {
		return nil
	}
	out := make([]int64, 0, len(pos.TPOIDs)+1)
	if pos.StopLossOID > 0 {
		out = append(out, pos.StopLossOID)
	}
	for _, oid := range pos.TPOIDs {
		if oid > 0 {
			out = append(out, oid)
		}
	}
	return out
}

// reconcileHedgePosition is deliberately separate from the primary
// reconciler: hedge legs have no strategy-owned SL/TP surface and must never
// be passed through a close evaluator. It only records confirmed external
// reductions and synchronizes entry metadata; unowned on-chain hedge size is
// left fail-closed for the operator rather than adopted into virtual state.
func reconcileHedgePosition(sc StrategyConfig, ss *StrategyState, primaryCoin string, positions []HLPosition, resolveFee hlReconcileFillResolver, logger *StrategyLogger) bool {
	if ss == nil || !hedgeEnabled(sc) {
		return false
	}
	hCoin := hedgeCoin(sc)
	hSymbol, hPos := hedgePositionFor(ss, hCoin)
	if hSymbol == "" {
		hSymbol = hCoin
	}
	var onChain *HLPosition
	for i := range positions {
		if normalizeHedgeCoin(positions[i].Coin) == hCoin {
			onChain = &positions[i]
			break
		}
	}
	if hPos == nil {
		if onChain != nil && mathAbs(onChain.Size) > hedgeQuantityEpsilon && logger != nil {
			logger.Error("hedge reconcile: on-chain %s size %.6f exists without a virtual hedge position; refusing adoption (#1159)", hCoin, onChain.Size)
		}
		return false
	}
	primarySymbol, primary := hedgePositionFor(ss, primaryCoin)
	_ = primarySymbol
	if primary == nil || primary.Quantity <= hedgeQuantityEpsilon {
		return false
	}
	if onChain == nil || mathAbs(onChain.Size) <= hedgeQuantityEpsilon {
		lookup := HLFillLookup{}
		useFillFee := false
		if resolveFee != nil {
			lookup, useFillFee = resolveFee(hCoin, 0, hPos.Quantity)
		}
		px := hPos.AvgCost
		if lookup.Px > 0 {
			px = lookup.Px
		}
		oidStr := ""
		if useFillFee && lookup.OID > 0 {
			oidStr = fmt.Sprintf("%d", lookup.OID)
		}
		if logger != nil {
			logger.Warn("hedge reconcile: %s is flat on-chain while virtual qty is %.6f; booking a hedge close at %.4f", hCoin, hPos.Quantity, px)
		}
		return bookPerpsCloseWithFillFee(ss, hSymbol, px, lookup.Fee, useFillFee, oidStr, "hedge_reconcile", "Hedge reconcile close", "Hedge reconcile close", logger)
	}
	expectedSide := hedgeDesiredSide(primary.Side)
	onChainSide := "long"
	if onChain.Size < 0 {
		onChainSide = "short"
	}
	if expectedSide != "" && onChainSide != expectedSide {
		if logger != nil {
			logger.Error("hedge reconcile: %s on-chain side=%s conflicts with expected inverse side=%s; refusing state mutation (#1159)", hCoin, onChainSide, expectedSide)
		}
		return false
	}
	onChainQty := mathAbs(onChain.Size)
	if onChainQty < hPos.Quantity-hedgeQuantityEpsilon {
		oldQty := hPos.Quantity
		closeQty := oldQty - onChainQty
		lookup, useFillFee := resolveFee(hCoin, 0, closeQty)
		px := lookup.Px
		if px <= 0 {
			px = onChain.EntryPrice
		}
		if px <= 0 {
			px = hPos.AvgCost
		}
		oidStr := ""
		if useFillFee && lookup.OID > 0 {
			oidStr = fmt.Sprintf("%d", lookup.OID)
		}
		if !bookPerpsPartialCloseWithFillFee(ss, hSymbol, closeQty, px, lookup.Fee, useFillFee, oidStr, "hedge_reconcile", "Hedge reconcile reduce", "Hedge reconcile reduce", logger) {
			return false
		}
		if remaining := ss.Positions[hSymbol]; remaining != nil {
			remaining.HedgePrimaryQtyBasis *= onChainQty / oldQty
		}
		return true
	}
	if onChainQty > hPos.Quantity+hedgeQuantityEpsilon {
		if logger != nil {
			logger.Error("hedge reconcile: on-chain %s qty %.6f exceeds virtual qty %.6f; refusing adoption and leaving primary fail-closed", hCoin, onChainQty, hPos.Quantity)
		}
		return false
	}
	changed := false
	if onChain.EntryPrice > 0 && mathAbs(hPos.AvgCost-onChain.EntryPrice) > 1e-9 {
		hPos.AvgCost = onChain.EntryPrice
		changed = true
	}
	if hPos.Multiplier != 1 {
		hPos.Multiplier = 1
		changed = true
	}
	if sc.Hedge != nil && sc.Hedge.Leverage > 0 && hPos.Leverage != sc.Hedge.Leverage {
		hPos.Leverage = sc.Hedge.Leverage
		changed = true
	}
	return changed
}
