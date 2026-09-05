package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal info request: %w", err)
	}
	reqCtx, cancel := context.WithTimeout(ctx, hlFundingFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, hlMainnetURL+"/info", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build info request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: hlFundingFetchTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http %d from %s/info", resp.StatusCode, hlMainnetURL)
	}
	return io.ReadAll(resp.Body)
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

func fetchHyperliquidFundingHistory(ctx context.Context, coin string, startMs int64) ([]feedFundingRecord, error) {
	data, err := hlPostInfo(ctx, map[string]any{
		"type":      "fundingHistory",
		"coin":      coin,
		"startTime": startMs,
	})
	if err != nil {
		return nil, err
	}
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
		timeRaw, ok := row["time"]
		if !ok {
			return nil, fmt.Errorf("fundingHistory row %d has no time", i)
		}
		ts, terr := hlParseNumeric(timeRaw)
		if terr != nil {
			return nil, fmt.Errorf("fundingHistory row %d time: %w", i, terr)
		}
		out = append(out, feedFundingRecord{Rate: rate, TimeMs: int64(ts)})
	}
	return out, nil
}

func hlFundingAverage7d(ctx context.Context, coin string, now time.Time) (float64, error) {
	startMs := now.UTC().UnixMilli() - int64(hlFundingAvgWindowDays)*86_400_000
	records, err := fetchHyperliquidFundingHistoryFn(ctx, coin, startMs)
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

func hlFundingRecordsSince(ctx context.Context, coin string, startMs int64, now time.Time) ([]feedFundingRecord, error) {
	endMs := now.UTC().UnixMilli()
	var out []feedFundingRecord
	seen := make(map[int64]bool)
	cursor := startMs
	for pass := 0; pass < hlFundingMaxRangePasses && cursor < endMs; pass++ {
		records, err := fetchHyperliquidFundingHistoryFn(ctx, coin, cursor)
		if err != nil {
			return nil, err
		}
		if len(records) == 0 {
			break
		}
		progressed := false
		for _, r := range records {
			if r.TimeMs > endMs || seen[r.TimeMs] {
				continue
			}
			seen[r.TimeMs] = true
			out = append(out, r)
			progressed = true
		}
		lastT := records[len(records)-1].TimeMs
		if lastT <= cursor || !progressed {
			break
		}
		cursor = lastT + 1
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TimeMs < out[j].TimeMs })
	return out, nil
}
