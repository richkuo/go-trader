package main

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

const entryTestHour = int64(3_600_000)

var entryTestBar0 = time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC)

func entryTestBar(i int) time.Time {
	return entryTestBar0.Add(time.Duration(i) * time.Hour)
}

func entryTestDecision(barOpen, cutoff time.Time) StrategyDecisionFields {
	return StrategyDecisionFields{ClosedBar: &ClosedBarDecision{
		Enabled:            true,
		CutoffMs:           cutoff.UnixMilli(),
		DecisionBoundaryMs: barOpen.UnixMilli() + entryTestHour,
		BarOpenMs:          barOpen.UnixMilli(),
		IntervalMs:         entryTestHour,
		ClosureRule:        "hyperliquid_native_close",
	}}
}

func entryTestHLStrategy(id, mode string) StrategyConfig {
	return StrategyConfig{
		ID:                 id,
		Type:               "perps",
		Platform:           "hyperliquid",
		Script:             hyperliquidCheckScript,
		Args:               []string{"breakout", "BTC", "1h", "--mode=" + mode},
		ClosedBarDecisions: true,
		Direction:          DirectionBoth,
		Capital:            1000,
		Leverage:           1,
		SizingLeverage:     1,
		MaxDrawdownPct:     50,
	}
}

type entryTestEnv struct {
	t     *testing.T
	cfg   *Config
	store *StateStore
	state *AppState
	mu    sync.RWMutex
}

func newEntryTestConfig(t *testing.T, strategies ...StrategyConfig) *Config {
	t.Helper()
	return &Config{DBFile: filepath.Join(t.TempDir(), "state.db"), IntervalSeconds: 600, Strategies: strategies}
}

func openEntryTestEnv(t *testing.T, cfg *Config) *entryTestEnv {
	t.Helper()
	layout, err := resolveStorageLayout(cfg)
	if err != nil {
		t.Fatalf("layout: %v", err)
	}
	ident, err := buildStorageIdentityMap(cfg, layout)
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	store, err := openStateStoreWith(layout, ident, openStateDBFromTemplate)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	state, _, err := LoadStateWithStore(cfg, store)
	if err != nil {
		store.Close()
		t.Fatalf("load state: %v", err)
	}
	persisted := make(map[string]bool, len(state.Strategies))
	for id := range state.Strategies {
		persisted[id] = true
	}
	for _, sc := range cfg.Strategies {
		if state.Strategies[sc.ID] == nil {
			state.Strategies[sc.ID] = NewStrategyState(sc)
		}
	}
	if err := store.LoadEntryGuards(state, cfg.Strategies, persisted); err != nil {
		store.Close()
		t.Fatalf("load entry guards: %v", err)
	}
	env := &entryTestEnv{t: t, cfg: cfg, store: store, state: state}
	t.Cleanup(func() { env.store.Close() })
	return env
}

func (e *entryTestEnv) restart() *entryTestEnv {
	e.t.Helper()
	e.store.Close()
	return openEntryTestEnv(e.t, e.cfg)
}

func (e *entryTestEnv) saveAll() {
	e.t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	for part, err := range e.store.SaveAll(e.state) {
		if err != nil {
			e.t.Fatalf("save %s: %v", partitionLabel(part), err)
		}
	}
}

func (e *entryTestEnv) strat(id string) *StrategyState {
	return e.state.Strategies[id]
}

func (e *entryTestEnv) position(id, symbol string) (float64, string) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if p := e.strat(id).Positions[symbol]; p != nil && p.Quantity > 0 {
		return p.Quantity, p.Side
	}
	return 0, ""
}

func (e *entryTestEnv) fileFor(id string) *StateDB {
	e.t.Helper()
	db, err := e.store.dbForStrategy(id)
	if err != nil {
		e.t.Fatalf("db for %s: %v", id, err)
	}
	return db
}

func (e *entryTestEnv) consumedBar(id, symbol, side string) int64 {
	e.t.Helper()
	db := e.fileFor(id)
	sid, err := db.toStorageID(id)
	if err != nil {
		e.t.Fatal(err)
	}
	var v int64
	err = db.db.QueryRow(`SELECT consumed_through_bar_ms FROM entry_decision_state WHERE strategy_id = ? AND symbol = ? AND target_side = ?`, sid, symbol, side).Scan(&v)
	if err == sql.ErrNoRows {
		return 0
	}
	if err != nil {
		e.t.Fatalf("read consumed bar: %v", err)
	}
	return v
}

func (e *entryTestEnv) openTradeRows(id string) int {
	e.t.Helper()
	db := e.fileFor(id)
	sid, err := db.toStorageID(id)
	if err != nil {
		e.t.Fatal(err)
	}
	var n int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM trades WHERE strategy_id = ? AND is_close = 0 AND trade_type != 'funding'`, sid).Scan(&n); err != nil {
		e.t.Fatalf("count trades: %v", err)
	}
	return n
}

func (e *entryTestEnv) intentStatuses(id string) []string {
	e.t.Helper()
	db := e.fileFor(id)
	sid, err := db.toStorageID(id)
	if err != nil {
		e.t.Fatal(err)
	}
	rows, err := db.db.Query(`SELECT status FROM entry_order_intents WHERE strategy_id = ? ORDER BY created_at_ms, intent_id`, sid)
	if err != nil {
		e.t.Fatalf("read intents: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var st string
		if err := rows.Scan(&st); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, st)
	}
	return out
}

func (e *entryTestEnv) stopOut(id, symbol string, px float64) {
	e.t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	if !recordPerpsStopLossClose(e.strat(id), symbol, px, "stop_loss", silentStrategyLogger(id)) {
		e.t.Fatalf("stop-out of %s %s booked nothing", id, symbol)
	}
}

func (e *entryTestEnv) dispatchHLPaper(sc StrategyConfig, signal int, price float64, fields StrategyDecisionFields, now time.Time) (entryAdmission, int) {
	e.t.Helper()
	s := e.strat(sc.ID)
	logger := silentStrategyLogger(sc.ID)
	result := &HyperliquidResult{StrategyDecisionFields: fields, Symbol: "BTC", Signal: signal, Price: price, Indicators: map[string]interface{}{"atr": 2.0}}
	e.mu.RLock()
	var snap scaleInSnapshot
	posQty, posSide := 0.0, ""
	if p := s.Positions["BTC"]; p != nil && p.Quantity > 0 {
		posQty, posSide = p.Quantity, p.Side
		snap = scaleInSnapshot{Side: p.Side, Quantity: p.Quantity, AvgCost: p.AvgCost, EntryATR: p.EntryATR, ScaleInCount: p.ScaleInCount, AddedNotionalUSD: p.AddedNotionalUSD, LastAddPrice: p.LastAddPrice}
	}
	e.mu.RUnlock()
	adm := admitClosedBarEntry(sc, s, &e.mu, result.Symbol, result.StrategyDecisionFields, result.Signal, result.CloseFraction, posQty, posSide, now)
	applyEntryAdmission(adm, &result.Signal, &result.CloseFraction)
	scaleInAddQty := 0.0
	if result.Signal != 0 && sc.AllowScaleIn {
		if q, ok, _ := perpsScaleInDecision(sc, snap, result.Signal, price, 100); ok {
			scaleInAddQty = q
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	execAt := len(s.TradeHistory)
	end := beginEntryBookingLocked(s, adm, nil)
	var trades int
	var openTrade *Trade
	if scaleInAddQty > 0 {
		trades, _, openTrade, _ = executeHyperliquidScaleInDeferredOpen(sc, s, result, nil, "SIGNAL", price, scaleInAddQty, logger)
	} else {
		trades, _, openTrade, _ = executeHyperliquidResultDeferredOpen(sc, s, result, nil, "SIGNAL", price, nil, &Config{}, HurstGateDecision{}, logger)
	}
	if openTrade != nil {
		recordPositionOpen(s, sc, openTrade, s.Positions[result.Symbol])
	}
	finishEntryBookingLocked(e.store, s, adm, nil, execAt, end(), entryIntentEvidence{}, logger)
	return adm, trades
}

func useEntryTradeRecorder(t *testing.T, store *StateStore) {
	t.Helper()
	prev := tradeRecorder
	tradeRecorder = store.InsertTrade
	t.Cleanup(func() { tradeRecorder = prev })
}

func TestClosedBarEntryPaperStopOutHoldsSameCandleAtEveryCadence(t *testing.T) {
	useHLLotMetadataForTest(t, map[string]int{"BTC": 5})
	cases := []struct {
		name    string
		cadence time.Duration
		signal  int
		side    string
		stopPx  float64
	}{
		{"600s long", 600 * time.Second, 1, "long", 99},
		{"90s long", 90 * time.Second, 1, "long", 99},
		{"600s short", 600 * time.Second, -1, "short", 101},
		{"90s short", 90 * time.Second, -1, "short", 101},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc := entryTestHLStrategy("hl-cb", "paper")
			cfg := newEntryTestConfig(t, sc)
			env := openEntryTestEnv(t, cfg)
			useEntryTradeRecorder(t, env.store)
			decisionBar := entryTestBar(0)
			boundary := entryTestBar(1)

			first := boundary.Add(10 * time.Second)
			adm, trades := env.dispatchHLPaper(sc, tc.signal, 100, entryTestDecision(decisionBar, first), first)
			if !adm.admitted() || trades == 0 {
				t.Fatalf("first check: admitted=%t trades=%d reason=%q", adm.admitted(), trades, adm.HoldReason)
			}
			if qty, side := env.position(sc.ID, "BTC"); qty <= 0 || side != tc.side {
				t.Fatalf("first check booked %v %q, want a %s position", qty, side, tc.side)
			}
			if n := env.openTradeRows(sc.ID); n != 0 {
				t.Fatalf("opening trade reached the database before the book save (%d rows)", n)
			}
			env.saveAll()
			if n := env.openTradeRows(sc.ID); n != 1 {
				t.Fatalf("after the save the database holds %d opening trades, want 1", n)
			}
			if got := env.consumedBar(sc.ID, "BTC", tc.side); got != decisionBar.UnixMilli() {
				t.Fatalf("consumed bar %d, want %d", got, decisionBar.UnixMilli())
			}

			env.stopOut(sc.ID, "BTC", tc.stopPx)
			if qty, _ := env.position(sc.ID, "BTC"); qty != 0 {
				t.Fatalf("stop-out left %v", qty)
			}
			checks := 0
			for at := first.Add(tc.cadence); at.Before(boundary.Add(time.Hour)); at = at.Add(tc.cadence) {
				adm, trades := env.dispatchHLPaper(sc, tc.signal, 100, entryTestDecision(decisionBar, at), at)
				checks++
				if !adm.Held || trades != 0 {
					t.Fatalf("repeated check at %s: held=%t trades=%d", at.Format(time.RFC3339), adm.Held, trades)
				}
				if qty, _ := env.position(sc.ID, "BTC"); qty != 0 {
					t.Fatalf("repeated check at %s reopened %v", at.Format(time.RFC3339), qty)
				}
			}
			if want := int(time.Hour/tc.cadence) - 1; checks < want {
				t.Fatalf("ran %d repeated checks, want at least %d", checks, want)
			}
			env.saveAll()

			env = env.restart()
			useEntryTradeRecorder(t, env.store)
			again := boundary.Add(50 * time.Minute)
			adm, trades = env.dispatchHLPaper(sc, tc.signal, 100, entryTestDecision(decisionBar, again), again)
			if !adm.Held || trades != 0 {
				t.Fatalf("after restart the consumed candle was admitted again: held=%t trades=%d", adm.Held, trades)
			}

			next := entryTestBar(2).Add(10 * time.Second)
			adm, trades = env.dispatchHLPaper(sc, tc.signal, 100, entryTestDecision(entryTestBar(1), next), next)
			if !adm.admitted() || trades == 0 {
				t.Fatalf("next candle: admitted=%t trades=%d reason=%q", adm.admitted(), trades, adm.HoldReason)
			}
			env.saveAll()
			if n := env.openTradeRows(sc.ID); n != 2 {
				t.Fatalf("database holds %d opening trades, want 2 (one per decision candle)", n)
			}
			if got := env.consumedBar(sc.ID, "BTC", tc.side); got != entryTestBar(1).UnixMilli() {
				t.Fatalf("consumed bar %d, want %d", got, entryTestBar(1).UnixMilli())
			}
		})
	}
}

func TestClosedBarEntryImmediateOpeningStopConsumesCandle(t *testing.T) {
	useHLLotMetadataForTest(t, map[string]int{"BTC": 5})
	sc := entryTestHLStrategy("hl-cb-stop", "paper")
	pct := 0.001
	sc.StopLossPct = &pct
	env := openEntryTestEnv(t, newEntryTestConfig(t, sc))
	at := entryTestBar(1).Add(10 * time.Second)
	s := env.strat(sc.ID)
	logger := silentStrategyLogger(sc.ID)
	result := &HyperliquidResult{StrategyDecisionFields: entryTestDecision(entryTestBar(0), at), Symbol: "BTC", Signal: 1, Price: 100, Indicators: map[string]interface{}{"atr": 2.0}}
	adm := admitClosedBarEntry(sc, s, &env.mu, "BTC", result.StrategyDecisionFields, 1, 0, 0, "", at)
	if !adm.admitted() {
		t.Fatalf("not admitted: %q", adm.HoldReason)
	}
	env.mu.Lock()
	execAt := len(s.TradeHistory)
	end := beginEntryBookingLocked(s, adm, nil)
	_, _, openTrade, _ := executeHyperliquidResultDeferredOpen(sc, s, result, nil, "BUY", 100, nil, &Config{}, HurstGateDecision{}, logger)
	if openTrade != nil {
		recordPositionOpen(s, sc, openTrade, s.Positions["BTC"])
	}
	finishEntryBookingLocked(env.store, s, adm, nil, execAt, end(), entryIntentEvidence{}, logger)
	env.mu.Unlock()
	if qty, _ := env.position(sc.ID, "BTC"); qty != 0 {
		t.Fatalf("fixture: the opening stop inside the slippage band must fire at open, %v remains", qty)
	}
	if n := len(s.TradeHistory); n != 2 || s.TradeHistory[0].IsClose || !s.TradeHistory[1].IsClose {
		t.Fatalf("fixture: want an opening trade then its stop close, got %d trades", n)
	}
	env.saveAll()
	if got := env.consumedBar(sc.ID, "BTC", "long"); got != entryTestBar(0).UnixMilli() {
		t.Fatalf("an opening closed at once by its stop consumed bar %d, want %d", got, entryTestBar(0).UnixMilli())
	}
}

func TestClosedBarEntryReversalClosesWhileOpeningLegWaits(t *testing.T) {
	useHLLotMetadataForTest(t, map[string]int{"BTC": 5})
	sc := entryTestHLStrategy("hl-cb-rev", "paper")
	env := openEntryTestEnv(t, newEntryTestConfig(t, sc))
	open := entryTestBar(1).Add(10 * time.Second)
	if adm, n := env.dispatchHLPaper(sc, 1, 100, entryTestDecision(entryTestBar(0), open), open); !adm.admitted() || n == 0 {
		t.Fatalf("long open: admitted=%t trades=%d", adm.admitted(), n)
	}

	flip := entryTestBar(2).Add(10 * time.Second)
	adm, n := env.dispatchHLPaper(sc, -1, 100, entryTestDecision(entryTestBar(1), flip), flip)
	if !adm.CloseOnly || n != 1 {
		t.Fatalf("reversal: closeOnly=%t trades=%d", adm.CloseOnly, n)
	}
	if qty, side := env.position(sc.ID, "BTC"); qty != 0 {
		t.Fatalf("reversal opened %v %s in the same check", qty, side)
	}
	if got := env.consumedBar(sc.ID, "BTC", "short"); got != 0 {
		t.Fatalf("the close consumed the short side (%d)", got)
	}

	later := flip.Add(10 * time.Minute)
	adm, n = env.dispatchHLPaper(sc, -1, 100, entryTestDecision(entryTestBar(1), later), later)
	if !adm.admitted() || n == 0 {
		t.Fatalf("opening leg on the next check: admitted=%t trades=%d reason=%q", adm.admitted(), n, adm.HoldReason)
	}
	if qty, side := env.position(sc.ID, "BTC"); qty <= 0 || side != "short" {
		t.Fatalf("opening leg booked %v %q", qty, side)
	}
	env.saveAll()
	if got := env.consumedBar(sc.ID, "BTC", "short"); got != entryTestBar(1).UnixMilli() {
		t.Fatalf("short consumed bar %d, want %d", got, entryTestBar(1).UnixMilli())
	}

	env.stopOut(sc.ID, "BTC", 101)
	if adm, n := env.dispatchHLPaper(sc, 1, 100, entryTestDecision(entryTestBar(1), later.Add(time.Minute)), later.Add(time.Minute)); !adm.admitted() || n == 0 {
		t.Fatalf("the long side of the same candle is independent: admitted=%t trades=%d reason=%q", adm.admitted(), n, adm.HoldReason)
	}

	heldFlip := later.Add(2 * time.Minute)
	adm, n = env.dispatchHLPaper(sc, -1, 100, entryTestDecision(entryTestBar(1), heldFlip), heldFlip)
	if !adm.CloseOnly || !adm.Held || n != 1 {
		t.Fatalf("a held reversal must still close: closeOnly=%t held=%t trades=%d", adm.CloseOnly, adm.Held, n)
	}
	if qty, _ := env.position(sc.ID, "BTC"); qty != 0 {
		t.Fatalf("held reversal left %v open", qty)
	}
}

func TestClosedBarEntryShortToLongReversalHeldStillCloses(t *testing.T) {
	useHLLotMetadataForTest(t, map[string]int{"BTC": 5})
	sc := entryTestHLStrategy("hl-cb-rev-short", "paper")
	env := openEntryTestEnv(t, newEntryTestConfig(t, sc))
	open := entryTestBar(1).Add(10 * time.Second)
	if adm, n := env.dispatchHLPaper(sc, 1, 100, entryTestDecision(entryTestBar(0), open), open); !adm.admitted() || n == 0 {
		t.Fatalf("long open: admitted=%t trades=%d", adm.admitted(), n)
	}
	env.stopOut(sc.ID, "BTC", 99)
	short := open.Add(5 * time.Minute)
	if adm, n := env.dispatchHLPaper(sc, -1, 100, entryTestDecision(entryTestBar(0), short), short); !adm.admitted() || n == 0 {
		t.Fatalf("short open: admitted=%t trades=%d reason=%q", adm.admitted(), n, adm.HoldReason)
	}
	flip := short.Add(5 * time.Minute)
	adm, n := env.dispatchHLPaper(sc, 1, 100, entryTestDecision(entryTestBar(0), flip), flip)
	if !adm.CloseOnly || !adm.Held || n != 1 {
		t.Fatalf("held short-to-long reversal: closeOnly=%t held=%t trades=%d", adm.CloseOnly, adm.Held, n)
	}
	if qty, _ := env.position(sc.ID, "BTC"); qty != 0 {
		t.Fatalf("the held reversal must close the short and open nothing, %v remains", qty)
	}
	next := entryTestBar(2).Add(10 * time.Second)
	if adm, n := env.dispatchHLPaper(sc, 1, 100, entryTestDecision(entryTestBar(1), next), next); !adm.admitted() || n == 0 {
		t.Fatalf("the next candle opens long: admitted=%t trades=%d reason=%q", adm.admitted(), n, adm.HoldReason)
	}
}

func TestClosedBarEntryScaleInSharesTheSideAllowance(t *testing.T) {
	useHLLotMetadataForTest(t, map[string]int{"BTC": 5})
	sc := entryTestHLStrategy("hl-cb-add", "paper")
	sc.AllowScaleIn = true
	sc.ScaleIn = &ScaleInConfig{MaxAdds: 3, AddNotionalUSD: 50}
	env := openEntryTestEnv(t, newEntryTestConfig(t, sc))
	at := entryTestBar(1).Add(10 * time.Second)
	if adm, n := env.dispatchHLPaper(sc, 1, 100, entryTestDecision(entryTestBar(0), at), at); !adm.admitted() || n == 0 {
		t.Fatalf("open: admitted=%t trades=%d", adm.admitted(), n)
	}
	qty0, _ := env.position(sc.ID, "BTC")
	same := at.Add(10 * time.Minute)
	adm, n := env.dispatchHLPaper(sc, 1, 100, entryTestDecision(entryTestBar(0), same), same)
	if !adm.Held || n != 0 || adm.Kind != entryIncreaseAdd {
		t.Fatalf("same-candle add: kind=%s held=%t trades=%d", adm.Kind, adm.Held, n)
	}
	if qty, _ := env.position(sc.ID, "BTC"); qty != qty0 {
		t.Fatalf("same-candle add changed the position %v -> %v", qty0, qty)
	}
	next := entryTestBar(2).Add(10 * time.Second)
	adm, n = env.dispatchHLPaper(sc, 1, 100, entryTestDecision(entryTestBar(1), next), next)
	if !adm.admitted() || n == 0 {
		t.Fatalf("next-candle add: admitted=%t trades=%d reason=%q", adm.admitted(), n, adm.HoldReason)
	}
	if qty, _ := env.position(sc.ID, "BTC"); qty <= qty0 {
		t.Fatalf("next-candle add left %v (was %v)", qty, qty0)
	}
	env.saveAll()
	if got := env.consumedBar(sc.ID, "BTC", "long"); got != entryTestBar(1).UnixMilli() {
		t.Fatalf("consumed bar %d after the add, want %d", got, entryTestBar(1).UnixMilli())
	}

	plain := entryTestHLStrategy("hl-plain-add", "paper")
	plain.ClosedBarDecisions = false
	plain.AllowScaleIn = true
	plain.ScaleIn = &ScaleInConfig{MaxAdds: 3, AddNotionalUSD: 50}
	envPlain := openEntryTestEnv(t, newEntryTestConfig(t, plain))
	if adm, n := envPlain.dispatchHLPaper(plain, 1, 100, StrategyDecisionFields{}, at); adm.Applies || n == 0 {
		t.Fatalf("non-closed-bar open: applies=%t trades=%d", adm.Applies, n)
	}
	for i := 0; i < 2; i++ {
		if adm, n := envPlain.dispatchHLPaper(plain, 1, 100, StrategyDecisionFields{}, at.Add(time.Duration(i+1)*time.Minute)); adm.Applies || n != 1 {
			t.Fatalf("non-closed-bar add %d: applies=%t trades=%d", i, adm.Applies, n)
		}
	}
}

func TestClosedBarEntryRejectsUnusableDecisionIdentity(t *testing.T) {
	useHLLotMetadataForTest(t, map[string]int{"BTC": 5})
	sc := entryTestHLStrategy("hl-cb-id", "paper")
	env := openEntryTestEnv(t, newEntryTestConfig(t, sc))
	bar := entryTestBar(5)
	at := entryTestBar(6).Add(10 * time.Second)
	mutate := func(f func(*ClosedBarDecision)) StrategyDecisionFields {
		d := entryTestDecision(bar, at)
		f(d.ClosedBar)
		return d
	}
	cases := []struct {
		name   string
		fields StrategyDecisionFields
		now    time.Time
	}{
		{"missing", StrategyDecisionFields{}, at},
		{"held", mutate(func(c *ClosedBarDecision) { c.Held = true; c.HoldReason = "fixture" }), at},
		{"interval mismatch", mutate(func(c *ClosedBarDecision) { c.IntervalMs = 900_000 }), at},
		{"off grid", mutate(func(c *ClosedBarDecision) { c.BarOpenMs += 1; c.DecisionBoundaryMs += 1 }), at},
		{"boundary mismatch", mutate(func(c *ClosedBarDecision) { c.DecisionBoundaryMs += 1000 }), at},
		{"future cutoff", entryTestDecision(bar, at.Add(5*time.Minute)), at},
		{"stale cutoff", entryTestDecision(bar, at), at.Add(2 * time.Hour)},
		{"not the newest candle", entryTestDecision(entryTestBar(4), at), at},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			adm, n := env.dispatchHLPaper(sc, 1, 100, tc.fields, tc.now)
			if !adm.Held || n != 0 {
				t.Fatalf("held=%t trades=%d reason=%q", adm.Held, n, adm.HoldReason)
			}
			if qty, _ := env.position(sc.ID, "BTC"); qty != 0 {
				t.Fatalf("opened %v", qty)
			}
		})
	}
	newer := entryTestBar(7).Add(10 * time.Second)
	if adm, n := env.dispatchHLPaper(sc, 0, 100, entryTestDecision(entryTestBar(6), newer), newer); adm.Applies || n != 0 {
		t.Fatalf("zero-signal observation: applies=%t trades=%d", adm.Applies, n)
	}
	if adm, n := env.dispatchHLPaper(sc, 1, 100, entryTestDecision(entryTestBar(5), entryTestBar(6).Add(time.Minute)), entryTestBar(6).Add(2*time.Minute)); !adm.Held || n != 0 {
		t.Fatalf("a candle older than the newest observed one was admitted: held=%t trades=%d", adm.Held, n)
	}
}

func TestClosedBarEntryUpgradeBootstrapHoldsFirstObservedCandle(t *testing.T) {
	useHLLotMetadataForTest(t, map[string]int{"BTC": 5})
	sc := entryTestHLStrategy("hl-cb-boot", "paper")
	cfg := newEntryTestConfig(t, sc)
	env := openEntryTestEnv(t, cfg)
	env.saveAll()
	env = env.restart()
	if !env.strat(sc.ID).EntryGuard.PreExisting {
		t.Fatal("a strategy loaded from state must be pre-existing")
	}
	first := entryTestBar(1).Add(10 * time.Second)
	if adm, n := env.dispatchHLPaper(sc, 0, 100, entryTestDecision(entryTestBar(0), first), first); adm.Applies || n != 0 {
		t.Fatalf("zero-signal bootstrap observation: applies=%t trades=%d", adm.Applies, n)
	}
	env.saveAll()
	env = env.restart()
	for _, sig := range []int{1, -1} {
		at := first.Add(20 * time.Minute)
		adm, n := env.dispatchHLPaper(sc, sig, 100, entryTestDecision(entryTestBar(0), at), at)
		if !adm.Held || n != 0 {
			t.Fatalf("signal %d on the bootstrap candle after restart: held=%t trades=%d", sig, adm.Held, n)
		}
	}
	if got := env.consumedBar(sc.ID, "BTC", "long"); got != 0 {
		t.Fatalf("bootstrap invented a consumed candle %d", got)
	}
	next := entryTestBar(2).Add(10 * time.Second)
	if adm, n := env.dispatchHLPaper(sc, 1, 100, entryTestDecision(entryTestBar(1), next), next); !adm.admitted() || n == 0 {
		t.Fatalf("the candle after the bootstrap: admitted=%t trades=%d reason=%q", adm.admitted(), n, adm.HoldReason)
	}

	fresh := entryTestHLStrategy("hl-cb-new", "paper")
	envNew := openEntryTestEnv(t, newEntryTestConfig(t, fresh))
	if envNew.strat(fresh.ID).EntryGuard.PreExisting {
		t.Fatal("a strategy created at start is not pre-existing")
	}
	if adm, n := envNew.dispatchHLPaper(fresh, 1, 100, entryTestDecision(entryTestBar(0), first), first); !adm.admitted() || n == 0 {
		t.Fatalf("a new strategy admits its first candle: admitted=%t trades=%d reason=%q", adm.admitted(), n, adm.HoldReason)
	}
}

func TestClosedBarEntryPartitionsAndAliasesKeepSeparateAllowances(t *testing.T) {
	useHLLotMetadataForTest(t, map[string]int{"BTC": 5})
	dir := t.TempDir()
	live := entryTestHLStrategy("hl-live", "live")
	paper := entryTestHLStrategy("hl-paper", "paper")
	sourced := entryTestHLStrategy("hl-paper-s1", "paper")
	sourced.PaperSource = "s1"
	sourced.StorageStrategyID = "hl-paper"
	cfg := &Config{
		DBFile:       filepath.Join(dir, "live.db"),
		PaperDBFile:  filepath.Join(dir, "paper.db"),
		PaperSources: []PaperSourceConfig{{ID: "s1", DBFile: filepath.Join(dir, "s1.db")}},
		Strategies:   []StrategyConfig{live, paper, sourced},
	}
	env := openEntryTestEnv(t, cfg)
	at := entryTestBar(1).Add(10 * time.Second)
	fields := entryTestDecision(entryTestBar(0), at)
	for _, sc := range []StrategyConfig{paper, sourced} {
		if adm, n := env.dispatchHLPaper(sc, 1, 100, fields, at); !adm.admitted() || n == 0 {
			t.Fatalf("%s: admitted=%t trades=%d reason=%q", sc.ID, adm.admitted(), n, adm.HoldReason)
		}
	}
	liveAdm := admitClosedBarEntry(live, env.strat(live.ID), &env.mu, "BTC", fields, 1, 0, 0, "", at)
	if !liveAdm.admitted() {
		t.Fatalf("the live partition must not see paper consumption: %q", liveAdm.HoldReason)
	}
	env.saveAll()

	read := func(path string) map[string]string {
		db, err := openStateDBReadOnly(path)
		if err != nil {
			t.Fatalf("open %s: %v", path, err)
		}
		defer db.Close()
		rows, err := db.db.Query(`SELECT strategy_id, scope || ':' || source FROM entry_decision_state WHERE consumed_through_bar_ms > 0`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]string{}
		for rows.Next() {
			var id, part string
			if err := rows.Scan(&id, &part); err != nil {
				t.Fatal(err)
			}
			out[id] = part
		}
		return out
	}
	if got := read(cfg.DBFile); len(got) != 0 {
		t.Fatalf("the live file holds paper consumption %v", got)
	}
	if got := read(cfg.PaperDBFile); len(got) != 1 || got["hl-paper"] != "paper:" {
		t.Fatalf("paper file consumption %v", got)
	}
	if got := read(filepath.Join(dir, "s1.db")); len(got) != 1 || got["hl-paper"] != "paper:s1" {
		t.Fatalf("named source consumption (stored under its alias) %v", got)
	}

	env = env.restart()
	env.stopOut(sourced.ID, "BTC", 99)
	if adm, n := env.dispatchHLPaper(sourced, 1, 100, entryTestDecision(entryTestBar(0), at.Add(time.Minute)), at.Add(time.Minute)); !adm.Held || n != 0 {
		t.Fatalf("the aliased source lost its consumption on restart: held=%t trades=%d", adm.Held, n)
	}

	s := env.strat(paper.ID)
	env.mu.Lock()
	s.EntryGuard.Partition = livePartition
	err := env.store.SaveStrategyBook(s)
	s.EntryGuard.Partition = defaultPaperPartition
	env.mu.Unlock()
	if err == nil {
		t.Fatal("an entry record naming a partition the strategy does not own must be refused before any write")
	}
}

func TestEntryDecisionMigrationIsIdempotentAndInspectionReadsWithoutMigrating(t *testing.T) {
	sc := entryTestHLStrategy("hl-insp", "paper")
	cfg := newEntryTestConfig(t, sc)
	raw, err := sql.Open("sqlite", cfg.DBFile)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(schemaDDL); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	layout, err := resolveStorageLayout(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ident, err := buildStorageIdentityMap(cfg, layout)
	if err != nil {
		t.Fatal(err)
	}
	si, err := inspectStorageOwnership(layout, ident, cfg, false)
	if err != nil || !si.OK() {
		t.Fatalf("inspection of a pre-#1769 file: err=%v rejections=%v", err, si.Rejections)
	}
	probe, err := openStateDBReadOnly(cfg.DBFile)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := tableExists(probe, "entry_decision_state"); ok {
		t.Fatal("read-only inspection created the entry tables")
	}
	probe.Close()

	for i := 0; i < 2; i++ {
		db, err := OpenStateDB(cfg.DBFile)
		if err != nil {
			t.Fatalf("migrating open %d: %v", i, err)
		}
		db.Close()
	}
	useHLLotMetadataForTest(t, map[string]int{"BTC": 5})
	env := openEntryTestEnv(t, cfg)
	at := entryTestBar(1).Add(10 * time.Second)
	env.dispatchHLPaper(sc, 1, 100, entryTestDecision(entryTestBar(0), at), at)
	env.saveAll()
	env.store.Close()
	before := fileDigest(t, cfg.DBFile)
	si, err = inspectStorageOwnership(layout, ident, cfg, false)
	if err != nil || !si.OK() {
		t.Fatalf("inspection: err=%v rejections=%v", err, si.Rejections)
	}
	if si.Files[0].EntryDecisions != 2 || si.Files[0].EntryUnmapped != 0 {
		t.Fatalf("inspection counted %d entry rows (%d unmapped), want 2 and 0", si.Files[0].EntryDecisions, si.Files[0].EntryUnmapped)
	}
	if after := fileDigest(t, cfg.DBFile); after != before {
		t.Fatal("read-only inspection changed the state file")
	}

	db, err := OpenStateDB(cfg.DBFile)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`INSERT INTO entry_decision_state (scope, source, strategy_id, symbol, interval_ms, target_side) VALUES ('paper', 'other', 'hl-insp', 'ETH', 3600000, 'long')`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	si, err = inspectStorageOwnership(layout, ident, cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	if si.OK() {
		t.Fatal("an entry row of a partition the file does not own must be rejected")
	}
	env = openEntryTestEnv(t, cfg)
	if _, ok := env.strat(sc.ID).EntryGuard.Decisions[entryDecisionKey{Symbol: "ETH", IntervalMs: entryTestHour, Side: "long"}]; ok {
		t.Fatal("a row of another partition was loaded into the strategy's allowance")
	}
}

type entryLiveVenue struct {
	t          *testing.T
	env        *entryTestEnv
	calls      int
	statusSeen []string
	respond    func(args []string) (*HyperliquidExecuteResult, error)
	lastSnap   hlExecuteSnapshot
}

func (v *entryLiveVenue) install() {
	prev := runHyperliquidExecuteFn
	v.t.Cleanup(func() { runHyperliquidExecuteFn = prev })
	runHyperliquidExecuteFn = func(script, symbol, side string, size, stopLossPct float64, cancelOID int64, prevPosQty float64, marginMode string, leverage float64, closeMode hlCloseMode, snapshot hlExecuteSnapshot, extraCancelOIDs ...int64) (*HyperliquidExecuteResult, string, error) {
		v.calls++
		v.lastSnap = snapshot
		for _, id := range []string{"hl-live"} {
			st := v.env.intentStatuses(id)
			if len(st) > 0 {
				v.statusSeen = append(v.statusSeen, st[len(st)-1])
			}
		}
		res, err := v.respond(buildHyperliquidExecuteArgs(symbol, side, size, stopLossPct, cancelOID, prevPosQty, marginMode, leverage, closeMode, snapshot, extraCancelOIDs...))
		return res, "", err
	}
}

func hlFilled(qty, px float64, oid int64) func([]string) (*HyperliquidExecuteResult, error) {
	return func([]string) (*HyperliquidExecuteResult, error) {
		return &HyperliquidExecuteResult{OrderOutcome: "filled", Execution: &HyperliquidExecution{Action: "buy", Symbol: "BTC", Size: qty, Fill: &HyperliquidFill{AvgPx: px, TotalSz: qty, OID: oid, Fee: 0.02}}}, nil
	}
}

func (e *entryTestEnv) dispatchHLLive(sc StrategyConfig, signal int, price float64, fields StrategyDecisionFields, now time.Time) entryAdmission {
	e.t.Helper()
	s := e.strat(sc.ID)
	logger := silentStrategyLogger(sc.ID)
	result := &HyperliquidResult{StrategyDecisionFields: fields, Symbol: "BTC", Signal: signal, Price: price, Indicators: map[string]interface{}{"atr": 2.0}}
	posQty, posSide := e.position(sc.ID, "BTC")
	adm := admitClosedBarEntry(sc, s, &e.mu, "BTC", result.StrategyDecisionFields, result.Signal, result.CloseFraction, posQty, posSide, now)
	applyEntryAdmission(adm, &result.Signal, &result.CloseFraction)
	if result.Signal == 0 {
		return adm
	}
	var in *entryIntent
	failed := false
	if adm.admitted() && adm.Kind == entryIncreaseOpen && posQty <= 0 {
		var err error
		in, err = reserveLiveEntryIntent(e.store, sc, s, &e.mu, adm, entryProviderHyperliquid, entryIntentRequest{Kind: adm.Kind.String(), Signal: result.Signal, PrevPosQty: posQty, PrevPosSide: posSide, Price: price, EntryATR: 2}, now)
		if err != nil {
			failed = true
		} else {
			result.EntrySubmitGate = hyperliquidEntrySubmitGate(e.store, s, &e.mu, in)
		}
	}
	var execResult *HyperliquidExecuteResult
	if !failed {
		e.mu.RLock()
		cash := s.Cash
		e.mu.RUnlock()
		er, ok := runHyperliquidExecuteOrder(sc, result, price, cash, true, posQty, posSide, 0, 0, 0, nil, []StrategyConfig{sc}, hlExecuteSnapshot{}, hlCloseContext{}, HurstGateDecision{}, nil, logger)
		if ok {
			execResult = er
		} else {
			failed = true
			outcome, detail := hyperliquidExecuteOutcome(er)
			finishFailedLiveEntry(e.store, sc, s, &e.mu, in, result.LiveOrderSubmitted, outcome, detail, nil, logger)
		}
	}
	if failed {
		return adm
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	execAt := len(s.TradeHistory)
	end := beginEntryBookingLocked(s, adm, in)
	_, _, openTrade, _ := executeHyperliquidResultDeferredOpen(sc, s, result, execResult, "SIGNAL", price, nil, &Config{}, HurstGateDecision{}, logger)
	if openTrade != nil {
		recordPositionOpen(s, sc, openTrade, s.Positions["BTC"])
	}
	finishEntryBookingLocked(e.store, s, adm, in, execAt, end(), hyperliquidEntryEvidence(execResult), logger)
	return adm
}

func TestClosedBarLiveEntrySavesSubmissionBeforeTheOrderAndCommitsWithTheBook(t *testing.T) {
	sc := entryTestHLStrategy("hl-live", "live")
	env := openEntryTestEnv(t, newEntryTestConfig(t, sc))
	useEntryTradeRecorder(t, env.store)
	venue := &entryLiveVenue{t: t, env: env, respond: hlFilled(0.5, 100.5, 77)}
	venue.install()
	at := entryTestBar(1).Add(10 * time.Second)
	adm := env.dispatchHLLive(sc, 1, 100, entryTestDecision(entryTestBar(0), at), at)
	if !adm.admitted() || venue.calls != 1 {
		t.Fatalf("admitted=%t calls=%d", adm.admitted(), venue.calls)
	}
	if len(venue.statusSeen) != 1 || venue.statusSeen[0] != entryIntentSubmitting {
		t.Fatalf("intent status seen by the order call %v, want [submitting]", venue.statusSeen)
	}
	if venue.lastSnap.Entry.Cloid == "" || venue.lastSnap.Entry.ExpiresAfterMs <= 0 || venue.lastSnap.Entry.StopCloid == "" {
		t.Fatalf("the order call carried no saved client identity: %+v", venue.lastSnap.Entry)
	}
	if got := env.intentStatuses(sc.ID); len(got) != 1 || got[0] != entryIntentCommitted {
		t.Fatalf("intent statuses %v, want [committed]", got)
	}
	if got := env.consumedBar(sc.ID, "BTC", "long"); got != entryTestBar(0).UnixMilli() {
		t.Fatalf("consumed bar %d", got)
	}
	if n := env.openTradeRows(sc.ID); n != 1 {
		t.Fatalf("settlement wrote %d opening trades, want 1", n)
	}
	env = env.restart()
	if qty, side := env.position(sc.ID, "BTC"); qty != 0.5 || side != "long" {
		t.Fatalf("the settled book lost the fill: %v %q", qty, side)
	}
}

func TestClosedBarLiveEntryPartialAndImmediatelyStoppedFillsConsumeOnce(t *testing.T) {
	cases := []struct {
		name     string
		respond  func([]string) (*HyperliquidExecuteResult, error)
		wantQty  float64
		wantRows int
	}{
		{"partial fill", hlFilled(0.2, 100, 81), 0.2, 1},
		{"opening stop filled at submit", func([]string) (*HyperliquidExecuteResult, error) {
			return &HyperliquidExecuteResult{OrderOutcome: "filled", StopLossFilledImmediately: true,
				Execution: &HyperliquidExecution{Action: "buy", Symbol: "BTC", Size: 0.5, Fill: &HyperliquidFill{AvgPx: 100, TotalSz: 0.5, OID: 82, Fee: 0.02, StopLossTriggerPx: 99.9}}}, nil
		}, 0, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc := entryTestHLStrategy("hl-live", "live")
			pct := 0.1
			sc.StopLossPct = &pct
			env := openEntryTestEnv(t, newEntryTestConfig(t, sc))
			useEntryTradeRecorder(t, env.store)
			venue := &entryLiveVenue{t: t, env: env, respond: tc.respond}
			venue.install()
			at := entryTestBar(1).Add(10 * time.Second)
			env.dispatchHLLive(sc, 1, 100, entryTestDecision(entryTestBar(0), at), at)
			if qty, _ := env.position(sc.ID, "BTC"); qty != tc.wantQty {
				t.Fatalf("book %v, want %v", qty, tc.wantQty)
			}
			if got := env.intentStatuses(sc.ID); len(got) != 1 || got[0] != entryIntentCommitted {
				t.Fatalf("intents %v", got)
			}
			if got := env.consumedBar(sc.ID, "BTC", "long"); got != entryTestBar(0).UnixMilli() {
				t.Fatalf("consumed bar %d", got)
			}
			if n := env.openTradeRows(sc.ID); n != tc.wantRows {
				t.Fatalf("opening trade rows %d, want %d", n, tc.wantRows)
			}
			again := at.Add(10 * time.Minute)
			env.dispatchHLLive(sc, 1, 100, entryTestDecision(entryTestBar(0), again), again)
			if venue.calls != 1 {
				t.Fatalf("the consumed candle was sent again (%d calls)", venue.calls)
			}
		})
	}
}

func TestClosedBarLiveEntryRejectionReleasesAndUnknownHoldsWithoutResend(t *testing.T) {
	sc := entryTestHLStrategy("hl-live", "live")
	env := openEntryTestEnv(t, newEntryTestConfig(t, sc))
	venue := &entryLiveVenue{t: t, env: env}
	venue.respond = func([]string) (*HyperliquidExecuteResult, error) {
		return &HyperliquidExecuteResult{OrderOutcome: "rejected", Error: "exchange rejected order: insufficient margin"}, errors.New("exit status 1")
	}
	venue.install()
	at := entryTestBar(1).Add(10 * time.Second)
	env.dispatchHLLive(sc, 1, 100, entryTestDecision(entryTestBar(0), at), at)
	if got := env.intentStatuses(sc.ID); len(got) != 1 || got[0] != entryIntentNoFill {
		t.Fatalf("rejection: intents %v, want [no_fill]", got)
	}
	if got := env.consumedBar(sc.ID, "BTC", "long"); got != 0 {
		t.Fatalf("a rejection consumed the candle (%d)", got)
	}

	venue.respond = func([]string) (*HyperliquidExecuteResult, error) {
		return nil, errors.New("signal: killed")
	}
	retry := at.Add(10 * time.Minute)
	adm := env.dispatchHLLive(sc, 1, 100, entryTestDecision(entryTestBar(0), retry), retry)
	if !adm.admitted() || venue.calls != 2 {
		t.Fatalf("retry after a rejection: admitted=%t calls=%d", adm.admitted(), venue.calls)
	}
	if got := env.intentStatuses(sc.ID); len(got) != 2 || got[1] != entryIntentSubmitting {
		t.Fatalf("unknown outcome: intents %v, want the second held as submitting", got)
	}
	for i, later := range []time.Time{retry.Add(10 * time.Minute), entryTestBar(2).Add(10 * time.Second)} {
		fields := entryTestDecision(entryTestBar(0), later)
		if i == 1 {
			fields = entryTestDecision(entryTestBar(1), later)
		}
		adm := env.dispatchHLLive(sc, 1, 100, fields, later)
		if !adm.Held || venue.calls != 2 {
			t.Fatalf("check %d with an unresolved attempt: held=%t calls=%d", i, adm.Held, venue.calls)
		}
	}
	env = env.restart()
	venue.env = env
	again := entryTestBar(2).Add(20 * time.Minute)
	if adm := env.dispatchHLLive(sc, 1, 100, entryTestDecision(entryTestBar(1), again), again); !adm.Held || venue.calls != 2 {
		t.Fatalf("after restart the unresolved attempt must still hold: held=%t calls=%d", adm.Held, venue.calls)
	}
}

type entryRecoveryStub struct {
	calls int
	res   *entryRecoveryResult
	err   error
}

func installHLRecoveryStub(t *testing.T, stub *entryRecoveryStub) {
	prev := runHyperliquidRecoverEntryFn
	t.Cleanup(func() { runHyperliquidRecoverEntryFn = prev })
	runHyperliquidRecoverEntryFn = func(script, symbol, cloid, stopCloid string, sinceMs, untilMs int64) (*entryRecoveryResult, string, error) {
		stub.calls++
		return stub.res, "", stub.err
	}
}

func entryTestUnknownAttempt(t *testing.T) (*entryTestEnv, StrategyConfig) {
	t.Helper()
	sc := entryTestHLStrategy("hl-live", "live")
	pct := 2.0
	sc.StopLossPct = &pct
	useEntryTestHLAccount(t)
	env := openEntryTestEnv(t, newEntryTestConfig(t, sc))
	useEntryTradeRecorder(t, env.store)
	venue := &entryLiveVenue{t: t, env: env, respond: func([]string) (*HyperliquidExecuteResult, error) { return nil, errors.New("execute error: timeout") }}
	venue.install()
	at := time.Now().Add(-10 * time.Minute)
	bar := at.Truncate(time.Hour).Add(-time.Hour)
	if at.Sub(bar.Add(time.Hour)) >= time.Hour {
		t.Fatal("fixture clock")
	}
	env.dispatchHLLive(sc, 1, 100, entryTestDecision(bar, at), at)
	if got := env.intentStatuses(sc.ID); len(got) != 1 || got[0] != entryIntentSubmitting {
		t.Fatalf("fixture intents %v", got)
	}
	return env.restart(), sc
}

func backdateEntryDeadlines(t *testing.T, env *entryTestEnv, id string) {
	t.Helper()
	g := env.strat(id).EntryGuard
	for _, in := range g.Intents {
		req, err := parseEntryIntentRequest(in)
		if err != nil {
			t.Fatal(err)
		}
		req.ExpiresAfterMs = time.Now().Add(-10 * time.Minute).UnixMilli()
		req.StopExpiresAfterMs = time.Now().Add(-9 * time.Minute).UnixMilli()
		req.SubmittedAtMs = req.ExpiresAfterMs - hlEntryOrderExpiry.Milliseconds()
		in.RequestJSON = marshalEntryJSON(req)
	}
}

func TestClosedBarLiveEntryRecoveryNamesTheUnresolvedAttemptAtStartupAndBooksProvenFillOnce(t *testing.T) {
	env, sc := entryTestUnknownAttempt(t)
	lines := entryGuardStartupLines(env.state)
	sort.Strings(lines)
	if len(lines) != 1 {
		t.Fatalf("startup lines name unresolved attempts: startup lines %v", lines)
	}
	var loaded *entryIntent
	for _, v := range env.strat(sc.ID).EntryGuard.Intents {
		loaded = v
	}
	if loaded == nil || loaded.ClientOrderID == "" || loaded.Status != entryIntentSubmitting {
		t.Fatalf("startup lines name unresolved attempts: loaded intent %+v", loaded)
	}
	stub := &entryRecoveryStub{res: &entryRecoveryResult{Platform: "hyperliquid",
		Order: &entryRecoveryOrder{Found: true, Status: "filled", OrderID: "901", OrigSz: 0.4, ExecutedSz: 0.4, Terminal: true, FillQty: 0.4, FillNotional: 40.2, FillFee: 0.018, FillCount: 2, FillAvgPx: 100.5, FillsComplete: true},
		Stop:  &entryRecoveryOrder{Found: true, Status: "open", OrderID: "902", Terminal: false, TriggerPx: 98.5},
	}}
	installHLRecoveryStub(t, stub)

	recoverEntryIntents(env.cfg, env.state, env.store, &env.mu, nil, nil, time.Now())
	if stub.calls != 0 {
		t.Fatalf("recovery queried the venue before the signed deadline passed (%d calls)", stub.calls)
	}
	if got := env.intentStatuses(sc.ID); got[0] != entryIntentSubmitting {
		t.Fatalf("intents %v before the deadline", got)
	}

	backdateEntryDeadlines(t, env, sc.ID)
	recoverEntryIntents(env.cfg, env.state, env.store, &env.mu, nil, nil, time.Now())
	if stub.calls != 1 {
		t.Fatalf("recovery calls %d", stub.calls)
	}
	qty, side := env.position(sc.ID, "BTC")
	if qty != 0.4 || side != "long" {
		t.Fatalf("recovered book %v %q", qty, side)
	}
	if oid := env.strat(sc.ID).Positions["BTC"].StopLossOID; oid != 902 {
		t.Fatalf("recovered stop oid %d, want 902", oid)
	}
	if got := env.intentStatuses(sc.ID); len(got) != 1 || got[0] != entryIntentCommitted {
		t.Fatalf("intents after recovery %v", got)
	}
	if n := env.openTradeRows(sc.ID); n != 1 {
		t.Fatalf("recovery wrote %d opening trades, want 1", n)
	}
	recoverEntryIntents(env.cfg, env.state, env.store, &env.mu, nil, nil, time.Now())
	env = env.restart()
	recoverEntryIntents(env.cfg, env.state, env.store, &env.mu, nil, nil, time.Now())
	if stub.calls != 1 || env.openTradeRows(sc.ID) != 1 {
		t.Fatalf("a resolved attempt was recovered again: calls=%d trades=%d", stub.calls, env.openTradeRows(sc.ID))
	}
}

func TestClosedBarLiveEntryRecoveryKeepsAmbiguousEvidenceHeld(t *testing.T) {
	cases := []struct {
		name  string
		res   *entryRecoveryResult
		err   error
		final string
	}{
		{"query failure", nil, errors.New("connection refused"), entryIntentSubmitting},
		{"incomplete fills", &entryRecoveryResult{Order: &entryRecoveryOrder{Found: true, Status: "filled", OrderID: "9", OrigSz: 0.4, ExecutedSz: 0.4, Terminal: true, FillQty: 0.1, FillAvgPx: 100, FillCount: 1, FillsComplete: false}, Stop: &entryRecoveryOrder{Found: false}}, nil, entryIntentSubmitting},
		{"order still open", &entryRecoveryResult{Order: &entryRecoveryOrder{Found: true, Status: "open", OrderID: "9", Terminal: false}}, nil, entryIntentSubmitting},
		{"fills without an order", &entryRecoveryResult{Order: &entryRecoveryOrder{Found: false, FillQty: 0.2}}, nil, entryIntentSubmitting},
		{"unknown after the deadline", &entryRecoveryResult{Account: entryTestHLAccountKey, Order: &entryRecoveryOrder{Found: false, Coverage: entryTestCoverage()}, Stop: &entryRecoveryOrder{Found: false, Coverage: entryTestCoverage()}}, nil, entryIntentNoFill},
		{"terminal zero fill", &entryRecoveryResult{Order: &entryRecoveryOrder{Found: true, Status: "canceled", OrderID: "9", OrigSz: 0.4, RemainingSz: 0.4, Terminal: true}}, nil, entryIntentNoFill},
		{"partial fill on an open order", &entryRecoveryResult{Order: &entryRecoveryOrder{Found: true, Status: "open", OrderID: "9", OrigSz: 0.4, ExecutedSz: 0.1, Terminal: false, FillQty: 0.1, FillAvgPx: 100, FillCount: 1, FillsComplete: true}}, nil, entryIntentSettling},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env, sc := entryTestUnknownAttempt(t)
			backdateEntryDeadlines(t, env, sc.ID)
			stub := &entryRecoveryStub{res: tc.res, err: tc.err}
			installHLRecoveryStub(t, stub)
			recoverEntryIntents(env.cfg, env.state, env.store, &env.mu, nil, nil, time.Now())
			if got := env.intentStatuses(sc.ID); len(got) != 1 || got[0] != tc.final {
				t.Fatalf("intents %v, want [%s]", got, tc.final)
			}
			if qty, _ := env.position(sc.ID, "BTC"); qty != 0 {
				t.Fatalf("ambiguous evidence booked %v", qty)
			}
			at := time.Now()
			bar := at.Truncate(time.Hour).Add(-time.Hour)
			adm := admitClosedBarEntry(sc, env.strat(sc.ID), &env.mu, "BTC", entryTestDecision(bar, at.Add(-time.Second)), 1, 0, 0, "", at)
			if tc.final == entryIntentNoFill && !adm.admitted() {
				t.Fatalf("a released attempt still holds: %q", adm.HoldReason)
			}
			if tc.final != entryIntentNoFill && !adm.Held {
				t.Fatal("an unresolved attempt must hold new entries")
			}
			if tc.final == entryIntentSettling && env.consumedBar(sc.ID, "BTC", "long") == 0 {
				t.Fatal("a proven partial fill must consume its candle")
			}
		})
	}
}

func TestClosedBarLiveEntryReservedRowIsReleasedWithoutAVenueQuery(t *testing.T) {
	sc := entryTestHLStrategy("hl-live", "live")
	env := openEntryTestEnv(t, newEntryTestConfig(t, sc))
	at := entryTestBar(1).Add(10 * time.Second)
	adm := admitClosedBarEntry(sc, env.strat(sc.ID), &env.mu, "BTC", entryTestDecision(entryTestBar(0), at), 1, 0, 0, "", at)
	if _, err := reserveLiveEntryIntent(env.store, sc, env.strat(sc.ID), &env.mu, adm, entryProviderHyperliquid, entryIntentRequest{Kind: "open", Signal: 1, Price: 100}, at); err != nil {
		t.Fatal(err)
	}
	env = env.restart()
	stub := &entryRecoveryStub{}
	installHLRecoveryStub(t, stub)
	recoverEntryIntents(env.cfg, env.state, env.store, &env.mu, nil, nil, time.Now())
	if stub.calls != 0 {
		t.Fatal("a reserved attempt never reached submission and needs no venue query")
	}
	if got := env.intentStatuses(sc.ID); len(got) != 1 || got[0] != entryIntentNoFill {
		t.Fatalf("reserved attempt after recovery %v", got)
	}
}

func TestClosedBarLiveEntrySettlementCommitFailureKeepsTheAttemptHeld(t *testing.T) {
	sc := entryTestHLStrategy("hl-live", "live")
	env := openEntryTestEnv(t, newEntryTestConfig(t, sc))
	useEntryTradeRecorder(t, env.store)
	venue := &entryLiveVenue{t: t, env: env, respond: hlFilled(0.5, 100, 55)}
	venue.install()
	failSettlement := false
	prevHook := storeCommitHook
	t.Cleanup(func() { storeCommitHook = prevHook })
	storeCommitHook = func(storageRole) error {
		if failSettlement {
			return errors.New("disk full")
		}
		return nil
	}
	venue.respond = func(args []string) (*HyperliquidExecuteResult, error) {
		failSettlement = true
		return hlFilled(0.5, 100, 55)(args)
	}
	at := entryTestBar(1).Add(10 * time.Second)
	env.dispatchHLLive(sc, 1, 100, entryTestDecision(entryTestBar(0), at), at)
	if got := env.intentStatuses(sc.ID); len(got) != 1 || got[0] != entryIntentSubmitting {
		t.Fatalf("a failed settlement commit made the attempt %v", got)
	}
	if n := env.openTradeRows(sc.ID); n != 0 {
		t.Fatalf("a failed settlement commit wrote %d trades", n)
	}
	if qty, _ := env.position(sc.ID, "BTC"); qty != 0.5 {
		t.Fatalf("the in-memory book must keep the venue exposure for protection (%v)", qty)
	}
	env.stopOut(sc.ID, "BTC", 99)
	failSettlement = false
	next := entryTestBar(2).Add(10 * time.Second)
	env.dispatchHLLive(sc, 1, 100, entryTestDecision(entryTestBar(1), next), next)
	if venue.calls != 1 {
		t.Fatalf("a new entry was sent while the saved attempt is still unresolved (%d calls)", venue.calls)
	}
	if qty, _ := env.position(sc.ID, "BTC"); qty != 0 {
		t.Fatalf("a refused reservation booked %v", qty)
	}
	env.saveAll()
	if got := env.intentStatuses(sc.ID); got[0] != entryIntentCommitted {
		t.Fatalf("the retried save must commit the attempt, got %v", got)
	}
	if n := env.openTradeRows(sc.ID); n != 1 {
		t.Fatalf("the retried save wrote %d trades, want 1", n)
	}
	later := entryTestBar(2).Add(20 * time.Minute)
	env.dispatchHLLive(sc, 1, 100, entryTestDecision(entryTestBar(1), later), later)
	if venue.calls != 2 {
		t.Fatalf("after the settlement committed the next candle must be admitted (%d calls)", venue.calls)
	}
}

func TestClosedBarLiveEntryReplayEventsPublishOnceAfterCommit(t *testing.T) {
	sc := entryTestHLStrategy("hl-live", "live")
	sc.ReplaySharing = ReplaySharingLiveMirror
	cfg := newEntryTestConfig(t, sc)
	rebuildReplayLiveSources(cfg)
	t.Cleanup(func() { rebuildReplayLiveSources(nil) })
	dl, err := OpenDecisionLogDB(filepath.Join(t.TempDir(), "replay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dl.Close() })
	prevWriter := decisionLogWriter
	decisionLogWriter = dl.InsertDecision
	t.Cleanup(func() { decisionLogWriter = prevWriter })

	env := openEntryTestEnv(t, cfg)
	venue := &entryLiveVenue{t: t, env: env, respond: hlFilled(0.5, 100, 66)}
	venue.install()
	at := entryTestBar(1).Add(10 * time.Second)
	env.dispatchHLLive(sc, 1, 100, entryTestDecision(entryTestBar(0), at), at)
	pending, err := dl.PendingDecisions(sc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].DecisionType != ReplayDecisionOpen {
		t.Fatalf("replay decisions %+v, want one open", pending)
	}
	db := env.fileFor(sc.ID)
	var payload string
	var seq, delivered int64
	if err := db.db.QueryRow(`SELECT replay_payload_json, settlement_seq, replay_delivered_seq FROM entry_order_intents`).Scan(&payload, &seq, &delivered); err != nil {
		t.Fatal(err)
	}
	if payload == "" || seq != 1 || delivered != 1 {
		t.Fatalf("intent replay columns payload=%q seq=%d delivered=%d", payload, seq, delivered)
	}
	if _, err := db.db.Exec(`UPDATE entry_order_intents SET replay_delivered_seq = 0`); err != nil {
		t.Fatal(err)
	}
	env = env.restart()
	recoverEntryIntents(env.cfg, env.state, env.store, &env.mu, nil, nil, time.Now())
	pending, err = dl.PendingDecisions(sc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("a retried publication added replay rows: %d", len(pending))
	}
}

type entryOKXVenue struct {
	calls int
	opts  []okxExecuteOptions
	res   *OKXExecuteResult
	err   error
}

func (v *entryOKXVenue) install(t *testing.T) {
	prev := okxExecuteGuardedFn
	t.Cleanup(func() { okxExecuteGuardedFn = prev })
	okxExecuteGuardedFn = func(script, symbol, side string, size float64, instType string, opts okxExecuteOptions) (*OKXExecuteResult, string, error) {
		v.calls++
		v.opts = append(v.opts, opts)
		return v.res, "", v.err
	}
}

func (e *entryTestEnv) dispatchOKX(sc StrategyConfig, signal int, price float64, fields StrategyDecisionFields, now time.Time) entryAdmission {
	e.t.Helper()
	s := e.strat(sc.ID)
	logger := silentStrategyLogger(sc.ID)
	db := e.fileFor(sc.ID)
	result := &OKXResult{StrategyDecisionFields: fields, Symbol: "BTC", Signal: signal, Price: price, Indicators: map[string]interface{}{"atr": 2.0}}
	posQty, posSide := e.position(sc.ID, "BTC")
	adm := admitClosedBarEntry(sc, s, &e.mu, "BTC", result.StrategyDecisionFields, result.Signal, result.CloseFraction, posQty, posSide, now)
	applyEntryAdmission(adm, &result.Signal, &result.CloseFraction)
	result.CloseReduceOnly = adm.CloseOnly
	var in *entryIntent
	var execResult *OKXExecuteResult
	failed := false
	if okxIsLive(sc.Args) && result.Signal != 0 && adm.admitted() {
		var err error
		in, err = reserveLiveEntryIntent(e.store, sc, s, &e.mu, adm, entryProviderOKX, entryIntentRequest{Kind: adm.Kind.String(), Signal: result.Signal, PrevPosQty: posQty, PrevPosSide: posSide, Price: price, EntryATR: 2}, now)
		if err != nil {
			failed = true
		} else {
			result.EntrySubmitGate = okxEntrySubmitGateFor(e.store, s, &e.mu, in)
		}
	}
	if okxIsLive(sc.Args) && result.Signal != 0 && !failed {
		e.mu.RLock()
		cash := s.Cash
		e.mu.RUnlock()
		er, ok := runOKXExecuteOrder(sc, result, price, cash, true, false, posQty, posSide, 100, 0, HurstGateDecision{}, nil, logger)
		if ok {
			execResult = er
		} else {
			failed = true
			outcome, detail := okxExecuteOutcome(er)
			finishFailedLiveEntry(e.store, sc, s, &e.mu, in, result.OrderSubmitted, outcome, detail, nil, logger)
		}
	}
	if failed {
		return adm
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	execAt := len(s.TradeHistory)
	end := beginEntryBookingLocked(s, adm, in)
	executeOKXResult(sc, s, db, result, execResult, "SIGNAL", price, nil, &Config{}, HurstGateDecision{}, logger)
	finishEntryBookingLocked(e.store, s, adm, in, execAt, end(), okxEntryEvidence(execResult), logger)
	return adm
}

func TestClosedBarOKXLiveEntryNeedsAConfirmedFillAndRecovers(t *testing.T) {
	for _, instType := range []string{"spot", "swap"} {
		t.Run(instType, func(t *testing.T) {
			typ := "spot"
			if instType == "swap" {
				typ = "perps"
			}
			sc := StrategyConfig{ID: "okx-live", Type: typ, Platform: "okx", Script: "shared_scripts/check_okx.py",
				Args: []string{"breakout", "BTC", "1h", "--mode=live", "--inst-type=" + instType}, ClosedBarDecisions: true,
				Direction: DirectionLong, Capital: 1000, Leverage: 1, SizingLeverage: 1}
			env := openEntryTestEnv(t, newEntryTestConfig(t, sc))
			useEntryTradeRecorder(t, env.store)
			venue := &entryOKXVenue{res: &OKXExecuteResult{OrderOutcome: "unknown", Error: "order has no confirmed outcome"}, err: nil}
			venue.install(t)
			at := time.Now().Add(-5 * time.Minute)
			bar := at.Truncate(time.Hour).Add(-time.Hour)
			env.dispatchOKX(sc, 1, 100, entryTestDecision(bar, at), at)
			if venue.calls != 1 || venue.opts[0].ClientOrderID == "" {
				t.Fatalf("calls=%d opts=%+v", venue.calls, venue.opts)
			}
			if qty, _ := env.position(sc.ID, "BTC"); qty != 0 {
				t.Fatalf("an unconfirmed OKX entry was booked from paper sizing (%v)", qty)
			}
			if got := env.intentStatuses(sc.ID); len(got) != 1 || got[0] != entryIntentSubmitting {
				t.Fatalf("intents %v", got)
			}
			if adm := env.dispatchOKX(sc, 1, 100, entryTestDecision(bar, at.Add(time.Minute)), at.Add(time.Minute)); !adm.Held || venue.calls != 1 {
				t.Fatalf("an unresolved OKX entry must hold without resending: held=%t calls=%d", adm.Held, venue.calls)
			}

			env = env.restart()
			prev := runOKXRecoverEntryFn
			t.Cleanup(func() { runOKXRecoverEntryFn = prev })
			recCalls := 0
			runOKXRecoverEntryFn = func(script, symbol, inst, cid string, sinceMs, untilMs int64) (*entryRecoveryResult, string, error) {
				recCalls++
				if cid != venue.opts[0].ClientOrderID || inst != instType {
					return nil, "", fmt.Errorf("recovery asked for %s %s", inst, cid)
				}
				return &entryRecoveryResult{Platform: "okx", Order: &entryRecoveryOrder{Found: true, Status: "closed", OrderID: "okx-1", ExecutedSz: 0.3, Terminal: true, FillQty: 0.3, FillNotional: 30.3, FillFee: 0.03, FillCount: 1, FillAvgPx: 101, FillsComplete: true}}, "", nil
			}
			recoverEntryIntents(env.cfg, env.state, env.store, &env.mu, nil, nil, time.Now())
			if qty, side := env.position(sc.ID, "BTC"); recCalls != 1 || qty != 0.3 || side != "long" {
				t.Fatalf("recovered OKX book %v %q (calls %d)", qty, side, recCalls)
			}
			if got := env.intentStatuses(sc.ID); got[0] != entryIntentCommitted {
				t.Fatalf("intents after recovery %v", got)
			}
			if n := env.openTradeRows(sc.ID); n != 1 {
				t.Fatalf("OKX recovery wrote %d opening trades", n)
			}
			if got := env.consumedBar(sc.ID, "BTC", "long"); got != bar.UnixMilli() {
				t.Fatalf("OKX consumed bar %d", got)
			}
		})
	}
}

func TestClosedBarOKXLiveEntryConfirmedFillBooksAndPerpsReversalCloseIsReduceOnly(t *testing.T) {
	sc := StrategyConfig{ID: "okx-live", Type: "perps", Platform: "okx", Script: "shared_scripts/check_okx.py",
		Args: []string{"breakout", "BTC", "1h", "--mode=live", "--inst-type=swap"}, ClosedBarDecisions: true,
		Direction: DirectionBoth, Capital: 1000, Leverage: 1, SizingLeverage: 1}
	env := openEntryTestEnv(t, newEntryTestConfig(t, sc))
	venue := &entryOKXVenue{res: &OKXExecuteResult{OrderOutcome: "filled", OrderStatus: "closed", Execution: &OKXExecution{Action: "buy", Symbol: "BTC", Size: 0.2, Fill: &OKXFill{AvgPx: 100, TotalSz: 0.2, Fee: 0.01, OID: "o-1"}}}}
	venue.install(t)
	at := time.Now().Add(-5 * time.Minute)
	bar := at.Truncate(time.Hour).Add(-time.Hour)
	env.dispatchOKX(sc, 1, 100, entryTestDecision(bar, at), at)
	if qty, side := env.position(sc.ID, "BTC"); qty != 0.2 || side != "long" {
		t.Fatalf("confirmed OKX fill booked %v %q", qty, side)
	}
	if got := env.intentStatuses(sc.ID); len(got) != 1 || got[0] != entryIntentCommitted {
		t.Fatalf("intents %v", got)
	}
	plainCalls := 0
	prev := okxExecuteFn
	t.Cleanup(func() { okxExecuteFn = prev })
	okxExecuteFn = func(script, symbol, side string, size float64, instType string) (*OKXExecuteResult, string, error) {
		plainCalls++
		return nil, "", errors.New("the guarded reversal close must use the reduce-only path")
	}
	venue.res = &OKXExecuteResult{Execution: &OKXExecution{Action: "sell", Symbol: "BTC", Size: 0.2, Fill: &OKXFill{AvgPx: 99, TotalSz: 0.2, OID: "o-2"}}}
	flip := at.Add(time.Minute)
	adm := env.dispatchOKX(sc, -1, 99, entryTestDecision(bar, flip), flip)
	if !adm.CloseOnly || plainCalls != 0 {
		t.Fatalf("reversal closeOnly=%t plain calls=%d", adm.CloseOnly, plainCalls)
	}
	last := venue.opts[len(venue.opts)-1]
	if !last.ReduceOnly || last.ClientOrderID != "" {
		t.Fatalf("reversal close options %+v, want reduce-only with no entry id", last)
	}
	if qty, _ := env.position(sc.ID, "BTC"); qty != 0 {
		t.Fatalf("reversal left %v", qty)
	}
}

func TestClosedBarBinanceUSPaperSpotEntryUsesTheSameAllowance(t *testing.T) {
	sc := StrategyConfig{ID: "bus-cb", Type: "spot", Platform: "binanceus", Script: "shared_scripts/check_strategy.py",
		Args: []string{"breakout", "BTC/USDT", "1h", "--mode=paper"}, ClosedBarDecisions: true, Capital: 1000}
	env := openEntryTestEnv(t, newEntryTestConfig(t, sc))
	dispatch := func(signal int, fields StrategyDecisionFields, now time.Time) (entryAdmission, int) {
		s := env.strat(sc.ID)
		result := &SpotResult{StrategyDecisionFields: fields, Symbol: "BTC/USDT", Signal: signal, Price: 100, Indicators: map[string]interface{}{"atr": 2.0}}
		qty, side := 0.0, ""
		env.mu.RLock()
		if p := s.Positions["BTC/USDT"]; p != nil {
			qty, side = p.Quantity, p.Side
		}
		env.mu.RUnlock()
		adm := admitClosedBarEntry(sc, s, &env.mu, result.Symbol, result.StrategyDecisionFields, result.Signal, result.CloseFraction, qty, side, now)
		applyEntryAdmission(adm, &result.Signal, &result.CloseFraction)
		env.mu.Lock()
		defer env.mu.Unlock()
		execAt := len(s.TradeHistory)
		end := beginEntryBookingLocked(s, adm, nil)
		trades, _ := executeSpotResult(sc, s, env.fileFor(sc.ID), result, "SIGNAL", 100, nil, &Config{}, HurstGateDecision{}, silentStrategyLogger(sc.ID))
		finishEntryBookingLocked(env.store, s, adm, nil, execAt, end(), entryIntentEvidence{}, silentStrategyLogger(sc.ID))
		return adm, trades
	}
	at := entryTestBar(1).Add(10 * time.Second)
	if adm, n := dispatch(1, entryTestDecision(entryTestBar(0), at), at); !adm.admitted() || n == 0 {
		t.Fatalf("open: admitted=%t trades=%d reason=%q", adm.admitted(), n, adm.HoldReason)
	}
	if _, n := dispatch(-1, entryTestDecision(entryTestBar(0), at.Add(time.Minute)), at.Add(time.Minute)); n != 1 {
		t.Fatalf("the sell close must pass (%d trades)", n)
	}
	if adm, n := dispatch(1, entryTestDecision(entryTestBar(0), at.Add(2*time.Minute)), at.Add(2*time.Minute)); !adm.Held || n != 0 {
		t.Fatalf("same candle re-entry: held=%t trades=%d", adm.Held, n)
	}
	next := entryTestBar(2).Add(10 * time.Second)
	if adm, n := dispatch(1, entryTestDecision(entryTestBar(1), next), next); !adm.admitted() || n == 0 {
		t.Fatalf("next candle: admitted=%t trades=%d", adm.admitted(), n)
	}
}

func entryTestUnknownScaleIn(t *testing.T) (*entryTestEnv, StrategyConfig) {
	t.Helper()
	sc := entryTestHLStrategy("hl-live", "live")
	sc.AllowScaleIn = true
	sc.ScaleIn = &ScaleInConfig{MaxAdds: 3, AddNotionalUSD: 20}
	env := openEntryTestEnv(t, newEntryTestConfig(t, sc))
	useEntryTradeRecorder(t, env.store)
	s := env.strat(sc.ID)
	env.mu.Lock()
	s.Positions["BTC"] = &Position{Symbol: "BTC", Quantity: 0.4, InitialQuantity: 0.4, AvgCost: 100, EntryATR: 2, Side: "long", Multiplier: 1, Leverage: 1, OwnerStrategyID: sc.ID}
	env.mu.Unlock()
	at := time.Now().Add(-10 * time.Minute)
	bar := at.Truncate(time.Hour).Add(-time.Hour)
	adm := admitClosedBarEntry(sc, s, &env.mu, "BTC", entryTestDecision(bar, at), 1, 0, 0.4, "long", at)
	if !adm.admitted() || adm.Kind != entryIncreaseAdd {
		t.Fatalf("fixture add admission: admitted=%t kind=%s reason=%q", adm.admitted(), adm.Kind, adm.HoldReason)
	}
	in, err := reserveLiveEntryIntent(env.store, sc, s, &env.mu, adm, entryProviderHyperliquid, entryIntentRequest{Kind: adm.Kind.String(), Signal: 1, PrevPosQty: 0.4, PrevPosSide: "long", PrevAvgCost: 100, Price: 100, EntryATR: 2}, at)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hyperliquidEntrySubmitGate(env.store, s, &env.mu, in)("buy", 0.2, 0); err != nil {
		t.Fatal(err)
	}
	env.saveAll()
	if got := env.intentStatuses(sc.ID); len(got) != 1 || got[0] != entryIntentSubmitting {
		t.Fatalf("fixture intents %v", got)
	}
	return env, sc
}

func (e *entryTestEnv) hlAccountSync(sc StrategyConfig, onChainQty, entryPx float64) {
	e.t.Helper()
	positions := []HLPosition{{Coin: "BTC", Size: onChainQty, EntryPrice: entryPx, Leverage: 1}}
	resolver := hlReconcileFillResolver(func(string, int64, float64) (HLFillLookup, bool) { return HLFillLookup{}, false })
	var alerts []ProtectionFillAlert
	var dms []string
	e.mu.Lock()
	reconcileHyperliquidPositionsForStrategy(sc, e.strat(sc.ID), "BTC", positions, resolver, silentStrategyLogger(sc.ID), &alerts, nil, &dms)
	e.mu.Unlock()
}

func entryTestScaleInFill() *entryRecoveryResult {
	return &entryRecoveryResult{Platform: "hyperliquid", Order: &entryRecoveryOrder{Found: true, Status: "filled", OrderID: "903", OrigSz: 0.2, ExecutedSz: 0.2, Terminal: true, FillQty: 0.2, FillNotional: 20.3, FillFee: 0.01, FillCount: 1, FillAvgPx: 101.5, FillsComplete: true}}
}

func TestClosedBarLiveScaleInRecoveryAfterAccountSyncBooksTheAddOnce(t *testing.T) {
	env, sc := entryTestUnknownScaleIn(t)
	env.hlAccountSync(sc, 0.6, 100.5)
	if qty, _ := env.position(sc.ID, "BTC"); qty != 0.6 {
		t.Fatalf("the sole-owner account sync did not copy the on-chain size into the book (%v); the fixture does not reproduce the case", qty)
	}
	env.mu.RLock()
	cashBefore := env.strat(sc.ID).Cash
	env.mu.RUnlock()
	stub := &entryRecoveryStub{res: entryTestScaleInFill()}
	installHLRecoveryStub(t, stub)
	backdateEntryDeadlines(t, env, sc.ID)
	recoverEntryIntents(env.cfg, env.state, env.store, &env.mu, nil, nil, time.Now())

	if got := env.intentStatuses(sc.ID); len(got) != 1 || got[0] != entryIntentCommitted {
		t.Fatalf("intents %v, want [committed]", got)
	}
	qty, side := env.position(sc.ID, "BTC")
	if math.Abs(qty-0.6) > 1e-12 || side != "long" {
		t.Fatalf("book after recovery %v %q, want 0.6 long", qty, side)
	}
	env.mu.RLock()
	pos := env.strat(sc.ID).Positions["BTC"]
	avg, adds, cashAfter := pos.AvgCost, pos.ScaleInCount, env.strat(sc.ID).Cash
	env.mu.RUnlock()
	if want := (0.4*100 + 0.2*101.5) / 0.6; math.Abs(avg-want) > 1e-9 {
		t.Fatalf("average cost %v, want the pre-add book blended with the fill %v", avg, want)
	}
	if adds != 1 {
		t.Fatalf("scale-in count %d, want 1", adds)
	}
	if math.Abs((cashBefore-cashAfter)-0.01) > 1e-12 {
		t.Fatalf("cash moved %v, want the 0.01 fill fee once", cashBefore-cashAfter)
	}
	if n := env.openTradeRows(sc.ID); n != 1 {
		t.Fatalf("recovery wrote %d add rows, want 1", n)
	}
	env.hlAccountSync(sc, 0.6, 100.5)
	recoverEntryIntents(env.cfg, env.state, env.store, &env.mu, nil, nil, time.Now())
	env = env.restart()
	recoverEntryIntents(env.cfg, env.state, env.store, &env.mu, nil, nil, time.Now())
	if qty, _ := env.position(sc.ID, "BTC"); math.Abs(qty-0.6) > 1e-12 || stub.calls != 1 || env.openTradeRows(sc.ID) != 1 {
		t.Fatalf("the add was applied again: qty=%v calls=%d rows=%d", qty, stub.calls, env.openTradeRows(sc.ID))
	}
}

func TestClosedBarLiveScaleInRecoveryHoldsWhenTheBookMovedOtherwise(t *testing.T) {
	env, sc := entryTestUnknownScaleIn(t)
	env.hlAccountSync(sc, 0.5, 100.5)
	stub := &entryRecoveryStub{res: entryTestScaleInFill()}
	installHLRecoveryStub(t, stub)
	backdateEntryDeadlines(t, env, sc.ID)
	recoverEntryIntents(env.cfg, env.state, env.store, &env.mu, nil, nil, time.Now())

	if got := env.intentStatuses(sc.ID); len(got) != 1 || got[0] != entryIntentSubmitting {
		t.Fatalf("intents %v, want the attempt held as submitting", got)
	}
	if qty, _ := env.position(sc.ID, "BTC"); qty != 0.5 {
		t.Fatalf("a held recovery changed the book to %v", qty)
	}
	if n := env.openTradeRows(sc.ID); n != 0 {
		t.Fatalf("a held recovery wrote %d rows", n)
	}
	env.mu.RLock()
	var reason string
	for _, in := range env.strat(sc.ID).EntryGuard.Intents {
		reason = in.HoldReason
	}
	env.mu.RUnlock()
	if !strings.Contains(reason, "position changed since the scale-in was sent") {
		t.Fatalf("hold reason %q", reason)
	}
}

func TestClosedBarOKXLiveEntryVenueDeadlineGatesRelease(t *testing.T) {
	sc := StrategyConfig{ID: "okx-live", Type: "perps", Platform: "okx", Script: "shared_scripts/check_okx.py",
		Args: []string{"breakout", "BTC", "1h", "--mode=live", "--inst-type=swap"}, ClosedBarDecisions: true,
		Direction: DirectionLong, Capital: 1000, Leverage: 1, SizingLeverage: 1}
	useEntryTestOKXAccount(t)
	newAttempt := func(t *testing.T) (*entryTestEnv, *entryOKXVenue, time.Time, time.Time) {
		env := openEntryTestEnv(t, newEntryTestConfig(t, sc))
		useEntryTradeRecorder(t, env.store)
		venue := &entryOKXVenue{res: &OKXExecuteResult{OrderOutcome: "unknown", Error: "order placement failed: network error"}}
		venue.install(t)
		at := time.Now().Add(-5 * time.Minute)
		bar := at.Truncate(time.Hour).Add(-time.Hour)
		before := time.Now()
		env.dispatchOKX(sc, 1, 100, entryTestDecision(bar, at), at)
		if venue.calls != 1 {
			t.Fatalf("fixture calls %d", venue.calls)
		}
		opts := venue.opts[0]
		if opts.ClientOrderID == "" || opts.ExpiresAfterMs < before.Add(okxEntryOrderExpiry).UnixMilli() {
			t.Fatalf("the guarded OKX order carries no venue deadline: %+v", opts)
		}
		req, err := parseEntryIntentRequest(env.strat(sc.ID).EntryGuard.unresolvedIntent())
		if err != nil || req.ExpiresAfterMs != opts.ExpiresAfterMs {
			t.Fatalf("saved deadline %d (err %v) differs from the sent deadline %d", req.ExpiresAfterMs, err, opts.ExpiresAfterMs)
		}
		args := buildOKXExecuteArgs("BTC", "buy", 0.3, "swap", opts)
		if !slices.Contains(args, fmt.Sprintf("--exp-time-ms=%d", opts.ExpiresAfterMs)) || !slices.Contains(args, "--client-order-id="+opts.ClientOrderID) {
			t.Fatalf("execute argv %v does not carry the deadline", args)
		}
		return env, venue, at, bar
	}
	stubOKX := func(t *testing.T, results ...*entryRecoveryResult) *int {
		prev := runOKXRecoverEntryFn
		t.Cleanup(func() { runOKXRecoverEntryFn = prev })
		calls := 0
		runOKXRecoverEntryFn = func(script, symbol, inst, cid string, sinceMs, untilMs int64) (*entryRecoveryResult, string, error) {
			res := results[len(results)-1]
			if calls < len(results) {
				res = results[calls]
			}
			calls++
			return res, "", nil
		}
		return &calls
	}
	missing := &entryRecoveryResult{Platform: "okx", Account: okxEntryAccountKey("swap"), Order: &entryRecoveryOrder{Found: false, Coverage: entryTestCoverage()}}

	t.Run("unsent order released only after the deadline and never resent before", func(t *testing.T) {
		env, venue, at, bar := newAttempt(t)
		calls := stubOKX(t, missing)
		recoverEntryIntents(env.cfg, env.state, env.store, &env.mu, nil, nil, time.Now())
		if got := env.intentStatuses(sc.ID); got[0] != entryIntentSubmitting || *calls != 1 {
			t.Fatalf("before the deadline: intents %v calls %d", got, *calls)
		}
		if adm := env.dispatchOKX(sc, 1, 100, entryTestDecision(bar, at.Add(time.Minute)), at.Add(time.Minute)); !adm.Held || venue.calls != 1 {
			t.Fatalf("held attempt: held=%t sends=%d", adm.Held, venue.calls)
		}
		backdateEntryDeadlines(t, env, sc.ID)
		recoverEntryIntents(env.cfg, env.state, env.store, &env.mu, nil, nil, time.Now().Add(2*entryRecoveryQueryInterval))
		if got := env.intentStatuses(sc.ID); got[0] != entryIntentNoFill {
			t.Fatalf("after the deadline: intents %v", got)
		}
		if adm := env.dispatchOKX(sc, 1, 100, entryTestDecision(bar, at.Add(2*time.Minute)), at.Add(2*time.Minute)); !adm.admitted() || venue.calls != 2 {
			t.Fatalf("released attempt: admitted=%t sends=%d reason=%q", adm.admitted(), venue.calls, adm.HoldReason)
		}
	})

	t.Run("a fill missed once before the deadline stays held and is booked when found", func(t *testing.T) {
		env, _, _, _ := newAttempt(t)
		filled := &entryRecoveryResult{Platform: "okx", Order: &entryRecoveryOrder{Found: true, Status: "closed", OrderID: "okx-1", ExecutedSz: 0.3, Terminal: true, FillQty: 0.3, FillNotional: 30.3, FillFee: 0.03, FillCount: 1, FillAvgPx: 101, FillsComplete: true}}
		calls := stubOKX(t, missing, filled)
		recoverEntryIntents(env.cfg, env.state, env.store, &env.mu, nil, nil, time.Now())
		if got := env.intentStatuses(sc.ID); got[0] != entryIntentSubmitting {
			t.Fatalf("a not-found read before the deadline released the attempt: %v", got)
		}
		recoverEntryIntents(env.cfg, env.state, env.store, &env.mu, nil, nil, time.Now().Add(2*entryRecoveryQueryInterval))
		if qty, _ := env.position(sc.ID, "BTC"); *calls != 2 || qty != 0.3 || env.intentStatuses(sc.ID)[0] != entryIntentCommitted {
			t.Fatalf("calls=%d qty=%v intents=%v", *calls, qty, env.intentStatuses(sc.ID))
		}
	})

	t.Run("an order the venue still shows is never released at the deadline", func(t *testing.T) {
		env, _, _, _ := newAttempt(t)
		stubOKX(t, &entryRecoveryResult{Platform: "okx", Order: &entryRecoveryOrder{Found: true, Status: "open", OrderID: "okx-1", Terminal: false}})
		backdateEntryDeadlines(t, env, sc.ID)
		recoverEntryIntents(env.cfg, env.state, env.store, &env.mu, nil, nil, time.Now())
		if got := env.intentStatuses(sc.ID); got[0] != entryIntentSubmitting {
			t.Fatalf("an existing order was released: %v", got)
		}
	})
}
