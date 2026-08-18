package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// replay_log.go — #1431 live-to-paper decision replay log.
//
// A shared SQLite database (outside both deploy trees, configured via the
// root-level replay_log_path knob) that a LIVE Hyperliquid perps deployment
// writes one row per exposure-changing decision to — open, scale_in,
// partial_close, full_close — and a PAPER deployment with the same
// strategy_id replays from when the strategy opts in with
// replay_sharing="live_mirror". Paper's only legitimate differences become
// (a) no real money moves and (b) the exit price may differ when live's fill
// is asynchronous.
//
// Fail-safe contract: the decision insert runs in its OWN transaction
// immediately after the position/trade write commits — never in the same
// transaction — so a log failure can never roll back trade state. Insert
// failures are counted; decisionLogPersistWarn fires at 3 consecutive
// failures (mirroring the "primary at 3" alerting convention) and the trade
// itself always stands.
//
// Single-mirror scope (v1): one replay_status column tracks one consumer.
// Multiple paper mirrors of the same live strategy need per-mirror cursors
// and are deferred to a follow-up.

// Decision types written to the log.
const (
	ReplayDecisionOpen         = "open"
	ReplayDecisionScaleIn      = "scale_in"
	ReplayDecisionPartialClose = "partial_close"
	ReplayDecisionFullClose    = "full_close"
)

// Replay statuses.
const (
	replayStatusPending = "pending"
	replayStatusApplied = "applied"
)

// ReplaySharing config values (StrategyConfig.ReplaySharing).
const (
	ReplaySharingNone       = "none"
	ReplaySharingLiveMirror = "live_mirror"
)

// ReplayDecision is one live exposure-changing decision, recorded AFTER fill
// resolution so quantity/reference_price are live's ACTUAL filled quantity
// and VWAP (never intended size).
type ReplayDecision struct {
	DecisionID     int64
	StrategyID     string
	DecisionType   string // open | scale_in | partial_close | full_close
	DecidedAt      time.Time
	Symbol         string
	Side           string // position side: "long" | "short"
	Quantity       float64
	ReferencePrice float64
	CloseReason    string // close decisions only; "" for open/scale_in
	// EntryATR/Regime carry live's open-time stamps on open/scale_in rows so
	// the paper mirror seeds the SAME stop geometry and regime label live
	// booked (paper's own check payload can disagree with live's on the same
	// bar). 0/"" on close rows and on rows written before the columns
	// existed — the mirror falls back to its own payload stamps then.
	EntryATR     float64
	Regime       string
	ReplayStatus string
}

const replayLogSchemaDDL = `
CREATE TABLE IF NOT EXISTS decisions (
	decision_id INTEGER PRIMARY KEY,
	strategy_id TEXT NOT NULL,
	decision_type TEXT NOT NULL,
	decided_at TEXT NOT NULL,
	symbol TEXT NOT NULL,
	side TEXT NOT NULL,
	quantity REAL NOT NULL,
	reference_price REAL NOT NULL,
	close_reason TEXT NOT NULL DEFAULT '',
	replay_status TEXT NOT NULL DEFAULT 'pending',
	entry_atr REAL NOT NULL DEFAULT 0,
	regime TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_decisions_strategy_decided ON decisions(strategy_id, decided_at);
-- Covering index for the per-cycle pending read: filter (strategy_id,
-- replay_status) + order (decision_id) resolve from the index alone, so the
-- lookup stays O(pending) instead of scanning every applied row the strategy
-- ever wrote.
CREATE INDEX IF NOT EXISTS idx_decisions_strategy_status_id ON decisions(strategy_id, replay_status, decision_id);
`

// replayLogColumnMigrations brings pre-column databases forward. Idempotent:
// a "duplicate column name" error means the column already exists.
var replayLogColumnMigrations = []string{
	"ALTER TABLE decisions ADD COLUMN entry_atr REAL NOT NULL DEFAULT 0",
	"ALTER TABLE decisions ADD COLUMN regime TEXT NOT NULL DEFAULT ''",
}

// DecisionLogDB wraps the shared decision-log SQLite database. Live and paper
// deployments on the same host open the same path concurrently, so WAL +
// busy_timeout match the state-DB pragmas.
type DecisionLogDB struct {
	db *sql.DB
}

// OpenDecisionLogDB opens (or creates) the decision-log database at path.
func OpenDecisionLogDB(path string) (*DecisionLogDB, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("create replay log dir: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open replay log db: %w", err)
	}
	db.SetMaxOpenConns(1)
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA foreign_keys=ON",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("pragma %q: %w", pragma, err)
		}
	}
	if _, err := db.Exec(replayLogSchemaDDL); err != nil {
		db.Close()
		return nil, fmt.Errorf("create replay log schema: %w", err)
	}
	for _, migration := range replayLogColumnMigrations {
		if _, err := db.Exec(migration); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			db.Close()
			return nil, fmt.Errorf("replay log migration %q: %w", migration, err)
		}
	}
	return &DecisionLogDB{db: db}, nil
}

// Close closes the database connection.
func (d *DecisionLogDB) Close() error {
	if d == nil || d.db == nil {
		return nil
	}
	return d.db.Close()
}

// InsertDecision appends one pending decision row. It runs as its own
// auto-committed statement — callers rely on that for the fail-safe contract
// (a log failure never touches trade state).
func (d *DecisionLogDB) InsertDecision(dec ReplayDecision) error {
	if d == nil || d.db == nil {
		return fmt.Errorf("replay log db unavailable")
	}
	_, err := d.db.Exec(`INSERT INTO decisions
		(strategy_id, decision_type, decided_at, symbol, side, quantity, reference_price, close_reason, replay_status, entry_atr, regime)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		dec.StrategyID, dec.DecisionType, formatTime(dec.DecidedAt), dec.Symbol, dec.Side,
		dec.Quantity, dec.ReferencePrice, dec.CloseReason, replayStatusPending, dec.EntryATR, dec.Regime)
	if err != nil {
		return fmt.Errorf("insert replay decision for %s: %w", dec.StrategyID, err)
	}
	return nil
}

// PendingDecisions returns the unconsumed decisions for a strategy in
// application order (decision_id is monotonic with insert order).
func (d *DecisionLogDB) PendingDecisions(strategyID string) ([]ReplayDecision, error) {
	if d == nil || d.db == nil {
		return nil, fmt.Errorf("replay log db unavailable")
	}
	rows, err := d.db.Query(`SELECT decision_id, strategy_id, decision_type, decided_at, symbol, side, quantity, reference_price, close_reason, entry_atr, regime, replay_status
		FROM decisions WHERE strategy_id = ? AND replay_status = ? ORDER BY decision_id ASC`, strategyID, replayStatusPending)
	if err != nil {
		return nil, fmt.Errorf("query pending replay decisions for %s: %w", strategyID, err)
	}
	defer rows.Close()
	var out []ReplayDecision
	for rows.Next() {
		var dec ReplayDecision
		var decidedAt string
		if err := rows.Scan(&dec.DecisionID, &dec.StrategyID, &dec.DecisionType, &decidedAt, &dec.Symbol, &dec.Side, &dec.Quantity, &dec.ReferencePrice, &dec.CloseReason, &dec.EntryATR, &dec.Regime, &dec.ReplayStatus); err != nil {
			return nil, fmt.Errorf("scan replay decision: %w", err)
		}
		dec.DecidedAt = parseTime(decidedAt)
		out = append(out, dec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate replay decisions: %w", err)
	}
	return out, nil
}

// MarkDecisionsApplied flips the given decision rows to replay_status='applied'
// in a single transaction. Ids are the rows the mirror successfully applied
// (or deliberately skipped as drift) this cycle.
func (d *DecisionLogDB) MarkDecisionsApplied(ids []int64) error {
	if d == nil || d.db == nil {
		return fmt.Errorf("replay log db unavailable")
	}
	if len(ids) == 0 {
		return nil
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	tx, err := d.db.Begin()
	if err != nil {
		return fmt.Errorf("begin replay mark-applied tx: %w", err)
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`UPDATE decisions SET replay_status = ? WHERE decision_id = ?`)
	if err != nil {
		return fmt.Errorf("prepare replay mark-applied: %w", err)
	}
	defer stmt.Close()
	for _, id := range ids {
		if _, err := stmt.Exec(replayStatusApplied, id); err != nil {
			return fmt.Errorf("mark replay decision %d applied: %w", id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit replay mark-applied: %w", err)
	}
	return nil
}

// ─── write-side gating + failure alerting ────────────────────────────────────

// replayLiveSources is the set of strategy IDs whose LIVE bookings are written
// to the decision log: HL perps strategies with replay_sharing="live_mirror"
// running with --mode=live. Rebuilt from config at startup and after every
// successful hot reload (toggling replay_sharing is flat-only, so the set can
// never change under an open position). Paper deployments never appear here —
// isLiveArgs(sc.Args) is false for them — so paper's own bookings never echo
// back into the log.
var replayLiveSources = struct {
	sync.RWMutex
	ids map[string]bool
}{ids: make(map[string]bool)}

// replaySourceActive reports whether live bookings for strategyID are written
// to the decision log.
func replaySourceActive(strategyID string) bool {
	replayLiveSources.RLock()
	defer replayLiveSources.RUnlock()
	return replayLiveSources.ids[strategyID]
}

// rebuildReplayLiveSources recomputes the live-write source set from cfg.
func rebuildReplayLiveSources(cfg *Config) {
	ids := make(map[string]bool)
	if cfg != nil {
		for _, sc := range cfg.Strategies {
			if replaySharingSourceEnabled(sc) {
				ids[sc.ID] = true
			}
		}
	}
	replayLiveSources.Lock()
	replayLiveSources.ids = ids
	replayLiveSources.Unlock()
}

// replaySharingSourceEnabled reports whether sc writes decisions to the log:
// the opt-in flag on a LIVE HL perps strategy.
func replaySharingSourceEnabled(sc StrategyConfig) bool {
	return sc.ReplaySharing == ReplaySharingLiveMirror &&
		sc.Type == "perps" && sc.Platform == "hyperliquid" && isLiveArgs(sc.Args)
}

// replayMirrorPaperActive reports whether sc replays the live decision log
// instead of computing its own opens: the opt-in flag on a PAPER HL perps
// strategy.
func replayMirrorPaperActive(sc StrategyConfig) bool {
	return sc.ReplaySharing == ReplaySharingLiveMirror &&
		sc.Type == "perps" && sc.Platform == "hyperliquid" && !isLiveArgs(sc.Args)
}

// decisionLogWriter is the package-level insert hook, mirroring the
// tradeRecorder pattern (#289). main.go sets it to DecisionLogDB.InsertDecision
// after OpenDecisionLogDB; nil (tests, early boot, replay_log_path unset)
// disables logging entirely.
//
// Test caveat: tests that swap this hook must NOT use t.Parallel() — the swap
// mutates package state and will race. Same applies to decisionLogPersistWarn.
var decisionLogWriter func(dec ReplayDecision) error

// decisionLog is the package-level handle the paper mirror consumes from.
// main.go sets it after OpenDecisionLogDB; nil disables mirroring (and
// validateConfig already rejects replay_sharing=live_mirror without a
// configured replay_log_path).
var decisionLog *DecisionLogDB

// decisionLogPersistWarn is the operator-visible warning hook for consecutive
// decision-log insert failures (owner DM), mirroring tradePersistWarn. When
// nil, failures fall back to stderr only.
var decisionLogPersistWarn func(msg string)

// decisionLogInsertFailures counts CONSECUTIVE insert failures (reset on the
// first success). The DM fires once per streak at the threshold.
var decisionLogInsertFailures struct {
	sync.Mutex
	count         int
	alertedStreak bool
}

// decisionLogAlertThreshold mirrors the "primary at 3" failure-alert
// convention: three consecutive insert failures before the owner DM fires.
const decisionLogAlertThreshold = 3

// recordReplayDecision writes one decision row when the strategy is a live
// replay source and the writer hook is set. Invoke AFTER the position/trade
// write commits, never before — the insert is its own transaction, so a
// failure here can never roll back trade state; failures surface via stderr
// plus a throttled owner DM at decisionLogAlertThreshold consecutive failures.
func recordReplayDecision(s *StrategyState, decisionType, symbol, side string, qty, referencePrice float64, closeReason string, decidedAt time.Time, entryATR float64, regime string) {
	if s == nil || decisionLogWriter == nil || !replaySourceActive(s.ID) {
		return
	}
	dec := ReplayDecision{
		StrategyID:     s.ID,
		DecisionType:   decisionType,
		DecidedAt:      decidedAt,
		Symbol:         symbol,
		Side:           side,
		Quantity:       qty,
		ReferencePrice: referencePrice,
		CloseReason:    closeReason,
		EntryATR:       entryATR,
		Regime:         regime,
	}
	if err := decisionLogWriter(dec); err != nil {
		msg := fmt.Sprintf("replay decision log insert failed for %s (%s %s %.6f @ %.4f): %v",
			s.ID, decisionType, symbol, qty, referencePrice, err)
		fmt.Fprintf(os.Stderr, "[replay] WARN: %s\n", msg)
		decisionLogInsertFailures.Lock()
		decisionLogInsertFailures.count++
		streak := decisionLogInsertFailures.count
		fire := streak >= decisionLogAlertThreshold && !decisionLogInsertFailures.alertedStreak
		if fire {
			decisionLogInsertFailures.alertedStreak = true
		}
		decisionLogInsertFailures.Unlock()
		if fire && decisionLogPersistWarn != nil {
			decisionLogPersistWarn(fmt.Sprintf("%s — %d consecutive failures; paper mirrors are now stale until the log recovers (#1431)", msg, streak))
		}
		return
	}
	decisionLogInsertFailures.Lock()
	decisionLogInsertFailures.count = 0
	decisionLogInsertFailures.alertedStreak = false
	decisionLogInsertFailures.Unlock()
}

// validReplaySharing reports whether v is a legal replay_sharing value.
// "" and "none" both mean the default (no sharing).
func validReplaySharing(v string) bool {
	switch v {
	case "", ReplaySharingNone, ReplaySharingLiveMirror:
		return true
	}
	return false
}

// normalizeReplaySharing collapses the default spellings.
func normalizeReplaySharing(v string) string {
	if v == "" {
		return ReplaySharingNone
	}
	return strings.ToLower(strings.TrimSpace(v))
}
