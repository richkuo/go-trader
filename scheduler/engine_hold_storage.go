package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

type engineHeldIdentity struct {
	ProcessID string
	storageIdentity
}

func engineHeldStorageIdentities(cfg *Config, ident storageIdentityMap, admittedQuote map[string]bool) (map[string]engineHeldIdentity, []string) {
	out := map[string]engineHeldIdentity{}
	if cfg == nil {
		return out, nil
	}
	var rejections []string
	for _, sc := range cfg.Strategies {
		if admittedQuote[sc.ID] {
			continue
		}
		if _, ok := engineHoldFor(sc); !ok {
			continue
		}
		id, ok := ident.storageFor(sc.ID)
		if !ok {
			rejections = append(rejections, fmt.Sprintf("engine-held strategy %s has no storage identity; its saved state cannot be proved inert", sc.ID))
			continue
		}
		if want := partitionFor(sc); id.Partition != want {
			rejections = append(rejections, fmt.Sprintf("engine-held strategy %s maps to the %s partition but is configured for %s", sc.ID, partitionLabel(id.Partition), partitionLabel(want)))
			continue
		}
		out[id.StorageID] = engineHeldIdentity{ProcessID: sc.ID, storageIdentity: id}
	}
	return out, rejections
}

func engineHeldCount(db *StateDB, table, query string, args ...interface{}) (int, bool, error) {
	ok, err := tableExists(db, table)
	if err != nil || !ok {
		return 0, ok, err
	}
	var n int
	if err := db.db.QueryRow(query, args...).Scan(&n); err != nil {
		return 0, true, err
	}
	return n, true, nil
}

func inspectEngineHeldRows(db *StateDB, fi *storageFileInspection, held map[string]engineHeldIdentity) ([]string, error) {
	if len(held) == 0 {
		return nil, nil
	}
	storageIDs := make([]string, 0, len(held))
	for sid := range held {
		storageIDs = append(storageIDs, sid)
	}
	sort.Strings(storageIDs)
	var rejections []string
	for _, sid := range storageIDs {
		h := held[sid]
		label := fmt.Sprintf("%s state file: engine-held strategy %s (stored as %q, %s)", fi.Role, h.ProcessID, sid, quoteEngineHoldReason)
		var evidence []string
		checks := []struct {
			table, what, query string
		}{
			{"positions", "open position rows", "SELECT COUNT(*) FROM positions WHERE strategy_id = ?"},
			{"option_positions", "open option position rows", "SELECT COUNT(*) FROM option_positions WHERE strategy_id = ?"},
			{"pending_manual_actions", "pending manual action rows", "SELECT COUNT(*) FROM pending_manual_actions WHERE strategy_id = ?"},
			{"pending_limit_orders", "pending limit order rows", "SELECT COUNT(*) FROM pending_limit_orders WHERE strategy_id = ?"},
			{"entry_order_intents", "unresolved entry intents", "SELECT COUNT(*) FROM entry_order_intents WHERE strategy_id = ? AND status IN ('reserved','submitting','settling')"},
			{"robinhood_order_attempts", "order attempt rows", "SELECT COUNT(*) FROM robinhood_order_attempts WHERE strategy_id = ?"},
		}
		for _, c := range checks {
			n, _, err := engineHeldCount(db, c.table, c.query, sid)
			if err != nil {
				rejections = append(rejections, fmt.Sprintf("%s: %s are unreadable (%v); refusing to start the hold", label, c.what, err))
				continue
			}
			if n > 0 {
				evidence = append(evidence, fmt.Sprintf("%d %s", n, c.what))
			}
		}
		stratEvidence, err := engineHeldStrategyRowEvidence(db, sid)
		if err != nil {
			rejections = append(rejections, fmt.Sprintf("%s: its strategy row is unreadable (%v); refusing to start the hold", label, err))
		}
		evidence = append(evidence, stratEvidence...)
		if len(evidence) == 0 {
			continue
		}
		if fi.Role != h.Role {
			rejections = append(rejections, fmt.Sprintf("%s: the %s state file holds %s for it, but the strategy is mapped to the %s state file; move or resolve it by hand before the hold can start", label, fi.Role, strings.Join(evidence, ", "), h.Role))
			continue
		}
		rejections = append(rejections, fmt.Sprintf("%s holds %s; the engine hold would suppress their management, so startup is refused — flatten and resolve them with the strategy unheld, or remove the quote ownership", label, strings.Join(evidence, ", ")))
	}
	for _, sid := range storageIDs {
		if held[sid].Role == fi.Role {
			fi.EngineHeld = append(fi.EngineHeld, held[sid].ProcessID)
		}
	}
	sort.Strings(fi.EngineHeld)
	return rejections, nil
}

func engineHeldStrategyRowEvidence(db *StateDB, storageID string) ([]string, error) {
	ok, err := tableExists(db, "strategies")
	if err != nil || !ok {
		return nil, err
	}
	var evidence []string
	textColumns := []struct{ column, what string }{
		{"risk_pending_circuit_closes_json", "pending circuit-breaker closes"},
		{"risk_pending_hl_close_json", "pending circuit-breaker closes"},
		{"paper_funding_state", "paper funding evidence"},
	}
	for _, c := range textColumns {
		has, err := db.tableHasColumn("strategies", c.column)
		if err != nil {
			return nil, err
		}
		if !has {
			continue
		}
		var v string
		err = db.db.QueryRow("SELECT COALESCE("+c.column+", '') FROM strategies WHERE id = ?", storageID).Scan(&v)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return evidence, nil
			}
			return nil, err
		}
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if c.column == "paper_funding_state" {
			open, perr := paperFundingStateHasOpenEvidence(v)
			if perr != nil {
				return nil, fmt.Errorf("paper funding evidence: %w", perr)
			}
			if open {
				evidence = append(evidence, c.what)
			}
			continue
		}
		if v != "[]" && v != "{}" && v != "null" {
			evidence = append(evidence, c.what)
		}
	}
	return evidence, nil
}

func paperFundingStateHasOpenEvidence(raw string) (bool, error) {
	var st paperFundingState
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		return false, err
	}
	for _, c := range st.Coins {
		if c == nil {
			continue
		}
		if c.Anchor.SignedQty != 0 || len(c.Segments) > 0 || len(c.Unknown) > 0 || len(c.Held) > 0 {
			return true, nil
		}
	}
	return false, nil
}
