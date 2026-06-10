package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBuildIssueRequest(t *testing.T) {
	req := buildIssueRequest("  Bug in /status  ", "It crashes\n", "bug", "alice (Discord ID 42)")
	if req.Title != "Bug in /status" {
		t.Fatalf("title not trimmed: %q", req.Title)
	}
	if len(req.Labels) != 1 || req.Labels[0] != "bug" {
		t.Fatalf("expected label [bug], got %v", req.Labels)
	}
	if !strings.Contains(req.Body, "It crashes") {
		t.Fatalf("body missing original text: %q", req.Body)
	}
	if !strings.Contains(req.Body, "Filed via `/go-trader-report-an-issue` by alice (Discord ID 42).") {
		t.Fatalf("body missing reporter footer: %q", req.Body)
	}
	if !strings.Contains(req.Body, "\n---\n") {
		t.Fatalf("body missing footer separator: %q", req.Body)
	}
}

func TestBuildIssueRequestNoLabelNoReporter(t *testing.T) {
	req := buildIssueRequest("t", "b", "  ", "")
	if req.Labels != nil {
		t.Fatalf("expected no labels, got %v", req.Labels)
	}
	if !strings.HasSuffix(strings.TrimRight(req.Body, "\n"), "Filed via `/go-trader-report-an-issue`.") {
		t.Fatalf("footer should omit reporter when unknown: %q", req.Body)
	}
}

func TestCreateGitHubIssueSuccess(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/repos/richkuo/go-trader/issues" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok123" {
			t.Errorf("missing/incorrect auth header: %q", got)
		}
		var body githubIssueRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		if body.Title != "hello" {
			t.Errorf("unexpected title %q", body.Title)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"html_url":"https://github.com/richkuo/go-trader/issues/99","number":99}`)
	}))
	defer ts.Close()

	orig := githubAPIBaseURL
	githubAPIBaseURL = ts.URL
	defer func() { githubAPIBaseURL = orig }()

	url, num, err := createGitHubIssue(context.Background(), ts.Client(), "richkuo/go-trader", "tok123",
		buildIssueRequest("hello", "world", "", "bob"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if num != 99 {
		t.Fatalf("expected number 99, got %d", num)
	}
	if url != "https://github.com/richkuo/go-trader/issues/99" {
		t.Fatalf("unexpected url %q", url)
	}
}

func TestCreateGitHubIssueErrorStatus(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"message":"Bad credentials"}`)
	}))
	defer ts.Close()

	orig := githubAPIBaseURL
	githubAPIBaseURL = ts.URL
	defer func() { githubAPIBaseURL = orig }()

	_, _, err := createGitHubIssue(context.Background(), ts.Client(), "richkuo/go-trader", "bad",
		buildIssueRequest("t", "b", "", ""))
	if err == nil {
		t.Fatal("expected error on 401, got nil")
	}
	if !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "Bad credentials") {
		t.Fatalf("error should surface status + body: %v", err)
	}
}

func TestReportCommandIsOwnerDMGated(t *testing.T) {
	// Non-owner is rejected.
	if ok, _ := authorizeCommand("report-an-issue", "intruder", "", "owner"); ok {
		t.Fatal("report must reject non-owner")
	}
	// Owner in a guild (guildID != "") is rejected.
	if ok, _ := authorizeCommand("report-an-issue", "owner", "guild1", "owner"); ok {
		t.Fatal("report must reject guild context")
	}
	// Owner in a DM is allowed.
	if ok, _ := authorizeCommand("report-an-issue", "owner", "", "owner"); !ok {
		t.Fatal("owner DM should be allowed to report")
	}
}

func TestReportTokenResolution(t *testing.T) {
	t.Setenv("GO_TRADER_GITHUB_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	c := DiscordConfig{ReportGitHubToken: "cfgtok"}
	if got := c.reportToken(); got != "cfgtok" {
		t.Fatalf("expected config token, got %q", got)
	}
	t.Setenv("GITHUB_TOKEN", "envtok")
	if got := c.reportToken(); got != "envtok" {
		t.Fatalf("env should win over config, got %q", got)
	}
	t.Setenv("GO_TRADER_GITHUB_TOKEN", "primary")
	if got := c.reportToken(); got != "primary" {
		t.Fatalf("GO_TRADER_GITHUB_TOKEN should win, got %q", got)
	}
}

func TestReportRepoDefault(t *testing.T) {
	if got := (DiscordConfig{}).reportRepo(); got != defaultReportRepo {
		t.Fatalf("expected default repo, got %q", got)
	}
	if got := (DiscordConfig{ReportRepo: "me/x"}).reportRepo(); got != "me/x" {
		t.Fatalf("expected override, got %q", got)
	}
}
