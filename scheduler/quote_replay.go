package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	quoteReplayProtocolName    = "go-trader.quote-replay"
	quoteReplayProtocolVersion = 1
	quoteReplayMaxSpecBytes    = 1 << 31
	quoteReplayMaxDeadlines    = 200_000
	quoteReplayClockOwner      = "quote-replay"
	quoteReplayOwnerID         = "quote-replay-owner"
	quoteReplayTimeBasis       = "receipt_ms"
	quoteReplayFundingSource   = "quote-replay-pinned"
	quoteReplayCandleTimeframe = "1m"
	quoteReplayFutureSkewMs    = feedBookMaxFutureSkewMs
)

var quoteReplayModelNotes = map[string]string{
	"time_basis":     "every market event, acceptance, fill, cancel, coverage and position sum uses the recorded first-receipt time plus modeled latency; exchange event times only bound freshness",
	"queue":          "strict_trade_through_v1: a resting quote fills only on a later deduplicated opposing aggressor trade strictly through its price, at its own price as maker, capped at the frozen participation of each trade across all own orders (price, then acceptance time priority); trades at the quote price only reduce the queue ahead; depth increases at the price are charged ahead; depth decreases never improve rank; a replacement starts a new rank",
	"aggressor_side": "a recorded Hyperliquid trade side A is a sell aggressor that hits bids and side B is a buy aggressor that lifts asks (assumption)",
	"stop":           "a reduce-only stop triggers on the first recorded trade received strictly after its acceptance at a price at or through the trigger, then fills as a taker against the last recorded book at that receipt, level by level (any remainder past the recorded depth at the deepest level), less the frozen slippage reserve",
	"close":          "the inventory exit is a reduce-only immediate-or-cancel order filled as a taker against the recorded book at its effect time inside its limit price, less the slippage reserve; the remainder is cancelled",
	"reduce_only":    "reduce-only orders fill only toward flat up to the venue position; when the position in their reducing direction reaches zero they are cancelled (assumption)",
	"minimum":        "the minimum order value applies to risk-increasing quotes; reduce-only quotes, stops and closes below it are assumed accepted (unverified venue rule)",
	"fees":           "maker fee on quote fills, taker fee on stop and close fills, each rounded up to 12 decimals; no fee corrections",
	"latency":        "each place or cancel takes effect at max(call clock, previous effect) + its frozen latency (at least 1 ms); an effect at or after the next deadline refuses the run",
	"funding":        "pinned public funding records settle through the shared paper funding core inside the quote settlement; no other funding writer runs",
	"latches":        "circuit-breaker and portfolio kill-switch triggers are not evaluated by the runner, so their exits are outside the replay economics",
}

type quoteReplaySpec struct {
	Protocol  string                  `json:"protocol"`
	Version   int                     `json:"version"`
	Mode      string                  `json:"mode"`
	Run       basketReplayRunSpec     `json:"run"`
	Coin      string                  `json:"coin"`
	Window    quoteReplayWindow       `json:"window"`
	Params    map[string]interface{}  `json:"params"`
	Config    quoteReplayConfigSpec   `json:"config"`
	Quote     quoteReplayQuoteSpec    `json:"quote"`
	Lot       quoteReplayLotSpec      `json:"lot"`
	Costs     quoteReplayCostSpec     `json:"costs"`
	Latency   quoteReplayLatencySpec  `json:"latency"`
	Queue     quoteReplayQueueSpec    `json:"queue"`
	Funding   quoteReplayFundingSpec  `json:"funding"`
	Books     []quoteReplayBookSpec   `json:"books"`
	BookGaps  []quoteReplayGapSpec    `json:"book_gaps"`
	Trades    []quoteReplayTradeSpec  `json:"trades"`
	TradeCov  []quoteReplayRangeSpec  `json:"trade_coverage"`
	Scoring   quoteReplayScoringSpec  `json:"scoring"`
	books     []*quoteReplayBook      `json:"-"`
	trades    []*quoteReplayTrade     `json:"-"`
	funding   []feedFundingRecord     `json:"-"`
	coverage  [][2]int64              `json:"-"`
	gaps      [][2]int64              `json:"-"`
	deadlines []int64                 `json:"-"`
	params    quotePolicyParams       `json:"-"`
	costs     quotePaperCosts         `json:"-"`
	bookChain []string                `json:"-"`
	tradeIdx  []int64                 `json:"-"`
	tradeHash []string                `json:"-"`
	recvIndex []int64                 `json:"-"`
	fundRaw   []quoteReplayFundingRow `json:"-"`
}

type quoteReplayWindow struct {
	FirstDeadlineMs int64  `json:"first_deadline_ms"`
	LastDeadlineMs  int64  `json:"last_deadline_ms"`
	Label           string `json:"label"`
	MaxDeadlines    int    `json:"max_deadlines"`
}

type quoteReplayConfigSpec struct {
	IntervalSeconds int     `json:"interval_seconds"`
	MaxInventoryUSD float64 `json:"max_inventory_usd"`
	StopLossPct     float64 `json:"stop_loss_pct"`
}

type quoteReplayQuoteSpec struct {
	MaxOrderQty          float64 `json:"max_order_qty"`
	RequestExpiryMs      int64   `json:"request_expiry_ms"`
	FeeReserveRate       float64 `json:"fee_reserve_rate"`
	MarginPriceBufferPct float64 `json:"margin_price_buffer_pct"`
}

type quoteReplayLotSpec struct {
	SzDecimals int `json:"sz_decimals"`
}

type quoteReplayCostSpec struct {
	MakerFeeBps        string `json:"maker_fee_bps"`
	TakerFeeBps        string `json:"taker_fee_bps"`
	SlippageReserveBps string `json:"slippage_reserve_bps"`
	MinOrderValueUSD   string `json:"min_order_value_usd"`
}

type quoteReplayLatencySpec struct {
	PlaceMs  int64 `json:"place_ms"`
	CancelMs int64 `json:"cancel_ms"`
}

type quoteReplayQueueSpec struct {
	Model         string `json:"model"`
	Participation string `json:"participation"`
}

type quoteReplayFundingRow struct {
	TimeMs int64  `json:"time"`
	Rate   string `json:"rate"`
}

type quoteReplayFundingSpec struct {
	FromMs  int64                   `json:"from_ms"`
	Records []quoteReplayFundingRow `json:"records"`
}

type quoteReplayBookSpec struct {
	RecvMs   int64       `json:"recv_ms"`
	EventMs  int64       `json:"event_ms"`
	Session  int64       `json:"session"`
	Seq      int64       `json:"seq"`
	Identity string      `json:"identity"`
	Bids     [][3]string `json:"bids"`
	Asks     [][3]string `json:"asks"`
}

type quoteReplayGapSpec struct {
	FromMs int64  `json:"from_ms"`
	ToMs   int64  `json:"to_ms"`
	Reason string `json:"reason"`
}

type quoteReplayRangeSpec struct {
	FromMs int64 `json:"from_ms"`
	ToMs   int64 `json:"to_ms"`
}

type quoteReplayTradeSpec struct {
	RecvMs int64  `json:"recv_ms"`
	TimeMs int64  `json:"time"`
	Tid    uint64 `json:"tid"`
	Side   string `json:"side"`
	Px     string `json:"px"`
	Sz     string `json:"sz"`
	Hash   string `json:"hash"`
}

type quoteReplayScoringSpec struct {
	Scored bool     `json:"scored"`
	Limits []string `json:"limits,omitempty"`
}

func decodeQuoteReplaySpec(r io.Reader) (*quoteReplaySpec, []byte, error) {
	raw, err := io.ReadAll(io.LimitReader(r, quoteReplayMaxSpecBytes+1))
	if err != nil {
		return nil, nil, refuseReplay("spec_unreadable", err.Error())
	}
	if len(raw) > quoteReplayMaxSpecBytes {
		return nil, nil, refuseReplay("spec_invalid", "the replay specification is over the size bound")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	dec.UseNumber()
	var spec quoteReplaySpec
	if err := dec.Decode(&spec); err != nil {
		return nil, nil, refuseReplay("spec_malformed", err.Error())
	}
	if dec.More() {
		return nil, nil, refuseReplay("spec_malformed", "trailing data after the replay specification")
	}
	return &spec, raw, nil
}

func quoteReplayRat(name, text string, allowNegative bool) (*big.Rat, error) {
	r, err := parseQuoteDecimal(text)
	if err != nil {
		return nil, fmt.Errorf("%s: %v", name, err)
	}
	if !allowNegative && r.Sign() < 0 {
		return nil, fmt.Errorf("%s must be >= 0, got %s", name, text)
	}
	return r, nil
}

func parseQuoteReplayLevels(rows [][3]string) ([]quoteReplayLevel, error) {
	out := make([]quoteReplayLevel, 0, len(rows))
	for _, row := range rows {
		px, err := parseQuoteDecimal(row[0])
		if err != nil || px.Sign() <= 0 {
			return nil, fmt.Errorf("level price %q is not positive", row[0])
		}
		sz, err := parseQuoteDecimal(row[1])
		if err != nil || sz.Sign() <= 0 {
			return nil, fmt.Errorf("level size %q is not positive", row[1])
		}
		if _, err := strconv.ParseUint(row[2], 10, 64); err != nil {
			return nil, fmt.Errorf("level order count %q is not an unsigned integer", row[2])
		}
		out = append(out, quoteReplayLevel{Px: px, Sz: sz})
	}
	return out, nil
}

func validateQuoteReplaySpec(spec *quoteReplaySpec) error {
	var errs []string
	add := func(format string, args ...any) { errs = append(errs, fmt.Sprintf(format, args...)) }
	if spec.Protocol != quoteReplayProtocolName || spec.Version != quoteReplayProtocolVersion {
		add("protocol must be %s version %d, got %q version %d", quoteReplayProtocolName, quoteReplayProtocolVersion, spec.Protocol, spec.Version)
	}
	if spec.Mode != basketReplayModeNew && spec.Mode != basketReplayModeResume {
		add("mode must be new or resume, got %q", spec.Mode)
	}
	if !basketReplayRunIDPattern.MatchString(spec.Run.RunID) {
		add("run.run_id %q is not a valid identifier", spec.Run.RunID)
	}
	if !filepath.IsAbs(spec.Run.StateDir) {
		add("run.state_dir %q must be an absolute path", spec.Run.StateDir)
	}
	for k := range spec.Run.Source {
		lk := strings.ToLower(k)
		for _, w := range []string{"secret", "private", "password", "mnemonic", "api_key", "apikey", "token", "wallet", "credential"} {
			if strings.Contains(lk, w) {
				add("run.source key %q names credential material; a replay takes no credentials", k)
			}
		}
	}
	coin := strings.ToUpper(strings.TrimSpace(spec.Coin))
	if coin == "" || coin != spec.Coin {
		add("coin %q must be a non-empty upper-case Hyperliquid perps coin", spec.Coin)
	}
	p, err := parseQuotePolicyParams(spec.Params)
	if err != nil {
		add("params: %v", err)
	}
	spec.params = p
	if len(spec.Params) != len(quotePolicySpec.Params) {
		add("params must list every specification parameter explicitly (%d given, %d specified)", len(spec.Params), len(quotePolicySpec.Params))
	}
	w := spec.Window
	cycleMs := int64(spec.Config.IntervalSeconds) * 1000
	if w.FirstDeadlineMs <= 0 || w.FirstDeadlineMs%1000 != 0 || w.LastDeadlineMs < w.FirstDeadlineMs || w.LastDeadlineMs%1000 != 0 {
		add("window deadlines must be positive whole seconds with first <= last")
	} else if cycleMs > 0 {
		if (w.LastDeadlineMs-w.FirstDeadlineMs)%cycleMs != 0 {
			add("the window length must be a whole number of %dms cycles", cycleMs)
		}
		n := (w.LastDeadlineMs-w.FirstDeadlineMs)/cycleMs + 1
		limit := int64(w.MaxDeadlines)
		if limit <= 0 || limit > quoteReplayMaxDeadlines {
			add("window.max_deadlines must be in [1, %d]", quoteReplayMaxDeadlines)
		} else if n > limit {
			add("the window holds %d deadlines, above its frozen bound %d; split it into bounded runs", n, limit)
		} else {
			for d := w.FirstDeadlineMs; d <= w.LastDeadlineMs; d += cycleMs {
				spec.deadlines = append(spec.deadlines, d)
			}
		}
	}
	if spec.Quote.MaxOrderQty <= 0 || spec.Quote.RequestExpiryMs <= 0 || spec.Quote.RequestExpiryMs > quoteMaxRequestExpiryMs || spec.Quote.FeeReserveRate < 0 || spec.Quote.MarginPriceBufferPct < 0 {
		add("quote: max_order_qty must be > 0, request_expiry_ms in (0, %d], reserves >= 0", quoteMaxRequestExpiryMs)
	}
	if spec.Lot.SzDecimals < 0 || spec.Lot.SzDecimals > 8 {
		add("lot.sz_decimals must be in [0, 8]")
	}
	costs := quotePaperCosts{PlaceLatencyMs: spec.Latency.PlaceMs, CancelLatencyMs: spec.Latency.CancelMs}
	bps := func(name, text string, neg bool) *big.Rat {
		r, err := quoteReplayRat(name, text, neg)
		if err != nil {
			add("costs.%s", err.Error())
			return quoteRatZero()
		}
		return quoteRatQuo(r, big.NewRat(10000, 1))
	}
	costs.MakerRate = bps("maker_fee_bps", spec.Costs.MakerFeeBps, true)
	costs.TakerRate = bps("taker_fee_bps", spec.Costs.TakerFeeBps, false)
	costs.SlipRate = bps("slippage_reserve_bps", spec.Costs.SlippageReserveBps, false)
	if mv, err := quoteReplayRat("min_order_value_usd", spec.Costs.MinOrderValueUSD, false); err != nil {
		add("costs.%s", err.Error())
	} else {
		costs.MinOrderValue = mv
	}
	if spec.Latency.PlaceMs < 1 || spec.Latency.CancelMs < 1 || spec.Latency.PlaceMs > 10_000 || spec.Latency.CancelMs > 10_000 {
		add("latency place_ms and cancel_ms must be in [1, 10000]")
	}
	if spec.Queue.Model != quoteQueueModelStrict {
		add("queue.model must be %q", quoteQueueModelStrict)
	}
	if part, ok := new(big.Rat).SetString(spec.Queue.Participation); !ok || part.Sign() <= 0 || part.Cmp(big.NewRat(1, 1)) > 0 {
		add("queue.participation must be a fraction in (0, 1]")
	} else {
		costs.Participation = part
	}
	spec.costs = costs
	var lastRecv int64
	for i, b := range spec.Books {
		bids, err := parseQuoteReplayLevels(b.Bids)
		if err != nil {
			add("books[%d]: %v", i, err)
			continue
		}
		asks, err := parseQuoteReplayLevels(b.Asks)
		if err != nil {
			add("books[%d]: %v", i, err)
			continue
		}
		if b.RecvMs <= 0 || b.RecvMs < lastRecv || b.EventMs <= 0 || b.EventMs > b.RecvMs+quoteReplayFutureSkewMs || len(bids) == 0 || len(asks) == 0 || len(bids) > feedBookMaxLevels || len(asks) > feedBookMaxLevels {
			add("books[%d] has out-of-order or invalid receipt %d / event %d times or empty or oversized sides", i, b.RecvMs, b.EventMs)
			continue
		}
		for j := 1; j < len(bids); j++ {
			if bids[j].Px.Cmp(bids[j-1].Px) >= 0 {
				add("books[%d] bids are not strictly descending", i)
			}
		}
		for j := 1; j < len(asks); j++ {
			if asks[j].Px.Cmp(asks[j-1].Px) <= 0 {
				add("books[%d] asks are not strictly ascending", i)
			}
		}
		if bids[0].Px.Cmp(asks[0].Px) >= 0 {
			add("books[%d] is crossed", i)
		}
		lastRecv = b.RecvMs
		spec.books = append(spec.books, &quoteReplayBook{RecvMs: b.RecvMs, EventMs: b.EventMs, Session: b.Session, Seq: b.Seq, Identity: b.Identity, Bids: bids, Asks: asks, BidText: b.Bids[0][0], AskText: b.Asks[0][0]})
	}
	lastRecv = 0
	seenTid := map[uint64]bool{}
	for i, t := range spec.Trades {
		px, err1 := parseQuoteDecimal(t.Px)
		sz, err2 := parseQuoteDecimal(t.Sz)
		if err1 != nil || err2 != nil || px.Sign() <= 0 || sz.Sign() <= 0 || (t.Side != quotePaperAggressorBuy && t.Side != quotePaperAggressorSell) || t.RecvMs < lastRecv || t.TimeMs <= 0 || t.TimeMs > t.RecvMs+quoteReplayFutureSkewMs {
			add("trades[%d] is invalid or out of receipt order", i)
			continue
		}
		if seenTid[t.Tid] {
			add("trades[%d] repeats trade id %d; the plan must hold deduplicated trades", i, t.Tid)
			continue
		}
		seenTid[t.Tid] = true
		lastRecv = t.RecvMs
		spec.trades = append(spec.trades, &quoteReplayTrade{RecvMs: t.RecvMs, TimeMs: t.TimeMs, Tid: t.Tid, Side: t.Side, Px: px, Sz: sz, PxText: t.Px, SzText: t.Sz, Hash: t.Hash})
	}
	for i, g := range spec.BookGaps {
		if g.ToMs < g.FromMs || g.FromMs <= 0 {
			add("book_gaps[%d] is not an ordered positive range", i)
			continue
		}
		spec.gaps = append(spec.gaps, [2]int64{g.FromMs, g.ToMs})
	}
	var lastTo int64
	for i, c := range spec.TradeCov {
		if c.ToMs < c.FromMs || c.FromMs <= lastTo {
			add("trade_coverage[%d] is not an ordered disjoint range", i)
			continue
		}
		lastTo = c.ToMs
		spec.coverage = append(spec.coverage, [2]int64{c.FromMs, c.ToMs})
	}
	var lastFund int64
	for i, f := range spec.Funding.Records {
		rate, err := parseQuoteDecimal(f.Rate)
		if err != nil || f.TimeMs <= lastFund {
			add("funding.records[%d] is unreadable or out of order", i)
			continue
		}
		rf, _ := rate.Float64()
		lastFund = f.TimeMs
		spec.funding = append(spec.funding, feedFundingRecord{Rate: rf, TimeMs: f.TimeMs})
		spec.fundRaw = append(spec.fundRaw, f)
	}
	if spec.Funding.FromMs <= 0 || (len(spec.funding) > 0 && spec.funding[0].TimeMs < spec.Funding.FromMs) {
		add("funding.from_ms must be positive and at or before the first record")
	}
	if len(errs) > 0 {
		sort.Strings(errs)
		return refuseReplay("spec_invalid", errs...)
	}
	sc := spec.strategyConfigForChecks()
	if cerrs := inventoryMarketMakingConfigErrors(sc, spec.Config.IntervalSeconds); len(cerrs) > 0 {
		return refuseReplay("config_refused", cerrs...)
	}
	spec.indexEvidence()
	return nil
}

func (spec *quoteReplaySpec) strategyConfigForChecks() StrategyConfig {
	stop := spec.Config.StopLossPct
	return StrategyConfig{ID: quoteReplayOwnerID, IntervalSeconds: spec.Config.IntervalSeconds, StopLossPct: &stop,
		OpenStrategy: StrategyRef{Name: inventoryMarketMakingStrategyName, Params: spec.Params},
		Quote:        &QuoteConfig{Coin: spec.Coin, MaxInventoryUSD: spec.Config.MaxInventoryUSD}}
}

func quoteReplayChain(prev string, item []byte) string {
	h := sha256.New()
	h.Write([]byte(prev))
	h.Write([]byte{0})
	h.Write(item)
	return hex.EncodeToString(h.Sum(nil))
}

func (spec *quoteReplaySpec) indexEvidence() {
	prev := ""
	spec.bookChain = make([]string, len(spec.books))
	spec.recvIndex = make([]int64, len(spec.books))
	for i, b := range spec.books {
		blob, _ := json.Marshal(spec.Books[i])
		prev = quoteReplayChain(prev, blob)
		spec.bookChain[i] = prev
		spec.recvIndex[i] = b.RecvMs
	}
	prev = ""
	spec.tradeHash = make([]string, len(spec.trades))
	spec.tradeIdx = make([]int64, len(spec.trades))
	for i, t := range spec.trades {
		blob, _ := json.Marshal(spec.Trades[i])
		prev = quoteReplayChain(prev, blob)
		spec.tradeHash[i] = prev
		spec.tradeIdx[i] = t.RecvMs
	}
}

func (spec *quoteReplaySpec) marketEvents() []quoteMarketEvent {
	out := make([]quoteMarketEvent, 0, len(spec.books)+len(spec.trades))
	i, j := 0, 0
	for i < len(spec.books) || j < len(spec.trades) {
		if j >= len(spec.trades) || (i < len(spec.books) && spec.books[i].RecvMs <= spec.trades[j].RecvMs) {
			out = append(out, quoteMarketEvent{RecvMs: spec.books[i].RecvMs, Book: spec.books[i]})
			i++
			continue
		}
		out = append(out, quoteMarketEvent{RecvMs: spec.trades[j].RecvMs, Trade: spec.trades[j]})
		j++
	}
	return out
}

func (spec *quoteReplaySpec) configDocument(stateDir string) ([]byte, error) {
	params := map[string]interface{}{}
	for k, v := range spec.Params {
		params[k] = v
	}
	sc := map[string]interface{}{
		"id":               quoteReplayOwnerID,
		"type":             "perps",
		"platform":         "hyperliquid",
		"script":           hyperliquidCheckScript,
		"args":             []interface{}{inventoryMarketMakingStrategyName, spec.Coin, quoteReplayCandleTimeframe, "--mode=paper"},
		"open_strategy":    map[string]interface{}{"name": inventoryMarketMakingStrategyName, "params": params},
		"close_strategy":   map[string]interface{}{"name": inventoryQuoteExitCloseName},
		"interval_seconds": spec.Config.IntervalSeconds,
		"capital":          quotePolicySpec.Fixed["capital"],
		"leverage":         quotePolicySpec.Fixed["leverage"],
		"margin_mode":      quotePolicySpec.Fixed["margin_mode"],
		"stop_loss_pct":    spec.Config.StopLossPct,
		"circuit_breaker":  false,
		"quote": map[string]interface{}{
			"coin": spec.Coin, "max_inventory_usd": spec.Config.MaxInventoryUSD, "max_order_qty": spec.Quote.MaxOrderQty,
			"request_expiry_ms": spec.Quote.RequestExpiryMs, "fee_reserve_rate": spec.Quote.FeeReserveRate,
			"margin_price_buffer_pct": spec.Quote.MarginPriceBufferPct,
		},
	}
	root := map[string]interface{}{
		"config_version":   CurrentConfigVersion,
		"interval_seconds": spec.Config.IntervalSeconds,
		"db_file":          filepath.Join(stateDir, quoteReplayDBName),
		"log_dir":          filepath.Join(stateDir, quoteReplayLogDirName),
		"market_feed":      marketFeedWebsocket,
		"strategies":       []interface{}{sc},
	}
	return json.MarshalIndent(root, "", "  ")
}

func quoteReplayCanonical(v any) string {
	blob, _ := json.Marshal(v)
	return sha256Hex(blob)
}

func (spec *quoteReplaySpec) fingerprints(policyFP string) (quoteReplayFingerprints, error) {
	fp := quoteReplayFingerprints{Policy: policyFP}
	fp.Protocol = quoteReplayCanonical(struct {
		Name    string            `json:"name"`
		Version int               `json:"version"`
		Spec    string            `json:"policy_spec_sha256"`
		Notes   map[string]string `json:"model_notes"`
	}{quoteReplayProtocolName, quoteReplayProtocolVersion, quotePolicySpecSHA256(), quoteReplayModelNotes})
	fp.Source = basketReplaySourceFingerprint(spec.Run.Source)
	fp.Data = quoteReplayCanonical(struct {
		Coin     string                 `json:"coin"`
		Books    []quoteReplayBookSpec  `json:"books"`
		Gaps     []quoteReplayGapSpec   `json:"book_gaps"`
		Trades   []quoteReplayTradeSpec `json:"trades"`
		Coverage []quoteReplayRangeSpec `json:"trade_coverage"`
		Funding  quoteReplayFundingSpec `json:"funding"`
		Lot      quoteReplayLotSpec     `json:"lot"`
	}{spec.Coin, spec.Books, spec.BookGaps, spec.Trades, spec.TradeCov, spec.Funding, spec.Lot})
	portable, err := spec.configDocument("/quote-replay-state")
	if err != nil {
		return fp, err
	}
	fp.Config = sha256Hex(portable)
	fp.Model = quoteReplayCanonical(struct {
		Window  quoteReplayWindow      `json:"window"`
		Costs   quoteReplayCostSpec    `json:"costs"`
		Latency quoteReplayLatencySpec `json:"latency"`
		Queue   quoteReplayQueueSpec   `json:"queue"`
		Scoring quoteReplayScoringSpec `json:"scoring"`
	}{spec.Window, spec.Costs, spec.Latency, spec.Queue, spec.Scoring})
	fp.Run = quoteReplayCanonical([]string{fp.Protocol, spec.Run.RunID, fp.Source, fp.Data, fp.Config, fp.Policy, fp.Model})
	return fp, nil
}

type quoteReplayOptions struct {
	MaxDeadlines int
	Fault        *quoteReplayFaultState
}

type quoteReplayRuntime struct {
	spec      *quoteReplaySpec
	opts      quoteReplayOptions
	fp        quoteReplayFingerprints
	manifest  quoteReplayManifest
	cfg       *Config
	cfgJSON   []byte
	store     *StateStore
	owner     *stateOwnership
	file      *StateDB
	clock     *simulatedClock
	adm       *quoteAdmission
	venue     *quotePaperVenue
	ctx       quotePolicyContext
	cleanups  []func()
	cursorMs  int64
	nextIndex int
	resumed   bool
	pending   *quoteReplayResultRow
	stopped   string
	timings   []int64
	unscored  []string
	logf      func(format string, args ...any)
}

func (r *quoteReplayRuntime) Close() {
	for i := len(r.cleanups) - 1; i >= 0; i-- {
		r.cleanups[i]()
	}
	r.cleanups = nil
}

func quoteReplayConfigRefusal(cfg *Config, spec *quoteReplaySpec, stateDir string) error {
	var errs []string
	if len(cfg.Strategies) != 1 {
		errs = append(errs, fmt.Sprintf("the replay runs exactly one quote owner, got %d strategies", len(cfg.Strategies)))
	}
	if len(cfg.PaperSources) > 0 || strings.TrimSpace(cfg.PaperDBFile) != "" || cfg.SharedMarketFeed != nil || cfg.Egress != nil {
		errs = append(errs, "the replay owns one state file; paper sources, a paper db file, a shared feed and egress settings are refused")
	}
	if filepath.Dir(cfg.DBFile) != stateDir {
		errs = append(errs, fmt.Sprintf("db_file %q is outside the run state directory", cfg.DBFile))
	}
	if len(cfg.Baskets) > 0 || len(cfg.Pairs) > 0 {
		errs = append(errs, "baskets and pairs are refused in a quote replay")
	}
	for _, sc := range cfg.Strategies {
		if sc.ID != quoteReplayOwnerID || sc.Quote == nil || effectiveOpenStrategy(sc) != inventoryMarketMakingStrategyName {
			errs = append(errs, fmt.Sprintf("strategy %s is not the single inventory_market_making quote owner", sc.ID))
		}
		if isLiveArgs(sc.Args) || partitionFor(sc).IsLive() || sc.PaperSource != "" {
			errs = append(errs, fmt.Sprintf("strategy %s is live-shaped or names a paper source", sc.ID))
		}
		errs = append(errs, inventoryMarketMakingConfigErrors(sc, cfg.IntervalSeconds)...)
		if sc.CircuitBreakerEnabled() {
			errs = append(errs, "the replay freezes the circuit breaker off")
		}
	}
	req, err := deriveFeedRequirements(cfg)
	if err != nil {
		errs = append(errs, err.Error())
	} else {
		if len(req.Observations) > 0 {
			errs = append(errs, "the configuration needs observations the replay does not freeze")
		}
		if len(req.Books) != 1 {
			errs = append(errs, fmt.Sprintf("the configuration needs %d l2_book keys; the replay freezes exactly the owner's one", len(req.Books)))
		}
		for k, need := range req.Books {
			want := contractBookNeed(inventoryMarketMakingBookWindowMs(spec.params), inventoryMarketMakingBookCapacity, inventoryMarketMakingBookDepth)
			if k != l2BookKeyFor(spec.Coin) || need != want {
				errs = append(errs, fmt.Sprintf("the l2_book need %s %+v is not the owner's frozen need", k.PayloadID(), need))
			}
		}
		for _, id := range sortedStrategyRequirementIDs(req) {
			e := req.Strategies[id]
			if e.MacroEvents || e.TradeFlow || e.OpenInterest || e.Premium || e.VolumeProfile || e.HasHTF || e.HasRegime || e.HasLeader || e.FundingScalar || e.FundingRecords {
				errs = append(errs, fmt.Sprintf("strategy %s needs inputs the replay does not freeze", id))
			}
		}
	}
	if len(errs) > 0 {
		sort.Strings(errs)
		return refuseReplay("config_refused", errs...)
	}
	return nil
}

func openQuoteReplay(spec *quoteReplaySpec, opts quoteReplayOptions) (*quoteReplayRuntime, error) {
	r := &quoteReplayRuntime{spec: spec, opts: opts, clock: &simulatedClock{}, logf: func(format string, args ...any) { fmt.Fprintf(os.Stderr, format+"\n", args...) }}
	ok := false
	created := false
	stateDir := spec.Run.StateDir
	defer func() {
		if !ok {
			r.Close()
			if created {
				os.RemoveAll(stateDir)
			}
		}
	}()
	if err := checkQuoteReplayStateDir(spec.Mode, stateDir); err != nil {
		return nil, err
	}
	cfgJSON, err := spec.configDocument(stateDir)
	if err != nil {
		return nil, err
	}
	r.cfgJSON = cfgJSON
	if err := r.clock.Set(timeFromMs(spec.Window.FirstDeadlineMs)); err != nil {
		return nil, err
	}
	if err := bindTradingClock(quoteReplayClockOwner, r.clock.Now); err != nil {
		return nil, err
	}
	r.cleanups = append(r.cleanups, func() { unbindTradingClock(quoteReplayClockOwner) })
	lots := newFrozenHLLotMetadataCache(map[string]int{spec.Coin: spec.Lot.SzDecimals}, func(format string, a ...any) { fmt.Fprintf(os.Stderr, format+"\n", a...) })
	prevLots := hlLotMetadata
	hlLotMetadata = lots
	r.cleanups = append(r.cleanups, func() { hlLotMetadata = prevLots })
	prevRecorder := tradeRecorder
	tradeRecorder = nil
	r.cleanups = append(r.cleanups, func() { tradeRecorder = prevRecorder })
	prevFundingClock := paperFundingClock
	paperFundingClock = func() time.Time { return tradingNow().UTC() }
	r.cleanups = append(r.cleanups, func() { paperFundingClock = prevFundingClock })

	load, err := newQuoteResearchLoadAdmission(quoteReplayOwnerID, stateDirForAdmission(stateDir), cfgJSON)
	if err != nil {
		return nil, refuseReplay("config_refused", err.Error())
	}
	cfg, err := loadQuoteAdmittedConfigData(cfgJSON, load)
	if err != nil {
		return nil, refuseReplay("config_refused", err.Error())
	}
	if err := quoteReplayConfigRefusal(cfg, spec, stateDir); err != nil {
		return nil, err
	}
	r.cfg = cfg
	sc := cfg.Strategies[0]
	policyFP, err := quotePolicyFingerprint(sc)
	if err != nil {
		return nil, err
	}
	fp, err := spec.fingerprints(policyFP)
	if err != nil {
		return nil, err
	}
	r.fp = fp
	r.manifest = quoteReplayManifest{Owner: quoteReplayOwnerMarker, RunID: spec.Run.RunID, Fingerprints: fp, OwnerStrategy: quoteReplayOwnerID,
		FirstDeadlineMs: spec.Window.FirstDeadlineMs, LastDeadlineMs: spec.Window.LastDeadlineMs, UID: os.Getuid()}
	dbPath := filepath.Join(stateDir, quoteReplayDBName)
	if spec.Mode == basketReplayModeResume {
		saved, err := readQuoteReplayManifest(stateDir)
		if err != nil {
			return nil, err
		}
		if err := compareQuoteReplayManifest(saved, r.manifest); err != nil {
			return nil, err
		}
		stored, err := verifyQuoteReplayDBReadOnly(dbPath, r.manifest)
		if err != nil {
			return nil, err
		}
		if stored.Refused != "" {
			return nil, refuseReplay("resume_refused", "the run was refused terminally: "+stored.Refused)
		}
		r.cursorMs = stored.CursorMs
		r.resumed = true
	} else {
		r.manifest.CreatedAt = time.Now().UTC().Format(time.RFC3339)
		if err := createQuoteReplayStateDir(stateDir, r.manifest); err != nil {
			return nil, err
		}
		created = true
	}
	layout, err := resolveStorageLayout(cfg)
	if err != nil {
		return nil, refuseReplay("config_refused", err.Error())
	}
	if len(layout.Files) != 1 {
		return nil, refuseReplay("config_refused", "the replay layout must hold exactly one state file")
	}
	for _, f := range layout.Files {
		if f.InMemory || filepath.Dir(f.Path) != stateDir || f.Path != dbPath {
			return nil, refuseReplay("state_dir_refused", fmt.Sprintf("state file %q is not the run state file", f.Path))
		}
	}
	ident, err := buildStorageIdentityMap(cfg, layout)
	if err != nil {
		return nil, refuseReplay("config_refused", err.Error())
	}
	owned, err := acquireStateOwnership(layout.Files)
	if err != nil {
		return nil, refuseReplay("state_busy", err.Error())
	}
	r.owner = owned
	r.cleanups = append(r.cleanups, func() { owned.Release() })
	if spec.Mode == basketReplayModeResume {
		if err := checkQuoteReplayStateDir(basketReplayModeResume, stateDir); err != nil {
			return nil, err
		}
		stored, err := verifyQuoteReplayDBReadOnly(dbPath, r.manifest)
		if err != nil {
			return nil, err
		}
		if stored.CursorMs != r.cursorMs {
			return nil, refuseReplay("resume_refused", "the saved cursor changed while the runner took ownership")
		}
	}
	store, err := OpenStateStore(layout, ident)
	if err != nil {
		return nil, err
	}
	r.store = store
	r.cleanups = append(r.cleanups, func() { store.Close() })
	part := partitionFor(sc)
	r.file = store.file(layout.roleForPartition(part))
	if r.file == nil {
		return nil, fmt.Errorf("no state file owns the %s partition", partitionLabel(part))
	}
	storageID, _ := ident.storageFor(sc.ID)
	if err := r.file.ensureQuoteReplaySchema(r.manifest, storageID.StorageID, spec.Mode == basketReplayModeNew); err != nil {
		return nil, err
	}
	if spec.Mode == basketReplayModeResume {
		if _, err := readQuoteReplayRun(r.file, r.manifest); err != nil {
			return nil, err
		}
	}
	rule := newQuotePriceRule(hlLotLookup{Known: true, SzDecimals: spec.Lot.SzDecimals, Coin: spec.Coin})
	r.venue = newQuotePaperVenue(spec.Run.RunID, fp.Run, part, spec.Coin, r.file, r.clock, rule, spec.costs, spec.marketEvents(), spec.coverage)
	r.venue.fault = opts.Fault
	if r.resumed {
		if err := r.rebuildVenue(); err != nil {
			return nil, err
		}
	}
	adm, err := bindQuoteAdmission(load, cfg, store, owned, r.venue, r.clock.Now)
	if err != nil {
		return nil, refuseReplay("admission_refused", err.Error())
	}
	r.adm = adm
	r.cleanups = append(r.cleanups, func() { adm.Close() })
	if adm.PolicyFingerprint() != policyFP {
		return nil, refuseReplay("admission_refused", "the bound policy fingerprint differs from the replay fingerprint")
	}
	r.ctx = quotePolicyContext{Params: spec.params, Coin: spec.Coin, CapUSD: spec.Config.MaxInventoryUSD, MaxOrderQty: spec.Quote.MaxOrderQty,
		CycleSeconds: spec.Config.IntervalSeconds, Prices: adm.prices, PolicyFingerprint: adm.PolicyFingerprint()}
	if err := r.resumePoint(); err != nil {
		return nil, err
	}
	ok = true
	return r, nil
}

func stateDirForAdmission(dir string) string {
	if eval, err := filepath.EvalSymlinks(dir); err == nil {
		return eval
	}
	return dir
}

func (r *quoteReplayRuntime) rebuildVenue() error {
	events, err := r.file.quoteReplayVenueEvents(r.spec.Run.RunID)
	if err != nil {
		return err
	}
	var digest string
	var seq int64
	cpMs := r.cursorMs
	if r.cursorMs > 0 {
		blob, s, ok, err := r.file.quoteReplayCheckpoint(r.spec.Run.RunID, r.cursorMs)
		if err != nil {
			return err
		}
		if !ok {
			return refuseReplay("resume_refused", fmt.Sprintf("no checkpoint is saved at the cursor deadline %d", r.cursorMs))
		}
		var cp quotePaperCheckpoint
		if err := json.Unmarshal([]byte(blob), &cp); err != nil {
			return refuseReplay("state_foreign", err.Error())
		}
		digest, seq = sha256Hex([]byte(blob)), s
		cpMs = cp.ProcessedMs
	}
	if err := r.venue.rebuild(events, seq, cpMs, digest); err != nil {
		return err
	}
	if r.venue.lastEff > r.clock.Now().UnixMilli() {
		if err := r.clock.Set(timeFromMs(r.venue.lastEff)); err != nil {
			return err
		}
	}
	return nil
}

func (r *quoteReplayRuntime) resumePoint() error {
	results, err := r.file.quoteReplayResults(r.spec.Run.RunID)
	if err != nil {
		return err
	}
	r.nextIndex = 0
	if r.cursorMs == 0 {
		if r.resumed {
			r.adm.mu.RLock()
			s := r.adm.state.Strategies[quoteReplayOwnerID]
			started := s != nil && s.Quote != nil && s.Quote.Contract != nil
			var flat bool
			if started {
				c := s.Quote.Contract
				flat = mustQuoteRat(c.Book.Qty).Sign() == 0 && c.LastDeadlineMs == 0 && c.LastGeneration == 0 && len(s.Quote.Orders) == 0 && len(s.Quote.Unapplied) == 0
			}
			r.adm.mu.RUnlock()
			if started && !(flat && s.Quote.Contract.StartMs == r.spec.Window.FirstDeadlineMs) {
				return refuseReplay("resume_refused", "the saved quote contract holds progress that no committed deadline record covers")
			}
		}
		return nil
	}
	idx := -1
	for i, d := range r.spec.deadlines {
		if d == r.cursorMs {
			idx = i
		}
	}
	if idx < 0 {
		return refuseReplay("resume_refused", fmt.Sprintf("the saved cursor %d is not a deadline of this window", r.cursorMs))
	}
	r.adm.mu.RLock()
	s := r.adm.state.Strategies[quoteReplayOwnerID]
	last := int64(0)
	if s != nil && s.Quote != nil && s.Quote.Contract != nil {
		last = s.Quote.Contract.LastDeadlineMs
	}
	r.adm.mu.RUnlock()
	if last != r.cursorMs {
		return refuseReplay("resume_refused", fmt.Sprintf("the quote contract is at deadline %d but the replay cursor is %d; the contract moved ahead of its audited progress", last, r.cursorMs))
	}
	var latest, done *quoteReplayResultRow
	for i := range results {
		if results[i].DeadlineMs == r.cursorMs {
			latest = &results[i]
			if results[i].State == quoteReplayResultCompleted || results[i].State == quoteReplayResultRecovery {
				done = &results[i]
			}
		}
	}
	if latest == nil {
		return refuseReplay("resume_refused", fmt.Sprintf("no deadline result exists for the cursor %d", r.cursorMs))
	}
	switch {
	case done != nil:
		r.nextIndex = idx + 1
		r.restoreLastHolds(done)
	case latest.State == quoteReplayResultPending || latest.State == quoteReplayResultInterrupted:
		if _, err := r.file.markQuoteReplayInterrupted(r.spec.Run.RunID, r.cursorMs); err != nil {
			return err
		}
		r.unscored = append(r.unscored, fmt.Sprintf("deadline %d was interrupted after its progress committed; its current-deadline result is lost and the repeat is a recovery result", r.cursorMs))
		r.pending = latest
		r.nextIndex = idx
		for j := idx - 1; j >= 0; j-- {
			for i := range results {
				if results[i].DeadlineMs == r.spec.deadlines[j] && (results[i].State == quoteReplayResultCompleted || results[i].State == quoteReplayResultRecovery) {
					r.restoreLastHolds(&results[i])
					j = -1
					break
				}
			}
		}
	default:
		return refuseReplay("resume_refused", fmt.Sprintf("the cursor %d result is in state %q", r.cursorMs, latest.State))
	}
	for _, res := range results {
		if res.State == quoteReplayResultInterrupted || res.State == quoteReplayResultRecovery {
			r.unscored = append(r.unscored, fmt.Sprintf("deadline %d attempt %d is %s", res.DeadlineMs, res.Attempt, res.State))
		}
	}
	return nil
}

type quoteReplayResultRecord struct {
	DeadlineMs     int64    `json:"deadline_ms"`
	SettlementKind string   `json:"settlement_kind"`
	Revision       int64    `json:"revision"`
	Generation     int64    `json:"generation"`
	HoldReasons    []string `json:"hold_reasons,omitempty"`
	SettlementHold []string `json:"settlement_holds,omitempty"`
	ElapsedNs      int64    `json:"-"`
}

func (r *quoteReplayRuntime) restoreLastHolds(row *quoteReplayResultRow) {
	var res quoteReplayResultRecord
	if len(row.Result) == 0 || json.Unmarshal(row.Result, &res) != nil {
		return
	}
	r.adm.mu.Lock()
	if s := r.adm.state.Strategies[quoteReplayOwnerID]; s != nil && s.Quote != nil {
		s.Quote.LastHolds = append([]string(nil), res.HoldReasons...)
	}
	r.adm.mu.Unlock()
}

func (spec *quoteReplaySpec) lastBookAt(t int64) int {
	i := sort.Search(len(spec.recvIndex), func(i int) bool { return spec.recvIndex[i] > t })
	return i - 1
}

func (spec *quoteReplaySpec) gapIn(fromMs, toMs int64) bool {
	for _, g := range spec.gaps {
		if g[0] <= toMs && fromMs <= g[1] {
			return true
		}
	}
	return false
}

func (spec *quoteReplaySpec) tradeCovered(fromMs, toMs int64) bool {
	for _, c := range spec.coverage {
		if c[0] <= fromMs && toMs <= c[1] {
			return true
		}
	}
	return false
}

func (spec *quoteReplaySpec) eligibleBook(t int64, maxAge int64) (int, string) {
	i := spec.lastBookAt(t)
	if i < 0 {
		return -1, fmt.Sprintf("no book was received at or before %d", t)
	}
	b := spec.books[i]
	if t-b.RecvMs > maxAge || t-b.EventMs > maxAge {
		return -1, fmt.Sprintf("the last book at %d is %dms old by receipt and %dms by event, above the %dms bound", t, t-b.RecvMs, t-b.EventMs, maxAge)
	}
	if spec.gapIn(b.RecvMs, t) {
		return -1, fmt.Sprintf("a book coverage gap lies between the last book at %d and %d", b.RecvMs, t)
	}
	return i, ""
}

type quoteReplaySealObject struct {
	Version     int      `json:"version"`
	DeadlineMs  int64    `json:"deadline_ms"`
	TimeBasis   string   `json:"time_basis"`
	Coin        string   `json:"coin"`
	Need        any      `json:"need"`
	BookPrefix  string   `json:"book_prefix"`
	BookCount   int      `json:"book_count"`
	TradePrefix string   `json:"trade_prefix"`
	TradeCount  int      `json:"trade_count"`
	Gaps        [][2]int `json:"-"`
	GapsBefore  int      `json:"gaps_before"`
	Funding     int      `json:"funding_records_before"`
	Eligible    string   `json:"eligible_book,omitempty"`
}

func (r *quoteReplayRuntime) evidence(d int64) (quotePolicyEvidence, *quoteDeadlineMark, string) {
	spec := r.spec
	p := spec.params
	maxAge := int64(p.MaxInputAgeMs)
	ev := quotePolicyEvidence{DeadlineMs: d, Healthy: true}
	idx, why := spec.eligibleBook(d, maxAge)
	var mark *quoteDeadlineMark
	if idx < 0 {
		ev.Healthy = false
		ev.BookIssue = why
		ev.Reasons = append(ev.Reasons, why)
	} else {
		b := spec.books[idx]
		ev.Book = &quotePolicyBook{EventMs: b.EventMs, RecvMs: b.RecvMs, Identity: b.Identity, BestBid: quoteDecimalText(b.bestBid()), BestAsk: quoteDecimalText(b.bestAsk())}
		if mid, err := quotePolicyMid(ev.Book); err == nil {
			mark = &quoteDeadlineMark{Coin: spec.Coin, Price: quoteRatFloat(mid), EvidenceMs: b.RecvMs, MaxAgeMs: maxAge}
		}
	}
	if !spec.tradeCovered(d-int64(spec.Config.IntervalSeconds)*1000, d) {
		ev.Healthy = false
		ev.Reasons = append(ev.Reasons, fmt.Sprintf("raw-trade coverage does not span the cycle ending at %d", d))
	}
	window := p.VolatilityWindowSeconds
	prevIdx := -2
	for i := 0; i <= window; i++ {
		s := d - int64(window-i)*1000
		smp := quotePolicySample{SecondMs: s}
		bi, why := spec.eligibleBook(s, maxAge)
		switch {
		case bi < 0:
			smp.Missing = why
		case prevIdx != -2 && spec.gapIn(s-1000, s):
			smp.Missing = fmt.Sprintf("book coverage breaks between %d and %d", s-1000, s)
		default:
			b := spec.books[bi]
			mid, err := quotePolicyMid(&quotePolicyBook{BestBid: quoteDecimalText(b.bestBid()), BestAsk: quoteDecimalText(b.bestAsk())})
			if err != nil {
				smp.Missing = err.Error()
			} else {
				smp.Mid = quoteDecimalText(mid)
				smp.RecvMs = b.RecvMs
				smp.Identity = b.Identity
			}
		}
		prevIdx = bi
		ev.Samples = append(ev.Samples, smp)
	}
	bookCount := spec.lastBookAt(d) + 1
	tradeCount := sort.Search(len(spec.tradeIdx), func(i int) bool { return spec.tradeIdx[i] > d })
	obj := quoteReplaySealObject{Version: 1, DeadlineMs: d, TimeBasis: quoteReplayTimeBasis, Coin: spec.Coin,
		Need: contractBookNeed(inventoryMarketMakingBookWindowMs(p), inventoryMarketMakingBookCapacity, inventoryMarketMakingBookDepth), BookCount: bookCount, TradeCount: tradeCount}
	if bookCount > 0 {
		obj.BookPrefix = spec.bookChain[bookCount-1]
	}
	if tradeCount > 0 {
		obj.TradePrefix = spec.tradeHash[tradeCount-1]
	}
	for _, g := range spec.gaps {
		if g[0] <= d {
			obj.GapsBefore++
		}
	}
	for _, f := range spec.funding {
		if f.TimeMs <= d {
			obj.Funding++
		}
	}
	if ev.Book != nil {
		obj.Eligible = ev.Book.Identity
	}
	return ev, mark, quoteReplayCanonical(obj)
}

func (r *quoteReplayRuntime) fundingInputs(d int64) *paperFundingInputs {
	cov := feedFundingCoverage{Coin: r.spec.Coin, FromMs: r.spec.Funding.FromMs}
	for _, f := range r.spec.funding {
		if f.TimeMs <= d {
			cov.Records = append(cov.Records, f)
			cov.ToMs = f.TimeMs
		}
	}
	return &paperFundingInputs{Source: quoteReplayFundingSource, Coverage: map[string]feedFundingCoverage{r.spec.Coin: cov}, Detail: map[string]string{}, NotServed: map[string]bool{}}
}

type quoteReplayDecisionRecord struct {
	DeadlineMs        int64               `json:"deadline_ms"`
	CycleSeconds      int                 `json:"cycle_seconds"`
	Fingerprints      map[string]string   `json:"fingerprints"`
	Generation        int64               `json:"snapshot_generation"`
	Revision          int64               `json:"snapshot_revision"`
	Inventory         string              `json:"inventory"`
	FirstOpenMs       int64               `json:"first_open_ms,omitempty"`
	AppliedBoundaryMs int64               `json:"applied_boundary_ms"`
	Mark              *quoteDeadlineMark  `json:"mark,omitempty"`
	Verdict           quoteInputVerdict   `json:"verdict"`
	Evidence          quotePolicyEvidence `json:"evidence"`
	Outcome           quotePolicyOutcome  `json:"outcome"`
	Stop              *quoteSnapshotStop  `json:"stop,omitempty"`
	FundingHeld       []paperFundingHeld  `json:"funding_held,omitempty"`
	FundingGaps       []paperFundingGap   `json:"funding_gaps,omitempty"`
	PreviousHolds     []string            `json:"previous_holds,omitempty"`
	SettlementHeld    bool                `json:"settlement_held"`
	SettlementHolds   []string            `json:"settlement_holds,omitempty"`
	Sides             []quoteSnapshotSide `json:"sides,omitempty"`
}

type quoteReplayProgress struct {
	r           *quoteReplayRuntime
	deadlineMs  int64
	record      []byte
	checkpoint  []byte
	venueSeq    int64
	attempt     int
	repeat      bool
	recoveryRun bool
}

func (p *quoteReplayProgress) WriteQuoteProgress(tx *sql.Tx, summary quoteCommitSummary) error {
	runID := p.r.spec.Run.RunID
	fault := p.r.opts.Fault
	if fault != nil {
		fault.hit("progress-before-write")
	}
	var existing string
	err := tx.QueryRow(`SELECT record_json FROM quote_replay_decisions WHERE run_id = ? AND deadline_ms = ?`, runID, p.deadlineMs).Scan(&existing)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if p.repeat {
			return fmt.Errorf("the repeated deadline %d has no saved decision", p.deadlineMs)
		}
		if _, err := tx.Exec(`INSERT INTO quote_replay_decisions (run_id, deadline_ms, record_json, digest) VALUES (?, ?, ?, ?)`, runID, p.deadlineMs, string(p.record), sha256Hex(p.record)); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO quote_replay_checkpoints (run_id, deadline_ms, venue_seq, checkpoint_json, digest) VALUES (?, ?, ?, ?, ?)`, runID, p.deadlineMs, p.venueSeq, string(p.checkpoint), sha256Hex(p.checkpoint)); err != nil {
			return err
		}
	case err != nil:
		return err
	default:
		if existing != string(p.record) {
			return fmt.Errorf("deadline %d already holds a different decision record", p.deadlineMs)
		}
	}
	if fault != nil {
		fault.hit("progress-mid-write")
	}
	sum, _ := json.Marshal(summary)
	if _, err := tx.Exec(`INSERT INTO quote_replay_deadline_results (run_id, deadline_ms, attempt, state, summary_json) VALUES (?, ?, ?, 'pending', ?)`, runID, p.deadlineMs, p.attempt, string(sum)); err != nil {
		return err
	}
	res, err := tx.Exec(`UPDATE quote_replay_runs SET cursor_ms = ? WHERE run_id = ? AND cursor_ms <= ?`, p.deadlineMs, runID, p.deadlineMs)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("the replay cursor would move back to %d", p.deadlineMs)
	}
	var venueFills, settled int
	through := summary.AppliedBoundaryMs + 1
	if err := tx.QueryRow(`SELECT COUNT(*) FROM quote_replay_venue_events WHERE run_id = ? AND kind = 'fill' AND effect_ms <= ?`, runID, through).Scan(&venueFills); err != nil {
		return err
	}
	if err := tx.QueryRow(`SELECT COUNT(*) FROM quote_fills WHERE event_time_ms <= ? AND settlement_id != 0`, through).Scan(&settled); err != nil {
		return err
	}
	if venueFills != settled {
		return fmt.Errorf("the cursor at %d would pass %d persisted venue fill(s) through the applied read time %d with %d settled", p.deadlineMs, venueFills, through, settled)
	}
	if fault != nil {
		fault.hit("progress-after-write")
		fault.armCommit()
	}
	return nil
}

func (r *quoteReplayRuntime) decide(d int64, attempt int, saved string, ev quotePolicyEvidence, mark *quoteDeadlineMark, verdict quoteInputVerdict) quoteDecisionFunc {
	return func(in quoteDecisionInput) (quoteDecision, error) {
		if r.opts.Fault.triggered("decide-error") {
			return quoteDecision{}, fmt.Errorf("injected decision callback fault at deadline %d", d)
		}
		snap := in.Snapshot
		if saved != "" {
			var rec quoteReplayDecisionRecord
			if err := json.Unmarshal([]byte(saved), &rec); err != nil {
				return quoteDecision{}, err
			}
			return quoteDecision{Desired: rec.Outcome.Desired, Exit: rec.Outcome.Exit,
				Progress: &quoteReplayProgress{r: r, deadlineMs: d, record: []byte(saved), attempt: attempt, repeat: true}}, nil
		}
		out := decideQuotePolicy(r.ctx, ev, in)
		rec := quoteReplayDecisionRecord{DeadlineMs: d, CycleSeconds: r.spec.Config.IntervalSeconds,
			Fingerprints: map[string]string{"source": verdict.Source, "seal": verdict.SealFingerprint, "config": verdict.ConfigFingerprint,
				"policy": verdict.PolicyFingerprint, "portable_config": r.fp.Config, "run": r.fp.Run, "data": r.fp.Data},
			Generation: snap.Generation, Revision: snap.Revision, Inventory: snap.Inventory, FirstOpenMs: snap.FirstOpenMs,
			AppliedBoundaryMs: snap.AppliedBoundaryMs, Mark: mark, Verdict: verdict, Evidence: ev, Outcome: out, Stop: snap.Stop,
			FundingHeld: snap.FundingHeld, FundingGaps: snap.FundingGaps, PreviousHolds: snap.Holds, SettlementHeld: in.SettlementHeld,
			SettlementHolds: in.SettlementHolds, Sides: snap.Sides}
		blob, err := json.Marshal(rec)
		if err != nil {
			return quoteDecision{}, err
		}
		cp, err := json.Marshal(r.venue.checkpoint())
		if err != nil {
			return quoteDecision{}, err
		}
		dec := out.Decision
		dec.Progress = &quoteReplayProgress{r: r, deadlineMs: d, record: blob, checkpoint: cp, venueSeq: r.venue.seq, attempt: attempt}
		return dec, nil
	}
}

func (r *quoteReplayRuntime) verdictFor(d int64, ev quotePolicyEvidence, sealFP string) quoteInputVerdict {
	v := quoteInputVerdict{OwnerID: quoteReplayOwnerID, Status: quoteVerdictHealthy, Source: "quote-replay:" + shortFingerprint(r.fp.Data),
		SealFingerprint: sealFP, ConfigFingerprint: r.adm.ConfigFingerprint(), PolicyFingerprint: r.adm.PolicyFingerprint(), DeadlineMs: d}
	if !ev.Healthy {
		v.Status = quoteVerdictUnhealthy
		v.Reasons = append([]string(nil), ev.Reasons...)
	}
	return v
}

func (r *quoteReplayRuntime) Run() error {
	ctx := context.Background()
	for i := r.nextIndex; i < len(r.spec.deadlines); i++ {
		if r.opts.MaxDeadlines > 0 && i-r.nextIndex >= r.opts.MaxDeadlines {
			r.stopped = fmt.Sprintf("stopped after %d deadline(s) by --max-deadlines; the run is resumable", r.opts.MaxDeadlines)
			return nil
		}
		if r.venue.brokenAt > 0 {
			r.stopped = fmt.Sprintf("the window ended before deadline %d: raw-trade coverage broke at %d while an own order rested", r.spec.deadlines[i], r.venue.brokenAt)
			return nil
		}
		d := r.spec.deadlines[i]
		repeat := r.pending != nil && r.pending.DeadlineMs == d
		attempt := 1
		saved := ""
		if repeat {
			attempt = r.pending.Attempt + 1
			blob, ok, err := r.file.quoteReplayDecision(r.spec.Run.RunID, d)
			if err != nil {
				return err
			}
			if !ok {
				return refuseReplay("resume_refused", fmt.Sprintf("the interrupted deadline %d has no saved decision", d))
			}
			saved = blob
		}
		now := r.clock.Now().UnixMilli()
		if now > d && !repeat {
			return refuseReplay("timing_refused", fmt.Sprintf("the simulated clock %d is already past deadline %d", now, d))
		}
		if now < d {
			if err := r.clock.Set(timeFromMs(d)); err != nil {
				return err
			}
		}
		r.venue.limitMs = 0
		if i+1 < len(r.spec.deadlines) {
			r.venue.limitMs = r.spec.deadlines[i+1]
		}
		r.venue.mu.Lock()
		err := r.venue.advanceTo(r.clock.Now().UnixMilli())
		r.venue.mu.Unlock()
		if err != nil {
			return err
		}
		ev, mark, sealFP := r.evidence(d)
		var marks []quoteDeadlineMark
		if mark != nil {
			marks = append(marks, *mark)
		}
		verdict := r.verdictFor(d, ev, sealFP)
		started := time.Now()
		res, err := RunDeadline(ctx, r.adm, quoteDeadlineRequest{DeadlineMs: d, Marks: marks, Funding: r.fundingInputs(d),
			Verdict: &verdict, Decide: r.decide(d, attempt, saved, ev, mark, verdict)})
		elapsed := time.Since(started).Nanoseconds()
		if err != nil {
			r.stopped = fmt.Sprintf("deadline %d stopped: %v", d, err)
			return err
		}
		if r.opts.Fault != nil {
			r.opts.Fault.hit("after-call")
		}
		state := quoteReplayResultCompleted
		if repeat {
			state = quoteReplayResultRecovery
			r.pending = nil
		}
		if r.venue.fatal != "" {
			if err := r.file.markQuoteReplayRefused(r.spec.Run.RunID, r.venue.fatal); err != nil {
				return err
			}
			state = quoteReplayResultInterrupted
		}
		rec := quoteReplayResultRecord{DeadlineMs: d, SettlementKind: res.SettlementKind, Revision: res.Revision, Generation: res.Generation,
			HoldReasons: res.HoldReasons, SettlementHold: res.SettlementHold}
		if err := r.file.finishQuoteReplayResult(r.spec.Run.RunID, d, attempt, state, rec); err != nil {
			r.stopped = fmt.Sprintf("the deadline %d result could not be saved: %v", d, err)
			return err
		}
		if r.opts.Fault != nil {
			r.opts.Fault.hit("after-result")
		}
		r.timings = append(r.timings, elapsed)
		r.cursorMs = d
		if r.venue.fatal != "" {
			return refuseReplay("timing_refused", r.venue.fatal)
		}
		if r.venue.brokenAt > 0 {
			r.stopped = fmt.Sprintf("the window ended at deadline %d: raw-trade coverage broke at %d while an own order rested", d, r.venue.brokenAt)
			return nil
		}
	}
	return nil
}

type quoteReplayFaultState struct {
	mu     sync.Mutex
	point  string
	target int
	count  int
	exit   func(int)
}

var quoteReplayFaultPoints = map[string]bool{
	"issue-before-reserve": true, "venue-before-persist": true, "venue-after-persist": true, "progress-before-write": true,
	"progress-mid-write": true, "progress-after-write": true, "commit-hook": true, "after-call": true, "after-result": true,
	"decide-error": true,
}

func parseQuoteReplayFault(raw string) (*quoteReplayFaultState, error) {
	if raw == "" {
		return nil, nil
	}
	name, val, ok := strings.Cut(raw, ":")
	n, err := strconv.Atoi(val)
	if !ok || !quoteReplayFaultPoints[name] || err != nil || n <= 0 {
		names := make([]string, 0, len(quoteReplayFaultPoints))
		for k := range quoteReplayFaultPoints {
			names = append(names, k)
		}
		sort.Strings(names)
		return nil, fmt.Errorf("--fault %q must be <point>:<occurrence> with point in %s", raw, strings.Join(names, ", "))
	}
	return &quoteReplayFaultState{point: name, target: n, exit: os.Exit}, nil
}

func (f *quoteReplayFaultState) hit(point string) {
	if f == nil || point != f.point {
		return
	}
	f.mu.Lock()
	f.count++
	fire := f.count == f.target
	f.mu.Unlock()
	if fire {
		fmt.Fprintf(os.Stderr, "[quote-replay] fault injection: exiting at %s occurrence %d\n", point, f.target)
		f.exit(basketReplayExitFault)
	}
}

func (f *quoteReplayFaultState) triggered(point string) bool {
	if f == nil || point != f.point {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.count++
	return f.count == f.target
}

func (f *quoteReplayFaultState) armCommit() {
	if f == nil || f.point != "commit-hook" {
		return
	}
	prev := storeCommitHook
	storeCommitHook = func(role storageRole) error {
		storeCommitHook = prev
		f.hit("commit-hook")
		if prev != nil {
			return prev(role)
		}
		return nil
	}
}
