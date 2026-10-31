package main

import (
	"net/http"
)

const uiPartitionQueryKey = "partition"

// uiPartitionFilter is the resolved dashboard scope of one request. An absent
// query key selects every partition the process owns, which is what every
// caller saw before source navigation existed.
type uiPartitionFilter struct {
	All       bool
	Partition RiskPartition
}

func (f uiPartitionFilter) includes(sc StrategyConfig) bool {
	return f.All || partitionFor(sc) == f.Partition
}

func (f uiPartitionFilter) configs(cfgs []StrategyConfig) []StrategyConfig {
	if f.All {
		return cfgs
	}
	return strategiesInPartition(cfgs, f.Partition)
}

// uiPartitionParam resolves the request's partition. It answers only with a
// partition this process actually owns, so a stale selector cannot read a
// neighbour deployment's view: unparseable text is a bad request and a
// well-formed partition with no configured strategy is not found.
func (ss *StatusServer) uiPartitionParam(w http.ResponseWriter, r *http.Request) (uiPartitionFilter, bool) {
	filter, status, msg := ss.resolveUIPartition(r)
	if status != http.StatusOK {
		writeJSONError(w, status, msg)
		return uiPartitionFilter{}, false
	}
	return filter, true
}

func (ss *StatusServer) resolveUIPartition(r *http.Request) (uiPartitionFilter, int, string) {
	raw := r.URL.Query().Get(uiPartitionQueryKey)
	if raw == "" {
		return uiPartitionFilter{All: true}, http.StatusOK, ""
	}
	p, err := parseRiskPartition(raw)
	if err != nil {
		return uiPartitionFilter{}, http.StatusBadRequest, err.Error()
	}
	if p == unassignedPartition {
		return uiPartitionFilter{}, http.StatusBadRequest, "partition must be live, paper or paper:<source id>"
	}
	ss.strategiesMu.RLock()
	cfgs := append([]StrategyConfig(nil), ss.strategies...)
	ss.strategiesMu.RUnlock()
	if !partitionInList(p, activePartitions(cfgs)) {
		return uiPartitionFilter{}, http.StatusNotFound, "this process owns no partition " + p.String()
	}
	return uiPartitionFilter{Partition: p}, http.StatusOK, ""
}

func (ss *StatusServer) partitionStrategyIDs(p RiskPartition) []string {
	ss.strategiesMu.RLock()
	cfgs := append([]StrategyConfig(nil), ss.strategies...)
	ss.strategiesMu.RUnlock()
	ids := make([]string, 0, len(cfgs))
	for _, sc := range strategiesInPartition(cfgs, p) {
		ids = append(ids, sc.ID)
	}
	return ids
}

func (ss *StatusServer) strategyInPartition(id string, filter uiPartitionFilter) bool {
	if filter.All {
		return true
	}
	ss.strategiesMu.RLock()
	cfgs := append([]StrategyConfig(nil), ss.strategies...)
	ss.strategiesMu.RUnlock()
	p, ok := partitionOfStrategyID(cfgs, id)
	return ok && p == filter.Partition
}
