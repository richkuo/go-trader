package main

func optionsPaperOnlyReason(platform string) string {
	return "options strategies run as paper only: no venue order path exists for options on " + platform
}

func optionsLiveModeConfigError(platform string) string {
	return optionsPaperOnlyReason(platform) + "; remove --mode=live (or --mode live) or use --mode=paper"
}
