//go:build pyintegration

package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
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

func paperManualGuardRecoveryVenue(t *testing.T) (func(status string), func(kind string) int) {
	t.Helper()
	dir := t.TempDir()
	marker := filepath.Join(dir, "launches.log")
	statusFile := filepath.Join(dir, "status.json")
	py := filepath.Join(dir, ".venv", "bin", "python3")
	if err := os.MkdirAll(filepath.Dir(py), 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\necho \"$*\" >> '" + marker + "'\ncase \"$*\" in\n*--limit-status*) cat '" + statusFile + "' ;;\n*--cancel-order*) echo '{\"platform\":\"hyperliquid\",\"oid\":9001,\"cancelled\":true}' ;;\n*) echo '{\"platform\":\"hyperliquid\",\"error\":\"unexpected launch\"}'; exit 1 ;;\nesac\n"
	if err := os.WriteFile(py, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	setStatus := func(status string) {
		if err := os.WriteFile(statusFile, []byte(status), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(marker); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	count := func(kind string) int {
		raw, err := os.ReadFile(marker)
		if os.IsNotExist(err) {
			return 0
		}
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			if strings.Contains(line, kind) {
				n++
			}
		}
		return n
	}
	return setStatus, count
}

func TestPaperManualVenueGuardKillSwitchRecoveryRows(t *testing.T) {
	setStatus, launches := paperManualGuardRecoveryVenue(t)
	paper := StrategyConfig{ID: "pm-eth", Type: "manual", Platform: "hyperliquid", Symbol: "ETH", Script: "shared_scripts/check_hyperliquid.py", Args: []string{"hold", "ETH", "30m", "--mode=paper"}}
	live := StrategyConfig{ID: "hl-ema-eth", Type: "perps", Platform: "hyperliquid", Symbol: "ETH", Script: "shared_scripts/check_hyperliquid.py", Args: []string{"ema_crossover", "ETH", "1h", "--mode=live"}}
	realDeps := func(db *StateDB) killSwitchLimitOrderDeps {
		return killSwitchLimitOrderDeps{
			Cancel: func(script, symbol string, oid int64) (*HyperliquidCancelOrderResult, string, error) {
				return RunHyperliquidCancelOrder(hlAccountRecoveryWriteAuth(symbol, hlWriteCancel).forOrder(oid), script, symbol, oid)
			},
			Status:              RunHyperliquidLimitStatus,
			Delete:              db.DeletePendingLimitOrder,
			Flush:               func() error { return nil },
			MarkCancelRequested: db.MarkPendingLimitOrderCancelRequested,
		}
	}
	cases := []struct {
		name, status string
		cancels      int
		want         string
	}{
		{"resting with unreadable fills", `{"platform":"hyperliquid","orders":[{"oid":9001,"resting":true,"filled_size":0,"avg_px":0,"fee":0,"count":0,"fills_error":"fills fetch failed"}]}`, 1, "fills unreadable (fills fetch failed)"},
		{"open orders unreadable", `{"platform":"hyperliquid","orders":[{"oid":9001,"resting":null,"filled_size":0,"avg_px":0,"fee":0,"count":0}],"open_orders_error":"429 rate limited"}`, 0, "open-orders state unknown (429 rate limited)"},
		{"off book with unreadable fills", `{"platform":"hyperliquid","orders":[{"oid":9001,"resting":false,"filled_size":0,"avg_px":0,"fee":0,"count":0,"fills_error":"fills fetch failed"}]}`, 0, "fills unreadable (fills fetch failed)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := newLimitTestStateDB(t)
			id := seedKillSwitchLimitRow(t, db, paper.ID)
			setStatus(tc.status)
			plan := planKillSwitchClose(killSwitchLimitInputs(db, []StrategyConfig{paper, live}, realDeps(db)))
			if got := launches("--cancel-order"); got != tc.cancels {
				t.Fatalf("cancel launches = %d, want %d: %+v", got, tc.cancels, plan.LimitOrderReport)
			}
			if tc.cancels == 1 && launches("--oid=9001") != 1 {
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

	t.Run("only paper strategies remain", func(t *testing.T) {
		db := newLimitTestStateDB(t)
		row := seedKillSwitchLimitRow(t, db, paper.ID)
		setStatus(`{"platform":"hyperliquid","orders":[]}`)
		plan := planKillSwitchClose(killSwitchLimitInputs(db, []StrategyConfig{paper}, realDeps(db)))
		const want = "no live Hyperliquid strategy with a script remains in this config"
		if len(plan.LimitOrderReport.Unresolved) != 1 || !strings.Contains(plan.LimitOrderReport.Unresolved[0], want) || !strings.Contains(strings.Join(plan.LimitOrderReport.LogLines, "\n"), want) {
			t.Fatalf("the alert must name the missing live strategy: %+v", plan.LimitOrderReport)
		}
		orders, err := db.LoadPendingLimitOrders()
		if err != nil || len(orders) != 1 || orders[0].ID != row {
			t.Fatalf("the row must be kept: %v %+v", err, orders)
		}
		out := cancelOrphanedLimitOrder(NewAppState(), &Config{Strategies: []StrategyConfig{paper}}, nil, nil, orders[0])
		if out.Resolved || out.State != orphanLimitStateUnknown || !strings.Contains(out.Reason, want) {
			t.Fatalf("the orphan lane must name the missing live strategy: %+v", out)
		}
		if launches("") != 0 {
			t.Fatal("a paper strategy's script must run no recovery read or cancel")
		}
	})
}
