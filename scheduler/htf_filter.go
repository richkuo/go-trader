package main

import (
	"fmt"
	"os"
	"strings"
)

const (
	htfFilterStatusOK           = "ok"
	htfFilterStatusUnavailable  = "unavailable"
	htfFilterStatusSkipped      = "skipped"
	htfFilterStatusNotEvaluated = "not_evaluated"
)

type HTFFilterEcho struct {
	Status    string `json:"status"`
	Timeframe string `json:"timeframe"`
	Trend     *int   `json:"trend"`
	Reason    string `json:"reason"`
}

type htfFilterContext struct {
	MarketClosed bool
}

func topStepHTFFilterContext(result *TopStepResult) htfFilterContext {
	if result == nil || result.MarketOpen == nil {
		return htfFilterContext{}
	}
	return htfFilterContext{MarketClosed: !*result.MarketOpen && result.Signal == 0 && result.CloseFraction == 0}
}

func htfFilterContractError(sc StrategyConfig, fields StrategyDecisionFields, ctx htfFilterContext) string {
	const redeploy = " (redeploy with scripts/update.sh so Go and Python match)"
	echo := fields.HTFFilter
	if !sc.HTFFilter {
		if echo != nil {
			return "higher-timeframe filter contract mismatch: htf_filter is off but the check returned an htf_filter block" + redeploy
		}
		return ""
	}
	if echo == nil {
		return "higher-timeframe filter contract mismatch: sent --htf-filter but the check returned no htf_filter block; holding this signal because the check cannot show the filter ran" + redeploy
	}
	if strings.TrimSpace(echo.Timeframe) == "" {
		return "higher-timeframe filter contract mismatch: the htf_filter block has no timeframe" + redeploy
	}
	if echo.Trend == nil {
		return "higher-timeframe filter contract mismatch: the htf_filter block has no trend" + redeploy
	}
	trend := *echo.Trend
	if trend < -1 || trend > 1 {
		return fmt.Sprintf("higher-timeframe filter contract mismatch: the htf_filter block reports trend %d (want -1, 0 or 1)%s", trend, redeploy)
	}
	reason := strings.TrimSpace(echo.Reason)
	switch echo.Status {
	case htfFilterStatusOK:
		return ""
	case htfFilterStatusUnavailable, htfFilterStatusSkipped:
		if trend != 0 || reason == "" {
			return fmt.Sprintf("higher-timeframe filter contract mismatch: an htf_filter status %q needs trend 0 and a reason%s", echo.Status, redeploy)
		}
		return ""
	case htfFilterStatusNotEvaluated:
		if trend != 0 || reason == "" {
			return fmt.Sprintf("higher-timeframe filter contract mismatch: an htf_filter status %q needs trend 0 and a reason%s", echo.Status, redeploy)
		}
		if heldClosedBar := sc.ClosedBarDecisions && fields.ClosedBar != nil && fields.ClosedBar.Enabled && fields.ClosedBar.Held && strings.TrimSpace(fields.ClosedBar.HoldReason) != ""; heldClosedBar {
			return ""
		}
		if ctx.MarketClosed {
			return ""
		}
		return "higher-timeframe filter contract mismatch: the check reported the filter as not evaluated without a held closed-bar decision or a closed market, so it cannot authorize this signal" + redeploy
	}
	return fmt.Sprintf("higher-timeframe filter contract mismatch: unknown htf_filter status %q%s", echo.Status, redeploy)
}

func htfFilterEntryHold(sc StrategyConfig, fields StrategyDecisionFields) (bool, string) {
	if !sc.HTFFilter {
		return false, ""
	}
	echo := fields.HTFFilter
	if echo == nil {
		return true, "the check returned no higher-timeframe filter result"
	}
	switch echo.Status {
	case htfFilterStatusOK, htfFilterStatusSkipped:
		return false, ""
	case htfFilterStatusUnavailable:
		return true, fmt.Sprintf("higher-timeframe %s input unavailable: %s", echo.Timeframe, echo.Reason)
	case htfFilterStatusNotEvaluated:
		return true, fmt.Sprintf("higher-timeframe %s filter not evaluated: %s", echo.Timeframe, echo.Reason)
	}
	return true, fmt.Sprintf("higher-timeframe %s filter status %q is unknown", echo.Timeframe, echo.Status)
}

func htfFilterHoldsSignal(sc StrategyConfig, fields StrategyDecisionFields, signal int, closeFraction, posQty float64, posSide string, allowsLong, allowsShort bool) (bool, string) {
	held, why := htfFilterEntryHold(sc, fields)
	if !held || !pausedBlocksSignal(signal, closeFraction, posQty, posSide, allowsLong, allowsShort) {
		return false, ""
	}
	return true, why
}

func logHTFFilterHold(signalStr, why string, logger *StrategyLogger) {
	if logger == nil {
		return
	}
	logger.Warn("HTF filter: %s signal suppressed — %s; entries and additions are held while filter input is unavailable, and closes, stops, ratchet and protection continue (#1826)", signalStr, why)
}

var (
	htfFilterTracker          = &ScriptFailureTracker{}
	htfFilterTransientTracker = &ScriptFailureTracker{}
)

func formatHTFFilterUnavailableAlert(sc StrategyConfig, echo *HTFFilterEcho, part RiskPartition, count int, transient bool) string {
	kind := ""
	if transient {
		kind = " (sustained upstream error)"
	}
	return fmt.Sprintf("**HTF FILTER UNAVAILABLE%s** [%s] %s (pid=%d, partition=%s, HTF=%s, %d unavailable evaluations): %s - new entries and additions are held while filter input is unavailable; closes and protection continue",
		kind, sc.ID, sc.Platform, os.Getpid(), partitionLabel(part), echo.Timeframe, count, echo.Reason)
}

func formatHTFFilterRecoveredAlert(sc StrategyConfig, echo *HTFFilterEcho, part RiskPartition, priorCount int) string {
	return fmt.Sprintf("**HTF FILTER RECOVERED** [%s] %s (pid=%d, partition=%s, HTF=%s): the filter evaluated again after %d unavailable evaluations; entries follow the filter again",
		sc.ID, sc.Platform, os.Getpid(), partitionLabel(part), echo.Timeframe, priorCount)
}

func sendHTFFilterAlert(notifier *MultiNotifier, part RiskPartition, level, msg string) {
	fmt.Printf("[%s] %s\n", level, msg)
	if notifier == nil || !notifier.HasBackends() {
		return
	}
	notifier.SendToPartitionChannels(part, msg)
	notifier.SendOwnerDM(partitionPrefixedDM(part, msg))
}

func clearHTFFilterTrackers(id string) (bool, int) {
	recovered, prior := htfFilterTracker.Clear(id)
	transientRecovered, transientPrior := htfFilterTransientTracker.Clear(id)
	if transientPrior > prior {
		prior = transientPrior
	}
	return recovered || transientRecovered, prior
}

func observeHTFFilterResult(notifier *MultiNotifier, sc StrategyConfig, fields StrategyDecisionFields) {
	if !sc.HTFFilter || fields.HTFFilter == nil {
		return
	}
	echo := fields.HTFFilter
	part := partitionFor(sc)
	switch echo.Status {
	case htfFilterStatusUnavailable:
		transient := scriptFailureErrorIsTransient(echo.Reason)
		var shouldNotify bool
		var count int
		if transient {
			shouldNotify, count = recordScriptFailureAtThreshold(htfFilterTransientTracker, sc.ID, echo.Reason, tradingNow(),
				scriptFailureTransientAlertThreshold, scriptFailureTransientAlertMaxDuration)
		} else {
			shouldNotify, count = recordScriptFailureAtThreshold(htfFilterTracker, sc.ID, echo.Reason, tradingNow(),
				scriptFailureAlertThreshold, 0)
		}
		if shouldNotify {
			sendHTFFilterAlert(notifier, part, "WARN", formatHTFFilterUnavailableAlert(sc, echo, part, count, transient))
		}
	case htfFilterStatusOK:
		if recovered, prior := clearHTFFilterTrackers(sc.ID); recovered {
			sendHTFFilterAlert(notifier, part, "INFO", formatHTFFilterRecoveredAlert(sc, echo, part, prior))
		}
	case htfFilterStatusSkipped:
		clearHTFFilterTrackers(sc.ID)
	}
}
