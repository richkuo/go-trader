package main

import (
	"bytes"
	"encoding/json"
	"fmt"
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
