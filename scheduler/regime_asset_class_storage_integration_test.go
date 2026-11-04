package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

const legacyRegimeTablesDDL = `
CREATE TABLE regime_window_history (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    platform TEXT NOT NULL,
    symbol TEXT NOT NULL,
    timeframe TEXT NOT NULL,
    spec_json TEXT NOT NULL,
    window TEXT NOT NULL,
    label TEXT NOT NULL,
    bar_time TEXT NOT NULL DEFAULT '',
    ts TEXT NOT NULL
);
CREATE INDEX idx_regime_hist_key ON regime_window_history(platform, symbol, timeframe, spec_json, window, id);
CREATE INDEX idx_regime_hist_ts ON regime_window_history(ts);
CREATE TABLE regime_window_transitions (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    platform TEXT NOT NULL,
    symbol TEXT NOT NULL,
    timeframe TEXT NOT NULL,
    spec_json TEXT NOT NULL,
    window TEXT NOT NULL,
    old_label TEXT NOT NULL,
    new_label TEXT NOT NULL,
    bar_time TEXT NOT NULL DEFAULT '',
    ts TEXT NOT NULL,
    alerted_at TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_regime_trans_key ON regime_window_transitions(platform, symbol, timeframe, spec_json, window, id);
CREATE INDEX idx_regime_trans_ts ON regime_window_transitions(ts);
CREATE TABLE regime_reversal_alerts (
    platform TEXT NOT NULL,
    symbol TEXT NOT NULL,
    timeframe TEXT NOT NULL,
    spec_json TEXT NOT NULL,
    signature TEXT NOT NULL,
    alerted_at TEXT NOT NULL,
    PRIMARY KEY (platform, symbol, timeframe, spec_json)
);
`

const regimeClassTestSpec = `{"default":{"classifier":"adx","period":14,"adx_threshold":20}}`

func seedLegacyRegimeFile(t *testing.T, path string, extraDDL string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	stmts := []string{legacyRegimeTablesDDL, extraDDL}
	for i := 0; i < 3; i++ {
		ts := fmt.Sprintf("2026-11-0%dT00:00:00Z", i+1)
		stmts = append(stmts,
			fmt.Sprintf(`INSERT INTO regime_window_history (platform, symbol, timeframe, spec_json, window, label, bar_time, ts) VALUES ('robinhood', 'LTC', '4h', '%s', 'default', 'trending_up', 'bar-%d', '%s')`, regimeClassTestSpec, i, ts),
			fmt.Sprintf(`INSERT INTO regime_window_history (platform, symbol, timeframe, spec_json, window, label, bar_time, ts) VALUES ('hyperliquid', 'BTC', '4h', '%s', 'default', 'ranging', 'bar-%d', '%s')`, regimeClassTestSpec, i, ts))
	}
	stmts = append(stmts,
		fmt.Sprintf(`INSERT INTO regime_window_transitions (platform, symbol, timeframe, spec_json, window, old_label, new_label, bar_time, ts, alerted_at) VALUES ('robinhood', 'LTC', '4h', '%s', 'default', 'ranging', 'trending_up', 'bar-0', '2026-11-01T00:00:00Z', '')`, regimeClassTestSpec),
		fmt.Sprintf(`INSERT INTO regime_window_transitions (platform, symbol, timeframe, spec_json, window, old_label, new_label, bar_time, ts, alerted_at) VALUES ('hyperliquid', 'BTC', '4h', '%s', 'default', 'trending_up', 'ranging', 'bar-0', '2026-11-01T00:00:00Z', '2026-11-01T00:00:00Z')`, regimeClassTestSpec),
		fmt.Sprintf(`INSERT INTO regime_reversal_alerts (platform, symbol, timeframe, spec_json, signature, alerted_at) VALUES ('robinhood', 'LTC', '4h', '%s', 'legacy-sig', '2026-11-01T00:00:00Z')`, regimeClassTestSpec),
		fmt.Sprintf(`INSERT INTO regime_reversal_alerts (platform, symbol, timeframe, spec_json, signature, alerted_at) VALUES ('hyperliquid', 'BTC', '4h', '%s', 'hl-sig', '2026-11-01T00:00:00Z')`, regimeClassTestSpec),
	)
	for _, stmt := range stmts {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed legacy file: %v\n%s", err, stmt)
		}
	}
}

func regimeTableDump(t *testing.T, db *sql.DB) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for table, cols := range map[string]string{
		"regime_window_history":     "id, platform, symbol, timeframe, spec_json, window, label, bar_time, ts",
		"regime_window_transitions": "id, platform, symbol, timeframe, spec_json, window, old_label, new_label, bar_time, ts, alerted_at",
		"regime_reversal_alerts":    "platform, symbol, timeframe, spec_json, signature, alerted_at",
	} {
		rows, err := db.Query("SELECT " + cols + " FROM " + table)
		if err != nil {
			t.Fatal(err)
		}
		n := len(strings.Split(cols, ","))
		for rows.Next() {
			vals := make([]any, n)
			ptrs := make([]any, n)
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			out[table] = append(out[table], fmt.Sprint(vals...))
		}
		rows.Close()
		sort.Strings(out[table])
	}
	return out
}

func regimeIndexNames(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'index' AND name LIKE 'idx_regime_%' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		out = append(out, name)
	}
	return out
}

func regimeClassCounts(t *testing.T, db *sql.DB, table string) map[string]int {
	t.Helper()
	rows, err := db.Query("SELECT platform || '/' || asset_class, COUNT(*) FROM " + table + " GROUP BY 1")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var k string
		var n int
		if err := rows.Scan(&k, &n); err != nil {
			t.Fatal(err)
		}
		out[k] = n
	}
	return out
}

func regimeClassBundle(class, label, barTime string) *RegimeBundle {
	return &RegimeBundle{
		Key:     regimeBundleKey{Platform: "robinhood", AssetClass: class, Symbol: "LTC", Timeframe: "4h", SpecJSON: regimeClassTestSpec},
		Payload: RegimePayload{MultiMode: true, Windows: map[string]RegimeSnapshot{"default": {Regime: label}}},
		BarTime: barTime,
	}
}

func TestRegimeAssetClassStorageMigratesAndSeparatesClasses(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	seedLegacyRegimeFile(t, path, "")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	before := regimeTableDump(t, raw)
	raw.Close()

	ro, err := openStateDBReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	legacyRows, err := ro.RecentRegimeWindowTransitions(10)
	if err != nil {
		t.Fatalf("read-only read of an unmigrated file: %v", err)
	}
	if len(legacyRows) != 2 || legacyRows[0].AssetClass != "" {
		t.Fatalf("read-only legacy rows = %+v", legacyRows)
	}
	if has, err := ro.tableHasColumn("regime_window_history", "asset_class"); err != nil || has {
		t.Fatalf("a read-only open migrated the file: has=%v err=%v", has, err)
	}
	ro.Close()

	for open := 1; open <= 2; open++ {
		sdb, err := OpenStateDB(path)
		if err != nil {
			t.Fatalf("open %d: %v", open, err)
		}
		if got := regimeTableDump(t, sdb.db); fmt.Sprint(got) != fmt.Sprint(before) {
			t.Fatalf("open %d changed legacy rows:\nbefore %v\nafter  %v", open, before, got)
		}
		if got := strings.Join(regimeIndexNames(t, sdb.db), ","); got != "idx_regime_hist_class_key,idx_regime_hist_ts,idx_regime_trans_class_key,idx_regime_trans_ts" {
			t.Fatalf("open %d indexes = %s", open, got)
		}
		for _, table := range []string{"regime_window_history", "regime_window_transitions", "regime_reversal_alerts"} {
			if got := regimeClassCounts(t, sdb.db, table); got["robinhood/"] == 0 || got["hyperliquid/"] == 0 || len(got) != 2 {
				t.Fatalf("open %d %s classes = %v, want legacy rows kept unclassified", open, table, got)
			}
		}
		sdb.Close()
	}

	sdb, err := OpenStateDB(path)
	if err != nil {
		t.Fatal(err)
	}
	crypto := regimeClassBundle(robinhoodAssetClassCrypto, "", "").Key
	stock := regimeClassBundle(robinhoodAssetClassStock, "", "").Key
	legacy := crypto
	legacy.AssetClass = ""
	if trail, _ := sdb.RegimeWindowTrailing(legacy, "default", 10); len(trail) != 3 {
		t.Fatalf("legacy history must stay readable as unclassified rows, got %d", len(trail))
	}
	for _, key := range []regimeBundleKey{crypto, stock} {
		if trail, _ := sdb.RegimeWindowTrailing(key, "default", 10); len(trail) != 0 {
			t.Fatalf("legacy Robinhood history seeded %s: %+v", key, trail)
		}
		if pending, _ := sdb.UnalertedRegimeWindowTransitions(key, "default"); len(pending) != 0 {
			t.Fatalf("legacy Robinhood transition seeded %s: %+v", key, pending)
		}
		if sig, _ := sdb.RegimeReversalSignature(key); sig != "" {
			t.Fatalf("legacy Robinhood signature seeded %s: %q", key, sig)
		}
	}

	var sent []string
	prevSend := regimeAlertSendFn
	regimeAlertSendFn = func(_ *MultiNotifier, msg string) { sent = append(sent, msg) }
	t.Cleanup(func() {
		regimeAlertSendFn = prevSend
		regimeReversalPendingState = map[regimeBundleKey]*regimeReversalPending{}
	})
	rc := &RegimeConfig{Enabled: true, Timeframe: "4h", Period: 14, ADXThreshold: 20, Transitions: &RegimeTransitionAlertsConfig{Enabled: true, DebounceCycles: 1}}
	cycle := func(db *StateDB, i int, cryptoLabel, stockLabel string) {
		store := &RegimeStore{}
		gen := store.resetForCycle(time.Now())
		store.set(regimeClassBundle(robinhoodAssetClassCrypto, cryptoLabel, fmt.Sprintf("c-%d", i)), gen)
		store.set(regimeClassBundle(robinhoodAssetClassStock, stockLabel, fmt.Sprintf("s-%d", i)), gen)
		processRegimeTransitionAlerts(db, store, rc, nil, time.Date(2026, 11, 4, i, 0, 0, 0, time.UTC))
	}
	cycle(sdb, 1, "trending_up", "ranging")
	cycle(sdb, 2, "trending_down", "ranging")
	sdb.Close()
	sdb, err = OpenStateDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer sdb.Close()
	cycle(sdb, 3, "trending_down", "trending_up")

	if len(sent) != 2 || !strings.Contains(sent[0], "robinhood:crypto/LTC/4h window default: trending_up → trending_down") ||
		!strings.Contains(sent[1], "robinhood:stock/LTC/4h window default: ranging → trending_up") {
		t.Fatalf("alerts = %q", sent)
	}
	if got := regimeClassCounts(t, sdb.db, "regime_window_history"); got["robinhood/crypto"] != 3 || got["robinhood/stock"] != 3 || got["robinhood/"] != 3 || got["hyperliquid/"] != 3 {
		t.Fatalf("history by class = %v", got)
	}
	if got := regimeClassCounts(t, sdb.db, "regime_window_transitions"); got["robinhood/crypto"] != 1 || got["robinhood/stock"] != 1 || got["robinhood/"] != 1 {
		t.Fatalf("transitions by class = %v", got)
	}
	var legacyAlerted string
	if err := sdb.db.QueryRow(`SELECT alerted_at FROM regime_window_transitions WHERE platform = 'robinhood' AND asset_class = ''`).Scan(&legacyAlerted); err != nil || legacyAlerted != "" {
		t.Fatalf("a class alert acknowledged the legacy row: %q %v", legacyAlerted, err)
	}

	if err := sdb.SetRegimeReversalSignature(crypto, "crypto-sig", "2026-11-04T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if err := sdb.SetRegimeReversalSignature(stock, "stock-sig", "2026-11-04T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if err := sdb.SetRegimeReversalSignature(crypto, "crypto-sig-2", "2026-11-04T01:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if err := sdb.ClearRegimeReversalSignature(stock); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[regimeBundleKey]string{crypto: "crypto-sig-2", stock: "", legacy: "legacy-sig"} {
		if got, err := sdb.RegimeReversalSignature(key); err != nil || got != want {
			t.Fatalf("signature %s = %q (%v), want %q", key, got, err, want)
		}
	}

	server := &StatusServer{stateDB: singleFileStore(sdb), statusToken: "regime-class-token"}
	req := httptest.NewRequest(http.MethodGet, "/api/regime/transitions?limit=10", nil)
	req.Header.Set("Authorization", "Bearer regime-class-token")
	rec := httptest.NewRecorder()
	server.handleAPIRegimeTransitions(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("transitions API status %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Transitions []map[string]any `json:"transitions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	classes := map[string]bool{}
	for _, row := range body.Transitions {
		class, ok := row["asset_class"].(string)
		if !ok {
			t.Fatalf("transition record lacks asset_class: %v", row)
		}
		classes[fmt.Sprint(row["platform"])+"/"+class] = true
	}
	if !classes["robinhood/crypto"] || !classes["robinhood/stock"] || !classes["robinhood/"] || !classes["hyperliquid/"] {
		t.Fatalf("transition API classes = %v", classes)
	}
	note := recentRegimeTransitionsNote(singleFileStore(sdb), rc, time.Date(2026, 11, 4, 4, 0, 0, 0, time.UTC))
	if !strings.Contains(note, "robinhood:crypto/LTC/4h default: trending_up → trending_down") || !strings.Contains(note, "robinhood:stock/LTC/4h default: ranging → trending_up") {
		t.Fatalf("status note = %q", note)
	}
}

func TestRegimeAssetClassMigrationRollsBackAsOneTransaction(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	seedLegacyRegimeFile(t, path, "CREATE VIEW regime_reversal_alerts_v2 AS SELECT 1 AS blocker;")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	before := regimeTableDump(t, raw)
	raw.Close()
	if sdb, err := OpenStateDB(path); err == nil {
		sdb.Close()
		t.Fatal("the migration must fail on the blocking view")
	} else if !strings.Contains(err.Error(), "migrate regime_reversal_alerts to the asset-class key") {
		t.Fatalf("open error = %v", err)
	}
	raw, err = sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if got := regimeTableDump(t, raw); fmt.Sprint(got) != fmt.Sprint(before) {
		t.Fatalf("failed migration changed rows:\nbefore %v\nafter  %v", before, got)
	}
	sdb := &StateDB{db: raw}
	for _, table := range []string{"regime_window_history", "regime_window_transitions", "regime_reversal_alerts"} {
		if has, err := sdb.tableHasColumn(table, "asset_class"); err != nil || has {
			t.Fatalf("%s kept a partial migration: has=%v err=%v", table, has, err)
		}
	}
	if got := strings.Join(regimeIndexNames(t, raw), ","); got != "idx_regime_hist_key,idx_regime_hist_ts,idx_regime_trans_key,idx_regime_trans_ts" {
		t.Fatalf("failed migration changed indexes: %s", got)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}
