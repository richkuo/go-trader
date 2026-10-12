package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

const (
	entryTestHLAddress    = "0x0000000000000000000000000000000000c0ffee"
	entryTestHLAccountKey = "hyperliquid:mainnet:" + entryTestHLAddress
	entryTestOwnerID      = "owner-1"
)

func useEntryTestHLAccount(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", entryTestHLAddress)
	t.Setenv("HYPERLIQUID_TESTNET", "")
}

func useEntryTestOKXAccount(t *testing.T) {
	t.Setenv("OKX_API_KEY", "okx-test-key")
	t.Setenv("OKX_SANDBOX", "")
}

func entryTestCoverage() *entryFillCoverage {
	return &entryFillCoverage{Complete: true, FromMs: 1, ThroughMs: time.Now().Add(time.Hour).UnixMilli(), Pages: 1, Rows: 3, Source: "test"}
}

func entryAbsentHL() *entryRecoveryResult {
	return &entryRecoveryResult{Platform: "hyperliquid", Account: entryTestHLAccountKey, CheckedAtMs: time.Now().UnixMilli(),
		Order: &entryRecoveryOrder{Found: false, Coverage: entryTestCoverage()},
		Stop:  &entryRecoveryOrder{Found: false, Coverage: entryTestCoverage()}}
}

func entryAbsentOKX(instType string) *entryRecoveryResult {
	return &entryRecoveryResult{Platform: "okx", Account: okxEntryAccountKey(instType), CheckedAtMs: time.Now().UnixMilli(),
		Order: &entryRecoveryOrder{Found: false, Coverage: entryTestCoverage()}}
}

type entryVenueQuery struct {
	mu      sync.Mutex
	calls   int
	respond func(call int) (*entryRecoveryResult, error)
}

func (q *entryVenueQuery) answer() (*entryRecoveryResult, string, error) {
	q.mu.Lock()
	q.calls++
	call := q.calls
	q.mu.Unlock()
	res, err := q.respond(call)
	return res, "", err
}

func (q *entryVenueQuery) count() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.calls
}

func installEntryVenueQuery(t *testing.T, q *entryVenueQuery) {
	t.Helper()
	prevHL, prevOKX := runHyperliquidRecoverEntryFn, runOKXRecoverEntryFn
	t.Cleanup(func() { runHyperliquidRecoverEntryFn, runOKXRecoverEntryFn = prevHL, prevOKX })
	runHyperliquidRecoverEntryFn = func(script, symbol, cloid, stopCloid string, sinceMs, untilMs int64) (*entryRecoveryResult, string, error) {
		return q.answer()
	}
	runOKXRecoverEntryFn = func(script, symbol, instType, clientOrderID string, sinceMs, untilMs int64) (*entryRecoveryResult, string, error) {
		return q.answer()
	}
}

func fixedVenue(res *entryRecoveryResult, err error) *entryVenueQuery {
	return &entryVenueQuery{respond: func(int) (*entryRecoveryResult, error) { return res, err }}
}

type entryOperatorFixture struct {
	t        *testing.T
	env      *entryTestEnv
	sc       StrategyConfig
	intentID string
}

func entryOKXTestStrategy(id, instType string) StrategyConfig {
	typ := "spot"
	if instType == "swap" {
		typ = "perps"
	}
	return StrategyConfig{ID: id, Type: typ, Platform: "okx", Script: "shared_scripts/check_okx.py",
		Args: []string{"breakout", "BTC", "1h", "--mode=live", "--inst-type=" + instType}, ClosedBarDecisions: true,
		Direction: DirectionLong, Capital: 1000, Leverage: 1, SizingLeverage: 1}
}

func newEntryOperatorFixture(t *testing.T, provider string, extra ...StrategyConfig) *entryOperatorFixture {
	t.Helper()
	releaseHeldLiveEntryAttempts()
	useEntryTestHLAccount(t)
	useEntryTestOKXAccount(t)
	var sc StrategyConfig
	switch provider {
	case "hyperliquid":
		sc = entryTestHLStrategy("hl-live", "live")
		pct := 2.0
		sc.StopLossPct = &pct
	case "okx-spot":
		sc = entryOKXTestStrategy("okx-live", "spot")
	case "okx-swap":
		sc = entryOKXTestStrategy("okx-live", "swap")
	default:
		t.Fatalf("unknown provider %q", provider)
	}
	env := openEntryTestEnv(t, newEntryTestConfig(t, append([]StrategyConfig{sc}, extra...)...))
	useEntryTradeRecorder(t, env.store)
	at := time.Now().Add(-10 * time.Minute)
	bar := at.Truncate(time.Hour).Add(-time.Hour)
	if sc.Platform == "hyperliquid" {
		venue := &entryLiveVenue{t: t, env: env, respond: func([]string) (*HyperliquidExecuteResult, error) { return nil, errors.New("execute error: timeout") }}
		venue.install()
		env.dispatchHLLive(sc, 1, 100, entryTestDecision(bar, at), at)
	} else {
		venue := &entryOKXVenue{res: &OKXExecuteResult{OrderOutcome: "unknown", Error: "order placement failed: network error"}}
		venue.install(t)
		env.dispatchOKX(sc, 1, 100, entryTestDecision(bar, at), at)
	}
	env = env.restart()
	in := env.strat(sc.ID).EntryGuard.unresolvedIntent()
	if in == nil || in.Status != entryIntentSubmitting {
		t.Fatalf("fixture attempt %+v", in)
	}
	return &entryOperatorFixture{t: t, env: env, sc: sc, intentID: in.IntentID}
}

func (f *entryOperatorFixture) intent() *entryIntent {
	f.env.mu.RLock()
	defer f.env.mu.RUnlock()
	return f.env.strat(f.sc.ID).EntryGuard.Intents[f.intentID]
}

func (f *entryOperatorFixture) rewrite(mutate func(in *entryIntent, req *entryIntentRequest)) {
	f.t.Helper()
	f.env.mu.Lock()
	in := f.env.strat(f.sc.ID).EntryGuard.Intents[f.intentID]
	req, err := parseEntryIntentRequest(in)
	if err != nil {
		f.env.mu.Unlock()
		f.t.Fatal(err)
	}
	mutate(in, &req)
	if in.RequestJSON != "" && !strings.Contains(in.RequestJSON, `"version":99`) {
		in.RequestJSON = marshalEntryJSON(req)
	}
	snap := *in
	f.env.mu.Unlock()
	db := f.env.fileFor(f.sc.ID)
	if _, err := db.db.Exec(`UPDATE entry_order_intents SET request_json = ?, evidence_json = ?, venue_order_id = ?, account_key = ?,
		confirmed_qty = ?, confirmed_notional = ?, confirmed_fee = ? WHERE intent_id = ?`,
		snap.RequestJSON, snap.EvidenceJSON, snap.VenueOrderID, snap.AccountKey, snap.ConfirmedQty, snap.ConfirmedNotional, snap.ConfirmedFee, snap.IntentID); err != nil {
		f.t.Fatal(err)
	}
}

func (f *entryOperatorFixture) backdate() {
	f.t.Helper()
	now := time.Now()
	f.rewrite(func(in *entryIntent, req *entryIntentRequest) {
		req.ExpiresAfterMs = now.Add(-10 * time.Minute).UnixMilli()
		if in.InitialStopClientID != "" {
			req.StopExpiresAfterMs = now.Add(-9 * time.Minute).UnixMilli()
		}
		req.SubmittedAtMs = req.ExpiresAfterMs - hlEntryOrderExpiry.Milliseconds()
	})
}

func (f *entryOperatorFixture) notifier() *DiscordNotifier {
	return &DiscordNotifier{ownerID: entryTestOwnerID, ss: &StatusServer{state: f.env.state, mu: &f.env.mu, stateDB: f.env.store, strategies: f.env.cfg.Strategies}}
}

func entryCommandOpts(strategy, attempt string) []*discordgo.ApplicationCommandInteractionDataOption {
	return []*discordgo.ApplicationCommandInteractionDataOption{
		{Name: "strategy", Type: discordgo.ApplicationCommandOptionString, Value: strategy},
		{Name: "attempt", Type: discordgo.ApplicationCommandOptionString, Value: attempt},
	}
}

func (f *entryOperatorFixture) command() string {
	return f.notifier().resolveEntryCommand(entryTestOwnerID, "", entryCommandOpts(f.sc.ID, f.intentID))
}

type entryRowState struct {
	Status   string
	Evidence string
	Hold     string
	Request  string
}

func (f *entryOperatorFixture) row() entryRowState {
	f.t.Helper()
	var r entryRowState
	if err := f.env.fileFor(f.sc.ID).db.QueryRow(`SELECT status, evidence_json, hold_reason, request_json FROM entry_order_intents WHERE intent_id = ?`, f.intentID).Scan(&r.Status, &r.Evidence, &r.Hold, &r.Request); err != nil {
		f.t.Fatal(err)
	}
	return r
}

func (f *entryOperatorFixture) cash() float64 {
	f.env.mu.RLock()
	defer f.env.mu.RUnlock()
	return f.env.strat(f.sc.ID).Cash
}

func (f *entryOperatorFixture) admitNextCandle() entryAdmission {
	at := time.Now()
	bar := at.Truncate(time.Hour).Add(-time.Hour)
	return admitClosedBarEntry(f.sc, f.env.strat(f.sc.ID), &f.env.mu, "BTC", entryTestDecision(bar, at.Add(-time.Second)), 1, 0, 0, "", at)
}

func (f *entryOperatorFixture) assertUnchanged(msg string, cashBefore float64) {
	f.t.Helper()
	if !strings.HasPrefix(msg, "Refused: ") && !strings.Contains(msg, "not authorized") && !strings.Contains(msg, "only available in a DM") && !strings.Contains(msg, "ops commands are disabled") {
		f.t.Fatalf("expected a refusal, got %q", msg)
	}
	if r := f.row(); r.Status != entryIntentSubmitting {
		f.t.Fatalf("a refused command changed the attempt to %s (%q)", r.Status, msg)
	}
	if in := f.intent(); in == nil || in.Status != entryIntentSubmitting {
		f.t.Fatalf("a refused command dropped the in-memory hold (%q)", msg)
	}
	if adm := f.admitNextCandle(); !adm.Held {
		f.t.Fatalf("a refused command let a new entry through (%q)", msg)
	}
	if got := f.env.consumedBar(f.sc.ID, "BTC", "long"); got != 0 {
		f.t.Fatalf("a refused command moved the consumed candle to %d", got)
	}
	if n := f.env.openTradeRows(f.sc.ID); n != 0 {
		f.t.Fatalf("a refused command booked %d trades", n)
	}
	if qty, _ := f.env.position(f.sc.ID, "BTC"); qty != 0 {
		f.t.Fatalf("a refused command booked a position of %v", qty)
	}
	if c := f.cash(); c != cashBefore {
		f.t.Fatalf("a refused command moved cash %v -> %v", cashBefore, c)
	}
}

func TestEntryOperatorReleasesAVerifiedAbsentAttemptExactlyOnce(t *testing.T) {
	for _, provider := range []string{"hyperliquid", "okx-spot", "okx-swap"} {
		t.Run(provider, func(t *testing.T) {
			f := newEntryOperatorFixture(t, provider)
			f.backdate()
			res := entryAbsentHL()
			if f.sc.Platform == "okx" {
				res = entryAbsentOKX(okxInstType(f.sc.Args))
			}
			q := fixedVenue(res, nil)
			installEntryVenueQuery(t, q)
			cashBefore := f.cash()

			msg := f.command()
			if !strings.HasPrefix(msg, "Released attempt "+f.intentID) {
				t.Fatalf("release reply %q", msg)
			}
			if q.count() != 1 {
				t.Fatalf("venue queries %d, want 1", q.count())
			}
			r := f.row()
			if r.Status != entryIntentNoFill || r.Hold != "" {
				t.Fatalf("row after release %+v", r)
			}
			var ev entryIntentEvidence
			if err := json.Unmarshal([]byte(r.Evidence), &ev); err != nil {
				t.Fatal(err)
			}
			if ev.Outcome != "operator_verified_absent" || ev.Operator == nil || ev.Operator.OperatorID != entryTestOwnerID || ev.Operator.PriorStatus != entryIntentSubmitting || ev.Operator.PriorHoldReason == "" {
				t.Fatalf("operator audit %+v", ev.Operator)
			}
			if ev.Absence == nil || ev.Absence.Account != res.Account || !ev.Absence.OrderCoverage.Complete || ev.Absence.ClientOrderID == "" || ev.Absence.EntryDeadlineMs <= 0 || ev.Absence.ReleasableAfterMs <= ev.Absence.EntryDeadlineMs {
				t.Fatalf("absence audit %+v", ev.Absence)
			}
			if (f.sc.Platform == "hyperliquid") != (ev.Absence.StopCoverage != nil && ev.Absence.InitialStopClientID != "" && ev.Absence.StopDeadlineMs > 0) {
				t.Fatalf("initial-stop audit %+v", ev.Absence)
			}
			if f.intent() != nil {
				t.Fatal("the in-memory hold stayed after the release committed")
			}
			if adm := f.admitNextCandle(); !adm.admitted() {
				t.Fatalf("a later valid closed-bar entry is still held: %q", adm.HoldReason)
			}
			if got := f.env.consumedBar(f.sc.ID, "BTC", "long"); got != 0 {
				t.Fatalf("the release moved the consumed candle to %d", got)
			}
			if n := f.env.openTradeRows(f.sc.ID); n != 0 || f.cash() != cashBefore {
				t.Fatalf("the release booked trades=%d cash %v -> %v", n, cashBefore, f.cash())
			}

			again := f.command()
			if !strings.Contains(again, "already resolved (no_fill)") || q.count() != 1 {
				t.Fatalf("duplicate request: %q (queries %d)", again, q.count())
			}
			if r2 := f.row(); r2 != r {
				t.Fatalf("the duplicate request changed the row: %+v -> %+v", r, r2)
			}

			f.env = f.env.restart()
			if in := f.env.strat(f.sc.ID).EntryGuard.unresolvedIntent(); in != nil {
				t.Fatalf("the released attempt is held again after restart: %s", in.IntentID)
			}
			if r3 := f.row(); r3 != r {
				t.Fatalf("the audit changed across restart: %+v", r3)
			}
			if adm := f.admitNextCandle(); !adm.admitted() {
				t.Fatalf("after restart a later valid entry is still held: %q", adm.HoldReason)
			}
			if n := f.env.openTradeRows(f.sc.ID); n != 0 {
				t.Fatalf("restart found %d trades", n)
			}
			if again := f.command(); !strings.Contains(again, "already resolved (no_fill)") {
				t.Fatalf("request after restart: %q", again)
			}
		})
	}
}

func TestEntryOperatorQueriesFillHistoryOnlyThroughTheReleaseDeadline(t *testing.T) {
	for _, provider := range []string{"hyperliquid", "okx-swap"} {
		t.Run(provider, func(t *testing.T) {
			f := newEntryOperatorFixture(t, provider)
			f.backdate()
			in := f.intent()
			req, err := parseEntryIntentRequest(in)
			if err != nil {
				t.Fatal(err)
			}
			wantUntil, why := entryReleasableAfterMs(in, req)
			if why != "" {
				t.Fatal(why)
			}
			wantSince := min(entryRecoveryWindowStart(in), req.SubmittedAtMs)
			var gotSince, gotUntil int64
			answer := func(sinceMs, untilMs int64) (*entryRecoveryResult, string, error) {
				gotSince, gotUntil = sinceMs, untilMs
				res := entryAbsentHL()
				if f.sc.Platform == "okx" {
					res = entryAbsentOKX(okxInstType(f.sc.Args))
				}
				for _, o := range []*entryRecoveryOrder{res.Order, res.Stop} {
					if o != nil {
						o.Coverage.FromMs = sinceMs
						o.Coverage.ThroughMs = untilMs
					}
				}
				return res, "", nil
			}
			prevHL, prevOKX := runHyperliquidRecoverEntryFn, runOKXRecoverEntryFn
			t.Cleanup(func() { runHyperliquidRecoverEntryFn, runOKXRecoverEntryFn = prevHL, prevOKX })
			runHyperliquidRecoverEntryFn = func(script, symbol, cloid, stopCloid string, sinceMs, untilMs int64) (*entryRecoveryResult, string, error) {
				return answer(sinceMs, untilMs)
			}
			runOKXRecoverEntryFn = func(script, symbol, instType, clientOrderID string, sinceMs, untilMs int64) (*entryRecoveryResult, string, error) {
				return answer(sinceMs, untilMs)
			}

			msg := f.command()
			if gotUntil != wantUntil || gotSince != wantSince {
				t.Fatalf("query window %d..%d, want %d..%d", gotSince, gotUntil, wantSince, wantUntil)
			}
			if !strings.HasPrefix(msg, "Released attempt "+f.intentID) {
				t.Fatalf("coverage through exactly the release deadline must release: %q", msg)
			}
			if r := f.row(); r.Status != entryIntentNoFill {
				t.Fatalf("row after release %+v", r)
			}
		})
	}
}

func TestEntryRecoveryAndCommandHoldOnAClientIDFillBeforeTheReleaseDeadline(t *testing.T) {
	f := newEntryOperatorFixture(t, "hyperliquid")
	f.backdate()
	in := f.intent()
	req, err := parseEntryIntentRequest(in)
	if err != nil {
		t.Fatal(err)
	}
	until, _ := entryReleasableAfterMs(in, req)
	res := entryAbsentHL()
	res.Order.Coverage.ThroughMs = until
	res.Stop.Coverage.ThroughMs = until
	res.Order.Coverage.MatchedCount, res.Order.Coverage.MatchedQty = 1, 0.1
	installEntryVenueQuery(t, fixedVenue(res, nil))
	cashBefore := f.cash()
	recoverEntryIntents(f.env.cfg, f.env.state, f.env.store, &f.env.mu, nil, nil, time.Now())
	if r := f.row(); r.Status != entryIntentSubmitting {
		t.Fatalf("recovery changed the attempt to %s", r.Status)
	}
	msg := f.command()
	if !strings.Contains(msg, "holds 1 fill(s)") {
		t.Fatalf("reply %q", msg)
	}
	f.assertUnchanged(msg, cashBefore)
}

func TestEntryRecoveryOKXFillHistoryFailureOnlyBlocksTheRelease(t *testing.T) {
	failed := func() *entryFillCoverage {
		return &entryFillCoverage{Complete: false, Reason: "the fill history query failed: OKX fills query failed: {'code': '50011'}", Source: "trade/fills"}
	}
	t.Run("a found order with complete trades is booked once", func(t *testing.T) {
		f := newEntryOperatorFixture(t, "okx-swap")
		f.backdate()
		res := &entryRecoveryResult{Platform: "okx", Account: okxEntryAccountKey("swap"), Order: &entryRecoveryOrder{Found: true, Status: "closed", OrderID: "okx-1", ExecutedSz: 0.3, Terminal: true,
			FillQty: 0.3, FillNotional: 30.3, FillFee: 0.03, FillCount: 1, FillAvgPx: 101, FillsComplete: true, Coverage: failed()}}
		installEntryVenueQuery(t, fixedVenue(res, nil))
		recoverEntryIntents(f.env.cfg, f.env.state, f.env.store, &f.env.mu, nil, nil, time.Now())
		recoverEntryIntents(f.env.cfg, f.env.state, f.env.store, &f.env.mu, nil, nil, time.Now().Add(2*entryRecoveryQueryInterval))
		if qty, _ := f.env.position(f.sc.ID, "BTC"); qty != 0.3 {
			t.Fatalf("position %v, want the proven 0.3 booked", qty)
		}
		if n := f.env.openTradeRows(f.sc.ID); n != 1 {
			t.Fatalf("open trade rows %d, want 1", n)
		}
		if got := f.env.intentStatuses(f.sc.ID); len(got) != 1 || got[0] != entryIntentCommitted {
			t.Fatalf("intents %v", got)
		}
	})
	t.Run("a terminal zero-fill order with a failed fill history stays held", func(t *testing.T) {
		f := newEntryOperatorFixture(t, "okx-swap")
		f.backdate()
		cov := failed()
		cov.QueryError = "OKX fills query failed: {'code': '50011'}"
		res := &entryRecoveryResult{Platform: "okx", Account: okxEntryAccountKey("swap"), Order: &entryRecoveryOrder{Found: true, Status: "canceled", OrderID: "okx-1", Terminal: true, Coverage: cov}}
		installEntryVenueQuery(t, fixedVenue(res, nil))
		recoverEntryIntents(f.env.cfg, f.env.state, f.env.store, &f.env.mu, nil, nil, time.Now())
		if r := f.row(); r.Status != entryIntentSubmitting || !strings.Contains(r.Hold, "fill history query failed") {
			t.Fatalf("row %+v", r)
		}
		cov.QueryError = ""
		recoverEntryIntents(f.env.cfg, f.env.state, f.env.store, &f.env.mu, nil, nil, time.Now().Add(2*entryRecoveryQueryInterval))
		if got := f.env.intentStatuses(f.sc.ID); len(got) != 1 || got[0] != entryIntentNoFill {
			t.Fatalf("after the fill history recovers: intents %v", got)
		}
	})
	t.Run("a missing order with a failed fill history stays held", func(t *testing.T) {
		f := newEntryOperatorFixture(t, "okx-swap")
		f.backdate()
		res := entryAbsentOKX("swap")
		res.Order.Coverage = failed()
		installEntryVenueQuery(t, fixedVenue(res, nil))
		cashBefore := f.cash()
		recoverEntryIntents(f.env.cfg, f.env.state, f.env.store, &f.env.mu, nil, nil, time.Now())
		if r := f.row(); r.Status != entryIntentSubmitting {
			t.Fatalf("recovery changed the attempt to %s", r.Status)
		}
		msg := f.command()
		if !strings.Contains(msg, "fill history query failed") {
			t.Fatalf("reply %q", msg)
		}
		f.assertUnchanged(msg, cashBefore)
	})
}

func TestEntryOperatorAuthorizationAndOwnershipRefuseBeforeMutation(t *testing.T) {
	paper := entryTestHLStrategy("hl-paper", "paper")
	other := entryTestHLStrategy("hl-other", "live")
	f := newEntryOperatorFixture(t, "hyperliquid", paper, other)
	f.backdate()
	q := fixedVenue(entryAbsentHL(), nil)
	installEntryVenueQuery(t, q)
	cashBefore := f.cash()
	d := f.notifier()
	opts := entryCommandOpts(f.sc.ID, f.intentID)

	f.assertUnchanged(d.resolveEntryCommand("intruder", "", opts), cashBefore)
	f.assertUnchanged(d.resolveEntryCommand(entryTestOwnerID, "guild-1", opts), cashBefore)
	f.assertUnchanged((&DiscordNotifier{ss: d.ss}).resolveEntryCommand(entryTestOwnerID, "", opts), cashBefore)
	for _, tc := range []struct {
		strategy, attempt, want string
	}{
		{"ghost", f.intentID, "not configured in this instance"},
		{paper.ID, f.intentID, "is not live"},
		{other.ID, f.intentID, "cannot be read for strategy hl-other"},
		{f.sc.ID, "ei-zz", "is not a live entry attempt id"},
		{f.sc.ID, "ei-000000000000000000000000", "does not exist"},
	} {
		msg := d.resolveEntryCommand(entryTestOwnerID, "", entryCommandOpts(tc.strategy, tc.attempt))
		if !strings.Contains(msg, tc.want) {
			t.Fatalf("%s/%s: reply %q, want %q", tc.strategy, tc.attempt, msg, tc.want)
		}
		f.assertUnchanged(msg, cashBefore)
	}
	out := resolveEntryAttemptByOperator(entryOperatorDeps{state: f.env.state, mu: &f.env.mu, strategy: d.ss.strategyConfig}, entryTestOwnerID, f.sc.ID, f.intentID)
	if !strings.Contains(out.Message, "state store is unavailable") {
		t.Fatalf("unavailable storage: %q", out.Message)
	}
	f.assertUnchanged(out.Message, cashBefore)
	unready := &DiscordNotifier{ownerID: entryTestOwnerID, ss: &StatusServer{state: f.env.state, mu: &f.env.mu, strategies: f.env.cfg.Strategies}}
	unreadyMsg := unready.resolveEntryCommand(entryTestOwnerID, "", opts)
	if unreadyMsg != "status server not ready" {
		t.Fatalf("unready status server: %q", unreadyMsg)
	}
	f.assertUnchanged("Refused: "+unreadyMsg, cashBefore)
	if q.count() != 0 {
		t.Fatalf("a refused request reached the venue %d time(s)", q.count())
	}
	if msg := f.command(); !strings.HasPrefix(msg, "Released attempt") {
		t.Fatalf("the owner's own request was refused: %q", msg)
	}
}

func TestEntryOperatorRefusesEveryUnprovenOrContradictedState(t *testing.T) {
	found := func(r *entryRecoveryResult) *entryRecoveryResult {
		r.Order = &entryRecoveryOrder{Found: true, Status: "canceled", OrderID: "901", Terminal: true, Coverage: entryTestCoverage()}
		return r
	}
	cases := []struct {
		name     string
		provider string
		prepare  func(f *entryOperatorFixture)
		res      func() *entryRecoveryResult
		err      error
		want     string
		queries  int
	}{
		{"entry order exists", "hyperliquid", nil, func() *entryRecoveryResult { return found(entryAbsentHL()) }, nil, "entry order exists at the venue", 1},
		{"open entry order", "okx-swap", nil, func() *entryRecoveryResult {
			r := entryAbsentOKX("swap")
			r.Order = &entryRecoveryOrder{Found: true, Status: "open", OrderID: "o-1", Coverage: entryTestCoverage()}
			return r
		}, nil, "entry order exists at the venue", 1},
		{"client-id fill without an order", "okx-spot", nil, func() *entryRecoveryResult {
			r := entryAbsentOKX("spot")
			r.Order.FillQty, r.Order.FillCount = 0.1, 1
			return r
		}, nil, "reports 1 fill(s)", 1},
		{"fill history matches the client id", "hyperliquid", nil, func() *entryRecoveryResult {
			r := entryAbsentHL()
			r.Order.Coverage.MatchedCount, r.Order.Coverage.MatchedQty = 1, 0.2
			return r
		}, nil, "holds 1 fill(s)", 1},
		{"initial stop order exists", "hyperliquid", nil, func() *entryRecoveryResult {
			r := entryAbsentHL()
			r.Stop = &entryRecoveryOrder{Found: true, Status: "canceled", OrderID: "902", Terminal: true, Coverage: entryTestCoverage()}
			return r
		}, nil, "initial stop exists at the venue", 1},
		{"initial stop fill", "hyperliquid", nil, func() *entryRecoveryResult {
			r := entryAbsentHL()
			r.Stop.FillQty, r.Stop.FillCount = 0.4, 1
			return r
		}, nil, "for the initial stop client id", 1},
		{"no initial stop evidence", "hyperliquid", nil, func() *entryRecoveryResult {
			r := entryAbsentHL()
			r.Stop = nil
			return r
		}, nil, "initial stop lookup returned no evidence", 1},
		{"missing order default zero fill without coverage", "okx-swap", nil, func() *entryRecoveryResult {
			return &entryRecoveryResult{Platform: "okx", Account: okxEntryAccountKey("swap"), Order: &entryRecoveryOrder{Found: false}}
		}, nil, "returned no fill-history coverage", 1},
		{"partial history", "hyperliquid", nil, func() *entryRecoveryResult {
			r := entryAbsentHL()
			r.Order.Coverage.Complete, r.Order.Coverage.Reason = false, "more than 40 fill orders need a client id lookup"
			return r
		}, nil, "is incomplete: more than 40", 1},
		{"history starts after the window", "okx-spot", nil, func() *entryRecoveryResult {
			r := entryAbsentOKX("spot")
			r.Order.Coverage.FromMs = time.Now().UnixMilli()
			return r
		}, nil, "after the attempt window start", 1},
		{"history ends before the deadline", "okx-swap", nil, func() *entryRecoveryResult {
			r := entryAbsentOKX("swap")
			r.Order.Coverage.ThroughMs = 2
			return r
		}, nil, "before the release deadline", 1},
		{"query against another account", "hyperliquid", nil, func() *entryRecoveryResult {
			r := entryAbsentHL()
			r.Account = "hyperliquid:mainnet:0x00000000000000000000000000000000deadbeef"
			return r
		}, nil, "ran against account", 1},
		{"query error", "hyperliquid", nil, func() *entryRecoveryResult { return nil }, errors.New("recovery query error: exit status 1"), "venue query failed", 1},
		{"result with an error", "okx-swap", nil, func() *entryRecoveryResult { return entryAbsentOKX("swap") }, errors.New("recovery query error: exit status 1 (fills page 3 failed)"), "venue query failed", 1},
		{"timeout", "hyperliquid", nil, func() *entryRecoveryResult { return entryAbsentHL() }, context.DeadlineExceeded, "venue query failed", 1},
		{"venue error field", "okx-spot", nil, func() *entryRecoveryResult {
			r := entryAbsentOKX("spot")
			r.Error = "entry recovery query failed: 50011 rate limit"
			return r
		}, nil, "50011 rate limit", 1},
		{"saved confirmed fill", "hyperliquid", func(f *entryOperatorFixture) {
			f.rewrite(func(in *entryIntent, _ *entryIntentRequest) { in.ConfirmedQty, in.ConfirmedNotional = 0.2, 20 })
		}, entryAbsentHL, nil, "saved confirmed fill", 0},
		{"saved venue order", "okx-swap", func(f *entryOperatorFixture) {
			f.rewrite(func(in *entryIntent, _ *entryIntentRequest) { in.VenueOrderID = "o-7" })
		}, func() *entryRecoveryResult { return entryAbsentOKX("swap") }, nil, "saved venue order o-7", 0},
		{"saved unbooked fill evidence", "hyperliquid", func(f *entryOperatorFixture) {
			f.rewrite(func(in *entryIntent, _ *entryIntentRequest) {
				in.EvidenceJSON = marshalEntryJSON(entryIntentEvidence{Outcome: "filled_unbooked", FilledQty: 0.3})
			})
		}, entryAbsentHL, nil, "saved evidence of a 0.30000000 fill", 0},
		{"deadline not passed", "hyperliquid", func(f *entryOperatorFixture) {
			f.rewrite(func(_ *entryIntent, req *entryIntentRequest) {
				req.ExpiresAfterMs = time.Now().Add(time.Minute).UnixMilli()
				req.StopExpiresAfterMs = time.Now().Add(2 * time.Minute).UnixMilli()
				req.SubmittedAtMs = time.Now().Add(-time.Minute).UnixMilli()
			})
		}, entryAbsentHL, nil, "deadline plus grace has not passed", 0},
		{"stop deadline not passed", "hyperliquid", func(f *entryOperatorFixture) {
			f.rewrite(func(_ *entryIntent, req *entryIntentRequest) {
				req.StopExpiresAfterMs = time.Now().Add(-time.Minute).UnixMilli()
			})
		}, entryAbsentHL, nil, "deadline plus grace has not passed", 0},
		{"missing stop deadline", "hyperliquid", func(f *entryOperatorFixture) {
			f.rewrite(func(_ *entryIntent, req *entryIntentRequest) { req.StopExpiresAfterMs = 0 })
		}, entryAbsentHL, nil, "no initial-stop deadline", 0},
		{"missing entry deadline", "okx-spot", func(f *entryOperatorFixture) {
			f.rewrite(func(_ *entryIntent, req *entryIntentRequest) { req.ExpiresAfterMs = 0 })
		}, func() *entryRecoveryResult { return entryAbsentOKX("spot") }, nil, "no venue deadline", 0},
		{"malformed request", "okx-swap", func(f *entryOperatorFixture) {
			f.rewrite(func(in *entryIntent, _ *entryIntentRequest) { in.RequestJSON = `{"version":99}` })
		}, func() *entryRecoveryResult { return entryAbsentOKX("swap") }, nil, "version 99 is not supported", 0},
		{"legacy account binding", "okx-swap", func(f *entryOperatorFixture) {
			f.rewrite(func(in *entryIntent, _ *entryIntentRequest) { in.AccountKey = "okx:swap" })
		}, func() *entryRecoveryResult { return entryAbsentOKX("swap") }, nil, "no saved venue account binding", 0},
		{"missing account binding", "hyperliquid", func(f *entryOperatorFixture) {
			f.rewrite(func(in *entryIntent, _ *entryIntentRequest) { in.AccountKey = "" })
		}, entryAbsentHL, nil, "no saved venue account binding", 0},
		{"configured account changed", "hyperliquid", func(f *entryOperatorFixture) {
			f.t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0x00000000000000000000000000000000deadbeef")
		}, entryAbsentHL, nil, "configured venue account differs", 0},
		{"configured network changed", "okx-spot", func(f *entryOperatorFixture) {
			f.t.Setenv("OKX_SANDBOX", "1")
		}, func() *entryRecoveryResult { return entryAbsentOKX("spot") }, nil, "configured venue account differs", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newEntryOperatorFixture(t, tc.provider)
			f.backdate()
			if tc.prepare != nil {
				tc.prepare(f)
			}
			q := fixedVenue(nil, tc.err)
			q.respond = func(int) (*entryRecoveryResult, error) { return tc.res(), tc.err }
			installEntryVenueQuery(t, q)
			cashBefore := f.cash()
			msg := f.command()
			if !strings.Contains(msg, tc.want) {
				t.Fatalf("reply %q, want %q", msg, tc.want)
			}
			if q.count() != tc.queries {
				t.Fatalf("venue queries %d, want %d", q.count(), tc.queries)
			}
			f.assertUnchanged(msg, cashBefore)
		})
	}
}

func TestEntryOperatorRefusesSettlingAndReservedAttempts(t *testing.T) {
	f := newEntryOperatorFixture(t, "hyperliquid")
	f.backdate()
	partial := &entryRecoveryResult{Platform: "hyperliquid", Account: entryTestHLAccountKey,
		Order: &entryRecoveryOrder{Found: true, Status: "open", OrderID: "9", OrigSz: 0.4, ExecutedSz: 0.1, FillQty: 0.1, FillAvgPx: 100, FillCount: 1, FillsComplete: true, Coverage: entryTestCoverage()}}
	q := fixedVenue(partial, nil)
	installEntryVenueQuery(t, q)
	recoverEntryIntents(f.env.cfg, f.env.state, f.env.store, &f.env.mu, nil, nil, time.Now())
	if r := f.row(); r.Status != entryIntentSettling {
		t.Fatalf("fixture status %s", r.Status)
	}
	consumed := f.env.consumedBar(f.sc.ID, "BTC", "long")
	if consumed == 0 {
		t.Fatal("a known partial fill must consume its candle")
	}
	q.respond = func(int) (*entryRecoveryResult, error) { return entryAbsentHL(), nil }
	before := q.count()
	msg := f.command()
	if !strings.Contains(msg, "is settling because a fill was proven") || q.count() != before {
		t.Fatalf("settling attempt: %q (queries %d -> %d)", msg, before, q.count())
	}
	if r := f.row(); r.Status != entryIntentSettling || f.env.consumedBar(f.sc.ID, "BTC", "long") != consumed {
		t.Fatalf("the refused command changed a settling attempt: %+v", r)
	}

	sc := entryTestHLStrategy("hl-reserved", "live")
	env := openEntryTestEnv(t, newEntryTestConfig(t, sc))
	at := entryTestBar(1).Add(10 * time.Second)
	adm := admitClosedBarEntry(sc, env.strat(sc.ID), &env.mu, "BTC", entryTestDecision(entryTestBar(0), at), 1, 0, 0, "", at)
	in, err := reserveLiveEntryIntent(env.store, sc, env.strat(sc.ID), &env.mu, adm, entryProviderHyperliquid, entryIntentRequest{Kind: "open", Signal: 1, Price: 100}, at)
	if err != nil {
		t.Fatal(err)
	}
	releaseLiveEntryAttempt(in)
	d := &DiscordNotifier{ownerID: entryTestOwnerID, ss: &StatusServer{state: env.state, mu: &env.mu, stateDB: env.store, strategies: env.cfg.Strategies}}
	if msg := d.resolveEntryCommand(entryTestOwnerID, "", entryCommandOpts(sc.ID, in.IntentID)); !strings.Contains(msg, "was reserved but never submitted") {
		t.Fatalf("reserved attempt: %q", msg)
	}
	if got := env.intentStatuses(sc.ID); len(got) != 1 || got[0] != entryIntentReserved {
		t.Fatalf("the refused command changed a reserved attempt: %v", got)
	}
}

type blockingVenue struct {
	q       *entryVenueQuery
	entered chan int
	proceed chan struct{}
}

func newBlockingVenue(t *testing.T, respond func(call int) (*entryRecoveryResult, error), blockCalls ...int) *blockingVenue {
	b := &blockingVenue{entered: make(chan int, 8), proceed: make(chan struct{})}
	blocks := map[int]bool{}
	for _, c := range blockCalls {
		blocks[c] = true
	}
	b.q = &entryVenueQuery{respond: func(call int) (*entryRecoveryResult, error) {
		if blocks[call] {
			b.entered <- call
			<-b.proceed
		}
		return respond(call)
	}}
	installEntryVenueQuery(t, b.q)
	return b
}

func waitEntered(t *testing.T, b *blockingVenue) {
	t.Helper()
	select {
	case <-b.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the venue query was never reached")
	}
}

func TestEntryOperatorSerializesWithRecoveryAndOtherRequests(t *testing.T) {
	filled := func() *entryRecoveryResult {
		return &entryRecoveryResult{Platform: "hyperliquid", Account: entryTestHLAccountKey,
			Order: &entryRecoveryOrder{Found: true, Status: "filled", OrderID: "901", OrigSz: 0.4, ExecutedSz: 0.4, Terminal: true, FillQty: 0.4, FillNotional: 40.2, FillFee: 0.018, FillCount: 2, FillAvgPx: 100.5, FillsComplete: true, Coverage: entryTestCoverage()},
			Stop:  &entryRecoveryOrder{Found: true, Status: "open", OrderID: "902", Terminal: false, TriggerPx: 98.5, Coverage: entryTestCoverage()}}
	}

	t.Run("release first; recovery and a duplicate request wait their turn", func(t *testing.T) {
		f := newEntryOperatorFixture(t, "hyperliquid")
		f.backdate()
		b := newBlockingVenue(t, func(int) (*entryRecoveryResult, error) { return entryAbsentHL(), nil }, 1)
		done := make(chan string, 1)
		go func() { done <- f.command() }()
		waitEntered(t, b)
		recoverEntryIntents(f.env.cfg, f.env.state, f.env.store, &f.env.mu, nil, nil, time.Now().Add(time.Hour))
		if dup := f.command(); !strings.Contains(dup, "in progress") {
			t.Fatalf("a duplicate request during the check: %q", dup)
		}
		if b.q.count() != 1 {
			t.Fatalf("recovery or the duplicate queried the venue during the operator check (%d calls)", b.q.count())
		}
		close(b.proceed)
		if msg := <-done; !strings.HasPrefix(msg, "Released attempt") {
			t.Fatalf("release: %q", msg)
		}
		recoverEntryIntents(f.env.cfg, f.env.state, f.env.store, &f.env.mu, nil, nil, time.Now().Add(2*time.Hour))
		if got := f.env.intentStatuses(f.sc.ID); len(got) != 1 || got[0] != entryIntentNoFill || b.q.count() != 1 {
			t.Fatalf("after release: intents %v queries %d", got, b.q.count())
		}
		if n := f.env.openTradeRows(f.sc.ID); n != 0 {
			t.Fatalf("booked %d trades", n)
		}
	})

	t.Run("recovery first books the fill; the command then reports it resolved", func(t *testing.T) {
		f := newEntryOperatorFixture(t, "hyperliquid")
		f.backdate()
		b := newBlockingVenue(t, func(int) (*entryRecoveryResult, error) { return filled(), nil }, 1)
		done := make(chan struct{})
		go func() {
			recoverEntryIntents(f.env.cfg, f.env.state, f.env.store, &f.env.mu, nil, nil, time.Now())
			close(done)
		}()
		waitEntered(t, b)
		if msg := f.command(); !strings.Contains(msg, "in progress") {
			t.Fatalf("command during recovery: %q", msg)
		}
		close(b.proceed)
		<-done
		if got := f.env.intentStatuses(f.sc.ID); len(got) != 1 || got[0] != entryIntentCommitted {
			t.Fatalf("recovery result %v", got)
		}
		if msg := f.command(); !strings.Contains(msg, "already resolved (committed)") {
			t.Fatalf("command after recovery: %q", msg)
		}
		if n := f.env.openTradeRows(f.sc.ID); n != 1 || b.q.count() != 1 {
			t.Fatalf("trades %d queries %d", n, b.q.count())
		}
	})

	t.Run("evidence changed in memory during the operator query", func(t *testing.T) {
		f := newEntryOperatorFixture(t, "hyperliquid")
		f.backdate()
		b := newBlockingVenue(t, func(int) (*entryRecoveryResult, error) { return entryAbsentHL(), nil }, 1)
		done := make(chan string, 1)
		go func() { done <- f.command() }()
		waitEntered(t, b)
		f.env.mu.Lock()
		f.env.strat(f.sc.ID).EntryGuard.Intents[f.intentID].ConfirmedQty = 0.1
		f.env.mu.Unlock()
		close(b.proceed)
		msg := <-done
		if !strings.Contains(msg, "the attempt changed during the venue query") {
			t.Fatalf("reply %q", msg)
		}
		if r := f.row(); r.Status != entryIntentSubmitting {
			t.Fatalf("row %+v", r)
		}
		if adm := f.admitNextCandle(); !adm.Held {
			t.Fatal("the hold was dropped")
		}
	})

	t.Run("evidence changed in the state file during the operator query", func(t *testing.T) {
		f := newEntryOperatorFixture(t, "okx-swap")
		f.backdate()
		b := newBlockingVenue(t, func(int) (*entryRecoveryResult, error) { return entryAbsentOKX("swap"), nil }, 1)
		done := make(chan string, 1)
		go func() { done <- f.command() }()
		waitEntered(t, b)
		if _, err := f.env.fileFor(f.sc.ID).db.Exec(`UPDATE entry_order_intents SET confirmed_qty = 0.2 WHERE intent_id = ?`, f.intentID); err != nil {
			t.Fatal(err)
		}
		close(b.proceed)
		msg := <-done
		if !strings.Contains(msg, "saving the release failed") || !strings.Contains(msg, errEntryOperatorReleaseStale.Error()) {
			t.Fatalf("reply %q", msg)
		}
		if r := f.row(); r.Status != entryIntentSubmitting {
			t.Fatalf("row %+v", r)
		}
		if f.intent() == nil {
			t.Fatal("the in-memory hold was dropped after a refused commit")
		}
	})

	t.Run("a stale recovery snapshot cannot book, release or restore the hold", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			res  func() *entryRecoveryResult
		}{
			{"proven fill", filled},
			{"verified absence", entryAbsentHL},
		} {
			t.Run(tc.name, func(t *testing.T) {
				f := newEntryOperatorFixture(t, "hyperliquid")
				f.backdate()
				b := newBlockingVenue(t, func(int) (*entryRecoveryResult, error) { return tc.res(), nil }, 1)
				done := make(chan struct{})
				go func() {
					recoverEntryIntents(f.env.cfg, f.env.state, f.env.store, &f.env.mu, nil, nil, time.Now())
					close(done)
				}()
				waitEntered(t, b)
				f.env.mu.Lock()
				in := f.env.strat(f.sc.ID).EntryGuard.Intents[f.intentID]
				in.EvidenceJSON = marshalEntryJSON(entryIntentEvidence{Outcome: "operator_verified_absent"})
				in.HoldReason = "changed by another writer"
				f.env.mu.Unlock()
				close(b.proceed)
				<-done
				if n := f.env.openTradeRows(f.sc.ID); n != 0 {
					t.Fatalf("a stale job booked %d trades", n)
				}
				if qty, _ := f.env.position(f.sc.ID, "BTC"); qty != 0 {
					t.Fatalf("a stale job booked %v", qty)
				}
				if r := f.row(); r.Status != entryIntentSubmitting {
					t.Fatalf("a stale job changed the row: %+v", r)
				}
				if got := f.env.consumedBar(f.sc.ID, "BTC", "long"); got != 0 {
					t.Fatalf("a stale job moved the consumed candle to %d", got)
				}
				f.env.mu.RLock()
				hold := in.HoldReason
				f.env.mu.RUnlock()
				if hold != "changed by another writer" {
					t.Fatalf("a stale job rewrote the hold reason: %q", hold)
				}
			})
		}
	})

	t.Run("a stale recovery job cannot restore a hold the operator released", func(t *testing.T) {
		f := newEntryOperatorFixture(t, "hyperliquid")
		f.backdate()
		b := newBlockingVenue(t, func(int) (*entryRecoveryResult, error) { return nil, errors.New("connection reset") }, 1)
		done := make(chan struct{})
		go func() {
			recoverEntryIntents(f.env.cfg, f.env.state, f.env.store, &f.env.mu, nil, nil, time.Now())
			close(done)
		}()
		waitEntered(t, b)
		f.env.mu.Lock()
		g := f.env.strat(f.sc.ID).EntryGuard
		in := g.Intents[f.intentID]
		delete(g.Intents, f.intentID)
		f.env.mu.Unlock()
		close(b.proceed)
		<-done
		if g.Intents[f.intentID] != nil || in.HoldReason == "the venue recovery query failed: connection reset" {
			t.Fatal("a stale job restored or rewrote a released hold")
		}
		if adm := f.admitNextCandle(); !adm.admitted() {
			t.Fatalf("a stale job blocked a later entry: %q", adm.HoldReason)
		}
	})
}

func TestEntryOperatorFailedCommitKeepsTheHoldAndRetrySucceeds(t *testing.T) {
	f := newEntryOperatorFixture(t, "okx-spot")
	f.backdate()
	q := fixedVenue(entryAbsentOKX("spot"), nil)
	installEntryVenueQuery(t, q)
	cashBefore := f.cash()
	prev := storeCommitHook
	t.Cleanup(func() { storeCommitHook = prev })
	storeCommitHook = func(storageRole) error { return errors.New("disk full") }
	msg := f.command()
	if !strings.Contains(msg, "saving the release failed") || !strings.Contains(msg, "retry is safe") {
		t.Fatalf("reply %q", msg)
	}
	f.assertUnchanged(msg, cashBefore)
	storeCommitHook = prev
	if msg := f.command(); !strings.HasPrefix(msg, "Released attempt") {
		t.Fatalf("retry: %q", msg)
	}
	if got := f.env.intentStatuses(f.sc.ID); len(got) != 1 || got[0] != entryIntentNoFill {
		t.Fatalf("intents %v", got)
	}
}

func TestEntryRecoveryKeepsProvenExecutionAgainstLaterEmptyResults(t *testing.T) {
	t.Run("a booking conflict saves the proven fill before holding", func(t *testing.T) {
		useEntryTestHLAccount(t)
		env, sc := entryTestUnknownScaleIn(t)
		backdateEntryDeadlines(t, env, sc.ID)
		env.mu.Lock()
		env.strat(sc.ID).Positions["BTC"].Quantity = 0.5
		env.mu.Unlock()
		q := fixedVenue(entryTestScaleInFill(), nil)
		installEntryVenueQuery(t, q)
		recoverEntryIntents(env.cfg, env.state, env.store, &env.mu, nil, nil, time.Now())
		in := env.strat(sc.ID).EntryGuard.unresolvedIntent()
		if in == nil {
			t.Fatal("the conflicting attempt was released")
		}
		var confirmed float64
		var evidence string
		if err := env.fileFor(sc.ID).db.QueryRow(`SELECT confirmed_qty, evidence_json FROM entry_order_intents WHERE intent_id = ?`, in.IntentID).Scan(&confirmed, &evidence); err != nil {
			t.Fatal(err)
		}
		if confirmed != 0.2 || !strings.Contains(evidence, `"outcome":"filled_unbooked"`) {
			t.Fatalf("the proven fill was not saved before the hold: confirmed=%v evidence=%s", confirmed, evidence)
		}
		env = env.restart()
		absent := entryAbsentHL()
		q.respond = func(int) (*entryRecoveryResult, error) { return absent, nil }
		recoverEntryIntents(env.cfg, env.state, env.store, &env.mu, nil, nil, time.Now().Add(time.Hour))
		if got := env.intentStatuses(sc.ID); len(got) != 1 || got[0] != entryIntentSubmitting {
			t.Fatalf("a later empty result released proven execution: %v", got)
		}
		d := &DiscordNotifier{ownerID: entryTestOwnerID, ss: &StatusServer{state: env.state, mu: &env.mu, stateDB: env.store, strategies: env.cfg.Strategies}}
		if msg := d.resolveEntryCommand(entryTestOwnerID, "", entryCommandOpts(sc.ID, in.IntentID)); !strings.Contains(msg, "saved confirmed fill") {
			t.Fatalf("the command did not refuse saved proof: %q", msg)
		}
	})

	t.Run("contradictory terminal evidence holds", func(t *testing.T) {
		for _, tc := range []struct {
			name    string
			prepare func(env *entryTestEnv, id string)
			res     *entryRecoveryResult
		}{
			{"saved proof against a zero-fill order", func(env *entryTestEnv, id string) {
				env.mu.Lock()
				env.strat(id).EntryGuard.unresolvedIntent().ConfirmedQty = 0.3
				env.mu.Unlock()
			}, &entryRecoveryResult{Account: entryTestHLAccountKey, Order: &entryRecoveryOrder{Found: true, Status: "canceled", OrderID: "9", OrigSz: 0.4, RemainingSz: 0.4, Terminal: true}}},
			{"client-id fills against a zero-fill order", nil, &entryRecoveryResult{Account: entryTestHLAccountKey, Order: &entryRecoveryOrder{Found: true, Status: "canceled", OrderID: "9", OrigSz: 0.4, RemainingSz: 0.4, Terminal: true, Coverage: &entryFillCoverage{Complete: true, MatchedCount: 1, MatchedQty: 0.1}}}},
			{"missing order without coverage", nil, &entryRecoveryResult{Account: entryTestHLAccountKey, Order: &entryRecoveryOrder{Found: false}, Stop: &entryRecoveryOrder{Found: false}}},
			{"result and query error together", nil, nil},
		} {
			t.Run(tc.name, func(t *testing.T) {
				env, sc := entryTestUnknownAttempt(t)
				backdateEntryDeadlines(t, env, sc.ID)
				if tc.prepare != nil {
					tc.prepare(env, sc.ID)
				}
				q := fixedVenue(tc.res, nil)
				if tc.res == nil {
					q = fixedVenue(entryAbsentHL(), errors.New("recovery query error: exit status 1"))
				}
				installEntryVenueQuery(t, q)
				recoverEntryIntents(env.cfg, env.state, env.store, &env.mu, nil, nil, time.Now())
				if got := env.intentStatuses(sc.ID); len(got) != 1 || got[0] != entryIntentSubmitting {
					t.Fatalf("intents %v, want the attempt held", got)
				}
			})
		}
	})
}
