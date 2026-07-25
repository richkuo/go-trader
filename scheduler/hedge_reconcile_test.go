package main

import (
	"strings"
	"sync"
	"testing"
)

// ---------------------------------------------------------------------------
// Phase F — marks, wallet attribution, fill-resolver universe
// ---------------------------------------------------------------------------

func TestCollectPerpsMarkSymbols_IncludesHedgeCoins(t *testing.T) {
	hedger := hlHedgeTestStrategy("hl-eth", &HedgeConfig{Enabled: true, Symbol: "BTC"})
	disabled := hlHedgeTestStrategy("hl-sol", &HedgeConfig{Enabled: false, Symbol: "DOGE"})
	disabled.Args = []string{"sma_crossover", "SOL", "1h"}
	plain := hlHedgeTestStrategy("hl-link", nil)
	plain.Args = []string{"sma_crossover", "LINK", "1h"}

	hlCoins, _ := collectPerpsMarkSymbols([]StrategyConfig{hedger, disabled, plain})
	joined := strings.Join(hlCoins, ",")
	for _, want := range []string{"ETH", "BTC", "SOL", "LINK"} {
		if !strings.Contains(joined, want) {
			t.Errorf("hlCoins %v missing %s", hlCoins, want)
		}
	}
	if strings.Contains(joined, "DOGE") {
		t.Errorf("disabled hedge coin must not be mark-fetched, got %v", hlCoins)
	}
}

func TestHedgeMarkForSync_PrefersPricesMapThenFetch(t *testing.T) {
	sc := hlHedgeTestStrategy("hl-eth", &HedgeConfig{Enabled: true, Symbol: "BTC"})

	fetchCalls := 0
	prevFetch := hedgeMarkFetchFn
	t.Cleanup(func() { hedgeMarkFetchFn = prevFetch })
	hedgeMarkFetchFn = func(coin string) float64 {
		fetchCalls++
		if coin != "BTC" {
			t.Errorf("fetch coin = %q, want BTC", coin)
		}
		return 89900
	}

	// Prices-map hit wins; no fetch.
	if got := hedgeMarkForSync(sc, map[string]float64{"BTC": 90100}); got != 90100 {
		t.Errorf("prices-map mark = %g, want 90100", got)
	}
	if fetchCalls != 0 {
		t.Errorf("fetch called %d times despite prices-map hit", fetchCalls)
	}
	// Absent / non-positive map entries fall back to the one-shot fetch.
	if got := hedgeMarkForSync(sc, map[string]float64{}); got != 89900 {
		t.Errorf("fallback mark = %g, want 89900", got)
	}
	if got := hedgeMarkForSync(sc, map[string]float64{"BTC": 0}); got != 89900 {
		t.Errorf("zero mark must fall back, got %g", got)
	}
	if fetchCalls != 2 {
		t.Errorf("fetch calls = %d, want 2", fetchCalls)
	}
	// No hedge coin → 0, no fetch.
	if got := hedgeMarkForSync(hlHedgeTestStrategy("hl-eth", nil), nil); got != 0 {
		t.Errorf("no hedge block: mark = %g, want 0", got)
	}
}

func TestBuildSharedWalletBooks_IncludesHedgeLegs(t *testing.T) {
	sc := hlHedgeTestStrategy("hl-eth", &HedgeConfig{Enabled: true, Symbol: "BTC"})
	state := NewAppState()
	state.Strategies["hl-eth"] = hedgeTestStrategyState()

	key := SharedWalletKey{Platform: "hyperliquid", Account: "test"}
	_, virtualQty := buildSharedWalletBooks(key, []string{"hl-eth"}, map[string]StrategyConfig{"hl-eth": sc}, state)

	if got := virtualQty["ETH"]["hl-eth"]; got != 1.5 {
		t.Errorf("primary virtual qty = %g, want 1.5", got)
	}
	if got := virtualQty["BTC"]["hl-eth"]; got != 0.05 {
		t.Errorf("hedge virtual qty = %g, want 0.05 — orphan-coin misclassification risk", got)
	}

	// A position on the hedge coin WITHOUT the HedgeFor stamp is not
	// attributed (ownership comes from the persisted stamp, never the coin).
	state.Strategies["hl-eth"].Positions["BTC"].HedgeFor = ""
	_, virtualQty = buildSharedWalletBooks(key, []string{"hl-eth"}, map[string]StrategyConfig{"hl-eth": sc}, state)
	if _, ok := virtualQty["BTC"]; ok {
		t.Errorf("unstamped hedge-coin position must not be attributed, got %v", virtualQty["BTC"])
	}
}

func TestBuildCachedResolver_IncludesHedgeCoinCandidates(t *testing.T) {
	prev := lookupHyperliquidReconcileFillFee
	t.Cleanup(func() { lookupHyperliquidReconcileFillFee = prev })
	lookupHyperliquidReconcileFillFee = func(accountAddress, coin string, oid int64, expectedQty float64) (HLFillLookup, bool) {
		return HLFillLookup{Fee: 0.5, Px: 91000, FilledQty: expectedQty, Count: 1, OID: oid}, true
	}

	sc := hlHedgeTestStrategy("hl-eth", &HedgeConfig{Enabled: true, Symbol: "BTC"})

	// Hedge coin missing on-chain (external close) → candidate prefetched.
	state := NewAppState()
	state.Strategies["hl-eth"] = hedgeTestStrategyState()
	var mu sync.RWMutex
	resolver, _ := buildCachedHyperliquidReconcileFillResolver("addr", []StrategyConfig{sc}, state, &mu, []HLPosition{
		{Coin: "ETH", Size: 1.5, EntryPrice: 3000},
	})
	if _, ok := resolver("BTC", 0, 0.05); !ok {
		t.Error("hedge coin+size candidate missing — external hedge close would book the modeled fee")
	}

	// Control: hedge matches on-chain → no prefetch → resolver miss.
	resolver, _ = buildCachedHyperliquidReconcileFillResolver("addr", []StrategyConfig{sc}, state, &mu, []HLPosition{
		{Coin: "ETH", Size: 1.5, EntryPrice: 3000},
		{Coin: "BTC", Size: -0.05, EntryPrice: 90000},
	})
	if _, ok := resolver("BTC", 0, 0.05); ok {
		t.Error("in-sync hedge leg must not prefetch a close candidate")
	}

	// Partial drop (on-chain same-direction subset) → drop-qty candidate.
	resolver, _ = buildCachedHyperliquidReconcileFillResolver("addr", []StrategyConfig{sc}, state, &mu, []HLPosition{
		{Coin: "ETH", Size: 1.5, EntryPrice: 3000},
		{Coin: "BTC", Size: -0.02, EntryPrice: 90000},
	})
	if _, ok := resolver("BTC", 0, 0.03); !ok {
		t.Error("hedge partial-drop candidate (virtual−on-chain) missing")
	}
}

// ---------------------------------------------------------------------------
// Phase G — reconcile hedge-coin pass
// ---------------------------------------------------------------------------

// reconcileHedgeTestFixture runs reconcileHyperliquidAccountPositions for a
// single hedge-enabled strategy.
func reconcileHedgeTestFixture(t *testing.T, sc StrategyConfig, state *AppState, positions []HLPosition, notifier ownerDMSender) bool {
	t.Helper()
	logMgr, err := NewLogManager("")
	if err != nil {
		t.Fatalf("NewLogManager: %v", err)
	}
	var mu sync.RWMutex
	changed, _, _ := reconcileHyperliquidAccountPositions(
		[]StrategyConfig{sc}, []StrategyConfig{sc}, state, &mu, logMgr,
		positions, nil, "", notifier, false)
	return changed
}

func TestReconcileHedgeCoinPass_QtySideResync(t *testing.T) {
	sc := hlHedgeTestStrategy("hl-eth", &HedgeConfig{Enabled: true, Symbol: "BTC"})
	state := NewAppState()
	state.Strategies["hl-eth"] = hedgeTestStrategyState()

	// On-chain hedge leg drifted: short 0.06 vs virtual 0.05.
	changed := reconcileHedgeTestFixture(t, sc, state, []HLPosition{
		{Coin: "ETH", Size: 1.5, EntryPrice: 3000},
		{Coin: "BTC", Size: -0.06, EntryPrice: 90500},
	}, nil)
	if !changed {
		t.Error("hedge qty drift should mark the reconcile changed")
	}
	pos := state.Strategies["hl-eth"].Positions["BTC"]
	if pos.Quantity != 0.06 || pos.Side != "short" || pos.AvgCost != 90500 {
		t.Errorf("resynced hedge leg = %+v", pos)
	}
	if pos.HedgeFor != "ETH" || pos.HedgePrimaryQtyBasis != 1.5 {
		t.Errorf("resync must not touch hedge metadata: hedge_for=%q basis=%g", pos.HedgeFor, pos.HedgePrimaryQtyBasis)
	}
}

func TestReconcileHedgeCoinPass_ExternalCloseBooks(t *testing.T) {
	sc := hlHedgeTestStrategy("hl-eth", &HedgeConfig{Enabled: true, Symbol: "BTC"})
	state := NewAppState()
	state.Strategies["hl-eth"] = hedgeTestStrategyState()

	// Hedge coin vanished on-chain → hl_sync_external booking, hedge-typed.
	changed := reconcileHedgeTestFixture(t, sc, state, []HLPosition{
		{Coin: "ETH", Size: 1.5, EntryPrice: 3000},
	}, nil)
	if !changed {
		t.Error("external hedge close should mark the reconcile changed")
	}
	s := state.Strategies["hl-eth"]
	if _, ok := s.Positions["BTC"]; ok {
		t.Error("externally-closed hedge leg must be removed from state")
	}
	if len(s.TradeHistory) == 0 {
		t.Fatal("external hedge close must book a trade row (#954 never-drop)")
	}
	tr := s.TradeHistory[len(s.TradeHistory)-1]
	if tr.TradeType != hedgeTradeType || !tr.IsClose || tr.Symbol != "BTC" {
		t.Errorf("external close trade = %+v, want hedge-typed BTC close", tr)
	}
	// Risk routing: hedge close PnL feeds DailyPnL, never the streak.
	if s.RiskState.ConsecutiveLosses != 0 {
		t.Errorf("streak = %d, want 0 for a hedge-leg close", s.RiskState.ConsecutiveLosses)
	}
	// The primary is untouched.
	if s.Positions["ETH"] == nil || s.Positions["ETH"].Quantity != 1.5 {
		t.Error("primary must be untouched by the hedge-coin pass")
	}
}

func TestReconcileHedgeCoinPass_OrphanConflictSkipped(t *testing.T) {
	// A SHORT hedge leg under direction=long is counter-direction BY
	// CONSTRUCTION — without the HedgeFor guard the #822 check would queue
	// it for orphan auto-close every cycle.
	sc := hlHedgeTestStrategy("hl-eth", &HedgeConfig{Enabled: true, Symbol: "BTC"})
	sc.Direction = DirectionLong
	// The #822 orphan check only engages for live HL perps strategies.
	sc.Args = []string{"sma_crossover", "ETH", "1h", "--mode=live"}

	state := NewAppState()
	state.Strategies["hl-eth"] = hedgeTestStrategyState()

	logMgr, err := NewLogManager("")
	if err != nil {
		t.Fatalf("NewLogManager: %v", err)
	}
	var mu sync.RWMutex
	_, _, orphanJobs := reconcileHyperliquidAccountPositions(
		[]StrategyConfig{sc}, []StrategyConfig{sc}, state, &mu, logMgr,
		[]HLPosition{
			{Coin: "ETH", Size: 1.5, EntryPrice: 3000},
			{Coin: "BTC", Size: -0.05, EntryPrice: 90000},
		}, nil, "", nil, false)
	for _, job := range orphanJobs {
		if job.Symbol == "BTC" {
			t.Errorf("inverse hedge leg queued for orphan auto-close: %+v", job)
		}
	}

	// Control: strip the HedgeFor stamp and the same shape DOES queue —
	// proving the guard is what protects the hedge leg.
	state.Strategies["hl-eth"].Positions["BTC"].HedgeFor = ""
	_, _, orphanJobs = reconcileHyperliquidAccountPositions(
		[]StrategyConfig{sc}, []StrategyConfig{sc}, state, &mu, logMgr,
		[]HLPosition{
			{Coin: "ETH", Size: 1.5, EntryPrice: 3000},
			{Coin: "BTC", Size: -0.05, EntryPrice: 90000},
		}, nil, "", nil, false)
	queued := false
	for _, job := range orphanJobs {
		if job.Symbol == "BTC" {
			queued = true
		}
	}
	if !queued {
		t.Error("control: unstamped counter-direction position should queue an orphan close")
	}
}

func TestReconcileHedgeCoinPass_ForeignPositionWarnsNoAdoption(t *testing.T) {
	sc := hlHedgeTestStrategy("hl-eth", &HedgeConfig{Enabled: true, Symbol: "BTC"})
	state := NewAppState()
	s := hedgeTestStrategyState()
	delete(s.Positions, "BTC") // no virtual hedge leg
	state.Strategies["hl-eth"] = s

	notifier := &recordingDMSender{}
	// On-chain holds the hedge coin anyway (manual buy, stale leg, …).
	reconcileHedgeTestFixture(t, sc, state, []HLPosition{
		{Coin: "ETH", Size: 1.5, EntryPrice: 3000},
		{Coin: "BTC", Size: -0.07, EntryPrice: 90000},
	}, notifier)

	// Never adopted.
	if state.Strategies["hl-eth"].Positions["BTC"] != nil {
		t.Error("foreign hedge-coin position must NOT be adopted into state")
	}
	// Throttled operator DM fired (first sighting is always due).
	if len(notifier.msgs) != 1 {
		t.Fatalf("want 1 foreign-position DM, got %d: %v", len(notifier.msgs), notifier.msgs)
	}
	if !strings.Contains(notifier.msgs[0], "FOREIGN on-chain position") || !strings.Contains(notifier.msgs[0], "BTC") {
		t.Errorf("unexpected DM: %q", notifier.msgs[0])
	}

	// Second consecutive cycle: throttled, no repeat DM.
	reconcileHedgeTestFixture(t, sc, state, []HLPosition{
		{Coin: "ETH", Size: 1.5, EntryPrice: 3000},
		{Coin: "BTC", Size: -0.07, EntryPrice: 90000},
	}, notifier)
	if len(notifier.msgs) != 1 {
		t.Errorf("foreign-position DM must be throttled, got %d messages", len(notifier.msgs))
	}
}

func TestReconcileHedgeCoinPass_DisabledStrategySkipped(t *testing.T) {
	// No hedge block → the hedge-coin pass is a no-op; an on-chain position
	// on the (undeclared) coin is ignored exactly like before #1159.
	sc := hlHedgeTestStrategy("hl-eth", nil)
	state := NewAppState()
	s := hedgeTestStrategyState()
	delete(s.Positions, "BTC")
	state.Strategies["hl-eth"] = s
	notifier := &recordingDMSender{}

	reconcileHedgeTestFixture(t, sc, state, []HLPosition{
		{Coin: "ETH", Size: 1.5, EntryPrice: 3000},
		{Coin: "BTC", Size: -0.07, EntryPrice: 90000},
	}, notifier)
	if state.Strategies["hl-eth"].Positions["BTC"] != nil {
		t.Error("non-hedge strategy must not adopt anything")
	}
	if len(notifier.msgs) != 0 {
		t.Errorf("no hedge block → no foreign-coin DM, got %v", notifier.msgs)
	}
}

// TestStartupRecovery_HedgeLegSurvivesLoadAndResyncs pins acceptance 3: a
// hedge leg persisted by SaveState reloads with its ownership metadata, and
// the first reconcile resyncs qty against on-chain without touching the
// watermark — hedge sync then resumes from the basis.
func TestStartupRecovery_HedgeLegSurvivesLoadAndResyncs(t *testing.T) {
	db := openTestDB(t)
	state := NewAppState()
	state.Strategies["hl-eth"] = hedgeTestStrategyState()
	if err := db.SaveState(state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	loaded, err := db.LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	pos := loaded.Strategies["hl-eth"].Positions["BTC"]
	if pos == nil || pos.HedgeFor != "ETH" || pos.HedgePrimaryQtyBasis != 1.5 {
		t.Fatalf("reloaded hedge leg = %+v", pos)
	}

	sc := hlHedgeTestStrategy("hl-eth", &HedgeConfig{Enabled: true, Symbol: "BTC"})
	changed := reconcileHedgeTestFixture(t, sc, loaded, []HLPosition{
		{Coin: "ETH", Size: 1.5, EntryPrice: 3000},
		{Coin: "BTC", Size: -0.055, EntryPrice: 90100},
	}, nil)
	if !changed {
		t.Error("post-restart drift should resync")
	}
	pos = loaded.Strategies["hl-eth"].Positions["BTC"]
	if pos.Quantity != 0.055 || pos.HedgePrimaryQtyBasis != 1.5 || pos.HedgeFor != "ETH" {
		t.Errorf("post-resync hedge leg = %+v", pos)
	}
}
