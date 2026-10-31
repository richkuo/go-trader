package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

type UIRegimeWindow struct {
	Ready      bool               `json:"ready"`
	Bars       int                `json:"bars"`
	Need       int                `json:"need"`
	Status     string             `json:"status"`
	Regime     string             `json:"regime"`
	Score      float64            `json:"score"`
	Metrics    map[string]float64 `json:"metrics,omitempty"`
	ADX3       RegimeSnapshot     `json:"adx3,omitempty"`
	Composite7 RegimeSnapshot     `json:"composite7,omitempty"`
}

type UIRegimeEntry struct {
	Error     string                    `json:"error,omitempty"`
	Platform  string                    `json:"platform"`
	Symbol    string                    `json:"symbol"`
	Timeframe string                    `json:"timeframe"`
	BarTime   string                    `json:"bar_time,omitempty"`
	Windows   map[string]UIRegimeWindow `json:"windows"`
	At        time.Time                 `json:"at"`
}

func uiRegimeEntries(store *RegimeStore) ([]UIRegimeEntry, time.Time) {
	bundles, builtAt := store.Snapshot()
	out := make([]UIRegimeEntry, 0, len(bundles))
	for _, b := range bundles {
		windows := make(map[string]UIRegimeWindow)
		for name, snap := range b.Payload.Snapshots() {
			windows[name] = UIRegimeWindow{Regime: snap.Regime, Score: snap.Score, Metrics: snap.Metrics, Ready: snap.Ready, Bars: snap.Bars, Need: snap.Need, Status: snap.Display()}
		}
		if b.Err != "" {
			var specs RegimeWindowsMap
			_ = json.Unmarshal([]byte(b.Key.SpecJSON), &specs)
			for name := range specs {
				windows[name] = UIRegimeWindow{Status: "unavailable"}
			}
		}

		for name, views := range b.Views {
			w := windows[name]
			w.ADX3 = views.ADX3
			w.Composite7 = views.Composite7
			windows[name] = w
		}
		out = append(out, UIRegimeEntry{
			Error:     b.Err,
			Platform:  b.Key.Platform,
			Symbol:    b.Key.Symbol,
			Timeframe: b.Key.Timeframe,
			BarTime:   b.BarTime,
			Windows:   windows,
			At:        b.At,
		})
	}
	return out, builtAt
}

func (ss *StatusServer) handleAPIRegime(w http.ResponseWriter, r *http.Request) {
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
	if r.URL.Path != "/api/regime" && r.URL.Path != "/api/regime/" {
		http.NotFound(w, r)
		return
	}
	entries, builtAt := uiRegimeEntries(globalRegimeStore)
	writeJSON(w, map[string]interface{}{
		"built_at": builtAt,
		"regimes":  entries,
	})
}

func (ss *StatusServer) handleAPIRegimeManual(w http.ResponseWriter, r *http.Request) {
	owner, active := globalManualRegimeOwner.identity()
	q := r.URL.Query()
	var sign manualRegimeReplySigner
	challenge := r.Header.Get(manualRegimeChallengeHeader)
	if active && validManualRegimeNonce(challenge) &&
		manualRegimeMACEqual(r.Header.Get(manualRegimeAuthHeader), manualRegimeRequestMAC(owner.Secret, challenge, q.Get("strategy"), q.Get(uiPartitionQueryKey))) {
		secret := owner.Secret
		sign = func(status int, body []byte) string { return manualRegimeReplyMAC(secret, challenge, status, body) }
	}
	if isDraining() {
		writeManualRegimeEnvelope(w, manualRegimeEnvelope{Result: manualRegimeResultDraining}, sign)
		return
	}
	if sign == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"unauthorized"}`))
		return
	}
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if r.URL.Path != manualRegimeRoute {
		http.NotFound(w, r)
		return
	}
	id := strings.TrimSpace(q.Get("strategy"))
	if id == "" || strings.TrimSpace(q.Get(uiPartitionQueryKey)) == "" {
		writeManualRegimeEnvelope(w, manualRegimeEnvelope{Result: manualRegimeResultInvalid}, sign)
		return
	}
	filter, ok := ss.uiPartitionParam(w, r)
	if !ok {
		return
	}
	if filter.All {
		writeManualRegimeEnvelope(w, manualRegimeEnvelope{Result: manualRegimeResultInvalid}, sign)
		return
	}
	if ss.stateDB == nil || ss.mu == nil {
		writeManualRegimeEnvelope(w, manualRegimeEnvelope{Result: manualRegimeResultInternal}, sign)
		return
	}

	var (
		sc        StrategyConfig
		found     bool
		binding   manualRegimeBinding
		bindErr   error
		intervalS int
	)
	ss.mu.RLock()
	ss.strategiesMu.RLock()
	cfg := ss.uiCfg
	ss.strategiesMu.RUnlock()
	if cfg != nil {
		for _, candidate := range cfg.Strategies {
			if candidate.ID == id {
				sc, found = candidate, true
				break
			}
		}
		if found {
			binding, bindErr = manualRegimeBindingFor(cfg, ss.stateDB.ident, sc)
		}
		intervalS = cfg.IntervalSeconds
	}
	ss.mu.RUnlock()

	if cfg == nil {
		writeManualRegimeEnvelope(w, manualRegimeEnvelope{Result: manualRegimeResultInternal}, sign)
		return
	}
	if !found || sc.Type != "manual" || partitionFor(sc) != filter.Partition {
		writeManualRegimeEnvelope(w, manualRegimeEnvelope{Result: manualRegimeResultUnknown}, sign)
		return
	}
	if bindErr != nil {
		writeManualRegimeEnvelope(w, manualRegimeEnvelope{Result: manualRegimeResultInternal}, sign)
		return
	}
	key := manualRegimeKeyFor(binding.Request.Key)
	env := manualRegimeEnvelope{
		OwnerNonce:          owner.Nonce,
		StrategyID:          binding.Contract.StrategyID,
		Partition:           binding.Contract.Partition,
		StorageRole:         binding.Contract.StorageRole,
		StorageID:           binding.Contract.StorageID,
		ContractFingerprint: binding.Fingerprint,
		Key:                 &key,
	}
	view := globalRegimeStore.manualExport(binding.Request.Key, id)
	switch {
	case !view.ContractFound:
		env.Result = manualRegimeResultUnavailable
	case view.Contract.Fingerprint != binding.Fingerprint || view.Contract.Key != binding.Request.Key:
		env = manualRegimeEnvelope{Result: manualRegimeResultMismatch}
	case !view.Found || view.Err == regimeBundlePendingErr:
		env.Result = manualRegimeResultUnavailable
	case view.Err != "":
		env.Result = manualRegimeResultClassification
	case view.PayloadJSON == "" || view.At.IsZero():
		env = manualRegimeEnvelope{Result: manualRegimeResultInternal}
	default:
		at := view.At
		env.Result = manualRegimeResultBundle
		env.Generation = view.Gen
		env.IntervalSeconds = intervalS
		env.BundleAt = &at
		env.Payload = json.RawMessage(view.PayloadJSON)
	}
	if current, stillActive := globalManualRegimeOwner.identity(); !stillActive || current != owner {
		env = manualRegimeEnvelope{Result: manualRegimeResultInternal}
	}
	writeManualRegimeEnvelope(w, env, sign)
}
