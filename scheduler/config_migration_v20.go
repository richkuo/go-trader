package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

const v20LegacyAllowDeprecatedKey = "allow_deprecated"

var v20FormerResearchStrategies = map[string]struct{}{
	"analog_retrieval":            {},
	"awesome_oscillator":          {},
	"chaikin_money_flow_breakout": {},
	"commodity_channel_trend":     {},
	"connors_rsi_reversion":       {},
	"open_interest_breakout":      {},
	"relative_vigor_index":        {},
	"vortex_trend":                {},
}

type noEdgeStampedStrategy struct {
	ID   string
	Refs []edgeReference
}

type noEdgeMigrationReport struct {
	Applied        bool
	InputVersion   int
	RemovedKeyIDs  []string
	Stamped        []noEdgeStampedStrategy
	VetoedResearch []string
}

func (r *noEdgeMigrationReport) changedLegacyInput() bool {
	if r == nil || !r.Applied {
		return false
	}
	return r.InputVersion != 0 || len(r.RemovedKeyIDs) > 0 || len(r.Stamped) > 0
}

func (r *noEdgeMigrationReport) lines() []string {
	if r == nil || !r.Applied {
		return nil
	}
	var lines []string
	if len(r.Stamped) == 0 {
		lines = append(lines, "v20 no-edge migration stamped no strategy with allow_no_edge: true.")
	} else {
		parts := make([]string, 0, len(r.Stamped))
		for _, st := range r.Stamped {
			refs := make([]string, 0, len(st.Refs))
			for _, ref := range st.Refs {
				ev, _ := noEdgeEvidenceFor(ref.Name)
				refs = append(refs, fmt.Sprintf("%s (%s; %s)", ref.String(), ev.Source, ev.Ref))
			}
			parts = append(parts, st.ID+": "+strings.Join(refs, ", "))
		}
		lines = append(lines, fmt.Sprintf("v20 no-edge migration stamped allow_no_edge: true on %d live strategy(ies) that ran before the update: %s.",
			len(r.Stamped), strings.Join(parts, "; ")))
	}
	if len(r.VetoedResearch) > 0 {
		lines = append(lines, fmt.Sprintf("v20 no-edge migration did not stamp %s: a former research-only reference was never allowed live, so add allow_no_edge: true yourself only if you accept live use.",
			strings.Join(r.VetoedResearch, ", ")))
	}
	if len(r.RemovedKeyIDs) > 0 {
		lines = append(lines, fmt.Sprintf("v20 no-edge migration removed allow_deprecated from %s.", strings.Join(r.RemovedKeyIDs, ", ")))
	}
	return lines
}

const v20NoEdgeNotice = "**Note:** strategies without approved edge evidence now share one label, `edge_status: no_edge` (#1681). " +
	"Paper evaluation needs an explicit `--mode=paper` in args and no acknowledgement. Live use, or args with no `--mode`, " +
	"needs `\"allow_no_edge\": true` on the strategy, and startup and inspect still warn. `allow_deprecated` is removed: " +
	"the former explicit paper warning option has no successor, so paper shows the edge tag without an edge-warning direct message."

func (r *noEdgeMigrationReport) notice() string {
	lines := r.lines()
	if len(lines) == 0 {
		return ""
	}
	return v20NoEdgeNotice + "\n" + strings.Join(lines, "\n")
}

func needsV20NoEdgeMigration(data []byte) bool {
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return false
	}
	ver := 0
	if v, ok := raw["config_version"].(float64); ok {
		ver = int(v)
	}
	return ver < 20 || hasLegacyAllowDeprecatedKey(raw)
}

func hasLegacyAllowDeprecatedKey(raw map[string]interface{}) bool {
	strategies, _ := raw["strategies"].([]interface{})
	for _, item := range strategies {
		sc, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		if _, present := sc[v20LegacyAllowDeprecatedKey]; present {
			return true
		}
	}
	return false
}

func migrateV20NoEdge(raw map[string]interface{}, inputVersion int) (*noEdgeMigrationReport, error) {
	report := &noEdgeMigrationReport{Applied: true, InputVersion: inputVersion}
	legacyInput := inputVersion < 20
	strategies, _ := raw["strategies"].([]interface{})
	for _, item := range strategies {
		sc, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		id := strictStringFromJSON(sc["id"])
		if _, present := sc[v20LegacyAllowDeprecatedKey]; present {
			delete(sc, v20LegacyAllowDeprecatedKey)
			report.RemovedKeyIDs = append(report.RemovedKeyIDs, id)
		}
		if !legacyInput {
			continue
		}
		if _, present := sc["allow_no_edge"]; present {
			continue
		}
		blob, err := json.Marshal(sc)
		if err != nil {
			return nil, fmt.Errorf("v20 no-edge migration: marshal strategy %q: %w", id, err)
		}
		var typed StrategyConfig
		if err := json.Unmarshal(blob, &typed); err != nil {
			continue
		}
		effective := effectiveIdentityStrategy(typed)
		if edgeGateModeForStrategy(effective).Kind != edgeGateModeLive {
			continue
		}
		refs := effectiveStrategyReferences(effective)
		var gated []edgeReference
		vetoed := false
		for _, ref := range refs {
			if _, research := v20FormerResearchStrategies[ref.Name]; research {
				vetoed = true
			}
			if _, ok := noEdgeEvidenceFor(ref.Name); ok {
				gated = append(gated, ref)
			}
		}
		if len(gated) == 0 {
			continue
		}
		if vetoed {
			report.VetoedResearch = append(report.VetoedResearch, id)
			continue
		}
		sc["allow_no_edge"] = true
		report.Stamped = append(report.Stamped, noEdgeStampedStrategy{ID: id, Refs: gated})
	}
	sort.Strings(report.RemovedKeyIDs)
	sort.Strings(report.VetoedResearch)
	sort.Slice(report.Stamped, func(i, j int) bool { return report.Stamped[i].ID < report.Stamped[j].ID })
	return report, nil
}

func deliverNoEdgeMigrationNotice(report *noEdgeMigrationReport, notifier *MultiNotifier) {
	if !report.changedLegacyInput() {
		return
	}
	msg := report.notice()
	if notifier != nil && notifier.HasOwner() {
		notifier.SendOwnerDM(msg)
		return
	}
	for _, line := range strings.Split(msg, "\n") {
		fmt.Printf("[migration] %s\n", line)
	}
}
