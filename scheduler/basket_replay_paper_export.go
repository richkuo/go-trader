package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
)

const (
	pairRecordingManifestKind    = "go-trader.paper-recording"
	pairRecordingManifestVersion = 1
	pairRecordingRefVersion      = 1
	pairRecordingManifestName    = "recording-manifest.json"
	pairRecordingLogName         = "recording-log.jsonl"
	pairRecordingSeedName        = "seed-evidence.json"
	pairRecordingSpecName        = "reproduce-spec.json"
)

type pairReplayRecordingRef struct {
	Version   int    `json:"version"`
	Path      string `json:"path"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
}

type pairRecordingLogRef struct {
	Path            string `json:"path"`
	SHA256          string `json:"sha256"`
	SizeBytes       int64  `json:"size_bytes"`
	Records         int64  `json:"records"`
	FinalSeq        int64  `json:"final_seq"`
	FinalPrefixHash string `json:"final_prefix_hash"`
}

type pairRecordingManifest struct {
	V                    int                          `json:"v"`
	Kind                 string                       `json:"kind"`
	RunID                string                       `json:"run_id"`
	PaperIdentity        string                       `json:"paper_identity"`
	ContractFP           string                       `json:"contract_fingerprint"`
	Partition            string                       `json:"partition"`
	ResearchConfigSHA256 string                       `json:"research_config_sha256"`
	Collection           pairPaperCollection          `json:"collection"`
	Scenario             pairPaperScenario            `json:"scenario"`
	Lifetime             pairReplayLifetime           `json:"lifetime"`
	EvidenceIdentity     pairRecordedEvidenceIdentity `json:"evidence_identity"`
	FundingBinding       json.RawMessage              `json:"funding_binding"`
	MemberCosts          json.RawMessage              `json:"member_costs"`
	Seed                 pairReplayObjectRef          `json:"seed"`
	Log                  pairRecordingLogRef          `json:"log"`
	Objects              []pairReplayObjectRef        `json:"objects"`
	Calls                int                          `json:"calls"`
	ProcessFailures      int                          `json:"process_failures"`
	InterruptedCalls     int                          `json:"interrupted_calls"`
	RecordFailures       int                          `json:"record_failures"`
	StandaloneNotes      []string                     `json:"standalone_notes"`
}

type pairPaperSealResult struct {
	Sealed           bool     `json:"sealed"`
	Refused          []string `json:"refused,omitempty"`
	ExportDir        string   `json:"export_dir,omitempty"`
	ManifestSHA256   string   `json:"recording_manifest_sha256,omitempty"`
	ReproduceSpec    string   `json:"reproduce_spec,omitempty"`
	FinalSeq         int64    `json:"final_seq,omitempty"`
	FinalPrefixHash  string   `json:"final_prefix_hash,omitempty"`
	SealSeq          int64    `json:"seal_seq,omitempty"`
	ProcessFailures  int      `json:"process_failures,omitempty"`
	InterruptedCalls int      `json:"interrupted_calls,omitempty"`
}

type pairRecLogLine struct {
	Seq         int64  `json:"seq"`
	Schema      int    `json:"schema_version"`
	Partition   string `json:"partition"`
	ContractFP  string `json:"contract_fingerprint"`
	Kind        string `json:"event_kind"`
	CallID      string `json:"call_id"`
	PrevPrefix  string `json:"prev_prefix_hash"`
	Prefix      string `json:"prefix_hash"`
	PayloadHash string `json:"payload_hash"`
	Payload     []byte `json:"payload"`
}

type pairPaperSealFile struct {
	RunID           string `json:"run_id"`
	FinalSeq        int64  `json:"final_seq"`
	FinalPrefixHash string `json:"final_prefix_hash"`
	SealSeq         int64  `json:"seal_seq"`
	ManifestSHA256  string `json:"recording_manifest_sha256"`
	ExportDir       string `json:"export_dir"`
}

func (r *pairReplayRuntime) sealRefusals(l *pairRecLog) []string {
	var out []string
	if !r.finished {
		out = append(out, "the run has not finished its collection and drain: "+r.stopReason)
	} else if r.stopReason != "" {
		out = append(out, r.stopReason)
	}
	if r.cursor.PendingID != "" {
		out = append(out, fmt.Sprintf("step %s is still pending", r.cursor.PendingID))
	}
	if blocked, reason, _ := r.rec.barrier(); blocked {
		out = append(out, "the recording barrier is set: "+reason)
	}
	r.mu.RLock()
	if b := r.rt.Pair.Progress.RecordingBlocked; b != nil {
		out = append(out, "the saved recording barrier from input "+fmt.Sprint(b.FailedSeq)+" is not cleared")
	}
	r.mu.RUnlock()
	if ids := l.unterminated(); len(ids) > 0 {
		out = append(out, "calls without a terminal or recovery event: "+strings.Join(ids, ", "))
	}
	if why := r.rec.equalityReasons(); len(why) > 0 {
		for _, w := range why {
			if strings.HasPrefix(w, "the recorded call sequence diverged") {
				out = append(out, w)
			}
		}
	}
	sort.Strings(out)
	return out
}

func (r *pairReplayRuntime) sealPaper() {
	res := &pairPaperSealResult{}
	r.paper.sealResult = res
	l, err := readVerifiedPairRecLog(r.file, r.spec.Run.RunID, r.partition.String(), r.adm.contract)
	if err != nil {
		res.Refused = []string{err.Error()}
		return
	}
	if res.Refused = r.sealRefusals(l); len(res.Refused) > 0 {
		return
	}
	files, man, err := r.buildExport(l)
	if err != nil {
		res.Refused = []string{"the export could not be built: " + err.Error()}
		return
	}
	dir := filepath.Join(r.spec.Run.StateDir, pairPaperExportDirName)
	if err := pairWriteExport(r.spec.Run.StateDir, dir, files); err != nil {
		res.Refused = []string{err.Error()}
		return
	}
	manSHA := sha256Hex(files[pairRecordingManifestName])
	head := l.head()
	seal := pairRecSeal{FinalSeq: head.Seq, FinalPrefixHash: head.Prefix, ExportManifest: manSHA, CallCount: man.Calls, ProcessFailures: man.ProcessFailures, InterruptedCalls: man.InterruptedCalls}
	if err := r.rec.appendDirect(pairRecEvent{Kind: pairRecEventSeal, Seal: &seal}); err != nil {
		res.Refused = []string{"the seal event could not be committed: " + err.Error()}
		return
	}
	sf := pairPaperSealFile{RunID: r.spec.Run.RunID, FinalSeq: head.Seq, FinalPrefixHash: head.Prefix, SealSeq: r.rec.headNow().Seq, ManifestSHA256: manSHA, ExportDir: dir}
	blob, _ := json.MarshalIndent(sf, "", "  ")
	if err := pairWriteClaimFile(r.spec.Run.StateDir, filepath.Join(r.spec.Run.StateDir, pairPaperSealName), append(blob, '\n')); err != nil {
		res.Refused = []string{"the seal entry could not be written after the seal event committed: " + err.Error()}
		return
	}
	res.Sealed, res.ExportDir, res.ManifestSHA256 = true, dir, manSHA
	res.ReproduceSpec = filepath.Join(dir, pairRecordingSpecName)
	res.FinalSeq, res.FinalPrefixHash, res.SealSeq = head.Seq, head.Prefix, sf.SealSeq
	res.ProcessFailures, res.InterruptedCalls = man.ProcessFailures, man.InterruptedCalls
}

func pairWriteExport(stateDir, dir string, files map[string][]byte) error {
	if info, err := os.Lstat(dir); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s exists and is not a plain export directory", dir)
		}
	} else if err := os.Mkdir(dir, 0o700); err != nil {
		return fmt.Errorf("create the export directory: %w", err)
	}
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		path := filepath.Join(dir, n)
		if old, err := os.ReadFile(path); err == nil {
			if !bytes.Equal(old, files[n]) {
				return fmt.Errorf("the existing export file %s differs from the sealed recording; an export is never replaced", n)
			}
			if err := os.Remove(path + ".partial"); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("remove the partial export file %s: %w", n, err)
			}
			continue
		}
		partial := path + ".partial"
		f, err := os.OpenFile(partial, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, 0o600)
		if err != nil {
			return fmt.Errorf("create export file %s: %w", n, err)
		}
		if _, err := f.Write(files[n]); err != nil {
			f.Close()
			return err
		}
		if err := f.Sync(); err != nil {
			f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
		if err := os.Link(partial, path); err != nil {
			return fmt.Errorf("publish export file %s: %w", n, err)
		}
		if err := os.Remove(partial); err != nil {
			return fmt.Errorf("remove the partial export file %s: %w", n, err)
		}
	}
	for _, d := range []string{dir, stateDir} {
		h, err := os.Open(d)
		if err != nil {
			return err
		}
		err = h.Sync()
		h.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func pairExportRef(kind, member, name string, blob []byte) pairReplayObjectRef {
	return pairReplayObjectRef{Kind: kind, MemberID: member, Path: name, SHA256: sha256Hex(blob), SizeBytes: int64(len(blob))}
}

func (r *pairReplayRuntime) buildExport(l *pairRecLog) (map[string][]byte, pairRecordingManifest, error) {
	files := map[string][]byte{}
	var notes []string
	var objects []pairReplayObjectRef
	evBlob, err := pairEvidenceDocumentBytes(r.paper.evidence.document())
	if err != nil {
		return nil, pairRecordingManifest{}, fmt.Errorf("evidence document: %w", err)
	}
	if src, ferr := newPairFrozenEvidenceSource(evBlob, sha256Hex(evBlob)); ferr != nil {
		notes = append(notes, "the merged evidence document does not decode as a frozen document: "+ferr.Error())
	} else if berr := src.bind(r.rt.Contract); berr != nil {
		notes = append(notes, "the merged evidence document holds a recorded capability that the frozen admission would refuse: "+berr.Error())
	}
	files["evidence.json"] = evBlob
	objects = append(objects, pairExportRef(pairReplayKindEvidence, "", "evidence.json", evBlob))
	fd := pairFundingDocument{V: pairFundingDocumentVersion, SourceID: r.paper.funding.SourceID(), Rates: []pairFundingRate{}, Prices: []pairFundingPrice{}, Coverage: []pairFundingCoverageRecord{}}
	for _, b := range r.paper.funding.chain() {
		fd.Rates = append(fd.Rates, b.Rates...)
		fd.Prices = append(fd.Prices, b.Prices...)
		fd.Coverage = append(fd.Coverage, b.Coverage...)
	}
	fBlob, err := pairFundingDocumentBytes(fd)
	if err != nil {
		return nil, pairRecordingManifest{}, fmt.Errorf("funding document: %w", err)
	}
	if _, ferr := newPairFrozenFundingSource(fBlob, sha256Hex(fBlob)); ferr != nil {
		notes = append(notes, "the merged funding document does not decode as a frozen document: "+ferr.Error())
	}
	files["funding.json"] = fBlob
	objects = append(objects, pairExportRef(pairReplayKindFunding, "", "funding.json", fBlob))
	quotes := map[string][]pairReplayQuoteRecord{}
	candles := map[string]map[int64][6]float64{}
	calls, procFail, interrupted, recFail := 0, 0, 0, 0
	for _, row := range l.rows {
		switch row.Kind {
		case pairRecEventStart:
			if row.Event.Start.ParentID == "" {
				calls++
			}
		case pairRecEventRecovery:
			interrupted++
		case pairRecEventFailure:
			recFail++
		case pairRecEventEnd:
			s := l.rows[l.starts[row.CallID]].Event.Start
			end := row.Event.End
			if pairRecProcessKinds[s.Kind] && s.Phase == pairRecPhaseStep && end.Class != pairRecClassResult {
				procFail++
			}
			if end.Class != pairRecClassResult {
				continue
			}
			switch s.Kind {
			case pairRecKindReference, pairRecKindMatchPoll:
				if rd, err := pairPaperDecodeRead(end.Result); err == nil {
					quotes[s.MemberID] = append(quotes[s.MemberID], pairReplayQuoteRecord{ReceiptMs: rd.ReceiptMs, Payload: rd.Payload})
				}
			case pairRecKindCollect:
				var cr pairPaperCollectResult
				if pairReplayStrictDecode(end.Result, &cr) == nil {
					for _, mr := range cr.Members {
						quotes[mr.MemberID] = append(quotes[mr.MemberID], pairReplayQuoteRecord{ReceiptMs: mr.Read.ReceiptMs, Payload: mr.Read.Payload})
					}
				}
			case pairRecKindCandles:
				var hc pairPaperHistoryCandles
				if pairReplayStrictDecode(end.Result, &hc) == nil {
					if candles[s.MemberID] == nil {
						candles[s.MemberID] = map[int64][6]float64{}
					}
					for _, b := range hc.Bars {
						candles[s.MemberID][int64(b[0])] = b
					}
				}
			}
		}
	}
	for _, sc := range r.members {
		recs := quotes[sc.ID]
		sort.SliceStable(recs, func(i, j int) bool { return recs[i].ReceiptMs < recs[j].ReceiptMs })
		if len(recs) == 0 {
			notes = append(notes, fmt.Sprintf("member %s has no recorded public quote, so its standalone quotes object is empty and a plain version-2 replay refuses it", sc.ID))
		}
		qb, _ := json.Marshal(pairReplayQuoteDoc{V: pairReplayObjectVersion, MemberID: sc.ID, Records: recs})
		name := "quotes-" + sc.ID + ".json"
		files[name] = qb
		objects = append(objects, pairExportRef(pairReplayKindQuotes, sc.ID, name, qb))
		tf := r.paper.timeframes[sc.ID]
		ivl, _ := hlCandleIntervalMs(tf)
		var opens []int64
		for t := range candles[sc.ID] {
			opens = append(opens, t)
		}
		sort.Slice(opens, func(i, j int) bool { return opens[i] < opens[j] })
		start := 0
		for i := 1; i < len(opens); i++ {
			if opens[i]-opens[i-1] != ivl {
				start = i
			}
		}
		if start > 0 {
			notes = append(notes, fmt.Sprintf("member %s recorded candles have a gap before %d; the standalone candles object keeps the gap-free suffix", sc.ID, opens[start]))
		}
		rows := [][]float64{}
		for _, t := range opens[start:] {
			b := candles[sc.ID][t]
			rows = append(rows, []float64{b[0], b[1], b[2], b[3], b[4], b[5]})
		}
		cb, _ := json.Marshal(pairReplayCandleDoc{V: pairReplayObjectVersion, MemberID: sc.ID, Timeframe: tf, Rows: rows})
		cname := "candles-" + sc.ID + ".json"
		files[cname] = cb
		objects = append(objects, pairExportRef(pairReplayKindCandles, sc.ID, cname, cb))
	}
	var logBuf bytes.Buffer
	for _, row := range l.rows {
		line, err := json.Marshal(pairRecLogLine{Seq: row.Seq, Schema: row.Schema, Partition: row.Partition, ContractFP: row.ContractFP, Kind: row.Kind, CallID: row.CallID,
			PrevPrefix: row.PrevPrefix, Prefix: row.Prefix, PayloadHash: row.PayloadHash, Payload: row.Payload})
		if err != nil {
			return nil, pairRecordingManifest{}, err
		}
		logBuf.Write(line)
		logBuf.WriteByte('\n')
	}
	files[pairRecordingLogName] = logBuf.Bytes()
	files[pairRecordingSeedName] = r.paper.seedBlob
	head := l.head()
	sort.Strings(notes)
	if notes == nil {
		notes = []string{}
	}
	sort.Slice(objects, func(i, j int) bool { return objects[i].Path < objects[j].Path })
	man := pairRecordingManifest{V: pairRecordingManifestVersion, Kind: pairRecordingManifestKind, RunID: r.spec.Run.RunID, PaperIdentity: r.paper.identity,
		ContractFP: r.adm.contract, Partition: r.partition.String(), ResearchConfigSHA256: sha256Hex([]byte(pairReplayCompactJSON(r.spec.ResearchConfig))),
		Collection: r.paper.collection, Scenario: r.paper.scenario, Lifetime: r.lt, EvidenceIdentity: r.paper.evidence.Identity(), FundingBinding: json.RawMessage(r.adm.fundingBinding),
		MemberCosts: r.paper.manifest.MemberCosts, Seed: pairExportRef(pairReplayKindEvidence, "", pairRecordingSeedName, r.paper.seedBlob),
		Log:     pairRecordingLogRef{Path: pairRecordingLogName, SHA256: sha256Hex(logBuf.Bytes()), SizeBytes: int64(logBuf.Len()), Records: int64(len(l.rows)), FinalSeq: head.Seq, FinalPrefixHash: head.Prefix},
		Objects: objects, Calls: calls, ProcessFailures: procFail, InterruptedCalls: interrupted, RecordFailures: recFail, StandaloneNotes: notes}
	mb, err := json.MarshalIndent(man, "", "  ")
	if err != nil {
		return nil, man, err
	}
	files[pairRecordingManifestName] = append(mb, '\n')
	repro := map[string]interface{}{
		"protocol": basketReplayProtocolName, "version": pairReplayProtocolVersion, "mode": basketReplayModeNew,
		"run":             map[string]interface{}{"run_id": r.spec.Run.RunID, "state_dir": filepath.Join(filepath.Dir(r.spec.Run.StateDir), r.spec.Run.RunID+"-reproduction"), "source": map[string]string{"recording": "sealed paper recording " + head.Prefix}},
		"research_config": json.RawMessage(r.spec.ResearchConfig),
		"input_manifest":  map[string]interface{}{"root": filepath.Join(r.spec.Run.StateDir, pairPaperExportDirName), "objects": objects},
		"scenario":        r.paper.scenario.replay(),
		"bounds":          r.spec.Bounds,
		"recording":       pairReplayRecordingRef{Version: pairRecordingRefVersion, Path: pairRecordingManifestName, SHA256: sha256Hex(files[pairRecordingManifestName]), SizeBytes: int64(len(files[pairRecordingManifestName]))},
	}
	rb, err := json.MarshalIndent(repro, "", "  ")
	if err != nil {
		return nil, man, err
	}
	files[pairRecordingSpecName] = append(rb, '\n')
	return files, man, nil
}

type pairPlaybackInput struct {
	manifest pairRecordingManifest
	seed     []byte
	log      *pairRecLog
}

func pairValidateRecordingRef(ref *pairReplayRecordingRef) []string {
	var errs []string
	if ref.Version != pairRecordingRefVersion {
		errs = append(errs, fmt.Sprintf("recording.version %d must be %d", ref.Version, pairRecordingRefVersion))
	}
	if !pairReplayObjectPathPattern.MatchString(ref.Path) || strings.Contains(ref.Path, "..") {
		errs = append(errs, fmt.Sprintf("recording.path %q must be a clean relative path inside the input root", ref.Path))
	}
	if !pairReplaySHAPattern.MatchString(ref.SHA256) {
		errs = append(errs, fmt.Sprintf("recording.sha256 %q must be 64 lowercase hex characters", ref.SHA256))
	}
	if ref.SizeBytes <= 0 || ref.SizeBytes > pairReplayMaxObjectBytes {
		errs = append(errs, fmt.Sprintf("recording.size_bytes %d must be in [1, %d]", ref.SizeBytes, pairReplayMaxObjectBytes))
	}
	return errs
}

func loadPairPlaybackInput(spec *pairReplaySpec, lt pairReplayLifetime) (*pairPlaybackInput, error) {
	ref := spec.Recording
	root := spec.InputManifest.Root
	blob, err := pairReplayReadObject(root, pairReplayObjectRef{Path: ref.Path, SHA256: ref.SHA256, SizeBytes: ref.SizeBytes})
	if err != nil {
		return nil, refuseReplay("input_refused", "recording manifest: "+err.Error())
	}
	var man pairRecordingManifest
	if err := pairReplayStrictDecode(blob, &man); err != nil {
		return nil, refuseReplay("input_refused", "recording manifest: "+err.Error())
	}
	var errs []string
	add := func(format string, a ...interface{}) { errs = append(errs, fmt.Sprintf(format, a...)) }
	if man.V != pairRecordingManifestVersion || man.Kind != pairRecordingManifestKind {
		add("the recording manifest is %q version %d, not %q version %d", man.Kind, man.V, pairRecordingManifestKind, pairRecordingManifestVersion)
	}
	if man.RunID != spec.Run.RunID {
		add("run.run_id %q must be the recorded run id %q: client ids and the virtual ledger carry the run id, so a reproduction keeps it", spec.Run.RunID, man.RunID)
	}
	if got := sha256Hex([]byte(pairReplayCompactJSON(spec.ResearchConfig))); got != man.ResearchConfigSHA256 {
		add("research_config differs from the recorded research configuration")
	}
	if a, b := mustJSON(spec.Scenario), mustJSON(man.Scenario.replay()); !bytes.Equal(a, b) {
		add("scenario %s differs from the recorded scenario %s", a, b)
	}
	if lt.String() != man.Lifetime.String() {
		add("bounds %s differ from the recorded lifetime %s", lt, man.Lifetime)
	}
	if pairReplayObjectSet(spec.InputManifest.Objects) != pairReplayObjectSet(man.Objects) {
		add("input_manifest.objects differ from the objects the recording exported")
	}
	if len(errs) > 0 {
		sort.Strings(errs)
		return nil, refuseReplay("input_refused", errs...)
	}
	seed, err := pairReplayReadObject(root, man.Seed)
	if err != nil {
		return nil, refuseReplay("input_refused", "recording seed: "+err.Error())
	}
	logBlob, err := pairReplayReadObject(root, pairReplayObjectRef{Path: man.Log.Path, SHA256: man.Log.SHA256, SizeBytes: man.Log.SizeBytes})
	if err != nil {
		return nil, refuseReplay("input_refused", "recording log: "+err.Error())
	}
	var rows []pairRecRow
	sc := bufio.NewScanner(bytes.NewReader(logBlob))
	sc.Buffer(make([]byte, 1<<20), 4*pairRecMaxPayloadBytes)
	for sc.Scan() {
		var line pairRecLogLine
		if err := pairReplayStrictDecode(sc.Bytes(), &line); err != nil {
			return nil, refuseReplay("input_refused", fmt.Sprintf("recording log line %d: %v", len(rows)+1, err))
		}
		rows = append(rows, pairRecRow{Seq: line.Seq, Schema: line.Schema, Partition: line.Partition, ContractFP: line.ContractFP, Kind: line.Kind, CallID: line.CallID,
			PrevPrefix: line.PrevPrefix, Prefix: line.Prefix, PayloadHash: line.PayloadHash, Payload: line.Payload})
	}
	if err := sc.Err(); err != nil {
		return nil, refuseReplay("input_refused", "recording log: "+err.Error())
	}
	l, err := verifyPairRecLog(rows, man.RunID, man.Partition, man.ContractFP)
	if err != nil {
		return nil, refuseReplay("input_refused", "recording log: "+err.Error())
	}
	if h := l.head(); h.Seq != man.Log.FinalSeq || h.Prefix != man.Log.FinalPrefixHash || int64(len(rows)) != man.Log.Records {
		return nil, refuseReplay("input_refused", fmt.Sprintf("the recording log ends at input %d, the manifest seals input %d", h.Seq, man.Log.FinalSeq))
	}
	if l.bind == nil || l.bind.PaperIdentity != man.PaperIdentity || l.bind.SeedSHA256 != man.Seed.SHA256 {
		return nil, refuseReplay("input_refused", "the recording log binds another paper identity or seed")
	}
	if ids := l.unterminated(); len(ids) > 0 {
		return nil, refuseReplay("input_refused", "the sealed recording holds calls without a terminal or recovery event: "+strings.Join(ids, ", "))
	}
	return &pairPlaybackInput{manifest: man, seed: seed, log: l}, nil
}

func openPairPlayback(spec *pairReplaySpec, raw []byte, lt pairReplayLifetime, opts basketReplayOptions) (*pairReplayRuntime, error) {
	r := &pairReplayRuntime{spec: spec, lt: lt, opts: opts, mu: &sync.RWMutex{}, notifier: NewMultiNotifier(), clock: &simulatedClock{}, faultCount: map[string]int{}}
	ok := false
	defer func() {
		if !ok {
			r.Close()
		}
	}()
	stateDir := spec.Run.StateDir
	if err := basketReplayPathAllowed(stateDir); err != nil {
		return nil, refuseReplay("state_dir_refused", err.Error())
	}
	if _, err := basketReplayRealParent(stateDir); err != nil {
		return nil, refuseReplay("state_dir_refused", err.Error())
	}
	resume := spec.Mode == basketReplayModeResume
	if !resume {
		if _, err := os.Lstat(stateDir); err == nil {
			return nil, refuseReplay("state_exists", fmt.Sprintf("new-run mode refuses the existing path %q; a new run claims its own directory", stateDir))
		}
	} else if err := checkPairReplayResumeDir(stateDir); err != nil {
		return nil, err
	}
	inputFP, err := pairReplayInputFingerprint(raw)
	if err != nil {
		return nil, refuseReplay("spec_malformed", err.Error())
	}
	in, err := loadPairReplayInputs(spec)
	if err != nil {
		return nil, err
	}
	r.inputs = in
	pb, err := loadPairPlaybackInput(spec, lt)
	if err != nil {
		return nil, err
	}
	man := pb.manifest
	r.paper = &pairPaperRun{playback: true, collection: man.Collection, scenario: man.Scenario, seedBlob: pb.seed, seedRef: man.Seed, identity: man.PaperIdentity, recording: &man, timeframes: map[string]string{}}
	cfgJSON, err := pairReplayConfigDocument(spec.ResearchConfig, stateDir)
	if err != nil {
		return nil, err
	}
	portable, err := pairReplayConfigDocument(spec.ResearchConfig, "/basket-replay-state")
	if err != nil {
		return nil, err
	}
	r.cfgJSON = cfgJSON
	shape, err := pairPaperShape(spec.ResearchConfig)
	if err != nil {
		return nil, err
	}
	r.interval = int64(shape.IntervalSeconds) * 1000
	r.fp = basketReplayFingerprints{Protocol: fmt.Sprintf("%s/%d", basketReplayProtocolName, pairReplayProtocolVersion), Source: basketReplaySourceFingerprint(spec.Run.Source), Input: inputFP, Config: sha256Hex(portable)}
	runBlob, _ := json.Marshal([]string{r.fp.Protocol, spec.Run.RunID, r.fp.Source, r.fp.Input, r.fp.Config})
	r.fp.Run = sha256Hex(runBlob)
	venues, err := r.buildRecordedSources(pairRecordedSetup{runID: spec.Run.RunID, stateDir: stateDir, resume: resume, shape: shape, seed: pb.seed, seedDigest: man.Seed.SHA256,
		scenario: man.Scenario, collection: man.Collection, recMode: pairRecPlayback})
	if err != nil {
		return nil, err
	}
	r.venues = venues
	r.rec.log = pb.log
	r.rec.partition, r.rec.contractFP = man.Partition, man.ContractFP
	if r.paper.evidence.Identity() != man.EvidenceIdentity {
		return nil, refuseReplay("input_refused", "the recorded evidence identity differs from the seed the recording names")
	}
	r.manifest = pairReplayManifest{Owner: basketReplayOwnerMarker, Version: pairReplayManifestVersion, RunID: spec.Run.RunID, Fingerprints: r.fp, EvidenceDigest: r.paper.evidence.Digest(),
		FundingDigest: sha256Hex(in.funding), Objects: pairReplayObjectsWithTotals(in), Lifetime: lt, Scenario: spec.Scenario, EventOrder: pairPaperEventOrder, AdministrativeFields: pairReplayAdministrativeFields, UID: os.Getuid()}
	var saved pairReplayManifest
	if resume {
		saved, err = readPairReplayManifest(stateDir)
		if err != nil {
			return nil, err
		}
		if err := comparePairReplayManifest(saved, r.manifest, false); err != nil {
			return nil, err
		}
		if err := r.playbackFundingBeforeAdmission(stateDir, man); err != nil {
			return nil, err
		}
	}
	if err := r.bindRecordedProcess(); err != nil {
		return nil, err
	}
	venueFor := func(sc StrategyConfig) (basketVenue, error) {
		if v := r.venues[sc.ID]; v != nil {
			return v, nil
		}
		return nil, fmt.Errorf("no virtual venue for %s", sc.ID)
	}
	adm, cfg, err := newPairResearchAdmission(cfgJSON, spec.Run.RunID, stateDir, resume, pairResearchDeps{VenueFor: venueFor, Evidence: r.paper.evidence, Quotes: r.paper.quotes, Funding: r.paper.funding})
	if err != nil {
		return nil, refuseReplay("admission_refused", err.Error())
	}
	r.cleanups = append(r.cleanups, adm.release)
	r.adm, r.cfg = adm, cfg
	if adm.contract != man.ContractFP {
		return nil, refuseReplay("input_refused", fmt.Sprintf("the admitted contract %s is not the recorded contract %s", shortFingerprint(adm.contract), shortFingerprint(man.ContractFP)))
	}
	if err := pairFundingCompareBinding(json.RawMessage(adm.fundingBinding), man.FundingBinding); err != nil {
		return nil, refuseReplay("input_refused", "the playback funding binding differs from the recorded one: "+err.Error())
	}
	r.fp.Contract = adm.contract
	r.manifest.Fingerprints.Contract = adm.contract
	r.manifest.FundingBinding = json.RawMessage(adm.fundingBinding)
	if resume {
		if err := comparePairReplayManifest(saved, r.manifest, true); err != nil {
			return nil, err
		}
		r.manifest.CostEvidence, r.manifest.MemberCosts, r.manifest.CreatedAt = saved.CostEvidence, saved.MemberCosts, saved.CreatedAt
		cur, err := verifyPairReplayDBReadOnly(filepath.Join(stateDir, basketReplayDBName), r.manifest)
		if err != nil {
			return nil, err
		}
		r.cursor = cur
	} else if err := os.Mkdir(filepath.Join(stateDir, basketReplayLogDirName), 0o700); err != nil {
		return nil, err
	}
	if err := r.openRecordedStore(); err != nil {
		return nil, err
	}
	if !resume {
		if err := r.file.insertPairReplayRun(r.manifest, r.manifest.Objects, in.recordTotal); err != nil {
			return nil, err
		}
	} else {
		cur, err := r.file.readPairReplayCursor(spec.Run.RunID)
		if err != nil {
			return nil, err
		}
		if cur != r.cursor {
			return nil, refuseReplay("resume_refused", "the run cursor changed while the adapter opened the state file")
		}
	}
	if err := r.restorePlaybackBootstrap(); err != nil {
		return nil, err
	}
	if err := r.loadState(); err != nil {
		return nil, err
	}
	if !resume {
		if err := r.startupCommit(); err != nil {
			return nil, err
		}
		if want := pairReplayCompactJSON(man.MemberCosts); pairReplayCompactJSON(r.manifest.MemberCosts) != want {
			return nil, refuseReplay("input_refused", "the playback member cost bindings differ from the recorded ones")
		}
		r.initPaperGrids()
	} else {
		if err := r.restoreSourcesFrom(pb.log, -1); err != nil {
			return nil, err
		}
		if err := r.resumeState(); err != nil {
			return nil, err
		}
	}
	r.store.saveExtra = r.saveExtra
	ok = true
	return r, nil
}

func (r *pairReplayRuntime) restorePlaybackBootstrap() error {
	l := r.rec.log
	for _, row := range l.rows {
		if row.Kind != pairRecEventEnd || row.Event.End.Class != pairRecClassResult {
			continue
		}
		s := l.rows[l.starts[row.CallID]].Event.Start
		if s.Phase != pairRecPhaseBootstrap {
			continue
		}
		switch s.Kind {
		case pairRecKindAccount, pairRecKindConversions, pairRecKindCapability:
			if err := r.admitSeedRecords(s.Kind, row.Event.End.Result, row.Event.End.LogicalMs); err != nil {
				return refuseReplay("input_refused", fmt.Sprintf("the recorded seed records of call %s are refused: %v", row.CallID, err))
			}
		}
	}
	r.paper.evidence.activate()
	return nil
}

func (r *pairReplayRuntime) playbackFundingBeforeAdmission(stateDir string, man pairRecordingManifest) error {
	part, err := parseRiskPartition(man.Partition)
	if err != nil {
		return refuseReplay("input_refused", err.Error())
	}
	var pairID string
	var shape pairReplayResearchShape
	if json.Unmarshal(r.spec.ResearchConfig, &shape) == nil {
		var ids struct {
			Pairs []struct {
				ID string `json:"id"`
			} `json:"pairs"`
		}
		_ = json.Unmarshal(r.spec.ResearchConfig, &ids)
		if len(ids.Pairs) == 1 {
			pairID = ids.Pairs[0].ID
		}
	}
	saved, err := pairFundingReadSavedChain(filepath.Join(stateDir, basketReplayDBName), part, pairID)
	if err != nil {
		return refuseReplay("state_foreign", err.Error())
	}
	l := r.rec.log
	for _, row := range l.rows {
		if len(r.paper.funding.chain()) >= len(saved) {
			break
		}
		if row.Kind != pairRecEventEnd || row.Event.End.Class != pairRecClassResult {
			continue
		}
		if l.rows[l.starts[row.CallID]].Event.Start.Kind != pairRecKindFunding {
			continue
		}
		_ = r.paper.funding.Append(row.Event.End.Result)
	}
	return nil
}

func pairPaperSealedFromLog(stateDir string, l *pairRecLog) {
	if l == nil || l.sealed == nil {
		return
	}
	path := filepath.Join(stateDir, pairPaperSealName)
	if _, err := os.Lstat(path); err == nil {
		return
	}
	sf := pairPaperSealFile{RunID: l.runID, FinalSeq: l.sealed.FinalSeq, FinalPrefixHash: l.sealed.FinalPrefixHash, SealSeq: l.head().Seq, ManifestSHA256: l.sealed.ExportManifest,
		ExportDir: filepath.Join(stateDir, pairPaperExportDirName)}
	blob, _ := json.MarshalIndent(sf, "", "  ")
	_ = pairWriteClaimFile(stateDir, path, append(blob, '\n'))
}

type pairPaperOutputInfo struct {
	Mode           string               `json:"mode"`
	EgressConfig   *pairResearchEgress  `json:"egress_config,omitempty"`
	Collection     pairPaperCollection  `json:"collection"`
	HeadSeq        int64                `json:"head_seq"`
	HeadPrefixHash string               `json:"head_prefix_hash"`
	Calls          int                  `json:"calls"`
	ProcessCalls   int                  `json:"process_calls"`
	Interrupted    int                  `json:"interrupted_calls"`
	RecordFailures int                  `json:"record_failures"`
	Epochs         int                  `json:"process_epochs"`
	BarrierSet     bool                 `json:"barrier_set,omitempty"`
	Consumed       int                  `json:"consumed_stream_items"`
	StreamItems    int                  `json:"stream_items"`
	Seal           *pairPaperSealResult `json:"seal,omitempty"`
}

func (r *pairReplayRuntime) recordingChecks() (*pairPaperOutputInfo, []string, []string, []string) {
	info := &pairPaperOutputInfo{Mode: "paper", EgressConfig: r.paper.egress, Collection: r.paper.collection, Seal: r.paper.sealResult}
	var ok, bad, incomplete []string
	var l *pairRecLog
	if r.paper.playback {
		info.Mode = "playback"
		l = r.rec.log
		ok = append(ok, "the sealed recording log chain, its call terminals and its manifest binding were verified before admission")
	} else {
		var err error
		l, err = readVerifiedPairRecLog(r.file, r.spec.Run.RunID, r.partition.String(), r.adm.contract)
		if err != nil {
			bad = append(bad, "the committed recording is refused: "+err.Error())
			return info, ok, bad, incomplete
		}
		ok = append(ok, fmt.Sprintf("%d recording inputs, their prefix chain, the call index and the slot index", len(l.rows)))
		r.mu.RLock()
		err = pairPaperSlotCheck(r.file, r.spec.Run.RunID, r.rt)
		r.mu.RUnlock()
		if err != nil {
			bad = append(bad, "slot binding: "+err.Error())
		} else {
			ok = append(ok, "every frozen slot envelope equals its recorded collection binding")
		}
	}
	if l == nil {
		return info, ok, bad, incomplete
	}
	h := l.head()
	info.HeadSeq, info.HeadPrefixHash, info.Epochs = h.Seq, h.Prefix, l.epochs
	info.StreamItems, info.Consumed = len(l.stream), r.rec.cursorNow()
	for _, row := range l.rows {
		switch row.Kind {
		case pairRecEventStart:
			if row.Event.Start.ParentID == "" && row.Event.Start.Phase == pairRecPhaseStep {
				info.Calls++
				if pairRecProcessKinds[row.Event.Start.Kind] {
					info.ProcessCalls++
				}
			}
		case pairRecEventRecovery:
			info.Interrupted++
			incomplete = append(incomplete, fmt.Sprintf("call %s has an unknown outcome after an interruption (input %d); it was never repeated", row.CallID, row.Seq))
		case pairRecEventFailure:
			info.RecordFailures++
			incomplete = append(incomplete, fmt.Sprintf("the %s call at input %d was refused because the recording could not commit: %s", row.Event.Failure.CallKind, row.Seq, row.Event.Failure.Reason))
		}
	}
	if blocked, reason, failed := r.rec.barrier(); blocked {
		info.BarrierSet = true
		incomplete = append(incomplete, fmt.Sprintf("the recording barrier from input %d is still set: %s", failed, reason))
	}
	if !r.paper.playback {
		for _, id := range l.unterminated() {
			incomplete = append(incomplete, fmt.Sprintf("call %s has no terminal or recovery event", id))
		}
	}
	sort.Strings(incomplete)
	return info, ok, bad, incomplete
}
