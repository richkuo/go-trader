package main

import (
	"bytes"
	"crypto/sha256"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func ocSC(id, platform, underlying string) StrategyConfig {
	return StrategyConfig{ID: id, Type: "options", Platform: platform, Script: "shared_scripts/check_options.py",
		Args: []string{"wheel", underlying, "--platform=" + platform, "--mode=paper"}, Capital: 100000, MaxDrawdownPct: 50}
}

func ocLogger(id string) (*StrategyLogger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	return &StrategyLogger{stratID: id, writer: buf}, buf
}

func ocResult(platform, underlying string, spot float64, actions ...OptionsAction) *OptionsResult {
	return &OptionsResult{Underlying: underlying, Platform: platform, PremiumUnit: optionPremiumUnitUnderlying,
		Signal: -1, SpotPrice: spot, Actions: actions}
}

func ocNear(a, b float64) bool {
	return math.Abs(a-b) <= 1e-9*math.Max(1, math.Max(math.Abs(a), math.Abs(b)))
}

func ocStoredTrades(t *testing.T, env *entryTestEnv, id string) []qocTradeRow {
	t.Helper()
	rows, err := env.fileFor(id).db.Query(`SELECT side, quantity, price, value, trade_type FROM trades WHERE strategy_id = ? ORDER BY rowid`, id)
	if err != nil {
		t.Fatalf("query trades: %v", err)
	}
	defer rows.Close()
	var out []qocTradeRow
	for rows.Next() {
		var r qocTradeRow
		if err := rows.Scan(&r.side, &r.qty, &r.price, &r.value, &r.tradeType); err != nil {
			t.Fatalf("scan trade: %v", err)
		}
		out = append(out, r)
	}
	return out
}

type qocTradeRow struct {
	side      string
	qty       float64
	price     float64
	value     float64
	tradeType string
}

func ocEnv(t *testing.T, scs ...StrategyConfig) *entryTestEnv {
	t.Helper()
	env := openEntryTestEnv(t, newEntryTestConfig(t, scs...))
	useEntryTradeRecorder(t, env.store)
	return env
}

type ocPricer struct {
	platform   string
	mark, spot float64
	expirySpot float64
}

func (p ocPricer) Refresh(reqs []optionLegRequest, refreshAt time.Time) []optionLegReply {
	var out []optionLegReply
	for _, req := range reqs {
		r := optionLegReply{Key: req.Key}
		cutoff, ready, ok, _ := optionGoExpirySchedule(p.platform, req.Expiry)
		if !ok {
			day, _ := time.Parse("2006-01-02", req.Expiry)
			cutoff, ready = day.Add(21*time.Hour), day.Add(22*time.Hour)
		}
		r.CutoffAt, r.ReadyAt = cutoff, ready
		if refreshAt.Before(cutoff) {
			r.Mark = &optionLegMark{PerUnitUSD: p.mark * p.spot, Spot: p.spot, Greeks: OptGreeks{Delta: 0.3}, Source: "contract-test"}
		} else {
			basis := optionSettlementBasisDelivery
			switch p.platform {
			case "robinhood":
				basis = optionSettlementBasisRawClose
			case "ibkr":
				basis = optionSettlementBasisIBKR
			}
			r.Settlement = &optionLegSettlement{Price: p.expirySpot, Basis: basis, Date: req.Expiry, Source: "contract-test"}
		}
		out = append(out, r)
	}
	return out
}

func (p ocPricer) Name() string { return "contract-test" }

func ocMarkCycle(s *StrategyState, pricer ocPricer, logger *StrategyLogger) {
	pricer.platform = s.Platform
	now := time.Now().UTC()
	reqs := collectOptionLegRequests(s, s.Platform, now)
	applyOptionLegReplies(s, s.Platform, reqs, refreshOptionLegs(pricer, reqs, now), now, logger)
}

func ocExpiredLeg(id, optionType, action string, strike, qty float64) *OptionPosition {
	return &OptionPosition{ID: id, Underlying: "SPY", OptionType: optionType, Strike: strike, Expiry: "2020-01-17",
		Action: action, Quantity: qty, EntryPremiumUSD: 150, CurrentValueUSD: -150, OpenedAt: time.Unix(100, 0).UTC()}
}

func TestOptionContractSizeIntegration(t *testing.T) {
	t.Run("robinhood_put_collateral_is_reserved", func(t *testing.T) {
		sc := ocSC("rh-opt-put", "robinhood", "SPY")
		env := ocEnv(t, sc)
		s := env.strat(sc.ID)
		s.Cash = 47000
		logger, buf := ocLogger(sc.ID)
		n, err := ExecuteOptionsSignal(s, ocResult("robinhood", "SPY", 480, OptionsAction{Action: "sell", OptionType: "put", Strike: 470, Expiry: "2026-12-18", PremiumUSD: 2}), logger)
		if err != nil || n != 1 {
			t.Fatalf("first put trades=%d err=%v; log:\n%s", n, err, buf.String())
		}
		if !ocNear(s.Cash, 47000+200-0.03) {
			t.Fatalf("cash = %v, want 47199.97", s.Cash)
		}
		leg := s.OptionPositions["SPY-put-sell-470-2026-12-18"]
		if leg == nil || leg.ContractMultiplier != 100 || !ocNear(leg.EntryPremiumUSD, 199.97) {
			t.Fatalf("leg = %+v, want multiplier 100 and net premium 199.97", leg)
		}
		if reserve, err := reservedShortPutCollateralUSD(s); err != nil || reserve != 47000 {
			t.Fatalf("reserve = %v, %v; want 47000", reserve, err)
		}
		if got := portfolioNotional(map[string]*StrategyState{sc.ID: s}, nil); got != 47000 {
			t.Fatalf("notional = %v, want 47000", got)
		}

		before := s.Cash
		buf.Reset()
		n, _ = ExecuteOptionsSignal(s, ocResult("robinhood", "SPY", 480, OptionsAction{Action: "sell", OptionType: "put", Strike: 460, Expiry: "2026-12-18", PremiumUSD: 3}), logger)
		if n != 0 || s.Cash != before || len(s.OptionPositions) != 1 {
			t.Fatalf("second put must not reuse reserved cash: trades=%d cash=%v legs=%d", n, s.Cash, len(s.OptionPositions))
		}
		if !strings.Contains(buf.String(), "required $93000.00 (new $46000.00 + reserved $47000.00), available cash $47199.97") {
			t.Fatalf("refusal must log required and available amounts:\n%s", buf.String())
		}

		buf.Reset()
		n, _ = ExecuteOptionsSignal(s, ocResult("robinhood", "SPY", 480, OptionsAction{Action: "buy", OptionType: "call", Strike: 500, Expiry: "2026-12-18", PremiumUSD: 2}), logger)
		if n != 0 || s.Cash != before {
			t.Fatalf("a buy must not spend reserved collateral: trades=%d cash=%v", n, s.Cash)
		}
		if !strings.Contains(buf.String(), "required $200.03 (premium $200.00 + fee $0.03), available $199.97") {
			t.Fatalf("buy refusal amounts missing:\n%s", buf.String())
		}
		n, _ = ExecuteOptionsSignal(s, ocResult("robinhood", "SPY", 480, OptionsAction{Action: "buy", OptionType: "call", Strike: 500, Expiry: "2026-12-18", PremiumUSD: 1.5}), logger)
		if n != 1 || !ocNear(s.Cash, before-150.03) {
			t.Fatalf("a buy inside free cash must book: trades=%d cash=%v", n, s.Cash)
		}
		trades := ocStoredTrades(t, env, sc.ID)
		if len(trades) != 2 || trades[0].side != "sell" || trades[0].qty != 1 || trades[1].side != "buy" {
			t.Fatalf("stored trades = %+v, want only the accepted sell and buy", trades)
		}
	})

	t.Run("sell_keeps_the_reserve_after_its_fee", func(t *testing.T) {
		sc := ocSC("rh-opt-fee", "robinhood", "SPY")
		env := ocEnv(t, sc)
		s := env.strat(sc.ID)
		s.Cash = 47000
		s.Positions["SPY"] = &Position{Symbol: "SPY", Side: "long", Quantity: 100, AvgCost: 450, OwnerStrategyID: sc.ID}
		s.OptionPositions["SPY-put-sell-470-2026-12-18"] = &OptionPosition{ID: "SPY-put-sell-470-2026-12-18", Underlying: "SPY", OptionType: "put",
			Strike: 470, Expiry: "2026-12-18", Action: "sell", Quantity: 1, ContractMultiplier: 100, EntryPremiumUSD: 100, CurrentValueUSD: -100}
		logger, buf := ocLogger(sc.ID)
		n, _ := ExecuteOptionsSignal(s, ocResult("robinhood", "SPY", 480, OptionsAction{Action: "sell", OptionType: "call", Strike: 520, Expiry: "2026-12-18", PremiumUSD: 0.0002}), logger)
		if n != 0 || s.Cash != 47000 || len(s.OptionPositions) != 1 {
			t.Fatalf("a covered call whose fee exceeds its premium at the reserve must hold: trades=%d cash=%v legs=%d", n, s.Cash, len(s.OptionPositions))
		}
		if !strings.Contains(buf.String(), "would leave cash below reserved put collateral: required $47000.00, available $46999.99") {
			t.Fatalf("after-fee refusal not logged:\n%s", buf.String())
		}
		if trades := ocStoredTrades(t, env, sc.ID); len(trades) != 0 {
			t.Fatalf("stored trades = %+v, want none", trades)
		}
	})

	t.Run("robinhood_short_calls_need_free_held_shares", func(t *testing.T) {
		cases := []struct {
			name      string
			held      float64
			reserved  bool
			longCall  bool
			peerHeld  bool
			wantTrade bool
		}{
			{name: "absent"},
			{name: "partial", held: 50},
			{name: "exact", held: 100, wantTrade: true},
			{name: "already_reserved", held: 100, reserved: true},
			{name: "long_call_is_no_cover", longCall: true},
			{name: "other_strategy_shares_are_no_cover", peerHeld: true},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				sc := ocSC("rh-opt-call", "robinhood", "SPY")
				peer := ocSC("rh-opt-peer", "robinhood", "SPY")
				env := ocEnv(t, sc, peer)
				s := env.strat(sc.ID)
				if tc.held > 0 {
					s.Positions["SPY"] = &Position{Symbol: "SPY", Side: "long", Quantity: tc.held, AvgCost: 450, OwnerStrategyID: sc.ID}
				}
				if tc.peerHeld {
					env.strat(peer.ID).Positions["SPY"] = &Position{Symbol: "SPY", Side: "long", Quantity: 500, AvgCost: 450, OwnerStrategyID: peer.ID}
				}
				if tc.reserved {
					s.OptionPositions["SPY-call-sell-510-2026-12-18"] = &OptionPosition{ID: "SPY-call-sell-510-2026-12-18", Underlying: "SPY", OptionType: "call",
						Strike: 510, Expiry: "2026-12-18", Action: "sell", Quantity: 1, ContractMultiplier: 100, EntryPremiumUSD: 100, CurrentValueUSD: -100}
				}
				if tc.longCall {
					s.OptionPositions["SPY-call-buy-450-2026-12-18"] = &OptionPosition{ID: "SPY-call-buy-450-2026-12-18", Underlying: "SPY", OptionType: "call",
						Strike: 450, Expiry: "2026-12-18", Action: "buy", Quantity: 5, ContractMultiplier: 100, EntryPremiumUSD: 1000, CurrentValueUSD: 1000}
				}
				legs := len(s.OptionPositions)
				logger, buf := ocLogger(sc.ID)
				n, _ := ExecuteOptionsSignal(s, ocResult("robinhood", "SPY", 480, OptionsAction{Action: "sell", OptionType: "call", Strike: 520, Expiry: "2026-12-18", PremiumUSD: 1.2}), logger)
				if tc.wantTrade != (n == 1) {
					t.Fatalf("trades = %d, want trade=%t; log:\n%s", n, tc.wantTrade, buf.String())
				}
				if !tc.wantTrade {
					if len(s.OptionPositions) != legs || s.Cash != 100000 || !strings.Contains(buf.String(), "Uncovered short call refused SPY strike=520 qty=1: required 100 free shares") {
						t.Fatalf("refusal changed the book or did not log: legs=%d cash=%v log:\n%s", len(s.OptionPositions), s.Cash, buf.String())
					}
					if trades := ocStoredTrades(t, env, sc.ID); len(trades) != 0 {
						t.Fatalf("stored trades = %+v, want none", trades)
					}
				}
			})
		}
	})

	t.Run("uncovered_short_call_refuses_the_whole_entry_signal", func(t *testing.T) {
		expiry := "2026-12-18"
		for _, tc := range []struct {
			name     string
			platform string
			held     float64
			actions  []OptionsAction
			want     int
		}{
			{name: "robinhood_butterfly", platform: "robinhood", actions: []OptionsAction{
				{Action: "buy", OptionType: "call", Strike: 570, Expiry: expiry, PremiumUSD: 1},
				{Action: "sell", OptionType: "call", Strike: 600, Expiry: expiry, PremiumUSD: 1, Quantity: 2},
				{Action: "buy", OptionType: "call", Strike: 630, Expiry: expiry, PremiumUSD: 1},
			}},
			{name: "robinhood_high_iv_strangle", platform: "robinhood", actions: []OptionsAction{
				{Action: "sell", OptionType: "call", Strike: 660, Expiry: expiry, PremiumUSD: 1},
				{Action: "sell", OptionType: "put", Strike: 540, Expiry: expiry, PremiumUSD: 1},
			}},
			{name: "robinhood_covered_wheel_call", platform: "robinhood", held: 100, want: 1, actions: []OptionsAction{
				{Action: "sell", OptionType: "call", Strike: 600, Expiry: expiry, PremiumUSD: 1},
			}},
			{name: "deribit_butterfly", platform: "deribit", want: 3, actions: []OptionsAction{
				{Action: "buy", OptionType: "call", Strike: 570, Expiry: expiry, PremiumUSD: 1},
				{Action: "sell", OptionType: "call", Strike: 600, Expiry: expiry, PremiumUSD: 1, Quantity: 2},
				{Action: "buy", OptionType: "call", Strike: 630, Expiry: expiry, PremiumUSD: 1},
			}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				sc := ocSC("opt-atomic-cover", tc.platform, "SPY")
				env := ocEnv(t, sc)
				s := env.strat(sc.ID)
				s.Cash = 80000
				if tc.held > 0 {
					s.Positions["SPY"] = &Position{Symbol: "SPY", Side: "long", Quantity: tc.held, AvgCost: 450, OwnerStrategyID: sc.ID}
				}
				logger, _ := ocLogger(sc.ID)
				n, err := ExecuteOptionsSignal(s, ocResult(tc.platform, "SPY", 600, tc.actions...), logger)
				if err != nil || n != tc.want || len(s.OptionPositions) != tc.want || len(ocStoredTrades(t, env, sc.ID)) != tc.want {
					t.Fatalf("trades=%d legs=%d err=%v, want %d complete entries", n, len(s.OptionPositions), err, tc.want)
				}
				if tc.want == 0 && s.Cash != 80000 {
					t.Fatalf("refused signal cash = %v, want unchanged 80000", s.Cash)
				}
			})
		}
	})

	t.Run("contract_fees_and_trade_quantities", func(t *testing.T) {
		sc := ocSC("rh-opt-qty", "robinhood", "SPY")
		env := ocEnv(t, sc)
		s := env.strat(sc.ID)
		logger, _ := ocLogger(sc.ID)
		n, err := ExecuteOptionsSignal(s, ocResult("robinhood", "SPY", 480, OptionsAction{Action: "buy", OptionType: "put", Strike: 450, Expiry: "2026-12-18", PremiumUSD: 1.25, Quantity: 2}), logger)
		if err != nil || n != 1 {
			t.Fatalf("buy trades=%d err=%v", n, err)
		}
		if !ocNear(s.Cash, 100000-250-0.06) {
			t.Fatalf("cash = %v, want premium 250 and fee 2 x $0.03", s.Cash)
		}
		trades := ocStoredTrades(t, env, sc.ID)
		if len(trades) != 1 || trades[0].qty != 2 || !ocNear(trades[0].price, 250) || !ocNear(trades[0].value, 250.06) {
			t.Fatalf("stored trades = %+v, want quantity 2 at $250 plus $0.06 fee", trades)
		}
		key := "SPY-put-buy-450-2026-12-18"
		if js, err := EncodeAllPositionsJSON(s.Platform, s.OptionPositions, s.Positions); err != nil || !strings.Contains(js, `"entry_premium_usd":1.25`) {
			t.Fatalf("encoder = %s, %v; want the per-share entry premium 1.25", js, err)
		}
		if js, err := EncodePositionsJSON(s.Platform, s.OptionPositions); err != nil || !strings.Contains(js, `"entry_premium_usd":1.25`) {
			t.Fatalf("legacy encoder = %s, %v; want the per-share entry premium 1.25", js, err)
		}
		if s.OptionPositions[key] == nil {
			t.Fatalf("leg %s missing", key)
		}
	})

	t.Run("short_signal_close_debits_the_whole_position", func(t *testing.T) {
		sc := ocSC("rh-opt-close", "robinhood", "SPY")
		env := ocEnv(t, sc)
		s := env.strat(sc.ID)
		s.Cash = 50000
		s.OptionPositions["SPY-put-sell-470-2026-12-18"] = &OptionPosition{ID: "SPY-put-sell-470-2026-12-18", Underlying: "SPY", OptionType: "put",
			Strike: 470, Expiry: "2026-12-18", Action: "sell", Quantity: 2, ContractMultiplier: 100, EntryPremiumUSD: 400, CurrentValueUSD: -400}
		logger, _ := ocLogger(sc.ID)
		n, err := ExecuteOptionsSignal(s, ocResult("robinhood", "SPY", 480, OptionsAction{Action: "close", OptionType: "put", Strike: 470, PremiumUSD: 1.5}), logger)
		if err != nil || n != 1 {
			t.Fatalf("close trades=%d err=%v", n, err)
		}
		if !ocNear(s.Cash, 50000-300) || len(s.ClosedOptionPositions) != 1 || !ocNear(s.ClosedOptionPositions[0].RealizedPnL, 100) || !ocNear(s.ClosedOptionPositions[0].ClosePriceUSD, 300) {
			t.Fatalf("cash=%v closed=%+v, want a $300 debit and $100 PnL", s.Cash, s.ClosedOptionPositions)
		}
		trades := ocStoredTrades(t, env, sc.ID)
		if len(trades) != 1 || trades[0].qty != 2 || !ocNear(trades[0].value, 300) {
			t.Fatalf("stored close = %+v, want quantity 2 valued $300", trades)
		}
	})

	t.Run("result_contract_refuses_every_action", func(t *testing.T) {
		sc := ocSC("rh-opt-contract", "robinhood", "SPY")
		env := ocEnv(t, sc)
		s := env.strat(sc.ID)
		s.Positions["SPY"] = &Position{Symbol: "SPY", Side: "long", Quantity: 100, AvgCost: 450, OwnerStrategyID: sc.ID}
		s.OptionPositions["SPY-put-sell-470-2026-12-18"] = &OptionPosition{ID: "SPY-put-sell-470-2026-12-18", Underlying: "SPY", OptionType: "put",
			Strike: 470, Expiry: "2026-12-18", Action: "sell", Quantity: 1, ContractMultiplier: 100, EntryPremiumUSD: 200, CurrentValueUSD: -200}
		actions := []OptionsAction{
			{Action: "close", OptionType: "put", Strike: 470, PremiumUSD: 1},
			{Action: "buy", OptionType: "call", Strike: 500, Expiry: "2026-12-18", PremiumUSD: 1},
		}
		for _, tc := range []struct{ name, platform, unit string }{
			{"missing_unit", "robinhood", ""},
			{"other_unit", "robinhood", "contract"},
			{"missing_platform", "", optionPremiumUnitUnderlying},
			{"other_platform", "deribit", optionPremiumUnitUnderlying},
		} {
			t.Run(tc.name, func(t *testing.T) {
				r := ocResult(tc.platform, "SPY", 480, actions...)
				r.PremiumUnit = tc.unit
				logger, _ := ocLogger(sc.ID)
				n, err := ExecuteOptionsSignal(s, r, logger)
				if n != 0 || err == nil || !strings.Contains(err.Error(), "options result contract") {
					t.Fatalf("trades=%d err=%v, want a contract refusal", n, err)
				}
				if s.Cash != 100000 || len(s.OptionPositions) != 1 || len(s.TradeHistory) != 0 {
					t.Fatalf("book changed: cash=%v legs=%d trades=%d", s.Cash, len(s.OptionPositions), len(s.TradeHistory))
				}
			})
		}
	})

	t.Run("marks_scale_by_contract_size_and_quantity", func(t *testing.T) {
		logger, _ := ocLogger("marks")
		mk := func(platform string, qty float64, action string) *StrategyState {
			underlying := "SPY"
			if platform == "deribit" {
				underlying = "BTC"
			}
			return &StrategyState{ID: platform + "-marks", Type: "options", Platform: platform, Positions: map[string]*Position{},
				OptionPositions: map[string]*OptionPosition{"leg": {ID: "leg", Underlying: underlying, OptionType: "call", Strike: 500,
					Expiry: "2099-01-16", Action: action, Quantity: qty}}}
		}
		for _, tc := range []struct {
			platform string
			qty      float64
			action   string
			want     float64
		}{
			{"robinhood", 2, "buy", 0.01 * 500 * 100 * 2},
			{"robinhood", 2, "sell", -0.01 * 500 * 100 * 2},
			{"deribit", 1, "buy", 0.01 * 500},
			{"deribit", 2, "buy", 0.01 * 500 * 2},
			{"deribit", 2, "sell", -0.01 * 500 * 2},
		} {
			s := mk(tc.platform, tc.qty, tc.action)
			ocMarkCycle(s, ocPricer{mark: 0.01, spot: 500}, logger)
			if got := s.OptionPositions["leg"].CurrentValueUSD; !ocNear(got, tc.want) {
				t.Fatalf("%s qty=%g %s value = %v, want %v", tc.platform, tc.qty, tc.action, got, tc.want)
			}
		}
	})

	t.Run("robinhood_expiry_and_assignment", func(t *testing.T) {
		cases := []struct {
			name       string
			optionType string
			strike     float64
			spot       float64
			held       float64
			wantCash   float64
			wantShares float64
			wantTrade  float64
			wantWarn   bool
		}{
			{name: "itm_put_buys_scaled_shares", optionType: "put", strike: 470, spot: 460, wantCash: 50000 - 47000, wantShares: 100, wantTrade: 100},
			{name: "itm_put_adds_to_held_shares", optionType: "put", strike: 470, spot: 460, held: 50, wantCash: 50000 - 47000, wantShares: 150, wantTrade: 100},
			{name: "covered_call_delivers_held_shares", optionType: "call", strike: 470, spot: 480, held: 100, wantCash: 50000 + 47000, wantShares: 0, wantTrade: 100},
			{name: "covered_call_leaves_extra_shares", optionType: "call", strike: 470, spot: 480, held: 130, wantCash: 50000 + 47000, wantShares: 30, wantTrade: 100},
			{name: "uncovered_call_settles_intrinsic", optionType: "call", strike: 470, spot: 480, wantCash: 50000 - 1000, wantWarn: true},
			{name: "partly_covered_call", optionType: "call", strike: 470, spot: 480, held: 40, wantCash: 50000 + 470*40 - 10*60, wantTrade: 40, wantWarn: true},
			{name: "otm_put_expires", optionType: "put", strike: 470, spot: 480, wantCash: 50000},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				sc := ocSC("rh-opt-expiry", "robinhood", "SPY")
				env := ocEnv(t, sc)
				s := env.strat(sc.ID)
				s.Cash = 50000
				if tc.held > 0 {
					s.Positions["SPY"] = &Position{Symbol: "SPY", Side: "long", Quantity: tc.held, AvgCost: 450, OwnerStrategyID: sc.ID}
				}
				s.OptionPositions["leg"] = ocExpiredLeg("leg", tc.optionType, "sell", tc.strike, 1)
				logger, buf := ocLogger(sc.ID)
				ocMarkCycle(s, ocPricer{expirySpot: tc.spot}, logger)
				if len(s.OptionPositions) != 0 {
					t.Fatalf("the expired leg must settle once")
				}
				env.saveAll()
				if !ocNear(s.Cash, tc.wantCash) {
					t.Fatalf("cash = %v, want %v; log:\n%s", s.Cash, tc.wantCash, buf.String())
				}
				shares := 0.0
				if p := s.Positions["SPY"]; p != nil {
					shares = p.Quantity
				}
				if !ocNear(shares, tc.wantShares) {
					t.Fatalf("shares = %v, want %v", shares, tc.wantShares)
				}
				var assigned []qocTradeRow
				for _, tr := range ocStoredTrades(t, env, sc.ID) {
					if tr.tradeType == "assignment" {
						assigned = append(assigned, tr)
					}
				}
				if tc.wantTrade == 0 && len(assigned) != 0 || tc.wantTrade > 0 && (len(assigned) != 1 || assigned[0].qty != tc.wantTrade) {
					t.Fatalf("assignment trades = %+v, want quantity %v", assigned, tc.wantTrade)
				}
				if tc.wantWarn != strings.Contains(buf.String(), "UNCOVERED CALL ASSIGNMENT") {
					t.Fatalf("uncovered warning presence != %t:\n%s", tc.wantWarn, buf.String())
				}
				ocMarkCycle(s, ocPricer{expirySpot: tc.spot}, logger)
				if !ocNear(s.Cash, tc.wantCash) {
					t.Fatalf("a repeated refresh applied the settlement twice: cash=%v", s.Cash)
				}
			})
		}
	})

	t.Run("deribit_call_assignment_settles_only_held_units", func(t *testing.T) {
		for _, tc := range []struct {
			name      string
			held      float64
			wantCash  float64
			wantTrade int
		}{
			{name: "uncovered_call_settles_intrinsic", wantCash: 0},
			{name: "covered_call_delivers_held_units", held: 1, wantCash: 51000, wantTrade: 1},
		} {
			t.Run(tc.name, func(t *testing.T) {
				sc := ocSC("deribit-opt-expiry", "deribit", "BTC")
				env := ocEnv(t, sc)
				s := env.strat(sc.ID)
				s.Cash = 1000
				if tc.held > 0 {
					s.Positions["BTC"] = &Position{Symbol: "BTC", Side: "long", Quantity: tc.held, AvgCost: 40000, OwnerStrategyID: sc.ID}
				}
				s.OptionPositions["leg"] = ocExpiredLeg("leg", "call", "sell", 50000, 1)
				s.OptionPositions["leg"].Underlying = "BTC"
				logger, _ := ocLogger(sc.ID)
				ocMarkCycle(s, ocPricer{expirySpot: 51000}, logger)
				if !ocNear(s.Cash, tc.wantCash) {
					t.Fatalf("cash = %v, want %v", s.Cash, tc.wantCash)
				}
				env.saveAll()
				if p := s.Positions["BTC"]; tc.held > 0 && p != nil {
					t.Fatalf("covered units remain after assignment: %+v", p)
				}
				trades := ocStoredTrades(t, env, sc.ID)
				if len(trades) != tc.wantTrade {
					t.Fatalf("assignment trades = %+v, want %d", trades, tc.wantTrade)
				}
				if tc.wantTrade == 1 && (trades[0].tradeType != "assignment" || trades[0].qty != 1) {
					t.Fatalf("assignment trade = %+v, want quantity 1", trades[0])
				}
				ocMarkCycle(s, ocPricer{expirySpot: 51000}, logger)
				if !ocNear(s.Cash, tc.wantCash) {
					t.Fatalf("repeated mark cash = %v, want %v", s.Cash, tc.wantCash)
				}
			})
		}
	})

	t.Run("paper_options_debt_survives_restart_and_validation", func(t *testing.T) {
		sc := ocSC("rh-opt-debt", "robinhood", "SPY")
		env := ocEnv(t, sc)
		s := env.strat(sc.ID)
		s.Cash = 200
		s.OptionPositions["leg"] = ocExpiredLeg("leg", "call", "sell", 470, 1)
		logger, _ := ocLogger(sc.ID)
		ocMarkCycle(s, ocPricer{expirySpot: 480}, logger)
		if !ocNear(s.Cash, -800) || len(s.OptionPositions) != 0 {
			t.Fatalf("cash=%v legs=%d, want -800 and no legs", s.Cash, len(s.OptionPositions))
		}
		env.saveAll()
		env = env.restart()
		ValidateState(env.state, env.cfg.Strategies)
		if got := env.strat(sc.ID).Cash; !ocNear(got, -800) {
			t.Fatalf("restart cash = %v, want the retained -800 debt", got)
		}
		book := env.strat(sc.ID)
		book.Cash = -50
		if err := env.store.SaveStrategyBook(book); err != nil {
			t.Fatalf("book save: %v", err)
		}
		env = env.restart()
		ValidateState(env.state, env.cfg.Strategies)
		if got := env.strat(sc.ID).Cash; got != -50 {
			t.Fatalf("book-save restart cash = %v, want -50", got)
		}
		spot := StrategyConfig{ID: "spot-debt", Type: "spot", Platform: "binanceus", Args: []string{"sma", "BTC/USDT", "1h", "--mode=paper"}}
		other := &AppState{Strategies: map[string]*StrategyState{spot.ID: {ID: spot.ID, Type: "spot", Cash: -5, Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{}}}}
		ValidateState(other, []StrategyConfig{spot})
		if other.Strategies[spot.ID].Cash != 0 {
			t.Fatalf("unrelated strategies keep the clamp: cash=%v", other.Strategies[spot.ID].Cash)
		}
	})

	t.Run("risk_inputs_include_contract_size_per_partition", func(t *testing.T) {
		dir := t.TempDir()
		rh := ocSC("rh-opt-risk", "robinhood", "SPY")
		src := ocSC("rh-opt-risk-src", "robinhood", "SPY")
		src.PaperSource = "src"
		cfg := &Config{DBFile: filepath.Join(dir, "state.db"), PaperSources: []PaperSourceConfig{{ID: "src", DBFile: filepath.Join(dir, "src.db")}},
			Strategies: []StrategyConfig{rh, src}}
		env := openEntryTestEnv(t, cfg)
		env.strat(rh.ID).OptionPositions["put"] = &OptionPosition{ID: "put", Underlying: "SPY", OptionType: "put", Strike: 470, Expiry: "2099-01-16", Action: "sell", Quantity: 1, Greeks: OptGreeks{Delta: -0.4}}
		env.strat(src.ID).OptionPositions["call"] = &OptionPosition{ID: "call", Underlying: "SPY", OptionType: "call", Strike: 500, Expiry: "2099-01-16", Action: "sell", Quantity: 2}
		env.saveAll()
		env = env.restart()
		for id, want := range map[string]float64{rh.ID: 100, src.ID: 100} {
			for _, leg := range env.strat(id).OptionPositions {
				if leg.ContractMultiplier != want {
					t.Fatalf("%s leg %s stamped %v, want %v", id, leg.ID, leg.ContractMultiplier, want)
				}
			}
		}
		defaultPaper := filterStatesByPartition(env.state.Strategies, cfg.Strategies, partitionFor(rh))
		sourcePaper := filterStatesByPartition(env.state.Strategies, cfg.Strategies, partitionFor(src))
		if got := portfolioNotional(defaultPaper, nil); got != 47000 {
			t.Fatalf("default paper notional = %v, want 47000", got)
		}
		if got := portfolioNotional(sourcePaper, nil); got != 500*2*100 {
			t.Fatalf("source paper notional = %v, want 100000", got)
		}
		prices := map[string]float64{"SPY": 480}
		assets, _ := computeAssetDeltas(defaultPaper, strategiesInPartition(cfg.Strategies, partitionFor(rh)), prices)
		if a := assets["SPY"]; a == nil || len(a.Strategies) != 1 || !ocNear(a.Strategies[0].DeltaUSD, 0.4*100*480) {
			t.Fatalf("default paper measured delta = %+v, want +19200", a)
		}
		assets, _ = computeAssetDeltas(sourcePaper, strategiesInPartition(cfg.Strategies, partitionFor(src)), prices)
		if a := assets["SPY"]; a == nil || len(a.Strategies) != 1 || !ocNear(a.Strategies[0].DeltaUSD, -1*2*100*480) {
			t.Fatalf("source paper coarse delta = %+v, want -96000", a)
		}
	})

	t.Run("legacy_rows_stamp_on_load_without_changing_history", func(t *testing.T) {
		dir := t.TempDir()
		rh := ocSC("rh-opt-legacy", "robinhood", "SPY")
		der := ocSC("deribit-opt-legacy", "deribit", "BTC")
		der.StorageStrategyID = "der-store"
		cfg := &Config{DBFile: filepath.Join(dir, "state.db"), Strategies: []StrategyConfig{rh, der}}
		env := openEntryTestEnv(t, cfg)
		env.strat(rh.ID).Cash = 1234.5
		env.strat(rh.ID).OptionPositions["rh-leg"] = &OptionPosition{ID: "rh-leg", Underlying: "SPY", OptionType: "put", Strike: 470, Expiry: "2099-01-16", Action: "sell", Quantity: 1, EntryPremiumUSD: 1.5, CurrentValueUSD: -1.2}
		env.strat(rh.ID).OptionPositions["rh-explicit"] = &OptionPosition{ID: "rh-explicit", Underlying: "SPY", OptionType: "put", Strike: 400, Expiry: "2099-01-16", Action: "sell", Quantity: 1, ContractMultiplier: 10, EntryPremiumUSD: 9, CurrentValueUSD: -9}
		env.strat(rh.ID).Positions["SPY"] = &Position{Symbol: "SPY", Side: "long", Quantity: 1, AvgCost: 470, OwnerStrategyID: rh.ID}
		env.strat(der.ID).OptionPositions["der-leg"] = &OptionPosition{ID: "der-leg", Underlying: "BTC", OptionType: "call", Strike: 90000, Expiry: "2099-01-16", Action: "buy", Quantity: 2, EntryPremiumUSD: 800, CurrentValueUSD: 900}
		env.saveAll()
		env.store.Close()

		raw, err := OpenStateDB(cfg.DBFile)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := raw.db.Exec(`UPDATE option_positions SET contract_multiplier = 0 WHERE id != 'rh-explicit'`); err != nil {
			t.Fatal(err)
		}
		if _, err := raw.db.Exec(`ALTER TABLE option_positions DROP COLUMN contract_multiplier`); err != nil {
			t.Fatalf("drop column to build an old file: %v", err)
		}
		raw.Close()
		sum := func() [32]byte {
			b, err := os.ReadFile(cfg.DBFile)
			if err != nil {
				t.Fatal(err)
			}
			return sha256.Sum256(b)
		}
		oldSum := sum()

		layout, err := resolveStorageLayout(cfg)
		if err != nil {
			t.Fatal(err)
		}
		ident, err := buildStorageIdentityMap(cfg, layout)
		if err != nil {
			t.Fatal(err)
		}
		ro, err := OpenStateStoreReadOnly(layout, ident)
		if err != nil {
			t.Fatalf("read-only open: %v", err)
		}
		roState, _, err := LoadStateWithStore(cfg, ro)
		if err != nil {
			t.Fatalf("read-only load of an old file: %v", err)
		}
		if got := roState.Strategies[rh.ID].OptionPositions["rh-leg"].ContractMultiplier; got != 100 {
			t.Fatalf("read-only robinhood stamp = %v, want 100", got)
		}
		if got := roState.Strategies[rh.ID].OptionPositions["rh-explicit"].ContractMultiplier; got != 100 {
			t.Fatalf("a dropped column reads as legacy zero and stamps from the platform: got %v", got)
		}
		if got := roState.Strategies[der.ID].OptionPositions["der-leg"].ContractMultiplier; got != 1 {
			t.Fatalf("read-only deribit stamp = %v, want 1", got)
		}
		ro.Close()
		if sum() != oldSum {
			t.Fatal("a read-only load changed the old state file")
		}

		env = openEntryTestEnv(t, cfg)
		if has, err := env.fileFor(rh.ID).tableHasColumn("option_positions", "contract_multiplier"); err != nil || !has {
			t.Fatalf("writable open must migrate the column: %v %v", has, err)
		}
		env.strat(rh.ID).OptionPositions["rh-explicit"].ContractMultiplier = 10
		env.saveAll()
		env = env.restart()
		rhBook := env.strat(rh.ID)
		if rhBook.Cash != 1234.5 || rhBook.Positions["SPY"].Quantity != 1 {
			t.Fatalf("stamping changed history: cash=%v shares=%v", rhBook.Cash, rhBook.Positions["SPY"].Quantity)
		}
		if leg := rhBook.OptionPositions["rh-leg"]; leg.ContractMultiplier != 100 || leg.EntryPremiumUSD != 1.5 || leg.CurrentValueUSD != -1.2 {
			t.Fatalf("robinhood leg = %+v, want stamp 100 with stored premium and value unchanged", leg)
		}
		if leg := rhBook.OptionPositions["rh-explicit"]; leg.ContractMultiplier != 10 {
			t.Fatalf("explicit stamp = %v, want the retained 10", leg.ContractMultiplier)
		}
		if leg := env.strat(der.ID).OptionPositions["der-leg"]; leg.ContractMultiplier != 1 || leg.EntryPremiumUSD != 800 {
			t.Fatalf("deribit leg = %+v, want stamp 1", leg)
		}
		var stored float64
		if err := env.fileFor(der.ID).db.QueryRow(`SELECT contract_multiplier FROM option_positions WHERE strategy_id = 'der-store' AND id = 'der-leg'`).Scan(&stored); err != nil || stored != 1 {
			t.Fatalf("stored deribit stamp under its storage alias = %v, %v; want 1", stored, err)
		}

		for _, bad := range []struct{ name, value string }{{"negative", "-5"}, {"infinite", "9e999"}} {
			if _, err := env.fileFor(rh.ID).db.Exec(`UPDATE option_positions SET contract_multiplier = ` + bad.value + ` WHERE id = 'rh-leg'`); err != nil {
				t.Fatal(err)
			}
			env.store.Close()
			layout, _ := resolveStorageLayout(cfg)
			ident, _ := buildStorageIdentityMap(cfg, layout)
			store, err := openStateStoreWith(layout, ident, openStateDBFromTemplate)
			if err != nil {
				t.Fatal(err)
			}
			_, _, err = LoadStateWithStore(cfg, store)
			if err == nil || !strings.Contains(err.Error(), "strategy rh-opt-legacy option leg rh-leg: invalid contract multiplier") {
				t.Fatalf("%s multiplier load err = %v, want a named leg refusal", bad.name, err)
			}
			var n int
			if err := store.primary().db.QueryRow(`SELECT COUNT(*) FROM option_positions WHERE id = 'rh-leg'`).Scan(&n); err != nil || n != 1 {
				t.Fatalf("%s multiplier refusal removed the liability: rows=%d err=%v", bad.name, n, err)
			}
			if _, err := store.primary().db.Exec(`UPDATE option_positions SET contract_multiplier = 100 WHERE id = 'rh-leg'`); err != nil {
				t.Fatal(err)
			}
			store.Close()
			env = openEntryTestEnv(t, cfg)
		}

		env.strat(rh.ID).OptionPositions["rh-leg"].ContractMultiplier = math.Inf(1)
		for part, err := range env.store.SaveAll(env.state) {
			if part == partitionFor(rh) && (err == nil || !strings.Contains(err.Error(), "invalid contract multiplier")) {
				t.Fatalf("save of a non-finite multiplier = %v, want refusal", err)
			}
		}
		if err := env.store.SaveStrategyBook(env.strat(rh.ID)); err == nil {
			t.Fatal("book save of a non-finite multiplier must refuse")
		}
		var stamp float64
		if err := env.fileFor(rh.ID).db.QueryRow(`SELECT contract_multiplier FROM option_positions WHERE id = 'rh-leg'`).Scan(&stamp); err != nil || stamp != 100 {
			t.Fatalf("refused saves replaced the stored leg: %v %v", stamp, err)
		}
	})
}
