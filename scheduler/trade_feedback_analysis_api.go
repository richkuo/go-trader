package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	tradeFeedbackAPIKeyEnv           = "ANTHROPIC_API_KEY"
	tradeFeedbackAPIDefaultEffort    = "high"
	tradeFeedbackAPIRecommendedModel = "claude-opus-5-5"
	tradeFeedbackAPIStdoutLimit      = 4 << 20
	tradeFeedbackAPIStderrLimit      = 64 << 10
	tradeFeedbackAPIValidatorReserve = 30 * time.Second
	tradeFeedbackAPIModelLabelLimit  = 128
)

var (
	tradeFeedbackAPIEfforts = []string{"low", "medium", "high", "xhigh", "max"}
	tradeFeedbackAPIModelRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

	tradeFeedbackAPIURLOverride string
)

type tradeFeedbackAPISettings struct {
	Model  string `json:"api_model"`
	Effort string `json:"api_effort"`
}

func (s tradeFeedbackAPISettings) String() string {
	model := s.Model
	if model == "" {
		model = "(off)"
	}
	return fmt.Sprintf("{api_model:%s api_effort:%s}", model, s.Effort)
}

func tradeFeedbackAPIEffortSupported(effort string) bool {
	for _, e := range tradeFeedbackAPIEfforts {
		if e == effort {
			return true
		}
	}
	return false
}

func (c *Config) tradeFeedbackAPISettings() tradeFeedbackAPISettings {
	s := tradeFeedbackAPISettings{Effort: tradeFeedbackAPIDefaultEffort}
	if c == nil || c.TradeFeedbackAnalysis == nil {
		return s
	}
	if c.TradeFeedbackAnalysis.APIModel != nil {
		s.Model = strings.TrimSpace(*c.TradeFeedbackAnalysis.APIModel)
	}
	if c.TradeFeedbackAnalysis.APIEffort != nil {
		s.Effort = strings.TrimSpace(*c.TradeFeedbackAnalysis.APIEffort)
	}
	return s
}

func validateTradeFeedbackAPIField(key string, raw json.RawMessage) string {
	var v string
	if err := json.Unmarshal(raw, &v); err != nil || strings.TrimSpace(string(raw)) == "null" {
		return fmt.Sprintf("%s.%s: must be a string, got %s", tradeFeedbackAnalysisConfigKey, key, strings.TrimSpace(string(raw)))
	}
	switch key {
	case "api_model":
		if !tradeFeedbackAPIModelRE.MatchString(v) {
			return fmt.Sprintf("%s.api_model: must be a model id of 1 to %d letters, digits, '.', '_', ':' or '-' (for example %s), got %q",
				tradeFeedbackAnalysisConfigKey, tradeFeedbackAPIModelLabelLimit, tradeFeedbackAPIRecommendedModel, v)
		}
	case "api_effort":
		if !tradeFeedbackAPIEffortSupported(v) {
			return fmt.Sprintf("%s.api_effort: must be one of %s, got %q", tradeFeedbackAnalysisConfigKey, strings.Join(tradeFeedbackAPIEfforts, ", "), v)
		}
	}
	return ""
}

func (ss *StatusServer) currentFeedbackAnalysisConfig() (tradeFeedbackAnalysisSettings, tradeFeedbackAPISettings) {
	ss.strategiesMu.RLock()
	defer ss.strategiesMu.RUnlock()
	api := ss.feedbackAnalysisAPI
	if api.Effort == "" {
		api.Effort = tradeFeedbackAPIDefaultEffort
	}
	return ss.feedbackAnalysisSettings.resolved(), api
}

func tradeFeedbackAPIUnavailableReason(s tradeFeedbackAPISettings) string {
	switch {
	case s.Model == "":
		return fmt.Sprintf("set %s.api_model (for example %q) to turn on the Anthropic API provider", tradeFeedbackAnalysisConfigKey, tradeFeedbackAPIRecommendedModel)
	case !tradeFeedbackAPIModelRE.MatchString(s.Model):
		return fmt.Sprintf("%s.api_model is not a supported model id", tradeFeedbackAnalysisConfigKey)
	case !tradeFeedbackAPIEffortSupported(s.Effort):
		return fmt.Sprintf("%s.api_effort must be one of %s", tradeFeedbackAnalysisConfigKey, strings.Join(tradeFeedbackAPIEfforts, ", "))
	case strings.TrimSpace(os.Getenv(tradeFeedbackAPIKeyEnv)) == "":
		return tradeFeedbackAPIKeyEnv + " is not set in the scheduler environment"
	}
	return ""
}

// tradeFeedbackAPIAdapter is the Anthropic API provider branch. The model and
// effort are captured with the bundle inputs before admission; the API key is
// read from the daemon environment by the provider child only and never enters
// the bundle, the arguments, the stored events or a log line.
type tradeFeedbackAPIAdapter struct {
	ss *StatusServer
}

func (a *tradeFeedbackAPIAdapter) Available() (bool, string) {
	_, s := a.ss.currentFeedbackAnalysisConfig()
	if reason := tradeFeedbackAPIUnavailableReason(s); reason != "" {
		return false, reason
	}
	return true, fmt.Sprintf("model %s, effort %s", s.Model, s.Effort)
}

func (a *tradeFeedbackAPIAdapter) Prepare(req tradeFeedbackAnalysisSubmit, build *tradeFeedbackBundleBuild) (tradeFeedbackAdmission, *tradeReviewError) {
	s := build.inputs.api
	if reason := tradeFeedbackAPIUnavailableReason(s); reason != "" {
		return tradeFeedbackAdmission{}, reviewErr(http.StatusConflict, "provider %s is not configured: %s; nothing was started", tradeFeedbackProviderAPI, reason)
	}
	return tradeFeedbackAdmission{Model: s.Model, Effort: s.Effort}, nil
}

func tradeFeedbackAPIProviderBudget(deadline time.Time) time.Duration {
	left := time.Until(deadline)
	reserve := tradeFeedbackAPIValidatorReserve
	if left/4 < reserve {
		reserve = left / 4
	}
	return left - reserve
}

func (a *tradeFeedbackAPIAdapter) Run(ctx context.Context, job *tradeFeedbackAnalysisJob) (tradeFeedbackProviderOutput, error) {
	budget := tradeFeedbackAPIProviderBudget(job.Deadline)
	if budget <= 0 {
		return tradeFeedbackProviderOutput{}, fmt.Errorf("no time left before the analysis deadline; the API was not called")
	}
	args := []string{"--model", job.Admission.Model, "--effort", job.Admission.Effort,
		"--timeout-s", strconv.FormatFloat(budget.Seconds(), 'f', 3, 64)}
	if tradeFeedbackAPIURLOverride != "" {
		args = append(args, "--api-url", tradeFeedbackAPIURLOverride)
	}
	stdout, stderr, runErr := spawnPythonProcessWithEnvLimited(ctx, tradeFeedbackValidatorScript, args, job.Bundle, budget, nil,
		pythonOutputLimits{Stdout: tradeFeedbackAPIStdoutLimit, Stderr: tradeFeedbackAPIStderrLimit})
	var resp struct {
		Error         string          `json:"error"`
		ResultText    *string         `json:"result_text"`
		ModelReported string          `json:"model_reported"`
		Usage         json.RawMessage `json:"usage"`
	}
	parseErr := json.Unmarshal(stdout, &resp)
	var overflow *pythonOutputOverflowError
	switch {
	case errors.As(runErr, &overflow):
		return tradeFeedbackProviderOutput{}, runErr
	case parseErr == nil && resp.Error != "":
		return tradeFeedbackProviderOutput{}, errors.New(resp.Error)
	case runErr != nil:
		return tradeFeedbackProviderOutput{}, fmt.Errorf("%v (stderr: %s)", runErr, firstLine(stderr))
	case parseErr != nil:
		return tradeFeedbackProviderOutput{}, fmt.Errorf("unreadable provider output: %v", parseErr)
	case resp.ResultText == nil:
		return tradeFeedbackProviderOutput{}, fmt.Errorf("the provider output has no result_text")
	}
	verification := "the service reported no model id"
	if reported := strings.TrimSpace(resp.ModelReported); reported != "" {
		verification = "service reported " + truncateUTF8(tradeFeedbackRedact(reported, a.ss.statusToken), tradeFeedbackAPIModelLabelLimit)
	}
	out := tradeFeedbackProviderOutput{Result: json.RawMessage(*resp.ResultText), Model: job.Admission.Model, ModelVerification: verification}
	if len(resp.Usage) > 0 && json.Valid(resp.Usage) && strings.TrimSpace(string(resp.Usage)) != "null" {
		out.Usage = resp.Usage
	}
	return out, nil
}
