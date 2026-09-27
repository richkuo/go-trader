package main

import (
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
)

type forceCloseHarness struct {
	d         manualCoreDeps
	db        *StateDB
	sized     []hlSizedCloseRequest
	whole     int
	updateSLs []float64
	chain     []HLPosition
	chainErr  error
	result    func(req hlSizedCloseRequest) (*HyperliquidCloseResult, error)
}

func newForceCloseHarness(t *testing.T, pos *Position, peers []*Position) *forceCloseHarness {
	t.Helper()
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xoperator")
	h := &forceCloseHarness{db: openTestDB(t)}
	sc := hedgePerpsStrategy("s1", "ETH")
	cfg := &Config{Strategies: []StrategyConfig{sc}}
	states := map[string]*StrategyState{"s1": {ID: "s1", Positions: map[string]*Position{"ETH": pos}}}
	for i, p := range peers {
		id := fmt.Sprintf("p%d", i+1)
		cfg.Strategies = append(cfg.Strategies, hedgePerpsStrategy(id, "ETH"))
		states[id] = &StrategyState{ID: id, Positions: map[string]*Position{"ETH": p}}
	}
	h.d = manualCoreDeps{
		cfg:     cfg,
		stateDB: openTestStore(t, h.db),
		loadState: func(strategyID, symbol string) (manualStateView, error) {
			v := manualStateView{HasStrategy: true}
			if p := states[strategyID].Positions[symbol]; p != nil {
				cp := *p
				v.Pos = &cp
				v.PeerSame, v.PeerOppQty = hlPeerBookListOnCoin(states, cfg.Strategies, symbol, strategyID, p.Side)
				v.PeerSameQty = hlShareBookSum(v.PeerSame)
			}
			return v, nil
		},
		fetchMids: func(coins []string) (map[string]float64, error) {
			return map[string]float64{"ETH": 2000}, nil
		},
		fetchPositions: func(string) ([]HLPosition, error) { return h.chain, h.chainErr },
		closer: func(symbol string, partialSz *float64, oids []int64) (*HyperliquidCloseResult, error) {
			h.whole++
			return &HyperliquidCloseResult{Close: &HyperliquidClose{Fill: &HyperliquidCloseFill{AvgPx: 2000, TotalSz: pos.Quantity}}}, nil
		},
		sizedCloser: func(req hlSizedCloseRequest) (*HyperliquidCloseResult, error) {
			h.sized = append(h.sized, req)
			if h.result != nil {
				return h.result(req)
			}
			return sizedFill(req.Size, req.CancelOIDs...), nil
		},
		updateSL: func(script, symbol, side string, size, triggerPx float64, cancelOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
			h.updateSLs = append(h.updateSLs, size)
			return &HyperliquidStopLossUpdateResult{StopLossOID: 999, StopLossTriggerPx: triggerPx, CancelStopLossSucceeded: true}, "", nil
		},
		recordRearmedStopLoss: func(string, string, string, float64, int64, *HyperliquidStopLossUpdateResult) error { return nil },
		syncProtection: func(sc StrategyConfig, plan hlProtectionPlan) (*HyperliquidProtectionSyncResult, string, error) {
			return &HyperliquidProtectionSyncResult{}, "", nil
		},
		recordRestoredTakeProfits: func(string, string, string, string, []manualCloseTPTierOutcome) error { return nil },
	}
	return h
}

func sizedFill(sz float64, cancelled ...int64) *HyperliquidCloseResult {
	return &HyperliquidCloseResult{
		OrderOutcome:                "filled",
		Close:                       &HyperliquidClose{Symbol: "ETH", Fill: &HyperliquidCloseFill{AvgPx: 2000, TotalSz: sz, OID: 77, Fee: 0.1}, SubmittedSz: sz},
		CancelStopLossSucceeded:     len(cancelled) > 0,
		CancelStopLossSucceededOIDs: cancelled,
	}
}

func (h *forceCloseHarness) run(t *testing.T, qty float64) (*manualCoreResult, error) {
	t.Helper()
	return forceCloseCore(h.d, hedgePerpsStrategy("s1", "ETH"), "ETH", forceCloseInputs{StrategyID: "s1", Qty: qty})
}

func (h *forceCloseHarness) queuedCloses(t *testing.T) []PendingManualAction {
	t.Helper()
	actions, err := h.db.LoadPendingManualActions()
	if err != nil {
		t.Fatalf("LoadPendingManualActions: %v", err)
	}
	var out []PendingManualAction
	for _, a := range actions {
		if a.Action == "close" {
			out = append(out, a)
		}
	}
	return out
}

func book(side string, qty float64) *Position {
	return &Position{Symbol: "ETH", Side: side, Quantity: qty, InitialQuantity: qty, AvgCost: 2000, Multiplier: 1,
		StopLossOID: 111, StopLossTriggerPx: 1900, TPOIDs: []int64{222}, TPArmedTiers: []bool{true}}
}

func TestForceCloseSizesSharedCoinClosesAgainstTheStrategyAndPeers(t *testing.T) {
	cases := []struct {
		name     string
		pos      *Position
		peers    []*Position
		chain    float64
		qty      float64
		wantSide string
		wantMode hlCloseMode
		wantSize float64
	}{
		{"same-side peer", book("long", 1), []*Position{book("long", 2)}, 3, 0, "sell", hlCloseModeReduceOnly, 1},
		{"opposite-side peer long sells, never buys from the net", book("long", 1), []*Position{book("short", 3)}, -2, 0, "sell", hlCloseModeCross, 1},
		{"opposite-side peer short buys", book("short", 1), []*Position{book("long", 3)}, 2, 0, "buy", hlCloseModeCross, 1},
		{"short with same-side peer", book("short", 1), []*Position{book("short", 1)}, -2, 0, "buy", hlCloseModeReduceOnly, 1},
		{"chain below the book caps the close", book("long", 1), []*Position{book("long", 2)}, 2.5, 0, "sell", hlCloseModeReduceOnly, 0.5},
		{"sole-owner partial", book("long", 1), nil, 1, 0.4, "sell", hlCloseModeReduceOnly, 0.4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newForceCloseHarness(t, tc.pos, tc.peers)
			h.chain = []HLPosition{{Coin: "ETH", Size: tc.chain}}
			if _, err := h.run(t, tc.qty); err != nil {
				t.Fatalf("forceCloseCore: %v", err)
			}
			if h.whole != 0 || len(h.sized) != 1 {
				t.Fatalf("whole=%d sized=%v, want exactly one sized close", h.whole, h.sized)
			}
			req := h.sized[0]
			if req.Side != tc.wantSide || req.Mode != tc.wantMode || math.Abs(req.Size-tc.wantSize) > 1e-9 {
				t.Fatalf("req = %+v, want side=%s mode=%s size=%v", req, tc.wantSide, tc.wantMode, tc.wantSize)
			}
			full := tc.qty == 0 && tc.wantSize >= tc.pos.Quantity-1e-9
			if full != (len(req.CancelOIDs) > 0) {
				t.Fatalf("cancel OIDs = %v, want cancel only on an uncapped full close", req.CancelOIDs)
			}
			if full && math.Abs(req.CancelMinFill-(tc.pos.Quantity-0.0001)) > 1e-12 {
				t.Fatalf("cancel threshold = %v, want the book less the full-close tolerance", req.CancelMinFill)
			}
			closes := h.queuedCloses(t)
			if len(closes) != 1 || math.Abs(closes[0].Quantity-tc.wantSize) > 1e-9 {
				t.Fatalf("queued closes = %+v, want one booking of the %v fill", closes, tc.wantSize)
			}
			if closes[0].IsFullClose != full {
				t.Fatalf("IsFullClose = %v, want %v", closes[0].IsFullClose, full)
			}
		})
	}
}

func TestForceCloseCapInsideTheFullCloseToleranceKeepsTheBook(t *testing.T) {
	pos := book("long", 0.01)
	h := newForceCloseHarness(t, pos, []*Position{book("long", 0.02)})
	h.chain = []HLPosition{{Coin: "ETH", Size: 0.02995}}
	res, err := h.run(t, 0)
	if err != nil {
		t.Fatalf("forceCloseCore: %v", err)
	}
	if len(h.sized) != 1 || len(h.sized[0].CancelOIDs) != 0 || math.Abs(h.sized[0].Size-0.00995) > 1e-9 {
		t.Fatalf("sized = %+v, want a capped 0.00995 close with no protection cancel", h.sized)
	}
	closes := h.queuedCloses(t)
	if len(closes) != 1 || closes[0].IsFullClose || math.Abs(closes[0].Quantity-0.00995) > 1e-9 {
		t.Fatalf("queued = %+v, want the capped fill booked as a partial", closes)
	}
	if len(h.updateSLs) != 1 {
		t.Fatalf("stop updates = %v, want the short-fill re-arm (%v)", h.updateSLs, res.lines)
	}
}

func TestForceCloseCappedFullCloseResizesTheRemainderProtection(t *testing.T) {
	h := newForceCloseHarness(t, book("long", 1), []*Position{book("long", 2)})
	h.chain = []HLPosition{{Coin: "ETH", Size: 2.5}}
	res, err := h.run(t, 0)
	if err != nil {
		t.Fatalf("forceCloseCore: %v", err)
	}
	if len(h.updateSLs) != 1 {
		t.Fatalf("stop updates = %v, want one verify-first remainder re-arm (%v)", h.updateSLs, res.lines)
	}
	closes := h.queuedCloses(t)
	if len(closes) != 1 || closes[0].IsFullClose || math.Abs(closes[0].Quantity-0.5) > 1e-9 {
		t.Fatalf("queued = %+v, want a 0.5 partial booking", closes)
	}
}

func TestForceClosePartialFillIsNotAFullClose(t *testing.T) {
	h := newForceCloseHarness(t, book("long", 1), []*Position{book("long", 2)})
	h.chain = []HLPosition{{Coin: "ETH", Size: 3}}
	h.result = func(req hlSizedCloseRequest) (*HyperliquidCloseResult, error) { return sizedFill(0.6), nil }
	if _, err := h.run(t, 0); err != nil {
		t.Fatalf("forceCloseCore: %v", err)
	}
	if len(h.sized[0].CancelOIDs) == 0 {
		t.Fatalf("an uncapped full close carries the protection for the post-fill cancel")
	}
	closes := h.queuedCloses(t)
	if len(closes) != 1 || closes[0].IsFullClose || math.Abs(closes[0].Quantity-0.6) > 1e-9 {
		t.Fatalf("queued = %+v, want a 0.6 partial booking", closes)
	}
	if len(h.updateSLs) != 1 || math.Abs(h.updateSLs[0]) > 1 {
		t.Fatalf("stop updates = %v, want one re-arm sized to the remainder share", h.updateSLs)
	}
}

func TestForceCloseSendsNothingWithoutAUsablePlanOrFill(t *testing.T) {
	t.Run("skipped plan", func(t *testing.T) {
		h := newForceCloseHarness(t, book("long", 1), []*Position{book("long", 2)})
		h.chain = []HLPosition{{Coin: "ETH", Size: 2}}
		_, err := h.run(t, 0)
		if err == nil || len(h.sized) != 0 || h.whole != 0 || len(h.updateSLs) != 0 {
			t.Fatalf("err=%v sized=%v whole=%d updates=%v, want a refusal with no order", err, h.sized, h.whole, h.updateSLs)
		}
	})
	t.Run("failed read with an opposite peer defers", func(t *testing.T) {
		h := newForceCloseHarness(t, book("long", 1), []*Position{book("short", 3)})
		h.chainErr = fmt.Errorf("clearinghouse down")
		_, err := h.run(t, 0)
		if err == nil || len(h.sized) != 0 {
			t.Fatalf("err=%v sized=%v, want a deferral with no order", err, h.sized)
		}
	})
	t.Run("failed read with no opposite peer sends uncapped reduce-only", func(t *testing.T) {
		h := newForceCloseHarness(t, book("long", 1), []*Position{book("long", 2)})
		h.chainErr = fmt.Errorf("clearinghouse down")
		if _, err := h.run(t, 0); err != nil {
			t.Fatalf("forceCloseCore: %v", err)
		}
		if len(h.sized) != 1 || h.sized[0].Mode != hlCloseModeReduceOnly || h.sized[0].Size != 1 {
			t.Fatalf("sized = %+v, want reduce-only 1", h.sized)
		}
	})
	for _, tc := range []struct {
		name string
		res  *HyperliquidCloseResult
		err  error
	}{
		{"not sent (floored to zero)", &HyperliquidCloseResult{OrderOutcome: "not_sent", Error: "floors to zero lots"}, fmt.Errorf("close failed: floors to zero lots")},
		{"rejected", &HyperliquidCloseResult{OrderOutcome: "rejected", Error: "could not match"}, fmt.Errorf("close failed: could not match")},
		{"unknown", &HyperliquidCloseResult{OrderOutcome: "unknown", Error: "socket closed"}, fmt.Errorf("close failed: socket closed")},
		{"zero fill", &HyperliquidCloseResult{OrderOutcome: "filled", Close: &HyperliquidClose{Fill: &HyperliquidCloseFill{AvgPx: 2000, TotalSz: 0}}}, nil},
		{"absent fill", &HyperliquidCloseResult{OrderOutcome: "filled", Close: &HyperliquidClose{}}, nil},
		{"non-finite fill", &HyperliquidCloseResult{OrderOutcome: "filled", Close: &HyperliquidClose{Fill: &HyperliquidCloseFill{AvgPx: 2000, TotalSz: math.Inf(1)}}}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newForceCloseHarness(t, book("long", 1), []*Position{book("long", 2)})
			h.chain = []HLPosition{{Coin: "ETH", Size: 3}}
			h.result = func(hlSizedCloseRequest) (*HyperliquidCloseResult, error) { return tc.res, tc.err }
			_, err := h.run(t, 0)
			if err == nil {
				t.Fatal("want an error")
			}
			if len(h.sized) != 1 {
				t.Fatalf("sized calls = %d, want exactly one (no blind second order)", len(h.sized))
			}
			if closes := h.queuedCloses(t); len(closes) != 0 {
				t.Fatalf("queued = %+v, want nothing booked", closes)
			}
			if len(h.updateSLs) != 0 {
				t.Fatalf("stop updates = %v, want protection untouched", h.updateSLs)
			}
		})
	}
}

func TestForceCloseCoupledHedgeLegUsesTheLegSideAndChain(t *testing.T) {
	cases := []struct {
		name      string
		chain     []HLPosition
		chainErr  error
		wantSends int
		wantMode  hlCloseMode
		wantSize  float64
	}{
		{"matching chain", []HLPosition{{Coin: "BTC", Size: -0.4}}, nil, 1, hlCloseModeReduceOnly, 0.4},
		{"chain below the book", []HLPosition{{Coin: "BTC", Size: -0.25}}, nil, 1, hlCloseModeReduceOnly, 0.25},
		{"flat chain", nil, nil, 0, 0, 0},
		{"mismatched side", []HLPosition{{Coin: "BTC", Size: 0.4}}, nil, 0, 0, 0},
		{"failed refetch", nil, fmt.Errorf("down"), 1, hlCloseModeReduceOnly, 0.4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xoperator")
			db := openTestDB(t)
			sc := hedgeTestConfig()
			var sent []hlSizedCloseRequest
			d := manualCoreDeps{
				cfg:     &Config{Strategies: []StrategyConfig{sc}},
				stateDB: openTestStore(t, db),
				loadState: func(strategyID, symbol string) (manualStateView, error) {
					return manualStateView{HasStrategy: true, Pos: hedgePos(0.4, "short", 10)}, nil
				},
				fetchPositions: func(string) ([]HLPosition, error) { return tc.chain, tc.chainErr },
				sizedCloser: func(req hlSizedCloseRequest) (*HyperliquidCloseResult, error) {
					sent = append(sent, req)
					return &HyperliquidCloseResult{OrderOutcome: "filled", Close: &HyperliquidClose{Fill: &HyperliquidCloseFill{AvgPx: 51000, TotalSz: req.Size, OID: 5}}}, nil
				},
			}
			res := &manualCoreResult{}
			forceCloseCoupledHedgeLeg(d, sc, res, "eth-long", "ETH", 10, 10, true)
			if len(sent) != tc.wantSends {
				t.Fatalf("sends = %+v, want %d (%v)", sent, tc.wantSends, res.lines)
			}
			if tc.wantSends == 0 {
				return
			}
			if sent[0].Side != "buy" || sent[0].Mode != tc.wantMode || math.Abs(sent[0].Size-tc.wantSize) > 1e-9 {
				t.Fatalf("req = %+v, want buy %s %v", sent[0], tc.wantMode, tc.wantSize)
			}
		})
	}
}

func TestForceCloseCoupledHedgeLegLotFloorIsNotOversized(t *testing.T) {
	run := func(t *testing.T, held, submitted, filled float64, primaryFull bool, primaryClosed, primaryBefore float64) *manualCoreResult {
		t.Helper()
		t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xoperator")
		db := openTestDB(t)
		sc := hedgeTestConfig()
		d := manualCoreDeps{
			cfg:     &Config{Strategies: []StrategyConfig{sc}},
			stateDB: openTestStore(t, db),
			loadState: func(strategyID, symbol string) (manualStateView, error) {
				return manualStateView{HasStrategy: true, Pos: hedgePos(held, "short", 10)}, nil
			},
			sizedCloser: func(req hlSizedCloseRequest) (*HyperliquidCloseResult, error) {
				return &HyperliquidCloseResult{OrderOutcome: "filled", Close: &HyperliquidClose{
					SubmittedSz: submitted,
					Fill:        &HyperliquidCloseFill{AvgPx: 51000, TotalSz: filled, OID: 5},
				}}, nil
			},
		}
		res := &manualCoreResult{}
		forceCloseCoupledHedgeLeg(d, sc, res, "eth-long", "ETH", primaryClosed, primaryBefore, primaryFull)
		return res
	}
	oversized := func(res *manualCoreResult) bool {
		for _, l := range res.lines {
			if strings.Contains(l.text, "OVERSIZED") {
				return true
			}
		}
		return false
	}
	t.Run("fill equals the lot-floored submitted size", func(t *testing.T) {
		res := run(t, 0.37, 0.1233, 0.1233, false, 1, 3)
		if oversized(res) {
			t.Fatalf("a complete fill of the submitted size must not be OVERSIZED: %v", res.lines)
		}
	})
	t.Run("fill below the submitted size", func(t *testing.T) {
		res := run(t, 0.37, 0.1233, 0.12, false, 1, 3)
		if !oversized(res) {
			t.Fatalf("a fill below the submitted size must name the gap: %v", res.lines)
		}
	})
	t.Run("full close of a whole-lot book", func(t *testing.T) {
		res := run(t, 0.4, 0.4, 0.4, true, 10, 10)
		if oversized(res) {
			t.Fatalf("a complete whole-lot hedge close must not be OVERSIZED: %v", res.lines)
		}
	})
}

func TestHedgeLegReduceUsesThePlanner(t *testing.T) {
	cases := []struct {
		name      string
		chain     hlOnChainCoinView
		chainErr  error
		wantSends int
		wantSize  float64
		wantLeg   bool
	}{
		{"matching chain", hlCoinView("BTC", -0.4), nil, 1, 0.4, false},
		{"chain below the book", hlCoinView("BTC", -0.3), nil, 1, 0.3, true},
		{"flat chain clears the leg", hlCoinView("BTC", 0), nil, 0, 0, false},
		{"mismatched side sends nothing", hlCoinView("BTC", 0.4), nil, 0, 0, true},
		{"failed refetch sends uncapped reduce-only", hlOnChainCoinView{}, fmt.Errorf("down"), 1, 0.4, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prev := tradeRecorder
			tradeRecorder = nil
			t.Cleanup(func() { tradeRecorder = prev })
			sc := hedgeTestConfig()
			s := hedgeTestState("eth-long")
			s.Positions["BTC"] = hedgePos(0.4, "short", 10)
			var mu sync.RWMutex
			var sent []hlSizedCloseRequest
			exec := hedgeExecutor{
				Reduce: func(sc StrategyConfig, req hlSizedCloseRequest) (*HyperliquidCloseResult, error) {
					sent = append(sent, req)
					return closeFill(testHedgePx, req.Size, 1), nil
				},
				Refetch: func() (hlOnChainCoinView, error) { return tc.chain, tc.chainErr },
			}
			runHedgeSync(sc, s, &mu, exec, hedgeSyncInputs{PrimaryPx: testPrimaryPx, HedgePx: testHedgePx, Live: true}, nil, silentStrategyLogger("eth-long"))
			if len(sent) != tc.wantSends {
				t.Fatalf("sends = %+v, want %d", sent, tc.wantSends)
			}
			if tc.wantSends > 0 && (sent[0].Side != "buy" || sent[0].Mode != hlCloseModeReduceOnly || math.Abs(sent[0].Size-tc.wantSize) > 1e-9) {
				t.Fatalf("req = %+v, want reduce-only buy %v", sent[0], tc.wantSize)
			}
			if _, held := s.Positions["BTC"]; held != tc.wantLeg {
				t.Fatalf("hedge leg held = %v, want %v", held, tc.wantLeg)
			}
		})
	}
}

func TestHedgeUnwindSizesThePrimaryAgainstPeers(t *testing.T) {
	cases := []struct {
		name       string
		primary    float64
		fresh      float64
		peers      hedgePrimaryPeers
		chain      hlOnChainCoinView
		fill       float64
		wantSends  int
		wantMode   hlCloseMode
		wantSize   float64
		wantCancel bool
		wantPrim   float64
		wantAlert  string
	}{
		{"sole owner full unwind", 10, 10, hedgePrimaryPeers{Known: true, Side: "long"}, hlCoinView("ETH", 10), 10, 1, hlCloseModeReduceOnly, 10, true, 0, "primary unwound"},
		{"opposite peer: sell crosses", 10, 10, hedgePrimaryPeers{Known: true, Side: "long", OppQty: 15}, hlCoinView("ETH", -5), 10, 1, hlCloseModeCross, 10, true, 0, "primary unwound"},
		{"same-side peer keeps its units", 15, 5, hedgePrimaryPeers{Known: true, Side: "long", SameQty: 4}, hlCoinView("ETH", 19), 5, 1, hlCloseModeReduceOnly, 5, false, 10, "primary unwound"},
		{"chain below the book caps and alerts", 10, 10, hedgePrimaryPeers{Known: true, Side: "long"}, hlCoinView("ETH", 6), 6, 1, hlCloseModeReduceOnly, 6, false, 4, "4.00000000 of the increment is still open"},
		{"partial fill alerts the remainder", 10, 10, hedgePrimaryPeers{Known: true, Side: "long"}, hlCoinView("ETH", 10), 7, 1, hlCloseModeReduceOnly, 10, true, 3, "3.00000000 of the increment is still open"},
		{"unknown peers send nothing", 10, 10, hedgePrimaryPeers{}, hlCoinView("ETH", 10), 0, 0, 0, 0, false, 10, "no order was sent"},
		{"flat net skip is not a resolution", 10, 10, hedgePrimaryPeers{Known: true, Side: "long"}, hlCoinView("ETH", 0), 0, 0, 0, 0, false, 10, "still open and UNHEDGED"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prev := tradeRecorder
			tradeRecorder = nil
			t.Cleanup(func() { tradeRecorder = prev })
			sc := hedgeTestConfig()
			s := hedgeTestState("eth-long")
			pos := primaryPos(tc.primary, "long")
			pos.StopLossOID = 555
			s.Positions["ETH"] = pos
			var mu sync.RWMutex
			var sent []hlSizedCloseRequest
			f := &fakeHedgeExec{openResult: &HyperliquidExecuteResult{Error: "insufficient margin"}}
			exec := f.executor()
			exec.UnwindPrimary = func(sc StrategyConfig, req hlSizedCloseRequest) (*HyperliquidCloseResult, error) {
				sent = append(sent, req)
				return closeFill(testPrimaryPx, tc.fill, 1), nil
			}
			exec.Refetch = func() (hlOnChainCoinView, error) { return tc.chain, nil }
			n := &mockNotifier{}
			notifier := NewMultiNotifier(notifierBackend{notifier: n, ownerID: "o"})
			if tc.fresh < tc.primary {
				s.Positions["BTC"] = hedgePos(0.4, "short", tc.primary-tc.fresh)
			}
			runHedgeSync(sc, s, &mu, exec, hedgeSyncInputs{
				PrimaryPx: testPrimaryPx, HedgePx: testHedgePx, FreshExposureQty: tc.fresh,
				PrimaryCancelOIDs: []int64{555}, PrimaryPeers: tc.peers, Live: true,
			}, notifier, silentStrategyLogger("eth-long"))
			if len(sent) != tc.wantSends {
				t.Fatalf("sends = %+v, want %d", sent, tc.wantSends)
			}
			if tc.wantSends > 0 {
				req := sent[0]
				if req.Side != "sell" || req.Mode != tc.wantMode || math.Abs(req.Size-tc.wantSize) > 1e-9 {
					t.Fatalf("req = %+v, want sell %s %v", req, tc.wantMode, tc.wantSize)
				}
				if (len(req.CancelOIDs) > 0) != tc.wantCancel {
					t.Fatalf("cancel OIDs = %v, want cancel=%v", req.CancelOIDs, tc.wantCancel)
				}
			}
			got := 0.0
			if p := s.Positions["ETH"]; p != nil {
				got = p.Quantity
			}
			if math.Abs(got-tc.wantPrim) > 1e-9 {
				t.Fatalf("primary book = %v, want %v", got, tc.wantPrim)
			}
			var alerts []string
			n.mu.Lock()
			for _, dm := range n.dms {
				alerts = append(alerts, dm.content)
			}
			n.mu.Unlock()
			if !strings.Contains(strings.Join(alerts, "\n"), tc.wantAlert) {
				t.Fatalf("alerts %q do not contain %q", alerts, tc.wantAlert)
			}
		})
	}
}
