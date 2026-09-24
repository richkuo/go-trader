package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func getReport(t *testing.T, path, method string) *httptest.ResponseRecorder {
	t.Helper()
	ss := &StatusServer{}
	rr := httptest.NewRecorder()
	ss.handleReports(rr, httptest.NewRequest(method, path, nil))
	return rr
}

func TestReportsIndexListsAudit(t *testing.T) {
	rr := getReport(t, "/reports", http.MethodGet)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, `href="/reports/strategy-audit"`) {
		t.Errorf("index missing link to strategy-audit report")
	}
	if !strings.Contains(body, strategyAuditReportData.Meta.Title) {
		t.Errorf("index missing audit title")
	}
}

func TestStrategyAuditDatasetIntegrity(t *testing.T) {
	d := strategyAuditReportData
	if len(d.Ranking) == 0 || len(d.Candidates) == 0 {
		t.Fatalf("audit dataset is empty: ranking=%d candidates=%d", len(d.Ranking), len(d.Candidates))
	}
	validVerdict := map[string]bool{"keep": true, "watch": true, "deprecate": true, "bug": true, "na": true}
	for _, r := range d.Ranking {
		if !validVerdict[r.Verdict] {
			t.Errorf("row %s has invalid verdict class %q", r.Strategy, r.Verdict)
		}
		if r.Trades == 0 && r.HasVsBH {
			t.Errorf("row %s has 0 trades but claims a vs-B&H value", r.Strategy)
		}
	}
	validTag := map[string]bool{"CONFIRM": true, "CUT": true, "BLOCKED": true}
	for _, c := range d.Candidates {
		if !validTag[c.Verdict] {
			t.Errorf("candidate %s has invalid verdict %q", c.Name, c.Verdict)
		}
	}
}

func TestVsBHSortPushesUnmeasuredToBottom(t *testing.T) {
	measured := auditRow{HasVsBH: true, VsBH: -10.9}
	unmeasured := auditRow{HasVsBH: false}
	if !(unmeasured.VsBHSort() < measured.VsBHSort()) {
		t.Errorf("unmeasured VsBHSort (%v) should sort below measured (%v)",
			unmeasured.VsBHSort(), measured.VsBHSort())
	}
}

func TestReportsUnknownPath404(t *testing.T) {
	rr := getReport(t, "/reports/does-not-exist", http.MethodGet)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rr.Code)
	}
}

func TestReportsRejectsNonGet(t *testing.T) {
	rr := getReport(t, "/reports", http.MethodPost)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rr.Code)
	}
}

func TestReportsRejectsWhileDraining(t *testing.T) {
	shutdownDraining.Store(true)
	defer shutdownDraining.Store(false)
	rr := getReport(t, "/reports/strategy-audit", http.MethodGet)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 while draining", rr.Code)
	}
}
