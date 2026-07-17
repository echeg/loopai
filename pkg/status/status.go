// Package status defines shared execution-model types for ralphex.
// signal constants, phase types, and section types used by processor, executor, progress, and web packages.
package status

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
)

// Phase represents execution phase for color coding.
type Phase string

// Phase constants for execution stages.
const (
	PhaseTask           Phase = "task"            // execution phase (green)
	PhaseReview         Phase = "review"          // code review phase (cyan)
	PhaseExternalReview Phase = "external-review" // external reviewer phase (existing codex color)
	PhaseExternalEval   Phase = "external-eval"   // primary evaluator phase (existing claude-eval color)
	PhasePlan           Phase = "plan"            // plan creation phase (info color)
	PhaseFinalize       Phase = "finalize"        // finalize step phase (green)

	// PhaseCodex and PhaseClaudeEval remain readable for historical progress logs.
	PhaseCodex      Phase = "codex"
	PhaseClaudeEval Phase = "claude-eval"
)
