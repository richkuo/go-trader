package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"time"
)

const (
	hlFundingFetchTimeout   = 20 * time.Second
	hlFundingAvgWindowDays  = 7
	hlFundingMaxRangePasses = 64
	hlFundingPublishGraceMs = int64(120_000)
)

func hlPostInfo(ctx context.Context, payload any) ([]byte, error) {
	return hlPostInfoTo(ctx, hlMainnetURL, payload, 0)
}

func hlPostInfoTo(ctx context.Context, baseURL string, payload any, maxBytes int64) ([]byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal info request: %w", err)
	}
	reqType := "info"
	switch m := payload.(type) {
	case map[string]any:
		if t, ok := m["type"].(string); ok {
			reqType = t
		}
	case map[string]string:
		if t, ok := m["type"]; ok {
			reqType = t
		}
	}
	var data []byte
	err = hlInfoPost(ctx, hlInfoCall{
		BaseURL: baseURL, Type: reqType, Body: body, Timeout: hlFundingFetchTimeout, MaxBytes: maxBytes,
		Decode: func(b []byte) (int, error) {
			data = b
			return 0, nil
		},
	})
	if err != nil {
		return nil, err
	}
	return data, nil
}

type hlInfoCall struct {
	BaseURL  string
	Type     string
	Body     []byte
	Timeout  time.Duration
	MaxBytes int64
	RowBound int
	Decode   func([]byte) (int, error)
}

var hlInfoBeforeSendHook func(ctx context.Context, reqType string)

func hlInfoPost(ctx context.Context, call hlInfoCall) error {
	bound := -1
	if hlInfoRowCap(call.Type) > 0 {
		bound = call.RowBound
		if bound <= 0 {
			bound = -1
		}
	}
	turn := feedTurnFrom(ctx)
	if err := turn.enter(ctx); err != nil {
		turn.leave()
		return err
	}
	permit, err := feedBudgetAcquireBounded(ctx, call.BaseURL, call.Type, bound)
	turn.leave()
	if err != nil {
		return err
	}
	defer permit.closeOpen()
	reqCtx, cancel := context.WithTimeout(ctx, call.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, call.BaseURL+"/info", bytes.NewReader(call.Body))
	if err != nil {
		return fmt.Errorf("build %s request: %w", call.Type, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if hook := hlInfoBeforeSendHook; hook != nil {
		hook(ctx, call.Type)
	}
	if err := permit.beginSend(); err != nil {
		return err
	}
	client := egressHTTPClient(call.Timeout)
	resp, err := client.Do(req)
	if err != nil {
		permit.finish(0, "")
		return fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()
	permit.finish(resp.StatusCode, resp.Header.Get("Retry-After"))
	if resp.StatusCode == http.StatusTooManyRequests {
		return fmt.Errorf("http %d from %s/info %s: %w", resp.StatusCode, call.BaseURL, call.Type, errHLInfoRateLimited)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("http %d from %s/info %s", resp.StatusCode, call.BaseURL, call.Type)
	}
	var body []byte
	if call.MaxBytes <= 0 {
		body, err = io.ReadAll(resp.Body)
	} else {
		body, err = io.ReadAll(io.LimitReader(resp.Body, call.MaxBytes+1))
		if err == nil && int64(len(body)) > call.MaxBytes {
			err = fmt.Errorf("%s/info %s response exceeds %d bytes", call.BaseURL, call.Type, call.MaxBytes)
		}
	}
	if err != nil {
		permit.retain()
		return fmt.Errorf("read %s response: %w", call.Type, err)
	}
	rows, err := call.Decode(body)
	if err != nil {
		permit.retain()
		return err
	}
	if bound > 0 && rows > permit.rowBound(bound) {
		permit.violation(bound, rows)
		return fmt.Errorf("%s/info %s returned %d rows, over its %d-row bound; the reservation is kept and the excess is charged as debt", call.BaseURL, call.Type, rows, bound)
	}
	permit.settle(rows)
	return nil
}

func hlParseNumeric(raw json.RawMessage) (float64, error) {
	var asNumber json.Number
	if err := json.Unmarshal(raw, &asNumber); err == nil {
		return asNumber.Float64()
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err != nil {
		return 0, fmt.Errorf("value is neither number nor string")
	}
	return strconv.ParseFloat(asString, 64)
}

var fetchHyperliquidFundingContextsFn = fetchHyperliquidFundingContexts

type hlFundingContext struct {
	Rate float64
	Err  error
}

func fetchHyperliquidFundingContexts(ctx context.Context) (map[string]hlFundingContext, error) {
	var out map[string]hlFundingContext
	body, err := json.Marshal(map[string]string{"type": "metaAndAssetCtxs"})
	if err != nil {
		return nil, fmt.Errorf("marshal metaAndAssetCtxs request: %w", err)
	}
	err = hlInfoPost(ctx, hlInfoCall{
		BaseURL: hlMainnetURL, Type: "metaAndAssetCtxs", Body: body, Timeout: hlFundingFetchTimeout,
		Decode: func(data []byte) (int, error) {
			contexts, derr := parseHyperliquidFundingContexts(data)
			out = contexts
			return 0, derr
		},
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func parseHyperliquidFundingContexts(data []byte) (map[string]hlFundingContext, error) {
	var payload []json.RawMessage
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("parse metaAndAssetCtxs: %w", err)
	}
	if len(payload) < 2 {
		return nil, fmt.Errorf("metaAndAssetCtxs response has %d elements, want 2", len(payload))
	}
	var meta struct {
		Universe []struct {
			Name string `json:"name"`
		} `json:"universe"`
	}
	if err := json.Unmarshal(payload[0], &meta); err != nil {
		return nil, fmt.Errorf("parse metaAndAssetCtxs universe: %w", err)
	}
	var ctxs []json.RawMessage
	if err := json.Unmarshal(payload[1], &ctxs); err != nil {
		return nil, fmt.Errorf("parse metaAndAssetCtxs contexts: %w", err)
	}
	out := make(map[string]hlFundingContext, len(meta.Universe))
	for i, asset := range meta.Universe {
		if _, seen := out[asset.Name]; seen {
			continue
		}
		if i >= len(ctxs) {
			out[asset.Name] = hlFundingContext{Err: fmt.Errorf("metaAndAssetCtxs has no context for %s", asset.Name)}
			continue
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(ctxs[i], &fields); err != nil || fields == nil {
			out[asset.Name] = hlFundingContext{Err: fmt.Errorf("metaAndAssetCtxs context for %s is malformed", asset.Name)}
			continue
		}
		raw, ok := fields["funding"]
		if !ok {
			out[asset.Name] = hlFundingContext{}
			continue
		}
		rate, err := hlParseNumeric(raw)
		if err == nil && (math.IsNaN(rate) || math.IsInf(rate, 0)) {
			err = fmt.Errorf("value is not finite")
		}
		if err != nil {
			out[asset.Name] = hlFundingContext{Err: fmt.Errorf("metaAndAssetCtxs funding for %s: %w", asset.Name, err)}
			continue
		}
		out[asset.Name] = hlFundingContext{Rate: rate}
	}
	return out, nil
}

func hlFundingContextRate(contexts map[string]hlFundingContext, coin string) (float64, error) {
	c, ok := contexts[coin]
	if !ok {
		return 0, fmt.Errorf("metaAndAssetCtxs has no universe entry for %s", coin)
	}
	return c.Rate, c.Err
}

var fetchHyperliquidFundingHistoryFn = fetchHyperliquidFundingHistory

func fetchHyperliquidFundingHistory(ctx context.Context, coin string, startMs, endMs int64) ([]feedFundingRecord, error) {
	bound, err := hlFundingRowBound(startMs, endMs)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(map[string]any{
		"type":      "fundingHistory",
		"coin":      coin,
		"startTime": startMs,
		"endTime":   endMs,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal fundingHistory request: %w", err)
	}
	var out []feedFundingRecord
	err = hlInfoPost(ctx, hlInfoCall{
		BaseURL: hlMainnetURL, Type: "fundingHistory", Body: body, Timeout: hlFundingFetchTimeout, RowBound: bound,
		Decode: func(data []byte) (int, error) {
			records, derr := parseHyperliquidFundingHistory(data, startMs, endMs)
			out = records
			return len(records), derr
		},
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func parseHyperliquidFundingHistory(data []byte, startMs, endMs int64) ([]feedFundingRecord, error) {
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(data, &rows); err != nil {
		return nil, fmt.Errorf("parse fundingHistory: %w", err)
	}
	out := make([]feedFundingRecord, 0, len(rows))
	for i, row := range rows {
		rateRaw, ok := row["fundingRate"]
		if !ok {
			return nil, fmt.Errorf("fundingHistory row %d has no fundingRate", i)
		}
		rate, rerr := hlParseNumeric(rateRaw)
		if rerr != nil {
			return nil, fmt.Errorf("fundingHistory row %d rate: %w", i, rerr)
		}
		if math.IsNaN(rate) || math.IsInf(rate, 0) {
			return nil, fmt.Errorf("fundingHistory row %d rate is not finite", i)
		}
		timeRaw, ok := row["time"]
		if !ok {
			return nil, fmt.Errorf("fundingHistory row %d has no time", i)
		}
		ts, terr := hlParseNumeric(timeRaw)
		if terr != nil {
			return nil, fmt.Errorf("fundingHistory row %d time: %w", i, terr)
		}
		if math.IsNaN(ts) || math.IsInf(ts, 0) || ts <= 0 || ts != math.Trunc(ts) || ts > float64(math.MaxInt64/2) {
			return nil, fmt.Errorf("fundingHistory row %d time %v is not a positive whole millisecond", i, ts)
		}
		if t := int64(ts); t < startMs || t > endMs {
			return nil, fmt.Errorf("fundingHistory row %d time %d is outside the requested [%d, %d] interval", i, t, startMs, endMs)
		}
		out = append(out, feedFundingRecord{Rate: rate, TimeMs: int64(ts)})
	}
	return out, nil
}

func hlFundingAverageStart(endMs int64) int64 {
	return endMs - int64(hlFundingAvgWindowDays)*86_400_000
}

func hlFundingAverage7d(ctx context.Context, coin string, now time.Time) (float64, error) {
	endMs := now.UTC().UnixMilli()
	records, err := fetchHyperliquidFundingHistoryFn(ctx, coin, hlFundingAverageStart(endMs), endMs)
	if err != nil {
		return 0, err
	}
	return hlFundingMean(records), nil
}

func hlFundingMean(records []feedFundingRecord) float64 {
	if len(records) == 0 {
		return 0
	}
	sum := 0.0
	for _, r := range records {
		sum += r.Rate
	}
	return sum / float64(len(records))
}

type feedFundingCoverage struct {
	Coin    string
	FromMs  int64
	ToMs    int64
	Records []feedFundingRecord
}

func hlFundingPagedRecords(ctx context.Context, coin string, startMs, endMs int64, maxPasses int) ([]feedFundingRecord, error) {
	var out []feedFundingRecord
	seen := make(map[int64]float64)
	cursor := startMs
	for pass := 0; pass < maxPasses && cursor <= endMs; pass++ {
		records, err := fetchHyperliquidFundingHistoryFn(ctx, coin, cursor, endMs)
		if err != nil {
			return nil, err
		}
		if len(records) == 0 {
			break
		}
		progressed := false
		var lastT int64
		for _, r := range records {
			if math.IsNaN(r.Rate) || math.IsInf(r.Rate, 0) {
				return nil, fmt.Errorf("fundingHistory %s record at %d carries a non-finite rate", coin, r.TimeMs)
			}
			if r.TimeMs <= 0 {
				return nil, fmt.Errorf("fundingHistory %s record carries time %d", coin, r.TimeMs)
			}
			if r.TimeMs > lastT {
				lastT = r.TimeMs
			}
			if prior, ok := seen[r.TimeMs]; ok {
				if prior != r.Rate {
					return nil, fmt.Errorf("fundingHistory %s has conflicting records at %d (rate %v and %v)", coin, r.TimeMs, prior, r.Rate)
				}
				continue
			}
			if r.TimeMs < startMs || r.TimeMs > endMs {
				continue
			}
			seen[r.TimeMs] = r.Rate
			out = append(out, r)
			progressed = true
		}
		if lastT < cursor || !progressed {
			break
		}
		cursor = lastT + 1
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TimeMs < out[j].TimeMs })
	return out, nil
}

var (
	errFundingRangeIncomplete = errors.New("the funding history range is not proved complete")
	errFundingTailMismatch    = errors.New("the funding history page does not start with the retained boundary record")
)

func hlFundingRangeProvedComplete(pageRows int, lastMs, endMs int64) bool {
	if pageRows == 0 {
		return true
	}
	if pageRows >= hlFundingRowCap {
		return false
	}
	lastHour, endHour := lastMs/hlFundingRecordStepMs, endMs/hlFundingRecordStepMs
	if lastHour >= endHour {
		return true
	}
	return lastHour == endHour-1 && endMs%hlFundingRecordStepMs <= hlFundingPublishGraceMs
}

func hlFundingPageRecords(coin string, rows []feedFundingRecord, fromMs, endMs int64) ([]feedFundingRecord, error) {
	out := make([]feedFundingRecord, 0, len(rows))
	for _, r := range rows {
		if math.IsNaN(r.Rate) || math.IsInf(r.Rate, 0) {
			return nil, fmt.Errorf("fundingHistory %s record at %d carries a non-finite rate", coin, r.TimeMs)
		}
		if r.TimeMs < fromMs || r.TimeMs > endMs || r.TimeMs <= 0 {
			return nil, fmt.Errorf("fundingHistory %s record at %d is outside the requested [%d, %d] interval", coin, r.TimeMs, fromMs, endMs)
		}
		if n := len(out); n > 0 {
			prev := out[n-1]
			if r.TimeMs < prev.TimeMs {
				return nil, fmt.Errorf("fundingHistory %s record at %d follows a later record at %d", coin, r.TimeMs, prev.TimeMs)
			}
			if r.TimeMs == prev.TimeMs {
				if r.Rate != prev.Rate {
					return nil, fmt.Errorf("fundingHistory %s has conflicting records at %d (rate %v and %v)", coin, r.TimeMs, prev.Rate, r.Rate)
				}
				continue
			}
		}
		out = append(out, r)
	}
	return out, nil
}

func hlFundingReadPage(ctx context.Context, coin string, cursor, endMs int64, boundary *feedFundingRecord) ([]feedFundingRecord, bool, error) {
	from := cursor
	if boundary != nil {
		from = boundary.TimeMs
	}
	if endMs < from {
		return nil, false, fmt.Errorf("fundingHistory %s range end %d is before its start %d", coin, endMs, from)
	}
	rows, err := fetchHyperliquidFundingHistoryFn(ctx, coin, from, endMs)
	if err != nil {
		return nil, false, err
	}
	page, err := hlFundingPageRecords(coin, rows, from, endMs)
	if err != nil {
		return nil, false, err
	}
	lastMs := int64(0)
	if n := len(page); n > 0 {
		lastMs = page[n-1].TimeMs
	}
	complete := hlFundingRangeProvedComplete(len(rows), lastMs, endMs) || (len(page) > 0 && lastMs >= endMs)
	if boundary == nil {
		return page, complete, nil
	}
	if len(page) == 0 || page[0].TimeMs != boundary.TimeMs {
		return nil, false, fmt.Errorf("fundingHistory %s page from %d did not return the boundary record: %w", coin, boundary.TimeMs, errFundingTailMismatch)
	}
	if page[0].Rate != boundary.Rate {
		return nil, false, fmt.Errorf("fundingHistory %s boundary record at %d changed rate from %v to %v: %w", coin, boundary.TimeMs, boundary.Rate, page[0].Rate, errFundingTailMismatch)
	}
	return page[1:], complete, nil
}

func hlFundingRecordsCoverage(ctx context.Context, coin string, startMs int64, now time.Time, maxPasses int) (feedFundingCoverage, error) {
	if maxPasses <= 0 {
		maxPasses = 1
	}
	records, err := hlFundingPagedRecords(ctx, coin, startMs, now.UTC().UnixMilli(), maxPasses)
	if err != nil {
		return feedFundingCoverage{}, err
	}
	cov := feedFundingCoverage{Coin: coin, FromMs: startMs, Records: records}
	if n := len(records); n > 0 {
		cov.ToMs = records[n-1].TimeMs
	}
	return cov, nil
}
