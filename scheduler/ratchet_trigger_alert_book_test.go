package main

import (
	"errors"
	"strings"
	"sync"
	"testing"
)

// Numeric fixtures: no tighter current stop, no breach, no pending one-shot
// normalization, and a mark-based high-water mark.

type ratchetBookSender struct {
	mu       *sync.RWMutex
	messages []string
	lockFree []bool
}

func (s *ratchetBookSender) SendOwnerDM(content string) {
	if s.mu != nil {
		free := s.mu.TryLock()
		if free {
			s.mu.Unlock()
		}
		s.lockFree = append(s.lockFree, free)
	}
	s.messages = append(s.messages, content)
}

func ratchetBookPos(sc StrategyConfig, side string, qty, mark, current float64, oid int64) *Position {
	return &Position{
		Symbol: "ETH", Side: side, Quantity: qty, InitialQuantity: qty,
		AvgCost: 100, EntryATR: 10, Multiplier: 1, RiskAnchorPrice: 100,
		TradePositionID: "pos-ratchet", OwnerStrategyID: sc.ID,
		StopLossHighWaterPx: mark, StopLossTriggerPx: current, StopLossOID: oid,
	}
}

func ratchetBookState(sc StrategyConfig, pos *Position) *StrategyState {
	return &StrategyState{
		ID: sc.ID, Type: sc.Type, Platform: "hyperliquid", Cash: 10000,
		Positions: map[string]*Position{pos.Symbol: pos},
	}
}

func ratchetBookLive(sc StrategyConfig) StrategyConfig {
	sc.Script = "x.py"
	sc.Symbol = "ETH"
	sc.Args = []string{"x.py", "ETH", "1h", "--mode=live"}
	return sc
}

type ratchetStubCall struct {
	size, trigger float64
	cancelOID     int64
	called        bool
}

func ratchetInstallStub(t *testing.T, reply *HyperliquidStopLossUpdateResult, replyErr error, call *ratchetStubCall) {
	t.Helper()
	old := runHyperliquidUpdateStopLossFunc
	t.Cleanup(func() { runHyperliquidUpdateStopLossFunc = old })
	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		call.called = true
		call.size = size
		call.trigger = triggerPx
		call.cancelOID = cancelStopLossOID
		return reply, "", replyErr
	}
}

func ratchetFinish(t *testing.T, sc StrategyConfig, st *StrategyState, alert *RatchetTriggerAlert, ev ratchetStopEvidence, enabled bool) *ratchetBookSender {
	t.Helper()
	var mu sync.RWMutex
	sender := &ratchetBookSender{mu: &mu}
	completeAndNotifyRatchetTrigger(sender, enabled, alert, sc, st, "ETH", &mu, ev)
	for i, free := range sender.lockFree {
		if !free {
			t.Errorf("send %d held mu", i)
		}
	}
	return sender
}

func TestRatchetAlertBook_PaperLongAndShort(t *testing.T) {
	var call ratchetStubCall
	ratchetInstallStub(t, &HyperliquidStopLossUpdateResult{StopLossOID: 1, StopLossTriggerPx: 1}, nil, &call)

	longSC := ratchetAlertSC(3.0, tier(1.0, 0, 2.0))
	longPos := ratchetBookPos(longSC, "long", 1, 115, 80, 0)
	longSt := ratchetBookState(longSC, longPos)
	tightened, alert := applyTrailingTPRatchetToPosition(longSC, longPos, "ETH", 115, nil)
	if !tightened || alert == nil {
		t.Fatal("expected a long tighten")
	}
	var mu sync.RWMutex
	_, _, ev := runTrailingStopUpdateAfterRatchetTighten(longSC, longSt, "ETH", 115, nil, nil, nil, &mu, nil, silentStrategyLogger(longSC.ID))
	sender := ratchetFinish(t, longSC, longSt, alert, ev, true)
	if call.called {
		t.Fatal("paper must not call the venue")
	}
	if len(sender.messages) != 1 || len(sender.lockFree) != 1 {
		t.Fatalf("sends=%d lockChecks=%d", len(sender.messages), len(sender.lockFree))
	}
	msg := sender.messages[0]
	if !strings.Contains(msg, "SL trigger: $92.0000 (moved; was $80.0000)") {
		t.Fatalf("long message:\n%s", msg)
	}
	if !strings.Contains(msg, "Trail: 2×ATR (20.0000% of entry anchor) | HWM $115.0000") {
		t.Fatalf("long trail line:\n%s", msg)
	}

	shortSC := ratchetAlertSC(3.0, tier(1.0, 0, 2.0))
	shortPos := ratchetBookPos(shortSC, "short", 1, 85, 110, 0)
	shortSt := ratchetBookState(shortSC, shortPos)
	tightened, alert = applyTrailingTPRatchetToPosition(shortSC, shortPos, "ETH", 85, nil)
	if !tightened || alert == nil {
		t.Fatal("expected a short tighten")
	}
	_, _, ev = runTrailingStopUpdateAfterRatchetTighten(shortSC, shortSt, "ETH", 85, nil, nil, nil, &mu, nil, silentStrategyLogger(shortSC.ID))
	sender = ratchetFinish(t, shortSC, shortSt, alert, ev, true)
	if len(sender.messages) != 1 || !strings.Contains(sender.messages[0], "SL trigger: $102.0000 (moved; was $110.0000)") {
		t.Fatalf("short message: %v", sender.messages)
	}
}

func TestRatchetAlertBook_LiveRoundedMovedAndDecisionUnchanged(t *testing.T) {
	sc := ratchetBookLive(ratchetAlertSC(3.0, tier(1.0, 0, 2.0)))
	pos := ratchetBookPos(sc, "long", 1, 115, 80, 410)
	st := ratchetBookState(sc, pos)
	var call ratchetStubCall
	ratchetInstallStub(t, &HyperliquidStopLossUpdateResult{StopLossOID: 900, StopLossTriggerPx: 91.5}, nil, &call)
	_, alert := applyTrailingTPRatchetToPosition(sc, pos, "ETH", 115, nil)
	var mu sync.RWMutex
	_, _, ev := runTrailingStopUpdateAfterRatchetTighten(sc, st, "ETH", 115, nil, nil, nil, &mu, nil, silentStrategyLogger(sc.ID))
	sender := ratchetFinish(t, sc, st, alert, ev, true)
	if !call.called || call.cancelOID != 410 || !approxEq(call.size, 1) || !approxEq(call.trigger, 92) {
		t.Fatalf("stub call=%+v want trigger 92 size 1 cancel 410", call)
	}
	if len(sender.messages) != 1 || !strings.Contains(sender.messages[0], "SL trigger: $91.5000 (moved; was $80.0000)") {
		t.Fatalf("message: %v", sender.messages)
	}
}

func TestRatchetAlertBook_TighterStopUnchanged(t *testing.T) {
	sc := ratchetBookLive(ratchetAlertSC(3.0, tier(1.0, 0, 2.0)))
	pos := ratchetBookPos(sc, "long", 1, 115, 96, 55)
	st := ratchetBookState(sc, pos)
	var call ratchetStubCall
	ratchetInstallStub(t, &HyperliquidStopLossUpdateResult{StopLossOID: 1, StopLossTriggerPx: 1}, nil, &call)
	_, alert := applyTrailingTPRatchetToPosition(sc, pos, "ETH", 115, nil)
	var mu sync.RWMutex
	_, _, ev := runTrailingStopUpdateAfterRatchetTighten(sc, st, "ETH", 115, nil, nil, nil, &mu, nil, silentStrategyLogger(sc.ID))
	sender := ratchetFinish(t, sc, st, alert, ev, true)
	if call.called {
		t.Fatal("a tighter resting stop must not be replaced")
	}
	if len(sender.messages) != 1 || !strings.Contains(sender.messages[0], "SL trigger: $96.0000 (unchanged)") {
		t.Fatalf("message: %v", sender.messages)
	}
}

func TestRatchetAlertBook_LiquidationClamp(t *testing.T) {
	sc := ratchetBookLive(ratchetAlertSC(3.0, tier(1.0, 0, 2.0)))
	pos := ratchetBookPos(sc, "long", 1, 115, 80, 410)
	st := ratchetBookState(sc, pos)
	var call ratchetStubCall
	ratchetInstallStub(t, nil, nil, &call)
	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		call.called = true
		call.size = size
		call.trigger = triggerPx
		call.cancelOID = cancelStopLossOID
		return &HyperliquidStopLossUpdateResult{StopLossOID: 901, StopLossTriggerPx: triggerPx}, "", nil
	}
	_, alert := applyTrailingTPRatchetToPosition(sc, pos, "ETH", 115, nil)
	var mu sync.RWMutex
	_, _, ev := runTrailingStopUpdateAfterRatchetTighten(sc, st, "ETH", 115, nil, map[string]float64{"ETH": 100}, map[string]string{"ETH": "long"}, &mu, nil, silentStrategyLogger(sc.ID))
	sender := ratchetFinish(t, sc, st, alert, ev, true)
	if !approxEq(call.trigger, 100.5) {
		t.Fatalf("stub trigger=%v want 100.5", call.trigger)
	}
	if len(sender.messages) != 1 || !strings.Contains(sender.messages[0], "SL trigger: $100.5000 (moved; was $80.0000)") {
		t.Fatalf("message: %v", sender.messages)
	}
}

func TestRatchetAlertBook_ErrorLeavesOldStop(t *testing.T) {
	sc := ratchetBookLive(ratchetAlertSC(3.0, tier(1.0, 0, 2.0)))
	pos := ratchetBookPos(sc, "long", 1, 115, 80, 410)
	st := ratchetBookState(sc, pos)
	var call ratchetStubCall
	ratchetInstallStub(t, nil, errors.New("venue down"), &call)
	_, alert := applyTrailingTPRatchetToPosition(sc, pos, "ETH", 115, nil)
	var mu sync.RWMutex
	_, _, ev := runTrailingStopUpdateAfterRatchetTighten(sc, st, "ETH", 115, nil, nil, nil, &mu, nil, silentStrategyLogger(sc.ID))
	sender := ratchetFinish(t, sc, st, alert, ev, true)
	if !call.called || call.cancelOID != 410 {
		t.Fatalf("stub call=%+v want cancel 410", call)
	}
	if ev.Confirmed || ev.Result != nil {
		t.Fatalf("confirmed=%v result=%v, want an unconfirmed nil result", ev.Confirmed, ev.Result)
	}
	if !approxEq(pos.StopLossTriggerPx, 80) || pos.StopLossOID != 410 {
		t.Fatalf("book trigger=%v oid=%d, want 80 and 410", pos.StopLossTriggerPx, pos.StopLossOID)
	}
	if len(sender.messages) != 1 || !strings.Contains(sender.messages[0], "SL trigger: $80.0000 (replacement not confirmed; previous stop retained)") || strings.Contains(sender.messages[0], "(unchanged)") {
		t.Fatalf("message: %v", sender.messages)
	}
}

func TestRatchetAlertBook_ExternalFillNotRetained(t *testing.T) {
	sc := ratchetBookLive(ratchetAlertSC(3.0, tier(1.0, 0, 2.0)))
	pos := ratchetBookPos(sc, "long", 1, 115, 80, 410)
	st := ratchetBookState(sc, pos)
	var call ratchetStubCall
	ratchetInstallStub(t, &HyperliquidStopLossUpdateResult{StopLossFilledExternally: true}, nil, &call)
	_, alert := applyTrailingTPRatchetToPosition(sc, pos, "ETH", 115, nil)
	var mu sync.RWMutex
	_, _, ev := runTrailingStopUpdateAfterRatchetTighten(sc, st, "ETH", 115, nil, nil, nil, &mu, nil, silentStrategyLogger(sc.ID))
	sender := ratchetFinish(t, sc, st, alert, ev, true)
	if !call.called || call.cancelOID != 410 {
		t.Fatalf("stub call=%+v want cancel 410", call)
	}
	if ev.Confirmed || ev.Result == nil || !ev.Result.StopLossFilledExternally {
		t.Fatalf("confirmed=%v result=%v, want an unconfirmed external fill", ev.Confirmed, ev.Result)
	}
	if !approxEq(pos.StopLossTriggerPx, 80) || pos.StopLossOID != 410 || pos.Quantity != 1 {
		t.Fatalf("book trigger=%v oid=%d qty=%v, want 80, 410, and 1", pos.StopLossTriggerPx, pos.StopLossOID, pos.Quantity)
	}
	if len(sender.messages) != 1 {
		t.Fatalf("sends=%d", len(sender.messages))
	}
	msg := sender.messages[0]
	if !strings.Contains(msg, "SL trigger: $80.0000 (previous stop filled on venue; close pending reconcile)") || strings.Contains(msg, "previous stop retained") || strings.Contains(msg, "(unchanged)") {
		t.Fatalf("message:\n%s", msg)
	}
}

func TestRatchetAlertBook_FullAndPartialFill(t *testing.T) {
	sc := ratchetBookLive(ratchetAlertSC(3.0, tier(1.0, 0, 2.0)))
	pos := ratchetBookPos(sc, "long", 1, 115, 80, 410)
	st := ratchetBookState(sc, pos)
	var call ratchetStubCall
	ratchetInstallStub(t, &HyperliquidStopLossUpdateResult{StopLossFilledImmediately: true, StopLossTriggerPx: 92}, nil, &call)
	_, alert := applyTrailingTPRatchetToPosition(sc, pos, "ETH", 115, nil)
	var mu sync.RWMutex
	_, _, ev := runTrailingStopUpdateAfterRatchetTighten(sc, st, "ETH", 115, nil, nil, nil, &mu, nil, silentStrategyLogger(sc.ID))
	sender := ratchetFinish(t, sc, st, alert, ev, true)
	if len(sender.messages) != 1 || !strings.Contains(sender.messages[0], "SL trigger: position closed in this cycle") {
		t.Fatalf("full message: %v", sender.messages)
	}
	if strings.Contains(sender.messages[0], "remainder open") {
		t.Fatalf("full close reported a remainder: %s", sender.messages[0])
	}

	pos = ratchetBookPos(sc, "long", 1, 115, 80, 410)
	st = ratchetBookState(sc, pos)
	ratchetInstallStub(t, &HyperliquidStopLossUpdateResult{StopLossFilledImmediately: true, StopLossTriggerPx: 90, StopLossSize: 0.4}, nil, &call)
	_, alert = applyTrailingTPRatchetToPosition(sc, pos, "ETH", 115, nil)
	_, _, ev = runTrailingStopUpdateAfterRatchetTighten(sc, st, "ETH", 115, nil, nil, nil, &mu, nil, silentStrategyLogger(sc.ID))
	sender = ratchetFinish(t, sc, st, alert, ev, true)
	if len(sender.messages) != 1 {
		t.Fatalf("partial sends=%d", len(sender.messages))
	}
	msg := sender.messages[0]
	if !strings.Contains(msg, "remainder open, qty 0.600000; recorded protection cleared") || strings.Contains(msg, "position closed in this cycle") {
		t.Fatalf("partial message:\n%s", msg)
	}
}

func TestRatchetAlertBook_UnknownCancelAndSamePrice(t *testing.T) {
	sc := ratchetBookLive(ratchetAlertSC(3.0, tier(1.0, 0, 2.0)))
	logger := silentStrategyLogger(sc.ID)
	var mu sync.RWMutex

	pos := ratchetBookPos(sc, "long", 1, 115, 80, 771)
	st := ratchetBookState(sc, pos)
	var call ratchetStubCall
	ratchetInstallStub(t, &HyperliquidStopLossUpdateResult{StopLossOutcomeUnknown: true, StopLossTriggerPx: 92}, nil, &call)
	_, alert := applyTrailingTPRatchetToPosition(sc, pos, "ETH", 115, nil)
	_, _, ev := runTrailingStopUpdateAfterRatchetTighten(sc, st, "ETH", 115, nil, nil, nil, &mu, nil, logger)
	sender := ratchetFinish(t, sc, st, alert, ev, true)
	if len(sender.messages) != 1 || !strings.Contains(sender.messages[0], "SL trigger: $92.0000 (requested, outcome unknown)") || strings.Contains(sender.messages[0], "(moved") {
		t.Fatalf("unknown message: %v", sender.messages)
	}

	pos = ratchetBookPos(sc, "long", 1, 115, 80, 772)
	st = ratchetBookState(sc, pos)
	t.Cleanup(func() { hlUnreadableStopPlace.Delete(hlStopPlaceUnreadKey("ETH", 772)) })
	ratchetInstallStub(t, &HyperliquidStopLossUpdateResult{StopLossOutcomeUnknown: true, StopLossOldStillOpen: true, StopLossTriggerPx: 80}, nil, &call)
	_, alert = applyTrailingTPRatchetToPosition(sc, pos, "ETH", 115, nil)
	_, _, ev = runTrailingStopUpdateAfterRatchetTighten(sc, st, "ETH", 115, nil, nil, nil, &mu, nil, logger)
	sender = ratchetFinish(t, sc, st, alert, ev, true)
	msg := ""
	if len(sender.messages) == 1 {
		msg = sender.messages[0]
	}
	if !strings.Contains(msg, "SL trigger: $80.0000 (outcome unknown; previous stop retained)") || strings.Contains(msg, "(unchanged)") {
		t.Fatalf("unreadable message: %v", sender.messages)
	}
	hlUnreadableStopPlace.Delete(hlStopPlaceUnreadKey("ETH", 772))

	pos = ratchetBookPos(sc, "long", 1, 115, 80, 773)
	st = ratchetBookState(sc, pos)
	ratchetInstallStub(t, &HyperliquidStopLossUpdateResult{CancelStopLossSucceeded: true}, nil, &call)
	_, alert = applyTrailingTPRatchetToPosition(sc, pos, "ETH", 115, nil)
	_, _, ev = runTrailingStopUpdateAfterRatchetTighten(sc, st, "ETH", 115, nil, nil, nil, &mu, nil, logger)
	sender = ratchetFinish(t, sc, st, alert, ev, true)
	if len(sender.messages) != 1 || !strings.Contains(sender.messages[0], "SL trigger: none (cancelled; no replacement resting)") || strings.Contains(sender.messages[0], "(unchanged)") {
		t.Fatalf("cancel message: %v", sender.messages)
	}

	pos = ratchetBookPos(sc, "long", 1, 115, 80, 774)
	st = ratchetBookState(sc, pos)
	ratchetInstallStub(t, &HyperliquidStopLossUpdateResult{StopLossOID: 88, StopLossTriggerPx: 80}, nil, &call)
	_, alert = applyTrailingTPRatchetToPosition(sc, pos, "ETH", 115, nil)
	_, _, ev = runTrailingStopUpdateAfterRatchetTighten(sc, st, "ETH", 115, nil, nil, nil, &mu, nil, logger)
	sender = ratchetFinish(t, sc, st, alert, ev, true)
	if len(sender.messages) != 1 || !strings.Contains(sender.messages[0], "SL trigger: $80.0000 (replaced at the same price)") || strings.Contains(sender.messages[0], "(unchanged)") {
		t.Fatalf("same-price message: %v", sender.messages)
	}
}

func TestRatchetAlertBook_ManualPathUsesBookedTrigger(t *testing.T) {
	sc := ratchetBookLive(ratchetAlertSC(3.0, tier(1.0, 0, 2.0)))
	sc.Type = "manual"
	pos := ratchetBookPos(sc, "long", 1, 115, 80, 410)
	st := ratchetBookState(sc, pos)
	var call ratchetStubCall
	ratchetInstallStub(t, nil, nil, &call)
	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		call.called = true
		call.size = size
		call.trigger = triggerPx
		call.cancelOID = cancelStopLossOID
		return &HyperliquidStopLossUpdateResult{StopLossOID: 902, StopLossTriggerPx: triggerPx}, "", nil
	}
	_, alert := applyTrailingTPRatchetToPosition(sc, pos, "ETH", 115, nil)
	var mu sync.RWMutex
	_, _, ev := runManualTrailingStopUpdate(sc, st, map[string]*StrategyState{sc.ID: st}, []StrategyConfig{sc}, nil, nil, nil, 115, true, &mu, nil, silentStrategyLogger(sc.ID))
	sender := ratchetFinish(t, sc, st, alert, ev, true)
	if !call.called || !approxEq(call.trigger, 92) || call.cancelOID != 410 {
		t.Fatalf("manual stub=%+v", call)
	}
	if len(sender.messages) != 1 || !strings.Contains(sender.messages[0], "SL trigger: $92.0000 (moved; was $80.0000)") {
		t.Fatalf("manual message: %v", sender.messages)
	}
}

func TestRatchetAlertBook_DisabledSkippedAndOneEvent(t *testing.T) {
	sc := ratchetAlertSC(3.0, tier(1.0, 0, 2.0))
	pos := ratchetBookPos(sc, "long", 1, 115, 80, 0)
	st := ratchetBookState(sc, pos)
	_, alert := applyTrailingTPRatchetToPosition(sc, pos, "ETH", 115, nil)
	var mu sync.RWMutex
	_, _, ev := runTrailingStopUpdateAfterRatchetTighten(sc, st, "ETH", 115, nil, nil, nil, &mu, nil, silentStrategyLogger(sc.ID))
	sender := ratchetFinish(t, sc, st, alert, ev, false)
	if len(sender.messages) != 0 {
		t.Fatalf("disabled sent %d", len(sender.messages))
	}

	already := 2.0
	skipPos := ratchetBookPos(sc, "long", 1, 115, 80, 0)
	skipPos.PostTPTrailingATRMult = &already
	tightened, skipped := applyTrailingTPRatchetToPosition(sc, skipPos, "ETH", 115, nil)
	if tightened || skipped != nil {
		t.Fatalf("watermark without a tighten produced tightened=%v alert=%v", tightened, skipped)
	}
	if skipPos.SLAdjustedTiersProcessed != 1 {
		t.Fatalf("watermark=%d want 1", skipPos.SLAdjustedTiersProcessed)
	}

	sc = ratchetAlertSC(3.0, tier(1.0, 0, 2.0), tier(2.0, 0, 1.0))
	pos = ratchetBookPos(sc, "long", 1, 130, 100, 0)
	st = ratchetBookState(sc, pos)
	tightened, alert = applyTrailingTPRatchetToPosition(sc, pos, "ETH", 130, nil)
	if !tightened || alert == nil || alert.TierIdx != 1 || alert.NewTrailMult != 1 {
		t.Fatalf("one event alert=%+v tightened=%v", alert, tightened)
	}
	_, _, ev = runTrailingStopUpdateAfterRatchetTighten(sc, st, "ETH", 130, nil, nil, nil, &mu, nil, silentStrategyLogger(sc.ID))
	sender = ratchetFinish(t, sc, st, alert, ev, true)
	if len(sender.messages) != 1 || !strings.Contains(sender.messages[0], "Ratchet Tier 2/2 cleared") || !strings.Contains(sender.messages[0], "SL trigger: $117.0000 (moved; was $100.0000)") {
		t.Fatalf("one-event message: %v", sender.messages)
	}
}

func TestRatchetAlertBook_DoesNotAttachLaterPosition(t *testing.T) {
	sc := ratchetAlertSC(3.0, tier(1.0, 0, 2.0))
	pos := ratchetBookPos(sc, "long", 1, 115, 80, 0)
	st := ratchetBookState(sc, pos)
	_, alert := applyTrailingTPRatchetToPosition(sc, pos, "ETH", 115, nil)
	var mu sync.RWMutex
	_, _, ev := runTrailingStopUpdateAfterRatchetTighten(sc, st, "ETH", 115, nil, nil, nil, &mu, nil, silentStrategyLogger(sc.ID))
	st.Positions["ETH"] = &Position{
		Symbol: "ETH", Side: "short", Quantity: 3, AvgCost: 100, EntryATR: 10,
		TradePositionID: "later", OwnerStrategyID: "other",
		StopLossTriggerPx: 77.7777, StopLossOID: 12345, StopLossHighWaterPx: 50,
	}
	sender := ratchetFinish(t, sc, st, alert, ev, true)
	if alert.Outcome != ratchetOutcomeClosed || alert.StopTriggerPx != 0 {
		t.Fatalf("attached later protection outcome=%q trigger=%g", alert.Outcome, alert.StopTriggerPx)
	}
	if len(sender.messages) != 1 || !strings.Contains(sender.messages[0], "position closed in this cycle") || strings.Contains(sender.messages[0], "77.7777") {
		t.Fatalf("message: %v", sender.messages)
	}
}
