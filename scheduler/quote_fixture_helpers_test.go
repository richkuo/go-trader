package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const quoteTestOwner = "hl-quote-btc"

func newQuoteFixtureLoadAdmission(ownerID, stateDir string, configBytes []byte) (*quoteLoadAdmission, error) {
	return newQuoteLoadAdmission(quoteAdmissionFixture, ownerID, stateDir, configBytes)
}

type quoteTestOptions struct {
	partition  string
	paperDB    bool
	extra      []map[string]any
	quote      map[string]any
	strategy   map[string]any
	root       map[string]any
	paperSrc   bool
	stopFields map[string]any
}

func quoteTestConfigBytes(t *testing.T, dir string, o quoteTestOptions) []byte {
	t.Helper()
	mode := "--mode=paper"
	if o.partition == "live" {
		mode = "--mode=live"
	}
	q := map[string]any{"coin": "BTC", "max_inventory_usd": 5000, "max_order_qty": 0.5, "request_expiry_ms": 5000, "fee_reserve_rate": 0.0005, "margin_price_buffer_pct": 1}
	for k, v := range o.quote {
		if v == nil {
			delete(q, k)
			continue
		}
		q[k] = v
	}
	sc := map[string]any{
		"id": quoteTestOwner, "type": "perps", "platform": "hyperliquid", "script": "shared_scripts/check_hyperliquid.py",
		"args": []string{"quote_fixture", "BTC", "1m", mode}, "open_strategy": map[string]any{"name": "quote_fixture"},
		"close_strategy": map[string]any{"name": inventoryQuoteExitCloseName}, "capital": 1000, "leverage": 5, "margin_mode": "isolated",
		"stop_loss_pct": 5, "quote": q,
	}
	if o.stopFields != nil {
		delete(sc, "stop_loss_pct")
		for k, v := range o.stopFields {
			sc[k] = v
		}
	}
	for k, v := range o.strategy {
		if v == nil {
			delete(sc, k)
			continue
		}
		sc[k] = v
	}
	if o.paperSrc {
		sc["paper_source"] = "lab"
	}
	strategies := []map[string]any{sc}
	strategies = append(strategies, o.extra...)
	root := map[string]any{
		"config_version":   CurrentConfigVersion,
		"interval_seconds": 60,
		"log_dir":          filepath.Join(dir, "logs"),
		"db_file":          filepath.Join(dir, "state.db"),
		"strategies":       strategies,
	}
	if o.paperDB {
		root["paper_db_file"] = filepath.Join(dir, "paper.db")
	}
	if o.paperSrc {
		root["paper_sources"] = []map[string]any{{"id": "lab", "label": "Lab", "db_file": filepath.Join(dir, "lab.db")}}
	}
	for k, v := range o.root {
		root[k] = v
	}
	blob, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	return blob
}

type quoteFixtureOrder struct {
	req        quoteOrderRequest
	oid        string
	status     string
	filled     *big.Rat
	acceptedAt int64
}

type quoteFixtureVenue struct {
	mu        sync.Mutex
	clock     func() time.Time
	live      bool
	provider  string
	account   string
	namespace string
	rule      quotePriceRule
	orders    map[string]*quoteFixtureOrder
	fills     []quoteRawFill
	position  *big.Rat
	liqPx     float64
	free      string
	nextOID   int
	nextTID   int
	sends     []quoteOrderRequest
	cancels   []string

	placeHook     func(req quoteOrderRequest) (quoteOrderOutcome, bool)
	cancelHook    func(req quoteOrderRequest) (quoteOrderOutcome, bool)
	statusHook    func(req quoteOrderRequest) (quoteOrderOutcome, bool)
	recoverHook   func(sinceMs, untilMs int64) (quoteFillRecovery, error, bool)
	positionHook  func() (quotePositionRead, error, bool)
	afterPosition func()
}

func newQuoteFixtureVenue(clock func() time.Time, live bool, part RiskPartition) *quoteFixtureVenue {
	v := &quoteFixtureVenue{clock: clock, live: live, provider: "fixture", account: "fixture:" + part.String(), orders: map[string]*quoteFixtureOrder{},
		position: new(big.Rat), nextOID: 1000, nextTID: 5000, rule: newQuotePriceRule(hlLotLookup{Known: true, SzDecimals: 3, Coin: "BTC"})}
	if live {
		v.provider = "hyperliquid"
		v.account = "hyperliquid:testnet:0xquotefixture"
	}
	return v
}

func (v *quoteFixtureVenue) Name() string                      { return "quote-fixture" }
func (v *quoteFixtureVenue) LiveShaped() bool                  { return v.live }
func (v *quoteFixtureVenue) Provider() string                  { return v.provider }
func (v *quoteFixtureVenue) AccountKey(p RiskPartition) string { return v.account }
func (v *quoteFixtureVenue) Namespace() string                 { return v.namespace }
func (v *quoteFixtureVenue) NewClientOrderID() (string, error) {
	return newQuoteClientOrderID()
}

func (v *quoteFixtureVenue) nowMs() int64 { return v.clock().UnixMilli() }

func (v *quoteFixtureVenue) outcomeOf(o *quoteFixtureOrder) quoteOrderOutcome {
	if o == nil {
		return quoteOrderOutcome{Status: quoteOutcomeAbsent, VenueTimeMs: v.nowMs()}
	}
	out := quoteOrderOutcome{Status: o.status, VenueOrderID: o.oid, VenueTimeMs: v.nowMs(), AcceptedAtMs: o.acceptedAt, FilledQty: quoteDecimalText(o.filled)}
	return out
}

func (v *quoteFixtureVenue) Place(ctx context.Context, req quoteOrderRequest) (quoteOrderOutcome, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.sends = append(v.sends, req)
	if v.placeHook != nil {
		if out, ok := v.placeHook(req); ok {
			if out.Status == quoteOutcomeResting || out.Status == quoteOutcomeFilled {
				v.acceptLocked(req, out.Status)
			}
			return out, nil
		}
	}
	if v.nowMs() >= req.ExpiryMs {
		return quoteOrderOutcome{Status: quoteOutcomeRejected, VenueTimeMs: v.nowMs(), Detail: "expired request"}, nil
	}
	if err := v.rule.check(req); err != nil {
		return quoteOrderOutcome{Status: quoteOutcomeRejected, VenueTimeMs: v.nowMs(), Detail: err.Error()}, nil
	}
	o := v.acceptLocked(req, quoteOutcomeResting)
	if req.Role == quoteRoleClose {
		v.fillLocked(o, req.Size, req.Px, "0", v.nowMs())
	}
	return v.outcomeOf(o), nil
}

func (v *quoteFixtureVenue) acceptLocked(req quoteOrderRequest, status string) *quoteFixtureOrder {
	v.nextOID++
	o := &quoteFixtureOrder{req: req, oid: fmt.Sprintf("%d", v.nextOID), status: status, filled: new(big.Rat), acceptedAt: v.nowMs()}
	v.orders[strings.ToLower(req.ClientOrderID)] = o
	return o
}

func (v *quoteFixtureVenue) Cancel(ctx context.Context, req quoteOrderRequest, venueOrderID string) (quoteOrderOutcome, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.cancels = append(v.cancels, req.ClientOrderID)
	if v.cancelHook != nil {
		if out, ok := v.cancelHook(req); ok {
			return out, nil
		}
	}
	o := v.orders[strings.ToLower(req.ClientOrderID)]
	if o != nil && (o.status == quoteOutcomeResting || o.status == quoteOutcomeTriggered) {
		o.status = quoteOutcomeCancelled
	}
	return v.outcomeOf(o), nil
}

func (v *quoteFixtureVenue) Status(ctx context.Context, req quoteOrderRequest, venueOrderID string) (quoteOrderOutcome, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.statusHook != nil {
		if out, ok := v.statusHook(req); ok {
			return out, nil
		}
	}
	return v.outcomeOf(v.orders[strings.ToLower(req.ClientOrderID)]), nil
}

func (v *quoteFixtureVenue) RecoverFills(ctx context.Context, coin string, sinceMs, untilMs int64) (quoteFillRecovery, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.recoverHook != nil {
		if rec, err, ok := v.recoverHook(sinceMs, untilMs); ok {
			return rec, err
		}
	}
	through := untilMs
	if through <= 0 || through > v.nowMs() {
		through = v.nowMs()
	}
	rec := quoteFillRecovery{Coverage: quoteFillCoverage{Complete: true, FromMs: sinceMs, ThroughMs: through, Pages: 1}}
	for _, f := range v.fills {
		if f.Coin == coin && f.TimeMs >= sinceMs && f.TimeMs <= through {
			rec.Fills = append(rec.Fills, f)
		}
	}
	return rec, nil
}

func (v *quoteFixtureVenue) Position(ctx context.Context, coin string) (quotePositionRead, error) {
	v.mu.Lock()
	if v.positionHook != nil {
		if p, err, ok := v.positionHook(); ok {
			v.mu.Unlock()
			return p, err
		}
	}
	p := quotePositionRead{Account: v.account, Coin: coin, Size: quoteDecimalText(v.position), LiquidationPx: v.liqPx, FreeMargin: v.free, ReadAtMs: v.nowMs()}
	hook := v.afterPosition
	v.mu.Unlock()
	if hook != nil {
		hook()
	}
	return p, nil
}

func (v *quoteFixtureVenue) fillLocked(o *quoteFixtureOrder, size, px, fee string, tMs int64) quoteRawFill {
	sz := mustQuoteRat(size)
	start := quoteDecimalText(v.position)
	if o.req.Side == "buy" {
		v.position = quoteRatAdd(v.position, sz)
	} else {
		v.position = quoteRatSub(v.position, sz)
	}
	o.filled = quoteRatAdd(o.filled, sz)
	if req := mustQuoteRat(o.req.Size); o.filled.Cmp(req) >= 0 {
		o.status = quoteOutcomeFilled
	}
	v.nextTID++
	f := quoteRawFill{Account: v.account, Namespace: v.namespace, Coin: o.req.Coin, Side: o.req.Side, Px: px, Size: size, Fee: fee, FeeAsset: "USDC",
		StartPosition: start, TimeMs: tMs, TradeID: fmt.Sprintf("%d", v.nextTID), OrderID: o.oid, ClientOrderID: strings.ToLower(o.req.ClientOrderID)}
	f.Raw = fmt.Sprintf(`{"oid":%q,"tid":%q,"px":%q,"sz":%q,"fee":%q,"time":%d}`, f.OrderID, f.TradeID, px, size, fee, tMs)
	v.fills = append(v.fills, f)
	return f
}

func (v *quoteFixtureVenue) fill(t *testing.T, cloid, size, px, fee string, tMs int64) quoteRawFill {
	t.Helper()
	v.mu.Lock()
	defer v.mu.Unlock()
	o := v.orders[strings.ToLower(cloid)]
	if o == nil {
		t.Fatalf("no venue order for client %s", cloid)
	}
	return v.fillLocked(o, size, px, fee, tMs)
}

func (v *quoteFixtureVenue) sendCount() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.sends)
}

type quoteTestClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *quoteTestClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *quoteTestClock) Set(t time.Time) {
	c.mu.Lock()
	c.now = t.UTC()
	c.mu.Unlock()
}

func (c *quoteTestClock) Advance(d time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	return c.now
}

type quoteTestEnv struct {
	t        *testing.T
	dir      string
	opts     quoteTestOptions
	cfgBytes []byte
	cfg      *Config
	store    *StateStore
	owned    *stateOwnership
	venue    *quoteFixtureVenue
	clock    *quoteTestClock
	adm      *quoteAdmission
	notifier *MultiNotifier
	part     RiskPartition
	progress []quoteCommitSummary
}

var quoteTestStart = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

func newQuoteTestEnv(t *testing.T, o quoteTestOptions) *quoteTestEnv {
	t.Helper()
	useHLLotMetadataForTest(t, map[string]int{"BTC": 3, "ETH": 4})
	dir := t.TempDir()
	if eval, err := filepath.EvalSymlinks(dir); err == nil {
		dir = eval
	}
	clock := &quoteTestClock{now: quoteTestStart}
	if err := bindTradingClock("quote-test", clock.Now); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unbindTradingClock("quote-test") })
	prevFundingClock := paperFundingClock
	paperFundingClock = clock.Now
	t.Cleanup(func() { paperFundingClock = prevFundingClock })
	e := &quoteTestEnv{t: t, dir: dir, opts: o, clock: clock}
	e.cfgBytes = quoteTestConfigBytes(t, dir, o)
	e.open()
	part := e.adm.partition
	e.part = part
	return e
}

func (e *quoteTestEnv) open() {
	e.t.Helper()
	load, err := newQuoteFixtureLoadAdmission(quoteTestOwner, e.dir, e.cfgBytes)
	if err != nil {
		e.t.Fatal(err)
	}
	cfg, err := loadQuoteAdmittedConfigData(e.cfgBytes, load)
	if err != nil {
		e.t.Fatalf("admitted load: %v", err)
	}
	layout, err := resolveStorageLayout(cfg)
	if err != nil {
		e.t.Fatal(err)
	}
	ident, err := buildStorageIdentityMap(cfg, layout)
	if err != nil {
		e.t.Fatal(err)
	}
	owned, err := acquireStateOwnership(layout.Files)
	if err != nil {
		e.t.Fatal(err)
	}
	store, err := OpenStateStore(layout, ident)
	if err != nil {
		owned.Release()
		e.t.Fatal(err)
	}
	part := partitionFor(cfg.Strategies[0])
	if e.venue == nil {
		e.venue = newQuoteFixtureVenue(e.clock.Now, part.Scope == ScopeLive, part)
	}
	adm, err := bindQuoteAdmission(load, cfg, store, owned, e.venue, e.clock.Now)
	if err != nil {
		store.Close()
		owned.Release()
		e.t.Fatalf("bind: %v", err)
	}
	if e.notifier != nil {
		adm.SetNotifier(e.notifier)
	}
	e.cfg, e.store, e.owned, e.adm = cfg, store, owned, adm
	e.t.Cleanup(e.close)
}

func (e *quoteTestEnv) close() {
	if e.adm != nil {
		e.adm.Close()
		e.adm = nil
	}
	if e.store != nil {
		e.store.Close()
		e.store = nil
	}
	if e.owned != nil {
		e.owned.Release()
		e.owned = nil
	}
}

func (e *quoteTestEnv) reopen() {
	e.t.Helper()
	e.close()
	e.open()
}

func (e *quoteTestEnv) deadlineMs(n int) int64 {
	return quoteTestStart.Add(time.Duration(n) * time.Minute).UnixMilli()
}

func (e *quoteTestEnv) verdict(deadlineMs int64) *quoteInputVerdict {
	return &quoteInputVerdict{OwnerID: quoteTestOwner, Status: quoteVerdictHealthy, Source: "fixture", SealFingerprint: "seal", ConfigFingerprint: e.adm.ConfigFingerprint(), PolicyFingerprint: e.adm.PolicyFingerprint(), DeadlineMs: deadlineMs}
}

func (e *quoteTestEnv) marks(deadlineMs int64, px float64) []quoteDeadlineMark {
	return []quoteDeadlineMark{{Coin: "BTC", Price: px, EvidenceMs: deadlineMs, MaxAgeMs: 5000}}
}

func (e *quoteTestEnv) pair(bidPx, askPx, size string) quoteDesiredPair {
	p := quoteDesiredPair{PolicyFingerprint: e.adm.PolicyFingerprint()}
	if bidPx != "" {
		p.Bid = &quoteSideRequest{Px: bidPx, Size: size}
	}
	if askPx != "" {
		p.Ask = &quoteSideRequest{Px: askPx, Size: size}
	}
	return p
}

type quoteTestProgress struct {
	env  *quoteTestEnv
	fail error
}

func (p *quoteTestProgress) WriteQuoteProgress(tx *sql.Tx, s quoteCommitSummary) error {
	if p.fail != nil {
		return p.fail
	}
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS quote_test_progress (deadline_ms INTEGER PRIMARY KEY, revision INTEGER, boundary_ms INTEGER, kind TEXT)`); err != nil {
		return err
	}
	_, err := tx.Exec(`INSERT OR REPLACE INTO quote_test_progress (deadline_ms, revision, boundary_ms, kind) VALUES (?, ?, ?, ?)`, s.DeadlineMs, s.Revision, s.AppliedBoundaryMs, s.SettlementKind)
	if err == nil {
		p.env.progress = append(p.env.progress, s)
	}
	return err
}

func (e *quoteTestEnv) run(n int, mark float64, desired quoteDesiredPair, exit quoteExitRequest) (quoteDeadlineResult, error) {
	e.t.Helper()
	d := e.deadlineMs(n)
	if e.clock.Now().UnixMilli() < d {
		e.clock.Set(time.UnixMilli(d))
	}
	return RunDeadline(context.Background(), e.adm, quoteDeadlineRequest{
		DeadlineMs: d,
		Marks:      e.marks(d, mark),
		Verdict:    e.verdict(d),
		Decide: func(in quoteDecisionInput) (quoteDecision, error) {
			return quoteDecision{Desired: desired, Exit: exit, Progress: &quoteTestProgress{env: e}}, nil
		},
	})
}

func (e *quoteTestEnv) mustRun(n int, mark float64, desired quoteDesiredPair, exit quoteExitRequest) quoteDeadlineResult {
	e.t.Helper()
	res, err := e.run(n, mark, desired, exit)
	if err != nil {
		e.t.Fatalf("deadline %d: %v", n, err)
	}
	return res
}

func (e *quoteTestEnv) db() *StateDB {
	return e.adm.db
}

func (e *quoteTestEnv) count(query string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.db().db.QueryRow(query, args...).Scan(&n); err != nil {
		e.t.Fatalf("%s: %v", query, err)
	}
	return n
}

func (e *quoteTestEnv) contract() *quoteContractRow {
	e.t.Helper()
	k, err := e.adm.ownerKey()
	if err != nil {
		e.t.Fatal(err)
	}
	c, err := readQuoteContractTx(e.db().db, k)
	if err != nil {
		e.t.Fatal(err)
	}
	return c
}

func (e *quoteTestEnv) owner() *StrategyState {
	e.adm.mu.RLock()
	defer e.adm.mu.RUnlock()
	return e.adm.state.Strategies[quoteTestOwner]
}

func (e *quoteTestEnv) sides(gen int64) map[string]*quoteOrderRow {
	e.t.Helper()
	k, _ := e.adm.ownerKey()
	d, err := e.db().loadQuoteDurableOwner(k)
	if err != nil {
		e.t.Fatal(err)
	}
	out := map[string]*quoteOrderRow{}
	for _, o := range d.Orders {
		if o.Generation == gen && (o.Role == quoteRoleBid || o.Role == quoteRoleAsk) {
			out[o.Role] = o
		}
	}
	return out
}

func (e *quoteTestEnv) stops() []*quoteOrderRow {
	e.t.Helper()
	k, _ := e.adm.ownerKey()
	d, err := e.db().loadQuoteDurableOwner(k)
	if err != nil {
		e.t.Fatal(err)
	}
	var out []*quoteOrderRow
	for _, o := range d.Orders {
		if o.Role == quoteRoleStop {
			out = append(out, o)
		}
	}
	return out
}

func ratEq(t *testing.T, label, got, want string) {
	t.Helper()
	g, err := parseQuoteRat(got)
	if err != nil {
		t.Fatalf("%s: %v", label, err)
	}
	w := mustQuoteRat(want)
	if g.Cmp(w) != 0 {
		t.Fatalf("%s = %s, want %s", label, g.RatString(), w.RatString())
	}
}

var errQuoteInjected = errors.New("injected failure")
