package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// #1431 — decision-log DB, write-side gating, failure alerting, config
// validation, and hot-reload rules. Tests here swap package-level hooks
// (decisionLogWriter, decisionLogPersistWarn, replayLiveSources) so they must
// NOT use t.Parallel() (same caveat as tradeRecorder in state.go).

func replayTestConfig(ids ...string) *Config {
	cfg := &Config{ReplayLogPath: "/tmp/replay.db"}
	for _, id := range ids {
		cfg.Strategies = append(cfg.Strategies, StrategyConfig{
			ID:            id,
			Type:          "perps",
			Platform:      "hyperliquid",
			Script:        "shared_scripts/check_hyperliquid.py",
			Args:          []string{"--mode", "live"},
			ReplaySharing: ReplaySharingLiveMirror,
		})
	}
	return cfg
}

// swapReplayHooks installs capture hooks and returns the captured decisions;
// cleanup restores the nil/disabled defaults.
func swapReplayHooks(t *testing.T, sources *Config) *[]ReplayDecision {
	t.Helper()
	captured := &[]ReplayDecision{}
	prevWriter := decisionLogWriter
	prevWarn := decisionLogPersistWarn
	decisionLogWriter = func(dec ReplayDecision) error {
		*captured = append(*captured, dec)
		return nil
	}
	rebuildReplayLiveSources(sources)
	decisionLogInsertFailures.Lock()
	decisionLogInsertFailures.count = 0
	decisionLogInsertFailures.alertedStreak = false
	decisionLogInsertFailures.Unlock()
	t.Cleanup(func() {
		decisionLogWriter = prevWriter
		decisionLogPersistWarn = prevWarn
		rebuildReplayLiveSources(nil)
		decisionLogInsertFailures.Lock()
		decisionLogInsertFailures.count = 0
		decisionLogInsertFailures.alertedStreak = false
		decisionLogInsertFailures.Unlock()
	})
	return captured
}

func replayTestStrategyState(id string) *StrategyState {
	return NewStrategyState(StrategyConfig{ID: id, Type: "perps", Platform: "hyperliquid", Capital: 10000})
}

func TestDecisionLogDBRoundTrip(t *testing.T) {
	db, err := OpenDecisionLogDB(filepath.Join(t.TempDir(), "replay.db"))
	if err != nil {
		t.Fatalf("OpenDecisionLogDB: %v", err)
	}
	defer db.Close()

	now := time.Date(2026, 8, 10, 12, 53, 0, 0, time.UTC)
	rows := []ReplayDecision{
		{StrategyID: "s1", DecisionType: ReplayDecisionOpen, DecidedAt: now, Symbol: "ETH", Side: "long", Quantity: 0.5, ReferencePrice: 1908.25},
		{StrategyID: "s1", DecisionType: ReplayDecisionScaleIn, DecidedAt: now.Add(time.Minute), Symbol: "ETH", Side: "long", Quantity: 0.25, ReferencePrice: 1901.0},
		{StrategyID: "s1", DecisionType: ReplayDecisionPartialClose, DecidedAt: now.Add(2 * time.Minute), Symbol: "ETH", Side: "long", Quantity: 0.3, ReferencePrice: 1912.0, CloseReason: "tiered_tp"},
		{StrategyID: "s2", DecisionType: ReplayDecisionOpen, DecidedAt: now.Add(3 * time.Minute), Symbol: "BTC", Side: "short", Quantity: 0.01, ReferencePrice: 65000},
		{StrategyID: "s1", DecisionType: ReplayDecisionFullClose, DecidedAt: now.Add(4 * time.Minute), Symbol: "ETH", Side: "long", Quantity: 0.45, ReferencePrice: 1900.5, CloseReason: "hl_sync_stop_loss"},
	}
	for _, r := range rows {
		if err := db.InsertDecision(r); err != nil {
			t.Fatalf("InsertDecision: %v", err)
		}
	}

	pending, err := db.PendingDecisions("s1")
	if err != nil {
		t.Fatalf("PendingDecisions: %v", err)
	}
	if len(pending) != 4 {
		t.Fatalf("pending for s1 = %d, want 4", len(pending))
	}
	// Insert order preserved (decision_id ASC), values round-trip.
	wantTypes := []string{ReplayDecisionOpen, ReplayDecisionScaleIn, ReplayDecisionPartialClose, ReplayDecisionFullClose}
	for i, wt := range wantTypes {
		if pending[i].DecisionType != wt {
			t.Errorf("pending[%d].DecisionType = %q, want %q", i, pending[i].DecisionType, wt)
		}
		if pending[i].ReplayStatus != replayStatusPending {
			t.Errorf("pending[%d].ReplayStatus = %q, want pending", i, pending[i].ReplayStatus)
		}
	}
	if !pending[0].DecidedAt.Equal(now) {
		t.Errorf("decided_at round-trip = %v, want %v", pending[0].DecidedAt, now)
	}
	if pending[0].Quantity != 0.5 || pending[0].ReferencePrice != 1908.25 || pending[0].Side != "long" {
		t.Errorf("open row mismatch: %+v", pending[0])
	}
	if pending[3].CloseReason != "hl_sync_stop_loss" {
		t.Errorf("close_reason = %q, want hl_sync_stop_loss", pending[3].CloseReason)
	}

	// Mark the first two applied; only the rest stay pending.
	if err := db.MarkDecisionsApplied([]int64{pending[0].DecisionID, pending[1].DecisionID}); err != nil {
		t.Fatalf("MarkDecisionsApplied: %v", err)
	}
	pending, err = db.PendingDecisions("s1")
	if err != nil {
		t.Fatalf("PendingDecisions after mark: %v", err)
	}
	if len(pending) != 2 || pending[0].DecisionType != ReplayDecisionPartialClose || pending[1].DecisionType != ReplayDecisionFullClose {
		t.Fatalf("pending after mark = %+v, want partial_close+full_close", pending)
	}

	// Idempotent re-mark is a no-op.
	if err := db.MarkDecisionsApplied(nil); err != nil {
		t.Fatalf("MarkDecisionsApplied(nil): %v", err)
	}
}

func TestRecordReplayDecisionGating(t *testing.T) {
	captured := swapReplayHooks(t, replayTestConfig("hl-live-eth"))
	live := replayTestStrategyState("hl-live-eth")
	other := replayTestStrategyState("hl-other-eth")
	now := time.Now().UTC()

	recordReplayDecision(other, ReplayDecisionFullClose, "ETH", "long", 1, 100, "signal", now)
	if len(*captured) != 0 {
		t.Fatalf("non-source strategy wrote %d rows, want 0", len(*captured))
	}
	recordReplayDecision(live, ReplayDecisionFullClose, "ETH", "long", 1.5, 100.5, "signal", now)
	if len(*captured) != 1 {
		t.Fatalf("source strategy wrote %d rows, want 1", len(*captured))
	}
	dec := (*captured)[0]
	if dec.StrategyID != "hl-live-eth" || dec.DecisionType != ReplayDecisionFullClose || dec.Quantity != 1.5 || dec.ReferencePrice != 100.5 || dec.CloseReason != "signal" || !dec.DecidedAt.Equal(now) {
		t.Errorf("row mismatch: %+v", dec)
	}

	// Nil writer disables logging entirely (replay_log_path unset / tests).
	decisionLogWriter = nil
	recordReplayDecision(live, ReplayDecisionFullClose, "ETH", "long", 1, 100, "signal", now)
	if len(*captured) != 1 {
		t.Fatalf("nil writer still wrote; captured=%d", len(*captured))
	}
}

func TestRecordReplayDecisionFailureAlerting(t *testing.T) {
	captured := swapReplayHooks(t, replayTestConfig("hl-live-eth"))
	_ = captured
	var warns []string
	decisionLogPersistWarn = func(msg string) { warns = append(warns, msg) }
	fail := true
	decisionLogWriter = func(dec ReplayDecision) error {
		if fail {
			return fmt.Errorf("disk full")
		}
		return nil
	}
	s := replayTestStrategyState("hl-live-eth")
	now := time.Now().UTC()

	// Two consecutive failures: no DM yet (threshold is 3).
	for i := 0; i < decisionLogAlertThreshold-1; i++ {
		recordReplayDecision(s, ReplayDecisionOpen, "ETH", "long", 1, 100, "", now)
	}
	if len(warns) != 0 {
		t.Fatalf("warns after %d failures = %d, want 0", decisionLogAlertThreshold-1, len(warns))
	}
	// Third consecutive failure fires exactly one DM for the streak.
	recordReplayDecision(s, ReplayDecisionOpen, "ETH", "long", 1, 100, "", now)
	recordReplayDecision(s, ReplayDecisionOpen, "ETH", "long", 1, 100, "", now)
	if len(warns) != 1 {
		t.Fatalf("warns = %d, want 1 (one per streak)", len(warns))
	}
	if !strings.Contains(warns[0], "consecutive failures") {
		t.Errorf("warn message missing streak note: %q", warns[0])
	}
	// A success resets the streak; the next streak re-alerts.
	fail = false
	recordReplayDecision(s, ReplayDecisionOpen, "ETH", "long", 1, 100, "", now)
	fail = true
	for i := 0; i < decisionLogAlertThreshold; i++ {
		recordReplayDecision(s, ReplayDecisionOpen, "ETH", "long", 1, 100, "", now)
	}
	if len(warns) != 2 {
		t.Fatalf("warns after second streak = %d, want 2", len(warns))
	}
}

func TestRecordClosedPositionWritesFullCloseDecision(t *testing.T) {
	captured := swapReplayHooks(t, replayTestConfig("hl-live-eth"))
	s := replayTestStrategyState("hl-live-eth")
	pos := &Position{Symbol: "ETH", Quantity: 0.5, AvgCost: 1900, Side: "long", Multiplier: 1}
	s.Positions["ETH"] = pos
	now := time.Now().UTC()

	recordClosedPosition(s, pos, 1880.5, -9.75, "hl_sync_stop_loss", now)
	if len(*captured) != 1 {
		t.Fatalf("captured = %d, want 1", len(*captured))
	}
	dec := (*captured)[0]
	if dec.DecisionType != ReplayDecisionFullClose || dec.Quantity != 0.5 || dec.ReferencePrice != 1880.5 || dec.CloseReason != "hl_sync_stop_loss" || dec.Side != "long" {
		t.Errorf("full_close row mismatch: %+v", dec)
	}

	// Hedge legs never write: paper's state-derived reconciler mirrors them.
	hedge := &Position{Symbol: "BTC", Quantity: 0.01, AvgCost: 65000, Side: "short", Multiplier: 1, HedgeFor: "hl-live-eth"}
	recordClosedPosition(s, hedge, 65100, -1, "signal", now)
	if len(*captured) != 1 {
		t.Fatalf("hedge leg close wrote a row; captured=%d", len(*captured))
	}

	// Zero-qty residuals (phantom cleanups) carry no mirrorable exposure.
	dust := &Position{Symbol: "SOL", Quantity: 0, AvgCost: 100, Side: "long", Multiplier: 1}
	recordClosedPosition(s, dust, 0, 0, "hl_sync_external", now)
	if len(*captured) != 1 {
		t.Fatalf("zero-qty residual wrote a row; captured=%d", len(*captured))
	}
}

func TestRecordPositionOpenWritesOpenAndScaleIn(t *testing.T) {
	captured := swapReplayHooks(t, replayTestConfig("hl-live-eth"))
	s := replayTestStrategyState("hl-live-eth")
	sc := StrategyConfig{ID: "hl-live-eth", Type: "perps", Platform: "hyperliquid"}
	now := time.Now().UTC()

	// Fresh open: position qty == trade qty.
	pos := &Position{Symbol: "ETH", Quantity: 0.5, AvgCost: 1900, Side: "long", Multiplier: 1}
	s.Positions["ETH"] = pos
	openTrade := &Trade{Timestamp: now, StrategyID: s.ID, Symbol: "ETH", Side: "buy", Quantity: 0.5, Price: 1900.25}
	if !recordPositionOpen(s, sc, openTrade, pos) {
		t.Fatal("recordPositionOpen returned false")
	}
	// Scale-in add: position qty already grown past the trade qty.
	pos.Quantity = 0.75
	addTrade := &Trade{Timestamp: now.Add(time.Minute), StrategyID: s.ID, Symbol: "ETH", Side: "buy", Quantity: 0.25, Price: 1895.0}
	recordPositionOpen(s, sc, addTrade, pos)

	if len(*captured) != 2 {
		t.Fatalf("captured = %d, want 2", len(*captured))
	}
	if (*captured)[0].DecisionType != ReplayDecisionOpen || (*captured)[0].Quantity != 0.5 || (*captured)[0].ReferencePrice != 1900.25 {
		t.Errorf("open row mismatch: %+v", (*captured)[0])
	}
	if (*captured)[1].DecisionType != ReplayDecisionScaleIn || (*captured)[1].Quantity != 0.25 || (*captured)[1].ReferencePrice != 1895.0 {
		t.Errorf("scale_in row mismatch: %+v", (*captured)[1])
	}

	// Hedge leg opens never write.
	hedgePos := &Position{Symbol: "BTC", Quantity: 0.01, AvgCost: 65000, Side: "short", Multiplier: 1, HedgeFor: "hl-live-eth"}
	hedgeTrade := &Trade{Timestamp: now, StrategyID: s.ID, Symbol: "BTC", Side: "sell", Quantity: 0.01, Price: 65000}
	recordPositionOpen(s, sc, hedgeTrade, hedgePos)
	if len(*captured) != 2 {
		t.Fatalf("hedge open wrote a row; captured=%d", len(*captured))
	}
}

func TestBookPerpsPartialCloseWritesDecision(t *testing.T) {
	captured := swapReplayHooks(t, replayTestConfig("hl-live-eth"))
	s := replayTestStrategyState("hl-live-eth")
	logger := silentStrategyLogger("hl-live-eth")

	// True partial: remaining > 0 → partial_close row.
	s.Positions["ETH"] = &Position{Symbol: "ETH", Quantity: 1.0, InitialQuantity: 1.0, AvgCost: 1900, Side: "long", Multiplier: 1}
	if !bookPerpsPartialCloseWithFillFee(s, "ETH", 0.4, 1910, 0, false, "", "tiered_tp", "TP", "TP", logger) {
		t.Fatal("partial close returned false")
	}
	if len(*captured) != 1 || (*captured)[0].DecisionType != ReplayDecisionPartialClose {
		t.Fatalf("captured = %+v, want one partial_close", *captured)
	}
	if (*captured)[0].Quantity != 0.4 || (*captured)[0].ReferencePrice != 1910 {
		t.Errorf("partial_close row mismatch: %+v", (*captured)[0])
	}

	// Partial that zeroes the position → full_close row via recordClosedPosition.
	if !bookPerpsPartialCloseWithFillFee(s, "ETH", 0.6, 1905, 0, false, "", "tiered_tp", "TP", "TP", logger) {
		t.Fatal("final partial close returned false")
	}
	if len(*captured) != 2 || (*captured)[1].DecisionType != ReplayDecisionFullClose {
		t.Fatalf("captured = %+v, want partial_close then full_close", *captured)
	}
	if (*captured)[1].Quantity != 0.6 {
		t.Errorf("full_close qty = %.6f, want 0.6 (pre-close remainder)", (*captured)[1].Quantity)
	}
}

func TestReplaySharingValidation(t *testing.T) {
	base := func() *Config {
		return &Config{
			ReplayLogPath: "/var/lib/go-trader/shared/replay.db",
			Strategies: []StrategyConfig{{
				ID: "hl-vwap-eth-60", Type: "perps", Platform: "hyperliquid",
				Script: "shared_scripts/check_hyperliquid.py",
				Args:   []string{"vwap", "ETH", "1h", "--mode", "paper"},
			}},
		}
	}

	// Valid: live_mirror on HL perps with replay_log_path set.
	cfg := base()
	cfg.Strategies[0].ReplaySharing = "live_mirror"
	if err := validateConfig(cfg, true); err != nil && strings.Contains(err.Error(), "replay_sharing") {
		t.Fatalf("valid live_mirror rejected: %v", err)
	}

	// Invalid value fails loudly.
	cfg = base()
	cfg.Strategies[0].ReplaySharing = "mirror"
	if err := validateConfig(cfg, true); err == nil || !strings.Contains(err.Error(), "replay_sharing must be") {
		t.Fatalf("invalid value not rejected: %v", err)
	}

	// Non-HL-perps scope rejected.
	cfg = base()
	cfg.Strategies[0].ReplaySharing = "live_mirror"
	cfg.Strategies[0].Type = "spot"
	if err := validateConfig(cfg, true); err == nil || !strings.Contains(err.Error(), "HL perps strategies only") {
		t.Fatalf("spot scope not rejected: %v", err)
	}

	// Missing replay_log_path rejected.
	cfg = base()
	cfg.Strategies[0].ReplaySharing = "live_mirror"
	cfg.ReplayLogPath = ""
	if err := validateConfig(cfg, true); err == nil || !strings.Contains(err.Error(), "requires the root replay_log_path") {
		t.Fatalf("missing replay_log_path not rejected: %v", err)
	}

	// Default (unset / "none") needs no replay_log_path.
	cfg = base()
	cfg.ReplayLogPath = ""
	if err := validateConfig(cfg, true); err != nil && strings.Contains(err.Error(), "replay") {
		t.Fatalf("default replay_sharing rejected: %v", err)
	}
}

func TestReplayLogPathHotReloadRestartRequired(t *testing.T) {
	cfg := &Config{ReplayLogPath: "/a/replay.db"}
	next := &Config{ReplayLogPath: "/b/replay.db"}
	err := validateHotReloadCompatible(cfg, next)
	if err == nil || !strings.Contains(err.Error(), "replay_log_path changed") {
		t.Fatalf("replay_log_path change not flagged restart-required: %v", err)
	}
	// Unchanged path passes this arm.
	next.ReplayLogPath = cfg.ReplayLogPath
	if err := validateHotReloadCompatible(cfg, next); err != nil && strings.Contains(err.Error(), "replay_log_path") {
		t.Fatalf("unchanged replay_log_path rejected: %v", err)
	}
}

func TestReplaySharingHotReloadFlatOnly(t *testing.T) {
	mk := func(sharing string) *Config {
		return &Config{Strategies: []StrategyConfig{{ID: "s1", Type: "perps", Platform: "hyperliquid", ReplaySharing: sharing}}}
	}
	state := NewAppState()
	ss := replayTestStrategyState("s1")
	ss.Positions["ETH"] = &Position{Symbol: "ETH", Quantity: 0.5, AvgCost: 1900, Side: "long", Multiplier: 1}
	state.Strategies["s1"] = ss

	// Toggle while open: refused.
	err := validateHotReloadStateCompatible(mk("none"), mk("live_mirror"), state)
	if err == nil || !strings.Contains(err.Error(), "replay_sharing changed with open positions") {
		t.Fatalf("toggle while open not refused: %v", err)
	}
	// Toggle while flat: allowed.
	ss.Positions = map[string]*Position{}
	if err := validateHotReloadStateCompatible(mk("none"), mk("live_mirror"), state); err != nil {
		t.Fatalf("toggle while flat refused: %v", err)
	}
	// Unset and "none" are the same value — no spurious block.
	ss.Positions["ETH"] = &Position{Symbol: "ETH", Quantity: 0.5, AvgCost: 1900, Side: "long", Multiplier: 1}
	if err := validateHotReloadStateCompatible(mk(""), mk("none"), state); err != nil {
		t.Fatalf("unset->none alias refused: %v", err)
	}
}

func TestReplaySharingMaskedFromRestartShape(t *testing.T) {
	// A pure replay_sharing toggle must not trip the restart-required
	// immutable-fields DeepEqual — the flat-only state-compat gate owns it.
	cfg := &Config{Strategies: []StrategyConfig{{ID: "s1", Type: "perps", Platform: "hyperliquid", ReplaySharing: "none"}}}
	next := &Config{Strategies: []StrategyConfig{{ID: "s1", Type: "perps", Platform: "hyperliquid", ReplaySharing: "live_mirror"}}}
	if err := validateHotReloadCompatible(cfg, next); err != nil {
		t.Fatalf("pure replay_sharing toggle flagged: %v", err)
	}
}

func TestReplaySharingUnknownKeyGuardAcceptsField(t *testing.T) {
	// The strategies-array unknown-key guard is reflective; replay_sharing is
	// a declared StrategyConfig field and must NOT be flagged.
	raw := []byte(`{"strategies":[{"id":"s1","type":"perps","platform":"hyperliquid","replay_sharing":"live_mirror"}]}`)
	for _, e := range validateStrategyJSONKeys(raw) {
		if strings.Contains(e, "replay_sharing") {
			t.Fatalf("replay_sharing flagged as unknown: %s", e)
		}
	}
}

func TestStopLossCloseDetailsPrefixReplayMirror(t *testing.T) {
	if got := stopLossCloseDetailsPrefix("replay_live_mirror"); got != "Live mirror replay close" {
		t.Errorf("stopLossCloseDetailsPrefix(replay_live_mirror) = %q, want %q", got, "Live mirror replay close")
	}
}

func TestReplaySourceAndMirrorPredicates(t *testing.T) {
	liveHL := StrategyConfig{ID: "a", Type: "perps", Platform: "hyperliquid", Args: []string{"--mode", "live"}, ReplaySharing: "live_mirror"}
	paperHL := StrategyConfig{ID: "b", Type: "perps", Platform: "hyperliquid", Args: []string{"--mode", "paper"}, ReplaySharing: "live_mirror"}
	off := StrategyConfig{ID: "c", Type: "perps", Platform: "hyperliquid", Args: []string{"--mode", "live"}}
	if !replaySharingSourceEnabled(liveHL) {
		t.Error("live HL live_mirror should be a write source")
	}
	if replaySharingSourceEnabled(paperHL) {
		t.Error("paper must never be a write source")
	}
	if replaySharingSourceEnabled(off) {
		t.Error("flag-less strategy must not be a write source")
	}
	if !replayMirrorPaperActive(paperHL) {
		t.Error("paper HL live_mirror should be an active mirror")
	}
	if replayMirrorPaperActive(liveHL) {
		t.Error("live deployment never mirrors")
	}
	if replayMirrorPaperActive(off) {
		t.Error("flag-less strategy never mirrors")
	}
}
