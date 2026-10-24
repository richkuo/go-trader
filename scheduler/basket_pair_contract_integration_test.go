package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type pairMutation = func(root map[string]interface{})

func pairSetMeta(i int, field string, v interface{}) pairMutation {
	return func(r map[string]interface{}) { pairTestMemberEntry(r, i)["meta"].(map[string]interface{})[field] = v }
}

func pairDelMeta(i int, field string) pairMutation {
	return func(r map[string]interface{}) {
		delete(pairTestMemberEntry(r, i)["meta"].(map[string]interface{}), field)
	}
}

func pairSetMember(i int, field string, v interface{}) pairMutation {
	return func(r map[string]interface{}) { pairTestMemberEntry(r, i)[field] = v }
}

func pairSetLimit(field string, v interface{}) pairMutation {
	return func(r map[string]interface{}) { pairTestPair(r)["limits"].(map[string]interface{})[field] = v }
}

func pairSetBook(field string, v interface{}) pairMutation {
	return func(r map[string]interface{}) { pairTestPair(r)["book_need"].(map[string]interface{})[field] = v }
}

func pairSetCost(i int, field string, v interface{}) pairMutation {
	return func(r map[string]interface{}) {
		pairTestMemberEntry(r, i)["cost_policy"].(map[string]interface{})[field] = v
	}
}

func pairAll(fs ...pairMutation) pairMutation {
	return func(r map[string]interface{}) {
		for _, f := range fs {
			f(r)
		}
	}
}

func pairAdmitAccepted(t *testing.T, mutate pairMutation) (*pairResearchAdmission, *Config, basketContract) {
	t.Helper()
	registerPairTestPolicy(t)
	dir := filepath.Join(t.TempDir(), "run")
	doc := pairTestDoc(t, dir, mutate)
	adm, cfg, err := newPairResearchAdmission(doc, "run-1", dir, false, pairTestDeps(t, doc, newPairTestVenues(false)))
	if err != nil {
		t.Fatalf("the research admission refused a valid contract: %v", err)
	}
	t.Cleanup(adm.release)
	c, err := buildPairContract(cfg, adm.pair, adm.limits)
	if err != nil {
		t.Fatal(err)
	}
	return adm, cfg, c
}

func pairAdmitBytesRefused(t *testing.T, doc []byte, deps pairResearchDeps, want string) {
	t.Helper()
	registerPairTestPolicy(t)
	dir := filepath.Join(t.TempDir(), "run")
	_, _, err := newPairResearchAdmission(doc, "run-1", dir, false, deps)
	if err == nil {
		t.Fatalf("the research admission accepted input that must be refused (%s)", want)
	}
	if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
		t.Fatalf("a refused admission wrote state into %s (%v)", dir, statErr)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("the refusal must name %q, got %v", want, err)
	}
}

func TestBasketPairContractNewFieldsAtTheirLimits(t *testing.T) {
	const maxMs = int64(2147483647)
	accepted := []struct {
		name   string
		mutate pairMutation
	}{
		{"book depth 1", pairSetBook("depth", 1)},
		{"book depth 20", pairAll(pairSetBook("depth", 20), pairSetLimit("envelope_max_bytes", 4194304))},
		{"book window 1", pairSetBook("window_ms", 1)},
		{"book window 900000", pairSetBook("window_ms", 900000)},
		{"book capacity 1", pairSetBook("capacity", 1)},
		{"book capacity 4096", pairAll(pairSetBook("capacity", 4096), pairSetMember(0, "source", "public_snapshot"))},
		{"archive delivery 1", pairAll(pairSetMember(0, "source", "archive"), pairSetMember(0, "modeled_delivery_ms", 1))},
		{"archive delivery at the maximum", pairAll(pairSetMember(0, "source", "archive"), pairSetMember(0, "modeled_delivery_ms", maxMs))},
		{"hyperliquid public snapshot", pairSetMember(0, "source", "public_snapshot")},
		{"zero cost rates", pairAll(pairSetCost(0, "taker_fee_pct", 0), pairSetCost(0, "slippage_pct", 0))},
		{"cost rates at the maximum", pairAll(pairSetCost(1, "taker_fee_pct", 0.05), pairSetCost(1, "slippage_pct", 0.05))},
		{"quote age 1 with timeout and skew 1", pairAll(pairSetLimit("quote_age_ms", 1), pairSetLimit("collection_timeout_ms", 1), pairSetLimit("receive_skew_ms", 1))},
		{"quote age at the maximum", pairSetLimit("quote_age_ms", maxMs)},
		{"timeout and skew equal to the quote age", pairAll(pairSetLimit("collection_timeout_ms", 5000), pairSetLimit("receive_skew_ms", 5000))},
		{"envelope at the maximum", pairSetLimit("envelope_max_bytes", 4194304)},
		{"protective width just above zero", pairSetLimit("protective_width_bps", 1e-9)},
		{"protective width just below 10000", pairSetLimit("protective_width_bps", 9999.999999)},
		{"hyperliquid size decimals 0", pairAll(pairSetMeta(0, "sz_decimals", 0), pairSetMeta(0, "lot_step", 1), pairSetMeta(0, "min_qty", 1), pairSetMeta(0, "tick_step", 0.000001))},
		{"hyperliquid size decimals 12", pairAll(pairSetMeta(0, "sz_decimals", 12), pairSetMeta(0, "lot_step", 1e-12), pairSetMeta(0, "min_qty", 1e-12), pairSetMeta(0, "tick_step", 1))},
		{"okx step text with a redundant zero", pairSetMeta(1, "tick_step_text", "0.10")},
	}
	for _, tc := range accepted {
		t.Run(tc.name, func(t *testing.T) { pairAdmitAccepted(t, tc.mutate) })
	}
	refused := []struct {
		name   string
		mutate pairMutation
		want   string
	}{
		{"book depth 0", pairSetBook("depth", 0), "book depth 0 levels is outside [1, 20]"},
		{"book depth 21", pairSetBook("depth", 21), "book depth 21 levels is outside [1, 20]"},
		{"book window 0", pairSetBook("window_ms", 0), "book window 0ms is outside (0, 900000]"},
		{"book window 900001", pairSetBook("window_ms", 900001), "book window 900001ms is outside (0, 900000]"},
		{"book capacity 0", pairSetBook("capacity", 0), "book capacity 0 observations"},
		{"book capacity 4097", pairSetBook("capacity", 4097), "book capacity 4097 observations"},
		{"archive delivery 0", pairSetMember(0, "source", "archive"), "modeled_delivery_ms must be in [1, 2147483647] for the archive source"},
		{"archive delivery above the maximum", pairAll(pairSetMember(0, "source", "archive"), pairSetMember(0, "modeled_delivery_ms", maxMs+1)), "modeled_delivery_ms must be in [1, 2147483647]"},
		{"sealed feed with a delivery", pairSetMember(0, "modeled_delivery_ms", 1), `modeled_delivery_ms must be explicit 0 for the "sealed_feed" source`},
		{"okx sealed feed", pairSetMember(1, "source", "sealed_feed"), `source "sealed_feed" is not supported for a okx member`},
		{"okx archive", pairSetMember(1, "source", "archive"), `source "archive" is not supported for a okx member`},
		{"unknown source", pairSetMember(0, "source", "websocket"), `source "websocket" is not supported`},
		{"taker fee above the maximum", pairSetCost(0, "taker_fee_pct", 0.0500001), "cost policy taker_fee_pct"},
		{"negative slippage", pairSetCost(1, "slippage_pct", -0.0001), "cost policy slippage_pct"},
		{"maker fill role", pairSetCost(0, "fill_role", "maker"), "cost policy fill_role"},
		{"separate spread", pairSetCost(0, "spread", "separate"), "cost policy spread"},
		{"zero model version key", pairSetCost(0, "model_version", 0), `cost_policy: unknown field "model_version"`},
		{"empty policy id key", pairSetCost(1, "policy_id", ""), `cost_policy: unknown field "policy_id"`},
		{"quote age 0", pairSetLimit("quote_age_ms", 0), "quote_age_ms must be in [1, 2147483647]"},
		{"quote age above the maximum", pairSetLimit("quote_age_ms", maxMs+1), "quote_age_ms must be in [1, 2147483647]"},
		{"timeout above the quote age", pairSetLimit("collection_timeout_ms", 5001), "collection_timeout_ms 5001 exceeds quote_age_ms 5000"},
		{"skew above the quote age", pairSetLimit("receive_skew_ms", 5001), "receive_skew_ms 5001 exceeds quote_age_ms 5000"},
		{"envelope 0", pairSetLimit("envelope_max_bytes", 0), "envelope_max_bytes must be in [1, 4194304]"},
		{"envelope 1 byte", pairSetLimit("envelope_max_bytes", 1), "is below the"},
		{"sealed capacity 4096", pairAll(pairSetBook("capacity", 4096), pairSetLimit("envelope_max_bytes", 4194304)), "worst-case quote envelope"},
		{"envelope above the maximum", pairSetLimit("envelope_max_bytes", 4194305), "envelope_max_bytes must be in [1, 4194304]"},
		{"protective width 0", pairSetLimit("protective_width_bps", 0), "protective_width_bps must be finite, above 0 and below 10000"},
		{"protective width 10000", pairSetLimit("protective_width_bps", 10000), "protective_width_bps must be finite, above 0 and below 10000"},
		{"hyperliquid size decimals 13", pairSetMeta(0, "sz_decimals", 13), "meta.sz_decimals must be an integer in [0, 12]"},
		{"hyperliquid size decimals -1", pairSetMeta(0, "sz_decimals", -1), "meta.sz_decimals must be an integer in [0, 12]"},
		{"okx size decimals 1", pairSetMeta(1, "sz_decimals", 1), "meta.sz_decimals must be explicit 0"},
		{"okx tick text differs", pairSetMeta(1, "tick_step_text", "0.2"), "meta.tick_step_text \"0.2\" differs from meta.tick_step 0.1"},
		{"okx lot text differs", pairSetMeta(1, "lot_step_text", "2"), "meta.lot_step_text \"2\" differs from meta.lot_step 1"},
		{"okx exponent step text", pairSetMeta(1, "tick_step_text", "1e-1"), "meta.tick_step_text must be a positive plain decimal string"},
		{"okx empty step text", pairSetMeta(1, "lot_step_text", ""), "meta.lot_step_text must be a positive plain decimal string"},
		{"hyperliquid step text", pairSetMeta(0, "tick_step_text", "0.001"), "OKX-only"},
		{"no fee currency", pairSetMeta(0, "fee_currencies", []interface{}{}), "meta.fee_currencies must list at least one currency"},
		{"duplicate fee currency", pairSetMeta(1, "fee_currencies", []interface{}{"USDT", "USDT"}), `meta.fee_currencies lists "USDT" twice`},
		{"lower-case fee currency", pairSetMeta(1, "fee_currencies", []interface{}{"usdt"}), "must be an upper-case currency code"},
		{"hyperliquid fixed tick rule", pairSetMeta(0, "price_rule", "fixed_tick_v1"), `meta.price_rule must be "hyperliquid_perps_v1"`},
		{"okx hyperliquid rule", pairSetMeta(1, "price_rule", "hyperliquid_perps_v1"), `meta.price_rule must be "fixed_tick_v1"`},
		{"okx BTC-USDC-SWAP", pairAll(pairSetMember(1, "instrument", "BTC-USDC-SWAP"), pairSetMeta(1, "quote_currency", "USDC"), pairSetMeta(1, "settlement_currency", "USDC"), pairSetMeta(1, "price_currency", "USDC"), pairSetMeta(1, "fee_currencies", []interface{}{"USDC"})), `meta.settlement_currency must be "USDT"`},
		{"okx USDC quote only", pairSetMeta(1, "quote_currency", "USDC"), `meta.quote_currency must be "USDT"`},
		{"okx USDC settlement only", pairSetMeta(1, "settlement_currency", "USDC"), `meta.settlement_currency must be "USDT"`},
		{"okx USDC price only", pairSetMeta(1, "price_currency", "USDC"), `meta.price_currency must be "USDT"`},
		{"hyperliquid USD quote", pairSetMeta(0, "quote_currency", "USD"), `meta.quote_currency must be "USDC"`},
		{"hyperliquid portfolio margin", pairSetMeta(0, "margin_mode", "portfolio"), `meta.margin_mode must be "cross" or "isolated"`},
		{"hyperliquid tick off the derivation", pairSetMeta(0, "tick_step", 1), `tick_step is "1", the derivation requires "0.001"`},
		{"hyperliquid minimum notional off the derivation", pairSetMeta(0, "min_notional", 15), `min_notional is "15", the derivation requires "10"`},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) { pairAdmitRefused(t, tc.mutate, tc.want) })
	}
}

func TestBasketPairContractRawBytesAreRefusedBeforeEvidence(t *testing.T) {
	registerPairTestPolicy(t)
	base := pairTestDoc(t, filepath.Join(t.TempDir(), "run"), nil)
	raw := func(path func(r map[string]interface{}) map[string]interface{}, field, text string) []byte {
		return pairTestDoc(t, filepath.Join(t.TempDir(), "run"), func(r map[string]interface{}) { path(r)[field] = json.RawMessage(text) })
	}
	edit := func(old, new string) []byte {
		if !bytes.Contains(base, []byte(old)) {
			t.Fatalf("fixture text %q is missing", old)
		}
		return bytes.Replace(base, []byte(old), []byte(new), 1)
	}
	hlMeta := func(r map[string]interface{}) map[string]interface{} {
		return pairTestMemberEntry(r, 0)["meta"].(map[string]interface{})
	}
	okxMeta := func(r map[string]interface{}) map[string]interface{} {
		return pairTestMemberEntry(r, 1)["meta"].(map[string]interface{})
	}
	member := func(r map[string]interface{}) map[string]interface{} { return pairTestMemberEntry(r, 0) }
	limits := func(r map[string]interface{}) map[string]interface{} {
		return pairTestPair(r)["limits"].(map[string]interface{})
	}
	book := func(r map[string]interface{}) map[string]interface{} {
		return pairTestPair(r)["book_need"].(map[string]interface{})
	}
	pair := func(r map[string]interface{}) map[string]interface{} { return pairTestPair(r) }
	cases := []struct {
		name string
		doc  []byte
		want string
	}{
		{"duplicate key", edit(`"underlying": "BTC"`, `"underlying": "BTC", "underlying": "BTC"`), `duplicate object key "underlying"`},
		{"escaped duplicate key", edit(`"underlying": "BTC"`, `"underlying": "BTC", "under\u006cying": "BTC"`), `duplicate object key "underlying"`},
		{"invalid UTF-8", edit(`"synthetic:hl-research"`, "\"synthetic:hl-research\xff\""), "JSON is not valid UTF-8"},
		{"lone high surrogate", edit(`"BTC-USDT-SWAP"`, `"BTC-USDT-SWAP\ud800"`), "lone high surrogate escape"},
		{"lone low surrogate", edit(`"BTC-USDT-SWAP"`, `"BTC-USDT-SWAP\udc00"`), "lone low surrogate escape"},
		{"trailing value", append(append([]byte{}, base...), []byte(" {}")...), "trailing value"},
		{"string linear", raw(hlMeta, "linear", `"true"`), "linear must be a JSON boolean, got string"},
		{"numeric linear", raw(okxMeta, "linear", `1`), "linear must be a JSON boolean, got number"},
		{"fractional size decimals", raw(hlMeta, "sz_decimals", `3.0`), "sz_decimals must be a JSON integer (no fraction or exponent)"},
		{"exponent size decimals", raw(hlMeta, "sz_decimals", `3e0`), "sz_decimals must be a JSON integer (no fraction or exponent)"},
		{"string size decimals", raw(okxMeta, "sz_decimals", `"0"`), "sz_decimals must be a JSON integer, got string"},
		{"string contract size", raw(hlMeta, "contract_size", `"1"`), "contract_size must be a JSON number, got string"},
		{"boolean contract size", raw(okxMeta, "contract_size", `true`), "contract_size must be a JSON number, got boolean"},
		{"numeric step text", raw(okxMeta, "tick_step_text", `0.1`), "tick_step_text must be a JSON string, got number"},
		{"string fee currencies", raw(hlMeta, "fee_currencies", `"USDC"`), "fee_currencies must be a JSON array of strings, got string"},
		{"numeric fee currency", raw(okxMeta, "fee_currencies", `[1]`), "fee_currencies [0] must be a JSON string, got number"},
		{"fractional delivery", raw(member, "modeled_delivery_ms", `1.5`), "modeled_delivery_ms must be a JSON integer (no fraction or exponent)"},
		{"null book depth", raw(book, "depth", `null`), "depth must be a JSON integer, got null"},
		{"null price currency", raw(hlMeta, "price_currency", `null`), "price_currency must be a JSON string, got null"},
		{"fractional envelope", raw(limits, "envelope_max_bytes", `1.0`), "envelope_max_bytes must be an integer number of bytes"},
		{"string protective width", raw(limits, "protective_width_bps", `"50"`), "protective_width_bps must be a JSON number, got string"},
		{"string member", raw(pair, "members", `["pair-hl-btc", {}]`), "pairs[0].members[0]: must be a JSON object, got string"},
		{"array meta", raw(member, "meta", `[]`), "meta must be a JSON object, got array"},
		{"missing price rule", pairTestDoc(t, filepath.Join(t.TempDir(), "run"), pairDelMeta(0, "price_rule")), "price_rule is missing"},
		{"missing book need", pairTestDoc(t, filepath.Join(t.TempDir(), "run"), func(r map[string]interface{}) { delete(pairTestPair(r), "book_need") }), "book_need is missing"},
		{"missing cost policy", pairTestDoc(t, filepath.Join(t.TempDir(), "run"), func(r map[string]interface{}) { delete(pairTestMemberEntry(r, 1), "cost_policy") }), "cost_policy is missing"},
		{"nested unknown cost key", pairTestDoc(t, filepath.Join(t.TempDir(), "run"), pairSetCost(0, "maker_fee_pct", 0.0001)), `cost_policy: unknown field "maker_fee_pct"`},
		{"nested unknown book key", pairTestDoc(t, filepath.Join(t.TempDir(), "run"), pairSetBook("levels", 5)), `book_need: unknown field "levels"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := pairTestEvidence(t, base, nil)
			pairAdmitBytesRefused(t, tc.doc, pairResearchDeps{VenueFor: newPairTestVenues(false).factory(), Evidence: src}, tc.want)
			if src.bound {
				t.Fatal("the evidence source was read before the raw contract was refused")
			}
		})
	}
	t.Run("live-shaped fixture uses the same strict scan", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "fixture")
		doc := pairTestDoc(t, dir, pairLiveShapedMutate)
		dup := bytes.Replace(doc, []byte(`"underlying": "BTC"`), []byte(`"underlying": "BTC", "underlying": "ETH"`), 1)
		venues := newPairTestVenues(true)
		_, _, err := newPairLiveShapedAdmission(t, dup, dir, pairLiveShapedDeps{
			VenueFor:     venues.factory(),
			HLCloser:     func(string, *float64, []int64) (*HyperliquidCloseResult, error) { return nil, nil },
			OKXCloser:    func(string, *float64) (*OKXCloseResult, error) { return nil, nil },
			HLFetcher:    func(string) ([]HLPosition, error) { return nil, nil },
			OKXFetcher:   func() ([]OKXPosition, error) { return nil, nil },
			EntryQueries: func(*entryIntent) error { return nil },
		})
		if err == nil {
			t.Fatal("the live-shaped fixture admitted a duplicate-key contract")
		}
		if !strings.Contains(err.Error(), "duplicate object key") || pairFileDigest(t, filepath.Join(dir, pairResearchManifestName)) != "missing" {
			t.Fatalf("the live-shaped refusal must name the duplicate key and write nothing: %v", err)
		}
	})
}

type pairOpenEvidenceSource struct{}

func (pairOpenEvidenceSource) Capability(context.Context, pairContractMember, int64) (pairCapabilityEvidence, error) {
	return pairCapabilityEvidence{}, nil
}
func (pairOpenEvidenceSource) Conversions(context.Context, pairContractMember, int64) ([]pairConversionEvidence, error) {
	return nil, nil
}
func (pairOpenEvidenceSource) Account(context.Context, pairContractMember, int64) (pairAccountEvidence, error) {
	return pairAccountEvidence{}, nil
}

func pairFindCap(d *pairEvidenceDocument, member string) *pairCapabilityEvidence {
	for i := range d.Capabilities {
		if d.Capabilities[i].Key.MemberID == member {
			return &d.Capabilities[i]
		}
	}
	return nil
}

func pairFindAcct(d *pairEvidenceDocument, member string) *pairAccountEvidence {
	for i := range d.Accounts {
		if d.Accounts[i].Key.MemberID == member {
			return &d.Accounts[i]
		}
	}
	return nil
}

func TestBasketPairContractEvidenceAdmissionRefusals(t *testing.T) {
	registerPairTestPolicy(t)
	doc := pairTestDoc(t, filepath.Join(t.TempDir(), "run"), nil)
	venues := newPairTestVenues(false)
	t.Run("missing source", func(t *testing.T) {
		pairAdmitBytesRefused(t, doc, pairResearchDeps{VenueFor: venues.factory()}, "immutable frozen evidence source")
	})
	t.Run("mutable source", func(t *testing.T) {
		pairAdmitBytesRefused(t, doc, pairResearchDeps{VenueFor: venues.factory(), Evidence: pairOpenEvidenceSource{}}, "immutable frozen evidence source")
	})
	mutations := []struct {
		name   string
		mutate func(d *pairEvidenceDocument)
		want   string
	}{
		{"missing capability schedule", func(d *pairEvidenceDocument) { d.Capabilities = d.Capabilities[1:] }, "no capability schedule for member pair-hl-btc"},
		{"missing account schedule", func(d *pairEvidenceDocument) { d.Accounts = d.Accounts[:1] }, "no account schedule for member pair-okx-btc"},
		{"missing conversion schedule", func(d *pairEvidenceDocument) {
			var kept []pairConversionEvidence
			for _, c := range d.Conversions {
				if c.Currency != "USDT" {
					kept = append(kept, c)
				}
			}
			d.Conversions = kept
		}, "no USDT-to-USD conversion schedule for member pair-okx-btc"},
		{"different metadata", func(d *pairEvidenceDocument) { pairFindCap(d, pairTestOKX).Meta.MaxLeverage = "25" }, "meta.max_leverage \"25\" differs from the frozen contract \"20\""},
		{"non-USDT okx capability", func(d *pairEvidenceDocument) {
			c := pairFindCap(d, pairTestOKX)
			c.Meta.SettlementCurrency, c.Meta.FeeCurrencies = "USDC", []string{"USDC"}
		}, `settlement_currency is "USDC", the derivation requires "USDT"`},
		{"position mode mismatch", func(d *pairEvidenceDocument) { pairFindAcct(d, pairTestHL).PositionMode = "net" }, `position_mode "net" differs from the required "one_way"`},
		{"leverage mismatch", func(d *pairEvidenceDocument) { pairFindAcct(d, pairTestOKX).Leverage = "3" }, "leverage 3 differs from the frozen selected leverage 2"},
		{"collateral mismatch", func(d *pairEvidenceDocument) { pairFindAcct(d, pairTestHL).CollateralCurrency = "USDT" }, `collateral_currency "USDT" differs from the settlement currency "USDC"`},
		{"foreign partition", func(d *pairEvidenceDocument) { pairFindCap(d, pairTestHL).Key.Partition = "paper:other" }, "is outside the pair partition paper"},
		{"foreign account", func(d *pairEvidenceDocument) { pairFindAcct(d, pairTestOKX).Key.AccountKey = "synthetic:other" }, "does not name member pair-okx-btc"},
		{"unknown member", func(d *pairEvidenceDocument) { pairFindCap(d, pairTestHL).Key.MemberID = "pair-ghost" }, "names no pair member"},
		{"USD conversion record", func(d *pairEvidenceDocument) {
			c := d.Conversions[0]
			c.Currency, c.EvidenceID = "USD", "conv-usd"
			d.Conversions = append(d.Conversions, c)
		}, "other than USD"},
		{"unrelated currency", func(d *pairEvidenceDocument) {
			c := d.Conversions[0]
			c.Currency, c.EvidenceID = "EUR", "conv-eur"
			d.Conversions = append(d.Conversions, c)
		}, "is not a settlement, price or fee currency"},
		{"unknown derivation", func(d *pairEvidenceDocument) { pairFindCap(d, pairTestHL).Provenance = "manual" }, `provenance "manual" is not the hyperliquid derivation`},
		{"observed after receipt", func(d *pairEvidenceDocument) { pairFindAcct(d, pairTestHL).ObservedMs = 1_700_000_000_600 }, "after its first receipt"},
		{"inverted validity", func(d *pairEvidenceDocument) { pairFindAcct(d, pairTestOKX).ValidThroughMs = 1_700_000_000_000 }, "invalid validity interval"},
		{"zero generation", func(d *pairEvidenceDocument) { pairFindCap(d, pairTestOKX).MetadataGeneration = 0 }, "metadata_generation must be at least 1"},
	}
	for _, tc := range mutations {
		t.Run(tc.name, func(t *testing.T) {
			blob := pairTestEvidenceBytes(t, doc, tc.mutate)
			src, err := newPairFrozenEvidenceSource(blob, sha256Hex(blob))
			if err != nil {
				if !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("the evidence refusal must name %q, got %v", tc.want, err)
				}
				return
			}
			pairAdmitBytesRefused(t, doc, pairResearchDeps{VenueFor: venues.factory(), Evidence: src}, tc.want)
		})
	}
	valid := pairTestEvidenceBytes(t, doc, nil)
	byteCases := []struct {
		name   string
		blob   []byte
		digest string
		want   string
	}{
		{"digest mismatch", valid, strings.Repeat("0", 64), "is not the expected"},
		{"hash-invalid record", bytes.Replace(valid, []byte(`"equity":"6000"`), []byte(`"equity":"6001"`), 1), "", "evidence_hash does not match its canonical bytes"},
		{"noncanonical document", func() []byte {
			var out bytes.Buffer
			_ = json.Indent(&out, valid, "", " ")
			return out.Bytes()
		}(), "", "not canonical"},
		{"unsupported version", bytes.Replace(valid, []byte(`"v":2}`), []byte(`"v":3}`), 1), "", "evidence document version 3 is not supported"},
	}
	for _, tc := range byteCases {
		t.Run(tc.name, func(t *testing.T) {
			digest := tc.digest
			if digest == "" {
				digest = sha256Hex(tc.blob)
			}
			_, err := newPairFrozenEvidenceSource(tc.blob, digest)
			if err == nil {
				t.Fatal("the frozen evidence source accepted a document that must be refused")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("the refusal must name %q, got %v", tc.want, err)
			}
		})
	}
}

func pairRestamp(s *pairEvidenceStamp, id string, observed, receipt, from, through int64) {
	s.EvidenceID, s.ObservedMs, s.FirstReceiptMs, s.ValidFromMs, s.ValidThroughMs = id, observed, receipt, from, through
}

func TestBasketPairContractEvidenceSelectionAtExplicitReference(t *testing.T) {
	registerPairTestPolicy(t)
	dir := filepath.Join(t.TempDir(), "run")
	doc := pairTestDoc(t, dir, nil)
	blob := pairTestEvidenceBytes(t, doc, func(d *pairEvidenceDocument) {
		for i := range d.Capabilities {
			pairRestamp(&d.Capabilities[i].pairEvidenceStamp, d.Capabilities[i].EvidenceID, 500, 500, 500, 20000)
		}
		for i := range d.Accounts {
			pairRestamp(&d.Accounts[i].pairEvidenceStamp, d.Accounts[i].EvidenceID, 500, 500, 500, 20000)
		}
		for i := range d.Conversions {
			pairRestamp(&d.Conversions[i].pairEvidenceStamp, d.Conversions[i].EvidenceID, 500, 500, 500, 20000)
		}
		hl := *pairFindCap(d, pairTestHL)
		d.Capabilities = []pairCapabilityEvidence{*pairFindCap(d, pairTestOKX)}
		a, b, c := hl, hl, hl
		a.Meta, b.Meta, c.Meta = hl.Meta.clone(), hl.Meta.clone(), hl.Meta.clone()
		pairRestamp(&a.pairEvidenceStamp, "cap-a", 900, 1000, 1000, 5000)
		pairRestamp(&b.pairEvidenceStamp, "cap-b", 2500, 3000, 3000, 9000)
		pairRestamp(&c.pairEvidenceStamp, "cap-c", 2500, 3000, 3000, 3500)
		a.MetadataGeneration, b.MetadataGeneration, c.MetadataGeneration = 1, 2, 3
		d.Capabilities = append(d.Capabilities, a, b, c)
		okx := *pairFindAcct(d, pairTestOKX)
		same, other := okx, okx
		pairRestamp(&same.pairEvidenceStamp, "acct-okx-same", 1500, 2000, 2000, 20000)
		okx2 := okx
		pairRestamp(&okx2.pairEvidenceStamp, "acct-okx-same-2", 1500, 2000, 2000, 20000)
		pairRestamp(&other.pairEvidenceStamp, "acct-okx-other", 6500, 7000, 7000, 20000)
		otherB := other
		otherB.EvidenceID, otherB.Equity = "acct-okx-other-b", "5999"
		d.Accounts = append(d.Accounts, same, okx2, other, otherB)
		var kept []pairConversionEvidence
		for _, cv := range d.Conversions {
			if cv.Key.MemberID == pairTestOKX {
				first, second := cv, cv
				pairRestamp(&first.pairEvidenceStamp, "conv-usdt-1", 500, 500, 500, 4000)
				pairRestamp(&second.pairEvidenceStamp, "conv-usdt-2", 5900, 6000, 6000, 20000)
				second.RateUSD = "0.9997"
				kept = append(kept, first, second)
				continue
			}
			kept = append(kept, cv)
		}
		d.Conversions = kept
	})
	src, err := newPairFrozenEvidenceSource(blob, sha256Hex(blob))
	if err != nil {
		t.Fatalf("the scheduled evidence document must load: %v", err)
	}
	venues := newPairTestVenues(false)
	adm, cfg, err := newPairResearchAdmission(doc, "run-1", dir, false, pairResearchDeps{VenueFor: venues.factory(), Evidence: src, Quotes: newPairFixtureQuotes(t, doc).collector})
	if err != nil {
		t.Fatalf("admission with scheduled evidence: %v", err)
	}
	t.Cleanup(adm.release)
	contract, err := buildPairContract(cfg, adm.pair, adm.limits)
	if err != nil {
		t.Fatal(err)
	}
	hl, _ := contract.pairMember(pairTestHL)
	okx, _ := contract.pairMember(pairTestOKX)
	ev := adm.evidenceSource()
	ctx := context.Background()
	for _, tc := range []struct {
		ref  int64
		want string
	}{
		{999, ""}, {1000, "cap-a"}, {2999, "cap-a"}, {3200, "tie"}, {3501, "cap-b"}, {9000, "cap-b"}, {9001, ""},
	} {
		got, err := ev.Capability(ctx, hl, tc.ref)
		switch tc.want {
		case "":
			if err == nil {
				t.Fatalf("ref %d selected %s; future-received or expired evidence must be refused", tc.ref, got.EvidenceID)
			}
		case "tie":
			if err == nil || !strings.Contains(err.Error(), "tie at receipt 3000 with different content") {
				t.Fatalf("ref %d must refuse the conflicting tie, got %s %v", tc.ref, got.EvidenceID, err)
			}
		default:
			if err != nil || got.EvidenceID != tc.want {
				t.Fatalf("ref %d selected %q (%v), want %s", tc.ref, got.EvidenceID, err, tc.want)
			}
			if err := pairCapabilityMatches(hl, got); err != nil {
				t.Fatalf("the selected capability must still match the contract: %v", err)
			}
		}
	}
	acct, err := ev.Account(ctx, okx, 2000)
	if err != nil || acct.EvidenceID != "acct-okx-same" {
		t.Fatalf("an equal-content tie selects the first revision deterministically, got %q (%v)", acct.EvidenceID, err)
	}
	if _, err := ev.Account(ctx, okx, 7000); err == nil {
		t.Fatal("an account tie with different equity must be refused")
	}
	if got, err := ev.Account(ctx, hl, 7000); err != nil || got.Key.MemberID != pairTestHL {
		t.Fatalf("the other member's account schedule is independent: %v", err)
	}
	convs, err := ev.Conversions(ctx, okx, 3000)
	if err != nil || len(convs) != 1 || convs[0].EvidenceID != "conv-usdt-1" {
		t.Fatalf("conversion at 3000: %+v %v", convs, err)
	}
	if _, err := ev.Conversions(ctx, okx, 5000); err == nil || !strings.Contains(err.Error(), "no USDT-to-USD conversion evidence") {
		t.Fatalf("a gap in the USDT schedule must be refused, got %v", err)
	}
	if convs, err := ev.Conversions(ctx, okx, 6000); err != nil || convs[0].RateUSD != "0.9997" {
		t.Fatalf("the later USDT revision must apply at its receipt: %+v %v", convs, err)
	}
	if convs, err := ev.Conversions(ctx, hl, 5000); err != nil || len(convs) != 1 || convs[0].Currency != "USDC" {
		t.Fatalf("the USDC schedule is selected independently: %+v %v", convs, err)
	}
	first, _ := ev.Capability(ctx, hl, 1000)
	first.Meta.FeeCurrencies[0] = "XXX"
	first.Meta.TradingStatus = "halted"
	again, _ := ev.Capability(ctx, hl, 1000)
	if again.Meta.FeeCurrencies[0] != "USDC" || again.Meta.TradingStatus != "live" {
		t.Fatal("a caller changed the frozen evidence through a returned record")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := ev.Capability(cancelled, hl, 1000); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled read must return the context error, got %v", err)
	}
	ghost := hl
	ghost.AccountKey = "synthetic:other"
	if _, err := ev.Account(ctx, ghost, 1000); err == nil {
		t.Fatal("an unsupported identity must be refused")
	}
	if _, err := pairRoundBound(hl, again, "buy", 60000.1); err != nil {
		t.Fatalf("a matching selected capability must round a bound: %v", err)
	}
}

func pairLoadBaskets(t *testing.T, cfg *Config) error {
	t.Helper()
	store := openSourceStore(t, cfg)
	defer store.Close()
	state, _, err := LoadStateWithStore(cfg, store)
	if err != nil {
		return err
	}
	for _, sc := range cfg.Strategies {
		if state.Strategies[sc.ID] == nil {
			state.Strategies[sc.ID] = NewStrategyState(sc)
		}
	}
	if err := store.LoadEntryGuards(state, cfg.Strategies, map[string]bool{}); err != nil {
		return err
	}
	_, err = store.LoadBaskets(state, cfg)
	return err
}

func pairRewriteSavedContract(t *testing.T, dbPath string, rewrite func(string) string) {
	t.Helper()
	db, err := OpenStateDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var raw string
	if err := db.db.QueryRow(`SELECT contract_json FROM basket_contracts WHERE basket_id = ?`, pairTestID).Scan(&raw); err != nil {
		t.Fatalf("read the saved pair contract: %v", err)
	}
	if _, err := db.db.Exec(`UPDATE basket_contracts SET contract_json = ? WHERE basket_id = ?`, rewrite(raw), pairTestID); err != nil {
		t.Fatal(err)
	}
}

func TestBasketPairContractVersionOneAndEvidenceDigestResume(t *testing.T) {
	e := newPairTestEnv(t, nil)
	pairEnterHoldRunner(e, 0.06)
	e.slotStep(0)
	e.store.Close()
	manifestPath := filepath.Join(e.dir, pairResearchManifestName)
	savedManifest, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	resume := func(deps pairResearchDeps) (*pairResearchAdmission, *Config, error) {
		return newPairResearchAdmission(e.doc, "run-1", e.dir, true, deps)
	}

	before := dirDigest(t, e.dir)
	changed := pairTestEvidence(t, e.doc, func(d *pairEvidenceDocument) { pairFindAcct(d, pairTestOKX).Equity = "6001" })
	if _, _, err := resume(pairResearchDeps{VenueFor: e.venues.factory(), Evidence: changed, Quotes: newPairFixtureQuotes(t, e.doc).collector}); err == nil || !strings.Contains(err.Error(), "evidence document digest") {
		t.Fatalf("a resume with a changed evidence document must be refused, got %v", err)
	}
	if dirDigest(t, e.dir) != before {
		t.Fatal("a refused evidence resume changed the saved state")
	}

	var m map[string]interface{}
	if err := json.Unmarshal(savedManifest, &m); err != nil {
		t.Fatal(err)
	}
	delete(m, "pair_v")
	delete(m, "evidence_digest")
	v1, _ := json.Marshal(m)
	if err := os.WriteFile(manifestPath, v1, 0o600); err != nil {
		t.Fatal(err)
	}
	pairRewriteSavedContract(t, e.cfg.DBFile, func(s string) string { return strings.Replace(s, `"pair_v":2`, `"pair_v":1`, 1) })
	before = dirDigest(t, e.dir)
	_, _, err = resume(pairTestDeps(t, e.doc, e.venues))
	if err == nil || !strings.Contains(err.Error(), "uses pair contract version 1, which this build does not support (version 2)") {
		t.Fatalf("a version-1 research run must be refused explicitly on resume, got %v", err)
	}
	if dirDigest(t, e.dir) != before {
		t.Fatal("the refused version-1 resume changed the saved state")
	}

	if err := os.WriteFile(manifestPath, savedManifest, 0o600); err != nil {
		t.Fatal(err)
	}
	adm, cfg, err := resume(pairTestDeps(t, e.doc, e.venues))
	if err != nil {
		t.Fatalf("the version-2 manifest must pass admission: %v", err)
	}
	t.Cleanup(adm.release)
	before = pairFileDigest(t, cfg.DBFile)
	if err := pairLoadBaskets(t, cfg); err == nil || !strings.Contains(err.Error(), "saved pair contract version 1 is not supported by this build (version 2)") {
		t.Fatalf("a saved version-1 pair contract must be refused at storage load, got %v", err)
	}
	if pairFileDigest(t, cfg.DBFile) != before {
		t.Fatal("the refused version-1 storage load changed the state file")
	}
	pairRewriteSavedContract(t, cfg.DBFile, func(s string) string { return strings.Replace(s, `"pair_v":1`, `"pair_v":2,"bogus":1`, 1) })
	if err := pairLoadBaskets(t, cfg); err == nil || !strings.Contains(err.Error(), `unknown field "bogus"`) {
		t.Fatalf("a saved pair contract with an unknown field must be refused, got %v", err)
	}
	pairRewriteSavedContract(t, cfg.DBFile, func(s string) string { return strings.Replace(s, `"pair_v":2,"bogus":1`, `"pair_v":2,"pair_v":2`, 1) })
	if err := pairLoadBaskets(t, cfg); err == nil || !strings.Contains(err.Error(), `duplicate object key "pair_v"`) {
		t.Fatalf("a saved pair contract with a duplicate key must be refused, got %v", err)
	}
	pairRewriteSavedContract(t, cfg.DBFile, func(s string) string {
		var out bytes.Buffer
		_ = json.Indent(&out, []byte(strings.Replace(s, `"pair_v":2,"pair_v":2`, `"pair_v":2`, 1)), "", "  ")
		return out.String()
	})
	if err := pairLoadBaskets(t, cfg); err != nil {
		t.Fatalf("saved pair contract bytes need not be canonical: %v", err)
	}
}

func TestBasketPairContractChangesAreRestartRequired(t *testing.T) {
	_, _, base := pairAdmitAccepted(t, nil)
	for name, mutate := range map[string]pairMutation{
		"book need":        pairSetBook("depth", 6),
		"member source":    pairAll(pairSetMember(0, "source", "archive"), pairSetMember(0, "modeled_delivery_ms", 250)),
		"cost policy":      pairSetCost(1, "taker_fee_pct", 0.0006),
		"quote age":        pairSetLimit("quote_age_ms", 6000),
		"timeout":          pairSetLimit("collection_timeout_ms", 2500),
		"skew":             pairSetLimit("receive_skew_ms", 300),
		"envelope":         pairSetLimit("envelope_max_bytes", 2097152),
		"protective width": pairSetLimit("protective_width_bps", 75),
		"selected leverage": func(r map[string]interface{}) {
			pairTestStrategy(r, pairTestOKX)["leverage"] = 3
		},
		"okx step text spelling": pairSetMeta(1, "tick_step_text", "0.10"),
	} {
		t.Run(name, func(t *testing.T) {
			_, _, c := pairAdmitAccepted(t, mutate)
			if c.Fingerprint() == base.Fingerprint() {
				t.Fatalf("changing the %s must change the pair contract fingerprint", name)
			}
		})
	}

	e := newPairTestEnv(t, nil)
	reloads := map[string]func(next *Config){
		"book need":       func(n *Config) { n.Pairs[0].BookNeed.Depth = 9 },
		"member source":   func(n *Config) { n.Pairs[0].Members[0].Source = "public_snapshot" },
		"cost policy":     func(n *Config) { n.Pairs[0].Members[1].CostPolicy.SlippagePct = 0.001 },
		"meta price rule": func(n *Config) { n.Pairs[0].Members[1].Meta.TickStepText = "0.10" },
		"limits":          func(n *Config) { n.Pairs[0].Limits = json.RawMessage(`{"quote_age_ms":1}`) },
		"member leverage": func(n *Config) {
			for i := range n.Strategies {
				if n.Strategies[i].ID == pairTestHL {
					n.Strategies[i].Leverage = 3
				}
			}
		},
	}
	for name, change := range reloads {
		next := *e.cfg
		next.Strategies = append([]StrategyConfig(nil), e.cfg.Strategies...)
		next.Pairs = append([]PairConfig(nil), e.cfg.Pairs...)
		next.Pairs[0].Members = append([]PairMemberConfig(nil), e.cfg.Pairs[0].Members...)
		change(&next)
		if err := validateHotReloadCompatible(e.cfg, &next); err == nil {
			t.Fatalf("a reload that changes the %s must be refused", name)
		}
	}

	pairEnterHoldRunner(e, 0.06)
	e.slotStep(0)
	e.slotStep(1)
	e.assertBooks("matched before the contract change", 0.06, -0.06)
	e.store.Close()
	pairRewriteSavedContract(t, e.cfg.DBFile, func(s string) string {
		return strings.Replace(s, `"quote_age_ms":5000`, `"quote_age_ms":4000`, 1)
	})
	e.store = openSourceStore(t, e.cfg)
	loaded, _, err := LoadStateWithStore(e.cfg, e.store)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.store.LoadEntryGuards(loaded, e.cfg.Strategies, map[string]bool{pairTestHL: true, pairTestOKX: true}); err != nil {
		t.Fatal(err)
	}
	runtimes, err := e.store.LoadBaskets(loaded, e.cfg)
	if err != nil {
		t.Fatal(err)
	}
	bc := newBasketCoordinator(e.cfg, loaded, e.store, e.mu, nil, nil, runtimes)
	if err := bc.startupCheck(); err == nil || !strings.Contains(err.Error(), "contract changed (pair) while a position") {
		t.Fatalf("startup must refuse a changed contract while positions remain, got %v", err)
	}
}

func TestBasketPairContractEvidenceReaderProbeRegistration(t *testing.T) {
	type call struct{ script, argv string }
	record := func(cfg *Config) []call {
		var calls []call
		prev := probeOneCheckScriptFn
		probeOneCheckScriptFn = func(script string, argv []string) error {
			if script == pairEvidenceReaderScript {
				calls = append(calls, call{script, strings.Join(argv, " ")})
			}
			return nil
		}
		defer func() { probeOneCheckScriptFn = prev }()
		if err := probeCheckScripts(cfg); err != nil {
			t.Fatal(err)
		}
		return calls
	}
	cfg := &Config{Strategies: []StrategyConfig{
		{ID: "hl-a", Platform: "hyperliquid", Type: "perps", Script: hyperliquidCheckScript},
		{ID: "hl-b", Platform: "hyperliquid", Type: "perps", Script: hyperliquidCheckScript},
		{ID: "okx-swap", Platform: "okx", Type: "perps", Script: okxCheckScriptPath, Args: []string{"sma_crossover", "BTC", "1h", "--inst-type=swap"}},
		{ID: "okx-spot", Platform: "okx", Type: "spot", Script: okxCheckScriptPath},
	}}
	calls := record(cfg)
	want := []call{{pairEvidenceReaderScript, strings.Join(pairEvidenceHyperliquidProbeArgv, " ")}, {pairEvidenceReaderScript, strings.Join(pairEvidenceOKXProbeArgv, " ")}}
	if len(calls) != len(want) || calls[0] != want[0] || calls[1] != want[1] {
		t.Fatalf("the reader must be probed once per configured provider: %+v", calls)
	}
	if calls := record(&Config{Strategies: []StrategyConfig{{ID: "spot", Platform: "binanceus", Type: "spot", Script: "shared_scripts/check_strategy.py"}}}); len(calls) != 0 {
		t.Fatalf("a configuration with no Hyperliquid perps or OKX swap adds no reader probe: %+v", calls)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	root := map[string]interface{}{
		"config_version": CurrentConfigVersion, "interval_seconds": 60, "db_file": filepath.Join(dir, "state.db"), "log_dir": filepath.Join(dir, "logs"),
		"strategies": []interface{}{
			map[string]interface{}{"id": "hl-btc", "type": "perps", "platform": "hyperliquid", "script": hyperliquidCheckScript,
				"args": []interface{}{"sma_crossover", "BTC", "1h", "--mode=paper"}, "capital": 1000, "allow_no_edge": true},
			map[string]interface{}{"id": "okx-btc", "type": "perps", "platform": "okx", "script": okxCheckScriptPath,
				"args": []interface{}{"sma_crossover", "BTC", "1h", "--mode=paper", "--inst-type=swap"}, "capital": 1000, "allow_no_edge": true},
		},
	}
	blob, _ := json.Marshal(root)
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		t.Fatal(err)
	}
	var explicit []string
	prev := probeOneCheckScriptFn
	probeOneCheckScriptFn = func(script string, argv []string) error {
		if script == pairEvidenceReaderScript {
			explicit = append(explicit, strings.Join(argv, " "))
		}
		return nil
	}
	defer func() { probeOneCheckScriptFn = prev }()
	if code := runProbe([]string{"--config", path}); code != 0 {
		t.Fatalf("the explicit probe of an ordinary configuration failed with %d", code)
	}
	if len(explicit) != 2 || explicit[0] != want[0].argv || explicit[1] != want[1].argv {
		t.Fatalf("the explicit probe must reach the same reader registration: %v", explicit)
	}
}
