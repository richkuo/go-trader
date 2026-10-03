package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"time"
)

const (
	feedWireVersion         = 1
	feedSealVersion         = 1
	feedWireMaxRequestBytes = 4 << 10
	feedWireMaxHeaderBytes  = 1 << 20
	feedSealMaxBytes        = 64 << 20

	feedWireOpDescribe = "describe"
	feedWireOpSnapshot = "snapshot"

	feedWireStatusSealed      = "sealed"
	feedWireStatusPending     = "pending"
	feedWireStatusUnavailable = "unavailable"
	feedWireStatusDescribe    = "describe"
	feedWireStatusError       = "error"

	feedSourceWebsocket = "websocket"
	feedSourceREST      = "rest"
)

type feedWireRequest struct {
	V   int    `json:"v"`
	Op  string `json:"op"`
	Key int64  `json:"key,omitempty"`
}

type feedWireHeader struct {
	V              int           `json:"v"`
	Status         string        `json:"status"`
	Detail         string        `json:"detail,omitempty"`
	Key            int64         `json:"key,omitempty"`
	Instance       string        `json:"instance"`
	Source         string        `json:"source"`
	Generation     uint64        `json:"generation"`
	SealVersion    int           `json:"seal_version"`
	PayloadVersion int           `json:"payload_version"`
	SealedAtMs     int64         `json:"sealed_at_ms,omitempty"`
	Hash           string        `json:"hash,omitempty"`
	Bytes          int           `json:"bytes,omitempty"`
	RetryAfterMs   int64         `json:"retry_after_ms,omitempty"`
	GiveUpAtMs     int64         `json:"give_up_at_ms,omitempty"`
	Describe       *feedDescribe `json:"describe,omitempty"`
}

type feedDescribeKey struct {
	Host      string `json:"host"`
	Namespace string `json:"namespace"`
	Symbol    string `json:"symbol"`
	Timeframe string `json:"timeframe"`
	Required  int    `json:"required"`
}

type feedDescribeFunding struct {
	Coin    string `json:"coin"`
	Scalar  bool   `json:"scalar"`
	Records bool   `json:"records"`
}

type feedDescribeCadence struct {
	Seconds       int   `json:"seconds"`
	FirstDeadline int64 `json:"first_deadline"`
}

type feedDescribe struct {
	StartedAtMs       int64                 `json:"started_at_ms"`
	Serving           bool                  `json:"serving"`
	FirstDeadline     int64                 `json:"first_deadline,omitempty"`
	SettleMs          int64                 `json:"settle_ms"`
	PrepareMs         int64                 `json:"prepare_ms"`
	PublishGraceMs    int64                 `json:"publish_grace_ms"`
	RetainPerCadence  int                   `json:"retain_per_cadence"`
	RetainedKeys      []int64               `json:"retained_keys"`
	Cadences          []feedDescribeCadence `json:"cadences"`
	Keys              []feedDescribeKey     `json:"keys"`
	MidCoins          []string              `json:"mid_coins"`
	Funding           []feedDescribeFunding `json:"funding"`
	LastSealKey       int64                 `json:"last_seal_key,omitempty"`
	LastSealHash      string                `json:"last_seal_hash,omitempty"`
	LastSealReady     int                   `json:"last_seal_ready"`
	LastSealKeysTotal int                   `json:"last_seal_keys_total"`
}

type feedSealBar struct {
	OpenMs   int64   `json:"t"`
	CloseMs  int64   `json:"e"`
	HasClose bool    `json:"x"`
	Open     float64 `json:"o"`
	High     float64 `json:"h"`
	Low      float64 `json:"l"`
	Close    float64 `json:"c"`
	Volume   float64 `json:"v"`
}

type feedSealReadiness struct {
	Status        string `json:"status"`
	Detail        string `json:"detail"`
	Ready         bool   `json:"ready"`
	Stale         bool   `json:"stale"`
	StaleReason   string `json:"stale_reason"`
	CoverageShort bool   `json:"coverage_short"`
	Bars          int    `json:"bars"`
	Required      int    `json:"required"`
	FirstOpenMs   int64  `json:"first_open_ms"`
	LastOpenMs    int64  `json:"last_open_ms"`
	LastCloseMs   int64  `json:"last_close_ms"`
	LastRecvAtMs  int64  `json:"last_recv_at_ms"`
	Source        string `json:"source"`
}

type feedSealKey struct {
	Host       string            `json:"host"`
	Namespace  string            `json:"namespace"`
	Symbol     string            `json:"symbol"`
	Timeframe  string            `json:"timeframe"`
	IntervalMs int64             `json:"interval_ms"`
	Readiness  feedSealReadiness `json:"readiness"`
	Bars       []feedSealBar     `json:"bars"`
}

type feedSealMid struct {
	Coin     string  `json:"coin"`
	Px       float64 `json:"px"`
	RecvAtMs int64   `json:"recv_at_ms"`
	Source   string  `json:"source"`
}

type feedSealFunding struct {
	Coin        string              `json:"coin"`
	Current     float64             `json:"current"`
	Avg7d       float64             `json:"avg_7d"`
	HasScalar   bool                `json:"has_scalar"`
	Records     []feedFundingRecord `json:"records"`
	HasRecords  bool                `json:"has_records"`
	FetchedAtMs int64               `json:"fetched_at_ms"`
	Source      string              `json:"source"`
	Error       string              `json:"error"`
}

type feedSealMetrics struct {
	BootstrapCalls    int `json:"bootstrap_calls"`
	RepairCalls       int `json:"repair_calls"`
	RecoveryCalls     int `json:"recovery_calls"`
	SteadyCandleCalls int `json:"steady_candle_calls"`
}

type feedSealDoc struct {
	V          int               `json:"v"`
	Key        int64             `json:"key"`
	Source     string            `json:"source"`
	Instance   string            `json:"instance"`
	Generation uint64            `json:"generation"`
	SealedAtMs int64             `json:"sealed_at_ms"`
	Connected  bool              `json:"connected"`
	Metrics    feedSealMetrics   `json:"metrics"`
	Keys       []feedSealKey     `json:"keys"`
	Mids       []feedSealMid     `json:"mids"`
	Funding    []feedSealFunding `json:"funding"`
}

func feedSealEvaluationID(key int64) string {
	return fmt.Sprintf("feed/%d", key)
}

func feedTimeMs(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UTC().UnixMilli()
}

func feedMsTime(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}

func feedSealDocFromSnapshot(snap *marketSnapshot, key int64, source, instance string) (*feedSealDoc, error) {
	if snap == nil {
		return nil, errors.New("no snapshot to seal")
	}
	doc := &feedSealDoc{
		V:          feedSealVersion,
		Key:        key,
		Source:     source,
		Instance:   instance,
		Generation: snap.ConfigGeneration,
		SealedAtMs: feedTimeMs(snap.SealedAt),
		Connected:  snap.Connected,
		Metrics: feedSealMetrics{
			BootstrapCalls:    snap.Metrics.BootstrapCalls,
			RepairCalls:       snap.Metrics.RepairCalls,
			RecoveryCalls:     snap.Metrics.RecoveryCalls,
			SteadyCandleCalls: snap.Metrics.SteadyCandleCalls,
		},
		Keys:    []feedSealKey{},
		Mids:    []feedSealMid{},
		Funding: []feedSealFunding{},
	}
	keys := make([]marketFeedKey, 0, len(snap.keys))
	for k := range snap.keys {
		keys = append(keys, k)
	}
	sortMarketFeedKeys(keys)
	for _, k := range keys {
		entry := snap.keys[k]
		if entry == nil {
			continue
		}
		r := entry.Readiness
		sk := feedSealKey{
			Host:       k.Host,
			Namespace:  k.Namespace,
			Symbol:     k.Symbol,
			Timeframe:  k.Timeframe,
			IntervalMs: entry.IntervalMs,
			Readiness: feedSealReadiness{
				Status:        string(r.Status),
				Detail:        r.Detail,
				Ready:         r.Ready,
				Stale:         r.Stale,
				StaleReason:   r.StaleReason,
				CoverageShort: r.CoverageShort,
				Bars:          r.Bars,
				Required:      r.Required,
				FirstOpenMs:   r.FirstOpenMs,
				LastOpenMs:    r.LastOpenMs,
				LastCloseMs:   r.LastCloseMs,
				LastRecvAtMs:  feedTimeMs(r.LastRecvAt),
				Source:        r.Source,
			},
			Bars: make([]feedSealBar, 0, len(entry.Bars)),
		}
		for _, b := range entry.Bars {
			sk.Bars = append(sk.Bars, feedSealBar{
				OpenMs:   b.OpenMs,
				CloseMs:  b.CloseMs,
				HasClose: b.HasClose,
				Open:     b.Open,
				High:     b.High,
				Low:      b.Low,
				Close:    b.Close,
				Volume:   b.Volume,
			})
		}
		doc.Keys = append(doc.Keys, sk)
	}
	coins := make([]string, 0, len(snap.mids))
	for c := range snap.mids {
		coins = append(coins, c)
	}
	sort.Strings(coins)
	for _, c := range coins {
		m := snap.mids[c]
		doc.Mids = append(doc.Mids, feedSealMid{Coin: c, Px: m.Px, RecvAtMs: feedTimeMs(m.RecvAt), Source: m.Source})
	}
	fundingCoins := make([]string, 0, len(snap.funding))
	for c := range snap.funding {
		fundingCoins = append(fundingCoins, c)
	}
	sort.Strings(fundingCoins)
	for _, c := range fundingCoins {
		f := snap.funding[c]
		records := append([]feedFundingRecord{}, f.Records...)
		doc.Funding = append(doc.Funding, feedSealFunding{
			Coin:        c,
			Current:     f.Current,
			Avg7d:       f.Avg7d,
			HasScalar:   f.HasScalar,
			Records:     records,
			HasRecords:  f.HasRecords,
			FetchedAtMs: feedTimeMs(f.FetchedAt),
			Source:      f.Source,
			Error:       f.Err,
		})
	}
	return doc, nil
}

func encodeFeedSeal(doc *feedSealDoc) ([]byte, string, error) {
	if doc == nil {
		return nil, "", errors.New("no seal document")
	}
	blob, err := json.Marshal(doc)
	if err != nil {
		return nil, "", fmt.Errorf("marshal seal: %w", err)
	}
	if len(blob) > feedSealMaxBytes {
		return nil, "", fmt.Errorf("seal is %d bytes, over the %d-byte transport cap", len(blob), feedSealMaxBytes)
	}
	return blob, feedSealHash(blob), nil
}

func feedSealHash(blob []byte) string {
	sum := sha256.Sum256(blob)
	return hex.EncodeToString(sum[:])
}

func decodeFeedSeal(blob []byte, wantKey int64) (*feedSealDoc, error) {
	dec := json.NewDecoder(bytes.NewReader(blob))
	dec.DisallowUnknownFields()
	var doc feedSealDoc
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("decode seal: %w", err)
	}
	if dec.More() {
		return nil, errors.New("decode seal: trailing data after the seal object")
	}
	canonical, err := json.Marshal(&doc)
	if err != nil {
		return nil, fmt.Errorf("re-encode seal: %w", err)
	}
	if !bytes.Equal(canonical, blob) {
		return nil, errors.New("seal bytes are not in canonical form")
	}
	if doc.V != feedSealVersion {
		return nil, fmt.Errorf("seal version %d, want %d", doc.V, feedSealVersion)
	}
	if doc.Key != wantKey {
		return nil, fmt.Errorf("seal key %d, requested %d", doc.Key, wantKey)
	}
	if doc.SealedAtMs <= 0 {
		return nil, errors.New("seal has no sealed_at_ms")
	}
	if err := validateFeedSealDoc(&doc); err != nil {
		return nil, err
	}
	return &doc, nil
}

func validateFeedSealDoc(doc *feedSealDoc) error {
	var prev *marketFeedKey
	for i := range doc.Keys {
		k := doc.Keys[i]
		key := marketFeedKey{Host: k.Host, Namespace: k.Namespace, Symbol: k.Symbol, Timeframe: k.Timeframe}
		if prev != nil && !marketFeedKeyLess(*prev, key) {
			return fmt.Errorf("seal keys are not strictly ordered at %s", key)
		}
		prev = &key
		intervalMs, ok := hlCandleIntervalMs(k.Timeframe)
		if !ok || intervalMs != k.IntervalMs {
			return fmt.Errorf("seal key %s carries interval %dms", key, k.IntervalMs)
		}
		var lastOpen int64
		for j, b := range k.Bars {
			bar := feedBar{OpenMs: b.OpenMs, CloseMs: b.CloseMs, HasClose: b.HasClose, Open: b.Open, High: b.High, Low: b.Low, Close: b.Close, Volume: b.Volume}
			if err := validateFeedBar(bar, k.IntervalMs); err != nil {
				return fmt.Errorf("seal key %s bar %d: %w", key, j, err)
			}
			if j > 0 && b.OpenMs <= lastOpen {
				return fmt.Errorf("seal key %s bars are not in open-time order at bar %d", key, j)
			}
			lastOpen = b.OpenMs
		}
		if k.Readiness.Bars != len(k.Bars) {
			return fmt.Errorf("seal key %s readiness counts %d bars, carries %d", key, k.Readiness.Bars, len(k.Bars))
		}
	}
	for i, m := range doc.Mids {
		if i > 0 && doc.Mids[i-1].Coin >= m.Coin {
			return fmt.Errorf("seal mids are not strictly ordered at %s", m.Coin)
		}
		if !isFiniteFeedPrice(m.Px) {
			return fmt.Errorf("seal mid %s is %v", m.Coin, m.Px)
		}
	}
	for i, f := range doc.Funding {
		if i > 0 && doc.Funding[i-1].Coin >= f.Coin {
			return fmt.Errorf("seal funding is not strictly ordered at %s", f.Coin)
		}
		if math.IsNaN(f.Current) || math.IsInf(f.Current, 0) || math.IsNaN(f.Avg7d) || math.IsInf(f.Avg7d, 0) {
			return fmt.Errorf("seal funding %s carries a non-finite rate", f.Coin)
		}
	}
	return nil
}

func (doc *feedSealDoc) snapshot() *marketSnapshot {
	snap := &marketSnapshot{
		Version:          marketSnapshotVersion,
		EvaluationID:     feedSealEvaluationID(doc.Key),
		ConfigGeneration: doc.Generation,
		SealedAt:         feedMsTime(doc.SealedAtMs),
		Connected:        doc.Connected,
		Metrics: feedMetrics{
			BootstrapCalls:    doc.Metrics.BootstrapCalls,
			RepairCalls:       doc.Metrics.RepairCalls,
			RecoveryCalls:     doc.Metrics.RecoveryCalls,
			SteadyCandleCalls: doc.Metrics.SteadyCandleCalls,
		},
		Deadline:          time.Unix(doc.Key, 0).UTC(),
		MarksAgeFromNow:   true,
		CorrectionUnknown: true,
		keys:              make(map[marketFeedKey]*marketSnapshotKey, len(doc.Keys)),
		mids:              make(map[string]feedMid, len(doc.Mids)),
		funding:           make(map[string]feedFunding, len(doc.Funding)),
	}
	for _, k := range doc.Keys {
		key := marketFeedKey{Host: k.Host, Namespace: k.Namespace, Symbol: k.Symbol, Timeframe: k.Timeframe}
		bars := make([]feedBar, 0, len(k.Bars))
		for _, b := range k.Bars {
			bars = append(bars, feedBar{OpenMs: b.OpenMs, CloseMs: b.CloseMs, HasClose: b.HasClose, Open: b.Open, High: b.High, Low: b.Low, Close: b.Close, Volume: b.Volume})
		}
		r := k.Readiness
		snap.keys[key] = &marketSnapshotKey{
			Key:        key,
			IntervalMs: k.IntervalMs,
			Bars:       bars,
			Readiness: feedKeyReadiness{
				Key:           key,
				Bars:          r.Bars,
				Required:      r.Required,
				Ready:         r.Ready,
				Stale:         r.Stale,
				StaleReason:   r.StaleReason,
				CoverageShort: r.CoverageShort,
				Status:        feedKeyStatus(r.Status),
				Detail:        r.Detail,
				FirstOpenMs:   r.FirstOpenMs,
				LastOpenMs:    r.LastOpenMs,
				LastCloseMs:   r.LastCloseMs,
				LastRecvAt:    feedMsTime(r.LastRecvAtMs),
				Source:        r.Source,
			},
		}
	}
	for _, m := range doc.Mids {
		snap.mids[m.Coin] = feedMid{Px: m.Px, RecvAt: feedMsTime(m.RecvAtMs), Source: m.Source}
	}
	for _, f := range doc.Funding {
		snap.funding[f.Coin] = feedFunding{
			Current:    f.Current,
			Avg7d:      f.Avg7d,
			HasScalar:  f.HasScalar,
			Records:    append([]feedFundingRecord(nil), f.Records...),
			HasRecords: f.HasRecords,
			FetchedAt:  feedMsTime(f.FetchedAtMs),
			Source:     f.Source,
			Err:        f.Error,
		}
	}
	return snap
}

func writeFeedFrame(w io.Writer, payload []byte) error {
	if len(payload) == 0 {
		return errors.New("empty frame")
	}
	if uint64(len(payload)) > math.MaxUint32 {
		return fmt.Errorf("frame of %d bytes does not fit the length prefix", len(payload))
	}
	var prefix [4]byte
	binary.BigEndian.PutUint32(prefix[:], uint32(len(payload)))
	if _, err := w.Write(prefix[:]); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

func readFeedFrame(r io.Reader, maxBytes int) ([]byte, error) {
	var prefix [4]byte
	if _, err := io.ReadFull(r, prefix[:]); err != nil {
		return nil, fmt.Errorf("read frame length: %w", err)
	}
	n := binary.BigEndian.Uint32(prefix[:])
	if n == 0 {
		return nil, errors.New("frame length is zero")
	}
	if uint64(n) > uint64(maxBytes) {
		return nil, fmt.Errorf("frame length %d is over the %d-byte limit", n, maxBytes)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, fmt.Errorf("read frame body: %w", err)
	}
	return buf, nil
}

func writeFeedJSONFrame(w io.Writer, v any) error {
	blob, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return writeFeedFrame(w, blob)
}

func decodeFeedStrict(blob []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(blob))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("trailing data after the JSON object")
	}
	return nil
}
