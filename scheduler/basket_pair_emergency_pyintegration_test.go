//go:build pyintegration

package main

import (
	"testing"
	"time"
)

func TestBasketPairEmergencyRealCloseScriptsBookFillEvidenceOnce(t *testing.T) {
	for _, tc := range []struct {
		name string
		drop bool
	}{{name: "complete fill records", drop: false}, {name: "missing fill records", drop: true}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPairVenuePy(t)
			closers := &pairNativeClosers{
				hl: func(sym string, p *float64, c []int64) (*HyperliquidCloseResult, error) {
					res, _, err := RunHyperliquidClose(f.drivers["hl_close"], sym, p, c)
					return res, err
				},
				okx: func(sym string, p *float64) (*OKXCloseResult, error) {
					res, _, err := RunOKXClose(f.drivers["okx_close"], sym, p)
					return res, err
				},
			}
			e := newPairLiveShapedEnv(t, closers)
			pairLiveMatched(t, e)
			now := e.now.UnixMilli()
			f.write("hyperliquid", map[string]any{"now_ms": now, "close_position": map[string]string{"BTC": "0.06"}, "fill_px": "59900", "fee": "0.9",
				"close_parts": []string{"0.02", "0.04"}, "drop_fills": tc.drop, "user_state": map[string]any{}})
			f.write("okx", map[string]any{"now_ms": now, "close_positions": []map[string]any{{"contracts": 6, "side": "short"}}, "fill_px": "60050", "fee": "-0.3",
				"close_parts": []string{"2", "4"}, "drop_fills": tc.drop})
			hlRes, hlErr := e.bc.pairKillSwitchHLCloser(closers.hl)("BTC", nil, nil)
			okxRes, okxErr := e.bc.pairKillSwitchOKXCloser(closers.okx)("BTC", nil)
			if hlErr != nil || okxErr != nil || hlRes == nil || okxRes == nil || okxRes.Close == nil || okxRes.Close.Fill == nil {
				t.Fatalf("the real close scripts must answer: %v %v %+v %+v", hlErr, okxErr, hlRes, okxRes)
			}
			if okxRes.Close.Fill.TotalSz != 6 {
				t.Fatalf("the ordinary okx close result must stay in contracts, got %v", okxRes.Close.Fill.TotalSz)
			}
			wantBase := 0.06
			if tc.drop {
				wantBase = 0
			}
			if !basketQtyEqual(okxRes.Close.Fill.BaseSz, wantBase) {
				t.Fatalf("the pair boundary must convert six contracts at size 0.01 to %v base once, got %v", wantBase, okxRes.Close.Fill.BaseSz)
			}
			e.step(e.now.Add(time.Second))
			e.step(e.now.Add(time.Second))
			hlWire, okxWire := f.read("hyperliquid")["wire"].([]any), f.read("okx")["wire"].([]any)
			if len(hlWire) != 1 || len(okxWire) != 1 {
				t.Fatalf("each native close must send once and accounting must send nothing: %d %d", len(hlWire), len(okxWire))
			}
			for _, p := range []string{"hyperliquid", "okx"} {
				if g := f.read(p)["guard"]; g != nil {
					t.Fatalf("the %s close script reached a refused network or signing call: %v", p, g)
				}
			}
			if tc.drop {
				e.assertBooks("missing fill records hold both books", 0.06, -0.06)
				for _, id := range []string{pairTestHL, pairTestOKX} {
					if !e.hasObligation(pairObligationNativeUnknown, id) {
						t.Fatalf("%s: a close without fill records must keep an unknown-fill obligation: %v", id, e.obligationKinds(id))
					}
				}
				if got, ok := pairIncompleteEvidence(e.rt()); !ok || got.Kind != pairObligationNativeUnknown {
					t.Fatalf("the incomplete-result reader must report the unknown native fill, got %+v %v", got, ok)
				}
				return
			}
			e.assertBooks("real native closes book from their fill records", 0, 0)
			for _, c := range []struct {
				id       string
				px, fee  float64
				fills    int
				orderPfx string
			}{{pairTestHL, 59900, 0.9 * 2, 2, "95"}, {pairTestOKX, 60050, 0.3 * 2, 2, "75"}} {
				tr, ok := e.closeTrade(c.id)
				if !ok || !basketQtyEqual(tr.Quantity, 0.06) || !pairCostNear(tr.Price, c.px) || !pairCostNear(tr.ExchangeFee, c.fee) || tr.FeeSource != FeeSourceUserFills {
					t.Fatalf("%s close must book 0.06 base once at the native fill price with the confirmed fee: %+v", c.id, tr)
				}
				var native *basketAction
				for _, a := range e.actionsByPurpose(pairPurposeKillSwitch) {
					if a.StrategyID == c.id {
						native = a
					}
				}
				ledger, err := pairParseLedger(native.SettlementJSON)
				if err != nil || len(ledger.Fills) != c.fills || len(ledger.Fees) != c.fills || native.Status != basketActionCommitted || len(native.VenueOrderID) < 2 || native.VenueOrderID[:2] != c.orderPfx {
					t.Fatalf("%s native action must save one ledger entry per venue fill and commit: %+v %v", c.id, native, err)
				}
				if e.savedCloseRows(c.id) != 1 {
					t.Fatalf("%s must save one close row", c.id)
				}
			}
		})
	}
}

func TestBasketPairEmergencyRealOKXCloseWithoutTerminalEvidenceStaysUnknown(t *testing.T) {
	for _, tc := range []struct {
		name        string
		orderState  string
		reserve     bool
		wantUnknown bool
	}{
		{name: "open order with no fills", orderState: "live", reserve: true, wantUnknown: true},
		{name: "cancelled order with no fills", orderState: "", reserve: true, wantUnknown: false},
		{name: "open order with no fills and no saved action", orderState: "live", reserve: false, wantUnknown: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPairVenuePy(t)
			closers := &pairNativeClosers{
				hl: func(string, *float64, []int64) (*HyperliquidCloseResult, error) {
					t.Fatal("the hyperliquid closer must not run")
					return nil, nil
				},
				okx: func(sym string, p *float64) (*OKXCloseResult, error) {
					res, _, err := RunOKXClose(f.drivers["okx_close"], sym, p)
					return res, err
				},
			}
			e := newPairLiveShapedEnv(t, closers)
			pairLiveMatched(t, e)
			f.write("okx", map[string]any{"now_ms": e.now.UnixMilli(), "close_positions": []map[string]any{{"contracts": 6, "side": "short"}}, "fill_px": "60050",
				"close_parts": []string{"0"}, "drop_fills": true, "order_state": tc.orderState})
			var release func()
			if !tc.reserve {
				r, ok := tryLockEntryAttempt(pairTestOKX)
				if !ok {
					t.Fatal("setup: the entry lock is busy")
				}
				release = r
			}
			_, err := e.bc.pairKillSwitchOKXCloser(closers.okx)("BTC", nil)
			if release != nil {
				release()
			}
			if err != nil {
				t.Fatalf("the real okx close script must answer: %v", err)
			}
			e.step(e.now.Add(time.Second))
			e.step(e.now.Add(time.Second))
			if w := f.read("okx")["wire"].([]any); len(w) != 1 {
				t.Fatalf("the native close must send once and accounting must send nothing, got %d sends", len(w))
			}
			e.assertBooks("a close without fills books nothing", 0.06, -0.06)
			var acts []*basketAction
			for _, a := range e.actionsByPurpose(pairPurposeKillSwitch) {
				if a.StrategyID == pairTestOKX {
					acts = append(acts, a)
				}
			}
			var unknown *pairObligation
			var native []string
			for i, o := range e.rt().Pair.Progress.Obligations {
				if o.MemberID != pairTestOKX || !pairNativeObligationKind(o.Kind) {
					continue
				}
				native = append(native, o.Kind)
				if o.Kind == pairObligationNativeUnknown {
					unknown = &e.rt().Pair.Progress.Obligations[i]
				}
			}
			if !tc.wantUnknown {
				if len(acts) != 1 || acts[0].Status != basketActionNoFill || len(native) != 0 {
					t.Fatalf("a terminal order with no fills must end no_fill with no obligation: %+v %v", acts, native)
				}
				return
			}
			if unknown == nil || unknown.FromMs <= 0 {
				t.Fatalf("an order that is not terminal must keep an unknown-fill obligation with a start time: %+v %v", unknown, native)
			}
			if tc.reserve {
				if len(acts) != 1 || acts[0].Status == basketActionNoFill || (acts[0].Status != basketActionUnresolved && acts[0].Status != basketActionHeld) {
					t.Fatalf("the native action must stay unresolved or held, never no_fill: %+v", acts)
				}
			} else if len(acts) != 0 {
				t.Fatalf("no native action may exist when the reservation failed: %+v", acts)
			}
			if got, ok := pairIncompleteEvidence(e.rt()); !ok || got.MemberID != pairTestOKX {
				t.Fatalf("the incomplete-result reader must report the okx member, got %+v %v", got, ok)
			}
		})
	}
}
