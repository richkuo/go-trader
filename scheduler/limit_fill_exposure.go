package main

import (
	"fmt"
	"math"
	"os"
	"strings"
	"sync"
	"time"
)

const limitFillExposureEpsilon = 1e-9

type limitFillExposureVerdict int

const (
	limitFillExposureUnreadable limitFillExposureVerdict = iota
	limitFillExposureUnbacked
	limitFillExposureLive
)

type limitFillExposureDecision struct {
	Verdict     limitFillExposureVerdict
	Reason      string
	BookedNet   float64
	SignedDelta float64
	RequiredNet float64
	OnChainNet  float64
	Owners      int
	Legs        int
}

func (d limitFillExposureDecision) adopts() bool {
	return d.Verdict == limitFillExposureLive
}

func hlSignedQty(side string, qty float64) float64 {
	if side == "short" {
		return -qty
	}
	return qty
}

func hyperliquidOnChainNetForCoin(positions []HLPosition, coin string) float64 {
	if coin == "" {
		return 0
	}
	net := 0.0
	for _, p := range positions {
		if p.Coin == coin {
			net += p.Size
		}
	}
	return net
}

func hyperliquidLiveExposureStrategies(strategies []StrategyConfig) []StrategyConfig {
	out := make([]StrategyConfig, 0, len(strategies))
	seen := make(map[string]bool, len(strategies))
	for _, sc := range strategies {
		if sc.Platform != "hyperliquid" {
			continue
		}
		if sc.Type != "perps" && sc.Type != "manual" {
			continue
		}
		if !hyperliquidIsLive(sc.Args) {
			continue
		}
		if seen[sc.ID] {
			continue
		}
		seen[sc.ID] = true
		out = append(out, sc)
	}
	return out
}

func hyperliquidBookedSignedNetForCoin(strategies []StrategyConfig, state *AppState, coin string) float64 {
	if state == nil || coin == "" {
		return 0
	}
	net := 0.0
	for _, sc := range hyperliquidLiveExposureStrategies(strategies) {
		ss := state.Strategies[sc.ID]
		if ss == nil {
			continue
		}
		pos := ss.Positions[coin]
		if pos == nil || pos.Quantity <= 0 {
			continue
		}
		net += hlSignedQty(pos.Side, pos.Quantity)
	}
	return net
}

func hyperliquidLiveOwnersForCoin(strategies []StrategyConfig, coin string) int {
	if coin == "" {
		return 0
	}
	owners := 0
	for _, sc := range hyperliquidLiveExposureStrategies(strategies) {
		if sc.Symbol == coin || hyperliquidSymbol(sc.Args) == coin {
			owners++
		}
	}
	return owners
}

func classifyLimitFillLiveExposure(coin string, bookedNet, signedDelta, onChainNet float64, readErr error, owners int) limitFillExposureDecision {
	d := limitFillExposureDecision{
		BookedNet:   bookedNet,
		SignedDelta: signedDelta,
		OnChainNet:  onChainNet,
		RequiredNet: bookedNet + signedDelta,
		Owners:      owners,
	}
	if readErr != nil {
		d.Verdict = limitFillExposureUnreadable
		d.Reason = fmt.Sprintf("the live Hyperliquid account state could not be read (%v), so the fill has no evidence of live exposure", readErr)
		return d
	}
	if math.Abs(signedDelta) <= limitFillExposureEpsilon {
		d.Verdict = limitFillExposureUnreadable
		d.Reason = "the fill delta is zero, so there is nothing to confirm"
		return d
	}
	if math.Abs(onChainNet) <= limitFillExposureEpsilon {
		d.Verdict = limitFillExposureUnbacked
		d.Reason = fmt.Sprintf("the account holds no %s position at all, so the order's fill of %+.6f is no longer on the exchange", coin, signedDelta)
		return d
	}
	if math.Abs(d.RequiredNet) <= limitFillExposureEpsilon || (d.RequiredNet > 0) != (onChainNet > 0) {
		d.Verdict = limitFillExposureUnbacked
		d.Reason = fmt.Sprintf("the account holds %+.6f where the book would hold %+.6f after this fill, and the two do not agree on direction", onChainNet, d.RequiredNet)
		return d
	}
	if math.Abs(d.RequiredNet) > math.Abs(onChainNet)+limitFillExposureEpsilon {
		d.Verdict = limitFillExposureUnbacked
		d.Reason = fmt.Sprintf("the account holds %+.6f where the book would hold %+.6f after this fill, so the exchange does not carry the exposure the fill claims", onChainNet, d.RequiredNet)
		return d
	}
	d.Verdict = limitFillExposureLive
	return d
}

type hlLiveExposureReader struct {
	held      bool
	fetchedAt time.Time
	positions []HLPosition
	err       error
}

var fetchHyperliquidStateFn = fetchHyperliquidState

func (r *hlLiveExposureReader) fetch() ([]HLPosition, error) {
	r.held = true
	r.fetchedAt = time.Now()
	addr := os.Getenv("HYPERLIQUID_ACCOUNT_ADDRESS")
	if addr == "" {
		r.positions, r.err = nil, fmt.Errorf("HYPERLIQUID_ACCOUNT_ADDRESS is not set")
		return nil, r.err
	}
	_, positions, err := fetchHyperliquidStateFn(addr)
	if err != nil {
		r.positions, r.err = nil, err
		return nil, err
	}
	r.positions, r.err = positions, nil
	return r.positions, nil
}

func (r *hlLiveExposureReader) snapshotNewerThan(t time.Time) ([]HLPosition, error) {
	if r.held && r.err == nil && r.fetchedAt.After(t) {
		return r.positions, r.err
	}
	return r.fetch()
}

type limitFillExposureKey struct {
	strategyID string
	symbol     string
	orderOID   int64
}

type limitFillExposureSlot struct {
	lastNotifiedAt   time.Time
	notifiedSeverity int
}

type limitFillExposureTracker struct {
	mu      sync.Mutex
	entries map[limitFillExposureKey]*limitFillExposureSlot
}

var limitFillExposureAlerts = &limitFillExposureTracker{}

func limitFillExposureKeyFor(o PendingLimitOrder) limitFillExposureKey {
	return limitFillExposureKey{strategyID: o.StrategyID, symbol: o.Symbol, orderOID: o.OrderOID}
}

func limitFillExposureSeverity(v limitFillExposureVerdict) int {
	switch v {
	case limitFillExposureUnbacked:
		return 2
	case limitFillExposureUnreadable:
		return 1
	default:
		return 0
	}
}

func (t *limitFillExposureTracker) Record(k limitFillExposureKey, v limitFillExposureVerdict, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.entries == nil {
		t.entries = make(map[limitFillExposureKey]*limitFillExposureSlot)
	}
	e := t.entries[k]
	if e == nil {
		e = &limitFillExposureSlot{}
		t.entries[k] = e
	}
	severity := limitFillExposureSeverity(v)
	windowOpen := !e.lastNotifiedAt.IsZero() && now.Sub(e.lastNotifiedAt) < effectiveAlertThrottleInterval()
	if windowOpen && severity <= e.notifiedSeverity {
		return false
	}
	e.lastNotifiedAt = now
	e.notifiedSeverity = severity
	return true
}

func (t *limitFillExposureTracker) Retain(orders []PendingLimitOrder) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.entries) == 0 {
		return
	}
	live := make(map[limitFillExposureKey]bool, len(orders))
	for _, o := range orders {
		live[limitFillExposureKeyFor(o)] = true
	}
	for k := range t.entries {
		if !live[k] {
			delete(t.entries, k)
		}
	}
}

func (t *limitFillExposureTracker) Clear(k limitFillExposureKey) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.entries, k)
}

func (t *limitFillExposureTracker) reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.entries = make(map[limitFillExposureKey]*limitFillExposureSlot)
}

func limitFillExposureHeadline(v limitFillExposureVerdict) string {
	if v == limitFillExposureUnbacked {
		return "limit-order fill NOT booked: the exchange no longer carries it"
	}
	return "limit-order fill NOT booked: the account state is unreadable"
}

func formatLimitFillExposureDM(o PendingLimitOrder, exchangeFill float64, d limitFillExposureDecision) string {
	var b strings.Builder
	if d.Verdict == limitFillExposureUnbacked {
		fmt.Fprintf(&b, "🚨 **LIMIT FILL REFUSED — NO LIVE EXPOSURE: %s**\n", killSwitchLimitOrderLabel(o))
		fmt.Fprintf(&b, "The order reports a filled size of %.6f %s and the book holds %.6f, but %s.\n",
			exchangeFill, o.Symbol, o.FilledSize, d.Reason)
		b.WriteString("• Order fill history never changes after a fill, so it is NOT evidence that the position is still open — the live account state is\n")
		b.WriteString("• The scheduler booked NOTHING and armed NO protection, so it does not report a position that is not on the exchange\n")
		b.WriteString("• The queue row is KEPT as the recovery record, because no automatic path may delete a row carrying an unbooked fill\n")
		if d.Owners > 1 {
			fmt.Fprintf(&b, "• %d live strategies share %s, so the account's net size cannot be attributed to this order on its own and the check refuses rather than guesses\n", d.Owners, o.Symbol)
		}
		fmt.Fprintf(&b, "• If you already flattened this position by hand, clear the record with `go-trader manual-clear-limit-row %d --flattened`. Until you do, this alert repeats every %s\n",
			o.OrderOID, effectiveAlertThrottleInterval())
		if d.Legs > 1 {
			fmt.Fprintf(&b, "• %d pending limit fills on %s are decided TOGETHER, because the account's net size for a coin cannot be attributed to one order — the exchange must back every one of them or the scheduler books none, so the outcome never depends on the order the rows are read in\n", d.Legs, o.Symbol)
			fmt.Fprintf(&b, "• If these positions ARE open on Hyperliquid, check that the account holds %+.6f or more of %s in the same direction across every live leg — the scheduler adopts them all on the next cycle once it does",
				d.RequiredNet, o.Symbol)
		} else {
			fmt.Fprintf(&b, "• If the position IS open on Hyperliquid, check that the account holds %+.6f or more of %s in the same direction — the scheduler adopts the fill on the next cycle once it does",
				d.RequiredNet, o.Symbol)
		}
		return b.String()
	}
	fmt.Fprintf(&b, "⚠️ **Limit fill deferred — account state unreadable: %s**\n", killSwitchLimitOrderLabel(o))
	fmt.Fprintf(&b, "The order reports a filled size of %.6f %s and the book holds %.6f, but %s.\n",
		exchangeFill, o.Symbol, o.FilledSize, d.Reason)
	b.WriteString("• The scheduler booked NOTHING and deleted NOTHING — the queue row is kept and the decision is deferred\n")
	b.WriteString("• The reconciler retries every cycle and adopts the fill as soon as it can confirm the exposure on the exchange\n")
	b.WriteString("• Check that HYPERLIQUID_ACCOUNT_ADDRESS is set and that the Hyperliquid info endpoint is reachable")
	return b.String()
}

func reportLimitFillExposureRefusal(notifier *MultiNotifier, o PendingLimitOrder, exchangeFill float64, d limitFillExposureDecision, now time.Time) string {
	fmt.Printf("[CRITICAL] limit: %s %s: exchange fill %.6f, book %.6f — %s\n",
		limitFillExposureHeadline(d.Verdict), killSwitchLimitOrderLabel(o), exchangeFill, o.FilledSize, d.Reason)
	if !limitFillExposureAlerts.Record(limitFillExposureKeyFor(o), d.Verdict, now) {
		return ""
	}
	msg := formatLimitFillExposureDM(o, exchangeFill, d)
	if notifier != nil && notifier.HasBackends() {
		notifier.SendOwnerDM(msg)
	}
	return msg
}

type limitFillCandidate struct {
	order       PendingLimitOrder
	sc          StrategyConfig
	logger      *StrategyLogger
	status      HyperliquidLimitOrderStatus
	polledAt    time.Time
	hasFill     bool
	avgPx       float64
	signedDelta float64
	resolveATR  bool
	entryATR    float64
	refused     bool
	applyFailed bool
}

func applyCoinLimitFills(
	state *AppState,
	cfg *Config,
	mu *sync.RWMutex,
	coin string,
	rows []*limitFillCandidate,
	totalDelta, onChainNet float64,
	readErr error,
	owners int,
	now time.Time,
) (limitFillExposureDecision, []int, []error) {
	mu.Lock()
	defer mu.Unlock()
	decision := classifyLimitFillLiveExposure(coin,
		hyperliquidBookedSignedNetForCoin(cfg.Strategies, state, coin),
		totalDelta, onChainNet, readErr, owners)
	decision.Legs = len(rows)
	if !decision.adopts() {
		return decision, nil, nil
	}
	trades := make([]int, len(rows))
	errs := make([]error, len(rows))
	for i, r := range rows {
		trades[i], errs[i] = applyLimitFillProgress(state, r.sc, r.order, r.status.FilledSize,
			r.avgPx, r.status.Fee, r.entryATR, resolveATRMethod(r.sc, cfg), now)
	}
	return decision, trades, errs
}
