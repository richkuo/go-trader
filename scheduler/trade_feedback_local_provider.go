package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

const (
	tradeFeedbackLocalModelVerification = "self_reported_unverified"
	tradeFeedbackLocalModelMaxLen       = 128
)

var (
	tradeFeedbackLocalModelRE  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._:/@+()\[\]-]*$`)
	tradeFeedbackLocalDigestRE = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

type tradeFeedbackLocalPayload struct {
	Model  string
	Result json.RawMessage
}

type tradeFeedbackLocalAdapter struct{}

func (tradeFeedbackLocalAdapter) Available() (bool, string) { return true, "" }

func (tradeFeedbackLocalAdapter) Prepare(req tradeFeedbackAnalysisSubmit, build *tradeFeedbackBundleBuild) (tradeFeedbackAdmission, *tradeReviewError) {
	if req.Model == nil || req.BundleSHA256 == nil || req.FeedbackRowIDs == nil || !tradeFeedbackRawPresent(req.Result) {
		return tradeFeedbackAdmission{}, reviewErr(http.StatusBadRequest, "provider claude_code needs model, bundle_sha256, feedback_rowids and result")
	}
	model := strings.TrimSpace(*req.Model)
	if model == "" || len(model) > tradeFeedbackLocalModelMaxLen || !tradeFeedbackLocalModelRE.MatchString(model) {
		return tradeFeedbackAdmission{}, reviewErr(http.StatusBadRequest,
			"model must be a label of 1 to %d letters, digits, spaces and . _ : / @ + ( ) [ ] -", tradeFeedbackLocalModelMaxLen)
	}
	digest := strings.TrimSpace(*req.BundleSHA256)
	if !tradeFeedbackLocalDigestRE.MatchString(digest) {
		return tradeFeedbackAdmission{}, reviewErr(http.StatusBadRequest, "bundle_sha256 must be 64 lowercase hex characters")
	}
	if reason := tradeFeedbackSelectionMismatch(build, *req.FeedbackRowIDs, digest); reason != "" {
		return tradeFeedbackAdmission{}, reviewErr(http.StatusConflict, "%s; pull the bundle again and analyze the current marks", reason)
	}
	result := append(json.RawMessage(nil), req.Result...)
	return tradeFeedbackAdmission{Model: model, Payload: tradeFeedbackLocalPayload{Model: model, Result: result}}, nil
}

func (tradeFeedbackLocalAdapter) Run(ctx context.Context, job *tradeFeedbackAnalysisJob) (tradeFeedbackProviderOutput, error) {
	p, ok := job.Admission.Payload.(tradeFeedbackLocalPayload)
	if !ok {
		return tradeFeedbackProviderOutput{}, fmt.Errorf("the admitted local result is missing")
	}
	return tradeFeedbackProviderOutput{Result: p.Result, Model: p.Model, ModelVerification: tradeFeedbackLocalModelVerification}, nil
}

func tradeFeedbackRawPresent(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null"))
}

func (req tradeFeedbackAnalysisSubmit) hasLocalFields() bool {
	return req.Model != nil || req.BundleSHA256 != nil || req.FeedbackRowIDs != nil || tradeFeedbackRawPresent(req.Result)
}

func (ss *StatusServer) serveTradeFeedbackBundle(w http.ResponseWriter, r *http.Request, id string) {
	filter, ok := ss.uiPartitionParam(w, r)
	if !ok {
		return
	}
	if _, found := ss.strategyConfig(id); !found || !ss.strategyInPartition(id, filter) {
		writeJSONError(w, http.StatusNotFound, "strategy not found")
		return
	}
	adapter := ss.feedbackAnalysisAdapters[tradeFeedbackProviderLocal]
	if adapter == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "the local Claude Code provider is not installed in this build, so the bundle pull endpoint is unavailable")
		return
	}
	if ok, reason := adapter.Available(); !ok {
		writeJSONError(w, http.StatusServiceUnavailable, "the local Claude Code provider is unavailable: "+reason)
		return
	}
	if ss.feedbackAnalysis == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "trade feedback analysis is unavailable: "+ss.feedbackAnalysisUnavailableReason())
		return
	}
	build, rerr := ss.buildTradeFeedbackBundle(r.Context(), id)
	if rerr != nil {
		writeJSONError(w, rerr.Status, rerr.Msg)
		return
	}
	if len(build.bundle.Selection.FeedbackRowIDs) == 0 {
		writeJSONError(w, http.StatusConflict, fmt.Sprintf("strategy %q has no usable saved marks to analyze (%d excluded)", id, len(build.bundle.Selection.Exclusions)))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(build.bundleJSON)
}
