package main

import (
	"fmt"
	"math"
	"sort"
	"time"
)

const syntheticTradeReadBound = 500

type candlePage struct {
	Bars        []UICandle
	Older       bool
	Before      int64
	Boundary    string
	Detail      string
	FormingTime int64
	ReadBound   bool
}

func syntheticCandlePage(store *StateStore, sc StrategyConfig, symbol, tf string, from, to, before int64, limit int) (candlePage, error) {
	bucket, ok := candleGridInterval(tf)
	if !ok {
		return candlePage{}, fmt.Errorf("timeframe %s has no fixed bar grid for trade-built bars", tf)
	}
	if store == nil {
		return candlePage{}, fmt.Errorf("database not available")
	}
	if symbol == "" {
		return candlePage{}, fmt.Errorf("strategy %s has no symbol", sc.ID)
	}
	var since, until time.Time
	if from > 0 {
		since = time.Unix(from-1, 0).UTC()
	}
	upper := int64(math.MaxInt64)
	if to > 0 {
		upper = to + bucket - 1
	}
	if before > 0 && before-1 < upper {
		upper = before - 1
	}
	if upper != math.MaxInt64 {
		until = time.Unix(upper+1, 0).UTC()
	}
	trades, _, err := store.QueryTradeHistory(sc.ID, symbol, since, until, syntheticTradeReadBound, 0)
	if err != nil {
		return candlePage{}, err
	}
	truncated := len(trades) >= syntheticTradeReadBound
	byBucket := make(map[int64]*UICandle)
	var keys []int64
	for i := len(trades) - 1; i >= 0; i-- {
		tr := trades[i]
		if tr.Symbol != symbol || tr.Timestamp.IsZero() || !tradeReviewFinite(tr.Price) || tr.Price <= 0 {
			continue
		}
		if role := tradeReviewRole(tr); role != "open" && role != "close" {
			continue
		}
		ts := tr.Timestamp.UTC().Unix()
		if ts > upper || (from > 0 && ts < from) {
			continue
		}
		b := tradeReviewFloor(ts, bucket)
		c, ok := byBucket[b]
		if !ok {
			byBucket[b] = &UICandle{Time: b, Open: tr.Price, High: tr.Price, Low: tr.Price, Close: tr.Price}
			keys = append(keys, b)
			continue
		}
		c.High = math.Max(c.High, tr.Price)
		c.Low = math.Min(c.Low, tr.Price)
		c.Close = tr.Price
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	page := candlePage{}
	if truncated && len(keys) > 0 {
		keys = keys[1:]
		if len(keys) == 0 {
			page.Boundary = candleBoundaryUnsupported
			page.Detail = fmt.Sprintf("the newest %d trades fall in one %s bar, so no complete older bar can be built inside the read bound", syntheticTradeReadBound, tf)
			page.Bars = []UICandle{}
			return page, nil
		}
	}
	if limit > 0 && len(keys) > limit {
		keys = keys[len(keys)-limit:]
		truncated = true
	}
	page.Bars = make([]UICandle, 0, len(keys))
	for _, k := range keys {
		page.Bars = append(page.Bars, *byBucket[k])
	}
	switch {
	case len(page.Bars) == 0:
		page.Boundary = candleBoundaryHistoryStart
		page.Detail = "no older trades exist for this strategy and symbol"
	case truncated:
		page.Older = true
		page.Before = page.Bars[0].Time
		page.ReadBound = true
		page.Detail = fmt.Sprintf("trade-built bars read at most %d trades per page", syntheticTradeReadBound)
	default:
		page.Boundary = candleBoundaryHistoryStart
		page.Detail = "the first trade of this strategy and symbol is in this page"
	}
	return page, nil
}
