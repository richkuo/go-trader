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

type HLFillLookup struct {
	Fee            float64
	ClosedPnLGross float64
	FilledQty      float64
	Px             float64
	Count          int
	OID            int64
}

func splitHyperliquidFillLookupByQty(lookup HLFillLookup, qty, totalQty float64) (HLFillLookup, bool) {
	if qty <= 0 || totalQty <= 0 {
		return HLFillLookup{}, false
	}
	share := qty / totalQty
	if share <= 0 {
		return HLFillLookup{}, false
	}
	if share > 1 {
		share = 1
	}
	out := lookup
	out.Fee = lookup.Fee * share
	out.ClosedPnLGross = lookup.ClosedPnLGross * share
	if lookup.FilledQty > 0 {
		out.FilledQty = lookup.FilledQty * share
	}
	return out, true
}

type hlFillRecord struct {
	Coin      string      `json:"coin"`
	Sz        string      `json:"sz"`
	Px        string      `json:"px"`
	OID       json.Number `json:"oid"`
	Fee       string      `json:"fee"`
	ClosedPnl string      `json:"closedPnl"`
	Time      int64       `json:"time"`
	Dir       string      `json:"dir"`
	Tid       json.Number `json:"tid"`
	Hash      string      `json:"hash"`
}

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

var (
	hlFillLookupRetries    = 4
	hlFillLookupRetryDelay = 500 * time.Millisecond
)

func lookupHyperliquidFillByOID(accountAddress string, oid int64, startTimeMs int64) (HLFillLookup, bool) {
	if oid <= 0 || accountAddress == "" {
		return HLFillLookup{}, false
	}
	for attempt := 0; attempt < hlFillLookupRetries; attempt++ {
		fills, err := fetchHyperliquidUserFillsByTime(accountAddress, startTimeMs)
		if err == nil {
			out := HLFillLookup{OID: oid}
			pxNumerator := 0.0
			for _, f := range fills {
				if !fillOIDMatches(f, oid) {
					continue
				}
				sz := parseHLFloat(f.Sz)
				out.Fee += parseHLFloat(f.Fee)
				out.ClosedPnLGross += parseHLFloat(f.ClosedPnl)
				out.FilledQty += sz
				pxNumerator += sz * parseHLFloat(f.Px)
				out.Count++
			}
			if out.Count > 0 {
				if out.FilledQty > 0 {
					out.Px = pxNumerator / out.FilledQty
				}
				return out, true
			}
		}
		if attempt < hlFillLookupRetries-1 {
			time.Sleep(hlFillLookupRetryDelay)
		}
	}
	return HLFillLookup{}, false
}

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
						Fee:            parseHLFloat(anchor.Fee),
						ClosedPnLGross: parseHLFloat(anchor.ClosedPnl),
						FilledQty:      parseHLFloat(anchor.Sz),
						Px:             parseHLFloat(anchor.Px),
						Count:          1,
					}, true
				}
				out := HLFillLookup{OID: anchorOID}
				pxNumerator := 0.0
				for _, f := range fills {
					if !fillOIDMatches(f, anchorOID) {
						continue
					}
					sz := parseHLFloat(f.Sz)
					out.Fee += parseHLFloat(f.Fee)
					out.ClosedPnLGross += parseHLFloat(f.ClosedPnl)
					out.FilledQty += sz
					pxNumerator += sz * parseHLFloat(f.Px)
					out.Count++
				}
				if out.Count > 0 {
					if out.FilledQty > 0 {
						out.Px = pxNumerator / out.FilledQty
					}
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

var hlReconcileFillLookupWindow = 24 * time.Hour

func reconcileFillLookupSinceMs(now time.Time) int64 {
	return now.Add(-hlReconcileFillLookupWindow).UnixMilli()
}

const hlReconcileFillSizeTolerance = 1e-4

func fillOIDMatches(f hlFillRecord, oid int64) bool {
	if f.OID == "" {
		return false
	}
	if v, err := f.OID.Int64(); err == nil {
		return v == oid
	}
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

var lookupHyperliquidReconcileFillFee = defaultLookupHyperliquidReconcileFillFee

func logHyperliquidReconcileFillLookup(logger *StrategyLogger, coin string, oid int64, expectedQty float64, lookup HLFillLookup, useFillFee bool) {
	if logger == nil {
		return
	}
	if useFillFee && lookup.Count > 0 {
		logger.Info("hl-sync: %s userFills hit oid=%d qty=%.6f → fee=$%.4f closedPnl_gross=$%.2f (%d fills)", coin, oid, expectedQty, lookup.Fee, lookup.ClosedPnLGross, lookup.Count)
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

type hlReconcileFillResolver func(coin string, oid int64, expectedQty float64) (HLFillLookup, bool)

type HyperliquidProtectionFillHint struct {
	OID       int64   `json:"oid"`
	Filled    bool    `json:"filled"`
	Fee       float64 `json:"fee,omitempty"`
	ClosedPnl float64 `json:"closed_pnl,omitempty"`
	Count     int     `json:"count,omitempty"`
}

var noFillFeeResolver hlReconcileFillResolver = func(string, int64, float64) (HLFillLookup, bool) {
	return HLFillLookup{}, false
}

type hyperliquidReconcileFeeCacheKey struct {
	coin string
	oid  int64
	qty  int64
}

func makeHLReconcileFeeCacheKey(coin string, oid int64, qty float64) hyperliquidReconcileFeeCacheKey {
	return hyperliquidReconcileFeeCacheKey{
		coin: coin,
		oid:  oid,
		qty:  int64(math.Round(qty * 1e8)),
	}
}

func buildCachedHyperliquidReconcileFillResolver(accountAddress string, allStrategies []StrategyConfig, state *AppState, mu *sync.RWMutex, positions []HLPosition) (hlReconcileFillResolver, []HyperliquidProtectionFillHint) {
	if accountAddress == "" {
		return noFillFeeResolver, nil
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
		mismatched := !present || math.Abs(math.Abs(onChainSize)-pos.Quantity) > 1e-9
		if !mismatched {
			continue
		}
		if pos.StopLossOID > 0 && pos.StopLossTriggerPx > 0 {
			addCandidate(sym, pos.StopLossOID, pos.Quantity)
		}
		for _, tpOID := range pos.TPOIDs {
			if tpOID > 0 {
				addCandidate(sym, tpOID, pos.Quantity)
			}
		}
		addCandidate(sym, 0, pos.Quantity)
		if present && onChainSize != 0 {
			signedVirtual := pos.Quantity
			if pos.Side == "short" {
				signedVirtual = -pos.Quantity
			}
			sameDirection := (signedVirtual > 0 && onChainSize > 0) || (signedVirtual < 0 && onChainSize < 0)
			onChainAbs := math.Abs(onChainSize)
			if sameDirection && onChainAbs+1e-9 < pos.Quantity {
				addCandidate(sym, 0, pos.Quantity-onChainAbs)
			}
			if sameDirection && hyperliquidAllTiersArmedAndCleared(sc, pos) {
				tiers := strategyTPTiersForRegime(sc, positionATRRegimeLabel(pos, sc))
				initQty := pos.InitialQuantity
				if initQty <= 0 {
					initQty = pos.Quantity
				}
				lookupOIDs := tpOIDsForReconcileLookup(ss, pos, sym, len(tiers))
				for i := range tiers {
					tierQty := hyperliquidTPTierIncrementalCloseQty(initQty, tiers, i)
					if tierQty <= 0 {
						continue
					}
					if i < len(lookupOIDs) && lookupOIDs[i] > 0 {
						addCandidate(sym, lookupOIDs[i], tierQty)
					}
					addCandidate(sym, 0, tierQty)
				}
			}
		}
		if hCoin := hedgeCoin(sc); hCoin != "" {
			if hPos := ss.Positions[hCoin]; hPos != nil && hPos.isHedgeLeg() && hPos.Quantity > 0 {
				onChainSize, present := onChainByCoin[hCoin]
				if !present || math.Abs(math.Abs(onChainSize)-hPos.Quantity) > 1e-9 {
					addCandidate(hCoin, 0, hPos.Quantity)
					if present && onChainSize != 0 {
						onChainAbs := math.Abs(onChainSize)
						if onChainAbs+1e-9 < hPos.Quantity {
							addCandidate(hCoin, 0, hPos.Quantity-onChainAbs)
						}
					}
				}
			}
		}
	}
	coinStratCount := make(map[string]int)
	coinVirtualQty := make(map[string]float64)
	for _, sc := range allStrategies {
		ss := state.Strategies[sc.ID]
		if ss == nil {
			continue
		}
		sym := hyperliquidSymbol(sc.Args)
		if sym == "" {
			continue
		}
		coinStratCount[sym]++
		pos := ss.Positions[sym]
		if pos == nil || pos.Quantity <= 0 {
			continue
		}
		switch pos.Side {
		case "long":
			coinVirtualQty[sym] += pos.Quantity
		case "short":
			coinVirtualQty[sym] -= pos.Quantity
		}
	}
	for coin, count := range coinStratCount {
		if count < 2 {
			continue
		}
		onChainQty := onChainByCoin[coin]
		if math.Abs(onChainQty) < 1e-6 && math.Abs(coinVirtualQty[coin]) > 1e-6 {
			addCandidate(coin, 0, math.Abs(coinVirtualQty[coin]))
		}
		if _, closeQty, ok := hyperliquidSharedPartialCloseDrift(coinVirtualQty[coin], onChainQty); ok {
			addCandidate(coin, 0, closeQty)
		}
	}
	mu.RUnlock()

	if len(candidates) == 0 {
		return noFillFeeResolver, nil
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

	hintsByOID := make(map[int64]HyperliquidProtectionFillHint, len(candidates))
	for _, c := range candidates {
		if c.oid <= 0 {
			continue
		}
		key := makeHLReconcileFeeCacheKey(c.coin, c.oid, c.qty)
		ent := cache[key]
		if _, exists := hintsByOID[c.oid]; exists {
			continue
		}
		filled := ent.ok && ent.lookup.Count > 0
		hintsByOID[c.oid] = HyperliquidProtectionFillHint{
			OID:       c.oid,
			Filled:    filled,
			Fee:       ent.lookup.Fee,
			ClosedPnl: ent.lookup.ClosedPnLGross,
			Count:     ent.lookup.Count,
		}
	}
	var hintOIDs []int64
	for oid := range hintsByOID {
		hintOIDs = append(hintOIDs, oid)
	}
	sort.Slice(hintOIDs, func(i, j int) bool { return hintOIDs[i] < hintOIDs[j] })
	fillHints := make([]HyperliquidProtectionFillHint, 0, len(hintOIDs))
	for _, oid := range hintOIDs {
		fillHints = append(fillHints, hintsByOID[oid])
	}

	resolver := func(coin string, oid int64, expectedQty float64) (HLFillLookup, bool) {
		entry, hit := cache[makeHLReconcileFeeCacheKey(coin, oid, expectedQty)]
		if !hit {
			return HLFillLookup{}, false
		}
		return entry.lookup, entry.ok
	}
	return resolver, fillHints
}
