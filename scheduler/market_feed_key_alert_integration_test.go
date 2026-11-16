package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	kaUnready = "**MARKET FEED KEY UNREADY**"
	kaReady   = "**MARKET FEED KEY READY AGAIN**"
)

type kaVenue struct {
	srv  *httptest.Server
	mu   sync.Mutex
	fail map[string]bool
}

func newKAVenue(t *testing.T) *kaVenue {
	t.Helper()
	v := &kaVenue{fail: map[string]bool{}}
	v.srv = httptest.NewServer(http.HandlerFunc(v.handle))
	t.Cleanup(v.srv.Close)
	return v
}

func (v *kaVenue) setFail(coin string, fail bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.fail[coin] = fail
}

func (v *kaVenue) handle(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Type string `json:"type"`
		Req  struct {
			Coin      string `json:"coin"`
			StartTime int64  `json:"startTime"`
			EndTime   int64  `json:"endTime"`
		} `json:"req"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	v.mu.Lock()
	fail := v.fail[req.Req.Coin]
	v.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch req.Type {
	case "candleSnapshot":
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`"venue outage"`))
			return
		}
		const step = int64(60_000)
		first := (req.Req.StartTime + step - 1) / step * step
		rows := []map[string]any{}
		for open := first; open <= req.Req.EndTime && len(rows) < 5000; open += step {
			rows = append(rows, map[string]any{"t": open, "T": open + step - 1, "o": "100", "h": "102", "l": "99", "c": "101", "v": "5"})
		}
		json.NewEncoder(w).Encode(rows)
	case "allMids":
		w.Write([]byte(`{"BTC":"101.5","ETH":"11"}`))
	case "meta":
		w.Write([]byte(hlMetaBody(`{"name":"BTC","szDecimals":5}`)))
	default:
		w.Write([]byte(`[]`))
	}
}

type kaFixture struct {
	t      *testing.T
	ctx    context.Context
	source string
	clock  *hlLotTestClock
	venue  *kaVenue
	ledger *feedRequestLedger
	owner  *marketFeedOwner
	sealer *feedSealer

	beforePrepare func(deadline int64)
	afterPrepare  func(deadline int64)

	mu    sync.Mutex
	logs  []string
	sent  []string
	hooks []string
}

func newKAFixture(t *testing.T, source string, start time.Time) *kaFixture {
	t.Helper()
	f := &kaFixture{t: t, source: source, clock: &hlLotTestClock{now: start.Add(-time.Hour)}}
	f.venue = newKAVenue(t)
	useHLInfoURLs(t, f.venue.srv.URL, f.venue.srv.URL+"/alt-host")
	f.ledger = newFeedRequestLedger(nil, false, f.clock.Now)
	f.ctx = withFeedLedger(context.Background(), f.ledger)
	f.owner = newMarketFeedOwner(f.clock.Now, f.logf)
	f.restart()
	return f
}

func (f *kaFixture) logf(format string, args ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logs = append(f.logs, fmt.Sprintf(format, args...))
}

func (f *kaFixture) restart() {
	s := newFeedSealer(f.owner, f.source, "inst-"+f.source, f.clock.Now, f.logf)
	s.ledger = f.ledger
	var base feedPrepareFunc
	if f.source == feedSourceREST {
		src := newFeedRESTSource(f.owner, f.logf)
		src.backoff = time.Millisecond
		base = src.prepare
	} else {
		base = websocketFeedPrepare(f.owner)
	}
	s.prepareFn = func(ctx context.Context, deadline int64, reqs cycleMarketRequirements, coverage feedRequirements) feedPrepareReport {
		if f.beforePrepare != nil {
			f.beforePrepare(deadline)
		}
		rep := base(ctx, deadline, reqs, coverage)
		if f.afterPrepare != nil {
			f.afterPrepare(deadline)
		}
		return rep
	}
	s.alertHook = func(msg string) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.hooks = append(f.hooks, msg)
	}
	s.keyAlertSend = func(msg string) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.sent = append(f.sent, msg)
	}
	f.sealer = s
}

func (f *kaFixture) apply(req feedRequirements) {
	f.t.Helper()
	select {
	case <-f.owner.ApplyGeneration(f.ctx, req):
	case <-time.After(10 * time.Second):
		f.t.Fatal("feed generation did not publish")
	}
}

func (f *kaFixture) install(req feedRequirements, cadences ...int) {
	f.t.Helper()
	f.apply(req)
	f.sealer.setGeneration(req, cadences, f.clock.Now().Add(-time.Hour))
}

func (f *kaFixture) sealAtOffset(deadline int64, offset time.Duration) []string {
	f.t.Helper()
	f.mu.Lock()
	before := len(f.sent)
	f.mu.Unlock()
	f.clock.mu.Lock()
	f.clock.now = time.Unix(deadline, 0).UTC().Add(offset)
	f.clock.mu.Unlock()
	f.sealer.sealOne(f.ctx, deadline)
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sent[before:]...)
}

func (f *kaFixture) seal(deadline int64) []string {
	f.t.Helper()
	return f.sealAtOffset(deadline, 5*time.Second)
}

func (f *kaFixture) stored(deadline int64) *feedSeal {
	f.sealer.mu.Lock()
	defer f.sealer.mu.Unlock()
	return f.sealer.ring[deadline]
}

func (f *kaFixture) episode(key marketFeedKey) (feedKeyEpisode, bool) {
	f.sealer.mu.Lock()
	defer f.sealer.mu.Unlock()
	ep, ok := f.sealer.keyEpisodes[key]
	if !ok {
		return feedKeyEpisode{}, false
	}
	return *ep, true
}

func (f *kaFixture) logsContaining(sub string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, l := range f.logs {
		if strings.Contains(l, sub) {
			out = append(out, l)
		}
	}
	return out
}

func (f *kaFixture) hooksContaining(sub string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, h := range f.hooks {
		if strings.Contains(h, sub) {
			n++
		}
	}
	return n
}

func kaRequirements(lookback int, cadence int, keys ...marketFeedKey) feedRequirements {
	req := feedRequirements{Keys: map[marketFeedKey]int{}, Funding: map[string]feedFundingNeed{}, Strategies: map[string]feedStrategyRequirement{}, KeyCadences: map[marketFeedKey][]int{}}
	for _, key := range keys {
		req.Keys[key] = lookback
		req.KeyCadences[key] = []int{cadence}
		id := "s-" + key.Host + "-" + key.Symbol
		req.Strategies[id] = feedStrategyRequirement{ID: id, Signal: key, SignalLookback: lookback, Coin: key.Symbol}
	}
	req.finalize()
	return req
}

func kaExpectNone(t *testing.T, label string, msgs []string) {
	t.Helper()
	if len(msgs) != 0 {
		t.Fatalf("%s: key alerts %q, want none", label, msgs)
	}
}

func kaExpectOne(t *testing.T, label, prefix string, msgs []string, contains ...string) string {
	t.Helper()
	if len(msgs) != 1 || !strings.HasPrefix(msgs[0], prefix) {
		t.Fatalf("%s: key alerts %q, want one %s", label, msgs, prefix)
	}
	for _, c := range contains {
		if !strings.Contains(msgs[0], c) {
			t.Fatalf("%s: alert %q does not contain %q", label, msgs[0], c)
		}
	}
	return msgs[0]
}

func kaCheckParts(t *testing.T, f *kaFixture, label, prefix string, msgs []string, keys []marketFeedKey, logMark string) {
	t.Helper()
	if len(msgs) < 2 {
		t.Fatalf("%s: %d message(s), want the alert split into parts", label, len(msgs))
	}
	listed := map[string]int{}
	omitted := 0
	for i, m := range msgs {
		if len(m) > discordCharLimit {
			t.Fatalf("%s: part %d is %d bytes, over %d", label, i+1, len(m), discordCharLimit)
		}
		if !strings.HasPrefix(m, prefix) || !strings.Contains(m, fmt.Sprintf("%d candle key(s)", len(keys))) ||
			!strings.Contains(m, fmt.Sprintf(" Part %d/%d. Keys: ", i+1, len(msgs))) {
			t.Fatalf("%s: part %d = %q, want the prefix, the total key count and its part label", label, i+1, m)
		}
		for _, key := range keys {
			listed[key.String()] += strings.Count(m, key.String()+" status=")
		}
		if j := strings.Index(m, "; and "); j >= 0 {
			if _, err := fmt.Sscanf(m[j:], "; and %d more key(s)", &omitted); err != nil || i != len(msgs)-1 {
				t.Fatalf("%s: part %d has a malformed or misplaced omitted note: %q", label, i+1, m[j:])
			}
		}
	}
	named := 0
	for _, key := range keys {
		c := listed[key.String()]
		if c > 1 {
			t.Fatalf("%s: key %s is listed %d times", label, key, c)
		}
		named += c
	}
	if named+omitted != len(keys) {
		t.Fatalf("%s: %d listed + %d omitted, want %d keys", label, named, omitted, len(keys))
	}
	lines := f.logsContaining("[feed-key-alert] " + prefix)
	if len(lines) != 1 {
		t.Fatalf("%s: %d [feed-key-alert] log line(s), want one full record", label, len(lines))
	}
	for _, key := range keys {
		if !strings.Contains(lines[0], key.String()+" status=") {
			t.Fatalf("%s: the log line does not name %s", label, key)
		}
	}
	if strings.Count(lines[0], logMark) != len(keys) {
		t.Fatalf("%s: the log line carries %q %d time(s), want %d", label, logMark, strings.Count(lines[0], logMark), len(keys))
	}
}

const kaStep = int64(600)

func TestFeedKeyAlertIntegration(t *testing.T) {
	start := time.Unix(1890000000, 0).UTC()
	k := start.Unix()

	t.Run("WebsocketEpisodeAlertsOnceAndRecoversOnce", func(t *testing.T) {
		f := newKAFixture(t, feedSourceWebsocket, start)
		btc := feedKeyFor("BTC", "1m")
		f.install(kaRequirements(100, 60, btc), 60)

		kaExpectNone(t, "healthy seal", f.seal(k))
		f.venue.setFail("BTC", true)
		kaExpectNone(t, "first failure", f.seal(k+kaStep))
		kaExpectNone(t, "second failure", f.seal(k+2*kaStep))
		f.venue.setFail("BTC", false)
		kaExpectNone(t, "ready before the threshold", f.seal(k+3*kaStep))
		if _, open := f.episode(btc); open {
			t.Fatal("a ready seal before the threshold left an episode open")
		}

		f.venue.setFail("BTC", true)
		kaExpectNone(t, "streak restarts 1", f.seal(k+4*kaStep))
		kaExpectNone(t, "streak restarts 2", f.seal(k+5*kaStep))
		d := k + 6*kaStep
		msg := kaExpectOne(t, "third failure", kaUnready, f.seal(d),
			"[websocket]", "instance inst-websocket", fmt.Sprintf("deadline %d (%s)", d, time.Unix(d, 0).UTC().Format(time.RFC3339)),
			btc.String()+" status=stale detail=", "bars=", "/100", "3 consecutive eligible deadlines",
			"unless another feed supplies ready inputs", "protection continues on verified inputs")
		if len(f.logsContaining("[feed-key-alert] "+msg)) != 1 {
			t.Fatalf("the unready transition has no single local log record")
		}
		kaExpectNone(t, "fourth failure", f.seal(k+7*kaStep))
		if ep, _ := f.episode(btc); ep.streak != scriptFailureAlertThreshold || !ep.alerted {
			t.Fatalf("episode after four failures = %+v, want the capped streak and alerted", ep)
		}

		f.venue.setFail("BTC", false)
		d = k + 8*kaStep
		kaExpectOne(t, "first ready seal", kaReady, f.seal(d),
			"[websocket]", "instance inst-websocket", fmt.Sprintf("deadline %d", d), btc.String()+" status=ready", "bars=")
		kaExpectNone(t, "second ready seal", f.seal(k+9*kaStep))
		if f.hooksContaining("KEY UNREADY")+f.hooksContaining("KEY READY AGAIN") != 0 {
			t.Fatalf("key alerts went through the direct alert hook: %q", f.hooks)
		}
	})

	t.Run("RestKeyIsObservedOnlyAtDueDeadlines", func(t *testing.T) {
		f := newKAFixture(t, feedSourceREST, start)
		btc := feedKeyFor("BTC", "1m")
		f.install(kaRequirements(100, 300, btc), 60, 300)

		kaExpectNone(t, "due healthy seal", f.seal(k))
		if s := f.stored(k); s == nil || s.KeysReady != 1 {
			t.Fatalf("due healthy seal = %+v, want one ready key", s)
		}
		for _, d := range []int64{k + 60, k + 120, k + 180, k + 240} {
			kaExpectNone(t, "non-due healthy seal", f.seal(d))
			if s := f.stored(d); s == nil || s.KeysReady != 0 {
				t.Fatalf("non-due seal %d = %+v, want the key sealed not ready", d, s)
			}
		}
		if _, open := f.episode(btc); open {
			t.Fatal("non-due not_due seals opened an episode")
		}

		f.venue.setFail("BTC", true)
		kaExpectNone(t, "due failure 1", f.seal(k+300))
		kaExpectNone(t, "non-due after failure", f.seal(k+360))
		kaExpectNone(t, "non-due after failure", f.seal(k+420))
		kaExpectNone(t, "due failure 2", f.seal(k+600))
		kaExpectNone(t, "non-due after failure", f.seal(k+660))
		kaExpectOne(t, "due failure 3", kaUnready, f.seal(k+900),
			"[rest]", "instance inst-rest", fmt.Sprintf("deadline %d", k+900), btc.String()+" status=failed", "refresh for deadline", "bars=")
		kaExpectNone(t, "due failure 4", f.seal(k+1200))

		f.venue.setFail("BTC", false)
		kaExpectNone(t, "non-due seal after the venue heals", f.seal(k+1260))
		if ep, ok := f.episode(btc); !ok || !ep.alerted {
			t.Fatalf("a non-due seal ended the episode: %+v %t", ep, ok)
		}
		kaExpectOne(t, "due ready seal", kaReady, f.seal(k+1500), "[rest]", btc.String()+" status=ready")
	})

	t.Run("FailedMissedAndDuplicateSealsLeaveKeyStateUnchanged", func(t *testing.T) {
		f := newKAFixture(t, feedSourceWebsocket, start)
		btc := feedKeyFor("BTC", "1m")
		f.install(kaRequirements(100, 60, btc), 60)
		missedOffset := f.sealer.settle + f.sealer.prepare + time.Second
		hardOffset := f.sealer.settle + f.sealer.prepare + f.sealer.grace + time.Second

		failingPaths := func(label string, base int64, dup int64) {
			t.Helper()
			kaExpectNone(t, label+" missed", f.sealAtOffset(base, missedOffset))
			f.afterPrepare = func(d int64) {
				f.clock.mu.Lock()
				f.clock.now = time.Unix(d, 0).UTC().Add(hardOffset)
				f.clock.mu.Unlock()
			}
			kaExpectNone(t, label+" hard deadline", f.seal(base+kaStep))
			var restore func()
			f.afterPrepare = func(int64) {
				f.owner.feedMu.Lock()
				defer f.owner.feedMu.Unlock()
				st := f.owner.keys[btc]
				last := &st.Bars[len(st.Bars)-1]
				orig := last.Close
				last.Close = math.NaN()
				restore = func() {
					f.owner.feedMu.Lock()
					defer f.owner.feedMu.Unlock()
					last.Close = orig
				}
			}
			kaExpectNone(t, label+" encoding failure", f.seal(base+2*kaStep))
			f.afterPrepare = nil
			restore()
			for _, d := range []int64{base, base + kaStep, base + 2*kaStep} {
				if f.stored(d) != nil {
					t.Fatalf("%s: deadline %d stored a seal, want none", label, d)
				}
			}
			before := f.stored(dup)
			kaExpectNone(t, label+" duplicate", f.seal(dup))
			if f.stored(dup) != before {
				t.Fatalf("%s: duplicate seal replaced the stored seal", label)
			}
			if len(f.logsContaining("status=failed reason=")) == 0 || len(f.logsContaining("status=duplicate")) == 0 {
				t.Fatalf("%s: the encoding failure or duplicate path did not run", label)
			}
		}

		f.venue.setFail("BTC", true)
		kaExpectNone(t, "failure 1", f.seal(k))
		kaExpectNone(t, "failure 2", f.seal(k+kaStep))
		failingPaths("unready", k+2*kaStep, k+kaStep)
		if ep, _ := f.episode(btc); ep.streak != 2 || ep.alerted {
			t.Fatalf("episode after the rejected seals = %+v, want streak 2 and no alert", ep)
		}
		kaExpectOne(t, "failure 3", kaUnready, f.seal(k+5*kaStep), btc.String())
		if f.hooksContaining("MARKET FEED SEAL MISSED") != 1 || f.hooksContaining("MARKET FEED SEALING RECOVERED") != 1 {
			t.Fatalf("seal-missed alerts = %q, want one missed and one recovered", f.hooks)
		}

		f.venue.setFail("BTC", false)
		failingPaths("ready", k+6*kaStep, k+5*kaStep)
		if ep, ok := f.episode(btc); !ok || !ep.alerted {
			t.Fatalf("rejected ready seals ended the episode: %+v %t", ep, ok)
		}
		kaExpectOne(t, "eligible ready seal", kaReady, f.seal(k+9*kaStep), btc.String())
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, s := range f.sent {
			if strings.Contains(s, "SEAL MISSED") || strings.Contains(s, "SEALING RECOVERED") {
				t.Fatalf("seal-missed alert went through the key-alert queue: %q", s)
			}
		}
	})

	t.Run("GroupsSortsAndKeepsFullKeyIdentity", func(t *testing.T) {
		f := newKAFixture(t, feedSourceWebsocket, start)
		btc, eth := feedKeyFor("BTC", "1m"), feedKeyFor("ETH", "1m")
		f.install(kaRequirements(100, 60, eth, btc), 60)

		f.venue.setFail("BTC", true)
		f.venue.setFail("ETH", true)
		kaExpectNone(t, "both fail 1", f.seal(k))
		kaExpectNone(t, "both fail 2", f.seal(k+kaStep))
		msg := kaExpectOne(t, "both fail 3", kaUnready, f.seal(k+2*kaStep), "2 candle key(s)")
		if strings.Index(msg, btc.String()) < 0 || strings.Index(msg, btc.String()) > strings.Index(msg, eth.String()) {
			t.Fatalf("grouped unready alert %q is not sorted BTC before ETH", msg)
		}
		f.venue.setFail("BTC", false)
		f.venue.setFail("ETH", false)
		msg = kaExpectOne(t, "both recover", kaReady, f.seal(k+3*kaStep), "2 candle key(s)")
		if strings.Index(msg, btc.String()) > strings.Index(msg, eth.String()) {
			t.Fatalf("grouped recovery alert %q is not sorted", msg)
		}

		f.venue.setFail("BTC", true)
		kaExpectNone(t, "mixed 1", f.seal(k+4*kaStep))
		f.venue.setFail("ETH", true)
		kaExpectNone(t, "mixed 2", f.seal(k+5*kaStep))
		kaExpectOne(t, "mixed 3", kaUnready, f.seal(k+6*kaStep), btc.String())
		f.venue.setFail("BTC", false)
		mixed := f.seal(k + 7*kaStep)
		if len(mixed) != 2 || !strings.HasPrefix(mixed[0], kaUnready) || !strings.HasPrefix(mixed[1], kaReady) {
			t.Fatalf("mixed transitions = %q, want one unready message then one recovery message", mixed)
		}
		if !strings.Contains(mixed[0], eth.String()) || strings.Contains(mixed[0], btc.String()) ||
			!strings.Contains(mixed[1], btc.String()) || strings.Contains(mixed[1], eth.String()) {
			t.Fatalf("mixed transitions = %q, want ETH unready and BTC recovered", mixed)
		}

		g := newKAFixture(t, feedSourceWebsocket, start)
		mainKey := feedKeyFor("BTC", "1m")
		alt := marketFeedKey{Host: hlTestnetURL, Namespace: feedNamespacePerps, Symbol: "BTC", Timeframe: "1m"}
		if mainKey.PayloadID() != alt.PayloadID() || mainKey == alt {
			t.Fatal("fixture keys must share symbol and timeframe and differ by host")
		}
		g.install(kaRequirements(100, 60, mainKey, alt), 60)
		breakAlt := true
		g.afterPrepare = func(int64) {
			if breakAlt {
				g.owner.markKeyUnready(alt, feedStatusFailed, "alternate host refused the read")
			}
		}
		kaExpectNone(t, "alt fail 1", g.seal(k))
		kaExpectNone(t, "alt fail 2", g.seal(k+kaStep))
		msg = kaExpectOne(t, "alt fail 3", kaUnready, g.seal(k+2*kaStep), "1 candle key(s)", alt.String()+" status=failed detail=\"alternate host refused the read\"")
		if strings.Contains(msg, mainKey.String()+" ") {
			t.Fatalf("alert %q names the healthy key on the other host", msg)
		}
		if _, open := g.episode(mainKey); open {
			t.Fatal("the healthy key on the other host has an episode")
		}
		breakAlt = false
		kaExpectOne(t, "alt recovers", kaReady, g.seal(k+3*kaStep), alt.String()+" status=ready")
	})

	t.Run("OversizedAlertsSplitWithinDiscordLimit", func(t *testing.T) {
		f := newKAFixture(t, feedSourceWebsocket, start)
		keys := make([]marketFeedKey, 30)
		for i := range keys {
			keys[i] = feedKeyFor(fmt.Sprintf("K%02d", i), "1m")
		}
		f.install(kaRequirements(100, 60, keys...), 60)
		detail := "refresh failed: " + strings.Repeat("venue outage text ", 8)
		if len(detail) < 150 {
			t.Fatalf("fixture detail is %d bytes, want at least 150", len(detail))
		}
		broken := true
		f.afterPrepare = func(int64) {
			if broken {
				for _, key := range keys {
					f.owner.markKeyUnready(key, feedStatusFailed, detail)
				}
			}
		}
		kaExpectNone(t, "many fail 1", f.seal(k))
		kaExpectNone(t, "many fail 2", f.seal(k+kaStep))
		kaCheckParts(t, f, "many fail 3", kaUnready, f.seal(k+2*kaStep), keys, fmt.Sprintf("detail=%q", detail))
		broken = false
		kaCheckParts(t, f, "many recover", kaReady, f.seal(k+3*kaStep), keys, "status=ready")

		g := newKAFixture(t, feedSourceWebsocket, start)
		btc := feedKeyFor("BTC", "1m")
		g.install(kaRequirements(100, 60, btc), 60)
		huge := "refresh failed: " + strings.Repeat("long venue error body ", 120)
		if len(huge) <= discordCharLimit {
			t.Fatalf("fixture detail is %d bytes, want more than %d", len(huge), discordCharLimit)
		}
		g.afterPrepare = func(int64) { g.owner.markKeyUnready(btc, feedStatusFailed, huge) }
		kaExpectNone(t, "huge fail 1", g.seal(k))
		kaExpectNone(t, "huge fail 2", g.seal(k+kaStep))
		msg := kaExpectOne(t, "huge fail 3", kaUnready, g.seal(k+2*kaStep), "1 candle key(s)", btc.String()+" status=failed detail=")
		if len(msg) > discordCharLimit {
			t.Fatalf("single-key alert is %d bytes, over %d", len(msg), discordCharLimit)
		}
		full := g.logsContaining("[feed-key-alert] " + kaUnready)
		if len(full) != 1 || !strings.Contains(full[0], fmt.Sprintf("detail=%q", huge)) {
			t.Fatalf("single-key log = %d line(s), want one line with the full detail", len(full))
		}
	})

	t.Run("ReloadCleanupRetentionAndInFlightSeals", func(t *testing.T) {
		f := newKAFixture(t, feedSourceWebsocket, start)
		btc, eth := feedKeyFor("BTC", "1m"), feedKeyFor("ETH", "1m")
		both := kaRequirements(100, 60, btc, eth)
		f.install(both, 60)

		f.venue.setFail("BTC", true)
		f.seal(k)
		f.seal(k + kaStep)
		kaExpectOne(t, "btc alert", kaUnready, f.seal(k+2*kaStep), btc.String())

		f.install(kaRequirements(100, 60, eth), 60)
		removed := f.logsContaining("requirements reload removed 1 key(s)")
		if len(removed) != 1 || !strings.Contains(removed[0], btc.String()) {
			t.Fatalf("reload removal log = %q, want one record naming %s", removed, btc)
		}
		if _, open := f.episode(btc); open {
			t.Fatal("reload left the removed key's episode")
		}
		kaExpectNone(t, "seal after removal", f.seal(k+3*kaStep))

		f.install(both, 60)
		kaExpectNone(t, "re-added fail 1", f.seal(k+4*kaStep))
		kaExpectNone(t, "re-added fail 2", f.seal(k+5*kaStep))
		kaExpectOne(t, "re-added fail 3", kaUnready, f.seal(k+6*kaStep), btc.String())

		changed := kaRequirements(120, 120, btc, eth)
		f.install(changed, 60, 120)
		if ep, ok := f.episode(btc); !ok || !ep.alerted || ep.streak != scriptFailureAlertThreshold {
			t.Fatalf("lookback/cadence reload changed the retained episode: %+v %t", ep, ok)
		}
		kaExpectNone(t, "retained key still failing", f.seal(k+7*kaStep))

		entered, release := make(chan struct{}), make(chan struct{})
		f.beforePrepare = func(int64) {
			close(entered)
			<-release
		}
		f.venue.setFail("BTC", false)
		done := make(chan []string)
		go func() { done <- f.seal(k + 8*kaStep) }()
		<-entered
		f.beforePrepare = nil
		f.sealer.setGeneration(kaRequirements(150, 120, btc, eth), []int{60, 120}, f.clock.Now().Add(-time.Hour))
		close(release)
		kaExpectNone(t, "old-revision ready seal", <-done)
		if s := f.stored(k + 8*kaStep); s == nil || s.KeysReady != 2 {
			t.Fatalf("in-flight seal = %+v, want it stored with both keys ready", s)
		}
		if ep, ok := f.episode(btc); !ok || !ep.alerted {
			t.Fatalf("an in-flight seal from a superseded revision cleared the retained episode: %+v %t", ep, ok)
		}
		kaExpectOne(t, "current-revision ready seal", kaReady, f.seal(k+9*kaStep), btc.String())

		f.venue.setFail("BTC", true)
		f.seal(k + 10*kaStep)
		f.seal(k + 11*kaStep)
		kaExpectOne(t, "btc alert again", kaUnready, f.seal(k+12*kaStep), btc.String())
		entered, release = make(chan struct{}), make(chan struct{})
		f.beforePrepare = func(int64) {
			close(entered)
			<-release
		}
		go func() { done <- f.seal(k + 13*kaStep) }()
		<-entered
		f.beforePrepare = nil
		f.sealer.setGeneration(kaRequirements(150, 120, eth), []int{60, 120}, f.clock.Now().Add(-time.Hour))
		close(release)
		kaExpectNone(t, "old-revision seal after removal", <-done)
		if s := f.stored(k + 13*kaStep); s == nil || s.KeysTotal != 2 || s.KeysReady != 1 {
			t.Fatalf("in-flight seal = %+v, want it to carry the removed unready key", s)
		}
		if _, open := f.episode(btc); open {
			t.Fatal("an in-flight seal from a superseded revision restored the removed key's episode")
		}

		f.restart()
		f.sealer.setGeneration(both, []int{60}, f.clock.Now().Add(-time.Hour))
		kaExpectNone(t, "restart fail 1", f.seal(k+14*kaStep))
		kaExpectNone(t, "restart fail 2", f.seal(k+15*kaStep))
		kaExpectOne(t, "restart fail 3", kaUnready, f.seal(k+16*kaStep), btc.String())
	})

	t.Run("StartupWithEmptyRequirementsOpensNoEpisode", func(t *testing.T) {
		f := newKAFixture(t, feedSourceWebsocket, start)
		btc := feedKeyFor("BTC", "1m")
		req := kaRequirements(100, 60, btc)
		f.sealer.setGeneration(feedRequirements{}, []int{60}, f.clock.Now().Add(-time.Hour))
		f.apply(req)
		f.venue.setFail("BTC", true)
		for i := int64(0); i < 4; i++ {
			kaExpectNone(t, "empty requirements", f.seal(k+i*kaStep))
		}
		if _, open := f.episode(btc); open {
			t.Fatal("a key opened an episode before its union was installed")
		}
		f.sealer.setGeneration(req, []int{60}, f.clock.Now().Add(-time.Hour))
		kaExpectNone(t, "installed fail 1", f.seal(k+4*kaStep))
		kaExpectNone(t, "installed fail 2", f.seal(k+5*kaStep))
		kaExpectOne(t, "installed fail 3", kaUnready, f.seal(k+6*kaStep), btc.String())
	})

	t.Run("BoundedSenderNeverBlocksSealingOrReload", func(t *testing.T) {
		f := newKAFixture(t, feedSourceWebsocket, start)
		btc, eth := feedKeyFor("BTC", "1m"), feedKeyFor("ETH", "1m")
		both := kaRequirements(100, 60, btc, eth)
		f.install(both, 60)

		var dmu sync.Mutex
		var delivered []string
		first := make(chan struct{}, 1)
		unblock := make(chan struct{})
		sender := newFeedAlertSender(func(msg string) {
			dmu.Lock()
			delivered = append(delivered, msg)
			dmu.Unlock()
			select {
			case first <- struct{}{}:
			default:
			}
			<-unblock
		}, f.logf)
		f.sealer.keyAlertSend = sender.enqueue

		within := func(label string, fn func()) {
			t.Helper()
			ok := make(chan struct{})
			go func() {
				fn()
				close(ok)
			}()
			select {
			case <-ok:
			case <-time.After(10 * time.Second):
				t.Fatalf("%s blocked behind the alert delivery", label)
			}
		}

		f.venue.setFail("BTC", true)
		f.seal(k)
		f.seal(k + kaStep)
		f.seal(k + 2*kaStep)
		select {
		case <-first:
		case <-time.After(10 * time.Second):
			t.Fatal("the unready alert never reached the delivery hook")
		}
		within("a seal", func() { f.seal(k + 3*kaStep) })
		within("a requirements reload", func() {
			f.sealer.setGeneration(both, []int{60}, f.clock.Now().Add(-time.Hour))
		})
		if f.stored(k+3*kaStep) == nil {
			t.Fatal("the seal during a blocked delivery was not stored")
		}

		for i := 0; i < feedAlertSenderDepth; i++ {
			sender.enqueue(fmt.Sprintf("filler %d", i))
		}
		f.venue.setFail("ETH", true)
		within("ETH failures with a full queue", func() {
			f.seal(k + 4*kaStep)
			f.seal(k + 5*kaStep)
			f.seal(k + 6*kaStep)
		})
		dropped := f.logsContaining("alert queue full")
		if len(dropped) != 1 || !strings.Contains(dropped[0], kaUnready) || !strings.Contains(dropped[0], eth.String()) {
			t.Fatalf("queue overflow logs = %q, want one dropped ETH unready alert", dropped)
		}
		within("a later ETH failure", func() { f.seal(k + 7*kaStep) })
		if n := len(f.logsContaining("[feed-key-alert] " + kaUnready)); n != 2 {
			t.Fatalf("unready transition log records = %d, want 2 (BTC delivered, ETH dropped and not retried)", n)
		}

		close(unblock)
		if !sender.drain(10 * time.Second) {
			t.Fatal("the bounded sender did not drain after delivery resumed")
		}
		dmu.Lock()
		got := append([]string(nil), delivered...)
		dmu.Unlock()
		unready := 0
		for _, m := range got {
			if strings.HasPrefix(m, kaUnready) {
				unready++
				if !strings.Contains(m, btc.String()) || strings.Contains(m, eth.String()) {
					t.Fatalf("delivered unready alert %q, want only BTC", m)
				}
			}
		}
		if unready != 1 || len(got) != 1+feedAlertSenderDepth {
			t.Fatalf("delivered %d message(s) with %d unready alert(s), want the BTC alert plus %d fillers", len(got), unready, feedAlertSenderDepth)
		}

		f.sealer.keyAlertSend = nil
		f.venue.setFail("BTC", false)
		f.venue.setFail("ETH", false)
		f.seal(k + 8*kaStep)
		if len(f.logsContaining("[feed-key-alert] "+kaReady)) != 1 {
			t.Fatal("a recovery with no key-alert hook has no local log record")
		}
		nilSender := newFeedAlertSender(nil, f.logf)
		nilSender.enqueue("no hook")
		if !nilSender.drain(10 * time.Second) {
			t.Fatal("a sender with no delivery hook did not drain")
		}
	})
}
