package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

type uiQuoteHeldOwner struct {
	SourceRole storageRole `json:"source_role"`
	Partition  string      `json:"partition"`
	StrategyID string      `json:"strategy_id"`
	StorageID  string      `json:"storage_strategy_id"`
	Mapped     bool        `json:"mapped"`
	Coin       string      `json:"coin"`
	Revision   int64       `json:"revision"`
	Inventory  string      `json:"inventory"`
	Pending    []string    `json:"pending"`
}

type uiQuotesResponse struct {
	Owners     []quoteSnapshot    `json:"owners"`
	Held       []uiQuoteHeldOwner `json:"held"`
	Unmapped   []string           `json:"unmapped,omitempty"`
	OwnerCount int                `json:"owner_count"`
	HeldCount  int                `json:"held_count"`
}

func (ss *StatusServer) handleAPIQuotes(w http.ResponseWriter, r *http.Request) {
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
	filter, ok := ss.uiPartitionParam(w, r)
	if !ok {
		return
	}
	if ss.stateDB == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "database not available")
		return
	}
	out := uiQuotesResponse{Owners: []quoteSnapshot{}, Held: []uiQuoteHeldOwner{}}
	adm := quoteAdmissionForStore(ss.stateDB)
	if ss.mu != nil {
		ss.mu.RLock()
	}
	ss.strategiesMu.RLock()
	cfgs := append([]StrategyConfig(nil), ss.strategies...)
	ss.strategiesMu.RUnlock()
	if ss.mu != nil {
		ss.mu.RUnlock()
	}
	if adm != nil && (filter.All || adm.partition == filter.Partition) {
		out.Owners = append(out.Owners, adm.Snapshot())
	}
	for _, role := range ss.stateDB.order {
		db := ss.stateDB.file(role)
		if db == nil {
			continue
		}
		summary, err := db.quoteFileSummary(db.mapStoredStrategyID)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("quote evidence in the %s state file is unreadable: %v", role, err))
			return
		}
		for _, u := range summary.Unmapped {
			out.Unmapped = append(out.Unmapped, fmt.Sprintf("%s: %s", role, u))
		}
		for _, o := range summary.Owners {
			if !filter.All && o.Partition != filter.Partition {
				continue
			}
			if adm != nil && o.ProcessID == adm.sc.ID && o.Partition == adm.partition && role == adm.db.storageRoleOf() {
				continue
			}
			if o.Mapped {
				if p, known := partitionOfStrategyID(cfgs, o.ProcessID); known && p != o.Partition {
					continue
				}
			}
			out.Held = append(out.Held, uiQuoteHeldOwner{SourceRole: role, Partition: o.Partition.String(), StrategyID: o.ProcessID, StorageID: o.StorageID,
				Mapped: o.Mapped, Coin: o.Coin, Revision: o.Revision, Inventory: o.Inventory, Pending: append([]string{}, o.Pending...)})
		}
	}
	sort.Slice(out.Held, func(i, j int) bool {
		if out.Held[i].SourceRole != out.Held[j].SourceRole {
			return out.Held[i].SourceRole < out.Held[j].SourceRole
		}
		if out.Held[i].Partition != out.Held[j].Partition {
			return out.Held[i].Partition < out.Held[j].Partition
		}
		return out.Held[i].StorageID < out.Held[j].StorageID
	})
	sort.Strings(out.Unmapped)
	out.OwnerCount = len(out.Owners)
	out.HeldCount = len(out.Held)
	writeJSON(w, out)
}

type uiQuoteControlRequest struct {
	StrategyID string `json:"strategy_id"`
	Generation int64  `json:"generation,omitempty"`
	Hold       *bool  `json:"hold,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

func (ss *StatusServer) quoteControlTarget(w http.ResponseWriter, r *http.Request) (*quoteAdmission, uiQuoteControlRequest, bool) {
	var req uiQuoteControlRequest
	if ss.rejectIfDraining(w) || !ss.requireAPIAuth(w, r) {
		return nil, req, false
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return nil, req, false
	}
	if !requireSameOrigin(w, r) || !requireJSONContentType(w, r) {
		return nil, req, false
	}
	filter, ok := ss.uiPartitionParam(w, r)
	if !ok {
		return nil, req, false
	}
	if filter.All {
		writeJSONError(w, http.StatusBadRequest, "a quote control needs an explicit ?partition=")
		return nil, req, false
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid quote control request: "+err.Error())
		return nil, req, false
	}
	req.StrategyID = strings.TrimSpace(req.StrategyID)
	if req.StrategyID == "" {
		writeJSONError(w, http.StatusBadRequest, "strategy_id is required")
		return nil, req, false
	}
	if !ss.strategyInPartition(req.StrategyID, filter) {
		writeJSONError(w, http.StatusNotFound, fmt.Sprintf("strategy %s is not owned by partition %s", req.StrategyID, filter.Partition.String()))
		return nil, req, false
	}
	adm := quoteAdmissionForStore(ss.stateDB)
	if adm == nil || adm.sc.ID != req.StrategyID || adm.partition != filter.Partition {
		writeJSONError(w, http.StatusNotFound, fmt.Sprintf("no admitted quote owner %s runs in partition %s", req.StrategyID, filter.Partition.String()))
		return nil, req, false
	}
	return adm, req, true
}

func (ss *StatusServer) handleAPIQuoteCancel(w http.ResponseWriter, r *http.Request) {
	adm, req, ok := ss.quoteControlTarget(w, r)
	if !ok {
		return
	}
	if req.Generation <= 0 {
		writeJSONError(w, http.StatusBadRequest, "generation must be > 0")
		return
	}
	n, err := adm.OperatorCancel(req.Generation)
	if err != nil {
		writeJSONError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true, "strategy_id": req.StrategyID, "generation": req.Generation, "cancel_requested": n})
}

func (ss *StatusServer) handleAPIQuoteHold(w http.ResponseWriter, r *http.Request) {
	adm, req, ok := ss.quoteControlTarget(w, r)
	if !ok {
		return
	}
	if req.Hold == nil {
		writeJSONError(w, http.StatusBadRequest, "hold must be true or false")
		return
	}
	reason := strings.TrimSpace(req.Reason)
	if *req.Hold && reason == "" {
		reason = "operator request"
	}
	if err := adm.OperatorHold(*req.Hold, reason); err != nil {
		writeJSONError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true, "strategy_id": req.StrategyID, "hold": *req.Hold})
}
