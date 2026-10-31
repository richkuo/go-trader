//go:build pyintegration

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

type mrpRoute struct {
	env     manualRegimeEnvelope
	status  int
	payload RegimePayload
}

func (e *mrpEnv) route(port int, strategyID, partition string) mrpRoute {
	e.t.Helper()
	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d%s?strategy=%s&partition=%s", port, manualRegimeRoute, strategyID, partition), nil)
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+e.token)
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var out mrpRoute
	out.status = resp.StatusCode
	if err := json.Unmarshal(body, &out.env); err != nil {
		e.t.Fatalf("route body %q: %v", body, err)
	}
	if out.env.Result == manualRegimeResultBundle && out.env.Key != nil {
		key := regimeBundleKey{Platform: out.env.Key.Platform, Symbol: out.env.Key.Symbol, Timeframe: out.env.Key.Timeframe, SpecJSON: out.env.Key.SpecJSON}
		p, err := decodeManualRegimePayload(key, out.env.Payload)
		if err != nil {
			e.t.Fatal(err)
		}
		out.payload = p
	}
	return out
}

func (e *mrpEnv) readStore(cfgPath string) (*Config, *StateStore) {
	e.t.Helper()
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		e.t.Fatal(err)
	}
	store, err := openToolStateStoreReadOnly(cfg)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { store.Close() })
	return cfg, store
}

func (e *mrpEnv) pendingActions(cfgPath string) []PendingManualAction {
	e.t.Helper()
	_, store := e.readStore(cfgPath)
	actions, err := store.files[storageRolePrimary].LoadPendingManualActions()
	if err != nil {
		e.t.Fatal(err)
	}
	store.Close()
	return actions
}

func (e *mrpEnv) pendingLimits(cfgPath string) []PendingLimitOrder {
	e.t.Helper()
	_, store := e.readStore(cfgPath)
	rows, err := store.files[storageRolePrimary].LoadPendingLimitOrders()
	if err != nil {
		e.t.Fatal(err)
	}
	store.Close()
	return rows
}

func (e *mrpEnv) seedPositions(cfgPath string, positions map[string]*Position) *AppState {
	e.t.Helper()
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		e.t.Fatal(err)
	}
	store, err := openToolStateStore(cfg)
	if err != nil {
		e.t.Fatal(err)
	}
	defer store.Close()
	state := NewAppState()
	for _, sc := range cfg.Strategies {
		state.Strategies[sc.ID] = NewStrategyState(sc)
		if pos := positions[sc.ID]; pos != nil {
			state.Strategies[sc.ID].Positions[pos.Symbol] = pos
		}
	}
	for part, err := range store.SaveAll(state) {
		if err != nil {
			e.t.Fatalf("seed %s: %v", part, err)
		}
	}
	loaded, _, err := LoadStateWithStore(cfg, store)
	if err != nil {
		e.t.Fatal(err)
	}
	return loaded
}

func mrpCycles(log string) int {
	return strings.Count(log, " complete (")
}

func (d *mrpDaemon) waitCycles(n int) {
	d.t.Helper()
	d.waitLog(fmt.Sprintf("cycle %d", n), 180*time.Second, func(s string) bool { return mrpCycles(s) >= n })
}

func (d *mrpDaemon) waitFreshCycle() {
	d.t.Helper()
	d.waitCycles(mrpCycles(d.output()) + 1)
}

type mrpProxy struct {
	port   int
	mu     sync.Mutex
	target int
	mutate func(status int, env map[string]any) (int, []byte)
}

func newMRPProxy(t *testing.T, target int) *mrpProxy {
	t.Helper()
	p := &mrpProxy{target: target}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req, _ := http.NewRequest(r.Method, fmt.Sprintf("http://127.0.0.1:%d%s", p.target, r.URL.RequestURI()), nil)
		req.Header.Set("Authorization", r.Header.Get("Authorization"))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		var env map[string]any
		_ = json.Unmarshal(body, &env)
		p.mu.Lock()
		mutate := p.mutate
		p.mu.Unlock()
		status, out := resp.StatusCode, body
		if mutate != nil {
			status, out = mutate(resp.StatusCode, env)
		}
		if status >= 300 && status < 400 {
			w.Header().Set("Location", "http://127.0.0.1:1/")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		w.Write(out)
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	p.port = ln.Addr().(*net.TCPAddr).Port
	return p
}

func (p *mrpProxy) set(fn func(status int, env map[string]any) (int, []byte)) {
	p.mu.Lock()
	p.mutate = fn
	p.mu.Unlock()
}

func mrpJSON(v any) []byte {
	blob, _ := json.Marshal(v)
	return blob
}

func newMRPForwarder(t *testing.T, target int) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				up, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", target))
				if err != nil {
					return
				}
				defer up.Close()
				done := make(chan struct{}, 2)
				go func() { io.Copy(up, c); done <- struct{}{} }()
				go func() { io.Copy(c, up); done <- struct{}{} }()
				<-done
			}()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

func mrpURL(port int) string {
	return manualSchedulerURLEnv + "=http://127.0.0.1:" + strconv.Itoa(port)
}

func mrpMarkerPath(dbPath string) string {
	return manualRegimeOwnerMarkerPath(dbPath)
}

func (e *mrpEnv) expectRefused(r mrpRun, what string, want ...string) {
	e.t.Helper()
	if r.code == 0 || strings.Contains(r.stderr, "using the manual fallback protection policy") {
		e.t.Fatalf("%s: admitted or fell back:\n%s", what, r)
	}
	for _, w := range want {
		if !strings.Contains(r.stderr+r.stdout, w) {
			e.t.Fatalf("%s: missing %q:\n%s", what, w, r)
		}
	}
}

func (e *mrpEnv) expectNoVenue(before map[string]int, what string) {
	e.t.Helper()
	after := e.venueKinds()
	if !reflect.DeepEqual(before, after) {
		e.t.Fatalf("%s: venue side effects %v -> %v", what, before, after)
	}
}

type mrpLocalRoute struct {
	cfg    *Config
	store  *StateStore
	db     *StateDB
	server *StatusServer
}

func newMRPLocalRoute(t *testing.T, cfg *Config, state *AppState) *mrpLocalRoute {
	t.Helper()
	cfg.DBFile = filepath.Join(t.TempDir(), "state.db")
	store, err := openToolStateStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	owned, err := acquireStateOwnership(store.layout.Files)
	if err != nil {
		t.Fatal(err)
	}
	if err := globalManualRegimeOwner.publish(store.layout); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		globalManualRegimeOwner.withdraw()
		owned.Release()
	})
	server := &StatusServer{state: state, stateDB: store, mu: &sync.RWMutex{}}
	server.UpdateStrategies(cfg.Strategies)
	server.SetConfigContext("", cfg)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc(manualRegimeRoute, server.handleAPIRegimeManual)
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	t.Setenv(manualSchedulerURLEnv, "http://"+ln.Addr().String())
	return &mrpLocalRoute{cfg: cfg, store: store, db: store.files[storageRolePrimary], server: server}
}

func (l *mrpLocalRoute) publish(bundle *RegimeBundle) {
	gen := globalRegimeStore.resetForCycleWithExports(time.Now().UTC(), manualRegimeExportsFor(l.cfg, l.store, l.cfg.Strategies))
	globalRegimeStore.set(bundle, gen)
}

func regimeReadinessProcessIntegration(t *testing.T) {
	t.Helper()
	regimeReadinessProcessWebsocket(t)
	regimeReadinessProcessShared(t)
}

func regimeReadinessProcessWebsocket(t *testing.T) {
	t.Helper()
	e := newMRPEnv(t, 40)
	port := htfITFreePort(t)
	strategies := func(doc map[string]any) {
		doc["status_port"] = port
		doc["strategies"] = []any{
			mrpManualStrategy("mr-ready", "short"), mrpManualStrategy("mr-add", "short"), mrpManualStrategy("mr-limit", "short"),
			mrpManualStrategy("mr-record", "short"), mrpManualStrategy("mr-adverse", "short"),
			mrpManualStrategy("mr-warming", "long"), mrpManualStrategy("mr-warm-add", "long"),
		}
	}
	cfgA := e.writeConfig("config-a.json", strategies)
	other := t.TempDir()
	cfgB := e.writeConfig("config-b.json", func(doc map[string]any) {
		strategies(doc)
		doc["db_file"], doc["log_dir"] = filepath.Join(other, "state.db"), filepath.Join(other, "logs")
	})
	original := map[string]RegimeSnapshot{"short": {Ready: true, Bars: 40, Need: 29, Regime: "ranging", Available: true}}
	original[regimeWindowReservedName] = original["short"]
	seeded := e.seedPositions(cfgA, map[string]*Position{
		"mr-add":      {Symbol: "BTC", Side: "long", Quantity: 0.01, InitialQuantity: 0.01, AvgCost: 100, EntryATR: 0.7, RegimeSnapshots: original, RegimeWindows: map[string]string{"short": "ranging"}, Regime: "ranging", OwnerStrategyID: "mr-add"},
		"mr-warm-add": {Symbol: "BTC", Side: "long", Quantity: 0.01, InitialQuantity: 0.01, AvgCost: 100, EntryATR: 0.7, OwnerStrategyID: "mr-warm-add"},
	})
	addEvidence := cloneRegimeSnapshots(seeded.Strategies["mr-add"].Positions["BTC"].RegimeSnapshots)
	addATR := seeded.Strategies["mr-add"].Positions["BTC"].EntryATR

	r := e.command(cfgA, nil, "manual-open", "mr-ready", "--size", "0.01", "--atr", "0.5", "--dry-run")
	if r.code != 0 || !strings.Contains(r.stderr, "no scheduler owns the configured state files") || !strings.Contains(r.stderr, "using the manual fallback protection policy") {
		t.Fatalf("no scheduler: ratchet-eligible market open must use the genuine-failure fallback:\n%s", r)
	}
	t.Logf("matrix: no running scheduler -> availability; ratchet-eligible dry-run open fell back")
	r = e.command(cfgA, nil, "manual-open", "mr-limit", "--size", "0.01", "--limit-price", "90", "--atr", "0.5", "--dry-run")
	e.expectRefused(r, "no scheduler limit", "unavailable")

	wrong := e.startDaemon(cfgB, port)
	wrong.waitCycles(1)
	daemon := e.startDaemon(cfgA, 0)
	daemon.waitLog("fallback bind", 60*time.Second, func(s string) bool {
		return strings.Contains(s, fmt.Sprintf("bound to %d instead", port+1))
	})
	daemon.waitCycles(1)
	good := mrpURL(port + 1)
	markerA := mrpMarkerPath(filepath.Join(e.dir, "state.db"))
	markerB := mrpMarkerPath(filepath.Join(other, "state.db"))
	ma, errA := readManualRegimeOwnerMarker(markerA)
	mb, errB := readManualRegimeOwnerMarker(markerB)
	if errA != nil || errB != nil || ma.OwnerNonce == mb.OwnerNonce {
		t.Fatalf("owner markers: %v %v %q %q", errA, errB, ma.OwnerNonce, mb.OwnerNonce)
	}
	if info, err := os.Stat(markerA); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("marker mode: %v %v", info, err)
	}
	t.Logf("identity: two schedulers with identical strategy and regime configuration publish different owner nonces")

	base := e.venueKinds()
	r = e.command(cfgA, nil, "manual-open", "mr-ready", "--size", "0.01", "--atr", "0.5")
	e.expectRefused(r, "default endpoint reaches the other scheduler", "different scheduler process")
	e.expectNoVenue(base, "wrong instance")
	t.Logf("matrix: configured port held by an identical scheduler with other state -> refused, no fallback, no order")

	r = e.command(cfgA, []string{good}, "manual-open", "mr-ready", "--size", "0.01", "--atr", "0.5", "--dry-run")
	if r.code != 0 || !strings.Contains(r.stdout, "[dry-run] manual-open mr-ready") {
		t.Fatalf("fallback listener override dry-run:\n%s", r)
	}
	e.expectNoVenue(base, "dry run")
	mapped := newMRPForwarder(t, port+1)
	r = e.command(cfgA, []string{mrpURL(mapped)}, "manual-open", "mr-ready", "--size", "0.01", "--atr", "0.5", "--dry-run")
	if r.code != 0 {
		t.Fatalf("mapped port:\n%s", r)
	}
	t.Logf("endpoint: listener fallback port %d and a mapped port %d admit through the override", port+1, mapped)

	lockPath := stateDBLockPath(filepath.Join(e.dir, "state.db"))
	pidBlob, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockPath, []byte(strconv.Itoa(wrong.cmd.Process.Pid)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r = e.command(cfgA, []string{good}, "manual-open", "mr-ready", "--size", "0.01", "--atr", "0.5", "--dry-run")
	if r.code != 0 {
		t.Fatalf("equal reported process identifiers changed admission:\n%s", r)
	}
	r = e.command(cfgA, nil, "manual-open", "mr-ready", "--size", "0.01", "--atr", "0.5", "--dry-run")
	e.expectRefused(r, "equal pid wrong instance", "different scheduler process")
	os.WriteFile(lockPath, pidBlob, 0o644)
	t.Logf("identity: equal reported process identifiers leave admission to the owner nonce")

	daemon.waitFreshCycle()
	before := e.route(port+1, "mr-ready", "paper")
	if before.status != http.StatusOK || before.env.Result != manualRegimeResultBundle {
		t.Fatalf("route: %+v", before.env)
	}
	r = e.command(cfgA, []string{good}, "manual-open", "mr-ready", "--size", "0.01", "--atr", "0.5")
	if r.code != 0 {
		t.Fatalf("ready market open:\n%s", r)
	}
	r = e.command(cfgA, []string{good}, "manual-add", "mr-add", "--size", "0.01")
	if r.code != 0 {
		t.Fatalf("ready add:\n%s", r)
	}
	limitPx := math.Round(mrpMark()*0.9*100) / 100
	r = e.command(cfgA, []string{good}, "manual-open", "mr-limit", "--size", "0.01", "--limit-price", strconv.FormatFloat(limitPx, 'f', 2, 64), "--atr", "0.5")
	if r.code != 0 {
		t.Fatalf("ready limit open:\n%s", r)
	}
	after := e.route(port+1, "mr-ready", "paper")
	if after.env.Generation != before.env.Generation || !after.env.BundleAt.Equal(*before.env.BundleAt) {
		t.Fatalf("a cycle ran during the evidence window: %d -> %d", before.env.Generation, after.env.Generation)
	}
	cfgLoaded, _ := e.readStore(cfgA)
	evidence := entryRegimeSnapshots(before.payload, cfgLoaded.Regime)
	actions := e.pendingActions(cfgA)
	byStrategy := map[string]PendingManualAction{}
	for _, a := range actions {
		byStrategy[a.StrategyID] = a
	}
	open := byStrategy["mr-ready"]
	if open.Action != "open" || !reflect.DeepEqual(open.RegimeSnapshots, evidence) || open.EntryATR != 0.5 {
		t.Fatalf("queued open evidence %+v want %+v", open, evidence)
	}
	if add := byStrategy["mr-add"]; add.Action != "add" {
		t.Fatalf("queued add: %+v", add)
	}
	limits := e.pendingLimits(cfgA)
	if len(limits) != 1 || limits[0].StrategyID != "mr-limit" || !reflect.DeepEqual(limits[0].RegimeSnapshots, evidence) || limits[0].EntryATR != 0.5 {
		t.Fatalf("pending limit evidence: %+v", limits)
	}
	kinds := e.venueKinds()
	if kinds["execute"] != 2 || kinds["limit_open"] != 1 || kinds["fetch_atr"] != 0 {
		t.Fatalf("venue after ready actions: %v", kinds)
	}
	t.Logf("ready: open, add and limit admitted across the process boundary; queued open and pending limit carry the route's snapshots; venue %v; private candle fetches %d", kinds, kinds["fetch_atr"])

	base = e.venueKinds()
	for _, args := range [][]string{
		{"manual-open", "mr-warming", "--size", "0.01", "--atr", "0.5"},
		{"manual-open", "mr-warming", "--size", "0.01", "--atr", "0.5", "--dry-run"},
		{"manual-add", "mr-warm-add", "--size", "0.01"},
		{"manual-add", "mr-warm-add", "--size", "0.01", "--dry-run"},
		{"manual-open", "mr-warming", "--size", "0.01", "--limit-price", strconv.FormatFloat(limitPx, 'f', 2, 64), "--atr", "0.5"},
	} {
		r = e.command(cfgA, []string{good}, args...)
		e.expectRefused(r, strings.Join(args, " "), "warming (40/101)")
	}
	e.expectNoVenue(base, "warming holds")
	t.Logf("warming: required long window holds open, add and limit with warming (40/101); the unrelated warming window did not hold the short-window strategies")

	for _, atr := range []string{"", "0", "-1", "NaN", "+Inf", strconv.FormatFloat(mrpMark(), 'f', 2, 64)} {
		args := []string{"manual-open", "mr-ready", "--margin", "10"}
		if atr != "" {
			args = append(args, "--atr", atr)
		}
		r = e.command(cfgA, []string{good}, args...)
		e.expectRefused(r, "atr "+atr)
		r = e.command(cfgA, []string{good}, "manual-open", "mr-limit", "--size", "0.01", "--limit-price", strconv.FormatFloat(limitPx, 'f', 2, 64), "--atr", func() string {
			if atr == "" {
				return "0"
			}
			return atr
		}())
		e.expectRefused(r, "limit atr "+atr)
	}
	e.expectNoVenue(base, "ATR preflight")
	r = e.command(cfgA, []string{good}, "manual-open", "mr-record", "--size", "0.01", "--fill-price", "100", "--record-only")
	if r.code != 0 {
		t.Fatalf("record-only without ATR:\n%s", r)
	}
	e.expectNoVenue(base, "record-only")
	t.Logf("ATR: omitted, zero, negative, NaN, infinite and implausible --atr refused before any order; record-only adoption needs none")

	fill := mrpMark() * 0.5
	r = e.command(cfgA, []string{good, manualRegimeTestFillPxEnv + "=" + strconv.FormatFloat(fill, 'f', 4, 64)}, "manual-open", "mr-adverse", "--size", "0.01", "--atr", strconv.FormatFloat(0.4*mrpMark(), 'f', 4, 64))
	if r.code != 0 || !strings.Contains(r.stderr, "arithmetic fallback") {
		t.Fatalf("adverse fill recovery:\n%s", r)
	}
	for _, a := range e.pendingActions(cfgA) {
		switch a.StrategyID {
		case "mr-adverse":
			want, _ := computeFallbackATR(fill, 1)
			if math.Abs(a.EntryATR-want) > 1e-6 || a.Quantity != 0.01 {
				t.Fatalf("adverse recovery queued %+v want ATR %v", a, want)
			}
		}
	}
	if k := e.venueKinds(); k["fetch_atr"] != 0 {
		t.Fatalf("private candle fetch: %v", k)
	}
	t.Logf("ATR: an adverse confirmed fill queues the arithmetic recovery ATR and no private candle fetch ran")

	proxy := newMRPProxy(t, port+1)
	via := mrpURL(proxy.port)
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
	stale := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	refusals := []struct {
		name   string
		mutate func(int, map[string]any) (int, []byte)
		want   string
	}{
		{"future timestamp", func(s int, m map[string]any) (int, []byte) { m["bundle_at"] = future; return s, mrpJSON(m) }, "in the future"},
		{"stale timestamp", func(s int, m map[string]any) (int, []byte) { m["bundle_at"] = stale; return s, mrpJSON(m) }, "older than"},
		{"missing timestamp", func(s int, m map[string]any) (int, []byte) { delete(m, "bundle_at"); return s, mrpJSON(m) }, "missing generation, timestamp or payload"},
		{"wrong key", func(s int, m map[string]any) (int, []byte) {
			m["key"].(map[string]any)["timeframe"] = "5m"
			return s, mrpJSON(m)
		}, "different bundle key"},
		{"wrong strategy", func(s int, m map[string]any) (int, []byte) { m["strategy_id"] = "mr-limit"; return s, mrpJSON(m) }, "different strategy"},
		{"wrong owner", func(s int, m map[string]any) (int, []byte) {
			m["owner_nonce"] = strings.Repeat("ab", manualRegimeOwnerNonceBytes)
			return s, mrpJSON(m)
		}, "different scheduler process"},
		{"wrong configuration", func(s int, m map[string]any) (int, []byte) {
			m["contract_fingerprint"] = strings.Repeat("0", 64)
			return s, mrpJSON(m)
		}, "configuration differs"},
		{"unsupported version", func(s int, m map[string]any) (int, []byte) { m["protocol_version"] = 2; return s, mrpJSON(m) }, "unsupported protocol version"},
		{"unknown field", func(s int, m map[string]any) (int, []byte) { m["extra"] = true; return s, mrpJSON(m) }, "malformed reply"},
		{"trailing data", func(s int, m map[string]any) (int, []byte) { return s, append(mrpJSON(m), []byte(" {}")...) }, "malformed reply"},
		{"oversized body", func(s int, m map[string]any) (int, []byte) {
			return s, []byte(`{"pad":"` + strings.Repeat("x", manualRegimeMaxBodyBytes) + `"}`)
		}, "exceeds"},
		{"redirect", func(int, map[string]any) (int, []byte) { return http.StatusFound, nil }, "unexpected HTTP 302"},
		{"server error", func(int, map[string]any) (int, []byte) { return http.StatusInternalServerError, nil }, "unexpected HTTP 500"},
		{"draining", func(int, map[string]any) (int, []byte) {
			return http.StatusServiceUnavailable, mrpJSON(map[string]any{"protocol_version": 1, "result": "draining"})
		}, "draining"},
		{"availability with bundle fields", func(s int, m map[string]any) (int, []byte) {
			m["result"] = manualRegimeResultUnavailable
			return http.StatusServiceUnavailable, mrpJSON(m)
		}, "availability reply carries bundle fields"},
		{"invalid readiness", func(s int, m map[string]any) (int, []byte) {
			m["payload"] = map[string]any{"short": map[string]any{"regime": "trending_up", "ready": true, "bars": 10, "need": 29}, "long": map[string]any{}}
			return s, mrpJSON(m)
		}, "unavailable"},
	}
	base = e.venueKinds()
	for _, tc := range refusals {
		proxy.set(tc.mutate)
		r = e.command(cfgA, []string{via}, "manual-open", "mr-ready", "--size", "0.01", "--atr", "0.5", "--dry-run")
		e.expectRefused(r, tc.name, tc.want)
		r = e.command(cfgA, []string{via}, "manual-add", "mr-add", "--size", "0.01", "--dry-run")
		e.expectRefused(r, tc.name+" add", tc.want)
	}
	e.expectNoVenue(base, "transport refusals")
	t.Logf("matrix: %d malformed, stale, future, foreign or unsupported replies refused open and add with no ratchet fallback and no order", len(refusals))

	r = e.command(cfgA, []string{good, "STATUS_AUTH_TOKEN=wrong"}, "manual-open", "mr-ready", "--size", "0.01", "--atr", "0.5", "--dry-run")
	e.expectRefused(r, "authentication", "HTTP 401")
	e.expectNoVenue(base, "authentication")

	proxy.set(func(_ int, m map[string]any) (int, []byte) {
		out := map[string]any{"protocol_version": 1, "result": manualRegimeResultClassification}
		for _, k := range []string{"owner_nonce", "strategy_id", "partition", "storage_role", "storage_id", "contract_fingerprint", "key"} {
			out[k] = m[k]
		}
		return http.StatusServiceUnavailable, mrpJSON(out)
	})
	r = e.command(cfgA, []string{via}, "manual-open", "mr-ready", "--size", "0.01", "--atr", "0.5", "--dry-run")
	if r.code != 0 || !strings.Contains(r.stderr, "using the manual fallback protection policy") || !strings.Contains(r.stderr, "classification") {
		t.Fatalf("ownership-checked classification failure must use the ratchet fallback:\n%s", r)
	}
	r = e.command(cfgA, []string{via}, "manual-open", "mr-limit", "--size", "0.01", "--limit-price", strconv.FormatFloat(limitPx, 'f', 2, 64), "--atr", "0.5")
	e.expectRefused(r, "classification limit", "unavailable")
	proxy.set(func(int, map[string]any) (int, []byte) {
		time.Sleep(manualRegimeRequestTimeout + time.Second)
		return http.StatusOK, nil
	})
	started := time.Now()
	r = e.command(cfgA, []string{via}, "manual-open", "mr-ready", "--size", "0.01", "--atr", "0.5", "--dry-run")
	if r.code != 0 || !strings.Contains(r.stderr, "using the manual fallback protection policy") || time.Since(started) > manualRegimeRequestTimeout+3*time.Second {
		t.Fatalf("bounded timeout must be an availability failure:\n%s", r)
	}
	closed := htfITFreePort(t)
	r = e.command(cfgA, []string{mrpURL(closed)}, "manual-open", "mr-ready", "--size", "0.01", "--atr", "0.5", "--dry-run")
	if r.code != 0 || !strings.Contains(r.stderr, "did not answer") || !strings.Contains(r.stderr, "using the manual fallback protection policy") {
		t.Fatalf("unreachable scheduler must use the ratchet fallback:\n%s", r)
	}
	r = e.command(cfgA, []string{mrpURL(closed)}, "manual-open", "mr-limit", "--size", "0.01", "--limit-price", strconv.FormatFloat(limitPx, 'f', 2, 64), "--atr", "0.5")
	e.expectRefused(r, "unreachable limit", "unavailable")
	e.expectNoVenue(base, "availability")
	t.Logf("matrix: classification failure, bounded timeout and unreachable scheduler use the ratchet fallback for market open; limit refuses")

	for _, bad := range []string{"https://127.0.0.1:1", "http://10.0.0.1:8099", "http://user@127.0.0.1:8099", "http://127.0.0.1:8099/api", "http://127.0.0.1:8099?x=1", "http://127.0.0.1"} {
		r = e.command(cfgA, []string{manualSchedulerURLEnv + "=" + bad}, "manual-open", "mr-ready", "--size", "0.01", "--atr", "0.5", "--dry-run")
		e.expectRefused(r, "endpoint "+bad, manualSchedulerURLEnv)
	}

	markerBlob, err := os.ReadFile(markerA)
	if err != nil {
		t.Fatal(err)
	}
	var foreign manualRegimeOwnerMarker
	json.Unmarshal(markerBlob, &foreign)
	foreign.OwnerNonce = strings.Repeat("cd", manualRegimeOwnerNonceBytes)
	if err := writeManualRegimeOwnerMarker(markerA, foreign); err != nil {
		t.Fatal(err)
	}
	r = e.command(cfgA, []string{good}, "manual-open", "mr-ready", "--size", "0.01", "--atr", "0.5", "--dry-run")
	e.expectRefused(r, "replaced marker", "different scheduler process")
	copyPath := markerA + ".copy"
	os.WriteFile(copyPath, markerBlob, 0o600)
	os.Remove(markerA)
	os.Symlink(copyPath, markerA)
	r = e.command(cfgA, []string{good}, "manual-open", "mr-ready", "--size", "0.01", "--atr", "0.5", "--dry-run")
	e.expectRefused(r, "symlinked marker", "not a regular file")
	os.Remove(markerA)
	os.WriteFile(markerA, []byte("{not json"), 0o600)
	r = e.command(cfgA, []string{good}, "manual-open", "mr-ready", "--size", "0.01", "--atr", "0.5", "--dry-run")
	e.expectRefused(r, "malformed marker", "malformed")
	if os.Geteuid() != 0 {
		os.WriteFile(markerA, markerBlob, 0o000)
		os.Chmod(markerA, 0o000)
		r = e.command(cfgA, []string{good}, "manual-open", "mr-ready", "--size", "0.01", "--atr", "0.5", "--dry-run")
		e.expectRefused(r, "unreadable marker", "unreadable")
	}
	os.Remove(markerA)
	os.Remove(copyPath)
	if err := os.WriteFile(markerA, markerBlob, 0o600); err != nil {
		t.Fatal(err)
	}
	e.expectNoVenue(base, "marker refusals")
	t.Logf("identity: replaced, symlinked, malformed and unreadable owner markers refuse with no fallback")

	cfgMismatch := e.writeConfig("config-mismatch.json", func(doc map[string]any) {
		strategies(doc)
		doc["user_defaults"] = map[string]any{"manual": map[string]any{"stop_loss_atr_mult": 3}}
	})
	r = e.command(cfgMismatch, []string{good}, "manual-open", "mr-ready", "--size", "0.01", "--atr", "0.5", "--dry-run")
	e.expectRefused(r, "command configuration differs", "configuration differs")

	daemon.waitFreshCycle()
	reloaded := e.writeConfig("config-a.json", func(doc map[string]any) {
		strategies(doc)
		doc["user_defaults"] = map[string]any{"manual": map[string]any{"stop_loss_atr_mult": 3}}
	})
	daemon.cmd.Process.Signal(syscall.SIGHUP)
	daemon.waitLog("reload", 30*time.Second, func(s string) bool { return strings.Contains(s, "[reload]") })
	time.Sleep(time.Second)
	if got := e.route(port+1, "mr-ready", "paper"); got.status != http.StatusConflict || got.env.Result != manualRegimeResultMismatch {
		t.Fatalf("a reload inside the generation must refuse the earlier contract: %d %+v\n%s", got.status, got.env, daemon.output())
	}
	r = e.command(reloaded, []string{good}, "manual-open", "mr-ready", "--size", "0.01", "--atr", "0.5", "--dry-run")
	e.expectRefused(r, "reload inside the generation", "HTTP 409")
	daemon.waitFreshCycle()
	if got := e.route(port+1, "mr-ready", "paper"); got.status != http.StatusOK {
		t.Fatalf("next generation after reload: %+v", got.env)
	}
	e.expectNoVenue(base, "reload")
	t.Logf("generation: a reload during the generation refused its earlier result (409) and the next cycle served the new contract")

	_, store := e.readStore(reloaded)
	adopted, _, err := LoadStateWithStore(cfgLoaded, store)
	store.Close()
	if err != nil {
		t.Fatal(err)
	}
	pos := adopted.Strategies["mr-add"].Positions["BTC"]
	if pos == nil || math.Abs(pos.Quantity-0.02) > 1e-9 || !reflect.DeepEqual(pos.RegimeSnapshots, addEvidence) || pos.EntryATR != addATR {
		t.Fatalf("add adoption replaced entry evidence: %+v", pos)
	}

	daemon.stop()
	if _, err := os.Lstat(markerA); !os.IsNotExist(err) {
		t.Fatalf("graceful shutdown left its owner marker: %v", err)
	}
	r = e.command(reloaded, []string{good}, "manual-open", "mr-ready", "--size", "0.01", "--atr", "0.5", "--dry-run")
	if r.code != 0 || !strings.Contains(r.stderr, "no scheduler owns") {
		t.Fatalf("stopped scheduler:\n%s", r)
	}
	restarted := e.startDaemon(reloaded, port+1)
	restarted.waitCycles(1)
	mr, err := readManualRegimeOwnerMarker(markerA)
	if err != nil || mr.OwnerNonce == ma.OwnerNonce {
		t.Fatalf("restart must publish a new owner nonce: %v", err)
	}
	_, store = e.readStore(reloaded)
	again, _, err := LoadStateWithStore(cfgLoaded, store)
	store.Close()
	if err != nil || !reflect.DeepEqual(again.Strategies["mr-add"].Positions["BTC"].RegimeSnapshots, addEvidence) {
		t.Fatalf("restart changed add entry evidence: %v", err)
	}
	r = e.command(reloaded, []string{good}, "manual-open", "mr-ready", "--size", "0.01", "--atr", "0.5", "--dry-run")
	if r.code != 0 || strings.Contains(r.stderr, "fallback") {
		t.Fatalf("restarted scheduler admission:\n%s", r)
	}
	restarted.cmd.Process.Kill()
	<-restarted.exited
	if _, err := readManualRegimeOwnerMarker(markerA); err != nil {
		t.Fatalf("a crash leaves its marker bytes: %v", err)
	}
	r = e.command(reloaded, []string{good}, "manual-open", "mr-ready", "--size", "0.01", "--atr", "0.5", "--dry-run")
	if r.code != 0 || !strings.Contains(r.stderr, "no scheduler owns") || !strings.Contains(r.stderr, "using the manual fallback protection policy") {
		t.Fatalf("crash-left marker must not authorize a result:\n%s", r)
	}
	r = e.command(reloaded, []string{good}, "manual-open", "mr-limit", "--size", "0.01", "--limit-price", strconv.FormatFloat(limitPx, 'f', 2, 64), "--atr", "0.5")
	e.expectRefused(r, "crash limit", "unavailable")
	wrong.stop()
	t.Logf("restart: add adoption kept original entry evidence through adoption and restart; graceful stop removed the marker; a crash-left marker authorized nothing")
}

func regimeReadinessProcessShared(t *testing.T) {
	t.Helper()
	e := newMRPEnv(t, 40)
	sockDir, err := os.MkdirTemp("", "mrp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	sock := filepath.Join(sockDir, "feed.sock")
	port := htfITFreePort(t)
	cfg := e.writeConfig("consumer.json", func(doc map[string]any) {
		doc["market_feed"] = "shared"
		doc["shared_market_feed"] = map[string]any{"primary_socket": sock}
		doc["strategies"] = []any{mrpManualStrategy("mr-ready", "short"), mrpManualStrategy("mr-limit", "short"), mrpManualStrategy("mr-warming", "long")}
	})
	feedPort := htfITFreePort(t)
	feedCfg := filepath.Join(e.dir, "feed.json")
	blob, _ := json.Marshal(map[string]any{"config_version": CurrentConfigVersion, "role": "feed", "status_port": feedPort,
		"feed": map[string]any{"source": "websocket", "socket_path": sock, "consumer_configs": []string{cfg}}})
	if err := os.WriteFile(feedCfg, blob, 0o600); err != nil {
		t.Fatal(err)
	}
	feed := e.startDaemon(feedCfg, feedPort)
	feed.waitLog("feed socket", 120*time.Second, func(s string) bool { _, err := os.Stat(sock); return err == nil })
	daemon := e.startDaemon(cfg, port)
	daemon.waitCycles(1)
	url := mrpURL(port)
	daemon.waitFreshCycle()
	before := e.route(port, "mr-ready", "paper")
	r := e.command(cfg, []string{url}, "manual-open", "mr-ready", "--size", "0.01", "--atr", "0.5")
	if r.code != 0 {
		t.Fatalf("shared ready open:\n%s\n%s", r, daemon.output())
	}
	limitPx := math.Round(mrpMark()*0.9*100) / 100
	r = e.command(cfg, []string{url}, "manual-open", "mr-limit", "--size", "0.01", "--limit-price", strconv.FormatFloat(limitPx, 'f', 2, 64), "--atr", "0.5")
	if r.code != 0 {
		t.Fatalf("shared ready limit:\n%s", r)
	}
	if after := e.route(port, "mr-ready", "paper"); after.env.Generation != before.env.Generation {
		t.Fatal("a cycle ran during the shared evidence window")
	}
	loaded, _ := e.readStore(cfg)
	evidence := entryRegimeSnapshots(before.payload, loaded.Regime)
	actions := e.pendingActions(cfg)
	limits := e.pendingLimits(cfg)
	if len(actions) != 1 || !reflect.DeepEqual(actions[0].RegimeSnapshots, evidence) || len(limits) != 1 || !reflect.DeepEqual(limits[0].RegimeSnapshots, evidence) {
		t.Fatalf("shared evidence: %+v %+v", actions, limits)
	}
	base := e.venueKinds()
	for _, args := range [][]string{
		{"manual-open", "mr-warming", "--size", "0.01", "--atr", "0.5"},
		{"manual-open", "mr-warming", "--size", "0.01", "--limit-price", strconv.FormatFloat(limitPx, 'f', 2, 64), "--atr", "0.5"},
	} {
		r = e.command(cfg, []string{url}, args...)
		e.expectRefused(r, "shared "+strings.Join(args, " "), "regime stop window long warming (")
		if !strings.Contains(r.stderr, "/101)") {
			t.Fatalf("shared warming must name the required long window bars:\n%s", r)
		}
	}
	e.expectNoVenue(base, "shared warming")
	if k := e.venueKinds(); k["fetch_atr"] != 0 || k["execute"] != 1 || k["limit_open"] != 1 {
		t.Fatalf("shared venue: %v", k)
	}
	daemon.stop()
	feed.stop()
	t.Logf("shared: open and limit admitted with the route's snapshots, warming held, venue %v", e.venueKinds())
}
