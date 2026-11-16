package main

import (
	"bytes"
	"context"
	"encoding/json"
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
	permit, err := feedBudgetAcquireBounded(ctx, call.BaseURL, call.Type, bound)
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
	if bound > 0 && rows > bound {
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

var fetchHyperliquidFundingRateFn = fetchHyperliquidFundingRate

func fetchHyperliquidFundingRate(ctx context.Context, coin string) (float64, error) {
	data, err := hlPostInfo(ctx, map[string]string{"type": "metaAndAssetCtxs"})
	if err != nil {
		return 0, err
	}
	var payload []json.RawMessage
	if err := json.Unmarshal(data, &payload); err != nil {
		return 0, fmt.Errorf("parse metaAndAssetCtxs: %w", err)
	}
	if len(payload) < 2 {
		return 0, fmt.Errorf("metaAndAssetCtxs response has %d elements, want 2", len(payload))
	}
	var meta struct {
		Universe []struct {
			Name string `json:"name"`
		} `json:"universe"`
	}
	if err := json.Unmarshal(payload[0], &meta); err != nil {
		return 0, fmt.Errorf("parse metaAndAssetCtxs universe: %w", err)
	}
	var ctxs []map[string]json.RawMessage
	if err := json.Unmarshal(payload[1], &ctxs); err != nil {
		return 0, fmt.Errorf("parse metaAndAssetCtxs contexts: %w", err)
	}
	for i, asset := range meta.Universe {
		if asset.Name != coin {
			continue
		}
		if i >= len(ctxs) {
			return 0, fmt.Errorf("metaAndAssetCtxs has no context for %s", coin)
		}
		raw, ok := ctxs[i]["funding"]
		if !ok {
			return 0, nil
		}
		return hlParseNumeric(raw)
	}
	return 0, fmt.Errorf("metaAndAssetCtxs has no universe entry for %s", coin)
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

func hlFundingAverage7d(ctx context.Context, coin string, now time.Time) (float64, error) {
	endMs := now.UTC().UnixMilli()
	startMs := endMs - int64(hlFundingAvgWindowDays)*86_400_000
	records, err := fetchHyperliquidFundingHistoryFn(ctx, coin, startMs, endMs)
	if err != nil {
		return 0, err
	}
	if len(records) == 0 {
		return 0, nil
	}
	sum := 0.0
	for _, r := range records {
		sum += r.Rate
	}
	return sum / float64(len(records)), nil
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

func hlFundingRecordsSince(ctx context.Context, coin string, startMs int64, now time.Time) ([]feedFundingRecord, error) {
	return hlFundingPagedRecords(ctx, coin, startMs, now.UTC().UnixMilli(), hlFundingMaxRangePasses)
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
