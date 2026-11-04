package main

import (
	"fmt"
	"time"
)

type IBKROptionPricer struct {
	script string
	run    optionMarksRunner
}

func NewIBKROptionPricer() *IBKROptionPricer {
	return &IBKROptionPricer{script: optionMarksScript, run: runPythonReadOnlyWithStdin}
}

func (p *IBKROptionPricer) Name() string { return "ibkr" }

func (p *IBKROptionPricer) Refresh(reqs []optionLegRequest, refreshAt time.Time) []optionLegReply {
	return runOptionMarksBatch("ibkr", p.script, p.run, reqs, refreshAt, true, ibkrOptionMarkReply)
}

func ibkrOptionMarkReply(req optionLegRequest, leg rhOptionMarksLeg) (optionLegReply, error) {
	reply := optionLegReply{Key: req.Key}
	switch leg.Status {
	case "mark", "settled", "pending", "error":
	default:
		return reply, fmt.Errorf("unknown status %q", leg.Status)
	}
	cutoff, cerr := time.Parse(time.RFC3339Nano, leg.CutoffUTC)
	ready, rerr := time.Parse(time.RFC3339Nano, leg.SettlementReadyUTC)
	if cerr != nil || rerr != nil || !cutoff.Equal(req.KnownCutoff) || !ready.Equal(req.KnownReady) {
		reply.Err = fmt.Sprintf("ibkr reply schedule cutoff_utc %q settlement_ready_utc %q differs from the expected %s and %s",
			leg.CutoffUTC, leg.SettlementReadyUTC, req.KnownCutoff.UTC().Format(time.RFC3339), req.KnownReady.UTC().Format(time.RFC3339))
		return reply, nil
	}
	reply.CutoffAt, reply.ReadyAt = cutoff.UTC(), ready.UTC()
	spec := optionContractSpecs["ibkr"]
	switch leg.Status {
	case "mark":
		mark := optionMarksReplyMark(leg)
		if mark == nil {
			reply.Err = "ibkr mark reply is missing its value, spot or greeks"
			return reply, nil
		}
		if mark.Source != spec.MarkSource {
			reply.Err = fmt.Sprintf("ibkr mark source %q is not %s", mark.Source, spec.MarkSource)
			return reply, nil
		}
		reply.Mark = mark
	case "settled":
		st := leg.Settlement
		if st == nil || st.Price == nil {
			reply.Err = "ibkr settlement reply has no price"
			return reply, nil
		}
		if st.Basis != spec.SettlementBasis || st.Source != spec.SettlementSource || st.Date != req.Expiry {
			reply.Err = fmt.Sprintf("ibkr settlement %s %s on %s is not the %s %s for expiry %s", st.Source, st.Basis, st.Date, spec.SettlementSource, spec.SettlementBasis, req.Expiry)
			return reply, nil
		}
		reply.Settlement = &optionLegSettlement{Price: *st.Price, Basis: st.Basis, Date: st.Date, Source: st.Source}
	case "pending":
		reply.Pending = firstNonEmpty(leg.Reason, "ibkr settlement pending")
	case "error":
		reply.Err = firstNonEmpty(leg.Reason, "ibkr option read failed")
	}
	return reply, nil
}
