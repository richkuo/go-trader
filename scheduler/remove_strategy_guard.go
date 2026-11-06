package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

const removeStrategyHistoryNote = "Removal is refused unless the strategy is paused and holds no position, option leg or queued action; its trade history stays in the state DB."

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
	if len(syms) > 0 {
		advice := "The running daemon does not list it in its roster; restart so it loads the strategy, then close them first."
		if ok {
			advice = removeStrategyCloseAdvice(sc, view)
		}
		return fmt.Sprintf("strategy %q still holds open position or option-leg rows (%s); removing it now would let the next restart delete them with no close trade. %s",
			id, strings.Join(syms, ", "), advice)
	}
	if !ok {
		return ""
	}
	if !sc.Paused {
		return fmt.Sprintf("strategy %q is not paused in the running daemon, so a cycle can still open a position after this check and the restart would then delete it with no close trade. Pause it first (dashboard pause toggle or `/go-trader-config set strategies.%s.paused true`), wait for the reload, then remove it.", id, id)
	}
	if src := replayMirrorSourceID(sc); src != "" {
		return fmt.Sprintf("strategy %q is a replay mirror of %s, and pause does not hold replayed opens. Turn off its replay_sharing while it is flat, wait for the reload, then remove it.", id, src)
	}
	if reason := ss.removeStrategyQueuedRowRefusal(id); reason != "" {
		return reason
	}
	return ""
}

func (ss *StatusServer) removeStrategyQueuedRowRefusal(id string) string {
	if ss.stateDB == nil {
		return "state store unavailable; cannot prove the strategy has no queued action"
	}
	actions, err := ss.stateDB.LoadPendingManualActions()
	if err != nil {
		return fmt.Sprintf("could not read queued manual actions (%v); cannot prove the strategy has no queued action", err)
	}
	var queued []string
	for _, a := range actions {
		if a.StrategyID == id {
			queued = append(queued, fmt.Sprintf("manual %s %s", a.Action, a.Symbol))
		}
	}
	orders, err := ss.stateDB.LoadPendingLimitOrders()
	if err != nil {
		return fmt.Sprintf("could not read resting limit orders (%v); cannot prove the strategy has no queued action", err)
	}
	for _, o := range orders {
		if o.StrategyID == id {
			queued = append(queued, fmt.Sprintf("resting limit %s %s", o.Side, o.Symbol))
		}
	}
	if len(queued) == 0 {
		return ""
	}
	sort.Strings(queued)
	return fmt.Sprintf("strategy %q has queued rows that can still open a position (%s); let them apply or cancel them, then remove it.", id, strings.Join(queued, ", "))
}

const removeStrategyRestartNote = "A removal always restarts the daemon, whatever the restart box says, and dashboard open and add refuse the strategy until then."

func (ss *StatusServer) removeStrategyGuarded(id string) error {
	ss.tradeActionMu.Lock()
	defer ss.tradeActionMu.Unlock()
	err := ss.mutateConfigRoot(func(root map[string]json.RawMessage) error {
		if reason := ss.removeStrategyRefusal(id); reason != "" {
			return fmt.Errorf("%s", reason)
		}
		return removeStrategyFromRoot(root, id)
	})
	if err != nil {
		return err
	}
	ss.removedMu.Lock()
	if ss.removedStrategies == nil {
		ss.removedStrategies = make(map[string]bool)
	}
	ss.removedStrategies[id] = true
	ss.removedMu.Unlock()
	return nil
}

func (ss *StatusServer) removedStrategyRefusal(id string) string {
	ss.removedMu.Lock()
	defer ss.removedMu.Unlock()
	if ss.removedStrategies[id] {
		return fmt.Sprintf("strategy %q was removed from the config; it opens nothing until the restart that drops it", id)
	}
	return ""
}
