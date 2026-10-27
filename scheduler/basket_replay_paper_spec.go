package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

const (
	pairPaperProtocolLabel       = "go-trader.basket-replay-paper"
	pairPaperManifestOwner       = "go-trader basket-replay-paper recorder"
	pairPaperManifestVer         = 1
	pairPaperManifestResearchVer = 2
	pairPaperRecordsPerStep      = 3
	pairPaperRecordOverhead      = 96
	pairPaperMinFundingMs        = 1000
)

type pairPaperCollection struct {
	FromMs         int64 `json:"from_ms"`
	ThroughMs      int64 `json:"through_ms"`
	DrainThroughMs int64 `json:"drain_through_ms"`
}

type pairPaperScenario struct {
	SubmitLatencyMs          int64                     `json:"submit_latency_ms"`
	ResponseLatencyMs        int64                     `json:"response_latency_ms"`
	ReceiptModel             string                    `json:"receipt_model"`
	VenueFailures            []pairReplayFailureScript `json:"venue_failures"`
	PaperPollIntervalMs      int64                     `json:"paper_poll_interval_ms"`
	PaperFundingPollMs       int64                     `json:"paper_funding_poll_ms"`
	PaperCapabilityRefreshMs int64                     `json:"paper_capability_refresh_ms"`
	FundingPolicy            *pairFundingPolicyV1      `json:"funding_policy,omitempty"`
}

type pairPaperSpec struct {
	Protocol       string                  `json:"protocol"`
	Version        int                     `json:"version"`
	Mode           string                  `json:"mode"`
	Run            basketReplayRunSpec     `json:"run"`
	ResearchConfig json.RawMessage         `json:"research_config"`
	InputManifest  pairReplayInputManifest `json:"input_manifest"`
	Collection     pairPaperCollection     `json:"collection"`
	Scenario       pairPaperScenario       `json:"scenario"`
	Bounds         pairReplayBounds        `json:"bounds"`
}

func (s pairPaperScenario) replay() pairReplayScenario {
	f := s.VenueFailures
	if f == nil {
		f = []pairReplayFailureScript{}
	}
	return pairReplayScenario{SubmitLatencyMs: s.SubmitLatencyMs, ResponseLatencyMs: s.ResponseLatencyMs, ReceiptModel: s.ReceiptModel, VenueFailures: f}
}

func (s *pairPaperSpec) replaySpec() *pairReplaySpec {
	return &pairReplaySpec{Protocol: s.Protocol, Version: s.Version, Mode: s.Mode, Run: s.Run, ResearchConfig: s.ResearchConfig, InputManifest: s.InputManifest,
		Scenario: s.Scenario.replay(), Bounds: s.Bounds}
}

func decodePairPaperSpec(raw []byte) (*pairPaperSpec, error) {
	if _, err := pairStrictParse(bytes.TrimSpace(raw)); err != nil {
		return nil, refuseReplay("spec_malformed", "the paper specification is not one strict JSON object (duplicate keys, invalid text and trailing data are refused): "+err.Error())
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var spec pairPaperSpec
	if err := dec.Decode(&spec); err != nil {
		return nil, refuseReplay("spec_malformed", err.Error())
	}
	if dec.More() {
		return nil, refuseReplay("spec_malformed", "trailing data after the paper specification")
	}
	var rawShape struct {
		Scenario map[string]json.RawMessage `json:"scenario"`
	}
	if err := json.Unmarshal(raw, &rawShape); err != nil {
		return nil, refuseReplay("spec_malformed", err.Error())
	}
	if policy, ok := rawShape.Scenario["funding_policy"]; ok && bytes.Equal(bytes.TrimSpace(policy), []byte("null")) {
		return nil, refuseReplay("spec_invalid", "scenario.funding_policy must be an object when present; null is refused")
	}
	return &spec, nil
}

func validatePairPaperSpec(spec *pairPaperSpec) (pairReplayLifetime, error) {
	var lt pairReplayLifetime
	var errs []string
	add := func(format string, a ...interface{}) { errs = append(errs, fmt.Sprintf(format, a...)) }
	if spec.Protocol != basketReplayProtocolName || spec.Version != pairReplayProtocolVersion {
		return lt, refuseReplay("protocol_unsupported", fmt.Sprintf("the paper specification names protocol %q version %d; the paper recorder reads %q version %d with a collection block", spec.Protocol, spec.Version, basketReplayProtocolName, pairReplayProtocolVersion))
	}
	if spec.Mode != basketReplayModeNew && spec.Mode != basketReplayModeResume {
		add("mode %q must be %q or %q", spec.Mode, basketReplayModeNew, basketReplayModeResume)
	}
	if !basketReplayRunIDPattern.MatchString(spec.Run.RunID) {
		add("run.run_id %q must match %s", spec.Run.RunID, basketReplayRunIDPattern.String())
	}
	if strings.TrimSpace(spec.Run.StateDir) == "" || !filepath.IsAbs(spec.Run.StateDir) || filepath.Clean(spec.Run.StateDir) != spec.Run.StateDir {
		add("run.state_dir %q must be an absolute, clean path", spec.Run.StateDir)
	}
	if len(spec.Run.Source) == 0 {
		add("run.source must record the provenance of the seed evidence and the public venues")
	}
	if len(bytes.TrimSpace(spec.ResearchConfig)) == 0 {
		add("research_config is required")
	} else {
		var root map[string]json.RawMessage
		if err := json.Unmarshal(spec.ResearchConfig, &root); err != nil {
			add("research_config must be one JSON object: %v", err)
		} else {
			for _, k := range pairReplayResearchRefusedKeys {
				if _, ok := root[k]; ok {
					add("research_config sets %q; the paper recorder owns storage, logs, routing, feeds and egress", k)
				}
			}
			var shape struct {
				Pairs []struct {
					Members []struct {
						StrategyID string `json:"strategy_id"`
						Source     string `json:"source"`
					} `json:"members"`
				} `json:"pairs"`
			}
			if json.Unmarshal(spec.ResearchConfig, &shape) == nil && len(shape.Pairs) == 1 {
				for _, m := range shape.Pairs[0].Members {
					if m.Source != pairSourcePublicSnapshot {
						add("member %s source %q: isolated paper records match polls, collections and references only through the public_snapshot readers, so a %q member would need a live sealed or archive producer this recorder does not have (it never relabels a public snapshot as sealed or archive coverage)", m.StrategyID, m.Source, m.Source)
					}
				}
			}
		}
	}
	m := spec.InputManifest
	if strings.TrimSpace(m.Root) == "" || !filepath.IsAbs(m.Root) || filepath.Clean(m.Root) != m.Root {
		add("input_manifest.root %q must be an absolute, clean path", m.Root)
	} else if rel, err := filepath.Rel(spec.Run.StateDir, m.Root); err == nil && (rel == "." || !strings.HasPrefix(rel, "..")) {
		add("input_manifest.root %q must be outside the run state directory", m.Root)
	} else if rel, err := filepath.Rel(m.Root, spec.Run.StateDir); err == nil && (rel == "." || !strings.HasPrefix(rel, "..")) {
		add("the run state directory must not be inside input_manifest.root")
	}
	evidence := 0
	for i, o := range m.Objects {
		label := fmt.Sprintf("input_manifest.objects[%d]", i)
		if o.Kind != pairReplayKindEvidence {
			add("%s kind %q: a paper run takes exactly one seed evidence object; funding, quotes, books and candles are recorded live before use", label, o.Kind)
			continue
		}
		evidence++
		if o.MemberID != "" {
			add("%s kind evidence is pair-wide and names no member", label)
		}
		if !pairReplayObjectPathPattern.MatchString(o.Path) || strings.Contains(o.Path, "..") {
			add("%s path %q must be a clean relative path inside the input root", label, o.Path)
		}
		if !pairReplaySHAPattern.MatchString(o.SHA256) {
			add("%s sha256 %q must be 64 lowercase hex characters", label, o.SHA256)
		}
		if o.SizeBytes <= 0 || o.SizeBytes > pairReplayMaxObjectBytes {
			add("%s size_bytes %d must be in [1, %d]", label, o.SizeBytes, pairReplayMaxObjectBytes)
		}
	}
	if evidence != 1 {
		add("input_manifest needs exactly one seed evidence object (capabilities, synthetic accounts and conversions)")
	}
	sc := spec.Scenario
	if sc.SubmitLatencyMs < 0 || sc.SubmitLatencyMs > pairVirtualMaxLatencyMs || sc.ResponseLatencyMs < 0 || sc.ResponseLatencyMs > pairVirtualMaxLatencyMs {
		add("scenario latencies must be in [0, %d] ms", pairVirtualMaxLatencyMs)
	}
	if sc.ReceiptModel != pairReplayReceiptModel {
		add("scenario.receipt_model %q must be %q: every record keeps its recorded first receipt", sc.ReceiptModel, pairReplayReceiptModel)
	}
	if sc.VenueFailures == nil || len(sc.VenueFailures) != 0 {
		add("scenario.venue_failures must be an empty list: a paper run records real outcomes and injects no scripted venue failure")
	}
	if sc.PaperPollIntervalMs < 1 || sc.PaperPollIntervalMs > pairMaxIntervalMs {
		add("scenario.paper_poll_interval_ms %d must be in [1, %d]", sc.PaperPollIntervalMs, pairMaxIntervalMs)
	}
	if sc.PaperFundingPollMs < pairPaperMinFundingMs || sc.PaperFundingPollMs > pairMaxIntervalMs {
		add("scenario.paper_funding_poll_ms %d must be in [%d, %d]", sc.PaperFundingPollMs, pairPaperMinFundingMs, pairMaxIntervalMs)
	}
	if sc.PaperCapabilityRefreshMs < 0 || sc.PaperCapabilityRefreshMs > pairMaxIntervalMs/2 {
		add("scenario.paper_capability_refresh_ms %d must be 0 (seed capabilities only) or in [1, %d]", sc.PaperCapabilityRefreshMs, pairMaxIntervalMs/2)
	}
	c := spec.Collection
	if c.FromMs <= 0 || c.ThroughMs <= c.FromMs || c.DrainThroughMs < c.ThroughMs {
		add("collection must satisfy 0 < from_ms < through_ms <= drain_through_ms (got %d, %d, %d)", c.FromMs, c.ThroughMs, c.DrainThroughMs)
	}
	if _, ok := pairAddMs(c.DrainThroughMs, pairMaxIntervalMs); !ok {
		add("collection.drain_through_ms %d leaves no room for checked time arithmetic", c.DrainThroughMs)
	}
	if p := sc.FundingPolicy; p != nil {
		if err := pairFundingValidatePolicy(*p); err != nil {
			add("scenario.funding_policy: %v", err)
		}
		if p.FromMs != c.FromMs || p.DrainThroughMs != c.DrainThroughMs {
			add("scenario.funding_policy bounds (%d, %d] must equal collection bounds (%d, %d]", p.FromMs, p.DrainThroughMs, c.FromMs, c.DrainThroughMs)
		}
		shape, serr := pairPaperShape(spec.ResearchConfig)
		if serr != nil || len(shape.Pairs[0].Members) != 2 {
			add("scenario.funding_policy needs the checked two-member research pair")
		} else {
			if len(p.MemberRules) != len(shape.Pairs[0].Members) {
				add("scenario.funding_policy must name every pair member exactly once")
			}
			for _, m := range shape.Pairs[0].Members {
				_, provider := pairPaperMemberPartition(shape, m.StrategyID)
				want := ""
				switch provider {
				case entryProviderHyperliquid:
					want = pairFundingRuleHLReceiptResearch
				case entryProviderOKX:
					want = pairFundingRuleOKX
				}
				version, ok := p.MemberRules[m.StrategyID]
				switch {
				case !ok:
					add("scenario.funding_policy does not name member %s", m.StrategyID)
				case want == "" || version != want:
					add("scenario.funding_policy member %s on %q must select %q, got %q", m.StrategyID, provider, want, version)
				}
			}
		}
	}
	start, err := pairReplayParseTime("bounds.start", spec.Bounds.Start)
	if err != nil {
		add("%v", err)
	}
	end, err2 := pairReplayParseTime("bounds.end", spec.Bounds.End)
	if err2 != nil {
		add("%v", err2)
	}
	if err == nil && err2 == nil && (start != c.FromMs || end != c.DrainThroughMs) {
		add("bounds.start %d and bounds.end %d must equal collection.from_ms %d and collection.drain_through_ms %d; the frozen lifetime is the collection plus its drain", start, end, c.FromMs, c.DrainThroughMs)
	}
	if spec.Bounds.MaxSteps <= 0 || spec.Bounds.MaxSteps > pairReplayMaxSteps {
		add("bounds.max_steps %d must be in [1, %d]", spec.Bounds.MaxSteps, pairReplayMaxSteps)
	}
	if len(errs) > 0 {
		sort.Strings(errs)
		return lt, refuseReplay("spec_invalid", errs...)
	}
	return pairReplayLifetime{StartMs: start, EndMs: end, MaxSteps: spec.Bounds.MaxSteps}, nil
}

func pairPaperIdentity(raw []byte) (string, error) {
	return pairReplayInputFingerprint(raw)
}

func pairPaperMaxQuotesBytes(maxSteps int, payloadMax int) int64 {
	per := int64(4*((payloadMax+2)/3) + pairPaperRecordOverhead)
	return int64(maxSteps) * pairPaperRecordsPerStep * per
}
