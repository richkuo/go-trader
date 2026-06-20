package main

import (
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	hlFillQtyAbsTolerance = 1e-9
	hlFillQtyRelTolerance = 1e-6
)

type hlFillOIDMatch struct {
	OID     string
	OIDInt  int64
	Summary HLFillSummary
}

func normalizeHLFillCoin(coin string) string {
	return strings.ToUpper(strings.TrimSpace(coin))
}

func hlFillSummaryEventTime(summary HLFillSummary) time.Time {
	ms := summary.LastTimeMS
	if ms <= 0 {
		ms = summary.FirstTimeMS
	}
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}

func hlFillQtyCloseEnough(a, b float64) bool {
	if a <= 0 || b <= 0 {
		return false
	}
	diff := math.Abs(a - b)
	if diff <= hlFillQtyAbsTolerance {
		return true
	}
	scale := math.Max(math.Abs(a), math.Abs(b))
	return diff <= scale*hlFillQtyRelTolerance
}

func hlFillQtyCovers(filledQty, wantedQty float64) bool {
	if filledQty <= 0 || wantedQty <= 0 {
		return false
	}
	if filledQty >= wantedQty {
		return true
	}
	return hlFillQtyCloseEnough(filledQty, wantedQty)
}

func parseHLFillOID(oid string) int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(oid), 10, 64)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

func findUniqueHLFillByCoinQty(fillMap map[string]HLFillSummary, coin string, qty float64, exactQty bool, at time.Time, window time.Duration) (hlFillOIDMatch, bool, bool) {
	matches := findHLFillCandidatesByCoinQty(fillMap, coin, qty, exactQty, at, window, nil)
	if len(matches) != 1 {
		return hlFillOIDMatch{}, false, len(matches) > 1
	}
	return matches[0], true, false
}

func findHLFillCandidatesByCoinQty(fillMap map[string]HLFillSummary, coin string, qty float64, exactQty bool, at time.Time, window time.Duration, excludedOIDs map[string]bool) []hlFillOIDMatch {
	targetCoin := normalizeHLFillCoin(coin)
	if targetCoin == "" || qty <= 0 || len(fillMap) == 0 {
		return nil
	}
	keys := make([]string, 0, len(fillMap))
	for oid := range fillMap {
		keys = append(keys, oid)
	}
	sort.Strings(keys)

	var matches []hlFillOIDMatch
	for _, oid := range keys {
		if excludedOIDs != nil && excludedOIDs[oid] {
			continue
		}
		summary := fillMap[oid]
		if normalizeHLFillCoin(summary.Coin) != targetCoin {
			continue
		}
		if summary.Px <= 0 || summary.Qty <= 0 {
			continue
		}
		if oidInt := parseHLFillOID(oid); oidInt <= 0 {
			continue
		}
		if exactQty {
			if !hlFillQtyCloseEnough(summary.Qty, qty) {
				continue
			}
		} else if !hlFillQtyCovers(summary.Qty, qty) {
			continue
		}
		if !at.IsZero() && window > 0 {
			fillTime := hlFillSummaryEventTime(summary)
			if fillTime.IsZero() {
				continue
			}
			if fillTime.Before(at.Add(-window)) || fillTime.After(at.Add(window)) {
				continue
			}
		}
		matches = append(matches, hlFillOIDMatch{
			OID:     oid,
			OIDInt:  parseHLFillOID(oid),
			Summary: summary,
		})
	}
	return matches
}
