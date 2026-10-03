package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

var uiStructuralActions = map[string]bool{
	"add-strategy":      true,
	"remove-strategy":   true,
	"paper-to-live":     true,
	"apply-regime-gate": true,
}

func sortedUIStructuralActions() []string {
	out := make([]string, 0, len(uiStructuralActions))
	for k := range uiStructuralActions {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (ss *StatusServer) mutateConfigRoot(fn func(root map[string]json.RawMessage) error) error {
	ss.configWriteMu.Lock()
	defer ss.configWriteMu.Unlock()
	raw, err := os.ReadFile(ss.configPath)
	if err != nil {
		return err
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		return fmt.Errorf("parse config: %w", err)
	}
	if err := fn(root); err != nil {
		return err
	}
	return writeValidatedConfigRoot(ss.configPath, root)
}

type structuralParams struct {
	Name     string `json:"name"`
	Platform string `json:"platform"`
	Asset    string `json:"asset"`
	Gate     string `json:"gate"`
	Restart  bool   `json:"restart"`
}

func decodeStructuralParams(raw json.RawMessage) (structuralParams, error) {
	var p structuralParams
	if len(raw) == 0 {
		return p, nil
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return p, fmt.Errorf("params must be a JSON object")
	}
	return p, nil
}

func structuralApplyMessage(restarting bool) string {
	if restarting {
		return "Applying via service restart — the daemon briefly goes offline; the new instance resumes the cycle."
	}
	return "This is a restart-required change: the config is written and validated, but the running daemon keeps its current strategy set until you restart go-trader (systemctl restart go-trader)."
}

func (ss *StatusServer) maybeRestart(restart bool) {
	if !restart {
		return
	}
	fn := ss.restartFn
	if fn == nil {
		fn = restartSelf
	}
	go func() { _ = fn() }()
}

func isOnlyStrategyOnDisk(path, id string) bool {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(raw, &root) != nil {
		return false
	}
	list, err := configStrategies(root)
	if err != nil || len(list) != 1 {
		return false
	}
	return strategyRawID(list[0]) == id
}

func (ss *StatusServer) confirmStructuralAction(action, strategyID string, params json.RawMessage) (phrase, description, payload string, err error) {
	p, perr := decodeStructuralParams(params)
	if perr != nil {
		return "", "", "", perr
	}
	switch action {
	case "add-strategy":
		newID, _, berr := buildAddStrategyEntry(p.Name, p.Platform, p.Asset)
		if berr != nil {
			return "", "", "", berr
		}
		if _, exists := ss.strategyConfig(newID); exists {
			return "", "", "", fmt.Errorf("strategy %q already exists", newID)
		}
		desc := fmt.Sprintf("add-strategy: create %s (%s on %s, asset %s) in PAPER mode. %s",
			newID, p.Name, p.Platform, strings.ToUpper(strings.TrimSpace(p.Asset)), structuralApplyMessage(p.Restart))
		return newID, desc, "", nil
	case "remove-strategy":
		sc, ok := ss.strategyConfig(strategyID)
		if !ok {
			return "", "", "", fmt.Errorf("strategy %q not found", strategyID)
		}
		if isOnlyStrategyOnDisk(ss.configPath, sc.ID) {
			return "", "", "", fmt.Errorf("refusing to remove the only strategy %q — a config must keep at least one strategy", sc.ID)
		}
		desc := fmt.Sprintf("remove-strategy: delete %s from the config. It stops trading after restart; its positions and trade history in the state DB are NOT touched.", sc.ID)
		if ss.strategyHasOpenPosition(strategyID) {
			desc += " ⚠️ This strategy currently holds an OPEN position — removing it stops all management (trailing SL sync, close evaluation) for that position after restart; flatten first unless you intend to manage it manually."
		}
		desc += " " + structuralApplyMessage(p.Restart)
		return sc.ID, desc, "", nil
	case "paper-to-live":
		sc, ok := ss.strategyConfig(strategyID)
		if !ok {
			return "", "", "", fmt.Errorf("strategy %q not found", strategyID)
		}
		hasPaper, hasLive := false, false
		for _, a := range sc.Args {
			switch strings.TrimSpace(a) {
			case "--mode=paper":
				hasPaper = true
			case "--mode=live":
				hasLive = true
			}
		}
		if !hasPaper {
			if hasLive {
				return "", "", "", fmt.Errorf("strategy %q is already live", strategyID)
			}
			return "", "", "", fmt.Errorf("strategy %q has no --mode=paper arg to flip (only perps/futures-style strategies support paper→live)", strategyID)
		}
		if reason := ss.paperToLiveBlockedReason(strategyID, false); reason != "" {
			return "", "", "", fmt.Errorf("%s", reason)
		}
		desc := fmt.Sprintf("paper-to-live: ⚠️ switch %s from PAPER to LIVE — after restart it places REAL ORDERS WITH REAL FUNDS. %s", sc.ID, structuralApplyMessage(p.Restart))
		return sc.ID, desc, "", nil
	case "apply-regime-gate":
		preset, target, alsoActivated, gerr := ss.regimeGateConfirmContext(strategyID, p.Gate)
		if gerr != nil {
			return "", "", "", gerr
		}
		desc := fmt.Sprintf("apply-regime-gate: wire gate %s (%s) onto %s — sets regime_gate_window=%s, allowed_regimes=%v, enables regime detection, and adds the %s window if missing. Only ENTRIES outside the allowed regime are blocked; closes/management always run.",
			preset.Name, preset.Label, target.ID, preset.WindowKey, preset.AllowedRegimes, preset.WindowKey)
		if len(alsoActivated) > 0 {
			desc += fmt.Sprintf(" ⚠️ Enabling regime detection also activates previously-dormant allowed_regimes entry-gates on %d other strategy(ies) you did NOT select: %s.",
				len(alsoActivated), strings.Join(alsoActivated, ", "))
		}
		desc += " " + structuralApplyMessage(p.Restart)
		pb, merr := json.Marshal(alsoActivated)
		if merr != nil {
			return "", "", "", merr
		}
		return target.ID, desc, string(pb), nil
	}
	return "", "", "", fmt.Errorf("unknown structural action %q", action)
}

func (ss *StatusServer) regimeGateConfirmContext(strategyID, gateName string) (regimeGatePreset, StrategyConfig, []string, error) {
	if strings.TrimSpace(gateName) == "" {
		gateName = defaultRegimeGatePresetName
	}
	preset, ok := regimeGatePresetByName(gateName)
	if !ok {
		return regimeGatePreset{}, StrategyConfig{}, nil, fmt.Errorf("unknown gate %q (available: %s)", gateName, strings.Join(regimeGatePresetNames(), ", "))
	}
	sc, ok := ss.strategyConfig(strategyID)
	if !ok {
		return regimeGatePreset{}, StrategyConfig{}, nil, fmt.Errorf("strategy %q not found", strategyID)
	}
	if !strategyEligibleForRegimeGate(sc, preset) {
		return regimeGatePreset{}, StrategyConfig{}, nil, fmt.Errorf("strategy %q is type %q, not eligible for gate %q (eligible: %s)",
			sc.ID, sc.Type, preset.Name, strings.Join(preset.EligibleTypes, ", "))
	}
	if ss.strategyHasOpenPosition(sc.ID) {
		return regimeGatePreset{}, StrategyConfig{}, nil, fmt.Errorf("strategy %q has an open position — a regime entry-gate can only be wired while the strategy is flat (it would otherwise rebind the open position's stamped regime semantics); flatten it first", sc.ID)
	}
	var alsoActivated []string
	if raw, rerr := os.ReadFile(ss.configPath); rerr == nil {
		var root map[string]json.RawMessage
		if json.Unmarshal(raw, &root) == nil {
			alsoActivated, _ = regimeGateSideEffectStrategies(root, sc.ID)
		}
	}
	return preset, sc, alsoActivated, nil
}

func (ss *StatusServer) uiStructuralGuards(w http.ResponseWriter, r *http.Request) bool {
	if ss.rejectIfDraining(w) {
		return false
	}
	return ss.uiMutationGuards(w, r)
}

func (ss *StatusServer) readStructuralRequest(w http.ResponseWriter, r *http.Request, action, strategyID string) (structuralParams, string, bool) {
	obj, ok := readUIMutationBody(w, r)
	if !ok {
		return structuralParams{}, "", false
	}
	var nonce string
	if raw, present := obj["nonce"]; present {
		_ = json.Unmarshal(raw, &nonce)
	}
	rawParams := obj["params"]
	if nonce == "" {
		writeJSONError(w, http.StatusBadRequest, "nonce is required — call POST /api/confirm first")
		return structuralParams{}, "", false
	}
	binding, err := canonicalConfirmBinding(action, strategyID, rawParams)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return structuralParams{}, "", false
	}
	payload, err := ss.consumeConfirmNonce(nonce, binding, time.Now())
	if err != nil {
		writeJSONError(w, http.StatusForbidden, err.Error())
		return structuralParams{}, "", false
	}
	p, perr := decodeStructuralParams(rawParams)
	if perr != nil {
		writeJSONError(w, http.StatusBadRequest, perr.Error())
		return structuralParams{}, "", false
	}
	return p, payload, true
}

func (ss *StatusServer) handleAPIAddStrategy(w http.ResponseWriter, r *http.Request) {
	if !ss.uiStructuralGuards(w, r) {
		return
	}
	p, _, ok := ss.readStructuralRequest(w, r, "add-strategy", "")
	if !ok {
		return
	}
	var newID string
	err := ss.mutateConfigRoot(func(root map[string]json.RawMessage) error {
		id, e := addStrategyToRoot(root, p.Name, p.Platform, p.Asset)
		newID = id
		return e
	})
	if err != nil {
		writeJSONError(w, http.StatusConflict, "add-strategy failed: "+err.Error())
		return
	}
	writeJSON(w, uiMutationResponse{OK: true, Message: fmt.Sprintf("Added strategy %s (paper mode). %s", newID, structuralApplyMessage(p.Restart))})
	ss.maybeRestart(p.Restart)
}

func (ss *StatusServer) handleAPIStrategyStructural(w http.ResponseWriter, r *http.Request, id, action string) {
	if !ss.uiStructuralGuards(w, r) {
		return
	}
	p, payload, ok := ss.readStructuralRequest(w, r, action, id)
	if !ok {
		return
	}
	var msg string
	var err error
	switch action {
	case "remove-strategy":
		err = ss.mutateConfigRoot(func(root map[string]json.RawMessage) error {
			return removeStrategyFromRoot(root, id)
		})
		msg = fmt.Sprintf("Removed strategy %s. Its positions and trade history in the state DB are not touched.", id)
	case "paper-to-live":
		msg, err = ss.executePaperToLive(id)
	case "apply-regime-gate":
		msg, err = ss.executeApplyRegimeGate(id, p, payload)
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusConflict, action+" failed: "+err.Error())
		return
	}
	writeJSON(w, uiMutationResponse{OK: true, Message: msg + " " + structuralApplyMessage(p.Restart)})
	ss.maybeRestart(p.Restart)
}

func (ss *StatusServer) paperToLiveBlockedReason(id string, afterConfirm bool) string {
	if !ss.strategyHasOpenPosition(id) {
		return ""
	}
	if afterConfirm {
		return fmt.Sprintf("strategy %q opened a position after the confirmation was issued — paper→live can only run while flat; nothing changed", id)
	}
	return fmt.Sprintf("strategy %q holds an OPEN position — paper→live can only run while flat: a simulated paper position has no on-chain backing, so carried into a real-funds live strategy it becomes a phantom the account reconcile flags as a gap and the strategy would mismanage; flatten it first", id)
}

func (ss *StatusServer) executePaperToLive(id string) (string, error) {
	if reason := ss.paperToLiveBlockedReason(id, true); reason != "" {
		return "", fmt.Errorf("%s", reason)
	}
	var after []string
	err := ss.mutateConfigRoot(func(root map[string]json.RawMessage) error {
		_, a, e := flipStrategyToLive(root, id)
		after = a
		return e
	})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Strategy %s switched to LIVE (args: %s).", id, strings.Join(after, " ")), nil
}

func (ss *StatusServer) executeApplyRegimeGate(id string, p structuralParams, payload string) (string, error) {
	gateName := strings.TrimSpace(p.Gate)
	if gateName == "" {
		gateName = defaultRegimeGatePresetName
	}
	preset, ok := regimeGatePresetByName(gateName)
	if !ok {
		return "", fmt.Errorf("unknown gate %q (available: %s)", gateName, strings.Join(regimeGatePresetNames(), ", "))
	}
	var shown []string
	if payload != "" {
		if err := json.Unmarshal([]byte(payload), &shown); err != nil {
			return "", fmt.Errorf("confirmation payload corrupt — request a new confirmation")
		}
	}
	if ss.strategyHasOpenPosition(id) {
		return "", fmt.Errorf("strategy %q opened a position after the confirmation was issued — cannot gate while open; nothing changed", id)
	}
	err := ss.mutateConfigRoot(func(root map[string]json.RawMessage) error {
		fresh, ferr := regimeGateSideEffectStrategies(root, id)
		if ferr != nil {
			return ferr
		}
		if grew := regimeGateBlastRadiusGrew(fresh, shown); len(grew) > 0 {
			return fmt.Errorf("blast radius changed since the confirmation was issued — strategy(ies) %s would now also be newly activated, which you were not shown; nothing changed — request a new confirmation to review the current set", strings.Join(grew, ", "))
		}
		return applyRegimeGateToRoot(root, id, preset)
	})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Wired regime gate %s onto strategy %s (regime_gate_window=%s, allowed_regimes=%v).",
		preset.Name, id, preset.WindowKey, preset.AllowedRegimes), nil
}
