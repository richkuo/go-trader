package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/gorilla/websocket"

	"trading-scheduler/internal/egresspolicy"
)

type EgressConfig struct {
	ProxyRequired       bool `json:"proxy_required"`
	NotificationsDirect bool `json:"notifications_direct"`
}

const (
	egressModeDisabled = "disabled"
	egressModeProxy    = "proxy"

	egressBootstrapScript    = "shared_tools/egress_bootstrap.py"
	egressOutcomePrefix      = "go-trader-egress-outcome "
	egressNoProxy            = "localhost,127.0.0.1,::1"
	egressAttestTimeout      = 10 * time.Second
	egressStartupTimeout     = 20 * time.Second
	egressAlertMinFailures   = 3
	egressAlertCheckEvery    = 30 * time.Second
	egressDiscordHTTPTimeout = 20 * time.Second
	egressDiscordWSTimeout   = 45 * time.Second
)

func (c *Config) egress() EgressConfig {
	if c == nil || c.Egress == nil {
		return EgressConfig{}
	}
	return *c.Egress
}

func egressConfigErrors(raw json.RawMessage) []string {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var ec EgressConfig
	if err := dec.Decode(&ec); err != nil {
		return []string{fmt.Sprintf("egress: %v (allowed keys: proxy_required, notifications_direct, both true or false)", err)}
	}
	return nil
}

func validateEgressJSON(data []byte) []string {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return nil
	}
	return egressConfigErrors(root["egress"])
}

func peekEgressConfig(path string) (*EgressConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, nil
	}
	if errs := egressConfigErrors(root["egress"]); len(errs) > 0 {
		return nil, errors.New(strings.Join(errs, "; "))
	}
	raw, ok := root["egress"]
	if !ok || string(raw) == "null" {
		return nil, nil
	}
	var ec EgressConfig
	if err := json.Unmarshal(raw, &ec); err != nil {
		return nil, err
	}
	return &ec, nil
}

func egressReloadErrors(cur, next *Config) []string {
	if cur.egress() != next.egress() {
		return []string{fmt.Sprintf("egress changed (%+v -> %+v; restart required: the proxy policy is frozen for the life of the process)", cur.egress(), next.egress())}
	}
	return nil
}

type egressRuntime struct {
	mu sync.Mutex

	resolved            bool
	active              bool
	notificationsDirect bool
	deployment          string
	endpoint            egresspolicy.Endpoint
	redactor            *egresspolicy.Redactor
	dialer              *egresspolicy.Dialer
	transport           *http.Transport
	attestation         *egresspolicy.Attestation

	lastSuccessAt       time.Time
	lastErrorAt         time.Time
	lastError           string
	consecutiveFailures int
	totalFailures       int64
	totalTunnels        int64

	childRequests      int64
	childFailures      int64
	childLastError     string
	childLastErrorAt   time.Time
	childLastSuccessAt time.Time

	lastAttestedAt time.Time
	attestError    string
}

var processEgress = &egressRuntime{}

type egressPolicyError struct {
	msg string
}

func (e *egressPolicyError) Error() string { return e.msg }

func egressRefusal(format string, a ...any) error {
	return &egressPolicyError{msg: fmt.Sprintf(format, a...)}
}

func resolveEgressPolicy(cfg *EgressConfig, rawProxy string) (bool, egresspolicy.Endpoint, error) {
	var ec EgressConfig
	if cfg != nil {
		ec = *cfg
	}
	if rawProxy == "" {
		if ec.ProxyRequired {
			return false, egresspolicy.Endpoint{}, egressRefusal("egress.proxy_required is true but %s is unset or empty", egresspolicy.EnvProxy)
		}
		if ec.NotificationsDirect {
			return false, egresspolicy.Endpoint{}, egressRefusal("egress.notifications_direct is true but proxy mode is off (%s is unset or empty); direct notifications only exist as an exception to proxy mode", egresspolicy.EnvProxy)
		}
		return false, egresspolicy.Endpoint{}, nil
	}
	ep, err := egresspolicy.ParseProxyURL(rawProxy)
	if err != nil {
		return false, egresspolicy.Endpoint{}, egressRefusal("%s is invalid: %v", egresspolicy.EnvProxy, err)
	}
	return true, ep, nil
}

func (r *egressRuntime) isActive() bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.active
}

func (r *egressRuntime) directNotifications() bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.active && r.notificationsDirect
}

func (r *egressRuntime) redact(s string) string {
	if r == nil {
		return s
	}
	r.mu.Lock()
	red := r.redactor
	r.mu.Unlock()
	return red.Redact(s)
}

func establishProcessEgress(ctx context.Context, cfg *EgressConfig, attest bool) error {
	r := processEgress
	r.mu.Lock()
	if r.resolved {
		r.mu.Unlock()
		return nil
	}
	r.mu.Unlock()
	raw := os.Getenv(egresspolicy.EnvProxy)
	active, ep, err := resolveEgressPolicy(cfg, raw)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resolved = true
	if !active {
		return nil
	}
	r.endpoint = ep
	r.redactor = egresspolicy.NewRedactor(ep)
	r.deployment = strings.TrimSpace(os.Getenv(egresspolicy.EnvDeployment))
	r.notificationsDirect = cfg != nil && cfg.NotificationsDirect
	if attest {
		actx, cancel := context.WithTimeout(ctx, egressAttestTimeout)
		a, err := egresspolicy.Attest(actx, r.deployment, egresspolicy.RoleWorker, ep.Identity())
		cancel()
		if err != nil {
			r.resolved = false
			return egressRefusal("proxy mode is on (%s) but enforcement is unavailable: %s. Run go-trader under the egress controller (see SKILL.md Egress proxy mode) or unset %s", ep.Redacted(), r.redactor.Redact(err.Error()), egresspolicy.EnvProxy)
		}
		if r.notificationsDirect && a.Notify == nil {
			r.resolved = false
			return egressRefusal("egress.notifications_direct is true but the egress controller runs no notification broker for deployment %q", r.deployment)
		}
		r.attestation = a
		r.lastAttestedAt = time.Now().UTC()
	}
	d := egresspolicy.NewDialer(ep)
	d.OnResult = r.recordTunnel
	r.dialer = d
	r.transport = d.Transport()
	r.active = true
	http.DefaultTransport = r.transport
	http.DefaultClient.Transport = r.transport
	websocket.DefaultDialer = d.WebsocketDialer(egressDiscordWSTimeout)
	return nil
}

func (r *egressRuntime) recordTunnel(target string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now().UTC()
	r.totalTunnels++
	if err == nil {
		r.lastSuccessAt = now
		r.consecutiveFailures = 0
		return
	}
	r.totalFailures++
	r.consecutiveFailures++
	r.lastErrorAt = now
	r.lastError = r.redactor.Redact(err.Error())
}

func egressHTTPClient(timeout time.Duration) *http.Client {
	c := &http.Client{Timeout: timeout}
	if t := processEgress.httpTransport(); t != nil {
		c.Transport = t
	}
	return c
}

func (r *egressRuntime) httpTransport() *http.Transport {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.active {
		return nil
	}
	return r.transport
}

func egressWebsocketDialer(handshake time.Duration) *websocket.Dialer {
	r := processEgress
	r.mu.Lock()
	d := r.dialer
	active := r.active
	r.mu.Unlock()
	if !active {
		return &websocket.Dialer{HandshakeTimeout: handshake}
	}
	return d.WebsocketDialer(handshake)
}

func pinDiscordSession(s *discordgo.Session) {
	if !processEgress.isActive() {
		return
	}
	s.Client = egressHTTPClient(egressDiscordHTTPTimeout)
	s.Dialer = egressWebsocketDialer(egressDiscordWSTimeout)
}

func egressPythonArgv(script string, args []string) []string {
	argv := make([]string, 0, len(args)+2)
	if processEgress.isActive() {
		argv = append(argv, egressBootstrapScript)
	}
	argv = append(argv, script)
	return append(argv, args...)
}

func (r *egressRuntime) childPolicyEnv() map[string]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.active {
		return nil
	}
	proxyURL := r.endpoint.RequestsURL()
	env := map[string]string{
		egresspolicy.EnvMode:  egresspolicy.ModeProxy,
		egresspolicy.EnvProxy: r.endpoint.Raw(),
		"NO_PROXY":            egressNoProxy,
		"no_proxy":            egressNoProxy,
	}
	for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		env[k] = proxyURL
	}
	if r.deployment != "" {
		env[egresspolicy.EnvDeployment] = r.deployment
	}
	return env
}

func egressChildEnvironment(overrides map[string]string) []string {
	policy := processEgress.childPolicyEnv()
	if policy == nil {
		if len(overrides) == 0 {
			return nil
		}
		return processEnvironment(overrides)
	}
	merged := make(map[string]string, len(overrides)+len(policy))
	for k, v := range overrides {
		merged[k] = v
	}
	for k, v := range policy {
		merged[k] = v
	}
	return processEnvironment(merged)
}

type egressChildOutcome struct {
	Requests      int64  `json:"requests"`
	ProxyFailures int64  `json:"proxy_failures"`
	LastError     string `json:"last_error"`
}

func (r *egressRuntime) recordChildOutcome(stderr []byte) []byte {
	if !r.isActive() || !bytes.Contains(stderr, []byte(egressOutcomePrefix)) {
		return stderr
	}
	var kept []string
	for _, line := range strings.Split(string(stderr), "\n") {
		idx := strings.Index(line, egressOutcomePrefix)
		if idx < 0 {
			kept = append(kept, line)
			continue
		}
		var o egressChildOutcome
		if json.Unmarshal([]byte(strings.TrimSpace(line[idx+len(egressOutcomePrefix):])), &o) != nil {
			continue
		}
		r.mu.Lock()
		now := time.Now().UTC()
		r.childRequests += o.Requests
		r.childFailures += o.ProxyFailures
		if o.ProxyFailures > 0 && o.LastError != "" {
			r.childLastError = r.redactor.Redact(o.LastError)
			r.childLastErrorAt = now
		}
		if o.Requests > o.ProxyFailures {
			r.childLastSuccessAt = now
		}
		r.mu.Unlock()
	}
	return []byte(strings.Join(kept, "\n"))
}

type egressStatus struct {
	Mode                string     `json:"mode"`
	Proxy               string     `json:"proxy,omitempty"`
	Deployment          string     `json:"deployment,omitempty"`
	NotificationsDirect bool       `json:"notifications_direct,omitempty"`
	Enforcement         string     `json:"enforcement,omitempty"`
	Generation          uint64     `json:"generation,omitempty"`
	LastSuccessAt       *time.Time `json:"last_success_at,omitempty"`
	LastErrorAt         *time.Time `json:"last_error_at,omitempty"`
	LastError           string     `json:"last_error,omitempty"`
	ConsecutiveFailures int        `json:"consecutive_failures,omitempty"`
	Tunnels             int64      `json:"tunnels,omitempty"`
	TunnelFailures      int64      `json:"tunnel_failures,omitempty"`
	ChildRequests       int64      `json:"child_requests,omitempty"`
	ChildProxyFailures  int64      `json:"child_proxy_failures,omitempty"`
	ChildLastError      string     `json:"child_last_error,omitempty"`
	ChildLastErrorAt    *time.Time `json:"child_last_error_at,omitempty"`
	ChildLastSuccessAt  *time.Time `json:"child_last_success_at,omitempty"`
	LastAttestedAt      *time.Time `json:"last_attested_at,omitempty"`
	AttestError         string     `json:"attest_error,omitempty"`
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func (r *egressRuntime) status() egressStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.active {
		return egressStatus{Mode: egressModeDisabled}
	}
	st := egressStatus{
		Mode:                egressModeProxy,
		Proxy:               r.endpoint.Redacted(),
		Deployment:          r.deployment,
		NotificationsDirect: r.notificationsDirect,
		LastSuccessAt:       timePtr(r.lastSuccessAt),
		LastErrorAt:         timePtr(r.lastErrorAt),
		LastError:           r.lastError,
		ConsecutiveFailures: r.consecutiveFailures,
		Tunnels:             r.totalTunnels,
		TunnelFailures:      r.totalFailures,
		ChildRequests:       r.childRequests,
		ChildProxyFailures:  r.childFailures,
		ChildLastError:      r.childLastError,
		ChildLastErrorAt:    timePtr(r.childLastErrorAt),
		ChildLastSuccessAt:  timePtr(r.childLastSuccessAt),
	}
	if r.attestation != nil {
		st.Enforcement = "attested"
		st.Generation = r.attestation.Generation
		st.LastAttestedAt = timePtr(r.lastAttestedAt)
		st.AttestError = r.attestError
		if r.attestError != "" {
			st.Enforcement = "unverified"
		}
	}
	return st
}

func urlHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

func egressStartupTargets(cfg *Config, feedRole bool) []string {
	set := map[string]bool{}
	hlHost := urlHost(hlMainnetURL)
	if os.Getenv("HYPERLIQUID_TESTNET") == "1" {
		hlHost = urlHost(hlTestnetURL)
	}
	if feedRole {
		set[hlHost] = true
	}
	if cfg != nil && !feedRole {
		for _, sc := range cfg.Strategies {
			switch sc.Platform {
			case "hyperliquid":
				set[hlHost] = true
			case "okx":
				set[urlHost(okxMainnetURL)] = true
			case "deribit":
				set[urlHost(deribitAPIBase)] = true
			}
		}
		if cfg.marketFeedWebsocketEnabled() {
			set[hlHost] = true
		}
	}
	if cfg != nil && !processEgress.directNotifications() {
		if cfg.Discord.Enabled {
			set["discord.com"] = true
		}
		if cfg.Telegram.Enabled {
			set[urlHost(telegramAPIBase)] = true
		}
	}
	delete(set, "")
	out := make([]string, 0, len(set))
	for h := range set {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

func egressStartupCheck(ctx context.Context, hosts []string) error {
	r := processEgress
	r.mu.Lock()
	d := r.dialer
	active := r.active
	r.mu.Unlock()
	if !active {
		return nil
	}
	for _, host := range hosts {
		cctx, cancel := context.WithTimeout(ctx, egressStartupTimeout)
		conn, err := d.DialContext(cctx, "tcp", net.JoinHostPort(host, "443"))
		if err != nil {
			cancel()
			return fmt.Errorf("startup check %s through the proxy: %s", host, r.redact(err.Error()))
		}
		tc := tls.Client(conn, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
		err = tc.HandshakeContext(cctx)
		_ = tc.Close()
		cancel()
		if err != nil {
			return fmt.Errorf("startup check %s through the proxy: TLS handshake: %s", host, r.redact(err.Error()))
		}
		fmt.Printf("[egress] startup check: %s reachable through %s\n", host, r.endpoint.Redacted())
	}
	return nil
}

func egressPythonFamilies(cfg *Config) []string {
	set := map[string]bool{"requests": true}
	if cfg != nil {
		for _, sc := range cfg.Strategies {
			switch sc.Platform {
			case "hyperliquid":
				set["hyperliquid"] = true
			case "robinhood":
				set["robinhood"] = true
				set["yfinance"] = true
			case "topstep":
				set["topstep"] = true
				set["yfinance"] = true
			case "deribit":
				set["deribit"] = true
				set["ccxt"] = true
			default:
				set["ccxt"] = true
			}
		}
	}
	out := make([]string, 0, len(set))
	for f := range set {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

func egressPythonPolicyCheck(ctx context.Context, families []string) error {
	if !processEgress.isActive() {
		return nil
	}
	args := append([]string{"--check-policy"}, families...)
	stdout, stderr, err := spawnPythonProcessRaw(ctx, ".venv/bin/python3", append([]string{egressBootstrapScript}, args...), nil, egressStartupTimeout, egressChildEnvironment(nil))
	if err != nil {
		detail := strings.TrimSpace(string(stdout))
		if detail == "" {
			detail = strings.TrimSpace(string(stderr))
		}
		return fmt.Errorf("python client policy check (%s): %s", strings.Join(families, ", "), processEgress.redact(detail))
	}
	fmt.Printf("[egress] python client policy check passed: %s\n", strings.Join(families, ", "))
	return nil
}

func formatEgressStartupLine() string {
	st := processEgress.status()
	if st.Mode != egressModeProxy {
		return "[egress] proxy mode: off"
	}
	notify := "through the proxy"
	if st.NotificationsDirect {
		notify = "direct through the isolated notification broker"
	}
	return fmt.Sprintf("[egress] proxy mode: on (%s, deployment %s, enforcement %s generation %d); notifications %s", st.Proxy, st.Deployment, st.Enforcement, st.Generation, notify)
}

type egressAlertState struct {
	alerting       bool
	lastNotifiedAt time.Time
}

func runEgressAlertMonitor(ctx context.Context, notify func(string)) {
	if !processEgress.isActive() || notify == nil {
		return
	}
	var state egressAlertState
	t := time.NewTicker(egressAlertCheckEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			if msg := state.evaluate(processEgress.status(), now); msg != "" {
				fmt.Println("[egress] " + msg)
				notify(msg)
			}
		}
	}
}

func (s *egressAlertState) evaluate(st egressStatus, now time.Time) string {
	failing := st.ConsecutiveFailures >= egressAlertMinFailures
	if failing {
		if !s.alerting || now.Sub(s.lastNotifiedAt) >= effectiveAlertThrottleInterval() {
			s.alerting = true
			s.lastNotifiedAt = now
			return fmt.Sprintf("**EGRESS PROXY FAILING** %s: %d consecutive tunnel failures; last error: %s. Venue calls fail or hold; no direct connection is attempted.", st.Proxy, st.ConsecutiveFailures, st.LastError)
		}
		return ""
	}
	if s.alerting && st.ConsecutiveFailures == 0 && st.LastSuccessAt != nil && st.LastSuccessAt.After(s.lastNotifiedAt) {
		s.alerting = false
		return fmt.Sprintf("**EGRESS PROXY RECOVERED** %s: tunnels succeed again.", st.Proxy)
	}
	return ""
}

const egressReattestEvery = 60 * time.Second

func runEgressAttestationMonitor(ctx context.Context, onLost func(error)) {
	r := processEgress
	r.mu.Lock()
	enabled := r.active && r.attestation != nil
	deployment, id := r.deployment, r.endpoint.Identity()
	r.mu.Unlock()
	if !enabled {
		return
	}
	t := time.NewTicker(egressReattestEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		actx, cancel := context.WithTimeout(ctx, egressAttestTimeout)
		a, err := egresspolicy.Attest(actx, deployment, egresspolicy.RoleWorker, id)
		cancel()
		r.mu.Lock()
		if err != nil {
			r.attestError = r.redactor.Redact(err.Error())
		} else {
			r.attestation = a
			r.lastAttestedAt = time.Now().UTC()
			r.attestError = ""
		}
		r.mu.Unlock()
		if err != nil && errors.Is(err, egresspolicy.ErrNamespaceMismatch) {
			onLost(err)
			return
		}
	}
}
