package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	candleArchiveFormat         = "go-trader-candle-archive/v1"
	candleArchiveRecordBytes    = 48
	candleArchiveManifestName   = "manifest.json"
	candleArchiveCurrentName    = "CURRENT"
	candleArchiveGenerationsDir = "generations"
	candleArchiveMaxManifest    = 16 << 20
	candleArchiveMaxCurrent     = 256
	candleArchiveMaxSeries      = 4096
	candleArchiveMaxChunkBars   = 1 << 16
	candleArchiveMaxSegments    = 1 << 16
	candleArchiveTimestampOpen  = "open"

	candleArchiveDefaultMaxBytes = int64(512 << 20)
	candleArchiveMinMaxBytes     = int64(1 << 20)
	candleArchiveCapMaxBytes     = int64(16 << 30)
)

var (
	candleArchiveGenerationPattern = regexp.MustCompile(`^g-([0-9]{1,16})-([0-9a-f]{16})$`)
	candleArchiveSeriesFilePattern = regexp.MustCompile(`^series/[A-Za-z0-9][A-Za-z0-9._-]{0,127}\.bars$`)
	candleArchiveHexPattern        = regexp.MustCompile(`^[0-9a-f]{64}$`)
	errCandleArchiveIntegrity      = errors.New("candle archive integrity check failed")
	errCandleArchiveInterrupted    = errors.New("candle archive validation stopped before it finished")
)

type candleArchivePublisher struct {
	ID       string `json:"id"`
	Tool     string `json:"tool"`
	Version  string `json:"version"`
	EgressID string `json:"egress_id"`
}

type candleArchiveFinalization struct {
	MinCloseAgeMs int64 `json:"min_close_age_ms"`
}

type candleArchiveSegment struct {
	FirstOpenMs int64 `json:"first_open_ms"`
	LastOpenMs  int64 `json:"last_open_ms"`
	Bars        int64 `json:"bars"`
}

type candleArchiveSource struct {
	Kind         string `json:"kind"`
	Detail       string `json:"detail"`
	ObservedAtMs int64  `json:"observed_at_ms"`
}

type candleArchiveSeriesManifest struct {
	Venue                  string                 `json:"venue"`
	Network                string                 `json:"network"`
	InstrumentClass        string                 `json:"instrument_class"`
	Symbol                 string                 `json:"symbol"`
	Timeframe              string                 `json:"timeframe"`
	IntervalMs             int64                  `json:"interval_ms"`
	Timestamp              string                 `json:"timestamp"`
	File                   string                 `json:"file"`
	Bars                   int64                  `json:"bars"`
	Bytes                  int64                  `json:"bytes"`
	SHA256                 string                 `json:"sha256"`
	ChunkBars              int64                  `json:"chunk_bars"`
	ChunkSHA256            []string               `json:"chunk_sha256"`
	Segments               []candleArchiveSegment `json:"segments"`
	FinalizedThroughOpenMs int64                  `json:"finalized_through_open_ms"`
	HistoryStartKnown      bool                   `json:"history_start_known"`
	Source                 candleArchiveSource    `json:"source"`
}

type candleArchiveManifest struct {
	Format        string                        `json:"format"`
	PublishedAtMs int64                         `json:"published_at_ms"`
	ExpiresAtMs   int64                         `json:"expires_at_ms"`
	Publisher     candleArchivePublisher        `json:"publisher"`
	Finalization  candleArchiveFinalization     `json:"finalization"`
	Series        []candleArchiveSeriesManifest `json:"series"`
}

type candleArchiveLimits struct {
	MaxBytes int64
	Deadline time.Duration
}

type candleArchiveSegmentIndex struct {
	FirstOpen int64
	LastOpen  int64
	Bars      int64
	Start     int64
}

type candleArchiveSeries struct {
	Identity          candleIdentity
	IntervalSec       int64
	Bars              int64
	ChunkBars         int64
	ChunkSHA          [][32]byte
	Segments          []candleArchiveSegmentIndex
	FinalizedThrough  int64
	HistoryStartKnown bool
	SourceKind        string
	SourceDetail      string
	File              string
	handle            *os.File
}

type candleArchiveGeneration struct {
	ID          string
	Dir         string
	PublishedAt time.Time
	ExpiresAt   time.Time
	Publisher   candleArchivePublisher
	MinCloseAge time.Duration
	Bytes       int64
	series      map[candleIdentity]*candleArchiveSeries

	mu      sync.Mutex
	refs    int
	retired bool
	closed  bool
}

type candleArchiveRefusal struct {
	Series string
	Msg    string
}

func (e *candleArchiveRefusal) Error() string {
	if e.Series == "" {
		return e.Msg
	}
	return "series " + e.Series + ": " + e.Msg
}

func archiveRefuse(series, format string, args ...interface{}) error {
	return &candleArchiveRefusal{Series: series, Msg: fmt.Sprintf(format, args...)}
}

func candleArchiveGenerationID(publishedAtMs int64, manifest []byte) string {
	sum := sha256.Sum256(manifest)
	return "g-" + strconv.FormatInt(publishedAtMs, 10) + "-" + hex.EncodeToString(sum[:8])
}

func openArchiveRegular(path string) (*os.File, os.FileInfo, error) {
	lst, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if lst.Mode()&os.ModeSymlink != 0 {
		return nil, nil, fmt.Errorf("%s is a symbolic link", path)
	}
	if !lst.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("%s is not a regular file", path)
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	if !st.Mode().IsRegular() || !os.SameFile(st, lst) {
		f.Close()
		return nil, nil, fmt.Errorf("%s changed while it was opened", path)
	}
	return f, st, nil
}

func checkArchiveDir(path string) error {
	lst, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if lst.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symbolic link", path)
	}
	if !lst.IsDir() {
		return fmt.Errorf("%s is not a directory", path)
	}
	return nil
}

func readArchiveSmallFile(path string, max int64) ([]byte, error) {
	f, st, err := openArchiveRegular(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if st.Size() > max {
		return nil, fmt.Errorf("%s is %d bytes, above the %d-byte limit", path, st.Size(), max)
	}
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("%s is above the %d-byte limit", path, max)
	}
	return data, nil
}

func readCandleArchiveCurrent(dir string) (string, error) {
	if err := checkArchiveDir(dir); err != nil {
		return "", err
	}
	raw, err := readArchiveSmallFile(filepath.Join(dir, candleArchiveCurrentName), candleArchiveMaxCurrent)
	if err != nil {
		return "", err
	}
	id := strings.TrimSuffix(string(raw), "\n")
	if !candleArchiveGenerationPattern.MatchString(id) {
		return "", fmt.Errorf("%s does not name a generation id", candleArchiveCurrentName)
	}
	return id, nil
}

func decodeCandleArchiveManifest(raw []byte) (candleArchiveManifest, error) {
	var m candleArchiveManifest
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return m, archiveRefuse("", "manifest does not decode: %v", err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return m, archiveRefuse("", "manifest has trailing data")
	}
	return m, nil
}

func candleArchiveSeriesLabel(s candleArchiveSeriesManifest) string {
	return candleIdentity{Venue: s.Venue, Network: s.Network, InstrumentClass: s.InstrumentClass, Symbol: s.Symbol, Timeframe: s.Timeframe}.String()
}

func loadCandleArchiveGeneration(ctx context.Context, dir, id string, limits candleArchiveLimits, now time.Time) (*candleArchiveGeneration, error) {
	m := candleArchiveGenerationPattern.FindStringSubmatch(id)
	if m == nil {
		return nil, archiveRefuse("", "generation id %q is malformed", id)
	}
	if limits.MaxBytes <= 0 {
		limits.MaxBytes = candleArchiveDefaultMaxBytes
	}
	if limits.Deadline > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, limits.Deadline)
		defer cancel()
	}
	genRoot := filepath.Join(dir, candleArchiveGenerationsDir)
	if err := checkArchiveDir(genRoot); err != nil {
		return nil, archiveRefuse("", "%v", err)
	}
	genDir := filepath.Join(genRoot, id)
	if err := checkArchiveDir(genDir); err != nil {
		return nil, archiveRefuse("", "%v", err)
	}
	raw, err := readArchiveSmallFile(filepath.Join(genDir, candleArchiveManifestName), candleArchiveMaxManifest)
	if err != nil {
		return nil, archiveRefuse("", "%v", err)
	}
	manifest, err := decodeCandleArchiveManifest(raw)
	if err != nil {
		return nil, err
	}
	if manifest.Format != candleArchiveFormat {
		return nil, archiveRefuse("", "format %q is not %q", manifest.Format, candleArchiveFormat)
	}
	if got := candleArchiveGenerationID(manifest.PublishedAtMs, raw); got != id {
		return nil, archiveRefuse("", "generation id %s does not match its manifest (%s)", id, got)
	}
	if manifest.PublishedAtMs <= 0 || manifest.PublishedAtMs > now.UTC().UnixMilli() {
		return nil, archiveRefuse("", "published_at_ms %d is not a past instant", manifest.PublishedAtMs)
	}
	if manifest.ExpiresAtMs <= manifest.PublishedAtMs {
		return nil, archiveRefuse("", "expires_at_ms %d is not after published_at_ms", manifest.ExpiresAtMs)
	}
	if manifest.Finalization.MinCloseAgeMs < 0 {
		return nil, archiveRefuse("", "finalization.min_close_age_ms is negative")
	}
	for name, v := range map[string]string{"id": manifest.Publisher.ID, "tool": manifest.Publisher.Tool, "version": manifest.Publisher.Version, "egress_id": manifest.Publisher.EgressID} {
		if strings.TrimSpace(v) == "" {
			return nil, archiveRefuse("", "publisher.%s is empty", name)
		}
	}
	if len(manifest.Series) == 0 {
		return nil, archiveRefuse("", "the manifest holds no series")
	}
	if len(manifest.Series) > candleArchiveMaxSeries {
		return nil, archiveRefuse("", "%d series is above the %d-series limit", len(manifest.Series), candleArchiveMaxSeries)
	}
	gen := &candleArchiveGeneration{
		ID:          id,
		Dir:         genDir,
		PublishedAt: time.UnixMilli(manifest.PublishedAtMs).UTC(),
		ExpiresAt:   time.UnixMilli(manifest.ExpiresAtMs).UTC(),
		Publisher:   manifest.Publisher,
		MinCloseAge: time.Duration(manifest.Finalization.MinCloseAgeMs) * time.Millisecond,
		series:      make(map[candleIdentity]*candleArchiveSeries, len(manifest.Series)),
	}
	files := make(map[string]bool, len(manifest.Series))
	ok := false
	defer func() {
		if !ok {
			gen.closeFiles()
		}
	}()
	for _, sm := range manifest.Series {
		label := candleArchiveSeriesLabel(sm)
		if files[sm.File] {
			return nil, archiveRefuse(label, "file %q is named by two series", sm.File)
		}
		files[sm.File] = true
		gen.Bytes += sm.Bytes
		if sm.Bytes < 0 || gen.Bytes > limits.MaxBytes {
			return nil, archiveRefuse(label, "the generation exceeds the %d-byte limit", limits.MaxBytes)
		}
		s, err := admitCandleArchiveSeries(ctx, genDir, sm, manifest)
		if err != nil {
			return nil, err
		}
		if _, dup := gen.series[s.Identity]; dup {
			s.handle.Close()
			return nil, archiveRefuse(label, "the identity appears twice")
		}
		gen.series[s.Identity] = s
	}
	ok = true
	return gen, nil
}

func admitCandleArchiveSeries(ctx context.Context, genDir string, sm candleArchiveSeriesManifest, manifest candleArchiveManifest) (*candleArchiveSeries, error) {
	label := candleArchiveSeriesLabel(sm)
	id := candleIdentity{Venue: sm.Venue, Network: sm.Network, InstrumentClass: sm.InstrumentClass, Symbol: sm.Symbol, Timeframe: sm.Timeframe}
	if !id.valid() {
		return nil, archiveRefuse(label, "identity fields must be non-empty canonical text")
	}
	if strings.ToLower(id.Venue) != id.Venue || strings.ToLower(id.Network) != id.Network || strings.ToLower(id.InstrumentClass) != id.InstrumentClass {
		return nil, archiveRefuse(label, "venue, network and instrument_class must be lower case")
	}
	switch id.Venue {
	case candleVenueOKX, candleVenueRobinhood, candleVenueTopStep:
		return nil, archiveRefuse(label, "venue %s has no verified opening-time range read; format v1 refuses it", id.Venue)
	}
	sec, known := candleGridInterval(id.Timeframe)
	if !known {
		return nil, archiveRefuse(label, "timeframe %q has no fixed bar grid", id.Timeframe)
	}
	if sm.IntervalMs != sec*1000 {
		return nil, archiveRefuse(label, "interval_ms %d does not match timeframe %s (%d)", sm.IntervalMs, id.Timeframe, sec*1000)
	}
	if sm.Timestamp != candleArchiveTimestampOpen {
		return nil, archiveRefuse(label, "timestamp %q is not %q", sm.Timestamp, candleArchiveTimestampOpen)
	}
	if !candleArchiveSeriesFilePattern.MatchString(sm.File) {
		return nil, archiveRefuse(label, "file %q is not a confined series/<name>.bars path", sm.File)
	}
	if sm.Bars <= 0 {
		return nil, archiveRefuse(label, "the series holds no bars")
	}
	if sm.Bytes != sm.Bars*candleArchiveRecordBytes {
		return nil, archiveRefuse(label, "bytes %d is not bars * %d", sm.Bytes, candleArchiveRecordBytes)
	}
	if sm.ChunkBars <= 0 || sm.ChunkBars > candleArchiveMaxChunkBars {
		return nil, archiveRefuse(label, "chunk_bars %d is outside 1..%d", sm.ChunkBars, candleArchiveMaxChunkBars)
	}
	chunks := (sm.Bars + sm.ChunkBars - 1) / sm.ChunkBars
	if int64(len(sm.ChunkSHA256)) != chunks {
		return nil, archiveRefuse(label, "%d chunk digests, want %d", len(sm.ChunkSHA256), chunks)
	}
	if !candleArchiveHexPattern.MatchString(sm.SHA256) {
		return nil, archiveRefuse(label, "sha256 is not 64 lower-case hex digits")
	}
	chunkSHA := make([][32]byte, chunks)
	for i, h := range sm.ChunkSHA256 {
		if !candleArchiveHexPattern.MatchString(h) {
			return nil, archiveRefuse(label, "chunk digest %d is not 64 lower-case hex digits", i)
		}
		b, _ := hex.DecodeString(h)
		copy(chunkSHA[i][:], b)
	}
	if len(sm.Segments) == 0 || len(sm.Segments) > candleArchiveMaxSegments {
		return nil, archiveRefuse(label, "%d segments is outside 1..%d", len(sm.Segments), candleArchiveMaxSegments)
	}
	if sm.Source.Kind == "" {
		return nil, archiveRefuse(label, "source.kind is empty")
	}
	if sm.Source.ObservedAtMs <= 0 {
		return nil, archiveRefuse(label, "source.observed_at_ms %d is not a positive venue read time", sm.Source.ObservedAtMs)
	}
	path := filepath.Join(genDir, filepath.FromSlash(sm.File))
	if err := checkArchiveDir(filepath.Dir(path)); err != nil {
		return nil, archiveRefuse(label, "%v", err)
	}
	f, st, err := openArchiveRegular(path)
	if err != nil {
		return nil, archiveRefuse(label, "%v", err)
	}
	keep := false
	defer func() {
		if !keep {
			f.Close()
		}
	}()
	if st.Size() != sm.Bytes {
		return nil, archiveRefuse(label, "file size %d does not equal bytes %d", st.Size(), sm.Bytes)
	}
	intervalMs := sm.IntervalMs
	aligned := candleEpochAligned(id.Timeframe)
	fileHash := sha256.New()
	buf := make([]byte, sm.ChunkBars*candleArchiveRecordBytes)
	var prev int64
	var runs []candleArchiveSegmentIndex
	var bar int64
	for chunk := int64(0); chunk < chunks; chunk++ {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("%w: series %s: %v", errCandleArchiveInterrupted, label, err)
		}
		n := sm.ChunkBars
		if rest := sm.Bars - chunk*sm.ChunkBars; rest < n {
			n = rest
		}
		part := buf[:n*candleArchiveRecordBytes]
		if _, err := f.ReadAt(part, chunk*sm.ChunkBars*candleArchiveRecordBytes); err != nil {
			return nil, archiveRefuse(label, "read chunk %d: %v", chunk, err)
		}
		if sha256.Sum256(part) != chunkSHA[chunk] {
			return nil, archiveRefuse(label, "chunk %d digest mismatch", chunk)
		}
		fileHash.Write(part)
		for i := int64(0); i < n; i++ {
			rec := part[i*candleArchiveRecordBytes : (i+1)*candleArchiveRecordBytes]
			c, openMs := decodeCandleArchiveRecord(rec)
			if openMs%1000 != 0 || openMs < 0 {
				return nil, archiveRefuse(label, "bar %d opening time %d is not a whole second", bar, openMs)
			}
			if aligned && openMs%intervalMs != 0 {
				return nil, archiveRefuse(label, "bar %d opening time %d is off the %s grid", bar, openMs, id.Timeframe)
			}
			if !tradeReviewCandleValid(c) {
				return nil, archiveRefuse(label, "bar %d at %d has invalid OHLC or volume", bar, openMs)
			}
			switch {
			case bar == 0:
				runs = append(runs, candleArchiveSegmentIndex{FirstOpen: openMs, LastOpen: openMs, Bars: 1, Start: 0})
			case openMs <= prev:
				return nil, archiveRefuse(label, "bar %d opening time %d is not after %d", bar, openMs, prev)
			case (openMs-prev)%intervalMs != 0:
				return nil, archiveRefuse(label, "bar %d opening time %d is off the %s grid", bar, openMs, id.Timeframe)
			case openMs-prev == intervalMs:
				r := &runs[len(runs)-1]
				r.LastOpen = openMs
				r.Bars++
			default:
				runs = append(runs, candleArchiveSegmentIndex{FirstOpen: openMs, LastOpen: openMs, Bars: 1, Start: bar})
			}
			prev = openMs
			bar++
		}
	}
	if hex.EncodeToString(fileHash.Sum(nil)) != sm.SHA256 {
		return nil, archiveRefuse(label, "file digest mismatch")
	}
	if len(runs) != len(sm.Segments) {
		return nil, archiveRefuse(label, "%d declared segments, the bars form %d", len(sm.Segments), len(runs))
	}
	for i, r := range runs {
		d := sm.Segments[i]
		if d.FirstOpenMs != r.FirstOpen || d.LastOpenMs != r.LastOpen || d.Bars != r.Bars {
			return nil, archiveRefuse(label, "segment %d is declared as %d..%d (%d bars), the bars hold %d..%d (%d bars)",
				i, d.FirstOpenMs, d.LastOpenMs, d.Bars, r.FirstOpen, r.LastOpen, r.Bars)
		}
	}
	if sm.FinalizedThroughOpenMs != prev {
		return nil, archiveRefuse(label, "finalized_through_open_ms %d is not the last bar %d", sm.FinalizedThroughOpenMs, prev)
	}
	if prev+intervalMs+manifest.Finalization.MinCloseAgeMs > manifest.PublishedAtMs {
		return nil, archiveRefuse(label, "the last bar %d had not closed for the finalization delay at publication", prev)
	}
	if prev+intervalMs+manifest.Finalization.MinCloseAgeMs > sm.Source.ObservedAtMs {
		return nil, archiveRefuse(label, "the last bar %d had not closed for the finalization delay when its rows were read at %d", prev, sm.Source.ObservedAtMs)
	}
	segs := make([]candleArchiveSegmentIndex, len(runs))
	for i, r := range runs {
		segs[i] = candleArchiveSegmentIndex{FirstOpen: r.FirstOpen / 1000, LastOpen: r.LastOpen / 1000, Bars: r.Bars, Start: r.Start}
	}
	keep = true
	return &candleArchiveSeries{
		Identity:          id,
		IntervalSec:       intervalMs / 1000,
		Bars:              sm.Bars,
		ChunkBars:         sm.ChunkBars,
		ChunkSHA:          chunkSHA,
		Segments:          segs,
		FinalizedThrough:  prev / 1000,
		HistoryStartKnown: sm.HistoryStartKnown,
		SourceKind:        sm.Source.Kind,
		SourceDetail:      sm.Source.Detail,
		File:              sm.File,
		handle:            f,
	}, nil
}

func decodeCandleArchiveRecord(rec []byte) (UICandle, int64) {
	openMs := int64(binary.LittleEndian.Uint64(rec[0:8]))
	f := func(off int) float64 { return math.Float64frombits(binary.LittleEndian.Uint64(rec[off : off+8])) }
	return UICandle{Time: openMs / 1000, Open: f(8), High: f(16), Low: f(24), Close: f(32), Volume: f(40)}, openMs
}

func (g *candleArchiveGeneration) acquire() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return false
	}
	g.refs++
	return true
}

func (g *candleArchiveGeneration) release() {
	g.mu.Lock()
	g.refs--
	done := g.retired && g.refs <= 0 && !g.closed
	if done {
		g.closed = true
	}
	g.mu.Unlock()
	if done {
		g.closeFiles()
	}
}

func (g *candleArchiveGeneration) retire() {
	g.mu.Lock()
	g.retired = true
	done := g.refs <= 0 && !g.closed
	if done {
		g.closed = true
	}
	g.mu.Unlock()
	if done {
		g.closeFiles()
	}
}

func (g *candleArchiveGeneration) closeFiles() {
	for _, s := range g.series {
		if s.handle != nil {
			s.handle.Close()
		}
	}
}

func (g *candleArchiveGeneration) expired(now time.Time) bool {
	return !now.UTC().Before(g.ExpiresAt)
}

func (g *candleArchiveGeneration) lookup(id candleIdentity) *candleArchiveSeries {
	if g == nil {
		return nil
	}
	return g.series[id]
}

func (g *candleArchiveGeneration) timeframes(series candleIdentity) []string {
	if g == nil {
		return nil
	}
	var out []string
	for id := range g.series {
		if id.series() == series {
			out = append(out, id.Timeframe)
		}
	}
	sortCandleTimeframes(out)
	return out
}

func (g *candleArchiveGeneration) identities() []candleIdentity {
	out := make([]candleIdentity, 0, len(g.series))
	for id := range g.series {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

func (s *candleArchiveSeries) firstOpen() int64 { return s.Segments[0].FirstOpen }

func (s *candleArchiveSeries) lastOpen() int64 { return s.Segments[len(s.Segments)-1].LastOpen }

func (s *candleArchiveSeries) segmentAt(t int64) (int, bool) {
	i := sort.Search(len(s.Segments), func(i int) bool { return s.Segments[i].LastOpen >= t })
	if i < len(s.Segments) && s.Segments[i].FirstOpen <= t {
		return i, true
	}
	return i, false
}

func (s *candleArchiveSeries) indexAtOrAfter(t int64) int64 {
	i := sort.Search(len(s.Segments), func(i int) bool { return s.Segments[i].LastOpen >= t })
	if i >= len(s.Segments) {
		return s.Bars
	}
	seg := s.Segments[i]
	if t <= seg.FirstOpen {
		return seg.Start
	}
	off := (t - seg.FirstOpen + s.IntervalSec - 1) / s.IntervalSec
	return seg.Start + off
}

func (s *candleArchiveSeries) indexAtOrBefore(t int64) int64 {
	i := sort.Search(len(s.Segments), func(i int) bool { return s.Segments[i].FirstOpen > t })
	if i == 0 {
		return -1
	}
	seg := s.Segments[i-1]
	if t >= seg.LastOpen {
		return seg.Start + seg.Bars - 1
	}
	return seg.Start + (t-seg.FirstOpen)/s.IntervalSec
}

func (s *candleArchiveSeries) segmentOfIndex(idx int64) int {
	i := sort.Search(len(s.Segments), func(i int) bool { return s.Segments[i].Start+s.Segments[i].Bars > idx })
	return i
}

func (s *candleArchiveSeries) readRange(ctx context.Context, lo, hi int64) ([]UICandle, error) {
	if lo < 0 {
		lo = 0
	}
	if hi > s.Bars {
		hi = s.Bars
	}
	if hi <= lo {
		return []UICandle{}, nil
	}
	out := make([]UICandle, 0, hi-lo)
	buf := make([]byte, s.ChunkBars*candleArchiveRecordBytes)
	for chunk := lo / s.ChunkBars; chunk*s.ChunkBars < hi; chunk++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n := s.ChunkBars
		if rest := s.Bars - chunk*s.ChunkBars; rest < n {
			n = rest
		}
		part := buf[:n*candleArchiveRecordBytes]
		if _, err := s.handle.ReadAt(part, chunk*s.ChunkBars*candleArchiveRecordBytes); err != nil {
			return nil, fmt.Errorf("%w: read chunk %d of %s: %v", errCandleArchiveIntegrity, chunk, s.Identity, err)
		}
		if sha256.Sum256(part) != s.ChunkSHA[chunk] {
			return nil, fmt.Errorf("%w: chunk %d of %s changed after validation", errCandleArchiveIntegrity, chunk, s.Identity)
		}
		for i := int64(0); i < n; i++ {
			idx := chunk*s.ChunkBars + i
			if idx < lo || idx >= hi {
				continue
			}
			c, _ := decodeCandleArchiveRecord(part[i*candleArchiveRecordBytes : (i+1)*candleArchiveRecordBytes])
			out = append(out, c)
		}
	}
	return out, nil
}
