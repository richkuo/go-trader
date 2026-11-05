package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	hlAgentCheckInterval      = time.Hour
	hlAgentCheckTimeout       = 20 * time.Second
	hlAgentStaleAfter         = 24 * time.Hour
	hlAgentExpiryRecheckDelay = 30 * time.Second
	hlAgentNameLimit          = 40
	hlAgentExcerptLimit       = 160
	hlAgentDetailLimit        = 300
)

const (
	hlAgentAlertWarn      = "warn"
	hlAgentAlertUrgent    = "urgent"
	hlAgentAlertCritical  = "critical"
	hlAgentAlertRecovered = "recovered"
	hlAgentAlertStale     = "stale"
)

type hlAgentExpiryThreshold struct {
	lead time.Duration
	kind string
}

var hlAgentExpiryThresholds = []hlAgentExpiryThreshold{
	{lead: 14 * 24 * time.Hour, kind: hlAgentAlertWarn},
	{lead: 7 * 24 * time.Hour, kind: hlAgentAlertWarn},
	{lead: 3 * 24 * time.Hour, kind: hlAgentAlertUrgent},
	{lead: 24 * time.Hour, kind: hlAgentAlertUrgent},
}

type hlAgentState string

const (
	hlAgentStateValid           hlAgentState = "valid"
	hlAgentStateUnknownLifetime hlAgentState = "valid_unknown_lifetime"
	hlAgentStateMasterKey       hlAgentState = "master_key"
	hlAgentStateMissing         hlAgentState = "missing"
	hlAgentStateWrongMaster     hlAgentState = "wrong_master"
	hlAgentStateExpired         hlAgentState = "expired"
	hlAgentStateCheckError      hlAgentState = "check_error"
)

var (
	hlAgentStatusScript       = "shared_scripts/fetch_hl_agent_status.py"
	hlAgentHTTPTimeoutSeconds = 6.0
	hlAgentStatusProbeArgv    = []string{"--probe-only"}
	hlAgentAddressRE          = regexp.MustCompile(`^0x[0-9a-fA-F]{40}$`)
	hlAgentKeyLikeHexRE       = regexp.MustCompile(`(?i)(0x)?[0-9a-f]{64,}`)
	hlAgentKeyLikeDigitsRE    = regexp.MustCompile(`\d{60,}`)
	hlAgentRejectionRE        = regexp.MustCompile(`(?i)user or api wallet[^.\n]{0,120}?does not exist\.?`)
)

type hlAgentReport struct {
	SignerAddress       string `json:"signer_address"`
	MasterAddress       string `json:"master_address"`
	Network             string `json:"network"`
	Role                string `json:"role"`
	RoleUser            string `json:"role_user"`
	ValidUntilMs        int64  `json:"valid_until_ms"`
	AgentName           string `json:"agent_name"`
	ListedInExtraAgents bool   `json:"listed_in_extra_agents"`
	Error               string `json:"error"`
}

type hlAgentAlert struct {
	Kind string
	Text string
}

type hlAgentStatusOwner struct {
	mu               sync.Mutex
	notifier         *MultiNotifier
	bad              bool
	badState         hlAgentState
	lastCriticalAt   time.Time
	lastRejectionAt  time.Time
	sentThresholds   map[string]bool
	firstErrorAt     time.Time
	lastStaleWarnAt  time.Time
	lastValidUntilMs int64
}

var hlAgentStatus = &hlAgentStatusOwner{}

func isHLAgentMissingRejection(text string) bool {
	lower := strings.ToLower(text)
	return strings.Contains(lower, "user or api wallet") && strings.Contains(lower, "does not exist")
}

func redactHLKeyLike(text string) string {
	out := hlAgentKeyLikeHexRE.ReplaceAllString(text, "[redacted]")
	out = hlAgentKeyLikeDigitsRE.ReplaceAllString(out, "[redacted]")
	return truncHLAgentText(out, hlAgentDetailLimit)
}

func truncHLAgentText(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut]
}

func hlAgentRejectionExcerpt(text string) string {
	m := hlAgentRejectionRE.FindString(text)
	if m == "" {
		m = "User or API Wallet ... does not exist."
	}
	m = strings.NewReplacer("`", "", "*", "", "_", "", "~", "", "|", "", "@", "", "<", "", ">", "").Replace(m)
	return truncHLAgentText(redactHLKeyLike(m), hlAgentExcerptLimit)
}

func sanitizeHLAgentName(name string) string {
	var b strings.Builder
	for _, r := range name {
		if r < 0x20 || r > 0x7e {
			continue
		}
		switch r {
		case '`', '@', '<', '>', '*', '_', '~', '|', '\\':
			continue
		}
		b.WriteRune(r)
	}
	out := strings.TrimSpace(b.String())
	if len(out) > hlAgentNameLimit {
		out = strings.TrimSpace(out[:hlAgentNameLimit])
	}
	if out == "" {
		return "unnamed"
	}
	return out
}

func classifyHLAgentReport(r hlAgentReport, now time.Time) (hlAgentState, string) {
	if r.Error != "" {
		return hlAgentStateCheckError, r.Error
	}
	if !hlAgentAddressRE.MatchString(r.SignerAddress) || !hlAgentAddressRE.MatchString(r.MasterAddress) {
		return hlAgentStateCheckError, "the report has no valid signer or master address"
	}
	switch r.Role {
	case "missing":
		return hlAgentStateMissing, ""
	case "user":
		if strings.EqualFold(r.SignerAddress, r.MasterAddress) {
			return hlAgentStateMasterKey, ""
		}
		return hlAgentStateWrongMaster, ""
	case "agent":
		if !hlAgentAddressRE.MatchString(r.RoleUser) {
			return hlAgentStateCheckError, "the agent report has no valid master address"
		}
		if !strings.EqualFold(r.RoleUser, r.MasterAddress) {
			return hlAgentStateWrongMaster, ""
		}
		if !r.ListedInExtraAgents || r.ValidUntilMs <= 0 {
			return hlAgentStateUnknownLifetime, ""
		}
		if !now.Before(time.UnixMilli(r.ValidUntilMs)) {
			return hlAgentStateExpired, ""
		}
		return hlAgentStateValid, ""
	default:
		return hlAgentStateCheckError, fmt.Sprintf("unexpected role %q", r.Role)
	}
}

const hlAgentEffectLine = "Effect: no live Hyperliquid order, stop-loss or take-profit can be placed, changed or cancelled on this account until this is fixed."

const hlAgentFixLine = "Fix: create a new API wallet for the account on Hyperliquid, set HYPERLIQUID_SECRET_KEY to its key, then restart go-trader. The scheduler never creates or approves keys."

func hlAgentWalletLabel(r hlAgentReport) string {
	if r.AgentName != "" {
		return fmt.Sprintf("`%s` (`%s`)", sanitizeHLAgentName(r.AgentName), r.SignerAddress)
	}
	return fmt.Sprintf("`%s`", r.SignerAddress)
}

func formatHLAgentRemaining(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	days := int(d / (24 * time.Hour))
	hours := int((d % (24 * time.Hour)) / time.Hour)
	return fmt.Sprintf("%dd %dh", days, hours)
}

func hlAgentCriticalText(state hlAgentState, r hlAgentReport) string {
	var cause string
	switch state {
	case hlAgentStateMissing:
		cause = fmt.Sprintf("Hyperliquid does not know the API wallet `%s` that signs for account `%s`. The API wallet expired, was removed, or was never approved (an account that was never funded reads the same).", r.SignerAddress, r.MasterAddress)
	case hlAgentStateExpired:
		cause = fmt.Sprintf("The API wallet %s for account `%s` expired at %s.", hlAgentWalletLabel(r), r.MasterAddress, time.UnixMilli(r.ValidUntilMs).UTC().Format("2006-01-02 15:04 UTC"))
	case hlAgentStateWrongMaster:
		if r.Role == "user" {
			cause = fmt.Sprintf("The key `%s` is its own Hyperliquid account, but HYPERLIQUID_ACCOUNT_ADDRESS is `%s`.", r.SignerAddress, r.MasterAddress)
		} else {
			cause = fmt.Sprintf("The API wallet `%s` signs for account `%s`, but HYPERLIQUID_ACCOUNT_ADDRESS is `%s`.", r.SignerAddress, r.RoleUser, r.MasterAddress)
		}
		return fmt.Sprintf("**HL API WALLET CRITICAL** %s %s Fix: set HYPERLIQUID_ACCOUNT_ADDRESS to the account this key trades for, or create an API wallet for the configured account and set HYPERLIQUID_SECRET_KEY to its key; then restart go-trader. The scheduler never creates or approves keys.", cause, hlAgentEffectLine)
	}
	return fmt.Sprintf("**HL API WALLET CRITICAL** %s %s %s", cause, hlAgentEffectLine, hlAgentFixLine)
}

func hlAgentRejectionCriticalText(excerpt string) string {
	return fmt.Sprintf("**HL API WALLET CRITICAL** Hyperliquid rejected a signed action: \"%s\" The API wallet in HYPERLIQUID_SECRET_KEY expired, was removed, or was never approved. %s %s", excerpt, hlAgentEffectLine, hlAgentFixLine)
}

func hlAgentWarningText(r hlAgentReport, now time.Time) string {
	until := time.UnixMilli(r.ValidUntilMs)
	remaining := until.Sub(now)
	head := "**HL API WALLET EXPIRES IN UNDER 1 DAY**"
	switch days := int(remaining / (24 * time.Hour)); {
	case days == 1:
		head = "**HL API WALLET EXPIRES IN 1 DAY**"
	case days > 1:
		head = fmt.Sprintf("**HL API WALLET EXPIRES IN %d DAYS**", days)
	}
	return fmt.Sprintf("%s The API wallet %s for account `%s` expires at %s (%s left). After that, every live Hyperliquid order, stop-loss and take-profit fails. Before then, create a new API wallet for the account on Hyperliquid, set HYPERLIQUID_SECRET_KEY to its key, and restart go-trader.", head, hlAgentWalletLabel(r), r.MasterAddress, until.UTC().Format("2006-01-02 15:04 UTC"), formatHLAgentRemaining(remaining))
}

func (o *hlAgentStatusOwner) bind(n *MultiNotifier) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.notifier = n
}

func (o *hlAgentStatusOwner) markBadLocked(state hlAgentState, text string, now time.Time) []hlAgentAlert {
	if !o.bad {
		o.bad = true
		o.badState = state
		o.lastCriticalAt = now
		return []hlAgentAlert{{Kind: hlAgentAlertCritical, Text: text}}
	}
	o.badState = state
	if alertThrottleDue(o.lastCriticalAt, now) {
		o.lastCriticalAt = now
		return []hlAgentAlert{{Kind: hlAgentAlertCritical, Text: text}}
	}
	return nil
}

func (o *hlAgentStatusOwner) applyReport(r hlAgentReport, checkStartedAt, now time.Time) []hlAgentAlert {
	state, detail := classifyHLAgentReport(r, now)
	o.mu.Lock()
	defer o.mu.Unlock()
	if state == hlAgentStateCheckError {
		return o.applyCheckErrorLocked(detail, now)
	}
	o.firstErrorAt = time.Time{}
	o.lastStaleWarnAt = time.Time{}
	o.lastValidUntilMs = 0
	switch state {
	case hlAgentStateMissing, hlAgentStateWrongMaster, hlAgentStateExpired:
		return o.markBadLocked(state, hlAgentCriticalText(state, r), now)
	}
	var alerts []hlAgentAlert
	if o.bad && checkStartedAt.After(o.lastRejectionAt) {
		o.bad = false
		o.badState = ""
		o.lastCriticalAt = time.Time{}
		alerts = append(alerts, hlAgentAlert{Kind: hlAgentAlertRecovered, Text: fmt.Sprintf("**HL API WALLET RECOVERED** Hyperliquid knows the signing key `%s` for account `%s` again. Live orders, stop-losses and take-profits can be placed.", r.SignerAddress, r.MasterAddress)})
	}
	switch state {
	case hlAgentStateMasterKey:
		logOnChangef("hl-agent", string(state), "[hl-agent] HYPERLIQUID_SECRET_KEY is the account key of %s; it has no API wallet lifetime\n", r.MasterAddress)
	case hlAgentStateUnknownLifetime:
		logOnChangef("hl-agent", string(state), "[hl-agent] WARN: API wallet %s for account %s is valid, but Hyperliquid lists no lifetime for it; no expiry warning can be sent\n", r.SignerAddress, r.MasterAddress)
	case hlAgentStateValid:
		o.lastValidUntilMs = r.ValidUntilMs
		until := time.UnixMilli(r.ValidUntilMs)
		logOnChangef("hl-agent", fmt.Sprintf("%s|%s|%d", state, strings.ToLower(r.SignerAddress), r.ValidUntilMs), "[hl-agent] API wallet %s for account %s is valid until %s (%s left)\n", r.SignerAddress, r.MasterAddress, until.UTC().Format("2006-01-02 15:04 UTC"), formatHLAgentRemaining(until.Sub(now)))
		alerts = append(alerts, o.thresholdAlertsLocked(r, now)...)
	}
	return alerts
}

func (o *hlAgentStatusOwner) thresholdAlertsLocked(r hlAgentReport, now time.Time) []hlAgentAlert {
	if o.sentThresholds == nil {
		o.sentThresholds = make(map[string]bool)
	}
	prefix := fmt.Sprintf("%s|%d|", strings.ToLower(r.SignerAddress), r.ValidUntilMs)
	remaining := time.UnixMilli(r.ValidUntilMs).Sub(now)
	reached := -1
	for i, th := range hlAgentExpiryThresholds {
		if remaining <= th.lead {
			reached = i
		}
	}
	if reached < 0 {
		return nil
	}
	th := hlAgentExpiryThresholds[reached]
	key := prefix + th.lead.String()
	if o.sentThresholds[key] {
		return nil
	}
	for _, earlier := range hlAgentExpiryThresholds[:reached+1] {
		o.sentThresholds[prefix+earlier.lead.String()] = true
	}
	return []hlAgentAlert{{Kind: th.kind, Text: hlAgentWarningText(r, now)}}
}

func (o *hlAgentStatusOwner) applyCheckError(detail string, now time.Time) []hlAgentAlert {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.applyCheckErrorLocked(detail, now)
}

func (o *hlAgentStatusOwner) applyCheckErrorLocked(detail string, now time.Time) []hlAgentAlert {
	detail = redactHLKeyLike(detail)
	fmt.Fprintf(os.Stderr, "[hl-agent] WARN: Hyperliquid API wallet check failed: %s\n", detail)
	if o.firstErrorAt.IsZero() {
		o.firstErrorAt = now
	}
	if now.Sub(o.firstErrorAt) < hlAgentStaleAfter || !alertThrottleDue(o.lastStaleWarnAt, now) {
		return nil
	}
	o.lastStaleWarnAt = now
	return []hlAgentAlert{{Kind: hlAgentAlertStale, Text: fmt.Sprintf("**HL API WALLET CHECK FAILING** The Hyperliquid API wallet check has failed for %s. Last error: %s. No expiry warning can be sent while it fails. A live order that Hyperliquid rejects for a missing API wallet still sends a CRITICAL.", formatHLAgentRemaining(now.Sub(o.firstErrorAt)), detail)}}
}

func (o *hlAgentStatusOwner) observeRejection(text string, now time.Time) []hlAgentAlert {
	excerpt := hlAgentRejectionExcerpt(text)
	o.mu.Lock()
	defer o.mu.Unlock()
	o.lastRejectionAt = now
	return o.markBadLocked(hlAgentStateMissing, hlAgentRejectionCriticalText(excerpt), now)
}

func (o *hlAgentStatusOwner) nextCheckDelay(now time.Time) time.Duration {
	o.mu.Lock()
	validUntilMs := o.lastValidUntilMs
	o.mu.Unlock()
	delay := hlAgentCheckInterval
	if validUntilMs > 0 {
		if untilExpiry := time.UnixMilli(validUntilMs).Sub(now) + hlAgentExpiryRecheckDelay; untilExpiry > 0 && untilExpiry < delay {
			delay = untilExpiry
		}
	}
	return delay
}

func (o *hlAgentStatusOwner) deliver(n *MultiNotifier, alerts []hlAgentAlert) {
	if len(alerts) == 0 {
		return
	}
	if n == nil {
		o.mu.Lock()
		n = o.notifier
		o.mu.Unlock()
	}
	for _, a := range alerts {
		fmt.Fprintf(os.Stderr, "[hl-agent] %s: %s\n", strings.ToUpper(a.Kind), a.Text)
		if n == nil || !n.HasBackends() {
			continue
		}
		if n.HasOwner() {
			n.SendOwnerDM(a.Text)
		} else {
			n.SendToAllChannels(a.Text)
		}
	}
}

func observeHLAgentScriptOutput(stdout, stderr []byte) {
	if !isHLAgentMissingRejection(string(stdout)) && !isHLAgentMissingRejection(string(stderr)) {
		return
	}
	hlAgentStatus.deliver(nil, hlAgentStatus.observeRejection(string(stdout)+"\n"+string(stderr), time.Now().UTC()))
}

func noteHLAgentRejection(n *MultiNotifier, sc StrategyConfig, text string) {
	if sc.Platform != "hyperliquid" || !isHLAgentMissingRejection(text) {
		return
	}
	hlAgentStatus.deliver(n, hlAgentStatus.observeRejection(text, time.Now().UTC()))
}

func parseHLAgentReport(stdout, stderr []byte, runErr error) (hlAgentReport, error) {
	var r hlAgentReport
	if jsonErr := json.Unmarshal(stdout, &r); jsonErr != nil {
		if runErr != nil {
			return r, errors.New(redactHLKeyLike(fmt.Sprintf("script error: %v (stderr: %s)", runErr, strings.TrimSpace(string(stderr)))))
		}
		return r, errors.New(redactHLKeyLike(fmt.Sprintf("parse output: %v", jsonErr)))
	}
	if r.Error == "" && runErr != nil {
		r.Error = fmt.Sprintf("script error: %v", runErr)
	}
	return r, nil
}

func runHLAgentStatusCheck(timeout time.Duration) (hlAgentReport, error) {
	args := []string{fmt.Sprintf("--timeout=%g", hlAgentHTTPTimeoutSeconds)}
	stdout, stderr, err := runPythonWithTimeout(shutdownReadOnlyCtx, hlAgentStatusScript, args, nil, timeout)
	return parseHLAgentReport(stdout, stderr, err)
}

func (o *hlAgentStatusOwner) runCheck(n *MultiNotifier, timeout time.Duration) time.Duration {
	started := time.Now().UTC()
	r, err := runHLAgentStatusCheck(timeout)
	now := time.Now().UTC()
	var alerts []hlAgentAlert
	if err != nil {
		alerts = o.applyCheckError(err.Error(), now)
	} else {
		alerts = o.applyReport(r, started, now)
	}
	o.deliver(n, alerts)
	return o.nextCheckDelay(now)
}

func anyHyperliquidLive(cfg *Config) bool {
	if cfg == nil {
		return false
	}
	for _, sc := range cfg.Strategies {
		if sc.Platform == "hyperliquid" && hyperliquidIsLive(sc.Args) {
			return true
		}
	}
	return false
}

func hlAgentStatusEnabled(cfg *Config) bool {
	return anyHyperliquidLive(cfg) && os.Getenv("HYPERLIQUID_SECRET_KEY") != ""
}

func runHLAgentStatusStartup(n *MultiNotifier) time.Duration {
	fmt.Println("[hl-agent] checking the Hyperliquid API wallet")
	return hlAgentStatus.runCheck(n, hlAgentCheckTimeout)
}

func runHLAgentStatusMonitor(ctx context.Context, n *MultiNotifier, first time.Duration) {
	delay := first
	for {
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		delay = hlAgentStatus.runCheck(n, hlAgentCheckTimeout)
	}
}
