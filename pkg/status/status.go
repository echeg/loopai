// Package status defines shared execution-model types for loopai.
// signal constants, phase types, and section types used by processor, executor, progress, and web packages.
package status

import "strings"

// signal constants using <<<RALPHEX:...>>> format for clear detection.
const (
	Completed  = "<<<RALPHEX:ALL_TASKS_DONE>>>"
	Failed     = "<<<RALPHEX:TASK_FAILED>>>"
	ReviewDone = "<<<RALPHEX:REVIEW_DONE>>>"
	// ExternalReviewDone is the provider-neutral external review completion signal.
	ExternalReviewDone = "<<<RALPHEX:EXTERNAL_REVIEW_DONE>>>"
	// CodexDone is retained for customized prompts and historical progress logs.
	CodexDone = "<<<RALPHEX:CODEX_REVIEW_DONE>>>"
	Question  = "<<<RALPHEX:QUESTION>>>"
	PlanReady = "<<<RALPHEX:PLAN_READY>>>"
	PlanDraft = "<<<RALPHEX:PLAN_DRAFT>>>"
	// FinalizeDone marks a base sync whose conflict resolution and validation succeeded.
	FinalizeDone = "<<<RALPHEX:FINALIZE_DONE>>>"
	// FinalizeBlocked marks a base sync the model could not accept; a one-line reason follows it.
	FinalizeBlocked = "<<<RALPHEX:FINALIZE_BLOCKED>>>"
	// ACPMerge ends an ACP chat answer whose user asked to merge the branch into its base.
	ACPMerge = "<<<RALPHEX:ACP_MERGE>>>"
	// ACPPR ends an ACP chat answer whose user asked to open a pull request.
	ACPPR = "<<<RALPHEX:ACP_PR>>>"
)

// signals lists every protocol signal StripSignals removes from user-visible text.
var signals = []string{
	Completed, Failed, ReviewDone, ExternalReviewDone, CodexDone, Question, PlanReady, PlanDraft,
	FinalizeDone, FinalizeBlocked, ACPMerge, ACPPR,
}

// StripSignals removes protocol signals from text shown to a user. A line left empty by the
// removal is dropped, and blank lines at either end of the result are trimmed; every other line,
// including text sharing a line with a signal, is kept.
func StripSignals(text string) string {
	if !strings.Contains(text, "<<<RALPHEX:") {
		return text
	}
	lines := strings.Split(text, "\n")
	kept := lines[:0]
	for _, line := range lines {
		stripped := line
		for _, sig := range signals {
			stripped = strings.ReplaceAll(stripped, sig, "")
		}
		if stripped != line {
			if strings.TrimSpace(stripped) == "" {
				continue
			}
			stripped = strings.TrimRight(stripped, " \t")
		}
		kept = append(kept, stripped)
	}
	return strings.Trim(strings.Join(kept, "\n"), "\n")
}

// Phase represents execution phase for color coding.
type Phase string

// Phase constants for execution stages.
const (
	PhaseTask           Phase = "task"            // execution phase (green)
	PhaseReview         Phase = "review"          // code review phase (cyan)
	PhaseExternalReview Phase = "external-review" // external reviewer phase (existing codex color)
	PhaseExternalEval   Phase = "external-eval"   // review-provider evaluator phase (existing claude-eval color)
	PhasePlan           Phase = "plan"            // plan creation phase (info color)
	PhaseFinalize       Phase = "finalize"        // finalize step phase (green)
	PhaseReport         Phase = "report"          // completion report phase (green)
	PhaseLimitWait      Phase = "limit-wait"      // waiting for a provider limit reset (red)

	// PhaseCodex and PhaseClaudeEval remain readable for historical progress logs.
	PhaseCodex      Phase = "codex"
	PhaseClaudeEval Phase = "claude-eval"
)
