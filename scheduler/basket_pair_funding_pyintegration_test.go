//go:build pyintegration

package main

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const pairFundingPyDriver = `import base64, importlib.util, json, os, sys
root = os.environ["PAIR_PY_ROOT"]
sys.path.insert(0, os.path.join(root, "shared_tools"))
import pair_funding as pf
spec = importlib.util.spec_from_file_location("okx_adapter_under_test", os.path.join(root, "platforms", "okx", "adapter.py"))
adapter = importlib.util.module_from_spec(spec)
spec.loader.exec_module(adapter)
req = json.load(sys.stdin)
out = []
for case in req["cases"]:
    try:
        kind = case["kind"]
        if kind == "decode_document":
            data = base64.b64decode(case["input"])
            doc = pf.decode_document(data, case.get("digest"))
            out.append({"ok": True, "hex": pf.document_bytes(doc).hex(), "hashes": [pf.source_hash(r) for r in doc["rates"]]})
        elif kind == "seal":
            sealed = pf.seal(case["record"], case["record_kind"])
            out.append({"ok": True, "hash": sealed["record_hash"]})
        elif kind == "decode_event":
            ev = pf.decode_event(base64.b64decode(case["input"]))
            out.append({"ok": True, "hash": pf.event_hash(ev), "value": ev["usd_amount"]})
        elif kind == "amounts":
            native, usd = pf.funding_amounts(case["qty"], case["price"], case["rate"], case["conv"])
            out.append({"ok": True, "value": native + "|" + usd})
        elif kind == "okx_range":
            a = adapter.OKXExchangeAdapter(public_only=True)
            a._exchange.urls["api"]["rest"] = case["base"]
            res = a.get_funding_history_range(case["symbol"], case["start"], case["end"], **case.get("opts", {}))
            out.append({"ok": True, "result": res})
        elif kind == "okx_legacy":
            a = adapter.OKXExchangeAdapter(public_only=True)
            a._exchange.urls["api"]["rest"] = case["base"]
            got = a.get_funding_history(case["symbol"], days=case["days"])
            out.append({"ok": True, "value": json.dumps(got)})
        elif kind == "okx_document":
            a = adapter.OKXExchangeAdapter(public_only=True)
            a._exchange.urls["api"]["rest"] = case["base"]
            res = a.get_funding_history_range(case["symbol"], case["start"], case["end"], page_limit=case["page"])
            rates, cov = pf.okx_range_records(case["key"], res, case["visible"], "okx-funding-history:fixture", "okx-range-1")
            doc = {"v": pf.DOCUMENT_VERSION, "source_id": "okx-range-producer", "rates": rates, "prices": [], "coverage": [cov]}
            data = pf.document_bytes(doc)
            out.append({"ok": True, "hex": data.hex(), "result": res})
        else:
            out.append({"ok": False, "error": "unknown case " + kind})
    except Exception as exc:
        out.append({"ok": False, "error": type(exc).__name__ + ": " + str(exc)})
json.dump(out, sys.stdout)
`

type pairFundingPyResult struct {
	OK     bool                   `json:"ok"`
	Hex    string                 `json:"hex"`
	Hash   string                 `json:"hash"`
	Hashes []string               `json:"hashes"`
	Value  string                 `json:"value"`
	Error  string                 `json:"error"`
	Result map[string]interface{} `json:"result"`
}

func pairFundingRunPy(t *testing.T, cases []map[string]interface{}) []pairFundingPyResult {
	t.Helper()
	root := basketPyRoot(t)
	driver := filepath.Join(t.TempDir(), "pair_funding_driver.py")
	if err := os.WriteFile(driver, []byte(pairFundingPyDriver), 0o600); err != nil {
		t.Fatal(err)
	}
	req, err := json.Marshal(map[string]interface{}{"cases": cases})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(filepath.Join(root, ".venv", "bin", "python3"), "-I", driver)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "PAIR_PY_ROOT="+root, "PYTHONDONTWRITEBYTECODE=1", "OKX_API_KEY=", "OKX_API_SECRET=", "OKX_PASSPHRASE=", "OKX_SANDBOX=")
	cmd.Stdin = bytes.NewReader(req)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("python driver: %v\n%s", err, stderr.String())
	}
	var res []pairFundingPyResult
	if err := json.Unmarshal(out, &res); err != nil || len(res) != len(cases) {
		t.Fatalf("python driver output %s (%v)", out, err)
	}
	return res
}

func pairFundingPyDocument(t *testing.T) ([]byte, pairFundingDocument) {
	t.Helper()
	hl := pairEvidenceKey{Partition: "paper", Provider: "hyperliquid", AccountKey: "synthetic:hl-research", Instrument: "BTC", MemberID: pairTestHL}
	okx := pairEvidenceKey{Partition: "paper", Provider: "okx", AccountKey: "synthetic:okx-research", Instrument: "BTC-USDT-SWAP", MemberID: pairTestOKX}
	t0 := pairTestStart().UnixMilli()
	d := pairFundingDocument{V: pairFundingDocumentVersion, SourceID: "parity-fixture",
		Rates:  []pairFundingRate{pfRate(okx, pairFundingRuleOKX, t0+28800000, "-0.00012345"), pfRate(hl, pairFundingRuleHyperliquid, t0+3600076, "0.0000125")},
		Prices: []pairFundingPrice{pfPrice(okx, pairFundingPriceMark, t0+28800000, "60100.5"), pfPrice(hl, pairFundingPriceOracle, t0+3600076, "59999.25")},
		Coverage: []pairFundingCoverageRecord{
			{V: pairFundingRecordVersion, Key: okx, EvidenceID: "okx-cov", RequestedFromMs: t0, RequestedThroughMs: t0 + 2*28800000,
				Proven: []pairFundingInterval{{FromMs: t0, ThroughMs: t0 + 28800000}}, Gaps: []pairCoverageGap{{FromMs: t0 + 28800000, ThroughMs: t0 + 2*28800000, Reason: "after_last_record"}},
				VisibleMs: t0 + 28801000, Provenance: "parity"},
			{V: pairFundingRecordVersion, Key: hl, EvidenceID: "hl-cov", RequestedFromMs: t0, RequestedThroughMs: t0 + 3600076,
				Proven: []pairFundingInterval{{FromMs: t0, ThroughMs: t0 + 3600076}}, Gaps: []pairCoverageGap{}, VisibleMs: t0 + 3601076, Provenance: "parity"},
		}}
	blob := pairTestFundingSeal(t, &d)
	return blob, d
}

func TestBasketPairFundingPythonCodecMatchesGo(t *testing.T) {
	blob, d := pairFundingPyDocument(t)
	if _, err := newPairFrozenFundingSource(blob, sha256Hex(blob)); err != nil {
		t.Fatalf("the Go reader must accept its own document: %v", err)
	}
	unsealed := d.Rates[0]
	unsealed.RecordHash = ""
	var rateMap map[string]interface{}
	raw, _ := json.Marshal(unsealed)
	_ = json.Unmarshal(raw, &rateMap)
	refusals := map[string][]byte{
		"unknown field":       bytes.Replace(blob, []byte(`"source_id"`), []byte(`"extra":1,"source_id"`), 1),
		"non-canonical rate":  bytes.Replace(blob, []byte(`"-0.00012345"`), []byte(`"-0.000123450"`), 1),
		"bad event id":        bytes.Replace(blob, []byte(`"okx:BTC-USDT-SWAP:`), []byte(`"okx:BTC-USDT-SWAP:9`), 1),
		"duplicate key":       bytes.Replace(blob, []byte(`{"coverage"`), []byte(`{"v":1,"coverage"`), 1),
		"tampered rate value": bytes.Replace(blob, []byte(`"0.0000125"`), []byte(`"0.0000126"`), 1),
	}
	cases := []map[string]interface{}{
		{"kind": "decode_document", "input": base64.StdEncoding.EncodeToString(blob), "digest": sha256Hex(blob)},
		{"kind": "seal", "record": rateMap, "record_kind": "rate"},
		{"kind": "amounts", "qty": "-0.05", "price": "60100.5", "rate": "-0.00012345", "conv": "0.9995"},
	}
	names := make([]string, 0, len(refusals))
	for name := range refusals {
		names = append(names, name)
	}
	for _, name := range names {
		cases = append(cases, map[string]interface{}{"kind": "decode_document", "input": base64.StdEncoding.EncodeToString(refusals[name])})
	}
	res := pairFundingRunPy(t, cases)
	if !res[0].OK || res[0].Hex != fmt.Sprintf("%x", blob) {
		t.Fatalf("Python must re-encode the Go document byte for byte: %+v", res[0])
	}
	sorted, err := pairDecodeFundingDocument(blob)
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range sorted.Rates {
		sh, _ := pairFundingSourceHash(r)
		if res[0].Hashes[i] != sh {
			t.Fatalf("source hash %d differs between Go %s and Python %s", i, sh, res[0].Hashes[i])
		}
	}
	if !res[1].OK || res[1].Hash != d.Rates[0].RecordHash {
		t.Fatalf("Python and Go must seal the same record hash: %+v want %s", res[1], d.Rates[0].RecordHash)
	}
	qty, _ := pairDecimal("-0.05").rat()
	px, _ := pairDecimal("60100.5").rat()
	rate, _ := pairDecimal("-0.00012345").rat()
	conv, _ := pairDecimal("0.9995").rat()
	native, usd, aerr := pairFundingAmounts(qty, px, rate, conv)
	if aerr != nil || res[2].Value != native+"|"+usd {
		t.Fatalf("Go %s|%s and Python %s must compute the same exact amounts (%v)", native, usd, res[2].Value, aerr)
	}
	for i, name := range names {
		r := res[3+i]
		if r.OK {
			t.Fatalf("Python accepted the %s input", name)
		}
		if _, err := pairDecodeFundingDocument(refusals[name]); err == nil {
			t.Fatalf("Go accepted the %s input", name)
		}
	}

	t0 := pairTestStart()
	tOKX := t0.Add(4 * time.Hour).UnixMilli()
	e := newPairFundingEnv(t, func(d *pairFundingDocument, k map[string]pairEvidenceKey) {
		d.Rates = []pairFundingRate{pfRate(k[pairTestOKX], pairFundingRuleOKX, tOKX, "-0.0002")}
		d.Prices = []pairFundingPrice{pfPrice(k[pairTestOKX], pairFundingPriceMark, tOKX, "60100")}
		pairFundingAutoCoverage(d, k)
	}, pairFundingConversions("1.0002", "0.9995"))
	e.openMatched(0.05)
	e.step(t0.Add(4*time.Hour + 61*time.Second))
	_, _, eventJSON := e.fundingRow(pairTestOKX, pairFundingEventID("okx", "BTC-USDT-SWAP", tOKX))
	ev, err := pairDecodeFundingEvent([]byte(eventJSON))
	if err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(eventJSON, `"usd_amount":"-0.6006995"`, `"usd_amount":"-0.6006996"`, 1)
	res = pairFundingRunPy(t, []map[string]interface{}{
		{"kind": "decode_event", "input": base64.StdEncoding.EncodeToString([]byte(eventJSON))},
		{"kind": "decode_event", "input": base64.StdEncoding.EncodeToString([]byte(tampered))},
	})
	if !res[0].OK || res[0].Hash != ev.EvidenceHash || res[0].Value != "-0.6006995" {
		t.Fatalf("Python must verify the saved Go funding event: %+v", res[0])
	}
	if res[1].OK || tampered == eventJSON {
		t.Fatalf("Python must refuse a tampered amount: %+v", res[1])
	}
}

type pairOKXFundingFixture struct {
	mu     sync.Mutex
	afters map[string][]string
	insts  map[string][]string
}

func (f *pairOKXFundingFixture) handler(t *testing.T) http.Handler {
	period := int64(28800000)
	row := func(k int64, rate string) map[string]string {
		return map[string]string{"instId": "BTC-USDT-SWAP", "instType": "SWAP", "fundingTime": strconv.FormatInt(k*period, 10), "realizedRate": rate, "fundingRate": rate, "method": "current_period", "formulaType": "withRate"}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 2)
		scenario := parts[0]
		q := r.URL.Query()
		f.mu.Lock()
		f.afters[scenario] = append(f.afters[scenario], q.Get("after"))
		f.insts[scenario] = append(f.insts[scenario], q.Get("instId"))
		f.mu.Unlock()
		if len(parts) < 2 || parts[1] != "api/v5/public/funding-rate-history" {
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusInternalServerError)
			return
		}
		after, _ := strconv.ParseInt(q.Get("after"), 10, 64)
		limit, _ := strconv.Atoi(q.Get("limit"))
		lo := int64(1)
		switch scenario {
		case "exhausted":
			lo = 6
		case "transport":
			http.Error(w, "upstream failure", http.StatusBadGateway)
			return
		case "venue":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"code": "51001", "msg": "Instrument ID does not exist", "data": []interface{}{}})
			return
		}
		var data []map[string]string
		for k := int64(20); k >= lo; k-- {
			if k*period >= after && scenario != "stuck" {
				continue
			}
			data = append(data, row(k, "0.0001"))
		}
		switch scenario {
		case "conflict":
			if len(data) > 1 {
				dup := row(mustParseFundingTime(data[0]["fundingTime"])/period, "0.0002")
				data = append([]map[string]string{data[0], dup}, data[1:]...)
			}
		case "duplicate":
			if len(data) > 0 {
				data = append([]map[string]string{data[0]}, data...)
			}
		}
		if scenario != "truncate" && len(data) > limit {
			data = data[:limit]
		}
		if scenario == "truncate" && len(data) > limit+1 {
			data = data[:limit+1]
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"code": "0", "msg": "", "data": data})
	})
}

func mustParseFundingTime(s string) int64 {
	v, _ := strconv.ParseInt(s, 10, 64)
	return v
}

func pairJSONInt(v interface{}) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case json.Number:
		i, _ := n.Int64()
		return i
	}
	return -1
}

func pairIntervals(v interface{}) [][3]interface{} {
	var out [][3]interface{}
	arr, _ := v.([]interface{})
	for _, raw := range arr {
		m := raw.(map[string]interface{})
		out = append(out, [3]interface{}{pairJSONInt(m["from_ms"]), pairJSONInt(m["through_ms"]), m["reason"]})
	}
	return out
}

func TestBasketPairFundingPythonOKXRangeReader(t *testing.T) {
	f := &pairOKXFundingFixture{afters: map[string][]string{}, insts: map[string][]string{}}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	p := int64(28800000)
	base := func(s string) string { return srv.URL + "/" + s }
	cases := []map[string]interface{}{
		{"kind": "okx_range", "base": base("complete"), "symbol": "BTC", "start": 3 * p, "end": 10 * p, "opts": map[string]interface{}{"page_limit": 3}},
		{"kind": "okx_range", "base": base("tail"), "symbol": "BTC-USDT-SWAP", "start": 3 * p, "end": 10*p + 5, "opts": map[string]interface{}{"page_limit": 3}},
		{"kind": "okx_range", "base": base("exhausted"), "symbol": "BTC", "start": 3 * p, "end": 10 * p, "opts": map[string]interface{}{"page_limit": 3}},
		{"kind": "okx_range", "base": base("limit"), "symbol": "BTC", "start": 3 * p, "end": 10 * p, "opts": map[string]interface{}{"page_limit": 1, "max_requests": 3}},
		{"kind": "okx_range", "base": base("conflict"), "symbol": "BTC", "start": 3 * p, "end": 10 * p, "opts": map[string]interface{}{"page_limit": 3}},
		{"kind": "okx_range", "base": base("duplicate"), "symbol": "BTC", "start": 3 * p, "end": 10 * p, "opts": map[string]interface{}{"page_limit": 4}},
		{"kind": "okx_range", "base": base("stuck"), "symbol": "BTC", "start": 3 * p, "end": 10 * p, "opts": map[string]interface{}{"page_limit": 3}},
		{"kind": "okx_range", "base": base("truncate"), "symbol": "BTC", "start": 3 * p, "end": 10 * p, "opts": map[string]interface{}{"page_limit": 3}},
		{"kind": "okx_range", "base": base("transport"), "symbol": "BTC", "start": 3 * p, "end": 10 * p, "opts": map[string]interface{}{"page_limit": 3}},
		{"kind": "okx_range", "base": base("venue"), "symbol": "BTC", "start": 3 * p, "end": 10 * p, "opts": map[string]interface{}{"page_limit": 3}},
		{"kind": "okx_range", "base": base("refuse"), "symbol": "BTC", "start": 3 * p, "end": 10 * p, "opts": map[string]interface{}{"page_limit": 401}},
		{"kind": "okx_range", "base": base("refuse"), "symbol": "BTC", "start": 3 * p, "end": 10 * p, "opts": map[string]interface{}{"page_limit": 0}},
		{"kind": "okx_range", "base": base("refuse"), "symbol": "BTC-USD-SWAP", "start": 3 * p, "end": 10 * p},
		{"kind": "okx_range", "base": base("refuse"), "symbol": "BTC", "start": 10 * p, "end": 10 * p},
		{"kind": "okx_range", "base": base("refuse"), "symbol": "BTC", "start": 3 * p, "end": 10 * p, "opts": map[string]interface{}{"max_requests": 65}},
		{"kind": "okx_legacy", "base": base("transport"), "symbol": "BTC", "days": 7},
	}
	res := pairFundingRunPy(t, cases)
	expect := func(i int, label string, events int, proven, gaps [][3]interface{}, complete bool, stop string) {
		t.Helper()
		r := res[i]
		if !r.OK {
			t.Fatalf("%s: the reader failed: %s", label, r.Error)
		}
		got := r.Result
		evs, _ := got["events"].([]interface{})
		if len(evs) != events {
			t.Fatalf("%s: %d events, want %d: %v", label, len(evs), events, got)
		}
		if gp, gg := pairIntervals(got["proven"]), pairIntervals(got["gaps"]); fmt.Sprint(gp) != fmt.Sprint(proven) || fmt.Sprint(gg) != fmt.Sprint(gaps) {
			t.Fatalf("%s: proven %v gaps %v, want %v and %v", label, gp, gg, proven, gaps)
		}
		if got["complete"] != complete || got["stop_reason"] != stop {
			t.Fatalf("%s: complete %v stop %v, want %v and %q", label, got["complete"], got["stop_reason"], complete, stop)
		}
	}
	expect(0, "complete", 7, [][3]interface{}{{3 * p, 10 * p, nil}}, nil, true, "")
	expect(1, "tail after the last record", 7, [][3]interface{}{{3 * p, 10 * p, nil}}, [][3]interface{}{{10 * p, 10*p + 5, "after_last_record"}}, false, "")
	expect(2, "history exhausted", 5, [][3]interface{}{{6*p - 1, 10 * p, nil}}, [][3]interface{}{{3 * p, 6*p - 1, "history_exhausted"}}, false, "history_exhausted")
	expect(3, "request limit", 3, [][3]interface{}{{8*p - 1, 10 * p, nil}}, [][3]interface{}{{3 * p, 8*p - 1, "request_limit"}}, false, "request_limit")
	expect(4, "conflicting duplicate", 0, nil, [][3]interface{}{{3 * p, 10 * p, "malformed_response"}}, false, "malformed_response")
	expect(5, "equal duplicate", 7, [][3]interface{}{{3 * p, 10 * p, nil}}, nil, true, "")
	expect(6, "no cursor progress", 0, nil, [][3]interface{}{{3 * p, 10 * p, "malformed_response"}}, false, "malformed_response")
	expect(7, "truncated page", 0, nil, [][3]interface{}{{3 * p, 10 * p, "truncation"}}, false, "truncation")
	expect(8, "transport failure", 0, nil, [][3]interface{}{{3 * p, 10 * p, "transport_error"}}, false, "transport_error")
	expect(9, "venue error", 0, nil, [][3]interface{}{{3 * p, 10 * p, "venue_error"}}, false, "venue_error")
	for i := 10; i <= 14; i++ {
		if res[i].OK || !strings.Contains(res[i].Error, "ValueError") {
			t.Fatalf("case %d must be refused before any request: %+v", i, res[i])
		}
	}
	if !res[15].OK || res[15].Value != "[]" {
		t.Fatalf("the legacy history read keeps its public behavior (an empty list on failure): %+v", res[15])
	}
	f.mu.Lock()
	afters, insts := map[string][]string{}, map[string][]string{}
	for k, v := range f.afters {
		afters[k] = append([]string(nil), v...)
	}
	for k, v := range f.insts {
		insts[k] = append([]string(nil), v...)
	}
	f.mu.Unlock()
	if len(afters["refuse"]) != 0 {
		t.Fatalf("refused arguments sent %d requests", len(afters["refuse"]))
	}
	for _, s := range []string{"complete", "exhausted", "limit"} {
		prev := int64(0)
		for i, a := range afters[s] {
			v, _ := strconv.ParseInt(a, 10, 64)
			if i == 0 && v != 10*p+1 {
				t.Fatalf("%s: the first cursor must be end+1, got %d", s, v)
			}
			if i > 0 && v >= prev {
				t.Fatalf("%s: the cursor must strictly decrease, got %v", s, afters[s])
			}
			prev = v
		}
		for _, inst := range insts[s] {
			if inst != "BTC-USDT-SWAP" {
				t.Fatalf("%s: the native instrument must be BTC-USDT-SWAP, got %q", s, inst)
			}
		}
	}
	if n := len(afters["limit"]); n != 3 {
		t.Fatalf("the request bound allows 3 requests, got %d", n)
	}
	if n := len(afters["complete"]); n != 3 {
		t.Fatalf("the complete window needs 3 pages of 3, got %d", n)
	}

	key := map[string]interface{}{"partition": "paper", "provider": "okx", "account_key": "synthetic:okx-research", "instrument": "BTC-USDT-SWAP", "member_id": pairTestOKX}
	res = pairFundingRunPy(t, []map[string]interface{}{{"kind": "okx_document", "base": base("complete"), "symbol": "BTC", "start": 3 * p, "end": 10 * p, "page": 3, "key": key, "visible": 10*p + 60000}})
	if !res[0].OK {
		t.Fatalf("the producer path failed: %s", res[0].Error)
	}
	doc, err := hex.DecodeString(res[0].Hex)
	if err != nil {
		t.Fatal(err)
	}
	d, err := pairDecodeFundingDocument(doc)
	if err != nil {
		t.Fatalf("the Go reader must accept the document the Python producer built from the range reader: %v", err)
	}
	if len(d.Rates) != 7 || len(d.Coverage) != 1 || d.Coverage[0].Proven[0] != (pairFundingInterval{FromMs: 3 * p, ThroughMs: 10 * p}) || d.Rates[0].Rate != "0.0001" || d.Rates[0].RuleVersion != pairFundingRuleOKX {
		t.Fatalf("the produced document is wrong: %+v (reader %v)", d, res[0].Result)
	}
}
