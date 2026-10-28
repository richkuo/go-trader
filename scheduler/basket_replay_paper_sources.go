package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var pairHistoryReaderScript = "shared_scripts/fetch_pair_history.py"

var pairHistoryHLCandlesProbeArgv = []string{"--kind=candles", "--provider=hyperliquid", "--instrument=BTC", "--timeframe=1h", "--limit=500", "--probe-only"}

var pairHistoryOKXCandlesProbeArgv = []string{"--kind=candles", "--provider=okx", "--instrument=BTC-USDT-SWAP", "--timeframe=1h", "--limit=300", "--probe-only"}

var pairHistoryHLFundingProbeArgv = []string{"--kind=funding", "--provider=hyperliquid", "--instrument=BTC", "--from-ms=1", "--through-ms=2",
	`--member-key-json={"partition":"paper","provider":"hyperliquid","account_key":"synthetic:probe","instrument":"BTC","member_id":"probe"}`,
	"--source-id=probe", "--seq=1", "--prev-hash=", "--probe-only"}

var pairHistoryOKXFundingProbeArgv = []string{"--kind=funding", "--provider=okx", "--instrument=BTC-USDT-SWAP", "--from-ms=1", "--through-ms=2",
	`--member-key-json={"partition":"paper","provider":"okx","account_key":"synthetic:probe","instrument":"BTC-USDT-SWAP","member_id":"probe"}`,
	"--source-id=probe", "--seq=1", "--prev-hash=", "--probe-only"}

var pairHistoryHLOracleProbeArgv, pairHistoryHLResearchFundingProbeArgv, pairHistoryOKXResearchFundingProbeArgv = func() ([]string, []string, []string) {
	policy := pairFundingPolicyV1{Version: 1, MemberRules: map[string]string{"hl_probe": pairFundingRuleHLReceiptResearch, "okx_probe": pairFundingRuleOKX},
		WindowMs: 5000, MaxPrintOffsetMs: 60000, PrepareLeadMs: 30000, FromMs: 1, DrainThroughMs: 7200000}
	digest, err := pairFundingPolicyDigest(policy)
	if err != nil {
		panic(err)
	}
	policyJSON, err := pairCanonicalJSON(policy)
	if err != nil {
		panic(err)
	}
	hlKey, err := pairCanonicalJSON(pairEvidenceKey{Partition: "paper", Provider: entryProviderHyperliquid, AccountKey: "synthetic:probe", Instrument: "BTC", MemberID: "hl_probe"})
	if err != nil {
		panic(err)
	}
	okxKey, err := pairCanonicalJSON(pairEvidenceKey{Partition: "paper", Provider: entryProviderOKX, AccountKey: "synthetic:probe", Instrument: "BTC-USDT-SWAP", MemberID: "okx_probe"})
	if err != nil {
		panic(err)
	}
	oracle := []string{"--kind=funding_oracle", "--provider=hyperliquid", "--instrument=BTC", "--member-key-json=" + string(hlKey),
		"--target-event-ms=3600000", "--not-before-ms=3600000", "--deadline-ms=3605000", "--funding-policy-json=" + string(policyJSON), "--policy-digest=" + digest, "--probe-only"}
	hl := []string{"--kind=funding", "--provider=hyperliquid", "--instrument=BTC", "--from-ms=1", "--through-ms=2", "--member-key-json=" + string(hlKey),
		"--source-id=probe", "--seq=1", "--prev-hash=", "--funding-policy-json=" + string(policyJSON), "--policy-digest=" + digest, "--rule-version=" + pairFundingRuleHLReceiptResearch, "--probe-only"}
	okx := []string{"--kind=funding", "--provider=okx", "--instrument=BTC-USDT-SWAP", "--from-ms=1", "--through-ms=2", "--member-key-json=" + string(okxKey),
		"--source-id=probe", "--seq=1", "--prev-hash=", "--funding-policy-json=" + string(policyJSON), "--policy-digest=" + digest, "--rule-version=" + pairFundingRuleOKX, "--probe-only"}
	return oracle, hl, okx
}()

const (
	pairPaperCandleLimitHL  = 500
	pairPaperCandleLimitOKX = 300
	pairPaperHistoryTimeout = 60 * time.Second
	pairPaperHistoryMaxOut  = 8 << 20
)

type pairPaperReadResult struct {
	Payload   []byte `json:"payload"`
	ReceiptMs int64  `json:"receipt_ms"`
	RequestMs int64  `json:"request_ms"`
	StartMs   int64  `json:"start_ms"`
	RecvMs    int64  `json:"recv_ms"`
}

type pairPaperMemberRead struct {
	MemberID string              `json:"member_id"`
	CallID   string              `json:"call_id"`
	Read     pairPaperReadResult `json:"read"`
}

type pairPaperCollectResult struct {
	Members []pairPaperMemberRead `json:"members"`
}

type pairPaperPoll struct {
	obs     pairObservation
	logical int64
	callID  string
}

func pairExitStatus(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

func pairClassFor(ctx context.Context, err error) string {
	switch {
	case err == nil:
		return pairRecClassResult
	case ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
		return pairRecClassCancelled
	}
	return pairRecClassInfra
}

func pairPaperPublicRead(ctx context.Context, egress *EgressConfig, m pairContractMember, depth, limit int) pairRecResult {
	fail := func(class, format string, a ...interface{}) pairRecResult {
		return pairRecResult{Class: class, Cause: processEgress.redact(fmt.Sprintf(format, a...))}
	}
	if _, ok := ctx.Deadline(); !ok {
		return fail(pairRecClassInfra, "a public quote read needs a caller deadline")
	}
	if err := pairPublicEgressReady(ctx, egress); err != nil {
		return fail(pairRecClassInfra, "%v", err)
	}
	release, err := acquirePythonSlot(ctx)
	if err != nil {
		return fail(pairClassFor(ctx, err), "waiting for a script slot: %v", err)
	}
	defer release()
	env := map[string]string{"PYTHONDONTWRITEBYTECODE": "1"}
	for _, k := range pairQuoteReaderClearedEnv {
		env[k] = ""
	}
	args := []string{"--provider=" + m.Provider, "--source=" + pairSourcePublicSnapshot, "--instrument=" + m.Instrument, "--depth=" + strconv.Itoa(depth)}
	start := pairPaperWall.Now()
	stdout, stderr, runErr := spawnPythonProcessWithEnvLimited(ctx, pairQuoteReaderScript, args, nil, 0, env, pythonOutputLimits{Stdout: limit, Stderr: limit})
	recv := pairPaperWall.Now()
	res := pairRecResult{Stdout: stdout, Stderr: processEgress.redact(string(stderr)), ExitStatus: pairExitStatus(runErr)}
	setFail := func(class, format string, a ...interface{}) pairRecResult {
		res.Class, res.Cause = class, processEgress.redact(fmt.Sprintf(format, a...))
		return res
	}
	var overflow *pythonOutputOverflowError
	if errors.As(runErr, &overflow) {
		res.Overflow = true
		return setFail(pairRecClassInfra, "%v", runErr)
	}
	if err := ctx.Err(); err != nil {
		return setFail(pairRecClassCancelled, "the quote reader was cancelled: %v", err)
	}
	tree, perr := pairStrictParse(bytes.TrimSpace(stdout))
	if perr != nil {
		if runErr != nil {
			return setFail(pairRecClassInfra, "the quote reader failed: %v (%s)", runErr, strings.TrimSpace(string(stderr)))
		}
		return setFail(pairRecClassInfra, "the quote reader returned unreadable output: %v", perr)
	}
	obj, ok := tree.(map[string]interface{})
	if !ok {
		return setFail(pairRecClassInfra, "the quote reader output is not a JSON object")
	}
	if ok, _ := obj["ok"].(bool); !ok {
		msg, _ := obj["error"].(string)
		return setFail(pairRecClassInfra, "the quote reader refused: %s", msg)
	}
	if runErr != nil {
		return setFail(pairRecClassInfra, "the quote reader failed: %v", runErr)
	}
	host := pairHLPublicHost
	if m.Provider == entryProviderOKX {
		host = pairOKXPublicHost
	}
	contract := pairSourceContractFor(m.Provider, pairSourcePublicSnapshot)
	if err := pairReaderEcho(obj, map[string]string{"provider": m.Provider, "source": pairSourcePublicSnapshot, "instrument": m.Instrument, "host": host, "provenance": contract + "@" + host}); err != nil {
		return setFail(pairRecClassInfra, "%v", err)
	}
	reqMs, err := pairTreeInt(obj["request_ms"], "request_ms", 0, recv.UnixMilli())
	if err != nil {
		return setFail(pairRecClassInfra, "%v", err)
	}
	receipt, err := pairTreeInt(obj["receipt_ms"], "receipt_ms", 0, recv.UnixMilli())
	if err != nil {
		return setFail(pairRecClassInfra, "%v", err)
	}
	if reqMs < start.UnixMilli() || receipt < reqMs {
		return setFail(pairRecClassInfra, "the reader times (request %d, receipt %d) are not inside the launch window [%d, %d]", reqMs, receipt, start.UnixMilli(), recv.UnixMilli())
	}
	pnode, ok := obj["payload"]
	if !ok {
		return setFail(pairRecClassInfra, "the quote reader output has no payload")
	}
	var buf bytes.Buffer
	if err := pairWriteCanonical(&buf, pnode); err != nil {
		return setFail(pairRecClassInfra, "the quote reader payload: %v", err)
	}
	out := pairPaperReadResult{Payload: buf.Bytes(), ReceiptMs: receipt, RequestMs: reqMs, StartMs: start.UnixMilli(), RecvMs: recv.UnixMilli()}
	blob, err := json.Marshal(out)
	if err != nil {
		return setFail(pairRecClassInfra, "%v", err)
	}
	res.Class, res.Result, res.ReceiptMs = pairRecClassResult, blob, receipt
	res.Stdout = nil
	return res
}

func pairPaperDecodeRead(blob []byte) (pairPaperReadResult, error) {
	var r pairPaperReadResult
	if err := pairReplayStrictDecode(blob, &r); err != nil {
		return r, fmt.Errorf("the recorded public read is unreadable: %w", err)
	}
	if r.StartMs <= 0 || r.RequestMs < r.StartMs || r.ReceiptMs < r.RequestMs || r.ReceiptMs > r.RecvMs || len(r.Payload) == 0 {
		return r, fmt.Errorf("the recorded public read times (start %d, request %d, receipt %d, end %d) are not ordered inside the launch window", r.StartMs, r.RequestMs, r.ReceiptMs, r.RecvMs)
	}
	return r, nil
}

type recordingQuoteSource struct {
	mu        sync.Mutex
	rec       *pairRecorder
	sources   map[string]string
	members   map[string]pairContractMember
	partition string
	need      pairBookNeed
	limit     int
	bound     string
	bindErr   error
	maxSteps  int
	egress    *EgressConfig
	polls     map[string][]pairPaperPoll
	collects  map[int64]string
	clock     func() time.Time
	timeout   time.Duration
}

func newRecordingQuoteSource(rec *pairRecorder, sources map[string]string, maxSteps int, egress *EgressConfig, clock func() time.Time) *recordingQuoteSource {
	return &recordingQuoteSource{rec: rec, sources: sources, maxSteps: maxSteps, egress: egress, clock: clock,
		polls: map[string][]pairPaperPoll{}, collects: map[int64]string{}}
}

func (s *recordingQuoteSource) Sources() map[string]string {
	out := make(map[string]string, len(s.sources))
	for k, v := range s.sources {
		out[k] = v
	}
	return out
}

func (s *recordingQuoteSource) Recorded() bool { return true }

func (s *recordingQuoteSource) bindContract(c basketContract) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fp := c.Fingerprint()
	if s.bound != "" {
		if s.bound != fp {
			return fmt.Errorf("the paper quote source is bound to contract %s, not %s", shortFingerprint(s.bound), shortFingerprint(fp))
		}
		return s.bindErr
	}
	s.bound = fp
	s.partition, s.need, s.members = c.Partition, c.Pair.BookNeed, map[string]pairContractMember{}
	s.limit = c.Pair.Limits.EnvelopeMaxBytes
	s.timeout = time.Duration(c.Pair.Limits.CollectionTimeoutMs) * time.Millisecond
	var errs []string
	for _, m := range c.pairMembersSorted() {
		s.members[m.MemberID] = m
		if m.Source != pairSourcePublicSnapshot {
			errs = append(errs, fmt.Sprintf("member %s source %q has no live paper producer; isolated paper records public_snapshot members only", m.MemberID, m.Source))
			continue
		}
		spec, ok := pairSourceSpecFor(m.Provider, m.Source)
		if !ok {
			errs = append(errs, fmt.Sprintf("member %s source %q is not a %s source", m.MemberID, m.Source, m.Provider))
			continue
		}
		payload := pairPayloadMaxBytes(spec, m, s.need)
		if got := pairPaperMaxQuotesBytes(s.maxSteps, payload); got > pairReplayMaxObjectBytes {
			errs = append(errs, fmt.Sprintf("member %s: %d steps of at most %d public records each, %d payload bytes worst case, can reach %d bytes, above the %d-byte object bound a sealed export must meet; lower bounds.max_steps", m.MemberID, s.maxSteps, pairPaperRecordsPerStep, payload, got, pairReplayMaxObjectBytes))
		}
	}
	if s.limit <= 0 {
		errs = append(errs, "the contract has no envelope_max_bytes to bound each public read")
	}
	if len(errs) > 0 {
		sort.Strings(errs)
		s.bindErr = fmt.Errorf("paper quote input refused: %s", strings.Join(errs, "; "))
	}
	return s.bindErr
}

func (s *recordingQuoteSource) member(id string) (pairContractMember, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.members[id]
	return m, ok && s.bound != "" && s.bindErr == nil
}

func (s *recordingQuoteSource) derive(m pairContractMember, r pairPaperReadResult, side string) (pairObservation, error) {
	s.mu.Lock()
	partition, need := s.partition, s.need
	s.mu.Unlock()
	o, err := pairObservationFromPayloadSide(m, partition, need, r.Payload, r.ReceiptMs, 0, side)
	if err != nil {
		return pairObservation{}, fmt.Errorf("member %s recorded public read does not derive: %w", m.MemberID, err)
	}
	return o, nil
}

func (s *recordingQuoteSource) checkRead(r pairMemberRead) error {
	m, ok := s.member(r.Member.MemberID)
	if !ok {
		return fmt.Errorf("the paper quote source is not bound to member %s", r.Member.MemberID)
	}
	if m.Source != r.Member.Source || m.Provider != r.Member.Provider || m.Instrument != r.Member.Instrument {
		return fmt.Errorf("member %s is recorded as %s %s %s", r.Member.MemberID, m.Provider, m.Instrument, m.Source)
	}
	if want := pairSourceContractFor(m.Provider, m.Source); want == "" || r.SourceContract != want {
		return fmt.Errorf("member %s source contract %q is not %q", m.MemberID, r.SourceContract, want)
	}
	return nil
}

type pairPaperCollectRequest struct {
	SlotMs     int64        `json:"slot_ms"`
	StartMs    int64        `json:"start_ms"`
	DeadlineMs int64        `json:"deadline_ms"`
	Members    []string     `json:"members"`
	BookNeed   pairBookNeed `json:"book_need"`
}

type pairPaperMemberRequest struct {
	MemberID   string `json:"member_id"`
	Provider   string `json:"provider"`
	Instrument string `json:"instrument"`
	Depth      int    `json:"depth"`
	Side       string `json:"side,omitempty"`
	StartMs    int64  `json:"start_ms,omitempty"`
	DeadlineMs int64  `json:"deadline_ms,omitempty"`
}

func (s *recordingQuoteSource) Collect(ctx context.Context, req pairCollectRequest) ([]pairObservation, error) {
	if len(req.Members) == 0 {
		return nil, fmt.Errorf("the collection request names no member")
	}
	cr := pairPaperCollectRequest{SlotMs: req.SlotMs, StartMs: req.StartMs, DeadlineMs: req.DeadlineMs, BookNeed: req.BookNeed}
	for _, m := range req.Members {
		if err := s.checkRead(pairMemberRead{Member: m, SourceContract: req.SourceContracts[m.MemberID]}); err != nil {
			return nil, err
		}
		cr.Members = append(cr.Members, m.MemberID)
	}
	reqBytes, err := json.Marshal(cr)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	limit, egress := s.limit, s.egress
	s.mu.Unlock()
	res, saved, err := s.rec.call(ctx, pairRecCall{Kind: pairRecKindCollect, SlotMs: req.SlotMs, Request: reqBytes}, func(ctx context.Context, h *pairRecHandle) pairRecResult {
		cctx, cancel := context.WithCancel(ctx)
		defer cancel()
		reads := make([]pairPaperMemberRead, len(req.Members))
		fails := make([]string, len(req.Members))
		classes := make([]string, len(req.Members))
		var wg sync.WaitGroup
		for i, m := range req.Members {
			wg.Add(1)
			go func(i int, m pairContractMember) {
				defer wg.Done()
				mr, _ := json.Marshal(pairPaperMemberRequest{MemberID: m.MemberID, Provider: m.Provider, Instrument: m.Instrument, Depth: req.BookNeed.Depth, StartMs: req.StartMs, DeadlineMs: req.DeadlineMs})
				var out pairRecResult
				child, callID, cerr := h.child(cctx, pairRecCall{Kind: pairRecKindMemberRead, MemberID: m.MemberID, SlotMs: req.SlotMs, Request: mr}, func(ctx context.Context) pairRecResult {
					out = pairPaperPublicRead(ctx, egress, m, req.BookNeed.Depth, limit)
					return out
				})
				switch {
				case cerr != nil:
					fails[i], classes[i] = cerr.Error(), pairRecClassInfra
				case child.Class != pairRecClassResult:
					fails[i], classes[i] = fmt.Sprintf("member %s public read: %s", m.MemberID, child.Cause), child.Class
				default:
					r, derr := pairPaperDecodeRead(child.Result)
					if derr != nil {
						fails[i], classes[i] = derr.Error(), pairRecClassInfra
					} else {
						reads[i] = pairPaperMemberRead{MemberID: m.MemberID, CallID: callID, Read: r}
						classes[i] = pairRecClassResult
					}
				}
				if fails[i] != "" {
					cancel()
				}
			}(i, m)
		}
		wg.Wait()
		var msgs []string
		class := pairRecClassResult
		for i, f := range fails {
			if f != "" {
				msgs = append(msgs, f)
				if classes[i] == pairRecClassInfra || class == pairRecClassResult {
					class = classes[i]
				}
			}
		}
		if len(msgs) > 0 {
			sort.Strings(msgs)
			if class == pairRecClassResult {
				class = pairRecClassInfra
			}
			return pairRecResult{Class: class, Cause: strings.Join(msgs, "; ")}
		}
		blob, merr := json.Marshal(pairPaperCollectResult{Members: reads})
		if merr != nil {
			return pairRecResult{Class: pairRecClassInfra, Cause: merr.Error()}
		}
		return pairRecResult{Class: pairRecClassResult, Result: blob}
	})
	if err != nil {
		return nil, err
	}
	if saved != nil {
		s.mu.Lock()
		s.collects[req.SlotMs] = saved.CallID
		s.mu.Unlock()
	}
	if res.Class != pairRecClassResult {
		return nil, fmt.Errorf("the recorded collection ended %s: %s", res.Class, res.Cause)
	}
	var cres pairPaperCollectResult
	if err := pairReplayStrictDecode(res.Result, &cres); err != nil {
		return nil, fmt.Errorf("the recorded collection is unreadable: %w", err)
	}
	if len(cres.Members) != len(req.Members) {
		return nil, fmt.Errorf("the recorded collection holds %d member reads for %d members", len(cres.Members), len(req.Members))
	}
	out := make([]pairObservation, 0, len(req.Members))
	for i, m := range req.Members {
		mr := cres.Members[i]
		if mr.MemberID != m.MemberID {
			return nil, fmt.Errorf("the recorded collection read %d names %s, not %s", i, mr.MemberID, m.MemberID)
		}
		if _, err := pairPaperDecodeRead(pairPaperJSON(mr.Read)); err != nil {
			return nil, err
		}
		o, err := s.derive(m, mr.Read, "")
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, nil
}

func pairPaperJSON(v interface{}) []byte {
	b, _ := json.Marshal(v)
	return b
}

func (s *recordingQuoteSource) collectCall(slotMs int64) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.collects[slotMs]
}

func (s *recordingQuoteSource) Reference(ctx context.Context, req pairReferenceRequest) (pairObservation, error) {
	if req.Side != "buy" && req.Side != "sell" {
		return pairObservation{}, fmt.Errorf("reference side %q is neither buy nor sell", req.Side)
	}
	if err := s.checkRead(pairMemberRead{Member: req.Member, SourceContract: req.SourceContract}); err != nil {
		return pairObservation{}, err
	}
	m := req.Member
	reqBytes, _ := json.Marshal(pairPaperMemberRequest{MemberID: m.MemberID, Provider: m.Provider, Instrument: m.Instrument, Depth: req.BookNeed.Depth, Side: req.Side, StartMs: req.StartMs, DeadlineMs: req.DeadlineMs})
	s.mu.Lock()
	limit, egress := s.limit, s.egress
	s.mu.Unlock()
	res, _, err := s.rec.call(ctx, pairRecCall{Kind: pairRecKindReference, MemberID: m.MemberID, Request: reqBytes}, func(ctx context.Context, h *pairRecHandle) pairRecResult {
		return pairPaperPublicRead(ctx, egress, m, req.BookNeed.Depth, limit)
	})
	if err != nil {
		return pairObservation{}, err
	}
	if res.Class != pairRecClassResult {
		return pairObservation{}, fmt.Errorf("the recorded %s reference read ended %s: %s", req.Side, res.Class, res.Cause)
	}
	r, err := pairPaperDecodeRead(res.Result)
	if err != nil {
		return pairObservation{}, err
	}
	o, err := s.derive(m, r, req.Side)
	if err != nil {
		return pairObservation{}, err
	}
	return o, nil
}

func (s *recordingQuoteSource) poll(ctx context.Context, memberID string) error {
	m, ok := s.member(memberID)
	if !ok {
		return fmt.Errorf("the paper quote source has no bound member %s", memberID)
	}
	s.mu.Lock()
	limit, egress, depth, timeout := s.limit, s.egress, s.need.Depth, s.timeout
	s.mu.Unlock()
	reqBytes, _ := json.Marshal(pairPaperMemberRequest{MemberID: m.MemberID, Provider: m.Provider, Instrument: m.Instrument, Depth: depth})
	res, saved, err := s.rec.call(ctx, pairRecCall{Kind: pairRecKindMatchPoll, MemberID: m.MemberID, Request: reqBytes}, func(ctx context.Context, h *pairRecHandle) pairRecResult {
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		return pairPaperPublicRead(ctx, egress, m, depth, limit)
	})
	if err != nil {
		return err
	}
	if res.Class != pairRecClassResult {
		return fmt.Errorf("the %s match poll ended %s: %s", memberID, res.Class, res.Cause)
	}
	r, err := pairPaperDecodeRead(res.Result)
	if err != nil {
		return err
	}
	o, err := s.derive(m, r, "")
	if err != nil {
		return err
	}
	s.addPoll(memberID, o, saved.End.LogicalMs, saved.CallID)
	return nil
}

func (s *recordingQuoteSource) addPoll(memberID string, o pairObservation, logical int64, callID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.polls[memberID] {
		if p.callID == callID {
			return
		}
	}
	s.polls[memberID] = append(s.polls[memberID], pairPaperPoll{obs: o, logical: logical, callID: callID})
}

func (s *recordingQuoteSource) newestPoll(memberID string, atMs int64) (pairPaperPoll, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var best *pairPaperPoll
	for i := range s.polls[memberID] {
		p := &s.polls[memberID][i]
		if p.logical > atMs || p.obs.FirstReceiptMs > atMs {
			continue
		}
		if best == nil || p.obs.FirstReceiptMs > best.obs.FirstReceiptMs || (p.obs.FirstReceiptMs == best.obs.FirstReceiptMs && p.logical > best.logical) {
			best = p
		}
	}
	if best == nil {
		return pairPaperPoll{}, false
	}
	return *best, true
}

type recordingVirtualMarket struct {
	src *recordingQuoteSource
}

func (m *recordingVirtualMarket) Events(ctx context.Context, key pairMemberKey, afterMs, throughMs int64) (pairVirtualMarketWindow, error) {
	if err := ctx.Err(); err != nil {
		return pairVirtualMarketWindow{}, err
	}
	s := m.src
	if now := s.clock().UnixMilli(); throughMs > now {
		throughMs = now
	}
	member, ok := s.member(key.MemberID)
	if !ok {
		return pairVirtualMarketWindow{}, fmt.Errorf("the recorded paper market has no bound member %s", key.MemberID)
	}
	if want := (pairMemberKey{Partition: key.Partition, Provider: member.Provider, AccountKey: member.AccountKey, Instrument: member.Instrument, MemberID: member.MemberID}); key != want {
		return pairVirtualMarketWindow{}, fmt.Errorf("the recorded paper market member key %s is not the admitted key %s", key, want)
	}
	spec, _ := pairSourceSpecFor(member.Provider, member.Source)
	cov := pairCoverage{SourceContract: spec.Contract, TimeSemantics: spec.TimeSemantics, IdentitySemantics: spec.IdentitySemantics,
		FromMs: afterMs, ThroughMs: throughMs, Status: pairCoverageIncomplete, Gaps: []pairCoverageGap{}}
	w := pairVirtualMarketWindow{Events: []pairObservation{}}
	if afterMs >= throughMs {
		cov.ThroughMs = cov.FromMs
		w.Coverage = cov
		return w, nil
	}
	s.mu.Lock()
	polls := append([]pairPaperPoll(nil), s.polls[member.MemberID]...)
	s.mu.Unlock()
	sort.SliceStable(polls, func(i, j int) bool { return polls[i].obs.FirstReceiptMs < polls[j].obs.FirstReceiptMs })
	for _, p := range polls {
		o := p.obs
		if o.VenueTimeMs == nil {
			continue
		}
		if v := *o.VenueTimeMs; v > afterMs && v <= throughMs && o.FirstReceiptMs <= throughMs && p.logical <= throughMs {
			w.Events = append(w.Events, pairCloneObservation(o))
		}
	}
	w.Coverage = cov
	return w, nil
}

func (s *recordingQuoteSource) mark(ctx context.Context, memberID string, atMs int64) (float64, error) {
	req, _ := json.Marshal(struct {
		MemberID string `json:"member_id"`
		AtMs     int64  `json:"at_ms"`
	}{memberID, atMs})
	res, _, err := s.rec.call(ctx, pairRecCall{Kind: pairRecKindMarks, MemberID: memberID, Request: req}, func(ctx context.Context, h *pairRecHandle) pairRecResult {
		p, ok := s.newestPoll(memberID, atMs)
		if !ok || len(p.obs.NativeBids) == 0 || len(p.obs.NativeAsks) == 0 {
			return pairRecResult{Class: pairRecClassInfra, Cause: fmt.Sprintf("no committed match poll of %s is visible at %d for its own management mark", memberID, atMs)}
		}
		mid, ok := pairReplayMid(p.obs.NativeBids[0].Px, p.obs.NativeAsks[0].Px)
		if !ok {
			return pairRecResult{Class: pairRecClassInfra, Cause: fmt.Sprintf("the newest committed match poll of %s has no positive mid", memberID)}
		}
		blob, _ := json.Marshal(struct {
			Mid        string `json:"mid"`
			PollCallID string `json:"poll_call_id"`
			ReceiptMs  int64  `json:"receipt_ms"`
		}{strconv.FormatFloat(mid, 'g', -1, 64), p.callID, p.obs.FirstReceiptMs})
		return pairRecResult{Class: pairRecClassResult, Result: blob}
	})
	if err != nil {
		return 0, err
	}
	if res.Class != pairRecClassResult {
		return 0, errors.New(res.Cause)
	}
	var out struct {
		Mid        string `json:"mid"`
		PollCallID string `json:"poll_call_id"`
		ReceiptMs  int64  `json:"receipt_ms"`
	}
	if err := pairReplayStrictDecode(res.Result, &out); err != nil {
		return 0, fmt.Errorf("the recorded management mark is unreadable: %w", err)
	}
	mid, err := strconv.ParseFloat(out.Mid, 64)
	if err != nil || !basketFinitePositive(mid) {
		return 0, fmt.Errorf("the recorded management mark %q is not a positive number", out.Mid)
	}
	return mid, nil
}

type pairPaperHistoryCandles struct {
	Provider   string       `json:"provider"`
	Instrument string       `json:"instrument"`
	Timeframe  string       `json:"timeframe"`
	Bars       [][6]float64 `json:"bars"`
	Forming    []float64    `json:"forming"`
	RequestMs  int64        `json:"request_ms"`
	ReceiptMs  int64        `json:"receipt_ms"`
}

func pairRunHistoryReader(ctx context.Context, egress *EgressConfig, args []string) pairRecResult {
	if err := pairPublicEgressReady(ctx, egress); err != nil {
		return pairRecResult{Class: pairRecClassInfra, Cause: processEgress.redact(err.Error())}
	}
	release, err := acquirePythonSlot(ctx)
	if err != nil {
		return pairRecResult{Class: pairClassFor(ctx, err), Cause: "waiting for a script slot: " + err.Error()}
	}
	defer release()
	env := map[string]string{"PYTHONDONTWRITEBYTECODE": "1"}
	for _, k := range pairQuoteReaderClearedEnv {
		env[k] = ""
	}
	start := pairPaperWall.Now()
	stdout, stderr, runErr := spawnPythonProcessWithEnvLimited(ctx, pairHistoryReaderScript, args, nil, 0, env, pythonOutputLimits{Stdout: pairPaperHistoryMaxOut, Stderr: pairRecMaxStderrBytes})
	recv := pairPaperWall.Now()
	res := pairRecResult{Stdout: stdout, Stderr: processEgress.redact(string(stderr)), ExitStatus: pairExitStatus(runErr)}
	fail := func(class, format string, a ...interface{}) pairRecResult {
		res.Class, res.Cause = class, processEgress.redact(fmt.Sprintf(format, a...))
		return res
	}
	var overflow *pythonOutputOverflowError
	if errors.As(runErr, &overflow) {
		res.Overflow = true
		return fail(pairRecClassInfra, "%v", runErr)
	}
	if err := ctx.Err(); err != nil {
		return fail(pairRecClassCancelled, "the history reader was cancelled: %v", err)
	}
	tree, perr := pairStrictParse(bytes.TrimSpace(stdout))
	if perr != nil {
		if runErr != nil {
			return fail(pairRecClassInfra, "the history reader failed: %v", runErr)
		}
		return fail(pairRecClassInfra, "the history reader returned unreadable output: %v", perr)
	}
	obj, ok := tree.(map[string]interface{})
	if !ok {
		return fail(pairRecClassInfra, "the history reader output is not an object")
	}
	if ok, _ := obj["ok"].(bool); !ok || runErr != nil {
		msg, _ := obj["error"].(string)
		return fail(pairRecClassInfra, "the history reader refused: %s", msg)
	}
	reqMs, err := pairTreeInt(obj["request_ms"], "request_ms", start.UnixMilli(), recv.UnixMilli())
	if err != nil {
		return fail(pairRecClassInfra, "%v", err)
	}
	receipt, err := pairTreeInt(obj["receipt_ms"], "receipt_ms", reqMs, recv.UnixMilli())
	if err != nil {
		return fail(pairRecClassInfra, "%v", err)
	}
	res.Class, res.ReceiptMs = pairRecClassResult, receipt
	return res
}

func pairPaperCandles(ctx context.Context, rec *pairRecorder, egress *EgressConfig, m pairContractMember, timeframe string) (pairPaperHistoryCandles, error) {
	limit := pairPaperCandleLimitHL
	if m.Provider == entryProviderOKX {
		limit = pairPaperCandleLimitOKX
	}
	args := []string{"--kind=candles", "--provider=" + m.Provider, "--instrument=" + m.Instrument, "--timeframe=" + timeframe, "--limit=" + strconv.Itoa(limit)}
	req, _ := json.Marshal(struct {
		Args []string `json:"args"`
	}{args})
	res, _, err := rec.call(ctx, pairRecCall{Kind: pairRecKindCandles, MemberID: m.MemberID, Request: req}, func(ctx context.Context, h *pairRecHandle) pairRecResult {
		ctx, cancel := context.WithTimeout(ctx, pairPaperHistoryTimeout)
		defer cancel()
		r := pairRunHistoryReader(ctx, egress, args)
		if r.Class != pairRecClassResult {
			return r
		}
		var out pairPaperHistoryCandles
		if err := pairPaperCheckCandles(r.Stdout, m, timeframe, r.ReceiptMs, &out); err != nil {
			r.Class, r.Cause = pairRecClassInfra, err.Error()
			return r
		}
		r.Result, _ = json.Marshal(out)
		r.Stdout = nil
		return r
	})
	var out pairPaperHistoryCandles
	if err != nil {
		return out, err
	}
	if res.Class != pairRecClassResult {
		return out, fmt.Errorf("the %s management candle read ended %s: %s", m.MemberID, res.Class, res.Cause)
	}
	if err := pairReplayStrictDecode(res.Result, &out); err != nil {
		return out, fmt.Errorf("the recorded management candles are unreadable: %w", err)
	}
	return out, nil
}

func pairPaperCheckCandles(stdout []byte, m pairContractMember, timeframe string, receiptMs int64, out *pairPaperHistoryCandles) error {
	var raw struct {
		OK         bool         `json:"ok"`
		Kind       string       `json:"kind"`
		Provider   string       `json:"provider"`
		Instrument string       `json:"instrument"`
		Timeframe  string       `json:"timeframe"`
		Bars       [][6]float64 `json:"bars"`
		Forming    []float64    `json:"forming"`
		RequestMs  int64        `json:"request_ms"`
		ReceiptMs  int64        `json:"receipt_ms"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(stdout), &raw); err != nil {
		return fmt.Errorf("the candle reader output is unreadable: %w", err)
	}
	if raw.Kind != "candles" || raw.Provider != m.Provider || raw.Instrument != m.Instrument || raw.Timeframe != timeframe {
		return fmt.Errorf("the candle reader answered %s %s %s %s, Go asked for candles %s %s %s", raw.Kind, raw.Provider, raw.Instrument, raw.Timeframe, m.Provider, m.Instrument, timeframe)
	}
	ivl, ok := hlCandleIntervalMs(timeframe)
	if !ok {
		return fmt.Errorf("timeframe %q is not a candle interval", timeframe)
	}
	prev := int64(-1)
	for i, b := range raw.Bars {
		open := int64(b[0])
		if float64(open) != b[0] || open <= 0 || open%ivl != 0 || open+ivl > receiptMs || (prev >= 0 && open <= prev) {
			return fmt.Errorf("candle %d opens at %v, which is not a closed %s bar after the previous one before the receipt %d", i, b[0], timeframe, receiptMs)
		}
		for _, v := range b[1:5] {
			if !basketFinitePositive(v) {
				return fmt.Errorf("candle %d has a nonpositive price", i)
			}
		}
		if b[5] < 0 || !pairReplayFinite(b[5]) {
			return fmt.Errorf("candle %d has an invalid volume", i)
		}
		prev = open
	}
	if len(raw.Forming) != 0 {
		if len(raw.Forming) != 2 || int64(raw.Forming[0])%ivl != 0 || int64(raw.Forming[0]) > receiptMs || !basketFinitePositive(raw.Forming[1]) || (prev >= 0 && int64(raw.Forming[0]) != prev+ivl) {
			return fmt.Errorf("the forming candle %v is not the open of the bar after the last closed bar", raw.Forming)
		}
	}
	*out = pairPaperHistoryCandles{Provider: raw.Provider, Instrument: raw.Instrument, Timeframe: raw.Timeframe, Bars: raw.Bars, Forming: raw.Forming, RequestMs: raw.RequestMs, ReceiptMs: raw.ReceiptMs}
	if out.Bars == nil {
		out.Bars = [][6]float64{}
	}
	return nil
}

type pairPaperOracleResponse struct {
	OK          bool                         `json:"ok"`
	Kind        string                       `json:"kind"`
	Provider    string                       `json:"provider"`
	Instrument  string                       `json:"instrument"`
	Host        string                       `json:"host"`
	Observation pairFundingOracleObservation `json:"observation"`
	RequestMs   int64                        `json:"request_ms"`
	ReceiptMs   int64                        `json:"receipt_ms"`
}

func pairFundingOracleDeadline(policy pairFundingPolicyV1, targetMs int64) (int64, error) {
	end, ok := pairAddMs(targetMs, policy.WindowMs)
	if !ok {
		return 0, fmt.Errorf("the oracle deadline overflows")
	}
	if end > policy.DrainThroughMs {
		end = policy.DrainThroughMs
	}
	return end, nil
}

func pairPaperCheckOracle(stdout []byte, key pairEvidenceKey, policy pairFundingPolicyV1, targetMs, deadlineMs, receiptMs int64) (pairFundingOracleObservation, error) {
	var raw pairPaperOracleResponse
	if err := pairReplayStrictDecode(bytes.TrimSpace(stdout), &raw); err != nil {
		return raw.Observation, fmt.Errorf("the oracle reader output is unreadable: %w", err)
	}
	deadline, err := pairFundingOracleDeadline(policy, targetMs)
	if err != nil {
		return raw.Observation, err
	}
	if !raw.OK || raw.Kind != "funding_oracle" || raw.Provider != entryProviderHyperliquid || raw.Instrument != key.Instrument || raw.Host != pairHLPublicHost {
		return raw.Observation, fmt.Errorf("the oracle reader response does not match the requested public Hyperliquid instrument")
	}
	if deadlineMs != deadline || raw.RequestMs != raw.Observation.RequestMs || raw.ReceiptMs != raw.Observation.ReceiptMs || receiptMs != raw.ReceiptMs {
		return raw.Observation, fmt.Errorf("the oracle reader response times do not match its observation and parent receipt")
	}
	o := raw.Observation
	if o.Key != key || o.TargetEventMs != targetMs {
		return o, fmt.Errorf("the oracle observation names another member or target")
	}
	if err := pairFundingValidateObservation(o, policy); err != nil {
		return o, err
	}
	if o.RequestMs < targetMs || o.ReceiptMs < o.RequestMs || o.ReceiptMs > deadlineMs {
		return o, fmt.Errorf("the oracle observation is outside [%d, %d]", targetMs, deadlineMs)
	}
	return o, nil
}

type recordingFundingSource struct {
	rec    *pairRecorder
	src    *pairRecordedFundingSource
	egress *EgressConfig
}

type pairPaperOracleRequest struct {
	Args          []string        `json:"args"`
	Key           pairEvidenceKey `json:"key"`
	SourceID      string          `json:"source_id"`
	Seq           int64           `json:"seq"`
	PrevHash      string          `json:"prev_hash"`
	TargetEventMs int64           `json:"target_event_ms"`
	PolicyDigest  string          `json:"policy_digest"`
	AttemptID     string          `json:"attempt_id"`
}

func pairPaperOracleArgs(key pairEvidenceKey, policy pairFundingPolicyV1, targetMs, deadlineMs int64, digest string) ([]string, error) {
	keyJSON, err := pairCanonicalJSON(key)
	if err != nil {
		return nil, err
	}
	policyJSON, err := pairCanonicalJSON(policy)
	if err != nil {
		return nil, err
	}
	return []string{"--kind=funding_oracle", "--provider=" + key.Provider, "--instrument=" + key.Instrument, "--member-key-json=" + string(keyJSON),
		"--target-event-ms=" + strconv.FormatInt(targetMs, 10), "--not-before-ms=" + strconv.FormatInt(targetMs, 10), "--deadline-ms=" + strconv.FormatInt(deadlineMs, 10),
		"--funding-policy-json=" + string(policyJSON), "--policy-digest=" + digest}, nil
}

func (f recordingFundingSource) readOracle(ctx context.Context, key pairEvidenceKey, seq int64, prev string, targetMs int64) ([]byte, *pairRecSaved, error) {
	if f.src.policy == nil {
		return nil, nil, fmt.Errorf("an oracle read requires a recorded research policy")
	}
	policy := *f.src.policy
	digest, err := pairFundingPolicyDigest(policy)
	if err != nil {
		return nil, nil, err
	}
	deadline, err := pairFundingOracleDeadline(policy, targetMs)
	if err != nil {
		return nil, nil, err
	}
	args, err := pairPaperOracleArgs(key, policy, targetMs, deadline, digest)
	if err != nil {
		return nil, nil, err
	}
	attemptID, err := pairFundingOracleAttemptID(key, targetMs, digest)
	if err != nil {
		return nil, nil, err
	}
	request := pairPaperOracleRequest{Args: args, Key: key, SourceID: f.src.SourceID(), Seq: seq, PrevHash: prev, TargetEventMs: targetMs, PolicyDigest: digest, AttemptID: attemptID}
	req, err := pairCanonicalJSON(request)
	if err != nil {
		return nil, nil, err
	}
	res, saved, err := f.rec.call(ctx, pairRecCall{Kind: pairRecKindFundingOracle, MemberID: key.MemberID, SlotMs: targetMs, Request: req}, func(ctx context.Context, h *pairRecHandle) pairRecResult {
		remaining := time.UnixMilli(deadline).Sub(pairPaperWall.Now())
		if remaining <= 0 {
			return pairRecResult{Class: pairRecClassCancelled, Cause: "the oracle sampling deadline elapsed before the reader launch"}
		}
		callCtx, cancel := context.WithDeadline(ctx, time.UnixMilli(deadline))
		defer cancel()
		r := pairRunHistoryReader(callCtx, f.egress, args)
		if r.Class != pairRecClassResult {
			return r
		}
		observation, checkErr := pairPaperCheckOracle(r.Stdout, key, policy, targetMs, deadline, r.ReceiptMs)
		if checkErr != nil {
			r.Class, r.Cause = pairRecClassInfra, checkErr.Error()
			return r
		}
		batch, sealErr := (pairFundingBatchV2{V: pairFundingResearchBatchVersion, SourceID: f.src.SourceID(), Seq: seq, PrevHash: prev, PolicyDigest: digest,
			Rates: []pairFundingRate{}, Prices: []pairFundingPrice{}, Observations: []pairFundingOracleObservation{observation}, Coverage: []pairFundingCoverageRecord{}}).sealed()
		if sealErr != nil {
			r.Class, r.Cause = pairRecClassInfra, sealErr.Error()
			return r
		}
		r.Result, sealErr = pairFundingBatchValueBytes(pairFundingBatchV2Value(batch))
		if sealErr != nil {
			r.Class, r.Cause = pairRecClassInfra, sealErr.Error()
			return r
		}
		r.Stdout = nil
		return r
	})
	if err != nil {
		return nil, saved, err
	}
	if res.Class != pairRecClassResult {
		return nil, saved, fmt.Errorf("the %s oracle read ended %s: %s", key.MemberID, res.Class, res.Cause)
	}
	if saved == nil || saved.End == nil {
		return nil, saved, fmt.Errorf("the oracle result has no committed terminal")
	}
	if err := pairPaperCheckFundingTerminal(f.src, res.Result, saved.Start, saved.End); err != nil {
		return nil, saved, err
	}
	return res.Result, saved, nil
}

func pairPaperCheckFundingTerminal(src *pairRecordedFundingSource, raw []byte, start pairRecStart, end *pairRecEnd) error {
	if end == nil || end.Class != pairRecClassResult || !bytes.Equal(end.Result, raw) {
		return fmt.Errorf("the recorded funding result has no matching committed terminal")
	}
	if start.Async {
		return fmt.Errorf("an asynchronous oracle observation is published only by its consumption event")
	}
	b, err := pairDecodeFundingBatch(raw)
	if err != nil {
		return err
	}
	if b.SourceID != src.SourceID() {
		return fmt.Errorf("the recorded funding batch belongs to another source")
	}
	if start.Kind == pairRecKindFundingOracle {
		if src.policy == nil || b.Research == nil || len(b.Research.Observations) != 1 || len(b.Rates) != 0 || len(b.Prices) != 0 || len(b.Coverage) != 0 {
			return fmt.Errorf("the oracle terminal is outside the research batch contract")
		}
		var req pairPaperOracleRequest
		if err := pairReplayStrictDecode(start.Request, &req); err != nil {
			return fmt.Errorf("the oracle request is unreadable: %w", err)
		}
		digest, _ := pairFundingPolicyDigest(*src.policy)
		attemptID, _ := pairFundingOracleAttemptID(req.Key, req.TargetEventMs, digest)
		deadline, err := pairFundingOracleDeadline(*src.policy, req.TargetEventMs)
		if err != nil {
			return err
		}
		args, _ := pairPaperOracleArgs(req.Key, *src.policy, req.TargetEventMs, deadline, digest)
		want, _ := pairCanonicalJSON(pairPaperOracleRequest{Args: args, Key: req.Key, SourceID: src.SourceID(), Seq: b.Seq, PrevHash: b.PrevHash, TargetEventMs: req.TargetEventMs, PolicyDigest: digest, AttemptID: attemptID})
		if !bytes.Equal(want, start.Request) || b.Research.PolicyDigest != digest || b.Seq != req.Seq || b.PrevHash != req.PrevHash {
			return fmt.Errorf("the oracle terminal does not match its committed request and policy")
		}
		o := b.Research.Observations[0]
		if o.Key != req.Key || o.TargetEventMs != req.TargetEventMs || o.AttemptID != req.AttemptID || end.ClockBackward || end.RawCompletionMs < o.ReceiptMs || end.RawCompletionMs > deadline || end.RawReceiptMs != o.ReceiptMs {
			return fmt.Errorf("the oracle terminal times or identity are ineligible")
		}
		return pairFundingValidateObservation(o, *src.policy)
	}
	return nil
}

func pairPaperAdmitFundingResult(src *pairRecordedFundingSource, raw []byte, start pairRecStart, end *pairRecEnd) error {
	if err := pairPaperCheckFundingTerminal(src, raw, start, end); err != nil {
		return err
	}
	b, err := pairDecodeFundingBatch(raw)
	if err != nil {
		return err
	}
	if held := src.chainRaw(b.Seq); held != nil && bytes.Equal(held, raw) {
		return nil
	}
	return src.Append(raw)
}

func (f recordingFundingSource) read(ctx context.Context, key pairEvidenceKey, seq int64, prev string, fromMs, throughMs int64) ([]byte, error) {
	rec, egress, sourceID := f.rec, f.egress, f.src.SourceID()
	keyJSON, err := pairCanonicalJSON(key)
	if err != nil {
		return nil, err
	}
	args := []string{"--kind=funding", "--provider=" + key.Provider, "--instrument=" + key.Instrument, "--from-ms=" + strconv.FormatInt(fromMs, 10), "--through-ms=" + strconv.FormatInt(throughMs, 10),
		"--member-key-json=" + string(keyJSON), "--source-id=" + sourceID, "--seq=" + strconv.FormatInt(seq, 10), "--prev-hash=" + prev}
	policyDigest := ""
	if f.src.policy != nil {
		rule, ok := pairFundingSelectedRule(f.src.policy, key)
		if !ok {
			return nil, fmt.Errorf("member %s has no selected funding rule", key.MemberID)
		}
		policyJSON, err := pairCanonicalJSON(*f.src.policy)
		if err != nil {
			return nil, err
		}
		policyDigest, err = pairFundingPolicyDigest(*f.src.policy)
		if err != nil {
			return nil, err
		}
		args = append(args, "--funding-policy-json="+string(policyJSON), "--policy-digest="+policyDigest, "--rule-version="+rule.RuleVersion)
	}
	req, _ := json.Marshal(struct {
		Args []string `json:"args"`
	}{args})
	res, _, err := rec.call(ctx, pairRecCall{Kind: pairRecKindFunding, MemberID: key.MemberID, Request: req}, func(ctx context.Context, h *pairRecHandle) pairRecResult {
		ctx, cancel := context.WithTimeout(ctx, pairPaperHistoryTimeout)
		defer cancel()
		r := pairRunHistoryReader(ctx, egress, args)
		if r.Class != pairRecClassResult {
			return r
		}
		var raw struct {
			Batch string `json:"batch"`
		}
		if err := json.Unmarshal(bytes.TrimSpace(r.Stdout), &raw); err != nil || raw.Batch == "" {
			r.Class, r.Cause = pairRecClassInfra, fmt.Sprintf("the funding reader output carries no batch: %v", err)
			return r
		}
		b, err := pairDecodeFundingBatch([]byte(raw.Batch))
		if err != nil {
			r.Class, r.Cause = pairRecClassInfra, "the funding reader batch is refused: "+err.Error()
			return r
		}
		if b.SourceID != sourceID || b.Seq != seq || b.PrevHash != prev {
			r.Class, r.Cause = pairRecClassInfra, fmt.Sprintf("the funding reader sealed batch %s/%d after %s, Go asked for %s/%d after %s", b.SourceID, b.Seq, shortFingerprint(b.PrevHash), sourceID, seq, shortFingerprint(prev))
			return r
		}
		if (f.src.policy == nil) != (b.Research == nil) || b.Research != nil && (b.Research.PolicyDigest != policyDigest || len(b.Research.Observations) != 0) {
			r.Class, r.Cause = pairRecClassInfra, "the funding reader batch version, policy or observations do not match the source"
			return r
		}
		for _, x := range b.Rates {
			if x.Key != key || x.VisibleMs > r.ReceiptMs {
				r.Class, r.Cause = pairRecClassInfra, fmt.Sprintf("funding rate %s names %s or is visible after the read receipt %d", x.EventID, x.Key, r.ReceiptMs)
				return r
			}
		}
		for _, x := range b.Prices {
			if x.Key != key || x.VisibleMs > r.ReceiptMs {
				r.Class, r.Cause = pairRecClassInfra, fmt.Sprintf("funding price %s names %s or is visible after the read receipt %d", x.EvidenceID, x.Key, r.ReceiptMs)
				return r
			}
			if f.src.policy != nil {
				if selected, ok := pairFundingSelectedRule(f.src.policy, key); !ok || x.PriceKind != selected.PriceKind {
					r.Class, r.Cause = pairRecClassInfra, fmt.Sprintf("funding price %s is not the %s member's selected price evidence", x.EvidenceID, key.MemberID)
					return r
				}
			}
		}
		for _, x := range b.Coverage {
			if x.Key != key || x.VisibleMs > r.ReceiptMs {
				r.Class, r.Cause = pairRecClassInfra, fmt.Sprintf("funding coverage %s names %s or is visible after the read receipt %d", x.EvidenceID, x.Key, r.ReceiptMs)
				return r
			}
		}
		r.Result = []byte(raw.Batch)
		r.Stdout = nil
		return r
	})
	if err != nil {
		return nil, err
	}
	if res.Class != pairRecClassResult {
		return nil, fmt.Errorf("the %s funding read ended %s: %s", key.MemberID, res.Class, res.Cause)
	}
	return res.Result, nil
}

type recordingEvidenceSource struct {
	rec    *pairRecorder
	src    *pairRecordedEvidenceSource
	egress *EgressConfig
}

func (e recordingEvidenceSource) refresh(ctx context.Context, key pairEvidenceKey, prev *pairCapabilityEvidence, validForMs int64) (pairCapabilityEvidence, int64, error) {
	rec, egress := e.rec, e.egress
	prevID := ""
	if prev != nil {
		prevID = prev.EvidenceID
	}
	req, _ := json.Marshal(struct {
		Key        pairEvidenceKey `json:"key"`
		PrevID     string          `json:"prev_evidence_id"`
		ValidForMs int64           `json:"valid_for_ms"`
	}{key, prevID, validForMs})
	res, saved, err := rec.call(ctx, pairRecCall{Kind: pairRecKindCapability, MemberID: key.MemberID, Request: req}, func(ctx context.Context, h *pairRecHandle) pairRecResult {
		ctx, cancel := context.WithTimeout(ctx, pairPaperHistoryTimeout)
		defer cancel()
		if err := pairPublicEgressReady(ctx, egress); err != nil {
			return pairRecResult{Class: pairRecClassInfra, Cause: processEgress.redact(err.Error())}
		}
		c, stdout, stderr, runErr := pairReadPublicCapabilityRaw(ctx, key, prev, validForMs)
		r := pairRecResult{Stderr: processEgress.redact(string(stderr)), ExitStatus: pairExitStatus(runErr)}
		if runErr != nil {
			r.Class, r.Cause, r.Stdout = pairClassFor(ctx, runErr), processEgress.redact(runErr.Error()), stdout
			return r
		}
		blob, err := json.Marshal(c)
		if err != nil {
			r.Class, r.Cause = pairRecClassInfra, err.Error()
			return r
		}
		r.Class, r.Result, r.ReceiptMs = pairRecClassResult, blob, c.FirstReceiptMs
		return r
	})
	if err != nil {
		return pairCapabilityEvidence{}, 0, err
	}
	if res.Class != pairRecClassResult {
		return pairCapabilityEvidence{}, 0, fmt.Errorf("the %s capability refresh ended %s: %s", key.MemberID, res.Class, res.Cause)
	}
	var c pairCapabilityEvidence
	if err := pairReplayStrictDecode(res.Result, &c); err != nil {
		return pairCapabilityEvidence{}, 0, fmt.Errorf("the recorded capability is unreadable: %w", err)
	}
	return c, saved.End.LogicalMs, nil
}

func pairNativeCheckProjection(stdout []byte) []byte {
	dec := json.NewDecoder(bytes.NewReader(bytes.TrimSpace(stdout)))
	dec.UseNumber()
	var obj map[string]interface{}
	if err := dec.Decode(&obj); err != nil || dec.More() {
		return append([]byte("raw:"), stdout...)
	}
	delete(obj, "timestamp")
	b, err := json.Marshal(obj)
	if err != nil {
		return append([]byte("raw:"), stdout...)
	}
	return b
}

type recordingManagementCheck struct {
	r        *pairReplayRuntime
	memberID string
}

func (m recordingManagementCheck) run(script string, args []string, stdin []byte) ([]byte, []byte, error) {
	r, memberID := m.r, m.memberID
	req, _ := json.Marshal(struct {
		Script string   `json:"script"`
		Args   []string `json:"args"`
		Stdin  []byte   `json:"stdin"`
	}{script, args, stdin})
	res, saved, err := r.rec.call(context.Background(), pairRecCall{Kind: pairRecKindNativeCheck, MemberID: memberID, Request: req}, func(ctx context.Context, h *pairRecHandle) pairRecResult {
		stdout, stderr, runErr := runPythonReadOnlyWithStdin(script, args, stdin)
		out := pairRecResult{Stdout: stdout, Stderr: processEgress.redact(string(stderr)), ExitStatus: pairExitStatus(runErr), Result: pairNativeCheckProjection(stdout)}
		switch {
		case len(stdout) > pairRecMaxStdoutBytes:
			out.Class, out.Cause, out.Overflow = pairRecClassInfra, fmt.Sprintf("the member check wrote %d bytes, above the %d-byte recording bound", len(stdout), pairRecMaxStdoutBytes), true
		case runErr != nil:
			out.Class, out.Cause = pairRecClassInfra, runErr.Error()
		default:
			out.Class = pairRecClassResult
		}
		return out
	})
	if err != nil {
		r.recordFailure(pairReplayFailure{Stage: pairReplayStageManagement, Kind: pairReplayKindNativeCheck, MemberID: memberID, Cause: "the member check call has no usable recorded outcome: " + err.Error()})
		return nil, nil, err
	}
	if saved != nil {
		r.lastNativeCall = saved.CallID
	}
	if res.Overflow && res.Class != pairRecClassResult {
		return nil, []byte(res.Stderr), errors.New(res.Cause)
	}
	if res.Class != pairRecClassResult {
		return res.Stdout, []byte(res.Stderr), errors.New(res.Cause)
	}
	return res.Stdout, []byte(res.Stderr), nil
}

type recordingPolicyRunner struct {
	r *pairReplayRuntime
}

func (p recordingPolicyRunner) run(ctx context.Context, stdin []byte, timeout time.Duration) ([]byte, []byte, error) {
	r := p.r
	var head struct {
		SlotMs int64 `json:"slot_ms"`
	}
	_ = json.Unmarshal(stdin, &head)
	res, saved, err := r.rec.call(ctx, pairRecCall{Kind: pairRecKindPolicy, SlotMs: head.SlotMs, Request: stdin}, func(ctx context.Context, h *pairRecHandle) pairRecResult {
		out, errb, runErr := runPairPolicyProcess(ctx, stdin, timeout)
		res := pairRecResult{Result: out, Stderr: processEgress.redact(string(errb)), ExitStatus: pairExitStatus(runErr), Class: pairClassFor(ctx, runErr)}
		if runErr != nil {
			res.Cause = runErr.Error()
		}
		return res
	})
	callID := ""
	if saved != nil {
		callID = saved.CallID
	}
	if err != nil {
		r.recordFailure(pairReplayFailure{Stage: pairReplayStagePolicy, Kind: pairReplayKindProcess, SlotMs: head.SlotMs, Cause: "the policy call has no usable recorded outcome: " + err.Error(), CallID: callID})
		return nil, nil, err
	}
	var runErr error
	switch res.Class {
	case pairRecClassResult:
		if _, derr := pairDecodePolicyResult(res.Result); derr != nil {
			r.recordFailure(pairReplayFailure{Stage: pairReplayStagePolicy, Kind: pairReplayKindProcess, SlotMs: head.SlotMs, Cause: "the policy process output is unreadable: " + derr.Error(), CallID: callID})
		}
	case pairRecClassCancelled:
		runErr = pairRecCauseError(res.Cause)
		r.recordFailure(pairReplayFailure{Stage: pairReplayStagePolicy, Kind: pairReplayKindProcess, SlotMs: head.SlotMs, Cause: "the policy context ended: " + res.Cause, CallID: callID})
	default:
		runErr = errors.New(res.Cause)
		r.recordFailure(pairReplayFailure{Stage: pairReplayStagePolicy, Kind: pairReplayKindProcess, SlotMs: head.SlotMs, Cause: "the canonical policy runner failed: " + res.Cause, CallID: callID})
	}
	return res.Result, []byte(res.Stderr), runErr
}
