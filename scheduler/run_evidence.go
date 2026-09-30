package main

import (
	"sync"
	"time"
)

type runEvidence struct {
	mu        sync.Mutex
	startedAt time.Time
	evaluated map[string]time.Time
	skipped   map[string]time.Time
	lastSave  time.Time
}

var globalRunEvidence = &runEvidence{startedAt: time.Now().UTC()}

func (r *runEvidence) markEvaluated(id string, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.evaluated == nil {
		r.evaluated = make(map[string]time.Time)
	}
	r.evaluated[id] = at.UTC()
}

func (r *runEvidence) markZeroCapitalSkipped(id string, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.skipped == nil {
		r.skipped = make(map[string]time.Time)
	}
	r.skipped[id] = at.UTC()
}

func (r *runEvidence) markStateSaved(at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastSave = at.UTC()
}

func (r *runEvidence) healthView() map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	evaluated := make(map[string]string, len(r.evaluated))
	for id, at := range r.evaluated {
		evaluated[id] = at.Format(time.RFC3339Nano)
	}
	skipped := make(map[string]string, len(r.skipped))
	for id, at := range r.skipped {
		skipped[id] = at.Format(time.RFC3339Nano)
	}
	view := map[string]any{
		"started_at":           r.startedAt.Format(time.RFC3339Nano),
		"evaluated":            evaluated,
		"zero_capital_skipped": skipped,
	}
	if !r.lastSave.IsZero() {
		view["last_state_save"] = r.lastSave.Format(time.RFC3339Nano)
	}
	return view
}
