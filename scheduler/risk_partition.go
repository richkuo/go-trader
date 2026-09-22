package main

import (
	"fmt"
	"sort"
	"strings"
)

// RiskPartition names one independent risk, evaluation and storage domain.
// Execution mode stays PortfolioScope and is still classified only by
// isLiveArgs; a partition adds the paper source identity so several paper
// deployments folded into one process keep separate limits, latches, peaks,
// correlation inputs and state files.
type RiskPartition struct {
	Scope  PortfolioScope
	Source string
}

const paperSourceSeparator = ":"

var (
	livePartition         = RiskPartition{Scope: ScopeLive}
	defaultPaperPartition = RiskPartition{Scope: ScopePaper}
	unassignedPartition   = RiskPartition{Scope: scopeUnassigned}
)

func paperSourcePartition(id string) RiskPartition {
	id = strings.TrimSpace(id)
	if id == "" {
		return defaultPaperPartition
	}
	return RiskPartition{Scope: ScopePaper, Source: id}
}

func (p RiskPartition) String() string {
	if p.Source == "" {
		return string(p.Scope)
	}
	return string(p.Scope) + paperSourceSeparator + p.Source
}

func (p RiskPartition) IsLive() bool { return p.Scope == ScopeLive }

func (p RiskPartition) MarshalText() ([]byte, error) { return []byte(p.String()), nil }

func (p *RiskPartition) UnmarshalText(b []byte) error {
	parsed, err := parseRiskPartition(string(b))
	if err != nil {
		return err
	}
	*p = parsed
	return nil
}

// parseRiskPartition reads the operator and wire text form. The separator is
// outside the source id pattern, so a source can never spell another partition.
func parseRiskPartition(s string) (RiskPartition, error) {
	text := strings.TrimSpace(s)
	switch text {
	case "":
		return unassignedPartition, nil
	case string(ScopeLive):
		return livePartition, nil
	case string(ScopePaper):
		return defaultPaperPartition, nil
	}
	scope, source, ok := strings.Cut(text, paperSourceSeparator)
	if !ok || PortfolioScope(scope) != ScopePaper || strings.TrimSpace(source) == "" {
		return RiskPartition{}, fmt.Errorf("unknown risk partition %q (want live, paper or paper:<source id>)", s)
	}
	return paperSourcePartition(source), nil
}

func partitionLabel(p RiskPartition) string {
	if p.Source == "" {
		return scopeLabel(p.Scope)
	}
	return p.String()
}

func partitionPrefixedDM(p RiskPartition, msg string) string {
	return fmt.Sprintf("[%s scope] %s", partitionLabel(p), msg)
}

// partitionFor resolves one strategy's partition. Live strategies never carry a
// source: paper_source on a live strategy is refused at load.
func partitionFor(sc StrategyConfig) RiskPartition {
	if portfolioScopeFor(sc) == ScopeLive {
		return livePartition
	}
	return paperSourcePartition(sc.PaperSource)
}

// activePartitions lists the partitions the roster actually populates, in the
// stable order live, default paper, then named sources by id.
func activePartitions(cfgs []StrategyConfig) []RiskPartition {
	hasLive := false
	hasPaper := false
	sources := make(map[string]bool)
	for _, sc := range cfgs {
		p := partitionFor(sc)
		switch {
		case p.IsLive():
			hasLive = true
		case p.Source == "":
			hasPaper = true
		default:
			sources[p.Source] = true
		}
	}
	out := make([]RiskPartition, 0, 2+len(sources))
	if hasLive {
		out = append(out, livePartition)
	}
	if hasPaper {
		out = append(out, defaultPaperPartition)
	}
	for _, id := range sortedSourceIDs(sources) {
		out = append(out, paperSourcePartition(id))
	}
	return out
}

func sortedSourceIDs(set map[string]bool) []string {
	ids := make([]string, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func strategiesInPartition(cfgs []StrategyConfig, p RiskPartition) []StrategyConfig {
	out := make([]StrategyConfig, 0, len(cfgs))
	for _, sc := range cfgs {
		if partitionFor(sc) == p {
			out = append(out, sc)
		}
	}
	return out
}

func partitionOfStrategyID(cfgs []StrategyConfig, id string) (RiskPartition, bool) {
	for _, sc := range cfgs {
		if sc.ID == id {
			return partitionFor(sc), true
		}
	}
	return unassignedPartition, false
}

func filterStatesByPartition(states map[string]*StrategyState, cfgs []StrategyConfig, p RiskPartition) map[string]*StrategyState {
	out := make(map[string]*StrategyState, len(states))
	for _, sc := range cfgs {
		if partitionFor(sc) != p {
			continue
		}
		if ss, ok := states[sc.ID]; ok {
			out[sc.ID] = ss
		}
	}
	return out
}

func stateIDsInPartition(states map[string]*StrategyState, cfgs []StrategyConfig, p RiskPartition) []string {
	ids := make([]string, 0, len(states))
	for _, sc := range cfgs {
		if partitionFor(sc) != p {
			continue
		}
		if _, ok := states[sc.ID]; ok {
			ids = append(ids, sc.ID)
		}
	}
	sort.Strings(ids)
	return ids
}

func partitionHasPersistedState(cfgs []StrategyConfig, p RiskPartition, persisted map[string]bool) bool {
	for _, sc := range strategiesInPartition(cfgs, p) {
		if persisted[sc.ID] {
			return true
		}
	}
	return false
}

func partitionInList(p RiskPartition, list []RiskPartition) bool {
	for _, candidate := range list {
		if candidate == p {
			return true
		}
	}
	return false
}

// sortedAppliedPartitions lists the partitions a drain actually touched, in the
// stable order, so each owning file is saved exactly once.
func sortedAppliedPartitions(set map[RiskPartition]bool) []RiskPartition {
	out := make([]RiskPartition, 0, len(set))
	for p, applied := range set {
		if applied {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return partitionSortKey(out[i]) < partitionSortKey(out[j]) })
	return out
}
