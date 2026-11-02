package main

func countKillSwitchEvents(prs *PortfolioRiskState, eventType string) int {
	n := 0
	for _, e := range prs.Events {
		if e.Type == eventType {
			n++
		}
	}
	return n
}
