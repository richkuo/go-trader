package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sync"
	"testing"
	"time"
)

func paperFundingFeedStrategies() []StrategyConfig {
	return []StrategyConfig{
		{ID: "hl-paper-eth", Type: "perps", Platform: "hyperliquid", Script: hyperliquidCheckScript,
			Args:  []string{"sma_crossover", "ETH", "1h", "--mode=paper"},
			Hedge: &HedgeConfig{Enabled: true, Symbol: "BTC"}},
		{ID: "hl-paper-manual", Type: "manual", Platform: "hyperliquid", Script: hyperliquidCheckScript,
			Symbol: "SOL", Timeframe: "1h", Args: []string{"hold", "SOL", "1h", "--mode=paper"}},
		{ID: "hl-live-hype", Type: "perps", Platform: "hyperliquid", Script: hyperliquidCheckScript,
			Args: []string{"sma_crossover", "HYPE", "1h", "--mode=live"}},
	}
}

func withFundingHistoryFn(t *testing.T, fn func(ctx context.Context, coin string, startMs int64) ([]feedFundingRecord, error)) {
	t.Helper()
	prev := fetchHyperliquidFundingHistoryFn
	fetchHyperliquidFundingHistoryFn = fn
	t.Cleanup(func() { fetchHyperliquidFundingHistoryFn = prev })
}

func TestPaperFundingFetchRejectsConflictsAndReturnsCoverage(t *testing.T) {
	now := time.UnixMilli(10 * 3_600_000).UTC()
	pages := map[int64][]feedFundingRecord{
		3_600_000: {{TimeMs: 3_600_000, Rate: 0.0001}, {TimeMs: 7_200_000, Rate: 0.0002}},
		7_200_001: {{TimeMs: 7_200_000, Rate: 0.0002}, {TimeMs: 10_800_000, Rate: -0.0001}},
	}
	withFundingHistoryFn(t, func(_ context.Context, _ string, startMs int64) ([]feedFundingRecord, error) {
		return pages[startMs], nil
	})
	cov, err := hlFundingRecordsCoverage(context.Background(), "ETH", 3_600_000, now, 8)
	if err != nil {
		t.Fatalf("coverage: %v", err)
	}
	if cov.FromMs != 3_600_000 || cov.ToMs != 10_800_000 || len(cov.Records) != 3 {
		t.Fatalf("coverage %+v, want from 3600000 to 10800000 with 3 records (identical duplicate collapsed)", cov)
	}

	withFundingHistoryFn(t, func(_ context.Context, _ string, startMs int64) ([]feedFundingRecord, error) {
		if startMs == 3_600_000 {
			return []feedFundingRecord{{TimeMs: 3_600_000, Rate: 0.0001}, {TimeMs: 7_200_000, Rate: 0.0002}}, nil
		}
		return []feedFundingRecord{{TimeMs: 7_200_000, Rate: 0.0009}, {TimeMs: 10_800_000, Rate: 0.0001}}, nil
	})
	if _, err := hlFundingRecordsCoverage(context.Background(), "ETH", 3_600_000, now, 8); err == nil {
		t.Fatal("two records at one time with different rates must be refused before deduplication")
	}
	if _, err := hlFundingRecordsSince(context.Background(), "ETH", 3_600_000, now); err == nil {
		t.Fatal("the signal pager must refuse a conflicting duplicate too")
	}

	withFundingHistoryFn(t, func(_ context.Context, _ string, _ int64) ([]feedFundingRecord, error) {
		return []feedFundingRecord{{TimeMs: 3_600_000, Rate: math.NaN()}}, nil
	})
	if _, err := hlFundingRecordsCoverage(context.Background(), "ETH", 3_600_000, now, 8); err == nil {
		t.Fatal("a non-finite rate must be refused")
	}

	withFundingHistoryFn(t, func(_ context.Context, _ string, _ int64) ([]feedFundingRecord, error) {
		return nil, nil
	})
	empty, err := hlFundingRecordsCoverage(context.Background(), "ETH", 3_600_000, now, 8)
	if err != nil || empty.ToMs != 0 || len(empty.Records) != 0 || empty.FromMs != 3_600_000 {
		t.Fatalf("an empty page asserts nothing past its start: %+v, %v", empty, err)
	}
}

func TestPaperFundingFeedRequirementsCarryAccountingCoinsApartFromSignalFunding(t *testing.T) {
	cfg := &Config{Strategies: paperFundingFeedStrategies()}
	req, err := deriveFeedRequirements(cfg)
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	want := []string{"BTC", "ETH", "SOL"}
	if fmt.Sprint(req.AccountingCoins) != fmt.Sprint(want) {
		t.Fatalf("accounting coins %v, want %v (paper perps, its hedge coin and paper manual; never the live coin)", req.AccountingCoins, want)
	}
	if len(req.Funding) != 0 {
		t.Fatalf("the accounting need must not add a signal funding need: %+v", req.Funding)
	}
	cycle := cycleRequirementsForDue(nil, req)
	if fmt.Sprint(cycle.AccountingCoins) != fmt.Sprint(want) || len(cycle.Funding) != 0 {
		t.Fatalf("a cycle with no due strategy must still carry the accounting need and no signal funding: %+v", cycle)
	}
	full := fullCycleRequirements(req)
	if fmt.Sprint(full.AccountingCoins) != fmt.Sprint(want) {
		t.Fatalf("the sealer cycle requirements must carry the accounting need: %+v", full.AccountingCoins)
	}

	liveOnly, err := deriveFeedRequirements(&Config{Strategies: paperFundingFeedStrategies()[2:]})
	if err != nil {
		t.Fatal(err)
	}
	if len(liveOnly.AccountingCoins) != 0 {
		t.Fatalf("a live-only config must carry no accounting need: %v", liveOnly.AccountingCoins)
	}
	union := unionFeedRequirements([]feedConsumer{
		{Path: "a", Loaded: true, Req: req},
		{Path: "b", Loaded: true, Req: liveOnly},
		{Path: "c", Loaded: false, Req: feedRequirements{AccountingCoins: []string{"DOGE"}}},
	})
	if fmt.Sprint(union.AccountingCoins) != fmt.Sprint(want) {
		t.Fatalf("union accounting coins %v, want %v from loaded consumers only", union.AccountingCoins, want)
	}
	if estimateFeedSealBytes(union) <= estimateFeedSealBytes(unionFeedRequirements([]feedConsumer{{Path: "b", Loaded: true, Req: liveOnly}})) {
		t.Fatal("the seal size estimate must grow with accounting coins")
	}

	desc := &feedDescribe{AccountingFunding: []string{"BTC", "ETH"}}
	for _, k := range req.Order {
		desc.Keys = append(desc.Keys, feedDescribeKey{Host: k.Host, Namespace: k.Namespace, Symbol: k.Symbol, Timeframe: k.Timeframe, Required: req.Keys[k]})
	}
	_, problems := sharedFeedCompatibilityLines("test", []feedEndpointDescription{{Endpoint: sharedFeedEndpoint{Name: "primary", Socket: "/tmp/x"}, Describe: desc}}, req, nil)
	if len(problems) != 1 || !bytes.Contains([]byte(problems[0]), []byte("accounting funding SOL not served")) {
		t.Fatalf("a feed that does not serve an accounting coin must report a coverage gap: %v", problems)
	}
	desc.AccountingFunding = want
	if _, problems := sharedFeedCompatibilityLines("test", []feedEndpointDescription{{Endpoint: sharedFeedEndpoint{Name: "primary", Socket: "/tmp/x"}, Describe: desc}}, req, nil); len(problems) != 0 {
		t.Fatalf("a feed serving every accounting coin must cover this consumer: %v", problems)
	}
}

func TestPaperFundingFeedAccountingWindowRefreshesMergesAndKeepsCoverageOnConflict(t *testing.T) {
	var mu sync.Mutex
	nowMs := int64(30 * 86_400_000)
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return time.UnixMilli(nowMs).UTC() }
	advance := func(d time.Duration) { mu.Lock(); nowMs += d.Milliseconds(); mu.Unlock() }
	hour := int64(3_600_000)
	rateAt := func(t int64) float64 { return float64(t/hour%7-3) * 0.00001 }
	conflictAt := int64(-1)
	var starts []int64
	withFundingHistoryFn(t, func(_ context.Context, coin string, startMs int64) ([]feedFundingRecord, error) {
		mu.Lock()
		end := nowMs
		starts = append(starts, startMs)
		mu.Unlock()
		var out []feedFundingRecord
		first := (startMs + hour - 1) / hour * hour
		for ts := first; ts <= end && len(out) < 500; ts += hour {
			r := rateAt(ts)
			if ts == conflictAt {
				r += 1
			}
			out = append(out, feedFundingRecord{TimeMs: ts, Rate: r})
		}
		return out, nil
	})
	owner := newMarketFeedOwner(clock, t.Logf)
	owner.feedMu.Lock()
	owner.applyAccountingCoinsLocked([]string{"ETH"})
	owner.feedMu.Unlock()
	window := feedAccountingWindow.Milliseconds()

	owner.EnsureAccountingFunding(context.Background())
	first := *owner.accountingFunding["ETH"]
	if starts[0] != nowMs-window || first.FromMs != nowMs-window || first.ToMs != nowMs || first.Err != "" {
		t.Fatalf("first fetch must start at now-W and cover through the last record: start %d entry %+v", starts[0], first)
	}
	owner.EnsureAccountingFunding(context.Background())
	if len(starts) != 1 {
		t.Fatalf("a fetch inside the refresh interval must be skipped, got starts %v", starts)
	}

	advance(2 * time.Hour)
	owner.EnsureAccountingFunding(context.Background())
	second := *owner.accountingFunding["ETH"]
	if starts[1] != first.ToMs-feedAccountingOverlap.Milliseconds() {
		t.Fatalf("incremental fetch start %d, want last record minus overlap %d", starts[1], first.ToMs-feedAccountingOverlap.Milliseconds())
	}
	if second.ToMs != nowMs || second.FromMs != nowMs-window || second.Records[0].TimeMs < second.FromMs {
		t.Fatalf("incremental merge must extend coverage and trim at W: %+v", second.FromMs)
	}
	if err := validateFundingCoverageRecords("ETH", second.FromMs, second.ToMs, second.Records); err != nil {
		t.Fatalf("merged coverage must stay valid: %v", err)
	}

	conflictAt = second.ToMs - hour
	advance(2 * time.Minute)
	owner.EnsureAccountingFunding(context.Background())
	if len(starts) != 2 {
		t.Fatalf("no refresh is due before the next hourly record can exist, got starts %v", starts)
	}
	advance(time.Hour)
	owner.EnsureAccountingFunding(context.Background())
	third := *owner.accountingFunding["ETH"]
	if third.Err == "" || third.ToMs != second.ToMs || len(third.Records) != len(second.Records) {
		t.Fatalf("a changed rate must keep the previous coverage and flag the error: %+v", third.Err)
	}
	alerts := owner.DrainAlerts()
	found := false
	for _, a := range alerts {
		found = found || a.Kind == "accounting_funding_failed"
	}
	if !found {
		t.Fatalf("a refused accounting refresh must raise an alert: %+v", alerts)
	}

	owner.feedMu.Lock()
	owner.applyAccountingCoinsLocked(nil)
	owner.feedMu.Unlock()
	if _, ok := owner.accountingFunding["ETH"]; ok {
		t.Fatal("dropping a coin from the accounting need must drop its window")
	}
}

func TestPaperFundingSealCarriesAccountingCoverageAsV3AndLeavesOtherConfigsUnchanged(t *testing.T) {
	nowMs := int64(40 * 86_400_000)
	clock := func() time.Time { return time.UnixMilli(nowMs).UTC() }
	owner := newMarketFeedOwner(clock, t.Logf)
	owner.feedMu.Lock()
	owner.applyAccountingCoinsLocked([]string{"BTC", "ETH"})
	owner.accountingFunding["ETH"] = &feedAccountingFunding{
		FromMs: nowMs - 7_200_000, ToMs: nowMs - 3_600_000,
		Records:   []feedFundingRecord{{TimeMs: nowMs - 7_200_000, Rate: 0.0001}, {TimeMs: nowMs - 3_600_000, Rate: 0}},
		FetchedAt: time.UnixMilli(nowMs).UTC(),
	}
	owner.feedMu.Unlock()
	reqs := cycleMarketRequirements{AccountingCoins: []string{"BTC", "ETH"}}
	snap := freezeMarketSnapshot(owner, reqs, "feed/1", time.UnixMilli(nowMs), time.UnixMilli(nowMs))
	doc, err := feedSealDocFromSnapshot(snap, nowMs/1000, feedSourceWebsocket, "inst")
	if err != nil {
		t.Fatal(err)
	}
	if doc.V != feedSealVersionAccounting || len(doc.AccountingFunding) != 2 {
		t.Fatalf("a seal with accounting coverage must be v%d with one entry per coin: v%d %+v", feedSealVersionAccounting, doc.V, doc.AccountingFunding)
	}
	blob, _, err := encodeFeedSeal(doc)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeFeedSeal(blob, nowMs/1000)
	if err != nil {
		t.Fatalf("decode v3 seal: %v", err)
	}
	consumer := decoded.snapshot()
	cov, detail, ok := consumer.accountingCoverage("ETH")
	if !ok || detail != "" || cov.FromMs != nowMs-7_200_000 || cov.ToMs != nowMs-3_600_000 || len(cov.Records) != 2 || cov.Records[1].Rate != 0 {
		t.Fatalf("sealed ETH coverage %+v detail %q ok %t", cov, detail, ok)
	}
	if _, detail, ok := consumer.accountingCoverage("BTC"); !ok || detail != feedAccountingNotFetched {
		t.Fatalf("an unfetched accounting coin must seal as present with an error and no coverage: %q %t", detail, ok)
	}
	if hold, _ := consumer.fundingHold("ETH", false, false); hold {
		t.Fatal("accounting funding must never create a signal funding hold")
	}

	relabeled := *decoded
	relabeled.V = feedSealVersion
	if b, _, err := encodeFeedSeal(&relabeled); err != nil {
		t.Fatal(err)
	} else if _, err := decodeFeedSeal(b, nowMs/1000); err == nil {
		t.Fatal("a v2 seal carrying accounting funding must be refused")
	}
	tampered := *decoded
	tampered.AccountingFunding = append([]feedSealAccountingFunding(nil), decoded.AccountingFunding...)
	tampered.AccountingFunding[1].ToMs = nowMs
	if b, _, err := encodeFeedSeal(&tampered); err != nil {
		t.Fatal(err)
	} else if _, err := decodeFeedSeal(b, nowMs/1000); err == nil {
		t.Fatal("a seal whose to_ms is not its last record time must be refused")
	}

	plainReq, err := deriveFeedRequirements(&Config{Strategies: paperFundingFeedStrategies()[2:]})
	if err != nil {
		t.Fatal(err)
	}
	plainSnap := freezeMarketSnapshot(owner, fullCycleRequirements(plainReq), "feed/2", time.UnixMilli(nowMs), time.UnixMilli(nowMs))
	plainDoc, err := feedSealDocFromSnapshot(plainSnap, nowMs/1000, feedSourceWebsocket, "inst")
	if err != nil {
		t.Fatal(err)
	}
	plainBlob, _, err := encodeFeedSeal(plainDoc)
	if err != nil {
		t.Fatal(err)
	}
	if plainDoc.V != feedSealVersionBase || bytes.Contains(plainBlob, []byte("accounting_funding")) {
		t.Fatalf("a config without eligible strategies must seal v1 bytes with no accounting field: v%d", plainDoc.V)
	}
	sealer := newFeedSealer(owner, feedSourceWebsocket, "inst", clock, t.Logf)
	sealer.setGeneration(plainReq, []int{3600}, time.UnixMilli(nowMs))
	descBlob, err := json.Marshal(sealer.describe())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(descBlob, []byte("accounting")) || sealer.coverageSealVersion() != feedSealVersionBase {
		t.Fatalf("describe for a config without eligible strategies must stay unchanged: %s", descBlob)
	}
	paperReq, err := deriveFeedRequirements(&Config{Strategies: paperFundingFeedStrategies()})
	if err != nil {
		t.Fatal(err)
	}
	sealer.setGeneration(paperReq, []int{3600}, time.UnixMilli(nowMs))
	if d := sealer.describe(); fmt.Sprint(d.AccountingFunding) != "[BTC ETH SOL]" || d.AccountingWindowMs != feedAccountingWindow.Milliseconds() || sealer.coverageSealVersion() != feedSealVersionAccounting {
		t.Fatalf("describe must name the accounting coins and window: %+v", d)
	}
}
