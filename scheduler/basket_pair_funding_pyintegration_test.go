//go:build pyintegration

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
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
        elif kind == "decode_batch":
            batch = pf.decode_batch(base64.b64decode(case["input"]))
            out.append({"ok": True, "hex": pf.batch_bytes(batch).hex(), "hash": pf.batch_hash(batch)})
        elif kind == "decode_binding":
            binding = pf.decode_binding(base64.b64decode(case["input"]))
            out.append({"ok": True, "hex": pf.canonical_bytes(binding).hex()})
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

func TestBasketPairFundingPythonLegacyCanonicalGoldens(t *testing.T) {
	blob, d := pairFundingPyDocument(t)
	batch, err := (pairFundingBatch{V: pairFundingBatchVersion, SourceID: d.SourceID, Seq: 1, Rates: d.Rates, Prices: d.Prices, Coverage: d.Coverage}).sealed()
	if err != nil {
		t.Fatal(err)
	}
	batchJSON, err := pairFundingBatchBytes(batch)
	if err != nil {
		t.Fatal(err)
	}
	rulesDigest, _, err := pairFundingRulesDigest()
	if err != nil {
		t.Fatal(err)
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
	eventID := pairFundingEventID("okx", "BTC-USDT-SWAP", tOKX)
	_, _, eventJSON := e.fundingRow(pairTestOKX, eventID)
	e.restart()
	_, _, reloaded := e.fundingRow(pairTestOKX, eventID)
	if reloaded != eventJSON {
		t.Fatal("legacy event bytes changed across the real coordinator save and reload")
	}
	files := map[string][]byte{
		"rules-digest.txt": []byte(rulesDigest + "\n"),
		"binding-v1.json":  append([]byte(nil), e.adm.fundingBinding...),
		"document-v1.json": blob,
		"batch-v1.json":    batchJSON,
		"event-v2.json":    []byte(eventJSON),
	}
	capture := os.Getenv("GO_TRADER_CAPTURE_PAIR_FUNDING_GOLDENS")
	if capture != "" {
		if err := os.MkdirAll(capture, 0o700); err != nil {
			t.Fatal(err)
		}
		base, err := exec.Command("git", "rev-parse", "HEAD").Output()
		if err != nil {
			t.Fatal(err)
		}
		hashes := map[string]string{"base_sha256": strings.TrimSpace(string(base)), "rules_digest": rulesDigest}
		for name, raw := range files {
			hashes[name] = sha256Hex(raw)
		}
		metadata, err := pairCanonicalJSON(hashes)
		if err != nil {
			t.Fatal(err)
		}
		files["hashes.json"] = metadata
		for name, raw := range files {
			if err := os.WriteFile(filepath.Join(capture, name), raw, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return
	}
	dir := filepath.Join(basketPyRoot(t), "scheduler", "testdata", "pair_funding_legacy")
	for _, name := range []string{"rules-digest.txt", "binding-v1.json", "document-v1.json", "batch-v1.json", "event-v2.json", "hashes.json"} {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read immutable legacy golden %s: %v", name, err)
		}
		if name != "hashes.json" && !bytes.Equal(raw, files[name]) {
			t.Fatalf("legacy golden %s differs from current canonical bytes", name)
		}
	}
}

func TestBasketPairFundingPythonResearchCodecMatchesGo(t *testing.T) {
	_, legacy := pairFundingPyDocument(t)
	t0 := pairTestStart().UnixMilli()
	target := t0 + 3600000
	policy := pairFundingPolicyV1{Version: 1, MemberRules: map[string]string{pairTestHL: pairFundingRuleHLReceiptResearch, pairTestOKX: pairFundingRuleOKX}, WindowMs: 5000, MaxPrintOffsetMs: 60000, PrepareLeadMs: 30000, FromMs: t0, DrainThroughMs: t0 + 7200000}
	digest, err := pairFundingPolicyDigest(policy)
	if err != nil {
		t.Fatal(err)
	}
	var hlKey pairEvidenceKey
	var hlRate pairFundingRate
	rates := append([]pairFundingRate(nil), legacy.Rates...)
	for i, r := range rates {
		if r.Key.Provider == entryProviderHyperliquid {
			hlKey = r.Key
			hlRate = r
			rates[i].RuleVersion = pairFundingRuleHLReceiptResearch
			rates[i], err = rates[i].sealed()
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	attempt, err := pairFundingOracleAttemptID(hlKey, target, digest)
	if err != nil {
		t.Fatal(err)
	}
	obs, err := (pairFundingOracleObservation{V: 2, Key: hlKey, EvidenceID: pairFundingOracleEvidenceID(hlKey, target, target+1), AttemptID: attempt, PolicyDigest: digest, PriceKind: pairFundingPriceOracleReceipt, TargetEventMs: target, RequestMs: target, ReceiptMs: target + 1, AtMs: target + 1, VisibleMs: target + 1, TimeBasis: pairFundingObservationTimeBasis, Price: "59999.25", Provenance: pairFundingOracleProvenance}).sealed()
	if err != nil {
		t.Fatal(err)
	}
	prices := []pairFundingPrice{}
	for _, p := range legacy.Prices {
		if p.Key.Provider == entryProviderOKX {
			prices = append(prices, p)
		}
	}
	doc := pairFundingDocumentV2{V: 2, SourceID: "research-codec", Policy: policy, PolicyDigest: digest, Rates: rates, Prices: prices, Observations: []pairFundingOracleObservation{obs}, Coverage: legacy.Coverage}
	docJSON, err := pairFundingDocumentV2Bytes(doc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newPairFrozenFundingSource(docJSON, sha256Hex(docJSON)); err != nil {
		t.Fatal(err)
	}
	batch, err := (pairFundingBatchV2{V: 2, SourceID: doc.SourceID, Seq: 1, PolicyDigest: digest, Rates: []pairFundingRate{}, Prices: []pairFundingPrice{}, Observations: []pairFundingOracleObservation{obs}, Coverage: []pairFundingCoverageRecord{}}).sealed()
	if err != nil {
		t.Fatal(err)
	}
	batchJSON, err := pairFundingBatchValueBytes(pairFundingBatchV2Value(batch))
	if err != nil {
		t.Fatal(err)
	}
	e := newPairFundingEnv(t, func(d *pairFundingDocument, k map[string]pairEvidenceKey) {
		d.Rates = []pairFundingRate{hlRate}
		d.Prices = []pairFundingPrice{pfPrice(hlKey, pairFundingPriceOracle, hlRate.EventMs, "59999.25")}
		pairFundingAutoCoverage(d, k)
	}, pairFundingConversions("1.0002", "0.9995"))
	e.openMatched(0.05)
	e.step(time.UnixMilli(hlRate.EventMs + 1000))
	eventID := pairFundingEventID(entryProviderHyperliquid, hlKey.Instrument, hlRate.EventMs)
	_, _, eventJSON := e.fundingRow(pairTestHL, eventID)
	legacyEvent, err := pairDecodeFundingEvent([]byte(eventJSON))
	if err != nil {
		t.Fatal(err)
	}
	researchEvent := pairFundingEventV3{pairFundingEvent: legacyEvent.pairFundingEvent, PolicyDigest: digest, ObservationEvidenceID: obs.EvidenceID, ObservationRecordHash: obs.RecordHash, ObservationTargetEventMs: obs.TargetEventMs, ObservationRequestMs: obs.RequestMs, ObservationReceiptMs: obs.ReceiptMs, ObservationTimeBasis: obs.TimeBasis}
	researchEvent.V = 3
	researchEvent.RuleVersion = pairFundingRuleHLReceiptResearch
	researchEvent.PriceHash = obs.RecordHash
	researchEvent, err = researchEvent.sealed()
	if err != nil {
		t.Fatal(err)
	}
	researchEventJSON, err := pairFundingEventValueBytes(pairFundingEventV3Value(researchEvent))
	if err != nil {
		t.Fatal(err)
	}
	if decoded, err := pairDecodeFundingEvent(researchEventJSON); err != nil || decoded.Research == nil || decoded.EvidenceHash != researchEvent.EvidenceHash {
		t.Fatalf("Go decodes its own sealed version 3 event strictly: %v", err)
	}
	_, legacyBinding, err := pairFundingBindingCanonical(e.adm.fundingBinding)
	if err != nil {
		t.Fatal(err)
	}
	rulesDigest, rules, err := pairFundingRulesDigestSelected(policy.MemberRules)
	if err != nil {
		t.Fatal(err)
	}
	bindingWire := pairFundingBindingV2{pairFundingBinding: legacyBinding.pairFundingBinding, Policy: policy, PolicyDigest: digest}
	bindingWire.V = 2
	bindingWire.RulesDigest = rulesDigest
	bindingWire.Rules = rules
	bindingJSON, err := pairCanonicalJSON(bindingWire)
	if err != nil {
		t.Fatal(err)
	}
	var obsMap map[string]interface{}
	obsRaw, _ := json.Marshal(obs)
	if err := json.Unmarshal(obsRaw, &obsMap); err != nil {
		t.Fatal(err)
	}
	cases := []map[string]interface{}{
		{"kind": "decode_document", "input": base64.StdEncoding.EncodeToString(docJSON)},
		{"kind": "decode_batch", "input": base64.StdEncoding.EncodeToString(batchJSON)},
		{"kind": "decode_binding", "input": base64.StdEncoding.EncodeToString(bindingJSON)},
		{"kind": "decode_event", "input": base64.StdEncoding.EncodeToString(researchEventJSON)},
		{"kind": "seal", "record": obsMap, "record_kind": "observation"},
	}
	res := pairFundingRunPy(t, cases)
	for i, want := range [][]byte{docJSON, batchJSON, bindingJSON} {
		if !res[i].OK || res[i].Hex != fmt.Sprintf("%x", want) {
			t.Fatalf("research codec case %d differs: %+v", i, res[i])
		}
	}
	if !res[3].OK || res[3].Hash != researchEvent.EvidenceHash || !res[4].OK || res[4].Hash != obs.RecordHash || batch.BatchHash != res[1].Hash {
		t.Fatalf("research hashes differ: %+v", res)
	}
	badVersion := pairFundingDocumentV2Value(doc)
	badVersion.Research.V = 99
	badVersionJSON, _ := pairCanonicalJSON(*badVersion.Research)
	var misplaced map[string]interface{}
	if err := json.Unmarshal(docJSON, &misplaced); err != nil {
		t.Fatal(err)
	}
	misplaced["prices"] = []interface{}{misplaced["observations"].([]interface{})[0]}
	misplacedJSON, _ := pairCanonicalJSON(misplaced)
	refusals := []struct {
		name string
		kind string
		raw  []byte
	}{
		{"unknown field", "document", bytes.Replace(docJSON, []byte(`"source_id"`), []byte(`"extra":1,"source_id"`), 1)},
		{"missing field", "document", bytes.Replace(docJSON, []byte(`"policy_digest":`+strconv.Quote(digest)+`,`), nil, 1)},
		{"duplicate field", "document", bytes.Replace(docJSON, []byte(`{"coverage"`), []byte(`{"v":2,"coverage"`), 1)},
		{"noncanonical decimal", "document", bytes.Replace(docJSON, []byte(`"59999.25"`), []byte(`"59999.250"`), 1)},
		{"unsupported version", "document", badVersionJSON},
		{"observation in prices", "document", misplacedJSON},
		{"unknown batch field", "batch", bytes.Replace(batchJSON, []byte(`"source_id"`), []byte(`"extra":1,"source_id"`), 1)},
		{"unknown binding field", "binding", bytes.Replace(bindingJSON, []byte(`"source_id"`), []byte(`"extra":1,"source_id"`), 1)},
		{"tampered event", "event", bytes.Replace(researchEventJSON, []byte(`"observation_request_ms":`+strconv.FormatInt(target, 10)), []byte(`"observation_request_ms":`+strconv.FormatInt(target-1, 10)), 1)},
	}
	for _, refusal := range refusals {
		t.Run(refusal.name, func(t *testing.T) {
			got := pairFundingRunPy(t, []map[string]interface{}{{"kind": "decode_" + refusal.kind, "input": base64.StdEncoding.EncodeToString(refusal.raw)}})
			if len(got) != 1 || got[0].OK {
				t.Fatalf("Python accepted %s: %+v", refusal.name, got)
			}
			var err error
			switch refusal.kind {
			case "document":
				_, err = pairDecodeFundingDocument(refusal.raw)
			case "batch":
				_, err = pairDecodeFundingBatch(refusal.raw)
			case "binding":
				_, _, err = pairFundingBindingCanonical(refusal.raw)
			case "event":
				_, err = pairDecodeFundingEvent(refusal.raw)
			}
			if err == nil {
				t.Fatalf("Go accepted %s", refusal.name)
			}
		})
	}
	for _, value := range []interface{}{pairFundingDocumentValue{}, pairFundingBatchValue{}, pairFundingEventValue{}, pairFundingBindingValue{}} {
		if _, err := json.Marshal(value); err == nil {
			t.Fatalf("envelope %T marshaled directly", value)
		}
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

type pairOracleReaderRun struct {
	res      pairRecResult
	clock    string
	callLog  string
	target   int64
	deadline int64
	key      pairEvidenceKey
	policy   pairFundingPolicyV1
}

func pairOracleReaderRunWith(t *testing.T, fx map[string]interface{}, startMs int64, ctxTimeout time.Duration) pairOracleReaderRun {
	t.Helper()
	root := basketPyRoot(t)
	t.Chdir(root)
	dir := t.TempDir()
	target := (startMs/3600000 + 1) * 3600000
	policy := pairFundingPolicyV1{Version: pairFundingPolicyVersion, MemberRules: map[string]string{pairTestHL: pairFundingRuleHLReceiptResearch, pairTestOKX: pairFundingRuleOKX},
		WindowMs: 5000, MaxPrintOffsetMs: 60000, PrepareLeadMs: 30000, FromMs: target - 120000, DrainThroughMs: target + 180000}
	digest, err := pairFundingPolicyDigest(policy)
	if err != nil {
		t.Fatal(err)
	}
	deadline, err := pairFundingOracleDeadline(policy, target)
	if err != nil {
		t.Fatal(err)
	}
	key := pairEvidenceKey{Partition: "paper", Provider: entryProviderHyperliquid, AccountKey: "synthetic:hl-research", Instrument: "BTC", MemberID: pairTestHL}
	args, err := pairPaperOracleArgs(key, policy, target, deadline, digest)
	if err != nil {
		t.Fatal(err)
	}
	run := pairOracleReaderRun{target: target, deadline: deadline, key: key, policy: policy, callLog: filepath.Join(dir, "calls.jsonl")}
	fixture := filepath.Join(dir, "fixture.json")
	blob, _ := json.Marshal(fx)
	if err := os.WriteFile(fixture, blob, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PYTHONPATH", filepath.Join(root, "scheduler", "testdata", "pair_paper"))
	t.Setenv("PAIR_PAPER_FIXTURE", fixture)
	t.Setenv("PAIR_PAPER_CALL_LOG", run.callLog)
	t.Setenv("HYPERLIQUID_SECRET_KEY", "0x"+strings.Repeat("1", 64))
	t.Setenv("OKX_API_KEY", "fixture-key")
	prev := pairPaperWall
	t.Cleanup(func() { pairPaperWall = prev })
	if ctxTimeout == 0 {
		run.clock = filepath.Join(dir, "clock")
		if err := os.WriteFile(run.clock, []byte(strconv.FormatInt(startMs, 10)), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv(pairPaperTestClockEnv, run.clock)
		pairPaperWall = &pairPaperTestWall{path: run.clock, ms: startMs}
		ctxTimeout = time.Minute
	} else {
		t.Setenv(pairPaperTestClockEnv, "")
	}
	ctx, cancel := context.WithTimeout(context.Background(), ctxTimeout)
	defer cancel()
	run.res = pairRunHistoryReader(ctx, nil, args)
	return run
}

func (r pairOracleReaderRun) metaCalls(t *testing.T) (int, []string) {
	t.Helper()
	b, _ := os.ReadFile(r.callLog)
	n := 0
	var other []string
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line == "" {
			continue
		}
		var row []interface{}
		if err := json.Unmarshal([]byte(line), &row); err != nil || len(row) < 2 {
			t.Fatalf("bad call log line %q", line)
		}
		switch {
		case row[0] == "hl" && row[1] == "metaAndAssetCtxs":
			n++
		case row[0] == "hl" && (row[1] == "meta" || row[1] == "spotMeta"):
		default:
			other = append(other, line)
		}
	}
	sort.Strings(other)
	return n, other
}

func TestBasketPairFundingPythonPublicOracleReader(t *testing.T) {
	start := pairTestStart().Add(time.Hour - 30*time.Second).UnixMilli()
	t.Run("valid sample at the hour", func(t *testing.T) {
		r := pairOracleReaderRunWith(t, map[string]interface{}{"oracle_px": "60123.50", "oracle_response_delay_ms": 250}, start, 0)
		if r.res.Class != pairRecClassResult {
			t.Fatalf("the public reader returns its sample: %+v", r.res)
		}
		o, err := pairPaperCheckOracle(r.res.Stdout, r.key, r.policy, r.target, r.deadline, r.res.ReceiptMs)
		if err != nil {
			t.Fatalf("the sample passes every echo, identity and time check: %v", err)
		}
		if o.RequestMs != r.target || o.ReceiptMs != r.target+250 || o.Price != "60123.5" || o.AtMs != o.ReceiptMs || o.VisibleMs != o.ReceiptMs {
			t.Fatalf("the reader waits for the hour, then takes request and receipt around the one read: %+v", o)
		}
		if n, other := r.metaCalls(t); n != 1 || len(other) != 0 {
			t.Fatalf("one public metaAndAssetCtxs read and no other venue call, got %d and %v", n, other)
		}
	})
	t.Run("receipt exactly at the deadline", func(t *testing.T) {
		r := pairOracleReaderRunWith(t, map[string]interface{}{"oracle_response_delay_ms": 5000}, start, 0)
		o, err := pairPaperCheckOracle(r.res.Stdout, r.key, r.policy, r.target, r.deadline, r.res.ReceiptMs)
		if r.res.Class != pairRecClassResult || err != nil || o.ReceiptMs != r.deadline {
			t.Fatalf("a receipt at the closed window end is in the window: %+v %v", r.res, err)
		}
	})
	refused := map[string]map[string]interface{}{
		"receipt after the deadline": {"oracle_response_delay_ms": 5001},
		"missing symbol":             {"oracle_context_mode": "missing_symbol"},
		"duplicate symbol":           {"oracle_context_mode": "duplicate_symbol"},
		"short context":              {"oracle_context_mode": "short_context"},
		"missing oracle":             {"oracle_context_mode": "missing_oracle"},
		"zero oracle":                {"oracle_context_mode": "nonpositive_oracle"},
		"negative oracle":            {"oracle_context_mode": "negative_oracle"},
		"malformed oracle":           {"oracle_context_mode": "malformed_oracle"},
		"float oracle":               {"oracle_context_mode": "float_oracle"},
		"bool oracle":                {"oracle_context_mode": "bool_oracle"},
		"nan oracle":                 {"oracle_context_mode": "nan_oracle"},
		"infinite oracle":            {"oracle_context_mode": "infinite_oracle"},
		"padded oracle":              {"oracle_context_mode": "padded_oracle"},
		"testnet host":               {"oracle_context_mode": "wrong_host"},
		"rewritten window":           {"oracle_context_mode": "oracle_envelope_override"},
		"oversize output":            {"oracle_context_mode": "oversize_output"},
	}
	names := make([]string, 0, len(refused))
	for name := range refused {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fx := refused[name]
		t.Run(name, func(t *testing.T) {
			r := pairOracleReaderRunWith(t, fx, start, 0)
			if r.res.Class == pairRecClassResult {
				if _, err := pairPaperCheckOracle(r.res.Stdout, r.key, r.policy, r.target, r.deadline, r.res.ReceiptMs); err == nil {
					t.Fatalf("%s must be refused by the reader or by the Go checks", name)
				}
			}
			if name == "rewritten window" {
				var env pairPaperOracleResponse
				if err := pairReplayStrictDecode(bytes.TrimSpace(r.res.Stdout), &env); err != nil {
					t.Fatalf("the rewritten envelope still decodes: %v", err)
				}
				if _, err := pairPaperCheckOracle(r.res.Stdout, r.key, r.policy, r.target, r.deadline, env.ReceiptMs); err == nil || !strings.Contains(err.Error(), "outside") {
					t.Fatalf("Go refuses request and receipt times outside the sample window on its own checks, got %v", err)
				}
			}
			if name == "oversize output" && !r.res.Overflow {
				t.Fatalf("an output over the reader limit is bounded and refused: %+v", r.res.Class)
			}
		})
	}
	t.Run("credentials and testnet are refused when set", func(t *testing.T) {
		root := basketPyRoot(t)
		args := append([]string{}, pairHistoryHLOracleProbeArgv[:len(pairHistoryHLOracleProbeArgv)-1]...)
		for _, env := range []string{"HYPERLIQUID_SECRET_KEY=0xabc", "HYPERLIQUID_TESTNET=1", "OKX_API_KEY=k"} {
			cmd := exec.Command(filepath.Join(root, ".venv", "bin", "python3"), append([]string{filepath.Join(root, pairHistoryReaderScript)}, args...)...)
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "PYTHONPATH="+filepath.Join(root, "scheduler", "testdata", "pair_paper"), "PAIR_PAPER_BLOCK_NETWORK=1", env)
			out, err := cmd.Output()
			if err == nil || !strings.Contains(string(out), "requires these variables to be empty") {
				t.Fatalf("%s must refuse the public reader before any read, got %v %s", env, err, out)
			}
		}
	})
	t.Run("cancellation stops the wait", func(t *testing.T) {
		now := time.Now().UnixMilli()
		r := pairOracleReaderRunWith(t, map[string]interface{}{}, now, 1500*time.Millisecond)
		if r.res.Class != pairRecClassCancelled {
			t.Fatalf("a cancelled oracle wait is recorded as cancelled, got %+v", r.res)
		}
		if n, _ := r.metaCalls(t); n != 0 {
			t.Fatalf("the cancelled reader never read the venue, got %d reads", n)
		}
	})
}
