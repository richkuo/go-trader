package main

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
)

func getTuningPage(t *testing.T, path, method string) *httptest.ResponseRecorder {
	t.Helper()
	ss := &StatusServer{}
	rr := httptest.NewRecorder()
	ss.handleTuning(rr, httptest.NewRequest(method, path, nil))
	return rr
}

func TestTuningPageServesDedicatedApp(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		rr := getTuningPage(t, "/tuning", method)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want 200", method, rr.Code)
		}
		if got := rr.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
			t.Errorf("%s content type = %q, want text/html", method, got)
		}
		if method == http.MethodGet {
			body := rr.Body.String()
			for _, want := range []string{
				`data-page="tuning"`,
				`id="tuning-launch-form"`,
				`id="tuning-runs"`,
				`id="tuning-results"`,
				`src="/dashboard/app.js"`,
			} {
				if !strings.Contains(body, want) {
					t.Errorf("tuning page missing %q", want)
				}
			}
		}
	}
}

func TestTuningPageRoutingGuards(t *testing.T) {
	if rr := getTuningPage(t, "/tuning/unknown", http.MethodGet); rr.Code != http.StatusNotFound {
		t.Errorf("unknown path status = %d, want 404", rr.Code)
	}
	if rr := getTuningPage(t, "/tuning", http.MethodPost); rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST status = %d, want 405", rr.Code)
	}

	shutdownDraining.Store(true)
	defer shutdownDraining.Store(false)
	if rr := getTuningPage(t, "/tuning", http.MethodGet); rr.Code != http.StatusServiceUnavailable {
		t.Errorf("draining status = %d, want 503", rr.Code)
	}
}

func TestTuningStaticAppWiresRunAndLiveConfigAPIs(t *testing.T) {
	sub, err := fs.Sub(uiAssets, "static/ui")
	if err != nil {
		t.Fatal(err)
	}
	app, err := fs.ReadFile(sub, "app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(app)
	for _, want := range []string{
		`document.body.dataset.page === "tuning"`,
		`/api/tuning/runs`,
		`/api/tuning/apply`,
		`/api/strategies/`,
		`baseline-drifted`,
		`baseline-unknown`,
		`BH-adjusted`,
		`Always re-read live config at render time`,
		`memoized server-side`,
		`detailReloadPending`,
		`Defer clearing until replacement content is ready`,
		`apply_eligibility`,
		`Apply tuning suggestion`,
		`open-as-close`,
		`loadRunDetail()`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("tuning app wiring missing %q", want)
		}
	}
	for _, forbidden := range []string{
		`liveSnapshots`,
		`forceLiveRead`,
	} {
		if strings.Contains(js, forbidden) {
			t.Errorf("tuning app still caches live config via %q", forbidden)
		}
	}

	index, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(index), `href="/tuning"`) {
		t.Error("dashboard navigation missing /tuning link")
	}
}

func TestTuningDiffAndBaselineStates(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is unavailable")
	}
	const script = `
const assert = require("assert");
const logic = require("./static/ui/app.js");

let diff = logic.paramDiff({params: {fast: 20}}, {fast: 10, slow: 50});
assert.deepStrictEqual(diff.keys, ["fast"]);
assert.strictEqual(diff.replacement, false);

diff = logic.paramDiff({params: {fast: 10}}, {fast: 10, slow: 50});
assert.deepStrictEqual(diff.keys, []);

diff = logic.paramDiff({patch: {open_strategy: {params: {fast: 20, slow: 50}}}}, {fast: 10, slow: 50});
assert.deepStrictEqual(diff.keys, ["fast"]);

diff = logic.paramDiff({patch: {open_strategy: {params: {fast: 10, slow: 50}}}}, {fast: 10, slow: 50});
assert.deepStrictEqual(diff.keys, []);

diff = logic.paramDiff({patch: {open_strategy: {params: {fast: 20}}}}, {fast: 10, slow: 50});
assert.deepStrictEqual(diff.keys, ["fast", "slow"]);
assert.strictEqual(diff.proposed.slow, undefined);
assert.strictEqual(diff.replacement, true);

assert.strictEqual(logic.baselineState({}, {name: "sma", params: {fast: 10}}), "unknown");
assert.strictEqual(logic.baselineState({open_strategy: "sma"}, {name: "sma", params: {fast: 10}}), "unknown");
assert.strictEqual(logic.baselineState(
  {open_strategy: "sma", baseline_params: {fast: 10}},
  {name: "sma", params: {fast: 10}}
), "current");
assert.strictEqual(logic.baselineState(
  {open_strategy: "sma", baseline_params: {}},
  {name: "sma", params: {}}
), "current");
assert.strictEqual(logic.baselineState(
  {open_strategy: "sma", baseline_params: {fast: 11}},
  {name: "sma", params: {fast: 10}}
), "drifted");

// Raw empty baseline vs Go-merged live params (common args-form) is current
// once defaults are applied to both sides — not drifted.
const defaults = {fast: 10, slow: 50};
assert.strictEqual(logic.baselineState(
  {open_strategy: "sma", baseline_params: {}},
  {name: "sma", params: {fast: 10, slow: 50}},
  defaults
), "current");
// Explicit override recorded at run start still matches merged live.
assert.strictEqual(logic.baselineState(
  {open_strategy: "sma", baseline_params: {fast: 11}},
  {name: "sma", params: {fast: 11, slow: 50}},
  defaults
), "current");
// Genuine post-run live edit still reports drifted.
assert.strictEqual(logic.baselineState(
  {open_strategy: "sma", baseline_params: {}},
  {name: "sma", params: {fast: 11, slow: 50}},
  defaults
), "drifted");

assert.strictEqual(logic.detailLoadAction("", false, ""), "idle");
assert.strictEqual(logic.detailLoadAction("run-a", false, ""), "start");
assert.strictEqual(logic.detailLoadAction("run-a", true, "run-a"), "skip");
assert.strictEqual(logic.detailLoadAction("run-b", true, "run-a"), "queue");

let apply = logic.applyButtonState("eligible");
assert.strictEqual(apply.enabled, true);
assert.strictEqual(apply.label, "Apply");

apply = logic.applyButtonState("already_applied", "2026-07-20T12:00:00Z");
assert.strictEqual(apply.enabled, false);
assert.ok(String(apply.reason).includes("2026-07-20T12:00:00Z"));

apply = logic.applyButtonState("baseline_drifted");
assert.strictEqual(apply.enabled, false);
assert.ok(String(apply.reason).includes("Re-run tuning"));

apply = logic.applyButtonState("legacy_artifact");
assert.strictEqual(apply.enabled, false);

apply = logic.applyButtonState("not_survivor");
assert.strictEqual(apply.enabled, false);

let confirm = logic.applyConfirmMessage("spot-a", "cand_1", {has_open_position: false});
assert.ok(confirm.includes("spot-a"));
assert.ok(!confirm.includes("open trade"));

confirm = logic.applyConfirmMessage("spot-a", "cand_1", {
  has_open_position: true,
  close_strategy: {name: "tiered_tp_atr"}
});
assert.ok(!confirm.includes("open trade"));

confirm = logic.applyConfirmMessage("spot-a", "cand_1", {has_open_position: true});
assert.ok(confirm.includes("open trade"));
assert.ok(confirm.includes("exits"));
`
	cmd := exec.Command("node", "-e", script)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("tuning logic checks failed: %v\n%s", err, output)
	}
}
