package main

import (
	"fmt"
	"strings"
	"testing"
)

func argsContains(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func argsHasPrefix(args []string, prefix string) bool {
	for _, a := range args {
		if len(a) >= len(prefix) && a[:len(prefix)] == prefix {
			return true
		}
	}
	return false
}

func TestBuildHyperliquidExecuteArgs_CloseMode(t *testing.T) {
	probeFlag := ""
	for _, a := range executeProbeArgv {
		if strings.HasPrefix(a, "--close-mode=") {
			probeFlag = a
		}
	}
	cases := []struct {
		mode      hlCloseMode
		wantSize  bool
		wantFull  bool
		wantClose string
	}{
		{hlCloseModeNone, true, false, ""},
		{hlCloseModeReduceOnly, true, false, "--close-mode=reduce_only"},
		{hlCloseModeCross, true, false, "--close-mode=cross"},
		{hlCloseModeWhole, false, true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.mode.String(), func(t *testing.T) {
			args := buildHyperliquidExecuteArgs("ETH", "sell", 0.42, 0, 0, 0, "", 0, tc.mode, hlExecuteSnapshot{})
			if argsContains(args, "--size=0.42") != tc.wantSize || argsContains(args, "--close-full-position") != tc.wantFull {
				t.Fatalf("argv %v: size=%t full=%t, want size=%t full=%t", args, argsContains(args, "--size=0.42"), argsContains(args, "--close-full-position"), tc.wantSize, tc.wantFull)
			}
			if tc.wantClose == "" {
				if argsHasPrefix(args, "--close-mode=") {
					t.Fatalf("argv %v must carry no --close-mode", args)
				}
				return
			}
			if !argsContains(args, tc.wantClose) {
				t.Fatalf("argv %v missing %s", args, tc.wantClose)
			}
		})
	}
	if probeFlag == "" || !argsContains(buildHyperliquidExecuteArgs("ETH", "sell", 0.42, 0, 0, 0, "", 0, hlCloseModeReduceOnly, hlExecuteSnapshot{}), probeFlag) {
		t.Fatalf("executeProbeArgv %v must carry the --close-mode flag the builder emits", executeProbeArgv)
	}
}

func TestHLExecuteFillOutcome(t *testing.T) {
	filled := func(sz float64) *HyperliquidExecuteResult {
		return &HyperliquidExecuteResult{OrderOutcome: "filled", Execution: &HyperliquidExecution{Fill: &HyperliquidFill{AvgPx: 3000, TotalSz: sz}}}
	}
	cases := []struct {
		name string
		res  *HyperliquidExecuteResult
		err  error
		want hlCloseFillOutcome
	}{
		{"filled books the fill", filled(4), nil, hlCloseFillOutcome{Filled: 4, Known: true}},
		{"fill above the book books the book", filled(12), nil, hlCloseFillOutcome{Filled: 10, Known: true}},
		{"filled without a confirmed fill is unknown", &HyperliquidExecuteResult{OrderOutcome: "filled"}, nil, hlCloseFillOutcome{}},
		{"rejected is a known zero fill", &HyperliquidExecuteResult{OrderOutcome: "rejected", Error: "exchange rejected order"}, fmt.Errorf("exit 1"), hlCloseFillOutcome{Known: true}},
		{"not sent is a known zero fill", &HyperliquidExecuteResult{OrderOutcome: "not_sent", Error: "no usable mid price"}, fmt.Errorf("exit 1"), hlCloseFillOutcome{Known: true}},
		{"catch-all is unknown", &HyperliquidExecuteResult{OrderOutcome: "unknown", Error: "socket closed"}, fmt.Errorf("exit 1"), hlCloseFillOutcome{}},
		{"missing field is unknown", &HyperliquidExecuteResult{Error: "exchange rejected order"}, fmt.Errorf("exit 1"), hlCloseFillOutcome{}},
		{"no result is unknown", nil, fmt.Errorf("parse execute output"), hlCloseFillOutcome{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hlExecuteFillOutcome(tc.res, tc.err, 10); got != tc.want {
				t.Fatalf("hlExecuteFillOutcome = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestClassifyProtectionSyncStopRearm(t *testing.T) {
	cases := []struct {
		name       string
		protection *HyperliquidProtectionSyncResult
		want       hlStopRearmStatus
	}{
		{"force-replace cancel rejected keeps the pre-close stop resting", &HyperliquidProtectionSyncResult{StopLossError: "force replace cancel rejected: busy", CancelStopLossError: "force replace cancel rejected: busy"}, hlStopRearmPreCloseStopResting},
		{"cancelled and replacement rejected loses protection", &HyperliquidProtectionSyncResult{CancelStopLossSucceeded: true, StopLossError: "open order limit"}, hlStopRearmProtectionLost},
		{"fresh placement rejected", &HyperliquidProtectionSyncResult{StopLossError: "open order limit"}, hlStopRearmPlacementFailed},
		{"replacement rests", &HyperliquidProtectionSyncResult{StopLossOID: 9002, StopLossTriggerPx: 2325}, hlStopRearmPlaced},
		{"sync failed", nil, hlStopRearmOutcomeUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyProtectionSyncStopRearm(1, tc.protection).Status; got != tc.want {
				t.Fatalf("status = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestClassifyStopRearmRemoval(t *testing.T) {
	cases := []struct {
		name   string
		result *HyperliquidStopLossUpdateResult
		want   hlStopRearmStatus
	}{
		{"cancelled", &HyperliquidStopLossUpdateResult{CancelOnly: true, CancelStopLossSucceeded: true}, hlStopRearmRemoved},
		{"not open and not filled", &HyperliquidStopLossUpdateResult{CancelOnly: true, StopLossNotOpen: true}, hlStopRearmRemoved},
		{"filled externally", &HyperliquidStopLossUpdateResult{CancelOnly: true, StopLossFilledExternally: true}, hlStopRearmClosed},
		{"cancel rejected", &HyperliquidStopLossUpdateResult{CancelOnly: true, Error: "cancel failed", CancelStopLossError: "busy"}, hlStopRearmPreCloseStopResting},
		{"open orders unreadable", &HyperliquidStopLossUpdateResult{CancelOnly: true, Error: "open orders unreadable", OpenOrderCheckError: "indexer down"}, hlStopRearmReadFailed},
		{"nil result", nil, hlStopRearmOutcomeUnknown},
		{"bare error", &HyperliquidStopLossUpdateResult{Error: "invalid side"}, hlStopRearmOutcomeUnknown},
		{"a cancel outcome from a run that was not cancel-only", &HyperliquidStopLossUpdateResult{CancelStopLossSucceeded: true, StopLossOID: 9002}, hlStopRearmOutcomeUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyStopRearmRemoval(tc.result).Status; got != tc.want {
				t.Fatalf("status = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestClassifyProtectionSyncTPRearm(t *testing.T) {
	tiers := []hlProtectionTier{{Multiple: 1, Fraction: 0.5}, {Multiple: 2, Fraction: 1}}
	full := hlProtectionPlan{Tiers: tiers, TPOIDs: []int64{7001, 7002}, ForceTPReplace: []bool{true, false}}
	cancel := hlProtectionPlan{CancelTPOIDs: []int64{7001}}
	cases := []struct {
		name       string
		plan       hlProtectionPlan
		result     *HyperliquidProtectionSyncResult
		want       hlTPRearmStatus
		wantDetail string
		wantReport []string
		notReport  string
	}{
		{"no take-profit leg", hlProtectionPlan{}, nil, hlTPRearmNone, "", nil, ""},
		{"force-replaced tier placed", full, &HyperliquidProtectionSyncResult{TPOIDs: []int64{9101, 7002}}, hlTPRearmPlaced, "", nil, ""},
		{"no force and ids unchanged", hlProtectionPlan{Tiers: tiers, TPOIDs: []int64{7001, 7002}}, &HyperliquidProtectionSyncResult{TPOIDs: []int64{7001, 7002}}, hlTPRearmKept, "", nil, ""},
		{"removed", cancel, &HyperliquidProtectionSyncResult{}, hlTPRearmRemoved, "7001", nil, ""},
		{"tier placement error", full, &HyperliquidProtectionSyncResult{TPOIDs: []int64{0, 7002}, TPErrors: []string{"open order limit", ""}}, hlTPRearmFailed, "tier 1: open order limit", nil, ""},
		{"force-replaced tier left at its old id", full, &HyperliquidProtectionSyncResult{TPOIDs: []int64{7001, 7002}}, hlTPRearmFailed, "OID=7001) still rests", nil, ""},
		{"cancel failed", cancel, &HyperliquidProtectionSyncResult{TPCancelFailedOIDs: []int64{7001}}, hlTPRearmFailed, "[7001]", nil, ""},
		{"outcome unknown", full, &HyperliquidProtectionSyncResult{TPOIDs: []int64{0, 7002}, TPOutcomeUnknown: []bool{true, false}}, hlTPRearmUnknown, "[1]", nil, ""},
		{"sync error with tiers", full, &HyperliquidProtectionSyncResult{Error: "avg-cost and entry-atr must be > 0"}, hlTPRearmFailed, "avg-cost", nil, ""},
		{"nil result", full, nil, hlTPRearmUnknown, "no result", nil, ""},
		{"placement error and unresolved outcome on one tier", full, &HyperliquidProtectionSyncResult{TPOIDs: []int64{0, 7002}, TPErrors: []string{"read timeout", ""}, TPOutcomeUnknown: []bool{true, false}}, hlTPRearmUnknown, "[1]", []string{"Take-profit leg UNKNOWN", "before you place anything"}, "by hand"},
		{"tier 1 unresolved and tier 2 rejected in one sync", full, &HyperliquidProtectionSyncResult{TPOIDs: []int64{0, 0}, TPErrors: []string{"read timeout", "open order limit"}, TPOutcomeUnknown: []bool{true, false}}, hlTPRearmFailed, "tier 2: open order limit", []string{"FAILED (tier 2: open order limit) and UNKNOWN (the placement of tier(s) [1]", "before you place anything"}, "tier 1: read timeout"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyProtectionSyncTPRearm(tc.plan, tc.result)
			if got.Status != tc.want {
				t.Fatalf("status = %d, want %d (%s)", got.Status, tc.want, got.Detail)
			}
			if !strings.Contains(got.Detail, tc.wantDetail) {
				t.Fatalf("detail = %q, want it to contain %q", got.Detail, tc.wantDetail)
			}
			report := formatCloseTPLegReport(got)
			for _, want := range tc.wantReport {
				if !strings.Contains(report, want) {
					t.Fatalf("report = %q, want it to contain %q", report, want)
				}
			}
			if tc.notReport != "" && strings.Contains(report, tc.notReport) {
				t.Fatalf("report = %q, want no %q", report, tc.notReport)
			}
		})
	}
}
