package status

import "fmt"

// SectionType represents the semantic type of a section header.
// the web layer uses these types to emit appropriate boundary events:
//   - SectionTaskIteration: emits task_start/task_end events
//   - SectionInternalReview, SectionExternalReviewIteration: emits iteration_start events
//   - SectionGeneric, SectionExternalEvaluation: no boundary events, just section headers
//
// invariants:
//   - Iteration > 0 for SectionTaskIteration, SectionExternalReviewIteration
//   - Iteration >= 0 for SectionInternalReview (first review pass uses 0)
//   - Iteration == 0 for SectionGeneric, SectionExternalEvaluation
//
// prefer using the constructor functions (NewTaskIterationSection, etc.) to ensure
// these invariants are maintained.
type SectionType int

const (
	// SectionGeneric is a static section header with no iteration.
	SectionGeneric SectionType = iota
	// SectionTaskIteration represents a task execution iteration.
	SectionTaskIteration
	// SectionInternalReview represents an internal review iteration.
	SectionInternalReview
	// SectionExternalReviewIteration represents an external review iteration.
	SectionExternalReviewIteration
	// SectionExternalEvaluation represents the primary executor evaluating external findings.
	SectionExternalEvaluation
	// SectionPlanIteration represents a plan creation iteration.
	SectionPlanIteration
	// SectionCustomIteration represents a custom review tool iteration.
	SectionCustomIteration
)

// Legacy section names are aliases so existing callers keep their structured behavior.
const (
	SectionCodexIteration = SectionExternalReviewIteration
	SectionClaudeEval     = SectionExternalEvaluation
	SectionExternalEval   = SectionExternalEvaluation
)

// Section carries structured information about a section header.
// instead of parsing section names with regex, consumers can access
// the Type and Iteration fields directly.
//
// use the provided constructors (NewTaskIterationSection, etc.) to create sections
// with proper Type/Iteration/Label consistency.
type Section struct {
	Type      SectionType
	Iteration int    // 0 for non-iterated sections
	Label     string // human-readable display text
}

// NewTaskIterationSection creates a section for task execution iteration.
func NewTaskIterationSection(iteration int) Section {
	return Section{
		Type:      SectionTaskIteration,
		Iteration: iteration,
		Label:     fmt.Sprintf("task iteration %d", iteration),
	}
}

// NewClaudeReviewSection creates a section for Claude review iteration.
// suffix is appended after the iteration number (e.g., ": critical/major").
func NewClaudeReviewSection(iteration int, suffix string) Section {
	return Section{
		Type:      SectionInternalReview,
		Iteration: iteration,
		Label:     fmt.Sprintf("claude review %d%s", iteration, suffix),
	}
}

// NewInternalReviewSection creates a section for executor-neutral internal review iteration.
// the label uses a fixed "review" prefix (no executor name) because the web
// dashboard's name-based phase routing (phaseFromSection) matches "codex"
// before "review" — embedding executor names in the label would misroute
// the codex-executor internal review into PhaseCodex on file-replay.
func NewInternalReviewSection(iteration int, suffix string) Section {
	return Section{
		Type:      SectionInternalReview,
		Iteration: iteration,
		Label:     fmt.Sprintf("review %d%s", iteration, suffix),
	}
}

// NewExternalReviewIterationSection creates a provider-aware external review section.
func NewExternalReviewIterationSection(reviewer string, iteration int) Section {
	return Section{
		Type:      SectionExternalReviewIteration,
		Iteration: iteration,
		Label:     fmt.Sprintf("%s external review iteration %d", reviewer, iteration),
	}
}

// NewExternalReviewSection is a concise alias for NewExternalReviewIterationSection.
func NewExternalReviewSection(reviewer string, iteration int) Section {
	return NewExternalReviewIterationSection(reviewer, iteration)
}

// NewExternalEvaluationSection creates a provider-aware external findings evaluation section.
func NewExternalEvaluationSection(evaluator, reviewer string) Section {
	return Section{
		Type:  SectionExternalEvaluation,
		Label: fmt.Sprintf("%s evaluating %s findings", evaluator, reviewer),
	}
}

// NewExternalEvalSection is a concise alias for NewExternalEvaluationSection.
func NewExternalEvalSection(evaluator, reviewer string) Section {
	return NewExternalEvaluationSection(evaluator, reviewer)
}

// NewCodexIterationSection creates a legacy Codex review iteration section.
func NewCodexIterationSection(iteration int) Section {
	return Section{
		Type:      SectionCodexIteration,
		Iteration: iteration,
		Label:     fmt.Sprintf("codex iteration %d", iteration),
	}
}

// NewClaudeEvalSection creates a legacy section for Claude evaluating Codex findings.
func NewClaudeEvalSection() Section {
	return Section{
		Type:  SectionClaudeEval,
		Label: "claude evaluating codex findings",
	}
}

// NewGenericSection creates a static section header with no iteration.
func NewGenericSection(label string) Section {
	return Section{
		Type:  SectionGeneric,
		Label: label,
	}
}

// NewPlanIterationSection creates a section for plan creation iteration.
func NewPlanIterationSection(iteration int) Section {
	return Section{
		Type:      SectionPlanIteration,
		Iteration: iteration,
		Label:     fmt.Sprintf("plan iteration %d", iteration),
	}
}

// NewCustomIterationSection creates a section for custom review tool iteration.
func NewCustomIterationSection(iteration int) Section {
	return Section{
		Type:      SectionCustomIteration,
		Iteration: iteration,
		Label:     fmt.Sprintf("custom review iteration %d", iteration),
	}
}
