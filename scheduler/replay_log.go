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


const (
	ReplayDecisionOpen         = "open"
	ReplayDecisionScaleIn      = "scale_in"
	ReplayDecisionPartialClose = "partial_close"
	ReplayDecisionFullClose    = "full_close"
)

const (
	replayStatusPending = "pending"
	replayStatusApplied = "applied"
)

const (
	ReplaySharingNone       = "none"
	ReplaySharingLiveMirror = "live_mirror"
)

type ReplayDecision struct {
	DecisionID     int64
	StrategyID     string
	DecisionType   string
	DecidedAt      time.Time
	Symbol         string
	Side           string
	Quantity       float64
	ReferencePrice float64
	CloseReason    string
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

var replayLogColumnMigrations = []string{
	"ALTER TABLE decisions ADD COLUMN entry_atr REAL NOT NULL DEFAULT 0",
	"ALTER TABLE decisions ADD COLUMN regime TEXT NOT NULL DEFAULT ''",
}

type DecisionLogDB struct {
	db *sql.DB
}

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

func (d *DecisionLogDB) Close() error {
	if d == nil || d.db == nil {
		return nil
	}
	return d.db.Close()
}

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


var replayLiveSources = struct {
	sync.RWMutex
	ids map[string]bool
}{ids: make(map[string]bool)}

func replaySourceActive(strategyID string) bool {
	replayLiveSources.RLock()
	defer replayLiveSources.RUnlock()
	return replayLiveSources.ids[strategyID]
}

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

func replaySharingSourceEnabled(sc StrategyConfig) bool {
	return sc.ReplaySharing == ReplaySharingLiveMirror &&
		sc.Type == "perps" && sc.Platform == "hyperliquid" && isLiveArgs(sc.Args)
}

func replayMirrorPaperActive(sc StrategyConfig) bool {
	return sc.ReplaySharing == ReplaySharingLiveMirror &&
		sc.Type == "perps" && sc.Platform == "hyperliquid" && !isLiveArgs(sc.Args)
}

var decisionLogWriter func(dec ReplayDecision) error

var decisionLog *DecisionLogDB

var decisionLogPersistWarn func(msg string)

var decisionLogInsertFailures struct {
	sync.Mutex
	count         int
	alertedStreak bool
}

const decisionLogAlertThreshold = 3

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

func validReplaySharing(v string) bool {
	switch v {
	case "", ReplaySharingNone, ReplaySharingLiveMirror:
		return true
	}
	return false
}

func normalizeReplaySharing(v string) string {
	if v == "" {
		return ReplaySharingNone
	}
	return strings.ToLower(strings.TrimSpace(v))
}
