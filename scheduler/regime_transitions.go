package main

// regime_transitions.go — per-window regime transition history, detection,
// and cross-window reversal alerting (#1224).
//
// Every cycle, after the regime-store population wait returns (the store is
// stable from that point — seal() only fires on budget exhaustion), the
// processor persists each bundle's per-window labels into SQLite
// (regime_window_history), diffs each (bundle key, window) against its last
// stored label, and records a transition row on change. After a new label
// persists for debounce_cycles consecutive populations, the operator gets
// exactly one DM per net label change — a flap that returns to the original
// label within the debounce window is marked handled without a DM.
//
// On top of raw transitions, a reversal-pattern classifier flags "the longest
// configured window reads direction X while the shorter windows read the
// opposing direction" (e.g. 30d trending_down while 1d/3d/7d read
// trending_up). Reversal alerts are deduped by a persisted per-key signature
// so restarts and SIGHUP never re-alert an unchanged pattern.
//
// Keying: history and transitions carry the FULL regimeBundleKey (data
// platform, symbol, timeframe, windows-spec JSON) plus the window name —
// same-symbol signatures on different platforms/timeframes/specs are distinct
// computations and must not collide.
//
// Placement and failure policy: runs on the sequential main loop immediately
// after regimeStoreReady(), outside `mu`, so MultiNotifier sends stay
// serialized with every other caller. Alerting only — never gates entries,
// mutates config, or touches positions. Any store/DB failure is fail-open:
// WARN + skip, never blocking the trading loop (#879 convention).

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

// regimeTransitionPruneInterval throttles the retention DELETE to once per
// interval instead of every cycle. Var so tests can shrink it.
var regimeTransitionPruneInterval = time.Hour

// RegimeTransitionAlertsConfig is the optional `regime.transitions` block.
// Alerting-only and hot-reloadable via SIGHUP (config_reload.go copies it).
type RegimeTransitionAlertsConfig struct {
	Enabled bool `json:"enabled"`
	// DebounceCycles is how many consecutive populations a new label must
	// persist before the transition DM fires (raw history rows are never
	// debounced). 0 means the default; the minimum effective value is 1.
	DebounceCycles int `json:"debounce_cycles,omitempty"`
	// RetentionDays bounds regime_window_history / regime_window_transitions
	// growth; rows older than this are pruned. 0 means the default.
	RetentionDays int `json:"retention_days,omitempty"`
	// ReversalMinOpposing is how many shorter windows must oppose the longest
	// window's direction before the reversal alert fires. 0 (default) means
	// ALL shorter windows must oppose.
	ReversalMinOpposing int `json:"reversal_min_opposing,omitempty"`
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

// validateRegimeTransitionsConfig rejects nonsensical values at load time.
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

// RegimeWindowTransitionRow is one persisted transition event.
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

// ─── StateDB layer ───────────────────────────────────────────────────────────

// RegimeWindowTrailingLabels returns the most-recent-first labels stored for
// (key, window), capped at limit. Used both as the transition baseline
// (index 0 is the last stored label) and as the debounce run.
func (sdb *StateDB) RegimeWindowTrailingLabels(key regimeBundleKey, window string, limit int) ([]string, error) {
	rows, err := sdb.db.Query(`SELECT label FROM regime_window_history
		WHERE platform = ? AND symbol = ? AND timeframe = ? AND spec_json = ? AND window = ?
		ORDER BY id DESC LIMIT ?`,
		key.Platform, key.Symbol, key.Timeframe, key.SpecJSON, window, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var label string
		if err := rows.Scan(&label); err != nil {
			return nil, err
		}
		out = append(out, label)
	}
	return out, rows.Err()
}

// InsertRegimeWindowHistoryRow appends one raw per-cycle label observation.
func (sdb *StateDB) InsertRegimeWindowHistoryRow(key regimeBundleKey, window, label, barTime, ts string) error {
	_, err := sdb.db.Exec(`INSERT INTO regime_window_history
		(platform, symbol, timeframe, spec_json, window, label, bar_time, ts)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		key.Platform, key.Symbol, key.Timeframe, key.SpecJSON, window, label, barTime, ts)
	return err
}

// InsertRegimeWindowTransition records a label flip (unalerted).
func (sdb *StateDB) InsertRegimeWindowTransition(key regimeBundleKey, window, oldLabel, newLabel, barTime, ts string) error {
	_, err := sdb.db.Exec(`INSERT INTO regime_window_transitions
		(platform, symbol, timeframe, spec_json, window, old_label, new_label, bar_time, ts, alerted_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, '')`,
		key.Platform, key.Symbol, key.Timeframe, key.SpecJSON, window, oldLabel, newLabel, barTime, ts)
	return err
}

// UnalertedRegimeWindowTransitions returns the pending (never-DM'd)
// transitions for (key, window), oldest first.
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

// MarkRegimeWindowTransitionsAlerted stamps alerted_at on the given rows —
// the persisted marker that makes the DM exactly-once across restarts/SIGHUP.
func (sdb *StateDB) MarkRegimeWindowTransitionsAlerted(ids []int64, ts string) error {
	for _, id := range ids {
		if _, err := sdb.db.Exec(`UPDATE regime_window_transitions SET alerted_at = ? WHERE id = ?`, ts, id); err != nil {
			return err
		}
	}
	return nil
}

// RecentRegimeWindowTransitions returns the newest transitions across all
// keys, newest first, for the /status note and the dashboard API.
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

// PruneRegimeWindowRows deletes history and transition rows older than
// cutoffTS (RFC3339 — lexicographic order matches time order).
func (sdb *StateDB) PruneRegimeWindowRows(cutoffTS string) error {
	if _, err := sdb.db.Exec(`DELETE FROM regime_window_history WHERE ts < ?`, cutoffTS); err != nil {
		return err
	}
	_, err := sdb.db.Exec(`DELETE FROM regime_window_transitions WHERE ts < ?`, cutoffTS)
	return err
}

// RegimeReversalSignature returns the last-alerted reversal signature for a
// bundle key ("" when none).
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

// SetRegimeReversalSignature upserts the alerted signature for a bundle key.
func (sdb *StateDB) SetRegimeReversalSignature(key regimeBundleKey, sig, ts string) error {
	_, err := sdb.db.Exec(`INSERT INTO regime_reversal_alerts (platform, symbol, timeframe, spec_json, signature, alerted_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(platform, symbol, timeframe, spec_json) DO UPDATE SET signature = excluded.signature, alerted_at = excluded.alerted_at`,
		key.Platform, key.Symbol, key.Timeframe, key.SpecJSON, sig, ts)
	return err
}

// ClearRegimeReversalSignature forgets a key's alerted signature so an
// identical pattern re-occurring after a confirmed clear re-alerts.
func (sdb *StateDB) ClearRegimeReversalSignature(key regimeBundleKey) error {
	_, err := sdb.db.Exec(`DELETE FROM regime_reversal_alerts
		WHERE platform = ? AND symbol = ? AND timeframe = ? AND spec_json = ?`,
		key.Platform, key.Symbol, key.Timeframe, key.SpecJSON)
	return err
}

// ─── Pure detection helpers (Go CI must not depend on the DB or Python) ─────

// regimeTransitionConfirmed reports whether currentLabel has persisted for at
// least debounce consecutive populations: the current observation plus the
// leading run of identical labels in trailing (most-recent-first labels
// stored BEFORE the current one).
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

// netRegimeTransition collapses a debounce window's pending transitions into
// the single net change to confirmedLabel. alert=false when the chain flapped
// back to where it started (net no change) — the rows are still marked
// alerted so they can never fire later.
func netRegimeTransition(pending []RegimeWindowTransitionRow, confirmedLabel string) (oldLabel string, alert bool) {
	if len(pending) == 0 {
		return "", false
	}
	oldLabel = pending[0].OldLabel
	return oldLabel, oldLabel != confirmedLabel
}

// regimeReversalResult is one detected cross-window reversal pattern.
type regimeReversalResult struct {
	LongestWindow string
	LongestLabel  string
	// Opposing maps shorter window name -> its label, for every shorter
	// window whose bias opposes the longest window's.
	Opposing map[string]string
}

// classifyRegimeReversal flags "longest window reads direction X, shorter
// windows read the opposing direction". snaps is the bundle's multi-window
// payload; periods maps window name -> configured bar period (rc.Windows).
// minOpposing = 0 requires ALL shorter windows to oppose; k > 0 requires at
// least k. Neutral shorter windows never count as opposing.
func classifyRegimeReversal(snaps map[string]RegimeSnapshot, periods map[string]int, minOpposing int) (regimeReversalResult, bool) {
	if len(snaps) < 2 || len(periods) < 2 {
		return regimeReversalResult{}, false
	}
	// Longest window = max configured period among windows present in the
	// payload; sorted-name tiebreak for determinism.
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

// regimeReversalSignatureString renders a stable dedupe signature for a
// detected pattern (sorted windows — map iteration is randomized).
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

// ─── DM formatting ───────────────────────────────────────────────────────────

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

// ─── Per-cycle processor ─────────────────────────────────────────────────────

// regimeAlertSendFn is the DM boundary — package var so tests capture alerts
// without a Discord backend (runRegimeBundleCheckFn pattern).
var regimeAlertSendFn = func(notifier *MultiNotifier, msg string) {
	if notifier != nil {
		notifier.SendOwnerDM(msg)
	}
}

// regimeReversalPending is the in-memory pattern debounce for one bundle key.
// Main-loop only (no lock); self-heals on restart — the persisted signature
// in regime_reversal_alerts is what prevents duplicate DMs across restarts.
type regimeReversalPending struct {
	sig      string
	count    int
	inactive int
}

var regimeReversalPendingState = map[regimeBundleKey]*regimeReversalPending{}

// lastRegimeTransitionPrune throttles retention pruning. Main-loop only.
var lastRegimeTransitionPrune time.Time

// processRegimeTransitionAlerts runs once per cycle right after
// regimeStoreReady(), on the sequential main loop, outside `mu`. Every
// failure path is fail-open (WARN + continue) — this must never block or
// delay the trading loop.
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
			trailing, err := sdb.RegimeWindowTrailingLabels(b.Key, window, debounce)
			if err != nil {
				fmt.Printf("[WARN] regime transitions %s/%s: history read failed: %v\n", b.Key, window, err)
				continue
			}
			if err := sdb.InsertRegimeWindowHistoryRow(b.Key, window, label, b.BarTime, ts); err != nil {
				fmt.Printf("[WARN] regime transitions %s/%s: history write failed: %v\n", b.Key, window, err)
				continue
			}
			// Transition event within one cycle of the flip. No prior row
			// (first cycle ever, or first after a retention prune of a dead
			// key) is NOT a transition — boot must not false-alert.
			if len(trailing) > 0 && trailing[0] != label {
				if err := sdb.InsertRegimeWindowTransition(b.Key, window, trailing[0], label, b.BarTime, ts); err != nil {
					fmt.Printf("[WARN] regime transitions %s/%s: transition write failed: %v\n", b.Key, window, err)
					continue
				}
			}
			if !regimeTransitionConfirmed(label, trailing, debounce) {
				continue
			}
			pending, err := sdb.UnalertedRegimeWindowTransitions(b.Key, window)
			if err != nil {
				fmt.Printf("[WARN] regime transitions %s/%s: pending read failed: %v\n", b.Key, window, err)
				continue
			}
			// Only fire when the debounced label matches the newest pending
			// transition — an unconfirmed newer flip stays pending.
			if len(pending) == 0 || pending[len(pending)-1].NewLabel != label {
				continue
			}
			oldLabel, alert := netRegimeTransition(pending, label)
			ids := make([]int64, len(pending))
			for i, p := range pending {
				ids[i] = p.ID
			}
			// Mark BEFORE sending: a crash between mark and send drops one DM
			// (fail-open); the reverse order would duplicate money-adjacent
			// operator signals on every crash-loop.
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

	// Drop in-memory reversal debounce state for keys absent this cycle
	// (strategy removed / not due) so a stale counter can't confirm later.
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

// processRegimeReversal evaluates the cross-window pattern for one bundle.
func processRegimeReversal(sdb *StateDB, b *RegimeBundle, rc *RegimeConfig, notifier *MultiNotifier, debounce int, ts string) {
	// Periods come from rc.Windows, so only bundles computed under rc's
	// current spec qualify (options bundles run a fixed single-window spec).
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
	if !active {
		pending.sig = ""
		pending.count = 0
		pending.inactive++
		if pending.inactive == debounce {
			// Confirmed clear: forget the alerted signature so a genuine
			// re-occurrence alerts again.
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
	// Persist BEFORE sending — same crash-ordering rationale as transitions.
	if err := sdb.SetRegimeReversalSignature(b.Key, sig, ts); err != nil {
		fmt.Printf("[WARN] regime reversal %s: signature write failed: %v\n", b.Key, err)
		return
	}
	regimeAlertSendFn(notifier, formatRegimeReversalDM(b.Key, result))
}

// ─── Operator surfaces ───────────────────────────────────────────────────────

// recentRegimeTransitionsNote renders the /status "recent transitions" block:
// newest transitions from the last 24h, capped. Empty string when the feature
// is disabled, the DB is unavailable, or nothing happened.
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

// handleAPIRegimeTransitions serves GET /api/regime/transitions — the
// dashboard's recent-transitions view (?limit=N, default 50, max 500).
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
