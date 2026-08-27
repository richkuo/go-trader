package main


func pausedBlocksSignal(signal int, closeFraction, posQty float64, posSide string, allowsLong, allowsShort bool) bool {
	if signal == 0 {
		return false
	}
	if posQty <= 0 {
		return true
	}
	if closeFraction > 0 {
		return false
	}
	if signal == -1 && posSide != "short" && !allowsShort {
		return false
	}
	if signal == 1 && posSide == "short" && !allowsLong {
		return false
	}
	return true
}

func pausedOptionsActions(actions []OptionsAction) (kept []OptionsAction, dropped int) {
	for _, a := range actions {
		if a.Action == "close" {
			kept = append(kept, a)
		} else {
			dropped++
		}
	}
	return kept, dropped
}
