package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

var quoteReplayTestStart = time.Date(2026, 10, 20, 12, 50, 0, 0, time.UTC).UnixMilli()

type quoteReplayMarket struct {
	seed        int64
	startMs     int64
	endMs       int64
	mid         func(ms int64, base float64) float64
	sweepEvery  int64
	sweepDepth  float64
	tradeSz     string
	bookEvery   int64
	gaps        [][2]int64
	tradeGaps   [][2]int64
	dropBooks   [][2]int64
	fundingRate string
}

func defaultQuoteReplayMarket() quoteReplayMarket {
	return quoteReplayMarket{seed: 1861, startMs: quoteReplayTestStart - 120_000, endMs: quoteReplayTestStart + 20*60_000,
		sweepEvery: 7_000, sweepDepth: 120, tradeSz: "0.5", bookEvery: 500, fundingRate: "0.0000125"}
}

func quoteReplayWalk(seed int64) func(int64, float64) float64 {
	cache := map[int64]float64{}
	return func(ms int64, base float64) float64 {
		sec := ms / 1000
		if v, ok := cache[sec]; ok {
			return v
		}
		r := rand.New(rand.NewSource(seed + sec))
		v := base + 40*math.Sin(float64(sec)/90) + float64(r.Intn(21)-10)
		cache[sec] = v
		return v
	}
}

func quoteReplayIn(ranges [][2]int64, ms int64) bool {
	for _, g := range ranges {
		if g[0] <= ms && ms <= g[1] {
			return true
		}
	}
	return false
}

func (m quoteReplayMarket) build() ([]quoteReplayBookSpec, []quoteReplayTradeSpec, []quoteReplayGapSpec, []quoteReplayRangeSpec) {
	mid := m.mid
	if mid == nil {
		mid = quoteReplayWalk(m.seed)
	}
	var books []quoteReplayBookSpec
	var seq int64
	for t := m.startMs; t <= m.endMs; t += m.bookEvery {
		if quoteReplayIn(m.gaps, t) || quoteReplayIn(m.dropBooks, t) {
			continue
		}
		c := math.Round(mid(t, 100000))
		var bids, asks [][3]string
		for i := 0; i < 20; i++ {
			bids = append(bids, [3]string{fmt.Sprintf("%d", int64(c)-5-int64(i)*10), fmt.Sprintf("%.5f", 0.2+0.05*float64(i)), "3"})
			asks = append(asks, [3]string{fmt.Sprintf("%d", int64(c)+5+int64(i)*10), fmt.Sprintf("%.5f", 0.2+0.05*float64(i)), "3"})
		}
		seq++
		books = append(books, quoteReplayBookSpec{RecvMs: t + 20, EventMs: t, Session: 1, Seq: seq, Identity: fmt.Sprintf("b%d", seq), Bids: bids, Asks: asks})
	}
	var trades []quoteReplayTradeSpec
	var tid uint64 = 1_000_000
	for t := m.startMs + 250; t <= m.endMs; t += 1000 {
		if quoteReplayIn(m.tradeGaps, t) {
			continue
		}
		c := math.Round(mid(t, 100000))
		tid++
		side, px := "B", int64(c)+5
		if (t/1000)%2 == 0 {
			side, px = "A", int64(c)-5
		}
		if m.sweepEvery > 0 && (t-m.startMs)%m.sweepEvery < 1000 {
			if ((t-m.startMs)/m.sweepEvery)%2 == 0 {
				side, px = "A", int64(c-m.sweepDepth)
			} else {
				side, px = "B", int64(c+m.sweepDepth)
			}
		}
		trades = append(trades, quoteReplayTradeSpec{RecvMs: t + 30, TimeMs: t, Tid: tid, Side: side, Px: fmt.Sprintf("%d", px), Sz: m.tradeSz, Hash: fmt.Sprintf("0x%x", tid)})
	}
	var gaps []quoteReplayGapSpec
	for _, g := range m.gaps {
		gaps = append(gaps, quoteReplayGapSpec{FromMs: g[0], ToMs: g[1], Reason: "disconnected"})
	}
	cov := []quoteReplayRangeSpec{}
	from := m.startMs
	for _, g := range m.tradeGaps {
		cov = append(cov, quoteReplayRangeSpec{FromMs: from, ToMs: g[0] - 1})
		from = g[1] + 1
	}
	cov = append(cov, quoteReplayRangeSpec{FromMs: from, ToMs: m.endMs + 1000})
	return books, trades, gaps, cov
}

func quoteReplayTestParams() map[string]interface{} {
	p := map[string]interface{}{}
	for _, f := range quotePolicySpec.Params {
		v, _ := f.Default.Float64()
		p[f.Name] = v
	}
	p["volatility_window_seconds"] = 60.0
	p["quote_lifetime_seconds"] = 20.0
	p["max_inventory_age_seconds"] = 120.0
	return p
}

func quoteReplayTestSpec(t *testing.T, dir string, m quoteReplayMarket, mutate func(*quoteReplaySpec)) *quoteReplaySpec {
	t.Helper()
	books, trades, gaps, cov := m.build()
	spec := &quoteReplaySpec{
		Protocol: quoteReplayProtocolName, Version: quoteReplayProtocolVersion, Mode: basketReplayModeNew,
		Run:     basketReplayRunSpec{RunID: "qr-test", StateDir: filepath.Join(dir, "run"), Source: map[string]string{"fixture": "synthetic market for integration tests"}},
		Coin:    "BTC",
		Window:  quoteReplayWindow{FirstDeadlineMs: quoteReplayTestStart, LastDeadlineMs: quoteReplayTestStart + 15*60_000, Label: "fixture", MaxDeadlines: 1000},
		Params:  quoteReplayTestParams(),
		Config:  quoteReplayConfigSpec{IntervalSeconds: 5, MaxInventoryUSD: 1000, StopLossPct: 2},
		Quote:   quoteReplayQuoteSpec{MaxOrderQty: 0.01, RequestExpiryMs: 2000, FeeReserveRate: 0.0005, MarginPriceBufferPct: 1},
		Lot:     quoteReplayLotSpec{SzDecimals: 5},
		Costs:   quoteReplayCostSpec{MakerFeeBps: "1.5", TakerFeeBps: "4.5", SlippageReserveBps: "5", MinOrderValueUSD: "10"},
		Latency: quoteReplayLatencySpec{PlaceMs: 50, CancelMs: 50},
		Queue:   quoteReplayQueueSpec{Model: quoteQueueModelStrict, Participation: "1/100"},
		Funding: quoteReplayFundingSpec{FromMs: quoteReplayTestStart - 3_600_000},
		Books:   books, BookGaps: gaps, Trades: trades, TradeCov: cov,
		Scoring: quoteReplayScoringSpec{Scored: true},
	}
	first := spec.Funding.FromMs - spec.Funding.FromMs%3_600_000
	if first < spec.Funding.FromMs {
		first += 3_600_000
	}
	for h := first; h <= m.endMs; h += 3_600_000 {
		spec.Funding.Records = append(spec.Funding.Records, quoteReplayFundingRow{TimeMs: h, Rate: m.fundingRate})
	}
	if mutate != nil {
		mutate(spec)
	}
	return spec
}

func writeQuoteReplaySpec(t *testing.T, spec *quoteReplaySpec, path string) {
	t.Helper()
	blob, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		t.Fatal(err)
	}
}

func quoteReplayTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if eval, err := filepath.EvalSymlinks(dir); err == nil {
		dir = eval
	}
	return dir
}

func runQuoteReplayForTest(t *testing.T, spec *quoteReplaySpec, extra ...string) (quoteReplayOutput, int) {
	t.Helper()
	path := filepath.Join(t.TempDir(), fmt.Sprintf("plan-%s-%d.json", spec.Mode, time.Now().UnixNano()))
	writeQuoteReplaySpec(t, spec, path)
	var out bytes.Buffer
	code := runQuoteReplayWith(append([]string{"--spec", path}, extra...), strings.NewReader(""), &out)
	var res quoteReplayOutput
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("quote-replay output is not JSON (exit %d): %v\n%s", code, err, out.String())
	}
	return res, code
}

func quoteReplayHelperBinary(t *testing.T) string {
	t.Helper()
	return os.Args[0]
}

func runQuoteReplaySubprocess(t *testing.T, spec *quoteReplaySpec, extra ...string) (quoteReplayOutput, int, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), fmt.Sprintf("plan-%s-%d.json", spec.Mode, time.Now().UnixNano()))
	writeQuoteReplaySpec(t, spec, path)
	cmd := exec.Command(quoteReplayHelperBinary(t), "-test.run=^TestQuoteReplayHelperProcess$")
	cmd.Env = append(quoteReplayCleanEnv(), "GO_TRADER_QUOTE_REPLAY_HELPER=1", "GO_TRADER_QUOTE_REPLAY_ARGS="+strings.Join(append([]string{"--spec", path}, extra...), "\x1f"))
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("helper process: %v", err)
	}
	var res quoteReplayOutput
	text := stdout.String()
	if i := strings.Index(text, "{\"protocol\""); i >= 0 {
		line := text[i:]
		if j := strings.IndexByte(line, '\n'); j >= 0 {
			line = line[:j]
		}
		_ = json.Unmarshal([]byte(line), &res)
	}
	return res, code, stderr.String()
}

func quoteReplayCleanEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		name := quoteReplayEnvName(kv)
		upper := strings.ToUpper(name)
		skip := false
		for _, p := range quoteReplayRefusedEnvPrefixes {
			if strings.HasPrefix(upper, p) {
				skip = true
			}
		}
		if !skip {
			out = append(out, kv)
		}
	}
	return out
}

func TestQuoteReplayHelperProcess(t *testing.T) {
	if os.Getenv("GO_TRADER_QUOTE_REPLAY_HELPER") != "1" {
		return
	}
	args := strings.Split(os.Getenv("GO_TRADER_QUOTE_REPLAY_ARGS"), "\x1f")
	protocolOut := os.Stdout
	os.Stdout = os.Stderr
	code := runQuoteReplayProcess(args, strings.NewReader(""), protocolOut)
	os.Exit(code)
}

func quoteReplayComparable(t *testing.T, out quoteReplayOutput) map[string]string {
	t.Helper()
	enc := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	var results []quoteReplayResultRecord
	for _, r := range out.Results {
		var rec quoteReplayResultRecord
		_ = json.Unmarshal(r.Result, &rec)
		results = append(results, rec)
	}
	var decisions []map[string]any
	for _, raw := range out.Decisions {
		var rec map[string]any
		if err := json.Unmarshal(raw, &rec); err != nil {
			t.Fatal(err)
		}
		if fp, ok := rec["fingerprints"].(map[string]any); ok {
			delete(fp, "config")
		}
		if v, ok := rec["verdict"].(map[string]any); ok {
			delete(v, "config_fingerprint")
		}
		decisions = append(decisions, rec)
	}
	var events []string
	for _, e := range out.VenueEvents {
		events = append(events, fmt.Sprintf("%d|%s|%s|%d|%s", e.Seq, e.Kind, e.ClientID, e.EffectMs, string(e.Event)))
	}
	nonce := regexp.MustCompile(`(quote-replay-owner:[A-Z]+:[0-9]+):[0-9]+`)
	strip := func(s string) string { return nonce.ReplaceAllString(s, "$1") }
	return map[string]string{
		"decisions":   enc(decisions),
		"results":     enc(results),
		"events":      enc(events),
		"fills":       enc(out.Fills),
		"settlements": enc(out.Settlements),
		"book":        strip(enc(out.Book)),
		"boundary":    enc(out.AppliedBoundary),
		"cursor":      enc(out.CursorMs),
		"trades":      strip(enc(out.Trades)),
	}
}
