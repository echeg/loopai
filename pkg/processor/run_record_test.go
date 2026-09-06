package processor

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunRecordJSONRoundTrip(t *testing.T) {
	original := RunRecord{
		Version:        runRecordVersion,
		Plan:           "docs/plans/example.md",
		Branch:         "completion-report",
		BaseRef:        "master",
		Mode:           ModeFull,
		Executor:       "codex",
		TaskModel:      "gpt-5.6-sol:medium",
		ReviewModel:    "gpt-5.6-sol:high",
		StartedAt:      time.Date(2026, 9, 6, 1, 41, 47, 0, time.UTC),
		FinishedAt:     time.Date(2026, 9, 6, 2, 4, 5, 0, time.UTC),
		PhaseDurations: map[string]Duration{"task": Duration(3 * time.Second)},
		Validation:     &ValidationRunRecord{Duration: Duration(1500 * time.Millisecond), Runs: 2},
		Tasks:          TaskRunRecord{Iterations: 6, FailedRetries: 1},
		InternalReview: InternalReviewRunRecord{FirstRan: true, LoopIterations: 2, EndedBy: "review_done"},
		External: []ExternalReviewerRecord{
			{
				Key: "claude:opus:high", Label: "claude (opus:high)", Duration: Duration(1830 * time.Second),
				EndedBy: "done", HadFindings: true,
				Iterations: []ExternalIterationRecord{
					{Index: 1, ReviewerOutput: "finding", EvaluatorResponse: "fix it", Truncated: false},
				},
			},
		},
		PostReview: PostReviewRunRecord{Ran: true, Iterations: 1},
		Report:     "# Report: example",
	}

	data, err := json.Marshal(original)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"duration_ms":1830000`)
	assert.Contains(t, string(data), `"task":3000`)
	assert.Contains(t, string(data), `"validation":{"duration_ms":1500,"runs":2}`)

	var decoded RunRecord
	require.NoError(t, json.Unmarshal(data, &decoded))
	assert.Equal(t, original, decoded)
}

func TestTruncateForRecord(t *testing.T) {
	tests := []struct {
		name          string
		input         string
		want          string
		wantTruncated bool
	}{
		{name: "below cap", input: strings.Repeat("a", runRecordTextCap-1), want: strings.Repeat("a", runRecordTextCap-1)},
		{name: "at cap", input: strings.Repeat("a", runRecordTextCap), want: strings.Repeat("a", runRecordTextCap)},
		{
			name: "above cap", input: strings.Repeat("a", runRecordTextCap+1),
			want: strings.Repeat("a", runRecordTextCap) + truncatedRecordMarker, wantTruncated: true,
		},
		{
			name: "multibyte boundary", input: strings.Repeat("a", runRecordTextCap-1) + "€suffix",
			want: strings.Repeat("a", runRecordTextCap-1) + truncatedRecordMarker, wantTruncated: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, truncated := truncateForRecord(tc.input)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.wantTruncated, truncated)
			assert.True(t, utf8.ValidString(got))
		})
	}
}

func TestRunRecordZeroValueMarshals(t *testing.T) {
	data, err := json.Marshal(RunRecord{})
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"version": 0,
		"plan": "",
		"branch": "",
		"base_ref": "",
		"mode": "",
		"executor": "",
		"task_model": "",
		"review_model": "",
		"started_at": "0001-01-01T00:00:00Z",
		"finished_at": "0001-01-01T00:00:00Z",
		"phase_durations": null,
		"tasks": {"iterations": 0, "failed_retries": 0},
		"internal_review": {"first_ran": false, "loop_iterations": 0, "ended_by": ""},
		"external": null,
		"post_review": {"ran": false, "iterations": 0},
		"report": ""
	}`, string(data))
}

func TestDurationRejectsInvalidJSON(t *testing.T) {
	var duration Duration
	err := json.Unmarshal([]byte(`"not milliseconds"`), &duration)
	require.ErrorContains(t, err, "decode duration milliseconds")
}
