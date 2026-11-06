package main

import "fmt"

func paperOnlyLiveStrategyErrors(cfg *Config) []string {
	if cfg == nil || !cfg.PaperOnly {
		return nil
	}
	var errs []string
	for i, sc := range cfg.Strategies {
		if portfolioScopeFor(sc) != ScopeLive {
			continue
		}
		label := sc.ID
		if label == "" {
			label = fmt.Sprintf("%d", i)
		}
		errs = append(errs, fmt.Sprintf("strategy[%s]: paper_only is true, so a live strategy (--mode=live) is refused; remove the strategy, or remove paper_only and restart", label))
	}
	return errs
}
