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
	hedgeQtyEpsilon          = 1e-9
	hedgeMinOrderNotionalUSD = 10.0
)

func HedgeEnabled(sc StrategyConfig) bool { return sc.Hedge != nil && sc.Hedge.Enabled }

func normalizeHedgeCoin(raw string) string {
	raw = strings.ToUpper(strings.TrimSpace(raw))
	if i := strings.IndexByte(raw, '/'); i >= 0 {
		raw = raw[:i]
	}
	if i := strings.IndexByte(raw, ':'); i >= 0 {
		raw = raw[:i]
	}
	return strings.TrimSpace(raw)
}

func hedgeCoin(sc StrategyConfig) string {
	if !HedgeEnabled(sc) {
		return ""
	}
	return normalizeHedgeCoin(sc.Hedge.Symbol)
}

func hedgeRatio(sc StrategyConfig) float64 {
	if !HedgeEnabled(sc) || sc.Hedge.Ratio == 0 {
		return 1
	}
	return sc.Hedge.Ratio
}

func hedgeLeverage(sc StrategyConfig) float64 {
	if !HedgeEnabled(sc) || sc.Hedge.Leverage == 0 {
		return 1
	}
	return sc.Hedge.Leverage
}

func hedgeMarginMode(sc StrategyConfig) string {
	if !HedgeEnabled(sc) || sc.Hedge.MarginMode == "" {
		return "isolated"
	}
	return strings.ToLower(strings.TrimSpace(sc.Hedge.MarginMode))
}

func validateHedgeConfigs(strategies []StrategyConfig) []string {
	configuredCoins := make(map[string][]string)
	for _, sc := range strategies {
		if coin := hyperliquidConfiguredCoin(sc); coin != "" {
			configuredCoins[coin] = append(configuredCoins[coin], sc.ID)
		}
	}
	hedgeOwners := make(map[string][]string)
	var errs []string
	for _, sc := range strategies {
		if !HedgeEnabled(sc) {
			continue
		}
		prefix := fmt.Sprintf("strategy[%s].hedge", sc.ID)
		h := sc.Hedge
		coin := hedgeCoin(sc)
		if sc.Platform != "hyperliquid" || sc.Type != "perps" {
			errs = append(errs, fmt.Sprintf("%s is supported only for platform=hyperliquid type=perps", prefix))
		}
		if h.Platform != "" && strings.ToLower(h.Platform) != "hyperliquid" {
			errs = append(errs, fmt.Sprintf("%s.platform must be hyperliquid, got %q", prefix, h.Platform))
		}
		if h.Type != "" && strings.ToLower(h.Type) != "perps" {
			errs = append(errs, fmt.Sprintf("%s.type must be perps, got %q", prefix, h.Type))
		}
		if h.Side != "" && strings.ToLower(h.Side) != "inverse" {
			errs = append(errs, fmt.Sprintf("%s.side must be inverse in phase 1, got %q", prefix, h.Side))
		}
		if EffectiveDirection(sc) == DirectionBoth {
			errs = append(errs, fmt.Sprintf("%s cannot be combined with direction=both in phase 1; use a fixed long/short direction", prefix))
		}
		if h.Ratio < 0 || h.Ratio > 10 {
			errs = append(errs, fmt.Sprintf("%s.ratio must be in (0, 10] when set, got %g", prefix, h.Ratio))
		}
		if h.Leverage < 0 || h.Leverage > 100 {
			errs = append(errs, fmt.Sprintf("%s.leverage must be in (0, 100] when set, got %g", prefix, h.Leverage))
		}
		mode := hedgeMarginMode(sc)
		if mode != "isolated" && mode != "cross" {
			errs = append(errs, fmt.Sprintf("%s.margin_mode must be isolated or cross, got %q", prefix, h.MarginMode))
		}
		if coin == "" {
			errs = append(errs, fmt.Sprintf("%s.symbol must name a hedge coin", prefix))
			continue
		}
		if coin == hyperliquidConfiguredCoin(sc) {
			errs = append(errs, fmt.Sprintf("%s.symbol %s collides with its primary coin", prefix, coin))
		}
		if owners := configuredCoins[coin]; len(owners) > 0 {
			sort.Strings(owners)
			errs = append(errs, fmt.Sprintf("%s.symbol %s collides with configured strategy coin owned by %s", prefix, coin, strings.Join(owners, ", ")))
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
			sort.Strings(owners)
			errs = append(errs, fmt.Sprintf("hedge coin %s is shared by strategies %s; phase 1 requires sole ownership", coin, strings.Join(owners, ", ")))
		}
	}
	return errs
}

type hedgeActionKind string

const (
	hedgeActionNone   hedgeActionKind = "none"
	hedgeActionOpen   hedgeActionKind = "open"
	hedgeActionAdd    hedgeActionKind = "add"
	hedgeActionReduce hedgeActionKind = "reduce"
	hedgeActionClose  hedgeActionKind = "close"
	hedgeActionUnwind hedgeActionKind = "unwind_primary"
)

type hedgeSnapshot struct {
	PrimaryQty, PrimaryAvgCost float64
	PrimarySide                string
	PrimaryPositionID          string
	PrimaryHedgeSymbol         string
	HedgeQty, HedgeAvgCost     float64
	HedgeBasis                 float64
	HedgeSide                  string
}

type hedgeAction struct {
	Kind   hedgeActionKind
	Qty    float64
	Side   string
	Reason string
}

func inverseHedgeSide(primarySide string) string {
	if primarySide == "short" {
		return "long"
	}
	if primarySide == "long" {
		return "short"
	}
	return ""
}

// hedgeTargetDecision is quantity-event based: marks size a new delta, but a
// change in marks alone never changes HedgeBasis and therefore never trades.
func hedgeTargetDecision(sc StrategyConfig, snap hedgeSnapshot, primaryPx, hedgePx float64) hedgeAction {
	if !HedgeEnabled(sc) {
		return hedgeAction{Kind: hedgeActionNone}
	}
	if snap.PrimaryQty <= hedgeQtyEpsilon {
		if snap.HedgeQty > hedgeQtyEpsilon {
			return hedgeAction{Kind: hedgeActionClose, Qty: snap.HedgeQty, Side: closeTradeSide(snap.HedgeSide)}
		}
		return hedgeAction{Kind: hedgeActionNone}
	}
	wantSide := inverseHedgeSide(snap.PrimarySide)
	if wantSide == "" {
		return hedgeAction{Kind: hedgeActionNone, Reason: "primary side is invalid"}
	}
	if snap.HedgeQty > hedgeQtyEpsilon && snap.HedgeSide != wantSide {
		return hedgeAction{Kind: hedgeActionUnwind, Reason: "hedge side disagrees with primary"}
	}
	if snap.HedgeQty <= hedgeQtyEpsilon {
		if snap.PrimaryHedgeSymbol != "" {
			return hedgeAction{Kind: hedgeActionOpen, Side: wantSide, Reason: "persisted hedge leg disappeared externally"}
		}
		if primaryPx <= 0 || hedgePx <= 0 {
			return hedgeAction{Kind: hedgeActionOpen, Side: wantSide, Reason: "primary or hedge mark is unavailable"}
		}
		qty := snap.PrimaryQty * primaryPx * hedgeRatio(sc) / hedgePx
		return hedgeAction{Kind: hedgeActionOpen, Qty: qty, Side: wantSide}
	}
	if primaryPx <= 0 || hedgePx <= 0 {
		return hedgeAction{Kind: hedgeActionNone, Reason: "primary or hedge mark is unavailable"}
	}
	basis := snap.HedgeBasis
	if basis <= hedgeQtyEpsilon {
		return hedgeAction{Kind: hedgeActionUnwind, Reason: "hedge coverage watermark is missing after reconciliation drift"}
	}
	if snap.PrimaryQty > basis+hedgeQtyEpsilon {
		qty := (snap.PrimaryQty - basis) * primaryPx * hedgeRatio(sc) / hedgePx
		return hedgeAction{Kind: hedgeActionAdd, Qty: qty, Side: wantSide}
	}
	if snap.PrimaryQty < basis-hedgeQtyEpsilon {
		fraction := (basis - snap.PrimaryQty) / basis
		qty := math.Min(snap.HedgeQty, snap.HedgeQty*fraction)
		if qty*hedgePx < hedgeMinOrderNotionalUSD && qty < snap.HedgeQty-hedgeQtyEpsilon {
			return hedgeAction{Kind: hedgeActionNone, Reason: "hedge reduction deferred below minimum notional"}
		}
		return hedgeAction{Kind: hedgeActionReduce, Qty: qty, Side: closeTradeSide(snap.HedgeSide)}
	}
	return hedgeAction{Kind: hedgeActionNone}
}

func hedgeSnapshotFor(s *StrategyState, primaryCoin, hedgeCoin string) hedgeSnapshot {
	var out hedgeSnapshot
	if p := s.Positions[primaryCoin]; p != nil && !p.IsHedge {
		out.PrimaryQty, out.PrimaryAvgCost, out.PrimarySide = p.Quantity, p.AvgCost, p.Side
		out.PrimaryPositionID = p.TradePositionID
		out.PrimaryHedgeSymbol = p.HedgeSymbol
	}
	if h := s.Positions[hedgeCoin]; h != nil && h.IsHedge {
		out.HedgeQty, out.HedgeAvgCost, out.HedgeSide = h.Quantity, h.AvgCost, h.Side
		out.HedgeBasis = h.HedgePrimaryQtyBasis
	}
	return out
}

func applyHedgeOpenOrAdd(s *StrategyState, sc StrategyConfig, primaryCoin string, snap hedgeSnapshot, action hedgeAction, fillPx, fillQty, fillFee float64, oid string) bool {
	if fillPx <= 0 || fillQty <= 0 {
		return false
	}
	coin := hedgeCoin(sc)
	now := time.Now().UTC()
	h := s.Positions[coin]
	if action.Kind == hedgeActionOpen {
		if h != nil {
			return false
		}
		pid := newTradePositionID(s.ID, coin, now)
		basis := snap.PrimaryQty
		if action.Qty > hedgeQtyEpsilon && fillQty < action.Qty {
			basis *= fillQty / action.Qty
		}
		h = &Position{Symbol: coin, Quantity: fillQty, InitialQuantity: fillQty, AvgCost: fillPx, Side: action.Side, Multiplier: 1, Leverage: hedgeLeverage(sc), OwnerStrategyID: s.ID, OpenedAt: now, TradePositionID: pid, IsHedge: true, HedgeForSymbol: primaryCoin, HedgeForPositionID: snap.PrimaryPositionID, HedgePrimaryQtyBasis: basis}
		s.Positions[coin] = h
		if p := s.Positions[primaryCoin]; p != nil {
			p.HedgeSymbol = coin
		}
	} else {
		if h == nil || !h.IsHedge || h.Side != action.Side {
			return false
		}
		newQty := h.Quantity + fillQty
		h.AvgCost = (h.AvgCost*h.Quantity + fillPx*fillQty) / newQty
		h.Quantity = newQty
		basis := snap.HedgeBasis
		if basis <= hedgeQtyEpsilon {
			basis = snap.PrimaryQty
		}
		advance := snap.PrimaryQty - basis
		if action.Qty > hedgeQtyEpsilon && fillQty < action.Qty {
			advance *= fillQty / action.Qty
		}
		h.HedgePrimaryQtyBasis = basis + advance
	}
	s.Cash -= fillFee
	feeSource := FeeSourceModeled
	if oid != "" {
		feeSource = FeeSourceUserFills
	}
	RecordTrade(s, Trade{Timestamp: now, StrategyID: s.ID, Symbol: coin, PositionID: h.TradePositionID, Side: map[string]string{"long": "buy", "short": "sell"}[action.Side], Quantity: fillQty, Price: fillPx, Value: fillQty * fillPx, TradeType: "hedge", Details: fmt.Sprintf("HEDGE %s for %s", action.Kind, primaryCoin), ExchangeOrderID: oid, ExchangeFee: fillFee, FeeSource: feeSource, PnLGross: true})
	return true
}

func executeHedgeAction(sc StrategyConfig, s *StrategyState, primaryCoin string, snap hedgeSnapshot, action hedgeAction, primaryPx, hedgePx float64, mu *sync.RWMutex, notifier *MultiNotifier, logger *StrategyLogger) (int, bool) {
	coin := hedgeCoin(sc)
	if action.Kind == hedgeActionNone {
		return 0, action.Reason == "" || strings.Contains(action.Reason, "minimum notional")
	}
	if action.Kind == hedgeActionUnwind {
		return 0, false
	}
	live := hyperliquidIsLive(sc.Args)
	if action.Kind == hedgeActionOpen || action.Kind == hedgeActionAdd {
		if action.Qty <= hedgeQtyEpsilon {
			return 0, false
		}
		fillPx, fillQty := hedgePx, action.Qty
		fee := CalculatePlatformSpotFee("hyperliquid", fillPx*fillQty)
		oid := ""
		if live {
			marginMode := ""
			leverage := 0.0
			if action.Kind == hedgeActionOpen {
				marginMode, leverage = hedgeMarginMode(sc), hedgeLeverage(sc)
			}
			res, stderr, err := RunHyperliquidExecute(sc.Script, coin, action.Side, action.Qty, 0, 0, 0, marginMode, leverage, false, hlExecuteSnapshot{})
			if err != nil || res == nil || res.Execution == nil || res.Execution.Fill == nil || res.Execution.Fill.TotalSz <= 0 {
				if logger != nil {
					logger.Error("hedge %s failed for %s: %v stderr=%s", action.Kind, coin, err, stderr)
				}
				return 0, false
			}
			fillPx, fillQty, fee = res.Execution.Fill.AvgPx, res.Execution.Fill.TotalSz, res.Execution.Fill.Fee
			oid = fmt.Sprint(res.Execution.Fill.OID)
		}
		mu.Lock()
		ok := applyHedgeOpenOrAdd(s, sc, primaryCoin, snap, action, fillPx, fillQty, fee, oid)
		mu.Unlock()
		return 1, ok
	}

	fillPx, fillQty := hedgePx, action.Qty
	fee := CalculatePlatformSpotFee("hyperliquid", fillPx*fillQty)
	oid := ""
	if live {
		partial := action.Qty
		res, stderr, err := RunHyperliquidClose(sc.Script, coin, &partial, nil)
		if err != nil || res == nil || res.Close == nil || res.Close.Fill == nil {
			if logger != nil {
				logger.Error("hedge %s failed for %s: %v stderr=%s", action.Kind, coin, err, stderr)
			}
			return 0, false
		}
		fillPx, fillQty, fee = res.Close.Fill.AvgPx, res.Close.Fill.TotalSz, res.Close.Fill.Fee
		oid = fmt.Sprint(res.Close.Fill.OID)
	}
	mu.Lock()
	ok := false
	if action.Kind == hedgeActionClose || fillQty >= snap.HedgeQty-hedgeQtyEpsilon {
		ok = bookPerpsCloseWithFillFee(s, coin, fillPx, fee, live, oid, "hedge_sync", "HEDGE close", "Hedge closed", logger)
		if p := s.Positions[primaryCoin]; p != nil {
			p.HedgeSymbol = ""
		}
	} else {
		ok = bookPerpsPartialCloseWithFillFee(s, coin, fillQty, fillPx, fee, live, oid, "hedge_sync", "HEDGE reduce", "Hedge reduced", logger)
		if h := s.Positions[coin]; h != nil {
			basis := snap.HedgeBasis
			if basis <= hedgeQtyEpsilon {
				basis = snap.PrimaryQty
			}
			advance := basis - snap.PrimaryQty
			if action.Qty > hedgeQtyEpsilon && fillQty < action.Qty {
				advance *= fillQty / action.Qty
			}
			h.HedgePrimaryQtyBasis = basis - advance
		}
	}
	mu.Unlock()
	return 1, ok
}

// runStrategyHedgeSync is the one lifecycle choke point. It is called after
// each HL-perps management cycle; primary opens, adds, partial/full closes and
// reconcile changes therefore converge without scattering mirror hooks.
func runStrategyHedgeSync(sc StrategyConfig, s *StrategyState, primaryCoin string, prices map[string]float64, mu *sync.RWMutex, notifier *MultiNotifier, logger *StrategyLogger) (int, string) {
	if !HedgeEnabled(sc) || s == nil || primaryCoin == "" {
		return 0, ""
	}
	coin := hedgeCoin(sc)
	mu.RLock()
	snap := hedgeSnapshotFor(s, primaryCoin, coin)
	mu.RUnlock()
	primaryPx := prices[primaryCoin]
	if primaryPx <= 0 {
		primaryPx = snap.PrimaryAvgCost
	}
	hedgePx := prices[coin]
	if hedgePx <= 0 {
		hedgePx = snap.HedgeAvgCost
	}
	action := hedgeTargetDecision(sc, snap, primaryPx, hedgePx)
	if action.Reason != "" && action.Kind == hedgeActionNone {
		if logger != nil {
			logger.Warn("hedge sync deferred for %s: %s", coin, action.Reason)
		}
		if snap.PrimaryQty > 0 && snap.HedgeQty <= 0 && notifier != nil {
			notifier.SendOwnerDM(fmt.Sprintf("**CRITICAL HEDGE GAP** [%s] %s: %s", sc.ID, coin, action.Reason))
		}
		return 0, ""
	}
	trades, ok := executeHedgeAction(sc, s, primaryCoin, snap, action, primaryPx, hedgePx, mu, notifier, logger)
	if ok {
		return trades, fmt.Sprintf("[%s] HEDGE %s %s %.6f", sc.ID, action.Kind, coin, action.Qty)
	}
	// Fail closed: if a primary is open without a confirmed hedge, close the
	// primary reduce-only and book only a confirmed unwind fill.
	if snap.PrimaryQty > 0 && (snap.HedgeQty <= 0 || action.Kind == hedgeActionOpen || action.Kind == hedgeActionAdd || action.Kind == hedgeActionUnwind) {
		msg := fmt.Sprintf("**CRITICAL HEDGE FAILURE** [%s] failed to %s %s; closing primary %s reduce-only", sc.ID, action.Kind, coin, primaryCoin)
		if notifier != nil {
			notifier.SendOwnerDM(msg)
		}
		fillPx, fillQty, fee, oid := primaryPx, snap.PrimaryQty, CalculatePlatformSpotFee("hyperliquid", primaryPx*snap.PrimaryQty), ""
		if hyperliquidIsLive(sc.Args) {
			partial := snap.PrimaryQty
			mu.RLock()
			var cancels []int64
			if p := s.Positions[primaryCoin]; p != nil {
				cancels = appendUniquePositiveStopLossOID(cancels, p.StopLossOID)
				for _, x := range p.TPOIDs {
					cancels = appendUniquePositiveStopLossOID(cancels, x)
				}
			}
			mu.RUnlock()
			res, _, err := RunHyperliquidClose(sc.Script, primaryCoin, &partial, cancels)
			if err != nil || res == nil || res.Close == nil || res.Close.Fill == nil {
				if notifier != nil {
					notifier.SendOwnerDM(fmt.Sprintf("**CRITICAL HEDGE UNWIND FAILED** [%s] primary %s remains exposed; retrying next cycle", sc.ID, primaryCoin))
				}
				return trades, ""
			}
			fillPx, fillQty, fee, oid = res.Close.Fill.AvgPx, res.Close.Fill.TotalSz, res.Close.Fill.Fee, fmt.Sprint(res.Close.Fill.OID)
		}
		mu.Lock()
		if fillQty >= snap.PrimaryQty-hedgeQtyEpsilon {
			bookPerpsCloseWithFillFee(s, primaryCoin, fillPx, fee, hyperliquidIsLive(sc.Args), oid, "hedge_open_failed", "Fail-closed primary unwind", "Primary unwound", logger)
		} else {
			bookPerpsPartialCloseWithFillFee(s, primaryCoin, fillQty, fillPx, fee, hyperliquidIsLive(sc.Args), oid, "hedge_open_failed", "Fail-closed primary unwind", "Primary reduced", logger)
		}
		mu.Unlock()
		return trades + 1, fmt.Sprintf("[%s] HEDGE FAILED — primary unwound", sc.ID)
	}
	return trades, ""
}

// validateHedgeStateConsistency catches config-edit-plus-restart drift that
// bypasses the SIGHUP flat-only guard. It is warning-only and non-destructive:
// ambiguous live ownership must never be guessed or auto-adopted at startup.
func validateHedgeStateConsistency(state *AppState, cfg *Config) []string {
	if state == nil || cfg == nil {
		return nil
	}
	byID := strategyConfigByID(cfg.Strategies)
	ids := make([]string, 0, len(state.Strategies))
	for id := range state.Strategies {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var warnings []string
	for _, id := range ids {
		s := state.Strategies[id]
		if s == nil {
			continue
		}
		sc, ok := byID[id]
		syms := make([]string, 0, len(s.Positions))
		for sym := range s.Positions {
			syms = append(syms, sym)
		}
		sort.Strings(syms)
		for _, sym := range syms {
			p := s.Positions[sym]
			if p == nil || !p.IsHedge {
				continue
			}
			switch {
			case !ok || !HedgeEnabled(sc):
				warnings = append(warnings, fmt.Sprintf("strategy %s has persisted hedge %s but hedge config is missing/disabled; leaving it frozen for operator reconciliation", id, sym))
			case sym != hedgeCoin(sc):
				warnings = append(warnings, fmt.Sprintf("strategy %s persisted hedge coin %s differs from configured %s; leaving it frozen", id, sym, hedgeCoin(sc)))
			case p.HedgeForSymbol == "" || p.HedgeForPositionID == "":
				warnings = append(warnings, fmt.Sprintf("strategy %s hedge %s lacks persisted primary ownership metadata; refusing inferred adoption", id, sym))
			case s.Positions[p.HedgeForSymbol] == nil:
				warnings = append(warnings, fmt.Sprintf("strategy %s hedge %s references missing primary %s; scheduler will close the orphan on its management cycle", id, sym, p.HedgeForSymbol))
			}
		}
	}
	return warnings
}
