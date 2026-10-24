package main

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

const (
	orcOwnerID   = "owner-1"
	orcChannelID = "ch-ops"
	orcBotID     = "bot-1"
	orcSymbol    = "BTC"
)

var orcStart = time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)

type orcRecorder struct {
	f            *orcFixture
	mu           sync.Mutex
	channelSends []string
	dmSends      []string
	attempts     int
	lockedSends  int
	failChannel  bool
	failDM       bool
}

func (r *orcRecorder) probe() {
	if r.f.env.mu.TryLock() {
		r.f.env.mu.Unlock()
		return
	}
	r.lockedSends++
}

func (r *orcRecorder) SendMessage(channelID, content string) error {
	r.probe()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.attempts++
	if r.failChannel {
		return errors.New("channel send refused")
	}
	r.channelSends = append(r.channelSends, content)
	return nil
}

func (r *orcRecorder) SendDM(userID, content string) error {
	r.probe()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.attempts++
	if r.failDM {
		return errors.New("dm send refused")
	}
	r.dmSends = append(r.dmSends, content)
	return nil
}

func (r *orcRecorder) AskDM(userID, question string, timeout time.Duration) (string, error) {
	return "", ErrDMTimeout
}

func (r *orcRecorder) Close() {}

func (r *orcRecorder) counts() (int, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.channelSends), len(r.dmSends)
}

func (r *orcRecorder) lastChannel() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.channelSends) == 0 {
		return ""
	}
	return r.channelSends[len(r.channelSends)-1]
}

type orcFixture struct {
	t        *testing.T
	env      *entryTestEnv
	rec      *orcRecorder
	notifier *MultiNotifier
	clockMu  sync.Mutex
	now      time.Time
}

func orcOKXSpot(id string) StrategyConfig {
	sc := entryOKXTestStrategy(id, "spot")
	sc.MaxDrawdownPct = 10
	return sc
}

func orcRobinhoodOptions(id string) StrategyConfig {
	return StrategyConfig{ID: id, Type: "options", Platform: "robinhood", Script: "shared_scripts/check_options.py",
		Args: []string{"vol_mean_reversion", "SPY", "--mode=live"}, Capital: 1000, MaxDrawdownPct: 10}
}

func newORCFixture(t *testing.T, strategies ...StrategyConfig) *orcFixture {
	t.Helper()
	f := &orcFixture{t: t, now: orcStart}
	if err := bindTradingClock(t.Name(), f.clock); err != nil {
		t.Fatal(err)
	}
	prevInterval := effectiveAlertThrottleInterval()
	applyAlertThrottleInterval(DefaultAlertThrottleInterval)
	t.Cleanup(func() {
		unbindTradingClock(t.Name())
		applyAlertThrottleInterval(prevInterval)
		drainModelOnlyCloseAlerts()
	})
	f.env = openEntryTestEnv(t, newEntryTestConfig(t, strategies...))
	f.rec = &orcRecorder{f: f}
	f.notifier = NewMultiNotifier(notifierBackend{
		notifier: f.rec,
		channels: map[string]string{"okx": orcChannelID, "robinhood": orcChannelID},
		ownerID:  orcOwnerID,
	})
	return f
}

func (f *orcFixture) clock() time.Time {
	f.clockMu.Lock()
	defer f.clockMu.Unlock()
	return f.now
}

func (f *orcFixture) setNow(t time.Time) {
	f.clockMu.Lock()
	defer f.clockMu.Unlock()
	f.now = t.UTC()
}

func (f *orcFixture) advance(d time.Duration) time.Time {
	f.clockMu.Lock()
	defer f.clockMu.Unlock()
	f.now = f.now.Add(d)
	return f.now
}

func (f *orcFixture) restart() {
	f.t.Helper()
	f.env = f.env.restart()
}

func (f *orcFixture) config(id string) *StrategyConfig {
	for i := range f.env.cfg.Strategies {
		if f.env.cfg.Strategies[i].ID == id {
			return &f.env.cfg.Strategies[i]
		}
	}
	f.t.Fatalf("no strategy %s", id)
	return nil
}

func (f *orcFixture) openSpot(id string, qty float64) {
	f.env.mu.Lock()
	defer f.env.mu.Unlock()
	s := f.env.strat(id)
	if s.Positions == nil {
		s.Positions = map[string]*Position{}
	}
	s.Positions[orcSymbol] = &Position{Symbol: orcSymbol, Quantity: qty, AvgCost: 100, Side: "long", OpenedAt: f.clock()}
}

func (f *orcFixture) openOption(id, optID string, qty float64) {
	f.env.mu.Lock()
	defer f.env.mu.Unlock()
	s := f.env.strat(id)
	if s.OptionPositions == nil {
		s.OptionPositions = map[string]*OptionPosition{}
	}
	s.OptionPositions[optID] = &OptionPosition{ID: optID, Underlying: "SPY", OptionType: "call", Strike: 500, Expiry: "2026-06-19",
		Action: "buy", Quantity: qty, EntryPremiumUSD: 100, CurrentValueUSD: 100, OpenedAt: f.clock()}
}

func (f *orcFixture) fireDrawdownErr(id string) error {
	sc := f.config(id)
	f.env.mu.Lock()
	defer f.env.mu.Unlock()
	s := f.env.strat(id)
	prices := map[string]float64{orcSymbol: 100}
	if ok, reason := CheckRisk(sc, s, 1000, prices, nil, nil); !ok {
		return fmt.Errorf("the peak check for %s was blocked: %s", id, reason)
	}
	if ok, reason := CheckRisk(sc, s, 500, prices, nil, nil); ok || !strings.Contains(reason, RiskReasonMaxDrawdownExceeded) {
		return fmt.Errorf("expected a drawdown fire for %s, got allowed=%v reason=%q", id, ok, reason)
	}
	return nil
}

func (f *orcFixture) fireLossStreakErr(id string) error {
	sc := f.config(id)
	f.env.mu.Lock()
	defer f.env.mu.Unlock()
	s := f.env.strat(id)
	pv := s.RiskState.PeakValue
	if pv <= 0 {
		pv = 1000
	}
	if s.RiskState.CircuitBreaker {
		if ok, reason := CheckRisk(sc, s, pv, map[string]float64{orcSymbol: 100}, nil, nil); !ok {
			return fmt.Errorf("the circuit breaker for %s is still latched: %s", id, reason)
		}
	}
	s.RiskState.ConsecutiveLosses = sc.CircuitBreakerLossStreakThreshold()
	ok, reason := CheckRisk(sc, s, pv, map[string]float64{orcSymbol: 100}, nil, nil)
	if ok || !strings.Contains(reason, RiskReasonConsecutiveLosses) {
		return fmt.Errorf("expected a loss-streak fire for %s, got allowed=%v reason=%q", id, ok, reason)
	}
	return nil
}

func (f *orcFixture) fireDrawdown(id string) {
	f.t.Helper()
	if err := f.fireDrawdownErr(id); err != nil {
		f.t.Fatal(err)
	}
}

func (f *orcFixture) fireLossStreak(id string) {
	f.t.Helper()
	if err := f.fireLossStreakErr(id); err != nil {
		f.t.Fatal(err)
	}
}

func (f *orcFixture) entry(id, platform string) *PendingCircuitClose {
	f.env.mu.RLock()
	defer f.env.mu.RUnlock()
	s := f.env.strat(id)
	if s == nil {
		return nil
	}
	p := s.RiskState.getPendingCircuitClose(platform)
	if p == nil {
		return nil
	}
	cp := *p
	cp.Symbols = append([]PendingCircuitCloseSymbol(nil), p.Symbols...)
	return &cp
}

func (f *orcFixture) pendingJSON(id string) string {
	f.env.mu.RLock()
	defer f.env.mu.RUnlock()
	return f.env.strat(id).RiskState.MarshalPendingCircuitClosesJSON()
}

func (f *orcFixture) storedPendingJSON(id string) string {
	f.t.Helper()
	db := f.env.fileFor(id)
	sid, err := db.toStorageID(id)
	if err != nil {
		f.t.Fatal(err)
	}
	var raw string
	err = db.db.QueryRow(`SELECT risk_pending_circuit_closes_json FROM strategies WHERE id = ?`, sid).Scan(&raw)
	if err == sql.ErrNoRows {
		return ""
	}
	if err != nil {
		f.t.Fatal(err)
	}
	return raw
}

func captureORCStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	out := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		out <- string(b)
	}()
	fn()
	w.Close()
	os.Stdout = orig
	return <-out
}

func (f *orcFixture) drain() string {
	return captureORCStdout(f.t, func() {
		drainOperatorRequiredPendingCloses(f.env.state, f.notifier, &f.env.mu, f.env.store)
	})
}

func (f *orcFixture) expectSends(label string, fn func(), wantChannel, wantDM int) string {
	f.t.Helper()
	c0, d0 := f.rec.counts()
	out := captureORCStdout(f.t, fn)
	c1, d1 := f.rec.counts()
	if c1-c0 != wantChannel || d1-d0 != wantDM {
		f.t.Fatalf("%s: got %d channel posts and %d owner DMs, want %d and %d\nlog:\n%s", label, c1-c0, d1-d0, wantChannel, wantDM, out)
	}
	return out
}

func (f *orcFixture) drainExpect(label string, wantChannel, wantDM int) string {
	f.t.Helper()
	return f.expectSends(label, func() {
		drainOperatorRequiredPendingCloses(f.env.state, f.notifier, &f.env.mu, f.env.store)
	}, wantChannel, wantDM)
}

func orcCriticalLines(log string) int {
	return strings.Count(log, "[CRITICAL] operator-required-close: strategy ") - strings.Count(log, "saving the reminder stamp failed")
}

func (f *orcFixture) assertNoLockedSends() {
	f.t.Helper()
	f.rec.mu.Lock()
	defer f.rec.mu.Unlock()
	if f.rec.lockedSends != 0 {
		f.t.Fatalf("%d notification sends ran while the state lock was held", f.rec.lockedSends)
	}
}

func TestOperatorRequiredCloseReminderThrottleSurvivesRestart(t *testing.T) {
	f := newORCFixture(t, orcOKXSpot("okx-a"))
	f.openSpot("okx-a", 0.5)
	f.fireDrawdown("okx-a")
	fired := f.clock()

	e := f.entry("okx-a", PlatformPendingCloseOKXSpot)
	if e == nil || !e.OperatorRequired || !strings.HasPrefix(e.Episode, "orc-") || !e.FiredAt.Equal(fired) || !e.LastNotifiedAt.IsZero() {
		t.Fatalf("the fire must create an operator-required entry with an episode, the fire time and no stamp: %+v", e)
	}
	if len(e.Symbols) != 1 || e.Symbols[0].Symbol != orcSymbol || e.Symbols[0].Size != 0.5 {
		t.Fatalf("the entry must hold the own-book leg: %+v", e.Symbols)
	}
	if qty, _ := f.env.position("okx-a", orcSymbol); qty != 0 {
		t.Fatalf("the fire must still close the virtual book, got %v", qty)
	}

	log := f.drainExpect("first drain", 1, 1)
	if orcCriticalLines(log) != 1 || !strings.Contains(log, "; warning due") {
		t.Fatalf("the first drain must log one CRITICAL line marked due:\n%s", log)
	}
	msg := f.rec.lastChannel()
	for _, want := range []string{"okx-a", "go-trader-clear-operator-close", DefaultAlertThrottleInterval.String(), "alert_throttle_interval", "virtual size at fire", "episode " + e.Episode, "CB until"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("the warning must contain %q:\n%s", want, msg)
		}
	}
	stamp := f.entry("okx-a", PlatformPendingCloseOKXSpot).LastNotifiedAt
	if !stamp.Equal(fired) {
		t.Fatalf("the drain must stamp the trading-clock time %s, got %s", fired, stamp)
	}
	if got := f.storedPendingJSON("okx-a"); !strings.Contains(got, `"last_notified_at":"`+fired.Format(time.RFC3339)) {
		t.Fatalf("the stamp must be saved at once, stored %s", got)
	}

	for i := 0; i < 2; i++ {
		f.advance(time.Hour)
		log = f.drainExpect(fmt.Sprintf("drain %d inside the window", i+2), 0, 0)
		if orcCriticalLines(log) != 1 || !strings.Contains(log, "next warning after "+fired.Add(DefaultAlertThrottleInterval).Format(operatorRequiredTimeFormat)) {
			t.Fatalf("a quiet drain must still log the CRITICAL line with the next warning time:\n%s", log)
		}
	}

	f.restart()
	got := f.entry("okx-a", PlatformPendingCloseOKXSpot)
	if got == nil || !got.LastNotifiedAt.Equal(stamp) || got.Episode != e.Episode || !got.FiredAt.Equal(fired) {
		t.Fatalf("restart must keep the stamp, episode and fire time: %+v", got)
	}
	f.drainExpect("drain after restart inside the window", 0, 0)

	f.setNow(stamp.Add(DefaultAlertThrottleInterval))
	f.drainExpect("drain at the interval", 1, 1)
	f.advance(time.Minute)
	f.drainExpect("drain after the repeat", 0, 0)
	f.assertNoLockedSends()
}

func TestOperatorRequiredCloseMixedDueEntriesListOnlyTheDueOne(t *testing.T) {
	f := newORCFixture(t, orcOKXSpot("okx-a"), orcOKXSpot("okx-b"))
	f.openSpot("okx-a", 0.5)
	f.fireDrawdown("okx-a")
	f.drainExpect("first strategy", 1, 1)
	stampA := f.entry("okx-a", PlatformPendingCloseOKXSpot).LastNotifiedAt

	f.advance(time.Hour)
	f.openSpot("okx-b", 0.25)
	f.fireDrawdown("okx-b")
	log := f.drainExpect("mixed drain", 1, 1)
	msg := f.rec.lastChannel()
	if !strings.Contains(msg, "okx-b") || strings.Contains(msg, "okx-a") || !strings.Contains(msg, "1 strategy-platform pair hit") {
		t.Fatalf("the message must list only the due entry:\n%s", msg)
	}
	if orcCriticalLines(log) != 2 {
		t.Fatalf("both entries must log a CRITICAL line:\n%s", log)
	}
	if got := f.entry("okx-a", PlatformPendingCloseOKXSpot).LastNotifiedAt; !got.Equal(stampA) {
		t.Fatalf("the entry that was not due must keep its stamp %s, got %s", stampA, got)
	}
	if got := f.entry("okx-b", PlatformPendingCloseOKXSpot).LastNotifiedAt; !got.Equal(f.clock()) {
		t.Fatalf("the due entry must be stamped, got %s", got)
	}
	f.assertNoLockedSends()
}

func TestOperatorRequiredCloseNewFireSendsAtOnceAndKeepsEarlierLegs(t *testing.T) {
	t.Run("okx_spot", func(t *testing.T) {
		f := newORCFixture(t, orcOKXSpot("okx-a"))
		f.openSpot("okx-a", 0.5)
		f.fireLossStreak("okx-a")
		f.drainExpect("first fire", 1, 1)
		first := f.entry("okx-a", PlatformPendingCloseOKXSpot)

		f.advance(2 * time.Hour)
		f.openSpot("okx-a", 0.5)
		f.fireLossStreak("okx-a")
		second := f.entry("okx-a", PlatformPendingCloseOKXSpot)
		if second.Episode == first.Episode || !second.LastNotifiedAt.IsZero() || !second.FiredAt.Equal(f.clock()) {
			t.Fatalf("a fire with identical legs must write a new episode and reset the stamp: first %+v second %+v", first, second)
		}
		if len(second.Symbols) != 1 || second.Symbols[0].Size != 0.5 {
			t.Fatalf("identical legs must merge to one leg: %+v", second.Symbols)
		}
		f.drainExpect("drain after an identical-leg fire inside the window", 1, 1)

		f.advance(2 * time.Hour)
		f.openSpot("okx-a", 0.8)
		f.fireLossStreak("okx-a")
		if got := f.entry("okx-a", PlatformPendingCloseOKXSpot).Symbols; len(got) != 1 || got[0].Size != 0.8 {
			t.Fatalf("a larger fire must keep the larger size: %+v", got)
		}
		f.drainExpect("drain after a larger fire", 1, 1)

		f.advance(2 * time.Hour)
		f.openSpot("okx-a", 0.3)
		f.fireLossStreak("okx-a")
		if got := f.entry("okx-a", PlatformPendingCloseOKXSpot).Symbols; len(got) != 1 || got[0].Size != 0.8 {
			t.Fatalf("a smaller fire must keep the earlier larger size: %+v", got)
		}
		f.drainExpect("drain after a smaller fire", 1, 1)
		if !strings.Contains(f.rec.lastChannel(), "virtual size at fire 0.800000") {
			t.Fatalf("the warning must show the historical size:\n%s", f.rec.lastChannel())
		}
		f.assertNoLockedSends()
	})
	t.Run("robinhood_options", func(t *testing.T) {
		f := newORCFixture(t, orcRobinhoodOptions("rh-opt"))
		f.openOption("rh-opt", "opt-b", 2)
		f.openOption("rh-opt", "opt-a", 1)
		f.fireLossStreak("rh-opt")
		first := f.entry("rh-opt", PlatformPendingCloseRobinhoodOptions)
		if first == nil || len(first.Symbols) != 2 || first.Symbols[0].Symbol != "opt-a" || first.Symbols[1].Symbol != "opt-b" {
			t.Fatalf("the fire must record every option leg in symbol order: %+v", first)
		}
		f.drainExpect("first options fire", 1, 1)

		f.advance(2 * time.Hour)
		f.openOption("rh-opt", "opt-c", 1)
		f.openOption("rh-opt", "opt-b", 3)
		f.fireLossStreak("rh-opt")
		got := f.entry("rh-opt", PlatformPendingCloseRobinhoodOptions)
		want := []PendingCircuitCloseSymbol{{Symbol: "opt-a", Size: 1}, {Symbol: "opt-b", Size: 3}, {Symbol: "opt-c", Size: 1}}
		if !operatorCloseLegsEqual(got.Symbols, want) || got.Episode == first.Episode || !got.LastNotifiedAt.IsZero() {
			t.Fatalf("the second fire must merge the union with the larger size in symbol order: %+v", got)
		}
		f.drainExpect("drain after the second options fire", 1, 1)
		msg := f.rec.lastChannel()
		if !strings.Contains(msg, "opt-a (virtual size at fire 1.000000)") || !strings.Contains(msg, "Robinhood options") {
			t.Fatalf("the warning must keep the earlier unacknowledged leg:\n%s", msg)
		}
		f.assertNoLockedSends()
	})
}

func TestOperatorRequiredCloseFlatBookFireAddsNothing(t *testing.T) {
	f := newORCFixture(t, orcOKXSpot("okx-a"), orcRobinhoodOptions("rh-opt"), orcOKXSpot("okx-old"))
	for _, c := range []struct{ id, platform string }{{"okx-a", PlatformPendingCloseOKXSpot}, {"rh-opt", PlatformPendingCloseRobinhoodOptions}} {
		log := captureORCStdout(t, func() { f.fireDrawdown(c.id) })
		if !strings.Contains(log, "circuit breaker fired with a flat own book; no new leg was added") {
			t.Fatalf("%s: a flat-book fire must log the WARN line:\n%s", c.id, log)
		}
		if e := f.entry(c.id, c.platform); e != nil {
			t.Fatalf("%s: a flat-book fire must create no entry, got %+v", c.id, e)
		}
	}
	if log := f.drainExpect("drain after flat fires", 0, 0); strings.Contains(log, "operator-required-close") {
		t.Fatalf("no entry means no CRITICAL line:\n%s", log)
	}

	f.openSpot("okx-old", 0.5)
	f.fireLossStreak("okx-old")
	f.drainExpect("older entry", 1, 1)
	before := f.entry("okx-old", PlatformPendingCloseOKXSpot)
	f.advance(2 * time.Hour)
	f.fireLossStreak("okx-old")
	after := f.entry("okx-old", PlatformPendingCloseOKXSpot)
	if after.Episode != before.Episode || !after.LastNotifiedAt.Equal(before.LastNotifiedAt) || !after.FiredAt.Equal(before.FiredAt) || !operatorCloseLegsEqual(after.Symbols, before.Symbols) {
		t.Fatalf("a flat-book fire must leave the earlier entry unchanged: before %+v after %+v", before, after)
	}
	f.drainExpect("drain inside the window after a flat-book fire", 0, 0)
	f.assertNoLockedSends()
}

func TestOperatorRequiredCloseExpiryKeepsTheEntry(t *testing.T) {
	f := newORCFixture(t, orcOKXSpot("okx-a"))
	f.openSpot("okx-a", 0.5)
	f.fireDrawdown("okx-a")
	f.drainExpect("first drain", 1, 1)

	f.advance(DefaultCBDrawdownCooldown + time.Hour)
	sc := f.config("okx-a")
	f.env.mu.Lock()
	s := f.env.strat("okx-a")
	ok, reason := CheckRisk(sc, s, s.RiskState.PeakValue, map[string]float64{orcSymbol: 100}, nil, nil)
	cb := s.RiskState.CircuitBreaker
	f.env.mu.Unlock()
	if !ok || cb {
		t.Fatalf("the circuit breaker must unlatch after its cooldown: allowed=%v reason=%q latched=%v", ok, reason, cb)
	}
	if e := f.entry("okx-a", PlatformPendingCloseOKXSpot); e == nil || !e.OperatorRequired {
		t.Fatal("circuit-breaker expiry must keep the operator-required entry")
	}
	f.drainExpect("reminder after expiry", 1, 1)
	if msg := f.rec.lastChannel(); !strings.Contains(msg, "CB cooldown ended") {
		t.Fatalf("the reminder must say the cooldown ended:\n%s", msg)
	}
	f.advance(time.Hour)
	f.drainExpect("quiet after the expiry reminder", 0, 0)
	f.assertNoLockedSends()
}

func TestOperatorRequiredCloseDeliveryAndSaveFailures(t *testing.T) {
	t.Run("save_failure_keeps_the_memory_stamp", func(t *testing.T) {
		f := newORCFixture(t, orcOKXSpot("okx-a"))
		f.openSpot("okx-a", 0.5)
		f.fireDrawdown("okx-a")
		f.env.fileFor("okx-a").readOnly = true
		log := f.drainExpect("drain with a failed save", 1, 1)
		if !strings.Contains(log, "[CRITICAL] operator-required-close: strategy okx-a: saving the reminder stamp failed") {
			t.Fatalf("a failed stamp save must log CRITICAL:\n%s", log)
		}
		stamp := f.entry("okx-a", PlatformPendingCloseOKXSpot).LastNotifiedAt
		if !stamp.Equal(f.clock()) {
			t.Fatalf("the memory stamp must stay after a failed save, got %s", stamp)
		}
		f.advance(time.Hour)
		f.drainExpect("drain inside the window after a failed save", 0, 0)
		f.env.fileFor("okx-a").readOnly = false
		f.env.saveAll()
		f.restart()
		if got := f.entry("okx-a", PlatformPendingCloseOKXSpot); got == nil || !got.LastNotifiedAt.Equal(stamp) {
			t.Fatalf("the cycle save must persist the stamp: %+v", got)
		}
		f.drainExpect("drain after restart", 0, 0)
		f.assertNoLockedSends()
	})
	t.Run("restart_before_the_save_repeats_once", func(t *testing.T) {
		f := newORCFixture(t, orcOKXSpot("okx-a"))
		f.openSpot("okx-a", 0.5)
		f.fireDrawdown("okx-a")
		f.env.saveAll()
		f.env.fileFor("okx-a").readOnly = true
		f.drainExpect("drain with a failed save", 1, 1)
		f.restart()
		if got := f.entry("okx-a", PlatformPendingCloseOKXSpot); got == nil || !got.LastNotifiedAt.IsZero() {
			t.Fatalf("the restart must load the unstamped entry: %+v", got)
		}
		f.advance(time.Minute)
		f.drainExpect("the repeated warning after restart", 1, 1)
		f.advance(time.Minute)
		f.drainExpect("quiet after the repeat", 0, 0)
	})
	t.Run("no_backend_stamps_nothing", func(t *testing.T) {
		f := newORCFixture(t, orcOKXSpot("okx-a"))
		f.openSpot("okx-a", 0.5)
		f.fireDrawdown("okx-a")
		f.notifier = NewMultiNotifier()
		log := f.drainExpect("drain with no backend", 0, 0)
		if !strings.Contains(log, "no notification backend is configured; the warning for 1 entry was not sent and nothing was stamped") || orcCriticalLines(log) != 1 {
			t.Fatalf("a drain with no backend must log both CRITICAL lines:\n%s", log)
		}
		if got := f.entry("okx-a", PlatformPendingCloseOKXSpot); !got.LastNotifiedAt.IsZero() {
			t.Fatalf("a drain with no backend must stamp nothing: %+v", got)
		}
		captureORCStdout(t, func() { drainOperatorRequiredPendingCloses(f.env.state, nil, &f.env.mu, f.env.store) })
		if got := f.entry("okx-a", PlatformPendingCloseOKXSpot); !got.LastNotifiedAt.IsZero() {
			t.Fatalf("a nil notifier must stamp nothing: %+v", got)
		}
	})
	t.Run("every_destination_failing_retries", func(t *testing.T) {
		f := newORCFixture(t, orcOKXSpot("okx-a"))
		f.openSpot("okx-a", 0.5)
		f.fireDrawdown("okx-a")
		f.rec.failChannel, f.rec.failDM = true, true
		log := f.drainExpect("drain where every send fails", 0, 0)
		if !strings.Contains(log, "the warning reached no channel and no owner DM; nothing was stamped and the next drain retries") {
			t.Fatalf("a fully failed send must log CRITICAL:\n%s", log)
		}
		if got := f.entry("okx-a", PlatformPendingCloseOKXSpot); !got.LastNotifiedAt.IsZero() {
			t.Fatalf("a fully failed send must stamp nothing: %+v", got)
		}
		attempts := f.rec.attempts
		f.advance(time.Minute)
		f.drainExpect("retry", 0, 0)
		if f.rec.attempts != attempts+2 {
			t.Fatalf("the next drain must retry both destinations, attempts %d -> %d", attempts, f.rec.attempts)
		}
		f.rec.failChannel = false
		f.advance(time.Minute)
		f.drainExpect("partial delivery", 1, 0)
		if got := f.entry("okx-a", PlatformPendingCloseOKXSpot); !got.LastNotifiedAt.Equal(f.clock()) {
			t.Fatalf("partial delivery must stamp the entry: %+v", got)
		}
		f.advance(time.Minute)
		f.drainExpect("quiet after partial delivery", 0, 0)
		f.assertNoLockedSends()
	})
	t.Run("backward_clock_is_due", func(t *testing.T) {
		f := newORCFixture(t, orcOKXSpot("okx-a"))
		f.openSpot("okx-a", 0.5)
		f.fireDrawdown("okx-a")
		f.drainExpect("first drain", 1, 1)
		earlier := f.advance(-time.Hour)
		f.drainExpect("drain on a clock that moved back", 1, 1)
		if got := f.entry("okx-a", PlatformPendingCloseOKXSpot); !got.LastNotifiedAt.Equal(earlier) {
			t.Fatalf("the stamp must move to the earlier time: %+v", got)
		}
		f.advance(time.Minute)
		f.drainExpect("quiet after the backward stamp", 0, 0)
	})
}

func TestOperatorRequiredCloseKillSwitchFireStillClearsBothKeys(t *testing.T) {
	f := newORCFixture(t, orcOKXSpot("okx-a"), orcRobinhoodOptions("rh-opt"))
	f.openSpot("okx-a", 0.5)
	f.fireDrawdown("okx-a")
	f.openOption("rh-opt", "opt-a", 1)
	f.fireDrawdown("rh-opt")
	f.env.mu.Lock()
	f.env.strat("okx-a").RiskState.setPendingCircuitClose(PlatformPendingCloseHyperliquid, &PendingCircuitClose{Symbols: []PendingCircuitCloseSymbol{{Symbol: "ETH", Size: 1}}})
	f.env.mu.Unlock()
	f.env.saveAll()

	f.env.mu.Lock()
	for _, id := range []string{"okx-a", "rh-opt"} {
		f.env.strat(id).RiskState.clearPendingCircuitClosesForKillSwitch()
	}
	f.env.mu.Unlock()
	f.env.saveAll()
	f.restart()
	for _, id := range []string{"okx-a", "rh-opt"} {
		if got := f.pendingJSON(id); got != "" {
			t.Fatalf("%s: the kill-switch clear must remove every pending close, kept %s", id, got)
		}
	}
	f.drainExpect("drain after the kill-switch clear", 0, 0)
}

type orcTransport struct {
	f            *orcFixture
	mu           sync.Mutex
	prompts      []string
	responds     []string
	followups    []string
	lockedPrompt int
}

func (tr *orcTransport) ChannelMessageSend(channelID, content string, _ ...discordgo.RequestOption) (*discordgo.Message, error) {
	locked := false
	if tr.f.env.mu.TryLock() {
		tr.f.env.mu.Unlock()
	} else {
		locked = true
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if locked {
		tr.lockedPrompt++
	}
	tr.prompts = append(tr.prompts, content)
	return &discordgo.Message{ID: "m", ChannelID: channelID, Content: content}, nil
}

func (tr *orcTransport) UserChannelCreate(recipientID string, _ ...discordgo.RequestOption) (*discordgo.Channel, error) {
	return &discordgo.Channel{ID: "dm-" + recipientID}, nil
}

func (tr *orcTransport) InteractionRespond(_ *discordgo.Interaction, resp *discordgo.InteractionResponse, _ ...discordgo.RequestOption) error {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	content := ""
	if resp != nil && resp.Data != nil {
		content = resp.Data.Content
	}
	tr.responds = append(tr.responds, content)
	return nil
}

func (tr *orcTransport) FollowupMessageCreate(_ *discordgo.Interaction, _ bool, data *discordgo.WebhookParams, _ ...discordgo.RequestOption) (*discordgo.Message, error) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.followups = append(tr.followups, data.Content)
	return &discordgo.Message{}, nil
}

func (tr *orcTransport) ApplicationCommandBulkOverwrite(string, string, []*discordgo.ApplicationCommand, ...discordgo.RequestOption) ([]*discordgo.ApplicationCommand, error) {
	return nil, nil
}

type orcCommander struct {
	f  *orcFixture
	tr *orcTransport
	d  *DiscordNotifier
}

func (f *orcFixture) commander(ownerID string) *orcCommander {
	st := discordgo.NewState()
	st.User = &discordgo.User{ID: orcBotID}
	tr := &orcTransport{f: f}
	d := &DiscordNotifier{
		session:   &discordgo.Session{State: st},
		transport: tr,
		ownerID:   ownerID,
		ss:        &StatusServer{state: f.env.state, mu: &f.env.mu, stateDB: f.env.store, strategies: f.env.cfg.Strategies},
	}
	return &orcCommander{f: f, tr: tr, d: d}
}

func orcCommandOpts(strategy, platform string) []*discordgo.ApplicationCommandInteractionDataOption {
	var opts []*discordgo.ApplicationCommandInteractionDataOption
	if strategy != "" {
		opts = append(opts, &discordgo.ApplicationCommandInteractionDataOption{Name: "strategy", Type: discordgo.ApplicationCommandOptionString, Value: strategy})
	}
	if platform != "" {
		opts = append(opts, &discordgo.ApplicationCommandInteractionDataOption{Name: "platform", Type: discordgo.ApplicationCommandOptionString, Value: platform})
	}
	return opts
}

func (c *orcCommander) run(userID, guildID string, opts []*discordgo.ApplicationCommandInteractionDataOption, reply string, hook func()) string {
	c.f.t.Helper()
	ic := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		Type:    discordgo.InteractionApplicationCommand,
		GuildID: guildID,
		Data:    discordgo.ApplicationCommandInteractionData{Name: commandPrefix + operatorCloseCommandName, Options: opts},
	}}
	if guildID != "" {
		ic.Member = &discordgo.Member{User: &discordgo.User{ID: userID}}
	} else {
		ic.User = &discordgo.User{ID: userID}
	}
	c.tr.mu.Lock()
	r0, f0 := len(c.tr.responds), len(c.tr.followups)
	c.tr.mu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		if reply == "" {
			return
		}
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			c.d.mu.Lock()
			n := len(c.d.dmHandlers)
			c.d.mu.Unlock()
			if n > 0 {
				break
			}
			time.Sleep(time.Millisecond)
		}
		if hook != nil {
			hook()
		}
		c.d.handleDirectMessage(&discordgo.MessageCreate{Message: &discordgo.Message{Author: &discordgo.User{ID: userID}, Content: reply}})
	}()
	c.d.interactionCreate(c.tr, ic)
	<-done
	c.tr.mu.Lock()
	defer c.tr.mu.Unlock()
	if len(c.tr.followups) > f0 {
		return c.tr.followups[len(c.tr.followups)-1]
	}
	if len(c.tr.responds) > r0 {
		return c.tr.responds[len(c.tr.responds)-1]
	}
	return ""
}

func (c *orcCommander) lastPrompt() string {
	c.tr.mu.Lock()
	defer c.tr.mu.Unlock()
	if len(c.tr.prompts) == 0 {
		return ""
	}
	return c.tr.prompts[len(c.tr.prompts)-1]
}

func (c *orcCommander) assertNoLockedPrompt() {
	c.f.t.Helper()
	c.tr.mu.Lock()
	defer c.tr.mu.Unlock()
	if c.tr.lockedPrompt != 0 {
		c.f.t.Fatalf("%d confirm prompts were sent while the state lock was held", c.tr.lockedPrompt)
	}
}

func TestOperatorRequiredCloseCommandDispatch(t *testing.T) {
	f := newORCFixture(t, orcOKXSpot("okx-a"), orcRobinhoodOptions("rh-opt"), orcOKXSpot("auto-1"), orcOKXSpot("both"))
	f.openSpot("okx-a", 0.5)
	f.fireDrawdown("okx-a")
	f.openOption("rh-opt", "opt-a", 1)
	f.fireDrawdown("rh-opt")
	f.env.mu.Lock()
	auto := f.env.strat("auto-1")
	for _, key := range []string{PlatformPendingCloseRobinhood, PlatformPendingCloseOKX, PlatformPendingCloseHyperliquid, PlatformPendingCloseTopStep, PlatformPendingCloseOKXSpot} {
		auto.RiskState.setPendingCircuitClose(key, &PendingCircuitClose{Symbols: []PendingCircuitCloseSymbol{{Symbol: "ETH", Size: 1}}})
	}
	both := f.env.strat("both")
	both.RiskState.setPendingCircuitClose(PlatformPendingCloseOKXSpot, &PendingCircuitClose{Symbols: []PendingCircuitCloseSymbol{{Symbol: orcSymbol, Size: 1}}, OperatorRequired: true, Episode: "orc-both-spot"})
	both.RiskState.setPendingCircuitClose(PlatformPendingCloseRobinhoodOptions, &PendingCircuitClose{Symbols: []PendingCircuitCloseSymbol{{Symbol: "opt-z", Size: 1}}, OperatorRequired: true, Episode: "orc-both-opt"})
	f.env.mu.Unlock()
	f.env.saveAll()

	cmds := slashCommands()
	var found *discordgo.ApplicationCommand
	for _, c := range cmds {
		if c.Name == commandPrefix+operatorCloseCommandName {
			found = c
		}
	}
	if found == nil || found.Contexts == nil || len(*found.Contexts) != 1 || (*found.Contexts)[0] != discordgo.InteractionContextBotDM || len(found.Description) > 100 {
		t.Fatalf("the slash command must be registered for bot DMs with a short description: %+v", found)
	}
	if len(found.Options) != 2 || found.Options[0].Name != "strategy" || !found.Options[0].Required || found.Options[1].Name != "platform" || found.Options[1].Required || len(found.Options[1].Choices) != 2 {
		t.Fatalf("the slash command must take a required strategy and an optional platform with two choices: %+v", found.Options)
	}
	if !opsCommandNames[operatorCloseCommandName] {
		t.Fatal("the command must be an owner-only ops command")
	}

	ids := []string{"okx-a", "rh-opt", "auto-1", "both"}
	snapshot := func() map[string][2]string {
		out := map[string][2]string{}
		for _, id := range ids {
			out[id] = [2]string{f.pendingJSON(id), f.storedPendingJSON(id)}
		}
		return out
	}
	before := snapshot()
	assertUnchanged := func(label, msg string) {
		t.Helper()
		after := snapshot()
		for _, id := range ids {
			if after[id] != before[id] {
				t.Fatalf("%s (%q) changed %s: before %v after %v", label, msg, id, before[id], after[id])
			}
		}
	}

	owner := f.commander(orcOwnerID)
	refusals := []struct {
		label, user, guild, strategy, platform, want string
		c                                            *orcCommander
	}{
		{"non-owner", "intruder", "", "okx-a", "", "not authorized", owner},
		{"guild", orcOwnerID, "guild-1", "okx-a", "", "only available in a DM", owner},
		{"owner unset", orcOwnerID, "", "okx-a", "", "owner is not configured", f.commander("")},
		{"missing strategy", orcOwnerID, "", "", "", "usage: /go-trader-clear-operator-close", owner},
		{"unknown strategy", orcOwnerID, "", "nope", "", "is not in the running state", owner},
		{"no entry", orcOwnerID, "", "auto-1", "", "has no operator-required circuit-breaker entry", owner},
		{"non-operator okx_spot entry", orcOwnerID, "", "auto-1", PlatformPendingCloseOKXSpot, "has no operator-required circuit-breaker entry under okx_spot", owner},
		{"unknown platform", orcOwnerID, "", "okx-a", "binanceus", "is not okx_spot or robinhood_options", owner},
		{"ambiguous platform", orcOwnerID, "", "both", "", "Run the command again with the platform option", owner},
		{"wrong platform for the strategy", orcOwnerID, "", "okx-a", PlatformPendingCloseRobinhoodOptions, "has no operator-required circuit-breaker entry under robinhood_options", owner},
	}
	for _, key := range []string{PlatformPendingCloseRobinhood, PlatformPendingCloseOKX, PlatformPendingCloseHyperliquid, PlatformPendingCloseTopStep} {
		refusals = append(refusals, struct {
			label, user, guild, strategy, platform, want string
			c                                            *orcCommander
		}{"automatic " + key, orcOwnerID, "", "auto-1", key, "is not okx_spot or robinhood_options", owner})
	}
	for _, r := range refusals {
		msg := r.c.run(r.user, r.guild, orcCommandOpts(r.strategy, r.platform), "", nil)
		if !strings.Contains(msg, r.want) {
			t.Fatalf("%s: got %q, want %q", r.label, msg, r.want)
		}
		assertUnchanged(r.label, msg)
	}
	if msg := owner.run(orcOwnerID, "", orcCommandOpts("auto-1", ""), "", nil); !strings.Contains(msg, "Other pending closes (hyperliquid, okx, okx_spot, robinhood, topstep) have automatic close owners") {
		t.Fatalf("the refusal must list the automatic pending closes in sorted order: %q", msg)
	}

	notReady := f.commander(orcOwnerID)
	notReady.d.ss.stateDB = nil
	if msg := notReady.run(orcOwnerID, "", orcCommandOpts("okx-a", ""), "", nil); msg != "status server not ready" {
		t.Fatalf("a missing store must refuse, got %q", msg)
	}
	assertUnchanged("store not ready", "")

	msg := owner.run(orcOwnerID, "", orcCommandOpts("okx-a", ""), "no", nil)
	if !strings.HasPrefix(msg, "Cancelled: no confirmation received") {
		t.Fatalf("a non-confirm reply must cancel, got %q", msg)
	}
	assertUnchanged("cancel", msg)
	entry := f.entry("okx-a", PlatformPendingCloseOKXSpot)
	prompt := owner.lastPrompt()
	for _, want := range []string{"`okx-a`", "OKX spot (`okx_spot`)", entry.Episode, "BTC (virtual size at fire 0.500000)", "does NOT read the venue", "Reply `confirm`"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("the confirm prompt must show %q:\n%s", want, prompt)
		}
	}

	f.env.fileFor("okx-a").readOnly = true
	saveLog := ""
	saveLog = captureORCStdout(t, func() {
		msg = owner.run(orcOwnerID, "", orcCommandOpts("okx-a", ""), "confirm", nil)
	})
	if !strings.Contains(msg, "saving the clear failed") || !strings.Contains(msg, "The entry is restored") || !strings.Contains(saveLog, "saving the operator clear failed") {
		t.Fatalf("a failed save must restore and refuse, got %q\n%s", msg, saveLog)
	}
	f.env.fileFor("okx-a").readOnly = false
	if got := f.entry("okx-a", PlatformPendingCloseOKXSpot); got == nil || got.Episode != entry.Episode || !operatorCloseLegsEqual(got.Symbols, entry.Symbols) {
		t.Fatalf("a failed save must restore the exact entry: %+v", got)
	}
	assertUnchanged("save failure", msg)
	f.drainExpect("the reminder continues after a failed clear", 1, 1)
	before = snapshot()

	auditLog := captureORCStdout(t, func() {
		msg = owner.run(orcOwnerID, "", orcCommandOpts("okx-a", PlatformPendingCloseOKXSpot), "confirm", nil)
	})
	if !strings.HasPrefix(msg, "Cleared the operator-required entry for `okx-a` [okx_spot]") {
		t.Fatalf("the confirmed clear must succeed, got %q", msg)
	}
	if !strings.Contains(auditLog, "[CRITICAL] operator-required-close: strategy okx-a platform okx_spot cleared by operator "+orcOwnerID) {
		t.Fatalf("the clear must log the audit line:\n%s", auditLog)
	}
	if f.entry("okx-a", PlatformPendingCloseOKXSpot) != nil || f.storedPendingJSON("okx-a") != "" {
		t.Fatalf("the clear must remove the entry from memory and the database: %q", f.storedPendingJSON("okx-a"))
	}

	msg = owner.run(orcOwnerID, "", orcCommandOpts("rh-opt", ""), "confirm", nil)
	if !strings.HasPrefix(msg, "Cleared the operator-required entry for `rh-opt` [robinhood_options]") {
		t.Fatalf("the options clear must succeed, got %q", msg)
	}
	msg = owner.run(orcOwnerID, "", orcCommandOpts("both", PlatformPendingCloseRobinhoodOptions), "confirm", nil)
	if !strings.HasPrefix(msg, "Cleared") || f.entry("both", PlatformPendingCloseOKXSpot) == nil || f.entry("both", PlatformPendingCloseRobinhoodOptions) != nil {
		t.Fatalf("a named platform must clear only that entry, got %q", msg)
	}
	if got := f.storedPendingJSON("auto-1"); got != before["auto-1"][1] {
		t.Fatalf("automatic entries must stay as they are: %s", got)
	}

	f.restart()
	if f.entry("okx-a", PlatformPendingCloseOKXSpot) != nil || f.entry("rh-opt", PlatformPendingCloseRobinhoodOptions) != nil {
		t.Fatal("cleared entries must stay cleared after a restart")
	}
	f.advance(DefaultAlertThrottleInterval)
	f.drainExpect("drain after the clears lists only the remaining entry", 1, 1)
	if msg := f.rec.lastChannel(); strings.Contains(msg, "okx-a") || strings.Contains(msg, "rh-opt") || !strings.Contains(msg, "both") {
		t.Fatalf("only the remaining entry may be warned:\n%s", msg)
	}
	owner.assertNoLockedPrompt()
	f.assertNoLockedSends()
}

func TestOperatorRequiredCloseConfirmationInterleavings(t *testing.T) {
	t.Run("drain_stamp_during_confirm_still_clears", func(t *testing.T) {
		f := newORCFixture(t, orcOKXSpot("okx-a"))
		f.openSpot("okx-a", 0.5)
		f.fireLossStreak("okx-a")
		c := f.commander(orcOwnerID)
		msg := c.run(orcOwnerID, "", orcCommandOpts("okx-a", ""), "confirm", func() {
			drainOperatorRequiredPendingCloses(f.env.state, f.notifier, &f.env.mu, f.env.store)
		})
		if !strings.HasPrefix(msg, "Cleared") {
			t.Fatalf("a reminder stamp is not a change of scope, got %q", msg)
		}
		if f.entry("okx-a", PlatformPendingCloseOKXSpot) != nil {
			t.Fatal("the entry must be cleared")
		}
		c.assertNoLockedPrompt()
		f.assertNoLockedSends()
	})
	t.Run("identical_leg_fire_during_confirm_refuses", func(t *testing.T) {
		f := newORCFixture(t, orcOKXSpot("okx-a"))
		f.openSpot("okx-a", 0.5)
		f.fireLossStreak("okx-a")
		f.drainExpect("first", 1, 1)
		c := f.commander(orcOwnerID)
		var hookErr error
		msg := c.run(orcOwnerID, "", orcCommandOpts("okx-a", ""), "confirm", func() {
			f.advance(2 * time.Hour)
			f.openSpot("okx-a", 0.5)
			hookErr = f.fireLossStreakErr("okx-a")
		})
		if hookErr != nil {
			t.Fatal(hookErr)
		}
		if !strings.Contains(msg, "changed while you were confirming") {
			t.Fatalf("a new fire during the confirm must refuse, got %q", msg)
		}
		got := f.entry("okx-a", PlatformPendingCloseOKXSpot)
		if got == nil || !got.LastNotifiedAt.IsZero() || !got.FiredAt.Equal(f.clock()) {
			t.Fatalf("the new entry must stay with a zero stamp: %+v", got)
		}
		if f.storedPendingJSON("okx-a") == "" {
			t.Fatal("a refused clear must not remove the stored entry")
		}
		c.assertNoLockedPrompt()
	})
	t.Run("added_leg_during_confirm_refuses", func(t *testing.T) {
		f := newORCFixture(t, orcRobinhoodOptions("rh-opt"))
		f.openOption("rh-opt", "opt-a", 1)
		f.fireLossStreak("rh-opt")
		c := f.commander(orcOwnerID)
		var hookErr error
		msg := c.run(orcOwnerID, "", orcCommandOpts("rh-opt", ""), "confirm", func() {
			f.advance(2 * time.Hour)
			f.openOption("rh-opt", "opt-b", 1)
			hookErr = f.fireLossStreakErr("rh-opt")
		})
		if hookErr != nil {
			t.Fatal(hookErr)
		}
		if !strings.Contains(msg, "changed while you were confirming") {
			t.Fatalf("an added leg during the confirm must refuse, got %q", msg)
		}
		if got := f.entry("rh-opt", PlatformPendingCloseRobinhoodOptions); got == nil || len(got.Symbols) != 2 {
			t.Fatalf("the merged entry must stay: %+v", got)
		}
		c.assertNoLockedPrompt()
	})
	t.Run("kill_switch_clear_during_confirm_refuses", func(t *testing.T) {
		f := newORCFixture(t, orcOKXSpot("okx-a"))
		f.openSpot("okx-a", 0.5)
		f.fireLossStreak("okx-a")
		c := f.commander(orcOwnerID)
		msg := c.run(orcOwnerID, "", orcCommandOpts("okx-a", ""), "confirm", func() {
			f.env.mu.Lock()
			f.env.strat("okx-a").RiskState.clearPendingCircuitClosesForKillSwitch()
			f.env.mu.Unlock()
		})
		if !strings.Contains(msg, "changed while you were confirming") {
			t.Fatalf("a kill-switch clear during the confirm must refuse, got %q", msg)
		}
		c.assertNoLockedPrompt()
	})
	t.Run("another_clear_that_finishes_first_wins", func(t *testing.T) {
		f := newORCFixture(t, orcOKXSpot("okx-a"))
		f.openSpot("okx-a", 0.5)
		f.fireLossStreak("okx-a")
		c := f.commander(orcOwnerID)
		var inner string
		msg := c.run(orcOwnerID, "", orcCommandOpts("okx-a", ""), "confirm", func() {
			ack, prep := prepareOperatorRequiredCloseAck(f.env.state, &f.env.mu, "okx-a", "")
			if ack == nil {
				inner = prep
				return
			}
			inner = applyOperatorRequiredCloseAck(f.env.state, &f.env.mu, f.env.store, "other-session", *ack)
		})
		if !strings.HasPrefix(inner, "Cleared") {
			t.Fatalf("the clear that finished first must succeed, got %q", inner)
		}
		if !strings.Contains(msg, "changed while you were confirming") {
			t.Fatalf("the slower clear must refuse, got %q", msg)
		}
		c.assertNoLockedPrompt()
	})
}
