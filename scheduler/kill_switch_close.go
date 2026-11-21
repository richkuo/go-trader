package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type HLNoFillRecoverer func(since time.Time) (*HLUserFillsResult, error)

const (
	hlKillSwitchNoFillRecoveryLookback = 10 * time.Minute
	hlKillSwitchNoFillRecoveryTimeout  = 20 * time.Second
)

const killSwitchVerifyReadTimeout = 45 * time.Second

type hlPositionTarget struct {
	Account  string
	Endpoint string
}

type hlStrictPositionSnapshot struct {
	Target    hlPositionTarget
	Positions []HLPosition
}

type HLStrictPositionReader func(ctx context.Context, target hlPositionTarget) (hlStrictPositionSnapshot, error)

type KillSwitchCloseInputs struct {
	HLAddr            string
	HLEndpoint        string
	HLLiveAll         []StrategyConfig
	HLHedgeCoins      map[string]bool
	HLCloser          HyperliquidLiveCloser
	HLReader          HLStrictPositionReader
	HLNoFillRecoverer HLNoFillRecoverer
	HLStopLossOIDs    map[string][]int64

	HLLimitOrderLoader  func() ([]PendingLimitOrder, error)
	QuoteLane           func(budget time.Duration) killSwitchQuoteReport
	HLLimitOrderRoster  []StrategyConfig
	HLLimitOrderDeps    killSwitchLimitOrderDeps
	HLLimitOrderTimeout time.Duration

	OKXLiveAllPerps []StrategyConfig
	OKXLiveAllSpot  []StrategyConfig
	OKXCloser       OKXLiveCloser
	OKXAccount      string
	OKXReader       OKXStrictPositionReader

	RHLiveCrypto        []StrategyConfig
	RHLiveOptions       []StrategyConfig
	RHCloser            RobinhoodLiveCloser
	RHFetcher           RobinhoodPositionsFetcher
	RHResolveUnresolved func() map[string]error
	RHFreshLongs        func() map[string]float64
	RHBookSnapshot      func() robinhoodBookSnapshot
	RHSettle            func() robinhoodSettleOutcome

	TSLiveAll []StrategyConfig
	TSCloser  TopStepLiveCloser
	TSFetcher TopStepPositionsFetcher

	PortfolioReason string

	CloseTimeout time.Duration

	HLCloseTimeout  time.Duration
	OKXCloseTimeout time.Duration
	RHCloseTimeout  time.Duration
	TSCloseTimeout  time.Duration
}

func (in KillSwitchCloseInputs) platformCloseBudget(override time.Duration) time.Duration {
	if override > 0 {
		return override
	}
	return in.CloseTimeout
}

func defaultHLKillSwitchNoFillRecoverer(since time.Time) (*HLUserFillsResult, error) {
	return runFetchHLUserFillsWithTimeout(since, hlKillSwitchNoFillRecoveryTimeout)
}

type KillSwitchClosePlan struct {
	OnChainConfirmedFlat bool

	LimitOrderReport killSwitchLimitOrderReport

	CloseReport HyperliquidLiveCloseReport

	OKXCloseReport OKXLiveCloseReport

	Unconfigured []HLPosition

	OKXUnconfigured []OKXPosition

	OKXSpotPresent bool

	RHCloseReport RobinhoodLiveCloseReport

	RHUnconfigured []RobinhoodPosition

	RHOptionsPresent bool

	TSCloseReport TopStepLiveCloseReport

	TSUnconfigured []TopStepPosition

	HLProof  killSwitchVenueProof
	OKXProof killSwitchVenueProof

	ObservedAt time.Time

	DiscordMessage string

	LogLines []string
}

type killSwitchVenueProof struct {
	Applicable bool
	Verified   bool
	Holds      []string
	Changes    []string
}

func (p KillSwitchClosePlan) CanAutoResetWithoutOwner() bool {
	return p.OnChainConfirmedFlat && p.LimitOrderReport.ConfirmedClear() && !p.OKXSpotPresent && !p.RHOptionsPresent
}

const killSwitchManualResetLine = "Virtual state cleared. Manual reset required."
const killSwitchAutoResetLine = "Virtual state cleared. Kill switch auto-reset; trading will resume next cycle."

func formatKillSwitchAutoResetMessage(msg string) string {
	return strings.Replace(msg, killSwitchManualResetLine, killSwitchAutoResetLine, 1)
}

type HLStateFetcher func(accountAddress string) ([]HLPosition, error)

func defaultHLStateFetcher(addr string) ([]HLPosition, error) {
	_, pos, err := fetchHyperliquidState(addr)
	return pos, err
}

var strictVenueSizePattern = regexp.MustCompile(`^[+-]?(\d+(\.\d*)?|\.\d+)([eE][+-]?\d+)?$`)

func parseStrictVenueSize(raw string) (float64, error) {
	s := strings.TrimSpace(raw)
	if s != raw || !strictVenueSizePattern.MatchString(s) {
		return 0, fmt.Errorf("size %q is not a decimal number", raw)
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("size %q does not parse: %w", raw, err)
	}
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, fmt.Errorf("size %q is not finite", raw)
	}
	if v == 0 {
		mantissa := s
		if i := strings.IndexAny(mantissa, "eE"); i >= 0 {
			mantissa = mantissa[:i]
		}
		if strings.ContainsAny(mantissa, "123456789") {
			return 0, fmt.Errorf("nonzero size %q decodes as zero", raw)
		}
		return 0, nil
	}
	return v, nil
}

func parseStrictVenueSizeJSON(raw json.RawMessage) (float64, error) {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return 0, fmt.Errorf("size is missing")
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return parseStrictVenueSize(asString)
	}
	var asNumber json.Number
	if err := json.Unmarshal(raw, &asNumber); err != nil {
		return 0, fmt.Errorf("size %s is neither a string nor a number", string(raw))
	}
	return parseStrictVenueSize(asNumber.String())
}

func positiveOIDs(oids []int64) []int64 {
	var out []int64
	for _, oid := range oids {
		out = appendUniquePositiveStopLossOID(out, oid)
	}
	return out
}

type killSwitchExposureRow struct {
	Instrument string
	Leg        string
	Coin       string
	Size       float64
}

func (r killSwitchExposureRow) key() string {
	return r.Instrument + "|" + r.Leg
}

func (r killSwitchExposureRow) label() string {
	name := r.Instrument
	if r.Leg != "" {
		name += " " + r.Leg
	}
	return name + " " + killSwitchSideSize(r.Size)
}

func killSwitchSideSize(size float64) string {
	switch {
	case size > 0:
		return fmt.Sprintf("long %.10g", size)
	case size < 0:
		return fmt.Sprintf("short %.10g", -size)
	}
	return "flat"
}

type killSwitchResidual struct {
	ByCoin  map[string][]killSwitchExposureRow
	Outside []killSwitchExposureRow
	Changes []string
}

func (r killSwitchResidual) flat() bool {
	return len(r.ByCoin) == 0 && len(r.Outside) == 0
}

func evaluateKillSwitchResidual(baseline, post []killSwitchExposureRow, closable func(killSwitchExposureRow) bool) killSwitchResidual {
	res := killSwitchResidual{ByCoin: map[string][]killSwitchExposureRow{}}
	before := make(map[string]float64, len(baseline))
	for _, row := range baseline {
		before[row.key()] = row.Size
	}
	for _, row := range post {
		if row.Size == 0 {
			continue
		}
		if closable != nil && closable(row) {
			res.ByCoin[row.Coin] = append(res.ByCoin[row.Coin], row)
		} else {
			res.Outside = append(res.Outside, row)
		}
		was := before[row.key()]
		switch {
		case was == 0:
			res.Changes = append(res.Changes, fmt.Sprintf("%s appeared (pre-close flat)", row.label()))
		case (was > 0) != (row.Size > 0):
			res.Changes = append(res.Changes, fmt.Sprintf("%s changed side (pre-close %s)", row.label(), killSwitchSideSize(was)))
		case math.Abs(row.Size) > math.Abs(was)*(1+1e-9):
			res.Changes = append(res.Changes, fmt.Sprintf("%s grew (pre-close %s)", row.label(), killSwitchSideSize(was)))
		}
	}
	sort.Strings(res.Changes)
	sort.Slice(res.Outside, func(i, j int) bool { return res.Outside[i].key() < res.Outside[j].key() })
	for coin := range res.ByCoin {
		rows := res.ByCoin[coin]
		sort.Slice(rows, func(i, j int) bool { return rows[i].key() < rows[j].key() })
	}
	return res
}

func killSwitchResidualLabels(rows []killSwitchExposureRow) string {
	labels := make([]string, 0, len(rows))
	for _, r := range rows {
		labels = append(labels, r.label())
	}
	return strings.Join(labels, ", ")
}

func removeKillSwitchCoin(list []string, coin string) []string {
	out := list[:0]
	for _, c := range list {
		if c != coin {
			out = append(out, c)
		}
	}
	return out
}

type killSwitchFlatLists struct {
	closed   *[]string
	already  *[]string
	errs     map[string]error
	resolved *map[string]error
}

func (l killSwitchFlatLists) block(coin string, detail string) {
	*l.closed = removeKillSwitchCoin(*l.closed, coin)
	*l.already = removeKillSwitchCoin(*l.already, coin)
	l.errs[coin] = errors.New(detail)
}

func (l killSwitchFlatLists) verify(coin string) error {
	err, ok := l.errs[coin]
	if !ok {
		return nil
	}
	delete(l.errs, coin)
	if *l.resolved == nil {
		*l.resolved = map[string]error{}
	}
	(*l.resolved)[coin] = err
	if !containsString(*l.already, coin) && !containsString(*l.closed, coin) {
		*l.already = append(*l.already, coin)
	}
	return err
}

func sortedKillSwitchCoins(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

func hlKillSwitchExposureRows(positions []HLPosition) []killSwitchExposureRow {
	rows := make([]killSwitchExposureRow, 0, len(positions))
	for _, p := range positions {
		rows = append(rows, killSwitchExposureRow{Instrument: p.Coin, Coin: p.Coin, Size: p.Size})
	}
	return rows
}

func okxKillSwitchExposureRows(snap okxStrictPositionSnapshot) []killSwitchExposureRow {
	rows := make([]killSwitchExposureRow, 0, len(snap.Rows))
	for _, p := range snap.Rows {
		rows = append(rows, killSwitchExposureRow{Instrument: p.InstID, Leg: p.PosSide + "/" + p.MarginMode, Coin: p.Coin, Size: p.Contracts})
	}
	return rows
}

func hlKillSwitchTradedCoins(hlLiveAll []StrategyConfig, hedgeCoins map[string]bool) map[string]bool {
	traded := make(map[string]bool)
	for _, sc := range hlLiveAll {
		if sym := hyperliquidRawCoin(sc); sym != "" {
			traded[strings.ToUpper(sym)] = true
		}
	}
	for coin, held := range hedgeCoins {
		if held && coin != "" {
			traded[strings.ToUpper(coin)] = true
		}
	}
	return traded
}

func hlKillSwitchUncoveredNamespaces(traded map[string]bool) []string {
	var out []string
	for coin := range traded {
		if strings.Contains(coin, ":") {
			out = append(out, coin)
		}
	}
	sort.Strings(out)
	return out
}

func hlKillSwitchAttemptFor(report HyperliquidLiveCloseReport, coin string) (hlKillSwitchCloseAttempt, bool) {
	for i := len(report.Attempts) - 1; i >= 0; i-- {
		if report.Attempts[i].Coin == coin {
			return report.Attempts[i], true
		}
	}
	return hlKillSwitchCloseAttempt{}, false
}

func hlKillSwitchAttemptEvidence(report HyperliquidLiveCloseReport, coin string, priorErr error) []string {
	a, ok := hlKillSwitchAttemptFor(report, coin)
	if !ok {
		if priorErr != nil {
			return []string{"no close was sent: " + priorErr.Error()}
		}
		return []string{"no close was sent this cycle"}
	}
	parts := []string{"pre-close " + killSwitchSideSize(a.PreSize)}
	switch {
	case a.Err != nil:
		parts = append(parts, "close reported error: "+a.Err.Error())
	case a.Result != nil && a.Result.Close != nil && a.Result.Close.AlreadyFlat:
		parts = append(parts, "close reported already flat")
	case a.Result != nil && a.Result.Close != nil && a.Result.Close.Fill != nil &&
		finitePositive(a.Result.Close.Fill.TotalSz) && finitePositive(a.Result.Close.Fill.AvgPx):
		parts = append(parts, fmt.Sprintf("close reported fill %.10g @ %.10g", a.Result.Close.Fill.TotalSz, a.Result.Close.Fill.AvgPx))
	default:
		parts = append(parts, "close reported no usable fill")
	}
	return append(parts, hlKillSwitchProtectionEvidence(a))
}

func hlKillSwitchProtectionEvidence(a hlKillSwitchCloseAttempt) string {
	if len(a.RequestedOIDs) == 0 {
		return "no trigger order cancel was requested"
	}
	if a.Result == nil {
		return fmt.Sprintf("trigger cancels requested for %v have no readable outcome; protection unverified", a.RequestedOIDs)
	}
	toSet := func(oids []int64) map[int64]bool {
		set := make(map[int64]bool, len(oids))
		for _, oid := range oids {
			set[oid] = true
		}
		return set
	}
	succeeded := toSet(a.Result.CancelStopLossSucceededOIDs)
	failed := toSet(a.Result.CancelStopLossFailedOIDs)
	unacked := toSet(a.Result.CancelStopLossUnacknowledgedOIDs)
	var cancelled, refused, unknown []int64
	for _, oid := range a.RequestedOIDs {
		switch {
		case unacked[oid]:
			unknown = append(unknown, oid)
		case succeeded[oid]:
			cancelled = append(cancelled, oid)
		case failed[oid]:
			refused = append(refused, oid)
		default:
			unknown = append(unknown, oid)
		}
	}
	var parts []string
	if len(cancelled) > 0 {
		parts = append(parts, fmt.Sprintf("cancel reported for %v before the close", cancelled))
	}
	if len(refused) > 0 {
		parts = append(parts, fmt.Sprintf("cancel reported failed for %v (they may still rest)", refused))
	}
	if len(unknown) > 0 {
		parts = append(parts, fmt.Sprintf("no cancel acknowledgement for %v", unknown))
	}
	out := "trigger orders: " + strings.Join(parts, ", ")
	switch {
	case len(unknown) > 0:
		out += "; protection unverified"
	case len(cancelled) > 0:
		out += "; the remaining position may be unprotected"
	}
	return out
}

func okxKillSwitchAttemptEvidence(report OKXLiveCloseReport, coin string, priorErr error) []string {
	var a okxKillSwitchCloseAttempt
	ok := false
	for i := len(report.Attempts) - 1; i >= 0; i-- {
		if report.Attempts[i].Coin == coin {
			a, ok = report.Attempts[i], true
			break
		}
	}
	if !ok {
		if priorErr != nil {
			return []string{"no close was sent: " + priorErr.Error()}
		}
		return []string{"no close was sent this cycle"}
	}
	switch {
	case a.Err != nil:
		return []string{"close reported error: " + a.Err.Error()}
	case a.Result != nil && a.Result.Close != nil && a.Result.Close.AlreadyFlat:
		return []string{"close reported already flat"}
	case a.Result != nil && a.Result.Close != nil && a.Result.Close.Fill != nil && finitePositive(a.Result.Close.Fill.TotalSz):
		return []string{fmt.Sprintf("close reported fill %.10g", a.Result.Close.Fill.TotalSz)}
	}
	return []string{"close reported an accepted order with no confirmed fill"}
}

func recoverHyperliquidAlreadyFlatFills(report *HyperliquidLiveCloseReport, positions []HLPosition, recoverer HLNoFillRecoverer, since time.Time) []string {
	if report == nil || len(report.AlreadyFlat) == 0 || recoverer == nil {
		return nil
	}
	type expectedFill struct {
		qty float64
	}
	expectedByCoin := make(map[string]expectedFill)
	for _, p := range positions {
		qty := math.Abs(p.Size)
		if qty <= 0 {
			continue
		}
		coin := normalizeHLFillCoin(p.Coin)
		if coin == "" {
			continue
		}
		expectedByCoin[coin] = expectedFill{qty: qty}
	}
	eligibleByRaw := make(map[string]expectedFill)
	for _, rawCoin := range report.AlreadyFlat {
		norm := normalizeHLFillCoin(rawCoin)
		if norm == "" {
			continue
		}
		if report.Fills != nil {
			if _, ok := report.Fills[rawCoin]; ok {
				continue
			}
			if _, ok := report.Fills[norm]; ok {
				continue
			}
		}
		expected, ok := expectedByCoin[norm]
		if !ok || expected.qty <= 0 {
			continue
		}
		eligibleByRaw[rawCoin] = expected
	}
	if len(eligibleByRaw) == 0 {
		return nil
	}

	result, err := recoverer(since)
	if err != nil {
		return []string{fmt.Sprintf("[WARN] hl-close: unable to recover already-flat fill from userFills: %v", err)}
	}
	if result == nil {
		return []string{"[WARN] hl-close: unable to recover already-flat fill from userFills: empty result"}
	}
	if strings.TrimSpace(result.Error) != "" {
		return []string{fmt.Sprintf("[WARN] hl-close: unable to recover already-flat fill from userFills: %s", result.Error)}
	}
	if report.Fills == nil {
		report.Fills = make(map[string]HyperliquidCloseFill)
	}

	raws := make([]string, 0, len(eligibleByRaw))
	for r := range eligibleByRaw {
		raws = append(raws, r)
	}
	sort.Strings(raws)
	candidates := make(map[string]HLFillSummary, len(result.ByOID))
	for oid, summary := range result.ByOID {
		if summary.ClosedPnLGross == 0 {
			continue
		}
		if t := hlFillSummaryEventTime(summary); !t.IsZero() && t.Before(since) {
			continue
		}
		candidates[oid] = summary
	}
	var lines []string
	for _, rawCoin := range raws {
		expected := eligibleByRaw[rawCoin]
		match, ok, ambiguous := findUniqueHLFillByCoinQty(candidates, rawCoin, expected.qty, true, time.Time{}, 0)
		switch {
		case ok:
			report.Fills[rawCoin] = HyperliquidCloseFill{
				AvgPx:   match.Summary.Px,
				TotalSz: expected.qty,
				OID:     match.OIDInt,
				Fee:     match.Summary.Fee,
			}
			lines = append(lines,
				fmt.Sprintf("[INFO] hl-close: recovered already-flat fill for %s from userFills oid=%s qty=%.6f px=%.6f fee=%.6f", rawCoin, match.OID, expected.qty, match.Summary.Px, match.Summary.Fee))
		case ambiguous:
			lines = append(lines,
				fmt.Sprintf("[WARN] hl-close: multiple userFills candidates for already-flat %s qty=%.6f; falling back to model-only cleanup", rawCoin, expected.qty))
		default:
			lines = append(lines,
				fmt.Sprintf("[WARN] hl-close: no userFills match for already-flat %s qty=%.6f; falling back to model-only cleanup", rawCoin, expected.qty))
		}
	}
	return lines
}

func settledKillSwitchSymbols(plan *KillSwitchClosePlan) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	add := func(platform string, coins []string) {
		if len(coins) == 0 {
			return
		}
		set := out[platform]
		if set == nil {
			set = map[string]bool{}
			out[platform] = set
		}
		for _, c := range coins {
			if c != "" {
				set[c] = true
			}
		}
	}
	add("okx", plan.OKXCloseReport.ClosedCoins)
	add("robinhood", plan.RHCloseReport.ClosedCoins)
	add("topstep", plan.TSCloseReport.ClosedCoins)
	return out
}

func applyKillSwitchSettledLegsWhileLatched(strategies map[string]*StrategyState, cfgs []StrategyConfig, plan *KillSwitchClosePlan, hlRoster []StrategyConfig, virtualQty hlVirtualQuantitySnapshot, prices map[string]float64, logger *StrategyLogger) {
	if plan == nil || len(cfgs) == 0 {
		return
	}
	settled := settledKillSwitchSymbols(plan)
	hasHLFills := len(plan.CloseReport.Fills) > 0
	for _, sc := range cfgs {
		s, ok := strategies[sc.ID]
		if !ok || s == nil || quoteFundingOwned(sc, s) {
			continue
		}
		if hasHLFills {
			applyHyperliquidKillSwitchCloseFill(s, sc, plan.CloseReport.Fills, hlRoster, virtualQty)
			applyHyperliquidKillSwitchHedgeFill(s, sc, plan.CloseReport.Fills)
		}
		forceCloseSettledPositions(s, sc, prices, settled, plan.RHCloseReport.ClosedCoinProofs(), logger)
	}
}

func planKillSwitchClose(in KillSwitchCloseInputs) KillSwitchClosePlan {
	plan := KillSwitchClosePlan{OnChainConfirmedFlat: true}

	plan.LimitOrderReport = cancelKillSwitchRestingLimitOrders(
		in.HLLimitOrderLoader, in.HLLimitOrderRoster, in.HLLimitOrderDeps,
		in.platformCloseBudget(in.HLLimitOrderTimeout))
	plan.LogLines = append(plan.LogLines, plan.LimitOrderReport.LogLines...)
	if !plan.LimitOrderReport.ConfirmedClear() {
		plan.OnChainConfirmedFlat = false
	}
	if in.QuoteLane != nil {
		qr := in.QuoteLane(in.platformCloseBudget(in.HLLimitOrderTimeout))
		plan.LogLines = append(plan.LogLines, qr.LogLines...)
		if !qr.Clear {
			plan.OnChainConfirmedFlat = false
		}
	}

	if in.HLAddr != "" || len(in.HLLiveAll) > 0 {
		planHyperliquidKillSwitchClose(in, &plan)
	}

	plan.OKXSpotPresent = len(in.OKXLiveAllSpot) > 0
	if plan.OKXSpotPresent {
		plan.LogLines = append(plan.LogLines,
			fmt.Sprintf("[CRITICAL] okx-close: %d live OKX spot strategies configured — kill switch cannot auto-close spot (no reduce-only); operator must verify manually (#345)", len(in.OKXLiveAllSpot)))
	}

	if len(in.OKXLiveAllPerps) > 0 {
		planOKXKillSwitchClose(in, &plan)
	}

	plan.RHOptionsPresent = len(in.RHLiveOptions) > 0
	if plan.RHOptionsPresent {
		plan.LogLines = append(plan.LogLines,
			fmt.Sprintf("[CRITICAL] rh-close: %d live Robinhood options strategies configured — kill switch cannot auto-close options (sell-to-close vs buy-to-close semantics); operator must verify manually (#346)", len(in.RHLiveOptions)))
	}

	switch {
	case len(in.RHLiveCrypto) > 0 && in.RHFetcher == nil:
		plan.LogLines = append(plan.LogLines,
			"[CRITICAL] rh-close: Robinhood live crypto strategies configured but RHFetcher unwired — cannot confirm flat (kill switch will retry next cycle)")
		plan.OnChainConfirmedFlat = false
	case len(in.RHLiveCrypto) > 0 && (in.RHBookSnapshot == nil || in.RHSettle == nil):
		plan.LogLines = append(plan.LogLines,
			"[CRITICAL] rh-close: Robinhood live crypto strategies configured but the book snapshot or durable settlement is unwired — no Robinhood order is sent and flat is not confirmed (kill switch will retry next cycle)")
		plan.OnChainConfirmedFlat = false
	case len(in.RHLiveCrypto) > 0:
		var rhUnresolved map[string]error
		if in.RHResolveUnresolved != nil {
			rhUnresolved = in.RHResolveUnresolved()
		}
		var rhFreshLongs map[string]float64
		if in.RHFreshLongs != nil {
			rhFreshLongs = in.RHFreshLongs()
		}
		rhBook := in.RHBookSnapshot()
		rhHoldings, err := in.RHFetcher()
		if err != nil {
			plan.LogLines = append(plan.LogLines,
				fmt.Sprintf("[CRITICAL] rh-close: kill switch unable to fetch Robinhood positions: %v — cannot confirm flat", err))
			plan.OnChainConfirmedFlat = false
		} else {
			ctx, cancel := context.WithTimeout(context.Background(), in.platformCloseBudget(in.RHCloseTimeout))
			plan.RHCloseReport = forceCloseRobinhoodLive(ctx, rhHoldings, in.RHLiveCrypto, robinhoodKillSwitchCloseDeps{
				Closer:     in.RHCloser,
				Fetcher:    in.RHFetcher,
				Unresolved: rhUnresolved,
				FreshLongs: rhFreshLongs,
				Book:       rhBook,
				BookAfter:  in.RHBookSnapshot,
				Settle:     in.RHSettle,
			})
			cancel()
			if !plan.RHCloseReport.ConfirmedFlat() {
				plan.OnChainConfirmedFlat = false
			}
			if len(plan.RHCloseReport.ClosedCoins) > 0 {
				plan.LogLines = append(plan.LogLines,
					fmt.Sprintf("[CRITICAL] rh-close: confirmed close for %v", plan.RHCloseReport.ClosedCoins))
			}
			if len(plan.RHCloseReport.AlreadyFlat) > 0 {
				plan.LogLines = append(plan.LogLines,
					fmt.Sprintf("[INFO] rh-close: already flat: %v", plan.RHCloseReport.AlreadyFlat))
			}
			for _, coin := range plan.RHCloseReport.SortedErrorCoins() {
				plan.LogLines = append(plan.LogLines,
					fmt.Sprintf("[CRITICAL] rh-close: %s failed: %v (kill switch will retry next cycle)", coin, plan.RHCloseReport.Errors[coin]))
			}
			for _, p := range plan.RHCloseReport.Residue {
				plan.LogLines = append(plan.LogLines,
					fmt.Sprintf("[CRITICAL] rh-close: %s holdings %.10g are above every go-trader book and were not sold; they are the operator's (#1825)", p.Coin, p.Size))
			}

			plan.RHUnconfigured = plan.RHCloseReport.Unconfigured
			if len(plan.RHUnconfigured) > 0 {
				plan.OnChainConfirmedFlat = false
				for _, p := range plan.RHUnconfigured {
					plan.LogLines = append(plan.LogLines,
						fmt.Sprintf("[CRITICAL] rh-close: live balance for unconfigured coin %s (size=%.6f) — manual intervention required, kill switch will retry next cycle", p.Coin, p.Size))
				}
			}
		}
	}

	switch {
	case len(in.TSLiveAll) > 0 && in.TSFetcher == nil:
		plan.LogLines = append(plan.LogLines,
			"[CRITICAL] ts-close: TopStep live futures strategies configured but TSFetcher unwired — cannot confirm flat (kill switch will retry next cycle)")
		plan.OnChainConfirmedFlat = false
	case len(in.TSLiveAll) > 0:
		tsPositions, err := in.TSFetcher()
		if err != nil {
			plan.LogLines = append(plan.LogLines,
				fmt.Sprintf("[CRITICAL] ts-close: kill switch unable to fetch TopStep positions: %v — cannot confirm flat", err))
			plan.OnChainConfirmedFlat = false
		} else {
			ctx, cancel := context.WithTimeout(context.Background(), in.platformCloseBudget(in.TSCloseTimeout))
			plan.TSCloseReport = forceCloseTopStepLive(ctx, tsPositions, in.TSLiveAll, in.TSCloser)
			cancel()
			if !plan.TSCloseReport.ConfirmedFlat() {
				plan.OnChainConfirmedFlat = false
			}
			if len(plan.TSCloseReport.ClosedCoins) > 0 {
				plan.LogLines = append(plan.LogLines,
					fmt.Sprintf("[CRITICAL] ts-close: confirmed close for %v", plan.TSCloseReport.ClosedCoins))
			}
			if len(plan.TSCloseReport.AlreadyFlat) > 0 {
				plan.LogLines = append(plan.LogLines,
					fmt.Sprintf("[INFO] ts-close: already flat: %v", plan.TSCloseReport.AlreadyFlat))
			}
			for _, coin := range plan.TSCloseReport.SortedErrorCoins() {
				plan.LogLines = append(plan.LogLines,
					fmt.Sprintf("[CRITICAL] ts-close: %s failed: %v (kill switch will retry next cycle)", coin, plan.TSCloseReport.Errors[coin]))
			}

			plan.TSUnconfigured = plan.TSCloseReport.Unconfigured
			if len(plan.TSUnconfigured) > 0 {
				plan.OnChainConfirmedFlat = false
				for _, p := range plan.TSUnconfigured {
					plan.LogLines = append(plan.LogLines,
						fmt.Sprintf("[CRITICAL] ts-close: live position for unconfigured symbol %s (size=%d) — manual intervention required, kill switch will retry next cycle", p.Coin, p.Size))
				}
			}
		}
	}

	plan.ObservedAt = time.Now().UTC()
	plan.DiscordMessage = formatKillSwitchMessage(in.HLAddr, plan, in.PortfolioReason)
	return plan
}

func (p *KillSwitchClosePlan) holdVenue(proof *killSwitchVenueProof, prefix, reason string) {
	proof.Holds = append(proof.Holds, reason)
	p.OnChainConfirmedFlat = false
	p.LogLines = append(p.LogLines,
		fmt.Sprintf("[CRITICAL] %s: %s — cannot confirm on-chain flat (kill switch will retry next cycle)", prefix, reason))
}

func readHLKillSwitchPositions(in KillSwitchCloseInputs, target hlPositionTarget) (hlStrictPositionSnapshot, error) {
	ctx, cancel := context.WithTimeout(context.Background(), killSwitchVerifyReadTimeout)
	defer cancel()
	snap, err := in.HLReader(ctx, target)
	if err != nil {
		return hlStrictPositionSnapshot{}, err
	}
	if !strings.EqualFold(snap.Target.Account, target.Account) || snap.Target.Endpoint != target.Endpoint {
		return hlStrictPositionSnapshot{}, fmt.Errorf("the read answered for account %s at %s, not the close target %s at %s",
			snap.Target.Account, snap.Target.Endpoint, target.Account, target.Endpoint)
	}
	return snap, nil
}

func readOKXKillSwitchPositions(in KillSwitchCloseInputs, target okxPositionTarget) (okxStrictPositionSnapshot, error) {
	ctx, cancel := context.WithTimeout(context.Background(), killSwitchVerifyReadTimeout)
	defer cancel()
	snap, err := in.OKXReader(ctx, target)
	if err != nil {
		return okxStrictPositionSnapshot{}, err
	}
	if snap.Account != target.Account {
		return okxStrictPositionSnapshot{}, fmt.Errorf("the read answered for account %s, not the close target %s", snap.Account, target.Account)
	}
	return snap, nil
}

func planHyperliquidKillSwitchClose(in KillSwitchCloseInputs, plan *KillSwitchClosePlan) {
	proof := &plan.HLProof
	proof.Applicable = true
	traded := hlKillSwitchTradedCoins(in.HLLiveAll, in.HLHedgeCoins)
	target := hlPositionTarget{Account: in.HLAddr, Endpoint: in.HLEndpoint}
	switch {
	case in.HLAddr == "":
		plan.holdVenue(proof, "hl-close", "live Hyperliquid strategies are configured but no account address is set")
		return
	case in.HLEndpoint == "":
		plan.holdVenue(proof, "hl-close", "no Hyperliquid info endpoint is bound to the close target")
		return
	case in.HLReader == nil:
		plan.holdVenue(proof, "hl-close", "the strict Hyperliquid position reader is unwired")
		return
	case len(traded) > 0 && in.HLCloser == nil:
		plan.holdVenue(proof, "hl-close", "the Hyperliquid closer is unwired")
		return
	}
	if ns := hlKillSwitchUncoveredNamespaces(traded); len(ns) > 0 {
		plan.holdVenue(proof, "hl-close", fmt.Sprintf("coins %v trade on a perp namespace the position reader does not cover", ns))
		return
	}

	baseline, err := readHLKillSwitchPositions(in, target)
	if err != nil {
		plan.holdVenue(proof, "hl-close", fmt.Sprintf("could not fetch HL positions before the close: %v", err))
		return
	}

	recoverSince := time.Now().UTC().Add(-hlKillSwitchNoFillRecoveryLookback)
	ctx, cancel := context.WithTimeout(context.Background(), in.platformCloseBudget(in.HLCloseTimeout))
	plan.CloseReport = forceCloseHyperliquidLive(ctx, baseline.Positions, in.HLLiveAll, in.HLHedgeCoins, in.HLCloser, in.HLStopLossOIDs)
	cancel()
	report := &plan.CloseReport
	lists := killSwitchFlatLists{closed: &report.ClosedCoins, already: &report.AlreadyFlat, errs: report.Errors, resolved: &report.ResolvedErrors}

	attempted := map[string]bool{}
	for _, a := range report.Attempts {
		attempted[a.Coin] = true
	}
	post := baseline
	if len(report.Attempts) > 0 {
		post, err = readHLKillSwitchPositions(in, target)
		if err != nil {
			reason := fmt.Sprintf("the position read after the last close attempt failed: %v", err)
			plan.holdVenue(proof, "hl-close", reason)
			for _, coin := range sortedKillSwitchCoins(attempted) {
				parts := append([]string{"flat not verified: " + reason}, hlKillSwitchAttemptEvidence(*report, coin, report.Errors[coin])...)
				lists.block(coin, strings.Join(parts, "; "))
			}
		}
	}
	if err == nil {
		res := evaluateKillSwitchResidual(hlKillSwitchExposureRows(baseline.Positions), hlKillSwitchExposureRows(post.Positions),
			func(r killSwitchExposureRow) bool { return traded[strings.ToUpper(r.Coin)] })
		proof.Changes = res.Changes
		for _, coin := range sortedKillSwitchResidualCoins(res.ByCoin) {
			parts := append([]string{"a fresh position read after the close shows " + killSwitchResidualLabels(res.ByCoin[coin]) + " still open"},
				hlKillSwitchAttemptEvidence(*report, coin, report.Errors[coin])...)
			lists.block(coin, strings.Join(parts, "; "))
		}
		for _, coin := range sortedKillSwitchCoins(attempted) {
			if _, open := res.ByCoin[coin]; open {
				continue
			}
			if prior := lists.verify(coin); prior != nil {
				plan.LogLines = append(plan.LogLines,
					fmt.Sprintf("[INFO] hl-close: %s verified flat by a fresh read after the close reported: %v", coin, prior))
			}
		}
		report.Unconfigured = nil
		for _, p := range post.Positions {
			if p.Size != 0 && !traded[strings.ToUpper(p.Coin)] {
				report.Unconfigured = append(report.Unconfigured, p)
			}
		}
		proof.Verified = res.flat() && len(report.Errors) == 0
		for _, change := range res.Changes {
			plan.LogLines = append(plan.LogLines, "[CRITICAL] hl-close: exposure changed since the pre-close read: "+change)
		}
		plan.LogLines = append(plan.LogLines,
			recoverHyperliquidAlreadyFlatFills(report, baseline.Positions, in.HLNoFillRecoverer, recoverSince)...)
	}

	if !report.ConfirmedFlat() || !proof.Verified {
		plan.OnChainConfirmedFlat = false
	}
	if len(report.ClosedCoins) > 0 {
		plan.LogLines = append(plan.LogLines,
			fmt.Sprintf("[CRITICAL] hl-close: close verified by a fresh position read for %v", report.ClosedCoins))
	}
	if len(report.AlreadyFlat) > 0 {
		plan.LogLines = append(plan.LogLines,
			fmt.Sprintf("[INFO] hl-close: already flat on-chain (fresh read): %v", report.AlreadyFlat))
	}
	for _, coin := range report.SortedErrorCoins() {
		plan.LogLines = append(plan.LogLines,
			fmt.Sprintf("[CRITICAL] hl-close: %s not flat: %v (kill switch will retry next cycle)", coin, report.Errors[coin]))
	}
	if len(report.Unconfigured) > 0 {
		plan.Unconfigured = append(plan.Unconfigured, report.Unconfigured...)
		sort.Slice(plan.Unconfigured, func(i, j int) bool { return plan.Unconfigured[i].Coin < plan.Unconfigured[j].Coin })
		plan.OnChainConfirmedFlat = false
		for _, p := range plan.Unconfigured {
			plan.LogLines = append(plan.LogLines,
				fmt.Sprintf("[CRITICAL] hl-close: on-chain position for unconfigured coin %s (szi=%.6f) — manual intervention required, kill switch will retry next cycle", p.Coin, p.Size))
		}
	}
}

func sortedKillSwitchResidualCoins(byCoin map[string][]killSwitchExposureRow) []string {
	out := make([]string, 0, len(byCoin))
	for c := range byCoin {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

func planOKXKillSwitchClose(in KillSwitchCloseInputs, plan *KillSwitchClosePlan) {
	proof := &plan.OKXProof
	proof.Applicable = true
	target := okxPositionTarget{Account: in.OKXAccount}
	switch {
	case in.OKXAccount == "":
		plan.holdVenue(proof, "okx-close", "live OKX perps strategies are configured but no OKX swap account identity is set")
		return
	case in.OKXReader == nil:
		plan.holdVenue(proof, "okx-close", "the strict OKX position reader is unwired")
		return
	case in.OKXCloser == nil:
		plan.holdVenue(proof, "okx-close", "the OKX closer is unwired")
		return
	}

	routes := map[string]string{}
	for _, sc := range in.OKXLiveAllPerps {
		if sc.Type != "perps" {
			continue
		}
		if sym := okxSymbol(sc.Args); sym != "" {
			routes[okxSwapRouteInstID(strings.ToUpper(sym))] = sym
		}
	}
	closable := func(r killSwitchExposureRow) bool {
		_, ok := routes[r.Instrument]
		return ok
	}
	routeCoin := func(r killSwitchExposureRow) killSwitchExposureRow {
		if sym, ok := routes[r.Instrument]; ok {
			r.Coin = sym
		}
		return r
	}

	baseline, err := readOKXKillSwitchPositions(in, target)
	if err != nil {
		plan.holdVenue(proof, "okx-close", fmt.Sprintf("could not fetch OKX positions before the close: %v", err))
		return
	}
	baseRows := okxKillSwitchExposureRows(baseline)
	gross := map[string]float64{}
	var order []string
	for _, r := range baseRows {
		if r.Size == 0 || !closable(r) {
			continue
		}
		r = routeCoin(r)
		if _, seen := gross[r.Coin]; !seen {
			order = append(order, r.Coin)
		}
		gross[r.Coin] += math.Abs(r.Size)
	}
	closeInput := make([]OKXPosition, 0, len(order))
	for _, coin := range order {
		closeInput = append(closeInput, OKXPosition{Coin: coin, Size: gross[coin], Account: baseline.Account})
	}

	ctx, cancel := context.WithTimeout(context.Background(), in.platformCloseBudget(in.OKXCloseTimeout))
	plan.OKXCloseReport = forceCloseOKXLive(ctx, closeInput, in.OKXLiveAllPerps, in.OKXCloser)
	cancel()
	report := &plan.OKXCloseReport
	lists := killSwitchFlatLists{closed: &report.ClosedCoins, already: &report.AlreadyFlat, errs: report.Errors, resolved: &report.ResolvedErrors}

	attempted := map[string]bool{}
	for _, a := range report.Attempts {
		attempted[a.Coin] = true
	}
	post := baseline
	if len(report.Attempts) > 0 {
		post, err = readOKXKillSwitchPositions(in, target)
		if err != nil {
			reason := fmt.Sprintf("the position read after the last close attempt failed: %v", err)
			plan.holdVenue(proof, "okx-close", reason)
			for _, coin := range sortedKillSwitchCoins(attempted) {
				parts := append([]string{"flat not verified: " + reason}, okxKillSwitchAttemptEvidence(*report, coin, report.Errors[coin])...)
				lists.block(coin, strings.Join(parts, "; "))
			}
		}
	}
	if err == nil {
		postRows := okxKillSwitchExposureRows(post)
		for i := range postRows {
			postRows[i] = routeCoin(postRows[i])
		}
		for i := range baseRows {
			baseRows[i] = routeCoin(baseRows[i])
		}
		res := evaluateKillSwitchResidual(baseRows, postRows, closable)
		proof.Changes = res.Changes
		for _, coin := range sortedKillSwitchResidualCoins(res.ByCoin) {
			parts := append([]string{"a fresh position read after the close shows " + killSwitchResidualLabels(res.ByCoin[coin]) + " contracts still open"},
				okxKillSwitchAttemptEvidence(*report, coin, report.Errors[coin])...)
			lists.block(coin, strings.Join(parts, "; "))
		}
		for _, coin := range sortedKillSwitchCoins(attempted) {
			if _, open := res.ByCoin[coin]; open {
				continue
			}
			if prior := lists.verify(coin); prior != nil {
				plan.LogLines = append(plan.LogLines,
					fmt.Sprintf("[INFO] okx-close: %s verified flat by a fresh read after the close reported: %v", coin, prior))
			}
		}
		report.Unconfigured = nil
		for _, r := range res.Outside {
			report.Unconfigured = append(report.Unconfigured, OKXPosition{
				Coin: r.Coin, Size: r.Size, Contracts: r.Size, InstID: r.Instrument, Side: r.Leg, Account: post.Account,
			})
		}
		proof.Verified = res.flat() && len(report.Errors) == 0
		for _, change := range res.Changes {
			plan.LogLines = append(plan.LogLines, "[CRITICAL] okx-close: exposure changed since the pre-close read: "+change)
		}
	}

	if !report.ConfirmedFlat() || !proof.Verified {
		plan.OnChainConfirmedFlat = false
	}
	if len(report.ClosedCoins) > 0 {
		plan.LogLines = append(plan.LogLines,
			fmt.Sprintf("[CRITICAL] okx-close: close verified by a fresh position read for %v", report.ClosedCoins))
	}
	if len(report.AlreadyFlat) > 0 {
		plan.LogLines = append(plan.LogLines,
			fmt.Sprintf("[INFO] okx-close: already flat on-chain (fresh read): %v", report.AlreadyFlat))
	}
	for _, coin := range report.SortedErrorCoins() {
		plan.LogLines = append(plan.LogLines,
			fmt.Sprintf("[CRITICAL] okx-close: %s not flat: %v (kill switch will retry next cycle)", coin, report.Errors[coin]))
	}
	plan.OKXUnconfigured = report.Unconfigured
	if len(plan.OKXUnconfigured) > 0 {
		plan.OnChainConfirmedFlat = false
		for _, p := range plan.OKXUnconfigured {
			plan.LogLines = append(plan.LogLines,
				fmt.Sprintf("[CRITICAL] okx-close: on-chain position %s %s (%.10g contracts) is outside every configured close route — manual intervention required, kill switch will retry next cycle", p.InstID, p.Side, p.Contracts))
		}
	}
}

func collectHLKillSwitchStopOIDs(strategies map[string]*StrategyState, roster []StrategyConfig) map[string][]int64 {
	out := map[string][]int64{}
	for _, sc := range roster {
		sym := hyperliquidRawCoin(sc)
		if sym == "" {
			continue
		}
		ss, ok := strategies[sc.ID]
		if !ok || ss == nil {
			continue
		}
		pos := hlVirtualPositionFor(ss, sc, sym)
		if pos == nil {
			continue
		}
		out[sym] = appendUniquePositiveStopLossOID(out[sym], pos.StopLossOID)
		for _, tpOID := range pos.TPOIDs {
			out[sym] = appendUniquePositiveStopLossOID(out[sym], tpOID)
		}
	}
	return out
}

func formatKillSwitchMessage(hlAddr string, plan KillSwitchClosePlan, portfolioReason string) string {
	if plan.OnChainConfirmedFlat {
		var parts []string
		if len(plan.CloseReport.ClosedCoins) > 0 {
			parts = append(parts, fmt.Sprintf("HL closes: %v", plan.CloseReport.ClosedCoins))
		} else if hlAddr == "" {
			parts = append(parts, "HL not configured")
		} else {
			parts = append(parts, "no live HL exposure")
		}
		if len(plan.OKXCloseReport.ClosedCoins) > 0 {
			parts = append(parts, fmt.Sprintf("OKX closes: %v", plan.OKXCloseReport.ClosedCoins))
		}
		if verified := killSwitchVerifiedVenues(plan); verified != "" {
			parts = append(parts, verified)
		}
		if len(plan.RHCloseReport.ClosedCoins) > 0 {
			parts = append(parts, fmt.Sprintf("Robinhood closes: %v", plan.RHCloseReport.ClosedCoins))
		}
		if residue := formatRobinhoodResidue(plan.RHCloseReport.Residue); residue != "" {
			parts = append(parts, residue)
		}
		if len(plan.TSCloseReport.ClosedCoins) > 0 {
			parts = append(parts, fmt.Sprintf("TopStep closes: %v", plan.TSCloseReport.ClosedCoins))
		}
		if len(plan.LimitOrderReport.Cancelled) > 0 {
			parts = append(parts, fmt.Sprintf("cancelled resting limit orders: %s", strings.Join(plan.LimitOrderReport.Cancelled, ", ")))
		}
		header := "**PORTFOLIO KILL SWITCH**"
		gapNotes := []string{}
		if plan.OKXSpotPresent {
			gapNotes = append(gapNotes, "OKX spot strategies present — kill switch cannot auto-close spot, verify balances manually")
		}
		if plan.RHOptionsPresent {
			gapNotes = append(gapNotes, "Robinhood options strategies present — kill switch cannot auto-close options, verify manually")
		}
		if len(gapNotes) > 0 {
			header = "**PORTFOLIO KILL SWITCH (GAPS — VERIFY MANUALLY)**"
			parts = append(parts, gapNotes...)
		}
		summary := strings.Join(parts, "; ")
		return fmt.Sprintf("%s\n%s\n%s. %s", header, portfolioReason, summary, killSwitchManualResetLine)
	}

	var segments []string

	if len(plan.LimitOrderReport.Unresolved) > 0 {
		segments = append(segments, "Resting limit orders NOT confirmed cancelled (they can still fill and re-enter) — "+strings.Join(plan.LimitOrderReport.Unresolved, "; "))
	}
	if len(plan.LimitOrderReport.Cancelled) > 0 {
		segments = append(segments, "Cancelled resting limit orders — "+strings.Join(plan.LimitOrderReport.Cancelled, ", "))
	}
	if len(plan.HLProof.Holds) > 0 {
		segments = append(segments, "HL flat not verified — "+strings.Join(plan.HLProof.Holds, "; "))
	}
	if len(plan.CloseReport.Errors) > 0 {
		parts := make([]string, 0, len(plan.CloseReport.Errors))
		for _, coin := range plan.CloseReport.SortedErrorCoins() {
			parts = append(parts, fmt.Sprintf("%s: %v", coin, plan.CloseReport.Errors[coin]))
		}
		segments = append(segments, "HL live close not flat (closer replies are reports; the size comes from a fresh read) — "+strings.Join(parts, "; "))
	}
	if len(plan.HLProof.Changes) > 0 {
		segments = append(segments, "HL exposure changed since the pre-close read — "+strings.Join(plan.HLProof.Changes, "; "))
	}
	if len(plan.OKXProof.Holds) > 0 {
		segments = append(segments, "OKX flat not verified — "+strings.Join(plan.OKXProof.Holds, "; "))
	}
	if len(plan.OKXCloseReport.Errors) > 0 {
		parts := make([]string, 0, len(plan.OKXCloseReport.Errors))
		for _, coin := range plan.OKXCloseReport.SortedErrorCoins() {
			parts = append(parts, fmt.Sprintf("%s: %v", coin, plan.OKXCloseReport.Errors[coin]))
		}
		segments = append(segments, "OKX live close not flat (closer replies are reports; the size comes from a fresh read) — "+strings.Join(parts, "; "))
	}
	if len(plan.OKXProof.Changes) > 0 {
		segments = append(segments, "OKX exposure changed since the pre-close read — "+strings.Join(plan.OKXProof.Changes, "; "))
	}
	if len(plan.Unconfigured) > 0 {
		names := make([]string, 0, len(plan.Unconfigured))
		for _, p := range plan.Unconfigured {
			names = append(names, fmt.Sprintf("%s szi=%.6f", p.Coin, p.Size))
		}
		sort.Strings(names)
		segments = append(segments, "On-chain HL positions for unconfigured coins (manual intervention required) — "+strings.Join(names, "; "))
	}
	if len(plan.OKXUnconfigured) > 0 {
		names := make([]string, 0, len(plan.OKXUnconfigured))
		for _, p := range plan.OKXUnconfigured {
			names = append(names, fmt.Sprintf("%s %s %s", p.InstID, p.Side, killSwitchSideSize(p.Contracts)))
		}
		sort.Strings(names)
		segments = append(segments, "On-chain OKX positions outside every configured close route (manual intervention required) — "+strings.Join(names, "; "))
	}
	if len(plan.RHCloseReport.Errors) > 0 {
		parts := make([]string, 0, len(plan.RHCloseReport.Errors))
		for _, coin := range plan.RHCloseReport.SortedErrorCoins() {
			parts = append(parts, fmt.Sprintf("%s: %v", coin, plan.RHCloseReport.Errors[coin]))
		}
		segments = append(segments, "Robinhood live close errors — "+strings.Join(parts, "; "))
	}
	if residue := formatRobinhoodResidue(plan.RHCloseReport.Residue); residue != "" {
		segments = append(segments, residue)
	}
	if len(plan.RHUnconfigured) > 0 {
		names := make([]string, 0, len(plan.RHUnconfigured))
		for _, p := range plan.RHUnconfigured {
			names = append(names, fmt.Sprintf("%s size=%.6f", p.Coin, p.Size))
		}
		sort.Strings(names)
		segments = append(segments, "Live Robinhood balances for unconfigured coins (manual intervention required) — "+strings.Join(names, "; "))
	}
	if len(plan.TSCloseReport.Errors) > 0 {
		parts := make([]string, 0, len(plan.TSCloseReport.Errors))
		for _, coin := range plan.TSCloseReport.SortedErrorCoins() {
			parts = append(parts, fmt.Sprintf("%s: %v", coin, plan.TSCloseReport.Errors[coin]))
		}
		segments = append(segments, "TopStep live close errors — "+strings.Join(parts, "; "))
	}
	if len(plan.TSUnconfigured) > 0 {
		names := make([]string, 0, len(plan.TSUnconfigured))
		for _, p := range plan.TSUnconfigured {
			names = append(names, fmt.Sprintf("%s size=%d", p.Coin, p.Size))
		}
		sort.Strings(names)
		segments = append(segments, "Live TopStep positions for unconfigured symbols (manual intervention required) — "+strings.Join(names, "; "))
	}
	if plan.OKXSpotPresent {
		segments = append(segments, "OKX spot strategies present — verify manually (kill switch cannot auto-close spot)")
	}
	if plan.RHOptionsPresent {
		segments = append(segments, "Robinhood options strategies present — verify manually (kill switch cannot auto-close options)")
	}
	if len(segments) == 0 {
		segments = append(segments, "Could not fetch on-chain state to confirm flat")
	}

	return fmt.Sprintf("**PORTFOLIO KILL SWITCH (LATCHED, RETRYING)**\n%s\n%s. Virtual state preserved. Next cycle will retry.", portfolioReason, strings.Join(segments, " | "))
}

func killSwitchVerifiedVenues(plan KillSwitchClosePlan) string {
	var venues []string
	if plan.HLProof.Applicable && plan.HLProof.Verified {
		venues = append(venues, "HL")
	}
	if plan.OKXProof.Applicable && plan.OKXProof.Verified {
		venues = append(venues, "OKX")
	}
	if len(venues) == 0 {
		return ""
	}
	return strings.Join(venues, " and ") + " positions verified flat by a fresh account read after the last close attempt"
}

func killSwitchObservationNote(plan KillSwitchClosePlan) string {
	when := "the cycle that prepared this prompt"
	if !plan.ObservedAt.IsZero() {
		when = fmt.Sprintf("the cycle that prepared this prompt (%s)", plan.ObservedAt.UTC().Format(time.RFC3339))
	}
	note := fmt.Sprintf("The live position status above is an observation from %s, and positions can change before you reply. Verify current venue positions before you reply.", when)
	if !plan.OnChainConfirmedFlat {
		note += " At that observation the on-chain close was still retrying, and resting stop-losses may already be cancelled ahead of the flatten attempt — do not assume any remaining position is protected."
	}
	return note
}

func formatRobinhoodResidue(residue []RobinhoodPosition) string {
	if len(residue) == 0 {
		return ""
	}
	names := make([]string, 0, len(residue))
	for _, p := range residue {
		names = append(names, fmt.Sprintf("%s %.10g", p.Coin, p.Size))
	}
	sort.Strings(names)
	return "Robinhood holdings above every go-trader book, not sold (the operator's coin) — " + strings.Join(names, "; ")
}

func killSwitchInstanceLabel(configPath string) string {
	dir := filepath.Base(filepath.Dir(configPath))
	if dir != "" && dir != "." && dir != string(filepath.Separator) {
		return dir
	}
	if host, err := os.Hostname(); err == nil && host != "" {
		return host
	}
	return "go-trader"
}

func formatKillSwitchResetPrompt(instanceLabel, hlAddr string, plan KillSwitchClosePlan, part RiskPartition, latched []RiskPartition) string {
	identity := instanceLabel
	if hlAddr != "" && part.IsLive() {
		identity = fmt.Sprintf("%s (Hyperliquid %s)", identity, hlAddr)
	}
	reply := "reset"
	if len(latched) > 1 {
		reply = "reset " + part.String()
	}
	resetNote := fmt.Sprintf("Replying '%s' only clears the %s kill switch latch so trading can resume next cycle — it does not itself close or protect any position.", reply, partitionLabel(part))
	if part.IsLive() {
		resetNote += " " + killSwitchObservationNote(plan)
	}
	if len(latched) > 1 {
		resetNote += fmt.Sprintf(" %d scopes are latched (%s); this prompt clears the %s scope only.", len(latched), joinScopeLabels(latched), partitionLabel(part))
	}
	return fmt.Sprintf("[KILL SWITCH %s] %s\n%s\n\n%s\nReply '%s' to proceed.", partitionLabel(part), identity, plan.DiscordMessage, resetNote, reply)
}

func formatKillSwitchResetPromptForScopes(instanceLabel, hlAddr string, plans map[RiskPartition]KillSwitchClosePlan, parts []RiskPartition, latched []RiskPartition) string {
	if len(parts) == 1 {
		return formatKillSwitchResetPrompt(instanceLabel, hlAddr, plans[parts[0]], parts[0], latched)
	}
	sections := make([]string, 0, len(parts))
	for _, part := range parts {
		identity := instanceLabel
		if hlAddr != "" && part.IsLive() {
			identity = fmt.Sprintf("%s (Hyperliquid %s)", identity, hlAddr)
		}
		section := fmt.Sprintf("[KILL SWITCH %s] %s\n%s", partitionLabel(part), identity, plans[part].DiscordMessage)
		if part.IsLive() {
			section += "\n" + killSwitchObservationNote(plans[part])
		}
		sections = append(sections, section)
	}
	note := fmt.Sprintf("%d scopes are latched (%s). One reply clears one scope only and does not itself close or protect any position; a new prompt follows for any scope still latched.", len(latched), joinScopeLabels(latched))
	return fmt.Sprintf("%s\n\n%s\nReply %s to proceed.", strings.Join(sections, "\n\n"), note, killSwitchResetReplyOptions(latched))
}
