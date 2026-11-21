package main

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type testArchiveSeries struct {
	ID           candleIdentity
	Bars         []UICandle
	HistoryStart bool
	ChunkBars    int64
	Raw          func(m *candleArchiveSeriesManifest)
}

type testArchiveSpec struct {
	PublishedAt time.Time
	ExpiresAt   time.Time
	MinCloseAge time.Duration
	Series      []testArchiveSeries
}

func testArchiveBars(from, to time.Time, step time.Duration) []UICandle {
	var out []UICandle
	for t := from.UTC(); !t.After(to.UTC()); t = t.Add(step) {
		out = append(out, UICandle{Time: t.Unix(), Open: 100, High: 120, Low: 90, Close: 105, Volume: 1})
	}
	return out
}

func testArchiveRecord(c UICandle) []byte {
	rec := make([]byte, candleArchiveRecordBytes)
	binary.LittleEndian.PutUint64(rec[0:8], uint64(c.Time*1000))
	for i, v := range []float64{c.Open, c.High, c.Low, c.Close, c.Volume} {
		binary.LittleEndian.PutUint64(rec[8+8*i:16+8*i], math.Float64bits(v))
	}
	return rec
}

func writeTestCandleArchive(t *testing.T, dir string, spec testArchiveSpec) string {
	t.Helper()
	if spec.PublishedAt.IsZero() {
		spec.PublishedAt = time.Now().UTC().Add(-time.Second)
	}
	if spec.ExpiresAt.IsZero() {
		spec.ExpiresAt = spec.PublishedAt.Add(24 * time.Hour)
	}
	tmp, err := os.MkdirTemp(dir, ".build-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(tmp, "series"), 0o755); err != nil {
		t.Fatal(err)
	}
	m := candleArchiveManifest{
		Format:        candleArchiveFormat,
		PublishedAtMs: spec.PublishedAt.UnixMilli(),
		ExpiresAtMs:   spec.ExpiresAt.UnixMilli(),
		Publisher:     candleArchivePublisher{ID: "test-publisher", Tool: "go-test", Version: "1", EgressID: "test-egress"},
		Finalization:  candleArchiveFinalization{MinCloseAgeMs: spec.MinCloseAge.Milliseconds()},
	}
	for i, s := range spec.Series {
		sec := candleGridSeconds[s.ID.Timeframe]
		chunk := s.ChunkBars
		if chunk <= 0 {
			chunk = 16
		}
		var data []byte
		var chunks []string
		for start := 0; start < len(s.Bars); start += int(chunk) {
			end := start + int(chunk)
			if end > len(s.Bars) {
				end = len(s.Bars)
			}
			var part []byte
			for _, c := range s.Bars[start:end] {
				part = append(part, testArchiveRecord(c)...)
			}
			sum := sha256.Sum256(part)
			chunks = append(chunks, hex.EncodeToString(sum[:]))
			data = append(data, part...)
		}
		var segs []candleArchiveSegment
		for j, c := range s.Bars {
			if j == 0 || c.Time-s.Bars[j-1].Time != sec {
				segs = append(segs, candleArchiveSegment{FirstOpenMs: c.Time * 1000, LastOpenMs: c.Time * 1000, Bars: 1})
				continue
			}
			segs[len(segs)-1].LastOpenMs = c.Time * 1000
			segs[len(segs)-1].Bars++
		}
		file := fmt.Sprintf("series/s%03d.bars", i)
		if err := os.WriteFile(filepath.Join(tmp, filepath.FromSlash(file)), data, 0o644); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		var last int64
		if len(s.Bars) > 0 {
			last = s.Bars[len(s.Bars)-1].Time * 1000
		}
		sm := candleArchiveSeriesManifest{
			Venue: s.ID.Venue, Network: s.ID.Network, InstrumentClass: s.ID.InstrumentClass, Symbol: s.ID.Symbol, Timeframe: s.ID.Timeframe,
			IntervalMs: sec * 1000, Timestamp: candleArchiveTimestampOpen, File: file, Bars: int64(len(s.Bars)), Bytes: int64(len(data)),
			SHA256: hex.EncodeToString(sum[:]), ChunkBars: chunk, ChunkSHA256: chunks, Segments: segs,
			FinalizedThroughOpenMs: last, HistoryStartKnown: s.HistoryStart,
			Source: candleArchiveSource{Kind: "test", Detail: "go test fixture", ObservedAtMs: spec.PublishedAt.UnixMilli()},
		}
		if s.Raw != nil {
			s.Raw(&sm)
		}
		m.Series = append(m.Series, sm)
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, candleArchiveManifestName), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	id := candleArchiveGenerationID(m.PublishedAtMs, raw)
	if err := os.MkdirAll(filepath.Join(dir, candleArchiveGenerationsDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, candleArchiveGenerationsDir, id)); err != nil {
		t.Fatal(err)
	}
	cur := filepath.Join(dir, candleArchiveCurrentName+".tmp")
	if err := os.WriteFile(cur, []byte(id+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(cur, filepath.Join(dir, candleArchiveCurrentName)); err != nil {
		t.Fatal(err)
	}
	return id
}

func attachTestCandleArchive(t *testing.T, ss *StatusServer, dir string) {
	t.Helper()
	ss.ConfigureDashboardCandles(&DashboardCandlesConfig{ArchiveDir: dir})
	ss.candles.archive.check(context.Background())
}

func hlTestIdentity(symbol, tf string) candleIdentity {
	return candleIdentity{Venue: candleVenueHyperliquid, Network: candleNetworkMainnet, InstrumentClass: candleClassPerps, Symbol: symbol, Timeframe: tf}
}
