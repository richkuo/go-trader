package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

const confirmNonceTTL = 60 * time.Second

var uiTradeActions = map[string]bool{
	"open":        true,
	"add":         true,
	"close":       true,
	"force-close": true,
	"update-sl":   true,
	"cancel-sl":   true,
}

func sortedUITradeActions() []string {
	out := make([]string, 0, len(uiTradeActions))
	for k := range uiTradeActions {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

type confirmNonceEntry struct {
	binding string

	payload string
	expires time.Time
}

func canonicalConfirmBinding(action, strategyID string, params json.RawMessage) (string, error) {
	obj := map[string]interface{}{}
	if len(params) > 0 {
		if err := json.Unmarshal(params, &obj); err != nil {
			return "", fmt.Errorf("params must be a JSON object")
		}
	}
	canon, err := json.Marshal(obj)
	if err != nil {
		return "", err
	}
	return action + "\x00" + strategyID + "\x00" + string(canon), nil
}

func (ss *StatusServer) issueConfirmNonce(binding, payload string, now time.Time) (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	nonce := hex.EncodeToString(buf)
	ss.confirmMu.Lock()
	defer ss.confirmMu.Unlock()
	if ss.confirmNonces == nil {
		ss.confirmNonces = map[string]confirmNonceEntry{}
	}
	for k, e := range ss.confirmNonces {
		if now.After(e.expires) {
			delete(ss.confirmNonces, k)
		}
	}
	ss.confirmNonces[nonce] = confirmNonceEntry{binding: binding, payload: payload, expires: now.Add(confirmNonceTTL)}
	return nonce, nil
}

func (ss *StatusServer) consumeConfirmNonce(nonce, binding string, now time.Time) (string, error) {
	ss.confirmMu.Lock()
	entry, ok := ss.confirmNonces[nonce]
	delete(ss.confirmNonces, nonce)
	ss.confirmMu.Unlock()
	if !ok {
		return "", fmt.Errorf("unknown or already-used confirmation — request a new confirmation")
	}
	if now.After(entry.expires) {
		return "", fmt.Errorf("confirmation expired — request a new confirmation")
	}
	if subtle.ConstantTimeCompare([]byte(entry.binding), []byte(binding)) != 1 {
		return "", fmt.Errorf("confirmation does not match this action — request a new confirmation")
	}
	return entry.payload, nil
}

type uiConfirmRequest struct {
	Action     string          `json:"action"`
	StrategyID string          `json:"strategy_id"`
	Params     json.RawMessage `json:"params"`
}

type uiConfirmResponse struct {
	Nonce            string `json:"nonce"`
	ExpiresInSeconds int    `json:"expires_in_seconds"`
	ConfirmPhrase    string `json:"confirm_phrase"`
	Description      string `json:"description"`
}

func uiConfirmDescription(action, strategyID string, params json.RawMessage) string {
	obj := map[string]interface{}{}
	_ = json.Unmarshal(params, &obj)
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", k, obj[k]))
	}
	desc := fmt.Sprintf("%s on %s", action, strategyID)
	if len(parts) > 0 {
		desc += " (" + strings.Join(parts, ", ") + ")"
	}
	return desc
}

func (ss *StatusServer) handleAPIConfirm(w http.ResponseWriter, r *http.Request) {
	if !ss.uiTradeActionGuards(w, r) {
		return
	}
	obj, ok := readUIMutationBody(w, r)
	if !ok {
		return
	}
	var req uiConfirmRequest
	if raw, present := obj["action"]; present {
		_ = json.Unmarshal(raw, &req.Action)
	}
	if raw, present := obj["strategy_id"]; present {
		_ = json.Unmarshal(raw, &req.StrategyID)
	}
	req.Params = obj["params"]

	structural := uiStructuralActions[req.Action]
	if !uiTradeActions[req.Action] && !structural {
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("unknown action %q (want one of %s)", req.Action,
			strings.Join(append(sortedUITradeActions(), sortedUIStructuralActions()...), ", ")))
		return
	}

	if strings.TrimSpace(req.StrategyID) == "" && req.Action != "add-strategy" {
		writeJSONError(w, http.StatusBadRequest, "strategy_id is required")
		return
	}

	phrase := req.StrategyID
	description := uiConfirmDescription(req.Action, req.StrategyID, req.Params)
	payload := ""
	if structural {
		if strings.TrimSpace(ss.configPath) == "" {
			writeJSONError(w, http.StatusServiceUnavailable, "config path not configured")
			return
		}
		var serr error
		phrase, description, payload, serr = ss.confirmStructuralAction(req.Action, req.StrategyID, req.Params)
		if serr != nil {
			writeJSONError(w, http.StatusBadRequest, serr.Error())
			return
		}
	} else {
		cfg := ss.uiTradeConfig()
		if cfg == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "config not available")
			return
		}

		var lookupErr error
		if req.Action == "force-close" {
			_, _, lookupErr = lookupForceCloseStrategy(cfg, req.StrategyID)
		} else {
			_, lookupErr = lookupManualStrategy(cfg, req.StrategyID)
		}
		if lookupErr != nil {
			writeJSONError(w, http.StatusBadRequest, lookupErr.Error())
			return
		}
	}
	binding, err := canonicalConfirmBinding(req.Action, req.StrategyID, req.Params)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	nonce, err := ss.issueConfirmNonce(binding, payload, time.Now())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "could not issue confirmation")
		return
	}
	writeJSON(w, uiConfirmResponse{
		Nonce:            nonce,
		ExpiresInSeconds: int(confirmNonceTTL / time.Second),
		ConfirmPhrase:    phrase,
		Description:      description,
	})
}
