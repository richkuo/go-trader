package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

const (
	strategyCatalogScript        = "shared_tools/strategy_catalog.py"
	strategyCatalogSchemaVersion = 1

	catalogRoleScalarOpen      = "scalar_open"
	catalogRoleQuoteCapability = "quote_capability"
	catalogRoleNativeClose     = "native_close"
)

type catalogPaperExecution struct {
	Status      string          `json:"status"`
	Route       json.RawMessage `json:"route"`
	Requires    []string        `json:"requires"`
	Reason      string          `json:"reason"`
	TrackingURL string          `json:"tracking_url"`
}

type catalogEntry struct {
	ID                string                 `json:"id"`
	Description       string                 `json:"description"`
	Kind              string                 `json:"kind"`
	Role              string                 `json:"role"`
	Platforms         []string               `json:"platforms"`
	DefaultParams     map[string]interface{} `json:"default_params"`
	EdgeStatus        string                 `json:"edge_status"`
	ProductionRefused bool                   `json:"production_refused"`
	ResearchOnly      bool                   `json:"research_only"`
	OwnerRole         string                 `json:"owner_role"`
	LiveExecution     string                 `json:"live_execution"`
	PaperExecution    catalogPaperExecution  `json:"paper_execution"`
}

type strategyCatalogDoc struct {
	SchemaVersion int            `json:"schema_version"`
	Entries       []catalogEntry `json:"entries"`
	Error         string         `json:"error"`
}

var errStrategyCatalogEnvironment = errors.New("python environment unavailable")

func decodeStrategyCatalog(stdout []byte) ([]catalogEntry, error) {
	var doc strategyCatalogDoc
	if err := json.Unmarshal(stdout, &doc); err != nil {
		return nil, fmt.Errorf("strategy catalog: invalid JSON: %w", err)
	}
	if doc.Error != "" {
		return nil, fmt.Errorf("strategy catalog: %s", doc.Error)
	}
	if doc.SchemaVersion != strategyCatalogSchemaVersion {
		return nil, fmt.Errorf("strategy catalog: schema_version %d, want %d", doc.SchemaVersion, strategyCatalogSchemaVersion)
	}
	for _, e := range doc.Entries {
		if strings.TrimSpace(e.ID) == "" || (e.Kind != "open" && e.Kind != "close") {
			return nil, fmt.Errorf("strategy catalog: malformed entry %+v", e)
		}
	}
	return doc.Entries, nil
}

func fetchStrategyCatalog(kind string) ([]catalogEntry, error) {
	stdout, stderr, err := runPythonReadOnly(strategyCatalogScript, []string{"--list-json", "--kind", kind})
	if err != nil {
		if entries, derr := decodeStrategyCatalog(stdout); derr != nil && len(stdout) > 0 {
			return nil, derr
		} else if derr == nil {
			return entries, nil
		}
		return nil, fmt.Errorf("%w: %v (stderr: %s)", errStrategyCatalogEnvironment, err, strings.TrimSpace(string(stderr)))
	}
	return decodeStrategyCatalog(stdout)
}

func (e catalogEntry) registeredFor(platform string) bool {
	for _, p := range e.Platforms {
		if p == platform {
			return true
		}
	}
	return false
}

func (e catalogEntry) labels() []string {
	var out []string
	if e.EdgeStatus == edgeStatusNoEdge {
		out = append(out, "no_edge")
	}
	if e.ProductionRefused || e.ResearchOnly {
		out = append(out, "research-only")
	}
	if e.LiveExecution == "refused" {
		out = append(out, "live refused")
	}
	if e.OwnerRole == "quote_coordinator" {
		out = append(out, "quote-owned")
	}
	if e.PaperExecution.Status == "held" {
		out = append(out, "engine held")
	}
	if e.PaperExecution.Status == "refused" {
		out = append(out, "paper refused")
	}
	return out
}

func catalogStratDefs(entries []catalogEntry, platform string, keep func(catalogEntry) bool) []stratDef {
	var out []stratDef
	for _, e := range entries {
		if e.Kind != "open" || !e.registeredFor(platform) || !keep(e) {
			continue
		}
		out = append(out, stratDef{ID: e.ID, ShortName: deriveShortName(e.ID)})
	}
	return out
}

func catalogCloseEntries(entries []catalogEntry) []closeRegistryEntry {
	var out []closeRegistryEntry
	for _, e := range entries {
		if e.Kind != "close" {
			continue
		}
		out = append(out, closeRegistryEntry{
			Name:          e.ID,
			Description:   e.Description,
			DefaultParams: e.DefaultParams,
			Platforms:     append([]string(nil), e.Platforms...),
			Role:          e.Role,
			OwnerRole:     e.OwnerRole,
			Labels:        e.labels(),
			HoldReason:    e.PaperExecution.Reason,
			TrackingURL:   e.PaperExecution.TrackingURL,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
