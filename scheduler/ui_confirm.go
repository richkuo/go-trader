package main

// #1257 (Phase 4 of #1229): server-issued confirm nonces for dashboard
// trade-affecting mutations. POST /api/confirm issues a short-lived,
// single-use nonce bound to the exact action (action name + strategy id +
// canonicalized params). The subsequent mutating POST must present the nonce;
// the server verifies the binding, expiry, and single-use (the nonce is
// deleted on lookup, even when the action then fails). This guards misclicks
// and stale dialogs, not attackers — same-origin + optional token remain the
// security boundary per the #1229 model.

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

// confirmNonceTTL bounds how long a typed-confirmation dialog can sit open
// before the nonce (and therefore the action it described) goes stale.
const confirmNonceTTL = 60 * time.Second

// uiTradeActions is the closed set of confirmable dashboard trade actions.
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
	expires time.Time
}

// canonicalConfirmBinding derives the exact-action binding string a nonce is
// tied to. Params are canonicalized by decoding to a generic object and
// re-marshaling (encoding/json sorts object keys), so key order on the wire
// never matters — but any value difference does.
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

// issueConfirmNonce mints a crypto/rand nonce bound to binding, storing it
// in-memory with a TTL. Expired entries are swept opportunistically so the
// map cannot grow unbounded.
func (ss *StatusServer) issueConfirmNonce(binding string, now time.Time) (string, error) {
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
	ss.confirmNonces[nonce] = confirmNonceEntry{binding: binding, expires: now.Add(confirmNonceTTL)}
	return nonce, nil
}

// consumeConfirmNonce validates and burns a nonce. Single-use is
// unconditional: the entry is deleted on lookup even when validation then
// fails, so a rejected attempt can never retry with the same nonce.
func (ss *StatusServer) consumeConfirmNonce(nonce, binding string, now time.Time) error {
	ss.confirmMu.Lock()
	entry, ok := ss.confirmNonces[nonce]
	delete(ss.confirmNonces, nonce)
	ss.confirmMu.Unlock()
	if !ok {
		return fmt.Errorf("unknown or already-used confirmation — request a new confirmation")
	}
	if now.After(entry.expires) {
		return fmt.Errorf("confirmation expired — request a new confirmation")
	}
	if subtle.ConstantTimeCompare([]byte(entry.binding), []byte(binding)) != 1 {
		return fmt.Errorf("confirmation does not match this action — request a new confirmation")
	}
	return nil
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

// uiConfirmDescription renders the server-authoritative action summary shown
// in the typed-confirmation dialog. Params are echoed key-sorted so the
// operator confirms exactly what the server will execute.
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

// handleAPIConfirm implements POST /api/confirm. It validates the action is a
// known trade action targeting a strategy this action can apply to, then
// issues the bound nonce. Eligibility is re-checked by the action endpoint —
// the check here only exists to fail the dialog early with a clear message.
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

	if !uiTradeActions[req.Action] {
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("unknown action %q (want one of %s)", req.Action, strings.Join(sortedUITradeActions(), ", ")))
		return
	}
	if strings.TrimSpace(req.StrategyID) == "" {
		writeJSONError(w, http.StatusBadRequest, "strategy_id is required")
		return
	}
	cfg := ss.uiTradeConfig()
	if cfg == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "config not available")
		return
	}
	// Early eligibility check so the dialog fails fast with the real reason.
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
	binding, err := canonicalConfirmBinding(req.Action, req.StrategyID, req.Params)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	nonce, err := ss.issueConfirmNonce(binding, time.Now())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "could not issue confirmation")
		return
	}
	writeJSON(w, uiConfirmResponse{
		Nonce:            nonce,
		ExpiresInSeconds: int(confirmNonceTTL / time.Second),
		ConfirmPhrase:    req.StrategyID,
		Description:      uiConfirmDescription(req.Action, req.StrategyID, req.Params),
	})
}
