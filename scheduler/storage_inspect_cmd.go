package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
)

func runStorageInspect(args []string) int {
	fs := flag.NewFlagSet("storage-inspect", flag.ContinueOnError)
	configPath := fs.String("config", "scheduler/config.json", "Path to config file")
	jsonOut := fs.Bool("json", false, "Emit the ownership report as JSON (machine-readable)")
	requireIdle := fs.Bool("require-idle", false, "Reject when any state file is owned by a running process")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	// --json must emit JSON and nothing else, so the config loader's progress
	// lines are routed to stderr for the load.
	cfg, err := loadConfigQuietForJSON(*configPath, *jsonOut)
	if err != nil {
		fmt.Fprintf(os.Stderr, "storage-inspect: failed to load config %s: %v\n", *configPath, err)
		return 1
	}
	si, err := inspectStorageLayoutForConfig(cfg, *requireIdle)
	if err != nil {
		fmt.Fprintf(os.Stderr, "storage-inspect: %v\n", err)
		return 1
	}
	if *jsonOut {
		out, err := json.MarshalIndent(si, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "storage-inspect: %v\n", err)
			return 1
		}
		fmt.Println(string(out))
	} else {
		fmt.Print(formatStorageInspection(si))
	}
	if !si.OK() {
		return 1
	}
	return 0
}

// inspectStorageLayoutForConfig resolves the layout and the identity map, then
// runs the read-only ownership check. A config that cannot produce a valid
// identity map is itself a rejection, reported without touching any file.
func inspectStorageLayoutForConfig(cfg *Config, requireIdle bool) (storageInspection, error) {
	layout, err := resolveStorageLayout(cfg)
	if err != nil {
		return storageInspection{Rejections: []string{err.Error()}}, nil
	}
	ident, err := buildStorageIdentityMap(cfg, layout)
	if err != nil {
		return storageInspection{Split: layout.Split, Layout: layout.describe(), Rejections: []string{err.Error()}}, nil
	}
	return inspectStorageOwnership(layout, ident, cfg, requireIdle)
}

func loadConfigQuietForJSON(path string, quiet bool) (*Config, error) {
	if !quiet {
		return LoadConfig(path)
	}
	saved := os.Stdout
	os.Stdout = os.Stderr
	defer func() { os.Stdout = saved }()
	return LoadConfig(path)
}
