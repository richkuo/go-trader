package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sort"
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
	stamped := append([]OptionsAction(nil), actions...)
	entries := 0
	for _, a := range stamped {
		if a.Action != "close" && a.StructureID == "" {
			entries++
		}
	}
	for i := range stamped {
		if stamped[i].Action != "close" && stamped[i].StructureID == "" {
			stamped[i].StructureID, stamped[i].StructureLegs = "oc-1", entries
		}
	}
	return &OptionsResult{Underlying: underlying, Platform: platform, PremiumUnit: optionPremiumUnitUnderlying,
		StructureContract: optionsStructureContractV1, Signal: -1, SpotPrice: spot, Actions: stamped}
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
		env.saveAll()
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
				env.saveAll()
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
		env.saveAll()
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

	t.Run("deribit_call_settles_in_cash_and_keeps_held_units", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			held float64
		}{
			{name: "no_held_units"},
			{name: "held_units_stay", held: 1},
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
				if !ocNear(s.Cash, 0) {
					t.Fatalf("cash = %v, want 0 (1000 minus intrinsic 1000)", s.Cash)
				}
				env.saveAll()
				p := s.Positions["BTC"]
				if tc.held > 0 && (p == nil || p.Quantity != tc.held || p.AvgCost != 40000) || tc.held == 0 && p != nil {
					t.Fatalf("a cash-settled call changed the BTC position: %+v", p)
				}
				trades := ocStoredTrades(t, env, sc.ID)
				if len(trades) != 1 || trades[0].tradeType != "options" || trades[0].side != "buy" || trades[0].qty != 1 || trades[0].value != 1000 {
					t.Fatalf("stored trades = %+v, want one option close row of 1 contract for $1000", trades)
				}
				ocMarkCycle(s, ocPricer{expirySpot: 51000}, logger)
				if !ocNear(s.Cash, 0) {
					t.Fatalf("repeated mark cash = %v, want 0", s.Cash)
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

const ocExpiry = "2026-12-18"

func ocLeg(action, optionType string, strike, premiumUSD float64) OptionsAction {
	return OptionsAction{Action: action, OptionType: optionType, Strike: strike, Expiry: ocExpiry, PremiumUSD: premiumUSD}
}

func ocStructure(id string, legs ...OptionsAction) []OptionsAction {
	out := append([]OptionsAction(nil), legs...)
	for i := range out {
		out[i].StructureID, out[i].StructureLegs = id, len(legs)
	}
	return out
}

func ocRaw(platform, underlying string, spot float64, actions ...OptionsAction) *OptionsResult {
	return &OptionsResult{Underlying: underlying, Platform: platform, PremiumUnit: optionPremiumUnitUnderlying,
		StructureContract: optionsStructureContractV1, Signal: 1, SpotPrice: spot, Actions: actions}
}

func ocBookSnapshot(s *StrategyState) map[string]OptionPosition {
	out := make(map[string]OptionPosition, len(s.OptionPositions))
	for k, v := range s.OptionPositions {
		out[k] = *v
	}
	return out
}

func ocDurable(t *testing.T, env *entryTestEnv, id, storageID string) (float64, int, int) {
	t.Helper()
	db := env.fileFor(id).db
	var cash float64
	if err := db.QueryRow(`SELECT cash FROM strategies WHERE id = ?`, storageID).Scan(&cash); err != nil {
		t.Fatalf("stored cash for %s: %v", storageID, err)
	}
	var legs, trades int
	if err := db.QueryRow(`SELECT COUNT(*) FROM option_positions WHERE strategy_id = ?`, storageID).Scan(&legs); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM trades WHERE strategy_id = ?`, storageID).Scan(&trades); err != nil {
		t.Fatal(err)
	}
	return cash, legs, trades
}

func ocUnpersisted(s *StrategyState) int {
	n := 0
	for _, tr := range s.TradeHistory {
		if !tr.persisted {
			n++
		}
	}
	return n
}

func ocAssertRefusedUnchanged(t *testing.T, env *entryTestEnv, s *StrategyState, n int, cash float64, book map[string]OptionPosition, log string, wants ...string) {
	t.Helper()
	if n != 0 || s.Cash != cash || !reflect.DeepEqual(ocBookSnapshot(s), book) || len(s.TradeHistory) != 0 {
		t.Fatalf("a refused structure changed the book: trades=%d cash=%v legs=%v history=%d; log:\n%s", n, s.Cash, ocBookSnapshot(s), len(s.TradeHistory), log)
	}
	for _, want := range wants {
		if !strings.Contains(log, want) {
			t.Fatalf("refusal log missing %q:\n%s", want, log)
		}
	}
	env.saveAll()
	if trades := ocStoredTrades(t, env, s.ID); len(trades) != 0 {
		t.Fatalf("stored trades = %+v, want none", trades)
	}
}

func TestOptionStructureEntryIntegration(t *testing.T) {
	t.Run("lossless_keys_for_fractional_strikes", func(t *testing.T) {
		sc := ocSC("ib-opt-keys", "ibkr", "BTC")
		env := ocEnv(t, sc)
		s := env.strat(sc.ID)
		logger, buf := ocLogger(sc.ID)
		n, err := ExecuteOptionsSignal(s, ocResult("ibkr", "BTC", 100, ocLeg("buy", "call", 101.5, 1), ocLeg("buy", "call", 102.5, 1), ocLeg("buy", "put", 100, 1)), logger)
		if err != nil || n != 3 {
			t.Fatalf("trades=%d err=%v; log:\n%s", n, err, buf.String())
		}
		for _, key := range []string{"BTC-call-buy-101.5-2026-12-18", "BTC-call-buy-102.5-2026-12-18", "BTC-put-buy-100-2026-12-18"} {
			if leg := s.OptionPositions[key]; leg == nil || leg.ID != key {
				t.Fatalf("leg %s missing from %v", key, ocBookSnapshot(s))
			}
		}
		env.saveAll()
		rows, err := env.fileFor(sc.ID).db.Query(`SELECT symbol FROM trades WHERE strategy_id = ? ORDER BY symbol`, sc.ID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var symbols []string
		for rows.Next() {
			var sym string
			if err := rows.Scan(&sym); err != nil {
				t.Fatal(err)
			}
			symbols = append(symbols, sym)
		}
		want := []string{"BTC-call-101.5-2026-12-18", "BTC-call-102.5-2026-12-18", "BTC-put-100-2026-12-18"}
		if !reflect.DeepEqual(symbols, want) {
			t.Fatalf("trade symbols = %v, want %v", symbols, want)
		}
		if !strings.Contains(buf.String(), "BUY OPTION BTC call strike=101.5 exp=2026-12-18") {
			t.Fatalf("entry log must carry the lossless strike:\n%s", buf.String())
		}
	})

	t.Run("repeated_or_occupied_contracts_refuse_the_whole_structure", func(t *testing.T) {
		held := func() map[string]*OptionPosition {
			return map[string]*OptionPosition{
				"legacy-held":                {ID: "legacy-held", Underlying: "btc", OptionType: "call", Strike: 150, Expiry: ocExpiry, Action: "buy", Quantity: 1, ContractMultiplier: 1, EntryPremiumUSD: 5, CurrentValueUSD: 5},
				"BTC-put-sell-80-2026-12-18": {ID: "BTC-put-sell-80-2026-12-18", Underlying: "BTC", OptionType: "put", Strike: 80, Expiry: ocExpiry, Action: "sell", Quantity: 1, ContractMultiplier: 1, EntryPremiumUSD: 3, CurrentValueUSD: -3},
			}
		}
		for _, tc := range []struct {
			name    string
			legacy  bool
			actions []OptionsAction
			want    string
		}{
			{"repeated_buy", false, []OptionsAction{ocLeg("buy", "put", 70, 1), ocLeg("buy", "call", 150, 1)}, "repeats open option leg legacy-held"},
			{"repeated_sell", false, []OptionsAction{ocLeg("buy", "call", 160, 1), ocLeg("sell", "put", 80, 1)}, "repeats open option leg BTC-put-sell-80-2026-12-18"},
			{"duplicate_inside_the_structure", false, []OptionsAction{ocLeg("buy", "call", 170, 1), ocLeg("buy", "call", 170, 2)}, "repeats leg 1 of the same structure"},
			{"occupied_legacy_destination", true, []OptionsAction{ocLeg("buy", "call", 175, 1), ocLeg("buy", "call", 120, 1)}, "destination key BTC-call-buy-120-2026-12-18 is already held by another option row"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				sc := ocSC("ib-opt-repeat", "ibkr", "BTC")
				env := ocEnv(t, sc)
				s := env.strat(sc.ID)
				s.Cash = 1000
				s.OptionPositions = held()
				if tc.legacy {
					s.OptionPositions["BTC-call-buy-120-2026-12-18"] = &OptionPosition{ID: "BTC-call-buy-120-2026-12-18", Underlying: "BTC", OptionType: "call", Strike: 119.5, Expiry: ocExpiry, Action: "buy", Quantity: 1, ContractMultiplier: 1, EntryPremiumUSD: 7, CurrentValueUSD: 7}
				}
				book := ocBookSnapshot(s)
				logger, buf := ocLogger(sc.ID)
				n, err := ExecuteOptionsSignal(s, ocResult("ibkr", "BTC", 100, tc.actions...), logger)
				if err != nil {
					t.Fatal(err)
				}
				ocAssertRefusedUnchanged(t, env, s, n, 1000, book, buf.String(), tc.want, "structure oc-1 leg 2/2")
			})
		}
	})

	t.Run("a_last_leg_failure_books_no_part_of_the_structure", func(t *testing.T) {
		two := func(last OptionsAction) []OptionsAction {
			return []OptionsAction{ocLeg("buy", "call", 100, 1), ocLeg("buy", "call", 110, 1), last}
		}
		withQty := func(a OptionsAction, q float64) OptionsAction { a.Quantity = q; return a }
		withExpiry := func(a OptionsAction, e string) OptionsAction { a.Expiry = e; return a }
		for _, tc := range []struct {
			name     string
			cash     float64
			existing map[string]*OptionPosition
			actions  []OptionsAction
			want     []string
		}{
			{name: "zero_premium", actions: two(ocLeg("sell", "put", 90, 0)), want: []string{"Zero premium, skipping option sell BTC put strike=90", "leg 3/3"}},
			{name: "non_finite_quantity", actions: two(withQty(ocLeg("buy", "put", 90, 1), math.NaN())), want: []string{"quantity NaN is not finite", "leg 3/3"}},
			{name: "non_finite_strike", actions: two(ocLeg("buy", "put", math.Inf(1), 1)), want: []string{"strike +Inf is not a finite positive number", "leg 3/3"}},
			{name: "unparseable_expiry", actions: two(withExpiry(ocLeg("buy", "put", 90, 1), "2026/12/18")), want: []string{`expiry "2026/12/18" is not a YYYY-MM-DD date`, "leg 3/3"}},
			{name: "free_cash_for_all_buys", actions: []OptionsAction{ocLeg("buy", "call", 100, 1), ocLeg("buy", "call", 110, 400), ocLeg("buy", "call", 120, 700)},
				want: []string{"Insufficient free cash for the 3 buys of option structure oc-1: required $1101.75", "leg 3/3"}},
			{name: "sell_premium_cannot_fund_buys", cash: 600, actions: []OptionsAction{ocLeg("sell", "call", 200, 500), ocLeg("buy", "call", 100, 1), ocLeg("buy", "call", 110, 700)},
				want: []string{"Insufficient free cash for the 2 buys of option structure oc-1", "before any premium from its sells", "leg 3/3"}},
			{name: "put_collateral", actions: two(ocLeg("sell", "put", 2000, 1)), want: []string{"Insufficient collateral for short put BTC strike=2000 qty=1: required $2000.00 (new $2000.00 + reserved $0.00), available cash $997.50", "leg 3/3"}},
			{name: "post_fee_reserve", actions: two(ocLeg("sell", "put", 997.5, 0.1)), want: []string{"would leave cash below reserved put collateral: required $997.50, available $997.35", "leg 3/3"}},
			{name: "repeat", existing: map[string]*OptionPosition{"held": {ID: "held", Underlying: "BTC", OptionType: "call", Strike: 150, Expiry: ocExpiry, Action: "buy", Quantity: 1, ContractMultiplier: 1, EntryPremiumUSD: 5, CurrentValueUSD: 5}},
				actions: two(ocLeg("buy", "call", 150, 1)), want: []string{"repeats open option leg held", "leg 3/3"}},
			{name: "destination_collision", existing: map[string]*OptionPosition{"BTC-call-buy-120-2026-12-18": {ID: "BTC-call-buy-120-2026-12-18", Underlying: "BTC", OptionType: "call", Strike: 119.5, Expiry: ocExpiry, Action: "buy", Quantity: 1, ContractMultiplier: 1, EntryPremiumUSD: 5, CurrentValueUSD: 5}},
				actions: two(ocLeg("buy", "call", 120, 1)), want: []string{"destination key BTC-call-buy-120-2026-12-18 is already held", "leg 3/3"}},
			{name: "row_cap", existing: map[string]*OptionPosition{
				"a": {ID: "a", Underlying: "BTC", OptionType: "call", Strike: 300, Expiry: ocExpiry, Action: "buy", Quantity: 1, ContractMultiplier: 1, EntryPremiumUSD: 5, CurrentValueUSD: 5},
				"b": {ID: "b", Underlying: "BTC", OptionType: "call", Strike: 310, Expiry: ocExpiry, Action: "buy", Quantity: 1, ContractMultiplier: 1, EntryPremiumUSD: 5, CurrentValueUSD: 5}},
				actions: two(ocLeg("buy", "put", 90, 1)), want: []string{"would hold 5 option rows with the 2 open, above the 4-row cap", "structure oc-1 refused"}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				sc := ocSC("ib-opt-last-leg", "ibkr", "BTC")
				env := ocEnv(t, sc)
				s := env.strat(sc.ID)
				s.Cash = 1000
				if tc.cash > 0 {
					s.Cash = tc.cash
				}
				for k, v := range tc.existing {
					s.OptionPositions[k] = v
				}
				cash, book := s.Cash, ocBookSnapshot(s)
				logger, buf := ocLogger(sc.ID)
				n, err := ExecuteOptionsSignal(s, ocResult("ibkr", "BTC", 100, tc.actions...), logger)
				if err != nil {
					t.Fatal(err)
				}
				ocAssertRefusedUnchanged(t, env, s, n, cash, book, buf.String(), tc.want...)
			})
		}
	})

	t.Run("row_cap_counts_rows_after_the_structure", func(t *testing.T) {
		sc := ocSC("ib-opt-cap", "ibkr", "BTC")
		env := ocEnv(t, sc)
		s := env.strat(sc.ID)
		for i, strike := range []float64{300, 310, 320} {
			id := fmt.Sprintf("open-%d", i)
			s.OptionPositions[id] = &OptionPosition{ID: id, Underlying: "BTC", OptionType: "call", Strike: strike, Expiry: ocExpiry, Action: "buy", Quantity: 1, ContractMultiplier: 1, EntryPremiumUSD: 5, CurrentValueUSD: 5}
		}
		logger, buf := ocLogger(sc.ID)
		if n, _ := ExecuteOptionsSignal(s, ocResult("ibkr", "BTC", 100, ocLeg("buy", "call", 100, 1), ocLeg("buy", "put", 90, 1)), logger); n != 0 || len(s.OptionPositions) != 3 {
			t.Fatalf("a two-row structure on three open rows booked %d; log:\n%s", n, buf.String())
		}
		withQty := ocLeg("buy", "call", 100, 1)
		withQty.Quantity = 2
		if n, _ := ExecuteOptionsSignal(s, ocResult("ibkr", "BTC", 100, withQty), logger); n != 1 || len(s.OptionPositions) != 4 {
			t.Fatalf("a one-row structure of quantity two must book as one row: trades=%d legs=%d; log:\n%s", n, len(s.OptionPositions), buf.String())
		}
		if leg := s.OptionPositions["BTC-call-buy-100-2026-12-18"]; leg == nil || leg.Quantity != 2 {
			t.Fatalf("quantity-two leg = %+v", leg)
		}
		if n, _ := ExecuteOptionsSignal(s, ocResult("ibkr", "BTC", 100, ocLeg("buy", "put", 80, 1)), logger); n != 0 || len(s.OptionPositions) != 4 {
			t.Fatalf("a fifth row booked: trades=%d legs=%d", n, len(s.OptionPositions))
		}
	})

	t.Run("independent_structures_plan_in_order", func(t *testing.T) {
		existingTwo := map[string]*OptionPosition{
			"a": {ID: "a", Underlying: "BTC", OptionType: "call", Strike: 300, Expiry: ocExpiry, Action: "buy", Quantity: 1, ContractMultiplier: 1, EntryPremiumUSD: 5, CurrentValueUSD: 5},
			"b": {ID: "b", Underlying: "BTC", OptionType: "call", Strike: 310, Expiry: ocExpiry, Action: "buy", Quantity: 1, ContractMultiplier: 1, EntryPremiumUSD: 5, CurrentValueUSD: 5},
		}
		for _, tc := range []struct {
			name      string
			existing  map[string]*OptionPosition
			first     []OptionsAction
			second    []OptionsAction
			firstKeys []string
			wantCash  float64
			want      string
		}{
			{name: "second_sees_the_first_cash", first: []OptionsAction{ocLeg("buy", "call", 100, 600)}, second: []OptionsAction{ocLeg("buy", "call", 200, 600)},
				firstKeys: []string{"BTC-call-buy-100-2026-12-18"}, wantCash: 1000 - 600.25, want: "available $399.75"},
			{name: "second_sees_the_first_contracts", first: []OptionsAction{ocLeg("buy", "call", 100, 1)}, second: []OptionsAction{ocLeg("buy", "call", 100, 1)},
				firstKeys: []string{"BTC-call-buy-100-2026-12-18"}, wantCash: 1000 - 1.25, want: "repeats open option leg BTC-call-buy-100-2026-12-18"},
			{name: "second_sees_the_first_rows", existing: existingTwo, first: []OptionsAction{ocLeg("buy", "call", 100, 1), ocLeg("buy", "call", 110, 1)}, second: []OptionsAction{ocLeg("buy", "put", 90, 1)},
				firstKeys: []string{"BTC-call-buy-100-2026-12-18", "BTC-call-buy-110-2026-12-18"}, wantCash: 1000 - 2.5, want: "above the 4-row cap"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				sc := ocSC("ib-opt-two", "ibkr", "BTC")
				env := ocEnv(t, sc)
				s := env.strat(sc.ID)
				s.Cash = 1000
				for k, v := range tc.existing {
					s.OptionPositions[k] = v
				}
				logger, buf := ocLogger(sc.ID)
				actions := append(ocStructure("first", tc.first...), ocStructure("second", tc.second...)...)
				n, err := ExecuteOptionsSignal(s, ocRaw("ibkr", "BTC", 100, actions...), logger)
				if err != nil || n != len(tc.first) || !ocNear(s.Cash, tc.wantCash) || len(s.OptionPositions) != len(tc.existing)+len(tc.first) {
					t.Fatalf("trades=%d cash=%v legs=%d err=%v; log:\n%s", n, s.Cash, len(s.OptionPositions), err, buf.String())
				}
				for _, key := range tc.firstKeys {
					if s.OptionPositions[key] == nil {
						t.Fatalf("first structure leg %s missing", key)
					}
				}
				if !strings.Contains(buf.String(), tc.want) || !strings.Contains(buf.String(), "structure second") {
					t.Fatalf("second refusal not logged:\n%s", buf.String())
				}
				env.saveAll()
				if trades := ocStoredTrades(t, env, sc.ID); len(trades) != len(tc.first) {
					t.Fatalf("stored trades = %+v, want only the first structure", trades)
				}
			})
		}
		for _, tc := range []struct {
			name string
			held float64
			want int
		}{
			{name: "robinhood_enough_aggregate_cover", held: 200, want: 2},
			{name: "robinhood_short_aggregate_cover_refuses_every_entry", held: 100, want: 0},
		} {
			t.Run(tc.name, func(t *testing.T) {
				sc := ocSC("rh-opt-two", "robinhood", "SPY")
				env := ocEnv(t, sc)
				s := env.strat(sc.ID)
				s.Positions["SPY"] = &Position{Symbol: "SPY", Side: "long", Quantity: tc.held, AvgCost: 450, OwnerStrategyID: sc.ID}
				logger, buf := ocLogger(sc.ID)
				actions := append(ocStructure("first", OptionsAction{Action: "sell", OptionType: "call", Strike: 600, Expiry: ocExpiry, PremiumUSD: 1}),
					ocStructure("second", OptionsAction{Action: "sell", OptionType: "call", Strike: 610, Expiry: ocExpiry, PremiumUSD: 1})...)
				n, err := ExecuteOptionsSignal(s, ocRaw("robinhood", "SPY", 580, actions...), logger)
				if err != nil || n != tc.want || len(s.OptionPositions) != tc.want {
					t.Fatalf("trades=%d legs=%d err=%v, want %d; log:\n%s", n, len(s.OptionPositions), err, tc.want, buf.String())
				}
				if tc.want == 0 && (!strings.Contains(buf.String(), "refusing every entry action in this signal") || s.Cash != 100000) {
					t.Fatalf("aggregate cover refusal changed cash %v or did not log:\n%s", s.Cash, buf.String())
				}
			})
		}
	})

	t.Run("malformed_structures_book_no_partial_entry", func(t *testing.T) {
		closeAction := OptionsAction{Action: "close", OptionType: "put", Strike: 80, PremiumUSD: 1}
		good := ocStructure("h", ocLeg("buy", "call", 130, 1))
		for _, tc := range []struct {
			name     string
			marker   string
			actions  []OptionsAction
			goodBook bool
			want     string
		}{
			{name: "missing_marker", marker: "", actions: append(ocStructure("g", ocLeg("buy", "call", 100, 1), ocLeg("buy", "call", 110, 1)), good...), want: `structure_contract "", want "options_structures_v1"`},
			{name: "wrong_marker", marker: "options_structures_v0", actions: append(ocStructure("g", ocLeg("buy", "call", 100, 1), ocLeg("buy", "call", 110, 1)), good...), want: `structure_contract "options_structures_v0"`},
			{name: "empty_id", marker: optionsStructureContractV1, actions: append([]OptionsAction{ocLeg("buy", "call", 100, 1)}, good...), want: "has no structure_id, so no group can be identified"},
			{name: "missing_leg_total", marker: optionsStructureContractV1, goodBook: true, actions: append([]OptionsAction{
				{Action: "buy", OptionType: "call", Strike: 100, Expiry: ocExpiry, PremiumUSD: 1, StructureID: "g", StructureLegs: 2},
				{Action: "buy", OptionType: "call", Strike: 110, Expiry: ocExpiry, PremiumUSD: 1, StructureID: "g"}}, good...), want: `structure "g" refused (2 leg(s))`},
			{name: "inconsistent_leg_total", marker: optionsStructureContractV1, goodBook: true, actions: append([]OptionsAction{
				{Action: "buy", OptionType: "call", Strike: 100, Expiry: ocExpiry, PremiumUSD: 1, StructureID: "g", StructureLegs: 3},
				{Action: "buy", OptionType: "call", Strike: 110, Expiry: ocExpiry, PremiumUSD: 1, StructureID: "g", StructureLegs: 3}}, good...), want: "structure_legs 3 does not match its 2 member(s)"},
			{name: "truncated_group", marker: optionsStructureContractV1, goodBook: true, actions: append([]OptionsAction{
				{Action: "buy", OptionType: "call", Strike: 100, Expiry: ocExpiry, PremiumUSD: 1, StructureID: "g", StructureLegs: 2}}, good...), want: "structure_legs 2 does not match its 1 member(s)"},
			{name: "unknown_grouped_action", marker: optionsStructureContractV1, goodBook: true, actions: append([]OptionsAction{
				{Action: "buy", OptionType: "call", Strike: 100, Expiry: ocExpiry, PremiumUSD: 1, StructureID: "g", StructureLegs: 2},
				{Action: "roll", OptionType: "call", Strike: 110, Expiry: ocExpiry, PremiumUSD: 1, StructureID: "g", StructureLegs: 2}}, good...), want: `action 2 ("roll") is not buy, sell or close`},
			{name: "unknown_ungrouped_action", marker: optionsStructureContractV1, actions: append([]OptionsAction{{Action: "roll", OptionType: "call", Strike: 110}}, good...), want: `action 1 ("roll") has no structure_id`},
			{name: "invalid_option_type", marker: optionsStructureContractV1, goodBook: true, actions: append([]OptionsAction{
				{Action: "buy", OptionType: "call", Strike: 100, Expiry: ocExpiry, PremiumUSD: 1, StructureID: "g", StructureLegs: 2},
				{Action: "buy", OptionType: "straddle", Strike: 110, Expiry: ocExpiry, PremiumUSD: 1, StructureID: "g", StructureLegs: 2}}, good...), want: `option_type "straddle" is not call or put`},
		} {
			t.Run(tc.name, func(t *testing.T) {
				sc := ocSC("ib-opt-malformed", "ibkr", "BTC")
				env := ocEnv(t, sc)
				s := env.strat(sc.ID)
				s.Cash = 1000
				s.OptionPositions["BTC-put-sell-80-2026-12-18"] = &OptionPosition{ID: "BTC-put-sell-80-2026-12-18", Underlying: "BTC", OptionType: "put", Strike: 80, Expiry: ocExpiry, Action: "sell", Quantity: 1, ContractMultiplier: 1, EntryPremiumUSD: 3, CurrentValueUSD: -3}
				r := ocRaw("ibkr", "BTC", 100, append(append([]OptionsAction(nil), tc.actions...), closeAction)...)
				r.StructureContract = tc.marker
				logger, buf := ocLogger(sc.ID)
				n, err := ExecuteOptionsSignal(s, r, logger)
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(buf.String(), "Options structure refused: ") || !strings.Contains(buf.String(), tc.want) {
					t.Fatalf("structure refusal not logged with %q:\n%s", tc.want, buf.String())
				}
				if s.OptionPositions["BTC-put-sell-80-2026-12-18"] != nil || len(s.ClosedOptionPositions) != 1 {
					t.Fatalf("the explicit close must still run: legs=%v closed=%d", ocBookSnapshot(s), len(s.ClosedOptionPositions))
				}
				for _, strike := range []string{"100", "110"} {
					if s.OptionPositions["BTC-call-buy-"+strike+"-2026-12-18"] != nil {
						t.Fatalf("a leg of the refused group booked: %v", ocBookSnapshot(s))
					}
				}
				wantTrades := 1
				if tc.goodBook {
					wantTrades = 2
				}
				if (s.OptionPositions["BTC-call-buy-130-2026-12-18"] != nil) != tc.goodBook || n != wantTrades {
					t.Fatalf("independent valid structure booked=%t trades=%d, want %t; log:\n%s", s.OptionPositions["BTC-call-buy-130-2026-12-18"] != nil, n, tc.goodBook, buf.String())
				}
			})
		}
	})

	t.Run("tolerant_decoding_refuses_only_the_named_group", func(t *testing.T) {
		for _, tc := range []struct {
			name, legs, id string
			whole          bool
		}{
			{name: "string_total", legs: `"2"`, id: `"g"`},
			{name: "bool_total", legs: `true`, id: `"g"`},
			{name: "fraction_total", legs: `1.5`, id: `"g"`},
			{name: "zero_total", legs: `0`, id: `"g"`},
			{name: "negative_total", legs: `-2`, id: `"g"`},
			{name: "null_total", legs: `null`, id: `"g"`},
			{name: "huge_total", legs: `1e300`, id: `"g"`},
			{name: "numeric_id", legs: `2`, id: `7`, whole: true},
			{name: "empty_id", legs: `2`, id: `""`, whole: true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				doc := `{"underlying":"BTC","platform":"ibkr","premium_unit":"underlying_unit","structure_contract":"options_structures_v1","signal":1,"spot_price":100,"actions":[` +
					`{"action":"buy","option_type":"call","strike":100,"expiry":"2026-12-18","premium_usd":1,"structure_id":"g","structure_legs":2},` +
					`{"action":"buy","option_type":"call","strike":110,"expiry":"2026-12-18","premium_usd":1,"structure_id":` + tc.id + `,"structure_legs":` + tc.legs + `},` +
					`{"action":"buy","option_type":"call","strike":130,"expiry":"2026-12-18","premium_usd":1,"structure_id":"h","structure_legs":1},` +
					`{"action":"close","option_type":"put","strike":80,"premium_usd":1}]}`
				var r OptionsResult
				if err := json.Unmarshal([]byte(doc), &r); err != nil {
					t.Fatalf("a malformed structure field must not fail the whole result: %v", err)
				}
				sc := ocSC("ib-opt-decode", "ibkr", "BTC")
				env := ocEnv(t, sc)
				s := env.strat(sc.ID)
				s.OptionPositions["BTC-put-sell-80-2026-12-18"] = &OptionPosition{ID: "BTC-put-sell-80-2026-12-18", Underlying: "BTC", OptionType: "put", Strike: 80, Expiry: ocExpiry, Action: "sell", Quantity: 1, ContractMultiplier: 1, EntryPremiumUSD: 3, CurrentValueUSD: -3}
				logger, buf := ocLogger(sc.ID)
				n, err := ExecuteOptionsSignal(s, &r, logger)
				if err != nil {
					t.Fatal(err)
				}
				goodBooked := s.OptionPositions["BTC-call-buy-130-2026-12-18"] != nil
				if s.OptionPositions["BTC-call-buy-100-2026-12-18"] != nil || s.OptionPositions["BTC-call-buy-110-2026-12-18"] != nil || goodBooked == tc.whole {
					t.Fatalf("legs=%v whole=%t; log:\n%s", ocBookSnapshot(s), tc.whole, buf.String())
				}
				if s.OptionPositions["BTC-put-sell-80-2026-12-18"] != nil || n != 1+boolInt(goodBooked) {
					t.Fatalf("the explicit close must run: trades=%d legs=%v", n, ocBookSnapshot(s))
				}
			})
		}
	})

	t.Run("exposure_cap_and_holds_drop_whole_structures", func(t *testing.T) {
		sc := ocSC("ib-opt-filter", "ibkr", "BTC")
		butterfly := ocStructure("fly",
			OptionsAction{Action: "buy", OptionType: "call", Strike: 95, Expiry: ocExpiry, PremiumUSD: 6, Greeks: OptGreeks{Delta: 0.7}},
			OptionsAction{Action: "sell", OptionType: "call", Strike: 100, Expiry: ocExpiry, PremiumUSD: 3, Quantity: 2, Greeks: OptGreeks{Delta: 0.5}},
			OptionsAction{Action: "buy", OptionType: "call", Strike: 105, Expiry: ocExpiry, PremiumUSD: 1, Greeks: OptGreeks{Delta: 0.3}})
		shortOnly := ocStructure("short", OptionsAction{Action: "sell", OptionType: "call", Strike: 140, Expiry: ocExpiry, PremiumUSD: 1, Greeks: OptGreeks{Delta: 0.2}})
		closeAction := OptionsAction{Action: "close", OptionType: "put", Strike: 80, PremiumUSD: 1}
		build := func() *OptionsResult {
			actions := append(append(append([]OptionsAction(nil), butterfly...), shortOnly...), closeAction)
			return ocRaw("ibkr", "BTC", 100, actions...)
		}
		names := func(actions []OptionsAction) []string {
			var out []string
			for _, a := range actions {
				out = append(out, a.Action+":"+a.StructureID)
			}
			return out
		}
		logger, buf := ocLogger(sc.ID)
		r := build()
		filterOptionsEntryActions(optionsDispatch{SC: sc, Logger: logger, Risk: &scopeCycleRisk{Partition: partitionFor(sc),
			ExposureCapStatus: ExposureCapStatus{Configured: true, CapUSD: 1000, LongUSD: 2000, LongBlocked: true}}}, r)
		if got := names(r.Actions); !reflect.DeepEqual(got, []string{"sell:short", "close:"}) {
			t.Fatalf("long block kept %v; log:\n%s", got, buf.String())
		}
		if !strings.Contains(buf.String(), "Exposure cap: 3 option open action(s) in 1 structure(s) dropped whole") {
			t.Fatalf("whole-structure exposure drop not logged:\n%s", buf.String())
		}
		for _, hold := range []struct {
			name string
			d    optionsDispatch
		}{
			{"paused", optionsDispatch{SC: func() StrategyConfig { c := sc; c.Paused = true; return c }(), Risk: &scopeCycleRisk{}}},
			{"daily_loss", optionsDispatch{SC: sc, Risk: &scopeCycleRisk{DailyLossEntriesHeld: true}}},
			{"notional", optionsDispatch{SC: sc, Risk: &scopeCycleRisk{NotionalBlocked: true}}},
			{"persistence", optionsDispatch{SC: sc, Risk: &scopeCycleRisk{}, PersistenceHold: true}},
		} {
			hold.d.Logger = logger
			r := build()
			filterOptionsEntryActions(hold.d, r)
			if got := names(r.Actions); !reflect.DeepEqual(got, []string{"close:"}) {
				t.Fatalf("%s hold kept %v", hold.name, got)
			}
		}
	})

	t.Run("accepted_entries_commit_with_the_owning_file_save", func(t *testing.T) {
		dir := t.TempDir()
		def := ocSC("ib-opt-persist", "ibkr", "BTC")
		src := ocSC("ib-opt-persist-src", "ibkr", "BTC")
		src.PaperSource = "src"
		src.StorageStrategyID = "src-store"
		cfg := &Config{DBFile: filepath.Join(dir, "state.db"), IntervalSeconds: 600, PaperSources: []PaperSourceConfig{{ID: "src", DBFile: filepath.Join(dir, "src.db")}},
			Strategies: []StrategyConfig{def, src}}
		env := openEntryTestEnv(t, cfg)
		useEntryTradeRecorder(t, env.store)
		env.saveAll()
		storage := map[string]string{def.ID: def.ID, src.ID: "src-store"}
		fly := func() *OptionsResult {
			return ocResult("ibkr", "BTC", 100, ocLeg("buy", "call", 95, 6), OptionsAction{Action: "sell", OptionType: "call", Strike: 100, Expiry: ocExpiry, PremiumUSD: 3, Quantity: 2}, ocLeg("buy", "call", 105, 1))
		}
		wantCash := 100000 - 6.25 + (6 - 0.5) - 1.25
		for _, id := range []string{def.ID, src.ID} {
			logger, buf := ocLogger(id)
			if n, err := ExecuteOptionsSignal(env.strat(id), fly(), logger); err != nil || n != 3 {
				t.Fatalf("%s trades=%d err=%v; log:\n%s", id, n, err, buf.String())
			}
			if cash, legs, trades := ocDurable(t, env, id, storage[id]); cash != 100000 || legs != 0 || trades != 0 {
				t.Fatalf("%s accepted entries wrote before the save: cash=%v legs=%d trades=%d", id, cash, legs, trades)
			}
			if got := ocUnpersisted(env.strat(id)); got != 3 || !ocNear(env.strat(id).Cash, wantCash) {
				t.Fatalf("%s unpersisted trades=%d cash=%v", id, got, env.strat(id).Cash)
			}
		}

		layout, _ := resolveStorageLayout(cfg)
		ident, _ := buildStorageIdentityMap(cfg, layout)
		ro, err := OpenStateStoreReadOnly(layout, ident)
		if err != nil {
			t.Fatal(err)
		}
		roState, _, err := LoadStateWithStore(cfg, ro)
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{def.ID, src.ID} {
			if b := roState.Strategies[id]; b == nil || b.Cash != 100000 || len(b.OptionPositions) != 0 || len(b.TradeHistory) != 0 {
				t.Fatalf("a stop before the save must reload the previous book for %s: %+v", id, b)
			}
		}
		ro.Close()

		failRole := paperSourceRole("src")
		prev := storeCommitHook
		t.Cleanup(func() { storeCommitHook = prev })
		storeCommitHook = func(role storageRole) error {
			if role == failRole {
				return errors.New("disk full")
			}
			return nil
		}
		results := env.store.SaveAll(env.state)
		if results[partitionFor(src)] == nil || results[partitionFor(def)] != nil {
			t.Fatalf("save results = %v, want only the source file to fail", results)
		}
		if cash, legs, trades := ocDurable(t, env, src.ID, "src-store"); cash != 100000 || legs != 0 || trades != 0 {
			t.Fatalf("a failed source commit changed its file: cash=%v legs=%d trades=%d", cash, legs, trades)
		}
		if cash, legs, trades := ocDurable(t, env, def.ID, def.ID); !ocNear(cash, wantCash) || legs != 3 || trades != 3 {
			t.Fatalf("the peer file must commit its own structure: cash=%v legs=%d trades=%d", cash, legs, trades)
		}
		if s := env.strat(src.ID); len(s.OptionPositions) != 3 || ocUnpersisted(s) != 3 || !ocNear(s.Cash, wantCash) {
			t.Fatalf("a failed save must keep the accepted structure in memory: legs=%d unpersisted=%d cash=%v", len(s.OptionPositions), ocUnpersisted(s), s.Cash)
		}
		if !env.store.persistenceHoldsPartition(partitionFor(src)) || env.store.persistenceHoldsPartition(partitionFor(def)) {
			t.Fatal("only the failed partition must hold entries")
		}
		next := ocResult("ibkr", "BTC", 100, ocLeg("buy", "put", 90, 1))
		logger, buf := ocLogger(src.ID)
		filterOptionsEntryActions(optionsDispatch{SC: src, Logger: logger, Risk: &scopeCycleRisk{Partition: partitionFor(src)},
			PersistenceHold: env.store.persistenceHoldsPartition(partitionFor(src)), Scope: partitionFor(src).Scope}, next)
		if n, _ := ExecuteOptionsSignal(env.strat(src.ID), next, logger); n != 0 || len(env.strat(src.ID).OptionPositions) != 3 {
			t.Fatalf("the next dispatch must hold entries after a failed save: trades=%d; log:\n%s", n, buf.String())
		}

		storeCommitHook = prev
		env.saveAll()
		env.saveAll()
		for _, id := range []string{def.ID, src.ID} {
			if cash, legs, trades := ocDurable(t, env, id, storage[id]); !ocNear(cash, wantCash) || legs != 3 || trades != 3 {
				t.Fatalf("%s retried save: cash=%v legs=%d trades=%d, want the structure once", id, cash, legs, trades)
			}
		}
		env = env.restart()
		for _, id := range []string{def.ID, src.ID} {
			s := env.strat(id)
			if len(s.OptionPositions) != 3 || !ocNear(s.Cash, wantCash) || len(s.TradeHistory) != 3 {
				t.Fatalf("%s restart: legs=%d cash=%v trades=%d", id, len(s.OptionPositions), s.Cash, len(s.TradeHistory))
			}
		}
	})

	t.Run("a_failed_book_save_keeps_the_structure_for_retry", func(t *testing.T) {
		sc := ocSC("ib-opt-book", "ibkr", "BTC")
		env := ocEnv(t, sc)
		env.saveAll()
		s := env.strat(sc.ID)
		logger, _ := ocLogger(sc.ID)
		if n, err := ExecuteOptionsSignal(s, ocResult("ibkr", "BTC", 100, ocLeg("buy", "call", 95, 6), ocLeg("buy", "call", 105, 1)), logger); err != nil || n != 2 {
			t.Fatalf("trades=%d err=%v", n, err)
		}
		prev := storeCommitHook
		t.Cleanup(func() { storeCommitHook = prev })
		storeCommitHook = func(storageRole) error { return errors.New("disk full") }
		if err := env.store.SaveStrategyBook(s); err == nil {
			t.Fatal("the forced book save must fail")
		}
		if cash, legs, trades := ocDurable(t, env, sc.ID, sc.ID); cash != 100000 || legs != 0 || trades != 0 {
			t.Fatalf("a failed book save changed the file: cash=%v legs=%d trades=%d", cash, legs, trades)
		}
		if len(s.OptionPositions) != 2 || ocUnpersisted(s) != 2 {
			t.Fatalf("a failed book save dropped the structure: legs=%d unpersisted=%d", len(s.OptionPositions), ocUnpersisted(s))
		}
		storeCommitHook = prev
		if err := env.store.SaveStrategyBook(s); err != nil {
			t.Fatal(err)
		}
		if err := env.store.SaveStrategyBook(s); err != nil {
			t.Fatal(err)
		}
		if cash, legs, trades := ocDurable(t, env, sc.ID, sc.ID); !ocNear(cash, 100000-7.5) || legs != 2 || trades != 2 {
			t.Fatalf("retried book save: cash=%v legs=%d trades=%d", cash, legs, trades)
		}
		env = env.restart()
		if b := env.strat(sc.ID); len(b.OptionPositions) != 2 || len(b.TradeHistory) != 2 || !ocNear(b.Cash, 100000-7.5) {
			t.Fatalf("restart: legs=%d trades=%d cash=%v", len(b.OptionPositions), len(b.TradeHistory), b.Cash)
		}
	})
}

func TestOptionLegacyKeyNormalizationIntegration(t *testing.T) {
	const e = ocExpiry
	leg := func(id, optionType, action string, strike float64, n int) *OptionPosition {
		return &OptionPosition{ID: id, TradePositionID: "tp-" + id, Underlying: "BTC", OptionType: optionType, Strike: strike, Expiry: e, DTE: 30,
			Action: action, Quantity: float64(n), ContractMultiplier: 1, EntryPremium: 0.01 * float64(n), EntryPremiumUSD: 10 * float64(n),
			CurrentValueUSD: 11 * float64(n), Greeks: OptGreeks{Delta: 0.1 * float64(n), Gamma: 0.01, Theta: -0.5, Vega: 0.2},
			OpenedAt: time.Unix(1000+int64(n), 0).UTC(), MarkUpdatedAt: time.Unix(2000+int64(n), 0).UTC(), MarkSource: "seed",
			ExpiryCutoffAt: time.Unix(3000, 0).UTC(), SettlementReadyAt: time.Unix(4000, 0).UTC()}
	}
	seed := func() map[string]*OptionPosition {
		return map[string]*OptionPosition{
			"BTC-call-buy-100000-" + e: leg("BTC-call-buy-100000-"+e, "call", "buy", 100000, 1),
			"BTC-call-sell-102-" + e:   leg("BTC-call-sell-102-"+e, "call", "sell", 101.5, 2),
			"BTC-call-buy-102-" + e:    leg("BTC-call-buy-102-"+e, "call", "buy", 102.5, 3),
			"my-leg":                   leg("my-leg", "call", "buy", 99.5, 4),
			"BTC-put-sell-90-" + e:     leg("BTC-put-sell-90-"+e, "put", "sell", 90.4, 5),
			"legacy-dup":               leg("legacy-dup", "put", "sell", 90.4, 6),
			"BTC-put-buy-81-" + e:      leg("BTC-put-buy-81-"+e, "put", "buy", 81.4, 7),
			"BTC-put-buy-81.4-" + e:    leg("BTC-put-buy-81.4-"+e, "put", "buy", 70, 8),
			"BTC-call-buy-55.5-" + e:   leg("BTC-call-buy-55.5-"+e, "call", "buy", 60, 9),
			"dup-two":                  leg("dup-two", "call", "buy", 60, 10),
			"BTC-call-buy-56-" + e:     leg("BTC-call-buy-56-"+e, "call", "buy", 55.5, 11),
		}
	}
	renamed := map[string]string{
		"BTC-call-sell-102-" + e: "BTC-call-sell-101.5-" + e,
		"BTC-call-buy-102-" + e:  "BTC-call-buy-102.5-" + e,
	}
	finalKey := func(old string) string {
		if k, ok := renamed[old]; ok {
			return k
		}
		return old
	}
	for _, order := range []string{"saved_order", "reversed_order"} {
		t.Run(order, func(t *testing.T) {
			sc := ocSC("ib-opt-normalize", "ibkr", "BTC")
			sc.StorageStrategyID = "normalize-store"
			env := ocEnv(t, sc)
			s := env.strat(sc.ID)
			s.Cash = 4321.5
			s.OptionPositions = seed()
			s.RiskState.setPendingCircuitClose(PlatformPendingCloseRobinhoodOptions, &PendingCircuitClose{OperatorRequired: true, ConsecutiveFailures: 2, Episode: "orc-x",
				FiredAt: time.Unix(5000, 0).UTC(), LastNotifiedAt: time.Unix(5100, 0).UTC(),
				Symbols: []PendingCircuitCloseSymbol{{Symbol: "BTC-call-sell-102-" + e, Size: 2, PositionID: "tp-BTC-call-sell-102-" + e}, {Symbol: "my-leg", Size: 4}}})
			s.RiskState.setPendingCircuitClose(PlatformPendingCloseOKXSpot, &PendingCircuitClose{OperatorRequired: true, Symbols: []PendingCircuitCloseSymbol{{Symbol: "BTC-call-sell-102-" + e, Size: 1}}})
			RecordTrade(s, Trade{Timestamp: time.Unix(1500, 0).UTC(), StrategyID: s.ID, Symbol: "BTC-call-sell-102-" + e, PositionID: "tp-BTC-call-sell-102-" + e, Side: "sell", Quantity: 2, Price: 20, Value: 20, TradeType: "options"})
			recordClosedOptionPosition(s, leg("BTC-call-sell-103-"+e, "call", "sell", 102.6, 1), 5, 5, "signal", time.Unix(1600, 0).UTC())
			env.saveAll()
			db := env.fileFor(sc.ID).db
			if order == "reversed_order" {
				for _, q := range []string{
					`CREATE TEMP TABLE opt_copy AS SELECT * FROM option_positions`,
					`DELETE FROM option_positions`,
					`INSERT INTO option_positions SELECT * FROM opt_copy ORDER BY rowid DESC`,
					`DROP TABLE opt_copy`,
				} {
					if _, err := db.Exec(q); err != nil {
						t.Fatalf("%s: %v", q, err)
					}
				}
			}
			env.store.Close()

			sum := func() [32]byte {
				b, err := os.ReadFile(env.cfg.DBFile)
				if err != nil {
					t.Fatal(err)
				}
				return sha256.Sum256(b)
			}
			before := sum()
			layout, _ := resolveStorageLayout(env.cfg)
			ident, _ := buildStorageIdentityMap(env.cfg, layout)
			ro, err := OpenStateStoreReadOnly(layout, ident)
			if err != nil {
				t.Fatal(err)
			}
			roState, _, err := LoadStateWithStore(env.cfg, ro)
			if err != nil {
				t.Fatal(err)
			}
			if roState.Strategies[sc.ID].OptionPositions["BTC-call-sell-101.5-"+e] == nil {
				t.Fatalf("read-only load must normalize in memory: %v", ocBookSnapshot(roState.Strategies[sc.ID]))
			}
			ro.Close()
			if sum() != before {
				t.Fatal("a read-only load changed the state file")
			}

			check := func(env *entryTestEnv, stage string) {
				t.Helper()
				b := env.strat(sc.ID)
				want := seed()
				if len(b.OptionPositions) != len(want) || b.Cash != 4321.5 {
					t.Fatalf("%s: legs=%d cash=%v, want %d legs and the stored cash", stage, len(b.OptionPositions), b.Cash, len(want))
				}
				for old, w := range want {
					key := finalKey(old)
					got := b.OptionPositions[key]
					if got == nil {
						t.Fatalf("%s: leg %s (stored %s) missing; book keys %v", stage, key, old, sortedOptionLegKeys(b.OptionPositions))
					}
					w.ID = key
					g := *got
					g.markVerifiedAt = time.Time{}
					if !reflect.DeepEqual(g, *w) {
						t.Fatalf("%s: leg %s = %+v, want %+v", stage, key, g, *w)
					}
				}
				pending := b.RiskState.getPendingCircuitClose(PlatformPendingCloseRobinhoodOptions)
				wantPending := []PendingCircuitCloseSymbol{{Symbol: "BTC-call-sell-101.5-" + e, Size: 2, PositionID: "tp-BTC-call-sell-102-" + e}, {Symbol: "my-leg", Size: 4}}
				if pending == nil || !reflect.DeepEqual(pending.Symbols, wantPending) || !pending.OperatorRequired || pending.ConsecutiveFailures != 2 || pending.Episode != "orc-x" ||
					!pending.FiredAt.Equal(time.Unix(5000, 0)) || !pending.LastNotifiedAt.Equal(time.Unix(5100, 0)) {
					t.Fatalf("%s: robinhood_options pending = %+v", stage, pending)
				}
				for _, p := range pending.Symbols {
					if b.OptionPositions[p.Symbol] == nil {
						t.Fatalf("%s: pending close names absent leg %s", stage, p.Symbol)
					}
				}
				if other := b.RiskState.getPendingCircuitClose(PlatformPendingCloseOKXSpot); other == nil || other.Symbols[0].Symbol != "BTC-call-sell-102-"+e {
					t.Fatalf("%s: another platform's pending close changed: %+v", stage, other)
				}
				var tradeSym, closedID string
				fdb := env.fileFor(sc.ID).db
				if err := fdb.QueryRow(`SELECT symbol FROM trades WHERE strategy_id = 'normalize-store'`).Scan(&tradeSym); err != nil || tradeSym != "BTC-call-sell-102-"+e {
					t.Fatalf("%s: historical trade symbol = %q, %v", stage, tradeSym, err)
				}
				if err := fdb.QueryRow(`SELECT position_id FROM closed_option_positions WHERE strategy_id = 'normalize-store'`).Scan(&closedID); err != nil || closedID != "BTC-call-sell-103-"+e {
					t.Fatalf("%s: closed record id = %q, %v", stage, closedID, err)
				}
			}

			env = openEntryTestEnv(t, env.cfg)
			check(env, "first load")
			env.saveAll()
			var stored []string
			rows, err := env.fileFor(sc.ID).db.Query(`SELECT id FROM option_positions WHERE strategy_id = 'normalize-store' ORDER BY id`)
			if err != nil {
				t.Fatal(err)
			}
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err != nil {
					t.Fatal(err)
				}
				stored = append(stored, id)
			}
			rows.Close()
			var want []string
			for old := range seed() {
				want = append(want, finalKey(old))
			}
			sort.Strings(want)
			if !reflect.DeepEqual(stored, want) {
				t.Fatalf("stored ids = %v, want %v", stored, want)
			}
			env = env.restart()
			check(env, "reload after save")
		})
	}
}
