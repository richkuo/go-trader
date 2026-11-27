//go:build okxstopacceptance

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestOKXStopOfflineAcceptance(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := wd
	if filepath.Base(wd) == "scheduler" {
		root = filepath.Dir(wd)
	}
	py := filepath.Join(root, ".venv", "bin", "python3")
	cmd := exec.Command(py, filepath.Join(root, "scripts", "okx_stop_offline_acceptance.py"))
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("offline acceptance: %v\n%s", err, out)
	}
	t.Log(string(out))

	db := openTestDB(t)
	store := singleFileStore(db)
	now := time.Now().UnixMilli()
	state := &StrategyState{ID: "okx-btc"}
	p := &okxProtection{
		RecordID: "r1", AccountKey: "acct", InstID: "BTC-USDT-SWAP", Symbol: "BTC",
		AlgoClientID: "cid-1", EntryClientID: "entry-1", Status: okxProtectReserved,
		Version: 1, Owner: okxStopOwnerPct, PolicyJSON: `{"Owner":"stop_loss_pct","Pct":"2"}`,
		CreatedAtMs: now, UpdatedAtMs: now,
	}
	if err := db.reserveOKXProtection(state, p); err != nil {
		t.Fatal(err)
	}
	p.Status = okxProtectArming
	p.RequestJSON = `{"trigger":"49000.0"}`
	tx, err := db.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := upsertOKXProtectionRow(tx, "okx-btc", p); err == nil {
		t.Fatal("a repeated version was saved")
	}
	_ = tx.Rollback()
	p.Version = 2
	tx, err = db.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := upsertOKXProtectionRow(tx, "okx-btc", p); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	p.durableVersion = 2
	p.AccountKey = "other"
	p.Version = 3
	tx, err = db.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := upsertOKXProtectionRow(tx, "okx-btc", p); err == nil {
		t.Fatal("an identity change was saved")
	}
	_ = tx.Rollback()
	p.AccountKey = "acct"
	p.Version = 2
	p.PendingFills = []okxProtectionFill{{FillID: "fill-1", Side: "sell", Qty: 4, Px: 49000, Fee: 0.02}}
	tx, err = db.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	state.OKXProtection = p
	if err := db.writeOKXProtectionTx(tx, state); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	pruneOKXProtectionAfterCommit(state)
	tx, err = db.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	p.PendingFills = []okxProtectionFill{{FillID: "fill-1", Side: "sell", Qty: 4, Px: 49000, Fee: 0.02}}
	if err := db.writeOKXProtectionTx(tx, state); err != nil {
		t.Fatal(err)
	}
	_ = tx.Rollback()
	p.AppliedFills = nil
	tx, err = db.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := db.writeOKXProtectionTx(tx, state); err == nil {
		t.Fatal("an unapplied duplicate fill was accepted")
	}
	_ = tx.Rollback()
	p.AppliedFills = map[string]bool{"fill-1": true}
	var n int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM okx_protection_fills`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("fill rows = %d, want 1", n)
	}
	if _, err := db.db.Exec(`DELETE FROM okx_protection_fills WHERE fill_id = 'fill-1'`); err == nil {
		t.Fatal("a fill row was deleted")
	}
	if _, err := db.db.Exec(`UPDATE okx_protection_fills SET qty = 1 WHERE fill_id = 'fill-1'`); err == nil {
		t.Fatal("a fill row was updated")
	}
	fi := &storageFileInspection{Role: storageRolePrimary}
	rejections, err := inspectOKXProtectionRows(db, fi, map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rejections) != 1 || fi.OKXStopRows != 1 {
		t.Fatalf("rejections=%v rows=%d", rejections, fi.OKXStopRows)
	}
	rejections, err = inspectOKXProtectionRows(db, fi, map[string]bool{"okx-btc": true})
	if err != nil {
		t.Fatal(err)
	}
	if len(rejections) != 0 {
		t.Fatalf("configured owner rejected: %v", rejections)
	}
	if _, err := db.db.Exec(`UPDATE okx_protection SET status = 'retired' WHERE record_id = 'r1'`); err != nil {
		t.Fatal(err)
	}

	pct := 2.0
	sc := StrategyConfig{ID: "okx-btc", Platform: "okx", Type: "perps", Script: "shared_scripts/check_okx.py", StopLossPct: &pct, Args: []string{"sma", "BTC", "1h", "--mode=live"}}
	guard := &StrategyState{EntryGuard: &entryGuardState{Partition: partitionFor(sc)}}
	openAdm := admitOKXStopEntry(sc, guard, "BTC", 1, 0, 0, "", time.UnixMilli(10))
	if !openAdm.Applies || openAdm.Key.IntervalMs != 1 || openAdm.Held || openAdm.BarOpenMs != 10 {
		t.Fatalf("open admission = %+v", openAdm)
	}
	sc.ClosedBarDecisions = true
	if adm := admitOKXStopEntry(sc, guard, "BTC", 1, 0, 0, "", time.UnixMilli(10)); adm.Applies {
		t.Fatalf("closed-bar admission was replaced: %+v", adm)
	}
	sc.ClosedBarDecisions = false
	sc.Direction = DirectionBoth
	rev := admitOKXStopEntry(sc, guard, "BTC", -1, 0, 1, "long", time.UnixMilli(11))
	if !rev.Applies || !rev.CloseOnly {
		t.Fatalf("reversal admission = %+v", rev)
	}
	held := admitOKXStopEntry(sc, &StrategyState{}, "BTC", 1, 0, 0, "", time.UnixMilli(12))
	if !held.Held {
		t.Fatal("a missing entry record was not held")
	}

	note := &okxProtection{RecordID: "r1", Status: okxProtectArming, Version: 1, Symbol: "BTC", AlgoClientID: "cid-1", StrategyID: "okx-btc"}
	okxProtectionIncident(nil, &StrategyState{OKXProtection: note}, nil, note, "lost reply", &MultiNotifier{}, nil)
	if !note.Alerted || note.IncidentID != "r1" || !note.holdsNewExposure() {
		t.Fatalf("incident = %+v", note)
	}
	okxProtectionIncident(nil, &StrategyState{OKXProtection: note}, nil, note, "lost reply", &MultiNotifier{}, nil)
	if !note.Alerted || note.IncidentID != "r1" {
		t.Fatal("the incident id changed")
	}
	armed := &okxProtection{Status: okxProtectArmed}
	if armed.holdsNewExposure() || !(&okxProtection{Status: okxProtectReserved}).holdsNewExposure() {
		t.Fatal("hold rule mismatched the stop status")
	}

	t.Chdir(root)
	fix := &okxAcceptFixture{}
	srv := httptest.NewServer(fix)
	t.Cleanup(srv.Close)
	t.Setenv("OKX_OFFLINE_FIXTURE", srv.URL)
	t.Setenv("OKX_API_KEY", "offline-fixture")
	t.Setenv("OKX_API_SECRET", "secret")
	t.Setenv("OKX_PASSPHRASE", "pass")
	live := okxAcceptanceBook(t)
	var mu sync.RWMutex
	logger := silentStrategyLogger("okx-btc")
	armOKXStopAfterEntry(store, sc, live, &mu, &entryIntent{ClientOrderID: "entry-1"}, &OKXFill{AvgPx: 50000, TotalSz: 10}, &MultiNotifier{}, logger)
	if live.Positions["BTC"].Quantity != 10 {
		t.Fatalf("entry quantity = %v", live.Positions["BTC"].Quantity)
	}
	if live.OKXProtection.PlaceAttempts != 1 || live.OKXProtection.Status != okxProtectArming {
		t.Fatalf("after lost place: %+v", live.OKXProtection)
	}
	if fix.placeCount() != 1 {
		t.Fatalf("places = %d", fix.placeCount())
	}
	reconcileOKXProtection(store, sc, live, &mu, &MultiNotifier{}, logger)
	if live.OKXProtection.Status != okxProtectArmed || fix.placeCount() != 1 {
		t.Fatalf("status=%s places=%d", live.OKXProtection.Status, fix.placeCount())
	}
	fix.setEffective()
	reconcileOKXProtection(store, sc, live, &mu, &MultiNotifier{}, logger)
	pos := live.Positions["BTC"]
	if len(live.TradeHistory) != 1 || pos != nil || live.OKXProtection != nil {
		t.Fatalf("trades=%d position=%+v protection=%+v", len(live.TradeHistory), pos, live.OKXProtection)
	}
	live.EntryGuard = guard.EntryGuard
	nextOpen := admitOKXStopEntry(sc, live, "BTC", 1, 0, 0, "", time.UnixMilli(12))
	if !nextOpen.admitted() {
		t.Fatalf("next entry stayed held after stop retirement: %+v", nextOpen)
	}
	reconcileOKXProtection(store, sc, live, &mu, &MultiNotifier{}, logger)
	if len(live.TradeHistory) != 1 {
		t.Fatalf("fill booked again: %d trades", len(live.TradeHistory))
	}
	var fills int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM okx_protection_fills WHERE fill_id = 'fill-9'`).Scan(&fills); err != nil {
		t.Fatal(err)
	}
	if fills != 1 {
		t.Fatalf("stored fills = %d", fills)
	}
}

func TestOKXStopReviewRecovery(t *testing.T) {
	if filepath.Base(mustGetwd(t)) == "scheduler" {
		t.Chdir("..")
	}
	pct := 2.0
	sc := StrategyConfig{ID: "okx-btc", Platform: "okx", Type: "perps", Script: okxStopScript, StopLossPct: &pct, Args: []string{"sma", "BTC", "1h", "--mode=live"}}
	for _, mode := range []string{"confirmed-reduce", "unresolved-entry", "unreadable-reduce", "partial-stop", "partial-stop-entry-only", "restart-after-book", "failed-close", "circuit-partial"} {
		t.Run(mode, func(t *testing.T) {
			fix := &okxAcceptFixture{hideReduceFill: mode == "unreadable-reduce", onlyEntry: mode == "partial-stop-entry-only"}
			server := httptest.NewServer(fix)
			t.Cleanup(server.Close)
			t.Setenv("OKX_OFFLINE_FIXTURE", server.URL)
			t.Setenv("OKX_API_KEY", "offline-fixture")
			t.Setenv("OKX_API_SECRET", "secret")
			t.Setenv("OKX_PASSPHRASE", "pass")
			db := openTestDB(t)
			store := singleFileStore(db)
			live := okxAcceptanceBook(t)
			p := live.OKXProtection
			var mu sync.RWMutex
			logger := silentStrategyLogger(sc.ID)
			if mode == "partial-stop" || mode == "partial-stop-entry-only" || mode == "restart-after-book" || mode == "failed-close" || mode == "circuit-partial" {
				armOKXStopAfterEntry(store, sc, live, &mu, &entryIntent{ClientOrderID: "entry-1"}, &OKXFill{AvgPx: 50000, TotalSz: 10}, nil, logger)
				if mode == "partial-stop" {
					if _, err := ExecutePerpsSignalWithLeverageDeferredOpen(live, -1, "BTC", 49500,
						PerpsSizingFor(sc, 49500, 0), 2, "ord-other", 0.01, DirectionBoth, 0.2, logger); err != nil {
						t.Fatal(err)
					}
				}
				fix.mu.Lock()
				fix.partial = mode == "partial-stop" || mode == "partial-stop-entry-only"
				fix.effective = mode == "restart-after-book"
				fix.failNextPosition = fix.effective
				fix.mu.Unlock()
			} else if mode == "unresolved-entry" {
				live.EntryGuard = &entryGuardState{Intents: map[string]*entryIntent{"pending": {IntentID: "pending", Status: entryIntentSubmitting}}}
			} else {
				fix.mu.Lock()
				fix.failMark = true
				fix.mu.Unlock()
				armOKXStopAfterEntry(store, sc, live, &mu, &entryIntent{ClientOrderID: "entry-1"}, &OKXFill{AvgPx: 50000, TotalSz: 10}, nil, logger)
				if p.RequestJSON != "" {
					t.Fatal("the arm-reference failure should leave no stop request")
				}
			}
			reconcileOKXProtection(store, sc, live, &mu, nil, logger)
			switch mode {
			case "confirmed-reduce":
				if live.OKXProtection != nil || live.Positions["BTC"] != nil || len(live.TradeHistory) != 1 || live.TradeHistory[0].Price != 48500 || !live.TradeHistory[0].IsClose {
					t.Fatalf("reduce did not settle: trades=%+v position=%+v protection=%+v", live.TradeHistory, live.Positions["BTC"], live.OKXProtection)
				}
			case "unresolved-entry":
				if fix.reduceSent() || live.OKXProtection != p || len(live.TradeHistory) != 0 {
					t.Fatal("the unresolved entry must defer the emergency reduce")
				}
			case "unreadable-reduce":
				reconcileOKXProtection(store, sc, live, &mu, nil, logger)
				if !fix.reduceSent() || live.OKXProtection != p || !p.holdsNewExposure() || p.IncidentID == "" || len(live.TradeHistory) != 0 {
					t.Fatal("the unknown reduce must hold the book with an incident")
				}
			case "failed-close":
				oldExec := okxExecuteGuardedFn
				t.Cleanup(func() { okxExecuteGuardedFn = oldExec })
				sent := false
				okxExecuteGuardedFn = func(_, _, side string, size float64, _ string, opts okxExecuteOptions) (*OKXExecuteResult, string, error) {
					sent = opts.ReduceOnly && side == "sell" && size == 10
					return nil, "", fmt.Errorf("fixture exchange refused the close")
				}
				app := &AppState{Strategies: map[string]*StrategyState{sc.ID: live}}
				runOKXStep(okxStepContext{
					Cfg: &Config{}, State: app, StratState: live, Store: store, StratDB: db, Mu: &mu, Logger: logger,
					Prices: map[string]float64{}, Risk: &scopeCycleRisk{}, Cash: 10000,
					Pos: okxStepPosition{Qty: 10, Side: "long", AvgCost: 50000},
				}, sc, func(StrategyConfig, PositionCtx, *StrategyLogger) (*OKXResult, string, float64, bool) {
					return &OKXResult{Symbol: "BTC", Signal: -1, StrategyDecisionFields: StrategyDecisionFields{CloseFraction: 1}}, "SELL", 50000, true
				})
				if !sent || live.OKXProtection.Status != okxProtectArmed || fix.placeCount() != 1 || len(live.TradeHistory) != 0 {
					t.Fatal("a failed strategy close must leave the original stop in place")
				}
			case "circuit-partial":
				app := &AppState{Strategies: map[string]*StrategyState{sc.ID: live}}
				live.RiskState.setPendingCircuitClose(PlatformPendingCloseOKX, &PendingCircuitClose{
					Symbols: []PendingCircuitCloseSymbol{{Symbol: "BTC", Size: 4}},
				})
				runPendingOKXCircuitCloses(context.Background(), store, app, []StrategyConfig{sc}, true,
					[]OKXPosition{{Coin: "BTC", Size: 10}}, true, nil,
					func(symbol string, contracts *float64) (*OKXCloseResult, error) {
						if symbol != "BTC" || contracts == nil || *contracts != 4 {
							t.Fatal("circuit close must request only its partial size")
						}
						fix.mu.Lock()
						fix.circuitRemainder = "6"
						fix.mu.Unlock()
						return &OKXCloseResult{}, nil
					}, 20*time.Second, &mu, nil)
				if live.OKXProtection.Status != okxProtectArmed || live.OKXProtection.SizeContracts != "6" || fix.placeCount() != 1 {
					t.Fatal("a partial circuit close must keep the original stop with the remaining size")
				}
			case "restart-after-book":
				if live.OKXProtection != p || live.Positions["BTC"] != nil || len(live.TradeHistory) != 1 {
					t.Fatal("a failed flat read must retain one booked fill and the unresolved protection")
				}
				restarted, err := db.LoadState()
				if err != nil {
					t.Fatal(err)
				}
				if err := store.LoadOKXProtections(restarted, []StrategyConfig{sc}); err != nil {
					t.Fatal(err)
				}
				loaded := restarted.Strategies[sc.ID]
				reconcileOKXProtection(store, sc, loaded, &mu, nil, logger)
				if loaded.OKXProtection != nil || len(loaded.TradeHistory) != 1 || loaded.Positions["BTC"] != nil {
					t.Fatal("restart must retire the row without booking the fill again")
				}
			case "partial-stop", "partial-stop-entry-only":
				wantQty, wantSize, wantTrades := 6.0, "6", 1
				if mode == "partial-stop" {
					wantQty, wantSize, wantTrades = 4.0, "4", 2
					if live.TradeHistory[0].ExchangeOrderID != "ord-other" {
						t.Fatal("the ordinary close must already be in the book")
					}
				}
				if live.OKXProtection != p || p.Status != okxProtectTriggered || live.Positions["BTC"].Quantity != wantQty ||
					len(live.TradeHistory) != wantTrades || live.TradeHistory[wantTrades-1].ExchangeOrderID != "ord-9" {
					t.Fatalf("unrelated fills changed the book: trades=%+v position=%+v protection=%+v", live.TradeHistory, live.Positions["BTC"], live.OKXProtection)
				}
				fix.mu.Lock()
				fix.partialTerminal = true
				fix.mu.Unlock()
				reconcileOKXProtection(store, sc, live, &mu, nil, logger)
				reconcileOKXProtection(store, sc, live, &mu, nil, logger)
				next := live.OKXProtection
				if next == nil || next == p || next.Status != okxProtectArmed || next.SizeContracts != wantSize ||
					len(live.TradeHistory) != wantTrades || fix.placeCount() != 2 {
					t.Fatalf("remaining exposure did not get one replacement stop: %+v", next)
				}
			}
		})
	}
}

func mustGetwd(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func okxAcceptanceBook(t *testing.T) *StrategyState {
	t.Helper()
	now := time.Now().UnixMilli()
	return &StrategyState{
		ID: "okx-btc", Platform: "okx", Type: "perps", Cash: 10000, InitialCapital: 10000,
		Positions: map[string]*Position{"BTC": {Symbol: "BTC", Side: "long", Quantity: 10, AvgCost: 50000, InitialQuantity: 10}},
		OKXProtection: &okxProtection{
			RecordID: "r-live", AccountKey: okxEntryAccountKey("swap"), InstID: "BTC-USDT-SWAP", Symbol: "BTC",
			AlgoClientID: "cid-live", EntryClientID: "entry-1", Status: okxProtectReserved,
			Version: 1, Owner: okxStopOwnerPct, PolicyJSON: `{"Owner":"stop_loss_pct","Pct":"2"}`,
			CreatedAtMs: now, UpdatedAtMs: now, AppliedFills: map[string]bool{},
		},
	}
}

type okxAcceptFixture struct {
	mu               sync.Mutex
	places           int
	stored           bool
	effective        bool
	partial          bool
	partialTerminal  bool
	onlyEntry        bool
	circuitRemainder string
	failMark         bool
	failNextPosition bool
	hideReduceFill   bool
	reduceClientID   string
	resting          map[string]string
}

func (f *okxAcceptFixture) placeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.places
}

func (f *okxAcceptFixture) reduceSent() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reduceClientID != ""
}

func (f *okxAcceptFixture) setEffective() {
	f.mu.Lock()
	f.effective = true
	f.mu.Unlock()
}

func (f *okxAcceptFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var params map[string]interface{}
	_ = json.Unmarshal(body.Params, &params)
	arg := func(key string) string {
		value, _ := params[key].(string)
		return value
	}
	switch body.Method {
	case "privatePostTradeOrderAlgo":
		f.places++
		f.stored = true
		f.resting = map[string]string{"instId": "BTC-USDT-SWAP", "algoId": "algo-live", "algoClOrdId": arg("algoClOrdId"), "state": "live", "slTriggerPx": arg("slTriggerPx"), "sz": arg("sz")}
		http.Error(w, "lost", 500)
		return
	case "privatePostTradeOrder":
		if f.reduceClientID != "" || params["reduceOnly"] != true || arg("side") != "sell" || arg("sz") != "10" || arg("instId") != "BTC-USDT-SWAP" || arg("clOrdId") == "" {
			http.Error(w, "invalid or duplicate reduce", 400)
			return
		}
		f.reduceClientID = arg("clOrdId")
		http.Error(w, "lost", 500)
		return
	}
	payload := map[string]interface{}{"code": "0", "data": []interface{}{}}
	switch body.Method {
	case "privateGetAccountPositions":
		if f.failNextPosition {
			f.failNextPosition = false
			http.Error(w, "position read failed", 500)
			return
		}
		if !f.effective && f.reduceClientID == "" {
			qty := "10"
			if f.partial {
				qty = "4"
				if f.onlyEntry {
					qty = "6"
				}
			} else if f.circuitRemainder != "" {
				qty = f.circuitRemainder
			}
			payload["data"] = []interface{}{map[string]string{"instId": "BTC-USDT-SWAP", "pos": qty, "posSide": "net", "mgnMode": "cross", "liqPx": "40000"}}
		}
	case "publicGetPublicInstruments":
		payload["data"] = []interface{}{map[string]string{"instId": "BTC-USDT-SWAP", "tickSz": "0.1", "lotSz": "1", "minSz": "1", "ctVal": "0.01", "state": "live", "instFamily": "BTC-USDT"}}
	case "publicGetPublicMarkPrice":
		if f.failMark {
			http.Error(w, "mark read failed", 500)
			return
		}
		payload["data"] = []interface{}{map[string]string{"instId": "BTC-USDT-SWAP", "markPx": "50000"}}
	case "publicGetPublicPositionTiers":
		payload["data"] = []interface{}{map[string]string{"minSz": "0", "maxSz": "100000", "mmr": "0.005"}}
	case "privateGetAccountBalance":
		payload["data"] = []interface{}{map[string]interface{}{"details": []interface{}{map[string]string{"ccy": "USDT", "eq": "1000", "cashBal": "1000"}}}}
	case "privateGetAccountTradeFee":
		payload["data"] = []interface{}{map[string]string{"taker": "-0.0005"}}
	case "privateGetTradeOrderAlgo":
		if !f.stored {
			payload["code"] = "51603"
			break
		}
		row := f.resting
		if f.effective || f.partial && arg("algoClOrdId") == "cid-live" {
			row = map[string]string{"instId": "BTC-USDT-SWAP", "algoId": "algo-live", "algoClOrdId": "cid-live", "state": "effective", "slTriggerPx": "49000.0", "sz": "10", "ordId": "ord-9"}
			if f.partial {
				row["state"] = "partially_effective"
			}
		}
		payload["data"] = []interface{}{row}
	case "privatePostTradeAmendAlgos":
		f.resting["sz"] = arg("newSz")
		f.resting["slTriggerPx"] = arg("newSlTriggerPx")
		payload["data"] = []interface{}{map[string]string{"sCode": "0", "algoId": "algo-live"}}
	case "privateGetTradeOrder":
		if arg("clOrdId") != "" {
			if f.reduceClientID != "" && !f.hideReduceFill {
				payload["data"] = []interface{}{map[string]string{"instId": "BTC-USDT-SWAP", "clOrdId": f.reduceClientID, "ordId": "reduce-1", "reduceOnly": "true", "state": "filled", "side": "sell", "avgPx": "48500", "accFillSz": "10", "fee": "-0.02", "feeCcy": "USDT"}}
			}
		} else if f.effective || f.partial {
			status := "filled"
			if f.partial && !f.partialTerminal {
				status = "partially_filled"
			}
			payload["data"] = []interface{}{map[string]string{"instId": "BTC-USDT-SWAP", "ordId": "ord-9", "reduceOnly": "true", "state": status}}
		}
	case "privateGetTradeFills":
		if f.effective || f.partial {
			qty := "10"
			if f.partial {
				qty = "4"
			}
			rows := []interface{}{map[string]string{"instId": "BTC-USDT-SWAP", "tradeId": "fill-9", "side": "sell", "fillSz": qty, "fillPx": "49000", "fee": "-0.02", "ordId": "ord-9"}}
			if f.partial {
				for _, ids := range [][3]string{{"ordinary", "ord-other", "sell"}, {"entry", "ord-entry", "buy"}, {"wrong-side", "ord-9", "buy"}} {
					if f.onlyEntry && ids[0] != "entry" {
						continue
					}
					rows = append(rows, map[string]string{"instId": "BTC-USDT-SWAP", "tradeId": ids[0], "side": ids[2], "fillSz": "2", "fillPx": "50000", "fee": "-0.01", "ordId": ids[1]})
				}
			}
			payload["data"] = rows
		}
	default:
		http.Error(w, "unexpected "+body.Method, 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(payload)
}
