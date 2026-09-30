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
