package main

import (
	"database/sql"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	regimeTransitionDefaultDebounceCycles = 2
	regimeTransitionDefaultRetentionDays  = 30
	regimeTransitionStatusNoteLimit       = 5
	regimeTransitionStatusNoteWindow      = 24 * time.Hour
	regimeTransitionAPIDefaultLimit       = 50
	regimeTransitionAPIMaxLimit           = 500
)

var regimeTransitionPruneInterval = time.Hour

type RegimeTransitionAlertsConfig struct {
	Enabled             bool `json:"enabled"`
	DebounceCycles      int  `json:"debounce_cycles,omitempty"`
	RetentionDays       int  `json:"retention_days,omitempty"`
	ReversalMinOpposing int  `json:"reversal_min_opposing,omitempty"`
}

func (c *RegimeTransitionAlertsConfig) enabled() bool {
	return c != nil && c.Enabled
}

func (c *RegimeTransitionAlertsConfig) debounceCycles() int {
	if c == nil || c.DebounceCycles <= 0 {
		return regimeTransitionDefaultDebounceCycles
	}
	return c.DebounceCycles
}

func (c *RegimeTransitionAlertsConfig) retentionDays() int {
	if c == nil || c.RetentionDays <= 0 {
		return regimeTransitionDefaultRetentionDays
	}
	return c.RetentionDays
}

func (c *RegimeTransitionAlertsConfig) reversalMinOpposing() int {
	if c == nil || c.ReversalMinOpposing < 0 {
		return 0
	}
	return c.ReversalMinOpposing
}

func validateRegimeTransitionsConfig(cfg *Config) []string {
	if cfg.Regime == nil || cfg.Regime.Transitions == nil {
		return nil
	}
	var errs []string
	t := cfg.Regime.Transitions
	if t.DebounceCycles < 0 {
		errs = append(errs, fmt.Sprintf("regime.transitions.debounce_cycles must be >= 0, got %d", t.DebounceCycles))
	}
	if t.RetentionDays < 0 {
		errs = append(errs, fmt.Sprintf("regime.transitions.retention_days must be >= 0, got %d", t.RetentionDays))
	}
	if t.ReversalMinOpposing < 0 {
		errs = append(errs, fmt.Sprintf("regime.transitions.reversal_min_opposing must be >= 0, got %d", t.ReversalMinOpposing))
	}
	return errs
}

type RegimeWindowTransitionRow struct {
	ID        int64  `json:"id"`
	Platform  string `json:"platform"`
	Symbol    string `json:"symbol"`
	Timeframe string `json:"timeframe"`
	SpecJSON  string `json:"-"`
	Window    string `json:"window"`
	OldLabel  string `json:"old_label"`
	NewLabel  string `json:"new_label"`
	BarTime   string `json:"bar_time,omitempty"`
	TS        string `json:"ts"`
	AlertedAt string `json:"alerted_at,omitempty"`
}

type RegimeWindowHistoryEntry struct {
	Label   string
	BarTime string
}

func (sdb *StateDB) RegimeWindowTrailing(key regimeBundleKey, window string, limit int) ([]RegimeWindowHistoryEntry, error) {
	rows, err := sdb.db.Query(`SELECT label, bar_time FROM regime_window_history
		WHERE platform = ? AND symbol = ? AND timeframe = ? AND spec_json = ? AND window = ?
		ORDER BY id DESC LIMIT ?`,
		key.Platform, key.Symbol, key.Timeframe, key.SpecJSON, window, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RegimeWindowHistoryEntry
	for rows.Next() {
		var e RegimeWindowHistoryEntry
		if err := rows.Scan(&e.Label, &e.BarTime); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (sdb *StateDB) RegimeWindowTrailingLabels(key regimeBundleKey, window string, limit int) ([]string, error) {
	entries, err := sdb.RegimeWindowTrailing(key, window, limit)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Label
	}
	return out, nil
}

func (sdb *StateDB) InsertRegimeWindowHistoryRow(key regimeBundleKey, window, label, barTime, ts string) error {
	_, err := sdb.db.Exec(`INSERT INTO regime_window_history
		(platform, symbol, timeframe, spec_json, window, label, bar_time, ts)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		key.Platform, key.Symbol, key.Timeframe, key.SpecJSON, window, label, barTime, ts)
	return err
}

func (sdb *StateDB) InsertRegimeWindowTransition(key regimeBundleKey, window, oldLabel, newLabel, barTime, ts string) error {
	_, err := sdb.db.Exec(`INSERT INTO regime_window_transitions
		(platform, symbol, timeframe, spec_json, window, old_label, new_label, bar_time, ts, alerted_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, '')`,
		key.Platform, key.Symbol, key.Timeframe, key.SpecJSON, window, oldLabel, newLabel, barTime, ts)
	return err
}

func (sdb *StateDB) UnalertedRegimeWindowTransitions(key regimeBundleKey, window string) ([]RegimeWindowTransitionRow, error) {
	rows, err := sdb.db.Query(`SELECT id, old_label, new_label, bar_time, ts FROM regime_window_transitions
		WHERE platform = ? AND symbol = ? AND timeframe = ? AND spec_json = ? AND window = ? AND alerted_at = ''
		ORDER BY id ASC`,
		key.Platform, key.Symbol, key.Timeframe, key.SpecJSON, window)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RegimeWindowTransitionRow
	for rows.Next() {
		r := RegimeWindowTransitionRow{
			Platform: key.Platform, Symbol: key.Symbol, Timeframe: key.Timeframe,
			SpecJSON: key.SpecJSON, Window: window,
		}
		if err := rows.Scan(&r.ID, &r.OldLabel, &r.NewLabel, &r.BarTime, &r.TS); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (sdb *StateDB) MarkRegimeWindowTransitionsAlerted(ids []int64, ts string) error {
	for _, id := range ids {
		if _, err := sdb.db.Exec(`UPDATE regime_window_transitions SET alerted_at = ? WHERE id = ?`, ts, id); err != nil {
			return err
		}
	}
	return nil
}

func (sdb *StateDB) RecentRegimeWindowTransitions(limit int) ([]RegimeWindowTransitionRow, error) {
	rows, err := sdb.db.Query(`SELECT id, platform, symbol, timeframe, spec_json, window, old_label, new_label, bar_time, ts, alerted_at
		FROM regime_window_transitions ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RegimeWindowTransitionRow
	for rows.Next() {
		var r RegimeWindowTransitionRow
		if err := rows.Scan(&r.ID, &r.Platform, &r.Symbol, &r.Timeframe, &r.SpecJSON, &r.Window,
			&r.OldLabel, &r.NewLabel, &r.BarTime, &r.TS, &r.AlertedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (sdb *StateDB) PruneRegimeWindowRows(cutoffTS string) error {
	if _, err := sdb.db.Exec(`DELETE FROM regime_window_history WHERE ts < ?`, cutoffTS); err != nil {
		return err
	}
	_, err := sdb.db.Exec(`DELETE FROM regime_window_transitions WHERE ts < ?`, cutoffTS)
	return err
}

func (sdb *StateDB) RegimeReversalSignature(key regimeBundleKey) (string, error) {
	var sig string
	err := sdb.db.QueryRow(`SELECT signature FROM regime_reversal_alerts
		WHERE platform = ? AND symbol = ? AND timeframe = ? AND spec_json = ?`,
		key.Platform, key.Symbol, key.Timeframe, key.SpecJSON).Scan(&sig)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return sig, err
}

func (sdb *StateDB) SetRegimeReversalSignature(key regimeBundleKey, sig, ts string) error {
	_, err := sdb.db.Exec(`INSERT INTO regime_reversal_alerts (platform, symbol, timeframe, spec_json, signature, alerted_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(platform, symbol, timeframe, spec_json) DO UPDATE SET signature = excluded.signature, alerted_at = excluded.alerted_at`,
		key.Platform, key.Symbol, key.Timeframe, key.SpecJSON, sig, ts)
	return err
}

func (sdb *StateDB) ClearRegimeReversalSignature(key regimeBundleKey) error {
	_, err := sdb.db.Exec(`DELETE FROM regime_reversal_alerts
		WHERE platform = ? AND symbol = ? AND timeframe = ? AND spec_json = ?`,
		key.Platform, key.Symbol, key.Timeframe, key.SpecJSON)
	return err
}

func regimeTransitionConfirmed(currentLabel string, trailing []string, debounce int) bool {
	if debounce < 1 {
		debounce = 1
	}
	run := 1
	for _, l := range trailing {
		if l != currentLabel {
			break
		}
		run++
	}
	return run >= debounce
}

func netRegimeTransition(pending []RegimeWindowTransitionRow, confirmedLabel string) (oldLabel string, alert bool) {
	if len(pending) == 0 {
		return "", false
	}
	oldLabel = pending[0].OldLabel
	return oldLabel, oldLabel != confirmedLabel
}

type regimeReversalResult struct {
	LongestWindow string
	LongestLabel  string
	Opposing      map[string]string
}

func classifyRegimeReversal(snaps map[string]RegimeSnapshot, periods map[string]int, minOpposing int) (regimeReversalResult, bool) {
	if len(snaps) < 2 || len(periods) < 2 {
		return regimeReversalResult{}, false
	}
	longest := ""
	longestPeriod := 0
	for _, name := range sortedStringKeys(periods) {
		snap, ok := snaps[name]
		if !ok || strings.TrimSpace(snap.Regime) == "" {
			continue
		}
		if p := periods[name]; p > longestPeriod {
			longestPeriod = p
			longest = name
		}
	}
	if longest == "" {
		return regimeReversalResult{}, false
	}
	longestSnap := snaps[longest]
	longestBias := regimeLabelBias(longestSnap.Regime, snapshotReturnEff(longestSnap))
	if longestBias == biasNeutral {
		return regimeReversalResult{}, false
	}
	opposing := make(map[string]string)
	shorter := 0
	for _, name := range sortedStringKeys(periods) {
		if name == longest || periods[name] >= longestPeriod {
			continue
		}
		snap, ok := snaps[name]
		if !ok || strings.TrimSpace(snap.Regime) == "" {
			continue
		}
		shorter++
		if regimeLabelBias(snap.Regime, snapshotReturnEff(snap)) == -longestBias {
			opposing[name] = strings.TrimSpace(snap.Regime)
		}
	}
	if shorter == 0 {
		return regimeReversalResult{}, false
	}
	need := minOpposing
	if need <= 0 {
		need = shorter
	}
	if len(opposing) < need {
		return regimeReversalResult{}, false
	}
	return regimeReversalResult{
		LongestWindow: longest,
		LongestLabel:  strings.TrimSpace(longestSnap.Regime),
		Opposing:      opposing,
	}, true
}

func snapshotReturnEff(s RegimeSnapshot) float64 {
	if s.Metrics == nil {
		return 0
	}
	return s.Metrics["return_eff"]
}

func regimeReversalSignatureString(r regimeReversalResult) string {
	parts := make([]string, 0, len(r.Opposing))
	for _, w := range sortedStringKeys(r.Opposing) {
		parts = append(parts, w+"="+r.Opposing[w])
	}
	return r.LongestWindow + "=" + r.LongestLabel + "|" + strings.Join(parts, ",")
}

func sortedStringKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func formatRegimeTransitionDM(key regimeBundleKey, window, oldLabel, newLabel, barTime string) string {
	msg := fmt.Sprintf("📊 Regime transition — %s window %s: %s → %s", key.String(), window, oldLabel, newLabel)
	if barTime != "" {
		msg += fmt.Sprintf(" (bar %s)", barTime)
	}
	return msg
}

func formatRegimeReversalDM(key regimeBundleKey, r regimeReversalResult) string {
	parts := make([]string, 0, len(r.Opposing))
	for _, w := range sortedStringKeys(r.Opposing) {
		parts = append(parts, fmt.Sprintf("%s=%s", w, r.Opposing[w]))
	}
	return fmt.Sprintf("🔄 Regime reversal pattern — %s: longest window %s=%s, opposing shorter windows: %s",
		key.String(), r.LongestWindow, r.LongestLabel, strings.Join(parts, ", "))
}

var regimeAlertSendFn = func(notifier *MultiNotifier, msg string) {
	if notifier != nil {
		notifier.SendOwnerDM(msg)
	}
}

type regimeReversalPending struct {
	sig         string
	count       int
	inactive    int
	lastBarTime string
}

var regimeReversalPendingState = map[regimeBundleKey]*regimeReversalPending{}

var lastRegimeTransitionPrune time.Time

func processRegimeTransitionAlerts(sdb *StateDB, store *RegimeStore, rc *RegimeConfig, notifier *MultiNotifier, now time.Time) {
	if sdb == nil || store == nil || rc == nil || !rc.Transitions.enabled() {
		return
	}
	tcfg := rc.Transitions
	debounce := tcfg.debounceCycles()
	ts := now.UTC().Format(time.RFC3339)
	bundles, _ := store.Snapshot()
	liveKeys := make(map[regimeBundleKey]bool, len(bundles))

	for _, b := range bundles {
		liveKeys[b.Key] = true
		labels := b.Payload.WindowLabels()
		for _, window := range sortedStringKeys(labels) {
			label := strings.TrimSpace(labels[window])
			if label == "" {
				continue
			}
			trailing, err := sdb.RegimeWindowTrailing(b.Key, window, debounce)
			if err != nil {
				fmt.Printf("[WARN] regime transitions %s/%s: history read failed: %v\n", b.Key, window, err)
				continue
			}
			if b.BarTime != "" && len(trailing) > 0 && trailing[0].BarTime == b.BarTime {
				continue
			}
			labels := make([]string, len(trailing))
			for i, e := range trailing {
				labels[i] = e.Label
			}
			if err := sdb.InsertRegimeWindowHistoryRow(b.Key, window, label, b.BarTime, ts); err != nil {
				fmt.Printf("[WARN] regime transitions %s/%s: history write failed: %v\n", b.Key, window, err)
				continue
			}
			if len(trailing) > 0 && trailing[0].Label != label {
				if err := sdb.InsertRegimeWindowTransition(b.Key, window, trailing[0].Label, label, b.BarTime, ts); err != nil {
					fmt.Printf("[WARN] regime transitions %s/%s: transition write failed: %v\n", b.Key, window, err)
					continue
				}
			}
			if !regimeTransitionConfirmed(label, labels, debounce) {
				continue
			}
			pending, err := sdb.UnalertedRegimeWindowTransitions(b.Key, window)
			if err != nil {
				fmt.Printf("[WARN] regime transitions %s/%s: pending read failed: %v\n", b.Key, window, err)
				continue
			}
			if len(pending) == 0 || pending[len(pending)-1].NewLabel != label {
				continue
			}
			oldLabel, alert := netRegimeTransition(pending, label)
			ids := make([]int64, len(pending))
			for i, p := range pending {
				ids[i] = p.ID
			}
			if err := sdb.MarkRegimeWindowTransitionsAlerted(ids, ts); err != nil {
				fmt.Printf("[WARN] regime transitions %s/%s: alert mark failed: %v\n", b.Key, window, err)
				continue
			}
			if alert {
				regimeAlertSendFn(notifier, formatRegimeTransitionDM(b.Key, window, oldLabel, label, b.BarTime))
			}
		}
		processRegimeReversal(sdb, b, rc, notifier, debounce, ts)
	}

	for key := range regimeReversalPendingState {
		if !liveKeys[key] {
			delete(regimeReversalPendingState, key)
		}
	}

	if now.Sub(lastRegimeTransitionPrune) >= regimeTransitionPruneInterval {
		lastRegimeTransitionPrune = now
		cutoff := now.UTC().Add(-time.Duration(tcfg.retentionDays()) * 24 * time.Hour).Format(time.RFC3339)
		if err := sdb.PruneRegimeWindowRows(cutoff); err != nil {
			fmt.Printf("[WARN] regime transitions: retention prune failed: %v\n", err)
		}
	}
}

func processRegimeReversal(sdb *StateDB, b *RegimeBundle, rc *RegimeConfig, notifier *MultiNotifier, debounce int, ts string) {
	if !b.Payload.MultiMode || b.Key.SpecJSON != regimeWindowsSpecJSON(rc) {
		return
	}
	periods := make(map[string]int, len(rc.Windows))
	for name, spec := range rc.Windows {
		if spec.Period > 0 {
			periods[normalizeRegimeWindowKey(name)] = spec.Period
		}
	}
	result, active := classifyRegimeReversal(b.Payload.Windows, periods, rc.Transitions.reversalMinOpposing())
	pending := regimeReversalPendingState[b.Key]
	if pending == nil {
		pending = &regimeReversalPending{}
		regimeReversalPendingState[b.Key] = pending
	}
	if b.BarTime != "" && pending.lastBarTime == b.BarTime {
		return
	}
	pending.lastBarTime = b.BarTime
	if !active {
		pending.sig = ""
		pending.count = 0
		pending.inactive++
		if pending.inactive == debounce {
			if err := sdb.ClearRegimeReversalSignature(b.Key); err != nil {
				fmt.Printf("[WARN] regime reversal %s: signature clear failed: %v\n", b.Key, err)
			}
		}
		return
	}
	pending.inactive = 0
	sig := regimeReversalSignatureString(result)
	if pending.sig == sig {
		pending.count++
	} else {
		pending.sig = sig
		pending.count = 1
	}
	if pending.count < debounce {
		return
	}
	stored, err := sdb.RegimeReversalSignature(b.Key)
	if err != nil {
		fmt.Printf("[WARN] regime reversal %s: signature read failed: %v\n", b.Key, err)
		return
	}
	if stored == sig {
		return
	}
	if err := sdb.SetRegimeReversalSignature(b.Key, sig, ts); err != nil {
		fmt.Printf("[WARN] regime reversal %s: signature write failed: %v\n", b.Key, err)
		return
	}
	regimeAlertSendFn(notifier, formatRegimeReversalDM(b.Key, result))
}

func recentRegimeTransitionsNote(sdb *StateDB, rc *RegimeConfig, now time.Time) string {
	if sdb == nil || rc == nil || !rc.Transitions.enabled() {
		return ""
	}
	rows, err := sdb.RecentRegimeWindowTransitions(regimeTransitionStatusNoteLimit)
	if err != nil {
		return ""
	}
	cutoff := now.UTC().Add(-regimeTransitionStatusNoteWindow).Format(time.RFC3339)
	var lines []string
	for _, r := range rows {
		if r.TS < cutoff {
			continue
		}
		line := fmt.Sprintf("  %s/%s/%s %s: %s → %s", r.Platform, r.Symbol, r.Timeframe, r.Window, r.OldLabel, r.NewLabel)
		if r.BarTime != "" {
			line += " @ " + r.BarTime
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return ""
	}
	return "\n📊 Regime transitions (24h):\n" + strings.Join(lines, "\n")
}

func (ss *StatusServer) handleAPIRegimeTransitions(w http.ResponseWriter, r *http.Request) {
	if ss.rejectIfDraining(w) {
		return
	}
	if !ss.requireAPIAuth(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	limit := regimeTransitionAPIDefaultLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= regimeTransitionAPIMaxLimit {
			limit = n
		}
	}
	if ss.stateDB == nil {
		writeJSON(w, map[string]interface{}{"transitions": []RegimeWindowTransitionRow{}})
		return
	}
	rows, err := ss.stateDB.RecentRegimeWindowTransitions(limit)
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	if rows == nil {
		rows = []RegimeWindowTransitionRow{}
	}
	writeJSON(w, map[string]interface{}{"transitions": rows})
}
