package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTransitionsTestDB(t *testing.T) *StateDB {
	t.Helper()
	db, err := OpenStateDB(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open state db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func transitionsTestKey() regimeBundleKey {
	return regimeBundleKey{Platform: "hyperliquid", Symbol: "BTC", Timeframe: "1h", SpecJSON: `{"w":1}`}
}

// resetRegimeTransitionGlobals isolates the main-loop-only package state.
func resetRegimeTransitionGlobals(t *testing.T) {
	t.Helper()
	prevPending := regimeReversalPendingState
	prevPrune := lastRegimeTransitionPrune
	prevSend := regimeAlertSendFn
	regimeReversalPendingState = map[regimeBundleKey]*regimeReversalPending{}
	lastRegimeTransitionPrune = time.Time{}
	t.Cleanup(func() {
		regimeReversalPendingState = prevPending
		lastRegimeTransitionPrune = prevPrune
		regimeAlertSendFn = prevSend
	})
}

// ─── Pure helpers ────────────────────────────────────────────────────────────

func TestRegimeTransitionConfirmed(t *testing.T) {
	cases := []struct {
		name     string
		current  string
		trailing []string
		debounce int
		want     bool
	}{
		{"no history debounce 1", "up", nil, 1, true},
		{"no history debounce 2", "up", nil, 2, false},
		{"one matching prior row meets debounce 2", "up", []string{"up"}, 2, true},
		{"prior row differs", "up", []string{"down"}, 2, false},
		{"run broken mid-way", "up", []string{"up", "down"}, 3, false},
		{"long run", "up", []string{"up", "up"}, 3, true},
		{"debounce 0 treated as 1", "up", nil, 0, true},
	}
	for _, tc := range cases {
		if got := regimeTransitionConfirmed(tc.current, tc.trailing, tc.debounce); got != tc.want {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}

func TestNetRegimeTransition_FlapBackSuppressed(t *testing.T) {
	pending := []RegimeWindowTransitionRow{
		{ID: 1, OldLabel: "trending_up", NewLabel: "ranging"},
		{ID: 2, OldLabel: "ranging", NewLabel: "trending_up"},
	}
	old, alert := netRegimeTransition(pending, "trending_up")
	if alert {
		t.Errorf("flap back to origin must not alert (old=%s)", old)
	}
	old, alert = netRegimeTransition(pending[:1], "ranging")
	if !alert || old != "trending_up" {
		t.Errorf("net change must alert with the chain origin, got old=%s alert=%v", old, alert)
	}
}

func TestClassifyRegimeReversal(t *testing.T) {
	periods := map[string]int{"1d": 24, "3d": 72, "7d": 168, "30d": 720}
	up := RegimeSnapshot{Regime: "trending_up"}
	down := RegimeSnapshot{Regime: "trending_down"}
	ranging := RegimeSnapshot{Regime: "ranging_quiet"}

	// The issue's headline scenario: 30d down, all shorter up.
	res, active := classifyRegimeReversal(map[string]RegimeSnapshot{
		"1d": up, "3d": up, "7d": up, "30d": down,
	}, periods, 0)
	if !active {
		t.Fatal("30d-down + all-shorter-up must be active")
	}
	if res.LongestWindow != "30d" || res.LongestLabel != "trending_down" {
		t.Errorf("longest = %s=%s, want 30d=trending_down", res.LongestWindow, res.LongestLabel)
	}
	if len(res.Opposing) != 3 {
		t.Errorf("opposing = %v, want all 3 shorter windows", res.Opposing)
	}

	// One shorter window neutral: all-oppose default fails, k=2 passes.
	snaps := map[string]RegimeSnapshot{"1d": up, "3d": up, "7d": ranging, "30d": down}
	if _, active := classifyRegimeReversal(snaps, periods, 0); active {
		t.Error("neutral shorter window must break the all-oppose default")
	}
	if _, active := classifyRegimeReversal(snaps, periods, 2); !active {
		t.Error("k=2 must accept 2 opposing shorter windows")
	}

	// Neutral longest window: never a reversal.
	if _, active := classifyRegimeReversal(map[string]RegimeSnapshot{
		"1d": up, "30d": ranging,
	}, periods, 0); active {
		t.Error("neutral longest window must not flag a reversal")
	}

	// Composite directional substates map by drift direction (#1124).
	res, active = classifyRegimeReversal(map[string]RegimeSnapshot{
		"1d":  {Regime: "ranging_directional_up"},
		"3d":  {Regime: "trending_up_clean"},
		"30d": {Regime: "trending_down_choppy"},
	}, map[string]int{"1d": 24, "3d": 72, "30d": 720}, 0)
	if !active || len(res.Opposing) != 2 {
		t.Errorf("composite substates: active=%v opposing=%v, want active with 2", active, res.Opposing)
	}

	// Single window: no pattern possible.
	if _, active := classifyRegimeReversal(map[string]RegimeSnapshot{"default": down},
		map[string]int{"default": 14}, 0); active {
		t.Error("single window must never flag a reversal")
	}
}

func TestRegimeReversalSignatureSorted(t *testing.T) {
	r := regimeReversalResult{
		LongestWindow: "30d", LongestLabel: "trending_down",
		Opposing: map[string]string{"7d": "trending_up", "1d": "trending_up", "3d": "trending_up"},
	}
	sig := regimeReversalSignatureString(r)
	want := "30d=trending_down|1d=trending_up,3d=trending_up,7d=trending_up"
	if sig != want {
		t.Errorf("signature = %q, want %q", sig, want)
	}
}

func TestFormatRegimeReversalDM_SortedWindows(t *testing.T) {
	msg := formatRegimeReversalDM(transitionsTestKey(), regimeReversalResult{
		LongestWindow: "30d", LongestLabel: "trending_down",
		Opposing: map[string]string{"7d": "trending_up", "1d": "trending_up"},
	})
	if !strings.Contains(msg, "1d=trending_up, 7d=trending_up") {
		t.Errorf("opposing windows must render sorted, got: %s", msg)
	}
	if !strings.Contains(msg, "hyperliquid/BTC/1h") || !strings.Contains(msg, "30d=trending_down") {
		t.Errorf("DM missing key or longest window: %s", msg)
	}
}

func TestValidateRegimeTransitionsConfig(t *testing.T) {
	cfg := &Config{Regime: &RegimeConfig{Transitions: &RegimeTransitionAlertsConfig{
		DebounceCycles: -1, RetentionDays: -2, ReversalMinOpposing: -3,
	}}}
	if errs := validateRegimeTransitionsConfig(cfg); len(errs) != 3 {
		t.Errorf("want 3 validation errors, got %v", errs)
	}
	cfg.Regime.Transitions = &RegimeTransitionAlertsConfig{Enabled: true}
	if errs := validateRegimeTransitionsConfig(cfg); len(errs) != 0 {
		t.Errorf("defaults must validate clean, got %v", errs)
	}
	if got := cfg.Regime.Transitions.debounceCycles(); got != regimeTransitionDefaultDebounceCycles {
		t.Errorf("default debounce = %d", got)
	}
	if got := cfg.Regime.Transitions.retentionDays(); got != regimeTransitionDefaultRetentionDays {
		t.Errorf("default retention = %d", got)
	}
}

// ─── DB layer ────────────────────────────────────────────────────────────────

func TestRegimeWindowHistoryRoundTrip(t *testing.T) {
	db := newTransitionsTestDB(t)
	key := transitionsTestKey()
	for i, label := range []string{"a", "b", "b"} {
		ts := time.Date(2026, 7, 1, i, 0, 0, 0, time.UTC).Format(time.RFC3339)
		if err := db.InsertRegimeWindowHistoryRow(key, "7d", label, "", ts); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	got, err := db.RegimeWindowTrailingLabels(key, "7d", 10)
	if err != nil {
		t.Fatalf("trailing: %v", err)
	}
	if len(got) != 3 || got[0] != "b" || got[2] != "a" {
		t.Errorf("trailing = %v, want [b b a]", got)
	}
	// Different window / key isolated.
	other := key
	other.Platform = "binanceus"
	if rows, _ := db.RegimeWindowTrailingLabels(other, "7d", 10); len(rows) != 0 {
		t.Errorf("cross-platform key must not collide, got %v", rows)
	}
	if rows, _ := db.RegimeWindowTrailingLabels(key, "1d", 10); len(rows) != 0 {
		t.Errorf("cross-window must not collide, got %v", rows)
	}
}

func TestRegimeTransitionAlertMarkAndPrune(t *testing.T) {
	db := newTransitionsTestDB(t)
	key := transitionsTestKey()
	oldTS := "2026-06-01T00:00:00Z"
	newTS := "2026-07-01T00:00:00Z"
	if err := db.InsertRegimeWindowTransition(key, "7d", "a", "b", "", oldTS); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertRegimeWindowTransition(key, "7d", "b", "c", "", newTS); err != nil {
		t.Fatal(err)
	}
	pending, err := db.UnalertedRegimeWindowTransitions(key, "7d")
	if err != nil || len(pending) != 2 {
		t.Fatalf("pending = %v (%v), want 2", pending, err)
	}
	if pending[0].OldLabel != "a" {
		t.Errorf("pending must be oldest-first, got %v", pending[0])
	}
	if err := db.MarkRegimeWindowTransitionsAlerted([]int64{pending[0].ID}, newTS); err != nil {
		t.Fatal(err)
	}
	pending, _ = db.UnalertedRegimeWindowTransitions(key, "7d")
	if len(pending) != 1 || pending[0].OldLabel != "b" {
		t.Errorf("after mark, pending = %v, want only b->c", pending)
	}
	if err := db.InsertRegimeWindowHistoryRow(key, "7d", "a", "", oldTS); err != nil {
		t.Fatal(err)
	}
	if err := db.PruneRegimeWindowRows("2026-06-15T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	recent, _ := db.RecentRegimeWindowTransitions(10)
	if len(recent) != 1 || recent[0].NewLabel != "c" {
		t.Errorf("prune must drop pre-cutoff rows only, got %v", recent)
	}
	if rows, _ := db.RegimeWindowTrailingLabels(key, "7d", 10); len(rows) != 0 {
		t.Errorf("prune must cover history rows, got %v", rows)
	}
}

func TestRegimeReversalSignaturePersistence(t *testing.T) {
	db := newTransitionsTestDB(t)
	key := transitionsTestKey()
	if sig, err := db.RegimeReversalSignature(key); err != nil || sig != "" {
		t.Fatalf("empty read = %q (%v)", sig, err)
	}
	if err := db.SetRegimeReversalSignature(key, "sig1", "t1"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetRegimeReversalSignature(key, "sig2", "t2"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if sig, _ := db.RegimeReversalSignature(key); sig != "sig2" {
		t.Errorf("sig = %q, want sig2", sig)
	}
	if err := db.ClearRegimeReversalSignature(key); err != nil {
		t.Fatal(err)
	}
	if sig, _ := db.RegimeReversalSignature(key); sig != "" {
		t.Errorf("after clear sig = %q", sig)
	}
}

// ─── End-to-end processor ────────────────────────────────────────────────────

func transitionsTestStore(t *testing.T, key regimeBundleKey, windows map[string]RegimeSnapshot, barTime string) *RegimeStore {
	t.Helper()
	store := &RegimeStore{}
	gen := store.resetForCycle(time.Now().UTC())
	store.set(&RegimeBundle{
		Key:     key,
		Payload: RegimePayload{MultiMode: true, Windows: windows},
		BarTime: barTime,
	}, gen)
	return store
}

func transitionsTestRegimeConfig() *RegimeConfig {
	return &RegimeConfig{
		Enabled: true,
		Windows: RegimeWindowsMap{
			"1d":  {Classifier: "composite", Period: 24},
			"3d":  {Classifier: "composite", Period: 72},
			"30d": {Classifier: "composite", Period: 720},
		},
		Transitions: &RegimeTransitionAlertsConfig{Enabled: true, DebounceCycles: 2},
	}
}

func runTransitionsCycle(db *StateDB, rc *RegimeConfig, windows map[string]RegimeSnapshot, now time.Time) {
	key := regimeBundleKey{Platform: "hyperliquid", Symbol: "BTC", Timeframe: "1h", SpecJSON: regimeWindowsSpecJSON(rc)}
	store := &RegimeStore{}
	gen := store.resetForCycle(now)
	store.set(&RegimeBundle{Key: key, Payload: RegimePayload{MultiMode: true, Windows: windows}, BarTime: "2026-07-01T00:00:00Z"}, gen)
	processRegimeTransitionAlerts(db, store, rc, nil, now)
}

func TestProcessRegimeTransitions_DebouncedSingleDM(t *testing.T) {
	resetRegimeTransitionGlobals(t)
	var dms []string
	regimeAlertSendFn = func(_ *MultiNotifier, msg string) { dms = append(dms, msg) }
	db := newTransitionsTestDB(t)
	rc := transitionsTestRegimeConfig()
	up := RegimeSnapshot{Regime: "trending_up"}
	down := RegimeSnapshot{Regime: "trending_down"}
	steadyUp := map[string]RegimeSnapshot{"1d": up, "3d": up, "30d": up}
	flippedNoReversal := map[string]RegimeSnapshot{"1d": down, "3d": up, "30d": up}

	now := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	// Boot: two steady cycles — no transition, no DM.
	runTransitionsCycle(db, rc, steadyUp, now)
	runTransitionsCycle(db, rc, steadyUp, now.Add(time.Minute))
	if len(dms) != 0 {
		t.Fatalf("boot/steady cycles must not DM, got %v", dms)
	}
	// 1d flips down; only 1 of 2 shorter windows opposes 30d=up... (3d still up)
	// so no reversal; transition alert after debounce (2 cycles).
	runTransitionsCycle(db, rc, flippedNoReversal, now.Add(2*time.Minute))
	if len(dms) != 0 {
		t.Fatalf("first flip cycle must not DM yet (debounce 2), got %v", dms)
	}
	runTransitionsCycle(db, rc, flippedNoReversal, now.Add(3*time.Minute))
	if len(dms) != 1 || !strings.Contains(dms[0], "1d: trending_up → trending_down") {
		t.Fatalf("confirmed flip must DM exactly once, got %v", dms)
	}
	// Steady after the flip: no repeat DM (idempotent per cycle).
	runTransitionsCycle(db, rc, flippedNoReversal, now.Add(4*time.Minute))
	if len(dms) != 1 {
		t.Fatalf("steady post-flip cycle re-DM'd: %v", dms)
	}
}

func TestProcessRegimeTransitions_RestartIdempotent(t *testing.T) {
	resetRegimeTransitionGlobals(t)
	var dms []string
	regimeAlertSendFn = func(_ *MultiNotifier, msg string) { dms = append(dms, msg) }
	dbPath := filepath.Join(t.TempDir(), "state.db")
	rc := transitionsTestRegimeConfig()
	up := RegimeSnapshot{Regime: "trending_up"}
	down := map[string]RegimeSnapshot{"1d": {Regime: "trending_down"}, "3d": up, "30d": up}
	steady := map[string]RegimeSnapshot{"1d": up, "3d": up, "30d": up}
	now := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

	db, err := OpenStateDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	runTransitionsCycle(db, rc, steady, now)
	runTransitionsCycle(db, rc, down, now.Add(time.Minute))
	runTransitionsCycle(db, rc, down, now.Add(2*time.Minute))
	if len(dms) != 1 {
		t.Fatalf("want 1 DM before restart, got %v", dms)
	}
	db.Close()

	// "Restart": fresh process state, same DB — same labels must not re-DM,
	// and the boot cycle must not fabricate a transition.
	regimeReversalPendingState = map[regimeBundleKey]*regimeReversalPending{}
	lastRegimeTransitionPrune = time.Time{}
	db2, err := OpenStateDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	runTransitionsCycle(db2, rc, down, now.Add(3*time.Minute))
	runTransitionsCycle(db2, rc, down, now.Add(4*time.Minute))
	if len(dms) != 1 {
		t.Fatalf("restart re-DM'd or fabricated a transition: %v", dms)
	}
}

func TestProcessRegimeTransitions_FlapBackNoDM(t *testing.T) {
	resetRegimeTransitionGlobals(t)
	var dms []string
	regimeAlertSendFn = func(_ *MultiNotifier, msg string) { dms = append(dms, msg) }
	db := newTransitionsTestDB(t)
	rc := transitionsTestRegimeConfig()
	up := RegimeSnapshot{Regime: "trending_up"}
	steady := map[string]RegimeSnapshot{"1d": up, "3d": up, "30d": up}
	blip := map[string]RegimeSnapshot{"1d": {Regime: "ranging_quiet"}, "3d": up, "30d": up}
	now := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

	runTransitionsCycle(db, rc, steady, now)
	runTransitionsCycle(db, rc, steady, now.Add(time.Minute))
	runTransitionsCycle(db, rc, blip, now.Add(2*time.Minute))   // 1-cycle blip
	runTransitionsCycle(db, rc, steady, now.Add(3*time.Minute)) // back before debounce
	runTransitionsCycle(db, rc, steady, now.Add(4*time.Minute))
	if len(dms) != 0 {
		t.Fatalf("sub-debounce flap must never DM, got %v", dms)
	}
	// The flap's transition rows are consumed (marked), not left pending.
	key := regimeBundleKey{Platform: "hyperliquid", Symbol: "BTC", Timeframe: "1h", SpecJSON: regimeWindowsSpecJSON(rc)}
	pending, _ := db.UnalertedRegimeWindowTransitions(key, "1d")
	if len(pending) != 0 {
		t.Errorf("flap rows must be marked alerted, still pending: %v", pending)
	}
}

func TestProcessRegimeTransitions_ReversalAlertOnceAndClears(t *testing.T) {
	resetRegimeTransitionGlobals(t)
	var dms []string
	regimeAlertSendFn = func(_ *MultiNotifier, msg string) { dms = append(dms, msg) }
	db := newTransitionsTestDB(t)
	rc := transitionsTestRegimeConfig()
	up := RegimeSnapshot{Regime: "trending_up"}
	down := RegimeSnapshot{Regime: "trending_down"}
	steadyDown := map[string]RegimeSnapshot{"1d": down, "3d": down, "30d": down}
	reversal := map[string]RegimeSnapshot{"1d": up, "3d": up, "30d": down}
	now := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

	runTransitionsCycle(db, rc, steadyDown, now)
	runTransitionsCycle(db, rc, steadyDown, now.Add(time.Minute))
	dms = nil // ignore any transition DMs from setup

	runTransitionsCycle(db, rc, reversal, now.Add(2*time.Minute))
	runTransitionsCycle(db, rc, reversal, now.Add(3*time.Minute))
	var reversalDMs []string
	for _, m := range dms {
		if strings.Contains(m, "reversal") {
			reversalDMs = append(reversalDMs, m)
		}
	}
	if len(reversalDMs) != 1 {
		t.Fatalf("want exactly 1 reversal DM after debounce, got %v", dms)
	}
	if !strings.Contains(reversalDMs[0], "30d=trending_down") ||
		!strings.Contains(reversalDMs[0], "1d=trending_up, 3d=trending_up") {
		t.Errorf("reversal DM must name windows and labels: %s", reversalDMs[0])
	}
	// Still-active identical pattern: no repeat.
	runTransitionsCycle(db, rc, reversal, now.Add(4*time.Minute))
	count := 0
	for _, m := range dms {
		if strings.Contains(m, "reversal") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("identical active pattern re-DM'd: %v", dms)
	}
	// Pattern clears for debounce cycles, then re-occurs → alerts again.
	allUp := map[string]RegimeSnapshot{"1d": up, "3d": up, "30d": up}
	runTransitionsCycle(db, rc, allUp, now.Add(5*time.Minute))
	runTransitionsCycle(db, rc, allUp, now.Add(6*time.Minute))
	runTransitionsCycle(db, rc, reversal, now.Add(7*time.Minute))
	runTransitionsCycle(db, rc, reversal, now.Add(8*time.Minute))
	count = 0
	for _, m := range dms {
		if strings.Contains(m, "reversal") {
			count++
		}
	}
	if count != 2 {
		t.Fatalf("re-occurrence after confirmed clear must re-alert, got %d reversal DMs: %v", count, dms)
	}
}

func TestProcessRegimeTransitions_DisabledNoRows(t *testing.T) {
	resetRegimeTransitionGlobals(t)
	regimeAlertSendFn = func(_ *MultiNotifier, msg string) { t.Errorf("unexpected DM: %s", msg) }
	db := newTransitionsTestDB(t)
	rc := transitionsTestRegimeConfig()
	rc.Transitions = nil
	runTransitionsCycle(db, rc, map[string]RegimeSnapshot{"1d": {Regime: "trending_up"}}, time.Now().UTC())
	rows, _ := db.RecentRegimeWindowTransitions(10)
	if len(rows) != 0 {
		t.Errorf("disabled feature wrote rows: %v", rows)
	}
}

func TestRecentRegimeTransitionsNote(t *testing.T) {
	db := newTransitionsTestDB(t)
	rc := &RegimeConfig{Transitions: &RegimeTransitionAlertsConfig{Enabled: true}}
	now := time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)
	if note := recentRegimeTransitionsNote(db, rc, now); note != "" {
		t.Errorf("empty DB note = %q", note)
	}
	key := transitionsTestKey()
	if err := db.InsertRegimeWindowTransition(key, "7d", "a", "b", "2026-07-01T22:00:00Z", "2026-07-01T22:00:01Z"); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertRegimeWindowTransition(key, "1d", "x", "y", "", "2026-06-01T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	note := recentRegimeTransitionsNote(db, rc, now)
	if !strings.Contains(note, "7d: a → b") {
		t.Errorf("note missing recent transition: %q", note)
	}
	if strings.Contains(note, "x → y") {
		t.Errorf("note must exclude >24h-old transitions: %q", note)
	}
	if recentRegimeTransitionsNote(nil, rc, now) != "" {
		t.Error("nil DB must yield empty note")
	}
	rc.Transitions = nil
	if recentRegimeTransitionsNote(db, rc, now) != "" {
		t.Error("disabled feature must yield empty note")
	}
}
