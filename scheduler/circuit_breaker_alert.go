package main

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	circuitBreakerAlertMaxRows  = 5
	circuitBreakerAlertMaxChars = 1900
)

type perStrategyCircuitBreakerSnapshot struct {
	Risk          RiskState
	Closed        []circuitBreakerPositionLine
	Open          []circuitBreakerPositionLine
	Pending       []circuitBreakerPendingLine
	RemainingLoss int
	Now           time.Time
}

type circuitBreakerPositionLine struct {
	Symbol   string
	Side     string
	Quantity float64
	Price    float64
	PnL      float64
	Status   string
}

type circuitBreakerPendingLine struct {
	Platform         string
	Symbol           string
	Size             float64
	OperatorRequired bool
}

type perStrategyCircuitBreakerFormatInput struct {
	Strategy            StrategyConfig
	Snapshot            perStrategyCircuitBreakerSnapshot
	Reason              string
	StrategyValue       float64
	TotalPortfolioValue float64
	RecentTrades        []Trade
}

var cbMaxDrawdownReasonRE = regexp.MustCompile(`\(([0-9.]+)% > ([0-9.]+)%, portfolio=\$([0-9.]+) peak=\$([0-9.]+), denom=([^=]+)=\$([0-9.]+)\)`)

func snapshotPerStrategyCircuitBreaker(s *StrategyState, prices map[string]float64) perStrategyCircuitBreakerSnapshot {
	snap := perStrategyCircuitBreakerSnapshot{Now: time.Now().UTC()}
	if s == nil {
		return snap
	}
	snap.Risk = s.RiskState

	for i := len(s.ClosedPositions) - 1; i >= 0 && len(snap.Closed) < circuitBreakerAlertMaxRows; i-- {
		cp := s.ClosedPositions[i]
		if cp.CloseReason != "circuit_breaker" {
			continue
		}
		snap.Closed = append(snap.Closed, circuitBreakerPositionLine{
			Symbol:   cp.Symbol,
			Side:     cp.Side,
			Quantity: cp.Quantity,
			Price:    cp.ClosePrice,
			PnL:      cp.RealizedPnL,
			Status:   "virtual close recorded",
		})
	}
	for i := len(s.ClosedOptionPositions) - 1; i >= 0 && len(snap.Closed) < circuitBreakerAlertMaxRows; i-- {
		cp := s.ClosedOptionPositions[i]
		if cp.CloseReason != "circuit_breaker" {
			continue
		}
		symbol := cp.PositionID
		if symbol == "" {
			symbol = cp.Underlying
		}
		snap.Closed = append(snap.Closed, circuitBreakerPositionLine{
			Symbol:   symbol,
			Side:     cp.Action,
			Quantity: cp.Quantity,
			Price:    cp.ClosePriceUSD,
			PnL:      cp.RealizedPnL,
			Status:   "virtual close recorded",
		})
	}

	symbols := make([]string, 0, len(s.Positions))
	for sym, pos := range s.Positions {
		if pos != nil && pos.Quantity > 0 {
			symbols = append(symbols, sym)
		}
	}
	sort.Strings(symbols)
	for _, sym := range symbols {
		if len(snap.Open) >= circuitBreakerAlertMaxRows {
			snap.RemainingLoss++
			continue
		}
		pos := s.Positions[sym]
		price := prices[sym]
		if price <= 0 {
			price = pos.AvgCost
		}
		snap.Open = append(snap.Open, circuitBreakerPositionLine{
			Symbol:   sym,
			Side:     pos.Side,
			Quantity: pos.Quantity,
			Price:    price,
			PnL:      circuitBreakerPositionPnL(pos, price),
			Status:   "still open",
		})
	}

	optionIDs := make([]string, 0, len(s.OptionPositions))
	for id, pos := range s.OptionPositions {
		if pos != nil && pos.Quantity > 0 {
			optionIDs = append(optionIDs, id)
		}
	}
	sort.Strings(optionIDs)
	for _, id := range optionIDs {
		if len(snap.Open) >= circuitBreakerAlertMaxRows {
			snap.RemainingLoss++
			continue
		}
		pos := s.OptionPositions[id]
		pnl := pos.CurrentValueUSD - pos.EntryPremiumUSD
		if pos.Action == "sell" {
			pnl = pos.EntryPremiumUSD - pos.CurrentValueUSD
		}
		snap.Open = append(snap.Open, circuitBreakerPositionLine{
			Symbol:   id,
			Side:     pos.Action,
			Quantity: pos.Quantity,
			Price:    pos.CurrentValueUSD,
			PnL:      pnl,
			Status:   "still open",
		})
	}

	if len(s.RiskState.PendingCircuitCloses) > 0 {
		platforms := make([]string, 0, len(s.RiskState.PendingCircuitCloses))
		for platform := range s.RiskState.PendingCircuitCloses {
			platforms = append(platforms, platform)
		}
		sort.Strings(platforms)
		for _, platform := range platforms {
			pending := s.RiskState.PendingCircuitCloses[platform]
			if pending == nil {
				continue
			}
			legs := append([]PendingCircuitCloseSymbol(nil), pending.Symbols...)
			sort.Slice(legs, func(i, j int) bool { return legs[i].Symbol < legs[j].Symbol })
			for _, leg := range legs {
				snap.Pending = append(snap.Pending, circuitBreakerPendingLine{
					Platform:         platform,
					Symbol:           leg.Symbol,
					Size:             leg.Size,
					OperatorRequired: pending.OperatorRequired,
				})
			}
		}
	}
	return snap
}

func formatPerStrategyCircuitBreakerBlock(in perStrategyCircuitBreakerFormatInput) string {
	now := in.Snapshot.Now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	sc := in.Strategy
	if sc.ID == "" {
		sc.ID = "unknown-strategy"
	}
	trigger := circuitBreakerTriggerLine(in.Reason)

	var b strings.Builder
	b.WriteString(fmt.Sprintf("**CIRCUIT BREAKER** [%s] - %s\n", sc.ID, circuitBreakerStrategyLabel(sc)))
	b.WriteString("Trigger: ")
	b.WriteString(trigger)
	b.WriteByte('\n')
	b.WriteString("Cooldown: ")
	b.WriteString(formatCircuitBreakerCooldown(in.Snapshot.Risk.CircuitBreakerUntil, now))
	b.WriteByte('\n')
	b.WriteString("Portfolio impact: ")
	b.WriteString(formatCircuitBreakerPortfolioImpact(sc, in.StrategyValue, in.TotalPortfolioValue))
	b.WriteByte('\n')
	if perps := formatCircuitBreakerPerpsContext(sc, in.Reason); perps != "" {
		b.WriteString(perps)
		b.WriteByte('\n')
	}
	if in.Snapshot.Risk.CurrentDrawdownPct > 0 || in.Snapshot.Risk.PeakValue > 0 {
		b.WriteString(fmt.Sprintf("\nStrategy drawdown: %.1f%% (peak $%s -> current $%s)\n",
			in.Snapshot.Risk.CurrentDrawdownPct, formatCBMoney(in.Snapshot.Risk.PeakValue), formatCBMoney(in.StrategyValue)))
	}

	if len(in.Snapshot.Closed) > 0 {
		b.WriteString("Positions force-closed:\n")
		for i, line := range in.Snapshot.Closed {
			if i >= circuitBreakerAlertMaxRows {
				break
			}
			b.WriteString("  ")
			b.WriteString(formatCircuitBreakerPositionLine(line))
			b.WriteByte('\n')
		}
	}
	if len(in.Snapshot.Open) > 0 {
		b.WriteString("Open positions still exposed:\n")
		for i, line := range in.Snapshot.Open {
			if i >= circuitBreakerAlertMaxRows {
				break
			}
			b.WriteString("  ")
			b.WriteString(formatCircuitBreakerPositionLine(line))
			b.WriteByte('\n')
		}
		if in.Snapshot.RemainingLoss > 0 {
			b.WriteString(fmt.Sprintf("  +%d more\n", in.Snapshot.RemainingLoss))
		}
	}
	b.WriteString(formatCircuitBreakerPending(in.Snapshot.Pending))

	if len(in.RecentTrades) > 0 {
		if strings.HasPrefix(in.Reason, RiskReasonConsecutiveLosses) {
			b.WriteString("\nLast 5 trades:\n")
		} else {
			b.WriteString("\nRecent trades:\n")
		}
		for i, tr := range in.RecentTrades {
			if i >= circuitBreakerAlertMaxRows {
				break
			}
			b.WriteString("  ")
			b.WriteString(formatCircuitBreakerTrade(tr))
			b.WriteByte('\n')
		}
		if strings.HasPrefix(in.Reason, RiskReasonConsecutiveLosses) && in.Snapshot.Risk.ConsecutiveLosses > 0 {
			b.WriteString(fmt.Sprintf("Consecutive loss run: %d/5\n", in.Snapshot.Risk.ConsecutiveLosses))
		}
	}

	b.WriteString("\nReason: ")
	b.WriteString(circuitBreakerRecommendation(in.Reason))

	msg := strings.TrimRight(b.String(), "\n")
	if len(msg) <= circuitBreakerAlertMaxChars {
		return msg
	}
	return msg[:circuitBreakerAlertMaxChars-3] + "..."
}

func circuitBreakerTriggerLine(reason string) string {
	if m := cbMaxDrawdownReasonRE.FindStringSubmatch(reason); len(m) == 7 {
		return fmt.Sprintf("%s - %s%% > %s%% (denom: %s=$%s)", RiskReasonMaxDrawdownExceeded, m[1], m[2], m[5], m[6])
	}
	if strings.HasPrefix(reason, RiskReasonConsecutiveLosses) {
		return RiskReasonConsecutiveLosses
	}
	return reason
}

func circuitBreakerStrategyLabel(sc StrategyConfig) string {
	parts := []string{humanPlatformName(sc.Platform)}
	if asset := circuitBreakerAsset(sc); asset != "" {
		parts = append(parts, asset)
	}
	if tf := circuitBreakerTimeframe(sc); tf != "" {
		parts = append(parts, tf)
	}
	if name := circuitBreakerStrategyName(sc); name != "" {
		parts = append(parts, name)
	}
	if sc.Type != "" {
		parts = append(parts, sc.Type)
	}
	filtered := parts[:0]
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			filtered = append(filtered, p)
		}
	}
	if len(filtered) == 0 {
		return sc.ID
	}
	return strings.Join(filtered, ", ")
}

func humanPlatformName(platform string) string {
	switch platform {
	case "hyperliquid":
		return "Hyperliquid"
	case "binanceus":
		return "BinanceUS"
	case "topstep":
		return "TopStep"
	case "robinhood":
		return "Robinhood"
	case "deribit":
		return "Deribit"
	case "ibkr":
		return "IBKR"
	case "okx":
		return "OKX"
	case "luno":
		return "Luno"
	default:
		return platform
	}
}

func circuitBreakerAsset(sc StrategyConfig) string {
	if sc.Symbol != "" {
		return sc.Symbol
	}
	return extractAsset(sc)
}

func circuitBreakerTimeframe(sc StrategyConfig) string {
	if sc.Timeframe != "" {
		return sc.Timeframe
	}
	if len(sc.Args) > 2 && !strings.HasPrefix(sc.Args[2], "--") {
		return sc.Args[2]
	}
	return ""
}

func circuitBreakerStrategyName(sc StrategyConfig) string {
	if sc.OpenStrategy.Name != "" {
		return sc.OpenStrategy.Name
	}
	if len(sc.Args) > 0 {
		return sc.Args[0]
	}
	return ""
}

func formatCircuitBreakerCooldown(until time.Time, now time.Time) string {
	if until.IsZero() {
		return "unknown"
	}
	until = until.UTC()
	remaining := until.Sub(now)
	if remaining < 0 {
		remaining = 0
	}
	return fmt.Sprintf("%s (until %s)", formatCBDuration(remaining), until.Format("2006-01-02 15:04 UTC"))
}

func formatCircuitBreakerPortfolioImpact(sc StrategyConfig, strategyValue, totalValue float64) string {
	if totalValue > 0 {
		impact := strategyValue
		pct := strategyValue / totalValue * 100
		if sc.CapitalPct > 0 {
			impact = sc.CapitalPct * totalValue
			pct = sc.CapitalPct * 100
		}
		return fmt.Sprintf("~$%s of ~$%s (%.1f%%)", formatCBMoney(impact), formatCBMoney(totalValue), pct)
	}
	return fmt.Sprintf("strategy value ~$%s", formatCBMoney(strategyValue))
}

func formatCircuitBreakerPerpsContext(sc StrategyConfig, reason string) string {
	if sc.Type != "perps" {
		return ""
	}
	parts := []string{}
	if sc.Leverage > 0 {
		parts = append(parts, fmt.Sprintf("%sx leverage", formatCBNumber(sc.Leverage)))
	}
	if m := cbMaxDrawdownReasonRE.FindStringSubmatch(reason); len(m) == 7 && m[5] == "margin" {
		parts = append(parts, fmt.Sprintf("margin deployed=$%s", m[6]))
	}
	if len(parts) == 0 {
		return ""
	}
	return "Perps context: " + strings.Join(parts, ", ")
}

func formatCircuitBreakerPositionLine(line circuitBreakerPositionLine) string {
	return fmt.Sprintf("%-5s %s %s @ $%s  P&L %s  (%s)",
		line.Side, formatCBQty(line.Quantity), line.Symbol, formatCBPrice(line.Price), formatCBSignedMoney(line.PnL), line.Status)
}

func formatCircuitBreakerPending(lines []circuitBreakerPendingLine) string {
	if len(lines) == 0 {
		return "Pending operator closes: none\n"
	}
	sort.SliceStable(lines, func(i, j int) bool {
		if lines[i].OperatorRequired != lines[j].OperatorRequired {
			return lines[i].OperatorRequired
		}
		if lines[i].Platform == lines[j].Platform {
			return lines[i].Symbol < lines[j].Symbol
		}
		return lines[i].Platform < lines[j].Platform
	})
	var b strings.Builder
	var wroteOperator, wroteAuto bool
	for _, line := range lines {
		if line.OperatorRequired {
			if !wroteOperator {
				b.WriteString("Pending operator closes:\n")
				wroteOperator = true
			}
			b.WriteString(fmt.Sprintf("  %s %s size %s (manual flatten required)\n", line.Platform, line.Symbol, formatCBQty(line.Size)))
			continue
		}
		if !wroteAuto {
			b.WriteString("Pending automated closes:\n")
			wroteAuto = true
		}
		b.WriteString(fmt.Sprintf("  %s %s size %s (close submitted)\n", line.Platform, line.Symbol, formatCBQty(line.Size)))
	}
	return b.String()
}

func formatCircuitBreakerTrade(tr Trade) string {
	kind := "open"
	if tr.IsClose {
		kind = "close"
	}
	details := strings.TrimSpace(tr.Details)
	if details != "" {
		details = " (" + truncateCircuitBreakerField(details, 44) + ")"
	}
	pnl := ""
	if netPnL := tradeNetPnL(tr); tr.IsClose || netPnL != 0 {
		pnl = " P&L " + formatCBSignedMoney(netPnL)
	}
	return fmt.Sprintf("%s  %s  %s %s %s @ $%s%s%s",
		tr.Timestamp.UTC().Format("15:04"), kind, tr.Side, formatCBQty(tr.Quantity), tr.Symbol, formatCBPrice(tr.Price), pnl, details)
}

func circuitBreakerRecommendation(reason string) string {
	if strings.HasPrefix(reason, RiskReasonConsecutiveLosses) {
		return "Strategy may have lost edge in the current regime; consider extending cooldown or pausing manually."
	}
	return "Investigate whether the signal is still valid or the regime has flipped."
}

func circuitBreakerPositionPnL(pos *Position, price float64) float64 {
	if pos == nil {
		return 0
	}
	mult := pos.Multiplier
	if mult <= 0 {
		mult = 1
	}
	if pos.Side == "short" {
		return pos.Quantity * mult * (pos.AvgCost - price)
	}
	return pos.Quantity * mult * (price - pos.AvgCost)
}

func formatCBDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		h := int(d.Hours())
		m := int(d.Minutes()) % 60
		return fmt.Sprintf("%dh%dm", h, m)
	}
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	return fmt.Sprintf("%dd%dh", days, hours)
}

func formatCBMoney(v float64) string {
	return fmt.Sprintf("%.0f", math.Abs(v))
}

func formatCBSignedMoney(v float64) string {
	if v < 0 {
		return fmt.Sprintf("-$%s", formatCBMoney(v))
	}
	return fmt.Sprintf("$%s", formatCBMoney(v))
}

func formatCBNumber(v float64) string {
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.2f", v), "0"), ".")
}

func formatCBQty(v float64) string {
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.6f", v), "0"), ".")
}

func formatCBPrice(v float64) string {
	abs := math.Abs(v)
	switch {
	case abs >= 1000:
		return fmt.Sprintf("%.0f", v)
	case abs >= 1:
		return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.4f", v), "0"), ".")
	default:
		return fmt.Sprintf("%.6g", v)
	}
}

func truncateCircuitBreakerField(s string, max int) string {
	if len(s) <= max {
		return s
	}
	if max <= 3 {
		return s[:max]
	}
	return s[:max-3] + "..."
}
