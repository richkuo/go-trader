//go:build okxstopacceptance

package main

import (
	"encoding/json"
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
	if len(live.TradeHistory) != 1 || (pos != nil && pos.Quantity != 0) {
		t.Fatalf("trades=%d position=%+v evidence=%s", len(live.TradeHistory), pos, live.OKXProtection.EvidenceJSON)
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
		t.Fatalf("stored fills = %d evidence=%s", fills, live.OKXProtection.EvidenceJSON)
	}
}

func okxAcceptanceBook(t *testing.T) *StrategyState {
	t.Helper()
	now := time.Now().UnixMilli()
	return &StrategyState{
		ID: "okx-btc", Platform: "okx", Type: "perps", Cash: 10000, InitialCapital: 10000,
		Positions: map[string]*Position{"BTC": {Symbol: "BTC", Side: "long", Quantity: 10, AvgCost: 50000, InitialQuantity: 10}},
		OKXProtection: &okxProtection{
			RecordID: "r-live", AccountKey: "acct", InstID: "BTC-USDT-SWAP", Symbol: "BTC",
			AlgoClientID: "cid-live", EntryClientID: "entry-1", Status: okxProtectReserved,
			Version: 1, Owner: okxStopOwnerPct, PolicyJSON: `{"Owner":"stop_loss_pct","Pct":"2"}`,
			CreatedAtMs: now, UpdatedAtMs: now, AppliedFills: map[string]bool{},
		},
	}
}

type okxAcceptFixture struct {
	mu        sync.Mutex
	places    int
	stored    bool
	effective bool
}

func (f *okxAcceptFixture) placeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.places
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
	if body.Method == "privatePostTradeOrderAlgo" {
		f.places++
		f.stored = true
		http.Error(w, "lost", 500)
		return
	}
	payload := map[string]interface{}{"code": "0", "data": []interface{}{}}
	switch body.Method {
	case "privateGetAccountPositions":
		payload["data"] = []interface{}{map[string]string{"instId": "BTC-USDT-SWAP", "pos": "10", "posSide": "net", "mgnMode": "cross", "liqPx": "40000"}}
	case "publicGetPublicInstruments":
		payload["data"] = []interface{}{map[string]string{"instId": "BTC-USDT-SWAP", "tickSz": "0.1", "lotSz": "1", "minSz": "1", "ctVal": "0.01", "state": "live", "instFamily": "BTC-USDT"}}
	case "publicGetMarketTicker":
		payload["data"] = []interface{}{map[string]string{"instId": "BTC-USDT-SWAP", "markPx": "50000"}}
	case "publicGetPublicPositionTiers":
		payload["data"] = []interface{}{map[string]string{"minSz": "0", "maxSz": "100000", "mmr": "0.005"}}
	case "privateGetAccountBalance":
		payload["data"] = []interface{}{map[string]interface{}{"details": []interface{}{map[string]string{"ccy": "USDT", "eq": "1000"}}}}
	case "privateGetAccountTradeFee":
		payload["data"] = []interface{}{map[string]string{"taker": "-0.0005"}}
	case "privateGetTradeOrdersAlgoPending":
		if f.stored && !f.effective {
			payload["data"] = []interface{}{map[string]string{"instId": "BTC-USDT-SWAP", "algoId": "algo-live", "algoClOrdId": "cid-live", "state": "live", "slTriggerPx": "49000.0", "sz": "10"}}
		}
	case "privateGetTradeOrdersAlgoHistory":
		if f.stored && f.effective {
			payload["data"] = []interface{}{map[string]string{"instId": "BTC-USDT-SWAP", "algoId": "algo-live", "algoClOrdId": "cid-live", "state": "effective", "slTriggerPx": "49000.0", "sz": "10"}}
		}
	case "privateGetTradeFills":
		if f.effective {
			payload["data"] = []interface{}{map[string]string{"instId": "BTC-USDT-SWAP", "tradeId": "fill-9", "side": "sell", "fillSz": "10", "fillPx": "49000", "fee": "-0.02", "ordId": "ord-9"}}
		}
	default:
		http.Error(w, "unexpected "+body.Method, 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(payload)
}
