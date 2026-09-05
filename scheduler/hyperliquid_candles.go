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
	hlCandleGapMargin       = 50
	hlCandleMaxCandles      = 5000
	hlCandleMaxExtendPasses = 12
	hlCandleFetchTimeout    = 20 * time.Second
)

var hlCandleIntervalMsTable = map[string]int64{
	"1m":  60_000,
	"3m":  180_000,
	"5m":  300_000,
	"15m": 900_000,
	"30m": 1_800_000,
	"1h":  3_600_000,
	"2h":  7_200_000,
	"4h":  14_400_000,
	"8h":  28_800_000,
	"12h": 43_200_000,
	"1d":  86_400_000,
	"3d":  259_200_000,
	"1w":  604_800_000,
	"1M":  2_678_400_000,
}

func hlCandleIntervalMs(interval string) (int64, bool) {
	ms, ok := hlCandleIntervalMsTable[interval]
	return ms, ok
}

func hlSupportedCandleIntervals() []string {
	out := make([]string, 0, len(hlCandleIntervalMsTable))
	for tf := range hlCandleIntervalMsTable {
		out = append(out, tf)
	}
	sort.Slice(out, func(i, j int) bool {
		return hlCandleIntervalMsTable[out[i]] < hlCandleIntervalMsTable[out[j]]
	})
	return out
}

type hlCandleRaw struct {
	OpenMs   int64
	CloseMs  int64
	HasClose bool
	Open     float64
	High     float64
	Low      float64
	Close    float64
	Volume   float64
}

type hlCandleRow struct {
	TimestampMs int64
	Open        float64
	High        float64
	Low         float64
	Close       float64
	Volume      float64
}

func (r hlCandleRow) MarshalJSON() ([]byte, error) {
	return json.Marshal([]any{r.TimestampMs, r.Open, r.High, r.Low, r.Close, r.Volume})
}

func (r *hlCandleRow) UnmarshalJSON(data []byte) error {
	var raw []json.Number
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if len(raw) != 6 {
		return fmt.Errorf("candle row must have 6 fields, got %d", len(raw))
	}
	ts, err := raw[0].Int64()
	if err != nil {
		return fmt.Errorf("candle row timestamp: %w", err)
	}
	vals := make([]float64, 5)
	for i := 0; i < 5; i++ {
		f, ferr := raw[i+1].Float64()
		if ferr != nil {
			return fmt.Errorf("candle row field %d: %w", i+1, ferr)
		}
		vals[i] = f
	}
	r.TimestampMs = ts
	r.Open, r.High, r.Low, r.Close, r.Volume = vals[0], vals[1], vals[2], vals[3], vals[4]
	return nil
}

func hlCandleRowFromRaw(c hlCandleRaw) hlCandleRow {
	ts := c.OpenMs
	if c.HasClose {
		ts = c.CloseMs
	}
	return hlCandleRow{
		TimestampMs: ts,
		Open:        c.Open,
		High:        c.High,
		Low:         c.Low,
		Close:       c.Close,
		Volume:      c.Volume,
	}
}

func hlCandleRowsFromRaws(raws []hlCandleRaw) []hlCandleRow {
	out := make([]hlCandleRow, 0, len(raws))
	for _, c := range raws {
		out = append(out, hlCandleRowFromRaw(c))
	}
	return out
}

func hlCandleNumberField(fields map[string]json.RawMessage, key string) (float64, bool, error) {
	raw, ok := fields[key]
	if !ok {
		return 0, false, nil
	}
	var asNumber json.Number
	if err := json.Unmarshal(raw, &asNumber); err == nil {
		f, ferr := asNumber.Float64()
		if ferr != nil {
			return 0, true, fmt.Errorf("field %q: %w", key, ferr)
		}
		return f, true, nil
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err != nil {
		return 0, true, fmt.Errorf("field %q is neither number nor string", key)
	}
	f, ferr := strconv.ParseFloat(asString, 64)
	if ferr != nil {
		return 0, true, fmt.Errorf("field %q: %w", key, ferr)
	}
	return f, true, nil
}

func hlCandleIntField(fields map[string]json.RawMessage, key string) (int64, bool, error) {
	f, present, err := hlCandleNumberField(fields, key)
	if err != nil || !present {
		return 0, present, err
	}
	return int64(f), true, nil
}

func hlCandleRawFromFields(fields map[string]json.RawMessage) (hlCandleRaw, error) {
	var out hlCandleRaw
	openMs, _, err := hlCandleIntField(fields, "t")
	if err != nil {
		return out, err
	}
	out.OpenMs = openMs
	closeMs, hasClose, err := hlCandleIntField(fields, "T")
	if err != nil {
		return out, err
	}
	out.CloseMs, out.HasClose = closeMs, hasClose
	for key, dest := range map[string]*float64{
		"o": &out.Open,
		"h": &out.High,
		"l": &out.Low,
		"c": &out.Close,
		"v": &out.Volume,
	} {
		v, present, ferr := hlCandleNumberField(fields, key)
		if ferr != nil {
			return out, ferr
		}
		if !present {
			return out, fmt.Errorf("candle is missing field %q", key)
		}
		*dest = v
	}
	return out, nil
}

func parseHyperliquidCandleRaws(data []byte) ([]hlCandleRaw, error) {
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(data, &rows); err != nil {
		return nil, fmt.Errorf("parse candleSnapshot response: %w", err)
	}
	out := make([]hlCandleRaw, 0, len(rows))
	for i, fields := range rows {
		raw, err := hlCandleRawFromFields(fields)
		if err != nil {
			return nil, fmt.Errorf("candleSnapshot row %d: %w", i, err)
		}
		out = append(out, raw)
	}
	return out, nil
}

var fetchHyperliquidCandleSnapshotFn = fetchHyperliquidCandleSnapshot

func fetchHyperliquidCandleSnapshot(ctx context.Context, coin, interval string, startMs, endMs int64) ([]hlCandleRaw, error) {
	payload := map[string]any{
		"type": "candleSnapshot",
		"req": map[string]any{
			"coin":      coin,
			"interval":  interval,
			"startTime": startMs,
			"endTime":   endMs,
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal candleSnapshot request: %w", err)
	}
	reqCtx, cancel := context.WithTimeout(ctx, hlCandleFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, hlMainnetURL+"/info", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build candleSnapshot request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: hlCandleFetchTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http %d from %s/info candleSnapshot", resp.StatusCode, hlMainnetURL)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read candleSnapshot response: %w", err)
	}
	return parseHyperliquidCandleRaws(data)
}

type hlCandleHistory struct {
	Raws  []hlCandleRaw
	Short bool
}

func hlFetchCandleHistory(ctx context.Context, coin, interval string, limit int, now time.Time) (hlCandleHistory, error) {
	intervalMs, ok := hlCandleIntervalMs(interval)
	if !ok {
		return hlCandleHistory{}, fmt.Errorf("unsupported Hyperliquid interval %q", interval)
	}
	if limit <= 0 {
		return hlCandleHistory{}, fmt.Errorf("candle history limit must be > 0, got %d", limit)
	}
	endMs := now.UTC().UnixMilli()
	requested := int64(limit) + hlCandleGapMargin
	var result []hlCandleRaw
	prevCount := -1
	staleWidens := 0
	for pass := 0; pass < hlCandleMaxExtendPasses; pass++ {
		startMs := endMs - intervalMs*requested
		raws, err := fetchHyperliquidCandleSnapshotFn(ctx, coin, interval, startMs, endMs)
		if err != nil {
			return hlCandleHistory{}, err
		}
		result = raws
		if len(result) == 0 || len(result) >= limit || len(result) >= hlCandleMaxCandles {
			break
		}
		if len(result) > prevCount {
			staleWidens = 0
		} else {
			staleWidens++
			if staleWidens >= 2 {
				break
			}
		}
		prevCount = len(result)
		requested *= 2
	}
	if len(result) > limit {
		result = result[len(result)-limit:]
	}
	return hlCandleHistory{Raws: result, Short: len(result) < limit}, nil
}
