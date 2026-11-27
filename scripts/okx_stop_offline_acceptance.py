import json
import os
import subprocess
import sys
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
sys.path.insert(0, os.path.join(ROOT, "shared_tools"))

from okx_stop_geometry import finalize_stop

INST = "BTC-USDT-SWAP"


class Fixture:
    def __init__(self):
        self.lock = threading.Lock()
        self.requests = []
        self.pending = {}
        self.history = {}
        self.fills = []
        self.positions = []
        self.fail_next = set()
        self.place_count = 0

    def record_place(self, params):
        self.place_count += 1
        algo_id = "algo-%d" % self.place_count
        row = {
            "instId": params.get("instId"),
            "algoId": algo_id,
            "algoClOrdId": params.get("algoClOrdId"),
            "state": "live",
            "slTriggerPx": params.get("slTriggerPx"),
            "sz": params.get("sz"),
            "ordType": "conditional",
        }
        self.pending[params.get("algoClOrdId")] = row
        return algo_id

    def reply(self, method, params):
        if method == "privateGetAccountConfig":
            return {"code": "0", "data": [{"posMode": "net_mode", "acctLv": "2"}]}
        if method == "privateGetAccountPositions":
            return {"code": "0", "data": list(self.positions)}
        if method == "privateGetTradeOrdersPending":
            return {"code": "0", "data": []}
        if method == "privateGetTradeOrdersAlgoPending":
            cid = params.get("algoClOrdId")
            if cid:
                row = self.pending.get(cid)
                return {"code": "0", "data": [row] if row else []}
            return {"code": "0", "data": list(self.pending.values())}
        if method == "privateGetTradeOrderAlgo":
            cid = params.get("algoClOrdId")
            row = self.pending.get(cid) or self.history.get(cid)
            return {"code": "0", "data": [row]} if row else {"code": "51603", "data": []}
        if method == "publicGetPublicInstruments":
            return {"code": "0", "data": [{
                "instId": INST, "tickSz": "0.1", "lotSz": "0.01", "minSz": "0.01",
                "ctVal": "0.01", "state": "live", "instFamily": "BTC-USDT",
            }]}
        if method == "publicGetPublicMarkPrice":
            return {"code": "0", "data": [{"instId": INST, "markPx": "50000"}]}
        if method == "publicGetPublicPositionTiers":
            return {"code": "0", "data": [{"minSz": "0", "maxSz": "100000", "mmr": "0.005"}]}
        if method == "privateGetAccountBalance":
            return {"code": "0", "data": [{"details": [{"ccy": "USDT", "eq": "1000", "cashBal": "1000"}]}]}
        if method == "privateGetAccountTradeFee":
            return {"code": "0", "data": [{"taker": "-0.0005"}]}
        if method == "privatePostTradeOrderAlgo":
            algo_id = self.record_place(params)
            return {"code": "0", "data": [{"sCode": "0", "algoId": algo_id, "algoClOrdId": params.get("algoClOrdId")}]}
        if method == "privatePostTradeAmendAlgos":
            return {"code": "0", "data": [{"sCode": "0", "algoId": params.get("algoId")}]}
        if method == "privatePostTradeCancelAlgos":
            if not isinstance(params, list) or len(params) != 1:
                return {"code": "1", "data": []}
            cid = params[0].get("algoClOrdId")
            row = self.pending.pop(cid, None)
            if row is not None:
                row["state"] = "canceled"
                self.history[cid] = row
            return {"code": "0", "data": [{"sCode": "0"}]}
        if method == "privateGetTradeOrder":
            return {"code": "0", "data": [{"instId": INST, "ordId": params.get("ordId"), "reduceOnly": "true", "state": "filled"}]}
        if method == "privateGetTradeFills":
            return {"code": "0", "data": list(self.fills)}
        return {"code": "1", "data": [], "msg": "unexpected " + method}


def serve(fix):
    class Handler(BaseHTTPRequestHandler):
        def do_POST(self):
            length = int(self.headers.get("Content-Length") or "0")
            body = json.loads(self.rfile.read(length).decode())
            method = body.get("method")
            params = body.get("params") or {}
            with fix.lock:
                fix.requests.append(body)
                fail = method in fix.fail_next
                if fail:
                    fix.fail_next.remove(method)
                    if method == "privatePostTradeOrderAlgo":
                        fix.record_place(params)
                else:
                    payload = fix.reply(method, params)
            if fail:
                self.send_response(500)
                self.end_headers()
                return
            raw = json.dumps(payload).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(raw)))
            self.end_headers()
            self.wfile.write(raw)

        def log_message(self, fmt, *args):
            return

    httpd = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    thread = threading.Thread(target=httpd.serve_forever, daemon=True)
    thread.start()
    return httpd


def run(args, env):
    return subprocess.run(
        [sys.executable, os.path.join(ROOT, "shared_scripts", "check_okx.py"), *args],
        cwd=ROOT, env=env, capture_output=True, text=True,
    )


def expect(cond, detail):
    if not cond:
        raise SystemExit(detail)


def load(proc, what):
    expect(proc.stdout.strip() != "", "%s produced no JSON\n%s" % (what, proc.stderr))
    return json.loads(proc.stdout)


def main():
    fix = Fixture()
    httpd = serve(fix)
    url = "http://127.0.0.1:%d" % httpd.server_address[1]
    base = os.environ.copy()
    for key in ("OKX_API_KEY", "OKX_API_SECRET", "OKX_PASSPHRASE", "OKX_OFFLINE_FIXTURE", "OKX_SANDBOX"):
        base.pop(key, None)
    try:
        before = len(fix.requests)
        probe = run(["--okx-stop", "--op", "preflight", "--symbol", "BTC", "--inst-type", "swap", "--probe-only"], base)
        expect(probe.returncode == 0, probe.stderr)
        payload = load(probe, "probe")
        expect(payload.get("contract") == "okx_stop_v1" and payload.get("probe") is True, payload)
        expect(len(fix.requests) == before, "probe contacted the fixture")

        refused_env = dict(base)
        refused_env["OKX_OFFLINE_FIXTURE"] = url
        refused_env["OKX_API_KEY"] = "real-key"
        refused_env["OKX_API_SECRET"] = "secret"
        refused_env["OKX_PASSPHRASE"] = "pass"
        refused = run(["--okx-stop", "--op", "preflight", "--symbol", "BTC", "--inst-type", "swap"], refused_env)
        expect(refused.returncode == 1, "a real key was allowed to use the fixture")
        expect("refusing to call OKX" in refused.stdout, refused.stdout)
        expect(len(fix.requests) == before, "the refused key contacted the fixture")

        env = dict(refused_env)
        env["OKX_API_KEY"] = "offline-fixture"
        pre = run(["--okx-stop", "--op", "preflight", "--symbol", "BTC", "--inst-type", "swap"], env)
        pre_body = load(pre, "preflight")
        expect(pre.returncode == 0 and pre_body.get("ok") is True, pre_body)
        expect(pre_body.get("flat") is True and pre_body.get("pos_mode") == "net_mode", pre_body)
        expect(pre_body.get("mark") == "50000" and pre_body.get("tick") == "0.1", pre_body)

        fix.fail_next.add("privatePostTradeOrderAlgo")
        lost = run([
            "--okx-stop", "--op", "place", "--symbol", "BTC", "--inst-type", "swap",
            "--position-side", "long", "--contracts", "10", "--trigger-px", "49000.0",
            "--algo-client-id", "cid-lost",
        ], env)
        expect(lost.returncode == 1, "a lost place reply was treated as success")
        found = run([
            "--okx-stop", "--op", "read", "--symbol", "BTC", "--inst-type", "swap",
            "--algo-client-id", "cid-lost",
        ], env)
        found_body = load(found, "read after lost place")
        expect(found.returncode == 0 and found_body.get("algo_state") == "live", found_body)
        expect(found_body.get("algo_client_id") == "cid-lost", found_body)
        places = [r for r in fix.requests if r.get("method") == "privatePostTradeOrderAlgo"]
        expect(len(places) == 1, places)
        body = places[0]["params"]
        expect(body.get("ordType") == "conditional", body)
        expect(body.get("slTriggerPx") == "49000.0" and body.get("slOrdPx") == "-1", body)
        expect(body.get("slTriggerPxType") == "mark", body)
        expect(body.get("reduceOnly") is True and body.get("cxlOnClosePos") is True, body)
        expect(body.get("posSide") == "net" and body.get("tdMode") == "cross", body)
        expect(body.get("side") == "sell" and body.get("sz") == "10", body)
        expect(body.get("algoClOrdId") == "cid-lost", body)

        amended = run([
            "--okx-stop", "--op", "amend", "--symbol", "BTC", "--inst-type", "swap",
            "--algo-id", found_body["algo_id"], "--algo-client-id", "cid-lost",
            "--trigger-px", "49500.0", "--contracts", "10",
        ], env)
        expect(amended.returncode == 0, amended.stdout + amended.stderr)
        amend_body = [r for r in fix.requests if r.get("method") == "privatePostTradeAmendAlgos"][-1]["params"]
        expect(amend_body.get("cxlOnFail") is False, amend_body)
        expect(amend_body.get("newSlTriggerPx") == "49500.0" and amend_body.get("newSlOrdPx") == "-1", amend_body)
        expect(amend_body.get("newSlTriggerPxType") == "mark", amend_body)

        canceled = run([
            "--okx-stop", "--op", "cancel", "--symbol", "BTC", "--inst-type", "swap",
            "--algo-id", found_body["algo_id"], "--algo-client-id", "cid-lost",
        ], env)
        expect(canceled.returncode == 0, canceled.stdout + canceled.stderr)
        cancel_body = [r for r in fix.requests if r.get("method") == "privatePostTradeCancelAlgos"][-1]["params"]
        expect(isinstance(cancel_body, list) and cancel_body[0].get("algoClOrdId") == "cid-lost", cancel_body)
        after = load(run([
            "--okx-stop", "--op", "read", "--symbol", "BTC", "--inst-type", "swap",
            "--algo-client-id", "cid-lost",
        ], env), "read after cancel")
        expect(after.get("algo_state") == "canceled", after)

        fix.fills.append({
            "instId": INST, "tradeId": "fill-1", "side": "sell",
            "fillSz": "4", "fillPx": "49000", "fee": "-0.02", "ordId": "ord-1",
        })
        first = load(run(["--okx-stop", "--op", "fills", "--symbol", "BTC", "--inst-type", "swap", "--order-id", "ord-1"], env), "fills")
        second = load(run(["--okx-stop", "--op", "fills", "--symbol", "BTC", "--inst-type", "swap", "--order-id", "ord-1"], env), "fills again")
        expect(first.get("fills") == second.get("fills"), (first, second))
        expect(len(first["fills"]) == 1 and first["fills"][0]["fill_id"] == "fill-1", first)
        expect(first["fills"][0]["qty"] == "4" and first["fills"][0]["px"] == "49000", first)

        sized = load(run(["--okx-stop", "--op", "preflight", "--symbol", "BTC", "--inst-type", "swap",
                          "--contracts", "0.0123456789"], env), "entry size")
        expect(sized.get("ok") and sized.get("contracts") == "0.01", sized)
        print("BTC swap entry contract rounding: 0.0123456789 -> 0.01")
        fix.positions.append({
            "instId": INST, "pos": "10", "posSide": "net", "mgnMode": "cross", "liqPx": "40000",
        })
        armed = load(run(["--okx-stop", "--op", "arm-reference", "--symbol", "BTC", "--inst-type", "swap"], env), "arm")
        expect(armed.get("ok") is True and armed.get("flat") is False, armed)
        expect(armed.get("pos_side") == "long" and armed.get("pos_contracts") == "10", armed)
        expect(armed.get("liq_known") is True and armed.get("liq_px") == "40000", armed)
        expect(armed.get("fee") == "0.0005" and armed.get("equity") == "1000", armed)

        stopped = finalize_stop({
            "side": "long", "owner": "stop_loss_pct", "entry": "50000", "mark": "50000",
            "pct": "2", "tick": "0.1", "equity": "1000", "contracts": "10", "ct_val": "0.01",
            "mmr": "0.005", "fee": "0.0005", "require_venue_liq": False,
        })
        expect(stopped["trigger"] == "49000.0" and stopped["moved"] is True, stopped)
        held = finalize_stop({
            "side": "long", "owner": "trailing_stop_pct", "entry": "50000", "mark": "50100",
            "pct": "2", "tick": "0.1", "high_water": "50000", "previous_trigger": "49000.0",
            "min_move_pct": "0.5", "equity": "1000", "contracts": "10", "ct_val": "0.01",
            "mmr": "0.005", "fee": "0.0005", "require_venue_liq": False,
        })
        expect(held["trigger"] == "49000.0" and held["moved"] is False, held)
        for side, previous, bound, trigger, moved in (
            ("long", "95.5", "90", "95.5", False),
            ("long", "95.5", "97", "97.5", True),
            ("short", "104.4", "110", "104.4", False),
        ):
            fixed = finalize_stop({"side": side, "owner": "stop_loss_pct", "entry": "100", "mark": "100",
                                   "pct": "20", "tick": "0.1", "previous_trigger": previous,
                                   "venue_liq": bound, "venue_liq_known": True, "require_venue_liq": True})
            expect(fixed["trigger"] == trigger and fixed["moved"] is moved, fixed)
            print(f"fixed {side}: previous={previous} bound={bound} trigger={trigger} moved={moved}")
        print("okx stop offline acceptance: place=%d reads-found-lost-reply=1 cancel=canceled fill=fill-1 trigger=49000.0" % fix.place_count)
    finally:
        httpd.shutdown()


if __name__ == "__main__":
    main()
