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

func robinhoodTimeframeITBinary(t *testing.T, root string) string {
	t.Helper()
	return pyintegrationBinary(t)
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

func TestRobinhoodTimeframeRealBinary(t *testing.T) {
	t.Run("RefusesUnsupportedTimeframesBeforeAnyCycle", func(t *testing.T) {
		root := htfITRoot(t)
		bin := robinhoodTimeframeITBinary(t, root)
		dir := t.TempDir()
		cases := []struct {
			name   string
			strat  map[string]any
			regime map[string]any
			want   string
		}{
			{"args-3m", map[string]any{"args": []string{"atr_breakout", "BTC", "3m", "--mode=paper"}}, nil,
				`strategy[rh-tf]: robinhood args timeframe "3m" is not supported: Yahoo candles serve only 12h, 15m, 1M, 1d, 1h, 1m, 1mo, 1w, 2h, 2m, 30m, 4h, 5m, 60m, 6h, 8h, 90m`},
			{"args-trailing-space", map[string]any{"args": []string{"atr_breakout", "BTC", "4h ", "--mode=paper"}}, nil,
				`strategy[rh-tf]: robinhood args timeframe "4h " is not supported`},
			{"regime-3d", map[string]any{"args": []string{"atr_breakout", "BTC", "1h", "--mode=paper"}}, map[string]any{"enabled": true, "timeframe": "3d"},
				`strategy[rh-tf]: robinhood regime.timeframe "3d" is not supported`},
			{"regime-1M", map[string]any{"args": []string{"atr_breakout", "BTC", "1h", "--mode=paper"}}, map[string]any{"enabled": true, "timeframe": "1M"},
				`strategy[rh-tf]: regime.timeframe "1M" is ambiguous for robinhood: it means 1mo on Yahoo but go-trader lowercases it to "1m" (1m); write "1mo" or "1m"`},
			{"live-closed-bar", map[string]any{"args": []string{"atr_breakout", "BTC", "1h", "--mode=live"}, "closed_bar_decisions": true}, nil,
				`strategy[rh-tf]: closed_bar_decisions is not supported: live Robinhood fills settle through robinhood_order_attempts`},
			{"paper-closed-bar-1m", map[string]any{"args": []string{"atr_breakout", "BTC", "1m", "--mode=paper"}, "closed_bar_decisions": true}, nil,
				`strategy[rh-tf]: closed_bar_decisions does not support timeframe "1m" (fixed-duration intervals only: 1m, 3m, 5m, 15m, 30m, 1h, 2h, 4h, 6h, 8h, 12h, 1d); on Robinhood Yahoo's newest closed 1m row was a flat zero-volume placeholder`},
			{"paper-closed-bar-60m", map[string]any{"args": []string{"atr_breakout", "BTC", "60m", "--mode=paper"}, "closed_bar_decisions": true}, nil,
				`strategy[rh-tf]: closed_bar_decisions does not support timeframe "60m"`},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				cfg := robinhoodTimeframeITConfig(t, dir, c.name, c.strat, c.regime)
				cmd := exec.Command(bin, "--config", cfg, "--once")
				cmd.Dir = root
				cmd.Env = append(os.Environ(), "ROBINHOOD_USERNAME=", "DISCORD_BOT_TOKEN=", "TELEGRAM_BOT_TOKEN=")
				out, err := cmd.CombinedOutput()
				log := string(out)
				if err == nil {
					t.Fatalf("--once accepted the config\n%s", log)
				}
				if !strings.Contains(log, "Failed to load config:") || !strings.Contains(log, c.want) {
					t.Fatalf("want a config error containing %q\n%s", c.want, log)
				}
				if strings.Contains(log, "[rh-tf] [") {
					t.Fatalf("a strategy cycle ran before the config error\n%s", log)
				}
			})
		}
	})
	t.Run("AcceptsAResampledPaperClosedBarStrategyOnAFormerlyUnmappedCoin", func(t *testing.T) {
		root := htfITRoot(t)
		bin := robinhoodTimeframeITBinary(t, root)
		dir := t.TempDir()
		cfg := robinhoodTimeframeITConfig(t, dir, "paper-xrp-2h", map[string]any{
			"args": []string{"atr_breakout", " xrp ", "2h", "--mode=paper"}, "closed_bar_decisions": true,
		}, map[string]any{"enabled": true, "timeframe": "12h"})
		cmd := exec.Command(bin, "probe", "--config", cfg)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "ROBINHOOD_USERNAME=", "DISCORD_BOT_TOKEN=", "TELEGRAM_BOT_TOKEN=")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("probe refused a normalized supported coin on a resampled paper closed-bar config: %v\n%s", err, out)
		}
	})
	t.Run("AcceptsAResampledPaperClosedBarStrategy", func(t *testing.T) {
		root := htfITRoot(t)
		bin := robinhoodTimeframeITBinary(t, root)
		dir := t.TempDir()
		cfg := robinhoodTimeframeITConfig(t, dir, "paper-4h", map[string]any{
			"args": []string{"atr_breakout", "BTC", "4h", "--mode=paper"}, "closed_bar_decisions": true, "htf_filter": true,
		}, map[string]any{"enabled": true, "timeframe": "1d"})
		cmd := exec.Command(bin, "probe", "--config", cfg)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "ROBINHOOD_USERNAME=", "DISCORD_BOT_TOKEN=", "TELEGRAM_BOT_TOKEN=")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("probe refused a supported Robinhood 4h paper closed-bar config: %v\n%s", err, out)
		}
	})
}
