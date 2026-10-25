//go:build pyintegration

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const pairVenueFakeDriver = `import importlib.util, json, os, runpy, socket, sys, time
from decimal import Decimal, ROUND_DOWN, ROUND_HALF_EVEN
PROVIDER = "@@PROVIDER@@"
SCRIPT = "@@SCRIPT@@"
root = os.environ["PAIR_FAKE_ROOT"]
state_path = os.environ["PAIR_FAKE_STATE_" + PROVIDER.upper()]
state = json.load(open(state_path))

def save():
    with open(state_path, "w") as fh:
        json.dump(state, fh)

import atexit
atexit.register(save)

def guard(kind):
    def refuse(*a, **k):
        state.setdefault("guard", []).append(kind)
        save()
        raise RuntimeError("the fixture refuses a %s attempt" % kind)
    return refuse

socket.socket.connect = guard("network")
socket.socket.connect_ex = guard("network")
socket.create_connection = guard("network")
clock = {"ms": int(state["now_ms"])}
time.time = lambda: clock["ms"] / 1000.0
def fake_sleep(s):
    clock["ms"] += int(s * 1000)
time.sleep = fake_sleep

def text(d):
    t = format(Decimal(d).normalize(), "f")
    if "." in t:
        t = t.rstrip("0").rstrip(".")
    return t or "0"

if PROVIDER == "hyperliquid":
    import eth_account
    eth_account.Account.sign_message = guard("signing")
    eth_account.Account.sign_typed_data = guard("signing")
    import hyperliquid.api, hyperliquid.exchange, hyperliquid.info
    import hyperliquid.utils.signing as hsign
    for name in dir(hsign):
        if name.startswith("sign_"):
            setattr(hsign, name, guard("signing"))

    class FakeAPI:
        def __init__(self, *a, **k):
            pass
        def post(self, path, payload):
            if payload.get("type") == "spotMeta":
                return {"universe": [], "tokens": []}
            return {"universe": [{"name": "BTC", "szDecimals": 3, "maxLeverage": 50}]}

    class FakeInfo:
        def __init__(self, *a, **k):
            self.asset_to_sz_decimals = {0: 3}
            self.coin_to_asset = {"BTC": 0}
        def name_to_asset(self, name):
            return {"BTC": 0}[name]
        def _wrap(self, o):
            return {"status": "order", "order": {"status": o["status"], "statusTimestamp": o["body"]["timestamp"], "order": o["body"]}}
        def query_order_by_cloid(self, user, cloid):
            o = state.get("orders", {}).get(cloid.to_raw())
            return {"status": "unknownOid"} if o is None else self._wrap(o)
        def query_order_by_oid(self, user, oid):
            for o in state.get("orders", {}).values():
                if o["body"]["oid"] == oid:
                    return self._wrap(o)
            return {"status": "unknownOid"}
        def user_fills_by_time(self, user, since):
            return [f for f in state.get("fills", []) if int(f["time"]) >= int(since)]
        def user_state(self, user):
            return state["user_state"]

    class FakeExchange:
        def __init__(self, *a, **k):
            pass
        def set_expires_after(self, ms):
            state["expiry"] = ms
            if "after_expiry_ms" in state:
                clock["ms"] = int(state["after_expiry_ms"])
        def order(self, coin, is_buy, sz, limit_px, order_type, reduce_only=False, cloid=None, builder=None):
            raw = cloid.to_raw() if cloid is not None else None
            state.setdefault("wire", []).append({"coin": coin, "is_buy": is_buy, "sz": repr(sz), "limit_px": repr(limit_px), "order_type": order_type,
                                                 "reduce_only": reduce_only, "cloid": raw, "expiry": state.get("expiry")})
            behavior = state.get("behavior", "fill")
            if behavior == "nomatch":
                return {"status": "ok", "response": {"type": "order", "data": {"statuses": [{"error": "Order could not immediately match against any resting orders. asset=0"}]}}}
            now = clock["ms"]
            oid = 9000 + len(state["wire"])
            size = Decimal(repr(sz))
            parts = [Decimal(p) for p in state.get("fill_parts", [repr(sz)])]
            filled = sum(parts, Decimal(0))
            px = state.get("fill_px", repr(limit_px))
            body = {"coin": coin, "side": "B" if is_buy else "A", "limitPx": repr(limit_px), "origSz": repr(sz), "sz": text(size - filled), "oid": oid,
                    "timestamp": now, "cloid": raw, "reduceOnly": reduce_only, "tif": order_type["limit"]["tif"], "orderType": "Limit"}
            state.setdefault("orders", {})[raw] = {"status": "filled" if filled == size else "canceled", "body": body}
            for p in parts:
                state.setdefault("fills", []).append({"coin": coin, "px": px, "sz": text(p), "side": "B" if is_buy else "A", "time": now, "oid": oid,
                                                      "tid": 500000 + len(state.get("fills", [])), "fee": state.get("fee", "0.01"), "feeToken": "USDC", "cloid": raw, "hash": "0x00"})
            if behavior == "accept_then_crash":
                save()
                os._exit(1)
            return {"status": "ok", "response": {"type": "order", "data": {"statuses": [{"filled": {"totalSz": text(filled), "avgPx": px, "oid": oid}}]}}}
        def market_close(self, coin, sz=None, px=None, slippage=0.05, cloid=None, builder=None):
            pos = Decimal(state.get("close_position", {}).get(coin, "0"))
            if pos == 0:
                return {"status": "ok", "response": {"type": "order", "data": {"statuses": []}}}
            size = Decimal(repr(sz)) if sz is not None else abs(pos)
            is_buy = pos < 0
            state.setdefault("wire", []).append({"coin": coin, "is_buy": is_buy, "sz": text(size), "market_close": True})
            now = clock["ms"]
            oid = 9500 + len(state["wire"])
            parts = [Decimal(p) for p in state.get("close_parts", [text(size)])]
            filled = sum(parts, Decimal(0))
            fill_px = state.get("fill_px", "60000")
            if not state.get("drop_fills"):
                for p in parts:
                    state.setdefault("fills", []).append({"coin": coin, "px": fill_px, "sz": text(p), "side": "B" if is_buy else "A", "time": now, "oid": oid,
                                                          "tid": 600000 + len(state.get("fills", [])), "fee": state.get("fee", "0.01"), "feeToken": "USDC", "cloid": None, "hash": "0x00"})
            return {"status": "ok", "response": {"type": "order", "data": {"statuses": [{"filled": {"totalSz": text(filled), "avgPx": fill_px, "oid": oid}}]}}}

    hyperliquid.api.API = FakeAPI
    hyperliquid.info.Info = FakeInfo
    hyperliquid.exchange.Exchange = FakeExchange
    adapter_path = os.path.join(root, "platforms", "hyperliquid", "adapter.py")
else:
    import ccxt

    class FakeOKX:
        def __init__(self, config):
            self.config = config
            self.headers = {}
            self.urls = {"api": {"rest": "https://{hostname}"}}
            self.hostname = "www.okx.com"
        def load_markets(self):
            state["markets_loaded"] = True
        def market(self, pair):
            return {"id": "BTC-USDT-SWAP", "symbol": pair, "contractSize": 0.01}
        def amount_to_precision(self, pair, amount):
            if "amount_text" in state:
                return state["amount_text"]
            return text(Decimal(repr(amount)).quantize(Decimal("1"), rounding=ROUND_DOWN))
        def price_to_precision(self, pair, price):
            if "price_text" in state:
                return state["price_text"]
            return text(Decimal(repr(price)).quantize(Decimal("0.1"), rounding=ROUND_HALF_EVEN))
        def publicGetPublicInstruments(self, params):
            if "after_instruments_ms" in state:
                clock["ms"] = int(state["after_instruments_ms"])
            return {"code": "0", "data": [{"instId": params["instId"], "ctVal": state.get("ct_val", "0.01"), "tickSz": "0.1", "lotSz": "1", "minSz": "1", "state": "live", "settleCcy": "USDT"}]}
        def create_order(self, symbol, type, side, amount, price, params=None):
            params = dict(params or {})
            state.setdefault("wire", []).append({"symbol": symbol, "type": type, "side": side, "amount": repr(amount), "price": repr(price), "params": params, "headers": dict(self.headers or {})})
            behavior = state.get("behavior", "fill")
            if behavior == "venue_reject":
                raise ccxt.InsufficientFunds("okx Insufficient margin")
            now = clock["ms"]
            cid = params["clOrdId"]
            oid = str(7000 + len(state["wire"]))
            size = Decimal(repr(amount))
            parts = [Decimal(p) for p in state.get("fill_parts", [repr(amount)])]
            filled = sum(parts, Decimal(0))
            px = state.get("fill_px", repr(price))
            if behavior == "nomatch":
                parts, filled = [], Decimal(0)
            info = {"instId": "BTC-USDT-SWAP", "clOrdId": cid, "ordId": oid, "side": side, "sz": text(size), "px": repr(price), "ordType": "ioc" if params.get("timeInForce") == "IOC" else "limit",
                    "reduceOnly": "true" if params.get("reduceOnly") else "false", "state": "filled" if filled == size else "canceled", "accFillSz": text(filled), "cTime": str(now)}
            state.setdefault("orders", {})[cid] = info
            for p in parts:
                bill = str(800000 + len(state.get("fills", [])))
                state.setdefault("fills", []).append({"billId": bill, "tradeId": "t" + bill, "ordId": oid, "clOrdId": cid, "instId": "BTC-USDT-SWAP", "side": side, "fillSz": text(p),
                                                      "fillPx": px, "fee": state.get("fee", "-0.25"), "feeCcy": "USDT", "fillTime": str(now), "ts": str(now)})
            if behavior == "accept_then_crash":
                save()
                os._exit(1)
            return {"id": oid, "info": info}
        def fetch_positions(self, symbols):
            return list(state.get("close_positions", []))
        def create_market_order(self, symbol, side, amount, params=None):
            params = dict(params or {})
            state.setdefault("wire", []).append({"symbol": symbol, "side": side, "amount": repr(amount), "params": params, "market_close": True})
            now = clock["ms"]
            oid = str(7500 + len(state["wire"]))
            size = Decimal(repr(amount))
            parts = [Decimal(p) for p in state.get("close_parts", [text(size)])]
            filled = sum(parts, Decimal(0))
            fill_px = state.get("fill_px", "60000")
            info = {"instId": "BTC-USDT-SWAP", "ordId": oid, "side": side, "sz": text(size), "ordType": "market", "reduceOnly": "true",
                    "state": state.get("order_state") or ("filled" if filled == size else "canceled"), "accFillSz": text(filled), "cTime": str(now)}
            state.setdefault("orders_by_id", {})[oid] = info
            if not state.get("drop_fills"):
                for p in parts:
                    bill = str(900000 + len(state.get("fills", [])))
                    state.setdefault("fills", []).append({"billId": bill, "tradeId": "t" + bill, "ordId": oid, "instId": "BTC-USDT-SWAP", "side": side, "fillSz": text(p),
                                                          "fillPx": fill_px, "fee": state.get("fee", "-0.25"), "feeCcy": "USDT", "fillTime": str(now), "ts": str(now)})
            return {"id": oid, "info": info, "amount": amount, "filled": None, "average": None, "price": None}
        def fetch_order_trades(self, oid, symbol=None, since=None, limit=None, params=None):
            return [{"info": f} for f in state.get("fills", []) if f.get("ordId") == oid]
        def fetch_order(self, oid, symbol, params=None):
            state["headers_after"] = dict(self.headers or {})
            if oid is not None and not (params or {}).get("clOrdId"):
                o = state.get("orders_by_id", {}).get(oid)
                if o is None:
                    raise ccxt.OrderNotFound("okx order not found")
                return {"id": oid, "info": o}
            o = state.get("orders", {}).get((params or {}).get("clOrdId"))
            if o is None:
                raise ccxt.OrderNotFound("okx order not found")
            return {"id": o["ordId"], "info": o}
        def privateGetTradeFills(self, params):
            return {"code": "0", "data": list(state.get("fills", []))}
        def privateGetTradeFillsHistory(self, params):
            return {"code": "0", "data": list(state.get("fills", []))}
        def privateGetAccountPositions(self, params):
            return state["positions"]

    ccxt.okx = FakeOKX
    adapter_path = os.path.join(root, "platforms", "okx", "adapter.py")

spec = importlib.util.spec_from_file_location("adapter", adapter_path)
mod = importlib.util.module_from_spec(spec)
sys.modules["adapter"] = mod
spec.loader.exec_module(mod)
if PROVIDER == "hyperliquid":
    mod._load_meta_cache = lambda *a, **k: None
    mod._save_meta_cache = lambda *a, **k: None
script = os.path.join(root, "shared_scripts", SCRIPT)
sys.argv = [script] + sys.argv[1:]
runpy.run_path(script, run_name="__main__")
`

const (
	pairVenueTestHLAddress = "0x00000000000000000000000000000000000000a1"
	pairVenueTestHLSecret  = "0x1111111111111111111111111111111111111111111111111111111111111111"
)

type pairVenuePy struct {
	t       *testing.T
	root    string
	dir     string
	states  map[string]string
	drivers map[string]string
}

func newPairVenuePy(t *testing.T) *pairVenuePy {
	t.Helper()
	root := entryOperatorRepoRoot(t)
	f := &pairVenuePy{t: t, root: root, dir: t.TempDir(), states: map[string]string{}, drivers: map[string]string{}}
	for name, spec := range map[string][2]string{"hl_check": {"hyperliquid", "check_hyperliquid.py"}, "okx_check": {"okx", "check_okx.py"}, "okx_positions": {"okx", "fetch_okx_positions.py"},
		"hl_close": {"hyperliquid", "close_hyperliquid_position.py"}, "okx_close": {"okx", "close_okx_position.py"}} {
		body := strings.ReplaceAll(strings.ReplaceAll(pairVenueFakeDriver, "@@PROVIDER@@", spec[0]), "@@SCRIPT@@", spec[1])
		path := filepath.Join(f.dir, name+".py")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		f.drivers[name] = path
	}
	for _, p := range []string{"hyperliquid", "okx"} {
		f.states[p] = filepath.Join(f.dir, p+"-state.json")
		t.Setenv("PAIR_FAKE_STATE_"+strings.ToUpper(p), f.states[p])
	}
	t.Setenv("PAIR_FAKE_ROOT", root)
	t.Setenv("HYPERLIQUID_TESTNET", "1")
	t.Setenv("HYPERLIQUID_SECRET_KEY", pairVenueTestHLSecret)
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", pairVenueTestHLAddress)
	t.Setenv("OKX_API_KEY", "fixture-key")
	t.Setenv("OKX_API_SECRET", "fixture-secret")
	t.Setenv("OKX_PASSPHRASE", "fixture-pass")
	t.Setenv("OKX_SANDBOX", "1")
	t.Chdir(root)
	return f
}

func (f *pairVenuePy) write(provider string, state map[string]any) {
	f.t.Helper()
	b, err := json.Marshal(state)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(f.states[provider], b, 0o600); err != nil {
		f.t.Fatal(err)
	}
}

func (f *pairVenuePy) read(provider string) map[string]any {
	f.t.Helper()
	b, err := os.ReadFile(f.states[provider])
	if err != nil {
		f.t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		f.t.Fatal(err)
	}
	return out
}

func pairVenueTestAccounts(t *testing.T) (string, string) {
	t.Helper()
	hl, okx := hyperliquidEntryAccountKey(), okxEntryAccountKey("swap")
	if hl != "hyperliquid:testnet:"+pairVenueTestHLAddress || !strings.HasPrefix(okx, "okx:swap:sandbox:key-") {
		t.Fatalf("fixture accounts %q %q", hl, okx)
	}
	return hl, okx
}

func pairVenueTestClock(ms *int64) func() time.Time {
	return func() time.Time { return time.UnixMilli(*ms).UTC() }
}

type pairVenueRig struct {
	f    *pairVenuePy
	e    *pairTestEnv
	hl   *hyperliquidPairVenue
	okx  *okxPairVenue
	hlM  pairContractMember
	okxM pairContractMember
	now  int64
}

func newPairVenueRig(t *testing.T) *pairVenueRig {
	t.Helper()
	e := newPairLiveShapedEnv(t, &pairNativeClosers{})
	f := newPairVenuePy(t)
	hlKey, okxKey := pairVenueTestAccounts(t)
	r := &pairVenueRig{f: f, e: e, now: pairTestStart().UnixMilli()}
	r.hlM, _ = e.rt().Contract.pairMember(pairTestHL)
	r.okxM, _ = e.rt().Contract.pairMember(pairTestOKX)
	r.hlM.AccountKey, r.okxM.AccountKey = hlKey, okxKey
	var err error
	r.hl, err = newHyperliquidPairVenue(e.adm, r.hlM, pairLiveTransport{Script: f.drivers["hl_check"], PositionScript: f.drivers["hl_check"], Partition: RiskPartition{Scope: ScopeLive}, Clock: pairVenueTestClock(&r.now), Substituted: true})
	if err != nil {
		t.Fatalf("hyperliquid pair venue: %v", err)
	}
	r.okx, err = newOKXPairVenue(e.adm, r.okxM, pairLiveTransport{Script: f.drivers["okx_check"], PositionScript: f.drivers["okx_positions"], Partition: RiskPartition{Scope: ScopeLive}, Clock: pairVenueTestClock(&r.now), Substituted: true})
	if err != nil {
		t.Fatalf("okx pair venue: %v", err)
	}
	return r
}

func (r *pairVenueRig) action(m pairContractMember, purpose, side, posSide string, baseQty, bound float64, cloid string) *basketAction {
	r.f.t.Helper()
	q, err := pairExactOrderQty(m, baseQty)
	if err != nil {
		r.f.t.Fatal(err)
	}
	dir := basketDirectionIncrease
	if purpose != pairPurposeEntry {
		dir = basketDirectionReduce
	}
	a := &basketAction{GroupMode: pairGroupMode, Partition: RiskPartition{Scope: ScopeLive}, BasketID: pairTestID, StrategyID: m.MemberID, Symbol: m.Symbol, Direction: dir,
		OrderSide: side, PositionSide: posSide, Status: basketActionSubmitting, RequestedQty: q.Base, Provider: m.Provider, AccountKey: m.AccountKey, ClientOrderID: cloid,
		Purpose: purpose, Instrument: m.Instrument, NativeQty: q.Native, ContractSize: m.Meta.ContractSize, PriceBound: bound, AcceptDeadlineMs: r.now + 30000,
		CreatedAtMs: r.now, SubmittedAtMs: r.now, ActionID: "pa-venue-" + cloid}
	req := pairActionRequest{V: pairActionRequestVersion, PairID: pairTestID, Partition: "live", MemberID: m.MemberID, Provider: m.Provider, AccountKey: m.AccountKey,
		Instrument: m.Instrument, ClientOrderID: cloid, Purpose: purpose, OrderSide: side, PositionSide: posSide, BaseQty: q.Base, NativeQty: q.Native,
		ContractSize: m.Meta.ContractSize, PriceBound: bound, AcceptDeadlineMs: a.AcceptDeadlineMs, OriginSlotMs: r.now, ReduceOnly: dir == basketDirectionReduce,
		ContractFingerprint: strings.Repeat("ab", 32)}
	if dir == basketDirectionIncrease {
		req.QuoteEventID, req.CapabilityHash, req.EvidenceHash = "pq-fixture", strings.Repeat("cd", 32), strings.Repeat("ef", 32)
	} else {
		px := bound + 500
		if side == "buy" {
			px = bound - 500
		}
		req.ReferenceReceiptMs, req.ReferenceMaxAgeMs, req.ReferencePx = r.now-100, 20000, strconv.FormatFloat(math.Round(px*10)/10, 'f', -1, 64)
	}
	a.RequestJSON = marshalEntryJSON(req)
	return a
}

func pairVenueWire(t *testing.T, state map[string]any) []map[string]any {
	t.Helper()
	raw, _ := state["wire"].([]any)
	var out []map[string]any
	for _, w := range raw {
		out = append(out, w.(map[string]any))
	}
	return out
}

func pairVenueNoGuard(t *testing.T, state map[string]any) {
	t.Helper()
	if g, ok := state["guard"]; ok {
		t.Fatalf("the fixture saw a network or signing attempt: %v", g)
	}
}

func TestBasketPairVenueRealHyperliquidSendsTheSavedIOCRequest(t *testing.T) {
	cases := []struct {
		name, purpose, side, pos string
		qty, bound               float64
		parts                    []string
		fee                      string
		outcome                  string
	}{
		{name: "entry with noisy lots", purpose: pairPurposeEntry, side: "buy", pos: entrySideLong, qty: 0.57, bound: 60400, fee: "0.01", outcome: basketVenueFilled},
		{name: "reduction partial", purpose: pairPurposeExit, side: "sell", pos: entrySideLong, qty: 0.35, bound: 59500, parts: []string{"0.2"}, fee: "0", outcome: basketVenuePartial},
		{name: "reduction with a rebate", purpose: pairPurposeExit, side: "buy", pos: entrySideShort, qty: 0.06, bound: 60600, parts: []string{"0.02", "0.04"}, fee: "-0.003", outcome: basketVenueFilled},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newPairVenueRig(t)
			st := map[string]any{"now_ms": r.now, "fee": tc.fee}
			if tc.parts != nil {
				st["fill_parts"] = tc.parts
			}
			r.f.write("hyperliquid", st)
			cloid := "0x0000000000000000000000000000000" + string(rune('1'+i))
			a := r.action(r.hlM, tc.purpose, tc.side, tc.pos, tc.qty, tc.bound, cloid)
			out, err := r.hl.Submit(a)
			if err != nil {
				t.Fatal(err)
			}
			got := r.f.read("hyperliquid")
			pairVenueNoGuard(t, got)
			wire := pairVenueWire(t, got)
			if len(wire) != 1 {
				t.Fatalf("want one order call, got %v", wire)
			}
			w := wire[0]
			ot, _ := w["order_type"].(map[string]any)
			lim, _ := ot["limit"].(map[string]any)
			if w["coin"] != "BTC" || w["is_buy"] != (tc.side == "buy") || !pairDecimalEqualsFloat(w["sz"].(string), a.NativeQty) || lim["tif"] != "Ioc" ||
				w["reduce_only"] != (tc.purpose != pairPurposeEntry) || w["cloid"] != cloid || int64(w["expiry"].(float64)) != a.AcceptDeadlineMs {
				t.Fatalf("the wire request %v does not carry the saved size %s, bound, IOC, reduction purpose, client id and expiry", w, a.nativeText())
			}
			if !pairDecimalEqualsFloat(w["limit_px"].(string), tc.bound) {
				t.Fatalf("the wire price %v is not the saved bound %v", w["limit_px"], tc.bound)
			}
			if out.Outcome != tc.outcome || !out.Terminal || out.AcceptedAtMs != r.now || out.ClientOrderID != cloid || out.AccountKey != r.hlM.AccountKey {
				t.Fatalf("outcome %+v, want terminal %s accepted at %d", out, tc.outcome, r.now)
			}
			wantQty := tc.qty
			if tc.parts != nil {
				wantQty = 0
				for _, p := range tc.parts {
					f, _ := pairCoverageRat("part", p, true)
					wantQty += pairRatFloat(f)
				}
			}
			if !basketQtyEqual(out.CumQty, wantQty) || len(out.FillCoverage) != len(out.FeeCoverage) || len(out.FillCoverage) == 0 || !out.FeeFinal ||
				out.FeeEvidenceVersion != int64(len(out.FeeCoverage)) || out.FeeEvidenceID != pairFeeEvidenceID(a) {
				t.Fatalf("outcome %+v must carry complete execution and fee coverage with finality", out)
			}
			for _, fee := range out.FeeCoverage {
				want, _ := pairNormalizeDecimal(tc.fee)
				if fee.FeeAmount != want || fee.FeeCurrency != "USDC" {
					t.Fatalf("fee record %+v, want signed %s USDC", fee, want)
				}
			}
			if why := pairOutcomeError(a, out); why != "" {
				t.Fatalf("the mapped outcome is refused by the coordinator rules: %s", why)
			}
		})
	}
}

func (a *basketAction) nativeText() string {
	var req pairActionRequest
	_ = json.Unmarshal([]byte(a.RequestJSON), &req)
	d, _ := pairDecimalFromFloat(req.NativeQty)
	return string(d)
}

func TestBasketPairVenueRealOKXSendsTheSavedIOCRequestAndNormalizesOnce(t *testing.T) {
	r := newPairVenueRig(t)
	r.f.write("okx", map[string]any{"now_ms": r.now, "fee": "-0.25", "fill_parts": []string{"20", "37"}})
	a := r.action(r.okxM, pairPurposeExit, "buy", entrySideShort, 0.57, 60600.3, "fx01")
	if a.NativeQty != 57 || a.RequestedQty != 0.57 {
		t.Fatalf("the exact request quantities are native %v base %v, want 57 and 0.57", a.NativeQty, a.RequestedQty)
	}
	out, err := r.okx.Submit(a)
	if err != nil {
		t.Fatal(err)
	}
	got := r.f.read("okx")
	pairVenueNoGuard(t, got)
	wire := pairVenueWire(t, got)
	if len(wire) != 1 {
		t.Fatalf("want one order call, got %v", wire)
	}
	w := wire[0]
	params, _ := w["params"].(map[string]any)
	headers, _ := w["headers"].(map[string]any)
	if w["symbol"] != "BTC/USDT:USDT" || w["type"] != "limit" || w["side"] != "buy" || w["amount"] != "57.0" || !pairDecimalEqualsFloat(w["price"].(string), 60600.3) ||
		params["tdMode"] != "cross" || params["clOrdId"] != "fx01" || params["reduceOnly"] != true || params["timeInForce"] != "IOC" ||
		headers["expTime"] != fmt.Sprint(a.AcceptDeadlineMs) {
		t.Fatalf("the wire request %v does not carry the saved native size, bound, IOC, reduce-only flag, client id and expTime", w)
	}
	if after, _ := got["headers_after"].(map[string]any); after["expTime"] != nil {
		t.Fatalf("the expTime header leaked past the order call: %v", after)
	}
	if out.Outcome != basketVenueFilled || !out.Terminal || !basketQtyEqual(out.CumQty, 0.57) || !basketQtyEqual(out.CumNotional, 0.57*60600.3) || !out.FeeFinal {
		t.Fatalf("outcome %+v, want 0.57 base at the bound with fee finality", out)
	}
	if len(out.FillCoverage) != 2 || out.FillCoverage[0].BaseQty != "0.2" || out.FillCoverage[1].BaseQty != "0.37" {
		t.Fatalf("fill coverage %+v must normalize each fill once to base units", out.FillCoverage)
	}
	for _, fee := range out.FeeCoverage {
		if fee.FeeAmount != "0.25" || fee.FeeCurrency != "USDT" {
			t.Fatalf("fee record %+v, want the OKX charged fee negated once to a 0.25 USDT cost", fee)
		}
	}
	if why := pairOutcomeError(a, out); why != "" {
		t.Fatalf("the mapped outcome is refused by the coordinator rules: %s", why)
	}
	r.f.write("okx", map[string]any{"now_ms": r.now, "fee": "0.1"})
	b := r.action(r.okxM, pairPurposeEntry, "sell", entrySideShort, 0.03, 59700, "fx02")
	out, err = r.okx.Submit(b)
	if err != nil || len(out.FeeCoverage) != 1 || out.FeeCoverage[0].FeeAmount != "-0.1" {
		t.Fatalf("an OKX rebate must stay a signed negative fee, got %+v (%v)", out, err)
	}
}

func TestBasketPairVenueRealRefusesBeforeAnyOrder(t *testing.T) {
	type tc struct {
		provider string
		state    map[string]any
		mutate   func(r *pairVenueRig, a *basketAction)
	}
	cases := map[string]tc{
		"hyperliquid deadline reached in the child": {provider: "hyperliquid", mutate: func(r *pairVenueRig, a *basketAction) {
			r.f.write("hyperliquid", map[string]any{"now_ms": a.AcceptDeadlineMs})
		}},
		"hyperliquid deadline reached after the expiry is set": {provider: "hyperliquid", mutate: func(r *pairVenueRig, a *basketAction) {
			r.f.write("hyperliquid", map[string]any{"now_ms": r.now, "after_expiry_ms": a.AcceptDeadlineMs})
		}},
		"hyperliquid request names another account": {provider: "hyperliquid", mutate: func(r *pairVenueRig, a *basketAction) {
			r.f.write("hyperliquid", map[string]any{"now_ms": r.now})
			t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0x00000000000000000000000000000000000000b2")
		}},
		"hyperliquid native-booked purpose": {provider: "hyperliquid", mutate: func(r *pairVenueRig, a *basketAction) {
			r.f.write("hyperliquid", map[string]any{"now_ms": r.now})
			a.RequestJSON = strings.Replace(a.RequestJSON, `"purpose":"pair_exit"`, `"purpose":"pair_kill_switch"`, 1)
		}},
		"okx deadline reached in the child": {provider: "okx", mutate: func(r *pairVenueRig, a *basketAction) {
			r.f.write("okx", map[string]any{"now_ms": a.AcceptDeadlineMs + 1})
		}},
		"okx sdk would change the size": {provider: "okx", mutate: func(r *pairVenueRig, a *basketAction) {
			r.f.write("okx", map[string]any{"now_ms": r.now, "amount_text": "58"})
		}},
		"okx sdk would change the price": {provider: "okx", mutate: func(r *pairVenueRig, a *basketAction) {
			r.f.write("okx", map[string]any{"now_ms": r.now, "price_text": "60600.4"})
		}},
		"okx venue contract size differs": {provider: "okx", mutate: func(r *pairVenueRig, a *basketAction) {
			r.f.write("okx", map[string]any{"now_ms": r.now, "ct_val": "0.1"})
		}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			r := newPairVenueRig(t)
			var a *basketAction
			var v pairVenue
			if c.provider == "hyperliquid" {
				a, v = r.action(r.hlM, pairPurposeExit, "sell", entrySideLong, 0.06, 59500, "0x000000000000000000000000000000aa"), r.hl
			} else {
				a, v = r.action(r.okxM, pairPurposeExit, "buy", entrySideShort, 0.57, 60600.3, "fxref"), r.okx
			}
			c.mutate(r, a)
			out, err := v.Submit(a)
			if err != nil {
				t.Fatal(err)
			}
			got := r.f.read(c.provider)
			pairVenueNoGuard(t, got)
			if len(pairVenueWire(t, got)) != 0 || out.Outcome != pairVenueNeverSent {
				t.Fatalf("the refusal must end never sent with no order call, got %+v and wire %v", out, got["wire"])
			}
		})
	}
	t.Run("the go clock refuses at the deadline before launching the child", func(t *testing.T) {
		r := newPairVenueRig(t)
		a := r.action(r.hlM, pairPurposeExit, "sell", entrySideLong, 0.06, 59500, "0x000000000000000000000000000000ab")
		r.f.write("hyperliquid", map[string]any{"now_ms": r.now})
		r.now = a.AcceptDeadlineMs
		out, err := r.hl.Submit(a)
		if err != nil || out.Outcome != pairVenueNeverSent || len(pairVenueWire(t, r.f.read("hyperliquid"))) != 0 {
			t.Fatalf("a Go-side deadline at equality must send nothing, got %+v (%v)", out, err)
		}
	})
	t.Run("a zero-fill IOC is a terminal rejection", func(t *testing.T) {
		r := newPairVenueRig(t)
		r.f.write("hyperliquid", map[string]any{"now_ms": r.now, "behavior": "nomatch"})
		a := r.action(r.hlM, pairPurposeEntry, "buy", entrySideLong, 0.06, 60400, "0x000000000000000000000000000000ac")
		out, err := r.hl.Submit(a)
		if err != nil || out.Outcome != basketVenueRejected || !out.Terminal || out.CumQty != 0 {
			t.Fatalf("an IOC with no match must be a terminal zero-fill rejection, got %+v (%v)", out, err)
		}
		r.f.write("okx", map[string]any{"now_ms": r.now, "behavior": "venue_reject"})
		b := r.action(r.okxM, pairPurposeEntry, "sell", entrySideShort, 0.06, 59700, "fxrej")
		out, err = r.okx.Submit(b)
		if err != nil || out.Outcome != basketVenueRejected || !out.Terminal || out.CumQty != 0 {
			t.Fatalf("an OKX venue refusal must be a terminal zero-fill rejection, got %+v (%v)", out, err)
		}
	})
}

func TestBasketPairVenueRealQueryRecoversTheSavedIdentityAfterAcceptance(t *testing.T) {
	r := newPairVenueRig(t)
	r.f.write("hyperliquid", map[string]any{"now_ms": r.now, "behavior": "accept_then_crash"})
	a := r.action(r.hlM, pairPurposeExit, "sell", entrySideLong, 0.06, 59500, "0x000000000000000000000000000000c1")
	out, err := r.hl.Submit(a)
	if err != nil || out.Outcome != basketVenueUnknown {
		t.Fatalf("a crash after acceptance must leave the outcome unknown, got %+v (%v)", out, err)
	}
	q, err := r.hl.Query(a)
	if err != nil || q.Outcome != basketVenueFilled || !q.Terminal || !q.FeeFinal || q.ClientOrderID != a.ClientOrderID || !basketQtyEqual(q.CumQty, 0.06) {
		t.Fatalf("the query by the saved client id must recover the accepted reduction, got %+v (%v)", q, err)
	}
	r.f.write("okx", map[string]any{"now_ms": r.now, "behavior": "accept_then_crash"})
	b := r.action(r.okxM, pairPurposeExit, "buy", entrySideShort, 0.06, 60600.3, "fxcrash")
	out, err = r.okx.Submit(b)
	if err != nil || out.Outcome != basketVenueUnknown {
		t.Fatalf("an OKX crash after acceptance must leave the outcome unknown, got %+v (%v)", out, err)
	}
	q, err = r.okx.Query(b)
	if err != nil || q.Outcome != basketVenueFilled || !basketQtyEqual(q.CumQty, 0.06) || !q.FeeFinal {
		t.Fatalf("the OKX query by the saved client id must recover the reduction, got %+v (%v)", q, err)
	}
	c := r.action(r.okxM, pairPurposeExit, "buy", entrySideShort, 0.06, 60600.3, "fxnever")
	q, err = r.okx.Query(c)
	if err != nil || q.Outcome != pairVenueAbsent || !q.CoverageComplete || q.CoverageThroughMs <= 0 {
		t.Fatalf("a never-placed identity must read absent with complete fill coverage, got %+v (%v)", q, err)
	}
	st := r.f.read("okx")
	st["orders"].(map[string]any)["fxcrash"].(map[string]any)["sz"] = "7"
	r.f.write("okx", st)
	q, err = r.okx.Query(b)
	if err != nil || q.Outcome != basketVenueUnknown {
		t.Fatalf("a venue record whose size differs from the saved request must be unknown, got %+v (%v)", q, err)
	}
}

func TestBasketPairVenueRealStrictPositionReads(t *testing.T) {
	hlRow := func(coin, szi string) map[string]any {
		return map[string]any{"type": "oneWay", "position": map[string]any{"coin": coin, "szi": szi, "leverage": map[string]any{"type": "cross", "value": 2}}}
	}
	hlCases := map[string]struct {
		rows any
		want string
	}{
		"short position":            {rows: []any{hlRow("ETH", "1"), hlRow("BTC", "-0.57")}, want: "-0.57"},
		"complete list without BTC": {rows: []any{hlRow("ETH", "1")}, want: "0"},
		"malformed row":             {rows: []any{"junk", hlRow("BTC", "0.1")}},
		"duplicate coin":            {rows: []any{hlRow("BTC", "0.1"), hlRow("BTC", "0.2")}},
		"missing list":              {rows: nil},
	}
	for name, c := range hlCases {
		t.Run("hyperliquid "+name, func(t *testing.T) {
			r := newPairVenueRig(t)
			us := map[string]any{"withdrawable": "100"}
			if c.rows != nil {
				us["assetPositions"] = c.rows
			}
			r.f.write("hyperliquid", map[string]any{"now_ms": r.now, "user_state": us})
			p, err := r.hl.Position(context.Background(), r.hlM, r.now)
			if c.want == "" {
				if err == nil {
					t.Fatalf("a %s read must not prove a position, got %+v", name, p)
				}
				return
			}
			if err != nil || p.SignedNativeQty != c.want || p.ContractSize != "1" || p.PositionMode != "one_way" {
				t.Fatalf("position %+v (%v), want %s base", p, err, c.want)
			}
			if h, _ := p.computeHash(); h != p.EvidenceHash {
				t.Fatal("the position record is not sealed")
			}
		})
	}
	okxRow := func(inst, pos, side string) map[string]any {
		return map[string]any{"instId": inst, "pos": pos, "posSide": side, "mgnMode": "cross"}
	}
	okxCases := map[string]struct {
		resp any
		want string
		base float64
	}{
		"net short contracts":          {resp: map[string]any{"code": "0", "data": []any{okxRow("BTC-USDT-SWAP", "-57", "net")}}, want: "-57", base: -0.57},
		"complete empty answer":        {resp: map[string]any{"code": "0", "data": []any{}}, want: "0", base: 0},
		"duplicate instrument":         {resp: map[string]any{"code": "0", "data": []any{okxRow("BTC-USDT-SWAP", "1", "net"), okxRow("BTC-USDT-SWAP", "2", "net")}}},
		"long and short position mode": {resp: map[string]any{"code": "0", "data": []any{okxRow("BTC-USDT-SWAP", "1", "long")}}},
		"missing list":                 {resp: map[string]any{"code": "0"}},
		"venue error":                  {resp: map[string]any{"code": "50001", "data": []any{}}},
		"foreign instrument":           {resp: map[string]any{"code": "0", "data": []any{okxRow("ETH-USDT-SWAP", "1", "net")}}},
	}
	for name, c := range okxCases {
		t.Run("okx "+name, func(t *testing.T) {
			r := newPairVenueRig(t)
			r.f.write("okx", map[string]any{"now_ms": r.now, "positions": c.resp})
			p, err := r.okx.Position(context.Background(), r.okxM, r.now)
			if c.want == "" {
				if err == nil {
					t.Fatalf("a %s read must not prove a position, got %+v", name, p)
				}
				return
			}
			if err != nil || p.SignedNativeQty != c.want || p.ContractSize != "0.01" || p.PositionMode != "net" {
				t.Fatalf("position %+v (%v), want %s contracts of 0.01", p, err, c.want)
			}
			base, err := pairPositionBase(p)
			if err != nil || !basketQtyEqual(pairRatFloat(base), c.base) {
				t.Fatalf("base quantity %v (%v), want %v", base, err, c.base)
			}
		})
	}
}

func TestBasketPairVenueRealProbeVectorsParseWithoutAdapters(t *testing.T) {
	root := entryOperatorRepoRoot(t)
	t.Chdir(root)
	for _, argv := range pairOrderHLProbeArgvs {
		if err := probeOneCheckScript(hyperliquidCheckScript, argv); err != nil {
			t.Fatalf("hyperliquid probe %v: %v", argv, err)
		}
	}
	for _, argv := range pairOrderOKXProbeArgvs {
		if err := probeOneCheckScript(okxCheckScriptPath, argv); err != nil {
			t.Fatalf("okx probe %v: %v", argv, err)
		}
	}
	if err := probeOneCheckScript(okxFetchPositionsScript, pairPositionOKXProbeArgv); err != nil {
		t.Fatalf("okx position probe: %v", err)
	}
	bad := []string{pairOrderFlag + "=" + strings.Replace(pairOrderHLProbeRequestJSON, `"v":2`, `"v":1`, 1), "--mode=live", "--probe-only"}
	if err := probeOneCheckScript(hyperliquidCheckScript, bad); err == nil {
		t.Fatal("a probe request with the wrong version must fail the probe")
	}
}

func newPairVenueRealEnv(t *testing.T) (*pairTestEnv, *pairVenuePy, func(StrategyConfig) (basketVenue, error)) {
	t.Helper()
	f := newPairVenuePy(t)
	hlKey, okxKey := pairVenueTestAccounts(t)
	registerPairTestPolicy(t)
	dir := filepath.Join(t.TempDir(), "fixture")
	doc := pairTestDoc(t, dir, func(root map[string]interface{}) {
		pairLiveShapedMutate(root)
		pairTestMemberEntry(root, 0)["account_key"] = hlKey
		pairTestMemberEntry(root, 1)["account_key"] = okxKey
		limits := pairTestPair(root)["limits"].(map[string]interface{})
		limits["quote_age_ms"] = 20000
		limits["collection_timeout_ms"] = 20000
	})
	venues := newPairTestVenues(true)
	venues.hl.account, venues.okx.account = hlKey, okxKey
	quotes := newPairFixtureQuotes(t, doc)
	deps := pairLiveShapedDeps{
		VenueFor: venues.factory(),
		HLCloser: func(string, *float64, []int64) (*HyperliquidCloseResult, error) {
			return nil, fmt.Errorf("the controlled fixture sends no hyperliquid close")
		},
		OKXCloser: func(string, *float64) (*OKXCloseResult, error) {
			return nil, fmt.Errorf("the controlled fixture sends no okx close")
		},
		HLFetcher:    func(string) ([]HLPosition, error) { return nil, nil },
		OKXFetcher:   func() ([]OKXPosition, error) { return nil, nil },
		EntryQueries: func(*entryIntent) error { return fmt.Errorf("no live entry query is permitted") },
		Quotes:       quotes.collector,
	}
	adm, cfg, err := newPairLiveShapedAdmission(t, doc, dir, deps)
	if err != nil {
		t.Fatalf("live-shaped admission with venue account formats: %v", err)
	}
	e := &pairTestEnv{t: t, dir: dir, doc: doc, adm: adm, cfg: cfg, venues: venues, mu: &sync.RWMutex{}, now: pairTestStart(), quotes: quotes, generation: 1}
	e.runner = &pairTestRunner{respond: func(req pairPolicyRequest) (pairPolicyResult, error) { return pairHoldResult(req, "no edge"), nil }}
	e.store = openSourceStore(t, cfg)
	e.state = NewAppState()
	for _, sc := range cfg.Strategies {
		e.state.Strategies[sc.ID] = NewStrategyState(sc)
	}
	e.reopen(false)
	hlM, _ := e.rt().Contract.pairMember(pairTestHL)
	okxM, _ := e.rt().Contract.pairMember(pairTestOKX)
	if hlM.AccountKey != hlKey || okxM.AccountKey != okxKey {
		t.Fatalf("the contract binds %q and %q, want the venue account formats", hlM.AccountKey, okxM.AccountKey)
	}
	clock := func() time.Time { return e.now }
	hl, err := newHyperliquidPairVenue(adm, hlM, pairLiveTransport{Script: f.drivers["hl_check"], PositionScript: f.drivers["hl_check"], Partition: RiskPartition{Scope: ScopeLive}, Clock: clock, Substituted: true})
	if err != nil {
		t.Fatalf("hyperliquid pair venue: %v", err)
	}
	okx, err := newOKXPairVenue(adm, okxM, pairLiveTransport{Script: f.drivers["okx_check"], PositionScript: f.drivers["okx_positions"], Partition: RiskPartition{Scope: ScopeLive}, Clock: clock, Substituted: true})
	if err != nil {
		t.Fatalf("okx pair venue: %v", err)
	}
	real := func(sc StrategyConfig) (basketVenue, error) {
		switch sc.ID {
		case pairTestHL:
			return hl, nil
		case pairTestOKX:
			return okx, nil
		}
		return nil, fmt.Errorf("no live-shaped pair venue for %s", sc.ID)
	}
	e.bc.venueFor = real
	return e, f, real
}

func pairVenueFilledNative(t *testing.T, provider string, state map[string]any) *big.Rat {
	t.Helper()
	total := new(big.Rat)
	raw, _ := state["fills"].([]any)
	for _, x := range raw {
		fill := x.(map[string]any)
		qty, side, buy := fill["sz"], fill["side"], "B"
		if provider == "okx" {
			qty, buy = fill["fillSz"], "buy"
		}
		q, ok := new(big.Rat).SetString(qty.(string))
		if !ok {
			t.Fatalf("fixture fill %v has no readable size", fill)
		}
		if side != buy {
			q.Neg(q)
		}
		total.Add(total, q)
	}
	return total
}

func (f *pairVenuePy) venueTruth(nowMs int64, behavior string) {
	f.t.Helper()
	for _, p := range []string{"hyperliquid", "okx"} {
		st := map[string]any{}
		if b, err := os.ReadFile(f.states[p]); err == nil {
			if err := json.Unmarshal(b, &st); err != nil {
				f.t.Fatal(err)
			}
		}
		st["now_ms"] = nowMs
		delete(st, "behavior")
		if behavior != "" {
			st["behavior"] = behavior
		}
		pos, err := pairRatDecimal(pairVenueFilledNative(f.t, p, st))
		if err != nil {
			f.t.Fatal(err)
		}
		if p == "hyperliquid" {
			rows := []any{}
			if pos != "0" {
				rows = append(rows, map[string]any{"type": "oneWay", "position": map[string]any{"coin": "BTC", "szi": pos, "leverage": map[string]any{"type": "cross", "value": 2}}})
			}
			st["user_state"] = map[string]any{"withdrawable": "100", "assetPositions": rows}
		} else {
			rows := []any{}
			if pos != "0" {
				rows = append(rows, map[string]any{"instId": "BTC-USDT-SWAP", "pos": pos, "posSide": "net", "mgnMode": "cross"})
			}
			st["positions"] = map[string]any{"code": "0", "data": rows}
		}
		f.write(p, st)
	}
}

func pairVenueWireCount(t *testing.T, state map[string]any, provider, cloid string) int {
	t.Helper()
	n := 0
	for _, w := range pairVenueWire(t, state) {
		id := w["cloid"]
		if provider == "okx" {
			params, _ := w["params"].(map[string]any)
			id = params["clOrdId"]
		}
		if id == cloid {
			n++
		}
	}
	return n
}

func TestBasketPairVenueRealCoordinatorRunsBothVenuesAndRecoversBySavedIdentity(t *testing.T) {
	e, f, real := newPairVenueRealEnv(t)
	pairEnterExitRunner(e, 0.06)
	run := func(i int, behavior string) time.Time {
		at := pairSlotAt(pairTestStart(), i)
		e.slot(at, fmt.Sprintf("in-%d", i))
		f.venueTruth(at.Add(time.Second).UnixMilli(), behavior)
		e.step(at.Add(time.Second))
		return at
	}
	run(0, "")
	e.assertBooks("first leg", 0, -0.06)
	entries := e.actionsByPurpose(pairPurposeEntry)
	if len(entries) != 1 || entries[0].StrategyID != pairTestOKX || entries[0].Status != basketActionCommitted || !entries[0].FeeFinal {
		t.Fatalf("the OKX first leg must commit from its own submit answer in the submit step, got %+v", entries)
	}
	first := entries[0]
	if n := pairVenueWireCount(t, f.read("okx"), "okx", first.ClientOrderID); n != 1 || len(pairVenueWire(t, f.read("hyperliquid"))) != 0 {
		t.Fatalf("the first leg must send one OKX order (%d) and no hedge yet", n)
	}
	if pm := e.savedPositionMatch(); pm == nil || pm.Status != pairPositionMatched {
		t.Fatalf("the barrier must match through the real position scripts before the first leg, got %+v", pm)
	}
	run(1, "")
	e.assertBooks("hedged", 0.06, -0.06)
	entries = e.actionsByPurpose(pairPurposeEntry)
	if len(entries) != 2 || e.lifecycle() != pairLifecycleMatched {
		t.Fatalf("the hedge must follow at the next slot and match the pair, got lifecycle %s and %+v", e.lifecycle(), entries)
	}
	for _, a := range entries {
		if a.Status != basketActionCommitted || !a.FeeFinal {
			t.Fatalf("entry %s must commit with fee finality, got %s", a.ActionID, a.Status)
		}
	}
	at := run(2, "accept_then_crash")
	var crashed []*basketAction
	for _, a := range e.actionsByPurpose(pairPurposeExit) {
		if basketActionPending(a.Status) {
			crashed = append(crashed, a)
		}
	}
	if len(crashed) == 0 || e.lifecycle() != pairLifecycleExiting {
		t.Fatalf("a crash after venue acceptance must leave the exit reduction unresolved in the exiting lifecycle, got %s and %+v", e.lifecycle(), e.actionsByPurpose(pairPurposeExit))
	}
	ids := map[string]string{}
	for _, a := range crashed {
		provider := "hyperliquid"
		if a.StrategyID == pairTestOKX {
			provider = "okx"
		}
		if n := pairVenueWireCount(t, f.read(provider), provider, a.ClientOrderID); n != 1 {
			t.Fatalf("the crashed reduction %s reached the venue %d times, want once", a.ActionID, n)
		}
		ids[a.ActionID] = provider
	}
	e.restart()
	e.bc.venueFor = real
	for id := range ids {
		if a := e.rt().Actions[id]; a == nil || !basketActionPending(a.Status) {
			t.Fatalf("the restart must keep the crashed reduction %s unresolved for its saved-identity query", id)
		}
	}
	for i := 1; i <= 10 && !(e.lifecycle() == pairLifecycleFlat && basketQtyEqual(e.signed(pairTestHL), 0) && basketQtyEqual(e.signed(pairTestOKX), 0)); i++ {
		now := at.Add(time.Second + time.Duration(i)*20*time.Second)
		f.venueTruth(now.UnixMilli(), "")
		e.step(now)
	}
	e.assertBooks("exited", 0, 0)
	if e.lifecycle() != pairLifecycleFlat {
		t.Fatalf("the exited pair must reset to flat, got %s", e.lifecycle())
	}
	for id, provider := range ids {
		a := e.rt().Actions[id]
		if a.Status != basketActionCommitted || !a.FeeFinal || !basketQtyEqual(a.ConfirmedQty, 0.06) {
			t.Fatalf("the query by the saved client id must settle %s, got %s fee final %v confirmed %v", id, a.Status, a.FeeFinal, a.ConfirmedQty)
		}
		if n := pairVenueWireCount(t, f.read(provider), provider, a.ClientOrderID); n != 1 {
			t.Fatalf("the recovered reduction %s was sent %d times, want once", id, n)
		}
	}
	for _, p := range []string{"hyperliquid", "okx"} {
		st := f.read(p)
		pairVenueNoGuard(t, st)
		member := pairTestHL
		if p == "okx" {
			member = pairTestOKX
		}
		n := 0
		for _, a := range e.rt().Actions {
			if a.StrategyID == member {
				n++
				if a.Status != basketActionCommitted {
					t.Fatalf("action %s ended %s, want committed", a.ActionID, a.Status)
				}
			}
		}
		if w := len(pairVenueWire(t, st)); w != n || n != 2 {
			t.Fatalf("%s saw %d order calls for %d saved actions, want one entry and one reduction each", p, w, n)
		}
		if pos := pairVenueFilledNative(t, p, st); pos.Sign() != 0 {
			t.Fatalf("%s venue position is %s after the exit, want flat", p, pos.FloatString(6))
		}
	}
	if pm := e.savedPositionMatch(); pm == nil || pm.Status != pairPositionMatched {
		t.Fatalf("the barrier must end matched through the real position scripts, got %+v", pm)
	}
}

func pairVenueChildDirect(t *testing.T, v *pairLiveBase, a *basketAction) basketVenueOutcome {
	t.Helper()
	wire, err := pairTransportRequestJSON(a.RequestJSON)
	if err != nil {
		t.Fatal(err)
	}
	stdout, stderr, runErr := runPythonSideEffectWithTimeout(v.t.Script, []string{pairOrderFlag + "=" + wire, "--mode=live"}, pairOrderScriptTimout)
	out, perr := pairParseScriptOutput(stdout, stderr, runErr)
	if perr != nil || out.Outcome == nil || out.Outcome.Mode != "order" {
		t.Fatalf("the child must answer with an order outcome, got %+v (%v; stderr %s)", out, perr, stderr)
	}
	return pairMapScriptOutcome(v.provider, a, v.m, *out.Outcome)
}

func TestBasketPairVenueRealReferenceAgeChecksInTheChild(t *testing.T) {
	type tc struct {
		provider string
		direct   bool
		want     string
		mutate   func(r *pairVenueRig, a *basketAction)
	}
	cases := map[string]tc{
		"hyperliquid reference expired when the child starts": {provider: "hyperliquid", want: "(checked before the adapter was built)", mutate: func(r *pairVenueRig, a *basketAction) {
			r.f.write("hyperliquid", map[string]any{"now_ms": r.now - 100 + 20001})
		}},
		"hyperliquid reference expires during the adapter setup": {provider: "hyperliquid", want: "(checked immediately before the order call)", mutate: func(r *pairVenueRig, a *basketAction) {
			r.f.write("hyperliquid", map[string]any{"now_ms": r.now, "after_expiry_ms": r.now - 100 + 20001})
		}},
		"hyperliquid request without reference authority": {provider: "hyperliquid", direct: true, want: "needs its saved reference authority", mutate: func(r *pairVenueRig, a *basketAction) {
			r.f.write("hyperliquid", map[string]any{"now_ms": r.now})
			a.RequestJSON = pairVenueStripAuthority(t, a.RequestJSON, "reference_receipt_ms", "reference_max_age_ms", "reference_px")
		}},
		"hyperliquid request with only the reference level": {provider: "hyperliquid", direct: true, want: "reference authority is incomplete", mutate: func(r *pairVenueRig, a *basketAction) {
			r.f.write("hyperliquid", map[string]any{"now_ms": r.now})
			a.RequestJSON = pairVenueStripAuthority(t, a.RequestJSON, "reference_receipt_ms", "reference_max_age_ms")
		}},
		"okx reference expired when the child starts": {provider: "okx", want: "(checked before the adapter was built)", mutate: func(r *pairVenueRig, a *basketAction) {
			r.f.write("okx", map[string]any{"now_ms": r.now - 100 + 20001})
		}},
		"okx reference expires during the precision check": {provider: "okx", want: "(checked immediately before the order call)", mutate: func(r *pairVenueRig, a *basketAction) {
			r.f.write("okx", map[string]any{"now_ms": r.now, "after_instruments_ms": r.now - 100 + 20001})
		}},
		"okx saved bound does not cross the saved reference": {provider: "okx", direct: true, want: "so it does not cross", mutate: func(r *pairVenueRig, a *basketAction) {
			r.f.write("okx", map[string]any{"now_ms": r.now})
			a.RequestJSON = strings.Replace(a.RequestJSON, `"reference_px":"60100.3"`, `"reference_px":"60700"`, 1)
		}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			r := newPairVenueRig(t)
			var a *basketAction
			var v pairVenue
			var base *pairLiveBase
			if c.provider == "hyperliquid" {
				a, v, base = r.action(r.hlM, pairPurposeProtective, "sell", entrySideLong, 0.06, 59500, "0x000000000000000000000000000000d1"), r.hl, &r.hl.pairLiveBase
			} else {
				a, v, base = r.action(r.okxM, pairPurposeProtective, "buy", entrySideShort, 0.57, 60600.3, "fxage"), r.okx, &r.okx.pairLiveBase
			}
			before := a.RequestJSON
			c.mutate(r, a)
			if c.direct && a.RequestJSON == before {
				t.Fatalf("the fixture request was not changed: %s", before)
			}
			var out basketVenueOutcome
			if c.direct {
				if why := pairReductionAgeRefusal(a.RequestJSON, r.now); why == "" {
					t.Fatal("the direct case must hold a request that the Go transport check would also refuse")
				}
				out = pairVenueChildDirect(t, base, a)
			} else {
				var err error
				if out, err = v.Submit(a); err != nil {
					t.Fatal(err)
				}
			}
			got := r.f.read(c.provider)
			pairVenueNoGuard(t, got)
			if len(pairVenueWire(t, got)) != 0 || out.Outcome != pairVenueNeverSent {
				t.Fatalf("the child must end never sent with no order call while the acceptance deadline is still open, got %+v and wire %v", out, got["wire"])
			}
			if strings.Contains(out.Detail, "the order script was not launched") || !strings.Contains(out.Detail, c.want) {
				t.Fatalf("the refusal must come from the child itself with %q, got %q", c.want, out.Detail)
			}
		})
	}
	t.Run("the go clock refuses an expired reference before launching the child", func(t *testing.T) {
		r := newPairVenueRig(t)
		a := r.action(r.hlM, pairPurposeExit, "sell", entrySideLong, 0.06, 59500, "0x000000000000000000000000000000d2")
		r.f.write("hyperliquid", map[string]any{"now_ms": r.now})
		r.now += 20000
		out, err := r.hl.Submit(a)
		if err != nil || out.Outcome != pairVenueNeverSent || len(pairVenueWire(t, r.f.read("hyperliquid"))) != 0 {
			t.Fatalf("an expired reference on the Go clock must send nothing, got %+v (%v)", out, err)
		}
	})
	t.Run("an older request without authority stays queryable and is never resent", func(t *testing.T) {
		r := newPairVenueRig(t)
		r.f.write("hyperliquid", map[string]any{"now_ms": r.now, "behavior": "accept_then_crash"})
		a := r.action(r.hlM, pairPurposeExit, "sell", entrySideLong, 0.06, 59500, "0x000000000000000000000000000000d3")
		if out, err := r.hl.Submit(a); err != nil || out.Outcome != basketVenueUnknown {
			t.Fatalf("a crash after acceptance must leave the outcome unknown, got %+v (%v)", out, err)
		}
		a.RequestJSON = pairVenueStripAuthority(t, a.RequestJSON, "reference_receipt_ms", "reference_max_age_ms", "reference_px")
		q, err := r.hl.Query(a)
		if err != nil || q.Outcome != basketVenueFilled || !basketQtyEqual(q.CumQty, 0.06) {
			t.Fatalf("the saved identity query must still settle a request without the new authority, got %+v (%v)", q, err)
		}
		if out, err := r.hl.Submit(a); err != nil || out.Outcome != pairVenueNeverSent {
			t.Fatalf("a request without the new authority must never send again, got %+v (%v)", out, err)
		}
		if n := pairVenueWireCount(t, r.f.read("hyperliquid"), "hyperliquid", a.ClientOrderID); n != 1 {
			t.Fatalf("the identity reached the venue %d times, want once", n)
		}
	})
}

func pairVenueStripAuthority(t *testing.T, raw string, fields ...string) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range fields {
		delete(m, k)
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
