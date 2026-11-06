//go:build pyintegration

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func trITFixture(t *testing.T, root string, start, end time.Time) {
	t.Helper()
	var hl []map[string]any
	var ccxtRows [][]any
	for ts := start; !ts.After(end); ts = ts.Add(time.Hour) {
		ms := ts.UnixMilli()
		hl = append(hl, map[string]any{"t": ms, "T": ms + 3_599_999, "s": "ETH", "i": "1h",
			"o": "100", "h": "120", "l": "90", "c": "105", "v": "3", "n": 1})
		ccxtRows = append(ccxtRows, []any{ms, 100.0, 120.0, 90.0, 105.0, 3.0})
	}
	doc := map[string]any{"hl": hl, "hl_page_cap": 25, "ccxt": map[string]any{"BTC/USDT|1h": ccxtRows}, "ccxt_page_cap": 25,
		"log": filepath.Join(t.TempDir(), "calls.log")}
	blob, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "fixture.json")
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PYTHONPATH", filepath.Join(root, "shared_scripts", "testdata", "candle_range_offline"))
	t.Setenv("CANDLE_RANGE_FIXTURE", path)
	t.Setenv("GO_TRADER_HL_OHLCV_CACHE", "0")
	t.Setenv("HYPERLIQUID_SECRET_KEY", "")
}

func TestTradeReviewRealCandleScriptRangeContract(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, ".venv", "bin", "python3")); err != nil {
		t.Skipf("repository virtualenv missing (%v); run uv sync", err)
	}
	cases := []struct {
		name   string
		sc     StrategyConfig
		symbol string
		source string
	}{
		{"hyperliquid", trConfig(), "ETH", "hyperliquid:adapter:range"},
		{"binanceus", StrategyConfig{ID: trSID, Type: "spot", Platform: "binanceus", Symbol: "BTC/USDT", Timeframe: "1h",
			Args: []string{"momentum", "BTC/USDT", "1h", "--mode=paper"}}, "BTC/USDT", "binanceus:ccxt:range"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newTREnvWith(t, "", singleFileStore(openTestDB(t)), []StrategyConfig{c.sc})
			e.ss.candleFetcher = FetchUICandles
			trITFixture(t, root, e.at(-30), e.at(40))
			t.Chdir(root)
			e.seedClosedLong(trSID, "pos-it", c.symbol, e.at(2.2), e.at(6.1))
			rec, resp := e.review(trSID, "pos-it")
			if rec.Code != http.StatusOK {
				t.Fatalf("review = %d: %s", rec.Code, rec.Body.String())
			}
			if resp.CandleSource != c.source || resp.Coverage.Status != tradeReviewCoverageFull || !resp.SaveAvailable {
				t.Fatalf("source=%q coverage=%+v save=%v reason=%q", resp.CandleSource, resp.Coverage, resp.SaveAvailable, resp.Reason)
			}
			if got, want := resp.Candles[0].Time, e.at(-18).Unix(); got != want {
				t.Fatalf("first bar opening time = %d, want %d", got, want)
			}
			if got, want := resp.Candles[len(resp.Candles)-1].Time, e.at(26).Unix(); got != want {
				t.Fatalf("last bar opening time = %d, want %d", got, want)
			}
			post := e.post(trSID, e.validBody("pos-it", fmt.Sprintf("%s label", c.name)), nil)
			if post.Code != http.StatusOK {
				t.Fatalf("save = %d: %s", post.Code, post.Body.String())
			}
			if n := trFeedbackRows(t, e.store.primary()); n != 1 {
				t.Fatalf("rows = %d, want 1", n)
			}
		})
	}
}
