//go:build pyintegration

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

const limitStatusVenueDriver = `import os, runpy, sys
script = sys.argv[1]
root = os.path.dirname(os.path.dirname(os.path.abspath(script)))
sys.path.insert(0, root)
import platforms.hyperliquid.adapter as adapter
adapter.MAINNET_URL = os.environ["LIMIT_STATUS_TEST_URL"]
adapter.TESTNET_URL = os.environ["LIMIT_STATUS_TEST_URL"]
adapter._load_meta_cache = lambda *a, **k: None
adapter._save_meta_cache = lambda *a, **k: None
sys.argv = [script] + sys.argv[2:]
runpy.run_path(script, run_name="__main__")
`

type limitStatusVenueOrder struct {
	status string
	origSz string
	sz     string
}

type limitStatusVenue struct {
	mu         sync.Mutex
	orders     map[int64]limitStatusVenueOrder
	open       map[int64]bool
	fills      []map[string]any
	requests   map[string]int
	unexpected []string
	nextTid    int64
	baseMs     int64
}

func (v *limitStatusVenue) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Type      string  `json:"type"`
		OID       int64   `json:"oid"`
		StartTime float64 `json:"startTime"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	v.mu.Lock()
	defer v.mu.Unlock()
	v.requests[req.Type]++
	var body any
	switch req.Type {
	case "meta":
		body = map[string]any{"universe": []any{map[string]any{"name": "ETH", "szDecimals": 4, "maxLeverage": 50}}}
	case "spotMeta":
		body = map[string]any{"universe": []any{}, "tokens": []any{}}
	case "openOrders", "frontendOpenOrders":
		list := []any{}
		oids := make([]int64, 0, len(v.open))
		for oid := range v.open {
			oids = append(oids, oid)
		}
		sort.Slice(oids, func(i, j int) bool { return oids[i] < oids[j] })
		for _, oid := range oids {
			list = append(list, map[string]any{"coin": "ETH", "oid": oid, "side": "B", "limitPx": "2000", "sz": v.orders[oid].sz})
		}
		body = list
	case "orderStatus":
		o, ok := v.orders[req.OID]
		if !ok {
			body = map[string]any{"status": "unknownOid"}
			break
		}
		body = map[string]any{"status": "order", "order": map[string]any{"status": o.status, "statusTimestamp": v.baseMs,
			"order": map[string]any{"coin": "ETH", "oid": req.OID, "side": "B", "limitPx": "2000", "origSz": o.origSz, "sz": o.sz}}}
	case "userFillsByTime":
		page := []any{}
		for _, f := range v.fills {
			if float64(f["time"].(int64)) < req.StartTime {
				continue
			}
			page = append(page, f)
			if len(page) == 2000 {
				break
			}
		}
		body = page
	default:
		v.unexpected = append(v.unexpected, req.Type)
		http.Error(w, "unexpected type", http.StatusBadRequest)
		return
	}
	_ = json.NewEncoder(w).Encode(body)
}

func (v *limitStatusVenue) addFill(oid int64, sz, px, fee string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.nextTid++
	v.fills = append(v.fills, map[string]any{"coin": "ETH", "oid": oid, "sz": sz, "px": px, "fee": fee,
		"time": v.baseMs + v.nextTid, "tid": v.nextTid, "hash": fmt.Sprintf("0x%x", v.nextTid), "side": "B"})
}

func (v *limitStatusVenue) setOrder(oid int64, status, origSz, sz string, resting bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.orders[oid] = limitStatusVenueOrder{status: status, origSz: origSz, sz: sz}
	if resting {
		v.open[oid] = true
	} else {
		delete(v.open, oid)
	}
}

func (v *limitStatusVenue) count(kind string) int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.requests[kind]
}

func newLimitStatusVenue(t *testing.T) *limitStatusVenue {
	t.Helper()
	root := entryOperatorRepoRoot(t)
	v := &limitStatusVenue{orders: map[int64]limitStatusVenueOrder{}, open: map[int64]bool{}, requests: map[string]int{},
		baseMs: time.Now().UnixMilli()}
	srv := httptest.NewServer(v)
	t.Cleanup(srv.Close)
	t.Setenv("LIMIT_STATUS_TEST_URL", srv.URL)
	t.Setenv("HYPERLIQUID_SECRET_KEY", "")
	t.Setenv("HYPERLIQUID_TESTNET", "")
	runHyperliquidLimitStatusFn = func(script, symbol string, oids []int64, sinceMs int64, sealed ...limitStatusWatermark) (*HyperliquidLimitStatusResult, string, error) {
		args, err := buildHyperliquidLimitStatusArgs(symbol, oids, sinceMs, sealed)
		if err != nil {
			return nil, "", err
		}
		cmd := exec.Command(filepath.Join(root, ".venv", "bin", "python3"),
			append([]string{"-c", limitStatusVenueDriver, filepath.Join(root, script)}, args...)...)
		cmd.Dir = root
		var stderr strings.Builder
		cmd.Stderr = &stderr
		stdout, runErr := cmd.Output()
		return parseHyperliquidLimitStatusOutput(stdout, stderr.String(), runErr)
	}
	t.Cleanup(func() {
		v.mu.Lock()
		defer v.mu.Unlock()
		if len(v.unexpected) > 0 {
			t.Errorf("the script sent request types the venue substitute does not answer: %v", v.unexpected)
		}
	})
	return v
}

func TestManualLimitStatusRealScriptSkipsTheFillScanForAnUnchangedRestingOrder(t *testing.T) {
	e := newLimitSettleEnv(t, false, false)
	v := newLimitStatusVenue(t)
	for i := 0; i < 4000; i++ {
		v.addFill(77, "0.01", "2000", "0.001")
	}
	row := e.insertRow(settleOwner, 9001, 1.0)
	v.setOrder(9001, "open", "1.0", "1.0", true)

	e.reconcile()
	if n := v.count("orderStatus"); n != 1 {
		t.Fatalf("orderStatus requests = %d, want 1 for the pass", n)
	}
	if n := v.count("userFillsByTime"); n != 0 {
		t.Fatalf("userFillsByTime requests = %d, want none for a resting order with no new fill", n)
	}
	held, ok := e.row(row.ID)
	if !ok || held.FilledSize != 0 || !held.OperatorRequiredSince.IsZero() {
		t.Fatalf("row = %+v present=%v, want the zero row kept", held, ok)
	}
	st, reason := readLimitOrderEvidence(settleManualSC(settleOwner).Script, held, "status")
	if reason != "" || !st.FillsComplete || st.FilledSize != 0 || st.Resting == nil || !*st.Resting {
		t.Fatalf("evidence = %+v reason %q, want complete evidence for the resting zero row", st, reason)
	}
	if n := v.count("userFillsByTime"); n != 0 {
		t.Fatalf("userFillsByTime requests = %d after a second read, want none", n)
	}

	v.addFill(9001, "0.2", "2000", "0.1")
	v.setOrder(9001, "open", "1.0", "0.8", true)
	e.account(0.2)
	e.reconcile()
	scans := v.count("userFillsByTime")
	if scans == 0 {
		t.Fatal("a moved executed size must scan the fill history")
	}
	d := e.reopen()
	if pos := d.state.Strategies[settleOwner].Positions["ETH"]; pos == nil || pos.Quantity != 0.2 || d.trades[settleOwner] != 1 {
		t.Fatalf("book = %+v trades = %d, want the 0.2 fill booked once", pos, d.trades[settleOwner])
	}
	sealed, _ := e.row(row.ID)
	if sealed.FilledSize != 0.2 || !sealed.fillSealed() || sealed.FillFee != 0.1 {
		t.Fatalf("row = %+v, want sealed at 0.2 with its fee", sealed)
	}

	e.reconcile()
	if n := v.count("userFillsByTime"); n != scans {
		t.Fatalf("userFillsByTime requests %d -> %d, want no scan while the executed size equals the saved fill", scans, n)
	}
	if d = e.reopen(); d.trades[settleOwner] != 1 {
		t.Fatalf("the saved fill was booked again: trades = %d", d.trades[settleOwner])
	}

	v.setOrder(9001, "canceled", "1.0", "0.8", false)
	e.reconcile()
	if n := v.count("userFillsByTime"); n <= scans {
		t.Fatalf("userFillsByTime requests %d -> %d, want a full scan before a terminal row is deleted", scans, n)
	}
	if _, ok := e.row(row.ID); ok {
		t.Fatal("a terminal order whose fills equal the saved fill must finalize")
	}
	if d = e.reopen(); d.trades[settleOwner] != 1 {
		t.Fatalf("finalize booked a trade: trades = %d", d.trades[settleOwner])
	}
}

func TestManualLimitStatusRealScriptBooksALaterFillOnATerminalOrder(t *testing.T) {
	e := newLimitSettleEnv(t, false, false)
	v := newLimitStatusVenue(t)
	row := e.insertRow(settleOwner, 9001, 1.0)
	v.addFill(9001, "0.3", "2000", "0.15")
	v.setOrder(9001, "open", "1.0", "0.7", true)
	e.account(0.3)
	e.reconcile()
	if sealed, _ := e.row(row.ID); sealed.FilledSize != 0.3 || !sealed.fillSealed() {
		t.Fatalf("setup: row = %+v, want sealed at 0.3", sealed)
	}
	scans := v.count("userFillsByTime")

	v.addFill(9001, "0.2", "2000", "0.1")
	v.setOrder(9001, "canceled", "1.0", "0.5", false)
	e.account(0.5)
	e.reconcile()
	if n := v.count("userFillsByTime"); n <= scans {
		t.Fatalf("userFillsByTime requests %d -> %d, want a scan when the executed size passed the saved fill", scans, n)
	}
	d := e.reopen()
	pos := d.state.Strategies[settleOwner].Positions["ETH"]
	if pos == nil || pos.Quantity < 0.5-1e-9 || pos.Quantity > 0.5+1e-9 || d.trades[settleOwner] != 2 {
		t.Fatalf("book = %+v trades = %d, want the 0.2 delta booked once on top of 0.3", pos, d.trades[settleOwner])
	}
}
