package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
)

const quoteReplayTradePage = 500

var quoteReplayRefusedEnvPrefixes = []string{"HYPERLIQUID_", "OKX_", "ROBINHOOD_", "TOPSTEP_", "DERIBIT_", "IBKR_", "BINANCE_", "LUNO_"}

var quoteReplayScrubbedEnvPrefixes = []string{"DISCORD_", "TELEGRAM_", "SLACK_", "ANTHROPIC_", "OPENAI_", "GH_", "GITHUB_TOKEN"}

func quoteReplayEnvName(kv string) string {
	if i := strings.IndexByte(kv, '='); i >= 0 {
		return kv[:i]
	}
	return kv
}

func quoteReplayCredentialRefusal() error {
	var held []string
	for _, kv := range os.Environ() {
		name := quoteReplayEnvName(kv)
		upper := strings.ToUpper(name)
		for _, p := range quoteReplayRefusedEnvPrefixes {
			if strings.HasPrefix(upper, p) {
				held = append(held, name)
			}
		}
	}
	if len(held) == 0 {
		return nil
	}
	sort.Strings(held)
	return refuseReplay("credentials_refused", fmt.Sprintf("the quote replay runs with no venue credentials or venue settings, but the environment holds %s; unset them (values are never printed)", strings.Join(held, ", ")))
}

func scrubQuoteReplayEnvironment() []string {
	var removed []string
	for _, kv := range os.Environ() {
		name := quoteReplayEnvName(kv)
		upper := strings.ToUpper(name)
		drop := false
		for _, p := range quoteReplayScrubbedEnvPrefixes {
			if strings.HasPrefix(upper, p) {
				drop = true
			}
		}
		for _, w := range basketReplayScrubbedEnvWords {
			if strings.Contains(upper, w) {
				drop = true
			}
		}
		if drop {
			os.Unsetenv(name)
			removed = append(removed, name)
		}
	}
	sort.Strings(removed)
	return removed
}

type quoteReplayDiscovery struct {
	Protocol       string          `json:"protocol"`
	Version        int             `json:"version"`
	Strategy       string          `json:"strategy"`
	Capability     map[string]any  `json:"capability"`
	SpecSHA256     string          `json:"spec_sha256"`
	Spec           json.RawMessage `json:"spec"`
	ModelNotes     map[string]any  `json:"model_notes"`
	QueueModel     string          `json:"queue_model"`
	FaultPoints    []string        `json:"fault_points"`
	ProductionNote string          `json:"production"`
}

func quoteReplayDiscover() (quoteReplayDiscovery, error) {
	canon, err := quotePolicySpecCanonical()
	if err != nil {
		return quoteReplayDiscovery{}, err
	}
	notes := map[string]any{}
	for k, v := range quoteReplayModelNotes {
		notes[k] = v
	}
	cap := map[string]any{}
	for k, v := range quotePolicySpec.Capability {
		cap[k] = v
	}
	cap["seam_version"] = quoteCapabilityStrategies[inventoryMarketMakingStrategyName].Version
	cap["production_refused"] = productionRefusedStrategies[inventoryMarketMakingStrategyName]
	var points []string
	for p := range quoteReplayFaultPoints {
		points = append(points, p)
	}
	sort.Strings(points)
	return quoteReplayDiscovery{Protocol: quoteReplayProtocolName, Version: quoteReplayProtocolVersion, Strategy: inventoryMarketMakingStrategyName,
		Capability: cap, SpecSHA256: quotePolicySpecSHA256(), Spec: json.RawMessage(canon), ModelNotes: notes, QueueModel: quoteQueueModelStrict,
		FaultPoints: points, ProductionNote: "production initialization, scalar, batch, fallback and live dispatch refuse this candidate in every mode; only this offline runner admits it"}, nil
}

func runQuoteReplay(args []string) int {
	protocolOut := os.Stdout
	os.Stdout = os.Stderr
	return runQuoteReplayProcess(args, os.Stdin, protocolOut)
}

func runQuoteReplayProcess(args []string, stdin io.Reader, out io.Writer) int {
	if len(args) == 0 || args[0] != "discover" {
		if err := quoteReplayCredentialRefusal(); err != nil {
			o, code := quoteReplayErrorOutput(err)
			if encErr := json.NewEncoder(out).Encode(o); encErr != nil {
				return basketReplayExitError
			}
			return code
		}
		scrubQuoteReplayEnvironment()
	}
	return runQuoteReplayWith(args, stdin, out)
}

func runQuoteReplayWith(args []string, stdin io.Reader, out io.Writer) int {
	emit := func(v any, code int) int {
		enc := json.NewEncoder(out)
		if err := enc.Encode(v); err != nil {
			fmt.Fprintf(os.Stderr, "[quote-replay] could not write the result: %v\n", err)
			return basketReplayExitError
		}
		return code
	}
	if len(args) > 0 && args[0] == "discover" {
		if len(args) != 1 {
			fmt.Fprintln(os.Stderr, "usage: go-trader quote-replay discover")
			return basketReplayExitUsage
		}
		d, err := quoteReplayDiscover()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return basketReplayExitError
		}
		return emit(d, basketReplayExitOK)
	}
	fs := flag.NewFlagSet("quote-replay", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	specPath := fs.String("spec", "", "path to the frozen go-trader.quote-replay plan (JSON), or - for standard input")
	maxDeadlines := fs.Int("max-deadlines", 0, "run at most this many further deadlines, then stop with the run resumable (0 = all)")
	faultRaw := fs.String("fault", "", "controlled fault for restart tests: <point>:<occurrence>")
	if err := fs.Parse(args); err != nil {
		return basketReplayExitUsage
	}
	if fs.NArg() > 0 || *specPath == "" || *maxDeadlines < 0 {
		fmt.Fprintln(os.Stderr, "usage: go-trader quote-replay --spec <plan.json|-> [--max-deadlines N] [--fault <point>:<n>] | go-trader quote-replay discover")
		return basketReplayExitUsage
	}
	fault, err := parseQuoteReplayFault(*faultRaw)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return basketReplayExitUsage
	}
	var src io.Reader = stdin
	if *specPath != "-" {
		f, err := os.Open(*specPath)
		if err != nil {
			o, code := quoteReplayErrorOutput(refuseReplay("spec_unreadable", err.Error()))
			return emit(o, code)
		}
		defer f.Close()
		src = f
	}
	spec, _, err := decodeQuoteReplaySpec(src)
	if err != nil {
		o, code := quoteReplayErrorOutput(err)
		return emit(o, code)
	}
	if err := validateQuoteReplaySpec(spec); err != nil {
		o, code := quoteReplayErrorOutput(err)
		return emit(o, code)
	}
	rt, err := openQuoteReplay(spec, quoteReplayOptions{MaxDeadlines: *maxDeadlines, Fault: fault})
	if err != nil {
		o, code := quoteReplayErrorOutput(err)
		return emit(o, code)
	}
	defer rt.Close()
	runErr := rt.Run()
	result, oerr := rt.output(runErr)
	if oerr != nil {
		o, code := quoteReplayErrorOutput(oerr)
		return emit(o, code)
	}
	code := basketReplayExitOK
	var refusal *basketReplayRefusal
	switch {
	case errors.As(runErr, &refusal):
		code = basketReplayExitRefused
	case runErr != nil:
		code = basketReplayExitError
	}
	return emit(result, code)
}

type quoteReplayFillRecord struct {
	ID           int64  `json:"id"`
	OrderID      string `json:"order_id"`
	TradeID      string `json:"trade_id"`
	TimeMs       int64  `json:"time_ms"`
	Role         string `json:"role"`
	ClientID     string `json:"client_order_id"`
	Side         string `json:"side"`
	Px           string `json:"px"`
	Size         string `json:"size"`
	Fee          string `json:"fee"`
	Start        string `json:"start_position"`
	SettlementID int64  `json:"settlement_id"`
}

type quoteReplaySettlementRecord struct {
	Revision          int64           `json:"revision"`
	Kind              string          `json:"kind"`
	DeadlineMs        int64           `json:"deadline_ms"`
	AppliedBoundaryMs int64           `json:"applied_boundary_ms"`
	FillIDs           json.RawMessage `json:"fill_ids"`
	Funding           string          `json:"funding,omitempty"`
}

type quoteReplayTradeRecord struct {
	Timestamp       string  `json:"timestamp"`
	TimeMs          int64   `json:"time_ms"`
	PositionID      string  `json:"position_id,omitempty"`
	Side            string  `json:"side"`
	Quantity        float64 `json:"quantity"`
	Price           float64 `json:"price"`
	TradeType       string  `json:"trade_type"`
	ExchangeOrderID string  `json:"exchange_order_id,omitempty"`
	ExchangeFee     float64 `json:"exchange_fee"`
	IsClose         bool    `json:"is_close"`
	RealizedPnL     float64 `json:"realized_pnl"`
	Details         string  `json:"details,omitempty"`
}

type quoteReplayTiming struct {
	Deadlines int     `json:"deadlines"`
	MeanNs    int64   `json:"mean_ns"`
	P50Ns     int64   `json:"p50_ns"`
	P90Ns     int64   `json:"p90_ns"`
	P99Ns     int64   `json:"p99_ns"`
	MaxNs     int64   `json:"max_ns"`
	Note      string  `json:"note"`
	Share     float64 `json:"max_share_of_cycle"`
}

type quoteReplayOutput struct {
	Protocol        string                        `json:"protocol"`
	Version         int                           `json:"version"`
	Status          string                        `json:"status"`
	Refusal         *basketReplayRefusal          `json:"refusal,omitempty"`
	Error           string                        `json:"error,omitempty"`
	RunID           string                        `json:"run_id,omitempty"`
	Mode            string                        `json:"mode,omitempty"`
	StateDir        string                        `json:"state_dir,omitempty"`
	Fingerprints    *quoteReplayFingerprints      `json:"fingerprints,omitempty"`
	PolicySpecHash  string                        `json:"policy_spec_sha256,omitempty"`
	Window          *quoteReplayWindow            `json:"window,omitempty"`
	DeadlinesTotal  int                           `json:"deadlines_total"`
	CursorMs        int64                         `json:"cursor_ms"`
	Resumed         bool                          `json:"resumed"`
	Stopped         string                        `json:"stopped,omitempty"`
	Scored          bool                          `json:"scored"`
	Unscored        []string                      `json:"unscored_reasons,omitempty"`
	ModelNotes      map[string]string             `json:"model_notes,omitempty"`
	Decisions       []json.RawMessage             `json:"decisions,omitempty"`
	Results         []quoteReplayResultRow        `json:"results,omitempty"`
	VenueEvents     []quoteReplayVenueEventRow    `json:"venue_events,omitempty"`
	Fills           []quoteReplayFillRecord       `json:"fills,omitempty"`
	Settlements     []quoteReplaySettlementRecord `json:"settlements,omitempty"`
	Trades          []quoteReplayTradeRecord      `json:"trades,omitempty"`
	Book            *quoteExactBook               `json:"book,omitempty"`
	AppliedBoundary int64                         `json:"applied_boundary_ms"`
	PendingOrders   []string                      `json:"pending_orders,omitempty"`
	Timing          *quoteReplayTiming            `json:"timing,omitempty"`
	LotFetches      int                           `json:"lot_metadata_fetch_refusals"`
}

func quoteReplayErrorOutput(err error) (quoteReplayOutput, int) {
	out := quoteReplayOutput{Protocol: quoteReplayProtocolName, Version: quoteReplayProtocolVersion}
	var refusal *basketReplayRefusal
	if errors.As(err, &refusal) {
		out.Status = basketReplayStatusRefused
		out.Refusal = refusal
		return out, basketReplayExitRefused
	}
	out.Status = basketReplayStatusError
	out.Error = err.Error()
	return out, basketReplayExitError
}

func quoteReplayPercentile(sorted []int64, q float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(q*float64(len(sorted)-1) + 0.5)
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return sorted[i]
}

func (r *quoteReplayRuntime) output(runErr error) (quoteReplayOutput, error) {
	out := quoteReplayOutput{Protocol: quoteReplayProtocolName, Version: quoteReplayProtocolVersion, RunID: r.spec.Run.RunID, Mode: r.spec.Mode,
		StateDir: r.spec.Run.StateDir, Fingerprints: &r.fp, PolicySpecHash: quotePolicySpecSHA256(), Window: &r.spec.Window,
		DeadlinesTotal: len(r.spec.deadlines), CursorMs: r.cursorMs, Resumed: r.resumed, Stopped: r.stopped, ModelNotes: quoteReplayModelNotes}
	var refusal *basketReplayRefusal
	switch {
	case errors.As(runErr, &refusal):
		out.Status = basketReplayStatusRefused
		out.Refusal = refusal
	case runErr != nil:
		out.Status = basketReplayStatusError
		out.Error = runErr.Error()
	case r.cursorMs == r.spec.Window.LastDeadlineMs && r.stopped == "":
		out.Status = basketReplayStatusComplete
	case r.stopped != "":
		out.Status = basketReplayStatusStopped
	default:
		out.Status = basketReplayStatusIncomplete
	}
	runID := r.spec.Run.RunID
	var err error
	if out.Decisions, err = r.file.quoteReplayDecisions(runID); err != nil {
		return out, err
	}
	if out.Results, err = r.file.quoteReplayResults(runID); err != nil {
		return out, err
	}
	if out.VenueEvents, err = r.file.quoteReplayVenueEvents(runID); err != nil {
		return out, err
	}
	k, err := r.adm.ownerKey()
	if err != nil {
		return out, err
	}
	fills, err := r.file.loadQuoteFillRows(r.file.db, k, false)
	if err != nil {
		return out, err
	}
	for _, f := range fills {
		out.Fills = append(out.Fills, quoteReplayFillRecord{ID: f.ID, OrderID: f.Fill.OrderID, TradeID: f.Fill.TradeID, TimeMs: f.Fill.TimeMs, Role: f.Role,
			ClientID: f.Fill.ClientOrderID, Side: f.Fill.Side, Px: f.Fill.Px, Size: f.Fill.Size, Fee: f.Fill.Fee, Start: f.Fill.StartPosition, SettlementID: f.SettlementID})
	}
	rows, err := r.file.db.Query(`SELECT revision, kind, deadline_ms, applied_boundary_ms, fill_ids_json, funding_json FROM quote_settlements WHERE scope = ? AND source = ? AND strategy_id = ? ORDER BY revision`,
		string(k.Partition.Scope), k.Partition.Source, k.sid)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var s quoteReplaySettlementRecord
		var ids string
		if err := rows.Scan(&s.Revision, &s.Kind, &s.DeadlineMs, &s.AppliedBoundaryMs, &ids, &s.Funding); err != nil {
			rows.Close()
			return out, err
		}
		s.FillIDs = json.RawMessage(ids)
		out.Settlements = append(out.Settlements, s)
	}
	rows.Close()
	if c, err := readQuoteContractTx(r.file.db, k); err == nil && c != nil {
		b := c.Book
		out.Book = &b
		out.AppliedBoundary = c.AppliedBoundaryMs
	}
	var trades []Trade
	for {
		page, total, err := r.store.QueryTradeHistory(quoteReplayOwnerID, "", time.Time{}, time.Time{}, quoteReplayTradePage, len(trades))
		if err != nil {
			return out, err
		}
		trades = append(trades, page...)
		if len(trades) >= total || len(page) == 0 {
			break
		}
	}
	sort.SliceStable(trades, func(i, j int) bool { return trades[i].Timestamp.Before(trades[j].Timestamp) })
	for _, t := range trades {
		out.Trades = append(out.Trades, quoteReplayTradeRecord{Timestamp: t.Timestamp.UTC().Format(time.RFC3339Nano), TimeMs: t.Timestamp.UnixMilli(), PositionID: t.PositionID,
			Side: t.Side, Quantity: t.Quantity, Price: t.Price, TradeType: t.TradeType, ExchangeOrderID: t.ExchangeOrderID, ExchangeFee: t.ExchangeFee,
			IsClose: t.IsClose, RealizedPnL: t.RealizedPnL, Details: t.Details})
	}
	d, err := r.file.loadQuoteDurableOwner(k)
	if err != nil {
		return out, err
	}
	for _, o := range d.Orders {
		if !o.resolved() {
			out.PendingOrders = append(out.PendingOrders, fmt.Sprintf("%s order %d (client %s) is %s/%s with applied %s of confirmed %s", o.Role, o.RowID, o.ClientOrderID, o.SubmitState, o.CancelState, o.AppliedQty, o.ConfirmedQty))
		}
	}
	if len(r.timings) > 0 {
		sorted := append([]int64(nil), r.timings...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
		var sum int64
		for _, v := range sorted {
			sum += v
		}
		t := &quoteReplayTiming{Deadlines: len(sorted), MeanNs: sum / int64(len(sorted)), P50Ns: quoteReplayPercentile(sorted, 0.5), P90Ns: quoteReplayPercentile(sorted, 0.9),
			P99Ns: quoteReplayPercentile(sorted, 0.99), MaxNs: sorted[len(sorted)-1],
			Note: "wall-clock time of each RunDeadline call in this controlled run (paper venue in process, temporary SQLite file); it is not production delivery or order latency, and it never enters a fingerprint or a decision"}
		cycleNs := float64(r.spec.Config.IntervalSeconds) * 1e9
		if cycleNs > 0 {
			t.Share = float64(t.MaxNs) / cycleNs
		}
		out.Timing = t
	}
	if r.venue.brokenAt > 0 {
		r.unscored = append(r.unscored, fmt.Sprintf("raw-trade coverage broke at %d while an own order rested", r.venue.brokenAt))
	}
	var gaps []int64
	for _, d := range r.spec.deadlines {
		if d <= r.cursorMs && !r.spec.tradeCovered(d-int64(r.spec.Config.IntervalSeconds)*1000, d) {
			gaps = append(gaps, d)
		}
	}
	if len(gaps) > 0 {
		r.unscored = append(r.unscored, fmt.Sprintf("%d deadline(s) lack raw-trade coverage over their cycle (first %d); book coverage alone gives no queue evidence", len(gaps), gaps[0]))
	}
	if !r.spec.Scoring.Scored {
		r.unscored = append(r.unscored, "the plan marks this run as not scored")
	}
	for _, l := range r.spec.Scoring.Limits {
		r.unscored = append(r.unscored, "plan limit: "+l)
	}
	if out.Status != basketReplayStatusComplete {
		r.unscored = append(r.unscored, "the run is "+out.Status)
	}
	sort.Strings(r.unscored)
	out.Unscored = dedupeStrings(r.unscored)
	out.Scored = len(out.Unscored) == 0
	return out, nil
}
