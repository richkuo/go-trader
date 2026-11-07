package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"
)

const ordinaryAccountingGoldenPath = "testdata/ordinary_accounting/golden.json"

var ordinaryAccountingClock = time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)

var goldenOpenedStamp = regexp.MustCompile(` \[[A-Z][a-z]{2} \d{2} \d{2}:\d{2}\]`)

func goldenNum(v float64) string {
	return strconv.FormatFloat(v, 'g', 12, 64)
}

func goldenTrade(t Trade) map[string]interface{} {
	return map[string]interface{}{
		"strategy_id":        t.StrategyID,
		"symbol":             t.Symbol,
		"side":               t.Side,
		"quantity":           goldenNum(t.Quantity),
		"price":              goldenNum(t.Price),
		"value":              goldenNum(t.Value),
		"trade_type":         t.TradeType,
		"details":            t.Details,
		"exchange_order_id":  t.ExchangeOrderID,
		"exchange_fee":       goldenNum(t.ExchangeFee),
		"is_close":           t.IsClose,
		"realized_pnl":       goldenNum(t.RealizedPnL),
		"pnl_gross":          t.PnLGross,
		"fee_source":         t.FeeSource,
		"cost_model_version": t.CostModelVersion,
		"cost_policy_id":     t.CostPolicyID,
		"regime":             t.Regime,
	}
}

func goldenPosition(p *Position) map[string]interface{} {
	if p == nil {
		return nil
	}
	return map[string]interface{}{
		"symbol":             p.Symbol,
		"quantity":           goldenNum(p.Quantity),
		"initial_quantity":   goldenNum(p.InitialQuantity),
		"avg_cost":           goldenNum(p.AvgCost),
		"side":               p.Side,
		"multiplier":         goldenNum(p.Multiplier),
		"leverage":           goldenNum(p.Leverage),
		"scale_in_count":     p.ScaleInCount,
		"last_add_price":     goldenNum(p.LastAddPrice),
		"added_notional_usd": goldenNum(p.AddedNotionalUSD),
		"risk_anchor_price":  goldenNum(p.RiskAnchorPrice),
		"hedge_for":          p.HedgeFor,
	}
}

func goldenStrategyBook(s *StrategyState) map[string]interface{} {
	if s == nil {
		return nil
	}
	positions := map[string]interface{}{}
	for sym, p := range s.Positions {
		positions[sym] = goldenPosition(p)
	}
	trades := make([]string, 0, len(s.TradeHistory))
	for _, t := range s.TradeHistory {
		b, _ := json.Marshal(goldenTrade(t))
		trades = append(trades, string(b))
	}
	sort.Strings(trades)
	closed := make([]string, 0, len(s.ClosedPositions))
	for _, c := range s.ClosedPositions {
		b, _ := json.Marshal(map[string]interface{}{
			"symbol":       c.Symbol,
			"quantity":     goldenNum(c.Quantity),
			"avg_cost":     goldenNum(c.AvgCost),
			"side":         c.Side,
			"close_price":  goldenNum(c.ClosePrice),
			"realized_pnl": goldenNum(c.RealizedPnL),
			"close_reason": c.CloseReason,
		})
		closed = append(closed, string(b))
	}
	sort.Strings(closed)
	options := map[string]interface{}{}
	for id, o := range s.OptionPositions {
		options[id] = map[string]interface{}{
			"action":            o.Action,
			"quantity":          goldenNum(o.Quantity),
			"current_value_usd": goldenNum(o.CurrentValueUSD),
		}
	}
	return map[string]interface{}{
		"cash":               goldenNum(s.Cash),
		"positions":          positions,
		"options":            options,
		"trades":             trades,
		"closed":             closed,
		"peak_value":         goldenNum(s.RiskState.PeakValue),
		"current_drawdown":   goldenNum(s.RiskState.CurrentDrawdownPct),
		"daily_pnl":          goldenNum(s.RiskState.DailyPnL),
		"consecutive_losses": s.RiskState.ConsecutiveLosses,
		"circuit_breaker":    s.RiskState.CircuitBreaker,
	}
}

func goldenPortfolioRisk(prs *PortfolioRiskState) map[string]interface{} {
	if prs == nil {
		return nil
	}
	return map[string]interface{}{
		"peak_value":                   goldenNum(prs.PeakValue),
		"current_drawdown_pct":         goldenNum(prs.CurrentDrawdownPct),
		"current_margin_drawdown_pct":  goldenNum(prs.CurrentMarginDrawdownPct),
		"drawdown_reading_substituted": prs.DrawdownReadingSubstituted,
		"kill_switch_active":           prs.KillSwitchActive,
		"warning_sent":                 prs.WarningSent,
		"kill_switch_close_applied":    prs.KillSwitchCloseApplied,
		"events":                       len(prs.Events),
	}
}

func goldenScopeRisk(sr *scopeCycleRisk) map[string]interface{} {
	if sr == nil {
		return nil
	}
	return map[string]interface{}{
		"partition":                 sr.Partition.String(),
		"total_pv":                  goldenNum(sr.TotalPV),
		"total_notional":            goldenNum(sr.TotalNotional),
		"perps_loss":                goldenNum(sr.PerpsLoss),
		"perps_margin":              goldenNum(sr.PerpsMargin),
		"used_pv_fallback":          sr.UsedPVFallback,
		"equity_available":          sr.EquityAvailable,
		"equity_trusted":            sr.EquityTrusted,
		"equity_guard_armed":        sr.EquityGuardArmed,
		"peak_rebaseline_available": sr.PeakRebaselineAvailable,
		"kill_switch_fired":         sr.KillSwitchFired,
		"notional_blocked":          sr.NotionalBlocked,
		"daily_loss_entries_held":   sr.DailyLossEntriesHeld,
		"daily_loss_tripped":        sr.DailyLossStatus.Tripped,
		"exposure":                  goldenExposure(sr.ExposureCapStatus),
		"warning":                   sr.Warning,
		"warn_band_entered":         sr.WarnBandEntered,
		"reason":                    sr.Reason,
		"close_applied":             sr.CloseApplied,
	}
}

func goldenExposure(st ExposureCapStatus) map[string]interface{} {
	over := map[string]interface{}{}
	for k, v := range st.OverConcentrated {
		over[k] = map[string]interface{}{"direction": v.Direction, "pct": goldenNum(v.Pct), "net_usd": goldenNum(v.NetUSD)}
	}
	skipped := append([]string(nil), st.SkippedPositions...)
	sort.Strings(skipped)
	return map[string]interface{}{
		"configured":        st.Configured,
		"cap_usd":           goldenNum(st.CapUSD),
		"long_usd":          goldenNum(st.LongUSD),
		"short_usd":         goldenNum(st.ShortUSD),
		"long_blocked":      st.LongBlocked,
		"short_blocked":     st.ShortBlocked,
		"concentration_pct": goldenNum(st.ConcentrationPct),
		"portfolio_value":   goldenNum(st.PortfolioValue),
		"pv_basis_miss":     st.PVBasisMiss,
		"over_concentrated": over,
		"skipped":           skipped,
	}
}

func goldenAssets(assets map[string]*AssetExposure) map[string]interface{} {
	out := map[string]interface{}{}
	for k, a := range assets {
		strats := make([]string, 0, len(a.Strategies))
		for _, se := range a.Strategies {
			strats = append(strats, se.StrategyID+"|"+se.Type+"|"+goldenNum(se.DeltaUSD))
		}
		sort.Strings(strats)
		out[k] = map[string]interface{}{
			"net_delta_usd":     goldenNum(a.NetDeltaUSD),
			"gross_delta_usd":   goldenNum(a.GrossDeltaUSD),
			"concentration_pct": goldenNum(a.ConcentrationPct),
			"strategies":        strats,
		}
	}
	return out
}

type ordinaryGoldenFixture struct {
	cfg    *Config
	state  *AppState
	prices map[string]float64
}

func newOrdinaryGoldenFixture(t *testing.T) ordinaryGoldenFixture {
	t.Helper()
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xgolden")
	t.Setenv("OKX_API_KEY", "")
	t.Setenv("ROBINHOOD_USERNAME", "")
	t.Setenv("TOPSTEP_ACCOUNT_ID", "")
	marginCap := 200.0
	pr := &PortfolioRiskConfig{
		MaxDrawdownPct:              12,
		MaxNotionalUSD:              60000,
		WarnThresholdPct:            50,
		DailyMaxLossUSD:             500,
		MaxSameDirectionNotionalUSD: 30000,
		MaxAssetConcentrationPct:    40,
	}
	cfg := &Config{
		PortfolioRisk: pr,
		PaperSources: []PaperSourceConfig{
			{ID: "alt", DBFile: "alt.db", PortfolioRisk: &PortfolioRiskConfig{MaxDrawdownPct: 5, MaxNotionalUSD: 9000}},
		},
		Correlation: &CorrelationConfig{Enabled: true, MaxConcentrationPct: 50, MaxSameDirectionPct: 70},
		Strategies: []StrategyConfig{
			{ID: "hl-long", Platform: "hyperliquid", Type: "perps", Args: []string{"sma", "BTC", "1h", "--mode=paper"}, Capital: 5000, MaxDrawdownPct: 20, Leverage: 5},
			{ID: "hl-short", Platform: "hyperliquid", Type: "perps", Args: []string{"tema", "ETH", "1h", "--mode=paper"}, Capital: 4000, MaxDrawdownPct: 20, Leverage: 3},
			{ID: "hl-live-a", Platform: "hyperliquid", Type: "perps", Args: []string{"sma", "BTC", "1h", "--mode=live"}, Capital: 0, MaxDrawdownPct: 25, Leverage: 5, MarginPerTradeUSD: &marginCap, sharedWalletPoolBudget: true},
			{ID: "hl-live-b", Platform: "hyperliquid", Type: "perps", Args: []string{"tema", "SOL", "1h", "--mode=live"}, Capital: 0, MaxDrawdownPct: 25, Leverage: 10, MarginPerTradeUSD: &marginCap, sharedWalletPoolBudget: true},
			{ID: "hl-manual", Platform: "hyperliquid", Type: "manual", Args: []string{"hold", "ETH", "1h", "--mode=live"}, Leverage: 2},
			{ID: "okx-swap", Platform: "okx", Type: "perps", Args: []string{"sma", "BTC", "1h", "--mode=paper"}, Capital: 3000, MaxDrawdownPct: 15, Leverage: 4},
			{ID: "okx-spot", Platform: "okx", Type: "spot", Args: []string{"sma", "ETH", "1h", "--mode=paper"}, Capital: 2000, MaxDrawdownPct: 15},
			{ID: "rh-spot", Platform: "robinhood", Type: "spot", Args: []string{"sma", "DOGE", "1h", "--mode=paper"}, Capital: 1000, MaxDrawdownPct: 30, PaperSource: "alt"},
			{ID: "deribit-opt", Platform: "deribit", Type: "options", Args: []string{"vol", "BTC", "--mode=paper"}, Capital: 6000, MaxDrawdownPct: 40},
			{ID: "hl-hedged", Platform: "hyperliquid", Type: "perps", Args: []string{"sma", "SOL", "1h", "--mode=paper"}, Capital: 2500, MaxDrawdownPct: 20, Leverage: 2},
			{ID: "hl-noprice", Platform: "hyperliquid", Type: "perps", Args: []string{"sma", "XYZ", "1h", "--mode=paper"}, Capital: 1500, MaxDrawdownPct: 20, Leverage: 2, PaperSource: "alt"},
			{ID: "hl-corrupt", Platform: "hyperliquid", Type: "perps", Args: []string{"sma", "AVAX", "1h", "--mode=paper"}, Capital: 1200, MaxDrawdownPct: 20, Leverage: 2},
		},
	}
	state := NewAppState()
	for _, sc := range cfg.Strategies {
		state.Strategies[sc.ID] = NewStrategyState(sc)
	}
	opened := ordinaryAccountingClock.Add(-6 * time.Hour)
	set := func(id string, cash float64, peak float64) *StrategyState {
		s := state.Strategies[id]
		s.Cash = cash
		s.RiskState.PeakValue = peak
		return s
	}
	set("hl-long", 4100, 5600).Positions["BTC"] = &Position{Symbol: "BTC", Quantity: 0.05, InitialQuantity: 0.05, AvgCost: 60000, Side: "long", Multiplier: 1, Leverage: 5, OpenedAt: opened, StopLossTriggerPx: 59000}
	set("hl-short", 3800, 4300).Positions["ETH"] = &Position{Symbol: "ETH", Quantity: 1.5, InitialQuantity: 1.5, AvgCost: 3200, Side: "short", Multiplier: 1, Leverage: 3, OpenedAt: opened, StopLossTriggerPx: 3300}
	set("hl-live-a", 0, 0).Positions["BTC"] = &Position{Symbol: "BTC", Quantity: 0.02, InitialQuantity: 0.02, AvgCost: 61000, Side: "long", Multiplier: 1, Leverage: 5, OpenedAt: opened}
	set("hl-live-b", 0, 0).Positions["SOL"] = &Position{Symbol: "SOL", Quantity: 10, InitialQuantity: 10, AvgCost: 150, Side: "short", Multiplier: 1, Leverage: 10, OpenedAt: opened}
	set("hl-manual", 0, 0).Positions["ETH"] = &Position{Symbol: "ETH", Quantity: 0.4, InitialQuantity: 0.4, AvgCost: 3000, Side: "long", Multiplier: 1, Leverage: 2, OpenedAt: opened}
	set("okx-swap", 2500, 3100).Positions["BTC"] = &Position{Symbol: "BTC", Quantity: 0.03, InitialQuantity: 0.03, AvgCost: 63000, Side: "short", Multiplier: 1, Leverage: 4, OpenedAt: opened, StopLossTriggerPx: 64000}
	set("okx-spot", 500, 2100).Positions["ETH"] = &Position{Symbol: "ETH", Quantity: 0.5, InitialQuantity: 0.5, AvgCost: 3000, Side: "long", OpenedAt: opened}
	set("rh-spot", 400, 1050).Positions["DOGE"] = &Position{Symbol: "DOGE", Quantity: 4000, InitialQuantity: 4000, AvgCost: 0.15, Side: "long", OpenedAt: opened}
	opt := set("deribit-opt", 5200, 6400)
	opt.OptionPositions["BTC-C-70000"] = &OptionPosition{ID: "BTC-C-70000", Underlying: "BTC", OptionType: "call", Strike: 70000, Expiry: "2026-04-01", DTE: 28, Action: "buy", Quantity: 1, EntryPremiumUSD: 900, CurrentValueUSD: 750, Greeks: OptGreeks{Delta: 0.35}, OpenedAt: opened}
	opt.OptionPositions["BTC-P-55000"] = &OptionPosition{ID: "BTC-P-55000", Underlying: "BTC", OptionType: "put", Strike: 55000, Expiry: "2026-04-01", DTE: 28, Action: "sell", Quantity: 1, EntryPremiumUSD: 600, CurrentValueUSD: -700, Greeks: OptGreeks{Delta: 0.2}, OpenedAt: opened}
	hedged := set("hl-hedged", 1800, 2600)
	hedged.Positions["SOL"] = &Position{Symbol: "SOL", Quantity: 4, InitialQuantity: 4, AvgCost: 140, Side: "long", Multiplier: 1, Leverage: 2, OpenedAt: opened}
	hedged.Positions["BTC"] = &Position{Symbol: "BTC", Quantity: 0.01, InitialQuantity: 0.01, AvgCost: 62500, Side: "short", Multiplier: 1, Leverage: 2, OpenedAt: opened, HedgeFor: "hl-hedged"}
	set("hl-noprice", 1200, 1500).Positions["XYZ"] = &Position{Symbol: "XYZ", Quantity: 30, InitialQuantity: 30, AvgCost: 10, Side: "long", Multiplier: 1, Leverage: 2, OpenedAt: opened}
	set("hl-corrupt", 1100, 1250).Positions["AVAX"] = &Position{Symbol: "AVAX", Quantity: 0, AvgCost: 0, Side: "long", Multiplier: 1, Leverage: 2, OpenedAt: opened}
	prices := map[string]float64{
		"BTC":  62000,
		"ETH":  3100,
		"SOL":  145,
		"DOGE": 0.12,
		"AVAX": 30,
	}
	return ordinaryGoldenFixture{cfg: cfg, state: state, prices: prices}
}

func goldenPartitionStates(f ordinaryGoldenFixture) map[RiskPartition]map[string]*StrategyState {
	out := map[RiskPartition]map[string]*StrategyState{}
	for _, part := range activePartitions(f.cfg.Strategies) {
		out[part] = filterStatesByPartition(f.state.Strategies, f.cfg.Strategies, part)
	}
	return out
}

func goldenValuations(t *testing.T, f ordinaryGoldenFixture) map[string]interface{} {
	out := map[string]interface{}{}
	ids := make([]string, 0, len(f.state.Strategies))
	for id := range f.state.Strategies {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	pv := map[string]interface{}{}
	display := map[string]interface{}{}
	collected := map[string]interface{}{}
	warn := map[string]interface{}{}
	risk := map[string]interface{}{}
	logger := &StrategyLogger{stratID: "golden", writer: io.Discard}
	cfgByID := map[string]StrategyConfig{}
	for _, sc := range f.cfg.Strategies {
		cfgByID[sc.ID] = sc
	}
	for _, id := range ids {
		s := f.state.Strategies[id]
		pv[id] = goldenNum(PortfolioValue(s, f.prices))
		display[id] = goldenNum(displayStrategyValue(s, f.prices))
		lines := collectPositions(cfgByID[id], s, f.prices)
		for i, line := range lines {
			lines[i] = goldenOpenedStamp.ReplaceAllString(line, "")
		}
		sort.Strings(lines)
		collected[id] = lines
		warn[id] = formatPortfolioWarningPosition(s, f.prices)
		sc := cfgByID[id]
		probe := cloneStrategyStateForGolden(s)
		ok, reason := CheckRisk(&sc, probe, PortfolioValue(probe, f.prices), f.prices, logger, nil)
		risk[id] = map[string]interface{}{"allowed": ok, "reason": reason, "book": goldenStrategyBook(probe)}
	}
	out["portfolio_value"] = pv
	out["display_strategy_value"] = display
	out["collect_positions"] = collected
	out["portfolio_warning_position"] = warn
	out["check_risk"] = risk

	shared := detectSharedWallets(f.cfg.Strategies)
	walletKey := SharedWalletKey{Platform: "hyperliquid", Account: "0xgolden"}
	balances := map[SharedWalletKey]float64{walletKey: 2500}
	subset := map[string]interface{}{}
	for _, part := range activePartitions(f.cfg.Strategies) {
		cfgs := strategiesInPartition(f.cfg.Strategies, part)
		v, fb := computeSubsetPortfolioValue(cfgs, f.state, f.prices, balances, shared)
		dv, dfb := computeSubsetDisplayValue(cfgs, f.state, f.prices, balances, shared)
		nv, nfb := computeSubsetPortfolioValue(cfgs, f.state, f.prices, nil, shared)
		subset[part.String()] = map[string]interface{}{
			"portfolio":          goldenNum(v),
			"portfolio_fallback": fb,
			"display":            goldenNum(dv),
			"display_fallback":   dfb,
			"no_balance":         goldenNum(nv),
			"no_balance_fallbk":  nfb,
			"latest_display":     goldenNum(latestDisplayTotalForPartition(f.state, f.cfg.Strategies, part, f.prices)),
		}
	}
	out["subset"] = subset
	out["latest_display_total"] = goldenNum(latestDisplayTotal(f.state, f.prices))
	pool := map[string]interface{}{}
	for _, id := range []string{"hl-live-a", "hl-live-b", "hl-long"} {
		avail, pooled, known := sharedWalletPoolAvailableMargin(cfgByID[id], f.cfg.Strategies, f.state, f.prices, shared, balances)
		pool[id] = map[string]interface{}{"available": goldenNum(avail), "pooled": pooled, "known": known}
	}
	out["shared_wallet_pool"] = pool

	parts := goldenPartitionStates(f)
	exposure := map[string]interface{}{}
	corr := map[string]interface{}{}
	notional := map[string]interface{}{}
	for _, part := range activePartitions(f.cfg.Strategies) {
		cfgs := strategiesInPartition(f.cfg.Strategies, part)
		states := parts[part]
		prCfg := partitionRiskConfig(f.cfg, part)
		pvTotal, _ := computeSubsetPortfolioValue(cfgs, f.state, f.prices, balances, shared)
		exposure[part.String()] = goldenExposure(evaluateExposureCap(prCfg, states, cfgs, f.prices, pvTotal))
		assets, skipped := computeAssetDeltas(states, cfgs, f.prices)
		sort.Strings(skipped)
		snap := ComputeCorrelation(states, cfgs, f.prices, f.cfg.Correlation)
		var snapOut map[string]interface{}
		if snap != nil {
			warnings := append([]string(nil), snap.Warnings...)
			sort.Strings(warnings)
			snapOut = map[string]interface{}{
				"assets":              goldenAssets(snap.Assets),
				"portfolio_gross_usd": goldenNum(snap.PortfolioGrossUSD),
				"warnings":            warnings,
			}
		}
		corr[part.String()] = map[string]interface{}{
			"deltas":      goldenAssets(assets),
			"skipped":     skipped,
			"correlation": snapOut,
		}
		held, detail := evaluateNotionalCapHold(prCfg, states, f.prices)
		notional[part.String()] = map[string]interface{}{"held": held, "detail": detail}
	}
	out["exposure_cap"] = exposure
	out["correlation"] = corr
	out["notional_cap"] = notional
	return out
}

func cloneStrategyStateForGolden(s *StrategyState) *StrategyState {
	c := *s
	c.Positions = map[string]*Position{}
	for k, p := range s.Positions {
		pc := *p
		c.Positions[k] = &pc
	}
	c.OptionPositions = map[string]*OptionPosition{}
	for k, o := range s.OptionPositions {
		oc := *o
		c.OptionPositions[k] = &oc
	}
	c.TradeHistory = append([]Trade(nil), s.TradeHistory...)
	c.ClosedPositions = append([]ClosedPosition(nil), s.ClosedPositions...)
	return &c
}

func goldenScopeCycles(t *testing.T, f ordinaryGoldenFixture) map[string]interface{} {
	out := map[string]interface{}{}
	shared := detectSharedWallets(f.cfg.Strategies)
	walletKey := SharedWalletKey{Platform: "hyperliquid", Account: "0xgolden"}
	balances := map[SharedWalletKey]float64{walletKey: 2500}
	for _, scenario := range []struct {
		name   string
		prices map[string]float64
		stale  bool
		peaks  [3]float64
	}{
		{name: "steady", prices: f.prices, peaks: [3]float64{2600, 22000, 2100}},
		{name: "drawdown", prices: map[string]float64{"BTC": 52000, "ETH": 3600, "SOL": 170, "DOGE": 0.09, "AVAX": 30}, peaks: [3]float64{4200, 31000, 2700}},
		{name: "stale_balance", prices: f.prices, stale: true, peaks: [3]float64{2900, 22000, 2100}},
		{name: "warn_band", prices: map[string]float64{"BTC": 58500, "ETH": 3300, "SOL": 152, "DOGE": 0.11, "AVAX": 30}, peaks: [3]float64{2750, 22800, 2120}},
	} {
		fx := newOrdinaryGoldenFixture(t)
		fx.state.PortfolioRisk[livePartition] = &PortfolioRiskState{PeakValue: scenario.peaks[0]}
		fx.state.PortfolioRisk[defaultPaperPartition] = &PortfolioRiskState{PeakValue: scenario.peaks[1]}
		fx.state.PortfolioRisk[paperSourcePartition("alt")] = &PortfolioRiskState{PeakValue: scenario.peaks[2]}
		res := map[string]interface{}{}
		for _, part := range activePartitions(fx.cfg.Strategies) {
			prCfg := partitionRiskConfig(fx.cfg, part)
			sr := measureScopeCycleRisk(part, prCfg, fx.cfg.Strategies, fx.state, scenario.prices, balances, shared, true, scenario.stale, ordinaryAccountingClock)
			prs := fx.state.partitionRisk(part)
			applyScopeCycleRisk(sr, prs)
			entry := map[string]interface{}{
				"scope":          goldenScopeRisk(sr),
				"portfolio_risk": goldenPortfolioRisk(prs),
			}
			if !part.IsLive() {
				for _, owner := range []bool{true, false} {
					fy := newOrdinaryGoldenFixture(t)
					for p, src := range fx.state.PortfolioRisk {
						cp := *src
						cp.Events = append([]KillSwitchEvent(nil), src.Events...)
						fy.state.PortfolioRisk[p] = &cp
					}
					sry := measureScopeCycleRisk(part, prCfg, fy.cfg.Strategies, fy.state, scenario.prices, balances, shared, true, scenario.stale, ordinaryAccountingClock)
					applyScopeCycleRisk(sry, fy.state.partitionRisk(part))
					ks := applyPaperKillSwitchCycle(fy.state, fy.cfg, scenario.prices, sry, owner)
					closed := append([]string(nil), ks.Closed...)
					sort.Strings(closed)
					books := map[string]interface{}{}
					for id, s := range filterStatesByPartition(fy.state.Strategies, fy.cfg.Strategies, part) {
						books[id] = goldenStrategyBook(s)
					}
					entry["paper_kill_switch_owner_"+strconv.FormatBool(owner)] = map[string]interface{}{
						"closed":         closed,
						"close_applied":  ks.CloseApplied,
						"auto_reset":     ks.AutoReset,
						"message":        ks.Message,
						"portfolio_risk": goldenPortfolioRisk(fy.state.partitionRisk(part)),
						"books":          books,
					}
				}
			}
			res[part.String()] = entry
		}
		out[scenario.name] = res
	}
	return out
}

func goldenBookings(t *testing.T) map[string]interface{} {
	out := map[string]interface{}{}
	logger := &StrategyLogger{stratID: "golden", writer: io.Discard}
	run := func(name string, sc StrategyConfig, setup func(s *StrategyState), step func(s *StrategyState) interface{}) {
		s := NewStrategyState(sc)
		if setup != nil {
			setup(s)
		}
		res := step(s)
		out[name] = map[string]interface{}{"result": res, "book": goldenStrategyBook(s)}
	}
	hlPaper := StrategyConfig{ID: "hl-book", Platform: "hyperliquid", Type: "perps", Args: []string{"sma", "BTC", "1h", "--mode=paper"}, Capital: 10000, MaxDrawdownPct: 20, Leverage: 5}
	sizing := PerpsSizing{SizingLeverage: 2, ExchangeLeverage: 5}
	for _, venue := range []struct {
		name string
		sc   StrategyConfig
	}{{"hl", hlPaper}} {
		venue := venue
		run(venue.name+"_open_add_close_flip", venue.sc, nil, func(s *StrategyState) interface{} {
			var steps []interface{}
			record := func(label string, n int, err error) {
				e := ""
				if err != nil {
					e = err.Error()
				}
				steps = append(steps, map[string]interface{}{"label": label, "trades": n, "err": e, "cash": goldenNum(s.Cash), "pos": goldenPosition(s.Positions["BTC"])})
			}
			n, err := ExecutePerpsSignalWithLeverage(s, 1, "BTC", 60000, sizing, 0, "", 0, "long", 0, logger)
			record("open_long", n, err)
			n, err = ExecutePerpsSignalWithLeverage(s, 1, "BTC", 61000, sizing, 0.01, "oid-add", 0.4, "long", 0, logger)
			record("add_long_fill", n, err)
			n, err = ExecutePerpsSignalWithLeverage(s, -1, "BTC", 62000, sizing, 0, "", 0, "long", 0.5, logger)
			record("partial_close", n, err)
			n, err = ExecutePerpsSignalWithLeverage(s, -1, "BTC", 63000, sizing, 0, "", 0, "both", 0, logger)
			record("flip_short", n, err)
			n, err = ExecutePerpsSignalWithLeverage(s, 1, "BTC", 61500, sizing, 0, "", 0, "short", 0, logger)
			record("close_short", n, err)
			return steps
		})
		run(venue.name+"_book_close", venue.sc, func(s *StrategyState) {
			s.Positions["BTC"] = &Position{Symbol: "BTC", Quantity: 0.1, InitialQuantity: 0.1, AvgCost: 60000, Side: "short", Multiplier: 1, Leverage: 5}
			s.Positions["AVAX"] = &Position{Symbol: "AVAX", Quantity: 0, AvgCost: 0, Side: "long", Multiplier: 1}
		}, func(s *StrategyState) interface{} {
			a := bookPerpsClose(s, "BTC", 58000, "manual_close", "Manual close", "golden close", logger)
			b := bookPerpsClose(s, "AVAX", 30, "manual_close", "Manual close", "golden close", logger)
			return []bool{a, b}
		})
		run(venue.name+"_paper_stop", venue.sc, func(s *StrategyState) {
			s.Positions["BTC"] = &Position{Symbol: "BTC", Quantity: 0.1, InitialQuantity: 0.1, AvgCost: 60000, Side: "long", Multiplier: 1, Leverage: 5, StopLossTriggerPx: 59000}
		}, func(s *StrategyState) interface{} {
			var mu sync.RWMutex
			n, reason := applyPaperStopLossBreach(venue.sc, s, "BTC", "long", 58800, &mu, logger)
			return map[string]interface{}{"trades": n, "reason": reason}
		})
		run(venue.name+"_scale_in", venue.sc, func(s *StrategyState) {
			s.Positions["BTC"] = &Position{Symbol: "BTC", Quantity: 0.1, InitialQuantity: 0.1, AvgCost: 60000, Side: "long", Multiplier: 1, Leverage: 5, RiskAnchorPrice: 60000}
		}, func(s *StrategyState) interface{} {
			n, trade := applyPerpsScaleIn(s, venue.sc, "BTC", 61000, 0.05, 0, "", false, logger)
			n2, trade2 := applyPerpsScaleIn(s, venue.sc, "BTC", 61500, 0.02, 0.9, "oid-scale", true, logger)
			res := map[string]interface{}{"trades": n, "trades_fill": n2}
			if trade != nil {
				res["trade"] = goldenTrade(*trade)
			}
			if trade2 != nil {
				res["trade_fill"] = goldenTrade(*trade2)
			}
			return res
		})
	}
	run("hl_circuit_close_fill", hlPaper, func(s *StrategyState) {
		s.Positions["BTC"] = &Position{Symbol: "BTC", Quantity: 0.2, InitialQuantity: 0.2, AvgCost: 60000, Side: "long", Multiplier: 1, Leverage: 5}
	}, func(s *StrategyState) interface{} {
		applyHyperliquidCircuitCloseFill(s, "BTC", 0.08, 59500, 1.25, 0.12, 4242, "")
		applyHyperliquidCircuitCloseFill(s, "BTC", 0.12, 59400, 1.5, 0, 4243, "kill_switch")
		return nil
	})
	run("kill_switch_close_paper", hlPaper, func(s *StrategyState) {
		s.Positions["BTC"] = &Position{Symbol: "BTC", Quantity: 0.2, InitialQuantity: 0.2, AvgCost: 62000, Side: "short", Multiplier: 1, Leverage: 5}
		s.Positions["ETH"] = &Position{Symbol: "ETH", Quantity: 1, InitialQuantity: 1, AvgCost: 2800, Side: "long", Multiplier: 1, Leverage: 5}
	}, func(s *StrategyState) interface{} {
		forceCloseKillSwitchPositions(s, hlPaper, map[string]float64{"BTC": 61000, "ETH": 2900}, nil, nil, hlVirtualQuantitySnapshot{}, nil, logger)
		return nil
	})
	futures := StrategyConfig{ID: "ts-book", Platform: "topstep", Type: "futures", Args: []string{"sma", "ES", "1h", "--mode=paper"}, Capital: 20000, MaxDrawdownPct: 20}
	run("futures_open_close", futures, nil, func(s *StrategyState) interface{} {
		spec := ContractSpec{Multiplier: 50, TickSize: 0.25, TickValue: 12.5, Margin: 1500}
		n1, err1 := ExecuteFuturesSignalWithFillFee(s, 1, "ES", 5000, spec, 2.5, 3, 0, 0, "", 0, logger)
		n2, err2 := ExecuteFuturesSignalWithFillFee(s, -1, "ES", 5010, spec, 2.5, 3, 0, 0, "", 0, logger)
		return []interface{}{n1, err1 == nil, n2, err2 == nil}
	})
	return out
}

func TestOrdinaryAccountingGolden(t *testing.T) {
	if err := bindTradingClock(t.Name(), func() time.Time { return ordinaryAccountingClock }); err != nil {
		t.Fatalf("bind trading clock: %v", err)
	}
	t.Cleanup(func() { unbindTradingClock(t.Name()) })
	restore := suspendEagerTradePersist()
	t.Cleanup(restore)

	got := map[string]interface{}{
		"valuations":  goldenValuations(t, newOrdinaryGoldenFixture(t)),
		"scope_cycle": goldenScopeCycles(t, newOrdinaryGoldenFixture(t)),
		"bookings":    goldenBookings(t),
	}
	gotBytes, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatalf("marshal golden: %v", err)
	}
	gotBytes = append(gotBytes, '\n')
	path := filepath.FromSlash(ordinaryAccountingGoldenPath)
	want, err := os.ReadFile(path)
	if os.IsNotExist(err) && os.Getenv("ORDINARY_ACCOUNTING_CAPTURE_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, gotBytes, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("captured %s", path)
		return
	}
	if err != nil {
		t.Fatalf("read golden %s: %v", path, err)
	}
	if !bytes.Equal(want, gotBytes) {
		scratch := filepath.Join(t.TempDir(), "got.json")
		_ = os.WriteFile(scratch, gotBytes, 0o644)
		t.Fatalf("ordinary accounting output differs from %s; got written to %s", path, scratch)
	}
}
