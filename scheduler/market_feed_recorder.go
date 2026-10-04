package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	observationRecordingSchema = "go_trader_observation_recording/v1"
	observationSegmentSchema   = "go_trader_observation_segment/v1"
	observationRunSchema       = "go_trader_observation_run/v1"
	observationRecorderQueue   = 65536
	observationFlushEvery      = time.Second
	observationDefaultSegment  = time.Hour
	observationRecorderWindow  = 2 * time.Hour
	observationHealthEvery     = time.Minute
)

type observationRecord struct {
	Kind      string
	Coin      string
	ObsKind   string
	RecvMs    int64
	Session   uint64
	Seq       uint64
	Value     float64
	Accepted  bool
	Refreshed bool
	Reason    string
	Raw       string
	State     string
}

func (r observationRecord) fields() map[string]any {
	out := map[string]any{"k": r.Kind, "recv_ms": r.RecvMs, "session": r.Session}
	switch r.Kind {
	case "obs":
		out["coin"] = r.Coin
		out["kind"] = r.ObsKind
		out["raw"] = r.Raw
		out["accepted"] = r.Accepted
		if r.Accepted {
			out["value"] = r.Value
			out["seq"] = r.Seq
			out["refreshed"] = r.Refreshed
			out["event_ms"] = nil
		} else {
			out["reason"] = r.Reason
		}
	case "conn":
		out["state"] = r.State
	}
	return out
}

type observationRecorderMeta struct {
	RunID     string
	Coins     []string
	Segment   time.Duration
	WSURL     string
	StartedAt time.Time
}

type observationSegmentManifest struct {
	Schema       string   `json:"schema"`
	RunID        string   `json:"run_id"`
	Index        int      `json:"index"`
	File         string   `json:"file"`
	Sha256       string   `json:"sha256"`
	Bytes        int64    `json:"bytes"`
	Records      int      `json:"records"`
	ObsRecords   int      `json:"obs_records"`
	Rejected     int      `json:"rejected"`
	Dropped      uint64   `json:"dropped"`
	FirstRecvMs  int64    `json:"first_recv_ms"`
	LastRecvMs   int64    `json:"last_recv_ms"`
	OpenedMs     int64    `json:"opened_ms"`
	ClosedMs     int64    `json:"closed_ms"`
	Sessions     []uint64 `json:"sessions"`
	Coins        []string `json:"coins"`
	Complete     bool     `json:"complete"`
	CloseReason  string   `json:"close_reason"`
	RecorderVers string   `json:"recorder_version"`
}

type observationRunManifest struct {
	Schema       string                       `json:"schema"`
	RunID        string                       `json:"run_id"`
	Source       string                       `json:"source"`
	Units        string                       `json:"units"`
	TimeBasis    string                       `json:"time_basis"`
	CadenceMs    int64                        `json:"cadence_ms"`
	Host         string                       `json:"host"`
	WSURL        string                       `json:"ws_url"`
	Coins        []string                     `json:"coins"`
	StartedMs    int64                        `json:"started_ms"`
	ClosedMs     int64                        `json:"closed_ms,omitempty"`
	Closed       bool                         `json:"closed"`
	DroppedTotal uint64                       `json:"dropped_total"`
	RecorderVers string                       `json:"recorder_version"`
	Segments     []observationSegmentManifest `json:"segments"`
}

type observationSegment struct {
	index    int
	name     string
	path     string
	file     *os.File
	buf      *bufio.Writer
	hash     io.Writer
	digest   interface{ Sum([]byte) []byte }
	bytes    int64
	records  int
	obs      int
	rejected int
	dropped  uint64
	first    int64
	last     int64
	opened   int64
	sessions map[uint64]bool
}

type observationRecorder struct {
	dir   string
	meta  observationRecorderMeta
	logf  func(string, ...any)
	clock func() time.Time

	queue chan observationRecord
	done  chan struct{}

	dropMu     sync.Mutex
	dropped    uint64
	dropFirst  int64
	dropLast   int64
	dropTotal  uint64
	closeOnce  sync.Once
	closeErr   error
	segment    *observationSegment
	segIndex   int
	manifest   observationRunManifest
	writeError error
}

func newObservationRecorder(dir string, meta observationRecorderMeta, clock func() time.Time, logf func(string, ...any)) (*observationRecorder, error) {
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if meta.Segment <= 0 {
		meta.Segment = observationDefaultSegment
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create recording directory %s: %w", dir, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "run.manifest.json")); err == nil {
		return nil, fmt.Errorf("%s already holds a recording run; choose an empty directory", dir)
	}
	coins := append([]string(nil), meta.Coins...)
	sort.Strings(coins)
	meta.Coins = coins
	r := &observationRecorder{
		dir:   dir,
		meta:  meta,
		logf:  logf,
		clock: clock,
		queue: make(chan observationRecord, observationRecorderQueue),
		done:  make(chan struct{}),
		manifest: observationRunManifest{
			Schema:       observationRunSchema,
			RunID:        meta.RunID,
			Source:       feedObservationSourceHLWS,
			Units:        feedObservationUnitsBase,
			TimeBasis:    feedObservationTimeReceipt,
			CadenceMs:    feedObservationCadenceMs,
			Host:         hlMainnetURL,
			WSURL:        meta.WSURL,
			Coins:        coins,
			StartedMs:    meta.StartedAt.UTC().UnixMilli(),
			RecorderVers: Version,
			Segments:     []observationSegmentManifest{},
		},
	}
	if err := r.writeRunManifest(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *observationRecorder) offer(rec observationRecord) {
	select {
	case r.queue <- rec:
	default:
		r.dropMu.Lock()
		if r.dropped == 0 {
			r.dropFirst = rec.RecvMs
		}
		r.dropped++
		r.dropTotal++
		r.dropLast = rec.RecvMs
		r.dropMu.Unlock()
	}
}

func (r *observationRecorder) takeDrops() (uint64, int64, int64) {
	r.dropMu.Lock()
	defer r.dropMu.Unlock()
	n, first, last := r.dropped, r.dropFirst, r.dropLast
	r.dropped, r.dropFirst, r.dropLast = 0, 0, 0
	return n, first, last
}

func (r *observationRecorder) run(ctx context.Context) {
	defer close(r.done)
	ticker := time.NewTicker(observationFlushEvery)
	defer ticker.Stop()
	for {
		select {
		case rec := <-r.queue:
			r.handle(rec)
		case <-ticker.C:
			r.flushDrops(r.clock().UnixMilli())
			if r.segment != nil && r.writeError == nil {
				if err := r.segment.buf.Flush(); err != nil {
					r.fail(err)
				}
			}
		case <-ctx.Done():
		drain:
			for {
				select {
				case rec := <-r.queue:
					r.handle(rec)
				default:
					break drain
				}
			}
			r.flushDrops(r.clock().UnixMilli())
			r.closeSegment("shutdown")
			r.manifest.Closed = true
			r.manifest.ClosedMs = r.clock().UnixMilli()
			r.manifest.DroppedTotal = r.totalDropped()
			if err := r.writeRunManifest(); err != nil {
				r.fail(err)
			}
			return
		}
	}
}

func (r *observationRecorder) totalDropped() uint64 {
	r.dropMu.Lock()
	defer r.dropMu.Unlock()
	return r.dropTotal
}

func (r *observationRecorder) fail(err error) {
	if r.writeError == nil {
		r.writeError = err
		r.logf("[recorder] CRITICAL: write failed, recording stops and the run is marked incomplete: %v", err)
	}
}

func (r *observationRecorder) flushDrops(nowMs int64) {
	n, first, last := r.takeDrops()
	if n == 0 {
		return
	}
	seg := r.segmentFor(nowMs)
	if seg == nil {
		return
	}
	seg.dropped += n
	r.writeLine(seg, map[string]any{"k": "drop", "count": n, "from_ms": first, "to_ms": last, "recv_ms": nowMs})
	r.logf("[recorder] queue overflow: %d record(s) dropped between %d and %d; the gap is marked in the recording", n, first, last)
}

func (r *observationRecorder) handle(rec observationRecord) {
	r.flushDrops(rec.RecvMs)
	seg := r.segmentFor(rec.RecvMs)
	if seg == nil {
		return
	}
	if rec.Kind == "obs" {
		seg.obs++
		if !rec.Accepted {
			seg.rejected++
		}
	}
	if seg.first == 0 || rec.RecvMs < seg.first {
		seg.first = rec.RecvMs
	}
	if rec.RecvMs > seg.last {
		seg.last = rec.RecvMs
	}
	seg.sessions[rec.Session] = true
	r.writeLine(seg, rec.fields())
}

func (r *observationRecorder) segmentFor(recvMs int64) *observationSegment {
	if r.writeError != nil {
		return nil
	}
	if r.segment != nil && recvMs-r.segment.opened >= r.meta.Segment.Milliseconds() {
		r.closeSegment("rotation")
	}
	if r.segment == nil {
		if err := r.openSegment(recvMs); err != nil {
			r.fail(err)
			return nil
		}
	}
	return r.segment
}

func (r *observationRecorder) openSegment(atMs int64) error {
	r.segIndex++
	name := fmt.Sprintf("segment-%05d.jsonl", r.segIndex)
	path := filepath.Join(r.dir, name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open segment %s: %w", path, err)
	}
	h := sha256.New()
	seg := &observationSegment{
		index:    r.segIndex,
		name:     name,
		path:     path,
		file:     f,
		hash:     h,
		digest:   h,
		opened:   atMs,
		sessions: map[uint64]bool{},
	}
	seg.buf = bufio.NewWriter(io.MultiWriter(f, h))
	r.segment = seg
	r.writeLine(seg, map[string]any{
		"k": "header", "schema": observationRecordingSchema, "run_id": r.meta.RunID, "index": seg.index,
		"recv_ms": atMs, "source": feedObservationSourceHLWS, "units": feedObservationUnitsBase,
		"time_basis": feedObservationTimeReceipt, "cadence_ms": feedObservationCadenceMs, "host": hlMainnetURL,
		"ws_url": r.meta.WSURL, "coins": r.meta.Coins, "recorder_version": Version,
	})
	return r.writeError
}

func (r *observationRecorder) writeLine(seg *observationSegment, fields map[string]any) {
	if r.writeError != nil {
		return
	}
	blob, err := json.Marshal(fields)
	if err != nil {
		r.fail(err)
		return
	}
	blob = append(blob, '\n')
	n, err := seg.buf.Write(blob)
	seg.bytes += int64(n)
	if err != nil {
		r.fail(err)
		return
	}
	if fields["k"] != "header" && fields["k"] != "end" {
		seg.records++
	}
}

func (r *observationRecorder) closeSegment(reason string) {
	seg := r.segment
	if seg == nil {
		return
	}
	r.segment = nil
	closedMs := r.clock().UnixMilli()
	r.writeLine(seg, map[string]any{"k": "end", "recv_ms": closedMs, "records": seg.records, "dropped": seg.dropped, "reason": reason})
	err := seg.buf.Flush()
	if err == nil {
		err = seg.file.Sync()
	}
	if cerr := seg.file.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		r.fail(fmt.Errorf("close segment %s: %w", seg.path, err))
		return
	}
	sessions := make([]uint64, 0, len(seg.sessions))
	for s := range seg.sessions {
		sessions = append(sessions, s)
	}
	sort.Slice(sessions, func(i, j int) bool { return sessions[i] < sessions[j] })
	m := observationSegmentManifest{
		Schema:       observationSegmentSchema,
		RunID:        r.meta.RunID,
		Index:        seg.index,
		File:         seg.name,
		Sha256:       hex.EncodeToString(seg.digest.Sum(nil)),
		Bytes:        seg.bytes,
		Records:      seg.records,
		ObsRecords:   seg.obs,
		Rejected:     seg.rejected,
		Dropped:      seg.dropped,
		FirstRecvMs:  seg.first,
		LastRecvMs:   seg.last,
		OpenedMs:     seg.opened,
		ClosedMs:     closedMs,
		Sessions:     sessions,
		Coins:        r.meta.Coins,
		Complete:     seg.dropped == 0 && r.writeError == nil,
		CloseReason:  reason,
		RecorderVers: Version,
	}
	if err := writeJSONAtomic(filepath.Join(r.dir, seg.name+".manifest.json"), m); err != nil {
		r.fail(err)
		return
	}
	r.manifest.Segments = append(r.manifest.Segments, m)
	r.manifest.DroppedTotal = r.totalDropped()
	if err := r.writeRunManifest(); err != nil {
		r.fail(err)
	}
}

func (r *observationRecorder) writeRunManifest() error {
	return writeJSONAtomic(filepath.Join(r.dir, "run.manifest.json"), r.manifest)
}

func writeJSONAtomic(path string, v any) error {
	blob, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	blob = append(blob, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, blob, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (r *observationRecorder) wait() error {
	<-r.done
	return r.writeError
}

func (o *marketFeedOwner) attachRecorder(r *observationRecorder) {
	o.feedMu.Lock()
	defer o.feedMu.Unlock()
	o.recorder = r
}

func (o *marketFeedOwner) trackObservations(coins []string, windowMs int64) {
	o.feedMu.Lock()
	defer o.feedMu.Unlock()
	needs := make(map[feedObservationKey]int64, len(coins))
	for _, c := range coins {
		needs[openInterestKeyFor(c)] = windowMs
	}
	o.applyObservationNeedsLocked(needs)
	o.subVersion++
}

func parseRecorderCoins(raw string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		c := strings.TrimSpace(part)
		if c == "" {
			continue
		}
		if strings.ContainsAny(c, " \t/|") {
			return nil, fmt.Errorf("coin %q is not a Hyperliquid perpetual coin name", c)
		}
		if seen[c] {
			return nil, fmt.Errorf("coin %q is listed twice", c)
		}
		seen[c] = true
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil, errors.New("--coins lists no coin")
	}
	sort.Strings(out)
	return out, nil
}

func runRecordObservations(args []string) int {
	fs := flag.NewFlagSet("record-observations", flag.ContinueOnError)
	coinsFlag := fs.String("coins", "", "Comma-separated Hyperliquid perpetual coins to record open interest for (e.g. BTC,ETH)")
	outDir := fs.String("out-dir", "", "Empty directory for the research recording (never a state or config directory)")
	duration := fs.Duration("duration", 0, "Stop after this long (0 = until SIGINT or SIGTERM)")
	segment := fs.Duration("segment", observationDefaultSegment, "Rotate to a new hashed segment after this long")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "record-observations: unexpected arguments %v\n", fs.Args())
		return 2
	}
	coins, err := parseRecorderCoins(*coinsFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "record-observations: %v\n", err)
		return 2
	}
	if strings.TrimSpace(*outDir) == "" {
		fmt.Fprintln(os.Stderr, "record-observations: --out-dir is required")
		return 2
	}
	if *segment < time.Minute {
		fmt.Fprintln(os.Stderr, "record-observations: --segment must be at least 1m")
		return 2
	}
	startedAt := time.Now().UTC()
	runID := fmt.Sprintf("%s-%d", strings.ReplaceAll(newFeedInstanceID(startedAt), " ", "_"), startedAt.Unix())
	rec, err := newObservationRecorder(*outDir, observationRecorderMeta{
		RunID: runID, Coins: coins, Segment: *segment, WSURL: hlFeedWebsocketURL(), StartedAt: startedAt,
	}, nil, feedLogf)
	if err != nil {
		fmt.Fprintf(os.Stderr, "record-observations: %v\n", err)
		return 1
	}
	fmt.Printf("[recorder] run=%s coins=%s out=%s ws=%s source=%s units=%s time_basis=%s cadence_ms=%d segment=%s duration=%s\n",
		runID, strings.Join(coins, ","), *outDir, hlFeedWebsocketURL(), feedObservationSourceHLWS, feedObservationUnitsBase,
		feedObservationTimeReceipt, feedObservationCadenceMs, *segment, *duration)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if *duration > 0 {
		var stop context.CancelFunc
		ctx, stop = context.WithTimeout(ctx, *duration)
		defer stop()
	}
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	go func() {
		select {
		case sig := <-sigCh:
			fmt.Printf("[recorder] received %s; sealing the open segment\n", sig)
			cancel()
		case <-ctx.Done():
		}
	}()

	owner := newMarketFeedOwner(nil, feedLogf)
	owner.trackObservations(coins, observationRecorderWindow.Milliseconds())
	owner.attachRecorder(rec)
	recCtx, stopRec := context.WithCancel(context.Background())
	go rec.run(recCtx)
	ownerDone := make(chan struct{})
	go func() {
		defer close(ownerDone)
		owner.Run(ctx)
	}()
	ticker := time.NewTicker(observationHealthEvery)
	defer ticker.Stop()
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case <-ticker.C:
			for _, rd := range owner.ObservationReadiness() {
				fmt.Printf("[recorder] %s status=%s samples=%d received=%d accepted=%d refreshed=%d rejected=%d covered_ms=%d detail=%q\n",
					rd.Key.PayloadID(), rd.Status, rd.Samples, rd.Stats.Received, rd.Stats.Accepted, rd.Stats.Refreshed,
					rd.Stats.Rejected, rd.CoveredMs, rd.Detail)
			}
		}
	}
	<-ownerDone
	owner.attachRecorder(nil)
	stopRec()
	if err := rec.wait(); err != nil {
		fmt.Fprintf(os.Stderr, "[recorder] CRITICAL: %v\n", err)
		return 1
	}
	fmt.Printf("[recorder] closed run=%s segments=%d dropped=%d dir=%s\n", runID, len(rec.manifest.Segments), rec.manifest.DroppedTotal, *outDir)
	return 0
}
