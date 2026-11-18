//go:build pyintegration

package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const robinhoodTimeframeParityScript = `import json, sys
sys.path.insert(0, "platforms/robinhood")
sys.path.insert(0, "shared_tools")
from adapter import ROBINHOOD_CLOSED_BAR_REFUSED, ROBINHOOD_TIMEFRAMES, YAHOO_CRYPTO_MAP, UnsupportedRobinhoodSymbol, robinhood_yahoo_ticker
from htf_filter import get_default_htf
from closed_bar import FIXED_DURATION_MS
def resolve(asset_class, symbol):
    try:
        return robinhood_yahoo_ticker(asset_class, symbol)
    except UnsupportedRobinhoodSymbol:
        return ""
probes = json.loads(sys.argv[1])
print(json.dumps({
    "table": {k: v.yahoo_interval for k, v in ROBINHOOD_TIMEFRAMES.items()},
    "session": {k: v.session_interval for k, v in ROBINHOOD_TIMEFRAMES.items()},
    "crypto": YAHOO_CRYPTO_MAP,
    "crypto_resolver": {s: resolve("crypto", s) for s in probes},
    "stock_resolver": {s: resolve("stock", s) for s in probes},
    "htf": {k: get_default_htf(k) for k in ROBINHOOD_TIMEFRAMES},
    "fixed": FIXED_DURATION_MS,
    "refused": ROBINHOOD_CLOSED_BAR_REFUSED,
}))
`

func TestRobinhoodTimeframeTablesMatchThePythonAdapter(t *testing.T) {
	root := htfITRoot(t)
	probes := []string{"AAPL", "SPY", "BRK.B", "BRK-B", "XRP", "xrp", "XRP-USD", "xrp-usd", "BTC/USD", "ETH-USDT", "SOL-USDC", "btc", "BTC", "LTC", "UNI", "PEPE", "MATIC", "POL", "1SPY", "", " LTC ", "TOOLONGX"}
	seen := map[string]bool{}
	for _, sym := range probes {
		seen[sym] = true
	}
	for sym := range robinhoodYahooCryptoMap {
		if !seen[sym] {
			probes = append(probes, sym)
		}
	}
	sort.Strings(probes)
	probeJSON, err := json.Marshal(probes)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(filepath.Join(root, ".venv", "bin", "python3"), "-c", robinhoodTimeframeParityScript, string(probeJSON))
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("parity script: %v\n%s", err, out)
	}
	var got struct {
		Table          map[string]string `json:"table"`
		Session        map[string]string `json:"session"`
		Crypto         map[string]string `json:"crypto"`
		CryptoResolver map[string]string `json:"crypto_resolver"`
		StockResolver  map[string]string `json:"stock_resolver"`
		HTF            map[string]string `json:"htf"`
		Fixed          map[string]int64  `json:"fixed"`
		Refused        map[string]string `json:"refused"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("parity output: %v\n%s", err, out)
	}
	keys := func(m map[string]string) []string {
		out := make([]string, 0, len(m))
		for k := range m {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	goKeys := make([]string, 0, len(robinhoodTimeframes))
	for k := range robinhoodTimeframes {
		goKeys = append(goKeys, k)
	}
	sort.Strings(goKeys)
	if strings.Join(keys(got.Table), ",") != strings.Join(goKeys, ",") {
		t.Fatalf("Python ROBINHOOD_TIMEFRAMES keys %v differ from Go robinhoodTimeframes %v", keys(got.Table), goKeys)
	}
	if strings.Join(keys(got.Crypto), ",") != strings.Join(keys(robinhoodYahooCryptoMap), ",") {
		t.Fatalf("Python YAHOO_CRYPTO_MAP keys %v differ from Go robinhoodYahooCryptoMap %v", keys(got.Crypto), keys(robinhoodYahooCryptoMap))
	}
	for sym, yahoo := range robinhoodYahooCryptoMap {
		if got.Crypto[sym] != yahoo {
			t.Errorf("crypto symbol %q: Python Yahoo pair %q, Go %q", sym, got.Crypto[sym], yahoo)
		}
	}
	if len(got.CryptoResolver) != len(probes) || len(got.StockResolver) != len(probes) {
		t.Fatalf("Python resolved %d crypto and %d stock probes, want %d each", len(got.CryptoResolver), len(got.StockResolver), len(probes))
	}
	for _, sym := range probes {
		if pyOK, goOK := got.CryptoResolver[sym] != "", robinhoodCryptoCoinSupported(sym); pyOK != goOK {
			t.Errorf("crypto admission for %q: Python accepts=%v, Go accepts=%v", sym, pyOK, goOK)
		} else if goOK && got.CryptoResolver[sym] != robinhoodYahooCryptoMap[sym] {
			t.Errorf("crypto ticker for %q: Python %q, Go %q", sym, got.CryptoResolver[sym], robinhoodYahooCryptoMap[sym])
		}
		if pyOK, goOK := got.StockResolver[sym] != "", robinhoodStockUnderlyingError(sym) == ""; pyOK != goOK {
			t.Errorf("stock admission for %q: Python accepts=%v, Go accepts=%v", sym, pyOK, goOK)
		}
	}
	for _, sym := range []string{"LTC", "BTC", "XRP"} {
		if got.StockResolver[sym] != sym || got.CryptoResolver[sym] == "" {
			t.Errorf("collision symbol %q must resolve to stock %q and to its crypto ticker, got stock %q crypto %q", sym, sym, got.StockResolver[sym], got.CryptoResolver[sym])
		}
	}
	for _, sym := range []string{"AAPL", "XRP-USD", "MATIC", "xrp"} {
		if got.CryptoResolver[sym] != "" {
			t.Errorf("crypto resolver accepted %q as %q", sym, got.CryptoResolver[sym])
		}
	}
	if strings.Join(keys(got.Refused), ",") != strings.Join(keys(robinhoodClosedBarRefusedTimeframes), ",") {
		t.Fatalf("Python ROBINHOOD_CLOSED_BAR_REFUSED %v differs from Go robinhoodClosedBarRefusedTimeframes %v", keys(got.Refused), keys(robinhoodClosedBarRefusedTimeframes))
	}
	for tf, why := range robinhoodClosedBarRefusedTimeframes {
		if got.Refused[tf] != why {
			t.Errorf("closed-bar refusal for %q: Python %q, Go %q", tf, got.Refused[tf], why)
		}
	}
	for _, tf := range goKeys {
		if got.Table[tf] != robinhoodTimeframes[tf].Yahoo {
			t.Errorf("timeframe %q: Python Yahoo interval %q, Go %q", tf, got.Table[tf], robinhoodTimeframes[tf].Yahoo)
		}
		if got.Session[tf] != robinhoodTimeframes[tf].Session {
			t.Errorf("timeframe %q: Python session interval %q, Go %q", tf, got.Session[tf], robinhoodTimeframes[tf].Session)
		}
		goHTF := hlFeedHTFTimeframe(tf)
		if got.HTF[tf] != goHTF {
			t.Errorf("timeframe %q: Python higher timeframe %q, Go %q", tf, got.HTF[tf], goHTF)
		}
		if !robinhoodTimeframeSupported(goHTF) {
			t.Errorf("timeframe %q: higher timeframe %q is not a Robinhood timeframe", tf, goHTF)
		}
		if ms, ok := closedBarDecisionIntervalsMs[tf]; ok {
			if got.Fixed[tf] != ms {
				t.Errorf("timeframe %q: Python closed-bar interval %d ms, Go %d ms", tf, got.Fixed[tf], ms)
			}
		} else if _, ok := got.Fixed[tf]; ok {
			t.Errorf("timeframe %q: Python has a closed-bar interval that Go lacks", tf)
		}
	}
}

func robinhoodTimeframeITConfig(t *testing.T, dir, name string, strat map[string]any, regime map[string]any) string {
	t.Helper()
	entry := map[string]any{
		"id": "rh-tf", "type": "spot", "platform": "robinhood", "script": "shared_scripts/check_robinhood.py",
		"capital": 1000, "max_drawdown_pct": 90, "interval_seconds": 60,
	}
	for k, v := range strat {
		entry[k] = v
	}
	doc := map[string]any{
		"config_version": CurrentConfigVersion, "interval_seconds": 60,
		"db_file": filepath.Join(dir, name+".db"), "strategies": []any{entry},
	}
	if regime != nil {
		doc["regime"] = regime
	}
	blob, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name+".json")
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
