package main

import (
	"context"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	pfHour = int64(3_600_000)
	pfT0   = int64(1_800_000_000_000)
)

type pfFixture struct {
	t     *testing.T
	clock *paperFundingTestClock
	cfg   *Config
	store *StateStore
	state *AppState
}

func pfPaperPerps(id, coin string) StrategyConfig {
	return StrategyConfig{ID: id, Type: "perps", Platform: "hyperliquid", Args: []string{"sma", coin, "1h", "--mode=paper"}, Capital: 10000}
}

func newPFFixture(t *testing.T, split bool, strategies ...StrategyConfig) *pfFixture {
	t.Helper()
	prevEpisodes := paperFundingEpisodes
	paperFundingEpisodes = make(map[string]bool)
	t.Cleanup(func() { paperFundingEpisodes = prevEpisodes })
	dir := t.TempDir()
	cfg := &Config{DBFile: filepath.Join(dir, "live.db"), Strategies: strategies}
	if split {
		cfg.PaperDBFile = filepath.Join(dir, "paper.db")
	}
	f := &pfFixture{t: t, clock: withPaperFundingClock(t, pfT0+10*60_000), cfg: cfg}
	f.store = openSplitStore(t, cfg)
	f.state = paperFundingFreshState(cfg)
	return f
}

func (f *pfFixture) sync() {
	syncPaperFundingEligibility(f.state, f.cfg.Strategies, f.clock.now())
}

func (f *pfFixture) at(ms int64) { f.clock.set(ms) }

func pfCoverage(coin string, fromMs int64, recs ...feedFundingRecord) feedFundingCoverage {
	cov := feedFundingCoverage{Coin: coin, FromMs: fromMs, Records: recs}
	if n := len(recs); n > 0 {
		cov.ToMs = recs[n-1].TimeMs
	}
	return cov
}

func (f *pfFixture) run(atMs int64, marks map[string]float64, covs ...feedFundingCoverage) paperFundingRunResult {
	f.t.Helper()
	f.at(atMs)
	in := paperFundingInputs{Source: paperFundingSourceREST, Coverage: map[string]feedFundingCoverage{}, Detail: map[string]string{}, NotServed: map[string]bool{}}
	for _, c := range covs {
		in.Coverage[c.Coin] = c
	}
	return runPaperFundingAccounting(f.state, f.cfg.Strategies, f.store, in, marks, func(p RiskPartition) bool { return false }, f.clock.now())
}

func (f *pfFixture) save() {
	f.t.Helper()
	for part, err := range f.store.SaveAll(f.state) {
		if err != nil {
			f.t.Fatalf("SaveAll(%s): %v", partitionLabel(part), err)
		}
	}
}

func (f *pfFixture) reload() {
	f.t.Helper()
	st, _, err := LoadStateWithStore(f.cfg, f.store)
	if err != nil {
		f.t.Fatal(err)
	}
	ValidateState(st, f.cfg.Strategies)
	f.state = st
	f.sync()
}

func pfFundingRows(s *StrategyState) []Trade {
	var out []Trade
	for _, tr := range s.TradeHistory {
		if tr.TradeType == TradeTypeFunding {
			out = append(out, tr)
		}
	}
	return out
}

func pfOpen(t *testing.T, s *StrategyState, coin string, signal int, qty, px float64) {
	t.Helper()
	if n, err := ExecutePerpsSignalWithLeverage(s, signal, coin, px, PerpsSizing{SizingLeverage: 1, ExchangeLeverage: 1}, qty, "", 0, DirectionBoth, 0, silentStrategyLogger(s.ID)); err != nil || n == 0 {
		t.Fatalf("open %s %d: n=%d err=%v", coin, signal, n, err)
	}
}

type pfDBRow struct {
	oid, pid, tradeType string
	pnl, fee            float64
}

func pfDBRows(t *testing.T, sdb *StateDB, storageID string) []pfDBRow {
	t.Helper()
	rows, err := sdb.db.Query("SELECT exchange_order_id, position_id, trade_type, realized_pnl, exchange_fee FROM trades WHERE strategy_id = ? ORDER BY rowid", storageID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []pfDBRow
	for rows.Next() {
		var r pfDBRow
		if err := rows.Scan(&r.oid, &r.pid, &r.tradeType, &r.pnl, &r.fee); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func pfAssertCashIdentity(t *testing.T, sdb *StateDB, s *StrategyState) {
	t.Helper()
	sum := s.InitialCapital
	keys := map[string]int{}
	for _, r := range pfDBRows(t, sdb, s.ID) {
		sum += r.pnl - r.fee
		if r.tradeType == TradeTypeFunding {
			keys[r.oid]++
		}
	}
	for k, n := range keys {
		if n != 1 {
			t.Fatalf("funding key %s stored %d times", k, n)
		}
	}
	if math.Abs(sum-s.Cash) > 1e-9 {
		t.Fatalf("cash %.10f != initial capital plus booked ledger %.10f", s.Cash, sum)
	}
}

func TestPaperFundingBooksLongAndShortWithSimulatorSign(t *testing.T) {
	f := newPFFixture(t, true, pfPaperPerps("hl-long", "ETH"), pfPaperPerps("hl-short", "ETH"))
	f.sync()
	long, short := f.state.Strategies["hl-long"], f.state.Strategies["hl-short"]
	pfOpen(t, long, "ETH", 1, 2, 2000)
	pfOpen(t, short, "ETH", -1, 1, 2000)
	longCash, shortCash := long.Cash, short.Cash
	marks := map[string]float64{"ETH": 2000}
	r1 := pfT0 + pfHour
	f.run(r1+2*60_000, marks, pfCoverage("ETH", pfT0, feedFundingRecord{TimeMs: r1, Rate: 0.0001}))
	lr, sr := pfFundingRows(long), pfFundingRows(short)
	if len(lr) != 1 || len(sr) != 1 {
		t.Fatalf("want one funding row each, got %d/%d", len(lr), len(sr))
	}
	if !approxEq(lr[0].RealizedPnL, -0.4) || !approxEq(long.Cash-longCash, -0.4) {
		t.Fatalf("long pays a positive rate: row %v cash delta %v, want -0.4", lr[0].RealizedPnL, long.Cash-longCash)
	}
	if !approxEq(sr[0].RealizedPnL, 0.2) || !approxEq(short.Cash-shortCash, 0.2) {
		t.Fatalf("short receives a positive rate: row %v cash delta %v, want +0.2", sr[0].RealizedPnL, short.Cash-shortCash)
	}
	row := lr[0]
	if row.Side != "funding" || !row.PnLGross || row.Quantity != 0 || row.Price != 0 || row.Value != 0 || row.ExchangeFee != 0 ||
		row.ExchangeOrderID != paperFundingOrderID("ETH", r1) || !row.Timestamp.Equal(time.UnixMilli(r1).UTC()) || row.PositionID != long.Positions["ETH"].TradePositionID || row.Symbol != "ETH" {
		t.Fatalf("funding row must match the live shape: %+v", row)
	}
	r2 := r1 + pfHour
	f.run(r2+60_000, map[string]float64{"ETH": 2500}, pfCoverage("ETH", pfT0, feedFundingRecord{TimeMs: r1, Rate: 0.0001}, feedFundingRecord{TimeMs: r2, Rate: -0.0002}))
	if lr := pfFundingRows(long); len(lr) != 2 || !approxEq(lr[1].RealizedPnL, 2*2500*0.0002) {
		t.Fatalf("second record must value at the first mark after it: %+v", lr)
	}
	f.run(r2+2*60_000, marks, pfCoverage("ETH", pfT0, feedFundingRecord{TimeMs: r1, Rate: 0.0001}, feedFundingRecord{TimeMs: r2, Rate: -0.0002}))
	if len(pfFundingRows(long)) != 2 || len(pfFundingRows(short)) != 2 {
		t.Fatal("a repeated run with the same coverage must book nothing new")
	}
	f.save()
	pfAssertCashIdentity(t, f.store.file(storageRolePaper), long)
	pfAssertCashIdentity(t, f.store.file(storageRolePaper), short)
}

func TestPaperFundingChargesTheQuantityHeldAtTheRecordTime(t *testing.T) {
	hedged := pfPaperPerps("hl-a", "ETH")
	hedged.Hedge = &HedgeConfig{Enabled: true, Symbol: "BTC"}
	f := newPFFixture(t, true, hedged, pfPaperPerps("hl-late", "ETH"))
	f.sync()
	a, late := f.state.Strategies["hl-a"], f.state.Strategies["hl-late"]
	logger := silentStrategyLogger("hl-a")
	pfOpen(t, a, "ETH", 1, 2, 2000)
	firstPID := a.Positions["ETH"].TradePositionID
	marks := map[string]float64{"ETH": 2000, "BTC": 50000}
	f.run(pfT0+30*60_000, marks)

	r1 := pfT0 + pfHour
	f.at(r1)
	if !bookPerpsPartialCloseWithFillFee(a, "ETH", 0.5, 2000, 0, false, "", "test", "test", "test", nil) {
		t.Fatal("partial close")
	}
	f.at(r1 + 1)
	pfOpen(t, late, "ETH", 1, 3, 2000)
	f.at(r1 + 5*60_000)
	if n, tr := applyPerpsScaleIn(a, hedged, "ETH", 2000, 1, 0, "", false, logger); n != 1 {
		t.Fatal("scale-in")
	} else {
		RecordTrade(a, *tr)
	}
	f.at(r1 + 20*60_000)
	applyHedgeFill(hedged, a, "ETH", hedgeAction{Kind: hedgeActionOpen, Side: "sell", HedgeSide: "short", Qty: 0.02, NewBasis: 2.5}, 0.02, 50000, 0, false, "", logger)
	f.at(r1 + 30*60_000)
	if !bookPerpsCloseWithFillFee(a, "ETH", 2000, 0, false, "", "test", "test", "test", nil) {
		t.Fatal("close")
	}
	f.at(r1 + 40*60_000)
	pfOpen(t, a, "ETH", -1, 1, 2000)
	secondPID := a.Positions["ETH"].TradePositionID

	r2 := r1 + pfHour
	covETH := pfCoverage("ETH", pfT0, feedFundingRecord{TimeMs: r1, Rate: 0.0001}, feedFundingRecord{TimeMs: r2, Rate: 0.0001})
	covBTC := pfCoverage("BTC", pfT0, feedFundingRecord{TimeMs: r1, Rate: 0.0001}, feedFundingRecord{TimeMs: r2, Rate: 0.0001})
	f.run(r2+60_000, marks, covETH, covBTC)

	var eth, btc []Trade
	for _, tr := range pfFundingRows(a) {
		if tr.Symbol == "ETH" {
			eth = append(eth, tr)
		} else {
			btc = append(btc, tr)
		}
	}
	if len(eth) != 2 || !approxEq(eth[0].RealizedPnL, -2*2000*0.0001) || eth[0].PositionID != firstPID {
		t.Fatalf("a change at exactly the record time counts as after it: the first ETH record charges qty 2 on the first position: %+v", eth)
	}
	if !approxEq(eth[1].RealizedPnL, 1*2000*0.0001) || eth[1].PositionID != secondPID {
		t.Fatalf("the second ETH record charges the reopened short: %+v", eth)
	}
	if len(btc) != 1 || !approxEq(btc[0].RealizedPnL, 0.02*50000*0.0001) || btc[0].ExchangeOrderID != paperFundingOrderID("BTC", r2) {
		t.Fatalf("the hedge leg pays nothing for a record before it opened and is charged on its own coin after: %+v", btc)
	}
	if lr := pfFundingRows(late); len(lr) != 1 || lr[0].ExchangeOrderID != paperFundingOrderID("ETH", r2) {
		t.Fatalf("a position opened after a record pays nothing for it: %+v", lr)
	}
	f.save()
	pfAssertCashIdentity(t, f.store.file(storageRolePaper), a)
}

func TestPaperFundingKeepsCapturedPositionIDAfterClose(t *testing.T) {
	f := newPFFixture(t, true, pfPaperPerps("hl-a", "ETH"))
	f.sync()
	a := f.state.Strategies["hl-a"]
	pfOpen(t, a, "ETH", 1, 1, 2000)
	pid := a.Positions["ETH"].TradePositionID
	marks := map[string]float64{"ETH": 2000}
	f.run(pfT0+59*60_000, marks)
	r1 := pfT0 + pfHour
	f.run(r1+60_000, marks)
	f.at(r1 + 5*60_000)
	if !bookPerpsCloseWithFillFee(a, "ETH", 2000, 0, false, "", "test", "test", "test", nil) {
		t.Fatal("close")
	}
	f.run(r1+10*60_000, marks, pfCoverage("ETH", pfT0, feedFundingRecord{TimeMs: r1, Rate: 0.0003}))
	rows := pfFundingRows(a)
	if len(rows) != 1 || rows[0].PositionID != pid || a.Positions["ETH"] != nil {
		t.Fatalf("settlement after close must carry the captured position id %s: %+v", pid, rows)
	}
}

func TestPaperFundingExactlyOnceAcrossRestartCrashAndSaveFailures(t *testing.T) {
	f := newPFFixture(t, true, pfPaperPerps("hl-a", "ETH"))
	prevRecorder := tradeRecorder
	tradeRecorder = f.store.InsertTrade
	t.Cleanup(func() { tradeRecorder = prevRecorder })
	f.sync()
	pfOpen(t, f.state.Strategies["hl-a"], "ETH", 1, 2, 2000)
	f.save()
	marks := map[string]float64{"ETH": 2000}
	r1, r2, r3 := pfT0+pfHour, pfT0+2*pfHour, pfT0+3*pfHour
	cov := func(recs ...feedFundingRecord) feedFundingCoverage { return pfCoverage("ETH", pfT0, recs...) }
	recs := []feedFundingRecord{{TimeMs: r1, Rate: 0.0001}, {TimeMs: r2, Rate: 0.0002}, {TimeMs: r3, Rate: -0.0001}}

	f.run(r1+60_000, marks, cov(recs[0]))
	if n := len(pfDBRows(t, f.store.file(storageRolePaper), "hl-a")); n != 1 {
		t.Fatalf("a funding row must not persist eagerly before the partition save (rows=%d, want only the open)", n)
	}
	f.save()
	f.reload()
	f.run(r1+2*60_000, marks, cov(recs[0]))
	if n := len(pfFundingRows(f.state.Strategies["hl-a"])); n != 1 {
		t.Fatalf("restart then rerun must not book again: %d rows", n)
	}

	f.run(r2+60_000, marks, cov(recs[0], recs[1]))
	if n := len(pfFundingRows(f.state.Strategies["hl-a"])); n != 2 {
		t.Fatalf("second record not booked: %d", n)
	}
	f.reload()
	if n := len(pfFundingRows(f.state.Strategies["hl-a"])); n != 1 {
		t.Fatalf("a crash before the save must roll the row back with cash and timeline: %d rows", n)
	}
	f.run(r2+2*60_000, marks, cov(recs[0], recs[1]))
	for i := 0; i < 3; i++ {
		f.run(r3+int64(i+1)*60_000, marks, cov(recs...))
	}
	if n := len(pfFundingRows(f.state.Strategies["hl-a"])); n != 3 {
		t.Fatalf("unsaved cycles must keep each record once in memory: %d rows", n)
	}
	f.save()
	f.reload()
	f.run(r3+10*60_000, marks, cov(recs...))
	s := f.state.Strategies["hl-a"]
	if n := len(pfFundingRows(s)); n != 3 {
		t.Fatalf("after the successful save each record exists once: %d rows", n)
	}
	f.save()
	pfAssertCashIdentity(t, f.store.file(storageRolePaper), s)
	want := 2 * 2000 * (-0.0001 - 0.0002 + 0.0001)
	got := 0.0
	for _, tr := range pfFundingRows(s) {
		got += tr.RealizedPnL
	}
	if !approxEq(got, want) {
		t.Fatalf("booked funding %v, want %v", got, want)
	}
}

func TestPaperFundingOutagesGapsAndAnomaliesNeverBookZero(t *testing.T) {
	f := newPFFixture(t, true, pfPaperPerps("hl-a", "ETH"))
	f.sync()
	a := f.state.Strategies["hl-a"]
	pfOpen(t, a, "ETH", 1, 1, 2000)
	marks := map[string]float64{"ETH": 2000}
	alerts := 0
	for h := int64(1); h <= 4; h++ {
		for _, m := range f.run(pfT0+h*pfHour+60_000, marks).Alerts {
			if containsAll(m, "records_unavailable") {
				alerts++
			}
		}
	}
	if len(pfFundingRows(a)) != 0 || a.PaperFunding.Coins["ETH"].SettledThroughMs > pfT0+10*60_000 {
		t.Fatal("unavailable records must leave exposure pending with no booking")
	}
	again := f.run(pfT0+4*pfHour+3*60_000, marks)
	for _, m := range again.Alerts {
		if containsAll(m, "records_unavailable") {
			t.Fatal("the outage alert must fire once per episode")
		}
	}
	if alerts != 1 {
		t.Fatalf("outage alerts %d, want exactly one per episode", alerts)
	}

	r := func(h int64, rate float64) feedFundingRecord {
		return feedFundingRecord{TimeMs: pfT0 + h*pfHour, Rate: rate}
	}
	gapRes := f.run(pfT0+4*pfHour+4*60_000, marks, pfCoverage("ETH", pfT0, r(1, 0.0001), r(3, 0.0001), r(4, 0)))
	rows := pfFundingRows(a)
	if len(rows) != 2 {
		t.Fatalf("records at hours 1 and 3 book, the confirmed zero rate at hour 4 books no row: %d rows", len(rows))
	}
	c := a.PaperFunding.Coins["ETH"]
	if len(c.Gaps) != 1 || c.SettledThroughMs != pfT0+4*pfHour {
		t.Fatalf("a missing hour between present records is a recorded gap: %+v settled %d", c.Gaps, c.SettledThroughMs)
	}
	gapAlert := false
	for _, m := range gapRes.Alerts {
		gapAlert = gapAlert || containsAll(m, "gap")
	}
	if !gapAlert {
		t.Fatal("a gap must alert")
	}
	f.run(pfT0+4*pfHour+5*60_000, marks, pfCoverage("ETH", pfT0, r(1, 0.0001), r(2, 0.0005), r(3, 0.0001), r(4, 0)))
	rows = pfFundingRows(a)
	if len(rows) != 3 || rows[2].ExchangeOrderID != paperFundingOrderID("ETH", pfT0+2*pfHour) || len(c.Gaps) != 0 {
		t.Fatalf("a late record inside a gap books once and closes the gap: rows %d gaps %+v", len(rows), c.Gaps)
	}
	f.run(pfT0+4*pfHour+6*60_000, marks, pfCoverage("ETH", pfT0, r(1, 0.0001), r(2, 0.0005), r(3, 0.0001), r(4, 0)))
	if len(pfFundingRows(a)) != 3 {
		t.Fatal("the late record must not book twice")
	}

	settled := c.SettledThroughMs
	conflict := pfCoverage("ETH", pfT0, r(5, 0.0001), r(5, 0.0002))
	f.run(pfT0+5*pfHour+60_000, marks, conflict)
	if c.SettledThroughMs != settled || len(pfFundingRows(a)) != 3 {
		t.Fatal("conflicting duplicate records must be refused with no progress")
	}
	close := pfCoverage("ETH", pfT0, feedFundingRecord{TimeMs: pfT0 + 4*pfHour + 20*60_000, Rate: 0.0001}, r(5, 0.0001))
	f.run(pfT0+5*pfHour+2*60_000, marks, close)
	if c.SettledThroughMs != settled || len(pfFundingRows(a)) != 3 {
		t.Fatal("records less than 30 minutes apart must be refused with no progress")
	}
	backlog := pfCoverage("ETH", pfT0+5*pfHour-1000, r(5, 0.0001))
	f.at(pfT0 + 5*pfHour + 3*60_000)
	c.SettledThroughMs = pfT0 + 4*pfHour - 1
	c.LastRecordMs = pfT0 + 3*pfHour
	bres := f.run(pfT0+5*pfHour+3*60_000, marks, backlog)
	backlogAlert := false
	for _, m := range bres.Alerts {
		backlogAlert = backlogAlert || containsAll(m, "backlog", "market_feed")
	}
	if !backlogAlert || c.SettledThroughMs != pfT0+4*pfHour-1 {
		t.Fatalf("exposure older than the coverage start stays pending with an alert naming the remedy: %v", bres.Alerts)
	}
}

func containsAll(s string, parts ...string) bool {
	for _, p := range parts {
		if !strings.Contains(s, p) {
			return false
		}
	}
	return true
}

func TestPaperFundingFeedModesReadOnlySealedCoverage(t *testing.T) {
	withFundingHistoryFn(t, func(context.Context, string, int64) ([]feedFundingRecord, error) {
		t.Fatal("feed modes must never make a private funding fetch")
		return nil, nil
	})
	r1 := pfT0 + pfHour
	snap := &marketSnapshot{
		funding: map[string]feedFunding{},
		accountingFunding: map[string]feedAccountingFunding{
			"ETH": {FromMs: pfT0, ToMs: r1, Records: []feedFundingRecord{{TimeMs: r1, Rate: 0.0001}}},
		},
	}
	need := map[string]int64{"ETH": pfT0, "SOL": pfT0}
	in := gatherPaperFundingInputs(context.Background(), true, snap, need, time.UnixMilli(r1+60_000))
	if cov, ok := in.Coverage["ETH"]; !ok || cov.ToMs != r1 || len(cov.Records) != 1 {
		t.Fatalf("sealed coverage must be read for ETH: %+v", in.Coverage)
	}
	if _, ok := in.Coverage["SOL"]; ok || !in.NotServed["SOL"] {
		t.Fatalf("a coin missing from the seal is not served and has no coverage: %+v", in)
	}
	if hold, _ := snap.fundingHold("SOL", false, false); hold {
		t.Fatal("a missing accounting entry must never hold a check")
	}
	if empty := gatherPaperFundingInputs(context.Background(), true, nil, need, time.UnixMilli(r1)); len(empty.Coverage) != 0 {
		t.Fatalf("no seal means no coverage and no fetch: %+v", empty)
	}
	gaps := applySealCoverage(snap, cycleMarketRequirements{AccountingCoins: []string{"ETH", "SOL"}})
	if len(gaps) != 1 || !containsAll(gaps[0], "accounting funding for SOL") || len(snap.accountingFunding) != 1 {
		t.Fatalf("a missing accounting coin is an informational gap and deletes nothing: %v", gaps)
	}

	calls := 0
	withFundingHistoryFn(t, func(_ context.Context, _ string, startMs int64) ([]feedFundingRecord, error) {
		calls++
		if startMs > r1 {
			return nil, nil
		}
		return []feedFundingRecord{{TimeMs: r1, Rate: 0.0001}}, nil
	})
	paperFundingRESTMu.Lock()
	paperFundingRESTCache = make(map[string]*paperFundingRESTEntry)
	paperFundingRESTMu.Unlock()
	rest := gatherPaperFundingInputs(context.Background(), false, nil, map[string]int64{"ETH": pfT0}, time.UnixMilli(r1+60_000))
	if cov := rest.Coverage["ETH"]; cov.FromMs != pfT0 || cov.ToMs != r1 || calls != 2 {
		t.Fatalf("REST mode fetches bounded coverage from the oldest need: %+v calls %d", cov, calls)
	}
	gatherPaperFundingInputs(context.Background(), false, nil, map[string]int64{"ETH": pfT0}, time.UnixMilli(r1+120_000))
	if calls != 2 {
		t.Fatalf("REST mode fetches a coin at most once per interval: %d calls", calls)
	}
}

func TestPaperFundingStaysInItsPartitionAndLeavesLiveUntouched(t *testing.T) {
	for _, split := range []bool{true, false} {
		live := hlLivePerps("hl-live", "ETH", 1000)
		f := newPFFixture(t, split, pfPaperPerps("hl-paper", "ETH"), live)
		f.sync()
		paper, liveState := f.state.Strategies["hl-paper"], f.state.Strategies["hl-live"]
		liveState.Positions["ETH"] = &Position{Symbol: "ETH", Side: "long", Quantity: 1, AvgCost: 2000, Multiplier: 1, OwnerStrategyID: "hl-live", TradePositionID: "live-pos"}
		pfOpen(t, paper, "ETH", 1, 1, 2000)
		for key, members := range detectSharedWallets(f.cfg.Strategies) {
			for _, id := range members {
				if id == "hl-paper" {
					t.Fatalf("a paper strategy must never be a shared wallet member (%v)", key)
				}
			}
		}
		liveDB := f.store.file(storageRolePrimary)
		key := SharedWalletKey{Platform: "hyperliquid", Account: "0xtest"}
		ev := hlLedgerEvent{Time: pfT0 + pfHour, Hash: "0xabc", Delta: hlLedgerEventDelta{Type: "funding", Coin: "ETH", USDC: "-0.5"}}
		if !ingestFundingEvent(liveDB, f.state, key, ev, map[string]map[string]float64{"ETH": {"hl-live": 1}}) {
			t.Fatal("live funding ingest")
		}
		liveRowsBefore := len(liveState.TradeHistory)
		liveCash := liveState.Cash
		f.run(pfT0+pfHour+60_000, map[string]float64{"ETH": 2000}, pfCoverage("ETH", pfT0, feedFundingRecord{TimeMs: pfT0 + pfHour, Rate: 0.0001}))
		if liveState.PaperFunding != nil || len(liveState.TradeHistory) != liveRowsBefore || liveState.Cash != liveCash {
			t.Fatal("paper accounting must leave the live strategy unchanged")
		}
		if lr := liveState.TradeHistory[len(liveState.TradeHistory)-1]; lr.ExchangeOrderID != fundingDedupID(ev) || !approxEq(lr.RealizedPnL, -0.5) {
			t.Fatalf("live funding keeps its wallet row: %+v", lr)
		}
		f.save()
		owner := f.store.file(storageRolePrimary)
		if split {
			owner = f.store.file(storageRolePaper)
			if rows := pfDBRows(t, f.store.file(storageRolePrimary), "hl-paper"); len(rows) != 0 {
				t.Fatalf("split layout: no paper row may land in the primary file: %+v", rows)
			}
		}
		paperRows := pfDBRows(t, owner, "hl-paper")
		if len(paperRows) != 2 || paperRows[1].tradeType != TradeTypeFunding {
			t.Fatalf("the owning file holds the paper open and its funding row: %+v", paperRows)
		}
		if raw, ok := paperFundingColumn(t, owner, "hl-paper"); !ok || raw == "" {
			t.Fatal("the owning file holds the timeline")
		}
		for _, sdb := range []*StateDB{f.store.file(storageRolePrimary), owner} {
			var n int
			if err := sdb.db.QueryRow("SELECT COUNT(*) FROM wallet_transfers").Scan(&n); err != nil || n != 0 {
				t.Fatalf("no wallet transfer may be written by paper accounting: n=%d err=%v", n, err)
			}
			if err := sdb.db.QueryRow("SELECT COUNT(*) FROM wallet_ledger_state").Scan(&n); err != nil || n != 0 {
				t.Fatalf("no wallet ledger state may be written by paper accounting: n=%d err=%v", n, err)
			}
		}
	}
}

func TestPaperFundingLeavesRiskCountersAndFeedsDrawdown(t *testing.T) {
	f := newPFFixture(t, true, pfPaperPerps("hl-a", "ETH"))
	f.cfg.PortfolioRisk = &PortfolioRiskConfig{MaxDrawdownPct: 50, WarnThresholdPct: 60}
	f.sync()
	a := f.state.Strategies["hl-a"]
	pfOpen(t, a, "ETH", 1, 10, 2000)
	a.RiskState.DailyPnL = -1.5
	a.RiskState.ConsecutiveLosses = 2
	a.RiskState.CircuitBreaker = false
	before := runScopeCycle(t, f.cfg, f.state, nil)[defaultPaperPartition].TotalPV
	f.run(pfT0+pfHour+60_000, map[string]float64{"ETH": 2000}, pfCoverage("ETH", pfT0, feedFundingRecord{TimeMs: pfT0 + pfHour, Rate: 0.01}))
	if a.RiskState.DailyPnL != -1.5 || a.RiskState.ConsecutiveLosses != 2 || a.RiskState.CircuitBreaker {
		t.Fatalf("funding must not touch daily PnL, consecutive losses or the circuit breaker: %+v", a.RiskState)
	}
	after := runScopeCycle(t, f.cfg, f.state, nil)[defaultPaperPartition].TotalPV
	if !approxEq(after-before, -10*2000*0.01) {
		t.Fatalf("the paper partition value must see the funding cash in the same cycle: %v -> %v", before, after)
	}
}

func TestPaperFundingEdgeCases(t *testing.T) {
	manual := StrategyConfig{ID: "hl-man", Type: "manual", Platform: "hyperliquid", Symbol: "SOL", Timeframe: "1h", Args: []string{"hold", "SOL", "1h", "--mode=paper"}, Capital: 1000}
	f := newPFFixture(t, true, pfPaperPerps("hl-a", "ETH"), manual)
	f.sync()
	a, m := f.state.Strategies["hl-a"], f.state.Strategies["hl-man"]
	if m.PaperFunding == nil {
		t.Fatal("a paper manual HL strategy is eligible")
	}
	pfOpen(t, a, "ETH", 1, 1, 2000)
	m.Positions["SOL"] = &Position{Symbol: "SOL", Side: "long", Quantity: 4, AvgCost: 100, Multiplier: 1, OwnerStrategyID: "hl-man", TradePositionID: "man-pos"}
	marks := map[string]float64{"ETH": 2000, "SOL": 100}
	f.run(pfT0+20*60_000, marks)
	if c := m.PaperFunding.Coins["SOL"]; len(c.Unknown) != 1 {
		t.Fatalf("a position written outside the helper must open an unknown window: %+v", c)
	}
	r1 := pfT0 + pfHour
	need := planPaperFunding(f.state, f.cfg.Strategies)
	f.at(r1 + 2*60_000)
	if n, tr := applyPerpsScaleIn(a, f.cfg.Strategies[0], "ETH", 2000, 1, 0, "", false, silentStrategyLogger("hl-a")); n != 1 {
		t.Fatal("scale-in between plan and run")
	} else {
		RecordTrade(a, *tr)
	}
	if need["ETH"] == 0 {
		t.Fatal("plan must request ETH coverage")
	}
	res := f.run(r1+3*60_000, marks, pfCoverage("ETH", pfT0, feedFundingRecord{TimeMs: r1, Rate: 0.0001}), pfCoverage("SOL", pfT0, feedFundingRecord{TimeMs: pfT0 + 15*60_000, Rate: 0.0002}, feedFundingRecord{TimeMs: r1, Rate: 0.0002}))
	if rows := pfFundingRows(a); len(rows) != 1 || !approxEq(rows[0].RealizedPnL, -1*2000*0.0001) {
		t.Fatalf("a hooked change between plan and run counts after the record: %+v", rows)
	}
	held := m.PaperFunding.Coins["SOL"].Held
	if len(held) != 1 || held[0].Reason != paperFundingHeldUnknown {
		t.Fatalf("a record inside an unknown window is held: %+v", held)
	}
	critical := false
	for _, msg := range res.Alerts {
		critical = critical || containsAll(msg, "CRITICAL", "held_unknown_window")
	}
	if !critical {
		t.Fatalf("a held record must alert CRITICAL: %v", res.Alerts)
	}
	if rows := pfFundingRows(m); len(rows) != 1 || !approxEq(rows[0].RealizedPnL, -4*100*0.0002) {
		t.Fatalf("the manual strategy accrues once its exposure is known: %+v", rows)
	}

	r2 := r1 + pfHour
	blockedIn := paperFundingInputs{Source: paperFundingSourceREST, Coverage: map[string]feedFundingCoverage{"ETH": pfCoverage("ETH", pfT0, feedFundingRecord{TimeMs: r2, Rate: 0.0001})}}
	f.at(r2 + 60_000)
	runPaperFundingAccounting(f.state, f.cfg.Strategies, f.store, blockedIn, marks, func(RiskPartition) bool { return true }, f.clock.now())
	if len(pfFundingRows(a)) != 1 || len(a.PaperFunding.Coins["ETH"].Samples) == 0 {
		t.Fatal("a blocked partition samples marks and skips settlement")
	}

	unmarshalPaperFundingState(a, "{not json")
	res = f.run(r2+2*60_000, marks, pfCoverage("ETH", pfT0, feedFundingRecord{TimeMs: r2, Rate: 0.0001}))
	if len(pfFundingRows(a)) != 1 || a.PaperFunding != nil {
		t.Fatal("corrupt state holds all accounting")
	}
	if raw, _ := marshalPaperFundingState(a); raw != "{not json" {
		t.Fatalf("corrupt raw text must be preserved, got %q", raw)
	}
	corruptAlert := false
	for _, msg := range res.Alerts {
		corruptAlert = corruptAlert || containsAll(msg, "CRITICAL", "corrupt_state")
	}
	if !corruptAlert {
		t.Fatal("corrupt state must alert CRITICAL")
	}
}

func TestPaperFundingRowsExportAsFundingEventsWithPositionIDs(t *testing.T) {
	f := newPFFixture(t, true, pfPaperPerps("hl-a", "ETH"))
	f.sync()
	a := f.state.Strategies["hl-a"]
	pfOpen(t, a, "ETH", 1, 1.5, 2000)
	pid := a.Positions["ETH"].TradePositionID
	r1 := pfT0 + pfHour
	f.run(r1+60_000, map[string]float64{"ETH": 2000}, pfCoverage("ETH", pfT0, feedFundingRecord{TimeMs: r1, Rate: 0.0002}))
	f.save()
	read, err := f.store.file(storageRolePaper).readLedgerSnapshot("hl-a", 100)
	if err != nil {
		t.Fatalf("readLedgerSnapshot: %v", err)
	}
	events, err := buildLedgerEvents(read, defaultPaperPartition, "hl-a")
	if err != nil {
		t.Fatalf("buildLedgerEvents: %v", err)
	}
	var funding []ledgerEvent
	for _, ev := range events {
		if ev.EventKind.Value != nil && *ev.EventKind.Value == "funding" {
			funding = append(funding, ev)
		}
	}
	if len(funding) != 1 {
		t.Fatalf("the export must list one funding event, got %d of %d events", len(funding), len(events))
	}
	ev := funding[0]
	if ev.PositionID.Value == nil || *ev.PositionID.Value != pid || ev.PositionAllocation.Value == nil || *ev.PositionAllocation.Value != "recorded" {
		t.Fatalf("the funding event must carry the captured position id %s: %+v", pid, ev.PositionID)
	}
	if ev.LedgerDelta.Value == nil || !approxEq(*ev.LedgerDelta.Value, -1.5*2000*0.0002) {
		t.Fatalf("the funding event ledger delta must equal the payment: %+v", ev.LedgerDelta)
	}
}

type pfStampRow struct {
	tradeType, tpTiers string
	triggerPx          float64
	entryATR           float64
}

func pfStampRows(t *testing.T, sdb *StateDB, storageID string) []pfStampRow {
	t.Helper()
	rows, err := sdb.db.Query("SELECT trade_type, stop_loss_trigger_px, entry_atr, tp_tiers_json FROM trades WHERE strategy_id = ? ORDER BY rowid", storageID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []pfStampRow
	for rows.Next() {
		var r pfStampRow
		if err := rows.Scan(&r.tradeType, &r.triggerPx, &r.entryATR, &r.tpTiers); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func TestPaperFundingRowsNeverTakeProtectionStamps(t *testing.T) {
	sc := pfPaperPerps("hl-a", "ETH")
	f := newPFFixture(t, true, sc)
	f.sync()
	a := f.state.Strategies["hl-a"]
	pfOpen(t, a, "ETH", 1, 1, 2000)
	f.save()
	marks := map[string]float64{"ETH": 2000}
	r1 := pfT0 + pfHour
	f.run(r1+60_000, marks, pfCoverage("ETH", pfT0, feedFundingRecord{TimeMs: r1, Rate: 0.0001}))
	f.save()
	paperDB := f.store.file(storageRolePaper)
	pos := a.Positions["ETH"]
	pos.EntryATR = 25
	pos.StopLossTriggerPx = 1950
	stampOpenTradeWithProtectionSnapshot(a, paperDB, sc, "ETH", pos)
	f.save()
	rows := pfStampRows(t, paperDB, "hl-a")
	if len(rows) != 2 || rows[0].tradeType == TradeTypeFunding || rows[0].triggerPx != 1950 || rows[1].tradeType != TradeTypeFunding || rows[1].triggerPx != 0 || rows[1].entryATR != 0 || rows[1].tpTiers != "" {
		t.Fatalf("the arm stamp must land on the opening row only: %+v", rows)
	}

	for h := int64(2); h <= 4; h++ {
		f.run(pfT0+h*pfHour+60_000, marks, pfCoverage("ETH", pfT0, feedFundingRecord{TimeMs: r1, Rate: 0.0001}, feedFundingRecord{TimeMs: pfT0 + 2*pfHour, Rate: 0.0001}, feedFundingRecord{TimeMs: pfT0 + 3*pfHour, Rate: 0.0001}, feedFundingRecord{TimeMs: pfT0 + 4*pfHour, Rate: 0.0001}))
	}
	f.save()
	pos.StopLossTriggerPx = 1970
	pos.TPTiersJSON = `[{"atr_multiple":1,"close_fraction":0.5}]`
	stampOpenTradeWithProtectionSnapshot(a, paperDB, sc, "ETH", pos)
	f.save()
	rows = pfStampRows(t, paperDB, "hl-a")
	funding := 0
	for _, r := range rows[1:] {
		if r.tradeType != TradeTypeFunding || r.triggerPx != 0 || r.entryATR != 0 || r.tpTiers != "" {
			t.Fatalf("a re-arm after several funding rows must stamp no funding row: %+v", rows)
		}
		funding++
	}
	if funding != 4 || rows[0].tpTiers == "" {
		t.Fatalf("the re-arm stamps the opening row and leaves %d funding rows clean: %+v", funding, rows)
	}
}

func TestLiveTPOIDsSkipWalletFundingRows(t *testing.T) {
	db := newLedgerTestDB(t)
	s := &StrategyState{ID: "hl-live", Positions: map[string]*Position{
		"BTC": {Symbol: "BTC", Side: "long", Quantity: 0.3, AvgCost: 60000, TradePositionID: "pos-1"},
	}}
	RecordTrade(s, Trade{StrategyID: "hl-live", Symbol: "BTC", Side: "buy", Quantity: 0.3, Price: 60000, TradeType: "perps", PositionID: "pos-1", TPOIDs: []int64{11, 12}, Timestamp: time.UnixMilli(pfT0).UTC()})
	state := &AppState{Strategies: map[string]*StrategyState{"hl-live": s}}
	ev := hlLedgerEvent{Time: pfT0 + pfHour, Hash: "0xf", Delta: hlLedgerEventDelta{Type: "funding", Coin: "BTC", USDC: "-1.0"}}
	if !ingestFundingEvent(db, state, SharedWalletKey{Platform: "hyperliquid", Account: "0xtest"}, ev, map[string]map[string]float64{"BTC": {"hl-live": 0.3}}) {
		t.Fatal("wallet funding ingest")
	}
	if last := s.TradeHistory[len(s.TradeHistory)-1]; last.TradeType != TradeTypeFunding {
		t.Fatalf("the newest row must be the wallet funding row: %+v", last)
	}
	if got := tpOIDsFromOpenTrade(s, "BTC", 2); len(got) != 2 || got[0] != 11 || got[1] != 12 {
		t.Fatalf("tpOIDsFromOpenTrade must return the opening trade's OIDs past a funding row, got %v", got)
	}
}

func TestPaperFundingRowsNeverCrowdTradesOutOfTheLoadedHistory(t *testing.T) {
	live := hlLivePerps("hl-live", "ETH", 1000)
	f := newPFFixture(t, true, pfPaperPerps("hl-p", "ETH"), live)
	f.sync()
	f.save()
	insert := func(sdb *StateDB, id string, n int, funding bool, startMs int64) {
		for i := 0; i < n; i++ {
			tr := Trade{StrategyID: id, Symbol: "ETH", Side: "buy", Quantity: 1, Price: 2000, TradeType: "perps", Timestamp: time.UnixMilli(startMs + int64(i)*60_000).UTC()}
			if funding {
				tr = Trade{StrategyID: id, Symbol: "ETH", Side: "funding", TradeType: TradeTypeFunding, RealizedPnL: -0.01, PnLGross: true, ExchangeOrderID: paperFundingOrderID("ETH", startMs+int64(i)*pfHour), Timestamp: time.UnixMilli(startMs + int64(i)*pfHour).UTC()}
			}
			if err := sdb.InsertTrade(id, tr); err != nil {
				t.Fatal(err)
			}
		}
	}
	insert(f.store.file(storageRolePaper), "hl-p", 50, false, pfT0)
	insert(f.store.file(storageRolePaper), "hl-p", 1200, true, pfT0+pfHour)
	insert(f.store.file(storageRolePrimary), "hl-live", 30, false, pfT0)
	insert(f.store.file(storageRolePrimary), "hl-live", 1100, true, pfT0+pfHour)
	f.reload()
	paper := f.state.Strategies["hl-p"]
	entry := newLeaderboardEntry(f.cfg.Strategies[0], paper, 1000, 1000, 0, 0, nil, nil, 3600)
	if entry.Trades != 50 {
		t.Fatalf("after a restart the leaderboard must count the 50 real trades, got %d", entry.Trades)
	}
	resp := statusRespForScopes(t, f.cfg.Strategies, f.state)
	strategies, _ := resp["strategies"].(map[string]any)
	liveStatus, _ := strategies["hl-live"].(map[string]any)
	if got, _ := liveStatus["trade_count"].(float64); got != 30 {
		t.Fatalf("after a restart /status must count the live strategy's 30 real trades, got %v", liveStatus["trade_count"])
	}
	last := paper.TradeHistory[len(paper.TradeHistory)-1]
	if last.TradeType != TradeTypeFunding || len(paper.TradeHistory) != 50+maxTradeHistory {
		t.Fatalf("each row kind keeps its own load window in time order: %d rows, newest %+v", len(paper.TradeHistory), last)
	}
}
