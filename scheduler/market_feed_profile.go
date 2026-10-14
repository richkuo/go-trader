package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

const (
	feedObservationVolumeProfile = "volume_profile"
	feedProfileSchema            = "go_trader_volume_profile/v1"
	feedProfileSessionMs         = int64(86_400_000)
	feedProfileMinutes           = int(feedProfileSessionMs / feedTradeIntervalMs)
	feedProfileWindowMs          = 2 * feedProfileSessionMs
	feedProfileTradeWindowMs     = feedTradeIntervalMs
	feedProfileMaxEntries        = 16_384
	feedProfileMaxPriceChars     = 24
	feedProfileMaxPairBytes      = 1 << 20
	feedProfileMaxCoins          = 16
	feedProfileMaxReasonChars    = 240
	feedProfileSealOverheadBytes = 4096
	feedProfileDualWindowMs      = int64(3_600_000)
	feedProfileMaxSealSessions   = 2

	feedProfileStatusAvailable   = "available"
	feedProfileStatusInvalidated = "invalidated"
	feedProfileStatusPending     = "pending"
	feedProfileStatusUnavailable = "unavailable"

	feedProfileCodeNotFinal      = "session_not_final"
	feedProfileCodeNotTracked    = "not_tracked"
	feedProfileCodeMinute        = "minute_invalid"
	feedProfileCodeRefinalized   = "minute_refinalized"
	feedProfileCodeLate          = "late_event"
	feedProfileCodeConflict      = "conflicting_duplicate"
	feedProfileCodeEntries       = "price_entries_over_limit"
	feedProfileCodeFieldLength   = "price_field_length"
	feedProfileCodeBytes         = "profile_bytes_over_limit"
	feedProfileCodeOverflow      = "volume_overflow"
	feedProfileCodeZeroVolume    = "zero_volume"
	feedProfileCodeZeroRange     = "zero_price_range"
	feedProfileCodeSealMalformed = "seal_malformed"
	feedProfileCodeAttached      = "profile_attached_mid_session"

	feedProfileDayAccumulating = "accumulating"
	feedProfileDayFinal        = "final"
	feedProfileDayUnavailable  = "unavailable"

	volumeProfileReclaimStrategyName = "volume_profile_reclaim"
)

func volumeProfileKeyFor(coin string) feedObservationKey {
	return feedObservationKey{Host: hlMainnetURL, Namespace: feedNamespacePerps, Coin: coin, Kind: feedObservationVolumeProfile}
}

func feedStrategyUsesVolumeProfile(sc StrategyConfig) bool {
	return strings.TrimSpace(effectiveOpenStrategy(sc)) == volumeProfileReclaimStrategyName
}

func feedProfileSessionStart(ms int64) int64 {
	return ms - ((ms%feedProfileSessionMs)+feedProfileSessionMs)%feedProfileSessionMs
}

func feedProfileTargetSession(cutoffMs int64) int64 {
	return feedProfileSessionStart(cutoffMs-1) - feedProfileSessionMs
}

func feedProfileSealSessions(cutoffMs int64) []int64 {
	target := feedProfileTargetSession(cutoffMs)
	if cutoffMs-feedProfileSessionStart(cutoffMs-1) < feedProfileDualWindowMs {
		return []int64{target - feedProfileSessionMs, target}
	}
	return []int64{target}
}

func boundedProfileReason(s string) string {
	if len(s) <= feedProfileMaxReasonChars {
		return s
	}
	return s[:feedProfileMaxReasonChars]
}

func compareCanonicalDecimal(a, b string) int {
	ai, af, _ := strings.Cut(a, ".")
	bi, bf, _ := strings.Cut(b, ".")
	if len(ai) != len(bi) {
		if len(ai) < len(bi) {
			return -1
		}
		return 1
	}
	if c := strings.Compare(ai, bi); c != 0 {
		return c
	}
	n := len(af)
	if len(bf) > n {
		n = len(bf)
	}
	af += strings.Repeat("0", n-len(af))
	bf += strings.Repeat("0", n-len(bf))
	return strings.Compare(af, bf)
}

type feedProfilePair struct {
	Px    string
	Units int64
}

type feedProfileArtifact struct {
	Coin            string
	SessionStartMs  int64
	SessionEndMs    int64
	Pairs           []feedProfilePair
	Trades          int64
	Units           int64
	TradesHash      string
	FirstEventMs    int64
	LastEventMs     int64
	CompleteMinutes int
	AvailableMs     int64
	SHA256          string
}

func feedProfileCanonicalText(coin string, sessionStartMs, sessionEndMs, trades int64, volume, tradesHash string,
	firstEventMs, lastEventMs int64, prices [][2]string) string {
	var b strings.Builder
	b.WriteString(feedProfileSchema)
	b.WriteString("\ncoin=")
	b.WriteString(coin)
	b.WriteString("\nsession_start_ms=")
	b.WriteString(strconv.FormatInt(sessionStartMs, 10))
	b.WriteString("\nsession_end_ms=")
	b.WriteString(strconv.FormatInt(sessionEndMs, 10))
	b.WriteString("\ntrades=")
	b.WriteString(strconv.FormatInt(trades, 10))
	b.WriteString("\nvolume=")
	b.WriteString(volume)
	b.WriteString("\ntrades_hash=")
	b.WriteString(tradesHash)
	b.WriteString("\nfirst_event_ms=")
	b.WriteString(strconv.FormatInt(firstEventMs, 10))
	b.WriteString("\nlast_event_ms=")
	b.WriteString(strconv.FormatInt(lastEventMs, 10))
	b.WriteString("\n")
	for _, p := range prices {
		b.WriteString(p[0])
		b.WriteString(" ")
		b.WriteString(p[1])
		b.WriteString("\n")
	}
	return b.String()
}

func feedProfileHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

func feedProfilePairBytes(px, sz string) int {
	return len(px) + len(sz) + 8
}

func (a *feedProfileArtifact) prices() [][2]string {
	out := make([][2]string, 0, len(a.Pairs))
	for _, p := range a.Pairs {
		out = append(out, [2]string{p.Px, formatFeedUnits(p.Units)})
	}
	return out
}

type feedProfileDay struct {
	StartMs         int64
	Status          string
	Code            string
	Detail          string
	UnavailableMs   int64
	CompleteMinutes int
	seen            [feedProfileMinutes]bool

	prices       map[string]int64
	Trades       int64
	Units        int64
	Hash         [32]byte
	FirstEventMs int64
	LastEventMs  int64

	Artifact    *feedProfileArtifact
	InvalidMs   int64
	InvalidCode string
	InvalidWhy  string
}

func (d *feedProfileDay) markUnavailable(atMs int64, code, detail string) {
	if d.Status != feedProfileDayAccumulating {
		return
	}
	d.Status = feedProfileDayUnavailable
	d.Code = code
	d.Detail = boundedProfileReason(detail)
	d.UnavailableMs = atMs
	d.prices = nil
}

type feedProfileStats struct {
	Accepted    uint64
	Finalized   uint64
	Unavailable uint64
	Invalidated uint64
}

type feedProfileState struct {
	Key         feedObservationKey
	Days        map[int64]*feedProfileDay
	Stats       feedProfileStats
	NotBeforeMs int64
}

func newFeedProfileState(key feedObservationKey) *feedProfileState {
	return &feedProfileState{Key: key, Days: make(map[int64]*feedProfileDay)}
}

func (p *feedProfileState) floorMs(nowMs int64) int64 {
	return feedProfileSessionStart(nowMs-feedProfileDualWindowMs) - feedProfileSessionMs
}

func (p *feedProfileState) day(startMs, nowMs int64, create bool) *feedProfileDay {
	day := feedProfileSessionStart(startMs)
	if d := p.Days[day]; d != nil {
		return d
	}
	if !create || day < p.floorMs(nowMs) {
		return nil
	}
	d := &feedProfileDay{StartMs: day, Status: feedProfileDayAccumulating, prices: make(map[string]int64)}
	p.Days[day] = d
	return d
}

func (p *feedProfileState) markUnavailable(d *feedProfileDay, atMs int64, code, detail string) {
	if d.Status != feedProfileDayAccumulating {
		return
	}
	d.markUnavailable(atMs, code, detail)
	p.Stats.Unavailable++
}

func (p *feedProfileState) addTrade(t feedTrade, digest [32]byte, startMs, recvMs int64) {
	d := p.day(startMs, recvMs, true)
	if d == nil || d.Status != feedProfileDayAccumulating {
		return
	}
	p.Stats.Accepted++
	if len(t.Px) > feedProfileMaxPriceChars {
		p.markUnavailable(d, recvMs, feedProfileCodeFieldLength, fmt.Sprintf("trade %d price %s is longer than %d characters", t.Tid, t.Px[:feedProfileMaxPriceChars], feedProfileMaxPriceChars))
		return
	}
	prior, exists := d.prices[t.Px]
	if !exists && len(d.prices) >= feedProfileMaxEntries {
		p.markUnavailable(d, recvMs, feedProfileCodeEntries, fmt.Sprintf("session holds more than %d distinct prices", feedProfileMaxEntries))
		return
	}
	sum, ok := addFeedUnits(prior, t.Units)
	total, okTotal := addFeedUnits(d.Units, t.Units)
	if !ok || !okTotal {
		p.markUnavailable(d, recvMs, feedProfileCodeOverflow, fmt.Sprintf("trade %d overflows the exact volume accumulator", t.Tid))
		return
	}
	d.prices[t.Px] = sum
	d.Units = total
	d.Trades++
	addFeedTradeHash(&d.Hash, digest)
	if d.FirstEventMs == 0 || t.TimeMs < d.FirstEventMs {
		d.FirstEventMs = t.TimeMs
	}
	if t.TimeMs > d.LastEventMs {
		d.LastEventMs = t.TimeMs
	}
}

func (p *feedProfileState) finalizeMinute(iv *feedTradeInterval, nowMs int64) {
	d := p.day(iv.StartMs, nowMs, true)
	if d == nil {
		return
	}
	idx := int((iv.StartMs - d.StartMs) / feedTradeIntervalMs)
	if idx < 0 || idx >= feedProfileMinutes {
		return
	}
	if d.seen[idx] {
		p.markUnavailable(d, nowMs, feedProfileCodeRefinalized, fmt.Sprintf("minute %d finalized twice", iv.StartMs))
		return
	}
	d.seen[idx] = true
	if iv.StartMs < p.NotBeforeMs {
		p.markUnavailable(d, nowMs, feedProfileCodeAttached, fmt.Sprintf("minute %d began before the profile was attached to an active trade stream", iv.StartMs))
		return
	}
	if iv.Status != feedTradeStatusComplete || iv.InvalidMs != 0 {
		p.markUnavailable(d, nowMs, feedProfileCodeMinute, fmt.Sprintf("minute %d is invalid: %s", iv.StartMs, iv.Reason))
		return
	}
	d.CompleteMinutes++
	if d.CompleteMinutes == feedProfileMinutes && d.Status == feedProfileDayAccumulating {
		p.finalizeDay(d, nowMs)
	}
}

func (p *feedProfileState) finalizeDay(d *feedProfileDay, nowMs int64) {
	if d.Units <= 0 || d.Trades <= 0 {
		p.markUnavailable(d, nowMs, feedProfileCodeZeroVolume, "the complete session recorded no accepted volume")
		return
	}
	if len(d.prices) < 2 {
		p.markUnavailable(d, nowMs, feedProfileCodeZeroRange, "the complete session traded at one price")
		return
	}
	pairs := make([]feedProfilePair, 0, len(d.prices))
	bytes := 0
	for px, units := range d.prices {
		pairs = append(pairs, feedProfilePair{Px: px, Units: units})
		bytes += feedProfilePairBytes(px, formatFeedUnits(units))
	}
	if bytes > feedProfileMaxPairBytes {
		p.markUnavailable(d, nowMs, feedProfileCodeBytes, fmt.Sprintf("the price pairs serialize to %d bytes, over the %d-byte cap", bytes, feedProfileMaxPairBytes))
		return
	}
	sort.Slice(pairs, func(i, j int) bool { return compareCanonicalDecimal(pairs[i].Px, pairs[j].Px) < 0 })
	art := &feedProfileArtifact{
		Coin: p.Key.Coin, SessionStartMs: d.StartMs, SessionEndMs: d.StartMs + feedProfileSessionMs, Pairs: pairs,
		Trades: d.Trades, Units: d.Units, TradesHash: hex.EncodeToString(d.Hash[:]), FirstEventMs: d.FirstEventMs,
		LastEventMs: d.LastEventMs, CompleteMinutes: d.CompleteMinutes, AvailableMs: nowMs,
	}
	art.SHA256 = feedProfileHash(feedProfileCanonicalText(art.Coin, art.SessionStartMs, art.SessionEndMs, art.Trades,
		formatFeedUnits(art.Units), art.TradesHash, art.FirstEventMs, art.LastEventMs, art.prices()))
	d.Artifact = art
	d.Status = feedProfileDayFinal
	d.prices = nil
	p.Stats.Finalized++
}

func (p *feedProfileState) invalidateMinute(startMs, atMs int64, code string) {
	d := p.Days[feedProfileSessionStart(startMs)]
	if d == nil {
		return
	}
	switch d.Status {
	case feedProfileDayAccumulating:
		p.markUnavailable(d, atMs, code, fmt.Sprintf("minute %d was revoked at %d: %s", startMs, atMs, code))
	case feedProfileDayFinal:
		if d.InvalidMs == 0 {
			d.InvalidMs = atMs
			d.InvalidCode = code
			d.InvalidWhy = boundedProfileReason(fmt.Sprintf("minute %d was revoked at %d after the session became final: %s", startMs, atMs, code))
			p.Stats.Invalidated++
		}
	}
}

func (p *feedProfileState) observeNewTrade(startMs, recvMs int64) {
	d := p.Days[feedProfileSessionStart(startMs)]
	if d == nil {
		return
	}
	idx := int((startMs - d.StartMs) / feedTradeIntervalMs)
	if idx < 0 || idx >= feedProfileMinutes || !d.seen[idx] {
		return
	}
	p.invalidateMinute(startMs, recvMs, feedProfileCodeLate)
}

func (p *feedProfileState) trim(nowMs int64) {
	floor := p.floorMs(nowMs)
	for start := range p.Days {
		if start < floor {
			delete(p.Days, start)
		}
	}
}

type feedProfileReadiness struct {
	Key            feedObservationKey
	Status         string
	Code           string
	Detail         string
	Ready          bool
	SessionStartMs int64
	Stats          feedProfileStats
}

func profileSessionAt(p *feedProfileState, target, cutoffMs int64) (string, string, string, *feedProfileDay) {
	if p == nil {
		return feedProfileStatusUnavailable, feedProfileCodeNotTracked, "this owner does not track the volume profile", nil
	}
	d := p.Days[target]
	switch {
	case d == nil:
		return feedProfileStatusPending, feedProfileCodeNotFinal, fmt.Sprintf("session %d has no finalized minute in this owner", target), nil
	case d.Status == feedProfileDayUnavailable && d.UnavailableMs <= cutoffMs:
		return feedProfileStatusUnavailable, d.Code, d.Detail, nil
	case d.Status == feedProfileDayFinal && d.Artifact != nil && d.Artifact.AvailableMs <= cutoffMs:
		if d.InvalidMs != 0 && d.InvalidMs <= cutoffMs {
			return feedProfileStatusInvalidated, d.InvalidCode, d.InvalidWhy, d
		}
		return feedProfileStatusAvailable, "", "", d
	}
	return feedProfileStatusPending, feedProfileCodeNotFinal, fmt.Sprintf("session %d is not final by %d (%d of %d minutes complete)", target, cutoffMs, d.CompleteMinutes, feedProfileMinutes), nil
}

func volumeProfileReadiness(p *feedProfileState, nowMs int64) feedProfileReadiness {
	target := feedProfileTargetSession(nowMs)
	status, code, detail, _ := profileSessionAt(p, target, nowMs)
	out := feedProfileReadiness{Key: p.Key, Status: status, Code: code, Detail: detail, SessionStartMs: target, Stats: p.Stats}
	out.Ready = status == feedProfileStatusAvailable
	return out
}

func (o *marketFeedOwner) profileStateLocked(coin string) *feedProfileState {
	st := o.tradeFlows[tradeFlowKeyFor(coin)]
	if st == nil {
		return nil
	}
	return st.profile
}

func (o *marketFeedOwner) VolumeProfileReadiness() []feedProfileReadiness {
	o.feedMu.Lock()
	defer o.feedMu.Unlock()
	nowMs := o.now().UnixMilli()
	var out []feedProfileReadiness
	for _, k := range sortedTradeFlowKeys(o.tradeFlows) {
		if p := o.tradeFlows[k].profile; p != nil {
			out = append(out, volumeProfileReadiness(p, nowMs))
		}
	}
	return out
}

func (o *marketFeedOwner) applyProfileNeedLocked(key feedObservationKey) {
	tk := tradeFlowKeyFor(key.Coin)
	st := o.tradeFlows[tk]
	if st == nil {
		st = newFeedTradeFlowState(tk, feedProfileTradeWindowMs)
		st.internal = true
		o.tradeFlows[tk] = st
	}
	if st.profile == nil {
		st.profile = newFeedProfileState(volumeProfileKeyFor(key.Coin))
		if st.CoverFromMs != 0 || len(st.Intervals) > 0 {
			notBefore := feedTradeIntervalStart(o.now().UTC().UnixMilli()) + feedTradeIntervalMs
			for _, iv := range st.Intervals {
				if next := iv.StartMs + feedTradeIntervalMs; next > notBefore {
					notBefore = next
				}
			}
			st.profile.NotBeforeMs = notBefore
		}
	}
}

func (o *marketFeedOwner) applyTradeFlowNeedLocked(key feedObservationKey, window int64) {
	if st := o.tradeFlows[key]; st != nil {
		st.raiseWindow(window)
		st.internal = false
		return
	}
	o.tradeFlows[key] = newFeedTradeFlowState(key, window)
}

func (o *marketFeedOwner) dropUnneededTradeStatesLocked(needs map[feedObservationKey]int64) {
	for key, st := range o.tradeFlows {
		_, flowNeeded := needs[key]
		_, profileNeeded := needs[volumeProfileKeyFor(key.Coin)]
		switch {
		case !flowNeeded && !profileNeeded:
			delete(o.tradeFlows, key)
		case !profileNeeded:
			st.profile = nil
		case !flowNeeded:
			st.internal = true
		}
	}
}

type feedProfileSessionSnapshot struct {
	SessionStartMs int64
	Status         string
	Code           string
	Reason         string
	InvalidMs      int64
	Artifact       *feedProfileArtifact
}

type marketSnapshotProfile struct {
	Key       feedObservationKey
	WindowMs  int64
	CutoffMs  int64
	Sessions  []feedProfileSessionSnapshot
	Readiness feedProfileReadiness
}

func freezeVolumeProfile(p *feedProfileState, key feedObservationKey, windowMs, cutoffMs, nowMs int64) *marketSnapshotProfile {
	out := &marketSnapshotProfile{Key: key, WindowMs: windowMs, CutoffMs: cutoffMs}
	for _, start := range feedProfileSealSessions(cutoffMs) {
		status, code, detail, d := profileSessionAt(p, start, cutoffMs)
		sess := feedProfileSessionSnapshot{SessionStartMs: start, Status: status, Code: code, Reason: detail}
		if d != nil {
			sess.Artifact = d.Artifact
			if status == feedProfileStatusInvalidated {
				sess.InvalidMs = d.InvalidMs
			}
		}
		out.Sessions = append(out.Sessions, sess)
	}
	if p != nil {
		out.Readiness = volumeProfileReadiness(p, cutoffMs)
	} else {
		last := out.Sessions[len(out.Sessions)-1]
		out.Readiness = feedProfileReadiness{Key: key, Status: last.Status, Code: last.Code, Detail: last.Reason, SessionStartMs: last.SessionStartMs}
	}
	return out
}

type marketVolumeProfileArtifact struct {
	SessionStartMs  int64       `json:"session_start_ms"`
	SessionEndMs    int64       `json:"session_end_ms"`
	Trades          int64       `json:"trades"`
	Volume          string      `json:"volume"`
	TradesHash      string      `json:"trades_hash"`
	FirstEventMs    int64       `json:"first_event_ms"`
	LastEventMs     int64       `json:"last_event_ms"`
	CompleteMinutes int         `json:"complete_minutes"`
	AvailableMs     int64       `json:"available_ms"`
	SHA256          string      `json:"sha256"`
	Prices          [][2]string `json:"prices"`
}

type marketVolumeProfileSession struct {
	SessionStartMs int64                        `json:"session_start_ms"`
	Status         string                       `json:"status"`
	ReasonCode     string                       `json:"reason_code"`
	Reason         string                       `json:"reason"`
	InvalidMs      *int64                       `json:"invalid_ms"`
	Artifact       *marketVolumeProfileArtifact `json:"artifact"`
}

type marketVolumeProfilePayload struct {
	Kind           string                       `json:"kind"`
	Schema         string                       `json:"schema"`
	Coin           string                       `json:"coin"`
	Source         string                       `json:"source"`
	Units          string                       `json:"units"`
	Identity       string                       `json:"identity"`
	TimeBasis      string                       `json:"time_basis"`
	SessionMs      int64                        `json:"session_ms"`
	IntervalMs     int64                        `json:"interval_ms"`
	ReorderGraceMs int64                        `json:"reorder_grace_ms"`
	DecimalScale   int                          `json:"decimal_scale"`
	MaxEntries     int                          `json:"max_entries"`
	MaxPriceChars  int                          `json:"max_price_chars"`
	MaxPairBytes   int                          `json:"max_pair_bytes"`
	WindowMs       int64                        `json:"window_ms"`
	CutoffMs       int64                        `json:"cutoff_ms"`
	BarIntervalMs  int64                        `json:"bar_interval_ms"`
	SnapshotID     string                       `json:"snapshot_id"`
	Available      bool                         `json:"available"`
	Reason         string                       `json:"reason,omitempty"`
	FeedStatus     string                       `json:"feed_status,omitempty"`
	Sessions       []marketVolumeProfileSession `json:"sessions"`
}

func marketProfileArtifact(a *feedProfileArtifact) *marketVolumeProfileArtifact {
	if a == nil {
		return nil
	}
	return &marketVolumeProfileArtifact{
		SessionStartMs: a.SessionStartMs, SessionEndMs: a.SessionEndMs, Trades: a.Trades, Volume: formatFeedUnits(a.Units),
		TradesHash: a.TradesHash, FirstEventMs: a.FirstEventMs, LastEventMs: a.LastEventMs, CompleteMinutes: a.CompleteMinutes,
		AvailableMs: a.AvailableMs, SHA256: a.SHA256, Prices: a.prices(),
	}
}

func newMarketVolumeProfilePayload(coin string, barIntervalMs int64) marketVolumeProfilePayload {
	return marketVolumeProfilePayload{
		Kind: feedObservationVolumeProfile, Schema: feedProfileSchema, Coin: coin, Source: feedTradeSourceHLWS, Units: feedTradeUnitsBase,
		Identity: feedTradeIdentity, TimeBasis: feedTradeTimeBasis, SessionMs: feedProfileSessionMs, IntervalMs: feedTradeIntervalMs,
		ReorderGraceMs: feedTradeReorderGraceMs, DecimalScale: feedTradeDecimalScale, MaxEntries: feedProfileMaxEntries,
		MaxPriceChars: feedProfileMaxPriceChars, MaxPairBytes: feedProfileMaxPairBytes, WindowMs: feedProfileWindowMs,
		BarIntervalMs: barIntervalMs, Sessions: []marketVolumeProfileSession{},
	}
}

func (s *marketSnapshot) volumeProfilePayload(key feedObservationKey, windowMs, barIntervalMs int64) marketVolumeProfilePayload {
	out := newMarketVolumeProfilePayload(key.Coin, barIntervalMs)
	if s == nil {
		out.Reason = "no sealed market snapshot"
		return out
	}
	out.SnapshotID = s.EvaluationID
	entry, ok := s.profiles[key]
	if !ok || entry == nil {
		out.Reason = fmt.Sprintf("%s is not in the sealed snapshot", key.PayloadID())
		out.CutoffMs = s.observationCutoff().UnixMilli()
		return out
	}
	out.CutoffMs = entry.CutoffMs
	out.FeedStatus = entry.Readiness.Status
	if entry.WindowMs < windowMs {
		out.Reason = fmt.Sprintf("%s window %dms in the seal is below the %dms this strategy needs", key.PayloadID(), entry.WindowMs, windowMs)
		return out
	}
	for _, es := range entry.Sessions {
		sess := marketVolumeProfileSession{SessionStartMs: es.SessionStartMs, Status: es.Status,
			ReasonCode: es.Code, Reason: es.Reason, Artifact: marketProfileArtifact(es.Artifact)}
		if es.InvalidMs != 0 {
			v := es.InvalidMs
			sess.InvalidMs = &v
		}
		out.Sessions = append(out.Sessions, sess)
	}
	out.Available = true
	return out
}

func attachVolumeProfilePayloads(payload *marketPayload, s *marketSnapshot, needs map[feedObservationKey]int64, barIntervalMs int64) {
	if payload == nil {
		return
	}
	var keys []feedObservationKey
	for k := range needs {
		if k.Kind == feedObservationVolumeProfile {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return
	}
	sortFeedObservationKeys(keys)
	payload.VolumeProfiles = make(map[string]marketVolumeProfilePayload, len(keys))
	for _, k := range keys {
		payload.VolumeProfiles[k.PayloadID()] = s.volumeProfilePayload(k, needs[k], barIntervalMs)
	}
}

type feedSealProfileReadiness struct {
	Status         string `json:"status"`
	Code           string `json:"code"`
	Detail         string `json:"detail"`
	Ready          bool   `json:"ready"`
	SessionStartMs int64  `json:"session_start_ms"`
	Accepted       uint64 `json:"accepted"`
	Finalized      uint64 `json:"finalized"`
	Unavailable    uint64 `json:"unavailable"`
	Invalidated    uint64 `json:"invalidated"`
}

type feedSealVolumeProfile struct {
	Host          string                   `json:"host"`
	Namespace     string                   `json:"namespace"`
	Coin          string                   `json:"coin"`
	Kind          string                   `json:"kind"`
	Schema        string                   `json:"schema"`
	Source        string                   `json:"source"`
	Units         string                   `json:"units"`
	Identity      string                   `json:"identity"`
	TimeBasis     string                   `json:"time_basis"`
	SessionMs     int64                    `json:"session_ms"`
	DecimalScale  int                      `json:"decimal_scale"`
	MaxEntries    int                      `json:"max_entries"`
	MaxPriceChars int                      `json:"max_price_chars"`
	MaxPairBytes  int                      `json:"max_pair_bytes"`
	WindowMs      int64                    `json:"window_ms"`
	CutoffMs      int64                    `json:"cutoff_ms"`
	Readiness     feedSealProfileReadiness `json:"readiness"`
	Sessions      []feedSealProfileSession `json:"sessions"`
}

type feedSealProfileSession struct {
	SessionStartMs int64                        `json:"session_start_ms"`
	Status         string                       `json:"status"`
	Code           string                       `json:"code"`
	Reason         string                       `json:"reason"`
	InvalidMs      int64                        `json:"invalid_ms"`
	Artifact       *marketVolumeProfileArtifact `json:"artifact"`
}

func sealVolumeProfiles(entries map[feedObservationKey]*marketSnapshotProfile) []feedSealVolumeProfile {
	keys := make([]feedObservationKey, 0, len(entries))
	for k, v := range entries {
		if v != nil {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return nil
	}
	sortFeedObservationKeys(keys)
	out := make([]feedSealVolumeProfile, 0, len(keys))
	for _, k := range keys {
		e := entries[k]
		r := e.Readiness
		out = append(out, feedSealVolumeProfile{
			Host: k.Host, Namespace: k.Namespace, Coin: k.Coin, Kind: k.Kind, Schema: feedProfileSchema,
			Source: feedTradeSourceHLWS, Units: feedTradeUnitsBase, Identity: feedTradeIdentity, TimeBasis: feedTradeTimeBasis,
			SessionMs: feedProfileSessionMs, DecimalScale: feedTradeDecimalScale, MaxEntries: feedProfileMaxEntries,
			MaxPriceChars: feedProfileMaxPriceChars, MaxPairBytes: feedProfileMaxPairBytes, WindowMs: e.WindowMs, CutoffMs: e.CutoffMs,
			Readiness: feedSealProfileReadiness{Status: r.Status, Code: r.Code, Detail: r.Detail, Ready: r.Ready, SessionStartMs: r.SessionStartMs,
				Accepted: r.Stats.Accepted, Finalized: r.Stats.Finalized, Unavailable: r.Stats.Unavailable, Invalidated: r.Stats.Invalidated},
			Sessions: sealProfileSessions(e.Sessions),
		})
	}
	return out
}

func sealProfileSessions(sessions []feedProfileSessionSnapshot) []feedSealProfileSession {
	out := make([]feedSealProfileSession, 0, len(sessions))
	for _, es := range sessions {
		out = append(out, feedSealProfileSession{SessionStartMs: es.SessionStartMs, Status: es.Status, Code: es.Code, Reason: es.Reason,
			InvalidMs: es.InvalidMs, Artifact: marketProfileArtifact(es.Artifact)})
	}
	return out
}

func validateFeedSealProfileIdentity(profiles []feedSealVolumeProfile) error {
	var prev *feedObservationKey
	for i := range profiles {
		f := profiles[i]
		key := feedObservationKey{Host: f.Host, Namespace: f.Namespace, Coin: f.Coin, Kind: f.Kind}
		if prev != nil && !feedObservationKeyLess(*prev, key) {
			return fmt.Errorf("seal volume profiles are not strictly ordered at %s", key)
		}
		prev = &key
		if f.Kind != feedObservationVolumeProfile {
			return fmt.Errorf("seal volume profile %s has kind %q", key, f.Kind)
		}
		if f.Schema != feedProfileSchema || f.Source != feedTradeSourceHLWS || f.Units != feedTradeUnitsBase || f.Identity != feedTradeIdentity ||
			f.TimeBasis != feedTradeTimeBasis || f.SessionMs != feedProfileSessionMs || f.DecimalScale != feedTradeDecimalScale ||
			f.MaxEntries != feedProfileMaxEntries || f.MaxPriceChars != feedProfileMaxPriceChars || f.MaxPairBytes != feedProfileMaxPairBytes {
			return fmt.Errorf("seal volume profile %s declares contract %q %q %q %q %q session %d scale %d limits %d/%d/%d; this consumer accepts only %q %q %q %q %q %d %d %d/%d/%d",
				key, f.Schema, f.Source, f.Units, f.Identity, f.TimeBasis, f.SessionMs, f.DecimalScale, f.MaxEntries, f.MaxPriceChars, f.MaxPairBytes,
				feedProfileSchema, feedTradeSourceHLWS, feedTradeUnitsBase, feedTradeIdentity, feedTradeTimeBasis, feedProfileSessionMs,
				feedTradeDecimalScale, feedProfileMaxEntries, feedProfileMaxPriceChars, feedProfileMaxPairBytes)
		}
		if f.WindowMs <= 0 || f.WindowMs > feedProfileWindowMs || f.CutoffMs <= 0 {
			return fmt.Errorf("seal volume profile %s has window %d, cutoff %d", key, f.WindowMs, f.CutoffMs)
		}
	}
	return nil
}

func profileUnits(text string) (int64, error) {
	canonical, err := canonicalFeedDecimal(text)
	if err != nil {
		return 0, err
	}
	if canonical != text {
		return 0, fmt.Errorf("%q is not canonical (%q)", text, canonical)
	}
	intPart, frac, _ := strings.Cut(canonical, ".")
	if len(frac) > feedTradeDecimalScale || len(intPart) > 12 {
		return 0, fmt.Errorf("%q does not fit the %d-decimal volume accumulator", text, feedTradeDecimalScale)
	}
	whole, err := strconv.ParseInt(intPart, 10, 64)
	if err != nil {
		return 0, err
	}
	part, err := strconv.ParseInt(frac+strings.Repeat("0", feedTradeDecimalScale-len(frac)), 10, 64)
	if err != nil {
		return 0, err
	}
	if whole > (math.MaxInt64-part)/feedTradeScale {
		return 0, fmt.Errorf("%q overflows the volume accumulator", text)
	}
	return whole*feedTradeScale + part, nil
}

func validateProfileArtifact(coin string, a *marketVolumeProfileArtifact, sessionStartMs, cutoffMs int64) error {
	if a.SessionStartMs != sessionStartMs || a.SessionEndMs != sessionStartMs+feedProfileSessionMs {
		return fmt.Errorf("artifact session [%d, %d) is not the target session %d", a.SessionStartMs, a.SessionEndMs, sessionStartMs)
	}
	if a.CompleteMinutes != feedProfileMinutes {
		return fmt.Errorf("artifact holds %d complete minutes, want %d", a.CompleteMinutes, feedProfileMinutes)
	}
	if a.AvailableMs < a.SessionEndMs+feedTradeReorderGraceMs || a.AvailableMs > cutoffMs {
		return fmt.Errorf("artifact became available at %d, outside [%d, %d]", a.AvailableMs, a.SessionEndMs+feedTradeReorderGraceMs, cutoffMs)
	}
	if a.FirstEventMs < a.SessionStartMs || a.LastEventMs >= a.SessionEndMs || a.FirstEventMs > a.LastEventMs {
		return fmt.Errorf("artifact event bounds [%d, %d] are outside its session", a.FirstEventMs, a.LastEventMs)
	}
	if hb, err := hex.DecodeString(a.TradesHash); err != nil || len(hb) != sha256.Size || hex.EncodeToString(hb) != a.TradesHash {
		return fmt.Errorf("artifact trades hash is malformed")
	}
	if len(a.Prices) < 2 || len(a.Prices) > feedProfileMaxEntries {
		return fmt.Errorf("artifact holds %d prices, want 2 to %d", len(a.Prices), feedProfileMaxEntries)
	}
	if a.Trades < int64(len(a.Prices)) {
		return fmt.Errorf("artifact counts %d trades for %d distinct prices", a.Trades, len(a.Prices))
	}
	volume, err := profileUnits(a.Volume)
	if err != nil || volume <= 0 {
		return fmt.Errorf("artifact volume %q is not a positive canonical decimal", a.Volume)
	}
	var sum int64
	bytes := 0
	for i, pair := range a.Prices {
		px, sz := pair[0], pair[1]
		canonical, err := canonicalFeedDecimal(px)
		if err != nil || canonical != px || len(px) > feedProfileMaxPriceChars || px == "0" {
			return fmt.Errorf("artifact price %d %q is not a positive canonical decimal of at most %d characters", i, px, feedProfileMaxPriceChars)
		}
		if i > 0 && compareCanonicalDecimal(a.Prices[i-1][0], px) >= 0 {
			return fmt.Errorf("artifact prices are not strictly increasing at %d", i)
		}
		units, err := profileUnits(sz)
		if err != nil || units <= 0 {
			return fmt.Errorf("artifact size %d %q is not a positive canonical decimal", i, sz)
		}
		var ok bool
		if sum, ok = addFeedUnits(sum, units); !ok {
			return fmt.Errorf("artifact sizes overflow")
		}
		bytes += feedProfilePairBytes(px, sz)
	}
	if sum != volume {
		return fmt.Errorf("artifact sizes sum to %s, volume is %s", formatFeedUnits(sum), a.Volume)
	}
	if bytes > feedProfileMaxPairBytes {
		return fmt.Errorf("artifact pairs serialize to %d bytes, over the %d-byte cap", bytes, feedProfileMaxPairBytes)
	}
	want := feedProfileHash(feedProfileCanonicalText(coin, a.SessionStartMs, a.SessionEndMs, a.Trades, a.Volume, a.TradesHash,
		a.FirstEventMs, a.LastEventMs, a.Prices))
	if want != a.SHA256 {
		return fmt.Errorf("artifact sha256 %s does not match its content (%s)", a.SHA256, want)
	}
	return nil
}

func validateFeedSealProfileContent(coin string, cutoffMs int64, f feedSealProfileSession) error {
	if len(f.Reason) > feedProfileMaxReasonChars || len(f.Code) > 64 {
		return fmt.Errorf("diagnostic text is over its length cap")
	}
	switch f.Status {
	case feedProfileStatusAvailable:
		if f.Artifact == nil || f.Code != "" || f.Reason != "" || f.InvalidMs != 0 {
			return fmt.Errorf("an available session needs an artifact and no reason")
		}
	case feedProfileStatusInvalidated:
		if f.Artifact == nil || f.Code == "" || f.InvalidMs <= 0 || f.InvalidMs > cutoffMs {
			return fmt.Errorf("an invalidated session needs an artifact and a dated reason by the cutoff")
		}
		if f.InvalidMs < f.Artifact.AvailableMs {
			return fmt.Errorf("the session was invalidated at %d before it became available at %d", f.InvalidMs, f.Artifact.AvailableMs)
		}
	case feedProfileStatusPending, feedProfileStatusUnavailable:
		if f.Artifact != nil || f.Code == "" || f.InvalidMs != 0 {
			return fmt.Errorf("a %s session carries an artifact or no reason", f.Status)
		}
		return nil
	default:
		return fmt.Errorf("status %q is unknown", f.Status)
	}
	return validateProfileArtifact(coin, f.Artifact, f.SessionStartMs, cutoffMs)
}

func validateFeedSealProfileSessions(f feedSealVolumeProfile) error {
	if len(f.Readiness.Detail) > feedProfileMaxReasonChars || len(f.Readiness.Code) > 64 {
		return fmt.Errorf("readiness text is over its length cap")
	}
	want := feedProfileSealSessions(f.CutoffMs)
	if len(f.Sessions) != len(want) {
		return fmt.Errorf("carries %d sessions, the cutoff %d seals %d", len(f.Sessions), f.CutoffMs, len(want))
	}
	for i, sess := range f.Sessions {
		if sess.SessionStartMs != want[i] {
			return fmt.Errorf("session %d starts at %d, the cutoff %d seals %d", i, sess.SessionStartMs, f.CutoffMs, want[i])
		}
		if err := validateFeedSealProfileContent(f.Coin, f.CutoffMs, sess); err != nil {
			return fmt.Errorf("session %d: %v", sess.SessionStartMs, err)
		}
	}
	return nil
}

func snapshotVolumeProfiles(profiles []feedSealVolumeProfile) map[feedObservationKey]*marketSnapshotProfile {
	out := make(map[feedObservationKey]*marketSnapshotProfile, len(profiles))
	for _, f := range profiles {
		key := feedObservationKey{Host: f.Host, Namespace: f.Namespace, Coin: f.Coin, Kind: f.Kind}
		r := f.Readiness
		entry := &marketSnapshotProfile{Key: key, WindowMs: f.WindowMs, CutoffMs: f.CutoffMs,
			Readiness: feedProfileReadiness{Key: key, Status: r.Status, Code: r.Code, Detail: r.Detail, Ready: r.Ready, SessionStartMs: r.SessionStartMs,
				Stats: feedProfileStats{Accepted: r.Accepted, Finalized: r.Finalized, Unavailable: r.Unavailable, Invalidated: r.Invalidated}}}
		if err := validateFeedSealProfileSessions(f); err != nil {
			for _, start := range feedProfileSealSessions(f.CutoffMs) {
				entry.Sessions = append(entry.Sessions, feedProfileSessionSnapshot{SessionStartMs: start, Status: feedProfileStatusUnavailable,
					Code: feedProfileCodeSealMalformed, Reason: boundedProfileReason("the sealed volume profile is malformed and is not used: " + err.Error())})
			}
			entry.Readiness.Detail = boundedProfileReason(entry.Readiness.Detail)
			out[key] = entry
			continue
		}
		for _, sess := range f.Sessions {
			entry.Sessions = append(entry.Sessions, snapshotProfileSession(f.Coin, sess))
		}
		out[key] = entry
	}
	return out
}

func snapshotProfileSession(coin string, f feedSealProfileSession) feedProfileSessionSnapshot {
	out := feedProfileSessionSnapshot{SessionStartMs: f.SessionStartMs, Status: f.Status, Code: f.Code, Reason: f.Reason, InvalidMs: f.InvalidMs}
	a := f.Artifact
	if a == nil {
		return out
	}
	art := &feedProfileArtifact{Coin: coin, SessionStartMs: a.SessionStartMs, SessionEndMs: a.SessionEndMs, Trades: a.Trades,
		TradesHash: a.TradesHash, FirstEventMs: a.FirstEventMs, LastEventMs: a.LastEventMs, CompleteMinutes: a.CompleteMinutes,
		AvailableMs: a.AvailableMs, SHA256: a.SHA256, Pairs: make([]feedProfilePair, 0, len(a.Prices))}
	art.Units, _ = profileUnits(a.Volume)
	for _, pair := range a.Prices {
		units, _ := profileUnits(pair[1])
		art.Pairs = append(art.Pairs, feedProfilePair{Px: pair[0], Units: units})
	}
	out.Artifact = art
	return out
}

func volumeProfileCoinCount(r feedRequirements) int {
	n := 0
	for k := range r.Observations {
		if k.Kind == feedObservationVolumeProfile {
			n++
		}
	}
	return n
}

func volumeProfileUnionError(r feedRequirements) error {
	if n := volumeProfileCoinCount(r); n > feedProfileMaxCoins {
		return fmt.Errorf("the requirements need volume profiles for %d coins, over the %d-coin budget (%d pair bytes per coin)", n, feedProfileMaxCoins, feedProfileMaxPairBytes)
	}
	return nil
}

func estimateVolumeProfileSealBytes(union feedRequirements) int {
	total := 0
	for k := range union.Observations {
		if k.Kind == feedObservationVolumeProfile {
			total += feedEstimateKeyBytes + feedProfileMaxSealSessions*(feedProfileSealOverheadBytes+feedProfileMaxPairBytes)
		}
	}
	return total
}

func volumeProfileStrategyIDs(cfg *Config) []string {
	var ids []string
	for _, sc := range cfg.Strategies {
		if feedScopedStrategy(sc) && feedStrategyUsesVolumeProfile(sc) {
			ids = append(ids, sc.ID)
		}
	}
	sort.Strings(ids)
	return ids
}
