//go:build pyintegration

package main

import (
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const hlAgentTestKey = "0x1111111111111111111111111111111111111111111111111111111111111111"

const hlAgentTestMaster = "0xabababababababababababababababababababab"

const hlAgentTestOther = "0xcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd"

const hlAgentStubDriver = `import os, runpy, sys
root = os.environ["HL_AGENT_TEST_ROOT"]
sys.path.insert(0, os.path.join(root, "platforms", "hyperliquid"))
import adapter
adapter.MAINNET_URL = os.environ["HL_AGENT_TEST_URL"]
adapter.TESTNET_URL = os.environ["HL_AGENT_TEST_URL"]
script = os.path.join(root, "shared_scripts", "fetch_hl_agent_status.py")
sys.argv = [script] + sys.argv[1:]
runpy.run_path(script, run_name="__main__")
`

const hlAgentRejectionVenueText = "User or API Wallet 0x1111111111111111111111111111111111111111 does not exist."

const hlAgentRejectingRunnerDriver = `import json, sys
print(json.dumps({"error": "exchange rejected order: {'status': 'err', 'response': '` + hlAgentRejectionVenueText + `'}", "order_outcome": "rejected"}))
sys.exit(1)
`

type hlAgentStub struct {
	mu           sync.Mutex
	mode         string
	validUntilMs int64
	agentName    string
	signer       string
	requests     []string
}

func (s *hlAgentStub) set(mode string, validUntilMs int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mode = mode
	s.validUntilMs = validUntilMs
}

func (s *hlAgentStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Type string `json:"type"`
		User string `json:"user"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	s.mu.Lock()
	mode, validUntil, name := s.mode, s.validUntilMs, s.agentName
	s.requests = append(s.requests, req.Type)
	if req.Type == "userRole" {
		s.signer = req.User
	}
	signer := s.signer
	s.mu.Unlock()
	switch mode {
	case "http500":
		http.Error(w, "upstream failure", http.StatusInternalServerError)
		return
	case "garbage":
		_, _ = w.Write([]byte("<html>not json</html>"))
		return
	case "timeout":
		time.Sleep(2 * time.Second)
		_, _ = w.Write([]byte(`{"role":"missing"}`))
		return
	}
	var body any
	switch req.Type {
	case "userRole":
		switch mode {
		case "missing":
			body = map[string]any{"role": "missing"}
		case "wrong_master":
			body = map[string]any{"role": "agent", "data": map[string]any{"user": hlAgentTestOther}}
		case "user":
			body = map[string]any{"role": "user"}
		default:
			body = map[string]any{"role": "agent", "data": map[string]any{"user": hlAgentTestMaster}}
		}
	case "extraAgents":
		if mode == "not_listed" {
			body = []any{map[string]any{"name": "other", "address": hlAgentTestOther, "validUntil": validUntil}}
		} else {
			body = []any{
				map[string]any{"name": "other", "address": hlAgentTestOther, "validUntil": validUntil + 1},
				map[string]any{"name": name, "address": strings.ToUpper(signer[:2]) + strings.ToUpper(signer[2:]), "validUntil": validUntil},
			}
		}
	default:
		http.Error(w, "unexpected type", http.StatusBadRequest)
		return
	}
	_ = json.NewEncoder(w).Encode(body)
}

type hlAgentFixture struct {
	t      *testing.T
	stub   *hlAgentStub
	mock   *mockNotifier
	n      *MultiNotifier
	driver string
}

func newHLAgentFixture(t *testing.T, master string) *hlAgentFixture {
	t.Helper()
	root := entryOperatorRepoRoot(t)
	stub := &hlAgentStub{agentName: "go-trader <@123> `bot`"}
	srv := httptest.NewServer(stub)
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	driver := filepath.Join(dir, "driver.py")
	if err := os.WriteFile(driver, []byte(hlAgentStubDriver), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HL_AGENT_TEST_ROOT", root)
	t.Setenv("HL_AGENT_TEST_URL", srv.URL)
	t.Setenv("HYPERLIQUID_SECRET_KEY", hlAgentTestKey)
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", master)
	t.Setenv("HYPERLIQUID_TESTNET", "")
	t.Chdir(root)
	prevScript, prevHTTP, prevOwner := hlAgentStatusScript, hlAgentHTTPTimeoutSeconds, hlAgentStatus
	hlAgentStatusScript = driver
	hlAgentHTTPTimeoutSeconds = 0.5
	hlAgentStatus = &hlAgentStatusOwner{}
	t.Cleanup(func() {
		hlAgentStatusScript, hlAgentHTTPTimeoutSeconds, hlAgentStatus = prevScript, prevHTTP, prevOwner
	})
	mock := &mockNotifier{}
	n := NewMultiNotifier(notifierBackend{notifier: mock, ownerID: "owner", channels: map[string]string{"hyperliquid": "hl-ch"}})
	hlAgentStatus.bind(n)
	return &hlAgentFixture{t: t, stub: stub, mock: mock, n: n, driver: driver}
}

func hlAgentKeyForms() []string {
	body := strings.TrimPrefix(hlAgentTestKey, "0x")
	dec, _ := new(big.Int).SetString(body, 16)
	return []string{body, strings.ToUpper(body), dec.String()}
}

func (f *hlAgentFixture) assertNoKey(where, text string) {
	f.t.Helper()
	for _, form := range hlAgentKeyForms() {
		if strings.Contains(text, form) {
			f.t.Fatalf("%s contains the secret key", where)
		}
	}
}

func (f *hlAgentFixture) run(mode string, validUntilMs int64) (hlAgentReport, error) {
	f.t.Helper()
	f.stub.set(mode, validUntilMs)
	args := []string{fmt.Sprintf("--timeout=%g", hlAgentHTTPTimeoutSeconds)}
	stdout, stderr, runErr := runPythonWithTimeout(shutdownReadOnlyCtx, hlAgentStatusScript, args, nil, hlAgentCheckTimeout)
	f.assertNoKey("script stdout", string(stdout))
	f.assertNoKey("script stderr", string(stderr))
	return parseHLAgentReport(stdout, stderr, runErr)
}

func (f *hlAgentFixture) criticals() int {
	f.mock.mu.Lock()
	defer f.mock.mu.Unlock()
	count := 0
	for _, dm := range f.mock.dms {
		f.assertNoKey("owner DM", dm.content)
		if strings.Contains(dm.content, "HL API WALLET CRITICAL") {
			count++
		}
	}
	for _, m := range f.mock.messages {
		f.assertNoKey("channel message", m.content)
	}
	return count
}

func alertKinds(alerts []hlAgentAlert) []string {
	out := make([]string, 0, len(alerts))
	for _, a := range alerts {
		out = append(out, a.Kind)
	}
	return out
}

func requireKinds(t *testing.T, step string, alerts []hlAgentAlert, want ...string) {
	t.Helper()
	got := alertKinds(alerts)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("%s: alert kinds %v, want %v", step, got, want)
	}
}

func TestHLAgentStatusScriptReportsTheVenueStates(t *testing.T) {
	f := newHLAgentFixture(t, hlAgentTestMaster)
	validUntil := time.Now().Add(100 * 24 * time.Hour).UnixMilli()

	r, err := f.run("valid", validUntil)
	if err != nil {
		t.Fatalf("valid: %v", err)
	}
	if r.Error != "" || r.Role != "agent" || !strings.EqualFold(r.MasterAddress, hlAgentTestMaster) || !strings.EqualFold(r.RoleUser, hlAgentTestMaster) || r.ValidUntilMs != validUntil || !r.ListedInExtraAgents {
		t.Fatalf("valid report = %+v", r)
	}
	if !hlAgentAddressRE.MatchString(r.SignerAddress) || strings.EqualFold(r.SignerAddress, hlAgentTestMaster) {
		t.Fatalf("valid report signer = %q", r.SignerAddress)
	}
	if state, _ := classifyHLAgentReport(r, time.Now()); state != hlAgentStateValid {
		t.Fatalf("valid state = %s", state)
	}

	cases := []struct {
		mode  string
		state hlAgentState
	}{
		{"missing", hlAgentStateMissing},
		{"wrong_master", hlAgentStateWrongMaster},
		{"user", hlAgentStateWrongMaster},
		{"not_listed", hlAgentStateUnknownLifetime},
		{"http500", hlAgentStateCheckError},
		{"garbage", hlAgentStateCheckError},
		{"timeout", hlAgentStateCheckError},
	}
	for _, tc := range cases {
		r, err := f.run(tc.mode, validUntil)
		if err != nil {
			t.Fatalf("%s: %v", tc.mode, err)
		}
		state, _ := classifyHLAgentReport(r, time.Now())
		if state != tc.state {
			t.Fatalf("%s: state %s, want %s (report %+v)", tc.mode, state, tc.state, r)
		}
		if tc.state == hlAgentStateCheckError && r.Error == "" {
			t.Fatalf("%s: the report has no error", tc.mode)
		}
	}

	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "")
	r, err = f.run("user", validUntil)
	if err != nil {
		t.Fatalf("master key: %v", err)
	}
	if state, _ := classifyHLAgentReport(r, time.Now()); state != hlAgentStateMasterKey {
		t.Fatalf("master key state = %s (report %+v)", state, r)
	}

	if got := f.criticals(); got != 0 {
		t.Fatalf("reading reports sent %d CRITICAL alerts", got)
	}
}

func TestHLAgentStatusCheckAlertsOnceAndNeverOnErrors(t *testing.T) {
	f := newHLAgentFixture(t, hlAgentTestMaster)

	for _, mode := range []string{"http500", "garbage", "timeout"} {
		f.stub.set(mode, 0)
		hlAgentStatus.runCheck(nil, hlAgentCheckTimeout)
	}
	if got := f.criticals(); got != 0 {
		t.Fatalf("failed checks sent %d CRITICAL alerts", got)
	}
	f.mock.mu.Lock()
	sent := len(f.mock.dms) + len(f.mock.messages)
	f.mock.mu.Unlock()
	if sent != 0 {
		t.Fatalf("failed checks sent %d messages", sent)
	}

	f.stub.set("missing", 0)
	hlAgentStatus.runCheck(nil, hlAgentCheckTimeout)
	hlAgentStatus.runCheck(nil, hlAgentCheckTimeout)
	if got := f.criticals(); got != 1 {
		t.Fatalf("a missing API wallet sent %d CRITICAL alerts, want 1", got)
	}
	f.mock.mu.Lock()
	text := f.mock.dms[0].content
	f.mock.mu.Unlock()
	for _, want := range []string{"does not know the API wallet", "no live Hyperliquid order, stop-loss or take-profit", "create a new API wallet", "HYPERLIQUID_SECRET_KEY", "restart"} {
		if !strings.Contains(text, want) {
			t.Fatalf("the CRITICAL does not name %q: %s", want, text)
		}
	}

	f.stub.set("valid", time.Now().Add(100*24*time.Hour).UnixMilli())
	hlAgentStatus.runCheck(nil, hlAgentCheckTimeout)
	f.mock.mu.Lock()
	last := f.mock.dms[len(f.mock.dms)-1].content
	f.mock.mu.Unlock()
	if !strings.Contains(last, "HL API WALLET RECOVERED") {
		t.Fatalf("a valid check after a missing one sent %q", last)
	}
}

func TestHLAgentStatusExpiryThresholdsAndThrottle(t *testing.T) {
	f := newHLAgentFixture(t, hlAgentTestMaster)
	base := time.Date(2027, 3, 1, 12, 0, 0, 0, time.UTC)
	validUntil := base.UnixMilli()
	r, err := f.run("valid", validUntil)
	if err != nil || r.Error != "" {
		t.Fatalf("valid report: %v %+v", err, r)
	}
	day := 24 * time.Hour
	o := &hlAgentStatusOwner{}
	at := func(d time.Duration) time.Time { return base.Add(-d) }

	requireKinds(t, "20 days left", o.applyReport(r, at(20*day), at(20*day)))
	requireKinds(t, "13 days left", o.applyReport(r, at(13*day), at(13*day)), hlAgentAlertWarn)
	requireKinds(t, "13 days left again", o.applyReport(r, at(13*day-time.Hour), at(13*day-time.Hour)))
	sevenDay := o.applyReport(r, at(6*day+12*time.Hour), at(6*day+12*time.Hour))
	requireKinds(t, "6.5 days left", sevenDay, hlAgentAlertWarn)
	if !strings.HasPrefix(sevenDay[0].Text, "**HL API WALLET EXPIRES IN 6 DAYS**") {
		t.Fatalf("the 7-day warning is %q", sevenDay[0].Text)
	}
	requireKinds(t, "6 days left", o.applyReport(r, at(6*day), at(6*day)))
	requireKinds(t, "2 days left", o.applyReport(r, at(2*day), at(2*day)), hlAgentAlertUrgent)
	requireKinds(t, "2 days left again", o.applyReport(r, at(2*day-time.Hour), at(2*day-time.Hour)))
	oneDay := o.applyReport(r, at(20*time.Hour), at(20*time.Hour))
	requireKinds(t, "20 hours left", oneDay, hlAgentAlertUrgent)
	if !strings.HasPrefix(oneDay[0].Text, "**HL API WALLET EXPIRES IN UNDER 1 DAY**") {
		t.Fatalf("the 1-day warning is %q", oneDay[0].Text)
	}
	requireKinds(t, "1 hour left", o.applyReport(r, at(time.Hour), at(time.Hour)))
	if d := o.nextCheckDelay(base.Add(-10 * time.Minute)); d != 10*time.Minute+hlAgentExpiryRecheckDelay {
		t.Fatalf("the check before expiry waits %s", d)
	}
	expired := o.applyReport(r, base, base)
	requireKinds(t, "past validUntil", expired, hlAgentAlertCritical)
	if !strings.Contains(expired[0].Text, "expired at 2027-03-01 12:00 UTC") {
		t.Fatalf("the expiry CRITICAL text is %q", expired[0].Text)
	}
	if strings.ContainsAny(strings.SplitN(expired[0].Text, "(", 2)[0], "<@") {
		t.Fatalf("the agent name is not sanitized: %q", expired[0].Text)
	}
	requireKinds(t, "past validUntil inside the throttle", o.applyReport(r, base.Add(time.Hour), base.Add(time.Hour)))
	requireKinds(t, "past validUntil after the throttle", o.applyReport(r, base.Add(effectiveAlertThrottleInterval()+time.Hour), base.Add(effectiveAlertThrottleInterval()+time.Hour)), hlAgentAlertCritical)

	renewed, err := f.run("valid", base.Add(60*day).UnixMilli())
	if err != nil || renewed.Error != "" {
		t.Fatalf("renewed report: %v %+v", err, renewed)
	}
	now := base.Add(47 * day)
	requireKinds(t, "new validUntil 13 days out", o.applyReport(renewed, now, now), hlAgentAlertRecovered, hlAgentAlertWarn)

	fresh := &hlAgentStatusOwner{}
	requireKinds(t, "first observation 2 days out", fresh.applyReport(r, at(2*day), at(2*day)), hlAgentAlertUrgent)
	requireKinds(t, "then 2 days out again", fresh.applyReport(r, at(2*day-time.Hour), at(2*day-time.Hour)))
	requireKinds(t, "then 1 day out", fresh.applyReport(r, at(day), at(day)), hlAgentAlertUrgent)
}

func TestHLAgentStatusRecoveryWaitsForACheckAfterTheRejection(t *testing.T) {
	f := newHLAgentFixture(t, hlAgentTestMaster)
	r, err := f.run("valid", time.Now().Add(100*24*time.Hour).UnixMilli())
	if err != nil || r.Error != "" {
		t.Fatalf("valid report: %v %+v", err, r)
	}
	o := &hlAgentStatusOwner{}
	t0 := time.Now().UTC()
	requireKinds(t, "rejection", o.observeRejection(hlAgentRejectionVenueText, t0), hlAgentAlertCritical)
	requireKinds(t, "check started before the rejection", o.applyReport(r, t0.Add(-time.Second), t0.Add(time.Second)))
	requireKinds(t, "check started after the rejection", o.applyReport(r, t0.Add(time.Second), t0.Add(2*time.Second)), hlAgentAlertRecovered)

	errReport, err := f.run("http500", 0)
	if err != nil {
		t.Fatal(err)
	}
	requireKinds(t, "first failed check", o.applyReport(errReport, t0, t0))
	requireKinds(t, "failed for 23h", o.applyReport(errReport, t0.Add(23*time.Hour), t0.Add(23*time.Hour)))
	stale := o.applyReport(errReport, t0.Add(24*time.Hour), t0.Add(24*time.Hour))
	requireKinds(t, "failed for 24h", stale, hlAgentAlertStale)
	if strings.Contains(stale[0].Text, "CRITICAL**") {
		t.Fatalf("the stale warning is a CRITICAL: %q", stale[0].Text)
	}
	requireKinds(t, "success", o.applyReport(r, t0.Add(25*time.Hour), t0.Add(25*time.Hour)))
	requireKinds(t, "failed again after a success", o.applyReport(errReport, t0.Add(26*time.Hour), t0.Add(26*time.Hour)))
}

func TestHLAgentRejectionThroughEverySignedRunnerSendsOneCritical(t *testing.T) {
	f := newHLAgentFixture(t, hlAgentTestMaster)
	driver := filepath.Join(t.TempDir(), "rejecting.py")
	if err := os.WriteFile(driver, []byte(hlAgentRejectingRunnerDriver), 0o600); err != nil {
		t.Fatal(err)
	}

	_, _, _ = RunHyperliquidExecute(driver, "ETH", "buy", 1, 0, 0, 0, "cross", 1, hlCloseModeReduceOnly, hlExecuteSnapshot{})
	_, _, _ = RunHyperliquidUpdateStopLoss(driver, "ETH", "long", 1, 100, 0)
	_, _, _ = RunHyperliquidSyncProtection(driver, hlProtectionPlan{Symbol: "ETH"}, nil)
	_, _, _ = RunHyperliquidSizedClose(driver, hlSizedCloseRequest{Symbol: "ETH", Side: "sell", Mode: hlCloseModeReduceOnly, Size: 1})
	_, _, _ = RunHyperliquidClose(driver, "ETH", nil, nil)
	_, _, _ = RunHyperliquidLimitOpen(driver, "ETH", "buy", 1, 100, "Alo", "cross", 1, hlExecuteSnapshot{})
	_, _, _ = RunHyperliquidCancelOrder(driver, "ETH", 5)

	hlAgentStatus.mu.Lock()
	rejectedAt, bad := hlAgentStatus.lastRejectionAt, hlAgentStatus.bad
	hlAgentStatus.mu.Unlock()
	if !bad || rejectedAt.IsZero() {
		t.Fatalf("the runners did not record the rejection: bad=%v at=%v", bad, rejectedAt)
	}
	if got := f.criticals(); got != 1 {
		t.Fatalf("7 rejected runner calls sent %d CRITICAL alerts, want 1", got)
	}

	venueErr := "exchange rejected order: {'status': 'err', 'response': '" + hlAgentRejectionVenueText + "'}"
	for i := 0; i < 5; i++ {
		sc := StrategyConfig{ID: fmt.Sprintf("hl-agent-test-%d", i), Platform: "hyperliquid"}
		notifyLiveExecFailure(f.n, sc, directionOpen, "ETH", venueErr)
		t.Cleanup(func() { clearLiveExecThrottle(sc, directionOpen, "ETH") })
	}
	if got := f.criticals(); got != 1 {
		t.Fatalf("5 more failing strategies raised the CRITICAL count to %d, want 1", got)
	}
	f.mock.mu.Lock()
	generic := 0
	for _, dm := range f.mock.dms {
		if strings.Contains(dm.content, "**LIVE ORDER FAILED**") {
			generic++
		}
	}
	f.mock.mu.Unlock()
	if generic != 5 {
		t.Fatalf("the generic order-failure alert was sent %d times, want 5", generic)
	}

	hlAgentStatus = &hlAgentStatusOwner{}
	hlAgentStatus.bind(f.n)
	rh := StrategyConfig{ID: "rh-agent-test", Platform: "robinhood"}
	notifyLiveExecFailure(f.n, rh, directionOpen, "BTC", venueErr)
	t.Cleanup(func() { clearLiveExecThrottle(rh, directionOpen, "BTC") })
	if hlAgentStatus.bad {
		t.Fatal("a Robinhood failure set the Hyperliquid API wallet state")
	}
	if isHLAgentMissingRejection("order value below minimum value of $10") || isHLAgentMissingRejection("Too many open orders") {
		t.Fatal("the classifier matched another venue rejection")
	}
}
