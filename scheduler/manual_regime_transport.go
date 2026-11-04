package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	manualRegimeProtocolVersion     = 1
	manualRegimeContractVersion     = 1
	manualRegimeOwnerMarkerVersion  = 1
	manualRegimeOwnerMarkerSuffix   = ".manual-regime-owner.json"
	manualRegimeOwnerMarkerMaxBytes = 4096
	manualRegimeOwnerNonceBytes     = 32
	manualRegimeChallengeHeader     = "X-Go-Trader-Manual-Challenge"
	manualRegimeAuthHeader          = "X-Go-Trader-Manual-Auth"
	manualRegimeProofHeader         = "X-Go-Trader-Manual-Proof"
	manualRegimeRoute               = "/api/regime/manual"
	manualSchedulerURLEnv           = "GO_TRADER_MANUAL_SCHEDULER_URL"
	manualRegimeConnectTimeout      = time.Second
	manualRegimeRequestTimeout      = 3 * time.Second
	manualRegimeMaxBodyBytes        = 1 << 20
	regimeBundlePendingErr          = "regime bundle unavailable"
)

const (
	manualRegimeResultBundle         = "bundle"
	manualRegimeResultUnavailable    = "unavailable"
	manualRegimeResultClassification = "classification_failure"
	manualRegimeResultDraining       = "draining"
	manualRegimeResultUnknown        = "unknown_strategy"
	manualRegimeResultMismatch       = "config_mismatch"
	manualRegimeResultInternal       = "internal_refusal"
	manualRegimeResultInvalid        = "invalid_request"
)

type manualRegimeTransportError struct {
	availability bool
	reason       string
}

func (e *manualRegimeTransportError) Error() string {
	if e.availability {
		return "scheduler regime bundle unavailable: " + e.reason
	}
	return "scheduler regime transport refused: " + e.reason
}

func manualRegimeUnavailable(format string, args ...any) *manualRegimeTransportError {
	return &manualRegimeTransportError{availability: true, reason: fmt.Sprintf(format, args...)}
}

func manualRegimeRefused(format string, args ...any) *manualRegimeTransportError {
	return &manualRegimeTransportError{reason: fmt.Sprintf(format, args...)}
}

func manualRegimeHardRefusal(err error) bool {
	var te *manualRegimeTransportError
	return errors.As(err, &te) && !te.availability
}

type manualRegimeOwnerMarker struct {
	Version    int      `json:"version"`
	OwnerNonce string   `json:"owner_nonce"`
	Secret     string   `json:"secret"`
	Role       string   `json:"role"`
	Partitions []string `json:"partitions"`
}

type manualRegimeOwnerIdentity struct {
	Nonce  string
	Secret string
}

func manualRegimeOwnerMarkerPath(dbPath string) string {
	return canonicalDBPath(dbPath) + manualRegimeOwnerMarkerSuffix
}

func manualRegimeMarkerFor(spec storageFileSpec, id manualRegimeOwnerIdentity) manualRegimeOwnerMarker {
	parts := make([]string, 0, len(spec.Partitions))
	for _, p := range spec.Partitions {
		parts = append(parts, p.String())
	}
	sort.Strings(parts)
	return manualRegimeOwnerMarker{Version: manualRegimeOwnerMarkerVersion, OwnerNonce: id.Nonce, Secret: id.Secret, Role: string(spec.Role), Partitions: parts}
}

func validManualRegimeNonce(nonce string) bool {
	if len(nonce) != 2*manualRegimeOwnerNonceBytes {
		return false
	}
	_, err := hex.DecodeString(nonce)
	return err == nil && strings.ToLower(nonce) == nonce
}

func newManualRegimeRandomHex() (string, error) {
	raw := make([]byte, manualRegimeOwnerNonceBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

func manualRegimeMAC(secret string, parts ...[]byte) string {
	key, _ := hex.DecodeString(secret)
	mac := hmac.New(sha256.New, key)
	for _, p := range parts {
		var n [8]byte
		l := uint64(len(p))
		for i := range n {
			n[i] = byte(l >> (56 - 8*i))
		}
		mac.Write(n[:])
		mac.Write(p)
	}
	return hex.EncodeToString(mac.Sum(nil))
}

func manualRegimeRequestMAC(secret, challenge, strategy, partition string) string {
	return manualRegimeMAC(secret, []byte("go-trader manual regime request v1"), []byte(challenge), []byte(strategy), []byte(partition))
}

func manualRegimeReplyMAC(secret, challenge string, status int, body []byte) string {
	return manualRegimeMAC(secret, []byte("go-trader manual regime reply v1"), []byte(challenge), []byte(strconv.Itoa(status)), body)
}

func manualRegimeMACEqual(got, want string) bool {
	return validManualRegimeNonce(got) && hmac.Equal([]byte(got), []byte(want))
}

var errManualRegimeMarkerAbsent = errors.New("owner marker absent")

func readManualRegimeOwnerMarker(path string) (manualRegimeOwnerMarker, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return manualRegimeOwnerMarker{}, errManualRegimeMarkerAbsent
	}
	if err != nil {
		return manualRegimeOwnerMarker{}, fmt.Errorf("owner marker %s unreadable: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return manualRegimeOwnerMarker{}, fmt.Errorf("owner marker %s is not a regular file (%s)", path, info.Mode().Type())
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return manualRegimeOwnerMarker{}, fmt.Errorf("owner marker %s unreadable: %w", path, err)
	}
	defer f.Close()
	held, err := f.Stat()
	if err != nil {
		return manualRegimeOwnerMarker{}, fmt.Errorf("owner marker %s unreadable: %w", path, err)
	}
	if !held.Mode().IsRegular() || !os.SameFile(info, held) {
		return manualRegimeOwnerMarker{}, fmt.Errorf("owner marker %s was replaced while it was read", path)
	}
	blob, err := io.ReadAll(io.LimitReader(f, manualRegimeOwnerMarkerMaxBytes+1))
	if err != nil {
		return manualRegimeOwnerMarker{}, fmt.Errorf("owner marker %s unreadable: %w", path, err)
	}
	if len(blob) > manualRegimeOwnerMarkerMaxBytes {
		return manualRegimeOwnerMarker{}, fmt.Errorf("owner marker %s exceeds %d bytes", path, manualRegimeOwnerMarkerMaxBytes)
	}
	var marker manualRegimeOwnerMarker
	if err := decodeManualRegimeStrict(blob, &marker); err != nil {
		return manualRegimeOwnerMarker{}, fmt.Errorf("owner marker %s is malformed: %w", path, err)
	}
	if marker.Version != manualRegimeOwnerMarkerVersion || !validManualRegimeNonce(marker.OwnerNonce) || !validManualRegimeNonce(marker.Secret) || marker.Secret == marker.OwnerNonce || marker.Role == "" {
		return manualRegimeOwnerMarker{}, fmt.Errorf("owner marker %s is malformed", path)
	}
	return marker, nil
}

func decodeManualRegimeStrict(blob []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(blob))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return fmt.Errorf("trailing data after the JSON value")
	}
	return nil
}

func writeManualRegimeOwnerMarker(path string, marker manualRegimeOwnerMarker) error {
	blob, err := json.Marshal(marker)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".manual-regime-owner-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	fail := func(err error) error {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		return fail(err)
	}
	if _, err := tmp.Write(blob); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}

func removeManualRegimeOwnerMarkerIfOwned(path, nonce string) {
	marker, err := readManualRegimeOwnerMarker(path)
	if err != nil || marker.OwnerNonce != nonce {
		return
	}
	os.Remove(path)
}

type manualRegimeOwner struct {
	mu        sync.RWMutex
	id        manualRegimeOwnerIdentity
	active    bool
	published []string
}

var globalManualRegimeOwner = &manualRegimeOwner{}

func (o *manualRegimeOwner) identity() (manualRegimeOwnerIdentity, bool) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.id, o.active
}

func (o *manualRegimeOwner) publish(layout storageLayout) error {
	nonce, err := newManualRegimeRandomHex()
	if err != nil {
		return fmt.Errorf("owner nonce: %w", err)
	}
	secret, err := newManualRegimeRandomHex()
	if err != nil {
		return fmt.Errorf("owner secret: %w", err)
	}
	id := manualRegimeOwnerIdentity{Nonce: nonce, Secret: secret}
	var written []string
	rollback := func() {
		for _, path := range written {
			removeManualRegimeOwnerMarkerIfOwned(path, nonce)
		}
	}
	for _, spec := range layout.Files {
		if spec.InMemory {
			continue
		}
		path := manualRegimeOwnerMarkerPath(spec.Path)
		if _, err := readManualRegimeOwnerMarker(path); err != nil && !errors.Is(err, errManualRegimeMarkerAbsent) {
			rollback()
			return err
		}
		if err := writeManualRegimeOwnerMarker(path, manualRegimeMarkerFor(spec, id)); err != nil {
			rollback()
			return fmt.Errorf("owner marker %s: %w", path, err)
		}
		written = append(written, path)
	}
	for _, path := range written {
		marker, err := readManualRegimeOwnerMarker(path)
		if err != nil || marker.OwnerNonce != nonce || marker.Secret != secret {
			rollback()
			return fmt.Errorf("owner marker %s did not read back after publication", path)
		}
	}
	if len(written) == 0 {
		return fmt.Errorf("no on-disk state file to publish an owner marker beside")
	}
	o.mu.Lock()
	o.id, o.active, o.published = id, true, written
	o.mu.Unlock()
	return nil
}

func (o *manualRegimeOwner) withdraw() {
	o.mu.Lock()
	nonce, paths := o.id.Nonce, o.published
	o.active, o.published = false, nil
	o.mu.Unlock()
	for _, path := range paths {
		removeManualRegimeOwnerMarkerIfOwned(path, nonce)
	}
}

func verifyManualRegimeOwner(layout storageLayout) (manualRegimeOwnerIdentity, *manualRegimeTransportError) {
	var id manualRegimeOwnerIdentity
	held, unheld, absent := 0, 0, 0
	for _, spec := range layout.Files {
		if spec.InMemory {
			continue
		}
		if _, ok := probeStateDBLockHolder(spec.Path); ok {
			held++
		} else {
			unheld++
			continue
		}
		path := manualRegimeOwnerMarkerPath(spec.Path)
		marker, err := readManualRegimeOwnerMarker(path)
		if errors.Is(err, errManualRegimeMarkerAbsent) {
			absent++
			continue
		}
		if err != nil {
			return manualRegimeOwnerIdentity{}, manualRegimeRefused("%v", err)
		}
		got := manualRegimeOwnerIdentity{Nonce: marker.OwnerNonce, Secret: marker.Secret}
		want := manualRegimeMarkerFor(spec, got)
		if marker.Role != want.Role || strings.Join(marker.Partitions, ",") != strings.Join(want.Partitions, ",") {
			return manualRegimeOwnerIdentity{}, manualRegimeRefused("owner marker %s belongs to a different storage layout", path)
		}
		if id.Nonce != "" && got != id {
			return manualRegimeOwnerIdentity{}, manualRegimeRefused("owner markers of the configured state files name different scheduler processes")
		}
		id = got
	}
	if held == 0 && unheld == 0 {
		return manualRegimeOwnerIdentity{}, manualRegimeRefused("the configured storage layout has no on-disk state file")
	}
	if held > 0 && unheld > 0 {
		return manualRegimeOwnerIdentity{}, manualRegimeRefused("only part of the configured state-file set is owned by a running scheduler")
	}
	if held == 0 {
		return manualRegimeOwnerIdentity{}, manualRegimeUnavailable("no scheduler owns the configured state files")
	}
	if absent > 0 {
		return manualRegimeOwnerIdentity{}, manualRegimeUnavailable("the owning scheduler has not published its identity yet")
	}
	return id, nil
}

type manualRegimeStrategyProjection struct {
	ID                        string                   `json:"id"`
	PaperSource               string                   `json:"paper_source"`
	Type                      string                   `json:"type"`
	Platform                  string                   `json:"platform"`
	Symbol                    string                   `json:"symbol"`
	Timeframe                 string                   `json:"timeframe"`
	Args                      []string                 `json:"args"`
	CloseStrategy             *StrategyRef             `json:"close_strategy"`
	AllowedRegimes            []string                 `json:"allowed_regimes"`
	RegimeGateOnFailure       string                   `json:"regime_gate_on_failure"`
	RegimeGateWindow          string                   `json:"regime_gate_window"`
	RegimeATRWindow           string                   `json:"regime_atr_window"`
	RegimeDirectionalWindow   string                   `json:"regime_directional_window"`
	Direction                 string                   `json:"direction"`
	Leverage                  float64                  `json:"leverage"`
	MarginMode                string                   `json:"margin_mode"`
	StopLossPct               *float64                 `json:"stop_loss_pct"`
	StopLossMarginPct         *float64                 `json:"stop_loss_margin_pct"`
	TrailingStopPct           *float64                 `json:"trailing_stop_pct"`
	TrailingStopATRMult       *float64                 `json:"trailing_stop_atr_mult"`
	StopLossATRMult           *float64                 `json:"stop_loss_atr_mult"`
	StopLossATRMultRegime     *RegimeATRBlock          `json:"stop_loss_atr_mult_regime"`
	TrailingStopATRMultRegime *RegimeATRBlock          `json:"trailing_stop_atr_mult_regime"`
	TrailingStopMinMovePct    *float64                 `json:"trailing_stop_min_move_pct"`
	RegimeDirectionalPolicy   *RegimeDirectionalPolicy `json:"regime_directional_policy"`
	RegimeWindowDivergence    *RegimeWindowDivergence  `json:"regime_window_divergence"`
	RegimeProfileAllocation   *RegimeProfileAllocation `json:"regime_profile_allocation"`
	IntervalSeconds           int                      `json:"interval_seconds"`
}

type manualRegimeRegimeProjection struct {
	Enabled       bool             `json:"enabled"`
	Period        int              `json:"period"`
	ADXThreshold  float64          `json:"adx_threshold"`
	Timeframe     string           `json:"timeframe"`
	Windows       RegimeWindowsMap `json:"windows"`
	GateOnFailure string           `json:"gate_on_failure"`
}

type manualRegimeKeyJSON struct {
	Platform  string `json:"platform"`
	Symbol    string `json:"symbol"`
	Timeframe string `json:"timeframe"`
	SpecJSON  string `json:"spec_json"`
}

func manualRegimeKeyFor(k regimeBundleKey) manualRegimeKeyJSON {
	return manualRegimeKeyJSON{Platform: k.Platform, Symbol: k.Symbol, Timeframe: k.Timeframe, SpecJSON: k.SpecJSON}
}

type manualRegimeRequestProjection struct {
	Key               manualRegimeKeyJSON `json:"key"`
	OhlcvLimit        int                 `json:"ohlcv_limit"`
	MinBars           int                 `json:"min_bars"`
	AllowSpotFallback bool                `json:"allow_spot_fallback"`
}

type manualRegimeContract struct {
	Version                int                            `json:"v"`
	StrategyID             string                         `json:"strategy_id"`
	StorageRole            string                         `json:"storage_role"`
	StorageID              string                         `json:"storage_id"`
	Partition              string                         `json:"partition"`
	Request                manualRegimeRequestProjection  `json:"request"`
	Strategy               manualRegimeStrategyProjection `json:"strategy"`
	Regime                 manualRegimeRegimeProjection   `json:"regime"`
	ATRMethod              string                         `json:"atr_method"`
	RatchetFallbackATRMult float64                        `json:"ratchet_fallback_atr_mult"`
	MarketFeed             string                         `json:"market_feed"`
	IntervalSeconds        int                            `json:"interval_seconds"`
}

type manualRegimeBinding struct {
	Contract    manualRegimeContract
	Fingerprint string
	Request     regimeBundleRequest
}

func manualRegimeStrategy(sc StrategyConfig) StrategyConfig {
	if len(sc.Args) < 3 {
		sc.Args = []string{"no_edge", sc.Symbol, resolveManualATRTimeframe(sc)}
	}
	return sc
}

func manualRegimeBindingFor(cfg *Config, ident storageIdentityMap, sc StrategyConfig) (manualRegimeBinding, error) {
	if cfg == nil || cfg.Regime == nil || !cfg.Regime.Enabled {
		return manualRegimeBinding{}, fmt.Errorf("regime processing is disabled")
	}
	if sc.Type != "manual" {
		return manualRegimeBinding{}, fmt.Errorf("strategy %q is not a manual strategy", sc.ID)
	}
	norm := manualRegimeStrategy(sc)
	req, ok := strategyRegimeBundleRequest(norm, cfg.Regime)
	if !ok {
		return manualRegimeBinding{}, fmt.Errorf("no regime bundle request")
	}
	storage, ok := ident.storageFor(sc.ID)
	if !ok {
		return manualRegimeBinding{}, fmt.Errorf("strategy %q has no storage identity", sc.ID)
	}
	allowed := append([]string(nil), norm.AllowedRegimes...)
	sort.Strings(allowed)
	rc := cfg.Regime
	contract := manualRegimeContract{
		Version:     manualRegimeContractVersion,
		StrategyID:  sc.ID,
		StorageRole: string(storage.Role),
		StorageID:   storage.StorageID,
		Partition:   storage.Partition.String(),
		Request: manualRegimeRequestProjection{
			Key:               manualRegimeKeyFor(req.Key),
			OhlcvLimit:        req.OhlcvLimit,
			MinBars:           req.MinBars,
			AllowSpotFallback: req.AllowSpotFallback,
		},
		Strategy: manualRegimeStrategyProjection{
			ID: norm.ID, PaperSource: norm.PaperSource, Type: norm.Type, Platform: norm.Platform, Symbol: norm.Symbol,
			Timeframe: norm.Timeframe, Args: append([]string(nil), norm.Args...), CloseStrategy: norm.CloseStrategy,
			AllowedRegimes: allowed, RegimeGateOnFailure: norm.RegimeGateOnFailure, RegimeGateWindow: norm.RegimeGateWindow,
			RegimeATRWindow: norm.RegimeATRWindow, RegimeDirectionalWindow: norm.RegimeDirectionalWindow,
			Direction: norm.Direction, Leverage: norm.Leverage, MarginMode: norm.MarginMode,
			StopLossPct: norm.StopLossPct, StopLossMarginPct: norm.StopLossMarginPct, TrailingStopPct: norm.TrailingStopPct,
			TrailingStopATRMult: norm.TrailingStopATRMult, StopLossATRMult: norm.StopLossATRMult,
			StopLossATRMultRegime: norm.StopLossATRMultRegime, TrailingStopATRMultRegime: norm.TrailingStopATRMultRegime,
			TrailingStopMinMovePct: norm.TrailingStopMinMovePct, RegimeDirectionalPolicy: norm.RegimeDirectionalPolicy,
			RegimeWindowDivergence: norm.RegimeWindowDivergence, RegimeProfileAllocation: norm.RegimeProfileAllocation,
			IntervalSeconds: norm.IntervalSeconds,
		},
		Regime: manualRegimeRegimeProjection{
			Enabled: rc.Enabled, Period: rc.Period, ADXThreshold: rc.ADXThreshold, Timeframe: rc.Timeframe,
			Windows: rc.Windows, GateOnFailure: rc.GateOnFailure,
		},
		ATRMethod:              resolveATRMethod(norm, cfg),
		RatchetFallbackATRMult: cfg.resolveManualRatchetFallbackATRMult(),
		MarketFeed:             cfg.marketFeedMode(),
		IntervalSeconds:        cfg.IntervalSeconds,
	}
	blob, err := json.Marshal(contract)
	if err != nil {
		return manualRegimeBinding{}, err
	}
	sum := sha256.Sum256(blob)
	return manualRegimeBinding{Contract: contract, Fingerprint: hex.EncodeToString(sum[:]), Request: req}, nil
}

func manualRegimeExportsFor(cfg *Config, store *StateStore, due []StrategyConfig) map[string]manualRegimeExport {
	if store == nil {
		return nil
	}
	out := make(map[string]manualRegimeExport)
	for _, sc := range due {
		if sc.Type != "manual" {
			continue
		}
		binding, err := manualRegimeBindingFor(cfg, store.ident, sc)
		if err != nil {
			continue
		}
		out[sc.ID] = manualRegimeExport{Fingerprint: binding.Fingerprint, Key: binding.Request.Key}
	}
	return out
}

type manualRegimeEnvelope struct {
	ProtocolVersion     int                  `json:"protocol_version"`
	Result              string               `json:"result"`
	Message             string               `json:"message,omitempty"`
	OwnerNonce          string               `json:"owner_nonce,omitempty"`
	StrategyID          string               `json:"strategy_id,omitempty"`
	Partition           string               `json:"partition,omitempty"`
	StorageRole         string               `json:"storage_role,omitempty"`
	StorageID           string               `json:"storage_id,omitempty"`
	ContractFingerprint string               `json:"contract_fingerprint,omitempty"`
	Key                 *manualRegimeKeyJSON `json:"key,omitempty"`
	Generation          uint64               `json:"generation,omitempty"`
	IntervalSeconds     int                  `json:"interval_seconds,omitempty"`
	BundleAt            *time.Time           `json:"bundle_at,omitempty"`
	Payload             json.RawMessage      `json:"payload,omitempty"`
}

func manualRegimeStatusFor(result string) int {
	switch result {
	case manualRegimeResultBundle:
		return http.StatusOK
	case manualRegimeResultInvalid:
		return http.StatusBadRequest
	case manualRegimeResultUnknown:
		return http.StatusNotFound
	case manualRegimeResultMismatch:
		return http.StatusConflict
	}
	return http.StatusServiceUnavailable
}

var manualRegimeMessages = map[string]string{
	manualRegimeResultBundle:         "current regime bundle",
	manualRegimeResultUnavailable:    "the current cycle holds no completed regime bundle for this strategy",
	manualRegimeResultClassification: "the current cycle's regime classification failed",
	manualRegimeResultDraining:       "the scheduler is draining",
	manualRegimeResultUnknown:        "this process owns no such manual strategy in that partition",
	manualRegimeResultMismatch:       "the bundle was produced under a different strategy or regime configuration",
	manualRegimeResultInternal:       "the scheduler cannot serve command admission",
	manualRegimeResultInvalid:        "strategy and partition are required",
}

type manualRegimeReplySigner func(status int, body []byte) string

func writeManualRegimeEnvelope(w http.ResponseWriter, env manualRegimeEnvelope, sign manualRegimeReplySigner) {
	env.ProtocolVersion = manualRegimeProtocolVersion
	env.Message = manualRegimeMessages[env.Result]
	blob, err := json.Marshal(env)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	status := manualRegimeStatusFor(env.Result)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if sign != nil {
		w.Header().Set(manualRegimeProofHeader, sign(status, blob))
	}
	w.WriteHeader(status)
	w.Write(blob)
}

type manualRegimeEndpoint struct {
	origin string
	host   string
	addrs  []string
}

func parseManualRegimeEndpoint(raw string) (manualRegimeEndpoint, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return manualRegimeEndpoint{}, fmt.Errorf("%s is not a URL: %v", manualSchedulerURLEnv, err)
	}
	if u.Scheme != "http" {
		return manualRegimeEndpoint{}, fmt.Errorf("%s must use http", manualSchedulerURLEnv)
	}
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" || (u.Path != "" && u.Path != "/") {
		return manualRegimeEndpoint{}, fmt.Errorf("%s must be a bare origin such as http://127.0.0.1:8099", manualSchedulerURLEnv)
	}
	host, port := u.Hostname(), u.Port()
	n, perr := strconv.Atoi(port)
	if port == "" || perr != nil || n < 1 || n > 65535 {
		return manualRegimeEndpoint{}, fmt.Errorf("%s must name a port", manualSchedulerURLEnv)
	}
	var ips []net.IP
	if strings.EqualFold(host, "localhost") {
		ctx, cancel := context.WithTimeout(context.Background(), manualRegimeConnectTimeout)
		defer cancel()
		resolved, lerr := net.DefaultResolver.LookupIPAddr(ctx, "localhost")
		if lerr != nil {
			return manualRegimeEndpoint{}, fmt.Errorf("resolve localhost: %v", lerr)
		}
		for _, a := range resolved {
			if a.IP.IsLoopback() {
				ips = append(ips, a.IP)
			}
		}
		sort.SliceStable(ips, func(i, j int) bool { return ips[i].To4() != nil && ips[j].To4() == nil })
		if len(ips) == 0 {
			return manualRegimeEndpoint{}, fmt.Errorf("localhost resolves to no loopback address")
		}
	} else {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return manualRegimeEndpoint{}, fmt.Errorf("%s must name a loopback address or localhost", manualSchedulerURLEnv)
		}
		ips = []net.IP{ip}
	}
	ep := manualRegimeEndpoint{origin: "http://" + net.JoinHostPort(host, port), host: net.JoinHostPort(host, port)}
	for _, ip := range ips {
		ep.addrs = append(ep.addrs, net.JoinHostPort(ip.String(), port))
	}
	return ep, nil
}

func manualRegimeDefaultEndpoint(cfg *Config) string {
	port := DefaultStatusPort
	if cfg != nil {
		port = resolveStatusPort(0, cfg.StatusPort)
	}
	return "http://localhost:" + strconv.Itoa(port)
}

func (ep manualRegimeEndpoint) client() *http.Client {
	dialer := &net.Dialer{Timeout: manualRegimeConnectTimeout}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var last error
			for _, addr := range ep.addrs {
				conn, err := dialer.DialContext(ctx, "tcp", addr)
				if err == nil {
					return conn, nil
				}
				last = err
			}
			return nil, last
		},
		DisableKeepAlives:      true,
		DisableCompression:     true,
		MaxResponseHeaderBytes: 64 << 10,
		ResponseHeaderTimeout:  manualRegimeRequestTimeout,
	}
	return &http.Client{
		Transport: transport,
		Timeout:   manualRegimeRequestTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func manualRegimeNetworkUnavailable(err error) bool {
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

type manualRegimeAdmissionRecord struct {
	layout   storageLayout
	nonce    string
	at       time.Time
	interval time.Duration
}

type manualRegimeRemote struct {
	cfg      *Config
	store    *StateStore
	now      func() time.Time
	admitted *manualRegimeAdmissionRecord
}

func newManualRegimeRemote(cfg *Config, store *StateStore) *manualRegimeRemote {
	return &manualRegimeRemote{cfg: cfg, store: store, now: time.Now}
}

func manualRegimeCadenceSeconds(cfg *Config, sc StrategyConfig) int {
	global := 0
	if cfg != nil {
		global = cfg.IntervalSeconds
	}
	return max(1, configuredStrategyIntervalSeconds(sc, global))
}

func manualRegimeMaxAge(cfg *Config, sc StrategyConfig) time.Duration {
	return time.Duration(manualRegimeCadenceSeconds(cfg, sc)) * time.Second
}

func (m *manualRegimeRemote) resolve(sc StrategyConfig) (RegimePayload, error) {
	m.admitted = nil
	if m.store == nil {
		return RegimePayload{}, manualRegimeRefused("no state store to verify the scheduler owner")
	}
	binding, err := manualRegimeBindingFor(m.cfg, m.store.ident, sc)
	if err != nil {
		return RegimePayload{}, manualRegimeRefused("%v", err)
	}
	raw := os.Getenv(manualSchedulerURLEnv)
	if strings.TrimSpace(raw) == "" {
		raw = manualRegimeDefaultEndpoint(m.cfg)
	}
	ep, err := parseManualRegimeEndpoint(raw)
	if err != nil {
		return RegimePayload{}, manualRegimeRefused("%v", err)
	}
	owner, terr := verifyManualRegimeOwner(m.store.layout)
	if terr != nil {
		return RegimePayload{}, terr
	}
	nonce := owner.Nonce
	challenge, err := newManualRegimeRandomHex()
	if err != nil {
		return RegimePayload{}, manualRegimeRefused("request challenge: %v", err)
	}
	q := url.Values{}
	q.Set("strategy", sc.ID)
	q.Set("partition", binding.Contract.Partition)
	ctx, cancel := context.WithTimeout(context.Background(), manualRegimeRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ep.origin+manualRegimeRoute+"?"+q.Encode(), nil)
	if err != nil {
		return RegimePayload{}, manualRegimeRefused("build request: %v", err)
	}
	req.Host = ep.host
	req.Header.Set(manualRegimeChallengeHeader, challenge)
	req.Header.Set(manualRegimeAuthHeader, manualRegimeRequestMAC(owner.Secret, challenge, sc.ID, binding.Contract.Partition))
	resp, err := ep.client().Do(req)
	if err != nil {
		if manualRegimeNetworkUnavailable(err) {
			return RegimePayload{}, manualRegimeUnavailable("scheduler at %s did not answer: %v", ep.origin, err)
		}
		return RegimePayload{}, manualRegimeRefused("request to %s failed: %v", ep.origin, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, manualRegimeMaxBodyBytes+1))
	if err != nil {
		if manualRegimeNetworkUnavailable(err) {
			return RegimePayload{}, manualRegimeUnavailable("scheduler at %s did not finish its reply: %v", ep.origin, err)
		}
		return RegimePayload{}, manualRegimeRefused("read reply: %v", err)
	}
	if len(body) > manualRegimeMaxBodyBytes {
		return RegimePayload{}, manualRegimeRefused("reply exceeds %d bytes", manualRegimeMaxBodyBytes)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return RegimePayload{}, manualRegimeRefused("the listener at %s rejected the owner authentication (HTTP 401)", ep.origin)
	}
	if !manualRegimeMACEqual(resp.Header.Get(manualRegimeProofHeader), manualRegimeReplyMAC(owner.Secret, challenge, resp.StatusCode, body)) {
		return RegimePayload{}, manualRegimeRefused("the reply from %s carries no valid owner proof; the listener is not the scheduler that owns the configured state files", ep.origin)
	}
	var env manualRegimeEnvelope
	envErr := decodeManualRegimeStrict(body, &env)
	switch resp.StatusCode {
	case http.StatusOK, http.StatusServiceUnavailable:
	default:
		if envErr == nil && env.ProtocolVersion == manualRegimeProtocolVersion && env.Result != "" && manualRegimeStatusFor(env.Result) == resp.StatusCode {
			return RegimePayload{}, manualRegimeRefused("the scheduler refused with HTTP %d (%s): %s", resp.StatusCode, env.Result, manualRegimeMessages[env.Result])
		}
		return RegimePayload{}, manualRegimeRefused("unexpected HTTP %d from %s", resp.StatusCode, ep.origin)
	}
	if envErr != nil {
		return RegimePayload{}, manualRegimeRefused("malformed reply: %v", envErr)
	}
	if env.ProtocolVersion != manualRegimeProtocolVersion {
		return RegimePayload{}, manualRegimeRefused("unsupported protocol version %d", env.ProtocolVersion)
	}
	if resp.StatusCode == http.StatusServiceUnavailable {
		switch env.Result {
		case manualRegimeResultUnavailable, manualRegimeResultClassification:
		case manualRegimeResultDraining:
			return RegimePayload{}, manualRegimeRefused("the scheduler is draining")
		default:
			return RegimePayload{}, manualRegimeRefused("scheduler refused with %q", env.Result)
		}
		if env.Payload != nil || env.BundleAt != nil || env.Generation != 0 {
			return RegimePayload{}, manualRegimeRefused("availability reply carries bundle fields")
		}
	} else if env.Result != manualRegimeResultBundle {
		return RegimePayload{}, manualRegimeRefused("HTTP 200 reply with result %q", env.Result)
	}
	if err := checkManualRegimeIdentity(env, nonce, binding); err != nil {
		return RegimePayload{}, err
	}
	again, terr := verifyManualRegimeOwner(m.store.layout)
	if terr != nil {
		if terr.availability {
			return RegimePayload{}, manualRegimeRefused("owner identity disappeared during the request: %s", terr.reason)
		}
		return RegimePayload{}, terr
	}
	if again != owner {
		return RegimePayload{}, manualRegimeRefused("owner identity changed during the request")
	}
	if env.Result == manualRegimeResultUnavailable {
		return RegimePayload{}, manualRegimeUnavailable("the scheduler holds no completed bundle for %s this cycle", binding.Request.Key)
	}
	if env.Result == manualRegimeResultClassification {
		return RegimePayload{}, manualRegimeUnavailable("the scheduler's regime classification for %s failed this cycle", binding.Request.Key)
	}
	if env.Generation == 0 || env.BundleAt == nil || env.BundleAt.IsZero() || len(env.Payload) == 0 {
		return RegimePayload{}, manualRegimeRefused("bundle reply is missing generation, timestamp or payload")
	}
	maxAge := manualRegimeMaxAge(m.cfg, sc)
	if cadence := manualRegimeCadenceSeconds(m.cfg, sc); env.IntervalSeconds != cadence {
		return RegimePayload{}, manualRegimeRefused("bundle interval %ds differs from the strategy's configured %ds", env.IntervalSeconds, cadence)
	}
	if err := checkManualRegimeAge(*env.BundleAt, m.now(), maxAge); err != nil {
		return RegimePayload{}, err
	}
	payload, err := decodeManualRegimePayload(binding.Request.Key, env.Payload)
	if err != nil {
		return RegimePayload{}, err
	}
	m.admitted = &manualRegimeAdmissionRecord{layout: m.store.layout, nonce: nonce, at: *env.BundleAt, interval: maxAge}
	return payload, nil
}

func checkManualRegimeIdentity(env manualRegimeEnvelope, nonce string, binding manualRegimeBinding) error {
	c := binding.Contract
	if env.OwnerNonce != nonce {
		return manualRegimeRefused("the reply came from a different scheduler process than the owner of the configured state files")
	}
	if env.StrategyID != c.StrategyID || env.Partition != c.Partition || env.StorageRole != c.StorageRole || env.StorageID != c.StorageID {
		return manualRegimeRefused("the reply names a different strategy, partition or storage identity")
	}
	if env.Key == nil || *env.Key != manualRegimeKeyFor(binding.Request.Key) {
		return manualRegimeRefused("the reply names a different bundle key")
	}
	if env.ContractFingerprint != binding.Fingerprint {
		return manualRegimeRefused("the scheduler's effective strategy or regime configuration differs from this command's configuration")
	}
	return nil
}

func checkManualRegimeAge(at, now time.Time, maxAge time.Duration) error {
	age := now.Sub(at)
	if age < 0 {
		return manualRegimeRefused("bundle timestamp %s is in the future", at.UTC().Format(time.RFC3339Nano))
	}
	if age > maxAge {
		return manualRegimeRefused("bundle timestamp %s is older than %s", at.UTC().Format(time.RFC3339Nano), maxAge)
	}
	return nil
}

func decodeManualRegimePayload(key regimeBundleKey, raw json.RawMessage) (RegimePayload, error) {
	var specs RegimeWindowsMap
	if err := json.Unmarshal([]byte(key.SpecJSON), &specs); err != nil {
		return RegimePayload{}, manualRegimeRefused("configured window specification is invalid: %v", err)
	}
	var windows map[string]json.RawMessage
	if err := json.Unmarshal(raw, &windows); err != nil || windows == nil {
		return RegimePayload{}, manualRegimeRefused("bundle payload is not a window map")
	}
	var payload RegimePayload
	if err := json.Unmarshal(raw, &payload); err != nil || !payload.MultiMode {
		return RegimePayload{}, manualRegimeRefused("bundle payload is malformed")
	}
	normalized, _ := normalizedRegimeWindows(specs, payload)
	return normalized, nil
}

func (m *manualRegimeRemote) confirm() error {
	rec := m.admitted
	if rec == nil {
		return nil
	}
	owner, terr := verifyManualRegimeOwner(rec.layout)
	if terr != nil {
		return manualRegimeRefused("owner identity no longer verifies: %s", terr.reason)
	}
	if owner.Nonce != rec.nonce {
		return manualRegimeRefused("the owning scheduler changed after admission")
	}
	return checkManualRegimeAge(rec.at, m.now(), rec.interval)
}

func sealedManualCommandFeed(cfg *Config) bool {
	return cfg != nil && cfg.marketFeedDeadlineScheduled()
}

func manualOpenRequiresATR(sc StrategyConfig, in manualOpenInputs) bool {
	mult := in.SLATRMult
	if mult == 0 && sc.StopLossATRMult != nil {
		mult = *sc.StopLossATRMult
	}
	if mult > 0 || strategyUsesTieredTPATRClose(sc) {
		return true
	}
	if strategyUsesTrailingTPRatchetClose(sc) && sc.TrailingStopATRMultRegime.IsConfigured() {
		return true
	}
	return strategyRegimeStopDependent(sc) && in.SLPct <= 0
}

func manualLimitRequiresATR(sc StrategyConfig) bool {
	return effectiveManualSLATRMult(sc) > 0 || strategyUsesTieredTPATRClose(sc) || strategyRegimeStopDependent(sc)
}

func validExplicitATR(atr float64) bool {
	return atr > 0 && !math.IsNaN(atr) && !math.IsInf(atr, 0)
}

func recoverSuppliedManualATR(label, strategyID, symbol string, supplied, fillPrice, leverage float64, notifier *MultiNotifier) float64 {
	if fb, ok := computeFallbackATR(fillPrice, leverage); ok && validExplicitATR(fb) && fb <= 0.5*fillPrice {
		warnNotifier(notifier, fmt.Sprintf("[%s] %s %s: supplied ATR=%.6f exceeds 50%% of the confirmed fill price %.4f; protection uses the arithmetic fallback ATR=%.6f (0.1*%.4f/%.2f lev) instead (no candle fetch)",
			label, strategyID, symbol, supplied, fillPrice, fb, fillPrice, leverage))
		return fb
	}
	warnNotifier(notifier, fmt.Sprintf("CRITICAL [%s] %s %s: supplied ATR=%.6f exceeds 50%% of the confirmed fill price %.4f and no plausible arithmetic fallback exists (leverage %.2f); the confirmed fill is kept with no ATR-based protection — arm a stop now",
		label, strategyID, symbol, supplied, fillPrice, leverage))
	return 0
}
