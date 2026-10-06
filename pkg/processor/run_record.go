package processor

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"
	"unicode/utf8"
)

const (
	runRecordVersion = 1
	runRecordTextCap = 16 * 1024
	// Bound combined reviewer/evaluator text, including truncation markers, across the whole chain.
	runRecordExternalTextCap = 64 * 1024
)

const truncatedRecordMarker = "\n[truncated]"

// Duration is a time duration persisted as milliseconds in run records.
type Duration time.Duration

// MarshalJSON encodes a duration as milliseconds.
func (d Duration) MarshalJSON() ([]byte, error) {
	return []byte(strconv.FormatInt(time.Duration(d).Milliseconds(), 10)), nil
}

// UnmarshalJSON decodes a duration expressed in milliseconds.
func (d *Duration) UnmarshalJSON(data []byte) error {
	var milliseconds int64
	if err := json.Unmarshal(data, &milliseconds); err != nil {
		return fmt.Errorf("decode duration milliseconds: %w", err)
	}
	*d = Duration(time.Duration(milliseconds) * time.Millisecond)
	return nil
}

// RunRecord contains the durable facts and generated report for one run.
type RunRecord struct {
	Version        int                      `json:"version"`
	Plan           string                   `json:"plan"`
	Branch         string                   `json:"branch"`
	BaseRef        string                   `json:"base_ref"`
	Mode           Mode                     `json:"mode"`
	Executor       string                   `json:"executor"`
	TaskModel      string                   `json:"task_model"`
	ReviewModel    string                   `json:"review_model"`
	StartedAt      time.Time                `json:"started_at"`
	FinishedAt     time.Time                `json:"finished_at"`
	PhaseDurations map[string]Duration      `json:"phase_durations"`
	Validation     *ValidationRunRecord     `json:"validation,omitempty"`
	Tasks          TaskRunRecord            `json:"tasks"`
	InternalReview InternalReviewRunRecord  `json:"internal_review"`
	External       []ExternalReviewerRecord `json:"external"`
	PostReview     PostReviewRunRecord      `json:"post_review"`
	Finalize       *FinalizeOutcome         `json:"finalize,omitempty"`
	Report         string                   `json:"report"`
	// PendingReviewFixes is set when a review_cadence = task block left its fixes uncommitted, so
	// a resumed run still runs the final post-review loop whose commit prefix commits them.
	PendingReviewFixes bool `json:"pending_review_fixes,omitempty"`
}

// ValidationRunRecord summarizes measured validation commands across invocations.
type ValidationRunRecord struct {
	Duration Duration `json:"duration_ms"`
	Runs     int      `json:"runs"`
}

// TaskRunRecord summarizes task-executor iterations.
type TaskRunRecord struct {
	Iterations    int `json:"iterations"`
	FailedRetries int `json:"failed_retries"`
}

// InternalReviewRunRecord summarizes the internal review loop.
type InternalReviewRunRecord struct {
	FirstRan       bool   `json:"first_ran"`
	LoopIterations int    `json:"loop_iterations"`
	EndedBy        string `json:"ended_by"`
}

// ExternalReviewerRecord summarizes one external reviewer and its iterations.
type ExternalReviewerRecord struct {
	Key         string                    `json:"key"`
	Label       string                    `json:"label"`
	Iterations  []ExternalIterationRecord `json:"iterations"`
	Duration    Duration                  `json:"duration_ms"`
	EndedBy     string                    `json:"ended_by"`
	HadFindings bool                      `json:"had_findings"`
	Blocks      int                       `json:"blocks,omitempty"` // completed loops; above one under review_cadence = task
}

// ExternalIterationRecord preserves reviewer and evaluator output for one iteration.
type ExternalIterationRecord struct {
	Index             int    `json:"index"`
	Block             int    `json:"block,omitempty"` // 1-based review block; zero in records that predate blocks
	ReviewerOutput    string `json:"reviewer_output"`
	EvaluatorResponse string `json:"evaluator_response"`
	Truncated         bool   `json:"truncated"`
}

// PostReviewRunRecord summarizes the post-review fix loop.
type PostReviewRunRecord struct {
	Ran        bool `json:"ran"`
	Iterations int  `json:"iterations"`
}

//go:generate moq -out run_record_store_mock_test.go -pkg processor_test -skip-ensure -fmt goimports . RunRecordStore

// RunRecordStore persists a run record between process invocations.
type RunRecordStore interface {
	Load() (RunRecord, bool, error)
	Save(RunRecord) error
	Remove() error
}

// truncateForRecord bounds persisted model output while preserving UTF-8 rune boundaries.
func truncateForRecord(s string) (string, bool) {
	if len(s) <= runRecordTextCap {
		return s, false
	}

	cut := runRecordTextCap
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + truncatedRecordMarker, true
}

// boundExternalReviewText shares the aggregate budget among nonempty outputs.
// Keep every iteration and its metadata so truncation never changes reported counts.
// Under review_cadence = task each reviewer's latest block keeps up to half the budget, so the
// final whole-branch review is not crowded out by the per-task blocks it re-reviews.
func boundExternalReviewText(reviewers []ExternalReviewerRecord) {
	var latest, earlier []boundedReviewField
	for i := range reviewers {
		last := lastReviewBlock(reviewers[i])
		for j := range reviewers[i].Iterations {
			iteration := &reviewers[i].Iterations[j]
			for _, field := range []*string{&iteration.ReviewerOutput, &iteration.EvaluatorResponse} {
				if *field == "" {
					continue
				}
				if iteration.Block < last {
					earlier = append(earlier, boundedReviewField{text: field, iteration: iteration})
					continue
				}
				latest = append(latest, boundedReviewField{text: field, iteration: iteration})
			}
		}
	}
	if len(earlier) == 0 {
		truncateReviewFields(latest, runRecordExternalTextCap)
		return
	}
	latestBudget := min(reviewFieldsSize(latest), runRecordExternalTextCap/2)
	truncateReviewFields(latest, latestBudget)
	truncateReviewFields(earlier, runRecordExternalTextCap-latestBudget)
}

// boundedReviewField is one nonempty output and the iteration whose truncation flag it sets.
type boundedReviewField struct {
	text      *string
	iteration *ExternalIterationRecord
}

// lastReviewBlock returns the highest block number among a reviewer's iterations.
func lastReviewBlock(reviewer ExternalReviewerRecord) int {
	last := 0
	for _, iteration := range reviewer.Iterations {
		last = max(last, iteration.Block)
	}
	return last
}

func reviewFieldsSize(fields []boundedReviewField) int {
	total := 0
	for _, field := range fields {
		total += len(*field.text)
	}
	return total
}

// truncateReviewFields splits budget evenly among fields when their total exceeds it.
func truncateReviewFields(fields []boundedReviewField, budget int) {
	if len(fields) == 0 || reviewFieldsSize(fields) <= budget {
		return
	}
	limit := budget / len(fields)
	for _, field := range fields {
		if len(*field.text) <= limit {
			continue
		}
		field.iteration.Truncated = true
		if limit < len(truncatedRecordMarker) {
			*field.text = ""
			continue
		}
		cut := limit - len(truncatedRecordMarker)
		for cut > 0 && !utf8.RuneStart((*field.text)[cut]) {
			cut--
		}
		*field.text = (*field.text)[:cut] + truncatedRecordMarker
	}
}
