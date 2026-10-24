//go:build pyintegration

package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBasketPairCadenceRealHostDispatchesInAWakeOnlyStep(t *testing.T) {
	p := newOKXPairStepEnv(t, pairSetLimit("decision_cadence_ms", 500))
	e := p.e
	e.collect = true
	e.quotes.enable()
	feed := pairPyFeed(t, e.cfg)
	hl, okx := pairPySC(e, pairTestHL), p.sc()
	pairHostRunner(e, 2)
	h := newPairTestHost(e, time.Minute)
	managed := 0
	h.manage = func(time.Time, map[RiskPartition]*scopeCycleRisk) {
		managed++
		pairPyStep(t, e, hl, feed, nil)
		runOKXStep(p.context(okx), okx, okxPairManagementCheck(pairPyManagementInput(60000), p.notifier))
	}
	t0 := pairTestStart()
	h.runUntil(t0.Add(3 * time.Second))
	if managed != 1 || len(h.runs) < 6 {
		t.Fatalf("the host must run real member management once and then wake on the 500ms pair cadence, got %d management runs and %d runs", managed, len(h.runs))
	}
	for _, id := range []string{pairTestHL, pairTestOKX} {
		obs, ok := e.rt().observations[id]
		if !ok || obs.AtMs != t0.UnixMilli() || indicatorsATRValue(obs.Indicators) <= 0 || e.rt().pairEchoFailures[id] != "" {
			t.Fatalf("member %s must carry the real management echo from the management run: %+v (echo failure %q)", id, obs, e.rt().pairEchoFailures[id])
		}
	}
	legs := e.actionsByPurpose(pairPurposeEntry)
	if len(legs) != 2 {
		t.Fatalf("want a first leg and a hedge from wake-only steps, got %d", len(legs))
	}
	for _, a := range legs {
		in := -1
		for i, r := range h.runs {
			if r.At.UnixMilli() == a.CreatedAtMs {
				in = i
			}
		}
		if in <= 0 || h.runs[in].Managed {
			t.Fatalf("action %s must be dispatched in a wake-only run, got run %d", a.ActionID, in)
		}
	}
	e.assertBooks("matched on real management output", 0.06, -0.06)
	if strings.Join(h.order[:6], ",") != "evidence,prepare_risk,risk,manage,step,save" || strings.Join(h.order[6:11], ",") != "evidence,prepare_risk,risk,step,save" {
		t.Fatalf("the host must follow the documented order, got %v", h.order[:11])
	}
}

func TestBasketPairCadenceRealPolicyChildTimeoutMarksTheRunIncomplete(t *testing.T) {
	e, root := pairPyEnv(t, nil)
	python := filepath.Join(root, ".venv", "bin", "python3")
	child := func(script string) pairPolicyRunner {
		return func(ctx context.Context, stdin []byte, timeout time.Duration) ([]byte, []byte, error) {
			return spawnPythonProcessRawLimited(ctx, python, []string{"-c", script}, stdin, timeout, nil, pythonOutputLimits{Stdout: 1 << 20, Stderr: 1 << 20})
		}
	}
	at := pairTestStart()
	t.Run("economic hold from a real child", func(t *testing.T) {
		e, _ := pairPyEnv(t, nil)
		e.bc.pairPolicyRun = child(`import json, sys
req = json.load(sys.stdin)
print(json.dumps({"v": 2, "pair_id": req["pair_id"], "slot_ms": req["slot_ms"], "contract_fingerprint": req["contract_fingerprint"],
    "input_fingerprint": req["input_fingerprint"], "status": "hold", "orientation": "", "targets": None, "hold_reason": "spread inside costs",
    "error": "", "decision": {}}))`)
		e.slot(at, "in-0")
		e.step(at.Add(time.Second))
		rb := e.rt().Rebalances[at.UnixMilli()]
		if rb.Status != basketRebalanceHeld || !strings.Contains(rb.TargetsJSON, "spread inside costs") {
			t.Fatalf("the child's explicit hold must freeze as a policy hold, got %s %s", rb.Status, rb.TargetsJSON)
		}
		if _, ok := pairIncompleteEvidence(e.rt()); ok {
			t.Fatal("an explicit economic hold never marks the run incomplete")
		}
	})
	e.bc.pairPolicyRun = child("import sys, time\nsys.stdin.read()\ntime.sleep(30)\n")
	e.slot(at, "in-0")
	start := time.Now()
	e.step(at.Add(4700 * time.Millisecond))
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("the quote-age budget must kill the child process group near 100ms, took %s", took)
	}
	rb := e.rt().Rebalances[at.UnixMilli()]
	if rb.Status != basketRebalanceHeld || !strings.Contains(rb.HoldReason, "infrastructure timeout") {
		t.Fatalf("a real child timeout must hold the slot as an infrastructure timeout, got %s %q", rb.Status, rb.HoldReason)
	}
	h := newPairTestHost(e, time.Minute)
	o, ok := h.incomplete()
	if !ok || !h.savedIncomplete() || !strings.Contains(o.Detail, "deadline") {
		t.Fatalf("the child timeout must leave a durable incomplete marker with its cause: %+v", o)
	}
	e.restart()
	if again, ok := h.incomplete(); !ok || again != o || !h.savedIncomplete() {
		t.Fatalf("a restart must keep the incomplete marker, got %+v", again)
	}
}
