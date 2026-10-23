//go:build pyintegration

package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

const quoteFakeAdapterDriver = `import atexit, importlib.util, json, os, runpy, sys, types
root = os.environ["QUOTE_FAKE_ROOT"]
state_path = os.environ["QUOTE_FAKE_STATE"]
state = json.load(open(state_path))
spec = importlib.util.spec_from_file_location("real_hl_adapter_for_quote_test", os.path.join(root, "platforms", "hyperliquid", "adapter.py"))
real = importlib.util.module_from_spec(spec)
spec.loader.exec_module(real)

def save():
    with open(state_path, "w") as fh:
        json.dump(state, fh)
atexit.register(save)

def order_reply(kind, oid=None, detail=""):
    if kind == "resting":
        return {"status": "ok", "response": {"type": "order", "data": {"statuses": [{"resting": {"oid": oid}}]}}}
    if kind == "filled":
        return {"status": "ok", "response": {"type": "order", "data": {"statuses": [{"filled": {"oid": oid, "totalSz": state.get("filled_sz", "0"), "avgPx": "1"}}]}}}
    return {"status": "ok", "response": {"type": "order", "data": {"statuses": [{"error": detail}]}}}

class HyperliquidExchangeAdapter:
    def __init__(self):
        if state.get("construct_raises"):
            raise RuntimeError("adapter construction reached the venue")
        self.account_address = state["account"]
    def lot_size_decimals(self, symbol):
        return 3
    def check_quote_request(self, symbol, size, px):
        real.quote_request_unchanged(symbol, size, px, 3)
    def set_order_expiry(self, ms):
        state["expiry"] = ms
    def _place(self, symbol, is_buy, size, px, cloid, reduce_only, kind):
        state.setdefault("placed", []).append({"symbol": symbol, "is_buy": is_buy, "size": size, "px": px, "cloid": cloid, "reduce_only": reduce_only, "kind": kind})
        behavior = state.get("behavior", "resting")
        state["next_oid"] = state.get("next_oid", 900) + 1
        oid = state["next_oid"]
        if behavior in ("resting", "accept_then_raise"):
            state.setdefault("orders", {})[cloid.lower()] = {"oid": oid, "status": "open", "origSz": str(size), "sz": str(size), "timestamp": 1700}
        if behavior == "accept_then_raise":
            raise ConnectionError("connection reset after send")
        if behavior == "raise":
            raise ConnectionError("connection refused")
        if behavior == "reject":
            return order_reply("error", detail="Post only order would have immediately matched")
        return order_reply("resting", oid)
    def limit_open(self, symbol, is_buy, size, limit_px, tif="Alo", cloid=None, reduce_only=False, require_exact=False):
        state.setdefault("tifs", []).append(tif)
        return self._place(symbol, is_buy, size, limit_px, cloid, reduce_only, "limit")
    def place_stop_loss(self, symbol, sz, trigger_px, is_buy, limit_slippage_pct=5.0, cloid=None, require_exact=False):
        return self._place(symbol, is_buy, sz, trigger_px, cloid, True, "stop")
    def cancel_by_cloid(self, symbol, cloid):
        o = state.get("orders", {}).get(cloid.lower())
        if o is not None:
            o["status"] = "canceled"
        return {"status": "ok"}
    def query_order_by_cloid(self, cloid):
        o = state.get("orders", {}).get(cloid.lower())
        if o is None:
            return {"status": "unknownOid"}
        return {"status": "order", "order": {"status": o["status"], "statusTimestamp": 1800, "order": {"oid": o["oid"], "origSz": o["origSz"], "sz": o["sz"], "timestamp": o["timestamp"], "cloid": cloid}}}
    def user_fills_since(self, since_ms):
        pages = state.get("pages")
        if pages is not None:
            idx = state.get("page_idx", 0)
            state["page_idx"] = idx + 1
            return pages[min(idx, len(pages) - 1)]
        return [f for f in state.get("fills", []) if int(f["time"]) >= since_ms]
    def strict_position(self, symbol):
        return state["position"]

mod = types.ModuleType("adapter")
mod.HyperliquidExchangeAdapter = HyperliquidExchangeAdapter
sys.modules["adapter"] = mod
script = os.path.join(root, "shared_scripts", "check_hyperliquid.py")
sys.argv = [script] + sys.argv[1:]
runpy.run_path(script, run_name="__main__")
`

type quotePyFixture struct {
	t      *testing.T
	root   string
	driver string
	state  string
	venue  *quoteLiveVenue
}

func newQuotePyFixture(t *testing.T, state map[string]any) *quotePyFixture {
	t.Helper()
	root := entryOperatorRepoRoot(t)
	dir := t.TempDir()
	f := &quotePyFixture{t: t, root: root, driver: filepath.Join(dir, "driver.py"), state: filepath.Join(dir, "state.json")}
	if err := os.WriteFile(f.driver, []byte(quoteFakeAdapterDriver), 0o600); err != nil {
		t.Fatal(err)
	}
	f.write(state)
	t.Setenv("QUOTE_FAKE_ROOT", root)
	t.Setenv("QUOTE_FAKE_STATE", f.state)
	t.Setenv("HYPERLIQUID_TESTNET", "1")
	t.Chdir(root)
	f.venue = newQuoteLiveVenue("hyperliquid:testnet:0xquote", "")
	f.venue.script = f.driver
	return f
}

func (f *quotePyFixture) write(state map[string]any) {
	f.t.Helper()
	if _, ok := state["account"]; !ok {
		state["account"] = "0xquote"
	}
	b, err := json.Marshal(state)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(f.state, b, 0o600); err != nil {
		f.t.Fatal(err)
	}
}

func (f *quotePyFixture) read() map[string]any {
	f.t.Helper()
	b, err := os.ReadFile(f.state)
	if err != nil {
		f.t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		f.t.Fatal(err)
	}
	return out
}

func (f *quotePyFixture) placed() []any {
	p, _ := f.read()["placed"].([]any)
	return p
}

func quotePyRequest(role string) quoteOrderRequest {
	r := quoteOrderRequest{Coin: "BTC", Role: role, Side: "buy", Px: "99.5", Size: "0.012", PostOnly: true, TIF: "Alo", ExpiryMs: 1 << 50,
		ClientOrderID: "0x0000000000000000000000000000abcd"}
	if role == quoteRoleStop {
		r.Side, r.Px, r.TriggerPx, r.TIF, r.PostOnly, r.ReduceOnly = "sell", "", "94.05", "", false, true
	}
	return r
}

func TestQuoteVenuePythonSavedIdentitiesRecoveryAndProbes(t *testing.T) {
	ctx := context.Background()
	t.Run("both startup probe paths accept every quote argument without venue access", func(t *testing.T) {
		f := newQuotePyFixture(t, map[string]any{"construct_raises": true})
		for _, argv := range quoteProbeArgvs {
			if err := probeOneCheckScript(f.driver, argv); err != nil {
				t.Fatalf("probe %v: %v", argv[0], err)
			}
		}
		if p := f.placed(); len(p) != 0 {
			t.Fatal("a probe sent an order")
		}
	})
	t.Run("a resting placement carries the saved client id, reduce-only flag, post-only tif and request expiry", func(t *testing.T) {
		f := newQuotePyFixture(t, map[string]any{})
		req := quotePyRequest(quoteRoleBid)
		req.ReduceOnly = true
		out, err := f.venue.Place(ctx, req)
		if err != nil || out.Status != quoteOutcomeResting || out.VenueOrderID == "" {
			t.Fatalf("outcome %+v %v", out, err)
		}
		st := f.read()
		placed := st["placed"].([]any)[0].(map[string]any)
		if placed["cloid"] != req.ClientOrderID || placed["reduce_only"] != true || st["tifs"].([]any)[0] != "Alo" || st["expiry"].(float64) != float64(req.ExpiryMs) {
			t.Fatalf("the adapter request must carry the saved identity: %v", st)
		}
	})
	t.Run("a request that rounding would change is refused before submission", func(t *testing.T) {
		f := newQuotePyFixture(t, map[string]any{})
		req := quotePyRequest(quoteRoleBid)
		req.Size = "0.0125"
		out, err := f.venue.Place(ctx, req)
		if err != nil || out.Status != quoteOutcomeRejected || len(f.placed()) != 0 {
			t.Fatalf("a changed request must be refused with nothing sent: %+v %v", out, err)
		}
	})
	t.Run("a post-only reject is a conclusive rejection", func(t *testing.T) {
		f := newQuotePyFixture(t, map[string]any{"behavior": "reject"})
		out, err := f.venue.Place(ctx, quotePyRequest(quoteRoleAsk))
		if err != nil || out.Status != quoteOutcomeRejected {
			t.Fatalf("outcome %+v %v", out, err)
		}
	})
	t.Run("an uncertain acceptance is unknown and client lookup recovers the order id with no resend", func(t *testing.T) {
		f := newQuotePyFixture(t, map[string]any{"behavior": "accept_then_raise"})
		req := quotePyRequest(quoteRoleBid)
		out, err := f.venue.Place(ctx, req)
		if err != nil || out.Status != quoteOutcomeUnknown {
			t.Fatalf("an exception after send must be unknown: %+v %v", out, err)
		}
		st, err := f.venue.Status(ctx, req, "")
		if err != nil || st.Status != quoteOutcomeResting || st.VenueOrderID == "" || st.AcceptedAtMs != 1700 {
			t.Fatalf("client lookup must recover the resting order: %+v %v", st, err)
		}
		if n := len(f.placed()); n != 1 {
			t.Fatalf("status recovery resent the order: %d sends", n)
		}
		c, err := f.venue.Cancel(ctx, req, st.VenueOrderID)
		if err != nil || c.Status != quoteOutcomeCancelled {
			t.Fatalf("cancel by client id: %+v %v", c, err)
		}
		missing := req
		missing.ClientOrderID = "0x0000000000000000000000000000ffff"
		if a, _ := f.venue.Status(ctx, missing, ""); a.Status != quoteOutcomeAbsent {
			t.Fatalf("an unknown client id is absent, never filled or zero: %+v", a)
		}
	})
	t.Run("the stop role places a reduce-only trigger with its saved identity", func(t *testing.T) {
		f := newQuotePyFixture(t, map[string]any{})
		out, err := f.venue.Place(ctx, quotePyRequest(quoteRoleStop))
		if err != nil || out.Status != quoteOutcomeResting {
			t.Fatalf("outcome %+v %v", out, err)
		}
		placed := f.placed()[0].(map[string]any)
		if placed["kind"] != "stop" || placed["reduce_only"] != true || placed["px"].(float64) != 94.05 {
			t.Fatalf("stop request %v", placed)
		}
	})
	t.Run("raw recovery keeps exact repeats, reports conflicting copies and never turns incomplete history into zero", func(t *testing.T) {
		fill := map[string]any{"coin": "BTC", "px": "99.5", "sz": "0.012", "side": "B", "time": 2000, "startPosition": "0.0", "oid": 901, "tid": 7,
			"fee": "0.0012", "feeToken": "USDC", "cloid": "0x0000000000000000000000000000ABCD"}
		changed := map[string]any{}
		for k, v := range fill {
			changed[k] = v
		}
		changed["fee"] = "0.0099"
		f := newQuotePyFixture(t, map[string]any{"fills": []any{fill, fill, changed}, "position": map[string]any{"size": "0.012", "liquidation_px": 80.5, "free_margin": "123.45"}})
		rec, err := f.venue.RecoverFills(ctx, "BTC", 1000, 3000)
		if err != nil {
			t.Fatal(err)
		}
		if len(rec.Fills) != 1 || len(rec.Conflicts) != 1 || !rec.Coverage.Complete || rec.Coverage.ThroughMs != 3000 {
			t.Fatalf("recovery %+v", rec)
		}
		got := rec.Fills[0]
		if got.Side != "buy" || got.Px != "99.5" || got.Size != "0.012" || got.Fee != "0.0012" || got.StartPosition != "0.0" || got.ClientOrderID != "0x0000000000000000000000000000abcd" || got.Account != "hyperliquid:testnet:0xquote" {
			t.Fatalf("canonical raw fill %+v", got)
		}
		if err := got.validate(); err != nil {
			t.Fatal(err)
		}
		pos, err := f.venue.Position(ctx, "BTC")
		if err != nil || pos.Size != "0.012" || pos.LiquidationPx != 80.5 || pos.FreeMargin != "123.45" {
			t.Fatalf("strict position %+v %v", pos, err)
		}
		page := make([]any, quoteHLFillsMinPageForTest)
		for i := range page {
			page[i] = map[string]any{"coin": "BTC", "px": "1", "sz": "1", "side": "A", "time": 1000, "oid": 1, "tid": i, "fee": "0", "feeToken": "USDC"}
		}
		f.write(map[string]any{"pages": []any{page}, "position": map[string]any{"size": "0", "liquidation_px": 0, "free_margin": ""}})
		rec, err = f.venue.RecoverFills(ctx, "BTC", 1000, 3000)
		if err != nil || rec.Coverage.Complete || rec.Coverage.Reason == "" {
			t.Fatalf("a nonadvancing full page must leave coverage incomplete: %+v %v", rec.Coverage, err)
		}
	})
}

const quoteHLFillsMinPageForTest = 500
