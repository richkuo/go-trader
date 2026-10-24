package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"trading-scheduler/internal/egresspolicy"
)

var pairQuoteReaderScript = "shared_scripts/fetch_pair_quotes.py"

var pairQuoteReaderClearedEnv = []string{
	"HYPERLIQUID_SECRET_KEY", "HYPERLIQUID_ACCOUNT_ADDRESS",
	"OKX_API_KEY", "OKX_API_SECRET", "OKX_PASSPHRASE",
	"HYPERLIQUID_TESTNET", "OKX_SANDBOX",
}

var pairQuoteHLPublicProbeArgv = []string{"--provider=hyperliquid", "--source=public_snapshot", "--instrument=BTC", "--depth=20", "--probe-only"}

var pairQuoteOKXPublicProbeArgv = []string{"--provider=okx", "--source=public_snapshot", "--instrument=BTC-USDT-SWAP", "--depth=20", "--probe-only"}

var pairQuoteHLArchiveProbeArgv = []string{"--provider=hyperliquid", "--source=archive", "--instrument=BTC", "--depth=20", "--window-ms=60000", "--capacity=4096",
	"--archive-dir=/nonexistent/go-trader-probe", "--cutoff-ms=1", "--as-of-ms=1", "--probe-only"}

type pairMemberRead struct {
	Partition      RiskPartition
	Member         pairContractMember
	BookNeed       pairBookNeed
	SourceContract string
	StartMs        int64
	Snapshot       *marketSnapshot
	SnapshotID     string
	Generation     uint64
	OutputLimit    int
	Side           string
}

type pairMemberReader interface {
	readQuote(ctx context.Context, r pairMemberRead) (pairObservation, error)
}

type pairQuoteCollector struct {
	sources map[string]string
	readers map[string]pairMemberReader
	limit   int
}

type pairQuoteCollectorOptions struct {
	Egress      *EgressConfig
	ArchiveDirs map[string]string
}

func newPairQuoteCollector(p PairConfig, limits pairOperationalLimits, opts pairQuoteCollectorOptions) (*pairQuoteCollector, error) {
	c := &pairQuoteCollector{sources: map[string]string{}, readers: map[string]pairMemberReader{}, limit: limits.EnvelopeMaxBytes}
	if c.limit <= 0 || c.limit > pairMaxEnvelopeBytes {
		return nil, fmt.Errorf("pair quote collection needs envelope_max_bytes in [1, %d]", pairMaxEnvelopeBytes)
	}
	for _, m := range p.Members {
		id := strings.TrimSpace(m.StrategyID)
		if id == "" {
			return nil, fmt.Errorf("pair %s lists a member with no strategy id", p.ID)
		}
		if _, dup := c.sources[id]; dup {
			return nil, fmt.Errorf("pair %s lists member %s twice", p.ID, id)
		}
		c.sources[id] = m.Source
		switch m.Source {
		case pairSourceSealedFeed:
			c.readers[id] = pairSealedReader{}
		case pairSourcePublicSnapshot:
			c.readers[id] = pairPublicReader{egress: opts.Egress}
		case pairSourceArchive:
			dir := opts.ArchiveDirs[id]
			if dir == "" || !filepath.IsAbs(dir) {
				return nil, fmt.Errorf("pair %s archive member %s needs an absolute prepared archive directory", p.ID, id)
			}
			c.readers[id] = pairArchiveReader{dir: filepath.Clean(dir)}
		default:
			return nil, fmt.Errorf("pair %s member %s source %q has no quote reader", p.ID, id, m.Source)
		}
	}
	for id := range opts.ArchiveDirs {
		if c.sources[id] != pairSourceArchive {
			return nil, fmt.Errorf("pair %s names an archive directory for %s, which is not an archive member", p.ID, id)
		}
	}
	return c, nil
}

func (c *pairQuoteCollector) Sources() map[string]string {
	out := make(map[string]string, len(c.sources))
	for k, v := range c.sources {
		out[k] = v
	}
	return out
}

func (c *pairQuoteCollector) Recorded() bool { return false }

func (c *pairQuoteCollector) read(ctx context.Context, r pairMemberRead) (pairObservation, error) {
	reader, ok := c.readers[r.Member.MemberID]
	if !ok || c.sources[r.Member.MemberID] != r.Member.Source {
		return pairObservation{}, fmt.Errorf("no %s quote reader for member %s", r.Member.Source, r.Member.MemberID)
	}
	if want := pairSourceContractFor(r.Member.Provider, r.Member.Source); want == "" || r.SourceContract != want {
		return pairObservation{}, fmt.Errorf("member %s source contract %q is not %q", r.Member.MemberID, r.SourceContract, want)
	}
	r.OutputLimit = c.limit
	if err := ctx.Err(); err != nil {
		return pairObservation{}, err
	}
	obs, err := reader.readQuote(ctx, r)
	if err != nil {
		return pairObservation{}, fmt.Errorf("member %s %s read: %w", r.Member.MemberID, r.Member.Source, err)
	}
	if err := ctx.Err(); err != nil {
		return pairObservation{}, err
	}
	return obs, nil
}

func (c *pairQuoteCollector) Collect(ctx context.Context, req pairCollectRequest) ([]pairObservation, error) {
	if len(req.Members) == 0 {
		return nil, fmt.Errorf("the collection request names no member")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	out := make([]pairObservation, len(req.Members))
	errs := make([]error, len(req.Members))
	var wg sync.WaitGroup
	for i, m := range req.Members {
		wg.Add(1)
		go func(i int, m pairContractMember) {
			defer wg.Done()
			out[i], errs[i] = c.read(ctx, pairMemberRead{Partition: req.Partition, Member: m, BookNeed: req.BookNeed, SourceContract: req.SourceContracts[m.MemberID],
				StartMs: req.StartMs, Snapshot: req.Snapshot, SnapshotID: req.SnapshotID, Generation: req.Generation})
			if errs[i] != nil {
				cancel()
			}
		}(i, m)
	}
	wg.Wait()
	var msgs []string
	for _, err := range errs {
		if err != nil {
			msgs = append(msgs, err.Error())
		}
	}
	if len(msgs) > 0 {
		sort.Strings(msgs)
		return nil, fmt.Errorf("%s", strings.Join(msgs, "; "))
	}
	return out, nil
}

func (c *pairQuoteCollector) Reference(ctx context.Context, req pairReferenceRequest) (pairObservation, error) {
	if req.Side != "buy" && req.Side != "sell" {
		return pairObservation{}, fmt.Errorf("reference side %q is neither buy nor sell", req.Side)
	}
	return c.read(ctx, pairMemberRead{Partition: req.Partition, Member: req.Member, BookNeed: req.BookNeed, SourceContract: req.SourceContract,
		StartMs: req.StartMs, Snapshot: req.Snapshot, SnapshotID: req.SnapshotID, Generation: req.Generation, Side: req.Side})
}

type pairSealedReader struct{}

func (pairSealedReader) readQuote(ctx context.Context, r pairMemberRead) (pairObservation, error) {
	if r.Snapshot == nil {
		return pairObservation{}, fmt.Errorf("no sealed market snapshot was captured; a sealed member holds and no public read replaces it")
	}
	if r.SnapshotID == "" || r.Snapshot.EvaluationID != r.SnapshotID || r.Snapshot.ConfigGeneration != r.Generation {
		return pairObservation{}, fmt.Errorf("the sealed snapshot %q generation %d is not the captured snapshot %q generation %d", r.Snapshot.EvaluationID, r.Snapshot.ConfigGeneration, r.SnapshotID, r.Generation)
	}
	need := contractBookNeed(r.BookNeed.WindowMs, r.BookNeed.Capacity, r.BookNeed.Depth)
	p := r.Snapshot.l2BookPayload(l2BookKeyFor(r.Member.Instrument), need)
	if !p.Available {
		return pairObservation{}, fmt.Errorf("the sealed l2_book section is unavailable (%s); a failed seal is never replaced", p.Reason)
	}
	raw, err := pairCanonicalJSON(p)
	if err != nil {
		return pairObservation{}, err
	}
	return pairObservationFromPayloadSide(r.Member, r.Partition.String(), r.BookNeed, raw, 0, r.Generation, r.Side)
}

func pairPublicEgressReady(ctx context.Context, cfg *EgressConfig) error {
	if err := establishProcessEgress(ctx, cfg, true); err != nil {
		return err
	}
	want := os.Getenv(egresspolicy.EnvProxy) != "" || (cfg != nil && cfg.ProxyRequired)
	r := processEgress
	r.mu.Lock()
	active, attested, attestErr := r.active, r.attestation != nil, r.attestError
	r.mu.Unlock()
	if !want && !active {
		return nil
	}
	if !active || !attested || attestErr != "" {
		return egressRefusal("proxy egress is required for public quote reads, but the process has no active enforcement attestation (active %v, attested %v, lost %q); no direct connection is attempted", active, attested, attestErr)
	}
	return nil
}

type pairReaderResult struct {
	obj       map[string]interface{}
	payload   []byte
	receiveMs int64
	startMs   int64
}

func pairRunQuoteReader(ctx context.Context, args []string, limit int) (pairReaderResult, error) {
	release, err := acquirePythonSlot(ctx)
	if err != nil {
		return pairReaderResult{}, fmt.Errorf("waiting for a script slot: %w", err)
	}
	defer release()
	env := map[string]string{"PYTHONDONTWRITEBYTECODE": "1"}
	for _, k := range pairQuoteReaderClearedEnv {
		env[k] = ""
	}
	start := time.Now()
	stdout, stderr, runErr := spawnPythonProcessWithEnvLimited(ctx, pairQuoteReaderScript, args, nil, 0, env, pythonOutputLimits{Stdout: limit, Stderr: limit})
	recv := time.Now()
	if runErr != nil {
		if _, overflow := runErr.(*pythonOutputOverflowError); overflow {
			return pairReaderResult{}, runErr
		}
	}
	if err := ctx.Err(); err != nil {
		return pairReaderResult{}, fmt.Errorf("the quote reader was cancelled: %w", err)
	}
	tree, perr := pairStrictParse(bytes.TrimSpace(stdout))
	if perr != nil {
		if runErr != nil {
			return pairReaderResult{}, fmt.Errorf("the quote reader failed: %v (%s)", runErr, processEgress.redact(strings.TrimSpace(string(stderr))))
		}
		return pairReaderResult{}, fmt.Errorf("the quote reader returned unreadable output: %w", perr)
	}
	obj, ok := tree.(map[string]interface{})
	if !ok {
		return pairReaderResult{}, fmt.Errorf("the quote reader output is not a JSON object")
	}
	if ok, _ := obj["ok"].(bool); !ok {
		msg, _ := obj["error"].(string)
		return pairReaderResult{}, fmt.Errorf("the quote reader refused: %s", msg)
	}
	if runErr != nil {
		return pairReaderResult{}, fmt.Errorf("the quote reader failed: %v", runErr)
	}
	pnode, ok := obj["payload"]
	if !ok {
		return pairReaderResult{}, fmt.Errorf("the quote reader output has no payload")
	}
	var buf bytes.Buffer
	if err := pairWriteCanonical(&buf, pnode); err != nil {
		return pairReaderResult{}, fmt.Errorf("the quote reader payload: %w", err)
	}
	return pairReaderResult{obj: obj, payload: buf.Bytes(), receiveMs: recv.UnixMilli(), startMs: start.UnixMilli()}, nil
}

func pairReaderEcho(obj map[string]interface{}, want map[string]string) error {
	keys := make([]string, 0, len(want))
	for k := range want {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		got, _ := obj[k].(string)
		if got != want[k] {
			return fmt.Errorf("the quote reader answered %s %q, Go asked for %q", k, got, want[k])
		}
	}
	return nil
}

type pairPublicReader struct {
	egress *EgressConfig
}

func (p pairPublicReader) readQuote(ctx context.Context, r pairMemberRead) (pairObservation, error) {
	if _, ok := ctx.Deadline(); !ok {
		return pairObservation{}, fmt.Errorf("a public quote read needs a caller deadline")
	}
	if err := pairPublicEgressReady(ctx, p.egress); err != nil {
		return pairObservation{}, err
	}
	m := r.Member
	host := pairHLPublicHost
	if m.Provider == entryProviderOKX {
		host = pairOKXPublicHost
	}
	args := []string{"--provider=" + m.Provider, "--source=" + pairSourcePublicSnapshot, "--instrument=" + m.Instrument, "--depth=" + strconv.Itoa(r.BookNeed.Depth)}
	res, err := pairRunQuoteReader(ctx, args, r.OutputLimit)
	if err != nil {
		return pairObservation{}, err
	}
	if err := pairReaderEcho(res.obj, map[string]string{"provider": m.Provider, "source": pairSourcePublicSnapshot, "instrument": m.Instrument, "host": host,
		"provenance": r.SourceContract + "@" + host}); err != nil {
		return pairObservation{}, err
	}
	reqMs, err := pairTreeInt(res.obj["request_ms"], "request_ms", 0, res.receiveMs)
	if err != nil {
		return pairObservation{}, err
	}
	receipt, err := pairTreeInt(res.obj["receipt_ms"], "receipt_ms", 0, res.receiveMs)
	if err != nil {
		return pairObservation{}, err
	}
	if reqMs < res.startMs || receipt < reqMs {
		return pairObservation{}, fmt.Errorf("the reader times (request %d, receipt %d) are not inside the launch window [%d, %d]", reqMs, receipt, res.startMs, res.receiveMs)
	}
	return pairObservationFromPayloadSide(m, r.Partition.String(), r.BookNeed, res.payload, receipt, 0, r.Side)
}

type pairArchiveReader struct {
	dir string
}

func (p pairArchiveReader) readQuote(ctx context.Context, r pairMemberRead) (pairObservation, error) {
	m := r.Member
	if m.ModeledDeliveryMs < 1 {
		return pairObservation{}, fmt.Errorf("an archive member needs a modeled delivery of at least 1ms")
	}
	cutoff, ok := pairAddMs(r.StartMs, -m.ModeledDeliveryMs)
	if !ok || cutoff <= 0 {
		return pairObservation{}, fmt.Errorf("the archive cutoff %d minus the modeled delivery %dms is not a positive time", r.StartMs, m.ModeledDeliveryMs)
	}
	args := []string{"--provider=" + m.Provider, "--source=" + pairSourceArchive, "--instrument=" + m.Instrument, "--depth=" + strconv.Itoa(r.BookNeed.Depth),
		"--window-ms=" + strconv.FormatInt(r.BookNeed.WindowMs, 10), "--capacity=" + strconv.Itoa(r.BookNeed.Capacity), "--archive-dir=" + p.dir,
		"--cutoff-ms=" + strconv.FormatInt(cutoff, 10), "--as-of-ms=" + strconv.FormatInt(cutoff, 10)}
	res, err := pairRunQuoteReader(ctx, args, r.OutputLimit)
	if err != nil {
		return pairObservation{}, err
	}
	if err := pairReaderEcho(res.obj, map[string]string{"provider": m.Provider, "source": pairSourceArchive, "instrument": m.Instrument}); err != nil {
		return pairObservation{}, err
	}
	return pairObservationFromPayloadSide(m, r.Partition.String(), r.BookNeed, res.payload, 0, 0, r.Side)
}
