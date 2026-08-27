package main


import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"
)

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

func (ss *StatusServer) uiTradeConfig() *Config {
	ss.strategiesMu.RLock()
	defer ss.strategiesMu.RUnlock()
	return ss.uiCfg
}

func (ss *StatusServer) SetNotifier(notifier *MultiNotifier) {
	if ss == nil {
		return
	}
	ss.strategiesMu.Lock()
	ss.uiNotifier = notifier
	ss.strategiesMu.Unlock()
}

func (ss *StatusServer) daemonManualCoreDeps(cfg *Config) manualCoreDeps {
	ss.strategiesMu.RLock()
	notifier := ss.uiNotifier
	ss.strategiesMu.RUnlock()
	d := newManualCoreDeps(cfg, ss.stateDB, notifier)
	d.loadState = func(strategyID, symbol string) (manualStateView, error) {
		ss.mu.RLock()
		defer ss.mu.RUnlock()
		return manualStateViewFromState(cfg, ss.state, strategyID, symbol), nil
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

func uiTradeActionHTTPStatus(err error) int {
	if ce, ok := err.(*manualCoreError); ok && ce.usage {
		return http.StatusBadRequest
	}
	return http.StatusConflict
}

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
	if _, err := ss.consumeConfirmNonce(req.Nonce, binding, time.Now()); err != nil {
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

	ss.tradeActionMu.Lock()
	defer ss.tradeActionMu.Unlock()

	guardSym := sc.Symbol
	if action == "force-close" {
		guardSym = sym
	}
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
