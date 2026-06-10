package main

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

const (
	portfolioWarningRecentWindow = 15 * time.Minute
	portfolioWarningMaxRows      = 5
	portfolioWarningMaxChars     = 1900
)

type PortfolioWarningMessageInputs struct {
	Reason      string
	Config      *PortfolioRiskConfig
	State       *AppState
	Prices      map[string]float64
	TotalValue  float64
	PerpsLoss   float64
	PerpsMargin float64
	Recent      []Trade
	Now         time.Time
}

type portfolioWarningContributor struct {
	ID             string
	PnLLabel       string
	PnL            float64
	DrawdownPct    float64
	PositionLine   string
	NegativeWeight float64
}

// BuildPortfolioWarningMessage expands the single-line portfolio risk reason
// into the operator triage block used by Discord warnings.
func BuildPortfolioWarningMessage(in PortfolioWarningMessageInputs) string {
	now := in.Now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	var prs PortfolioRiskState
	if in.State != nil {
		prs = in.State.PortfolioRisk
	}
	contribs := portfolioWarningContributors(in.State, in.Prices)

	var b strings.Builder
	b.WriteString("**PORTFOLIO WARNING**")
	if lead := portfolioWarningLead(contribs); lead != "" {
		b.WriteString(" - ")
		b.WriteString(lead)
	}
	b.WriteByte('\n')

	maxDD := 0.0
	warnDD := 0.0
	if in.Config != nil {
		maxDD = in.Config.MaxDrawdownPct
		warnDD = maxDD * in.Config.WarnThresholdPct / 100
	}
	entered := prs.WarnBandEnteredAt.UTC()
	if entered.IsZero() {
		entered = now
	}
	b.WriteString(fmt.Sprintf("Kill switch: %.1f%% drawdown | Warn threshold: %.1f%% | In band since: %s (%s)\n",
		maxDD, warnDD, entered.Format("2006-01-02 15:04 UTC"), formatWarningDuration(now.Sub(entered))))

	b.WriteString(fmt.Sprintf("Current: equity=%.1f%% ($%.0f / peak $%.0f)", prs.CurrentDrawdownPct, in.TotalValue, prs.PeakValue))
	if in.PerpsMargin > 0 {
		b.WriteString(fmt.Sprintf(" | perps margin=%.1f%% ($%.0f loss on $%.0f margin)", prs.CurrentMarginDrawdownPct, in.PerpsLoss, in.PerpsMargin))
	}
	b.WriteByte('\n')

	b.WriteString(fmt.Sprintf("Distance to kill switch: %.1f%% equity", positiveDistance(maxDD, prs.CurrentDrawdownPct)))
	if in.PerpsMargin > 0 {
		b.WriteString(fmt.Sprintf(" / %.1f%% margin", positiveDistance(maxDD, prs.CurrentMarginDrawdownPct)))
	}
	b.WriteByte('\n')
	b.WriteString(formatPortfolioWarningTrend(prs, in.PerpsMargin > 0))
	b.WriteByte('\n')

	if len(contribs) > 0 {
		b.WriteString("\nTop contributors:\n")
		b.WriteString("```\n")
		for _, c := range contribs {
			b.WriteString(fmt.Sprintf("%-20s %-9s %s  dd %.1f%%  %s\n",
				truncateWarningField(c.ID, 20), c.PnLLabel, formatSignedDollar(c.PnL), c.DrawdownPct, c.PositionLine))
		}
		b.WriteString("```\n")
	}

	if len(in.Recent) > 0 {
		b.WriteString("\nRecent activity (last 15m):\n")
		b.WriteString("```\n")
		for _, tr := range in.Recent {
			b.WriteString(formatPortfolioWarningTrade(tr))
			b.WriteByte('\n')
		}
		b.WriteString("```\n")
	}

	if rec := portfolioWarningRecommendation(contribs); rec != "" {
		b.WriteString("\nRecommended: ")
		b.WriteString(rec)
		b.WriteByte('\n')
	}

	msg := strings.TrimRight(b.String(), "\n")
	return truncateWarningField(msg, portfolioWarningMaxChars)
}

func portfolioWarningContributors(state *AppState, prices map[string]float64) []portfolioWarningContributor {
	if state == nil {
		return nil
	}
	totalNegative := 0.0
	out := make([]portfolioWarningContributor, 0, len(state.Strategies))
	ids := make([]string, 0, len(state.Strategies))
	for id := range state.Strategies {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		ss := state.Strategies[id]
		if ss == nil {
			continue
		}
		pv := PortfolioValue(ss, prices)
		initCap := ss.InitialCapital
		pnlLabel := "P&L"
		if initCap <= 0 {
			initCap = pv - ss.RiskState.DailyPnL
			pnlLabel = "daily P&L"
		}
		pnl := pv - initCap
		if pnl < 0 {
			totalNegative += -pnl
		}
		out = append(out, portfolioWarningContributor{
			ID:           id,
			PnLLabel:     pnlLabel,
			PnL:          pnl,
			DrawdownPct:  ss.RiskState.CurrentDrawdownPct,
			PositionLine: formatPortfolioWarningPosition(ss, prices),
		})
	}
	if totalNegative > 0 {
		for i := range out {
			if out[i].PnL < 0 {
				out[i].NegativeWeight = -out[i].PnL / totalNegative
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].PnL == out[j].PnL {
			return out[i].ID < out[j].ID
		}
		return out[i].PnL < out[j].PnL
	})
	if len(out) > portfolioWarningMaxRows {
		out = out[:portfolioWarningMaxRows]
	}
	return out
}

func portfolioWarningLead(contribs []portfolioWarningContributor) string {
	if len(contribs) == 0 || contribs[0].PnL >= 0 {
		return "portfolio is in the warn band"
	}
	return fmt.Sprintf("%s (dd=%.1f%%) is leading portfolio drawdown", contribs[0].ID, contribs[0].DrawdownPct)
}

func formatPortfolioWarningTrend(prs PortfolioRiskState, includeMargin bool) string {
	eq := prs.WarningEquityDeltaPct
	margin := prs.WarningMarginDeltaPct
	trend := "STABLE"
	primary := eq
	if includeMargin && math.Abs(margin) >= math.Abs(eq) {
		primary = margin
	}
	if primary > 0.05 {
		trend = "WORSENING"
	} else if primary < -0.05 {
		trend = "RECOVERING"
	}
	parts := []string{fmt.Sprintf("equity dd %s since last cycle", formatSignedPct(eq))}
	if includeMargin {
		parts = append(parts, fmt.Sprintf("margin dd %s", formatSignedPct(margin)))
	}
	return "Trend: " + trend + " - " + strings.Join(parts, "; ")
}

func formatPortfolioWarningPosition(ss *StrategyState, prices map[string]float64) string {
	if ss == nil {
		return "(flat)"
	}
	symbols := make([]string, 0, len(ss.Positions))
	for sym, pos := range ss.Positions {
		if pos != nil && pos.Quantity > 0 {
			symbols = append(symbols, sym)
		}
	}
	sort.Strings(symbols)
	if len(symbols) == 0 {
		return "(flat)"
	}
	sym := symbols[0]
	pos := ss.Positions[sym]
	price := prices[sym]
	if price <= 0 {
		price = pos.AvgCost
	}
	pnl := positionUnrealizedPnL(pos, price)
	line := fmt.Sprintf("pos: %s %s %s @ $%s (%s unrealized)", pos.Side, formatWarningQty(pos.Quantity), sym, formatWarningPrice(pos.AvgCost), formatSignedDollar(pnl))
	if len(symbols) > 1 {
		line += fmt.Sprintf(" +%d more", len(symbols)-1)
	}
	return line
}

func positionUnrealizedPnL(pos *Position, price float64) float64 {
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

func formatPortfolioWarningTrade(tr Trade) string {
	kind := "fill"
	if tr.IsClose {
		kind = "close"
	} else if tr.Manual {
		kind = "manual"
	} else if tr.TradeType != "" {
		kind = tr.TradeType
	}
	details := strings.TrimSpace(tr.Details)
	if details != "" {
		details = " (" + truncateWarningField(details, 42) + ")"
	}
	return fmt.Sprintf("%s  %s  %s  %s %s @ $%s%s",
		tr.Timestamp.UTC().Format("15:04"), kind, truncateWarningField(tr.StrategyID, 20), tr.Side, formatWarningQty(tr.Quantity), formatWarningPrice(tr.Price), details)
}

func portfolioWarningRecommendation(contribs []portfolioWarningContributor) string {
	if len(contribs) == 0 {
		return "review portfolio exposure and recent fills before adding risk."
	}
	if contribs[0].PnL < 0 && contribs[0].NegativeWeight >= 0.5 {
		return fmt.Sprintf("review open positions above; consider manually closing %s if the signal does not recover next cycle.", contribs[0].ID)
	}
	return "review open positions above and avoid adding risk until the drawdown recovers."
}

func positiveDistance(limit, current float64) float64 {
	d := limit - current
	if d < 0 {
		return 0
	}
	return d
}

func formatWarningDuration(d time.Duration) string {
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

func formatSignedDollar(v float64) string {
	if v < 0 {
		return fmt.Sprintf("-$%.0f", math.Abs(v))
	}
	return fmt.Sprintf("$%.0f", v)
}

func formatSignedPct(v float64) string {
	if v > 0 {
		return fmt.Sprintf("+%.1f%%", v)
	}
	if v < 0 {
		return fmt.Sprintf("%.1f%%", v)
	}
	return "+0.0%"
}

func formatWarningQty(v float64) string {
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.6f", v), "0"), ".")
}

func formatWarningPrice(v float64) string {
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

func truncateWarningField(s string, max int) string {
	if max <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	if max <= 3 {
		return string(runes[:max])
	}
	return string(runes[:max-3]) + "..."
}
