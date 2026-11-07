package main

import (
	"flag"
	"fmt"
	"os"
)

const (
	cpuClassPaper = "paper"
	cpuClassLive  = "live"
	cpuClassFeed  = "feed"
)

func runCPUClass(args []string) int {
	fs := flag.NewFlagSet("cpu-class", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configPath := fs.String("config", "scheduler/config.json", "Path to config file")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "cpu-class: unexpected arguments: %v\n", fs.Args())
		return 2
	}
	class, err := classifyConfigCPU(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cpu-class: %v\n", err)
		return 1
	}
	fmt.Println(class)
	return 0
}

func classifyConfigCPU(path string) (string, error) {
	role, err := peekConfigRole(path)
	if err != nil {
		return "", fmt.Errorf("read config %s: %w", path, err)
	}
	if role == configRoleFeed {
		return cpuClassFeed, nil
	}
	saved := os.Stdout
	os.Stdout = os.Stderr
	cfg, err := LoadConfigForProbe(path)
	os.Stdout = saved
	if err != nil {
		return "", fmt.Errorf("load config %s: %w", path, err)
	}
	if cfg.PaperOnly {
		return cpuClassPaper, nil
	}
	return cpuClassLive, nil
}
