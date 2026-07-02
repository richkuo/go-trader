package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// closing_strategies.go implements the #1203 /closing-strategies Discord
// command: a read-only catalog of every registered close evaluator (name,
// description, platforms, config params) sourced from the Python close
// registry (shared_strategies/close/registry.py) via
// shared_tools/close_registry_loader.py --list-json.

// closeRegistryEntry mirrors one row of the Python close registry's
// `--list-json` dump.
type closeRegistryEntry struct {
	Name          string                 `json:"name"`
	Description   string                 `json:"description"`
	DefaultParams map[string]interface{} `json:"default_params"`
	Platforms     []string               `json:"platforms"`
}

var (
	closeRegistryCatalogMu sync.Mutex
	closeRegistryCatalog   []closeRegistryEntry // nil until first successful fetch
)

// fetchCloseRegistryCatalog returns the cached close-evaluator catalog,
// populating it on first call. The registry is static per deploy (a new
// evaluator only ships with a rebuild+restart), so the read-only subprocess
// runs at most once per process lifetime; a failed fetch is never cached, so
// the next command invocation retries.
func fetchCloseRegistryCatalog() ([]closeRegistryEntry, error) {
	closeRegistryCatalogMu.Lock()
	defer closeRegistryCatalogMu.Unlock()
	if closeRegistryCatalog != nil {
		return closeRegistryCatalog, nil
	}
	stdout, stderr, err := runPythonReadOnly("shared_tools/close_registry_loader.py", []string{"--list-json"})
	if err != nil {
		return nil, fmt.Errorf("close registry dump failed: %w (stderr: %s)", err, strings.TrimSpace(string(stderr)))
	}
	var entries []closeRegistryEntry
	if jsonErr := json.Unmarshal(stdout, &entries); jsonErr != nil {
		return nil, fmt.Errorf("close registry dump: invalid JSON: %w", jsonErr)
	}
	closeRegistryCatalog = entries
	return entries, nil
}

// closingStrategiesHeaderBudget reserves room for the per-page header line
// formatClosingStrategiesResponse prepends after packing, so a page's final
// length (header + body) never exceeds discordCharLimit.
const closingStrategiesHeaderBudget = 80

// formatClosingStrategiesResponse renders the close-evaluator catalog as one
// or more Discord messages, chunked to stay under discordCharLimit (same
// splitting approach as writeCatTableChunks/FormatCategorySummary). Pure
// helper — no Python subprocess — so it's fully unit-testable.
func formatClosingStrategiesResponse(cfg *Config, entries []closeRegistryEntry) []string {
	sorted := make([]closeRegistryEntry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	userClose := cfg.userDefaultsClose()
	blocks := make([]string, 0, len(sorted))
	for _, e := range sorted {
		blocks = append(blocks, formatCloseRegistryEntry(e, userClose))
	}
	if len(blocks) == 0 {
		return []string{"No close evaluators registered."}
	}

	pages := packTextBlocks(blocks, discordCharLimit-closingStrategiesHeaderBudget)
	for idx := range pages {
		if idx == 0 {
			pages[idx] = fmt.Sprintf("**Close evaluators (%d registered)**\n\n%s", len(sorted), pages[idx])
		} else {
			pages[idx] = fmt.Sprintf("**Close evaluators (cont'd, page %d/%d)**\n\n%s", idx+1, len(pages), pages[idx])
		}
	}
	return pages
}

// formatCloseRegistryEntry renders one evaluator's name, description,
// platforms, and default params (one `key=value` per line, sorted). When
// user_defaults.close (#866/#1135) overrides a param for this evaluator, the
// effective value is shown in place of the registry default and marked as an
// override, since the registry default is not what actually runs.
func formatCloseRegistryEntry(e closeRegistryEntry, userClose CloseDefaultsMap) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "**%s** — %s\n", e.Name, e.Description)

	platforms := append([]string(nil), e.Platforms...)
	sort.Strings(platforms)
	fmt.Fprintf(&sb, "  platforms: %s\n", strings.Join(platforms, ", "))

	keys := make([]string, 0, len(e.DefaultParams)+1)
	for k := range e.DefaultParams {
		keys = append(keys, k)
	}
	userEntry, hasUserEntry := closeDefaultsEntry(userClose, e.Name)
	_, registryHasTPTiers := e.DefaultParams["tp_tiers"]
	if hasUserEntry && !registryHasTPTiers {
		// Some override-eligible evaluators (e.g. trailing_tp_ratchet_regime)
		// ship empty registry default_params — the operator-configured
		// tp_tiers is the only value that ever runs, so surface it even
		// though the registry itself has no tp_tiers key to iterate.
		if tp, ok := userEntry["tp_tiers"]; ok && tp != nil {
			keys = append(keys, "tp_tiers")
		}
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		sb.WriteString("  params: (none)\n")
	}
	for _, k := range keys {
		if k == "tp_tiers" && hasUserEntry {
			if tp, ok := userEntry["tp_tiers"]; ok && tp != nil {
				fmt.Fprintf(&sb, "  %s=%s (user_defaults.close override)\n", k, jsonInline(tp))
				continue
			}
		}
		fmt.Fprintf(&sb, "  %s=%s\n", k, jsonInline(e.DefaultParams[k]))
	}
	sb.WriteString("\n")
	return sb.String()
}

// jsonInline compact-encodes a param value for display; falls back to Go's
// default formatting if the value somehow isn't JSON-marshalable.
func jsonInline(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

// packTextBlocks greedily packs blocks into pages, each page's body capped at
// limit bytes. A single block longer than limit becomes its own oversized
// page rather than being split mid-block.
func packTextBlocks(blocks []string, limit int) []string {
	var pages []string
	var cur strings.Builder
	for _, b := range blocks {
		if cur.Len() > 0 && cur.Len()+len(b) > limit {
			pages = append(pages, cur.String())
			cur.Reset()
		}
		cur.WriteString(b)
	}
	if cur.Len() > 0 {
		pages = append(pages, cur.String())
	}
	return pages
}
