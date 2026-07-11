package main

import (
	"fmt"
	"math"
	"strings"
	"time"
)

const (
	hedgeQtyEpsilon                 = 1e-9
	DefaultHedgeRebalanceMinMovePct = 0.5
)

type hedgeTarget struct {
	Side     string
	Quantity float64
}

type hedgeOrder struct {
	Side      string
	Quantity  float64
	Close     bool
	FullClose bool
}

func hedgeEnabled(sc StrategyConfig) bool { return sc.Hedge != nil && sc.Hedge.Enabled }

func hedgeCoin(sc StrategyConfig) string {
	if !hedgeEnabled(sc) {
		return ""
	}
	return hyperliquidCoinFromSymbol(sc.Hedge.Symbol)
}

func hyperliquidCoinFromSymbol(symbol string) string {
	s := strings.TrimSpace(symbol)
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[:i]
	}
	return strings.ToUpper(strings.TrimSpace(s))
}

func hedgeTargetForPrimary(sc StrategyConfig, primarySide string, primaryQty, primaryPrice, hedgePrice float64) (hedgeTarget, error) {
	if !hedgeEnabled(sc) || primaryQty <= hedgeQtyEpsilon {
		return hedgeTarget{}, nil
	}
	if primaryPrice <= 0 || hedgePrice <= 0 {
		return hedgeTarget{}, fmt.Errorf("hedge sizing requires positive primary and hedge prices")
	}
	side := ""
	switch primarySide {
	case "long":
		side = "short"
	case "short":
		side = "long"
	default:
		return hedgeTarget{}, fmt.Errorf("invalid primary side %q", primarySide)
	}
	qty := primaryQty * primaryPrice * sc.Hedge.Ratio / hedgePrice
	if math.IsNaN(qty) || math.IsInf(qty, 0) || qty <= hedgeQtyEpsilon {
		return hedgeTarget{}, fmt.Errorf("resolved hedge quantity is not positive")
	}
	return hedgeTarget{Side: side, Quantity: qty}, nil
}

func planHedgeTransition(current *Position, target hedgeTarget) ([]hedgeOrder, error) {
	return planHedgeTransitionWithPolicy(current, target, true, 0)
}

// planHedgeTransitionWithPolicy suppresses hold-cycle churn below the
// configured relative quantity move. A confirmed primary lifecycle mutation
// passes force=true so even a small scale/reduce is mirrored immediately.
// Because the comparison is always target-vs-current, skipped drift accumulates
// and eventually crosses the threshold rather than resetting each cycle.
func planHedgeTransitionWithPolicy(current *Position, target hedgeTarget, force bool, minMovePct float64) ([]hedgeOrder, error) {
	if current != nil && (!current.IsHedge || current.Quantity <= 0 || (current.Side != "long" && current.Side != "short")) {
		return nil, fmt.Errorf("ambiguous or corrupt hedge ownership state")
	}
	if target.Quantity < 0 || (target.Quantity > hedgeQtyEpsilon && target.Side != "long" && target.Side != "short") {
		return nil, fmt.Errorf("invalid hedge target")
	}
	if current == nil {
		if target.Quantity <= hedgeQtyEpsilon {
			return nil, nil
		}
		return []hedgeOrder{{Side: openSideForPosition(target.Side), Quantity: target.Quantity}}, nil
	}
	if target.Quantity <= hedgeQtyEpsilon {
		return []hedgeOrder{{Close: true, Quantity: current.Quantity, FullClose: true}}, nil
	}
	if current.Side != target.Side {
		return []hedgeOrder{{Close: true, Quantity: current.Quantity, FullClose: true}, {Side: openSideForPosition(target.Side), Quantity: target.Quantity}}, nil
	}
	delta := target.Quantity - current.Quantity
	if math.Abs(delta) <= hedgeQtyEpsilon {
		return nil, nil
	}
	if !force && current.Quantity > hedgeQtyEpsilon && minMovePct > 0 {
		movePct := math.Abs(delta) / current.Quantity * 100
		if movePct < minMovePct {
			return nil, nil
		}
	}
	if delta > 0 {
		return []hedgeOrder{{Side: openSideForPosition(target.Side), Quantity: delta}}, nil
	}
	return []hedgeOrder{{Close: true, Quantity: -delta}}, nil
}

func hedgeRebalanceMinMovePct(sc StrategyConfig) float64 {
	if !hedgeEnabled(sc) || sc.Hedge.RebalanceMinMovePct <= 0 {
		return DefaultHedgeRebalanceMinMovePct
	}
	return sc.Hedge.RebalanceMinMovePct
}

func openSideForPosition(side string) string {
	if side == "long" {
		return "buy"
	}
	return "sell"
}

func validateHedgeConfigs(strategies []StrategyConfig) []string {
	var errs []string
	primaryOwners := make(map[string]string)
	hedgeOwners := make(map[string]string)
	for _, sc := range strategies {
		if coin := hyperliquidConfiguredCoin(sc); coin != "" {
			primaryOwners[coin] = sc.ID
		}
	}
	for _, sc := range strategies {
		if !hedgeEnabled(sc) {
			continue
		}
		prefix := fmt.Sprintf("strategy[%s].hedge", sc.ID)
		h := sc.Hedge
		coin := hedgeCoin(sc)
		primary := hyperliquidConfiguredCoin(sc)
		if sc.Platform != "hyperliquid" || sc.Type != "perps" || !hyperliquidIsLive(sc.Args) || h.Platform != "hyperliquid" || h.Type != "perps" {
			errs = append(errs, prefix+": phase 1 requires live Hyperliquid perps for both primary and hedge")
		}
		if coin == "" {
			errs = append(errs, prefix+".symbol is required")
		}
		if h.Side != HedgeSideInverse {
			errs = append(errs, prefix+".side must be \"inverse\"")
		}
		if h.Ratio <= 0 || math.IsNaN(h.Ratio) || math.IsInf(h.Ratio, 0) {
			errs = append(errs, prefix+".ratio must be > 0")
		}
		if h.MarginMode != "isolated" && h.MarginMode != "cross" {
			errs = append(errs, prefix+".margin_mode must be \"isolated\" or \"cross\"")
		}
		if h.Leverage < 1 || h.Leverage > 50 {
			errs = append(errs, prefix+".leverage must be in [1, 50]")
		}
		if h.RebalanceMinMovePct < 0 || h.RebalanceMinMovePct > 100 {
			errs = append(errs, prefix+".rebalance_min_move_pct must be in [0, 100] (0 = default 0.5%)")
		}
		if coin != "" && coin == primary {
			errs = append(errs, prefix+".symbol matches its primary coin")
		}
		if owner, ok := primaryOwners[coin]; ok && owner != sc.ID {
			errs = append(errs, fmt.Sprintf("%s.symbol matches configured strategy %s primary coin", prefix, owner))
		}
		if owner, ok := hedgeOwners[coin]; ok && owner != sc.ID {
			errs = append(errs, fmt.Sprintf("%s.symbol is shared by hedge-enabled strategies %s and %s", prefix, owner, sc.ID))
		} else if coin != "" {
			hedgeOwners[coin] = sc.ID
		}
	}
	return errs
}

func hedgeConfigEqual(a, b *HedgeConfig) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// strategyHasOpenHedgeLeg reports whether s owns a live phase-1 hedge leg.
func strategyHasOpenHedgeLeg(s *StrategyState) bool {
	return findHedgePosition(s, StrategyConfig{}) != nil
}

func findHedgePosition(s *StrategyState, sc StrategyConfig) *Position {
	if s == nil {
		return nil
	}
	coin := hedgeCoin(sc)
	for symbol, pos := range s.Positions {
		if pos == nil || !pos.IsHedge || pos.Quantity <= hedgeQtyEpsilon {
			continue
		}
		if coin == "" || strings.EqualFold(symbol, coin) {
			return pos
		}
	}
	return nil
}

func findPrimaryPosition(s *StrategyState, primarySym string) *Position {
	if s == nil {
		return nil
	}
	pos := s.Positions[primarySym]
	if pos == nil || pos.IsHedge || pos.Quantity <= hedgeQtyEpsilon {
		return nil
	}
	return pos
}

func applyHedgeOpen(s *StrategyState, sc StrategyConfig, primary *Position, side string, qty, px, fee float64, oid string, logger *StrategyLogger) *Trade {
	if s == nil || primary == nil || qty <= 0 || px <= 0 || (side != "long" && side != "short") {
		return nil
	}
	coin := hedgeCoin(sc)
	if coin == "" {
		return nil
	}
	now := time.Now().UTC()
	primaryID := ensurePositionTradeID(sc.ID, primary.Symbol, primary)
	pos := &Position{
		Symbol: coin, Quantity: qty, InitialQuantity: qty,
		AvgCost: px, Side: side, Multiplier: 1, Leverage: sc.Hedge.Leverage,
		OwnerStrategyID: sc.ID, IsHedge: true,
		HedgePrimarySymbol: primary.Symbol, HedgePrimaryPositionID: primaryID, OpenedAt: now,
	}
	pos.TradePositionID = primaryID + ":hedge"
	s.Positions[coin] = pos
	trade := Trade{
		Timestamp: now, StrategyID: sc.ID, Symbol: coin, Side: openTradeSide(side),
		Quantity: qty, Price: px, Value: qty * px, TradeType: "perps",
		Details:    fmt.Sprintf("[hedge] open %s %s @ $%.4f", side, coin, px),
		PositionID: pos.TradePositionID, ExchangeOrderID: oid,
		ExchangeFee: fee, FeeSource: FeeSourceUserFills, PnLGross: true,
	}
	RecordTrade(s, trade)
	s.Cash -= fee
	if logger != nil {
		logger.Info("[hedge] opened %s %.6f %s @ $%.4f", side, qty, coin, px)
	}
	return &trade
}

func applyHedgeScale(pos *Position, addQty, px float64) {
	if pos == nil || !pos.IsHedge || addQty <= 0 || px <= 0 {
		return
	}
	oldQty := pos.Quantity
	newQty := oldQty + addQty
	if newQty <= 0 {
		return
	}
	pos.AvgCost = (oldQty*pos.AvgCost + addQty*px) / newQty
	pos.Quantity = newQty
	pos.InitialQuantity += addQty
}
