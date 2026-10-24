//go:build pyintegration

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const pairQuotePyDriver = `import base64, json, os, sys
root = os.environ["PAIR_PY_ROOT"]
sys.path.insert(0, os.path.join(root, "shared_tools"))
import pair_evidence as pe
import pair_inputs as pi
req = json.load(sys.stdin)
out = []
for case in req["cases"]:
    try:
        if case["kind"] == "verify":
            env = pi.verify_envelope(pe.loads_strict(base64.b64decode(case["request"])))
            out.append({"ok": True, "fp": pe.sha256_hex(pe.canonical_bytes(env)), "env": env})
        elif case["kind"] == "crosses":
            env = json.loads(base64.b64decode(case["envelope"]))
            out.append({"ok": True, "value": pi.bound_crosses(env, case["member"], case["side"], case["bound"])})
        else:
            out.append({"ok": False, "error": "unknown case"})
    except Exception as exc:
        out.append({"ok": False, "error": type(exc).__name__ + ": " + str(exc)})
json.dump(out, sys.stdout, default=lambda o: getattr(o, "text", str(o)))
`

type pairQuotePyResult struct {
	OK    bool            `json:"ok"`
	FP    string          `json:"fp"`
	Value bool            `json:"value"`
	Error string          `json:"error"`
	Env   json.RawMessage `json:"env"`
}

func pairRunQuotePy(t *testing.T, cases []map[string]interface{}) []pairQuotePyResult {
	t.Helper()
	root := basketPyRoot(t)
	driver := filepath.Join(t.TempDir(), "pair_quote_driver.py")
	if err := os.WriteFile(driver, []byte(pairQuotePyDriver), 0o600); err != nil {
		t.Fatal(err)
	}
	req, err := json.Marshal(map[string]interface{}{"cases": cases})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(filepath.Join(root, ".venv", "bin", "python3"), "-I", driver)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "PAIR_PY_ROOT="+root, "PYTHONDONTWRITEBYTECODE=1")
	cmd.Stdin = bytes.NewReader(req)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("python driver: %v\n%s", err, stderr.String())
	}
	var res []pairQuotePyResult
	if err := json.Unmarshal(out, &res); err != nil || len(res) != len(cases) {
		t.Fatalf("python driver output %s (%v)", out, err)
	}
	return res
}

func pairRequestBytes(t *testing.T, req pairPolicyRequest, envelope []byte) []byte {
	t.Helper()
	req.Envelope = json.RawMessage("null")
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	out := bytes.Replace(b, []byte(`"envelope":null`), append(append([]byte(`"envelope":`), envelope...), []byte{}...), 1)
	if bytes.Equal(out, b) {
		t.Fatal("the request template has no envelope placeholder")
	}
	return out
}

func pairHex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func TestBasketPairQuoteGoPythonParity(t *testing.T) {
	variants := map[string]pairMutation{
		"sealed":  nil,
		"public":  pairSetMember(0, "source", "public_snapshot"),
		"archive": pairAll(pairSetMember(0, "source", "archive"), pairSetMember(0, "modeled_delivery_ms", 1000)),
	}
	for name, mutate := range variants {
		t.Run(name, func(t *testing.T) {
			e := newPairTestEnv(t, mutate)
			at := pairTestStart()
			rb := e.slot(at, "in-0")
			e.step(at.Add(time.Second))
			if len(e.runner.requests) != 1 {
				t.Fatalf("the policy must receive one real request, got %d", len(e.runner.requests))
			}
			req := e.runner.requests[0]
			if req.V != 2 || req.BookNeed != e.rt().Contract.Pair.BookNeed || len(req.Members) != 2 || req.Members[0].SourceContract == "" || req.Members[1].Leverage != 2 {
				t.Fatalf("the version 2 request must carry the verifier inputs: %+v", req)
			}
			exp := pairEnvelopeExpectFor(e.rt())
			cases := []map[string]interface{}{{"kind": "verify", "request": base64.StdEncoding.EncodeToString(pairRequestBytes(t, req, []byte(rb.EvidenceJSON)))}}
			wantOK := []bool{true}
			for tname, edit := range pairEnvelopeTampers(t) {
				raw := pairReencode(t, rb.EvidenceJSON, edit)
				tampered := req
				tampered.InputFingerprint = pairHex([]byte(raw))
				if _, err := pairCheckEnvelope(exp, []byte(raw)); err == nil {
					t.Fatalf("%s: Go accepted the tampered envelope", tname)
				}
				cases = append(cases, map[string]interface{}{"kind": "verify", "request": base64.StdEncoding.EncodeToString(pairRequestBytes(t, tampered, []byte(raw))), "name": tname})
				wantOK = append(wantOK, false)
			}
			for _, shift := range []int64{0, 1} {
				raw := pairReencode(t, rb.EvidenceJSON, func(env *pairQuoteEnvelope) {
					oldest := env.CutoffMs
					for _, m := range env.Members {
						if m.Observation.FirstReceiptMs < oldest {
							oldest = m.Observation.FirstReceiptMs
						}
					}
					env.CutoffMs = oldest + 5000 + shift
					env.StartMs = env.CutoffMs - 1
					env.DeadlineMs = env.StartMs + 2000
				})
				_, goErr := pairCheckEnvelope(exp, []byte(raw))
				var env pairQuoteEnvelope
				_ = json.Unmarshal([]byte(raw), &env)
				boundary := req
				boundary.CutoffMs, boundary.InputFingerprint = env.CutoffMs, pairHex([]byte(raw))
				cases = append(cases, map[string]interface{}{"kind": "verify", "request": base64.StdEncoding.EncodeToString(pairRequestBytes(t, boundary, []byte(raw)))})
				wantOK = append(wantOK, goErr == nil)
				if (goErr == nil) != (shift == 0) {
					t.Fatalf("Go age boundary at +%d: %v", shift, goErr)
				}
			}
			wrongFP := req
			wrongFP.InputFingerprint = strings.Repeat("0", 64)
			cases = append(cases, map[string]interface{}{"kind": "verify", "request": base64.StdEncoding.EncodeToString(pairRequestBytes(t, wrongFP, []byte(rb.EvidenceJSON)))})
			wantOK = append(wantOK, false)
			wrongSource := req
			wrongSource.Members = append([]pairPolicyMember(nil), req.Members...)
			wrongSource.Members[0].SourceContract = "okx_public_books_snapshot_v1"
			if wrongSource.Members[0].Provider == entryProviderOKX {
				wrongSource.Members[0].SourceContract = pairContractHLSealed
			}
			cases = append(cases, map[string]interface{}{"kind": "verify", "request": base64.StdEncoding.EncodeToString(pairRequestBytes(t, wrongSource, []byte(rb.EvidenceJSON)))})
			wantOK = append(wantOK, false)
			lev := req
			lev.Members = append([]pairPolicyMember(nil), req.Members...)
			lev.Members[1].Leverage = 3
			cases = append(cases, map[string]interface{}{"kind": "verify", "request": base64.StdEncoding.EncodeToString(pairRequestBytes(t, lev, []byte(rb.EvidenceJSON)))})
			wantOK = append(wantOK, false)
			envBytes := base64.StdEncoding.EncodeToString([]byte(rb.EvidenceJSON))
			env, why := verifyPairEnvelope(e.rt(), rb)
			if why != "" {
				t.Fatal(why)
			}
			type cross struct {
				member, side string
				bound        float64
			}
			crosses := []cross{{pairTestOKX, "buy", 60100.1}, {pairTestOKX, "buy", 60100.05}, {pairTestOKX, "sell", 60099.9}, {pairTestOKX, "sell", 60099.95}, {pairTestHL, "buy", 60001}, {pairTestHL, "sell", 60000}}
			for _, c := range crosses {
				cases = append(cases, map[string]interface{}{"kind": "crosses", "envelope": envBytes, "member": c.member, "side": c.side, "bound": c.bound})
			}
			res := pairRunQuotePy(t, cases)
			for i, ok := range wantOK {
				if res[i].OK != ok {
					t.Fatalf("case %d (%v): Python ok=%v (%s), Go ok=%v; the languages must agree", i, cases[i]["name"], res[i].OK, res[i].Error, ok)
				}
			}
			if res[0].FP != rb.InputFP {
				t.Fatalf("Python re-canonicalized fingerprint %s, Go %s", res[0].FP, rb.InputFP)
			}
			for j, c := range crosses {
				r := res[len(wantOK)+j]
				if !r.OK || r.Value != pairBoundCrosses(env, c.member, c.side, c.bound) {
					t.Fatalf("crossing %+v: Python %+v, Go %v", c, r, pairBoundCrosses(env, c.member, c.side, c.bound))
				}
			}
		})
	}
}

const pairQuoteFakeDriver = `import atexit, builtins, json, os, runpy, socket, sys, tempfile, time, importlib.util
root = os.environ["PAIR_FAKE_ROOT"]
state_path = os.environ["PAIR_FAKE_STATE"]
mode = os.environ.get("PAIR_FAKE_MODE", "normal")
state = {"violations": [], "env": {}, "calls": []}
for k in ("HYPERLIQUID_SECRET_KEY", "HYPERLIQUID_ACCOUNT_ADDRESS", "OKX_API_KEY", "OKX_API_SECRET", "OKX_PASSPHRASE", "HYPERLIQUID_TESTNET", "OKX_SANDBOX"):
    state["env"][k] = "set" if os.environ.get(k) else "empty"
real_open = builtins.open
def save():
    with real_open(state_path, "w") as fh:
        json.dump(state, fh)
atexit.register(save)
def violation(what):
    state["violations"].append(what)
    raise RuntimeError("forbidden: " + what)
def guarded_open(file, mode="r", *a, **k):
    m = mode if isinstance(mode, str) else "r"
    if any(c in m for c in "wax+"):
        violation("write " + str(file))
    if str(file).endswith("hl_meta.json"):
        violation("cache read " + str(file))
    return real_open(file, mode, *a, **k)
builtins.open = guarded_open
tempfile.mkstemp = lambda *a, **k: violation("mkstemp")
socket.socket.connect = lambda self, *a, **k: violation("socket connect")
if mode == "huge":
    sys.stdout.write("x" * 3000000)
    sys.stdout.flush()
    time.sleep(30)
if mode == "huge_err":
    sys.stderr.write("y" * 3000000)
    sys.stderr.flush()
    time.sleep(30)
if mode == "probe":
    real_spec = importlib.util.spec_from_file_location
    def spec(name, path, *a, **k):
        if "adapter" in str(path) or "pair_archive" in str(path):
            violation("load " + str(path))
        return real_spec(name, path, *a, **k)
    importlib.util.spec_from_file_location = spec
else:
    universe = [{"name": "ETH", "szDecimals": 4, "maxLeverage": 25}]
    if mode != "unknown":
        universe.append({"name": "BTC", "szDecimals": 3, "maxLeverage": 20})
    meta = {"universe": universe}
    spot = {"tokens": [{"name": "USDC", "szDecimals": 8, "weiDecimals": 8, "index": 0, "tokenId": "0x0", "isCanonical": True},
                       {"name": "PURR", "szDecimals": 0, "weiDecimals": 5, "index": 1, "tokenId": "0x1", "isCanonical": True}],
            "universe": [{"tokens": [1, 0], "name": "PURR/USDC", "index": 0, "isCanonical": True}]}
    import eth_account
    eth_account.Account.from_key = staticmethod(lambda *a, **k: violation("wallet construction"))
    import hyperliquid.exchange
    hyperliquid.exchange.Exchange.__init__ = lambda self, *a, **k: violation("signing client")
    import hyperliquid.api
    def post(self, path, payload=None):
        t = (payload or {}).get("type")
        state["calls"].append("hl:" + str(t))
        if path == "/info" and t == "meta":
            return meta
        if path == "/info" and t == "spotMeta":
            return spot
        if path == "/info" and t == "l2Book" and payload.get("coin") == "BTC":
            if mode == "sleep":
                time.sleep(30)
            now = int(time.time() * 1000)
            return {"coin": "BTC", "time": now - 30, "levels": [
                [{"px": "%d.0" % (60000 - i), "sz": "0.5", "n": 2} for i in range(20)],
                [{"px": "%d.0" % (60002 + i), "sz": "0.5", "n": 2} for i in range(20)]]}
        violation("hyperliquid call " + path + " " + str(t))
    hyperliquid.api.API.post = post
    import ccxt
    def fetch(self, url, method="GET", headers=None, body=None):
        state["calls"].append("okx:" + url.split("?")[0])
        if "/api/v5/market/books" in url and "instId=BTC-USDT-SWAP" in url and mode != "unknown":
            now = int(time.time() * 1000)
            sz = int(url.split("sz=")[1].split("&")[0])
            return {"code": "0", "msg": "", "data": [{"asks": [["%.1f" % (60100.1 + i / 10), "3", "0", "2"] for i in range(sz)],
                                                      "bids": [["%.1f" % (60099.9 - i / 10), "3", "0", "2"] for i in range(sz)], "ts": str(now - 20)}]}
        if "/api/v5/market/books" in url:
            return {"code": "51001", "msg": "Instrument ID does not exist", "data": []}
        violation("okx call " + url)
    ccxt.okx.fetch = fetch
    real_sign = ccxt.okx.sign
    def sign(self, path, api="public", method="GET", params={}, headers=None, body=None):
        if "private" in str(api):
            violation("okx private sign " + str(path))
        return real_sign(self, path, api, method, params, headers, body)
    ccxt.okx.sign = sign
    real_load = ccxt.okx.load_markets
    ccxt.okx.load_markets = lambda self, *a, **k: violation("okx market load")
script = os.path.join(root, "shared_scripts", "fetch_pair_quotes.py")
sys.argv = [script] + sys.argv[1:]
runpy.run_path(script, run_name="__main__")
`

type pairQuoteFixture struct {
	t     *testing.T
	root  string
	state string
}

func newPairQuoteFixture(t *testing.T) *pairQuoteFixture {
	t.Helper()
	root := basketPyRoot(t)
	dir := t.TempDir()
	driver := filepath.Join(dir, "quote_driver.py")
	if err := os.WriteFile(driver, []byte(pairQuoteFakeDriver), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &pairQuoteFixture{t: t, root: root, state: filepath.Join(dir, "state.json")}
	t.Setenv("PAIR_FAKE_ROOT", root)
	t.Setenv("PAIR_FAKE_STATE", f.state)
	for _, k := range pairQuoteReaderClearedEnv {
		t.Setenv(k, "sentinel-secret-1871")
	}
	t.Chdir(root)
	prev := pairQuoteReaderScript
	pairQuoteReaderScript = driver
	t.Cleanup(func() { pairQuoteReaderScript = prev })
	prevEgress := processEgress
	processEgress = &egressRuntime{}
	t.Cleanup(func() { processEgress = prevEgress })
	t.Setenv("GO_TRADER_EGRESS_PROXY", "")
	return f
}

type pairQuoteState struct {
	Violations []string          `json:"violations"`
	Env        map[string]string `json:"env"`
	Calls      []string          `json:"calls"`
}

func (f *pairQuoteFixture) read(mode string) pairQuoteState {
	f.t.Helper()
	b, err := os.ReadFile(f.state)
	if err != nil {
		return pairQuoteState{}
	}
	if bytes.Contains(b, []byte("sentinel-secret-1871")) {
		f.t.Fatalf("%s: the reader saw or logged a credential value", mode)
	}
	var s pairQuoteState
	if err := json.Unmarshal(b, &s); err != nil {
		f.t.Fatal(err)
	}
	_ = os.Remove(f.state)
	return s
}

func TestBasketPairQuoteReaderIsPublicOnly(t *testing.T) {
	_, _, contract := pairAdmitAccepted(t, pairSetMember(0, "source", "public_snapshot"))
	need := contract.Pair.BookNeed
	f := newPairQuoteFixture(t)
	read := func(mode string, m pairContractMember, timeout time.Duration) (pairObservation, error, pairQuoteState) {
		t.Setenv("PAIR_FAKE_MODE", mode)
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		o, err := pairPublicReader{}.readQuote(ctx, pairMemberRead{Partition: paperPartition(), Member: m, BookNeed: need,
			SourceContract: pairSourceContractFor(m.Provider, m.Source), StartMs: time.Now().UnixMilli(), OutputLimit: 1 << 20})
		return o, err, f.read(mode)
	}
	for _, id := range []string{pairTestHL, pairTestOKX} {
		m, _ := contract.pairMember(id)
		o, err, st := read("normal", m, 30*time.Second)
		if err != nil {
			t.Fatalf("%s public read: %v", id, err)
		}
		if len(st.Violations) > 0 {
			t.Fatalf("%s public read touched a forbidden path: %v", id, st.Violations)
		}
		for k, v := range st.Env {
			if v != "empty" {
				t.Fatalf("%s public read received %s", id, k)
			}
		}
		for _, call := range st.Calls {
			if call != "hl:meta" && call != "hl:spotMeta" && call != "hl:l2Book" && call != "okx:https://www.okx.com/api/v5/market/books" {
				t.Fatalf("%s public read made call %q", id, call)
			}
		}
		if len(o.NativeBids) != need.Depth || o.Coverage.Status != pairCoverageComplete || o.Coverage.FromMs != o.FirstReceiptMs || o.VenueTimeMs == nil {
			t.Fatalf("%s public observation is wrong: %+v", id, o)
		}
		if _, err := pairCheckObservationSource(o, m, o.Key, need); err != nil {
			t.Fatalf("%s public observation must verify from its payload: %v", id, err)
		}
		if id == pairTestHL && o.NativeBids[0].Px != "60000" {
			t.Fatalf("raw decimal text must normalize exactly, got %q", o.NativeBids[0].Px)
		}
		if id == pairTestOKX && (o.NativeAsks[0].Qty != "3" || !strings.Contains(string(o.Payload), `"sz":"5"`)) {
			t.Fatalf("OKX quantities stay native contracts with the requested depth bound: %s", o.Payload)
		}
	}
	for _, id := range []string{pairTestHL, pairTestOKX} {
		m, _ := contract.pairMember(id)
		if _, err, st := read("unknown", m, 30*time.Second); err == nil || len(st.Violations) > 0 {
			t.Fatalf("%s unknown instrument must be refused without a fallback: %v %v", id, err, st.Violations)
		}
	}
	m, _ := contract.pairMember(pairTestHL)
	var overflow *pythonOutputOverflowError
	if _, err, _ := read("huge", m, 30*time.Second); !errors.As(err, &overflow) || overflow.Stream != "stdout" {
		t.Fatalf("oversized stdout must cancel the reader with an overflow failure, got %v", err)
	}
	if _, err, _ := read("huge_err", m, 30*time.Second); !errors.As(err, &overflow) || overflow.Stream != "stderr" {
		t.Fatalf("oversized stderr must be a child failure, got %v", err)
	}
	start := time.Now()
	if _, err, _ := read("sleep", m, 3*time.Second); err == nil || time.Since(start) > 3*time.Second+pythonPipeWaitDelay+2*time.Second {
		t.Fatalf("the caller deadline must stop a slow read within the cleanup bound, got %v after %s", err, time.Since(start))
	}
	t.Setenv("PAIR_FAKE_MODE", "normal")
	if _, err := (pairPublicReader{}).readQuote(context.Background(), pairMemberRead{Member: m, BookNeed: need, SourceContract: pairContractHLPublic}); err == nil {
		t.Fatal("a public read without a caller deadline must be refused")
	}
	cmd := exec.Command(filepath.Join(f.root, ".venv", "bin", "python3"), "shared_scripts/fetch_pair_quotes.py", "--provider=okx", "--source=public_snapshot", "--instrument=BTC-USDT-SWAP", "--depth=5")
	cmd.Env = append(os.Environ(), "OKX_SANDBOX=1")
	out, err := cmd.Output()
	if err == nil || !strings.Contains(string(out), "OKX_SANDBOX") || strings.Contains(string(out), "sentinel") {
		t.Fatalf("the reader must refuse nonempty venue variables itself, got %v %s", err, out)
	}
}

func TestBasketPairQuoteCollectionThroughRealReaders(t *testing.T) {
	e := newPairTestEnv(t, pairAll(pairSetMember(0, "source", "public_snapshot"), pairSetLimit("quote_age_ms", 30000), pairSetLimit("collection_timeout_ms", 20000), pairSetLimit("receive_skew_ms", 20000)))
	f := newPairQuoteFixture(t)
	t.Setenv("PAIR_FAKE_MODE", "normal")
	e.adm.deps.Quotes = &pairQuoteCollector{sources: map[string]string{pairTestHL: pairSourcePublicSnapshot, pairTestOKX: pairSourcePublicSnapshot},
		readers: map[string]pairMemberReader{pairTestHL: pairPublicReader{}, pairTestOKX: pairPublicReader{}}, limit: 1 << 20}
	e.bc.pairClock = time.Now
	now := time.Now()
	e.step(now)
	st := f.read("normal")
	if len(st.Violations) > 0 {
		t.Fatalf("collection touched a forbidden path: %v", st.Violations)
	}
	slot := now.UnixMilli() - now.UnixMilli()%60000
	status, hold, input, evidence := e.slotRow(slot)
	if status == basketRebalanceHeld && strings.Contains(hold, "fails verification") || evidence == "" || input != pairHex([]byte(evidence)) {
		t.Fatalf("the real readers must freeze a verified envelope, got %s %q", status, hold)
	}
	if _, why := verifyPairEnvelope(e.rt(), e.rt().Rebalances[slot]); why != "" {
		t.Fatalf("the frozen envelope must verify: %s", why)
	}
}

func TestBasketPairQuoteProbeArgvsParseOffline(t *testing.T) {
	f := newPairQuoteFixture(t)
	t.Setenv("PAIR_FAKE_MODE", "probe")
	for _, argv := range [][]string{pairQuoteHLPublicProbeArgv, pairQuoteOKXPublicProbeArgv, pairQuoteHLArchiveProbeArgv} {
		if err := probeOneCheckScript(pairQuoteReaderScript, argv); err != nil {
			t.Fatalf("the guarded probe failed: %v", err)
		}
		st := f.read("probe")
		if len(st.Violations) > 0 || len(st.Calls) > 0 {
			t.Fatalf("probe-only constructed an adapter or reached the network: %+v", st)
		}
	}
	pairQuoteReaderScript = "shared_scripts/fetch_pair_quotes.py"
	for _, argv := range [][]string{pairQuoteHLPublicProbeArgv, pairQuoteOKXPublicProbeArgv, pairQuoteHLArchiveProbeArgv} {
		if err := probeOneCheckScript(pairQuoteReaderScript, argv); err != nil {
			t.Fatalf("the real reader rejected its probe argv: %v", err)
		}
	}
	for _, bad := range [][]string{
		{"--provider=binance", "--source=public_snapshot", "--instrument=BTC", "--depth=5", "--probe-only"},
		{"--provider=okx", "--source=archive", "--instrument=BTC-USDT-SWAP", "--depth=5", "--probe-only"},
		{"--provider=hyperliquid", "--source=public_snapshot", "--instrument=BTC", "--depth=21", "--probe-only"},
		{"--provider=hyperliquid", "--source=archive", "--instrument=BTC", "--depth=5", "--probe-only"},
		{"--provider=hyperliquid", "--source=public_snapshot", "--instrument=BTC", "--depth=5", "--capacity=5", "--probe-only"},
	} {
		if err := probeOneCheckScript(pairQuoteReaderScript, bad); err == nil {
			t.Fatalf("the reader must reject %v even in probe mode", bad)
		}
	}
	if err := probeOneCheckScript("shared_scripts/no_such_pair_quote_reader.py", pairQuoteHLPublicProbeArgv); err == nil {
		t.Fatal("a missing quote reader must fail the probe")
	}
}

const pairArchivePrepDriver = `import hashlib, json, os, shutil, sys
root = os.environ["PAIR_PY_ROOT"]
sys.path.insert(0, os.path.join(root, "backtest"))
import book_archive as ba
import pair_archive_book as pab
raw, out = sys.argv[1], sys.argv[2]
fixture = os.path.join(root, "backtest", "tests", "testdata", "obi_archive_hour.lz4")
key = ba.book_key("20260801", 0, "BTC")
os.makedirs(os.path.dirname(os.path.join(raw, key)), exist_ok=True)
shutil.copyfile(fixture, os.path.join(raw, key))
blob = open(fixture, "rb").read()
manifest = {"files": [{"key": key, "status": "present", "size": len(blob), "sha256": hashlib.sha256(blob).hexdigest()}]}
pab.prepare_archive(raw, out, ["BTC"], ["20260801"], manifest)
print(json.dumps({"ok": True}))
`

func pairPrepareArchive(t *testing.T) string {
	t.Helper()
	root := basketPyRoot(t)
	dir := t.TempDir()
	driver := filepath.Join(dir, "prep.py")
	if err := os.WriteFile(driver, []byte(pairArchivePrepDriver), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "import")
	cmd := exec.Command(filepath.Join(root, ".venv", "bin", "python3"), "-I", driver, filepath.Join(dir, "raw"), out)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "PAIR_PY_ROOT="+root, "PYTHONDONTWRITEBYTECODE=1")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("archive preparation: %v\n%s", err, b)
	}
	return out
}

func TestBasketPairQuoteArchiveAppliesDelayOnce(t *testing.T) {
	_, _, contract := pairAdmitAccepted(t, pairAll(pairSetMember(0, "source", "archive"), pairSetMember(0, "modeled_delivery_ms", 1000)))
	m, _ := contract.pairMember(pairTestHL)
	need := contract.Pair.BookNeed
	dir := pairPrepareArchive(t)
	root := basketPyRoot(t)
	t.Chdir(root)
	const delay = int64(1000)
	t0 := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	recv := func(i int) int64 { return t0 + 1000 + 5400*int64(i) }
	read := func(sourceCutoff int64) (pairObservation, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return pairArchiveReader{dir: dir}.readQuote(ctx, pairMemberRead{Partition: paperPartition(), Member: m, BookNeed: need,
			SourceContract: pairContractHLArchive, StartMs: sourceCutoff + delay, OutputLimit: 1 << 20})
	}
	at, err := read(recv(20))
	if err != nil {
		t.Fatalf("archive read at an observation receipt: %v", err)
	}
	if at.FirstReceiptMs != recv(19)+delay || at.SourceCutoffMs != recv(20) {
		t.Fatalf("the strict as-of boundary must exclude the observation received at the translated cutoff: receipt %d source cutoff %d", at.FirstReceiptMs, at.SourceCutoffMs)
	}
	after, err := read(recv(20) + 1)
	if err != nil {
		t.Fatal(err)
	}
	if after.FirstReceiptMs != recv(20)+delay || after.Coverage.ThroughMs != recv(20)+1+delay || after.Coverage.Status != pairCoverageComplete {
		t.Fatalf("the modeled delivery must be added once to receipt and coverage: %+v", after.Coverage)
	}
	var section map[string]interface{}
	dec := json.NewDecoder(bytes.NewReader(after.Payload))
	dec.UseNumber()
	if err := dec.Decode(&section); err != nil {
		t.Fatal(err)
	}
	shift := func(v interface{}) int64 { n, _ := v.(json.Number).Int64(); return n + delay }
	section["time_semantics"], section["snapshot_id"], section["owner_instance"] = feedBookTimeSemantics, "snap-equivalent", "feed-equivalent"
	section["cutoff_ms"], section["retained_from_ms"] = shift(section["cutoff_ms"]), shift(section["retained_from_ms"])
	for _, o := range section["observations"].([]interface{}) {
		o.(map[string]interface{})["recv_ms"] = shift(o.(map[string]interface{})["recv_ms"])
	}
	for _, mk := range section["markers"].([]interface{}) {
		mk.(map[string]interface{})["at_ms"] = shift(mk.(map[string]interface{})["at_ms"])
	}
	sealedPayload := pairFixtureCanonical(section)
	sealedMember := m
	sealedMember.Source, sealedMember.ModeledDeliveryMs = pairSourceSealedFeed, 0
	sealed, err := pairObservationFromPayload(sealedMember, "paper", need, sealedPayload, 0, 3)
	if err != nil {
		t.Fatalf("the equivalent sealed section: %v", err)
	}
	if sealed.FirstReceiptMs != after.FirstReceiptMs || *sealed.VenueTimeMs != *after.VenueTimeMs || sealed.Coverage.FromMs != after.Coverage.FromMs ||
		sealed.Coverage.ThroughMs != after.Coverage.ThroughMs || len(sealed.Coverage.Gaps) != len(after.Coverage.Gaps) ||
		strings.Join(pairLevelText(sealed.NativeBids), ",") != strings.Join(pairLevelText(after.NativeBids), ",") {
		t.Fatalf("sealed and archive observations of equivalent events must agree:\nsealed  %+v\narchive %+v", sealed, after)
	}
	if sealed.Provenance == after.Provenance || sealed.Coverage.SourceContract == after.Coverage.SourceContract {
		t.Fatal("sealed and archive observations must keep distinct provenance")
	}
	if _, err := read(t0 + 3600000 + 30000); err == nil {
		t.Fatal("a missing archive hour must hold instead of reading stale or substituted data")
	}
	e := newPairTestEnv(t, pairAll(pairSetMember(0, "source", "archive"), pairSetMember(0, "modeled_delivery_ms", 1000)))
	e.quotes.enable()
	e.adm.deps.Quotes = &pairQuoteCollector{sources: map[string]string{pairTestHL: pairSourceArchive, pairTestOKX: pairSourcePublicSnapshot},
		readers: map[string]pairMemberReader{pairTestHL: pairArchiveReader{dir: dir}, pairTestOKX: e.quotes}, limit: 1 << 20}
	start := time.UnixMilli(recv(20) + 1 + delay).UTC()
	e.step(start)
	slot := start.UnixMilli() - start.UnixMilli()%60000
	status, hold, _, evidence := e.slotRow(slot)
	if evidence == "" {
		t.Fatalf("a historical logical clock must collect and freeze an archive envelope under a duration budget, got %s %q", status, hold)
	}
	if _, why := verifyPairEnvelope(e.rt(), e.rt().Rebalances[slot]); why != "" {
		t.Fatalf("the archive envelope must verify: %s", why)
	}
}

func pairLevelText(levels []pairNativeLevel) []string {
	var out []string
	for _, l := range levels {
		out = append(out, l.Px+"x"+l.Qty)
	}
	return out
}

func paperPartition() RiskPartition {
	p, _ := parseRiskPartition("paper")
	return p
}
