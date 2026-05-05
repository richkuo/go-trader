package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"
)

// HLFillLookup carries the aggregated fee + closed PnL across the on-chain
// fills that match a single logical close. A "logical close" can fragment
// into multiple userFills entries (different price levels, partial fills),
// so callers always receive the sum.
type HLFillLookup struct {
	Fee       float64
	ClosedPnL float64
	Count     int
	OID       int64
}

// hlFillRecord is the trimmed userFills payload we care about. The HL indexer
// returns numeric fields as strings; ParseFloat tolerates missing/empty.
type hlFillRecord struct {
	Coin      string      `json:"coin"`
	Sz        string      `json:"sz"`
	OID       json.Number `json:"oid"`
	Fee       string      `json:"fee"`
	ClosedPnl string      `json:"closedPnl"`
	Time      int64       `json:"time"`
	Dir       string      `json:"dir"`
}

// fetchHyperliquidUserFillsByTime is a function variable so tests can stub the
// HTTP layer without spinning up an httptest server (some reconciler tests
// already stub clearinghouseState — composing two stubs in one process is
// awkward when both target hlMainnetURL).
var fetchHyperliquidUserFillsByTime = defaultFetchHyperliquidUserFillsByTime

func defaultFetchHyperliquidUserFillsByTime(accountAddress string, startTimeMs int64) ([]hlFillRecord, error) {
	if accountAddress == "" {
		return nil, fmt.Errorf("HYPERLIQUID_ACCOUNT_ADDRESS not set")
	}
	payload := map[string]any{
		"type":      "userFillsByTime",
		"user":      accountAddress,
		"startTime": startTimeMs,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(hlMainnetURL+"/info", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http %d from %s", resp.StatusCode, hlMainnetURL)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	var fills []hlFillRecord
	if err := json.Unmarshal(data, &fills); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	return fills, nil
}

// hlFillLookupRetries / hlFillLookupRetryDelay control the indexer-lag retry
// budget. HL fills can take a few hundred ms to surface after the on-chain
// trigger; we mirror the Python adapter's defaults (4 attempts × 500ms).
// Variables (not consts) so tests can shorten without sleeps.
var (
	hlFillLookupRetries    = 4
	hlFillLookupRetryDelay = 500 * time.Millisecond
)

// lookupHyperliquidFillByOID queries userFills for fills matching `oid`,
// summing fee + closedPnl across partial fills. Retries with backoff to
// absorb HL indexer lag. Returns ok=false when no fill matches within the
// retry budget — callers fall back to the modeled fee.
func lookupHyperliquidFillByOID(accountAddress string, oid int64, startTimeMs int64) (HLFillLookup, bool) {
	if oid <= 0 || accountAddress == "" {
		return HLFillLookup{}, false
	}
	for attempt := 0; attempt < hlFillLookupRetries; attempt++ {
		fills, err := fetchHyperliquidUserFillsByTime(accountAddress, startTimeMs)
		if err == nil {
			out := HLFillLookup{OID: oid}
			for _, f := range fills {
				if !fillOIDMatches(f, oid) {
					continue
				}
				out.Fee += parseHLFloat(f.Fee)
				out.ClosedPnL += parseHLFloat(f.ClosedPnl)
				out.Count++
			}
			if out.Count > 0 {
				return out, true
			}
		}
		if attempt < hlFillLookupRetries-1 {
			time.Sleep(hlFillLookupRetryDelay)
		}
	}
	return HLFillLookup{}, false
}

// lookupHyperliquidFillByCoinSize is the fallback for closes detected without
// a tracked OID — typically external UI closes for shared-coin peers, where
// only the SL owner has an OID stamped on its Position. Matches by coin and
// absolute size within `tolerance` (coin units).
//
// To avoid conflating multiple unrelated closes within the lookup window
// (#596), fills are sorted newest-first; the first matching record's OID
// anchors the result, and fee/closedPnl are aggregated across only that OID
// (one logical close group, including any partial fills). When the anchor
// has no OID, only that single record is returned.
func lookupHyperliquidFillByCoinSize(accountAddress, coin string, absSize, tolerance float64, startTimeMs int64) (HLFillLookup, bool) {
	if accountAddress == "" || coin == "" || absSize <= 0 {
		return HLFillLookup{}, false
	}
	for attempt := 0; attempt < hlFillLookupRetries; attempt++ {
		fills, err := fetchHyperliquidUserFillsByTime(accountAddress, startTimeMs)
		if err == nil {
			sorted := make([]hlFillRecord, len(fills))
			copy(sorted, fills)
			sort.SliceStable(sorted, func(i, j int) bool {
				return sorted[i].Time > sorted[j].Time
			})
			anchorIdx := -1
			for i, f := range sorted {
				if f.Coin != coin {
					continue
				}
				sz := parseHLFloat(f.Sz)
				if math.Abs(math.Abs(sz)-absSize) > tolerance {
					continue
				}
				anchorIdx = i
				break
			}
			if anchorIdx >= 0 {
				anchor := sorted[anchorIdx]
				anchorOID, oidErr := anchor.OID.Int64()
				if oidErr != nil || anchorOID <= 0 {
					return HLFillLookup{
						Fee:       parseHLFloat(anchor.Fee),
						ClosedPnL: parseHLFloat(anchor.ClosedPnl),
						Count:     1,
					}, true
				}
				out := HLFillLookup{OID: anchorOID}
				for _, f := range fills {
					if !fillOIDMatches(f, anchorOID) {
						continue
					}
					out.Fee += parseHLFloat(f.Fee)
					out.ClosedPnL += parseHLFloat(f.ClosedPnl)
					out.Count++
				}
				if out.Count > 0 {
					return out, true
				}
			}
		}
		if attempt < hlFillLookupRetries-1 {
			time.Sleep(hlFillLookupRetryDelay)
		}
	}
	return HLFillLookup{}, false
}

// hlReconcileFillLookupWindow is how far back the reconciler looks for the
// matching userFills entry. The reconciler runs once per scheduler cycle
// (typically 60s+) but a fill could pre-date the cycle by minutes — for
// example after a process restart with on-chain SL fired during downtime.
// 24h is a generous default that still bounds the indexer scan; tests
// shorten it.
var hlReconcileFillLookupWindow = 24 * time.Hour

// reconcileFillLookupSinceMs returns the userFills lookup window start in ms.
// Tests stub the window via hlReconcileFillLookupWindow.
func reconcileFillLookupSinceMs(now time.Time) int64 {
	return now.Add(-hlReconcileFillLookupWindow).UnixMilli()
}

// hlReconcileFillSizeTolerance bounds the fuzzy size match in the coin+size
// fallback. HL rounds to per-asset sz_decimals; 1e-4 covers the smallest
// denominations (BTC sz_decimals=4) without admitting cross-strategy drift.
const hlReconcileFillSizeTolerance = 1e-4

// fillOIDMatches handles HL's mixed-type OID encoding: indexer responses
// have arrived as both numeric and string-numeric depending on indexer
// version. json.Number normalises both.
func fillOIDMatches(f hlFillRecord, oid int64) bool {
	if f.OID == "" {
		return false
	}
	if v, err := f.OID.Int64(); err == nil {
		return v == oid
	}
	// Fallback to string compare for floats / scientific notation.
	return f.OID.String() == strconv.FormatInt(oid, 10)
}

func parseHLFloat(s string) float64 {
	if s == "" {
		return 0
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return v
}

// lookupHyperliquidReconcileFillFee is the reconciler-facing fee resolver.
// Tries OID lookup first (precise match against Position.StopLossOID); when
// that returns no match — or no OID is known — falls back to the coin+size
// search over the same indexer window. Stubbed to a no-op in tests that
// don't exercise fee plumbing.
//
// Returns ok=false on any failure so callers fall back to the modeled fee
// path. The function never returns an error: HL indexer hiccups should not
// block the reconciler from clearing virtual state. Failures emit a single
// stderr line so operators can correlate drift complaints with lookup misses.
var lookupHyperliquidReconcileFillFee = defaultLookupHyperliquidReconcileFillFee

// logHyperliquidReconcileFillLookup logs one INFO line per reconciler close
// summarising whether the userFills lookup hit, missed, or was skipped.
// Operators correlating drift / Trade.ExchangeFee=0 rows back to a specific
// close event use these markers; emitted at INFO so they stay out of the
// default-error stream during clean operation.
func logHyperliquidReconcileFillLookup(logger *StrategyLogger, coin string, oid int64, expectedQty float64, lookup HLFillLookup, useFillFee bool) {
	if logger == nil {
		return
	}
	if useFillFee && lookup.Count > 0 {
		logger.Info("hl-sync: %s userFills hit oid=%d qty=%.6f → fee=$%.4f closedPnl=$%.2f (%d fills)", coin, oid, expectedQty, lookup.Fee, lookup.ClosedPnL, lookup.Count)
		return
	}
	if oid > 0 || expectedQty > 0 {
		logger.Info("hl-sync: %s userFills miss oid=%d qty=%.6f — falling back to modeled fee", coin, oid, expectedQty)
	}
}

func defaultLookupHyperliquidReconcileFillFee(accountAddress, coin string, oid int64, expectedQty float64) (HLFillLookup, bool) {
	if accountAddress == "" {
		return HLFillLookup{}, false
	}
	since := reconcileFillLookupSinceMs(time.Now().UTC())
	if oid > 0 {
		if lookup, ok := lookupHyperliquidFillByOID(accountAddress, oid, since); ok {
			return lookup, true
		}
	}
	if coin != "" && expectedQty > 0 {
		if lookup, ok := lookupHyperliquidFillByCoinSize(accountAddress, coin, expectedQty, hlReconcileFillSizeTolerance, since); ok {
			return lookup, true
		}
	}
	return HLFillLookup{}, false
}

// hlReconcileFillResolver returns the userFills lookup result for a (coin,
// oid, expectedQty) tuple. The reconciler resolves fees via this indirection
// so the locked apply phase can read pre-fetched results without making any
// HTTP calls under mu.Lock(). See buildCachedHyperliquidReconcileFillResolver.
type hlReconcileFillResolver func(coin string, oid int64, expectedQty float64) (HLFillLookup, bool)

// noFillFeeResolver is the resolver used when fee lookups are disabled (no
// account address) or when no candidate close events were detected. Always
// returns ok=false so callers fall back to the modeled fee path.
var noFillFeeResolver hlReconcileFillResolver = func(string, int64, float64) (HLFillLookup, bool) {
	return HLFillLookup{}, false
}

// directHyperliquidReconcileFillResolver wraps lookupHyperliquidReconcileFillFee
// for paths that can safely block on HTTP I/O — primarily tests that stub the
// underlying function. Production reconcile paths must use
// buildCachedHyperliquidReconcileFillResolver to keep network calls outside
// mu.Lock().
func directHyperliquidReconcileFillResolver(accountAddress string) hlReconcileFillResolver {
	if accountAddress == "" {
		return noFillFeeResolver
	}
	return func(coin string, oid int64, expectedQty float64) (HLFillLookup, bool) {
		return lookupHyperliquidReconcileFillFee(accountAddress, coin, oid, expectedQty)
	}
}

// hyperliquidReconcileFeeCacheKey identifies one userFills lookup. Quantity is
// rounded to 1e-8 so float identity comparisons across the snapshot/apply
// boundary survive — HL sz_decimals tops out at 8 so the round is lossless
// for legitimate fills.
type hyperliquidReconcileFeeCacheKey struct {
	coin string
	oid  int64
	qty  int64 // qty * 1e8, rounded
}

func makeHLReconcileFeeCacheKey(coin string, oid int64, qty float64) hyperliquidReconcileFeeCacheKey {
	return hyperliquidReconcileFeeCacheKey{
		coin: coin,
		oid:  oid,
		qty:  int64(math.Round(qty * 1e8)),
	}
}

// buildCachedHyperliquidReconcileFillResolver runs a brief RLock pass to
// identify which (coin, oid, qty) lookups the reconciler is likely to need,
// performs all userFills queries OUTSIDE the lock (each can take up to ~1.5s
// under indexer-lag retry), then returns a pure map-reading resolver that the
// locked apply phase calls. Cache misses fall back to noFillFeeResolver
// behavior — never to a network call — so the lock-held region is bounded.
//
// The candidate-detection pass is permissive: false positives just mean an
// extra HTTP query per cycle, false negatives mean a close books with the
// modeled fee. Detector logic is duplicated approximately, not exactly, so
// the apply phase remains the source of truth for whether a close fires.
func buildCachedHyperliquidReconcileFillResolver(accountAddress string, allStrategies []StrategyConfig, state *AppState, mu *sync.RWMutex, positions []HLPosition) hlReconcileFillResolver {
	if accountAddress == "" {
		return noFillFeeResolver
	}

	type candidate struct {
		coin string
		oid  int64
		qty  float64
	}

	onChainByCoin := make(map[string]float64, len(positions))
	for _, p := range positions {
		onChainByCoin[p.Coin] = p.Size
	}

	seen := make(map[hyperliquidReconcileFeeCacheKey]bool)
	var candidates []candidate
	addCandidate := func(coin string, oid int64, qty float64) {
		if coin == "" || qty <= 0 {
			return
		}
		key := makeHLReconcileFeeCacheKey(coin, oid, qty)
		if seen[key] {
			return
		}
		seen[key] = true
		candidates = append(candidates, candidate{coin: coin, oid: oid, qty: qty})
	}

	mu.RLock()
	for _, sc := range allStrategies {
		ss := state.Strategies[sc.ID]
		if ss == nil {
			continue
		}
		sym := hyperliquidSymbol(sc.Args)
		if sym == "" {
			continue
		}
		pos := ss.Positions[sym]
		if pos == nil || pos.Quantity <= 0 {
			continue
		}
		onChainSize, present := onChainByCoin[sym]
		// Trigger lookup when on-chain is absent OR signed-qty differs from
		// virtual qty. Sign mismatch is intentional: it covers both Detector 1
		// (full external close, on-chain ≈ 0) and Detector 2 (partial close
		// where on-chain residual ≠ virtual).
		mismatched := !present || math.Abs(math.Abs(onChainSize)-pos.Quantity) > 1e-9
		if !mismatched {
			continue
		}
		if pos.StopLossOID > 0 && pos.StopLossTriggerPx > 0 {
			addCandidate(sym, pos.StopLossOID, pos.Quantity)
		}
		// Always include the (coin, 0, qty) form so peers without a tracked
		// OID — Detector 1 mark-based path and Detector 2 non-owner — hit a
		// cached entry. The resolver drops to coin+size internally.
		addCandidate(sym, 0, pos.Quantity)
	}
	mu.RUnlock()

	if len(candidates) == 0 {
		return noFillFeeResolver
	}

	type cacheEntry struct {
		lookup HLFillLookup
		ok     bool
	}
	cache := make(map[hyperliquidReconcileFeeCacheKey]cacheEntry, len(candidates))
	for _, c := range candidates {
		lookup, ok := lookupHyperliquidReconcileFillFee(accountAddress, c.coin, c.oid, c.qty)
		cache[makeHLReconcileFeeCacheKey(c.coin, c.oid, c.qty)] = cacheEntry{lookup: lookup, ok: ok}
	}

	return func(coin string, oid int64, expectedQty float64) (HLFillLookup, bool) {
		entry, hit := cache[makeHLReconcileFeeCacheKey(coin, oid, expectedQty)]
		if !hit {
			return HLFillLookup{}, false
		}
		return entry.lookup, entry.ok
	}
}
