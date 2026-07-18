package main

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
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
		`/api/strategies/`,
		`baseline-drifted`,
		`BH-adjusted`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("tuning app wiring missing %q", want)
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
