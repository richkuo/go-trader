package main

func optionsPaperOnlyReason(platform string) string {
	return "options strategies run as paper only: no venue order path exists for options on " + platform
}

func optionsLiveModeConfigError(platform string) string {
	return optionsPaperOnlyReason(platform) + "; remove the strategy, or replace --mode=live (or --mode live) with --mode=paper under a new id"
}
