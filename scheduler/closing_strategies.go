package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
)


type closeRegistryEntry struct {
	Name          string                 `json:"name"`
	Description   string                 `json:"description"`
	DefaultParams map[string]interface{} `json:"default_params"`
	Platforms     []string               `json:"platforms"`
}

var (
	closeRegistryCatalogMu sync.Mutex
	closeRegistryCatalog   []closeRegistryEntry
)

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

const closingStrategiesHeaderBudget = 80

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
	overrideKeys := []string{"tp_tiers"}
	if strings.ToLower(strings.TrimSpace(e.Name)) == trailingTPRatchetRegimeCloseName {
		overrideKeys = append(overrideKeys, userCloseDefaultTrailingStopATRRegimeKey)
	}
	isOverrideKey := func(k string) bool {
		for _, ok := range overrideKeys {
			if k == ok {
				return true
			}
		}
		return false
	}
	if hasUserEntry {
		for _, ok := range overrideKeys {
			if _, registryHas := e.DefaultParams[ok]; registryHas {
				continue
			}
			if v, present := userEntry[ok]; present && v != nil {
				keys = append(keys, ok)
			}
		}
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		sb.WriteString("  params: (none)\n")
	}
	for _, k := range keys {
		if hasUserEntry && isOverrideKey(k) {
			if v, ok := userEntry[k]; ok && v != nil {
				fmt.Fprintf(&sb, "  %s=%s (user_defaults.close override)\n", k, jsonInline(v))
				continue
			}
		}
		fmt.Fprintf(&sb, "  %s=%s\n", k, jsonInline(e.DefaultParams[k]))
	}
	sb.WriteString("\n")
	return sb.String()
}

func jsonInline(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

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
