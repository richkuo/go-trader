package main

import (
	"fmt"
	"reflect"
	"testing"
)

func TestLLMEntryAnalysisReportedUsage(t *testing.T) {
	cases := []struct {
		name   string
		stdout string
		stderr string
		want   *LLMEntryAnalysisUsage
	}{
		{
			name:   "stdout totals win over stderr",
			stdout: `{"verdict":"bullish","rationale":"r","usage":{"calls":4,"input_tokens":1000,"output_tokens":100}}`,
			stderr: "llm_review_usage {\"calls\":3,\"input_tokens\":600,\"output_tokens\":60}\n",
			want:   &LLMEntryAnalysisUsage{Calls: 4, InputTokens: 1000, OutputTokens: 100},
		},
		{
			name:   "killed at timeout takes last valid stderr line",
			stdout: "",
			stderr: "llm_review_usage {\"calls\":1,\"input_tokens\":100,\"output_tokens\":10}\n" +
				"some warning\n" +
				"llm_review_usage {\"calls\":2,\"input_tokens\":300,\"output_tokens\":30}\n" +
				"  llm_review_usage {\"calls\":3,\"input_tokens\":600,\"output_tokens\":60}\n" +
				"llm_review_usage {\"calls\":4,\"input_tokens\n",
			want: &LLMEntryAnalysisUsage{Calls: 3, InputTokens: 600, OutputTokens: 60},
		},
		{
			name:   "null stdout usage falls back to stderr",
			stdout: `{"error":"boom","usage":null}`,
			stderr: "llm_review_usage {\"calls\":2,\"input_tokens\":300,\"output_tokens\":30}\n",
			want:   &LLMEntryAnalysisUsage{Calls: 2, InputTokens: 300, OutputTokens: 30},
		},
		{
			name:   "malformed stdout usage falls back to stderr",
			stdout: `{"error":"boom","usage":"x"}`,
			stderr: "llm_review_usage {\"calls\":1,\"input_tokens\":100,\"output_tokens\":10}\n",
			want:   &LLMEntryAnalysisUsage{Calls: 1, InputTokens: 100, OutputTokens: 10},
		},
		{
			name:   "negative stdout usage falls back to stderr",
			stdout: `{"error":"boom","usage":{"calls":-1,"input_tokens":0,"output_tokens":0}}`,
			stderr: "llm_review_usage {\"calls\":1,\"input_tokens\":100,\"output_tokens\":10}\n",
			want:   &LLMEntryAnalysisUsage{Calls: 1, InputTokens: 100, OutputTokens: 10},
		},
		{
			name:   "unparseable stdout falls back to stderr",
			stdout: "Traceback (most recent call last):",
			stderr: "llm_review_usage {\"calls\":1,\"input_tokens\":100,\"output_tokens\":10}\n",
			want:   &LLMEntryAnalysisUsage{Calls: 1, InputTokens: 100, OutputTokens: 10},
		},
		{
			name:   "both sources missing",
			stdout: `{"error":"boom"}`,
			stderr: "RuntimeError: boom\nllm_review_usage [1]\n",
			want:   nil,
		},
		{
			name:   "empty stdout and stderr",
			stdout: "",
			stderr: "",
			want:   nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := llmEntryAnalysisReportedUsage([]byte(tc.stdout), []byte(tc.stderr))
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("usage = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestParseLLMEntryAnalysisOutputUsage(t *testing.T) {
	for _, usage := range []string{`"x"`, `null`, `[1]`, `1.5`, `{"calls":-1,"input_tokens":0,"output_tokens":0}`, `{"calls":1.5}`} {
		t.Run("malformed "+usage, func(t *testing.T) {
			res, err := parseLLMEntryAnalysisOutput([]byte(`{"verdict":"Bullish","rationale":"r","usage":` + usage + `}`))
			if err != nil {
				t.Fatalf("err = %v, want success", err)
			}
			if res.Verdict != "bullish" {
				t.Fatalf("verdict = %q, want bullish", res.Verdict)
			}
			if res.Usage != nil {
				t.Fatalf("usage = %+v, want nil", res.Usage)
			}
		})
	}

	res, err := parseLLMEntryAnalysisOutput([]byte(`{"verdict":"mixed","rationale":"r","usage":{"calls":4,"input_tokens":1000,"output_tokens":100}}`))
	if err != nil {
		t.Fatalf("valid usage: err = %v", err)
	}
	want := &LLMEntryAnalysisUsage{Calls: 4, InputTokens: 1000, OutputTokens: 100}
	if !reflect.DeepEqual(res.Usage, want) {
		t.Fatalf("valid usage = %+v, want %+v", res.Usage, want)
	}

	if res, err := parseLLMEntryAnalysisOutput([]byte(`{"error":"refused","usage":{"calls":3,"input_tokens":600,"output_tokens":60}}`)); err == nil {
		t.Fatalf("error JSON with usage parsed as success: %+v", res)
	}
}

func TestLLMEntryAnalysisFailureCarriesUsage(t *testing.T) {
	usage := &LLMEntryAnalysisUsage{Calls: 3, InputTokens: 600, OutputTokens: 60}
	err := fmt.Errorf("worker: %w", &llmEntryAnalysisFailure{err: fmt.Errorf("boom"), usage: usage})
	if got := llmEntryAnalysisFailureUsage(err); got != usage {
		t.Fatalf("failure usage = %+v, want %+v", got, usage)
	}
	if got := llmEntryAnalysisFailureUsage(fmt.Errorf("plain")); got != nil {
		t.Fatalf("plain error usage = %+v, want nil", got)
	}
	if got := formatLLMEntryAnalysisUsage(nil); got != "usage unavailable" {
		t.Fatalf("nil usage format = %q", got)
	}
	if got := formatLLMEntryAnalysisUsage(usage); got != "usage calls=3 input_tokens=600 output_tokens=60" {
		t.Fatalf("usage format = %q", got)
	}
}
