package main

import (
	"net/http"
	"time"
)

type UIRegimeWindow struct {
	Regime     string             `json:"regime"`
	Score      float64            `json:"score"`
	Metrics    map[string]float64 `json:"metrics,omitempty"`
	ADX3       string             `json:"adx3,omitempty"`
	Composite7 string             `json:"composite7,omitempty"`
}

type UIRegimeEntry struct {
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
		if b.Payload.MultiMode {
			for name, snap := range b.Payload.Windows {
				windows[name] = UIRegimeWindow{Regime: snap.Regime, Score: snap.Score, Metrics: snap.Metrics}
			}
		} else if b.Payload.Legacy != "" {
			windows[regimeWindowDefaultKey] = UIRegimeWindow{Regime: b.Payload.Legacy}
		}
		for name, views := range b.Views {
			w := windows[name]
			w.ADX3 = views.ADX3
			w.Composite7 = views.Composite7
			windows[name] = w
		}
		out = append(out, UIRegimeEntry{
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
