//go:build pyintegration

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

func trITPublish(t *testing.T, root, archive string, sc StrategyConfig, symbol string, from, to time.Time) string {
	t.Helper()
	py := filepath.Join(root, ".venv", "bin", "python3")
	rows := filepath.Join(t.TempDir(), "rows.json")
	fetch := exec.Command(py, "scripts/candle-archive-publish.py", "fetch", "--platform", sc.Platform, "--type", sc.Type,
		"--symbol", symbol, "--timeframe", "1h", "--from", from.UTC().Format(time.RFC3339), "--to", to.UTC().Format(time.RFC3339),
		"--egress-id", "pyintegration", "--max-requests", "10", "--out", rows)
	fetch.Dir = root
	if out, err := fetch.CombinedOutput(); err != nil {
		t.Fatalf("publisher fetch: %v: %s", err, out)
	}
	build := exec.Command(py, "scripts/candle-archive-publish.py", "build", "--archive-dir", archive, "--publisher-id", "pyintegration",
		"--expires-hours", "2", "--min-close-age-seconds", "0", rows)
	build.Dir = root
	out, err := build.CombinedOutput()
	if err != nil {
		t.Fatalf("publisher build: %v: %s", err, out)
	}
	var res struct {
		OK         bool   `json:"ok"`
		Generation string `json:"generation"`
	}
	if err := json.Unmarshal(out, &res); err != nil || !res.OK || res.Generation == "" {
		t.Fatalf("publisher build output %s (%v)", out, err)
	}
	return res.Generation
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
			e.noArchive = true
			trITFixture(t, root, e.at(-30), e.at(40))
			gen := trITPublish(t, root, e.archiveDir, c.sc, c.symbol, e.at(-30), e.at(40))
			attachTestCandleArchive(t, e.ss, e.archiveDir)
			if got := e.ss.candles.archive.generation(); got == nil || got.ID != gen {
				t.Fatalf("the server did not admit the published generation %s: %+v", gen, e.ss.candles.archive.status())
			}
			t.Chdir(root)
			e.seedClosedLong(trSID, "pos-it", c.symbol, e.at(2.2), e.at(6.1))
			rec, resp := e.review(trSID, "pos-it")
			if rec.Code != http.StatusOK {
				t.Fatalf("review = %d: %s", rec.Code, rec.Body.String())
			}
			if want := "archive:" + gen + ":" + c.source; resp.CandleSource != want || resp.Coverage.Status != tradeReviewCoverageFull || !resp.SaveAvailable {
				t.Fatalf("source=%q (want %q) coverage=%+v save=%v reason=%q", resp.CandleSource, want, resp.Coverage, resp.SaveAvailable, resp.Reason)
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
			var raw string
			if err := e.store.primary().db.QueryRow(`SELECT evidence_json FROM trade_feedback`).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var ev tradeFeedbackEvidence
			if err := json.Unmarshal([]byte(raw), &ev); err != nil {
				t.Fatal(err)
			}
			if ev.Version != tradeFeedbackEvidenceVersion || ev.Candles.Source != resp.CandleSource || int64(len(ev.Candles.Bars)) != ev.Window.ExpectedBars {
				t.Fatalf("evidence = version %d source %q bars %d/%d", ev.Version, ev.Candles.Source, len(ev.Candles.Bars), ev.Window.ExpectedBars)
			}
		})
	}
}

func TestCandleArchivePublisherGoldenParity(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	py := filepath.Join(root, ".venv", "bin", "python3")
	if _, err := os.Stat(py); err != nil {
		t.Skipf("repository virtualenv missing (%v); run uv sync", err)
	}
	golden := filepath.Join(root, "shared_tools", "testdata", "candle_archive")
	want, err := readCandleArchiveCurrent(filepath.Join(golden, "archive"))
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	args := []string{"scripts/candle-archive-publish.py", "build", "--archive-dir", out, "--publisher-id", "golden", "--expires-hours", "876000",
		"--min-close-age-seconds", "300", "--chunk-bars", "16", "--keep", "0", "--now-ms", "1767542400000"}
	for _, name := range []string{"eth-1h.json", "eth-4h.json", "btc-1h.json"} {
		args = append(args, filepath.Join(golden, "rows", name))
	}
	cmd := exec.Command(py, args...)
	cmd.Dir = root
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("publisher build: %v: %s", err, b)
	}
	got, err := readCandleArchiveCurrent(out)
	if err != nil || got != want {
		t.Fatalf("the Python writer built generation %q (%v), the golden generation is %q", got, err, want)
	}
	for _, rel := range []string{"manifest.json", "series/s0000.bars", "series/s0001.bars", "series/s0002.bars"} {
		a, errA := os.ReadFile(filepath.Join(golden, "archive", "generations", want, rel))
		b, errB := os.ReadFile(filepath.Join(out, "generations", got, rel))
		if errA != nil || errB != nil || string(a) != string(b) {
			t.Fatalf("%s differs between the golden generation and the rebuild (%v %v)", rel, errA, errB)
		}
	}
	for _, dir := range []string{filepath.Join(golden, "archive"), out} {
		gen, err := loadCandleArchiveGeneration(context.Background(), dir, want, candleArchiveLimits{}, time.Now())
		if err != nil {
			t.Fatalf("the Go reader refused %s: %v", dir, err)
		}
		if len(gen.series) != 3 || gen.lookup(hlTestIdentity("ETH", "1h")) == nil || gen.lookup(hlTestIdentity("ETH", "4h")) == nil {
			t.Fatalf("the Go reader admitted %d series from %s", len(gen.series), dir)
		}
		btc := gen.lookup(candleIdentity{Venue: "binanceus", Network: candleNetworkLive, InstrumentClass: candleClassSpot, Symbol: "BTC/USDT", Timeframe: "1h"})
		if btc == nil || len(btc.Segments) != 2 || btc.Bars != 68 {
			t.Fatalf("the BTC series with a declared gap was not admitted as two segments: %+v", btc)
		}
		gen.retire()
	}
	bin := pyintegrationBinary(t)
	verify := exec.Command(bin, "candle-archive", "verify", "--dir", filepath.Join(golden, "archive"))
	if b, err := verify.CombinedOutput(); err != nil || !strings.Contains(string(b), want) {
		t.Fatalf("candle-archive verify on the golden generation: %v: %s", err, b)
	}
	flipped := t.TempDir()
	cp := exec.Command("cp", "-R", filepath.Join(golden, "archive")+"/.", flipped)
	if b, err := cp.CombinedOutput(); err != nil {
		t.Fatalf("copy golden: %v: %s", err, b)
	}
	series := filepath.Join(flipped, "generations", want, "series", "s0000.bars")
	data, err := os.ReadFile(series)
	if err != nil {
		t.Fatal(err)
	}
	data[60] ^= 1
	if err := os.WriteFile(series, data, 0o644); err != nil {
		t.Fatal(err)
	}
	verify = exec.Command(bin, "candle-archive", "verify", "--dir", flipped)
	b, err := verify.CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 1 || !strings.Contains(string(b), "hyperliquid|mainnet|perps|ETH|1h") {
		t.Fatalf("candle-archive verify on a flipped byte = %v: %s", err, b)
	}
}
