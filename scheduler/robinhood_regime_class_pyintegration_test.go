//go:build pyintegration

package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func robinhoodRegimeClassHourly(start, end int64) [][]float64 {
	var rows [][]float64
	for i, t := 0, start; t <= end; i, t = i+1, t+3_600_000 {
		p := 100.0 + float64(i%7)*0.5 - float64(i%3)*0.25
		rows = append(rows, []float64{float64(t), p - 0.1, p + 0.6, p - 0.7, p, 10 + float64(i)})
	}
	return rows
}

func robinhoodRegimeClassFixture(t *testing.T, dir string) (string, string) {
	t.Helper()
	now := time.Now().UnixMilli()
	now -= now % 3_600_000
	hourly := robinhoodRegimeClassHourly(now-1200*3_600_000, now)
	var fourHour [][]float64
	for _, r := range robinhoodRegimeClassHourly(now-2400*3_600_000, now) {
		if int64(r[0])%(4*3_600_000) == 0 {
			fourHour = append(fourHour, r)
		}
	}
	yahooLog := filepath.Join(dir, "yahoo.log")
	blob, err := json.Marshal(map[string]any{
		"yahoo":     map[string]any{"LTC-USD|1h": hourly, "LTC|4h": fourHour},
		"yahoo_log": yahooLog,
		"ccxt_log":  filepath.Join(dir, "ccxt.log"),
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture := filepath.Join(dir, "fixture.json")
	if err := os.WriteFile(fixture, blob, 0o600); err != nil {
		t.Fatal(err)
	}
	return fixture, yahooLog
}

func TestRobinhoodRegimeClassesStaySeparateThroughTheRealWorker(t *testing.T) {
	root := htfITRoot(t)
	dir := t.TempDir()
	fixture, yahooLog := robinhoodRegimeClassFixture(t, dir)
	t.Chdir(root)
	t.Setenv("PYTHONPATH", filepath.Join(root, "shared_scripts", "testdata", "htf_offline"))
	t.Setenv("HTF_OFFLINE_FIXTURE", fixture)
	t.Setenv("ROBINHOOD_USERNAME", "")

	rc := &RegimeConfig{Enabled: true, Timeframe: optionsRegimeTimeframe, Period: 14, ADXThreshold: 20}
	spot := StrategyConfig{ID: "rh-ltc-spot", Type: "spot", Platform: "robinhood", Script: robinhoodCheckScript, Args: []string{"atr_breakout", "LTC", "1h", "--mode=paper"}}
	opts := StrategyConfig{ID: "rh-ltc-opts", Type: "options", Platform: "robinhood", Script: "shared_scripts/check_options.py", Args: []string{"vol_mean_reversion", "LTC", "--platform=robinhood", "--mode=paper"}}
	spotReq, ok := strategyRegimeBundleRequest(spot, rc)
	if !ok {
		t.Fatal("spot request missing")
	}
	optsReq, ok := strategyRegimeBundleRequest(opts, rc)
	if !ok {
		t.Fatal("options request missing")
	}
	if spotReq.Key.AssetClass != robinhoodAssetClassCrypto || optsReq.Key.AssetClass != robinhoodAssetClassStock {
		t.Fatalf("classes = %q / %q, want crypto / stock", spotReq.Key.AssetClass, optsReq.Key.AssetClass)
	}
	if spotReq.AllowSpotFallback || optsReq.AllowSpotFallback {
		t.Fatal("a Robinhood regime request must not allow the Binance.US fallback")
	}
	sameRest := spotReq.Key
	sameRest.AssetClass = optsReq.Key.AssetClass
	if sameRest != optsReq.Key {
		t.Fatalf("keys must differ only by class: %+v vs %+v", spotReq.Key, optsReq.Key)
	}
	reqs := collectRegimeBundleRequests([]StrategyConfig{opts, spot, opts, spot}, rc)
	if len(reqs) != 2 || reqs[0].Key != spotReq.Key || reqs[1].Key != optsReq.Key {
		t.Fatalf("requests = %+v, want crypto then stock", reqs)
	}

	store := &RegimeStore{}
	populateRegimeStore(store, []StrategyConfig{opts, spot}, rc, nil, nil)
	bundles, _ := store.Snapshot()
	if len(bundles) != 2 {
		t.Fatalf("bundles = %d, want 2", len(bundles))
	}
	for i, want := range []regimeBundleKey{spotReq.Key, optsReq.Key} {
		if bundles[i].Key != want || bundles[i].Err != "" || len(bundles[i].Payload.Windows) == 0 {
			t.Fatalf("bundle %d = %+v (err %q), want ready %+v", i, bundles[i].Key, bundles[i].Err, want)
		}
	}
	if got := bundles[0].Key.String(); got != "robinhood:crypto/LTC/4h" {
		t.Fatalf("crypto identity = %q", got)
	}
	if got := bundles[1].Key.String(); got != "robinhood:stock/LTC/4h" {
		t.Fatalf("stock identity = %q", got)
	}
	raw, err := os.ReadFile(yahooLog)
	if err != nil {
		t.Fatal(err)
	}
	var requested []string
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var rec []string
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatal(err)
		}
		requested = append(requested, rec[0]+"|"+rec[1])
	}
	sort.Strings(requested)
	if strings.Join(requested, ",") != "LTC-USD|1h,LTC|4h" {
		t.Fatalf("Yahoo requests = %v, want one crypto LTC-USD 1h read and one stock LTC 4h read", requested)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "ccxt.log")); err == nil && len(strings.TrimSpace(string(b))) > 0 {
		t.Fatalf("a Robinhood regime read used Binance.US:\n%s", b)
	}
	entries, _ := uiRegimeEntries(store)
	if len(entries) != 2 || entries[0].AssetClass != "crypto" || entries[1].AssetClass != "stock" {
		t.Fatalf("regime API entries = %+v", entries)
	}

	t.Run("worker output with a missing or mismatched class is refused", func(t *testing.T) {
		stdout, stderr, err := runPython(context.Background(), regimeCheckScript, regimeBundleCheckArgs(spotReq), nil)
		if err != nil {
			t.Fatalf("worker: %v\n%s", err, stderr)
		}
		if _, err := parseRegimeBundleOutput(spotReq.Key, stdout, time.Now()); err != nil {
			t.Fatalf("matching class refused: %v", err)
		}
		if _, err := parseRegimeBundleOutput(optsReq.Key, stdout, time.Now()); err == nil || !strings.Contains(err.Error(), `worker reported asset class "crypto", want "stock"`) {
			t.Fatalf("mismatched class = %v", err)
		}
		var doc map[string]any
		if err := json.Unmarshal(stdout, &doc); err != nil {
			t.Fatal(err)
		}
		delete(doc, "asset_class")
		missing, _ := json.Marshal(doc)
		if _, err := parseRegimeBundleOutput(spotReq.Key, missing, time.Now()); err == nil || !strings.Contains(err.Error(), `worker reported asset class "", want "crypto"`) {
			t.Fatalf("missing class = %v", err)
		}
		generic := spotReq.Key
		generic.Platform, generic.AssetClass = "binanceus", ""
		if _, err := parseRegimeBundleOutput(generic, stdout, time.Now()); err == nil {
			t.Fatal("a classed output was accepted for an unclassed key")
		}
	})
}
