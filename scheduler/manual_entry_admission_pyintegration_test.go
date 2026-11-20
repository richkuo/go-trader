//go:build pyintegration

package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

type meaEnv struct {
	*mrpEnv
	cfg      string
	faults   string
	gate     string
	feedSock string
	port     int
}

func meaPlainStrategy(id string) map[string]any {
	sc := mrpManualStrategy(id, "short")
	delete(sc, "regime_atr_window")
	sc["stop_loss_atr_mult"] = 2
	return sc
}

func newMEAEnv(t *testing.T, mode string, strategies []any) *meaEnv {
	t.Helper()
	e := &meaEnv{mrpEnv: newMRPEnv(t, 40), faults: t.TempDir(), gate: t.TempDir(), port: htfITFreePort(t)}
	if mode == "shared" {
		sockDir, err := os.MkdirTemp("", "mea")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.RemoveAll(sockDir) })
		e.feedSock = filepath.Join(sockDir, "feed.sock")
	}
	e.cfg = e.writeConfig("entry.json", func(doc map[string]any) {
		doc["interval_seconds"] = mrpPendingCycleSeconds
		doc["status_port"] = e.port
		doc["strategies"] = strategies
		switch mode {
		case "rest":
			doc["market_feed"] = "rest"
		case "shared":
			doc["market_feed"] = "shared"
			doc["shared_market_feed"] = map[string]any{"primary_socket": e.feedSock}
		}
	})
	return e
}

func (e *meaEnv) startFeed() *mrpDaemon {
	e.t.Helper()
	feedPort := htfITFreePort(e.t)
	feedCfg := filepath.Join(e.dir, "feed.json")
	blob, _ := json.Marshal(map[string]any{"config_version": CurrentConfigVersion, "role": "feed", "status_port": feedPort,
		"feed": map[string]any{"source": "websocket", "socket_path": e.feedSock, "consumer_configs": []string{e.cfg}}})
	if err := os.WriteFile(feedCfg, blob, 0o600); err != nil {
		e.t.Fatal(err)
	}
	feed := e.startDaemon(feedCfg, feedPort)
	feed.waitLog("feed socket", 120*time.Second, func(string) bool { _, err := os.Stat(e.feedSock); return err == nil })
	return feed
}

func (e *meaEnv) scheduler() *mrpDaemon {
	e.t.Helper()
	return e.startDaemon(e.cfg, e.port, manualEntryTestSaveFaultEnv+"="+e.faults)
}

func (e *meaEnv) layout() storageLayout {
	e.t.Helper()
	cfg, err := LoadConfig(e.cfg)
	if err != nil {
		e.t.Fatal(err)
	}
	layout, err := resolveStorageLayout(cfg)
	if err != nil {
		e.t.Fatal(err)
	}
	return layout
}

func (e *meaEnv) marker() string {
	return mrpMarkerPath(filepath.Join(e.dir, "state.db"))
}

func (e *meaEnv) witnessFile() string {
	return manualRegimeWitnessPath(filepath.Join(e.dir, "state.db"))
}

func (e *meaEnv) guard(p RiskPartition) string {
	return manualRegimeHoldGuardPath(filepath.Join(e.dir, "state.db"), p)
}

func meaWitnessPID(t *testing.T, path string, offset int64) int {
	t.Helper()
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	lk := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: io.SeekStart, Start: offset, Len: 1}
	if err := syscall.FcntlFlock(f.Fd(), syscall.F_GETLK, &lk); err != nil {
		t.Fatal(err)
	}
	if lk.Type == syscall.F_UNLCK {
		return 0
	}
	return int(lk.Pid)
}

func (e *meaEnv) slot(p RiskPartition) int64 {
	e.t.Helper()
	_, offset, ok := manualRegimeWitnessSlot(e.layout(), p)
	if !ok {
		e.t.Fatalf("no witness slot for %s", partitionLabel(p))
	}
	return offset
}

func (e *meaEnv) waitWitness(p RiskPartition, want bool) {
	e.t.Helper()
	offset := e.slot(p)
	deadline := time.Now().Add(150 * time.Second)
	for {
		owner := meaWitnessPID(e.t, e.witnessFile(), manualRegimeOwnershipWitnessOffset)
		got := meaWitnessPID(e.t, e.witnessFile(), offset)
		if owner > 0 && (got == owner) == want && (want || got == 0) {
			return
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("%s witness never became %v (owner %d, guard %d)", partitionLabel(p), want, owner, got)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (e *meaEnv) fault(p RiskPartition, on bool) {
	e.t.Helper()
	path := filepath.Join(e.faults, manualEntryTestPartitionName(p))
	if on {
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			e.t.Fatal(err)
		}
		return
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		e.t.Fatal(err)
	}
}

func (e *meaEnv) armGate(stage string) {
	e.t.Helper()
	name := manualEntryTestStageName(stage)
	for _, f := range []string{"blocked-" + name, "release-" + name} {
		os.Remove(filepath.Join(e.gate, f))
	}
	if err := os.WriteFile(filepath.Join(e.gate, "arm-"+name), nil, 0o600); err != nil {
		e.t.Fatal(err)
	}
}

func (e *meaEnv) waitGate(stage string) {
	e.t.Helper()
	path := filepath.Join(e.gate, "blocked-"+manualEntryTestStageName(stage))
	deadline := time.Now().Add(120 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("command never reached the %q barrier", stage)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (e *meaEnv) releaseGate(stage string) {
	e.t.Helper()
	name := manualEntryTestStageName(stage)
	os.Remove(filepath.Join(e.gate, "arm-"+name))
	if err := os.WriteFile(filepath.Join(e.gate, "release-"+name), nil, 0o600); err != nil {
		e.t.Fatal(err)
	}
}

func (e *meaEnv) run(extra []string, args ...string) mrpRun {
	e.t.Helper()
	return e.command(e.cfg, append([]string{manualEntryTestGateEnv + "=" + e.gate}, extra...), args...)
}

func (e *meaEnv) runAsync(extra []string, args ...string) <-chan mrpRun {
	e.t.Helper()
	return e.commandAsync(e.cfg, append([]string{manualEntryTestGateEnv + "=" + e.gate}, extra...), args...)
}

func (e *meaEnv) actions() map[string]int {
	out := map[string]int{}
	for _, a := range e.pendingActions(e.cfg) {
		out[a.StrategyID+"/"+a.Action]++
	}
	return out
}

func (e *meaEnv) limitRows(strategyID string) []PendingLimitOrder {
	var out []PendingLimitOrder
	for _, row := range e.pendingLimits(e.cfg) {
		if row.StrategyID == strategyID {
			out = append(out, row)
		}
	}
	return out
}

func (e *meaEnv) seedLimit(strategyID string, oid int64) PendingLimitOrder {
	e.t.Helper()
	cfg, err := LoadConfig(e.cfg)
	if err != nil {
		e.t.Fatal(err)
	}
	store, err := openToolStateStore(cfg)
	if err != nil {
		e.t.Fatal(err)
	}
	defer store.Close()
	row := PendingLimitOrder{StrategyID: strategyID, Symbol: "BTC", Side: "long", OrderOID: oid, LimitPrice: 90, OrderSize: 0.01, TIF: "Alo", CreatedAt: time.Now().UTC()}
	id, err := store.InsertPendingLimitOrder(row)
	if err != nil {
		e.t.Fatal(err)
	}
	row.ID = id
	return row
}

func (e *meaEnv) dropLimits(strategyID string) {
	e.t.Helper()
	cfg, err := LoadConfig(e.cfg)
	if err != nil {
		e.t.Fatal(err)
	}
	store, err := openToolStateStore(cfg)
	if err != nil {
		e.t.Fatal(err)
	}
	defer store.Close()
	for _, row := range e.limitRows(strategyID) {
		if err := store.DeletePendingLimitOrder(row.ID); err != nil {
			e.t.Fatal(err)
		}
	}
}

func (e *meaEnv) expectNoRows(what string, venue map[string]int, ids ...string) {
	e.t.Helper()
	e.expectNoVenue(venue, what)
	for _, a := range e.pendingActions(e.cfg) {
		for _, id := range ids {
			if a.StrategyID == id {
				e.t.Fatalf("%s: refused entry left a pending %s action for %s", what, a.Action, id)
			}
		}
	}
	for _, row := range e.pendingLimits(e.cfg) {
		for _, id := range ids {
			if row.StrategyID == id && row.OrderOID != 4401 {
				e.t.Fatalf("%s: refused entry left a pending limit row for %s: %+v", what, id, row)
			}
		}
	}
}

func meaEntryStrategies(prefix string) []any {
	plain := []string{"open", "open-clear", "open-live", "limit", "limit-clear", "add", "add-clear", "close"}
	out := make([]any, 0, 16)
	for _, name := range plain {
		out = append(out, meaPlainStrategy(prefix+name))
	}
	for _, name := range []string{"regime", "regime-pct", "regime-mult", "regime-clear"} {
		out = append(out, mrpManualStrategy(prefix+name, "short"))
	}
	return out
}

func meaSeed(e *meaEnv, ids ...string) {
	e.t.Helper()
	openedAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	positions := map[string]*Position{}
	for _, id := range ids {
		positions[id] = &Position{Symbol: "BTC", Side: "long", Quantity: 0.01, InitialQuantity: 0.01, AvgCost: 100, EntryATR: 0.7, OwnerStrategyID: id, TradePositionID: id + "-pos", OpenedAt: openedAt}
	}
	e.seedPositions(e.cfg, positions)
}

func regimeReadinessEntryAdmissionProcess(t *testing.T, mode string) {
	t.Helper()
	p := "ea-"
	e := newMEAEnv(t, mode, meaEntryStrategies(p))
	meaSeed(e, p+"add", p+"add-clear", p+"close")
	if mode == "shared" {
		feed := e.startFeed()
		defer feed.stop()
	}
	daemon := e.scheduler()
	daemon.waitCycles(1)
	e.waitWitness(defaultPaperPartition, true)
	e.waitWitness(livePartition, true)
	held := "holds the paper partition"
	atr := []string{"--atr", "0.5"}
	open := func(id string, extra ...string) []string {
		return append(append([]string{"manual-open", p + id, "--size", "0.01"}, atr...), extra...)
	}
	limitPx := strconv.FormatFloat(mrpMark()*0.9, 'f', 2, 64)
	limit := func(id string) []string {
		return append([]string{"manual-open", p + id, "--size", "0.01", "--limit-price", limitPx}, atr...)
	}
	add := func(id string) []string { return []string{"manual-add", p + id, "--size", "0.01"} }

	e.fault(defaultPaperPartition, true)
	e.waitWitness(defaultPaperPartition, false)
	resting := e.seedLimit(p+"add", 4401)
	venue := e.venueKinds()
	for _, c := range []struct {
		what string
		args []string
	}{
		{"market open without a regime stop dependency", open("open")},
		{"market open with an explicit --stop-loss-pct", open("regime-pct", "--stop-loss-pct", "2")},
		{"market open with an explicit --stop-loss-atr-mult", open("regime-mult", "--stop-loss-atr-mult", "2")},
		{"owner-verified regime market open", open("regime")},
		{"limit open without a regime stop dependency", limit("limit")},
		{"add with a resting remainder", add("add")},
	} {
		r := e.run([]string{manualEntryTestCancelEnv + "=0"}, c.args...)
		e.expectRefused(r, mode+" held "+c.what, held)
		t.Logf("%s hold: %s refused: %s", mode, c.what, strings.TrimSpace(r.stderr))
	}
	e.expectNoRows(mode+" held paper partition", venue, p+"open", p+"regime-pct", p+"regime-mult", p+"regime", p+"limit", p+"add")
	r := e.run(nil, "manual-close", p+"close")
	if strings.Contains(r.stderr, "entry admission") || strings.Contains(r.stderr, "holds the paper partition") {
		t.Fatalf("%s: a reduction was gated by the entry admission:\n%s", mode, r)
	}
	if got := e.venueKinds(); r.code != 0 || got["execute"] != venue["execute"]+1 {
		t.Fatalf("%s: the reduction under a paper hold did not reach the venue (%v -> %v):\n%s", mode, venue, got, r)
	}
	t.Logf("%s hold: the manual-close reduction still reached the venue", mode)
	if rows := e.limitRows(p + "add"); len(rows) != 1 || rows[0].ID != resting.ID || rows[0].CancelRequested || rows[0].OrderOID != resting.OrderOID {
		t.Fatalf("%s held add changed its resting row: %+v", mode, rows)
	}
	e.dropLimits(p + "add")

	e.fault(livePartition, true)
	e.fault(defaultPaperPartition, false)
	e.waitWitness(defaultPaperPartition, true)
	e.waitWitness(livePartition, false)
	venue = e.venueKinds()
	if r := e.run(nil, open("open-live")...); r.code != 0 {
		t.Fatalf("%s: a live hold on the shared state file blocked a paper open:\n%s", mode, r)
	}
	if got := e.venueKinds(); got["execute"] != venue["execute"]+1 {
		t.Fatalf("%s: paper open under a live hold: venue %v -> %v", mode, venue, got)
	}
	t.Logf("%s: a live hold on the same state file left the paper partition open", mode)
	e.fault(livePartition, false)
	e.waitWitness(livePartition, true)

	venue = e.venueKinds()
	e.seedLimit(p+"add-clear", 4402)
	for _, args := range [][]string{open("open-clear"), open("regime-clear", "--stop-loss-pct", "2"), limit("limit-clear"), add("add-clear")} {
		r := e.run([]string{manualEntryTestCancelEnv + "=0"}, args...)
		if r.code != 0 {
			t.Fatalf("%s clear %v:\n%s\n%s", mode, args, r, mrpTimeline(daemon.output()))
		}
	}
	got := e.venueKinds()
	if got["execute"] != venue["execute"]+3 || got["limit_open"] != venue["limit_open"]+1 || got["cancel"] != venue["cancel"]+1 {
		t.Fatalf("%s clear guard: venue %v -> %v", mode, venue, got)
	}
	if rows := e.limitRows(p + "limit-clear"); len(rows) != 1 || rows[0].OrderOID != 8001 {
		t.Fatalf("%s clear limit row: %+v", mode, rows)
	}
	if rows := e.limitRows(p + "add-clear"); len(rows) != 0 {
		t.Fatalf("%s: the cleanly cancelled remainder row was kept: %+v", mode, rows)
	}
	t.Logf("%s: after the partition saved again, market, stop-override, limit and add entries placed (venue %v)", mode, got)
	daemon.stop()
}

func regimeReadinessEntryAdmissionTransitions(t *testing.T) {
	t.Helper()
	p := "et-"
	strategies := []any{}
	for _, name := range []string{"open-exit", "open-swap", "open-after", "open-shared", "open-excl", "open-ok", "solo", "solo-race", "solo-bound", "solo-limit", "unpub", "add-cx", "add-fill", "record"} {
		strategies = append(strategies, meaPlainStrategy(p+name))
	}
	strategies = append(strategies, mrpManualStrategy(p+"ratchet", "short"), mrpManualStrategy(p+"ratchet-limit", "short"), mrpManualStrategy(p+"ratchet-unpub", "short"))
	e := newMEAEnv(t, "websocket", strategies)
	meaSeed(e, p+"add-cx", p+"add-fill")
	daemon := e.scheduler()
	daemon.waitCycles(1)
	e.waitWitness(defaultPaperPartition, true)
	atr := []string{"--atr", "0.5"}
	open := func(id string) []string {
		return append([]string{"manual-open", p + id, "--size", "0.01"}, atr...)
	}
	placement := "before placement"

	venue := e.venueKinds()
	resting := e.seedLimit(p+"add-cx", 4501)
	e.armGate(placement)
	pending := e.runAsync([]string{manualEntryTestCancelEnv + "=0"}, "manual-add", p+"add-cx", "--size", "0.01")
	e.waitGate(placement)
	e.fault(defaultPaperPartition, true)
	e.waitWitness(defaultPaperPartition, false)
	e.releaseGate(placement)
	r := <-pending
	e.expectRefused(r, "hold during cancellation", "holds the paper partition")
	got := e.venueKinds()
	if got["cancel"] != venue["cancel"]+1 || got["limit_status"] != venue["limit_status"]+1 || got["execute"] != venue["execute"] {
		t.Fatalf("hold during cancellation: venue %v -> %v", venue, got)
	}
	if rows := e.limitRows(p + "add-cx"); len(rows) != 0 {
		t.Fatalf("hold during cancellation kept the off-book row %d: %+v", resting.ID, rows)
	}
	if acts := e.actions(); acts[p+"add-cx/add"] != 0 {
		t.Fatalf("hold during cancellation queued an action: %v", acts)
	}
	t.Logf("cancellation: the remainder was cancelled and its off-book row deleted, then a hold raised during the cancellation refused the add with no order: %s", strings.TrimSpace(r.stderr))
	e.fault(defaultPaperPartition, false)
	e.waitWitness(defaultPaperPartition, true)

	venue = e.venueKinds()
	unadopted := e.seedLimit(p+"add-fill", 4502)
	r = e.run([]string{manualEntryTestCancelEnv + "=0.005"}, "manual-add", p+"add-fill", "--size", "0.01")
	e.expectRefused(r, "unadopted remainder fill", "unadopted fill")
	if rows := e.limitRows(p + "add-fill"); len(rows) != 1 || rows[0].ID != unadopted.ID || !rows[0].CancelRequested {
		t.Fatalf("unadopted fill: the row must stay for scheduler reconciliation: %+v", rows)
	}
	if got := e.venueKinds(); got["execute"] != venue["execute"] {
		t.Fatalf("unadopted fill placed an add: %v -> %v", venue, got)
	}
	e.dropLimits(p + "add-fill")
	t.Logf("cancellation: a remainder with an unadopted fill stays queued for the scheduler and the add sends no order")

	guard, err := os.Open(e.guard(defaultPaperPartition))
	if err != nil {
		t.Fatal(err)
	}
	e.fault(defaultPaperPartition, true)
	e.waitWitness(defaultPaperPartition, false)
	if err := syscall.Flock(int(guard.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	e.fault(defaultPaperPartition, false)
	daemon.waitFreshCycle()
	daemon.waitFreshCycle()
	if pid := meaWitnessPID(t, e.witnessFile(), e.slot(defaultPaperPartition)); pid != 0 {
		t.Fatalf("a failed guard relock published the clear witness (pid %d)", pid)
	}
	venue = e.venueKinds()
	r = e.run(nil, open("open-shared")...)
	e.expectRefused(r, "failed relock under shared contention", "holds the paper partition")
	e.expectNoVenue(venue, "failed relock")
	syscall.Flock(int(guard.Fd()), syscall.LOCK_UN)
	e.waitWitness(defaultPaperPartition, true)
	t.Logf("contention: a shared probe that blocked the relock kept entry held after the save succeeded; the next save cleared it")

	e.fault(defaultPaperPartition, true)
	e.waitWitness(defaultPaperPartition, false)
	if err := syscall.Flock(int(guard.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	marker, err := readManualRegimeOwnerMarker(e.marker())
	if err != nil {
		t.Fatal(err)
	}
	if err := checkManualRegimeHoldGuard(e.layout(), defaultPaperPartition, marker.OwnerNonce); err != nil {
		t.Fatalf("a foreign exclusive lock should look clear to the lock-only probe: %v", err)
	}
	r = e.run(nil, open("open-excl")...)
	e.expectRefused(r, "foreign exclusive guard lock", "attests no clear persistence guard")
	e.expectNoVenue(venue, "foreign exclusive guard lock")
	syscall.Flock(int(guard.Fd()), syscall.LOCK_UN)
	guard.Close()
	e.fault(defaultPaperPartition, false)
	e.waitWitness(defaultPaperPartition, true)
	if r := e.run(nil, open("open-ok")...); r.code != 0 {
		t.Fatalf("clear guard after contention:\n%s", r)
	}
	t.Logf("contention: an unrelated exclusive lock on a held guard passed the lock-only probe and was refused by the witness check")

	venue = e.venueKinds()
	e.armGate(placement)
	pending = e.runAsync(nil, open("open-exit")...)
	e.waitGate(placement)
	daemon.stop()
	e.releaseGate(placement)
	r = <-pending
	e.expectRefused(r, "owner exited before placement", "owner identity disappeared")
	e.expectNoRows("owner exit", venue, p+"open-exit")
	t.Logf("owner: the pinned scheduler drained and exited at the barrier; the open refused: %s", strings.TrimSpace(r.stderr))

	first := e.scheduler()
	first.waitCycles(1)
	e.waitWitness(defaultPaperPartition, true)
	e.armGate(placement)
	pending = e.runAsync(nil, open("open-swap")...)
	e.waitGate(placement)
	first.cmd.Process.Kill()
	<-first.exited
	second := e.scheduler()
	second.waitCycles(1)
	e.waitWitness(defaultPaperPartition, true)
	e.releaseGate(placement)
	r = <-pending
	e.expectRefused(r, "owner changed before placement", "owner identity changed")
	e.expectNoRows("owner change", venue, p+"open-swap")
	if r := e.run(nil, open("open-after")...); r.code != 0 {
		t.Fatalf("a fresh command under the new owner:\n%s", r)
	}
	t.Logf("owner: a replacement scheduler at the barrier refused the pinned command; a fresh command under the new owner placed")
	second.stop()

	venue = e.venueKinds()
	limits := len(e.pendingLimits(e.cfg))
	if r := e.run(nil, open("solo")...); r.code != 0 {
		t.Fatalf("standalone market open:\n%s", r)
	}
	limitPx := strconv.FormatFloat(mrpMark()*0.9, 'f', 2, 64)
	if r := e.run(nil, append([]string{"manual-open", p + "solo-limit", "--size", "0.01", "--limit-price", limitPx}, atr...)...); r.code != 0 {
		t.Fatalf("standalone limit open:\n%s", r)
	}
	r = e.run(nil, open("ratchet")...)
	if r.code != 0 || !strings.Contains(r.stderr, "this command holds them for a standalone entry") || !strings.Contains(r.stderr, "using the manual fallback protection policy") {
		t.Fatalf("standalone eligible ratchet fallback:\n%s", r)
	}
	r = e.run(nil, append([]string{"manual-open", p + "ratchet-limit", "--size", "0.01", "--limit-price", limitPx}, atr...)...)
	e.expectRefused(r, "standalone regime-dependent limit", "unavailable")
	got = e.venueKinds()
	if got["execute"] != venue["execute"]+2 || got["limit_open"] != venue["limit_open"]+1 || got["fetch_atr"] != venue["fetch_atr"] {
		t.Fatalf("standalone venue %v -> %v", venue, got)
	}
	if acts := e.actions(); acts[p+"solo/open"] != 1 || acts[p+"ratchet/open"] != 1 || len(e.pendingLimits(e.cfg)) != limits+1 {
		t.Fatalf("standalone rows: %v, limits %d", acts, len(e.pendingLimits(e.cfg)))
	}
	t.Logf("standalone: market and limit opens placed under exclusive ownership; the eligible ratchet fallback applied; a regime-dependent limit refused")

	venue = e.venueKinds()
	e.armGate(placement)
	pending = e.runAsync(nil, open("solo-race")...)
	e.waitGate(placement)
	waiting := e.scheduler()
	waiting.waitLog("the wait for the standalone entry", 120*time.Second, func(out string) bool {
		return strings.Contains(out, "a standalone manual entry command") && strings.Contains(out, "waiting up to")
	})
	select {
	case <-waiting.exited:
		t.Fatalf("a scheduler started during a standalone entry exited:\n%s", waiting.output())
	case <-time.After(2 * time.Second):
	}
	if strings.Contains(waiting.output(), "Cycle 1 complete") {
		t.Fatalf("a scheduler ran a cycle on files a standalone command held:\n%s", waiting.output())
	}
	e.releaseGate(placement)
	if r := <-pending; r.code != 0 {
		t.Fatalf("standalone entry while a scheduler waited:\n%s", r)
	}
	waiting.waitCycles(1)
	e.waitWitness(defaultPaperPartition, true)
	if pid := meaWitnessPID(t, e.witnessFile(), manualRegimeOwnershipWitnessOffset); pid != waiting.cmd.Process.Pid {
		t.Fatalf("ownership witness pid %d, want the waiting scheduler %d", pid, waiting.cmd.Process.Pid)
	}
	for _, spec := range e.layout().Files {
		if spec.InMemory {
			continue
		}
		if pid, held := probeStateDBLockHolder(spec.Path); !held || pid != waiting.cmd.Process.Pid {
			t.Fatalf("state file %s holder pid %d (held %v), want the waiting scheduler %d", spec.Path, pid, held, waiting.cmd.Process.Pid)
		}
	}
	if got := e.venueKinds(); got["execute"] != venue["execute"]+1 {
		t.Fatalf("standalone entry venue after the wait: %v -> %v", venue, got)
	}
	t.Logf("standalone: a scheduler starting during the entry waited; the command placed, then the scheduler started and owns every state file")

	refused := func(what string, d *mrpDaemon, want ...string) {
		t.Helper()
		select {
		case <-d.exited:
		case <-time.After(120 * time.Second):
			t.Fatalf("%s: the scheduler did not exit:\n%s", what, d.output())
		}
		if code := d.cmd.ProcessState.ExitCode(); code != ExitSingletonLock {
			t.Fatalf("%s: scheduler exited %d, want %d:\n%s", what, code, ExitSingletonLock, d.output())
		}
		for _, w := range want {
			if !strings.Contains(d.output(), w) {
				t.Fatalf("%s: scheduler output lacks %q:\n%s", what, w, d.output())
			}
		}
	}
	rival := e.scheduler()
	refused("a second scheduler beside a running scheduler", rival, "another go-trader is already running")
	if strings.Contains(rival.output(), "waiting up to") {
		t.Fatalf("a second scheduler waited on a running scheduler:\n%s", rival.output())
	}
	waiting.stop()
	exclusive, err := acquireStateOwnership(e.layout().Files)
	if err != nil {
		t.Fatal(err)
	}
	rival = e.scheduler()
	refused("a scheduler beside a plain ownership holder", rival, "another go-trader is already running")
	if strings.Contains(rival.output(), "waiting up to") {
		t.Fatalf("a scheduler waited on a plain ownership holder:\n%s", rival.output())
	}
	exclusive.Release()
	t.Logf("standalone: a running scheduler and a plain ownership holder (the lock backfill --apply and --once take) still made a starting scheduler exit %d with no wait", ExitSingletonLock)

	e.armGate(placement)
	pending = e.runAsync(nil, open("solo-bound")...)
	e.waitGate(placement)
	startedAt := time.Now()
	bounded := e.startDaemon(e.cfg, e.port, manualEntryTestSaveFaultEnv+"="+e.faults, manualEntryTestWaitEnv+"=1500")
	refused("a scheduler beside a standalone entry that outlasts the wait", bounded, "a standalone manual entry command", "still holds the lock", "after a 1.5s wait")
	if waited := time.Since(startedAt); waited < 1500*time.Millisecond {
		t.Fatalf("the scheduler refused after %s, before the 1.5s wait ended:\n%s", waited, bounded.output())
	}
	e.releaseGate(placement)
	if r := <-pending; r.code != 0 {
		t.Fatalf("standalone entry after the bounded scheduler refusal:\n%s", r)
	}
	t.Logf("standalone: a command that outlasted the wait made the scheduler exit %d with the singleton refusal; the command kept its locks and placed", ExitSingletonLock)

	cfg, err := LoadConfig(e.cfg)
	if err != nil {
		t.Fatal(err)
	}
	layout, err := resolveStorageLayout(cfg)
	if err != nil {
		t.Fatal(err)
	}
	stale := manualRegimeOwnerMarker{Version: manualRegimeOwnerMarkerVersion, OwnerNonce: strings.Repeat("ab", manualRegimeOwnerNonceBytes), Secret: strings.Repeat("cd", manualRegimeOwnerNonceBytes)}
	stale = manualRegimeMarkerFor(layout.Files[0], manualRegimeOwnerIdentity{Nonce: stale.OwnerNonce, Secret: stale.Secret})
	if err := writeManualRegimeOwnerMarker(e.marker(), stale); err != nil {
		t.Fatal(err)
	}
	owned, err := acquireStateOwnership(layout.Files)
	if err != nil {
		t.Fatal(err)
	}
	venue = e.venueKinds()
	r = e.run(nil, open("unpub")...)
	e.expectRefused(r, "held files with a stale marker", "holds no ownership witness")
	r = e.run(nil, open("ratchet-unpub")...)
	e.expectRefused(r, "regime fallback with an unproven owner", "holds no ownership witness")
	os.Remove(e.marker())
	r = e.run(nil, open("unpub")...)
	e.expectRefused(r, "held files with no published identity", "has not published its scheduler identity")
	r = e.run(nil, open("ratchet-unpub")...)
	e.expectRefused(r, "regime fallback with an unpublished owner", "has not published its scheduler identity")
	e.expectNoRows("unpublished owner", venue, p+"unpub", p+"ratchet-unpub")
	owned.Release()
	t.Logf("identity: locked files with a stale or no marker refused market entries and the regime fallback")

	split := e.writeConfig("split.json", func(doc map[string]any) {
		doc["interval_seconds"] = mrpPendingCycleSeconds
		doc["db_file"] = filepath.Join(e.dir, "split-live.db")
		doc["paper_db_file"] = filepath.Join(e.dir, "split-paper.db")
		doc["strategies"] = []any{meaPlainStrategy(p + "split")}
	})
	splitCfg, err := LoadConfig(split)
	if err != nil {
		t.Fatal(err)
	}
	splitLayout, err := resolveStorageLayout(splitCfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(splitLayout.Files) != 2 {
		t.Fatalf("split layout: %+v", splitLayout)
	}
	part, err := acquireStateOwnership(splitLayout.Files[:1])
	if err != nil {
		t.Fatal(err)
	}
	r = e.command(split, nil, "manual-open", p+"split", "--size", "0.01", "--atr", "0.5")
	e.expectRefused(r, "partial ownership", "only part of the configured state-file set")
	part.Release()
	paperLock := stateDBLockPath(splitLayout.Files[1].Path)
	os.Remove(paperLock)
	if err := os.Mkdir(paperLock, 0o700); err != nil {
		t.Fatal(err)
	}
	r = e.command(split, nil, "manual-open", p+"split", "--size", "0.01", "--atr", "0.5")
	e.expectRefused(r, "unreadable ownership lock", "is not a regular file")
	os.Remove(paperLock)
	e.expectNoVenue(venue, "partial or unreadable ownership")
	t.Logf("identity: partial ownership and an unreadable ownership lock refused before any order")

	r = e.run(nil, append(open("solo"), "--dry-run")...)
	if r.code != 0 || !strings.Contains(r.stdout, "[dry-run] manual-open") {
		t.Fatalf("dry-run keeps its checks:\n%s", r)
	}
	r = e.run(nil, "manual-open", p+"record", "--size", "0.01", "--fill-price", "100", "--record-only")
	if r.code != 0 {
		t.Fatalf("record-only adoption:\n%s", r)
	}
	e.expectNoVenue(venue, "dry-run and record-only")
	t.Logf("unchanged: dry-run and record-only need no entry admission")
}
