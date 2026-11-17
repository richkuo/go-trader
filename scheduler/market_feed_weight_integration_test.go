package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type wtRequest struct {
	Type      string
	Coin      string
	At        time.Time
	StartTime int64
	EndTime   int64
	HasEnd    bool
}

type wtVenue struct {
	srv   *httptest.Server
	clock func() time.Time

	mu            sync.Mutex
	reqs          []wtRequest
	candleExtra   int
	candleRows    int
	candleFloorMs int64
	malformed     map[string]bool
	fundingRows   int
	fundingOutset bool
	noCandles     map[string]bool
	gate          map[string]chan struct{}
	enter         map[string]chan struct{}
}

func newWTVenue(t *testing.T, clock func() time.Time) *wtVenue {
	t.Helper()
	v := &wtVenue{clock: clock, malformed: map[string]bool{}, gate: map[string]chan struct{}{}, enter: map[string]chan struct{}{}}
	v.srv = httptest.NewServer(http.HandlerFunc(v.handle))
	t.Cleanup(v.srv.Close)
	useHLInfoURLs(t, v.srv.URL, v.srv.URL)
	return v
}

func (v *wtVenue) set(fn func(v *wtVenue)) {
	v.mu.Lock()
	defer v.mu.Unlock()
	fn(v)
}

func (v *wtVenue) requests(reqType string) []wtRequest {
	v.mu.Lock()
	defer v.mu.Unlock()
	var out []wtRequest
	for _, r := range v.reqs {
		if r.Type == reqType {
			out = append(out, r)
		}
	}
	return out
}

func (v *wtVenue) hold(reqType string) (entered <-chan struct{}, release func()) {
	v.mu.Lock()
	defer v.mu.Unlock()
	g, e := make(chan struct{}), make(chan struct{}, 1)
	v.gate[reqType], v.enter[reqType] = g, e
	var once sync.Once
	return e, func() { once.Do(func() { close(g) }) }
}

func (v *wtVenue) handle(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Type      string           `json:"type"`
		Coin      string           `json:"coin"`
		StartTime int64            `json:"startTime"`
		EndTime   *int64           `json:"endTime"`
		Req       *json.RawMessage `json:"req"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	rec := wtRequest{Type: body.Type, Coin: body.Coin, StartTime: body.StartTime}
	if body.EndTime != nil {
		rec.EndTime, rec.HasEnd = *body.EndTime, true
	}
	if body.Req != nil {
		var inner struct {
			Coin      string `json:"coin"`
			StartTime int64  `json:"startTime"`
			EndTime   int64  `json:"endTime"`
		}
		_ = json.Unmarshal(*body.Req, &inner)
		rec.StartTime, rec.EndTime, rec.HasEnd = inner.StartTime, inner.EndTime, true
		rec.Coin = inner.Coin
	}
	v.mu.Lock()
	rec.At = v.clock()
	v.reqs = append(v.reqs, rec)
	gate, enter := v.gate[body.Type], v.enter[body.Type]
	delete(v.gate, body.Type)
	delete(v.enter, body.Type)
	malformed := v.malformed[body.Type]
	extra, fixedRows, floor := v.candleExtra, v.candleRows, v.candleFloorMs
	fundingRows, outset := v.fundingRows, v.fundingOutset
	empty := v.noCandles[rec.Coin]
	v.mu.Unlock()
	if gate != nil {
		enter <- struct{}{}
		select {
		case <-gate:
		case <-r.Context().Done():
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	if malformed {
		w.Write([]byte(`{"not":"a list"`))
		return
	}
	switch body.Type {
	case "candleSnapshot":
		const step = int64(60_000)
		start := rec.StartTime
		if start < floor {
			start = floor
		}
		rows := []map[string]any{}
		first := (start + step - 1) / step * step
		for open := first; open <= rec.EndTime && len(rows) < 5000; open += step {
			rows = append(rows, map[string]any{"t": open, "T": open + step - 1, "o": "100", "h": "102", "l": "99", "c": "101", "v": "5"})
		}
		if empty {
			rows = rows[:0]
			extra = 0
		}
		if fixedRows > 0 && !empty {
			rows = rows[:0]
			for i := 0; i < fixedRows; i++ {
				open := first + int64(i)*step
				rows = append(rows, map[string]any{"t": open, "T": open + step - 1, "o": "100", "h": "102", "l": "99", "c": "101", "v": "5"})
			}
		}
		for i := 0; i < extra; i++ {
			open := rec.EndTime + int64(i+1)*step
			rows = append(rows, map[string]any{"t": open, "T": open + step - 1, "o": "100", "h": "102", "l": "99", "c": "101", "v": "5"})
		}
		json.NewEncoder(w).Encode(rows)
	case "fundingHistory":
		rows := []map[string]any{}
		const hour = int64(3_600_000)
		first := (rec.StartTime + hour - 1) / hour * hour
		for ts := first; ts <= rec.EndTime && len(rows) < fundingRows; ts += hour {
			rows = append(rows, map[string]any{"coin": "BTC", "fundingRate": "0.0001", "premium": "0", "time": ts})
		}
		if outset {
			rows = append(rows, map[string]any{"coin": "BTC", "fundingRate": "0.0001", "premium": "0", "time": rec.EndTime + hour})
		}
		json.NewEncoder(w).Encode(rows)
	case "allMids":
		w.Write([]byte(`{"AAA":"10","BBB":"11","BTC":"101.5","CCC":"12","ETH":"11"}`))
	case "meta":
		w.Write([]byte(hlMetaBody(`{"name":"BTC","szDecimals":5}`)))
	case "metaAndAssetCtxs":
		w.Write([]byte(`[{"universe":[{"name":"BTC"}]},[{"funding":"0.0001"}]]`))
	default:
		w.Write([]byte(`[]`))
	}
}

func wtPolicy(perMinute, startup, reserve int, requests ...int) *feedBudgetConfig {
	cfg := &feedBudgetConfig{PerMinute: 1000, Startup: 1000, WeightPerMinute: feedIntPtr(perMinute), WeightStartup: feedIntPtr(startup), WeightReserve: feedIntPtr(reserve)}
	if len(requests) == 2 {
		cfg.PerMinute, cfg.Startup = requests[0], requests[1]
	}
	return cfg
}

func wtCtx(l *feedRequestLedger, reason, input string) context.Context {
	return withFeedInput(withFeedReason(withFeedLedger(context.Background(), l), reason), input)
}

func wtCandleCost(t *testing.T, r wtRequest) int {
	t.Helper()
	bound, err := hlCandleRowBound("1m", r.StartTime, r.EndTime)
	if err != nil {
		t.Fatal(err)
	}
	cost, err := hlInfoWeight("candleSnapshot", bound)
	if err != nil {
		t.Fatal(err)
	}
	return cost
}

func wtWait(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestFeedWeightSenderLifecycle(t *testing.T) {
	base := time.Unix(1890000000, 0).UTC()

	t.Run("SettlementRefundsUnusedRowsAndChargesLeaveSixtySecondsAfterCompletion", func(t *testing.T) {
		clock := &hlLotTestClock{now: base}
		newWTVenue(t, clock.Now)
		ledger := newFeedRequestLedger(wtPolicy(1000, 0, 0), true, clock.Now)
		ctx := wtCtx(ledger, string(feedRestRefresh), "BTC|1m")
		raws, err := fetchHyperliquidCandleSnapshot(ctx, "BTC", "1m", base.Add(-59*time.Minute).UnixMilli(), base.UnixMilli())
		if err != nil {
			t.Fatal(err)
		}
		reserved, _ := hlInfoWeight("candleSnapshot", 61)
		settled, _ := hlInfoWeight("candleSnapshot", len(raws))
		st := ledger.weightStatus()
		if st.Totals.Reserved != reserved || st.Totals.Settled != settled || st.Totals.Refunded != reserved-settled || st.Charged != settled {
			t.Fatalf("rows %d: weight = %+v, want reserved %d settled %d refunded %d charged %d", len(raws), st, reserved, settled, reserved-settled, settled)
		}
		if settled != 20+(len(raws)+59)/60 {
			t.Fatalf("settled cost %d for %d rows, want 20 + ceil(rows/60)", settled, len(raws))
		}
		clock.Advance(59 * time.Second)
		if st := ledger.weightStatus(); st.Charged != settled {
			t.Fatalf("charge after 59s = %d, want %d retained", st.Charged, settled)
		}
		clock.Advance(time.Second)
		if st := ledger.weightStatus(); st.Charged != 0 || st.Pending != 0 || st.InFlight != 0 {
			t.Fatalf("weight 60s after completion = %+v, want nothing charged, pending or in flight", st)
		}
	})

	t.Run("MalformedTimeoutAndOutOfRangeRowsKeepTheFullReservation", func(t *testing.T) {
		clock := &hlLotTestClock{now: base}
		venue := newWTVenue(t, clock.Now)
		ledger := newFeedRequestLedger(wtPolicy(1000, 0, 0), true, clock.Now)
		ctx := wtCtx(ledger, string(feedRestRefresh), "BTC|1m")
		start := base.Add(-59 * time.Minute).UnixMilli()
		cost, _ := hlInfoWeight("candleSnapshot", 61)

		venue.set(func(v *wtVenue) { v.malformed["candleSnapshot"] = true })
		if _, err := fetchHyperliquidCandleSnapshot(ctx, "BTC", "1m", start, base.UnixMilli()); err == nil {
			t.Fatal("a malformed body decoded")
		}
		if st := ledger.weightStatus(); st.Charged != cost || st.Totals.Retained != cost || st.Totals.Refunded != 0 {
			t.Fatalf("after a malformed body weight = %+v, want the full %d retained", st, cost)
		}
		venue.set(func(v *wtVenue) { v.malformed["candleSnapshot"] = false })

		entered, release := venue.hold("candleSnapshot")
		t.Cleanup(release)
		done := make(chan error, 1)
		body, _ := json.Marshal(map[string]any{"type": "candleSnapshot", "req": map[string]any{"coin": "BTC", "interval": "1m", "startTime": start, "endTime": base.UnixMilli()}})
		go func() {
			done <- hlInfoPost(ctx, hlInfoCall{BaseURL: hlMainnetURL, Type: "candleSnapshot", Body: body, Timeout: 100 * time.Millisecond, RowBound: 61,
				Decode: func(b []byte) (int, error) { raws, err := parseHyperliquidCandleRaws(b); return len(raws), err }})
		}()
		<-entered
		if st := ledger.weightStatus(); st.InFlight != cost {
			t.Fatalf("in flight = %d, want %d while the venue holds the request", st.InFlight, cost)
		}
		if err := <-done; err == nil {
			t.Fatal("a timed-out read returned no error")
		}
		release()
		if st := ledger.weightStatus(); st.Charged != 2*cost || st.Totals.Retained != 2*cost || st.InFlight != 0 {
			t.Fatalf("after a timeout weight = %+v, want both reads retained at %d and nothing in flight", st, cost)
		}

		venue.set(func(v *wtVenue) { v.fundingRows = 2; v.fundingOutset = true })
		fctx := wtCtx(ledger, string(feedRestFunding), "funding:BTC")
		if _, err := hlFundingRecordsSince(fctx, "BTC", base.Add(-2*time.Hour).UnixMilli(), base); err == nil || !strings.Contains(err.Error(), "outside the requested") {
			t.Fatalf("an out-of-range funding row err = %v, want a malformed-response refusal", err)
		}
		fcost, _ := hlInfoWeight("fundingHistory", 3)
		if st := ledger.weightStatus(); st.Totals.ChargedByType["fundingHistory"] != fcost || st.Totals.Refunded != 0 {
			t.Fatalf("after an out-of-range row weight = %+v, want the funding reservation %d kept", st, fcost)
		}
	})

	t.Run("FundingRequestsCarryEndTime", func(t *testing.T) {
		clock := &hlLotTestClock{now: base}
		venue := newWTVenue(t, clock.Now)
		venue.set(func(v *wtVenue) { v.fundingRows = 500 })
		ledger := newFeedRequestLedger(wtPolicy(1000, 0, 0), true, clock.Now)
		ctx := wtCtx(ledger, string(feedRestFunding), "funding:BTC")
		if _, err := hlFundingAverage7d(ctx, "BTC", base); err != nil {
			t.Fatal(err)
		}
		recs, err := hlFundingRecordsSince(ctx, "BTC", base.Add(-30*24*time.Hour).UnixMilli(), base)
		if err != nil {
			t.Fatal(err)
		}
		cov, err := hlFundingRecordsCoverage(ctx, "BTC", base.Add(-3*time.Hour).UnixMilli(), base, 2)
		if err != nil {
			t.Fatal(err)
		}
		if len(recs) != 30*24+1 || len(cov.Records) != 4 {
			t.Fatalf("records = %d and coverage %d, want 721 and 4 with both endpoints included", len(recs), len(cov.Records))
		}
		reqs := venue.requests("fundingHistory")
		if len(reqs) < 4 {
			t.Fatalf("funding requests = %d, want the average, at least two pages and the coverage read", len(reqs))
		}
		for i, r := range reqs {
			if !r.HasEnd || r.EndTime != base.UnixMilli() || r.EndTime < r.StartTime {
				t.Fatalf("funding request %d = %+v, want endTime %d on every page", i, r, base.UnixMilli())
			}
		}
	})

	t.Run("BoundViolationChargesDebtThenRepricesThenBlocks", func(t *testing.T) {
		clock := &hlLotTestClock{now: base}
		venue := newWTVenue(t, clock.Now)
		ledger := newFeedRequestLedger(wtPolicy(1100, 0, 0), true, clock.Now)
		var logs []string
		ledger.logf = func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }
		ctx := wtCtx(ledger, string(feedRestRefresh), "BTC|1m")
		start, end := base.Add(-58*time.Minute).UnixMilli(), base.UnixMilli()
		venue.set(func(v *wtVenue) { v.candleRows = 60; v.candleExtra = 1 })
		if _, err := fetchHyperliquidCandleSnapshot(ctx, "BTC", "1m", start, end); err == nil || !strings.Contains(err.Error(), "over its 60-row bound") {
			t.Fatalf("a 61-row response err = %v, want a bound violation", err)
		}
		reserved, _ := hlInfoWeight("candleSnapshot", 60)
		observed, _ := hlInfoWeight("candleSnapshot", 61)
		st := ledger.weightStatus()
		if st.Totals.Debt != observed-reserved || observed-reserved != 1 || st.Charged != observed || len(st.Violations) != 1 || st.Violations[0].State != "repriced_at_cap" {
			t.Fatalf("after one extra row weight = %+v, want debt %d, charged %d and a repriced violation", st, observed-reserved, observed)
		}
		events := ledger.weightReport(nil)
		if len(events) != 1 || events[0].Kind != feedWeightEventViolation || events[0].Observed != 61 {
			t.Fatalf("violation report = %+v, want one violation summary", events)
		}
		if again := ledger.weightReport(nil); len(again) != 0 {
			t.Fatalf("second report = %+v, want the violation alerted once", again)
		}

		venue.set(func(v *wtVenue) { v.candleRows = 0; v.candleExtra = 0 })
		before := ledger.weightStatus().Totals.Reserved
		if _, err := fetchHyperliquidCandleSnapshot(ctx, "BTC", "1m", start, end); err != nil {
			t.Fatal(err)
		}
		capCost, _ := hlInfoWeightAtCap("candleSnapshot")
		if got := ledger.weightStatus().Totals.Reserved - before; got != capCost {
			t.Fatalf("next read reserved %d, want the %d response-cap price", got, capCost)
		}

		admitted, releaseHook := make(chan struct{}), make(chan struct{})
		var hookOnce sync.Once
		hlInfoBeforeSendHook = func(c context.Context, reqType string) {
			if in, _ := c.Value(feedInputCtxKey{}).(string); in == "pending" {
				hookOnce.Do(func() { close(admitted) })
				<-releaseHook
			}
		}
		t.Cleanup(func() { hlInfoBeforeSendHook = nil })
		pendingDone := make(chan error, 1)
		go func() {
			_, err := fetchHyperliquidCandleSnapshot(wtCtx(ledger, string(feedRestRefresh), "pending"), "BTC", "1m", start, end)
			pendingDone <- err
		}()
		<-admitted
		clock.Advance(2 * feedBudgetWindow)
		venue.set(func(v *wtVenue) { v.candleRows = 5001; v.candleExtra = 0 })
		if _, err := fetchHyperliquidCandleSnapshot(ctx, "BTC", "1m", base.Add(-6000*time.Minute).UnixMilli(), end); err == nil {
			t.Fatal("a 5001-row response was accepted")
		}
		close(releaseHook)
		if err := <-pendingDone; feedWeightRefusalCause(err) != feedWeightCauseBound {
			t.Fatalf("a read priced before the violation err = %v, want %s at beginSend", err, feedWeightCauseBound)
		}
		sent := len(venue.requests("candleSnapshot"))
		_, err := fetchHyperliquidCandleSnapshot(ctx, "BTC", "1m", start, end)
		if feedWeightRefusalCause(err) != feedWeightCauseBound || !errors.Is(err, errFeedBudgetExhausted) {
			t.Fatalf("a read after an above-cap response err = %v, want %s", err, feedWeightCauseBound)
		}
		if got := len(venue.requests("candleSnapshot")); got != sent {
			t.Fatalf("venue saw %d candle request(s) after the block, want none", got-sent)
		}
		if st := ledger.weightStatus(); st.Violations[0].State != "blocked" || st.Totals.RefusedByCause[feedWeightCauseBound] != 2 {
			t.Fatalf("weight = %+v, want a blocked candleSnapshot and two bound refusals", st)
		}
		found := false
		for _, l := range logs {
			if strings.Contains(l, "bound_violation type=candleSnapshot") && strings.Contains(l, "observed=5001") {
				found = true
			}
		}
		if !found {
			t.Fatalf("logs %v lack the above-cap violation line", logs)
		}
	})

	t.Run("RepricedReadsSettleAtTheCapAndAlertOnce", func(t *testing.T) {
		clock := &hlLotTestClock{now: base}
		venue := newWTVenue(t, clock.Now)
		ledger := newFeedRequestLedger(wtPolicy(1100, 0, 0), true, clock.Now)
		ctx := wtCtx(ledger, string(feedRestRefresh), "BTC|1m")
		start, end := base.Add(-58*time.Minute).UnixMilli(), base.UnixMilli()
		venue.set(func(v *wtVenue) { v.candleRows = 60; v.candleExtra = 1 })
		if _, err := fetchHyperliquidCandleSnapshot(ctx, "BTC", "1m", start, end); err == nil {
			t.Fatal("the first 61-row response was accepted against its 60-row bound")
		}
		if events := ledger.weightReport(nil); len(events) != 1 || events[0].State != "repriced_at_cap" {
			t.Fatalf("first violation report = %+v, want one repriced summary", events)
		}
		debt := ledger.weightStatus().Totals.Debt
		before := ledger.weightStatus().Totals.Reserved
		bars, err := fetchHyperliquidCandleSnapshot(ctx, "BTC", "1m", start, end)
		if err != nil || len(bars) == 0 {
			t.Fatalf("a 61-row read priced at the cap = %d bar(s), err %v; want it settled and its bars returned", len(bars), err)
		}
		capCost, _ := hlInfoWeightAtCap("candleSnapshot")
		st := ledger.weightStatus()
		if st.Totals.Reserved-before != capCost || st.Totals.Debt != debt || st.Violations[0].State != "repriced_at_cap" {
			t.Fatalf("weight after the capped read = %+v, want %d reserved, debt unchanged at %d and the state still repriced", st, capCost, debt)
		}
		if again := ledger.weightReport(nil); len(again) != 0 {
			t.Fatalf("report after the capped read = %+v, want no second violation alert", again)
		}
	})

	t.Run("ViolationAlertOnAnOwnerWithoutPolicyNeverSaysRefused", func(t *testing.T) {
		clock := &hlLotTestClock{now: base}
		venue := newWTVenue(t, clock.Now)
		ledger := newFeedRequestLedger(nil, false, clock.Now)
		ctx := wtCtx(ledger, string(feedRestRefresh), "BTC|1m")
		end := base.UnixMilli()
		venue.set(func(v *wtVenue) { v.candleRows = 5001 })
		if _, err := fetchHyperliquidCandleSnapshot(ctx, "BTC", "1m", base.Add(-6000*time.Minute).UnixMilli(), end); err == nil {
			t.Fatal("a 5001-row response was accepted")
		}
		events := ledger.weightReport(nil)
		if len(events) != 1 || events[0].State != "blocked" || events[0].Enforced {
			t.Fatalf("violation report = %+v, want one unenforced blocked summary", events)
		}
		msg := formatFeedWeightAlert(events[0], feedSourceWebsocket, "fixture")
		if strings.Contains(msg, "refused") && !strings.Contains(msg, "not refused") || !strings.Contains(msg, "no weight policy") {
			t.Fatalf("alert %q claims later reads are refused on an owner with no weight policy", msg)
		}
		sent := len(venue.requests("candleSnapshot"))
		venue.set(func(v *wtVenue) { v.candleRows = 0 })
		if _, err := fetchHyperliquidCandleSnapshot(ctx, "BTC", "1m", base.Add(-58*time.Minute).UnixMilli(), end); err != nil {
			t.Fatalf("a later read on the owner with no weight policy err = %v, want it sent as the alert says", err)
		}
		if len(venue.requests("candleSnapshot")) != sent+1 {
			t.Fatal("the later read did not reach the venue")
		}
	})

	t.Run("DelayedSendAndSlowResponseNeverAgeOut", func(t *testing.T) {
		clock := &hlLotTestClock{now: base}
		venue := newWTVenue(t, clock.Now)
		ledger := newFeedRequestLedger(wtPolicy(100, 0, 0), true, clock.Now)
		start, end := base.Add(-59*time.Minute).UnixMilli(), base.UnixMilli()
		cost, _ := hlInfoWeight("candleSnapshot", 61)

		admitted, releaseHook := make(chan struct{}), make(chan struct{})
		var hookOnce sync.Once
		hlInfoBeforeSendHook = func(c context.Context, reqType string) {
			if in, _ := c.Value(feedInputCtxKey{}).(string); in == "delayed" {
				hookOnce.Do(func() { close(admitted) })
				<-releaseHook
			}
		}
		t.Cleanup(func() { hlInfoBeforeSendHook = nil })
		delayed := make(chan error, 1)
		go func() {
			_, err := fetchHyperliquidCandleSnapshot(wtCtx(ledger, string(feedRestRefresh), "delayed"), "BTC", "1m", start, end)
			delayed <- err
		}()
		<-admitted
		clock.Advance(3 * feedBudgetWindow)
		if st := ledger.weightStatus(); st.Pending != cost || st.SteadyUsed != cost {
			t.Fatalf("an unsent permit after 180s = %+v, want %d still pending and counted", st, cost)
		}
		ledger.setLimits(wtPolicy(10, 0, 0))
		close(releaseHook)
		if err := <-delayed; feedWeightRefusalCause(err) != feedWeightCauseExhausted {
			t.Fatalf("a delayed send after a lower limit err = %v, want %s at beginSend", err, feedWeightCauseExhausted)
		}
		if n := len(venue.requests("candleSnapshot")); n != 0 {
			t.Fatalf("venue saw %d request(s), want the delayed send refused before it sent", n)
		}
		if st := ledger.weightStatus(); st.Pending != 0 || st.Totals.Released != cost {
			t.Fatalf("after the refusal weight = %+v, want the unsent reservation released", st)
		}

		ledger.setLimits(wtPolicy(40, 0, 0))
		entered, release := venue.hold("candleSnapshot")
		slow := make(chan error, 1)
		go func() {
			_, err := fetchHyperliquidCandleSnapshot(wtCtx(ledger, string(feedRestRefresh), "slow"), "BTC", "1m", start, end)
			slow <- err
		}()
		<-entered
		clock.Advance(3 * feedBudgetWindow)
		other := wtCtx(ledger, string(feedRestRefresh), "other")
		if _, err := fetchHyperliquidCandleSnapshot(other, "BTC", "1m", start, end); feedWeightRefusalCause(err) != feedWeightCauseExhausted {
			t.Fatalf("a read while a slow response holds %d err = %v, want %s", cost, err, feedWeightCauseExhausted)
		}
		release()
		if err := <-slow; err != nil {
			t.Fatal(err)
		}
		if _, err := fetchHyperliquidCandleSnapshot(other, "BTC", "1m", start, end); feedWeightRefusalCause(err) != feedWeightCauseExhausted {
			t.Fatalf("a read right after the slow completion err = %v, want the settled charge retained", err)
		}
		clock.Advance(feedBudgetWindow)
		if _, err := fetchHyperliquidCandleSnapshot(other, "BTC", "1m", start, end); err != nil {
			t.Fatalf("a read 60s after the slow completion: %v", err)
		}
	})

	t.Run("ZeroAllocationAndUnpricedTypesSendNothing", func(t *testing.T) {
		clock := &hlLotTestClock{now: base}
		venue := newWTVenue(t, clock.Now)
		ledger := newFeedRequestLedger(wtPolicy(0, 0, 0), true, clock.Now)
		ctx := wtCtx(ledger, string(feedRestMids), "mids")
		if _, err := fetchHyperliquidMidsCtx(ctx, []string{"BTC"}); feedWeightRefusalCause(err) != feedWeightCauseUnfit {
			t.Fatalf("mids under a zero allocation err = %v, want %s", err, feedWeightCauseUnfit)
		}
		if _, err := hlPostInfoTo(ctx, hlMainnetURL, map[string]string{"type": "clearinghouseState"}, 0); feedWeightRefusalCause(err) != feedWeightCauseUnpriced {
			t.Fatalf("an unpriced type err = %v, want %s", err, feedWeightCauseUnpriced)
		}
		venue.mu.Lock()
		n := len(venue.reqs)
		venue.mu.Unlock()
		if n != 0 {
			t.Fatalf("venue saw %d request(s), want none", n)
		}
		disabled := newFeedRequestLedger(nil, false, clock.Now)
		if _, err := hlPostInfoTo(withFeedLedger(context.Background(), disabled), hlMainnetURL, map[string]string{"type": "clearinghouseState"}, 0); err != nil {
			t.Fatalf("a ledger with no weight policy refused: %v", err)
		}
		if st := disabled.weightStatus(); st.Participation != feedWeightNotEnrolled || st.Totals.Unpriced != 1 {
			t.Fatalf("disabled weight = %+v, want participation disabled and one unpriced read counted", st)
		}
	})
}

func TestFeedWeightOwners(t *testing.T) {
	base := time.Unix(1890000000, 0).UTC()

	t.Run("WideningBootstrapReservesEachPass", func(t *testing.T) {
		clock := &hlLotTestClock{now: base}
		venue := newWTVenue(t, clock.Now)
		venue.set(func(v *wtVenue) { v.candleFloorMs = base.Add(-40 * time.Minute).UnixMilli() })
		ledger := newFeedRequestLedger(wtPolicy(1000, 0, 0), true, clock.Now)
		ledger.openStartup()
		owner := newMarketFeedOwner(clock.Now, nil)
		owner.SetRequestLedger(ledger)
		<-owner.ApplyGeneration(withFeedLedger(context.Background(), ledger), rlRequirements(100, 60, "BTC"))
		reqs := venue.requests("candleSnapshot")
		if len(reqs) < 2 {
			t.Fatalf("bootstrap sent %d candle request(s), want a widening pass", len(reqs))
		}
		want := 0
		for _, r := range reqs {
			want += wtCandleCost(t, r)
		}
		st := ledger.weightStatus()
		if st.Totals.Reserved != want || st.Totals.ReservedByReason[string(feedRestBootstrap)] != want {
			t.Fatalf("reserved %d (bootstrap %d), want %d from %d separate passes", st.Totals.Reserved, st.Totals.ReservedByReason[string(feedRestBootstrap)], want, len(reqs))
		}
	})

	t.Run("ConcurrentBootstrapWaitsWithinTheAllocation", func(t *testing.T) {
		clock := &hlLotTestClock{now: base}
		venue := newWTVenue(t, clock.Now)
		ledger := newFeedRequestLedger(wtPolicy(50, 25, 0), true, clock.Now)
		ledger.openStartup()
		owner := newMarketFeedOwner(clock.Now, nil)
		owner.SetRequestLedger(ledger)
		coins := []string{"AAA", "BBB", "CCC", "DDD", "EEE", "FFF"}
		ready := owner.ApplyGeneration(withFeedLedger(context.Background(), ledger), rlRequirements(5, 60, coins...))
		stop := make(chan struct{})
		var ticker sync.WaitGroup
		ticker.Add(1)
		go func() {
			defer ticker.Done()
			for {
				select {
				case <-stop:
					return
				case <-time.After(20 * time.Millisecond):
				}
				if ledger.weightStatus().Pending == 0 && ledger.weightStatus().InFlight == 0 {
					clock.Advance(15 * time.Second)
				}
			}
		}()
		select {
		case <-ready:
		case <-time.After(20 * time.Second):
			t.Fatal("bootstrap did not finish by waiting for capacity")
		}
		close(stop)
		ticker.Wait()
		reqs := venue.requests("candleSnapshot")
		if len(reqs) != len(coins) {
			t.Fatalf("bootstrap sent %d request(s), want one per key", len(reqs))
		}
		for _, r := range owner.Readiness() {
			if r.Bars < r.Required || r.Status == feedStatusBudget || r.Status == feedStatusFailed {
				t.Fatalf("%s = %+v, want every key bootstrapped after waiting", r.Key, r)
			}
		}
		for i := range reqs {
			sum := 0
			for _, r := range reqs {
				if !r.At.Before(reqs[i].At) && r.At.Sub(reqs[i].At) < feedBudgetWindow {
					sum += wtCandleCost(t, r)
				}
			}
			if sum > 50+25 {
				t.Fatalf("fixture-observed 60s window from %s holds %d weight, over the 75 allocation", reqs[i].At, sum)
			}
		}
		if st := ledger.weightStatus(); st.Totals.Refused != 0 || st.StartupLeft != 0 {
			t.Fatalf("weight = %+v, want no refusal counted for waiting polls and the startup allowance spent", st)
		}
	})

	t.Run("CancelledBootstrapWaitRaisesOneBudgetFailure", func(t *testing.T) {
		clock := &hlLotTestClock{now: base}
		venue := newWTVenue(t, clock.Now)
		ledger := newFeedRequestLedger(wtPolicy(21, 0, 0), true, clock.Now)
		ledger.openStartup()
		owner := newMarketFeedOwner(clock.Now, nil)
		owner.SetRequestLedger(ledger)
		ctx, cancel := context.WithCancel(withFeedLedger(context.Background(), ledger))
		ready := owner.ApplyGeneration(ctx, rlRequirements(5, 60, "AAA", "BBB"))
		wtWait(t, "one key to send and the other to wait", func() bool { return len(venue.requests("candleSnapshot")) == 1 })
		time.Sleep(3 * feedWeightWaitTick)
		cancel()
		<-ready
		var failed []feedAlert
		for _, a := range owner.DrainAlerts() {
			if a.Kind == "bootstrap_failed" {
				failed = append(failed, a)
			}
		}
		if len(failed) != 1 || !strings.Contains(failed[0].Detail, feedWeightCauseExhausted) || !strings.Contains(failed[0].Detail, "key deadline") {
			t.Fatalf("bootstrap alerts = %+v, want one bootstrap_failed naming %s", failed, feedWeightCauseExhausted)
		}
		if st := ledger.weightStatus(); st.Totals.Refused != 1 || len(venue.requests("candleSnapshot")) != 1 {
			t.Fatalf("weight refused = %d and %d request(s), want one counted refusal and one send", st.Totals.Refused, len(venue.requests("candleSnapshot")))
		}
	})

	t.Run("RESTKeepProtectsMidsAndEpisodesSummarizeOnce", func(t *testing.T) {
		start := base
		clock := &hlLotTestClock{now: start.Add(-time.Minute)}
		venue := newWTVenue(t, clock.Now)
		boot := newFeedRequestLedger(nil, false, clock.Now)
		owner := newMarketFeedOwner(clock.Now, nil)
		req := rlRequirements(5, 60, "AAA", "BBB", "CCC")
		<-owner.ApplyGeneration(withFeedLedger(context.Background(), boot), req)
		refresh, _ := hlInfoWeight("candleSnapshot", 3)
		mids, _ := hlInfoWeight("allMids", 0)
		ledger := newFeedRequestLedger(wtPolicy(2*refresh+mids, 0, 0), true, clock.Now)
		ctx := withFeedLedger(context.Background(), ledger)
		owner.SetRequestLedger(ledger)
		var alerts, logs []string
		sealer := newFeedSealer(owner, feedSourceREST, "fixture", clock.Now, func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) })
		sealer.ledger = ledger
		src := newFeedRESTSource(owner, nil)
		src.backoff = time.Millisecond
		sealer.prepareFn = src.prepare
		sealer.alertHook = func(msg string) { alerts = append(alerts, "hook:"+msg) }
		sealer.rateLimitSend = func(msg string) { alerts = append(alerts, msg) }
		sealer.setGeneration(req, []int{60}, start.Add(-time.Hour))
		seal := func(key int64) *feedSeal {
			t.Helper()
			clock.mu.Lock()
			clock.now = time.Unix(key, 0).UTC().Add(2 * time.Second)
			clock.mu.Unlock()
			sealer.sealOne(ctx, key)
			sealer.mu.Lock()
			defer sealer.mu.Unlock()
			return sealer.ring[key]
		}
		k := start.Unix()
		midsBefore := len(venue.requests("allMids"))
		s := seal(k)
		if s == nil || s.KeysReady != 2 || len(venue.requests("allMids")) != midsBefore+1 || s.Mids == 0 {
			t.Fatalf("seal = %+v, mids requests %d; want two refreshed keys and the protected mids read", s, len(venue.requests("allMids"))-midsBefore)
		}
		if r := rlKeyStatus(t, owner, feedKeyFor("CCC", "1m")); r.Ready || r.Status != feedStatusBudget {
			t.Fatalf("CCC = %+v, want held by the weight allocation", r)
		}
		var starts []string
		for _, a := range alerts {
			if strings.HasPrefix(a, "**MARKET FEED WEIGHT REFUSED**") {
				starts = append(starts, a)
			}
			if strings.Contains(a, "REQUEST BUDGET") || strings.Contains(a, "RATE LIMIT") {
				t.Fatalf("a weight refusal raised another alert: %q", a)
			}
		}
		if len(starts) != 1 || !strings.Contains(starts[0], "Refused inputs: CCC|1m.") {
			t.Fatalf("weight alerts = %q, want one start summary naming CCC|1m", alerts)
		}
		budgetLine := ""
		for _, l := range logs {
			if strings.HasPrefix(l, "[feed-budget]") {
				budgetLine = l
			}
		}
		if !strings.Contains(budgetLine, "weight=enabled") || !strings.Contains(budgetLine, "weight_refused_keys=CCC|1m") {
			t.Fatalf("budget line %q lacks the weight fields", budgetLine)
		}
		for _, a := range owner.DrainAlerts() {
			if strings.HasSuffix(a.Kind, "_failed") {
				t.Fatalf("a weight refusal raised per-key alert %+v", a)
			}
		}
		ledger.setLimits(wtPolicy(1000, 0, 0))
		alerts = nil
		if s := seal(k + 60); s == nil || s.KeysReady != 3 {
			t.Fatalf("seal after the allocation rose = %+v, want three ready keys", s)
		}
		if len(alerts) != 1 || !strings.HasPrefix(alerts[0], "**MARKET FEED WEIGHT RECOVERED**") {
			t.Fatalf("alerts after recovery = %q, want one recovery summary", alerts)
		}
		alerts = nil
		seal(k + 120)
		if len(alerts) != 0 {
			t.Fatalf("alerts on a clean seal = %q, want none", alerts)
		}
	})

	t.Run("FundingRecordsWaitForTheFirstBar", func(t *testing.T) {
		start := base
		clock := &hlLotTestClock{now: start.Add(-time.Minute)}
		venue := newWTVenue(t, clock.Now)
		venue.set(func(v *wtVenue) { v.noCandles = map[string]bool{"AAA": true}; v.fundingRows = 3 })
		req := rlRequirements(5, 60, "AAA", "BTC")
		req.Funding["AAA"] = feedFundingNeed{Records: true}
		req.Funding["BTC"] = feedFundingNeed{Scalar: true}
		ledger := newFeedRequestLedger(wtPolicy(600, 400, 200), true, clock.Now)
		ledger.openStartup()
		ctx := withFeedLedger(context.Background(), ledger)
		owner := newMarketFeedOwner(clock.Now, nil)
		owner.SetRequestLedger(ledger)
		<-owner.ApplyGeneration(ctx, req)
		sealer := newFeedSealer(owner, feedSourceREST, "fixture", clock.Now, nil)
		sealer.ledger = ledger
		src := newFeedRESTSource(owner, nil)
		src.backoff = time.Millisecond
		sealer.prepareFn = src.prepare
		sealer.setGeneration(req, []int{60}, start.Add(-time.Hour))
		seal := func(key int64) *feedSeal {
			t.Helper()
			clock.mu.Lock()
			clock.now = time.Unix(key, 0).UTC().Add(2 * time.Second)
			clock.mu.Unlock()
			sealer.sealOne(ctx, key)
			sealer.mu.Lock()
			defer sealer.mu.Unlock()
			return sealer.ring[key]
		}
		aaaFunding := func() []wtRequest {
			var out []wtRequest
			for _, r := range venue.requests("fundingHistory") {
				if r.Coin == "AAA" {
					out = append(out, r)
				}
			}
			return out
		}
		k := start.Unix()
		for _, d := range []int64{k, k + 60} {
			s := seal(d)
			if s == nil {
				t.Fatalf("deadline %d stored no seal", d)
			}
			if r := rlKeyStatus(t, owner, feedKeyFor("BTC", "1m")); !r.Ready {
				t.Fatalf("deadline %d: BTC = %+v, want refreshed and ready", d, r)
			}
			if n := len(aaaFunding()); n != 0 {
				t.Fatalf("deadline %d: %d AAA fundingHistory request(s) reached the venue, want none while AAA has no bars", d, n)
			}
			if refused := ledger.weightStatus().Totals.Refused; refused != 0 {
				t.Fatalf("deadline %d: %d weight refusal(s), want none", d, refused)
			}
		}
		owner.feedMu.Lock()
		held := owner.funding["AAA"]
		owner.feedMu.Unlock()
		if held == nil || held.HasRecords || !held.waitingForBars || !strings.Contains(held.Err, "wait for the first candle bar") {
			t.Fatalf("AAA funding entry = %+v, want held with the missing-bars reason", held)
		}
		for _, a := range owner.DrainAlerts() {
			if a.Kind == "funding_failed" {
				t.Fatalf("a funding read held for missing bars raised %+v", a)
			}
		}

		btcAvg := 0
		for _, r := range venue.requests("fundingHistory") {
			if r.Coin == "BTC" && r.HasEnd && r.EndTime-r.StartTime == int64(hlFundingAvgWindowDays)*86_400_000 {
				btcAvg++
			}
		}
		if btcAvg == 0 || len(venue.requests("metaAndAssetCtxs")) == 0 {
			t.Fatalf("BTC scalar reads: %d 7-day average request(s), %d metaAndAssetCtxs request(s); want both sent", btcAvg, len(venue.requests("metaAndAssetCtxs")))
		}

		venue.set(func(v *wtVenue) { v.noCandles = nil })
		seal(k + 120)
		owner.feedMu.Lock()
		var firstClose int64
		if st := owner.keys[feedKeyFor("AAA", "1m")]; st != nil && len(st.Bars) > 0 {
			first := len(st.Bars) - 5
			if first < 0 {
				first = 0
			}
			firstClose = st.Bars[first].CloseMs
		}
		got := owner.funding["AAA"]
		owner.feedMu.Unlock()
		reads := aaaFunding()
		if firstClose == 0 || len(reads) == 0 || reads[0].StartTime != firstClose {
			t.Fatalf("after AAA got bars: frame start %d, AAA funding reads %+v; want the first read to start at the frame start", firstClose, reads)
		}
		if got == nil || !got.HasRecords || got.Err != "" || got.waitingForBars {
			t.Fatalf("AAA funding after bars = %+v, want settled records", got)
		}
	})

	t.Run("EmbeddedOwnerEpisodeAndStatusSurfaces", func(t *testing.T) {
		clock := &hlLotTestClock{now: base}
		venue := newWTVenue(t, clock.Now)
		ledger := newFeedRequestLedger((&feedWeightBudgetConfig{WeightPerMinute: feedIntPtr(0), WeightStartup: feedIntPtr(0), WeightReserve: feedIntPtr(0)}).budget(), false, clock.Now)
		ledger.openStartup()
		owner := newMarketFeedOwner(clock.Now, nil)
		owner.SetRequestLedger(ledger)
		ctx := withFeedLedger(context.Background(), ledger)
		req := rlRequirements(5, 60, "BBB", "AAA")
		<-owner.ApplyGeneration(ctx, req)
		if n := len(venue.requests("candleSnapshot")); n != 0 {
			t.Fatalf("a zero embedded allocation sent %d request(s)", n)
		}
		events := ledger.weightReport(owner.weightRefusedInputs())
		if len(events) != 1 || events[0].Kind != feedWeightEventStart || fmt.Sprint(events[0].Inputs) != "[AAA|1m BBB|1m]" {
			t.Fatalf("embedded report = %+v, want one start summary with sorted inputs", events)
		}
		reqs := fullCycleRequirements(req)
		prepareMarketSnapshot(ctx, owner, reqs)
		for _, a := range owner.DrainAlerts() {
			if a.Kind == "recovery_failed" {
				t.Fatalf("a weight refusal raised %+v", a)
			}
		}
		if events := ledger.weightReport(owner.weightRefusedInputs()); len(events) != 0 {
			t.Fatalf("report while still refused = %+v, want no duplicate summary", events)
		}
		ledger.setLimits((&feedWeightBudgetConfig{WeightPerMinute: feedIntPtr(500), WeightStartup: feedIntPtr(0), WeightReserve: feedIntPtr(0)}).budget())
		clock.Advance(feedHistoryRefusalFloor + feedHistoryJitterSpan(feedHistoryRefusalFloor))
		runFeedHistoryPass(t, owner, ctx)
		events = ledger.weightReport(owner.weightRefusedInputs())
		if len(events) != 1 || events[0].Kind != feedWeightEventRecover {
			t.Fatalf("report after recovery = %+v, want one recovery summary", events)
		}
		health := owner.Health("")
		st := ledger.status()
		if health.Weight == nil || !reflect.DeepEqual(*health.Weight, st.Weight) || !reflect.DeepEqual(ledger.weightStatus(), st.Weight) {
			t.Fatalf("embedded health weight %+v differs from ledger status %+v", health.Weight, st.Weight)
		}
		if st.Weight.Participation != feedWeightParticipating || st.Weight.PerMinute != 500 || st.Weight.Charged == 0 {
			t.Fatalf("status weight = %+v, want an enabled 500 allocation with charges", st.Weight)
		}

		plain := newFeedRequestLedger(nil, false, clock.Now)
		plainOwner := newMarketFeedOwner(clock.Now, nil)
		plainOwner.SetRequestLedger(plain)
		sent := len(venue.requests("candleSnapshot"))
		<-plainOwner.ApplyGeneration(withFeedLedger(context.Background(), plain), rlRequirements(5, 60, "CCC"))
		if got := len(venue.requests("candleSnapshot")) - sent; got != 1 || plain.weightStatus().Participation != feedWeightNotEnrolled {
			t.Fatalf("an owner without a policy sent %d request(s) with participation %q, want 1 and disabled", got, plain.weightStatus().Participation)
		}
	})

	t.Run("RefusedCorrectionKeepsReadinessAndTimestamps", func(t *testing.T) {
		clock := &hlLotTestClock{now: base}
		newWTVenue(t, clock.Now)
		ledger := newFeedRequestLedger(wtPolicy(1000, 0, 0), false, clock.Now)
		owner := newMarketFeedOwner(clock.Now, nil)
		owner.SetRequestLedger(ledger)
		ctx := withFeedLedger(context.Background(), ledger)
		<-owner.ApplyGeneration(ctx, rlRequirements(5, 60, "AAA"))
		key := feedKeyFor("AAA", "1m")
		clock.Advance(90 * time.Second)
		jobs := owner.correctionPass(10)
		if len(jobs) != 1 {
			t.Fatalf("correction jobs = %d, want 1", len(jobs))
		}
		before := rlKeyStatus(t, owner, key)
		ledger.setLimits(wtPolicy(1, 0, 0))
		owner.runCorrectionJob(ctx, jobs[0])
		after := rlKeyStatus(t, owner, key)
		if !after.Ready || !after.LastRecvAt.Equal(before.LastRecvAt) || after.Status != before.Status {
			t.Fatalf("after a refused correction %+v, want %+v unchanged", after, before)
		}
		owner.feedMu.Lock()
		refused := owner.correction.Refused
		owner.feedMu.Unlock()
		if refused != 1 {
			t.Fatalf("correction refused = %d, want 1", refused)
		}
	})

	t.Run("LotMetadataLedgers", func(t *testing.T) {
		clock := &hlLotTestClock{now: base}
		venue := newWTVenue(t, clock.Now)
		local := newHLLotMetadataCache(nil)
		lctx := local.baseCtx
		ll := local.requestLedger()
		ll.clock = clock.Now
		metaW, _ := hlInfoWeight("meta", 0)
		for i := 0; i < hlLotMetadataLedgerPerMinute; i++ {
			if _, err := fetchHLLotMetadataRaw(lctx, venue.srv.URL); err != nil {
				t.Fatalf("meta read %d: %v", i+1, err)
			}
		}
		if _, err := fetchHLLotMetadataRaw(lctx, venue.srv.URL); !errors.Is(err, errFeedBudgetExhausted) {
			t.Fatalf("fifth meta read err = %v, want a refusal", err)
		}
		st := ll.weightStatus()
		if st.Participation != feedWeightParticipating || st.PerMinute != hlLotMetadataLedgerPerMinute*metaW || st.Startup != 0 || st.StartupLeft != 0 || st.Charged != hlLotMetadataLedgerPerMinute*metaW {
			t.Fatalf("local lot weight = %+v, want four meta costs per minute with no startup allowance", st)
		}
		if got := len(venue.requests("meta")); got != hlLotMetadataLedgerPerMinute {
			t.Fatalf("venue meta requests = %d, want %d", got, hlLotMetadataLedgerPerMinute)
		}

		shared := newFeedRequestLedger(wtPolicy(500, 0, 0), false, clock.Now)
		cache := newHLLotMetadataCache(withFeedLedger(context.Background(), shared))
		cache.clock = clock.Now
		cache.endpoint = func() string { return venue.srv.URL }
		cache.Ensure("BTC")
		if cache.requestLedger() != shared || shared.weightStatus().Totals.ChargedByType["meta"] != metaW {
			t.Fatalf("an embedded cache charged %+v, want one meta cost on the owner ledger", shared.weightStatus().Totals)
		}

		sent := len(venue.requests("meta"))
		frozen := newFrozenHLLotMetadataCache(map[string]int{"BTC": 5}, t.Logf)
		frozen.Ensure("ETH")
		frozen.Ensure("BTC")
		if got := len(venue.requests("meta")); got != sent {
			t.Fatalf("a frozen cache sent %d request(s)", got-sent)
		}
	})
}

func TestFeedWeightReloadContract(t *testing.T) {
	base := time.Unix(1890000000, 0).UTC()
	clock := &hlLotTestClock{now: base}
	venue := newWTVenue(t, clock.Now)
	dir := t.TempDir()
	consumer := filepath.Join(dir, "consumer.json")
	if err := os.WriteFile(consumer, []byte(`{"market_feed":"shared","shared_market_feed":{"primary_socket":"/run/a/feed.sock","backup_socket":"/run/b/feed.sock"},
"strategies":[{"id":"hl-btc","type":"perps","platform":"hyperliquid","script":"shared_scripts/check_hyperliquid.py","args":["sma_crossover","BTC","1h","--mode=paper"],"capital":1000,"max_drawdown_pct":50}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "feed.json")
	write := func(budget string) {
		t.Helper()
		body := fmt.Sprintf(`{"role":"feed","status_port":8191,"feed":{"source":"rest","socket_path":"/run/b/feed.sock","consumer_configs":[%q],"request_budget":%s},"strategies":[]}`, consumer, budget)
		if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"per_minute":120,"startup":60}`)
	if _, err := loadFeedRoleConfig(cfgPath); err == nil || !strings.Contains(err.Error(), "weight_per_minute") {
		t.Fatalf("a REST feed with request limits 120/60 and no weight policy loaded: %v", err)
	}
	for _, bad := range []string{
		`{"per_minute":120,"startup":60,"weight_per_minute":600,"weight_startup":400}`,
		`{"per_minute":120,"startup":60,"weight_per_minute":-1,"weight_startup":0,"weight_reserve":0}`,
		`{"per_minute":120,"startup":60,"weight_per_minute":9223372036854775807,"weight_startup":9223372036854775807,"weight_reserve":0}`,
		`{"per_minute":120,"startup":60,"weight_per_minute":900,"weight_startup":200,"weight_reserve":200}`,
	} {
		write(bad)
		if _, err := loadFeedRoleConfig(cfgPath); err == nil {
			t.Fatalf("an unsafe policy %s loaded", bad)
		}
	}
	write(`{"per_minute":120,"startup":60,"weight_per_minute":600,"weight_startup":400,"weight_reserve":200}`)
	cfg, err := loadFeedRoleConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	ledger := newFeedRequestLedger(cfg.Feed.RequestBudget, true, clock.Now)
	ledger.openStartup()
	owner := newMarketFeedOwner(clock.Now, nil)
	sealer := newFeedSealer(owner, feedSourceREST, "fixture", clock.Now, nil)
	sealer.ledger = ledger
	ctx := withFeedLedger(context.Background(), ledger)
	rt := &feedRuntime{configPath: cfgPath, cfg: cfg, owner: owner, sealer: sealer, ledger: ledger, ctx: ctx}
	boot := wtCtx(ledger, string(feedRestBootstrap), "boot")
	if _, err := fetchHyperliquidCandleSnapshot(boot, "BTC", "1m", base.Add(-59*time.Minute).UnixMilli(), base.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	entered, release := venue.hold("allMids")
	t.Cleanup(release)
	inflight := make(chan error, 1)
	go func() {
		_, err := fetchHyperliquidMidsCtx(wtCtx(ledger, string(feedRestMids), "mids"), []string{"BTC"})
		inflight <- err
	}()
	<-entered
	host := feedHostOf(venue.srv.URL)
	if _, err := ledger.acquire(host, "refresh", "allMids", "x", 0); err != nil {
		t.Fatal(err)
	}
	ledger.mu.Lock()
	ledger.hostLocked(host).until = base.Add(30 * time.Second)
	ledger.mu.Unlock()
	before := ledger.status()

	write(`{"per_minute":100,"startup":500,"weight_per_minute":25,"weight_startup":800,"weight_reserve":200}`)
	rt.reload()
	after := ledger.status()
	if after.StartupLeft > before.StartupLeft || after.Weight.StartupLeft > before.Weight.StartupLeft {
		t.Fatalf("reload reopened startup: requests %d -> %d, weight %d -> %d", before.StartupLeft, after.StartupLeft, before.Weight.StartupLeft, after.Weight.StartupLeft)
	}
	if after.Weight.PerMinute != 25 || after.Weight.Charged < before.Weight.Charged || after.Weight.InFlight != before.Weight.InFlight || after.Weight.Pending < before.Weight.Pending {
		t.Fatalf("reload changed retained state: before %+v after %+v", before.Weight, after.Weight)
	}
	if got := ledger.pausedUntil(host); !got.Equal(base.Add(30 * time.Second)) {
		t.Fatalf("reload cleared the host pause: %s", got)
	}
	clock.Advance(31 * time.Second)
	refresh := wtCtx(ledger, string(feedRestRefresh), "BTC|1m")
	if _, err := fetchHyperliquidCandleSnapshot(refresh, "BTC", "1m", base.Add(-59*time.Minute).UnixMilli(), base.UnixMilli()); feedWeightRefusalCause(err) != feedWeightCauseExhausted {
		t.Fatalf("a read under the lowered limit err = %v, want %s while retained weight does not fit", err, feedWeightCauseExhausted)
	}

	write(`{"per_minute":100,"startup":500,"weight_per_minute":600,"weight_startup":400}`)
	rt.reload()
	if got := ledger.status(); got.Weight.PerMinute != 25 || got.PerMinute != 100 {
		t.Fatalf("a rejected reload changed the limits to %d requests and %d weight", got.PerMinute, got.Weight.PerMinute)
	}
	release()
	if err := <-inflight; err != nil {
		t.Fatal(err)
	}
}
