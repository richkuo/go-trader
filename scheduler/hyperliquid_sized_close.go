package main

func hlSizedCloseExecuteShim(res *HyperliquidCloseResult, out hlSizedCloseOutcome) *HyperliquidExecuteResult {
	shim := &HyperliquidExecuteResult{Platform: "hyperliquid", OrderOutcome: "unknown"}
	if res != nil {
		shim.Error = res.Error
		shim.CancelStopLossError = res.CancelStopLossError
		shim.CancelStopLossSucceeded = res.CancelStopLossSucceeded
		shim.CancelStopLossSucceededOIDs = cloneInt64s(res.CancelStopLossSucceededOIDs)
		shim.CancelStopLossFailedOIDs = cloneInt64s(res.CancelStopLossFailedOIDs)
	}
	switch {
	case out.NotSent:
		shim.OrderOutcome = "not_sent"
	case out.Known && out.Filled > 0:
		shim.OrderOutcome = "filled"
		shim.Execution = &HyperliquidExecution{Fill: &HyperliquidFill{AvgPx: out.AvgPx, TotalSz: out.Filled, OID: out.OID, Fee: out.Fee}}
	case out.Known:
		shim.OrderOutcome = "rejected"
	}
	return shim
}
