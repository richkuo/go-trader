package main

// #1256 (Phase 3 of #1229): low-risk dashboard mutations — per-strategy
// pause/unpause, per-strategy and global notify_ratchet_triggers toggles.
//
// Security model (#1229): loopback-only bind + requireSameOrigin on every
// POST; status_token enforced only when configured (requireMutatingAPIAuth).
// Every config write goes through the guarded paths shared with the tuner and
// Discord — applyStrategyConfigPatch / writeValidatedConfigRoot on
// configWriteMu — then signals a SIGHUP hot-reload (both fields are
// hot-reloadable always, including while a position is open: #1150/#1118).

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

type uiMutationResponse struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}

// uiMutationGuards runs the shared preamble for every #1256 mutating
// endpoint: POST-only, mutating auth, JSON content type, same-origin, and a
// wired config path. Returns false after writing the error response.
func (ss *StatusServer) uiMutationGuards(w http.ResponseWriter, r *http.Request) bool {
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
	if strings.TrimSpace(ss.configPath) == "" {
		writeJSONError(w, http.StatusServiceUnavailable, "config path not configured")
		return false
	}
	return true
}

// readUIMutationBody reads and parses a small JSON object body.
func readUIMutationBody(w http.ResponseWriter, r *http.Request) (map[string]json.RawMessage, bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "failed to read body")
		return nil, false
	}
	if len(body) == 0 {
		writeJSONError(w, http.StatusBadRequest, "empty body")
		return nil, false
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid json body")
		return nil, false
	}
	return obj, true
}

// triggerConfigReload signals the SIGHUP hot-reload path after a successful
// config write and returns the operator-facing apply message. The write has
// already landed; a failed signal only means the change waits for a manual
// SIGHUP or restart, so it degrades to a warning instead of an error.
func (ss *StatusServer) triggerConfigReload() string {
	if ss.reloadConfig == nil {
		return "Config written. Send SIGHUP or restart to apply."
	}
	if err := ss.reloadConfig(); err != nil {
		return "Config written, but signaling the reload failed: " + err.Error() + " — send SIGHUP or restart manually."
	}
	return "Applied via SIGHUP hot-reload; effective next cycle."
}

// applyUIStrategyOverrides funnels a #1256 override set through the same
// guarded patch path as the tuner (applyStrategyConfigPatch on configWriteMu)
// and then signals the hot-reload. Returns the apply message.
func (ss *StatusServer) applyUIStrategyOverrides(w http.ResponseWriter, id string, overrides map[string]json.RawMessage) (string, bool) {
	sc, ok := ss.strategyConfig(id)
	if !ok {
		writeJSONError(w, http.StatusNotFound, "strategy not found")
		return "", false
	}
	merged, err := mergeStrategyTunerOverrides(sc, overrides)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return "", false
	}
	hasOpen := ss.strategyHasOpenPosition(id)
	ss.configWriteMu.Lock()
	_, err = applyStrategyConfigPatch(ss.configPath, id, merged, overrides, hasOpen)
	ss.configWriteMu.Unlock()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return "", false
	}
	return ss.triggerConfigReload(), true
}

// handleAPIStrategyPause handles POST /api/strategies/{id}/pause with body
// {"paused": true|false}. Pause is hot-reloadable always, including while a
// position is open (#1150) — pausing never strands protection, it only holds
// position-increasing signals from the next cycle.
func (ss *StatusServer) handleAPIStrategyPause(w http.ResponseWriter, r *http.Request, id string) {
	if !ss.uiMutationGuards(w, r) {
		return
	}
	obj, ok := readUIMutationBody(w, r)
	if !ok {
		return
	}
	raw, present := obj["paused"]
	if !present {
		writeJSONError(w, http.StatusBadRequest, `body must include "paused": true|false`)
		return
	}
	var paused bool
	if err := json.Unmarshal(raw, &paused); err != nil {
		writeJSONError(w, http.StatusBadRequest, "paused must be true or false")
		return
	}
	msg, ok := ss.applyUIStrategyOverrides(w, id, map[string]json.RawMessage{"paused": raw})
	if !ok {
		return
	}
	verb := "resumed"
	if paused {
		verb = "paused"
	}
	writeJSON(w, uiMutationResponse{OK: true, Message: fmt.Sprintf("Strategy %s %s. %s", id, verb, msg)})
}

// handleAPIStrategyNotifications handles POST
// /api/strategies/{id}/notifications with body
// {"notify_ratchet_triggers": true|false|null} — null clears the per-strategy
// override so the strategy inherits the global default (#1118).
func (ss *StatusServer) handleAPIStrategyNotifications(w http.ResponseWriter, r *http.Request, id string) {
	if !ss.uiMutationGuards(w, r) {
		return
	}
	obj, ok := readUIMutationBody(w, r)
	if !ok {
		return
	}
	raw, present := obj["notify_ratchet_triggers"]
	if !present {
		writeJSONError(w, http.StatusBadRequest, `body must include "notify_ratchet_triggers": true|false|null`)
		return
	}
	if _, err := decodeOptionalBool(raw); err != nil {
		writeJSONError(w, http.StatusBadRequest, "notify_ratchet_triggers must be true, false, or null")
		return
	}
	msg, ok := ss.applyUIStrategyOverrides(w, id, map[string]json.RawMessage{"notify_ratchet_triggers": raw})
	if !ok {
		return
	}
	writeJSON(w, uiMutationResponse{OK: true, Message: fmt.Sprintf("Ratchet notifications for %s updated. %s", id, msg)})
}

type uiConfigNotificationsResponse struct {
	NotifyRatchetTriggers *bool  `json:"notify_ratchet_triggers"`
	Effective             bool   `json:"effective"`
	Message               string `json:"message,omitempty"`
}

// handleAPIConfigNotifications serves the GLOBAL notify_ratchet_triggers
// default (#1110). GET reports the configured value (null = built-in enabled)
// plus the effective boolean; POST {"notify_ratchet_triggers": bool|null}
// patches the config root through writeValidatedConfigRoot on configWriteMu
// (null deletes the key, restoring the built-in enabled default).
func (ss *StatusServer) handleAPIConfigNotifications(w http.ResponseWriter, r *http.Request) {
	if ss.rejectIfDraining(w) {
		return
	}
	if r.Method == http.MethodGet {
		if !ss.requireAPIAuth(w, r) {
			return
		}
		ss.strategiesMu.RLock()
		v := ss.globalNotifyRatchet
		ss.strategiesMu.RUnlock()
		writeJSON(w, uiConfigNotificationsResponse{
			NotifyRatchetTriggers: v,
			Effective:             v == nil || *v,
		})
		return
	}
	if !ss.uiMutationGuards(w, r) {
		return
	}
	obj, ok := readUIMutationBody(w, r)
	if !ok {
		return
	}
	raw, present := obj["notify_ratchet_triggers"]
	if !present {
		writeJSONError(w, http.StatusBadRequest, `body must include "notify_ratchet_triggers": true|false|null`)
		return
	}
	v, err := decodeOptionalBool(raw)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "notify_ratchet_triggers must be true, false, or null")
		return
	}
	ss.configWriteMu.Lock()
	err = func() error {
		data, err := os.ReadFile(ss.configPath)
		if err != nil {
			return err
		}
		var root map[string]json.RawMessage
		if err := json.Unmarshal(data, &root); err != nil {
			return fmt.Errorf("parse config: %w", err)
		}
		if v == nil {
			delete(root, "notify_ratchet_triggers")
		} else {
			root["notify_ratchet_triggers"] = raw
		}
		return writeValidatedConfigRoot(ss.configPath, root)
	}()
	ss.configWriteMu.Unlock()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Mirror the write so a GET right after the POST reflects it even before
	// the SIGHUP reload lands (SetConfigContext refreshes it again on reload).
	ss.strategiesMu.Lock()
	ss.globalNotifyRatchet = v
	ss.strategiesMu.Unlock()
	msg := ss.triggerConfigReload()
	writeJSON(w, uiConfigNotificationsResponse{
		NotifyRatchetTriggers: v,
		Effective:             v == nil || *v,
		Message:               "Global ratchet notifications updated. " + msg,
	})
}
