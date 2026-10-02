package phase

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/umputun/ralphex/pkg/status"
)

// Signal aliases mirror pkg/status values used by phase prompt contracts and parser helpers.
const (
	SignalCompleted          = status.Completed
	SignalFailed             = status.Failed
	SignalReviewDone         = status.ReviewDone
	SignalExternalReviewDone = status.ExternalReviewDone
	SignalCodexDone          = status.CodexDone
	SignalQuestion           = status.Question
	SignalPlanReady          = status.PlanReady
	SignalPlanDraft          = status.PlanDraft
	SignalFinalizeDone       = status.FinalizeDone
	SignalFinalizeBlocked    = status.FinalizeBlocked
)

var questionSignalRe = regexp.MustCompile(`<<<RALPHEX:QUESTION>>>\s*([\s\S]*?)\s*<<<RALPHEX:END>>>`)

var planDraftSignalRe = regexp.MustCompile(`<<<RALPHEX:PLAN_DRAFT>>>\s*([\s\S]*?)\s*<<<RALPHEX:END>>>`)

// QuestionPayload represents a question signal from the plan creation phase.
type QuestionPayload struct {
	Question string   `json:"question"`
	Options  []string `json:"options"`
	Context  string   `json:"context,omitempty"`
}

// IsReviewDone reports whether signal marks internal review completion.
func IsReviewDone(signal string) bool {
	return signal == SignalReviewDone
}

// IsExternalReviewDone reports whether either the current or legacy signal marks
// external review completion.
func IsExternalReviewDone(signal string) bool {
	return signal == SignalExternalReviewDone || signal == SignalCodexDone
}

// IsCodexDone is retained for compatibility with existing phase callers.
func IsCodexDone(signal string) bool {
	return IsExternalReviewDone(signal)
}

// IsPlanReady reports whether signal marks plan creation completion.
func IsPlanReady(signal string) bool {
	return signal == SignalPlanReady
}

// IsFinalizeDone reports whether signal marks an accepted base sync.
func IsFinalizeDone(signal string) bool {
	return signal == SignalFinalizeDone
}

// IsFinalizeBlocked reports whether signal marks a base sync the model refused or could not validate.
func IsFinalizeBlocked(signal string) bool {
	return signal == SignalFinalizeBlocked
}

// maxFinalizeReasonLen bounds the reason taken from model output, since it reaches
// one-line status surfaces such as the summary, notifications, and terminal titles.
const maxFinalizeReasonLen = 200

// ParseFinalizeBlockedReason returns the one-line reason following the last
// FINALIZE_BLOCKED signal in output, or an empty string when the signal is
// absent or carries no reason. The text after the signal on the same line wins;
// otherwise the first non-empty line below it is used.
func ParseFinalizeBlockedReason(output string) string {
	idx := strings.LastIndex(output, SignalFinalizeBlocked)
	if idx < 0 {
		return ""
	}
	for line := range strings.SplitSeq(output[idx+len(SignalFinalizeBlocked):], "\n") {
		reason := strings.TrimSpace(line)
		reason = strings.TrimSpace(strings.TrimLeft(reason, ":-–— \t"))
		if strings.HasPrefix(reason, "<<<RALPHEX:") {
			return ""
		}
		if reason == "" {
			continue
		}
		if r := []rune(reason); len(r) > maxFinalizeReasonLen {
			reason = string(r[:maxFinalizeReasonLen]) + "..."
		}
		return reason
	}
	return ""
}

// ErrNoQuestionSignal indicates no question signal was found in output.
var ErrNoQuestionSignal = errors.New("no question signal found")

// ErrNoPlanDraftSignal indicates no plan draft signal was found in output.
var ErrNoPlanDraftSignal = errors.New("no plan draft signal found")

// ParseQuestionPayload extracts a question payload from output containing a QUESTION signal.
func ParseQuestionPayload(output string) (*QuestionPayload, error) {
	if !strings.Contains(output, SignalQuestion) {
		return nil, ErrNoQuestionSignal
	}

	matches := questionSignalRe.FindStringSubmatch(output)
	if len(matches) < 2 {
		return nil, errors.New("malformed question signal: missing END marker or empty payload")
	}

	jsonStr := strings.TrimSpace(matches[1])
	if jsonStr == "" {
		return nil, errors.New("malformed question signal: empty JSON payload")
	}

	var payload QuestionPayload
	if err := json.Unmarshal([]byte(jsonStr), &payload); err != nil {
		return nil, fmt.Errorf("malformed question signal: invalid JSON: %w", err)
	}

	if payload.Question == "" {
		return nil, errors.New("malformed question signal: missing question field")
	}
	if len(payload.Options) == 0 {
		return nil, errors.New("malformed question signal: missing or empty options field")
	}

	return &payload, nil
}

// ParsePlanDraftPayload extracts plan content from output containing a PLAN_DRAFT signal.
func ParsePlanDraftPayload(output string) (string, error) {
	if !strings.Contains(output, SignalPlanDraft) {
		return "", ErrNoPlanDraftSignal
	}

	matches := planDraftSignalRe.FindStringSubmatch(output)
	if len(matches) < 2 {
		return "", errors.New("malformed plan draft signal: missing END marker or empty content")
	}

	content := strings.TrimSpace(matches[1])
	if content == "" {
		return "", errors.New("malformed plan draft signal: empty plan content")
	}

	return content, nil
}
