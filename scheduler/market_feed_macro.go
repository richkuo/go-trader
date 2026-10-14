package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	macroEventBreakoutStrategyName = "macro_event_breakout"
	macroCalendarSchema            = "macro_event_calendar/v1"
	macroQuoteSchema               = "macro_event_quotes/v1"
	macroImporter                  = "macro_event_inputs/1654-v1"
	macroKindCalendar              = "macro_calendar"
	macroKindQuotes                = "macro_quotes"
	macroVenue                     = "hyperliquid"
	macroEventTypeFOMC             = "us_fomc_rate_statement"
	macroEventTypeControl          = "research_control_us_fomc_rate_statement"
	macroStatusScheduled           = "scheduled"
	macroArtifactMaxBytes          = 64 << 20
	macroDecimalScale              = 12
	macroMaxSupportRows            = 200
	macroQuoteMaxSealRecords       = 20_000
	feedEstimateMacroRecordBytes   = 360
	macroCalendarSealAllowance     = 2_000
)

var (
	macroArtifactKindEventTypes = map[string][]string{
		"official":         {macroEventTypeFOMC},
		"research_control": {macroEventTypeControl},
		"fixture":          {macroEventTypeFOMC, macroEventTypeControl},
	}
	macroQuoteKinds     = map[string]bool{"fixture": true, "official": true, "recorded": true}
	macroStatuses       = map[string]bool{macroStatusScheduled: true, "postponed": true, "cancelled": true}
	macroHex64          = regexp.MustCompile(`^[0-9a-f]{64}$`)
	macroEventIDPattern = regexp.MustCompile(`^[A-Za-z0-9_./:-]{1,128}$`)
	macroLocalPattern   = regexp.MustCompile(`^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})$`)
	macroCoinPattern    = regexp.MustCompile(`^[A-Za-z0-9]{1,16}$`)
)

var macroParamDefaults = map[string]int64{
	"expected_interval_ms": 3_600_000,
	"range_bars":           6,
	"post_window_bars":     6,
	"cooldown_bars":        24,
	"max_quote_age_ms":     60_000,
}

type feedMacroKey struct {
	Kind      string
	Host      string
	Namespace string
	Coin      string
	SHA256    string
}

func (k feedMacroKey) String() string {
	return k.Kind + "|" + k.Host + "|" + k.Namespace + "|" + k.Coin + "|" + k.SHA256
}

func (k feedMacroKey) PayloadID() string {
	if k.Kind == macroKindQuotes {
		return k.Coin + "|" + k.SHA256
	}
	return k.SHA256
}

func feedMacroKeyLess(a, b feedMacroKey) bool {
	if a.Kind != b.Kind {
		return a.Kind < b.Kind
	}
	if a.Host != b.Host {
		return a.Host < b.Host
	}
	if a.Namespace != b.Namespace {
		return a.Namespace < b.Namespace
	}
	if a.Coin != b.Coin {
		return a.Coin < b.Coin
	}
	return a.SHA256 < b.SHA256
}

func sortFeedMacroKeys(keys []feedMacroKey) {
	sort.Slice(keys, func(i, j int) bool { return feedMacroKeyLess(keys[i], keys[j]) })
}

func macroCalendarKeyFor(sha string) feedMacroKey {
	return feedMacroKey{Kind: macroKindCalendar, SHA256: sha}
}

func macroQuoteKeyFor(coin, sha string) feedMacroKey {
	return feedMacroKey{Kind: macroKindQuotes, Host: hlMainnetURL, Namespace: feedNamespacePerps, Coin: coin, SHA256: sha}
}

type feedMacroNeed struct {
	Path     string
	Paths    []string
	WindowMs int64
	Err      string
}

type feedMacroIdentity struct {
	CalendarPath string
	CalendarSHA  string
	QuotePath    string
	QuoteSHA     string
}

type macroStrategyParams struct {
	feedMacroIdentity
	IntervalMs    int64
	RangeBars     int64
	PostBars      int64
	CooldownBars  int64
	MaxQuoteAgeMs int64
}

func feedStrategyUsesMacroEvents(sc StrategyConfig) bool {
	return strings.TrimSpace(effectiveOpenStrategy(sc)) == macroEventBreakoutStrategyName
}

func macroStringParam(params map[string]interface{}, name string) (string, error) {
	raw, ok := params[name]
	if !ok || raw == nil {
		return "", nil
	}
	s, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("%s must be a string, got %v", name, raw)
	}
	return s, nil
}

func macroIntParam(params map[string]interface{}, name string, def int64, allowZero bool) (int64, error) {
	raw, ok := params[name]
	if !ok || raw == nil {
		return def, nil
	}
	if f, isNum := raw.(float64); isNum && f == 0 && allowZero {
		return 0, nil
	}
	if i, isInt := raw.(int); isInt && i == 0 && allowZero {
		return 0, nil
	}
	return openInterestParamInt(params, name, def)
}

func macroIdentityFor(sc StrategyConfig) (feedMacroIdentity, error) {
	var id feedMacroIdentity
	params := sc.OpenStrategy.Params
	var err error
	if id.CalendarPath, err = macroStringParam(params, "calendar_path"); err != nil {
		return id, err
	}
	if id.CalendarSHA, err = macroStringParam(params, "calendar_sha256"); err != nil {
		return id, err
	}
	if id.QuotePath, err = macroStringParam(params, "quote_path"); err != nil {
		return id, err
	}
	if id.QuoteSHA, err = macroStringParam(params, "quote_sha256"); err != nil {
		return id, err
	}
	return id, nil
}

func macroParamsFor(sc StrategyConfig) (macroStrategyParams, error) {
	var p macroStrategyParams
	id, err := macroIdentityFor(sc)
	if err != nil {
		return p, err
	}
	p.feedMacroIdentity = id
	vals := make(map[string]int64, len(macroParamDefaults))
	for name, def := range macroParamDefaults {
		v, err := macroIntParam(sc.OpenStrategy.Params, name, def, name == "cooldown_bars")
		if err != nil {
			return p, err
		}
		vals[name] = v
	}
	p.IntervalMs, p.RangeBars, p.PostBars = vals["expected_interval_ms"], vals["range_bars"], vals["post_window_bars"]
	p.CooldownBars, p.MaxQuoteAgeMs = vals["cooldown_bars"], vals["max_quote_age_ms"]
	if rows := p.CooldownBars + p.PostBars + p.RangeBars + 1; rows > macroMaxSupportRows {
		return p, fmt.Errorf("cooldown_bars + post_window_bars + range_bars + 1 = %d rows exceeds the %d-row check window", rows, macroMaxSupportRows)
	}
	if p.MaxQuoteAgeMs > p.IntervalMs {
		return p, fmt.Errorf("max_quote_age_ms %d is longer than one %d ms bar", p.MaxQuoteAgeMs, p.IntervalMs)
	}
	for _, pair := range [][3]string{{"calendar", p.CalendarPath, p.CalendarSHA}, {"quote", p.QuotePath, p.QuoteSHA}} {
		label, path, sha := pair[0], pair[1], pair[2]
		if (path == "") != (sha == "") {
			return p, fmt.Errorf("%s_path and %s_sha256 must both be set or both be empty", label, label)
		}
		if path == "" {
			continue
		}
		if !filepath.IsAbs(path) {
			return p, fmt.Errorf("%s_path %q must be absolute; the feed owner never resolves a relative artifact path", label, path)
		}
		if !macroHex64.MatchString(sha) {
			return p, fmt.Errorf("%s_sha256 %q must be a lowercase 64-hex sha256", label, sha)
		}
	}
	if p.CalendarPath == "" {
		return p, errors.New("no calendar artifact is configured (calendar_path, calendar_sha256)")
	}
	return p, nil
}

func (p macroStrategyParams) quoteWindowMs() int64 {
	return (p.CooldownBars+p.PostBars+p.RangeBars+2)*p.IntervalMs + p.MaxQuoteAgeMs
}

func (e feedStrategyRequirement) macroNeeds() map[feedMacroKey]int64 {
	if !e.MacroEvents || e.MacroErr != "" {
		return nil
	}
	out := map[feedMacroKey]int64{e.MacroCalendar: 0}
	if e.MacroHasQuotes {
		out[e.MacroQuotes] = e.MacroQuoteWindowMs
	}
	return out
}

func (r *feedRequirements) addMacro(key feedMacroKey, path string, windowMs int64) {
	if r.Macro == nil {
		r.Macro = make(map[feedMacroKey]feedMacroNeed)
	}
	need, ok := r.Macro[key]
	if !ok {
		r.Macro[key] = feedMacroNeed{Path: path, Paths: []string{path}, WindowMs: windowMs}
		return
	}
	if path < need.Path {
		need.Path = path
	}
	seen := false
	for _, p := range need.Paths {
		seen = seen || p == path
	}
	if !seen {
		need.Paths = append(need.Paths, path)
		sort.Strings(need.Paths)
	}
	if windowMs > need.WindowMs {
		need.WindowMs = windowMs
	}
	r.Macro[key] = need
}

func (r feedRequirements) macroKeys() []feedMacroKey {
	out := make([]feedMacroKey, 0, len(r.Macro))
	for k := range r.Macro {
		out = append(out, k)
	}
	sortFeedMacroKeys(out)
	return out
}

func deriveMacroRequirement(sc StrategyConfig, symbol string, entry *feedStrategyRequirement, req *feedRequirements) {
	entry.MacroEvents = true
	p, err := macroParamsFor(sc)
	if err != nil {
		entry.MacroErr = fmt.Sprintf("%s parameters rejected: %v", macroEventBreakoutStrategyName, err)
		return
	}
	entry.MacroCalendar = macroCalendarKeyFor(p.CalendarSHA)
	entry.MacroCalendarPath = p.CalendarPath
	req.addMacro(entry.MacroCalendar, p.CalendarPath, 0)
	if p.QuotePath != "" {
		entry.MacroHasQuotes = true
		entry.MacroQuotePath = p.QuotePath
		entry.MacroQuotes = macroQuoteKeyFor(symbol, p.QuoteSHA)
		entry.MacroQuoteWindowMs = p.quoteWindowMs()
		req.addMacro(entry.MacroQuotes, p.QuotePath, entry.MacroQuoteWindowMs)
	}
}

func resolveMacroPathConflicts(req *feedRequirements) {
	if len(req.Macro) == 0 {
		return
	}
	digests := make(map[string]map[string]bool)
	note := func(path, sha string) {
		if digests[path] == nil {
			digests[path] = make(map[string]bool)
		}
		digests[path][sha] = true
	}
	for key, need := range req.Macro {
		for _, p := range need.Paths {
			note(p, key.SHA256)
		}
	}
	conflict := func(path string) string {
		if len(digests[path]) < 2 {
			return ""
		}
		shas := make([]string, 0, len(digests[path]))
		for s := range digests[path] {
			shas = append(shas, s)
		}
		sort.Strings(shas)
		return fmt.Sprintf("artifact path %s is pinned to %d different digests (%s); one file has one content", path, len(shas), strings.Join(shas, ", "))
	}
	for key, need := range req.Macro {
		var valid []string
		why := ""
		for _, p := range need.Paths {
			if c := conflict(p); c != "" {
				why = c
				continue
			}
			valid = append(valid, p)
		}
		if len(valid) == 0 {
			need.Err = why
		} else {
			need.Path = valid[0]
		}
		req.Macro[key] = need
	}
	for id, entry := range req.Strategies {
		if !entry.MacroEvents || entry.MacroErr != "" {
			continue
		}
		for _, p := range []string{entry.MacroCalendarPath, entry.MacroQuotePath} {
			if c := conflict(p); p != "" && c != "" {
				entry.MacroErr = c
			}
		}
		req.Strategies[id] = entry
	}
}

func pruneUnusedMacro(req *feedRequirements) {
	rebuilt := feedRequirements{Macro: make(map[feedMacroKey]feedMacroNeed)}
	for _, entry := range req.Strategies {
		if !entry.MacroEvents || entry.MacroErr != "" {
			continue
		}
		rebuilt.addMacro(entry.MacroCalendar, entry.MacroCalendarPath, 0)
		if entry.MacroHasQuotes {
			rebuilt.addMacro(entry.MacroQuotes, entry.MacroQuotePath, entry.MacroQuoteWindowMs)
		}
	}
	req.Macro = rebuilt.Macro
}

func sortedStrategyRequirementIDs(req feedRequirements) []string {
	ids := make([]string, 0, len(req.Strategies))
	for id := range req.Strategies {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func macroStrategyIDs(cfg *Config) []string {
	var ids []string
	for _, sc := range cfg.Strategies {
		if feedScopedStrategy(sc) && feedStrategyUsesMacroEvents(sc) {
			ids = append(ids, sc.ID)
		}
	}
	sort.Strings(ids)
	return ids
}

type macroCoverage struct {
	EventType string `json:"event_type"`
	StartMs   int64  `json:"start_ms"`
	EndMs     int64  `json:"end_ms"`
}

type macroCalendarSource struct {
	SourceID    string `json:"source_id"`
	Publisher   string `json:"publisher"`
	URL         string `json:"url"`
	RetrievedMs int64  `json:"retrieved_ms"`
	SHA256      string `json:"sha256"`
	PublishedMs *int64 `json:"published_ms"`
	Note        string `json:"note"`
}

type macroQuoteSource struct {
	SourceID    string `json:"source_id"`
	Publisher   string `json:"publisher"`
	URL         string `json:"url"`
	RetrievedMs int64  `json:"retrieved_ms"`
	SHA256      string `json:"sha256"`
	Note        string `json:"note"`
}

type macroCalendarRecord struct {
	EventID             string `json:"event_id"`
	EventType           string `json:"event_type"`
	Revision            int64  `json:"revision"`
	Status              string `json:"status"`
	ScheduledLocal      string `json:"scheduled_local"`
	SourceTimezone      string `json:"source_timezone"`
	ScheduledUTCMs      int64  `json:"scheduled_utc_ms"`
	KnownMs             *int64 `json:"known_ms"`
	PublicationEvidence string `json:"publication_evidence"`
	SourceID            string `json:"source_id"`
	RecordSHA256        string `json:"record_sha256"`
}

type macroQuoteRecord struct {
	EventMs      int64  `json:"event_ms"`
	RecvMs       int64  `json:"recv_ms"`
	Bid          string `json:"bid"`
	Ask          string `json:"ask"`
	SourceID     string `json:"source_id"`
	RecordSHA256 string `json:"record_sha256"`
}

type macroCalendarDoc struct {
	Schema     string                `json:"schema"`
	Kind       string                `json:"kind"`
	Importer   string                `json:"importer"`
	EventTypes []string              `json:"event_types"`
	Coverage   []macroCoverage       `json:"coverage"`
	Sources    []macroCalendarSource `json:"sources"`
	Records    []macroCalendarRecord `json:"records"`
}

type macroQuoteDoc struct {
	Schema    string             `json:"schema"`
	Kind      string             `json:"kind"`
	Importer  string             `json:"importer"`
	Venue     string             `json:"venue"`
	Namespace string             `json:"namespace"`
	Coin      string             `json:"coin"`
	Sources   []macroQuoteSource `json:"sources"`
	Records   []macroQuoteRecord `json:"records"`
}

var (
	macroCalendarTopKeys    = []string{"schema", "kind", "importer", "event_types", "coverage", "sources", "records"}
	macroCalendarSourceKeys = []string{"source_id", "publisher", "url", "retrieved_ms", "sha256", "published_ms", "note"}
	macroCalendarRecordKeys = []string{"event_id", "event_type", "revision", "status", "scheduled_local", "source_timezone", "scheduled_utc_ms", "known_ms", "publication_evidence", "source_id", "record_sha256"}
	macroCoverageKeys       = []string{"event_type", "start_ms", "end_ms"}
	macroQuoteTopKeys       = []string{"schema", "kind", "importer", "venue", "namespace", "coin", "sources", "records"}
	macroQuoteSourceKeys    = []string{"source_id", "publisher", "url", "retrieved_ms", "sha256", "note"}
	macroQuoteRecordKeys    = []string{"event_ms", "recv_ms", "bid", "ask", "source_id", "record_sha256"}
)

func macroCheckKeys(raw json.RawMessage, keys []string, what string) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return fmt.Errorf("%s must be an object: %v", what, err)
	}
	var missing, extra []string
	want := make(map[string]bool, len(keys))
	for _, k := range keys {
		want[k] = true
		if _, ok := m[k]; !ok {
			missing = append(missing, k)
		}
	}
	for k := range m {
		if !want[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	if len(missing) > 0 || len(extra) > 0 {
		return fmt.Errorf("%s keys differ from the contract (missing %v, unknown %v)", what, missing, extra)
	}
	return nil
}

func macroCheckListKeys(raw json.RawMessage, field string, keys []string, what string) error {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return err
	}
	var items []json.RawMessage
	if err := json.Unmarshal(top[field], &items); err != nil {
		return fmt.Errorf("%s %s must be a list: %v", what, field, err)
	}
	for i, it := range items {
		if err := macroCheckKeys(it, keys, fmt.Sprintf("%s %s %d", what, field, i)); err != nil {
			return err
		}
	}
	return nil
}

func macroDecodeStrict(blob []byte, v any) error {
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

func macroASCII(s, what string, limit int) error {
	if s == "" || len(s) > limit {
		return fmt.Errorf("%s must be a nonempty string of at most %d characters, got %q", what, limit, s)
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return fmt.Errorf("%s must be printable ASCII, got %q", what, s)
		}
	}
	return nil
}

func macroCanonicalJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func macroRecordHash(fields []any) string {
	blob, err := macroCanonicalJSON(fields)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(blob)
	return hex.EncodeToString(sum[:])
}

func (r macroCalendarRecord) hash() string {
	var known any
	if r.KnownMs != nil {
		known = *r.KnownMs
	}
	return macroRecordHash([]any{r.EventID, r.EventType, r.Revision, r.Status, r.ScheduledLocal, r.SourceTimezone,
		r.ScheduledUTCMs, known, r.PublicationEvidence, r.SourceID})
}

func (r macroQuoteRecord) hash() string {
	return macroRecordHash([]any{r.EventMs, r.RecvMs, r.Bid, r.Ask, r.SourceID})
}

func macroLocalToUTCMs(local, tzName string) (int64, error) {
	m := macroLocalPattern.FindStringSubmatch(local)
	if m == nil {
		return 0, fmt.Errorf("scheduled_local %q must be YYYY-MM-DDTHH:MM:SS", local)
	}
	if tzName == "" || tzName == "Local" {
		return 0, fmt.Errorf("source_timezone %q is not a known IANA zone", tzName)
	}
	loc, err := time.LoadLocation(tzName)
	if err != nil {
		return 0, fmt.Errorf("source_timezone %q is not a known IANA zone: %v", tzName, err)
	}
	var f [6]int
	for i := range f {
		fmt.Sscanf(m[i+1], "%d", &f[i])
	}
	t := time.Date(f[0], time.Month(f[1]), f[2], f[3], f[4], f[5], 0, loc)
	wall := func(u time.Time) [6]int {
		v := u.In(loc)
		return [6]int{v.Year(), int(v.Month()), v.Day(), v.Hour(), v.Minute(), v.Second()}
	}
	if wall(t) != f {
		return 0, fmt.Errorf("scheduled_local %q does not exist in %s", local, tzName)
	}
	for _, d := range []time.Duration{-2 * time.Hour, -time.Hour, -30 * time.Minute, 30 * time.Minute, time.Hour, 2 * time.Hour} {
		if wall(t.Add(d)) == f {
			return 0, fmt.Errorf("scheduled_local %q is ambiguous in %s", local, tzName)
		}
	}
	return t.UTC().UnixMilli(), nil
}

func macroCanonicalDecimal(s, what string) error {
	if s == "" {
		return fmt.Errorf("%s must be a decimal string", what)
	}
	whole, frac, dot := strings.Cut(s, ".")
	digits := whole + frac
	if whole == "" || (dot && frac == "") {
		return fmt.Errorf("%s %q is not a plain nonnegative decimal", what, s)
	}
	for i := 0; i < len(digits); i++ {
		if digits[i] < '0' || digits[i] > '9' {
			return fmt.Errorf("%s %q is not a plain nonnegative decimal", what, s)
		}
	}
	if len(whole) > 1 && whole[0] == '0' {
		return fmt.Errorf("%s %q has a leading zero", what, s)
	}
	if dot && strings.HasSuffix(frac, "0") {
		return fmt.Errorf("%s %q has a trailing zero", what, s)
	}
	if len(frac) > macroDecimalScale || len(whole) > 15 {
		return fmt.Errorf("%s %q exceeds the %d-digit scale or 15 integer digits", what, s, macroDecimalScale)
	}
	return nil
}

func macroDecimalKey(s string) string {
	whole, frac, _ := strings.Cut(s, ".")
	return strings.Repeat("0", 15-len(whole)) + whole + frac + strings.Repeat("0", macroDecimalScale-len(frac))
}

func macroCalendarSortLess(a, b macroCalendarRecord) bool {
	if a.EventID != b.EventID {
		return a.EventID < b.EventID
	}
	if a.Revision != b.Revision {
		return a.Revision < b.Revision
	}
	an, bn := a.KnownMs == nil, b.KnownMs == nil
	if an != bn {
		return !an
	}
	if !an && *a.KnownMs != *b.KnownMs {
		return *a.KnownMs < *b.KnownMs
	}
	return a.RecordSHA256 < b.RecordSHA256
}

func macroQuoteSortLess(a, b macroQuoteRecord) bool {
	if a.EventMs != b.EventMs {
		return a.EventMs < b.EventMs
	}
	if a.RecvMs != b.RecvMs {
		return a.RecvMs < b.RecvMs
	}
	return a.RecordSHA256 < b.RecordSHA256
}

func validateMacroCalendarRecord(r macroCalendarRecord, i int, types map[string]bool, sources map[string]bool) error {
	what := fmt.Sprintf("calendar record %d", i)
	if !macroEventIDPattern.MatchString(r.EventID) {
		return fmt.Errorf("%s event_id %q is malformed", what, r.EventID)
	}
	if !types[r.EventType] {
		return fmt.Errorf("%s event_type %q is not declared", what, r.EventType)
	}
	if r.Revision <= 0 {
		return fmt.Errorf("%s revision must be a positive integer", what)
	}
	if !macroStatuses[r.Status] {
		return fmt.Errorf("%s status %q is unknown", what, r.Status)
	}
	utc, err := macroLocalToUTCMs(r.ScheduledLocal, r.SourceTimezone)
	if err != nil {
		return fmt.Errorf("%s: %v", what, err)
	}
	if r.ScheduledUTCMs != utc {
		return fmt.Errorf("%s scheduled_utc_ms %d is not the UTC conversion %d", what, r.ScheduledUTCMs, utc)
	}
	if r.KnownMs != nil && *r.KnownMs <= 0 {
		return fmt.Errorf("%s known_ms must be positive or null", what)
	}
	if err := macroASCII(r.PublicationEvidence, what+" publication_evidence", 512); err != nil {
		return err
	}
	if sources != nil && !sources[r.SourceID] {
		return fmt.Errorf("%s source_id %q names no artifact source", what, r.SourceID)
	}
	if !macroHex64.MatchString(r.RecordSHA256) || r.hash() != r.RecordSHA256 {
		return fmt.Errorf("%s (%s r%d) record_sha256 does not match its content", what, r.EventID, r.Revision)
	}
	return nil
}

func validateMacroQuoteRecord(r macroQuoteRecord, i int, sources map[string]bool) error {
	what := fmt.Sprintf("quote record %d", i)
	if r.EventMs <= 0 || r.RecvMs <= 0 {
		return fmt.Errorf("%s needs positive event_ms and recv_ms", what)
	}
	if r.RecvMs < r.EventMs {
		return fmt.Errorf("%s was received at %d before its event time %d", what, r.RecvMs, r.EventMs)
	}
	if err := macroCanonicalDecimal(r.Bid, what+" bid"); err != nil {
		return err
	}
	if err := macroCanonicalDecimal(r.Ask, what+" ask"); err != nil {
		return err
	}
	if strings.Trim(macroDecimalKey(r.Bid), "0") == "" || macroDecimalKey(r.Ask) < macroDecimalKey(r.Bid) {
		return fmt.Errorf("%s needs 0 < bid <= ask, got bid %s ask %s", what, r.Bid, r.Ask)
	}
	if sources != nil && !sources[r.SourceID] {
		return fmt.Errorf("%s source_id %q names no artifact source", what, r.SourceID)
	}
	if !macroHex64.MatchString(r.RecordSHA256) || r.hash() != r.RecordSHA256 {
		return fmt.Errorf("%s record_sha256 does not match its content", what)
	}
	return nil
}

func validateMacroSources(ids []string, retrieved []int64, shas, publishers, urls, notes []string, published []*int64, what string) (map[string]bool, error) {
	if len(ids) == 0 {
		return nil, fmt.Errorf("%s sources must be a nonempty list", what)
	}
	out := make(map[string]bool, len(ids))
	for i, id := range ids {
		if err := macroASCII(id, fmt.Sprintf("%s source %d source_id", what, i), 128); err != nil {
			return nil, err
		}
		if i > 0 && id <= ids[i-1] {
			return nil, fmt.Errorf("%s sources are not strictly ordered by source_id at %q", what, id)
		}
		if err := macroASCII(publishers[i], what+" source publisher", 256); err != nil {
			return nil, err
		}
		if err := macroASCII(urls[i], what+" source url", 1024); err != nil {
			return nil, err
		}
		if err := macroASCII(notes[i], what+" source note", 2048); err != nil {
			return nil, err
		}
		if retrieved[i] <= 0 || !macroHex64.MatchString(shas[i]) {
			return nil, fmt.Errorf("%s source %q needs a positive retrieved_ms and a 64-hex sha256", what, id)
		}
		if published != nil && published[i] != nil && *published[i] <= 0 {
			return nil, fmt.Errorf("%s source %q published_ms must be positive or null", what, id)
		}
		out[id] = true
	}
	return out, nil
}

func validateMacroCalendarDoc(doc *macroCalendarDoc) error {
	if doc.Schema != macroCalendarSchema {
		return fmt.Errorf("calendar schema %q is not %q", doc.Schema, macroCalendarSchema)
	}
	allowed, ok := macroArtifactKindEventTypes[doc.Kind]
	if !ok {
		return fmt.Errorf("calendar kind %q is unknown", doc.Kind)
	}
	if doc.Importer != macroImporter {
		return fmt.Errorf("calendar importer %q is not %q", doc.Importer, macroImporter)
	}
	allowedSet := make(map[string]bool, len(allowed))
	for _, t := range allowed {
		allowedSet[t] = true
	}
	types := make(map[string]bool, len(doc.EventTypes))
	if len(doc.EventTypes) == 0 || !sort.StringsAreSorted(doc.EventTypes) {
		return fmt.Errorf("calendar event_types %v must be a sorted unique nonempty list", doc.EventTypes)
	}
	for _, t := range doc.EventTypes {
		if !allowedSet[t] || types[t] {
			return fmt.Errorf("calendar event_types %v do not fit kind %q", doc.EventTypes, doc.Kind)
		}
		types[t] = true
	}
	for i, c := range doc.Coverage {
		if !types[c.EventType] || c.StartMs <= 0 || c.EndMs <= c.StartMs {
			return fmt.Errorf("calendar coverage %d is malformed", i)
		}
		if i > 0 {
			p := doc.Coverage[i-1]
			if c.EventType < p.EventType || (c.EventType == p.EventType && c.StartMs < p.EndMs) {
				return fmt.Errorf("calendar coverage %d is out of order or overlaps", i)
			}
		}
	}
	n := len(doc.Sources)
	ids, shas, pubs, urls, notes := make([]string, n), make([]string, n), make([]string, n), make([]string, n), make([]string, n)
	retrieved, published := make([]int64, n), make([]*int64, n)
	for i, s := range doc.Sources {
		ids[i], shas[i], pubs[i], urls[i], notes[i], retrieved[i], published[i] = s.SourceID, s.SHA256, s.Publisher, s.URL, s.Note, s.RetrievedMs, s.PublishedMs
	}
	sources, err := validateMacroSources(ids, retrieved, shas, pubs, urls, notes, published, "calendar")
	if err != nil {
		return err
	}
	typeOf := make(map[string]string)
	for i, r := range doc.Records {
		if err := validateMacroCalendarRecord(r, i, types, sources); err != nil {
			return err
		}
		if i > 0 {
			p := doc.Records[i-1]
			if p.RecordSHA256 == r.RecordSHA256 {
				return fmt.Errorf("calendar record %d duplicates an earlier record", i)
			}
			if !macroCalendarSortLess(p, r) {
				return fmt.Errorf("calendar record %d is out of canonical order (event_id, revision, known_ms, hash)", i)
			}
		}
		if t, ok := typeOf[r.EventID]; ok && t != r.EventType {
			return fmt.Errorf("calendar event %s changes event_type between records", r.EventID)
		}
		typeOf[r.EventID] = r.EventType
	}
	return nil
}

func validateMacroQuoteDoc(doc *macroQuoteDoc, coin string) error {
	if doc.Schema != macroQuoteSchema {
		return fmt.Errorf("quote schema %q is not %q", doc.Schema, macroQuoteSchema)
	}
	if !macroQuoteKinds[doc.Kind] {
		return fmt.Errorf("quote kind %q is unknown", doc.Kind)
	}
	if doc.Importer != macroImporter {
		return fmt.Errorf("quote importer %q is not %q", doc.Importer, macroImporter)
	}
	if doc.Venue != macroVenue || doc.Namespace != feedNamespacePerps {
		return fmt.Errorf("quote artifact venue/namespace %q/%q is not %s/%s", doc.Venue, doc.Namespace, macroVenue, feedNamespacePerps)
	}
	if !macroCoinPattern.MatchString(doc.Coin) {
		return fmt.Errorf("quote artifact coin %q is malformed", doc.Coin)
	}
	if coin != "" && doc.Coin != coin {
		return fmt.Errorf("quote artifact coin %q is not the strategy coin %q", doc.Coin, coin)
	}
	n := len(doc.Sources)
	ids, shas, pubs, urls, notes := make([]string, n), make([]string, n), make([]string, n), make([]string, n), make([]string, n)
	retrieved := make([]int64, n)
	for i, s := range doc.Sources {
		ids[i], shas[i], pubs[i], urls[i], notes[i], retrieved[i] = s.SourceID, s.SHA256, s.Publisher, s.URL, s.Note, s.RetrievedMs
	}
	sources, err := validateMacroSources(ids, retrieved, shas, pubs, urls, notes, nil, "quote")
	if err != nil {
		return err
	}
	seen := make(map[string]bool, len(doc.Records))
	for i, r := range doc.Records {
		if err := validateMacroQuoteRecord(r, i, sources); err != nil {
			return err
		}
		if seen[r.RecordSHA256] {
			return fmt.Errorf("quote record %d duplicates an earlier record", i)
		}
		seen[r.RecordSHA256] = true
		if i > 0 && macroQuoteSortLess(r, doc.Records[i-1]) {
			return fmt.Errorf("quote record %d is out of canonical order (event_ms, recv_ms, hash)", i)
		}
	}
	return nil
}

func readMacroArtifactBytes(path, wantSHA string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("artifact %s is unreadable: %v", path, err)
	}
	defer f.Close()
	blob, err := io.ReadAll(io.LimitReader(f, macroArtifactMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("artifact %s is unreadable: %v", path, err)
	}
	if len(blob) > macroArtifactMaxBytes {
		return nil, fmt.Errorf("artifact %s is over the %d-byte cap", path, macroArtifactMaxBytes)
	}
	sum := sha256.Sum256(blob)
	if got := hex.EncodeToString(sum[:]); got != wantSHA {
		return nil, fmt.Errorf("artifact %s has sha256 %s, the configuration pins %s", path, got, wantSHA)
	}
	for _, b := range blob {
		if b > 0x7f {
			return nil, fmt.Errorf("artifact %s is not ASCII JSON", path)
		}
	}
	return blob, nil
}

func parseMacroCalendar(blob []byte) (*macroCalendarDoc, error) {
	if err := macroCheckKeys(blob, macroCalendarTopKeys, "calendar artifact"); err != nil {
		return nil, err
	}
	for field, keys := range map[string][]string{"sources": macroCalendarSourceKeys, "records": macroCalendarRecordKeys, "coverage": macroCoverageKeys} {
		if err := macroCheckListKeys(blob, field, keys, "calendar"); err != nil {
			return nil, err
		}
	}
	var doc macroCalendarDoc
	if err := macroDecodeStrict(blob, &doc); err != nil {
		return nil, fmt.Errorf("calendar artifact does not decode: %v", err)
	}
	if err := validateMacroCalendarDoc(&doc); err != nil {
		return nil, err
	}
	return &doc, nil
}

func parseMacroQuotes(blob []byte, coin string) (*macroQuoteDoc, error) {
	if err := macroCheckKeys(blob, macroQuoteTopKeys, "quote artifact"); err != nil {
		return nil, err
	}
	for field, keys := range map[string][]string{"sources": macroQuoteSourceKeys, "records": macroQuoteRecordKeys} {
		if err := macroCheckListKeys(blob, field, keys, "quote"); err != nil {
			return nil, err
		}
	}
	var doc macroQuoteDoc
	if err := macroDecodeStrict(blob, &doc); err != nil {
		return nil, fmt.Errorf("quote artifact does not decode: %v", err)
	}
	if err := validateMacroQuoteDoc(&doc, coin); err != nil {
		return nil, err
	}
	return &doc, nil
}

type feedMacroPrepared struct {
	Key        feedMacroKey
	Path       string
	Err        string
	PreparedAt time.Time
	Calendar   *macroCalendarDoc
	Quotes     *macroQuoteDoc
	byRecv     []int
}

func prepareMacroArtifact(key feedMacroKey, need feedMacroNeed, now time.Time) *feedMacroPrepared {
	out := &feedMacroPrepared{Key: key, Path: need.Path, PreparedAt: now}
	if need.Err != "" {
		out.Err = need.Err
		return out
	}
	blob, err := readMacroArtifactBytes(need.Path, key.SHA256)
	if err != nil {
		out.Err = err.Error()
		return out
	}
	switch key.Kind {
	case macroKindCalendar:
		doc, perr := parseMacroCalendar(blob)
		if perr != nil {
			out.Err = fmt.Sprintf("calendar artifact %s: %v", need.Path, perr)
			return out
		}
		out.Calendar = doc
	case macroKindQuotes:
		doc, perr := parseMacroQuotes(blob, key.Coin)
		if perr != nil {
			out.Err = fmt.Sprintf("quote artifact %s: %v", need.Path, perr)
			return out
		}
		out.Quotes = doc
		out.byRecv = make([]int, len(doc.Records))
		for i := range out.byRecv {
			out.byRecv[i] = i
		}
		sort.SliceStable(out.byRecv, func(a, b int) bool {
			return doc.Records[out.byRecv[a]].RecvMs < doc.Records[out.byRecv[b]].RecvMs
		})
	default:
		out.Err = fmt.Sprintf("unknown macro artifact kind %q", key.Kind)
	}
	return out
}

func (o *marketFeedOwner) prepareMacroArtifacts(needs map[feedMacroKey]feedMacroNeed) map[feedMacroKey]*feedMacroPrepared {
	if len(needs) == 0 {
		return map[feedMacroKey]*feedMacroPrepared{}
	}
	o.feedMu.Lock()
	existing := make(map[feedMacroKey]*feedMacroPrepared, len(o.macro))
	for k, v := range o.macro {
		existing[k] = v
	}
	o.feedMu.Unlock()
	keys := make([]feedMacroKey, 0, len(needs))
	for k := range needs {
		keys = append(keys, k)
	}
	sortFeedMacroKeys(keys)
	out := make(map[feedMacroKey]*feedMacroPrepared, len(needs))
	now := o.now()
	for _, key := range keys {
		need := needs[key]
		if prev := existing[key]; prev != nil && prev.Err == "" && need.Err == "" {
			out[key] = prev
			continue
		}
		prepared := prepareMacroArtifact(key, need, now)
		if prepared.Err != "" {
			o.logf("[feed] macro input %s unavailable: %s", key.PayloadID(), prepared.Err)
		} else {
			o.logf("[feed] macro input %s prepared from %s", key.PayloadID(), need.Path)
		}
		out[key] = prepared
	}
	return out
}

func (o *marketFeedOwner) installMacroLocked(needs map[feedMacroKey]feedMacroNeed, prepared map[feedMacroKey]*feedMacroPrepared) {
	o.macro = make(map[feedMacroKey]*feedMacroPrepared, len(prepared))
	for k, v := range prepared {
		if _, ok := needs[k]; ok {
			o.macro[k] = v
		}
	}
	o.macroNeeds = make(map[feedMacroKey]feedMacroNeed, len(needs))
	for k, v := range needs {
		o.macroNeeds[k] = v
	}
}

func (o *marketFeedOwner) MacroNeeds() map[feedMacroKey]feedMacroNeed {
	o.feedMu.Lock()
	defer o.feedMu.Unlock()
	out := make(map[feedMacroKey]feedMacroNeed, len(o.macroNeeds))
	for k, v := range o.macroNeeds {
		out[k] = v
	}
	return out
}

type marketSnapshotMacro struct {
	Key          feedMacroKey
	Available    bool
	Reason       string
	Schema       string
	ArtifactKind string
	EventTypes   []string
	Coverage     []macroCoverage
	CutoffMs     int64
	WindowMs     int64
	Calendar     []macroCalendarRecord
	Quotes       []macroQuoteRecord
}

func freezeMacroLocked(p *feedMacroPrepared, key feedMacroKey, windowMs, cutoffMs int64) *marketSnapshotMacro {
	out := &marketSnapshotMacro{Key: key, CutoffMs: cutoffMs, WindowMs: windowMs, EventTypes: []string{}, Coverage: []macroCoverage{},
		Calendar: []macroCalendarRecord{}, Quotes: []macroQuoteRecord{}}
	switch {
	case p == nil:
		out.Reason = fmt.Sprintf("macro input %s is not prepared for this feed generation", key.PayloadID())
		return out
	case p.Err != "":
		out.Reason = p.Err
		return out
	}
	if key.Kind == macroKindCalendar && p.Calendar != nil {
		doc := p.Calendar
		out.Schema, out.ArtifactKind = doc.Schema, doc.Kind
		out.EventTypes = append([]string{}, doc.EventTypes...)
		out.Coverage = append([]macroCoverage{}, doc.Coverage...)
		for _, r := range doc.Records {
			if r.KnownMs != nil && *r.KnownMs <= cutoffMs {
				out.Calendar = append(out.Calendar, r)
			}
		}
		out.Available = true
		return out
	}
	if key.Kind == macroKindQuotes && p.Quotes != nil {
		doc := p.Quotes
		out.Schema, out.ArtifactKind = doc.Schema, doc.Kind
		start := cutoffMs - windowMs
		lo := sort.Search(len(p.byRecv), func(i int) bool { return doc.Records[p.byRecv[i]].RecvMs >= start })
		hi := sort.Search(len(p.byRecv), func(i int) bool { return doc.Records[p.byRecv[i]].RecvMs > cutoffMs })
		if hi-lo > macroQuoteMaxSealRecords {
			out.Reason = fmt.Sprintf("quote window %dms holds %d records, over the %d-record seal cap", windowMs, hi-lo, macroQuoteMaxSealRecords)
			return out
		}
		idx := append([]int(nil), p.byRecv[lo:hi]...)
		sort.Ints(idx)
		for _, i := range idx {
			out.Quotes = append(out.Quotes, doc.Records[i])
		}
		out.Available = true
		return out
	}
	out.Reason = fmt.Sprintf("macro input %s carries no prepared records", key.PayloadID())
	return out
}

type marketMacroEventsPayload struct {
	Kind           string                `json:"kind"`
	Available      bool                  `json:"available"`
	Reason         string                `json:"reason"`
	CalendarSHA256 string                `json:"calendar_sha256"`
	Schema         string                `json:"schema"`
	ArtifactKind   string                `json:"artifact_kind"`
	EventTypes     []string              `json:"event_types"`
	Coverage       []macroCoverage       `json:"coverage"`
	CutoffMs       *int64                `json:"cutoff_ms"`
	Records        []macroCalendarRecord `json:"records"`
}

type marketMacroQuotesPayload struct {
	Kind         string             `json:"kind"`
	Available    bool               `json:"available"`
	Reason       string             `json:"reason"`
	QuoteSHA256  string             `json:"quote_sha256"`
	Schema       string             `json:"schema"`
	ArtifactKind string             `json:"artifact_kind"`
	Venue        string             `json:"venue"`
	Namespace    string             `json:"namespace"`
	Coin         string             `json:"coin"`
	CutoffMs     *int64             `json:"cutoff_ms"`
	WindowMs     *int64             `json:"window_ms"`
	Records      []macroQuoteRecord `json:"records"`
}

func (s *marketSnapshot) macroEntry(key feedMacroKey) (*marketSnapshotMacro, string) {
	if s == nil {
		return nil, "no sealed market snapshot"
	}
	entry, ok := s.macro[key]
	if !ok || entry == nil {
		return nil, fmt.Sprintf("macro input %s is not in the sealed snapshot", key.PayloadID())
	}
	if !entry.Available {
		return entry, entry.Reason
	}
	return entry, ""
}

func (s *marketSnapshot) macroEventsPayload(key feedMacroKey) marketMacroEventsPayload {
	out := marketMacroEventsPayload{Kind: macroKindCalendar, CalendarSHA256: key.SHA256, EventTypes: []string{},
		Coverage: []macroCoverage{}, Records: []macroCalendarRecord{}}
	entry, why := s.macroEntry(key)
	if why != "" {
		out.Reason = why
		return out
	}
	cutoff := entry.CutoffMs
	out.Available, out.Schema, out.ArtifactKind, out.CutoffMs = true, entry.Schema, entry.ArtifactKind, &cutoff
	out.EventTypes = append(out.EventTypes, entry.EventTypes...)
	out.Coverage = append(out.Coverage, entry.Coverage...)
	out.Records = append(out.Records, entry.Calendar...)
	return out
}

func (s *marketSnapshot) macroQuotesPayload(key feedMacroKey, windowMs int64) marketMacroQuotesPayload {
	out := marketMacroQuotesPayload{Kind: macroKindQuotes, QuoteSHA256: key.SHA256, Venue: macroVenue,
		Namespace: feedNamespacePerps, Coin: key.Coin, Records: []macroQuoteRecord{}}
	entry, why := s.macroEntry(key)
	if why != "" {
		out.Reason = why
		return out
	}
	if entry.WindowMs < windowMs {
		out.Reason = fmt.Sprintf("macro quote window %dms in the seal is below the %dms this strategy needs", entry.WindowMs, windowMs)
		return out
	}
	cutoff, window := entry.CutoffMs, windowMs
	out.Available, out.Schema, out.ArtifactKind, out.CutoffMs, out.WindowMs = true, entry.Schema, entry.ArtifactKind, &cutoff, &window
	start := cutoff - windowMs
	for _, r := range entry.Quotes {
		if r.RecvMs >= start && r.RecvMs <= cutoff {
			out.Records = append(out.Records, r)
		}
	}
	return out
}

func attachMacroPayloads(payload *marketPayload, s *marketSnapshot, needs map[feedMacroKey]int64) {
	if payload == nil || len(needs) == 0 {
		return
	}
	keys := make([]feedMacroKey, 0, len(needs))
	for k := range needs {
		keys = append(keys, k)
	}
	sortFeedMacroKeys(keys)
	for _, k := range keys {
		switch k.Kind {
		case macroKindCalendar:
			if payload.MacroEvents == nil {
				payload.MacroEvents = make(map[string]marketMacroEventsPayload)
			}
			payload.MacroEvents[k.PayloadID()] = s.macroEventsPayload(k)
		case macroKindQuotes:
			if payload.MacroQuotes == nil {
				payload.MacroQuotes = make(map[string]marketMacroQuotesPayload)
			}
			payload.MacroQuotes[k.PayloadID()] = s.macroQuotesPayload(k, needs[k])
		}
	}
}

func (s *marketSnapshot) macroHold(entry feedStrategyRequirement) (bool, string) {
	if !entry.MacroEvents {
		return false, ""
	}
	if entry.MacroErr != "" {
		return true, entry.MacroErr
	}
	needs := entry.macroNeeds()
	keys := make([]feedMacroKey, 0, len(needs))
	for k := range needs {
		keys = append(keys, k)
	}
	sortFeedMacroKeys(keys)
	for _, k := range keys {
		e, why := s.macroEntry(k)
		if why != "" {
			return true, why
		}
		if k.Kind == macroKindQuotes && e.WindowMs < needs[k] {
			return true, fmt.Sprintf("macro quote window %dms in the seal is below the %dms this strategy needs", e.WindowMs, needs[k])
		}
	}
	if !entry.MacroHasQuotes {
		return true, "no quote artifact is configured, so the spread gate has no supplied bid/ask record"
	}
	return false, ""
}

type feedSealMacroCalendar struct {
	SHA256       string                `json:"sha256"`
	Available    bool                  `json:"available"`
	Reason       string                `json:"reason"`
	Schema       string                `json:"schema"`
	ArtifactKind string                `json:"artifact_kind"`
	EventTypes   []string              `json:"event_types"`
	Coverage     []macroCoverage       `json:"coverage"`
	CutoffMs     int64                 `json:"cutoff_ms"`
	Records      []macroCalendarRecord `json:"records"`
}

type feedSealMacroQuotes struct {
	Host         string             `json:"host"`
	Namespace    string             `json:"namespace"`
	Coin         string             `json:"coin"`
	SHA256       string             `json:"sha256"`
	Available    bool               `json:"available"`
	Reason       string             `json:"reason"`
	Schema       string             `json:"schema"`
	ArtifactKind string             `json:"artifact_kind"`
	CutoffMs     int64              `json:"cutoff_ms"`
	WindowMs     int64              `json:"window_ms"`
	Records      []macroQuoteRecord `json:"records"`
}

func sealMacro(entries map[feedMacroKey]*marketSnapshotMacro) ([]feedSealMacroCalendar, []feedSealMacroQuotes) {
	keys := make([]feedMacroKey, 0, len(entries))
	for k, v := range entries {
		if v != nil {
			keys = append(keys, k)
		}
	}
	sortFeedMacroKeys(keys)
	var cals []feedSealMacroCalendar
	var quotes []feedSealMacroQuotes
	for _, k := range keys {
		e := entries[k]
		switch k.Kind {
		case macroKindCalendar:
			cals = append(cals, feedSealMacroCalendar{
				SHA256: k.SHA256, Available: e.Available, Reason: e.Reason, Schema: e.Schema, ArtifactKind: e.ArtifactKind,
				EventTypes: append([]string{}, e.EventTypes...), Coverage: append([]macroCoverage{}, e.Coverage...),
				CutoffMs: e.CutoffMs, Records: append([]macroCalendarRecord{}, e.Calendar...),
			})
		case macroKindQuotes:
			quotes = append(quotes, feedSealMacroQuotes{
				Host: k.Host, Namespace: k.Namespace, Coin: k.Coin, SHA256: k.SHA256, Available: e.Available, Reason: e.Reason,
				Schema: e.Schema, ArtifactKind: e.ArtifactKind, CutoffMs: e.CutoffMs, WindowMs: e.WindowMs,
				Records: append([]macroQuoteRecord{}, e.Quotes...),
			})
		}
	}
	return cals, quotes
}

func validateFeedSealMacro(cals []feedSealMacroCalendar, quotes []feedSealMacroQuotes) error {
	for i, c := range cals {
		if !macroHex64.MatchString(c.SHA256) {
			return fmt.Errorf("seal macro calendar %d sha256 is malformed", i)
		}
		if i > 0 && cals[i-1].SHA256 >= c.SHA256 {
			return fmt.Errorf("seal macro calendars are not strictly ordered at %s", c.SHA256)
		}
		if c.CutoffMs <= 0 {
			return fmt.Errorf("seal macro calendar %s has no cutoff", c.SHA256)
		}
		if !c.Available {
			if c.Reason == "" || len(c.Records) > 0 || c.Schema != "" || len(c.Coverage) > 0 || len(c.EventTypes) > 0 {
				return fmt.Errorf("seal macro calendar %s is unavailable but carries content or no reason", c.SHA256)
			}
			continue
		}
		doc := macroCalendarDoc{Schema: c.Schema, Kind: c.ArtifactKind, Importer: macroImporter, EventTypes: c.EventTypes, Coverage: c.Coverage,
			Sources: []macroCalendarSource{{SourceID: "seal", Publisher: "seal", URL: "seal", RetrievedMs: 1, SHA256: c.SHA256, Note: "seal"}}}
		if c.Reason != "" {
			return fmt.Errorf("seal macro calendar %s is available but carries a reason", c.SHA256)
		}
		if err := validateMacroCalendarDoc(&doc); err != nil {
			return fmt.Errorf("seal macro calendar %s: %v", c.SHA256, err)
		}
		types := make(map[string]bool, len(c.EventTypes))
		for _, t := range c.EventTypes {
			types[t] = true
		}
		for j, r := range c.Records {
			if err := validateMacroCalendarRecord(r, j, types, nil); err != nil {
				return fmt.Errorf("seal macro calendar %s: %v", c.SHA256, err)
			}
			if r.KnownMs == nil || *r.KnownMs > c.CutoffMs {
				return fmt.Errorf("seal macro calendar %s record %d is not known by the cutoff", c.SHA256, j)
			}
			if j > 0 && (c.Records[j-1].RecordSHA256 == r.RecordSHA256 || !macroCalendarSortLess(c.Records[j-1], r)) {
				return fmt.Errorf("seal macro calendar %s record %d is duplicated or out of order", c.SHA256, j)
			}
		}
	}
	for i, q := range quotes {
		key := macroQuoteKeyFor(q.Coin, q.SHA256)
		key.Host, key.Namespace = q.Host, q.Namespace
		if q.Host != hlMainnetURL || q.Namespace != feedNamespacePerps || !macroCoinPattern.MatchString(q.Coin) || !macroHex64.MatchString(q.SHA256) {
			return fmt.Errorf("seal macro quotes %d identity is malformed", i)
		}
		if i > 0 {
			prev := macroQuoteKeyFor(quotes[i-1].Coin, quotes[i-1].SHA256)
			if !feedMacroKeyLess(prev, key) {
				return fmt.Errorf("seal macro quotes are not strictly ordered at %s", key.PayloadID())
			}
		}
		if q.CutoffMs <= 0 || q.WindowMs <= 0 {
			return fmt.Errorf("seal macro quotes %s has cutoff %d, window %d", key.PayloadID(), q.CutoffMs, q.WindowMs)
		}
		if !q.Available {
			if q.Reason == "" || len(q.Records) > 0 || q.Schema != "" {
				return fmt.Errorf("seal macro quotes %s is unavailable but carries content or no reason", key.PayloadID())
			}
			continue
		}
		if q.Reason != "" || q.Schema != macroQuoteSchema || !macroQuoteKinds[q.ArtifactKind] {
			return fmt.Errorf("seal macro quotes %s declares schema %q kind %q reason %q", key.PayloadID(), q.Schema, q.ArtifactKind, q.Reason)
		}
		if len(q.Records) > macroQuoteMaxSealRecords {
			return fmt.Errorf("seal macro quotes %s carries %d records, over the cap", key.PayloadID(), len(q.Records))
		}
		start := q.CutoffMs - q.WindowMs
		for j, r := range q.Records {
			if err := validateMacroQuoteRecord(r, j, nil); err != nil {
				return fmt.Errorf("seal macro quotes %s: %v", key.PayloadID(), err)
			}
			if r.RecvMs < start || r.RecvMs > q.CutoffMs {
				return fmt.Errorf("seal macro quotes %s record %d receipt %d is outside [%d, %d]", key.PayloadID(), j, r.RecvMs, start, q.CutoffMs)
			}
			if j > 0 && (q.Records[j-1].RecordSHA256 == r.RecordSHA256 || macroQuoteSortLess(r, q.Records[j-1])) {
				return fmt.Errorf("seal macro quotes %s record %d is duplicated or out of order", key.PayloadID(), j)
			}
		}
	}
	return nil
}

func snapshotMacro(cals []feedSealMacroCalendar, quotes []feedSealMacroQuotes) map[feedMacroKey]*marketSnapshotMacro {
	out := make(map[feedMacroKey]*marketSnapshotMacro, len(cals)+len(quotes))
	for _, c := range cals {
		key := macroCalendarKeyFor(c.SHA256)
		out[key] = &marketSnapshotMacro{Key: key, Available: c.Available, Reason: c.Reason, Schema: c.Schema, ArtifactKind: c.ArtifactKind,
			EventTypes: append([]string{}, c.EventTypes...), Coverage: append([]macroCoverage{}, c.Coverage...), CutoffMs: c.CutoffMs,
			Calendar: append([]macroCalendarRecord{}, c.Records...), Quotes: []macroQuoteRecord{}}
	}
	for _, q := range quotes {
		key := macroQuoteKeyFor(q.Coin, q.SHA256)
		out[key] = &marketSnapshotMacro{Key: key, Available: q.Available, Reason: q.Reason, Schema: q.Schema, ArtifactKind: q.ArtifactKind,
			EventTypes: []string{}, Coverage: []macroCoverage{}, CutoffMs: q.CutoffMs, WindowMs: q.WindowMs,
			Calendar: []macroCalendarRecord{}, Quotes: append([]macroQuoteRecord{}, q.Records...)}
	}
	return out
}

type feedDescribeMacro struct {
	Kind      string `json:"kind"`
	Host      string `json:"host"`
	Namespace string `json:"namespace"`
	Coin      string `json:"coin"`
	SHA256    string `json:"sha256"`
	WindowMs  int64  `json:"window_ms"`
}

func describeMacro(req feedRequirements) []feedDescribeMacro {
	var out []feedDescribeMacro
	for _, k := range req.macroKeys() {
		out = append(out, feedDescribeMacro{Kind: k.Kind, Host: k.Host, Namespace: k.Namespace, Coin: k.Coin, SHA256: k.SHA256, WindowMs: req.Macro[k].WindowMs})
	}
	return out
}

func macroCoverageGaps(req feedRequirements, served []feedDescribeMacro) []string {
	got := make(map[feedMacroKey]int64, len(served))
	for _, m := range served {
		got[feedMacroKey{Kind: m.Kind, Host: m.Host, Namespace: m.Namespace, Coin: m.Coin, SHA256: m.SHA256}] = m.WindowMs
	}
	var gaps []string
	for _, k := range req.macroKeys() {
		w, ok := got[k]
		switch {
		case !ok:
			gaps = append(gaps, "macro input "+k.PayloadID()+" not served")
		case w < req.Macro[k].WindowMs:
			gaps = append(gaps, fmt.Sprintf("macro input %s window %dms < %dms", k.PayloadID(), w, req.Macro[k].WindowMs))
		}
	}
	return gaps
}

func applyMacroSealCoverage(snap *marketSnapshot, reqs cycleMarketRequirements) []string {
	keys := make([]feedMacroKey, 0, len(reqs.Macro))
	for k := range reqs.Macro {
		keys = append(keys, k)
	}
	sortFeedMacroKeys(keys)
	var gaps []string
	for _, k := range keys {
		e, ok := snap.macro[k]
		switch {
		case !ok || e == nil:
			gaps = append(gaps, fmt.Sprintf("macro input %s is not in the seal", k.PayloadID()))
		case k.Kind == macroKindQuotes && e.WindowMs < reqs.Macro[k]:
			gaps = append(gaps, fmt.Sprintf("macro input %s window %dms is below the %dms this consumer needs", k.PayloadID(), e.WindowMs, reqs.Macro[k]))
		}
	}
	return gaps
}

func estimateMacroSealBytes(union feedRequirements) int {
	total := 0
	for k := range union.Macro {
		if k.Kind == macroKindQuotes {
			total += feedEstimateKeyBytes + macroQuoteMaxSealRecords*feedEstimateMacroRecordBytes
		} else {
			total += feedEstimateKeyBytes + macroCalendarSealAllowance*feedEstimateMacroRecordBytes
		}
	}
	return total
}

func macroIdentityReloadErrors(cfg, next *Config) []string {
	nextByID := strategyConfigByID(next.Strategies)
	var errs []string
	for _, sc := range cfg.Strategies {
		ns, ok := nextByID[sc.ID]
		if !ok {
			continue
		}
		oldUses, newUses := feedStrategyUsesMacroEvents(sc), feedStrategyUsesMacroEvents(ns)
		if !oldUses && !newUses {
			continue
		}
		oldID, oldErr := macroIdentityFor(sc)
		newID, newErr := macroIdentityFor(ns)
		if oldUses != newUses || oldErr != nil || newErr != nil || oldID != newID {
			errs = append(errs, fmt.Sprintf("strategy[%s] %s artifact identity changed (calendar %q@%s -> %q@%s, quotes %q@%s -> %q@%s; restart required)",
				sc.ID, macroEventBreakoutStrategyName, oldID.CalendarPath, oldID.CalendarSHA, newID.CalendarPath, newID.CalendarSHA,
				oldID.QuotePath, oldID.QuoteSHA, newID.QuotePath, newID.QuoteSHA))
		}
	}
	return errs
}

func macroGenerationReloadErrors(current map[feedMacroKey]feedMacroNeed, next feedRequirements) []string {
	var errs []string
	seen := make(map[feedMacroKey]bool)
	for _, k := range next.macroKeys() {
		seen[k] = true
		cur, ok := current[k]
		switch {
		case !ok:
			errs = append(errs, fmt.Sprintf("macro input %s (%s) is new in this generation (restart required)", k.PayloadID(), next.Macro[k].Path))
		case cur.Path != next.Macro[k].Path:
			errs = append(errs, fmt.Sprintf("macro input %s path changed (%q -> %q; restart required)", k.PayloadID(), cur.Path, next.Macro[k].Path))
		}
	}
	keys := make([]feedMacroKey, 0, len(current))
	for k := range current {
		keys = append(keys, k)
	}
	sortFeedMacroKeys(keys)
	for _, k := range keys {
		if !seen[k] {
			errs = append(errs, fmt.Sprintf("macro input %s (%s) is dropped by this generation (restart required)", k.PayloadID(), current[k].Path))
		}
	}
	return errs
}
