package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const pairPolicyEscapedName = `"cross\u005fexchange\u005fperp\u005farbitrage"`

func pairPolicyAdmissionDoc(t *testing.T, dir string, mutate func(p map[string]interface{})) []byte {
	t.Helper()
	return pairTestDoc(t, dir, func(r map[string]interface{}) {
		p := pairTestPair(r)
		p["policy"] = pairPolicyCrossExchangePerpArbitrage
		p["params"] = map[string]interface{}{"entry_threshold_bps": 5, "exit_threshold_bps": 1, "adverse_band_bps": 0, "funding_allowance_bps": 0}
		if mutate != nil {
			mutate(p)
		}
	})
}

func pairPolicySetParam(key string, value interface{}) func(p map[string]interface{}) {
	return func(p map[string]interface{}) {
		p["params"].(map[string]interface{})[key] = value
	}
}

func TestBasketPairPolicyParamsRefusedBeforeTypedDecode(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(p map[string]interface{})
		want   []string
		docHas string
	}{
		{name: "params absent", mutate: func(p map[string]interface{}) { delete(p, "params") }, want: []string{
			"pairs[0].params: adverse_band_bps is missing", "pairs[0].params: entry_threshold_bps is missing",
			"pairs[0].params: exit_threshold_bps is missing", "pairs[0].params: funding_allowance_bps is missing"}},
		{name: "one key missing", mutate: func(p map[string]interface{}) { delete(p["params"].(map[string]interface{}), "funding_allowance_bps") }, want: []string{"funding_allowance_bps is missing; policy cross_exchange_perp_arbitrage has no parameter defaults"}},
		{name: "unknown key", mutate: pairPolicySetParam("entry_bps", 5), want: []string{`unknown field "entry_bps" in params for policy cross_exchange_perp_arbitrage`}},
		{name: "null", mutate: pairPolicySetParam("entry_threshold_bps", nil), want: []string{"entry_threshold_bps must be a finite JSON number, got null"}},
		{name: "boolean", mutate: pairPolicySetParam("exit_threshold_bps", true), want: []string{"exit_threshold_bps must be a finite JSON number, got boolean"}},
		{name: "string", mutate: pairPolicySetParam("adverse_band_bps", "5"), want: []string{"adverse_band_bps must be a finite JSON number, got string"}},
		{name: "array", mutate: pairPolicySetParam("funding_allowance_bps", []interface{}{1}), want: []string{"funding_allowance_bps must be a finite JSON number, got array"}},
		{name: "object", mutate: pairPolicySetParam("funding_allowance_bps", map[string]interface{}{"v": 1}), want: []string{"funding_allowance_bps must be a finite JSON number, got object"}},
		{name: "overflowing number", mutate: pairPolicySetParam("entry_threshold_bps", json.RawMessage("1e400")), want: []string{"entry_threshold_bps must be a finite number, got 1e400"}},
		{name: "exit equals entry", mutate: pairPolicySetParam("exit_threshold_bps", 5), want: []string{"exit_threshold_bps 5 must be below entry_threshold_bps 5"}},
		{name: "exit below zero", mutate: pairPolicySetParam("exit_threshold_bps", -1), want: []string{"exit_threshold_bps -1 must be at least 0"}},
		{name: "entry above 10000", mutate: pairPolicySetParam("entry_threshold_bps", 10000.5), want: []string{"entry_threshold_bps 10000.5 must not exceed 10000"}},
		{name: "adverse band at 10000", mutate: pairPolicySetParam("adverse_band_bps", 10000), want: []string{"adverse_band_bps 10000 must be at least 0 and below 10000"}},
		{name: "negative adverse band", mutate: pairPolicySetParam("adverse_band_bps", -0.5), want: []string{"adverse_band_bps -0.5 must be at least 0 and below 10000"}},
		{name: "negative funding allowance", mutate: pairPolicySetParam("funding_allowance_bps", -0.01), want: []string{"funding_allowance_bps -0.01 must be at least 0"}},
		{name: "untrimmed policy name with absent params", mutate: func(p map[string]interface{}) {
			p["policy"] = " cross_exchange_perp_arbitrage "
			delete(p, "params")
		}, want: []string{"pairs[0].params: entry_threshold_bps is missing"}},
		{name: "escaped policy spelling with a bad value", mutate: func(p map[string]interface{}) {
			p["policy"] = json.RawMessage(pairPolicyEscapedName)
			p["params"].(map[string]interface{})["entry_threshold_bps"] = "5"
		}, want: []string{"entry_threshold_bps must be a finite JSON number, got string"}, docHas: pairPolicyEscapedName},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "run")
			doc := pairPolicyAdmissionDoc(t, dir, func(p map[string]interface{}) {
				p["underlying"] = "btc"
				tc.mutate(p)
			})
			if tc.docHas != "" && !bytes.Contains(doc, []byte(tc.docHas)) {
				t.Fatalf("the research document must carry the raw spelling %s: %s", tc.docHas, doc)
			}
			venues := newPairTestVenues(false)
			_, _, err := newPairResearchAdmission(doc, "run-1", dir, false, pairTestDeps(t, doc, venues))
			if err == nil {
				t.Fatal("invalid parameters must be refused")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Fatalf("refusal %q must name %q", err, w)
				}
			}
			if strings.Contains(err.Error(), "underlying") {
				t.Fatalf("the parameter refusal must come before the typed decode that reports the lowercase underlying: %v", err)
			}
			if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
				t.Fatalf("a refused admission must not create its state directory: %v", statErr)
			}
			fixtureDir := filepath.Join(t.TempDir(), "fixture")
			fixtureDoc := pairTestDoc(t, fixtureDir, func(r map[string]interface{}) {
				pairLiveShapedMutate(r)
				p := pairTestPair(r)
				p["policy"] = pairPolicyCrossExchangePerpArbitrage
				p["params"] = map[string]interface{}{"entry_threshold_bps": 5, "exit_threshold_bps": 1, "adverse_band_bps": 0, "funding_allowance_bps": 0}
				tc.mutate(p)
			})
			if tc.docHas != "" && !bytes.Contains(fixtureDoc, []byte(tc.docHas)) {
				t.Fatalf("the controlled fixture must carry the raw spelling %s: %s", tc.docHas, fixtureDoc)
			}
			deps := pairLiveShapedDeps{
				VenueFor:     newPairTestVenues(true).factory(),
				HLCloser:     func(string, *float64, []int64) (*HyperliquidCloseResult, error) { return nil, fmt.Errorf("no close") },
				OKXCloser:    func(string, *float64) (*OKXCloseResult, error) { return nil, fmt.Errorf("no close") },
				HLFetcher:    func(string) ([]HLPosition, error) { return nil, nil },
				OKXFetcher:   func() ([]OKXPosition, error) { return nil, nil },
				EntryQueries: func(*entryIntent) error { return fmt.Errorf("no query") },
			}
			if _, _, err := newPairLiveShapedAdmission(t, fixtureDoc, fixtureDir, deps); err == nil || !strings.Contains(err.Error(), tc.want[0]) {
				t.Fatalf("the controlled fixture must refuse the same parameters at its raw stage: %v", err)
			}
			if _, statErr := os.Stat(fixtureDir); !os.IsNotExist(statErr) {
				t.Fatalf("the refused fixture must not create its state directory: %v", statErr)
			}
		})
	}
}

func TestBasketPairPolicyParamsBoundaryValuesAreAdmitted(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "run")
	doc := pairPolicyAdmissionDoc(t, dir, func(p map[string]interface{}) {
		p["params"] = map[string]interface{}{"entry_threshold_bps": 10000, "exit_threshold_bps": 0, "adverse_band_bps": 9999.99, "funding_allowance_bps": 0}
	})
	adm, cfg, err := newPairResearchAdmission(doc, "run-1", dir, false, pairTestDeps(t, doc, newPairTestVenues(false)))
	if err != nil {
		t.Fatalf("boundary parameter values must be admitted: %v", err)
	}
	defer adm.release()
	if cfg.Pairs[0].Policy != pairPolicyCrossExchangePerpArbitrage || len(cfg.Pairs[0].Params) != 4 {
		t.Fatalf("the admitted pair must carry the production policy and its four parameters: %+v", cfg.Pairs[0])
	}
	if _, _, err := buildAddStrategyEntry(pairPolicyCrossExchangePerpArbitrage, "hyperliquid", "BTC"); err == nil || !strings.Contains(err.Error(), "is production_refused (research-only, evidence backtest/candidates/cross_exchange_perp_arbitrage_1655/REPORT.md)") {
		t.Fatalf("the add-strategy command must refuse the production pair policy name as production_refused: %v", err)
	}
}

func TestBasketPairPolicyEscapedPolicySpellingIsAdmitted(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "run")
	doc := pairPolicyAdmissionDoc(t, dir, func(p map[string]interface{}) {
		p["policy"] = json.RawMessage(pairPolicyEscapedName)
	})
	if !bytes.Contains(doc, []byte(pairPolicyEscapedName)) {
		t.Fatalf("the research document must carry the escaped spelling: %s", doc)
	}
	adm, cfg, err := newPairResearchAdmission(doc, "run-1", dir, false, pairTestDeps(t, doc, newPairTestVenues(false)))
	if err != nil {
		t.Fatalf("the escaped spelling with four valid parameters must be admitted: %v", err)
	}
	defer adm.release()
	if cfg.Pairs[0].Policy != pairPolicyCrossExchangePerpArbitrage || len(cfg.Pairs[0].Params) != 4 {
		t.Fatalf("the admitted pair must carry the plain policy name and its four parameters: %+v", cfg.Pairs[0])
	}
}
