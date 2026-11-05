package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

const removeStrategyHistoryNote = "Removal is refused while it holds an open position or option leg; its trade history stays in the state DB."

func (ss *StatusServer) strategyPositionSymbols(id string) ([]string, *StrategyState, error) {
	if ss == nil || ss.state == nil || ss.mu == nil {
		return nil, nil, fmt.Errorf("status server or state not available; cannot prove the strategy is flat")
	}
	ss.mu.RLock()
	defer ss.mu.RUnlock()
	s := ss.state.Strategies[id]
	if s == nil {
		return nil, nil, nil
	}
	var syms []string
	for k, pos := range s.Positions {
		if pos != nil {
			syms = append(syms, k)
		}
	}
	for k, leg := range s.OptionPositions {
		if leg != nil {
			syms = append(syms, k)
		}
	}
	sort.Strings(syms)
	view := &StrategyState{Quote: s.Quote}
	return syms, view, nil
}

func removeStrategyCloseAdvice(sc StrategyConfig, s *StrategyState) string {
	switch {
	case quoteFundingOwned(sc, s):
		return fmt.Sprintf("It is quote-owned: its inventory exits only through the %s lane of the admitted quote coordinator.", inventoryQuoteExitCloseName)
	case portfolioScopeFor(sc) == ScopePaper:
		return fmt.Sprintf("Close them first with `/go-trader-%s %s` or the dashboard paper-close action.", paperCloseCommandName, sc.ID)
	case sc.Type == "manual":
		return fmt.Sprintf("Close them first with `go-trader manual-close %s` or the dashboard close action.", sc.ID)
	case sc.Platform == "hyperliquid" && sc.Type == "perps":
		return fmt.Sprintf("Close them first with `go-trader force-close %s` or the dashboard force-close action.", sc.ID)
	}
	return fmt.Sprintf("No close command exists for platform=%s type=%s; let it exit through its own signal or stop first.", sc.Platform, sc.Type)
}

func (ss *StatusServer) removeStrategyRefusal(id string) string {
	if ss == nil {
		return "status server not available; cannot prove the strategy is flat"
	}
	sc, ok := ss.strategyConfig(id)
	syms, view, err := ss.strategyPositionSymbols(id)
	if err != nil {
		return err.Error()
	}
	if len(syms) == 0 {
		return ""
	}
	advice := "The running daemon does not list it in its roster; restart so it loads the strategy, then close them first."
	if ok {
		advice = removeStrategyCloseAdvice(sc, view)
	}
	return fmt.Sprintf("strategy %q still holds open position or option-leg rows (%s); removing it now would let the next restart delete them with no close trade. %s",
		id, strings.Join(syms, ", "), advice)
}

func (ss *StatusServer) removeStrategyGuarded(id string) error {
	return ss.mutateConfigRoot(func(root map[string]json.RawMessage) error {
		if reason := ss.removeStrategyRefusal(id); reason != "" {
			return fmt.Errorf("%s", reason)
		}
		return removeStrategyFromRoot(root, id)
	})
}
