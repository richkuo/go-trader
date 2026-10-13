package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	basketReplayProtocolName    = "go-trader.basket-replay"
	basketReplayProtocolVersion = 1
	basketReplayMaxSpecBytes    = 1 << 30
	basketReplayMaxSteps        = 200000
	basketReplayModeNew         = "new"
	basketReplayModeResume      = "resume"
	basketReplayTargetsRanker   = "ranker"
	basketReplayTargetsBaseline = "baseline"
)

var basketReplayRunIDPattern = basketIDPattern

type basketReplaySpec struct {
	Protocol      string                         `json:"protocol"`
	Version       int                            `json:"version"`
	Mode          string                         `json:"mode"`
	Run           basketReplayRunSpec            `json:"run"`
	Basket        BasketConfig                   `json:"basket"`
	Members       []basketReplayMemberSpec       `json:"members"`
	PortfolioRisk *PortfolioRiskConfig           `json:"portfolio_risk,omitempty"`
	TargetInput   basketReplayTargetInput        `json:"target_input"`
	CostPolicy    *basketReplayCostPolicy        `json:"cost_policy,omitempty"`
	LotMetadata   map[string]int                 `json:"lot_metadata"`
	Candles       map[string][][]float64         `json:"candles"`
	Funding       map[string]basketReplayFunding `json:"funding,omitempty"`
	Steps         []basketReplayStep             `json:"steps"`
	parsedSteps   []time.Time
	parsedCandles map[marketFeedKey][]feedBar
	baseline      map[int64]map[string]float64
	funding       map[string]feedFundingCoverage
	memberMeta    map[string]basketReplayMemberSpec
}

type basketReplayRunSpec struct {
	RunID    string            `json:"run_id"`
	StateDir string            `json:"state_dir"`
	Source   map[string]string `json:"source"`
}

type basketReplayMemberSpec struct {
	ID        string                 `json:"id"`
	Coin      string                 `json:"coin"`
	Timeframe string                 `json:"timeframe"`
	Config    map[string]interface{} `json:"config"`
}

type basketReplayTargetInput struct {
	Mode    string                        `json:"mode"`
	Weights map[string]map[string]float64 `json:"weights,omitempty"`
}

type basketReplayCostPolicy struct {
	TakerFeePct float64 `json:"taker_fee_pct"`
	SlippagePct float64 `json:"slippage_pct"`
	FillRole    string  `json:"fill_role"`
	Spread      string  `json:"spread"`
}

type basketReplayFunding struct {
	FromMs  int64       `json:"from_ms"`
	ToMs    int64       `json:"to_ms"`
	Records [][]float64 `json:"records"`
}

type basketReplayStep struct {
	At          string                        `json:"at"`
	Marks       map[string]float64            `json:"marks"`
	Venue       []basketReplayScriptedOutcome `json:"venue,omitempty"`
	Queries     []basketReplayScriptedOutcome `json:"queries,omitempty"`
	SaveFailure bool                          `json:"save_failure,omitempty"`
}

type basketReplayScriptedOutcome struct {
	Outcome      string  `json:"outcome"`
	FillFraction float64 `json:"fill_fraction,omitempty"`
	Detail       string  `json:"detail,omitempty"`
}

const (
	basketReplayOutcomeFilled    = "filled"
	basketReplayOutcomePartial   = "partial"
	basketReplayOutcomeRejected  = "rejected"
	basketReplayOutcomeUnknown   = "unknown"
	basketReplayOutcomeTransport = "transport_error"
)

var basketReplayMemberConfigKeys = map[string]bool{
	"capital":                         true,
	"leverage":                        true,
	"max_drawdown_pct":                true,
	"circuit_breaker":                 true,
	"cb_drawdown_cooldown_minutes":    true,
	"cb_loss_streak_threshold":        true,
	"cb_loss_streak_cooldown_minutes": true,
	"close_strategy":                  true,
	"stop_loss_pct":                   true,
	"stop_loss_margin_pct":            true,
	"trailing_stop_pct":               true,
	"trailing_stop_atr_mult":          true,
	"stop_loss_atr_mult":              true,
	"trailing_stop_min_move_pct":      true,
	"margin_mode":                     true,
	"paused":                          true,
	"atr_method":                      true,
	"resting_tp_trade_through":        true,
}

type basketReplayRefusal struct {
	Reason string   `json:"reason"`
	Detail []string `json:"detail"`
}

func (r *basketReplayRefusal) Error() string {
	return r.Reason + ": " + strings.Join(r.Detail, "; ")
}

func refuseReplay(reason string, detail ...string) *basketReplayRefusal {
	return &basketReplayRefusal{Reason: reason, Detail: detail}
}

func decodeBasketReplaySpec(r io.Reader) (*basketReplaySpec, []byte, error) {
	raw, err := io.ReadAll(io.LimitReader(r, basketReplayMaxSpecBytes+1))
	if err != nil {
		return nil, nil, refuseReplay("spec_unreadable", err.Error())
	}
	if len(raw) > basketReplayMaxSpecBytes {
		return nil, nil, refuseReplay("spec_too_large", fmt.Sprintf("the step script exceeds %d bytes", basketReplayMaxSpecBytes))
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var spec basketReplaySpec
	if err := dec.Decode(&spec); err != nil {
		return nil, nil, refuseReplay("spec_malformed", err.Error())
	}
	if dec.More() {
		return nil, nil, refuseReplay("spec_malformed", "trailing data after the step script")
	}
	return &spec, raw, nil
}

func validateBasketReplaySpec(spec *basketReplaySpec) error {
	var errs []string
	add := func(format string, a ...interface{}) { errs = append(errs, fmt.Sprintf(format, a...)) }
	var baselineKeys map[int64]string
	if spec.Protocol != basketReplayProtocolName || spec.Version != basketReplayProtocolVersion {
		return refuseReplay("protocol_unsupported", fmt.Sprintf("the step script names protocol %q version %d; this adapter reads %q version %d", spec.Protocol, spec.Version, basketReplayProtocolName, basketReplayProtocolVersion))
	}
	if spec.Mode != basketReplayModeNew && spec.Mode != basketReplayModeResume {
		add("mode %q must be %q or %q", spec.Mode, basketReplayModeNew, basketReplayModeResume)
	}
	if !basketReplayRunIDPattern.MatchString(spec.Run.RunID) {
		add("run.run_id %q must match %s", spec.Run.RunID, basketReplayRunIDPattern.String())
	}
	if strings.TrimSpace(spec.Run.StateDir) == "" || !filepath.IsAbs(spec.Run.StateDir) || filepath.Clean(spec.Run.StateDir) != spec.Run.StateDir {
		add("run.state_dir %q must be an absolute, clean path", spec.Run.StateDir)
	}
	if len(spec.Run.Source) == 0 {
		add("run.source must record the frozen data provenance")
	}
	if len(spec.Members) < basketMinMembers {
		add("the replay needs at least %d members", basketMinMembers)
	}
	spec.memberMeta = make(map[string]basketReplayMemberSpec, len(spec.Members))
	coins := make(map[string]bool)
	listed := make(map[string]bool, len(spec.Basket.Members))
	for _, id := range spec.Basket.Members {
		listed[id] = true
	}
	for i, m := range spec.Members {
		label := fmt.Sprintf("members[%d]", i)
		if m.ID == "" || strings.ContainsAny(m.ID, " \t\n/") {
			add("%s id %q is not a valid strategy id", label, m.ID)
		}
		if _, dup := spec.memberMeta[m.ID]; dup {
			add("%s id %q is listed twice", label, m.ID)
		}
		spec.memberMeta[m.ID] = m
		if !listed[m.ID] {
			add("%s id %q is not listed in basket.members", label, m.ID)
		}
		if m.Coin == "" || strings.ContainsAny(m.Coin, "/: ") {
			add("%s coin %q must be a bare Hyperliquid perps coin", label, m.Coin)
		}
		if coins[m.Coin] {
			add("%s coin %s is used by two members", label, m.Coin)
		}
		coins[m.Coin] = true
		if _, ok := hlCandleIntervalMs(m.Timeframe); !ok {
			add("%s timeframe %q is not a Hyperliquid candle interval", label, m.Timeframe)
		}
		keys := make([]string, 0, len(m.Config))
		for k := range m.Config {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if !basketReplayMemberConfigKeys[k] {
				add("%s config field %q is not allowed: the adapter owns member identity, routing, mode and storage", label, k)
			}
		}
	}
	for _, id := range spec.Basket.Members {
		if _, ok := spec.memberMeta[id]; !ok {
			add("basket.members lists %q, which has no members entry", id)
		}
	}
	if spec.PortfolioRisk != nil && spec.PortfolioRisk.Paper != nil {
		add("portfolio_risk must not nest a paper block; it is the replay partition's own risk configuration")
	}
	switch spec.TargetInput.Mode {
	case basketReplayTargetsRanker:
		if len(spec.TargetInput.Weights) > 0 {
			add("target_input.weights is only valid in baseline mode")
		}
	case basketReplayTargetsBaseline:
		spec.baseline = make(map[int64]map[string]float64, len(spec.TargetInput.Weights))
		baselineKeys = make(map[int64]string, len(spec.TargetInput.Weights))
		dKeys := make([]string, 0, len(spec.TargetInput.Weights))
		for k := range spec.TargetInput.Weights {
			dKeys = append(dKeys, k)
		}
		sort.Strings(dKeys)
		for _, k := range dKeys {
			t, err := time.Parse(time.RFC3339, k)
			if err != nil {
				add("target_input.weights key %q is not an RFC 3339 deadline", k)
				continue
			}
			w := spec.TargetInput.Weights[k]
			for coin, v := range w {
				if !coins[coin] {
					add("target_input.weights[%s] names %s, which is not a member coin", k, coin)
				}
				if math.IsNaN(v) || math.IsInf(v, 0) {
					add("target_input.weights[%s][%s] is not finite", k, coin)
				}
			}
			ms := t.UTC().UnixMilli()
			if prev, dup := baselineKeys[ms]; dup {
				add("target_input.weights keys %q and %q name the same deadline", prev, k)
				continue
			}
			baselineKeys[ms] = k
			spec.baseline[ms] = w
		}
	default:
		add("target_input.mode %q must be %q or %q", spec.TargetInput.Mode, basketReplayTargetsRanker, basketReplayTargetsBaseline)
	}
	if spec.CostPolicy != nil {
		if err := validateFillCostRates(spec.CostPolicy.policy()); err != nil {
			add("cost_policy: %v", err)
		}
	}
	for coin := range coins {
		sz, ok := spec.LotMetadata[coin]
		if !ok {
			continue
		}
		if sz < 0 || sz > hlLotMetadataMaxSzDecimals {
			add("lot_metadata[%s] %d is outside 0..%d", coin, sz, hlLotMetadataMaxSzDecimals)
		}
	}
	for coin := range spec.LotMetadata {
		if !coins[coin] {
			add("lot_metadata names %s, which is not a member coin", coin)
		}
	}
	spec.parsedCandles = make(map[marketFeedKey][]feedBar)
	for key, rows := range spec.Candles {
		parts := strings.Split(key, "|")
		if len(parts) != 2 || !coins[parts[0]] {
			add("candles key %q must be <member coin>|<timeframe>", key)
			continue
		}
		ivl, ok := hlCandleIntervalMs(parts[1])
		if !ok {
			add("candles key %q names an unsupported timeframe", key)
			continue
		}
		bars, why := parseBasketReplayCandles(rows, ivl)
		if why != "" {
			add("candles[%s]: %s", key, why)
			continue
		}
		spec.parsedCandles[feedKeyFor(parts[0], parts[1])] = bars
	}
	for _, m := range spec.Members {
		if _, ok := spec.parsedCandles[feedKeyFor(m.Coin, m.Timeframe)]; !ok {
			add("candles has no %s|%s frame for member %s", m.Coin, m.Timeframe, m.ID)
		}
	}
	spec.funding = make(map[string]feedFundingCoverage, len(spec.Funding))
	for coin, f := range spec.Funding {
		if !coins[coin] {
			add("funding names %s, which is not a member coin", coin)
			continue
		}
		cov := feedFundingCoverage{Coin: coin, FromMs: f.FromMs, ToMs: f.ToMs}
		for i, rec := range f.Records {
			if len(rec) != 2 || rec[0] != math.Trunc(rec[0]) || math.IsNaN(rec[1]) || math.IsInf(rec[1], 0) {
				add("funding[%s].records[%d] must be [time_ms, finite rate]", coin, i)
				continue
			}
			cov.Records = append(cov.Records, feedFundingRecord{TimeMs: int64(rec[0]), Rate: rec[1]})
		}
		if err := validateFundingCoverageRecords(coin, cov.FromMs, cov.ToMs, cov.Records); err != nil {
			add("funding[%s]: %v", coin, err)
			continue
		}
		spec.funding[coin] = cov
	}
	if len(spec.Steps) == 0 {
		add("steps is empty")
	}
	if len(spec.Steps) > basketReplayMaxSteps {
		add("steps has %d entries, above the %d cap", len(spec.Steps), basketReplayMaxSteps)
	}
	var prev time.Time
	for i, st := range spec.Steps {
		t, err := time.Parse(time.RFC3339Nano, st.At)
		if err != nil {
			add("steps[%d].at %q is not RFC 3339", i, st.At)
			continue
		}
		t = t.UTC()
		if i > 0 && t.Before(prev) {
			add("steps[%d].at %s is earlier than the previous step %s; the simulated clock never moves back", i, t.Format(time.RFC3339Nano), prev.Format(time.RFC3339Nano))
		}
		prev = t
		spec.parsedSteps = append(spec.parsedSteps, t)
		for coin, px := range st.Marks {
			if !coins[coin] {
				add("steps[%d].marks names %s, which is not a member coin", i, coin)
			}
			if !finitePositive(px) {
				add("steps[%d].marks[%s] %v is not a finite positive price", i, coin, px)
			}
		}
		for j, o := range append(append([]basketReplayScriptedOutcome(nil), st.Venue...), st.Queries...) {
			if why := o.validate(); why != "" {
				add("steps[%d] scripted outcome %d: %s", i, j, why)
			}
		}
	}
	if len(baselineKeys) > 0 && spec.Basket.RebalanceIntervalSeconds > 0 {
		reached := make(map[int64]bool, len(spec.parsedSteps))
		window := time.Duration(spec.Basket.EvaluationWindowSeconds) * time.Second
		for _, t := range spec.parsedSteps {
			d := evaluationDeadline(t, spec.Basket.RebalanceIntervalSeconds)
			if window <= 0 || t.Before(d.Add(window)) {
				reached[d.UnixMilli()] = true
			}
		}
		deadlines := make([]int64, 0, len(baselineKeys))
		for ms := range baselineKeys {
			deadlines = append(deadlines, ms)
		}
		sort.Slice(deadlines, func(i, j int) bool { return deadlines[i] < deadlines[j] })
		for _, ms := range deadlines {
			if !reached[ms] {
				add("target_input.weights key %q is not a rebalance deadline (every %d s) that a step reaches inside its %d s evaluation window, so its weights would never be used", baselineKeys[ms], spec.Basket.RebalanceIntervalSeconds, spec.Basket.EvaluationWindowSeconds)
			}
		}
	}
	if len(errs) > 0 {
		return refuseReplay("spec_invalid", errs...)
	}
	return nil
}

func parseBasketReplayCandles(rows [][]float64, intervalMs int64) ([]feedBar, string) {
	if len(rows) == 0 {
		return nil, "no candles"
	}
	out := make([]feedBar, 0, len(rows))
	for i, r := range rows {
		if len(r) != 6 {
			return nil, fmt.Sprintf("row %d must be [open_ms, open, high, low, close, volume]", i)
		}
		if r[0] != math.Trunc(r[0]) || r[0] <= 0 {
			return nil, fmt.Sprintf("row %d open time %v is not a positive integer", i, r[0])
		}
		open := int64(r[0])
		if open%intervalMs != 0 {
			return nil, fmt.Sprintf("row %d open time %d is not aligned to the %d ms interval", i, open, intervalMs)
		}
		for _, v := range r[1:5] {
			if !finitePositive(v) {
				return nil, fmt.Sprintf("row %d price %v is not a finite positive number", i, v)
			}
		}
		if r[2] < math.Max(r[1], r[4]) || r[3] > math.Min(r[1], r[4]) {
			return nil, fmt.Sprintf("row %d high/low do not bound open and close", i)
		}
		if math.IsNaN(r[5]) || math.IsInf(r[5], 0) || r[5] < 0 {
			return nil, fmt.Sprintf("row %d volume %v is not finite and non-negative", i, r[5])
		}
		if i > 0 {
			prev := out[i-1].OpenMs
			if open != prev+intervalMs {
				return nil, fmt.Sprintf("row %d opens at %d, not %d: the frozen frame has a gap or is out of order", i, open, prev+intervalMs)
			}
		}
		out = append(out, feedBar{OpenMs: open, CloseMs: open + intervalMs - 1, HasClose: true, Open: r[1], High: r[2], Low: r[3], Close: r[4], Volume: r[5], Source: feedBarSource("replay")})
	}
	return out, ""
}

func (o basketReplayScriptedOutcome) validate() string {
	switch o.Outcome {
	case basketReplayOutcomeFilled, basketReplayOutcomeRejected, basketReplayOutcomeUnknown, basketReplayOutcomeTransport:
		if o.FillFraction != 0 {
			return fmt.Sprintf("fill_fraction is only valid for %q outcomes", basketReplayOutcomePartial)
		}
	case basketReplayOutcomePartial:
		if !(o.FillFraction > 0 && o.FillFraction < 1) {
			return fmt.Sprintf("a partial outcome needs fill_fraction in (0, 1), got %v", o.FillFraction)
		}
	default:
		return fmt.Sprintf("outcome %q must be filled, partial, rejected, unknown or transport_error", o.Outcome)
	}
	return ""
}

func (p *basketReplayCostPolicy) policy() fillCostPolicy {
	return fillCostPolicy{TakerFeePct: p.TakerFeePct, SlippagePct: p.SlippagePct, FillRole: p.FillRole, Spread: p.Spread}
}

type basketReplayFingerprints struct {
	Protocol string `json:"protocol"`
	Source   string `json:"source"`
	Contract string `json:"contract"`
	Input    string `json:"input"`
	Run      string `json:"run"`
	Config   string `json:"config"`
}

func basketReplayInputFingerprint(raw []byte) (string, error) {
	var generic map[string]interface{}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&generic); err != nil {
		return "", err
	}
	delete(generic, "mode")
	if run, ok := generic["run"].(map[string]interface{}); ok {
		delete(run, "state_dir")
	}
	canon, err := json.Marshal(generic)
	if err != nil {
		return "", err
	}
	return sha256Hex(canon), nil
}

func basketReplaySourceFingerprint(source map[string]string) string {
	blob, _ := json.Marshal(struct {
		Source       map[string]string `json:"source"`
		Version      string            `json:"adapter_version"`
		SourceCommit string            `json:"adapter_source_commit"`
	}{source, Version, SourceCommit})
	return sha256Hex(blob)
}

func basketReplayProtocolFingerprint() string {
	return fmt.Sprintf("%s/%d", basketReplayProtocolName, basketReplayProtocolVersion)
}

func (spec *basketReplaySpec) memberIntervalSeconds() int {
	best := 0
	for _, m := range spec.Members {
		ms, _ := hlCandleIntervalMs(m.Timeframe)
		s := int(ms / 1000)
		if best == 0 || s < best {
			best = s
		}
	}
	return best
}

func (spec *basketReplaySpec) configDocument(stateDir string) ([]byte, error) {
	var strategies []interface{}
	ids := make([]string, 0, len(spec.Members))
	for _, m := range spec.Members {
		ids = append(ids, m.ID)
	}
	sort.Strings(ids)
	for _, id := range ids {
		m := spec.memberMeta[id]
		sc := map[string]interface{}{}
		for k, v := range m.Config {
			sc[k] = v
		}
		sc["id"] = m.ID
		sc["basket_id"] = spec.Basket.ID
		sc["type"] = "perps"
		sc["platform"] = "hyperliquid"
		sc["script"] = hyperliquidCheckScript
		sc["args"] = []interface{}{basketMemberStrategyToken, m.Coin, m.Timeframe, "--mode=paper"}
		sc["closed_bar_decisions"] = true
		sc["direction"] = DirectionBoth
		strategies = append(strategies, sc)
	}
	root := map[string]interface{}{
		"config_version":   CurrentConfigVersion,
		"interval_seconds": spec.memberIntervalSeconds(),
		"db_file":          filepath.Join(stateDir, basketReplayDBName),
		"log_dir":          filepath.Join(stateDir, "logs"),
		"market_feed":      marketFeedWebsocket,
		"strategies":       strategies,
		"baskets":          []interface{}{spec.Basket},
	}
	if spec.PortfolioRisk != nil {
		root["portfolio_risk"] = spec.PortfolioRisk
	}
	return json.MarshalIndent(root, "", "  ")
}
