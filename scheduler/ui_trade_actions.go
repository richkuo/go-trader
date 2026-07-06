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
	"time"
)

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
