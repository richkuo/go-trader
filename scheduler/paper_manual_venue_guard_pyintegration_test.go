//go:build pyintegration

package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func paperManualGuardStandInInterpreter(t *testing.T) func() []string {
	t.Helper()
	dir := t.TempDir()
	marker := filepath.Join(dir, "launches.log")
	py := filepath.Join(dir, ".venv", "bin", "python3")
	if err := os.MkdirAll(filepath.Dir(py), 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\necho \"$*\" >> '" + marker + "'\necho '{\"platform\":\"hyperliquid\",\"error\":\"stand-in interpreter\"}'\nexit 1\n"
	if err := os.WriteFile(py, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	return func() []string {
		raw, err := os.ReadFile(marker)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			t.Fatal(err)
		}
		return strings.Fields(strings.ReplaceAll(strings.TrimSpace(string(raw)), " ", "_"))
	}
}

const paperManualGuardAllOps = hlWriteExecute | hlWriteClose | hlWriteStop | hlWriteProtection | hlWriteLimitOpen | hlWriteCancel

func TestPaperManualVenueGuardRunnersRefuseBeforeLaunch(t *testing.T) {
	launches := paperManualGuardStandInInterpreter(t)
	paper := hlStrategyWriteAuth(StrategyConfig{ID: "pm", Type: "manual", Platform: "hyperliquid", Symbol: "BTC", Args: []string{"hold", "BTC", "1h", "--mode=paper"}}, "BTC", paperManualGuardAllOps)
	liveETH := hlStrategyWriteAuth(StrategyConfig{ID: "lm", Type: "manual", Platform: "hyperliquid", Symbol: "ETH", Args: []string{"hold", "ETH", "1h", "--mode=live"}}, "ETH", paperManualGuardAllOps)
	wrongOp := hlAccountRecoveryWriteAuth("BTC", hlWriteCancel)
	for name, auth := range map[string]hlWriteAuth{"zero": {}, "paper owner": paper, "live owner on another coin": liveETH, "wrong operation": wrongOp} {
		refused := map[string]error{}
		_, _, refused["execute"] = RunHyperliquidExecute(auth, "shared_scripts/check_hyperliquid.py", "BTC", "sell", 1, 0, 0, 0, "", 0, hlCloseModeWhole, hlExecuteSnapshot{})
		_, _, refused["stop update"] = RunHyperliquidUpdateStopLoss(auth, "shared_scripts/check_hyperliquid.py", "BTC", "long", 1, 90, 7)
		_, _, refused["protection sync"] = RunHyperliquidSyncProtection(auth, "shared_scripts/check_hyperliquid.py", hlProtectionPlan{Symbol: "BTC", Side: "long", Size: 1}, nil)
		sized, _, sizedErr := RunHyperliquidSizedClose(hyperliquidLiveCloseScript, hlSizedCloseRequest{Symbol: "BTC", Side: "sell", Mode: hlCloseModeReduceOnly, Size: 1, Auth: auth})
		refused["sized close"] = sizedErr
		if out := classifyHLSizedClose(sized, sizedErr); !out.Known || !out.NotSent {
			t.Fatalf("%s: a refused sized close must classify as not sent: %+v", name, out)
		}
		whole, _, wholeErr := RunHyperliquidClose(auth, hyperliquidLiveCloseScript, "BTC", nil, nil)
		refused["whole close"] = wholeErr
		if whole == nil || whole.OrderOutcome != "not_sent" {
			t.Fatalf("%s: a refused whole close must report not_sent: %+v", name, whole)
		}
		_, _, refused["force close"] = RunHyperliquidCloseCancelAfterFill(auth, hyperliquidLiveCloseScript, "BTC", nil, []int64{3})
		_, _, refused["limit open"] = RunHyperliquidLimitOpen(auth, "shared_scripts/check_hyperliquid.py", "BTC", "buy", 1, 90, "Alo", "", 0, hlExecuteSnapshot{})
		_, _, refused["cancel"] = RunHyperliquidCancelOrder(auth.forOrder(4), "shared_scripts/check_hyperliquid.py", "BTC", 5)
		for op, err := range refused {
			if !isHLWriteRefused(err) || !strings.Contains(err.Error(), "no order was sent") {
				t.Fatalf("%s %s must refuse before launch: %v", name, op, err)
			}
		}
	}
	if got := launches(); len(got) != 0 {
		t.Fatalf("a refused Hyperliquid write reached the interpreter: %v", got)
	}
	if _, _, err := RunHyperliquidCancelOrder(hlAccountRecoveryWriteAuth("BTC", hlWriteCancel).forOrder(5), "shared_scripts/check_hyperliquid.py", "BTC", 5); isHLWriteRefused(err) {
		t.Fatalf("a bound recovery cancel must reach the launcher: %v", err)
	}
	if got := launches(); len(got) != 1 || !strings.Contains(got[0], "--cancel-order") || !strings.Contains(got[0], "--oid=5") {
		t.Fatalf("the authorized cancel must reach the stand-in once: %v", got)
	}
}

func TestPaperManualVenueGuardPaperOnlyPolicy(t *testing.T) {
	launches := paperManualGuardStandInInterpreter(t)
	prev := processPaperOnlyWrites.Load()
	applyProcessVenueWritePolicy(&Config{PaperOnly: true})
	t.Cleanup(func() { processPaperOnlyWrites.Store(prev) })
	live := hlAccountRecoveryWriteAuth("BTC", paperManualGuardAllOps)
	writes := map[string]error{}
	_, _, writes["hl execute"] = RunHyperliquidExecute(live, "shared_scripts/check_hyperliquid.py", "BTC", "buy", 1, 0, 0, 0, "", 0, hlCloseModeNone, hlExecuteSnapshot{})
	_, _, writes["hl stop"] = RunHyperliquidUpdateStopLoss(live, "shared_scripts/check_hyperliquid.py", "BTC", "long", 1, 90, 0)
	_, _, writes["hl protection"] = RunHyperliquidSyncProtection(live, "shared_scripts/check_hyperliquid.py", hlProtectionPlan{Symbol: "BTC", Side: "long", Size: 1}, nil)
	_, _, writes["hl sized close"] = RunHyperliquidSizedClose(hyperliquidLiveCloseScript, hlSizedCloseRequest{Symbol: "BTC", Side: "sell", Mode: hlCloseModeReduceOnly, Size: 1, Auth: live})
	_, _, writes["hl whole close"] = RunHyperliquidClose(live, hyperliquidLiveCloseScript, "BTC", nil, nil)
	_, _, writes["hl limit"] = RunHyperliquidLimitOpen(live, "shared_scripts/check_hyperliquid.py", "BTC", "buy", 1, 90, "Alo", "", 0, hlExecuteSnapshot{})
	_, _, writes["hl cancel"] = RunHyperliquidCancelOrder(live.forOrder(5), "shared_scripts/check_hyperliquid.py", "BTC", 5)
	_, _, writes["okx execute"] = RunOKXExecute("shared_scripts/check_okx.py", "BTC-USDT-SWAP", "buy", 1, "swap")
	_, _, writes["okx close"] = RunOKXClose("shared_scripts/close_okx_position.py", "BTC-USDT-SWAP", nil)
	_, _, writes["topstep execute"] = RunTopStepExecute("shared_scripts/check_topstep.py", "ES", "buy", 1)
	_, _, writes["topstep close"] = RunTopStepClose("shared_scripts/close_topstep_position.py", "ES")
	_, _, writes["robinhood execute"] = RunRobinhoodExecute("shared_scripts/check_robinhood.py", "BTC", "buy", 10, 0, "ref", RobinhoodSellArgs{})
	_, _, writes["robinhood close"] = RunRobinhoodClose("shared_scripts/close_robinhood_position.py", "BTC", "ref", 1, "acct")
	_, _, writes["robinhood resolve with cancel"] = RunRobinhoodResolveOrder("shared_scripts/check_robinhood.py", RobinhoodResolveRequest{Symbol: "BTC", Side: "buy", RefID: "ref", CancelOpen: true})
	_, _, writes["side effect with no mode"] = runPythonSideEffect("shared_scripts/check_hyperliquid.py", []string{"--execute", "--symbol=BTC", "--side=buy", "--size=1"})
	quote, quoteErr := newQuoteLiveVenue("hyperliquid:test:0xquote", "").sideEffect([]string{"--quote-place", "--symbol=BTC"})
	if quoteErr != nil || quote.Status != quoteOutcomeRejected || !strings.Contains(quote.Detail, "no order was sent") {
		t.Fatalf("a refused quote write must be a known rejection, never unknown: %+v %v", quote, quoteErr)
	}
	for name, err := range writes {
		if !isVenueWriteRefused(err) || !strings.Contains(err.Error(), "no order was sent") {
			t.Fatalf("%s under paper_only must refuse before launch: %v", name, err)
		}
	}
	if got := launches(); len(got) != 0 {
		t.Fatalf("paper_only reached the interpreter for a write: %v", got)
	}
	for _, args := range [][]string{{"hold", "BTC", "1h", "--mode=paper", "--execute"}, {"hold", "BTC", "1h", "--cancel-order", "--oid=1"}, {"hold", "BTC", "1h", "--sync-protection"}} {
		if _, _, err := runPythonReadOnlyPriority(pythonPriorityDefault, "shared_scripts/check_hyperliquid.py", args, nil); !isVenueWriteRefused(err) {
			t.Fatalf("a check launch whose args select a write must refuse: %v %v", args, err)
		}
	}
	if _, _, err := runPythonReadOnly(hyperliquidLiveCloseScript, []string{"--symbol=BTC"}); !isVenueWriteRefused(err) {
		t.Fatalf("a read-only launch of a close script must refuse: %v", err)
	}
	if got := launches(); len(got) != 0 {
		t.Fatalf("a refused read-only launch reached the interpreter: %v", got)
	}
	_, _ = runHyperliquidListAllOpenOrdersFn("shared_scripts/check_hyperliquid.py")
	_, _, _ = RunHyperliquidLimitStatus("shared_scripts/check_hyperliquid.py", "BTC", []int64{1}, 0)
	_, _, _ = RunRobinhoodResolveOrder("shared_scripts/check_robinhood.py", RobinhoodResolveRequest{Symbol: "BTC", Side: "buy", RefID: "ref"})
	_, _, _ = RunHyperliquidCheck(pythonPriorityDefault, "shared_scripts/check_hyperliquid.py", []string{"hold", "BTC", "1h", "--mode=paper"})
	got := launches()
	if len(got) != 4 || !strings.Contains(got[0], "--list-open-order-oids") || !strings.Contains(got[1], "--limit-status") || !strings.Contains(got[2], "--resolve-order") || strings.Contains(got[2], "--cancel-open") || !strings.Contains(got[3], "--mode=paper") {
		t.Fatalf("reads and checks must keep reaching the interpreter under paper_only: %v", got)
	}
}

func paperManualGuardCommandConfig(e *mrpEnv, name, dbDir string, livePeers int) string {
	e.t.Helper()
	return e.writeConfig(name, func(doc map[string]any) {
		doc["db_file"] = filepath.Join(dbDir, "state.db")
		doc["paper_sources"] = []any{map[string]any{"id": "alt", "db_file": filepath.Join(dbDir, "alt.db")}}
		noMode := mrpPaperManualStrategy("pm-nomode", "short")
		noMode["args"] = []any{"hold", "BTC", "1m"}
		alt := mrpPaperManualStrategy("pm-alt", "short")
		alt["paper_source"] = "alt"
		strategies := []any{mrpPaperManualStrategy("pm-args", "short"), noMode, alt}
		for i := 0; i < livePeers; i++ {
			strategies = append(strategies, mrpManualStrategy("lm-peer-"+strconv.Itoa(i), "short"))
		}
		doc["strategies"] = strategies
	})
}

func TestPaperManualVenueGuardCommands(t *testing.T) {
	e := newMRPEnv(t, 40)
	commands := func(id string) [][]string {
		return [][]string{
			{"manual-open", id, "--size", "0.01", "--atr", "0.5"},
			{"manual-open", id, "--size", "0.01", "--atr", "0.5", "--dry-run"},
			{"manual-open", id, "--size", "0.01", "--limit-price", "90", "--atr", "0.5"},
			{"manual-open", id, "--size", "0.01", "--limit-price", "90", "--atr", "0.5", "--dry-run"},
			{"manual-add", id, "--size", "0.01"},
			{"manual-add", id, "--size", "0.01", "--dry-run"},
			{"manual-close", id},
			{"manual-close", id, "--dry-run"},
			{"manual-close", id, "--qty", "0.005"},
			{"manual-update-sl", id, "--trigger", "90"},
			{"manual-update-sl", id, "--trigger", "90", "--dry-run"},
			{"manual-cancel-sl", id},
			{"manual-cancel-sl", id, "--dry-run"},
			{"manual-cancel", id},
		}
	}
	for _, peers := range []int{0, 1, 2} {
		dbDir := t.TempDir()
		cfg := paperManualGuardCommandConfig(e, "guard-"+strconv.Itoa(peers)+".json", dbDir, peers)
		for _, id := range []string{"pm-args", "pm-nomode", "pm-alt"} {
			wantPart := "the paper partition"
			if id == "pm-alt" {
				wantPart = "the paper:alt partition"
			}
			for _, args := range commands(id) {
				r := e.command(cfg, nil, args...)
				if r.code != 1 || !strings.Contains(r.stderr, wantPart) || !strings.Contains(r.stderr, "no order was sent") || strings.Contains(r.stdout, "[dry-run]") {
					t.Fatalf("peers=%d %v must refuse with exit 1 before any state:\n%s", peers, args, r)
				}
			}
		}
		entries, err := os.ReadDir(dbDir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Fatalf("peers=%d: a refused paper command touched trading state: %v", peers, entries)
		}
		t.Logf("peers=%d: market open, limit open, add, whole and sized close, stop update, stop cancel and manual-cancel refused for explicit-paper, no-mode and named-partition strategies with no state file and no order", peers)
	}
	if kinds := e.venueKinds(); len(kinds) != 0 {
		t.Fatalf("a refused paper command reached the venue substitute: %v", kinds)
	}

	locked := t.TempDir()
	cfg := paperManualGuardCommandConfig(e, "guard-locked.json", filepath.Join(locked, "state"), 1)
	if err := os.Chmod(locked, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o700) })
	for _, args := range [][]string{{"manual-close", "pm-args"}, {"manual-open", "pm-alt", "--size", "0.01", "--atr", "0.5"}, {"manual-cancel", "pm-nomode"}} {
		r := e.command(cfg, nil, args...)
		if r.code != 1 || !strings.Contains(r.stderr, "no order was sent") || strings.Contains(r.stderr, "state DB") {
			t.Fatalf("%v must refuse on the paper check while state is inaccessible:\n%s", args, r)
		}
	}
	os.Chmod(locked, 0o700)

	dbDir := t.TempDir()
	cfg = paperManualGuardCommandConfig(e, "guard-record.json", dbDir, 1)
	if r := e.command(cfg, nil, "manual-open", "pm-args", "--side", "short", "--size", "0.01", "--fill-price", "100", "--record-only"); r.code != 0 {
		t.Fatalf("a paper record-only open must queue:\n%s", r)
	}
	if r := e.command(cfg, nil, "manual-open", "pm-alt", "--side", "short", "--size", "0.01", "--fill-price", "100", "--record-only"); r.code != 0 {
		t.Fatalf("a named-partition record-only open must queue:\n%s", r)
	}
	store := openMRPStore(t, cfg)
	actions, err := store.LoadPendingManualActions()
	store.Close()
	if err != nil || len(actions) != 2 {
		t.Fatalf("record-only opens must queue one row each in the owning store: %v %+v", err, actions)
	}
	first := e.command(cfg, nil, "--once", "--status-port", strconv.Itoa(htfITFreePort(t)))
	if first.code != 0 {
		t.Fatalf("--once failed:\n%s", first)
	}
	second := e.command(cfg, nil, "--once", "--status-port", strconv.Itoa(htfITFreePort(t)))
	if second.code != 0 {
		t.Fatalf("second --once failed:\n%s", second)
	}
	for _, id := range []string{"pm-args", "pm-alt"} {
		want := "[WARN] paper manual position " + id + "/BTC"
		if n := strings.Count(second.stdout, want); n != 1 {
			t.Fatalf("a run with an open paper manual position must warn once for %s, got %d:\n%s", id, n, second.stdout)
		}
	}
	if strings.Contains(second.stdout, "paper manual position lm-peer") {
		t.Fatalf("a live manual strategy must not get the paper warning:\n%s", second.stdout)
	}
	if kinds := e.venueKinds(); kinds["execute"] != 0 || kinds["limit_open"] != 0 || kinds["cancel"] != 0 {
		t.Fatalf("paper record-only adoption and the paper cycles sent venue writes: %v", kinds)
	}
	for _, args := range [][]string{{"manual-close", "pm-args"}, {"manual-close", "pm-args", "--qty", "0.005"}, {"manual-update-sl", "pm-alt", "--trigger", "110"}} {
		if r := e.command(cfg, nil, args...); r.code != 1 || !strings.Contains(r.stderr, "no order was sent") {
			t.Fatalf("%v with an open paper position must refuse:\n%s", args, r)
		}
	}
	if kinds := e.venueKinds(); len(kinds) != 0 {
		t.Fatalf("the paper position commands reached the venue substitute: %v", kinds)
	}
}

func openMRPStore(t *testing.T, cfgPath string) *StateStore {
	t.Helper()
	cfg, err := LoadConfigForProbe(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	store, err := openToolStateStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

type paperManualGuardRecoveryVenue struct {
	t      *testing.T
	dir    string
	marker string
}

func newPaperManualGuardRecoveryVenue(t *testing.T) *paperManualGuardRecoveryVenue {
	t.Helper()
	v := &paperManualGuardRecoveryVenue{t: t, dir: t.TempDir()}
	v.marker = filepath.Join(v.dir, "launches.log")
	py := filepath.Join(v.dir, ".venv", "bin", "python3")
	if err := os.MkdirAll(filepath.Dir(py), 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\necho \"$*\" >> '" + v.marker + "'\ncase \"$*\" in\n*--limit-status*) if [ -f '" + v.file("cancelled") + "' ] && [ -f '" + v.file("after.json") + "' ]; then cat '" + v.file("after.json") + "'; else cat '" + v.file("status.json") + "'; fi ;;\n*--cancel-order*) touch '" + v.file("cancelled") + "'; echo '{\"platform\":\"hyperliquid\",\"oid\":9001,\"cancelled\":true}' ;;\n*) echo '{\"platform\":\"hyperliquid\",\"error\":\"unexpected launch\"}'; exit 1 ;;\nesac\n"
	if err := os.WriteFile(py, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(v.dir)
	return v
}

func (v *paperManualGuardRecoveryVenue) file(name string) string {
	return filepath.Join(v.dir, name)
}

func (v *paperManualGuardRecoveryVenue) set(status, afterCancel string) {
	v.t.Helper()
	for _, name := range []string{"launches.log", "cancelled", "after.json"} {
		if err := os.Remove(v.file(name)); err != nil && !os.IsNotExist(err) {
			v.t.Fatal(err)
		}
	}
	if err := os.WriteFile(v.file("status.json"), []byte(status), 0o600); err != nil {
		v.t.Fatal(err)
	}
	if afterCancel != "" {
		if err := os.WriteFile(v.file("after.json"), []byte(afterCancel), 0o600); err != nil {
			v.t.Fatal(err)
		}
	}
}

func (v *paperManualGuardRecoveryVenue) lines(kind string) []string {
	v.t.Helper()
	raw, err := os.ReadFile(v.marker)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		v.t.Fatal(err)
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if strings.Contains(line, kind) {
			out = append(out, line)
		}
	}
	return out
}

func paperManualGuardLimitStatus(coin, state string, orig float64, resting, proven bool, openOrdersErr string) string {
	raw := map[string]string{"live": "open", "cancelled": "canceled"}[state]
	restingJSON := strconv.FormatBool(resting)
	tail := ""
	if openOrdersErr != "" {
		restingJSON = "null"
		tail = `,"open_orders_error":"` + openOrdersErr + `"`
	}
	fills := `"fills_proven":true`
	if !proven {
		fills = `"fills_proven":false,"fills_error":"fills fetch failed","fills_reason":"transient"`
	}
	return `{"platform":"hyperliquid","orders":[{"oid":9001,"coin":"` + coin + `","resting":` + restingJSON + `,"order_status":"` + raw + `","order_state":"` + state + `","orig_size":` + strconv.FormatFloat(orig, 'f', -1, 64) + `,"executed_size":0,"sz_decimals":4,"filled_size":0,"avg_px":0,"fee":0,"count":0,` + fills + `}]` + tail + `}`
}

func TestPaperManualVenueGuardKillSwitchRecoveryRows(t *testing.T) {
	venue := newPaperManualGuardRecoveryVenue(t)
	paper := StrategyConfig{ID: "pm-eth", Type: "manual", Platform: "hyperliquid", Symbol: "ETH", Script: "shared_scripts/check_hyperliquid.py", Args: []string{"hold", "ETH", "30m", "--mode=paper"}}
	live := StrategyConfig{ID: "hl-ema-eth", Type: "perps", Platform: "hyperliquid", Symbol: "ETH", Script: "shared_scripts/check_hyperliquid.py", Args: []string{"ema_crossover", "ETH", "1h", "--mode=live"}}
	realDeps := func(db *StateDB) killSwitchLimitOrderDeps {
		return killSwitchLimitOrderDeps{
			Cancel: func(script, symbol string, oid int64) (*HyperliquidCancelOrderResult, string, error) {
				return RunHyperliquidCancelOrder(hlAccountRecoveryWriteAuth(symbol, hlWriteCancel).forOrder(oid), script, symbol, oid)
			},
			Status:              RunHyperliquidLimitStatus,
			Delete:              db.DeletePendingLimitOrderIfUnchanged,
			MarkCancelRequested: db.MarkPendingLimitOrderCancelRequested,
		}
	}
	cases := []struct {
		name, status, after string
		cancels             int
		want                string
	}{
		{"resting with unreadable fills", paperManualGuardLimitStatus("ETH", "live", 0.5, true, false, ""), paperManualGuardLimitStatus("ETH", "cancelled", 0.5, false, false, ""), 1, "fills unproven: fills fetch failed"},
		{"open orders unreadable", paperManualGuardLimitStatus("ETH", "live", 0.5, false, true, "429 rate limited"), "", 0, "open-orders state unknown (429 rate limited)"},
		{"off book with unreadable fills", paperManualGuardLimitStatus("ETH", "cancelled", 0.5, false, false, ""), "", 0, "fills unproven: fills fetch failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := newLimitTestStateDB(t)
			id := seedKillSwitchLimitRow(t, db, paper.ID)
			venue.set(tc.status, tc.after)
			plan := planKillSwitchClose(killSwitchLimitInputs(db, []StrategyConfig{paper, live}, realDeps(db)))
			if got := len(venue.lines("--cancel-order")); got != tc.cancels {
				t.Fatalf("cancel launches = %d, want %d: %+v", got, tc.cancels, plan.LimitOrderReport)
			}
			if tc.cancels == 1 && len(venue.lines("--oid=9001")) != 1 {
				t.Fatalf("the cancel must name the recovery row order: %+v", plan.LimitOrderReport)
			}
			if plan.OnChainConfirmedFlat || plan.LimitOrderReport.ConfirmedClear() || len(plan.LimitOrderReport.Unresolved) != 1 {
				t.Fatalf("the recovery row must stay unresolved: %+v", plan.LimitOrderReport)
			}
			if !strings.Contains(plan.LimitOrderReport.Unresolved[0], tc.want) {
				t.Fatalf("unresolved reason missing %q: %v", tc.want, plan.LimitOrderReport.Unresolved)
			}
			if !strings.Contains(strings.Join(plan.LimitOrderReport.LogLines, "\n"), "[CRITICAL] ks-limit") {
				t.Fatalf("the kept row must raise the CRITICAL line: %v", plan.LimitOrderReport.LogLines)
			}
			orders, err := db.LoadPendingLimitOrders()
			if err != nil || len(orders) != 1 || orders[0].ID != id {
				t.Fatalf("the recovery row must be kept: %v %+v", err, orders)
			}
		})
	}

	t.Run("budget exhausted after a resting read keeps the cancel intent", func(t *testing.T) {
		db := newLimitTestStateDB(t)
		seedKillSwitchLimitRow(t, db, "removed-strategy")
		venue.set(paperManualGuardLimitStatus("ETH", "live", 0.5, true, true, ""), "")
		clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		deps := realDeps(db)
		deps.Now = func() time.Time { return clock }
		deps.Status = func(script, symbol string, oids []int64, sinceMs int64) (*HyperliquidLimitStatusResult, string, error) {
			res, stderr, err := RunHyperliquidLimitStatus(script, symbol, oids, sinceMs)
			clock = clock.Add(time.Hour)
			return res, stderr, err
		}
		plan := planKillSwitchClose(killSwitchLimitInputs(db, []StrategyConfig{live}, deps))
		if got := venue.lines("--cancel-order"); len(got) != 0 {
			t.Fatalf("an exhausted budget must send no cancel in this pass: %v", got)
		}
		if len(plan.LimitOrderReport.Unresolved) != 1 || !strings.Contains(plan.LimitOrderReport.Unresolved[0], "verified resting but the pass exhausted") {
			t.Fatalf("the row must stay unresolved with the budget reason: %+v", plan.LimitOrderReport)
		}
		orders, err := db.LoadPendingLimitOrders()
		if err != nil || len(orders) != 1 || !orders[0].CancelRequested {
			t.Fatalf("the stored row must carry the cancel intent: %v %+v", err, orders)
		}
	})

	t.Run("adoptable row keeps one mark and one cancel", func(t *testing.T) {
		owner := StrategyConfig{ID: "hl-manual-eth-live", Type: "manual", Platform: "hyperliquid", Symbol: "ETH", Script: "shared_scripts/check_hyperliquid.py", Args: []string{"hold", "ETH", "30m", "--mode=live"}}
		db := newLimitTestStateDB(t)
		seedKillSwitchLimitRow(t, db, owner.ID)
		venue.set(paperManualGuardLimitStatus("ETH", "live", 0.5, true, true, ""), paperManualGuardLimitStatus("ETH", "cancelled", 0.5, false, true, ""))
		deps := realDeps(db)
		marks := 0
		deps.MarkCancelRequested = func(strategyID, symbol string) (int64, error) {
			marks++
			return db.MarkPendingLimitOrderCancelRequested(strategyID, symbol)
		}
		plan := planKillSwitchClose(killSwitchLimitInputs(db, []StrategyConfig{owner}, deps))
		if got := venue.lines("--cancel-order"); marks != 1 || len(got) != 1 || len(venue.lines("--limit-status")) != 1 {
			t.Fatalf("an adoptable row must get one mark, one cancel and one status read after it: marks=%d %v", marks, venue.lines(""))
		}
		if !plan.LimitOrderReport.ConfirmedClear() {
			t.Fatalf("the cancelled adoptable row must resolve: %+v", plan.LimitOrderReport)
		}
		if orders, err := db.LoadPendingLimitOrders(); err != nil || len(orders) != 0 {
			t.Fatalf("the resolved row must be deleted: %v %+v", err, orders)
		}
	})

	t.Run("only paper strategies remain", func(t *testing.T) {
		db := newLimitTestStateDB(t)
		row := seedKillSwitchLimitRow(t, db, paper.ID)
		venue.set(`{"platform":"hyperliquid","orders":[]}`, "")
		plan := planKillSwitchClose(killSwitchLimitInputs(db, []StrategyConfig{paper}, realDeps(db)))
		const want = "no live Hyperliquid strategy with a script remains in this config"
		if len(plan.LimitOrderReport.Unresolved) != 1 || !strings.Contains(plan.LimitOrderReport.Unresolved[0], want) || !strings.Contains(strings.Join(plan.LimitOrderReport.LogLines, "\n"), want) {
			t.Fatalf("the alert must name the missing live strategy: %+v", plan.LimitOrderReport)
		}
		orders, err := db.LoadPendingLimitOrders()
		if err != nil || len(orders) != 1 || orders[0].ID != row {
			t.Fatalf("the row must be kept: %v %+v", err, orders)
		}
		out := cancelOrphanedLimitOrder(&Config{Strategies: []StrategyConfig{paper}}, nil, orders[0])
		if out.Resolved || out.State != orphanLimitStateUnscripted || !strings.Contains(out.Reason, want) {
			t.Fatalf("the orphan lane must name the missing live strategy: %+v", out)
		}
		if len(venue.lines("")) != 0 {
			t.Fatal("a paper strategy's script must run no recovery read or cancel")
		}
	})
}

func TestPaperManualVenueGuardLimitRowRecovery(t *testing.T) {
	e := newMRPEnv(t, 40)
	const liveScript = "shared_scripts/check_hyperliquid.py"
	const paperScript = "shared_scripts/paper_owner_marker.py"
	type fixture struct {
		path  string
		cfg   *Config
		store *StateStore
	}
	var buildOwned func(name string, livePeer bool, owner string) *fixture
	build := func(name string, livePeer bool) *fixture {
		t.Helper()
		return buildOwned(name, livePeer, "pm-args")
	}
	buildOwned = func(name string, livePeer bool, owner string) *fixture {
		t.Helper()
		dbDir := t.TempDir()
		path := e.writeConfig(name, func(doc map[string]any) {
			doc["db_file"] = filepath.Join(dbDir, "state.db")
			strategies := []any{mrpPaperManualStrategy("pm-args", "short")}
			if livePeer {
				strategies = append(strategies, mrpManualStrategy("lm-peer", "short"))
			}
			if owner != "pm-args" {
				strategies = append(strategies, mrpManualStrategy(owner, "short"))
			}
			doc["strategies"] = strategies
		})
		cfg, err := LoadConfigForProbe(path)
		if err != nil {
			t.Fatal(err)
		}
		f := &fixture{path: path, cfg: cfg, store: openMRPStore(t, path)}
		t.Cleanup(func() { f.store.Close() })
		if _, err := f.store.InsertPendingLimitOrder(PendingLimitOrder{StrategyID: owner, Symbol: "BTC", Side: "long", OrderOID: 9001, LimitPrice: 90, OrderSize: 0.01, TIF: "Alo", CreatedAt: time.Now().UTC()}); err != nil {
			t.Fatalf("seed the paper manual limit row: %v", err)
		}
		kept := cfg.Strategies[:0]
		for _, sc := range cfg.Strategies {
			if sc.ID == "pm-args" {
				sc.Script = paperScript
			}
			if sc.ID == owner && owner != "pm-args" {
				continue
			}
			kept = append(kept, sc)
		}
		cfg.Strategies = kept
		return f
	}
	rows := func(f *fixture) []PendingLimitOrder {
		t.Helper()
		orders, err := f.store.LoadPendingLimitOrders()
		if err != nil {
			t.Fatal(err)
		}
		return orders
	}
	withPeer := build("limit-row-peer.json", true)
	paperOnly := build("limit-row-paper.json", false)
	startup := build("limit-row-startup.json", false)
	restored := build("limit-row-restored.json", false)
	latchReset := buildOwned("limit-row-latch-reset.json", true, "removed-strategy")
	fillsCases := []struct {
		name, status, after string
		cancels             int
		want                string
		f                   *fixture
	}{
		{"resting with unreadable fills", paperManualGuardLimitStatus("BTC", "live", 0.01, true, false, ""), paperManualGuardLimitStatus("BTC", "cancelled", 0.01, false, false, ""), 1, "fills unproven: fills fetch failed", build("limit-row-fills-resting.json", true)},
		{"off book with unreadable fills", paperManualGuardLimitStatus("BTC", "cancelled", 0.01, false, false, ""), "", 0, "fills unproven: fills fetch failed", build("limit-row-fills-offbook.json", true)},
		{"open orders unreadable", paperManualGuardLimitStatus("BTC", "live", 0.01, false, true, "429 rate limited"), "", 1, "cancel issued but unverified", build("limit-row-fills-openorders.json", true)},
	}
	venue := newPaperManualGuardRecoveryVenue(t)
	orphanLimitCancelAlerts.reset()
	t.Cleanup(orphanLimitCancelAlerts.reset)
	var mu sync.RWMutex

	t.Run("live peer cancels the paper owned row", func(t *testing.T) {
		if o := rows(withPeer)[0]; !o.ExpiresAt.IsZero() || o.CancelRequested {
			t.Fatalf("the seeded row must have no expiry and no cancel request: %+v", o)
		}
		venue.set(paperManualGuardLimitStatus("BTC", "live", 0.01, true, true, ""), paperManualGuardLimitStatus("BTC", "cancelled", 0.01, false, true, ""))
		mock := &mockNotifier{}
		reconcilePendingLimitOrders(NewAppState(), withPeer.cfg, withPeer.store, &mu, NewMultiNotifier(notifierBackend{notifier: mock, ownerID: "owner"}), nil)
		cancels := venue.lines("--cancel-order")
		if len(cancels) != 1 || !strings.Contains(cancels[0], "--oid=9001") || !strings.Contains(cancels[0], liveScript) {
			t.Fatalf("one reconcile pass must send one cancel for oid 9001 through the live owner's script: %v", venue.lines(""))
		}
		if got := venue.lines(paperScript); len(got) != 0 {
			t.Fatalf("the paper strategy's script must launch nothing: %v", got)
		}
		if got := rows(withPeer); len(got) != 0 {
			t.Fatalf("the row must be deleted after the status read shows the order off-book with no fill: %+v", got)
		}
	})

	for _, tc := range fillsCases {
		t.Run("live peer "+tc.name, func(t *testing.T) {
			orphanLimitCancelAlerts.reset()
			venue.set(tc.status, tc.after)
			mock := &mockNotifier{}
			reconcilePendingLimitOrders(NewAppState(), tc.f.cfg, tc.f.store, &mu, NewMultiNotifier(notifierBackend{notifier: mock, ownerID: "owner"}), nil)
			cancels := venue.lines("--cancel-order")
			if len(cancels) != tc.cancels {
				t.Fatalf("cancel launches = %d, want %d: %v", len(cancels), tc.cancels, venue.lines(""))
			}
			if tc.cancels == 1 && (!strings.Contains(cancels[0], "--oid=9001") || !strings.Contains(cancels[0], liveScript)) {
				t.Fatalf("the cancel must name oid 9001 and run through the live owner's script: %v", cancels)
			}
			if got := venue.lines(paperScript); len(got) != 0 {
				t.Fatalf("the paper strategy's script must launch nothing: %v", got)
			}
			if got := rows(tc.f); len(got) != 1 || got[0].OrderOID != 9001 {
				t.Fatalf("the row must stay: %+v", got)
			}
			if len(mock.dms) != 1 || !strings.Contains(mock.dms[0].content, "NOT confirmed cancelled: pm-args/BTC oid=9001") || !strings.Contains(mock.dms[0].content, tc.want) {
				t.Fatalf("the owner must get the state-unknown alert with %q: %+v", tc.want, mock.dms)
			}
		})
	}
	orphanLimitCancelAlerts.reset()

	t.Run("kill switch saves the cancel intent for a removed strategy row it does not cancel", func(t *testing.T) {
		orphanLimitCancelAlerts.reset()
		venue.set(paperManualGuardLimitStatus("BTC", "live", 0.01, false, true, "429 rate limited"), "")
		plan := planKillSwitchClose(KillSwitchCloseInputs{
			PortfolioReason:    "portfolio drawdown 25.0% exceeds limit 20.0%",
			CloseTimeout:       time.Second,
			HLLimitOrderLoader: latchReset.store.LoadPendingLimitOrders,
			HLLimitOrderRoster: killSwitchLimitOrderRoster(latchReset.cfg.Strategies),
			HLLimitOrderDeps: killSwitchLimitOrderDeps{
				Cancel: func(script, symbol string, oid int64) (*HyperliquidCancelOrderResult, string, error) {
					return RunHyperliquidCancelOrder(hlAccountRecoveryWriteAuth(symbol, hlWriteCancel).forOrder(oid), script, symbol, oid)
				},
				Status:              RunHyperliquidLimitStatus,
				Delete:              latchReset.store.DeletePendingLimitOrderIfUnchanged,
				MarkCancelRequested: latchReset.store.MarkPendingLimitOrderCancelRequested,
			},
		})
		if got := venue.lines("--cancel-order"); len(got) != 0 {
			t.Fatalf("an unknown resting state must send no cancel in the kill switch pass: %v", got)
		}
		if len(plan.LimitOrderReport.Unresolved) != 1 || !strings.Contains(plan.LimitOrderReport.Unresolved[0], "open-orders state unknown (429 rate limited)") {
			t.Fatalf("the row must stay unresolved: %+v", plan.LimitOrderReport)
		}
		stored := rows(latchReset)
		if len(stored) != 1 || !stored[0].CancelRequested {
			t.Fatalf("the kill switch must save the cancel intent on the row it did not cancel: %+v", stored)
		}
		venue.set(paperManualGuardLimitStatus("BTC", "live", 0.01, true, true, ""), paperManualGuardLimitStatus("BTC", "cancelled", 0.01, false, true, ""))
		reconcilePendingLimitOrders(NewAppState(), latchReset.cfg, latchReset.store, &mu, NewMultiNotifier(notifierBackend{notifier: &mockNotifier{}, ownerID: "owner"}), nil)
		if cancels := venue.lines("--cancel-order"); len(cancels) != 1 || !strings.Contains(cancels[0], "--oid=9001") {
			t.Fatalf("after the latch reset the reconciler must send the row to the orphan cancel lane: %v", venue.lines(""))
		}
		if got := rows(latchReset); len(got) != 0 {
			t.Fatalf("the orphan cancel lane must finalize the row: %+v", got)
		}
	})
	orphanLimitCancelAlerts.reset()

	t.Run("paper only config marks the row for the operator", func(t *testing.T) {
		venue.set(`{"platform":"hyperliquid","orders":[]}`, "")
		mock := &mockNotifier{}
		reconcilePendingLimitOrders(NewAppState(), paperOnly.cfg, paperOnly.store, &mu, NewMultiNotifier(notifierBackend{notifier: mock, ownerID: "owner"}), nil)
		if got := venue.lines(""); len(got) != 0 {
			t.Fatalf("a config with only paper strategies must launch nothing: %v", got)
		}
		got := rows(paperOnly)
		if len(got) != 1 || got[0].OperatorRequiredSince.IsZero() {
			t.Fatalf("the row must stay and carry the operator-required marker: %+v", got)
		}
		if len(mock.dms) != 1 || !strings.Contains(mock.dms[0].content, "pm-args/BTC oid=9001") || !strings.Contains(mock.dms[0].content, "no live Hyperliquid strategy with a script remains") || !strings.Contains(mock.dms[0].content, "manual-clear-limit-row 9001 --flattened") {
			t.Fatalf("the owner must get one alert that names the row and the clear command: %+v", mock.dms)
		}
		paperOnly.store.Close()
		if r := e.command(paperOnly.path, nil, "manual-clear-limit-row", "9001"); r.code != 1 || !strings.Contains(r.stderr, "pass --flattened") {
			t.Fatalf("the clear command must ask for --flattened:\n%s", r)
		}
		if r := e.command(paperOnly.path, nil, "manual-clear-limit-row", "9001", "--flattened"); r.code != 0 || !strings.Contains(r.stdout, "Cleared queue row") {
			t.Fatalf("the clear command must clear an operator-required row:\n%s", r)
		}
		check := openMRPStore(t, paperOnly.path)
		defer check.Close()
		if left, err := check.LoadPendingLimitOrders(); err != nil || len(left) != 0 {
			t.Fatalf("the cleared row must be gone: %v %+v", err, left)
		}
	})

	t.Run("a live strategy added after the unscripted marker takes the row back", func(t *testing.T) {
		orphanLimitCancelAlerts.reset()
		venue.set(`{"platform":"hyperliquid","orders":[]}`, "")
		quiet := func() *MultiNotifier {
			return NewMultiNotifier(notifierBackend{notifier: &mockNotifier{}, ownerID: "owner"})
		}
		reconcilePendingLimitOrders(NewAppState(), restored.cfg, restored.store, &mu, quiet(), nil)
		marked := rows(restored)
		if len(marked) != 1 || !limitRowUnscriptedMarker(marked[0]) {
			t.Fatalf("the paper only pass must set the unscripted marker: %+v", marked)
		}
		if refusal := clearOperatorRequiredLimitRowRefusal(restored.cfg, marked[0], true); refusal != "" {
			t.Fatalf("the operator must be able to clear an unscripted row while no live script exists: %s", refusal)
		}
		var peer StrategyConfig
		for _, sc := range withPeer.cfg.Strategies {
			if sc.ID == "lm-peer" {
				peer = sc
			}
		}
		withLive := *restored.cfg
		withLive.Strategies = append(append([]StrategyConfig(nil), restored.cfg.Strategies...), peer)
		if refusal := clearOperatorRequiredLimitRowRefusal(&withLive, marked[0], true); !strings.Contains(refusal, "reads and cancels the order itself") {
			t.Fatalf("with a live script the clear command must leave the row to the scheduler: %q", refusal)
		}
		venue.set(paperManualGuardLimitStatus("BTC", "live", 0.01, true, true, ""), paperManualGuardLimitStatus("BTC", "cancelled", 0.01, false, true, ""))
		reconcilePendingLimitOrders(NewAppState(), &withLive, restored.store, &mu, quiet(), nil)
		if cancels := venue.lines("--cancel-order"); len(cancels) != 1 || !strings.Contains(cancels[0], "--oid=9001") {
			t.Fatalf("the restored live script must cancel the order in the first pass with no poll deferral: %v", venue.lines(""))
		}
		if got := rows(restored); len(got) != 0 {
			t.Fatalf("the row must be finalized from the scheduler's own proof: %+v", got)
		}
	})
	orphanLimitCancelAlerts.reset()

	t.Run("startup warns once for the row with no open position", func(t *testing.T) {
		const want = "paper manual limit order pm-args/BTC oid=9001"
		warnings := paperManualLimitRowStartupWarnings(startup.cfg, startup.store.LoadPendingLimitOrders)
		if len(warnings) != 1 || !strings.Contains(warnings[0], want) {
			t.Fatalf("one warning must name the strategy, coin and order id: %v", warnings)
		}
		mock := &mockNotifier{}
		sendPaperManualStartupWarnings(NewMultiNotifier(notifierBackend{notifier: mock, ownerID: "owner"}), warnings)
		if len(mock.dms) != 1 || mock.dms[0].userID != "owner" || !strings.Contains(mock.dms[0].content, want) {
			t.Fatalf("the owner must get one direct message that names the row: %+v", mock.dms)
		}
		startup.store.Close()
		run := e.command(startup.path, nil, "--once", "--status-port", strconv.Itoa(htfITFreePort(t)))
		if run.code != 0 {
			t.Fatalf("--once failed:\n%s", run)
		}
		if n := strings.Count(run.stdout, "[WARN] "+want); n != 1 {
			t.Fatalf("the run must print one limit order warning, got %d:\n%s", n, run.stdout)
		}
		if strings.Contains(run.stdout, "[WARN] paper manual position") {
			t.Fatalf("no paper position is open, so no position warning is due:\n%s", run.stdout)
		}
		if !strings.Contains(run.stdout, "limit order cannot be read or cancelled from this config pm-args/BTC oid=9001") {
			t.Fatalf("the run must raise the CRITICAL line for the row:\n%s", run.stdout)
		}
		if kinds := e.venueKinds(); kinds["cancel"] != 0 || kinds["limit_status"] != 0 || kinds["refused_cancel"] != 0 {
			t.Fatalf("a config with only paper strategies reached the venue substitute: %v", kinds)
		}
		check := openMRPStore(t, startup.path)
		defer check.Close()
		left, err := check.LoadPendingLimitOrders()
		if err != nil || len(left) != 1 || left[0].OperatorRequiredSince.IsZero() {
			t.Fatalf("the run must mark the row for the operator: %v %+v", err, left)
		}
	})
}
