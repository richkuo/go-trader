package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

const (
	optionMarksScript          = "shared_scripts/fetch_option_marks.py"
	robinhoodOptionMarksScript = optionMarksScript
	optionMarksContract        = "option_marks_v1"
)

type optionMarksRunner func(script string, args []string, stdin []byte) ([]byte, []byte, error)

type optionMarksLegDecoder func(req optionLegRequest, leg rhOptionMarksLeg) (optionLegReply, error)

type RobinhoodOptionPricer struct {
	script string
	run    optionMarksRunner
}

func NewRobinhoodOptionPricer() *RobinhoodOptionPricer {
	return &RobinhoodOptionPricer{script: robinhoodOptionMarksScript, run: runPythonReadOnlyWithStdin}
}

func (p *RobinhoodOptionPricer) Name() string { return "robinhood" }

type rhOptionMarksRequestLeg struct {
	ID                 string  `json:"id"`
	Underlying         string  `json:"underlying"`
	OptionType         string  `json:"option_type"`
	Strike             float64 `json:"strike"`
	Expiry             string  `json:"expiry"`
	Action             string  `json:"action"`
	DTE                float64 `json:"dte"`
	Operation          string  `json:"operation"`
	CutoffUTC          string  `json:"cutoff_utc,omitempty"`
	SettlementReadyUTC string  `json:"settlement_ready_utc,omitempty"`
}

type rhOptionMarksRequest struct {
	Contract  string                    `json:"contract"`
	Platform  string                    `json:"platform"`
	RefreshAt string                    `json:"refresh_at"`
	Legs      []rhOptionMarksRequestLeg `json:"legs"`
}

type rhOptionMarksDoc struct {
	Contract    *string            `json:"contract"`
	Platform    *string            `json:"platform"`
	PremiumUnit *string            `json:"premium_unit"`
	Error       string             `json:"error"`
	Legs        []rhOptionMarksLeg `json:"legs"`
}

type rhOptionMarksLeg struct {
	ID                 string `json:"id"`
	Status             string `json:"status"`
	Reason             string `json:"reason"`
	CutoffUTC          string `json:"cutoff_utc"`
	SettlementReadyUTC string `json:"settlement_ready_utc"`
	SessionDate        string `json:"session_date"`
	NonSessionMapped   bool   `json:"non_session_mapped"`
	Mark               *struct {
		ValuePerUnit *float64 `json:"value_per_unit"`
		Spot         *float64 `json:"spot"`
		Greeks       *struct {
			Delta *float64 `json:"delta"`
			Gamma *float64 `json:"gamma"`
			Theta *float64 `json:"theta"`
			Vega  *float64 `json:"vega"`
		} `json:"greeks"`
		Source string `json:"source"`
	} `json:"mark"`
	Settlement *struct {
		Price  *float64 `json:"price"`
		Basis  string   `json:"basis"`
		Date   string   `json:"date"`
		Source string   `json:"source"`
	} `json:"settlement"`
}

func failAllOptionLegs(reqs []optionLegRequest, reason string) []optionLegReply {
	out := make([]optionLegReply, 0, len(reqs))
	for _, req := range reqs {
		out = append(out, optionLegReply{Key: req.Key, Err: reason})
	}
	return out
}

func (p *RobinhoodOptionPricer) Refresh(reqs []optionLegRequest, refreshAt time.Time) []optionLegReply {
	return runOptionMarksBatch("robinhood", p.script, p.run, reqs, refreshAt, false, robinhoodOptionMarkReply)
}

func runOptionMarksBatch(platform, script string, run optionMarksRunner, reqs []optionLegRequest, refreshAt time.Time, withSchedule bool, decodeLeg optionMarksLegDecoder) []optionLegReply {
	if len(reqs) == 0 {
		return nil
	}
	body := rhOptionMarksRequest{Contract: optionMarksContract, Platform: platform, RefreshAt: refreshAt.UTC().Format(time.RFC3339Nano)}
	for _, req := range reqs {
		leg := rhOptionMarksRequestLeg{ID: req.Key, Underlying: req.Underlying, OptionType: req.OptionType,
			Strike: req.Strike, Expiry: req.Expiry, Action: req.Action, DTE: req.DTE, Operation: req.Op}
		if withSchedule {
			if req.KnownCutoff.IsZero() || req.KnownReady.IsZero() {
				return failAllOptionLegs(reqs, fmt.Sprintf("%s option marks request for leg %s has no expiry schedule", platform, req.Key))
			}
			leg.CutoffUTC = req.KnownCutoff.UTC().Format(time.RFC3339Nano)
			leg.SettlementReadyUTC = req.KnownReady.UTC().Format(time.RFC3339Nano)
		}
		body.Legs = append(body.Legs, leg)
	}
	stdin, err := json.Marshal(body)
	if err != nil {
		return failAllOptionLegs(reqs, platform+" option marks request encoding failed: "+err.Error())
	}
	stdout, stderr, runErr := run(script, []string{"--platform=" + platform}, stdin)
	var timeout *pythonScriptTimeoutError
	var overflow *pythonOutputOverflowError
	switch {
	case runErr != nil && errors.As(runErr, &timeout):
		return failAllOptionLegs(reqs, platform+" option marks "+runErr.Error())
	case runErr != nil && errors.As(runErr, &overflow):
		return failAllOptionLegs(reqs, platform+" option marks "+runErr.Error())
	case runErr != nil && (errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded)):
		return failAllOptionLegs(reqs, platform+" option marks cancelled: "+runErr.Error())
	}
	if len(bytes.TrimSpace(stdout)) == 0 {
		detail := strings.TrimSpace(string(stderr))
		if len(detail) > 400 {
			detail = detail[len(detail)-400:]
		}
		if runErr != nil {
			return failAllOptionLegs(reqs, fmt.Sprintf("%s option marks script failed with no output: %v (stderr: %s)", platform, runErr, detail))
		}
		return failAllOptionLegs(reqs, platform+" option marks script wrote no output")
	}
	replies, err := decodeOptionMarks(platform, stdout, reqs, decodeLeg)
	if err != nil {
		return failAllOptionLegs(reqs, platform+" option marks reply rejected: "+err.Error())
	}
	return replies
}

func decodeRobinhoodOptionMarks(stdout []byte, reqs []optionLegRequest) ([]optionLegReply, error) {
	return decodeOptionMarks("robinhood", stdout, reqs, robinhoodOptionMarkReply)
}

func decodeOptionMarks(platform string, stdout []byte, reqs []optionLegRequest, decodeLeg optionMarksLegDecoder) ([]optionLegReply, error) {
	dec := json.NewDecoder(bytes.NewReader(stdout))
	var doc rhOptionMarksDoc
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("invalid JSON document: %w", err)
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("stdout holds more than one JSON document")
	}
	if doc.Contract == nil || *doc.Contract != optionMarksContract {
		return nil, fmt.Errorf("contract is not %s", optionMarksContract)
	}
	if doc.Platform == nil || *doc.Platform != platform {
		return nil, fmt.Errorf("platform is not %s", platform)
	}
	if doc.PremiumUnit == nil || *doc.PremiumUnit != optionPremiumUnitUnderlying {
		return nil, fmt.Errorf("premium_unit is not %s", optionPremiumUnitUnderlying)
	}
	if strings.TrimSpace(doc.Error) != "" {
		return nil, fmt.Errorf("script error: %s", doc.Error)
	}
	want := make(map[string]optionLegRequest, len(reqs))
	for _, req := range reqs {
		want[req.Key] = req
	}
	got := make(map[string]optionLegReply, len(doc.Legs))
	for _, leg := range doc.Legs {
		req, ok := want[leg.ID]
		if !ok {
			return nil, fmt.Errorf("unknown leg id %q", leg.ID)
		}
		if _, dup := got[leg.ID]; dup {
			return nil, fmt.Errorf("duplicate leg id %q", leg.ID)
		}
		reply, err := decodeLeg(req, leg)
		if err != nil {
			return nil, fmt.Errorf("leg %s: %w", leg.ID, err)
		}
		got[leg.ID] = reply
	}
	out := make([]optionLegReply, 0, len(reqs))
	for _, req := range reqs {
		r, ok := got[req.Key]
		if !ok {
			r = optionLegReply{Key: req.Key, Err: platform + " option marks reply omitted this leg"}
		}
		out = append(out, r)
	}
	return out, nil
}

func optionMarksReplyMark(leg rhOptionMarksLeg) *optionLegMark {
	m := leg.Mark
	if m == nil || m.ValuePerUnit == nil || m.Spot == nil || m.Greeks == nil || m.Greeks.Delta == nil || m.Greeks.Gamma == nil || m.Greeks.Theta == nil || m.Greeks.Vega == nil {
		return nil
	}
	return &optionLegMark{PerUnitUSD: *m.ValuePerUnit, Spot: *m.Spot, Source: m.Source,
		Greeks: OptGreeks{Delta: *m.Greeks.Delta, Gamma: *m.Greeks.Gamma, Theta: *m.Greeks.Theta, Vega: *m.Greeks.Vega}}
}

func robinhoodOptionMarkReply(req optionLegRequest, leg rhOptionMarksLeg) (optionLegReply, error) {
	reply := optionLegReply{Key: req.Key}
	if leg.CutoffUTC != "" {
		cutoff, err := time.Parse(time.RFC3339Nano, leg.CutoffUTC)
		if err != nil {
			return reply, fmt.Errorf("invalid cutoff_utc %q", leg.CutoffUTC)
		}
		ready, err := time.Parse(time.RFC3339Nano, leg.SettlementReadyUTC)
		if err != nil || ready.Before(cutoff) {
			return reply, fmt.Errorf("invalid settlement_ready_utc %q", leg.SettlementReadyUTC)
		}
		reply.CutoffAt, reply.ReadyAt = cutoff.UTC(), ready.UTC()
	}
	switch leg.Status {
	case "mark":
		mark := optionMarksReplyMark(leg)
		if mark == nil {
			reply.Err = "robinhood mark reply is missing its value, spot or greeks"
			return reply, nil
		}
		reply.Mark = mark
	case "settled":
		st := leg.Settlement
		if st == nil || st.Price == nil {
			reply.Err = "robinhood settlement reply has no price"
			return reply, nil
		}
		if st.Basis != optionSettlementBasisRawClose || st.Date == "" || st.Date != leg.SessionDate {
			reply.Err = fmt.Sprintf("robinhood settlement basis %q on %q does not match session %q", st.Basis, st.Date, leg.SessionDate)
			return reply, nil
		}
		if leg.SessionDate != req.Expiry && !leg.NonSessionMapped {
			reply.Err = fmt.Sprintf("robinhood settlement session %s differs from expiry %s without a confirmed non-session mapping", leg.SessionDate, req.Expiry)
			return reply, nil
		}
		reply.Settlement = &optionLegSettlement{Price: *st.Price, Basis: st.Basis, Date: st.Date, Source: st.Source}
	case "pending":
		reply.Pending = firstNonEmpty(leg.Reason, "robinhood settlement pending")
	case "error":
		reply.Err = firstNonEmpty(leg.Reason, "robinhood option read failed")
	default:
		return reply, fmt.Errorf("unknown status %q", leg.Status)
	}
	return reply, nil
}
