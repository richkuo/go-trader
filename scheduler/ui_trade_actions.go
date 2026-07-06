package main

// #1257 (Phase 4 of #1229): dashboard trade-affecting mutations. Each
// endpoint runs the SAME core as the corresponding manual CLI command
// (manual_core.go) — zero guard bypass — behind the Phase-3 preamble
// (POST-only, requireMutatingAPIAuth, JSON content type, requireSameOrigin)
// plus the #1257 confirm-nonce check (ui_confirm.go).
//
// Endpoints (all POST, body {"nonce": "...", "params": {...}}):
//
//	/api/strategies/{id}/open        — manual market open (type=manual)
//	/api/strategies/{id}/add         — manual scale-in (#873)
//	/api/strategies/{id}/close       — manual close, full or partial (params.qty)
//	/api/strategies/{id}/force-close — live HL perps only, reduce-only (#1140)
//	/api/strategies/{id}/update-sl   — cancel-then-queue SL move (#1050)
//	/api/strategies/{id}/cancel-sl   — SL removal (#1050)
//
// The handler snapshots daemon state under ss.mu.RLock and releases it before
// the core spawns any subprocess (6-phase lock pattern); results report what
// was submitted/queued for the scheduler drain, never an optimistic apply.

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"
)

// pendingManualActionExists reports whether any action of the given kinds
// for strategyID+symbol is still queued in pending_manual_actions (submitted
// on-chain but not yet drained/adopted by the scheduler). Mirrors
// pendingSLActionExists (#1050) for the trade-action classes.
func pendingManualActionExists(stateDB *StateDB, strategyID, symbol string, kinds ...string) (bool, error) {
	actions, err := stateDB.LoadPendingManualActions()
	if err != nil {
		return false, err
	}
	for _, a := range actions {
		if a.StrategyID != strategyID || !strings.EqualFold(a.Symbol, symbol) {
			continue
		}
		for _, k := range kinds {
			if a.Action == k {
				return true, nil
			}
		}
	}
	return false, nil
}

// uiTradeActionGuards is the shared preamble for /api/confirm and every trade
// action endpoint: not draining, POST-only, mutating auth, JSON content type,
// same-origin.
func (ss *StatusServer) uiTradeActionGuards(w http.ResponseWriter, r *http.Request) bool {
	if ss.rejectIfDraining(w) {
		return false
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return false
	}
	if !ss.requireMutatingAPIAuth(w, r) {
		return false
	}
	if !requireJSONContentType(w, r) {
		return false
	}
	if !requireSameOrigin(w, r) {
		return false
	}
	return true
}

// uiTradeConfig returns the live *Config snapshot (refreshed on SIGHUP via
// SetConfigContext).
func (ss *StatusServer) uiTradeConfig() *Config {
	ss.strategiesMu.RLock()
	defer ss.strategiesMu.RUnlock()
	return ss.uiCfg
}

// SetNotifier wires the daemon's notifier into the UI trade-action cores so
// their warning paths (naked-position alerts, cleanup failures) reach the
// operator exactly like the CLI's do.
func (ss *StatusServer) SetNotifier(notifier *MultiNotifier) {
	if ss == nil {
		return
	}
	ss.strategiesMu.Lock()
	ss.uiNotifier = notifier
	ss.strategiesMu.Unlock()
}

// daemonManualCoreDeps builds core deps for in-daemon execution: state comes
// from the live in-memory AppState (snapshot under ss.mu.RLock, released
// before subprocess work), the queue insert goes to the daemon's own stateDB
// handle, and on-chain side effects ride the existing RunHyperliquid*
// helpers (runPythonSideEffect lane).
func (ss *StatusServer) daemonManualCoreDeps(cfg *Config) manualCoreDeps {
	ss.strategiesMu.RLock()
	notifier := ss.uiNotifier
	ss.strategiesMu.RUnlock()
	d := newManualCoreDeps(cfg, ss.stateDB, notifier)
	d.loadState = func(strategyID, symbol string) (manualStateView, error) {
		ss.mu.RLock()
		defer ss.mu.RUnlock()
		return manualStateViewFromState(ss.state, strategyID, symbol), nil
	}
	return d
}

type uiTradeActionRequest struct {
	Nonce  string          `json:"nonce"`
	Params json.RawMessage `json:"params"`
}

type uiTradeActionResponse struct {
	OK      bool   `json:"ok"`
	Queued  bool   `json:"queued"`
	Message string `json:"message"`
}

// uiTradeActionHTTPStatus maps a core failure to an HTTP status: usage-class
// errors are the client's bad input (400), everything else is a guard refusal
// or venue failure (409).
func uiTradeActionHTTPStatus(err error) int {
	if ce, ok := err.(*manualCoreError); ok && ce.usage {
		return http.StatusBadRequest
	}
	return http.StatusConflict
}

// uiTradeParams accumulates typed extraction of optional params, keeping the
// first error (numbers must be non-negative and finite — a negative size
// would otherwise fall through countSizingFlags into the default-margin path).
type uiTradeParams struct {
	obj map[string]json.RawMessage
	err error
}

func (p *uiTradeParams) num(key string) float64 {
	raw, present := p.obj[key]
	if !present || p.err != nil {
		return 0
	}
	var v float64
	if err := json.Unmarshal(raw, &v); err != nil {
		p.err = fmt.Errorf("%s must be a number", key)
		return 0
	}
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		p.err = fmt.Errorf("%s must be a non-negative number", key)
		return 0
	}
	return v
}

func (p *uiTradeParams) str(key string) string {
	raw, present := p.obj[key]
	if !present || p.err != nil {
		return ""
	}
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		p.err = fmt.Errorf("%s must be a string", key)
		return ""
	}
	return v
}

// handleAPIStrategyTradeAction dispatches one confirmed trade action for a
// strategy. The nonce is consumed (single-use) BEFORE any validation deeper
// than the binding itself, so a failed attempt always needs a fresh confirm.
func (ss *StatusServer) handleAPIStrategyTradeAction(w http.ResponseWriter, r *http.Request, id, action string) {
	if !ss.uiTradeActionGuards(w, r) {
		return
	}
	obj, ok := readUIMutationBody(w, r)
	if !ok {
		return
	}
	var req uiTradeActionRequest
	if raw, present := obj["nonce"]; present {
		_ = json.Unmarshal(raw, &req.Nonce)
	}
	req.Params = obj["params"]
	if req.Nonce == "" {
		writeJSONError(w, http.StatusBadRequest, "nonce is required — call POST /api/confirm first")
		return
	}
	binding, err := canonicalConfirmBinding(action, id, req.Params)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := ss.consumeConfirmNonce(req.Nonce, binding, time.Now()); err != nil {
		writeJSONError(w, http.StatusForbidden, err.Error())
		return
	}

	var params map[string]json.RawMessage
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &params); err != nil {
			writeJSONError(w, http.StatusBadRequest, "params must be a JSON object")
			return
		}
	}

	cfg := ss.uiTradeConfig()
	if cfg == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "config not available")
		return
	}
	if ss.stateDB == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "state db not available")
		return
	}
	deps := ss.daemonManualCoreDeps(cfg)
	if ss.tradeDepsHook != nil {
		ss.tradeDepsHook(&deps)
	}

	var sc StrategyConfig
	var sym string
	var lookupErr error
	if action == "force-close" {
		sc, sym, lookupErr = lookupForceCloseStrategy(cfg, id)
	} else {
		sc, lookupErr = lookupManualStrategy(cfg, id)
	}
	if lookupErr != nil {
		writeJSONError(w, http.StatusBadRequest, lookupErr.Error())
		return
	}

	// Serialize trade-action submits: the double-fire guard below is a
	// check-then-submit, so without this two concurrent requests could both
	// observe no-pending/Flat and both fire on-chain. Held across the guard
	// AND the core (which inserts the pending row), making check+insert
	// atomic; /api/confirm and read paths never take it.
	ss.tradeActionMu.Lock()
	defer ss.tradeActionMu.Unlock()

	// Double-fire guard. The shared cores repeat this refusal
	// (refuseIfPositionActionQueued in manual_core.go) so the CLI and any future
	// core caller are covered too; running it HERE additionally makes the
	// check+submit atomic under tradeActionMu for concurrent HTTP requests (the
	// cores alone are not atomic without this lock) and lets the open branch add
	// the UI-only "already holds the symbol" refusal below (the CLI open
	// deliberately allows --record-only re-register onto an existing position).
	// Between an action submitting on-chain and the scheduler draining its
	// queued row, the dashboard still shows the pre-action state, inviting a
	// retry that would double the position (open/add) or — since a sized manual
	// close is a regular non-reduce-only order — close the remainder AND flip
	// into an opposite position (close). Refuse an open while this strategy
	// already holds the symbol or an open/add is queued; refuse an add while one
	// is queued; refuse a close/force-close while a close is queued. A peer
	// strategy's position on the same coin does not block (the view is scoped to
	// this strategy's own positions), and once the drain applies + deletes the
	// row a legitimate retry passes again.
	// The guard must key on the SAME symbol the core writes into the queued
	// row: perps configs leave the symbol config field empty (the coin lives
	// in args[1]) and forceCloseCore queues under the args-derived sym, so
	// sc.Symbol would never match a queued force-close row.
	guardSym := sc.Symbol
	if action == "force-close" {
		guardSym = sym
	}
	// All four position-changing actions share ONE in-flight class: an add
	// fired while a close is queued (or vice versa) would pass a class-scoped
	// guard, fire a real order, and orphan it on drain (the close applies
	// first, deletes the position, then the add row fails every cycle). At
	// most one un-drained open/add/close per strategy+symbol.
	if action == "open" || action == "add" || action == "close" || action == "force-close" {
		if pending, perr := pendingManualActionExists(ss.stateDB, id, guardSym, "open", "add", "close"); perr != nil {
			writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("could not check pending actions: %v", perr))
			return
		} else if pending {
			writeJSONError(w, http.StatusConflict, "a position-changing action (open/add/close) for this strategy is already submitted and awaiting the scheduler's next cycle — refresh after it applies before retrying")
			return
		}
		if action == "open" {
			if view, verr := deps.loadState(id, sc.Symbol); verr == nil && view.Pos != nil {
				writeJSONError(w, http.StatusConflict, fmt.Sprintf("strategy already holds an open %s position — use add or close instead", sc.Symbol))
				return
			}
		}
	}

	p := &uiTradeParams{obj: params}
	var res *manualCoreResult
	var coreErr error
	switch action {
	case "open":
		in := manualOpenInputs{
			StrategyID: id,
			Side:       p.str("side"),
			Size:       p.num("size"),
			Notional:   p.num("notional"),
			Margin:     p.num("margin"),
			ATR:        p.num("atr"),
			SLATRMult:  p.num("sl_atr_mult"),
			SLPct:      p.num("sl_pct"),
		}
		if p.err != nil {
			writeJSONError(w, http.StatusBadRequest, p.err.Error())
			return
		}
		res, coreErr = manualOpenCore(deps, sc, in)
	case "add":
		in := manualAddInputs{
			StrategyID: id,
			Size:       p.num("size"),
			Notional:   p.num("notional"),
			Margin:     p.num("margin"),
		}
		if p.err != nil {
			writeJSONError(w, http.StatusBadRequest, p.err.Error())
			return
		}
		res, coreErr = manualAddCore(deps, sc, in)
	case "close":
		qty := p.num("qty")
		if p.err != nil {
			writeJSONError(w, http.StatusBadRequest, p.err.Error())
			return
		}
		res, coreErr = manualCloseCore(deps, sc, manualCloseInputs{StrategyID: id, Qty: qty})
	case "force-close":
		qty := p.num("qty")
		if p.err != nil {
			writeJSONError(w, http.StatusBadRequest, p.err.Error())
			return
		}
		res, coreErr = forceCloseCore(deps, sc, sym, forceCloseInputs{StrategyID: id, Qty: qty})
	case "update-sl":
		in := manualSLInputs{StrategyID: id, Symbol: p.str("symbol"), Trigger: p.num("trigger")}
		if p.err != nil {
			writeJSONError(w, http.StatusBadRequest, p.err.Error())
			return
		}
		res, coreErr = manualUpdateSLCore(deps, sc, in)
	case "cancel-sl":
		in := manualSLInputs{StrategyID: id, Symbol: p.str("symbol")}
		if p.err != nil {
			writeJSONError(w, http.StatusBadRequest, p.err.Error())
			return
		}
		res, coreErr = manualCancelSLCore(deps, sc, in)
	default:
		http.NotFound(w, r)
		return
	}

	if coreErr != nil {
		msg := coreErr.Error()
		if res != nil {
			if ctx := res.uiMessage(); ctx != "" {
				msg = ctx + "\n" + msg
			}
		}
		writeJSONError(w, uiTradeActionHTTPStatus(coreErr), msg)
		return
	}
	writeJSON(w, uiTradeActionResponse{OK: true, Queued: res.queued, Message: res.uiMessage()})
}
