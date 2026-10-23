//go:build pyintegration

package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const pairEvidencePyDriver = `import base64, importlib.util, json, math, os, sys
root = os.environ["PAIR_PY_ROOT"]
spec = importlib.util.spec_from_file_location("pair_evidence_under_test", os.path.join(root, "shared_tools", "pair_evidence.py"))
pe = importlib.util.module_from_spec(spec)
spec.loader.exec_module(pe)
req = json.load(sys.stdin)
out = []
for case in req["cases"]:
    try:
        kind = case["kind"]
        if kind == "canonical":
            data = pe.canonical_bytes(pe.loads_strict(base64.b64decode(case["input"])))
            out.append({"ok": True, "hex": data.hex()})
        elif kind == "record":
            sealed = pe.seal_record(case["record"])
            out.append({"ok": True, "hex": pe.canonical_bytes(sealed).hex(), "hash": sealed["evidence_hash"]})
        elif kind == "document":
            data = pe.document_bytes(case["doc"])
            out.append({"ok": True, "hex": data.hex(), "hash": pe.sha256_hex(data)})
        elif kind == "decode_document":
            data = base64.b64decode(case["input"])
            pe.decode_document(data, pe.sha256_hex(data))
            out.append({"ok": True})
        elif kind == "round":
            value = pe.round_bound(case["member"], case["capability"], case["side"], case["bound"])
            out.append({"ok": True, "value": repr(value)})
        else:
            out.append({"ok": False, "error": "unknown case " + kind})
    except Exception as exc:
        out.append({"ok": False, "error": type(exc).__name__ + ": " + str(exc)})
json.dump(out, sys.stdout)
`

type pairPyResult struct {
	OK    bool   `json:"ok"`
	Hex   string `json:"hex"`
	Hash  string `json:"hash"`
	Value string `json:"value"`
	Error string `json:"error"`
}

func pairRunPyCases(t *testing.T, cases []map[string]interface{}) []pairPyResult {
	t.Helper()
	root := basketPyRoot(t)
	driver := filepath.Join(t.TempDir(), "pair_evidence_driver.py")
	if err := os.WriteFile(driver, []byte(pairEvidencePyDriver), 0o600); err != nil {
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
	var res []pairPyResult
	if err := json.Unmarshal(out, &res); err != nil || len(res) != len(cases) {
		t.Fatalf("python driver output %s (%v)", out, err)
	}
	return res
}

func pairLooseFill(t *testing.T, v reflect.Value, raw interface{}) {
	t.Helper()
	switch v.Kind() {
	case reflect.Struct:
		obj := raw.(map[string]interface{})
		for i := 0; i < v.NumField(); i++ {
			f := v.Type().Field(i)
			if f.Anonymous {
				pairLooseFill(t, v.Field(i), raw)
				continue
			}
			name := strings.SplitN(f.Tag.Get("json"), ",", 2)[0]
			if val, ok := obj[name]; ok {
				pairLooseFill(t, v.Field(i), val)
			}
		}
	case reflect.String:
		v.SetString(raw.(string))
	case reflect.Bool:
		v.SetBool(raw.(bool))
	case reflect.Int, reflect.Int64:
		n, err := raw.(json.Number).Int64()
		if err != nil {
			t.Fatal(err)
		}
		v.SetInt(n)
	case reflect.Uint64:
		var u uint64
		if _, err := fmt.Sscan(string(raw.(json.Number)), &u); err != nil {
			t.Fatal(err)
		}
		v.SetUint(u)
	case reflect.Slice:
		arr := raw.([]interface{})
		s := reflect.MakeSlice(v.Type(), len(arr), len(arr))
		for i, e := range arr {
			pairLooseFill(t, s.Index(i), e)
		}
		v.Set(s)
	default:
		t.Fatalf("unsupported loose kind %s", v.Kind())
	}
}

func pairLooseDecode(t *testing.T, text string, out interface{}) map[string]interface{} {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	var raw map[string]interface{}
	if err := dec.Decode(&raw); err != nil {
		t.Fatal(err)
	}
	pairLooseFill(t, reflect.ValueOf(out).Elem(), raw)
	return raw
}

const pairRawKey = `{"partition":"paper","provider":"okx","account_key":"synthetic:okx-research","instrument":"BTC-USDT-SWAP","member_id":"pair-okx-btc"}`

func TestBasketPairEvidenceCodecMatchesPython(t *testing.T) {
	canonicalInputs := []string{
		`{"b":1,"a":{"d":[3,2,1],"c":"x"},"A":true,"_":null,"é":0,"Z":false}`,
		`{"html":"<a href=\"x\">&amp;</a>","nonascii":"café 中文 😀","seps":"a\u2028b\u2029c","rawseps":"a b c","ctl":"\u0001\b\f\n\r\t\u001f\u007f","esc":"\/\\"}`,
		`{"big":9007199254740993,"neg":-9223372036854775808,"u":18446744073709551615,"z":-0,"huge":123456789012345678901234567890}`,
		`[ {"k" : [ ] , "j" : { } } , "x" ]`,
		`{"float":1.5}`,
		`{"exp":1e3}`,
	}
	var cases []map[string]interface{}
	type goOut struct {
		hex, hash string
		err       error
	}
	var want []goOut
	for _, in := range canonicalInputs {
		cases = append(cases, map[string]interface{}{"kind": "canonical", "input": []byte(in)})
		tree, err := pairStrictParse([]byte(in))
		var buf bytes.Buffer
		if err == nil {
			err = pairWriteCanonical(&buf, tree)
		}
		want = append(want, goOut{hex: hex.EncodeToString(buf.Bytes()), err: err})
	}
	stamp := `"v":2,"key":` + pairRawKey + `,"evidence_id":"rec-1","evidence_hash":"stale","observed_ms":9007199254740993,"first_receipt_ms":9007199254740994,"valid_from_ms":9007199254740994,"valid_through_ms":9223372036854775807,"provenance":"src <a>&b   é \"q\""`
	records := []struct {
		text string
		into interface{}
	}{
		{`{` + stamp + `,"currency":"USDT","rate_usd":"1.50"}`, &pairConversionEvidence{}},
		{`{` + stamp + `,"position_mode":"net","margin_mode":"cross","leverage":"2.0","collateral_currency":"USDT","equity":"-0.000","free_collateral":"1e-7","margin_used":"0012.3400"}`, &pairAccountEvidence{}},
		{`{` + stamp + `,"metadata_generation":18446744073709551615,"meta":{"native_unit":"contract","base_asset":"BTC","contract_size":"0.010","quote_currency":"USDT","settlement_currency":"USDT","price_currency":"USDT","tick_step":"1E-1","lot_step":"1","min_qty":"1.000","min_notional":"-0","trading_status":"live","linear":true,"max_leverage":"123456789012345678901234567890.123456789","price_rule":"fixed_tick_v1","sz_decimals":0,"tick_step_text":"0.1","lot_step_text":"1","fee_currencies":["USDT","AAA"]}}`, &pairCapabilityEvidence{}},
	}
	var sealedCaps []pairCapabilityEvidence
	var sealedAccts []pairAccountEvidence
	var sealedConvs []pairConversionEvidence
	for _, r := range records {
		plain := pairLooseDecode(t, r.text, r.into)
		var b []byte
		var h string
		var err error
		switch rec := r.into.(type) {
		case *pairConversionEvidence:
			s, e := rec.sealed()
			b, err, h = mustCanon(s), e, s.EvidenceHash
			sealedConvs = append(sealedConvs, s)
		case *pairAccountEvidence:
			s, e := rec.sealed()
			b, err, h = mustCanon(s), e, s.EvidenceHash
			sealedAccts = append(sealedAccts, s)
		case *pairCapabilityEvidence:
			s, e := rec.sealed()
			b, err, h = mustCanon(s), e, s.EvidenceHash
			sealedCaps = append(sealedCaps, s)
		}
		cases = append(cases, map[string]interface{}{"kind": "record", "record": plain})
		want = append(want, goOut{hex: hex.EncodeToString(b), hash: h, err: err})
	}
	second := sealedConvs[0]
	second.Key.MemberID, second.EvidenceID = "pair-a-first", "rec-0"
	second, _ = second.sealed()
	doc := pairEvidenceDocument{V: 2, Capabilities: sealedCaps, Accounts: sealedAccts, Conversions: []pairConversionEvidence{sealedConvs[0], second}}
	docBytes, err := pairEvidenceDocumentBytes(doc)
	if err != nil {
		t.Fatal(err)
	}
	var docPlain map[string]interface{}
	docDec := json.NewDecoder(bytes.NewReader(docBytes))
	docDec.UseNumber()
	if err := docDec.Decode(&docPlain); err != nil {
		t.Fatal(err)
	}
	convs := docPlain["conversions"].([]interface{})
	docPlain["conversions"] = []interface{}{convs[1], convs[0]}
	cases = append(cases, map[string]interface{}{"kind": "document", "doc": docPlain})
	want = append(want, goOut{hex: hex.EncodeToString(docBytes), hash: sha256Hex(docBytes)})

	res := pairRunPyCases(t, cases)
	for i, w := range want {
		got := res[i]
		if w.err != nil || !got.OK {
			if w.err == nil || got.OK {
				t.Fatalf("case %d: Go error %v, Python %+v; both languages must refuse the same input", i, w.err, got)
			}
			continue
		}
		if got.Hex != w.hex || got.Hash != w.hash {
			t.Fatalf("case %d bytes differ:\n  go %s %s\n  py %s %s", i, mustUnhex(w.hex), w.hash, mustUnhex(got.Hex), got.Hash)
		}
	}
	for _, want := range []string{`\u003ca\u003e\u0026b \u2028 é \"q\"`, `"free_collateral":"0.0000001"`, `"leverage":"2"`, `"margin_used":"12.34"`,
		`"equity":"0"`, `"metadata_generation":18446744073709551615`, `"observed_ms":9007199254740993`, `"fee_currencies":["AAA","USDT"]`} {
		if !bytes.Contains(docBytes, []byte(want)) {
			t.Fatalf("the canonical document must hold %s: %s", want, docBytes)
		}
	}
}

func mustCanon(v interface{}) []byte {
	b, err := pairCanonicalJSON(v)
	if err != nil {
		return nil
	}
	return b
}

func mustUnhex(s string) string {
	b, _ := hex.DecodeString(s)
	return string(b)
}

func TestBasketPairEvidenceStrictReadersRefuseTheSameInputs(t *testing.T) {
	registerPairTestPolicy(t)
	valid := pairTestEvidenceBytes(t, pairTestDoc(t, filepath.Join(t.TempDir(), "run"), nil), nil)
	edit := func(old, new string) []byte {
		if !bytes.Contains(valid, []byte(old)) {
			t.Fatalf("fixture text %q is missing", old)
		}
		return bytes.Replace(valid, []byte(old), []byte(new), 1)
	}
	indented := func() []byte {
		var out bytes.Buffer
		_ = json.Indent(&out, valid, "", "  ")
		return out.Bytes()
	}()
	inputs := []struct {
		name string
		blob []byte
		ok   bool
	}{
		{"valid", valid, true},
		{"duplicate key", edit(`"v":2}`, `"v":2,"v":2}`), false},
		{"invalid UTF-8", edit(`research-schedule:fixture`, "research-schedule:fixtur\xff"), false},
		{"lone high surrogate", edit(`research-schedule:fixture`, `research-schedule:fixture\ud800`), false},
		{"lone low surrogate", edit(`research-schedule:fixture`, `research-schedule:fixture\udc00`), false},
		{"trailing value", append(append([]byte{}, valid...), []byte(" 1")...), false},
		{"unsupported version", edit(`"v":2}`, `"v":3}`), false},
		{"noncanonical whitespace", indented, false},
		{"unknown field", edit(`{"accounts":`, `{"a":1,"accounts":`), false},
		{"wrong kind", edit(`"observed_ms":1700000000000`, `"observed_ms":"1700000000000"`), false},
		{"fractional integer", edit(`"observed_ms":1700000000000`, `"observed_ms":1700000000000.5`), false},
		{"noncanonical decimal", edit(`"equity":"6000"`, `"equity":"6000.0"`), false},
		{"hash-invalid", edit(`"equity":"6000"`, `"equity":"6001"`), false},
		{"unsorted schedule", func() []byte {
			var d pairEvidenceDocument
			_ = json.Unmarshal(valid, &d)
			d.Accounts[0], d.Accounts[1] = d.Accounts[1], d.Accounts[0]
			b, _ := json.Marshal(d)
			tree, _ := pairStrictParse(b)
			var buf bytes.Buffer
			_ = pairWriteCanonical(&buf, tree)
			return buf.Bytes()
		}(), false},
	}
	var cases []map[string]interface{}
	for _, in := range inputs {
		cases = append(cases, map[string]interface{}{"kind": "decode_document", "input": in.blob})
	}
	res := pairRunPyCases(t, cases)
	for i, in := range inputs {
		_, goErr := newPairFrozenEvidenceSource(in.blob, sha256Hex(in.blob))
		if (goErr == nil) != in.ok || res[i].OK != in.ok {
			t.Fatalf("%s: Go error %v, Python %+v; want accepted=%v in both languages", in.name, goErr, res[i], in.ok)
		}
	}
}

func pairRoundFixture(t *testing.T, m pairContractMember) pairCapabilityEvidence {
	t.Helper()
	wire, err := pairExpectedWireMeta(m)
	if err != nil {
		t.Fatal(err)
	}
	c := pairCapabilityEvidence{pairEvidenceStamp: pairEvidenceStamp{V: 2, Key: pairEvidenceKey{Partition: "paper", Provider: m.Provider, AccountKey: m.AccountKey, Instrument: m.Instrument, MemberID: m.MemberID},
		EvidenceID: "cap-round", ObservedMs: 1, FirstReceiptMs: 2, ValidFromMs: 2, ValidThroughMs: 3, Provenance: pairDerivationIDs[m.Provider]}, Meta: wire, MetadataGeneration: 1}
	c, err = c.sealed()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func pairHLAtSz(m pairContractMember, sz int) pairContractMember {
	tickDecimals := 6 - sz
	if tickDecimals < 0 {
		tickDecimals = 0
	}
	m.Meta.SzDecimals, m.Meta.LotStep, m.Meta.MinQty, m.Meta.TickStep = sz, math.Pow10(-sz), math.Pow10(-sz), math.Pow10(-tickDecimals)
	return m
}

func TestBasketPairEvidenceRoundBoundMatchesPython(t *testing.T) {
	_, _, contract := pairAdmitAccepted(t, nil)
	hl, _ := contract.pairMember(pairTestHL)
	okx, _ := contract.pairMember(pairTestOKX)
	hl0 := pairHLAtSz(hl, 0)
	fine := okx
	fine.Meta.TickStep, fine.Meta.TickStepText = 3e-19, "0.0000000000000000003"
	okxCap, hlCap, hl0Cap, fineCap := pairRoundFixture(t, okx), pairRoundFixture(t, hl), pairRoundFixture(t, hl0), pairRoundFixture(t, fine)
	tampered := okxCap
	tampered.Meta = okxCap.Meta.clone()
	tampered.Meta.TickStep, tampered.Meta.TickStepText = "0.5", "0.5"
	other := pairRoundFixture(t, pairHLAtSz(hl, 4))
	other.Key = hlCap.Key
	other, _ = other.sealed()
	type rc struct {
		m    pairContractMember
		c    pairCapabilityEvidence
		side string
		in   float64
		want float64
	}
	cases := []rc{
		{okx, okxCap, "buy", 60000.1, 60000.1}, {okx, okxCap, "sell", 60000.1, 60000.1},
		{okx, okxCap, "buy", 60000.0, 60000.0}, {okx, okxCap, "sell", 60000.2, 60000.2},
		{okx, okxCap, "buy", 60000.05, 60000.0}, {okx, okxCap, "sell", 60000.05, 60000.1},
		{okx, okxCap, "buy", 60000.15, 60000.1}, {okx, okxCap, "sell", 60000.15, 60000.2},
		{okx, okxCap, "buy", 60000.12, 60000.1}, {okx, okxCap, "sell", 60000.12, 60000.2},
		{hl, hlCap, "buy", 60000.1, 60000}, {hl, hlCap, "sell", 60000.1, 60001},
		{hl, hlCap, "buy", 9999.95, 9999.9}, {hl, hlCap, "sell", 9999.95, 10000},
		{hl, hlCap, "buy", 99999.5, 99999}, {hl, hlCap, "sell", 99999.5, 100000},
		{hl, hlCap, "buy", 0.0123456, 0.012}, {hl, hlCap, "sell", 0.0123456, 0.013},
		{hl, hlCap, "buy", 1.234567, 1.234}, {hl, hlCap, "sell", 1.234567, 1.235},
		{hl0, hl0Cap, "buy", 0.1234567, 0.12345}, {hl0, hl0Cap, "sell", 0.1234567, 0.12346},
		{fine, fineCap, "buy", 1.0, -1}, {fine, fineCap, "sell", 1.0, -1},
		{okx, tampered, "buy", 60000.1, -1},
		{hl, other, "buy", 60000.1, -1},
		{hl, okxCap, "buy", 60000.1, -1},
		{okx, okxCap, "hold", 60000.1, -1},
		{okx, okxCap, "buy", 0, -1}, {okx, okxCap, "sell", -5, -1},
	}
	var pyCases []map[string]interface{}
	for _, c := range cases {
		mb, _ := json.Marshal(c.m)
		cb, _ := json.Marshal(c.c)
		var mPlain, cPlain map[string]interface{}
		_ = json.Unmarshal(mb, &mPlain)
		_ = json.Unmarshal(cb, &cPlain)
		pyCases = append(pyCases, map[string]interface{}{"kind": "round", "member": mPlain, "capability": cPlain, "side": c.side, "bound": c.in})
	}
	res := pairRunPyCases(t, pyCases)
	for i, c := range cases {
		got, err := pairRoundBound(c.m, c.c, c.side, c.in)
		if c.want < 0 {
			if err == nil || res[i].OK {
				t.Fatalf("case %d (%s %v): Go %v %v, Python %+v; both must refuse", i, c.side, c.in, got, err, res[i])
			}
			continue
		}
		if err != nil || got != c.want {
			t.Fatalf("case %d (%s %v): Go %v (%v), want %v", i, c.side, c.in, got, err, c.want)
		}
		var py float64
		if _, perr := fmt.Sscan(res[i].Value, &py); perr != nil || !res[i].OK || py != got {
			t.Fatalf("case %d (%s %v): Python %+v, Go %v; the languages must agree", i, c.side, c.in, res[i], got)
		}
		if (c.side == "buy" && got > c.in) || (c.side == "sell" && got < c.in) {
			t.Fatalf("case %d widened the %s bound %v to %v", i, c.side, c.in, got)
		}
	}
}

const pairReaderFakeDriver = `import atexit, builtins, json, os, runpy, socket, sys, tempfile, time, importlib.util
root = os.environ["PAIR_FAKE_ROOT"]
state_path = os.environ["PAIR_FAKE_STATE"]
mode = os.environ.get("PAIR_FAKE_MODE", "normal")
state = {"violations": [], "creds": {}, "calls": []}
for k in ("HYPERLIQUID_SECRET_KEY", "HYPERLIQUID_ACCOUNT_ADDRESS", "OKX_API_KEY", "OKX_API_SECRET", "OKX_PASSPHRASE"):
    state["creds"][k] = "set" if os.environ.get(k) else "empty"
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
    path = str(file)
    if any(c in m for c in "wax+"):
        violation("write " + path)
    if path.endswith("hl_meta.json"):
        violation("cache read " + path)
    return real_open(file, mode, *a, **k)
builtins.open = guarded_open
tempfile.mkstemp = lambda *a, **k: violation("mkstemp")
os.replace = lambda *a, **k: violation("os.replace")
socket.socket.connect = lambda self, *a, **k: violation("socket connect")
if mode == "probe":
    real_spec = importlib.util.spec_from_file_location
    def spec(name, path, *a, **k):
        if "adapter" in str(path) or "pair_evidence" in str(path):
            violation("load " + str(path))
        return real_spec(name, path, *a, **k)
    importlib.util.spec_from_file_location = spec
else:
    lev = 25 if mode == "lev25" else 20
    universe = [{"name": "ETH", "szDecimals": 4, "maxLeverage": 25}]
    if mode != "unresolved":
        universe.append({"name": "BTC", "szDecimals": "3" if mode == "malformed" else 3, "maxLeverage": lev})
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
        if mode == "sleep":
            time.sleep(30)
        if path == "/info" and t == "meta":
            return meta
        if path == "/info" and t == "spotMeta":
            return spot
        violation("hyperliquid call " + path + " " + str(t))
    hyperliquid.api.API.post = post
    import ccxt
    okx_record = {"instId": "BTC-USDT-SWAP", "instType": "SWAP", "instFamily": "BTC-USDT", "uly": "BTC-USDT", "ctValCcy": "BTC", "settleCcy": "USDC" if mode == "usdc" else "USDT",
                  "ctVal": "0.01", "ctType": "linear", "tickSz": "0.1", "lotSz": "1", "minSz": "1", "lever": "20", "state": "live"}
    def fetch(self, url, method="GET", headers=None, body=None):
        state["calls"].append("okx:" + url.split("?")[0])
        if "/api/v5/public/instruments" in url:
            return {"code": "0", "msg": "", "data": [okx_record]}
        violation("okx call " + url)
    ccxt.okx.fetch = fetch
    real_sign = ccxt.okx.sign
    def sign(self, path, api="public", method="GET", params={}, headers=None, body=None):
        if "private" in str(api):
            violation("okx private sign " + str(path))
        return real_sign(self, path, api, method, params, headers, body)
    ccxt.okx.sign = sign
script = os.path.join(root, "shared_scripts", "fetch_pair_evidence.py")
sys.argv = [script] + sys.argv[1:]
runpy.run_path(script, run_name="__main__")
`

type pairReaderFixture struct {
	t     *testing.T
	root  string
	state string
}

func newPairReaderFixture(t *testing.T) *pairReaderFixture {
	t.Helper()
	root := basketPyRoot(t)
	dir := t.TempDir()
	driver := filepath.Join(dir, "reader_driver.py")
	if err := os.WriteFile(driver, []byte(pairReaderFakeDriver), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &pairReaderFixture{t: t, root: root, state: filepath.Join(dir, "state.json")}
	t.Setenv("PAIR_FAKE_ROOT", root)
	t.Setenv("PAIR_FAKE_STATE", f.state)
	for _, k := range pairEvidenceReaderClearedEnv {
		t.Setenv(k, "sentinel-secret-1869")
	}
	t.Chdir(root)
	prev := pairEvidenceReaderScript
	pairEvidenceReaderScript = driver
	t.Cleanup(func() { pairEvidenceReaderScript = prev })
	return f
}

type pairReaderState struct {
	Violations []string          `json:"violations"`
	Creds      map[string]string `json:"creds"`
	Calls      []string          `json:"calls"`
}

func (f *pairReaderFixture) read(mode string) pairReaderState {
	f.t.Helper()
	b, err := os.ReadFile(f.state)
	if err != nil && mode == "sleep" {
		return pairReaderState{}
	}
	if err != nil {
		f.t.Fatalf("%s: the reader driver wrote no state: %v", mode, err)
	}
	if bytes.Contains(b, []byte("sentinel-secret-1869")) {
		f.t.Fatalf("%s: the reader saw or logged a credential value", mode)
	}
	var s pairReaderState
	if err := json.Unmarshal(b, &s); err != nil {
		f.t.Fatal(err)
	}
	_ = os.Remove(f.state)
	return s
}

func TestBasketPairEvidencePublicReaderIsPublicOnly(t *testing.T) {
	_, _, contract := pairAdmitAccepted(t, nil)
	hl, _ := contract.pairMember(pairTestHL)
	okx, _ := contract.pairMember(pairTestOKX)
	f := newPairReaderFixture(t)
	read := func(mode string, m pairContractMember, prev *pairCapabilityEvidence, timeout time.Duration) (pairCapabilityEvidence, error, pairReaderState) {
		t.Setenv("PAIR_FAKE_MODE", mode)
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		c, err := pairReadPublicCapability(ctx, contract.pairMemberKey(m).evidenceKey(), prev, 60000)
		return c, err, f.read(mode)
	}
	for _, m := range []pairContractMember{hl, okx} {
		c, err, st := read("normal", m, nil, 30*time.Second)
		if err != nil {
			t.Fatalf("%s public read: %v", m.Provider, err)
		}
		if len(st.Violations) > 0 {
			t.Fatalf("%s public read touched a forbidden path: %v", m.Provider, st.Violations)
		}
		for k, v := range st.Creds {
			if v != "empty" {
				t.Fatalf("%s public read received credential %s", m.Provider, k)
			}
		}
		if err := pairCapabilityMatches(m, c); err != nil {
			t.Fatalf("%s live capability must match the frozen contract: %v", m.Provider, err)
		}
		if c.Provenance != pairDerivationIDs[m.Provider] || c.ObservedMs > c.FirstReceiptMs || c.ValidFromMs != c.FirstReceiptMs || c.ValidThroughMs != c.FirstReceiptMs+60000 || c.MetadataGeneration != 1 {
			t.Fatalf("%s live stamp is wrong: %+v", m.Provider, c.pairEvidenceStamp)
		}
		for _, call := range st.Calls {
			if call != "hl:meta" && call != "hl:spotMeta" && call != "okx:https://www.okx.com/api/v5/public/instruments" {
				t.Fatalf("%s public read made call %q", m.Provider, call)
			}
		}
		again, err, _ := read("normal", m, &c, 30*time.Second)
		if err != nil || again.MetadataGeneration != 1 {
			t.Fatalf("an unchanged read keeps the generation: %v %d", err, again.MetadataGeneration)
		}
	}
	first, _, _ := read("normal", hl, nil, 30*time.Second)
	changed, err, _ := read("lev25", hl, &first, 30*time.Second)
	if err != nil || changed.MetadataGeneration != 2 {
		t.Fatalf("changed public metadata must advance the generation: %v %d", err, changed.MetadataGeneration)
	}
	if err := pairCapabilityMatches(hl, changed); err == nil || !strings.Contains(err.Error(), "meta.max_leverage") {
		t.Fatalf("a changed public rule must not match the frozen contract: %v", err)
	}
	if _, err, _ := read("usdc", okx, nil, 30*time.Second); err == nil || !strings.Contains(err.Error(), "settlement_currency") {
		t.Fatalf("a non-USDT OKX instrument must be refused by the reader, got %v", err)
	}
	for _, mode := range []string{"unresolved", "malformed"} {
		_, err, st := read(mode, hl, nil, 30*time.Second)
		if err == nil || len(st.Violations) > 0 {
			t.Fatalf("%s Hyperliquid metadata must fail closed without a fallback: %v %v", mode, err, st.Violations)
		}
	}
	start := time.Now()
	if _, err, _ := read("sleep", hl, nil, 3*time.Second); err == nil || time.Since(start) > 20*time.Second {
		t.Fatalf("the caller deadline must stop the reader, got %v after %s", err, time.Since(start))
	}
	if _, err := pairReadPublicCapability(context.Background(), contract.pairMemberKey(hl).evidenceKey(), nil, 60000); err == nil {
		t.Fatal("a read without a caller deadline must be refused")
	}
	t.Setenv("GO_TRADER_EGRESS_MODE", "proxy")
	t.Setenv("GO_TRADER_EGRESS_PROXY", "")
	_, err, st := read("normal", hl, nil, 30*time.Second)
	if err == nil || len(st.Calls) > 0 {
		t.Fatalf("an invalid egress policy must refuse the read before any venue call: %v %v", err, st.Calls)
	}
}

func TestBasketPairEvidenceProbeArgvsParseOffline(t *testing.T) {
	f := newPairReaderFixture(t)
	t.Setenv("PAIR_FAKE_MODE", "probe")
	for _, argv := range [][]string{pairEvidenceHyperliquidProbeArgv, pairEvidenceOKXProbeArgv} {
		if err := probeOneCheckScript(pairEvidenceReaderScript, argv); err != nil {
			t.Fatalf("the guarded probe failed: %v", err)
		}
		st := f.read("probe")
		if len(st.Violations) > 0 || len(st.Calls) > 0 {
			t.Fatalf("probe-only constructed an adapter or reached the network: %+v", st)
		}
	}
	pairEvidenceReaderScript = "shared_scripts/fetch_pair_evidence.py"
	for _, argv := range [][]string{pairEvidenceHyperliquidProbeArgv, pairEvidenceOKXProbeArgv} {
		if err := probeOneCheckScript(pairEvidenceReaderScript, argv); err != nil {
			t.Fatalf("the real reader rejected its probe argv: %v", err)
		}
	}
	if err := probeOneCheckScript(pairEvidenceReaderScript, []string{"--provider=binance", "--instrument=BTC", "--probe-only"}); err == nil {
		t.Fatal("the reader must reject an unsupported provider even in probe mode")
	}
}
