package main

import (
	"fmt"
	"math"
	"testing"
	"time"
)

const testFeedIntervalMs int64 = 3_600_000

func testFeedKey() marketFeedKey {
	return marketFeedKey{Host: "https://api.hyperliquid.xyz", Namespace: feedNamespacePerps, Symbol: "BTC", Timeframe: "1h"}
}

func testRawBar(openMs int64, close float64) hlCandleRaw {
	return hlCandleRaw{
		OpenMs:   openMs,
		CloseMs:  openMs + testFeedIntervalMs - 1,
		HasClose: true,
		Open:     close - 1,
		High:     close + 2,
		Low:      close - 3,
		Close:    close,
		Volume:   10,
	}
}

func testRawSeries(startOpen int64, n int) []hlCandleRaw {
	out := make([]hlCandleRaw, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, testRawBar(startOpen+int64(i)*testFeedIntervalMs, 100+float64(i)))
	}
	return out
}

func TestMarketFeedHistoryContract(t *testing.T) {
	base := int64(1_700_000_000_000)
	now := time.UnixMilli(base + 30*testFeedIntervalMs).UTC()

	t.Run("empty startup history is never ready", func(t *testing.T) {
		st := newFeedKeyState(testFeedKey(), testFeedIntervalMs, 200)
		r := keyReadiness(st, now, true)
		if r.Ready {
			t.Fatalf("a key with no bars must not be ready")
		}
		if r.Status != feedStatusBootstrapping {
			t.Fatalf("status: got %q want %q", r.Status, feedStatusBootstrapping)
		}
	})

	t.Run("history below the 30-bar floor is not ready", func(t *testing.T) {
		st := newFeedKeyState(testFeedKey(), testFeedIntervalMs, 200)
		mergeRestRows(st, testRawSeries(base, 5), now)
		r := keyReadiness(st, now, true)
		if r.Ready {
			t.Fatalf("5 bars must not clear the readiness floor")
		}
		if r.Status != feedStatusShort {
			t.Fatalf("status: got %q want %q", r.Status, feedStatusShort)
		}
	})

	t.Run("short venue history is ready and flags coverage", func(t *testing.T) {
		st := newFeedKeyState(testFeedKey(), testFeedIntervalMs, 200)
		mergeRestRows(st, testRawSeries(base, 31), now.Add(-time.Minute))
		st.LastRecvAt = now
		r := keyReadiness(st, now, true)
		if !r.Ready {
			t.Fatalf("31 bars clear the floor: %+v", r)
		}
		if !r.CoverageShort {
			t.Fatalf("31 of 200 bars must flag coverage_short")
		}
	})

	t.Run("a socket bar fills a gap and keeps the ring sorted", func(t *testing.T) {
		st := newFeedKeyState(testFeedKey(), testFeedIntervalMs, 40)
		st.Status = feedStatusReady
		mergeRestRows(st, []hlCandleRaw{testRawBar(base, 100), testRawBar(base+2*testFeedIntervalMs, 102)}, now)
		if err := applySocketBar(st, feedBarFromRaw(testRawBar(base+testFeedIntervalMs, 101), feedBarSourceSocket, now)); err != nil {
			t.Fatalf("gap fill rejected: %v", err)
		}
		if len(st.Bars) != 3 {
			t.Fatalf("bars: got %d want 3", len(st.Bars))
		}
		for i := 1; i < len(st.Bars); i++ {
			if st.Bars[i-1].OpenMs >= st.Bars[i].OpenMs {
				t.Fatalf("bars are not sorted by open time: %+v", st.Bars)
			}
		}
	})

	t.Run("an out-of-order socket update lands in order", func(t *testing.T) {
		st := newFeedKeyState(testFeedKey(), testFeedIntervalMs, 40)
		st.Status = feedStatusReady
		applySocketBar(st, feedBarFromRaw(testRawBar(base+2*testFeedIntervalMs, 102), feedBarSourceSocket, now))
		applySocketBar(st, feedBarFromRaw(testRawBar(base, 100), feedBarSourceSocket, now))
		if st.Bars[0].OpenMs != base {
			t.Fatalf("oldest bar first: got %d", st.Bars[0].OpenMs)
		}
	})

	t.Run("a repeated update for the forming bar replaces it", func(t *testing.T) {
		st := newFeedKeyState(testFeedKey(), testFeedIntervalMs, 40)
		st.Status = feedStatusReady
		applySocketBar(st, feedBarFromRaw(testRawBar(base, 100), feedBarSourceSocket, now))
		applySocketBar(st, feedBarFromRaw(testRawBar(base, 107), feedBarSourceSocket, now.Add(time.Second)))
		if len(st.Bars) != 1 {
			t.Fatalf("the same open time must replace, not append: %d bars", len(st.Bars))
		}
		if st.Bars[0].Close != 107 {
			t.Fatalf("close: got %v want 107", st.Bars[0].Close)
		}
	})

	t.Run("an older view of the same bar is ignored", func(t *testing.T) {
		st := newFeedKeyState(testFeedKey(), testFeedIntervalMs, 40)
		st.Status = feedStatusReady
		fresh := testRawBar(base, 107)
		applySocketBar(st, feedBarFromRaw(fresh, feedBarSourceSocket, now))
		stale := fresh
		stale.CloseMs = fresh.CloseMs - 1000
		stale.Close = 90
		applySocketBar(st, feedBarFromRaw(stale, feedBarSourceSocket, now.Add(time.Second)))
		if st.Bars[0].Close != 107 {
			t.Fatalf("an older close timestamp must not overwrite the newer bar: %v", st.Bars[0].Close)
		}
	})

	t.Run("an older REST response never replaces a newer socket update", func(t *testing.T) {
		st := newFeedKeyState(testFeedKey(), testFeedIntervalMs, 40)
		st.Status = feedStatusReady
		socketAt := now
		applySocketBar(st, feedBarFromRaw(testRawBar(base, 999), feedBarSourceSocket, socketAt))
		out := mergeRestRows(st, []hlCandleRaw{testRawBar(base, 100)}, socketAt.Add(-time.Minute))
		if out.Kept != 1 || out.Replaced != 0 {
			t.Fatalf("stale REST row must be kept out: %+v", out)
		}
		if st.Bars[0].Close != 999 {
			t.Fatalf("socket bar overwritten by an older REST response: %v", st.Bars[0].Close)
		}
	})

	t.Run("a newer REST response repairs the stored bar", func(t *testing.T) {
		st := newFeedKeyState(testFeedKey(), testFeedIntervalMs, 40)
		st.Status = feedStatusReady
		applySocketBar(st, feedBarFromRaw(testRawBar(base, 999), feedBarSourceSocket, now.Add(-time.Hour)))
		out := mergeRestRows(st, []hlCandleRaw{testRawBar(base, 100)}, now)
		if out.Replaced != 1 {
			t.Fatalf("a newer REST response must repair the bar: %+v", out)
		}
	})

	t.Run("bootstrapping buffers socket bars until the history lands", func(t *testing.T) {
		st := newFeedKeyState(testFeedKey(), testFeedIntervalMs, 40)
		applySocketBar(st, feedBarFromRaw(testRawBar(base+3*testFeedIntervalMs, 103), feedBarSourceSocket, now))
		if len(st.Bars) != 0 || len(st.Pending) != 1 {
			t.Fatalf("socket bars must buffer while bootstrapping: bars=%d pending=%d", len(st.Bars), len(st.Pending))
		}
		mergeRestRows(st, testRawSeries(base, 3), now)
		drainPendingSocketBars(st)
		if len(st.Bars) != 4 {
			t.Fatalf("buffered bar must land after the history merge: %d", len(st.Bars))
		}
	})

	t.Run("invalid bars are rejected and never stored", func(t *testing.T) {
		bad := map[string]hlCandleRaw{
			"no close timestamp": {OpenMs: base, HasClose: false, Open: 1, High: 2, Low: 0.5, Close: 1.5, Volume: 1},
			"close before open":  {OpenMs: base, CloseMs: base - 1, HasClose: true, Open: 1, High: 2, Low: 0.5, Close: 1.5, Volume: 1},
			"spans two bars":     {OpenMs: base, CloseMs: base + 2*testFeedIntervalMs, HasClose: true, Open: 1, High: 2, Low: 0.5, Close: 1.5, Volume: 1},
			"zero price":         {OpenMs: base, CloseMs: base + 10, HasClose: true, Open: 0, High: 2, Low: 0.5, Close: 1.5, Volume: 1},
			"not finite":         {OpenMs: base, CloseMs: base + 10, HasClose: true, Open: math.NaN(), High: 2, Low: 0.5, Close: 1.5, Volume: 1},
			"high below close":   {OpenMs: base, CloseMs: base + 10, HasClose: true, Open: 1, High: 1.2, Low: 0.5, Close: 1.5, Volume: 1},
			"low above open":     {OpenMs: base, CloseMs: base + 10, HasClose: true, Open: 1, High: 2, Low: 1.4, Close: 1.5, Volume: 1},
			"negative volume":    {OpenMs: base, CloseMs: base + 10, HasClose: true, Open: 1, High: 2, Low: 0.5, Close: 1.5, Volume: -1},
		}
		for name, raw := range bad {
			st := newFeedKeyState(testFeedKey(), testFeedIntervalMs, 40)
			st.Status = feedStatusReady
			if err := applySocketBar(st, feedBarFromRaw(raw, feedBarSourceSocket, now)); err == nil {
				t.Fatalf("%s: expected the bar to be rejected", name)
			}
			if len(st.Bars) != 0 {
				t.Fatalf("%s: an invalid bar reached the ring", name)
			}
			out := mergeRestRows(st, []hlCandleRaw{raw}, now)
			if out.Invalid != 1 || len(st.Bars) != 0 {
				t.Fatalf("%s: an invalid REST row reached the ring: %+v", name, out)
			}
		}
	})

	t.Run("a key whose newest bar aged out is stale", func(t *testing.T) {
		st := newFeedKeyState(testFeedKey(), testFeedIntervalMs, 40)
		mergeRestRows(st, testRawSeries(base, 40), now)
		late := time.UnixMilli(st.Bars[len(st.Bars)-1].CloseMs + 3*testFeedIntervalMs).UTC()
		r := keyReadiness(st, late, false)
		if !r.Stale || r.Ready {
			t.Fatalf("an aged-out key must be stale and unready: %+v", r)
		}
	})

	t.Run("a silent key on a connected socket is stale", func(t *testing.T) {
		st := newFeedKeyState(testFeedKey(), testFeedIntervalMs, 40)
		recvAt := now
		mergeRestRows(st, testRawSeries(base, 40), recvAt)
		fresh := time.UnixMilli(st.Bars[len(st.Bars)-1].CloseMs + 1000).UTC()
		st.LastRecvAt = fresh.Add(-feedSilenceLimit(testFeedIntervalMs) - time.Minute)
		r := keyReadiness(st, fresh, true)
		if !r.Stale {
			t.Fatalf("a connected but silent key must be stale: %+v", r)
		}
		if quiet := keyReadiness(st, fresh, false); quiet.Stale {
			t.Fatalf("silence must not mark a key stale while disconnected: %+v", quiet)
		}
	})

	t.Run("the ring never grows past its capacity", func(t *testing.T) {
		st := newFeedKeyState(testFeedKey(), testFeedIntervalMs, 40)
		st.Status = feedStatusReady
		mergeRestRows(st, testRawSeries(base, 400), now)
		if len(st.Bars) > st.Capacity {
			t.Fatalf("bars %d exceed capacity %d", len(st.Bars), st.Capacity)
		}
		if st.Bars[len(st.Bars)-1].OpenMs != base+399*testFeedIntervalMs {
			t.Fatalf("the newest bar must survive trimming: %d", st.Bars[len(st.Bars)-1].OpenMs)
		}
	})
}

func TestMarketFeedReadyFloorNeverExceedsRequired(t *testing.T) {
	for _, required := range []int{1, 5, 29, 30, 200} {
		floor := feedReadyFloor(required)
		if floor > required {
			t.Fatalf("required=%d floor=%d: the floor must never exceed the requirement", required, floor)
		}
		if required >= feedMinReadyBars && floor != feedMinReadyBars {
			t.Fatalf("required=%d floor=%d: the floor is the %d-bar candle guard", required, floor, feedMinReadyBars)
		}
	}
}

func TestMarketFeedMidsOnlyTrackRequiredCoins(t *testing.T) {
	owner := newMarketFeedOwner(func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }, nil)
	owner.midCoins = map[string]bool{"BTC": true}
	owner.IngestMids(map[string]float64{"BTC": 100, "ETH": 200, "SOL": 0}, time.Unix(1_700_000_000, 0), "ws")
	if len(owner.mids) != 1 {
		t.Fatalf("only required coins are tracked: %v", owner.mids)
	}
	if owner.mids["BTC"].Px != 100 {
		t.Fatalf("BTC mid: got %v want 100", owner.mids["BTC"].Px)
	}
}

func TestFeedDecisionAgeLimitNeverBelowTheFloor(t *testing.T) {
	cases := map[int]time.Duration{
		0:    feedDecisionAgeFloor,
		60:   feedDecisionAgeFloor,
		120:  feedDecisionAgeFloor,
		300:  150 * time.Second,
		1200: 600 * time.Second,
	}
	for interval, want := range cases {
		if got := feedDecisionAgeLimit(interval); got != want {
			t.Fatalf("interval=%d: got %s want %s", interval, got, want)
		}
	}
}

func TestFeedBackoffClimbsAndCaps(t *testing.T) {
	d := time.Duration(0)
	seen := []time.Duration{}
	for i := 0; i < 8; i++ {
		d = feedNextBackoff(d)
		seen = append(seen, d)
	}
	if seen[0] != feedSocketBackoffMin {
		t.Fatalf("first backoff: got %s want %s", seen[0], feedSocketBackoffMin)
	}
	if seen[len(seen)-1] != feedSocketBackoffMax {
		t.Fatalf("backoff must cap at %s, got %s (%v)", feedSocketBackoffMax, seen[len(seen)-1], seen)
	}
	for i := 1; i < len(seen); i++ {
		if seen[i] < seen[i-1] {
			t.Fatalf("backoff must not shrink: %v", seen)
		}
	}
}

func TestFeedWebsocketURLDerivesFromTheInfoHost(t *testing.T) {
	orig := hlMainnetURL
	defer func() { hlMainnetURL = orig }()
	cases := map[string]string{
		"https://api.hyperliquid.xyz": "wss://api.hyperliquid.xyz/ws",
		"http://127.0.0.1:8080":       "ws://127.0.0.1:8080/ws",
	}
	for in, want := range cases {
		hlMainnetURL = in
		if got := hlFeedWebsocketURL(); got != want {
			t.Fatalf("%s: got %s want %s", in, got, want)
		}
	}
}

func TestFeedAlertsNeverBlockTheOwner(t *testing.T) {
	owner := newMarketFeedOwner(nil, nil)
	for i := 0; i < feedAlertChannelDepth*2; i++ {
		owner.raiseAlert(testFeedKey(), "bootstrap_failed", fmt.Sprintf("attempt %d", i))
	}
	drained := owner.DrainAlerts()
	if len(drained) != feedAlertChannelDepth {
		t.Fatalf("drained %d alerts, want the %d-deep buffer", len(drained), feedAlertChannelDepth)
	}
	if len(owner.DrainAlerts()) != 0 {
		t.Fatalf("draining twice must leave the channel empty")
	}
}
