//go:build pyintegration

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

type mrpObserved struct {
	Strategy  string
	Challenge string
	Status    int
	Body      []byte
	Verified  bool
	Result    string
	Env       map[string]any
	Seq       int
}

type mrpForward struct {
	Status      int
	Body        []byte
	Resign      bool
	BadProof    bool
	HeaderDelay time.Duration
	BodyDelay   time.Duration
}

type mrpPendingProxy struct {
	t      *testing.T
	port   int
	target int
	secret func() string
	mu     sync.Mutex
	seen   []mrpObserved
	hook   func(o mrpObserved) *mrpForward
}

func newMRPPendingProxy(t *testing.T, target int, secret func() string) *mrpPendingProxy {
	t.Helper()
	p := &mrpPendingProxy{t: t, target: target, secret: secret}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(p.serve)}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	p.port = ln.Addr().(*net.TCPAddr).Port
	return p
}

func (p *mrpPendingProxy) serve(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	target := p.target
	p.mu.Unlock()
	req, _ := http.NewRequest(r.Method, fmt.Sprintf("http://127.0.0.1:%d%s", target, r.URL.RequestURI()), nil)
	challenge := r.Header.Get(manualRegimeChallengeHeader)
	req.Header.Set(manualRegimeChallengeHeader, challenge)
	req.Header.Set(manualRegimeAuthHeader, r.Header.Get(manualRegimeAuthHeader))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	upstreamProof := resp.Header.Get(manualRegimeProofHeader)
	secret := p.secret()
	o := mrpObserved{Strategy: r.URL.Query().Get("strategy"), Challenge: challenge, Status: resp.StatusCode, Body: body}
	o.Verified = manualRegimeMACEqual(upstreamProof, manualRegimeReplyMAC(secret, challenge, resp.StatusCode, body))
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if dec.Decode(&o.Env) == nil {
		o.Result, _ = o.Env["result"].(string)
	}
	p.mu.Lock()
	for _, prev := range p.seen {
		if prev.Strategy == o.Strategy {
			o.Seq = prev.Seq
		}
	}
	o.Seq++
	p.seen = append(p.seen, o)
	hook := p.hook
	p.mu.Unlock()
	var fwd *mrpForward
	if hook != nil {
		fwd = hook(o)
	}
	status, out, proof := resp.StatusCode, body, upstreamProof
	if fwd != nil {
		if fwd.Status != 0 {
			status = fwd.Status
		}
		if fwd.Body != nil {
			out = fwd.Body
		}
		if fwd.Resign {
			proof = manualRegimeReplyMAC(secret, challenge, status, out)
		}
		if fwd.BadProof {
			foreign, _ := newManualRegimeRandomHex()
			proof = manualRegimeReplyMAC(foreign, challenge, status, out)
		}
		if fwd.HeaderDelay > 0 {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(fwd.HeaderDelay):
			}
		}
	}
	w.Header().Set(manualRegimeProofHeader, proof)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(out)))
	w.WriteHeader(status)
	if fwd != nil && fwd.BodyDelay > 0 && len(out) > 1 {
		w.Write(out[:len(out)/2])
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(fwd.BodyDelay):
		}
		w.Write(out[len(out)/2:])
		return
	}
	w.Write(out)
}

func (p *mrpPendingProxy) setHook(hook func(o mrpObserved) *mrpForward) {
	p.mu.Lock()
	p.hook = hook
	p.seen = nil
	p.mu.Unlock()
}

func (p *mrpPendingProxy) observed(strategy string) []mrpObserved {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []mrpObserved
	for _, o := range p.seen {
		if strategy == "" || o.Strategy == strategy {
			out = append(out, o)
		}
	}
	return out
}

func mrpRewrite(t *testing.T, body []byte, mutate func(map[string]any)) []byte {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var env map[string]any
	if err := dec.Decode(&env); err != nil {
		t.Fatalf("rewrite %q: %v", body, err)
	}
	mutate(env)
	out, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func mrpDistinctChallenges(t *testing.T, seen []mrpObserved, what string) {
	t.Helper()
	got := map[string]bool{}
	for _, o := range seen {
		if !validManualRegimeNonce(o.Challenge) || got[o.Challenge] {
			t.Fatalf("%s: request %d reused or lacked a fresh challenge %q", what, o.Seq, o.Challenge)
		}
		got[o.Challenge] = true
	}
}

type mrpProtocol1Envelope struct {
	ProtocolVersion     int                  `json:"protocol_version"`
	Result              string               `json:"result"`
	Message             string               `json:"message,omitempty"`
	OwnerNonce          string               `json:"owner_nonce,omitempty"`
	StrategyID          string               `json:"strategy_id,omitempty"`
	Partition           string               `json:"partition,omitempty"`
	StorageRole         string               `json:"storage_role,omitempty"`
	StorageID           string               `json:"storage_id,omitempty"`
	ContractFingerprint string               `json:"contract_fingerprint,omitempty"`
	Key                 *manualRegimeKeyJSON `json:"key,omitempty"`
	Generation          uint64               `json:"generation,omitempty"`
	IntervalSeconds     int                  `json:"interval_seconds,omitempty"`
	BundleAt            *time.Time           `json:"bundle_at,omitempty"`
	Payload             json.RawMessage      `json:"payload,omitempty"`
}

func mrpProtocol1Accepts(body []byte) error {
	var env mrpProtocol1Envelope
	if err := decodeManualRegimeStrict(body, &env); err != nil {
		return fmt.Errorf("malformed reply: %v", err)
	}
	if env.ProtocolVersion != 1 {
		return fmt.Errorf("unsupported protocol version %d", env.ProtocolVersion)
	}
	return nil
}

func mrpDirectRoute(t *testing.T, port int, strategyID string) (int, map[string]any, []byte) {
	t.Helper()
	id, active := globalManualRegimeOwner.identity()
	if !active {
		t.Fatal("no published owner")
	}
	challenge, _ := newManualRegimeRandomHex()
	req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d%s?strategy=%s&partition=paper", port, manualRegimeRoute, strategyID), nil)
	req.Header.Set(manualRegimeChallengeHeader, challenge)
	req.Header.Set(manualRegimeAuthHeader, manualRegimeRequestMAC(id.Secret, challenge, strategyID, "paper"))
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !manualRegimeMACEqual(resp.Header.Get(manualRegimeProofHeader), manualRegimeReplyMAC(id.Secret, challenge, resp.StatusCode, body)) {
		t.Fatalf("route reply %d carries no valid proof: %s", resp.StatusCode, body)
	}
	var env map[string]any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&env); err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, env, body
}

func mrpEnvUint(env map[string]any, key string) uint64 {
	n, ok := env[key].(json.Number)
	if !ok {
		return 0
	}
	v, _ := strconv.ParseUint(n.String(), 10, 64)
	return v
}

func regimeReadinessPendingIntegration(t *testing.T, bundles []*RegimeBundle, rc *RegimeConfig) {
	t.Helper()
	sc := regimeReadinessManual("pending-add", "paper")
	peer := regimeReadinessManual("pending-peer", "paper")
	peer.CloseStrategy = &StrategyRef{Name: trailingTPRatchetCloseName}
	peer.TrailingStopATRMultRegime = peer.StopLossATRMultRegime
	peer.StopLossATRMultRegime = nil
	cfg := &Config{Regime: rc, MarketFeed: "shared", IntervalSeconds: 60, Strategies: []StrategyConfig{sc, peer}}
	previousStore := globalRegimeStore
	globalRegimeStore = &RegimeStore{}
	t.Cleanup(func() { globalRegimeStore = previousStore })
	state := NewAppState()
	route := newMRPLocalRoute(t, cfg, state)
	store, db := route.store, route.db
	openedAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	original := &Position{Symbol: "BTC", Side: "long", Quantity: 1, InitialQuantity: 1, AvgCost: 100, EntryATR: 1, OwnerStrategyID: sc.ID, TradePositionID: "pending-add-pos-1", OpenedAt: openedAt}
	writeBook := func(mutate func(st *AppState)) {
		t.Helper()
		fresh, _, _, err := loadStateWithStore(cfg, store, true)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range []StrategyConfig{sc, peer} {
			if fresh.Strategies[s.ID] == nil {
				fresh.Strategies[s.ID] = NewStrategyState(s)
			}
		}
		mutate(fresh)
		for part, err := range store.SaveAll(fresh) {
			if err != nil {
				t.Fatalf("save %s: %v", partitionLabel(part), err)
			}
		}
	}
	resetBook := func() {
		writeBook(func(st *AppState) {
			cp := *original
			st.Strategies[sc.ID].Positions = map[string]*Position{"BTC": &cp}
			st.partitionRisk(defaultPaperPartition).KillSwitchActive = false
		})
	}
	resetBook()
	binding, err := manualRegimeBindingFor(cfg, store.ident, sc)
	if err != nil {
		t.Fatal(err)
	}
	key := binding.Request.Key
	routeURL, err := url.Parse(os.Getenv(manualSchedulerURLEnv))
	if err != nil {
		t.Fatal(err)
	}
	routePort, _ := strconv.Atoi(routeURL.Port())
	secret := func() string { id, _ := globalManualRegimeOwner.identity(); return id.Secret }
	proxy := newMRPPendingProxy(t, routePort, secret)
	t.Setenv(manualSchedulerURLEnv, "http://127.0.0.1:"+strconv.Itoa(proxy.port))

	begin := func(due []StrategyConfig, runnable bool) uint64 {
		return globalRegimeStore.beginPopulation(time.Now().UTC(), manualRegimeExportsFor(cfg, store, due), []regimeBundleRequest{binding.Request}, map[regimeBundleKey]bool{key: runnable})
	}
	bundleAt := func(src *RegimeBundle, at time.Time) *RegimeBundle {
		b := *src
		b.Key, b.At = key, at
		return &b
	}
	ready := func(src *RegimeBundle) *RegimeBundle { return bundleAt(src, time.Now().UTC()) }
	executes := 0
	deps := func() manualCoreDeps {
		d := newCLIManualCoreDeps(cfg, store, nil)
		d.execute = func(script, symbol, side string, size, stopLossPct float64, cancelStopLossOID int64, prevPosQty float64, marginMode string, leverage float64, closeMode hlCloseMode, snapshot hlExecuteSnapshot, extraCancelOIDs ...int64) (*HyperliquidExecuteResult, string, error) {
			executes++
			return &HyperliquidExecuteResult{Platform: "hyperliquid", Execution: &HyperliquidExecution{Action: side, Symbol: symbol, Size: size, Fill: &HyperliquidFill{AvgPx: 100, TotalSz: size, OID: 9100 + int64(executes)}}}, "", nil
		}
		d.fetchMids = func([]string) (map[string]float64, error) { return map[string]float64{"BTC": 100}, nil }
		return d
	}
	clearQueue := func() {
		if _, err := db.db.Exec("DELETE FROM pending_manual_actions"); err != nil {
			t.Fatal(err)
		}
	}
	add := func() (error, time.Duration) {
		start := time.Now()
		_, err := manualAddCore(deps(), sc, manualAddInputs{StrategyID: sc.ID, Size: 0.1})
		return err, time.Since(start)
	}
	expectErr := func(err error, what string, want ...string) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s: admitted", what)
		}
		for _, w := range want {
			if !strings.Contains(err.Error(), w) {
				t.Fatalf("%s: %q missing %q", what, err, w)
			}
		}
	}
	firstPendingThen := func(gen uint64, then func()) func(o mrpObserved) *mrpForward {
		return func(o mrpObserved) *mrpForward {
			if o.Seq == 1 {
				if !o.Verified || o.Result != manualRegimeResultPending || mrpEnvUint(o.Env, "pending_generation") != gen {
					t.Errorf("first reply is not a verified pending reply for generation %d: %s", gen, o.Body)
				}
				if then != nil {
					then()
				}
			}
			return nil
		}
	}

	gen := begin([]StrategyConfig{sc, peer}, true)
	status, env, body := mrpDirectRoute(t, routePort, sc.ID)
	if status != http.StatusServiceUnavailable || env["result"] != manualRegimeResultPending || mrpEnvUint(env, "pending_generation") != gen || mrpEnvUint(env, "protocol_version") != 2 {
		t.Fatalf("active due key: %d %s", status, body)
	}
	for _, name := range manualRegimeBundleOnlyFields {
		if _, present := env[name]; present {
			t.Fatalf("pending reply carries %s: %s", name, body)
		}
	}
	if err := mrpProtocol1Accepts(body); err == nil || !strings.Contains(err.Error(), "pending_generation") {
		t.Fatalf("a protocol 1 command must refuse the version 2 pending reply: %v", err)
	}
	globalRegimeStore.set(&RegimeBundle{Key: key, Err: "offline classifier error", At: time.Now().UTC()}, gen)
	if status, env, body = mrpDirectRoute(t, routePort, sc.ID); env["result"] != manualRegimeResultClassification || status != http.StatusServiceUnavailable {
		t.Fatalf("classifier error: %d %s", status, body)
	}
	if _, present := env["pending_generation"]; present {
		t.Fatalf("classification reply carries pending_generation: %s", body)
	}
	gen = begin([]StrategyConfig{sc, peer}, false)
	if _, env, body = mrpDirectRoute(t, routePort, sc.ID); env["result"] != manualRegimeResultUnavailable {
		t.Fatalf("missing sealed frame: %s", body)
	}
	gen = begin([]StrategyConfig{sc, peer}, true)
	globalRegimeStore.finish(gen)
	if _, env, body = mrpDirectRoute(t, routePort, sc.ID); env["result"] != manualRegimeResultUnavailable {
		t.Fatalf("finished population without a result: %s", body)
	}
	gen = begin([]StrategyConfig{sc, peer}, true)
	globalRegimeStore.seal(gen)
	if _, env, body = mrpDirectRoute(t, routePort, sc.ID); env["result"] != manualRegimeResultUnavailable {
		t.Fatalf("sealed population: %s", body)
	}
	begin([]StrategyConfig{sc, peer}, true)
	gen = begin([]StrategyConfig{peer}, true)
	if _, env, body = mrpDirectRoute(t, routePort, sc.ID); env["result"] != manualRegimeResultUnavailable {
		t.Fatalf("carried pending marker: %s", body)
	}
	if _, env, body = mrpDirectRoute(t, routePort, peer.ID); env["result"] != manualRegimeResultPending || mrpEnvUint(env, "pending_generation") != gen {
		t.Fatalf("the due peer must report its own pending generation: %s", body)
	}
	carriedGen := begin([]StrategyConfig{sc, peer}, true)
	globalRegimeStore.set(ready(bundles[3]), carriedGen)
	begin([]StrategyConfig{sc}, true)
	if _, env, body = mrpDirectRoute(t, routePort, peer.ID); env["result"] != manualRegimeResultBundle || mrpEnvUint(env, "generation") != carriedGen {
		t.Fatalf("a strategy sharing the key must read its own carried bundle: %s", body)
	}
	if err := mrpProtocol1Accepts(body); err == nil || !strings.Contains(err.Error(), "version 2") {
		t.Fatalf("a protocol 1 command must refuse a version 2 bundle reply: %v", err)
	}
	if _, env, _ = mrpDirectRoute(t, routePort, sc.ID); env["result"] != manualRegimeResultPending {
		t.Fatal("the due strategy must report pending while its peer reads the carried bundle")
	}
	t.Logf("route: an active due key answers signed pending with its generation; a classifier error, missing frame, finished or sealed population and a carried pending marker answer non-retryable results; a peer sharing the key reads its own carried bundle; a protocol 1 command refuses both version 2 replies")

	limit := manualLimitOpenInputs{strategyID: sc.ID, side: "long", openSide: "buy", size: 1, limitPrice: 100, tif: "Alo", atr: 1, dryRun: true}
	gen = begin([]StrategyConfig{sc, peer}, true)
	proxy.setHook(nil)
	if code := runManualLimitOpen(cfg, sc, store, limit); code != 1 || len(proxy.observed(sc.ID)) != 1 {
		t.Fatalf("limit on pending: exit %d after %d requests", code, len(proxy.observed(sc.ID)))
	}
	t.Logf("limit: refused on its first verified pending reply after %d request", len(proxy.observed(sc.ID)))

	for run := 1; run <= 5; run++ {
		gen = begin([]StrategyConfig{sc, peer}, true)
		g := gen
		proxy.setHook(firstPendingThen(g, func() { globalRegimeStore.set(ready(bundles[3]), g) }))
		before := executes
		err, took := add()
		seen := proxy.observed(sc.ID)
		if err != nil || executes != before+1 || len(seen) < 2 || seen[len(seen)-1].Result != manualRegimeResultBundle || mrpEnvUint(seen[len(seen)-1].Env, "generation") != g {
			t.Fatalf("run %d: waited ready add: %v, executes %d->%d, replies %d", run, err, before, executes, len(seen))
		}
		mrpDistinctChallenges(t, seen, fmt.Sprintf("run %d ready", run))
		clearQueue()

		gen = begin([]StrategyConfig{sc, peer}, true)
		g = gen
		proxy.setHook(firstPendingThen(g, func() { globalRegimeStore.set(ready(bundles[2]), g) }))
		before = executes
		werr, wtook := add()
		expectErr(werr, fmt.Sprintf("run %d warming", run), "warming (")
		if executes != before {
			t.Fatalf("run %d: warming add sent an order", run)
		}
		t.Logf("run %d: pending then ready admitted in %s with %d requests; pending then warming held in %s: %v", run, took.Round(time.Millisecond), len(seen), wtook.Round(time.Millisecond), werr)
	}

	gen = begin([]StrategyConfig{sc, peer}, true)
	g := gen
	proxy.setHook(firstPendingThen(g, func() {
		globalRegimeStore.set(&RegimeBundle{Key: key, Err: "offline classifier error", At: time.Now().UTC()}, g)
	}))
	err, _ = add()
	expectErr(err, "classification after pending", "failed in generation", "waited")
	gen = begin([]StrategyConfig{sc, peer}, true)
	g = gen
	proxy.setHook(firstPendingThen(g, func() { globalRegimeStore.finish(g) }))
	err, _ = add()
	expectErr(err, "finished after pending", "finished generation", "waited")

	gen = begin([]StrategyConfig{sc, peer}, true)
	proxy.setHook(nil)
	before := executes
	err, took := add()
	expectErr(err, "deadline", "within the 3s budget", "waited")
	if took < manualRegimeRequestTimeout || took > manualRegimeRequestTimeout+1500*time.Millisecond || executes != before || len(proxy.observed(sc.ID)) < 3 {
		t.Fatalf("deadline: took %s, %d requests, executes %d->%d", took, len(proxy.observed(sc.ID)), before, executes)
	}
	mrpDistinctChallenges(t, proxy.observed(sc.ID), "deadline")
	t.Logf("deadline: an ineligible market add refused after %s and %d paced requests: %v", took.Round(time.Millisecond), len(proxy.observed(sc.ID)), err)
	for _, slow := range []mrpForward{{HeaderDelay: 6 * time.Second}, {BodyDelay: 6 * time.Second}} {
		slow := slow
		proxy.setHook(func(o mrpObserved) *mrpForward {
			if o.Seq == 2 {
				return &slow
			}
			return nil
		})
		err, took = add()
		expectErr(err, "slow reply after pending", "within the 3s budget")
		if took > manualRegimeRequestTimeout+1500*time.Millisecond {
			t.Fatalf("a slow reply after pending extended the shared budget: %s", took)
		}
		t.Logf("slow reply (header %s, body %s) after pending ended at the shared deadline after %s", slow.HeaderDelay, slow.BodyDelay, took.Round(time.Millisecond))
	}

	tamper := []struct {
		name string
		hook func(o mrpObserved) *mrpForward
		want string
	}{
		{"bad proof", func(o mrpObserved) *mrpForward {
			if o.Seq == 2 {
				return &mrpForward{BadProof: true}
			}
			return nil
		}, "no valid owner proof"},
		{"moved pending generation", func(o mrpObserved) *mrpForward {
			if o.Seq == 2 {
				return &mrpForward{Resign: true, Body: mrpRewrite(t, o.Body, func(m map[string]any) { m["pending_generation"] = mrpEnvUint(m, "pending_generation") + 1 })}
			}
			return nil
		}, "pending generation moved"},
		{"changed fingerprint", func(o mrpObserved) *mrpForward {
			if o.Seq == 2 {
				return &mrpForward{Resign: true, Body: mrpRewrite(t, o.Body, func(m map[string]any) { m["contract_fingerprint"] = strings.Repeat("ab", 32) })}
			}
			return nil
		}, "configuration differs"},
		{"protocol 1 scheduler", func(o mrpObserved) *mrpForward {
			if o.Seq == 2 {
				return &mrpForward{Resign: true, Body: mrpRewrite(t, o.Body, func(m map[string]any) { m["protocol_version"] = 1; delete(m, "pending_generation") })}
			}
			return nil
		}, "unsupported protocol version 1"},
		{"pending with generation zero", func(o mrpObserved) *mrpForward {
			return &mrpForward{Resign: true, Body: mrpRewrite(t, o.Body, func(m map[string]any) { m["generation"] = 0 })}
		}, "carries the bundle field generation"},
		{"pending with null generation", func(o mrpObserved) *mrpForward {
			return &mrpForward{Resign: true, Body: mrpRewrite(t, o.Body, func(m map[string]any) { m["pending_generation"] = nil })}
		}, "positive pending_generation"},
		{"unavailable with pending generation", func(o mrpObserved) *mrpForward {
			return &mrpForward{Resign: true, Body: mrpRewrite(t, o.Body, func(m map[string]any) { m["result"] = manualRegimeResultUnavailable })}
		}, "unavailable reply carries pending_generation"},
	}
	for _, tc := range tamper {
		begin([]StrategyConfig{sc, peer}, true)
		proxy.setHook(tc.hook)
		before := executes
		err, _ := add()
		expectErr(err, tc.name, tc.want)
		if executes != before {
			t.Fatalf("%s: order sent", tc.name)
		}
		t.Logf("refusal: %s -> %v", tc.name, err)
	}
	gen = begin([]StrategyConfig{sc, peer}, true)
	g = gen
	proxy.setHook(func(o mrpObserved) *mrpForward {
		if o.Seq == 1 {
			globalRegimeStore.set(ready(bundles[3]), g)
		}
		if o.Seq == 2 {
			return &mrpForward{Resign: true, Body: mrpRewrite(t, o.Body, func(m map[string]any) { m["generation"] = g + 5 })}
		}
		return nil
	})
	err, _ = add()
	expectErr(err, "bundle from another generation", "not the pending generation")
	for _, at := range []struct {
		name string
		at   time.Time
		want string
	}{{"stale", time.Now().UTC().Add(-2 * time.Minute), "older than"}, {"future", time.Now().UTC().Add(time.Minute), "in the future"}} {
		gen = begin([]StrategyConfig{sc, peer}, true)
		g, stamp := gen, at.at
		proxy.setHook(firstPendingThen(g, func() { globalRegimeStore.set(bundleAt(bundles[3], stamp), g) }))
		err, _ = add()
		expectErr(err, at.name+" bundle after pending", at.want)
	}

	markerPath := manualRegimeOwnerMarkerPath(store.layout.Files[0].Path)
	markerBlob, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatal(err)
	}
	restoreMarker := func() {
		if err := os.WriteFile(markerPath, markerBlob, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, change := range []struct {
		name   string
		mutate func()
		want   string
	}{
		{"owner replaced", func() {
			var foreign manualRegimeOwnerMarker
			json.Unmarshal(markerBlob, &foreign)
			foreign.OwnerNonce = strings.Repeat("cd", manualRegimeOwnerNonceBytes)
			writeManualRegimeOwnerMarker(markerPath, foreign)
		}, "owner identity changed"},
		{"owner missing", func() { os.Remove(markerPath) }, "owner identity disappeared"},
	} {
		gen = begin([]StrategyConfig{sc, peer}, true)
		g = gen
		mutate := change.mutate
		proxy.setHook(firstPendingThen(g, func() {
			globalRegimeStore.set(ready(bundles[3]), g)
			mutate()
		}))
		before := executes
		err, _ = add()
		restoreMarker()
		expectErr(err, change.name, change.want)
		if executes != before {
			t.Fatalf("%s: order sent", change.name)
		}
	}
	t.Logf("refusal: bad proof, moved pending or bundle generation, changed fingerprint, protocol 1 scheduler, malformed pending fields, stale and future bundles, and a replaced or missing owner during the wait refuse with no order")

	for _, hold := range []struct {
		name   string
		mutate func(st *AppState)
		want   string
	}{
		{"kill switch", func(st *AppState) { st.partitionRisk(defaultPaperPartition).KillSwitchActive = true }, "kill switch"},
		{"replaced position", func(st *AppState) {
			cp := *original
			cp.TradePositionID = "pending-add-pos-2"
			st.Strategies[sc.ID].Positions["BTC"] = &cp
		}, "changed identity"},
		{"flipped side", func(st *AppState) {
			cp := *original
			cp.Side = "short"
			st.Strategies[sc.ID].Positions["BTC"] = &cp
		}, "changed identity"},
		{"closed position", func(st *AppState) { delete(st.Strategies[sc.ID].Positions, "BTC") }, "changed identity"},
	} {
		gen = begin([]StrategyConfig{sc, peer}, true)
		g = gen
		mutate := hold.mutate
		proxy.setHook(firstPendingThen(g, func() {
			globalRegimeStore.set(ready(bundles[3]), g)
			writeBook(mutate)
		}))
		before := executes
		err, _ = add()
		resetBook()
		expectErr(err, hold.name, "after the regime wait", hold.want)
		if executes != before {
			t.Fatalf("%s during the wait: order sent", hold.name)
		}
		t.Logf("recheck: %s applied during the wait -> %v", hold.name, err)
	}

	waitedAdd := func(during func()) error {
		gen := begin([]StrategyConfig{sc, peer}, true)
		proxy.setHook(firstPendingThen(gen, func() {
			globalRegimeStore.set(ready(bundles[3]), gen)
			if during != nil {
				during()
			}
		}))
		err, _ := add()
		clearQueue()
		return err
	}
	err = waitedAdd(func() { globalManualRegimeOwner.signalPersistence(defaultPaperPartition, true) })
	expectErr(err, "persistence hold", "holds the paper partition")
	err = waitedAdd(func() { globalManualRegimeOwner.signalPersistence(defaultPaperPartition, false) })
	if err != nil {
		t.Fatalf("persistence hold released after a successful save: %v", err)
	}
	err = waitedAdd(func() { globalManualRegimeOwner.signalPersistence(livePartition, true) })
	globalManualRegimeOwner.signalPersistence(livePartition, false)
	if err != nil {
		t.Fatalf("a live persistence hold blocked a paper add: %v", err)
	}
	scheduler := newStateStore(store.layout, store.ident)
	scheduler.setHoldSignal(globalManualRegimeOwner.signalPersistence)
	scheduler.recordSaveOutcome(defaultPaperPartition, fmt.Errorf("controlled save failure"))
	err = waitedAdd(nil)
	expectErr(err, "scheduler save failure", "holds the paper partition")
	scheduler.recordSaveOutcome(defaultPaperPartition, nil)
	if err = waitedAdd(nil); err != nil {
		t.Fatalf("after the successful save: %v", err)
	}
	t.Logf("persistence guard: a save failure holds the waited add, a later successful save releases it, and a live hold leaves the paper partition alone")

	guardPath := manualRegimeHoldGuardPath(store.layout.Files[0].Path, defaultPaperPartition)
	t.Run("persistence-guard-locks", func(t *testing.T) {
		partition := defaultPaperPartition
		owner, _ := globalManualRegimeOwner.identity()
		check := func() error { return checkManualRegimeHoldGuard(store.layout, partition, owner.Nonce) }
		probe, err := os.Open(guardPath)
		if err != nil {
			t.Fatal(err)
		}
		defer probe.Close()

		scheduler.recordSaveOutcome(partition, fmt.Errorf("controlled save failure"))
		if err := syscall.Flock(int(probe.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
			t.Fatal(err)
		}
		if err := check(); err == nil || !strings.Contains(err.Error(), "holds the paper partition") {
			t.Errorf("save hold with another probe: %v", err)
		} else {
			t.Log("save hold with an independent shared probe: the command refused")
		}

		scheduler.recordSaveOutcome(partition, nil)
		if err := check(); err == nil || !strings.Contains(err.Error(), "holds the paper partition") {
			t.Errorf("save succeeded while the probe held a shared lock: %v", err)
		}
		if err := syscall.Flock(int(probe.Fd()), syscall.LOCK_UN); err != nil {
			t.Fatal(err)
		}
		if err := check(); err == nil {
			t.Fatal("releasing the probe cleared the hold before the next save")
		}
		scheduler.recordSaveOutcome(partition, nil)
		if err := check(); err != nil {
			t.Fatalf("next successful save did not relock: %v", err)
		}
		t.Log("relock with a shared probe stayed held until the next successful save")

		start := make(chan struct{})
		results := make(chan error, 2)
		for range 2 {
			go func() {
				<-start
				results <- check()
			}()
		}
		close(start)
		for range 2 {
			if err := <-results; err != nil {
				t.Errorf("concurrent probe with the daemon holding the lock: %v", err)
			}
		}
		if !t.Failed() {
			t.Log("the daemon held the exclusive lock and both concurrent probes passed")
		}
	})

	writeBook(func(st *AppState) { delete(st.Strategies[sc.ID].Positions, sc.Symbol) })
	t.Run("persistence-guard-first-reply", func(t *testing.T) {
		for _, tc := range []struct {
			name, result string
			strategy     StrategyConfig
			held         bool
		}{
			{"ready-held", manualRegimeResultBundle, sc, true},
			{"fallback-held", manualRegimeResultUnavailable, peer, true},
			{"ready-clear", manualRegimeResultBundle, sc, false},
		} {
			t.Run(tc.name, func(t *testing.T) {
				if tc.held {
					scheduler.recordSaveOutcome(defaultPaperPartition, fmt.Errorf("controlled save failure"))
				} else {
					scheduler.recordSaveOutcome(defaultPaperPartition, nil)
				}
				bundleReady := tc.result == manualRegimeResultBundle
				gen := begin([]StrategyConfig{tc.strategy}, bundleReady)
				if bundleReady {
					globalRegimeStore.set(ready(bundles[3]), gen)
				}
				proxy.setHook(nil)
				before := executes
				result, err := manualOpenCore(deps(), tc.strategy, manualOpenInputs{StrategyID: tc.strategy.ID, Side: "long", Size: 0.1, ATR: 1})
				seen := proxy.observed(tc.strategy.ID)
				if len(seen) != 1 || !seen[0].Verified || seen[0].Result != tc.result {
					t.Fatalf("expected one signed %s reply, got %+v", tc.result, seen)
				}
				if tc.held {
					if err == nil || !strings.Contains(err.Error(), "holds the paper partition") || executes != before {
						t.Fatalf("expected a persistence hold before execute, got %v with %d execute calls", err, executes-before)
					}
					if !bundleReady {
						found := false
						for _, line := range result.lines {
							found = found || strings.Contains(line.text, "using the manual fallback protection policy")
						}
						if !found {
							t.Fatal("the fallback did not admit the unavailable bundle before the guard")
						}
					}
					t.Logf("the first signed %s reply reached the persistence hold with zero execute calls", tc.result)
				} else {
					if err != nil || executes != before+1 {
						t.Fatalf("expected one execute call with the daemon guard clear, got %v with %d execute calls", err, executes-before)
					}
					t.Log("the first signed ready bundle reached execute with the guard clear")
				}
			})
		}
	})
	clearQueue()
	t.Run("persistence-guard-limit-open", func(t *testing.T) {
		placement := limit
		placement.dryRun = false
		for _, tc := range []struct {
			name    string
			held    *RiskPartition
			blocked bool
		}{
			{"paper-held", &defaultPaperPartition, true},
			{"paper-clear", nil, false},
			{"live-held", &livePartition, false},
		} {
			t.Run(tc.name, func(t *testing.T) {
				if tc.held != nil {
					scheduler.recordSaveOutcome(*tc.held, fmt.Errorf("controlled save failure"))
					t.Cleanup(func() { scheduler.recordSaveOutcome(*tc.held, nil) })
					owner, _ := globalManualRegimeOwner.identity()
					if err := checkManualRegimeHoldGuard(store.layout, *tc.held, owner.Nonce); err == nil || !strings.Contains(err.Error(), "holds the "+partitionLabel(*tc.held)+" partition") {
						t.Fatalf("expected the %s guard to be held: %v", partitionLabel(*tc.held), err)
					}
				}
				gen := begin([]StrategyConfig{sc}, true)
				globalRegimeStore.set(ready(bundles[3]), gen)
				proxy.setHook(nil)

				const orderOID = 1842
				calls := 0
				originalOpen := runHyperliquidLimitOpenFn
				t.Cleanup(func() { runHyperliquidLimitOpenFn = originalOpen })
				runHyperliquidLimitOpenFn = func(script, symbol, side string, size, limitPx float64, tif, marginMode string, leverage float64, snapshot hlExecuteSnapshot) (*HyperliquidLimitOpenResult, string, error) {
					calls++
					return &HyperliquidLimitOpenResult{OrderOID: orderOID, Status: "resting"}, "", nil
				}
				code := runManualLimitOpen(cfg, sc, store, placement)
				seen := proxy.observed(sc.ID)
				if len(seen) != 1 || !seen[0].Verified || seen[0].Result != manualRegimeResultBundle {
					t.Fatalf("expected one signed ready bundle, got %+v", seen)
				}
				rows, err := pendingLimitOrdersForStrategySymbol(store, sc.ID, sc.Symbol)
				if err != nil {
					t.Fatal(err)
				}
				for _, row := range rows {
					t.Cleanup(func() {
						if err := store.DeletePendingLimitOrder(row.ID); err != nil {
							t.Error(err)
						}
					})
				}
				if tc.blocked {
					if code != 1 || calls != 0 || len(rows) != 0 {
						t.Fatalf("held limit open: exit %d, venue calls %d, pending rows %d", code, calls, len(rows))
					}
					t.Log("paper save hold refused the signed ready limit open with no venue call or pending row")
					return
				}
				if code != 0 || calls != 1 || len(rows) != 1 || rows[0].OrderOID != orderOID || len(rows[0].RegimeSnapshots) == 0 {
					t.Fatalf("accepted limit open: exit %d, venue calls %d, pending rows %+v", code, calls, rows)
				}
				t.Logf("%s accepted the signed ready paper limit open and recorded its order and regime evidence", tc.name)
			})
		}
	})
	resetBook()

	shutdownDraining.Store(true)
	proxy.setHook(nil)
	begin([]StrategyConfig{sc, peer}, true)
	err, _ = add()
	shutdownDraining.Store(false)
	expectErr(err, "draining", "draining")
	if n := len(proxy.observed(sc.ID)); n != 1 {
		t.Fatalf("draining: %d requests", n)
	}
	gen = begin([]StrategyConfig{sc, peer}, true)
	globalRegimeStore.set(&RegimeBundle{Key: key, Err: "offline classifier error", At: time.Now().UTC()}, gen)
	proxy.setHook(nil)
	err, _ = add()
	expectErr(err, "classification failure", "failed this cycle")
	if n := len(proxy.observed(sc.ID)); n != 1 {
		t.Fatalf("classification failure: %d requests", n)
	}
	closed := htfITFreePort(t)
	t.Setenv(manualSchedulerURLEnv, "http://127.0.0.1:"+strconv.Itoa(closed))
	err, took = add()
	expectErr(err, "connection refused", "did not answer")
	if took > time.Second {
		t.Fatalf("connection refused retried: %s", took)
	}
	t.Setenv(manualSchedulerURLEnv, "http://127.0.0.1:"+strconv.Itoa(proxy.port))
	t.Logf("no retry: draining, classification failure and connection refused answer on the first request")

	err = waitedAdd(func() {
		marker := manualRegimeHoldGuardMarker{Version: manualRegimeHoldGuardVersion, OwnerNonce: func() string { id, _ := globalManualRegimeOwner.identity(); return id.Nonce }(), Partition: defaultPaperPartition.String()}
		if err := writeManualRegimeJSONFile(guardPath, marker); err != nil {
			t.Error(err)
		}
	})
	expectErr(err, "replaced guard", "paper partition")
	os.Remove(guardPath)
	err = waitedAdd(nil)
	expectErr(err, "missing guard", "published no persistence guard")
	if executes == 0 {
		t.Fatal("no waited add reached the order")
	}
	t.Logf("persistence guard: a replaced or missing guard refuses the waited add")
}

const mrpPendingCycleSeconds = 10

func (e *mrpEnv) commandAsync(cfgPath string, extraEnv []string, args ...string) <-chan mrpRun {
	out := make(chan mrpRun, 1)
	full := append([]string{args[0], "--config", cfgPath}, args[1:]...)
	cmd := exec.Command(e.bin, full...)
	cmd.Dir = e.root
	cmd.Env = e.env(append([]string{manualRegimeTestVenueLogEnv + "=" + e.venueLog, manualRegimeTestFillPxEnv + "=" + strconv.FormatFloat(mrpMark(), 'f', 4, 64)}, extraEnv...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	go func() {
		err := cmd.Run()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			code = -1
			stderr.WriteString(err.Error())
		}
		out <- mrpRun{code: code, stdout: stdout.String(), stderr: stderr.String()}
	}()
	return out
}

type mrpGate struct {
	t    *testing.T
	dir  string
	once sync.Mutex
}

func newMRPGate(t *testing.T) *mrpGate {
	t.Helper()
	g := &mrpGate{t: t, dir: t.TempDir()}
	g.release()
	return g
}

func (g *mrpGate) blocked() int {
	entries, _ := os.ReadDir(g.dir)
	n := 0
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "blocked-") {
			n++
		}
	}
	return n
}

func (g *mrpGate) arm(d *mrpDaemon) {
	g.t.Helper()
	before := g.blocked()
	if err := os.Remove(filepath.Join(g.dir, "release")); err != nil && !os.IsNotExist(err) {
		g.t.Fatal(err)
	}
	d.waitLog("a gated regime population", 120*time.Second, func(string) bool { return g.blocked() > before })
}

func (g *mrpGate) release() {
	g.once.Lock()
	defer g.once.Unlock()
	if err := os.WriteFile(filepath.Join(g.dir, "release"), nil, 0o600); err != nil && !os.IsExist(err) {
		g.t.Error(err)
	}
}

func (p *mrpPendingProxy) releaseAfterPending(gate *mrpGate, strategies ...string) {
	want := map[string]bool{}
	for _, id := range strategies {
		want[id] = true
	}
	var mu sync.Mutex
	seen := map[string]bool{}
	p.setHook(func(o mrpObserved) *mrpForward {
		if o.Verified && o.Result == manualRegimeResultPending && want[o.Strategy] {
			mu.Lock()
			seen[o.Strategy] = true
			done := len(seen) == len(want)
			mu.Unlock()
			if done {
				gate.release()
			}
		}
		return nil
	})
}

func (p *mrpPendingProxy) firstPending(t *testing.T, strategy string) mrpObserved {
	t.Helper()
	for _, o := range p.observed(strategy) {
		if o.Verified && o.Result == manualRegimeResultPending {
			return o
		}
	}
	t.Fatalf("%s: no verified pending reply was observed: %+v", strategy, p.observed(strategy))
	return mrpObserved{}
}

func (e *mrpEnv) expectAdmitted(r mrpRun, what string) {
	e.t.Helper()
	if r.code != 0 || strings.Contains(r.stderr, "fallback") {
		e.t.Fatalf("%s: not admitted through the route:\n%s", what, r)
	}
}

func regimeReadinessPendingProcess(t *testing.T, mode string) {
	t.Helper()
	e := newMRPEnv(t, 40)
	gate := newMRPGate(t)
	port := htfITFreePort(t)
	const runs = 5
	var feedSock string
	strategies := []any{mrpManualStrategy("mp-add", "short"), mrpManualStrategy("mp-warm", "long"), mrpManualStrategy("mp-warm-add", "long"), mrpManualStrategy("mp-limit", "short"), mrpManualStrategy("mp-dry", "short")}
	for i := 1; i <= runs; i++ {
		strategies = append(strategies, mrpManualStrategy(fmt.Sprintf("mp-open-%d", i), "short"))
	}
	if mode == "shared" {
		sockDir, err := os.MkdirTemp("", "mrpp")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.RemoveAll(sockDir) })
		feedSock = filepath.Join(sockDir, "feed.sock")
	}
	cfg := e.writeConfig("pending.json", func(doc map[string]any) {
		doc["interval_seconds"] = mrpPendingCycleSeconds
		doc["status_port"] = port
		doc["strategies"] = strategies
		if mode == "shared" {
			doc["market_feed"] = "shared"
			doc["shared_market_feed"] = map[string]any{"primary_socket": feedSock}
		}
	})
	snapshots := map[string]RegimeSnapshot{"short": {Ready: true, Bars: 40, Need: 29, Regime: "ranging", Available: true}}
	snapshots[regimeWindowReservedName] = snapshots["short"]
	openedAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	e.seedPositions(cfg, map[string]*Position{
		"mp-add":      {Symbol: "BTC", Side: "long", Quantity: 0.01, InitialQuantity: 0.01, AvgCost: 100, EntryATR: 0.7, RegimeSnapshots: snapshots, RegimeWindows: map[string]string{"short": "ranging"}, Regime: "ranging", OwnerStrategyID: "mp-add", TradePositionID: "mp-add-pos", OpenedAt: openedAt},
		"mp-warm-add": {Symbol: "BTC", Side: "long", Quantity: 0.01, InitialQuantity: 0.01, AvgCost: 100, EntryATR: 0.7, OwnerStrategyID: "mp-warm-add", TradePositionID: "mp-warm-add-pos", OpenedAt: openedAt},
	})
	if mode == "shared" {
		feedPort := htfITFreePort(t)
		feedCfg := filepath.Join(e.dir, "feed.json")
		blob, _ := json.Marshal(map[string]any{"config_version": CurrentConfigVersion, "role": "feed", "status_port": feedPort,
			"feed": map[string]any{"source": "websocket", "socket_path": feedSock, "consumer_configs": []string{cfg}}})
		if err := os.WriteFile(feedCfg, blob, 0o600); err != nil {
			t.Fatal(err)
		}
		feed := e.startDaemon(feedCfg, feedPort)
		feed.waitLog("feed socket", 120*time.Second, func(string) bool { _, err := os.Stat(feedSock); return err == nil })
		defer feed.stop()
	}
	daemon := e.startDaemon(cfg, port, manualRegimeTestGateEnv+"="+gate.dir)
	daemon.waitCycles(1)
	proxy := newMRPPendingProxy(t, port, e.ownerSecret)
	via := []string{mrpURL(proxy.port)}
	limitPx := strconv.FormatFloat(math.Round(mrpMark()*0.9*100)/100, 'f', 2, 64)

	for run := 1; run <= runs; run++ {
		openID := fmt.Sprintf("mp-open-%d", run)
		base := e.venueKinds()
		proxy.releaseAfterPending(gate, openID, "mp-warm-add", "mp-add", "mp-limit")
		gate.arm(daemon)
		readyOpen := e.commandAsync(cfg, via, "manual-open", openID, "--size", "0.01", "--atr", "0.5")
		warmAdd := e.commandAsync(cfg, via, "manual-add", "mp-warm-add", "--size", "0.01")
		readyAdd := e.commandAsync(cfg, via, "manual-add", "mp-add", "--size", "0.01")
		limit := e.commandAsync(cfg, via, "manual-open", "mp-limit", "--size", "0.01", "--limit-price", limitPx, "--atr", "0.5")
		ro, wa, ra, li := <-readyOpen, <-warmAdd, <-readyAdd, <-limit
		gate.release()
		e.expectAdmitted(ro, fmt.Sprintf("%s run %d ready open", mode, run))
		e.expectAdmitted(ra, fmt.Sprintf("%s run %d ready add", mode, run))
		e.expectRefused(wa, fmt.Sprintf("%s run %d warming add", mode, run), "warming (", "/101)")
		e.expectRefused(li, fmt.Sprintf("%s run %d limit", mode, run), "does not wait for a pending bundle")
		pending := proxy.firstPending(t, openID)
		gen := mrpEnvUint(pending.Env, "pending_generation")
		for _, id := range []string{openID, "mp-add", "mp-warm-add"} {
			seen := proxy.observed(id)
			last := seen[len(seen)-1]
			if len(seen) < 2 || last.Result != manualRegimeResultBundle || mrpEnvUint(last.Env, "generation") != gen {
				t.Fatalf("%s run %d %s: the wait did not end on generation %d's bundle: %+v", mode, run, id, gen, seen)
			}
			mrpDistinctChallenges(t, seen, fmt.Sprintf("%s run %d %s", mode, run, id))
		}
		if n := len(proxy.observed("mp-limit")); n != 1 {
			t.Fatalf("%s run %d limit sent %d requests", mode, run, n)
		}
		after := e.venueKinds()
		if after["execute"] != base["execute"]+2 || after["limit_open"] != base["limit_open"] || after["cancel"] != base["cancel"] || after["fetch_atr"] != 0 {
			t.Fatalf("%s run %d venue %v -> %v", mode, run, base, after)
		}

		base = after
		proxy.releaseAfterPending(gate, "mp-warm")
		gate.arm(daemon)
		warmOpen := <-e.commandAsync(cfg, via, "manual-open", "mp-warm", "--size", "0.01", "--atr", "0.5")
		gate.release()
		e.expectRefused(warmOpen, fmt.Sprintf("%s run %d warming open", mode, run), "warming (", "/101)")
		if seen := proxy.observed("mp-warm"); len(seen) < 2 || seen[len(seen)-1].Result != manualRegimeResultBundle {
			t.Fatalf("%s run %d warming open did not wait for the bundle: %+v", mode, run, seen)
		}
		e.expectNoVenue(base, fmt.Sprintf("%s run %d warming open", mode, run))
		t.Logf("%s run %d: generation %d answered signed pending; the ready open and add waited and reached the order guards with no fallback (venue execute %d), the warming open and add held with %q, the limit refused after 1 request", mode, run, gen, after["execute"], strings.TrimSpace(warmOpen.stderr))
	}

	base := e.venueKinds()
	ready := e.route(port, "mp-dry", "paper")
	if ready.env.Result != manualRegimeResultBundle {
		t.Fatalf("no settled bundle to forge from: %+v", ready.env)
	}
	gate.arm(daemon)
	proxy.setHook(nil)
	r := <-e.commandAsync(cfg, via, "manual-open", "mp-dry", "--size", "0.01", "--atr", "0.5", "--dry-run")
	if r.code != 0 || !strings.Contains(r.stderr, "within the 3s budget (waited") || !strings.Contains(r.stderr, "using the manual fallback protection policy") {
		t.Fatalf("%s deadline: an eligible market open must report the wait and fall back:\n%s", mode, r)
	}
	pending := proxy.firstPending(t, "mp-dry")
	mrpDistinctChallenges(t, proxy.observed("mp-dry"), mode+" deadline")
	if err := mrpProtocol1Accepts(pending.Body); err == nil {
		t.Fatalf("%s: a protocol 1 command accepted the daemon's pending reply", mode)
	}
	t.Logf("%s deadline: %d paced requests with fresh challenges, then %s", mode, len(proxy.observed("mp-dry")), strings.TrimSpace(r.stderr))
	forged := func(o mrpObserved, at time.Time) *mrpForward {
		return &mrpForward{Status: http.StatusOK, Resign: true, Body: mrpRewrite(t, o.Body, func(m map[string]any) {
			m["result"] = manualRegimeResultBundle
			m["message"] = manualRegimeMessages[manualRegimeResultBundle]
			m["generation"] = m["pending_generation"]
			delete(m, "pending_generation")
			m["interval_seconds"] = mrpPendingCycleSeconds
			m["bundle_at"] = at.Format(time.RFC3339Nano)
			m["payload"] = ready.env.Payload
		})}
	}
	markerPath := mrpMarkerPath(filepath.Join(e.dir, "state.db"))
	markerBlob, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatal(err)
	}
	tamper := []struct {
		name string
		hook func(o mrpObserved) *mrpForward
		want string
	}{
		{"bad proof", func(o mrpObserved) *mrpForward {
			if o.Seq == 2 {
				return &mrpForward{BadProof: true}
			}
			return nil
		}, "no valid owner proof"},
		{"moved pending generation", func(o mrpObserved) *mrpForward {
			if o.Seq == 2 {
				return &mrpForward{Resign: true, Body: mrpRewrite(t, o.Body, func(m map[string]any) { m["pending_generation"] = mrpEnvUint(m, "pending_generation") + 1 })}
			}
			return nil
		}, "pending generation moved"},
		{"bundle from another generation", func(o mrpObserved) *mrpForward {
			if o.Seq == 2 {
				f := forged(o, time.Now().UTC())
				f.Body = mrpRewrite(t, f.Body, func(m map[string]any) { m["generation"] = mrpEnvUint(m, "generation") + 1 })
				return f
			}
			return nil
		}, "not the pending generation"},
		{"changed fingerprint", func(o mrpObserved) *mrpForward {
			if o.Seq == 2 {
				return &mrpForward{Resign: true, Body: mrpRewrite(t, o.Body, func(m map[string]any) { m["contract_fingerprint"] = strings.Repeat("ab", 32) })}
			}
			return nil
		}, "configuration differs"},
		{"protocol 1 scheduler", func(o mrpObserved) *mrpForward {
			if o.Seq == 2 {
				return &mrpForward{Resign: true, Body: mrpRewrite(t, o.Body, func(m map[string]any) { m["protocol_version"] = 1; delete(m, "pending_generation") })}
			}
			return nil
		}, "unsupported protocol version 1"},
		{"stale bundle", func(o mrpObserved) *mrpForward {
			if o.Seq == 2 {
				return forged(o, time.Now().UTC().Add(-time.Minute))
			}
			return nil
		}, "older than"},
		{"future bundle", func(o mrpObserved) *mrpForward {
			if o.Seq == 2 {
				return forged(o, time.Now().UTC().Add(time.Minute))
			}
			return nil
		}, "in the future"},
		{"owner replaced", func(o mrpObserved) *mrpForward {
			if o.Seq == 1 {
				var foreign manualRegimeOwnerMarker
				json.Unmarshal(markerBlob, &foreign)
				foreign.OwnerNonce = strings.Repeat("cd", manualRegimeOwnerNonceBytes)
				writeManualRegimeOwnerMarker(markerPath, foreign)
			}
			return nil
		}, "owner identity changed"},
		{"owner missing", func(o mrpObserved) *mrpForward {
			if o.Seq == 1 {
				os.Remove(markerPath)
			}
			return nil
		}, "owner identity disappeared"},
	}
	for _, tc := range tamper {
		proxy.setHook(tc.hook)
		r := <-e.commandAsync(cfg, via, "manual-open", "mp-dry", "--size", "0.01", "--atr", "0.5", "--dry-run")
		if err := os.WriteFile(markerPath, markerBlob, 0o600); err != nil {
			t.Fatal(err)
		}
		e.expectRefused(r, mode+" "+tc.name, tc.want)
		proxy.firstPending(t, "mp-dry")
		mrpDistinctChallenges(t, proxy.observed("mp-dry"), mode+" "+tc.name)
	}
	gate.release()
	e.expectNoVenue(base, mode+" tampered replies")
	t.Logf("%s refusal: bad proof, moved pending or bundle generation, changed fingerprint, a protocol 1 reply, stale and future bundles and a replaced or missing owner during the wait refused with no order or cancellation; a protocol 1 command refuses the version 2 pending reply", mode)

	daemon.waitFreshCycle()
	cfgLoaded, store := e.readStore(cfg)
	adopted, _, err := LoadStateWithStore(cfgLoaded, store)
	store.Close()
	if err != nil {
		t.Fatal(err)
	}
	pos := adopted.Strategies["mp-add"].Positions["BTC"]
	if pos == nil || pos.Quantity < 0.01*float64(runs) || !reflect.DeepEqual(pos.RegimeSnapshots, snapshots) || pos.EntryATR != 0.7 || pos.TradePositionID != "mp-add-pos" {
		t.Fatalf("%s: waited adds changed the original entry evidence: %+v", mode, pos)
	}
	daemon.stop()
	if matches, _ := filepath.Glob(filepath.Join(e.dir, "state.db"+manualRegimeHoldGuardInfix+"*")); len(matches) != 0 {
		t.Fatalf("%s: graceful shutdown left persistence guards: %v", mode, matches)
	}
	t.Logf("%s: adopted waited adds kept the original entry evidence (qty %.4f); shutdown removed the persistence guards", mode, pos.Quantity)
}
