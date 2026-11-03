package main

import "time"

type OptionPricer interface {
	Name() string

	Refresh(reqs []optionLegRequest, refreshAt time.Time) []optionLegReply
}
