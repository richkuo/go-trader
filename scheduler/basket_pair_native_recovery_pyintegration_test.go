//go:build pyintegration

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var pairNativeDriverEdits = [][2]string{
	{`        def user_fills_by_time(self, user, since):
            return [f for f in state.get("fills", []) if int(f["time"]) >= int(since)]`,
		`        def user_fills_by_time(self, user, since):
            if int(state.get("fills_errors", 0)) > 0:
                state["fills_errors"] = int(state["fills_errors"]) - 1
                save()
                from hyperliquid.utils.error import ClientError
                raise ClientError(429, None, "rate limited", {})
            return [f for f in state.get("fills", []) if int(f["time"]) >= int(since)]`},
	{`            oid = 9500 + len(state["wire"])
            parts = [Decimal(p) for p in state.get("close_parts", [text(size)])]
            filled = sum(parts, Decimal(0))
`,
		`            oid = 9500 + len(state["wire"])
            parts = [Decimal(p) for p in state.get("close_parts", [text(size)])]
            filled = sum(parts, Decimal(0))
            state.setdefault("orders", {})["mc-%d" % oid] = {"status": state.get("close_status") or ("filled" if filled == size else "canceled"), "body": {"coin": coin, "side": "B" if is_buy else "A",
                "limitPx": state.get("fill_px", "60000"), "origSz": text(size), "sz": text(size - filled), "oid": oid, "timestamp": now, "cloid": None, "reduceOnly": True, "tif": "Ioc", "orderType": "Limit"}}
`},
	{`        def create_market_order(self, symbol, side, amount, params=None):
            params = dict(params or {})
`,
		`        def create_market_order(self, symbol, side, amount, params=None):
            params = dict(params or {})
            if "fail_close_after" in state and len([w for w in state.get("wire", []) if w.get("market_close")]) >= int(state["fail_close_after"]):
                raise ccxt.NetworkError("okx request timed out")
`},
}

type pairNativeReal struct {
	*pairNativeRig
	py  *pairVenuePy
	hl  *hyperliquidPairVenue
	okx *okxPairVenue
	hlM pairContractMember
	okM pairContractMember
	now int64
}

func newPairNativeReal(t *testing.T) *pairNativeReal {
	t.Helper()
	py := newPairVenuePy(t)
	driver := pairVenueFakeDriver
	for _, e := range pairNativeDriverEdits {
		if !strings.Contains(driver, e[0]) {
			t.Fatalf("the shared venue driver no longer holds the fixture anchor %q", e[0][:40])
		}
		driver = strings.Replace(driver, e[0], e[1], 1)
	}
	for name, spec := range map[string][2]string{"hl_check": {"hyperliquid", "check_hyperliquid.py"}, "okx_check": {"okx", "check_okx.py"},
		"hl_close": {"hyperliquid", "close_hyperliquid_position.py"}, "okx_close": {"okx", "close_okx_position.py"}} {
		body := strings.ReplaceAll(strings.ReplaceAll(driver, "@@PROVIDER@@", spec[0]), "@@SCRIPT@@", spec[1])
		path := filepath.Join(py.dir, "native-"+name+".py")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		py.drivers[name] = path
	}
	f := &pairKillSwitchFixture{}
	f.closers = &pairNativeClosers{
		hl: func(sym string, p *float64, c []int64) (*HyperliquidCloseResult, error) {
			f.hlCalls++
			res, _, err := RunHyperliquidClose(py.drivers["hl_close"], sym, p, c)
			return res, err
		},
		okx: func(sym string, p *float64) (*OKXCloseResult, error) {
			f.okxCalls++
			res, _, err := RunOKXClose(py.drivers["okx_close"], sym, p)
			return res, err
		},
	}
	f.env = newPairLiveShapedEnv(t, f.closers)
	pairLiveMatched(t, f.env)
	r := &pairNativeReal{pairNativeRig: &pairNativeRig{t: t, f: f, e: f.env, answers: map[string][]pairNativeAnswer{}}, py: py, now: f.env.now.UnixMilli()}
	hlKey, okxKey := pairVenueTestAccounts(t)
	r.hlM, _ = f.env.rt().Contract.pairMember(pairTestHL)
	r.okM, _ = f.env.rt().Contract.pairMember(pairTestOKX)
	r.hlM.AccountKey, r.okM.AccountKey = hlKey, okxKey
	var err error
	r.hl, err = newHyperliquidPairVenue(f.env.adm, r.hlM, pairLiveTransport{Script: py.drivers["hl_check"], PositionScript: py.drivers["hl_check"], Partition: livePartition, Clock: pairVenueTestClock(&r.now), Substituted: true})
	if err != nil {
		t.Fatal(err)
	}
	r.okx, err = newOKXPairVenue(f.env.adm, r.okM, pairLiveTransport{Script: py.drivers["okx_check"], PositionScript: py.drivers["okx_positions"], Partition: livePartition, Clock: pairVenueTestClock(&r.now), Substituted: true})
	if err != nil {
		t.Fatal(err)
	}
	r.installReal()
	return r
}

func (r *pairNativeReal) installReal() {
	r.install()
	r.e.bc.pairNativeHistory = func(ctx context.Context, m pairContractMember, req pairNativeHistoryRequest) (pairNativeHistoryResult, error) {
		r.mu.Lock()
		r.reads = append(r.reads, req)
		r.mu.Unlock()
		if m.MemberID == pairTestHL {
			req.AccountKey = r.hlM.AccountKey
			return r.hl.NativeHistory(ctx, r.hlM, req)
		}
		req.AccountKey = r.okM.AccountKey
		return r.okx.NativeHistory(ctx, r.okM, req)
	}
}

func (r *pairNativeReal) restartReal() {
	r.e.restart()
	r.installReal()
}

func (r *pairNativeReal) venue(provider string, edit func(st map[string]any)) {
	st := r.py.read(provider)
	edit(st)
	st["now_ms"] = r.e.now.UnixMilli()
	r.py.write(provider, st)
}

func (r *pairNativeReal) noGuard() {
	r.t.Helper()
	for _, p := range []string{"hyperliquid", "okx"} {
		if _, err := os.Stat(r.py.states[p]); err != nil {
			continue
		}
		if g := r.py.read(p)["guard"]; g != nil {
			r.t.Fatalf("the %s script reached a refused network or signing call: %v", p, g)
		}
	}
}

func (r *pairNativeReal) closeSends(provider string) int {
	n := 0
	for _, w := range pairVenueWire(r.t, r.py.read(provider)) {
		if w["market_close"] == true {
			n++
		}
	}
	return n
}

func TestBasketPairNativeRecoveryRealHistoryBooksRecoveredFillsOnce(t *testing.T) {
	r := newPairNativeReal(t)
	e := r.e
	now := e.now.UnixMilli()
	r.py.write("hyperliquid", map[string]any{"now_ms": now, "close_position": map[string]string{"BTC": "0.06"}, "fill_px": "59900", "fee": "0.9",
		"close_parts": []string{"0.02", "0.04"}, "fills_errors": 2, "user_state": map[string]any{}})
	r.py.write("okx", map[string]any{"now_ms": now, "close_positions": []map[string]any{{"contracts": 2, "side": "short"}, {"contracts": 4, "side": "short"}},
		"fill_px": "60050", "fee": "-0.3", "drop_fills": true, "order_state": "live"})
	if _, err := e.bc.pairKillSwitchHLCloser(r.f.closers.hl)("BTC", nil, nil); err != nil {
		t.Fatalf("the real hyperliquid close must answer: %v", err)
	}
	if _, err := e.bc.pairKillSwitchOKXCloser(r.f.closers.okx)("BTC", nil); err != nil {
		t.Fatalf("the real okx close must answer: %v", err)
	}
	hlAtt, okxAtt := r.attempts(pairTestHL), r.attempts(pairTestOKX)
	if len(hlAtt) != 1 || len(hlAtt[0].VenueOrderIDs) != 1 || !hlAtt[0].OrdersComplete || r.obligation(pairObligationNativeUnknown, pairTestHL) == nil {
		t.Fatalf("a rate-limited fill lookup must keep the hyperliquid order id and the unknown obligation: %+v %v", hlAtt, e.obligationKinds(pairTestHL))
	}
	if len(okxAtt) != 1 || len(okxAtt[0].VenueOrderIDs) != 2 || !okxAtt[0].OrdersComplete || r.obligation(pairObligationNativeUnknown, pairTestOKX) == nil {
		t.Fatalf("a non-terminal multi-order okx close must keep both order ids and the unknown obligation: %+v %v", okxAtt, e.obligationKinds(pairTestOKX))
	}
	r.restartReal()
	r.step(6 * time.Second)
	e.assertBooks("a rate-limited history read and live orders book nothing", 0.06, -0.06)
	if r.obligation(pairObligationNativeUnknown, pairTestHL) == nil || r.obligation(pairObligationNativeUnknown, pairTestOKX) == nil {
		t.Fatalf("incomplete history must clear nothing: %v", e.obligationKinds(""))
	}
	r.venue("okx", func(st map[string]any) {
		orders := st["orders_by_id"].(map[string]any)
		var fills []any
		bill := 910000
		for _, oid := range okxAtt[0].VenueOrderIDs {
			o := orders[oid].(map[string]any)
			o["state"] = "filled"
			o["accFillSz"] = o["sz"]
			bill++
			fills = append(fills, map[string]any{"billId": fmt.Sprint(bill), "tradeId": fmt.Sprintf("t%d", bill), "ordId": oid, "instId": "BTC-USDT-SWAP", "side": "buy",
				"fillSz": o["sz"], "fillPx": "60050", "fee": "-0.3", "feeCcy": "USDT", "fillTime": o["cTime"], "ts": o["cTime"]})
		}
		st["fills"] = fills
	})
	for i := 0; i < 6; i++ {
		r.step(10 * time.Second)
	}
	e.assertBooks("the complete history reads flatten both books once", 0, 0)
	for _, id := range []string{pairTestHL, pairTestOKX} {
		res := r.savedResolutions(id)
		if len(res) != 1 || res[0].Mode != pairNativeModeVenueHistory || e.savedCloseRows(id) != 1 || r.obligation(pairObligationNativeUnknown, id) != nil {
			t.Fatalf("%s must resolve once from its venue history: %+v rows %d", id, res, e.savedCloseRows(id))
		}
	}
	if fills := r.savedResolutions(pairTestOKX)[0].FillIDs; len(fills) != 2 {
		t.Fatalf("the multi-order okx recovery must book both orders' fills: %v", fills)
	}
	if r.closeSends("hyperliquid") != 1 || r.closeSends("okx") != 2 {
		t.Fatalf("recovery must send no order: hl %d okx %d", r.closeSends("hyperliquid"), r.closeSends("okx"))
	}
	r.noGuard()
	r.restartReal()
	r.step(time.Minute)
	if e.savedCloseRows(pairTestHL) != 1 || e.savedCloseRows(pairTestOKX) != 1 || len(r.savedResolutions(pairTestHL)) != 1 {
		t.Fatal("a restart must not book a recovered fill twice")
	}
}

func TestBasketPairNativeRecoveryRealZeroFillAndLaterSubmissionError(t *testing.T) {
	t.Run("okx terminal zero fill", func(t *testing.T) {
		r := newPairNativeReal(t)
		e := r.e
		r.py.write("okx", map[string]any{"now_ms": e.now.UnixMilli(), "close_positions": []map[string]any{{"contracts": 6, "side": "short"}}, "fill_px": "60050",
			"close_parts": []string{"0"}, "drop_fills": true, "order_state": "live"})
		if _, err := e.bc.pairKillSwitchOKXCloser(r.f.closers.okx)("BTC", nil); err != nil {
			t.Fatalf("the real okx close must answer: %v", err)
		}
		att := r.attempts(pairTestOKX)
		if len(att) != 1 {
			t.Fatalf("setup: %+v", att)
		}
		r.step(6 * time.Second)
		if a := r.nativeAction(pairTestOKX); a.Status == basketActionNoFill {
			t.Fatal("an order that is still live never proves a zero fill")
		}
		r.venue("okx", func(st map[string]any) {
			st["orders_by_id"].(map[string]any)[att[0].VenueOrderIDs[0]].(map[string]any)["state"] = "canceled"
		})
		r.step(15 * time.Second)
		if a := r.nativeAction(pairTestOKX); a.Status != basketActionNoFill || e.signed(pairTestOKX) != -0.06 {
			t.Fatalf("a terminal order with no execution must end no_fill and book nothing: %+v %v", a, e.signed(pairTestOKX))
		}
		if res := r.savedResolutions(pairTestOKX); len(res) != 1 || res[0].Mode != pairNativeModeZeroFill {
			t.Fatalf("the zero-fill resolution must be saved: %+v", res)
		}
		r.noGuard()
	})
	t.Run("okx later submission error keeps the sent order id", func(t *testing.T) {
		r := newPairNativeReal(t)
		e := r.e
		r.py.write("okx", map[string]any{"now_ms": e.now.UnixMilli(), "close_positions": []map[string]any{{"contracts": 2, "side": "short"}, {"contracts": 4, "side": "short"}},
			"fill_px": "60050", "drop_fills": true, "fail_close_after": 1})
		if _, err := e.bc.pairKillSwitchOKXCloser(r.f.closers.okx)("BTC", nil); err == nil {
			t.Fatal("the failed second order must surface as a close error")
		}
		att := r.attempts(pairTestOKX)
		if len(att) != 1 || len(att[0].VenueOrderIDs) != 1 || att[0].OrdersComplete || att[0].CloseError == "" {
			t.Fatalf("the sent order id must be kept with incomplete membership: %+v", att)
		}
		r.restartReal()
		for i := 0; i < 3; i++ {
			r.step(10 * time.Second)
		}
		if r.readCount() != 0 || r.obligation(pairObligationNativeUnknown, pairTestOKX) == nil {
			t.Fatalf("incomplete order membership is never read automatically: reads %d %v", r.readCount(), e.obligationKinds(pairTestOKX))
		}
		r.noGuard()
	})
}

func TestBasketPairNativeRecoveryRealRoutesCheckIdentityAndCoverage(t *testing.T) {
	r := newPairNativeReal(t)
	e := r.e
	now := e.now.UnixMilli()
	r.py.write("hyperliquid", map[string]any{"now_ms": now, "close_position": map[string]string{"BTC": "0.06"}, "fill_px": "59900", "fee": "0.9", "fills_errors": 1, "user_state": map[string]any{}})
	if _, err := e.bc.pairKillSwitchHLCloser(r.f.closers.hl)("BTC", nil, nil); err != nil {
		t.Fatalf("the real hyperliquid close must answer: %v", err)
	}
	att := r.attempts(pairTestHL)[0]
	base := pairNativeHistoryRequest{V: pairNativeHistoryVersion, PairID: pairTestID, Partition: "live", MemberID: pairTestHL, Provider: entryProviderHyperliquid,
		AccountKey: r.hlM.AccountKey, Instrument: "BTC", AttemptID: att.ID, OrderSide: "sell", VenueOrderIDs: att.VenueOrderIDs, FromMs: att.SentAtMs - pairNativeAcceptSkewMs,
		ThroughMs: att.DeadlineMs + pairNativeAcceptSkewMs, Revision: "0123456789abcdef"}
	ctx := context.Background()
	res, err := r.hl.NativeHistory(ctx, r.hlM, base)
	if err != nil || !res.Complete || len(res.Evidence.Fills) != 1 {
		t.Fatalf("a complete hyperliquid history must pass the transport checks: %+v %v", res, err)
	}
	for _, tc := range []struct {
		name string
		edit func(q *pairNativeHistoryRequest)
		want string
	}{
		{"wrong side", func(q *pairNativeHistoryRequest) { q.OrderSide = "buy" }, "not a buy close"},
		{"outside the window", func(q *pairNativeHistoryRequest) { q.FromMs, q.ThroughMs = att.SentAtMs-60000, att.SentAtMs-30000 }, "outside the attempt window"},
		{"unknown order", func(q *pairNativeHistoryRequest) { q.VenueOrderIDs = []string{"424242"} }, "no order record"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := base
			q.VenueOrderIDs = append([]string(nil), base.VenueOrderIDs...)
			tc.edit(&q)
			got, err := r.hl.NativeHistory(ctx, r.hlM, q)
			if err != nil || got.Complete || !strings.Contains(got.Detail, tc.want) {
				t.Fatalf("want an incomplete answer naming %q: %+v %v", tc.want, got, err)
			}
		})
	}
	wrong := base
	wrong.AccountKey = "hyperliquid:testnet:0x00000000000000000000000000000000000000b2"
	if _, err := r.hl.NativeHistory(ctx, r.hlM, wrong); err == nil {
		t.Fatal("a request bound to another account must be refused before any read")
	}
	out, err := r.hl.read(ctx, r.py.drivers["hl_check"], []string{pairNativeHistoryFlag + "=" + mustJSON(t, map[string]any{"v": 1, "pair_id": pairTestID, "partition": "live", "member_id": pairTestHL,
		"provider": "hyperliquid", "account_key": "hyperliquid:testnet:0x00000000000000000000000000000000000000b2", "instrument": "BTC", "attempt_id": att.ID, "order_side": "sell",
		"venue_order_ids": att.VenueOrderIDs, "from_ms": base.FromMs, "through_ms": base.ThroughMs, "revision": "0123456789abcdef"}), "--mode=live"})
	if err == nil || !strings.Contains(err.Error(), "the credentials answer for account") || out.NativeHistory != nil {
		t.Fatalf("the script must refuse a request bound to another account: %v", err)
	}
	if _, err := r.hl.read(ctx, r.py.drivers["hl_check"], []string{pairNativeHistoryFlag + `={"v":1,"v":1}`, "--mode=live"}); err == nil {
		t.Fatal("a duplicate-key request must be refused")
	}
	r.py.write("okx", map[string]any{"now_ms": now, "close_positions": []map[string]any{{"contracts": 6, "side": "short"}}, "fill_px": "60050", "close_parts": []string{"6"}})
	if _, err := e.bc.pairKillSwitchOKXCloser(r.f.closers.okx)("BTC", nil); err != nil {
		t.Fatalf("the real okx close must answer: %v", err)
	}
	var oid string
	for k := range r.py.read("okx")["orders_by_id"].(map[string]any) {
		oid = k
	}
	r.venue("okx", func(st map[string]any) {
		var fills []any
		for i := 0; i < 100; i++ {
			fills = append(fills, map[string]any{"billId": fmt.Sprint(950000 + i), "ordId": oid, "instId": "BTC-USDT-SWAP", "side": "buy", "fillSz": "0.06", "fillPx": "60050",
				"fee": "-0.01", "feeCcy": "USDT", "fillTime": fmt.Sprint(now), "ts": fmt.Sprint(now)})
		}
		st["fills"] = fills
	})
	okq := pairNativeHistoryRequest{V: 1, PairID: pairTestID, Partition: "live", MemberID: pairTestOKX, Provider: entryProviderOKX, AccountKey: r.okM.AccountKey, Instrument: "BTC-USDT-SWAP",
		AttemptID: "pna-" + strings.Repeat("0", 32), OrderSide: "buy", VenueOrderIDs: []string{oid}, FromMs: now - pairNativeAcceptSkewMs, ThroughMs: now + 60000, Revision: "0123456789abcdef"}
	got, err := r.okx.NativeHistory(ctx, r.okM, okq)
	if err != nil || got.Complete || !strings.Contains(got.Detail, "may be clipped") {
		t.Fatalf("a full okx trade page must never prove coverage: %+v %v", got, err)
	}
	if r.closeSends("hyperliquid") != 1 || r.closeSends("okx") != 1 {
		t.Fatal("history reads must send no order")
	}
	r.noGuard()
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
