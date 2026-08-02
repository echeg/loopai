package phase

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/umputun/ralphex/pkg/config"
	"github.com/umputun/ralphex/pkg/status"
)

// ExternalReviewOutcome reports whether external review found issues.
type ExternalReviewOutcome struct {
	HadFindings bool
}

// ExternalReviewer pairs an external-review provider with its read-only executor.
type ExternalReviewer struct {
	Tool string
	Exec Executor
}

// ExternalReviewPhase runs provider-aware external review loops.
type ExternalReviewPhase struct {
	cfg            Config
	log            ExternalReviewLogger
	reviewers      []ExternalReviewer
	review         Executor
	policy         Policy
	prompts        ExternalReviewPrompts
	breaks         *BreakController
	git            *GitState
	phaseHolder    *status.PhaseHolder
	iterationDelay time.Duration
}

// ExternalReviewPhaseOpts contains dependencies for ExternalReviewPhase.
type ExternalReviewPhaseOpts struct {
	Cfg            Config
	Log            ExternalReviewLogger
	Reviewers      []ExternalReviewer
	Review         Executor
	Policy         Policy
	Prompts        ExternalReviewPrompts
	Breaks         *BreakController
	Git            *GitState
	PhaseHolder    *status.PhaseHolder
	IterationDelay time.Duration
}

// NewExternalReviewPhase creates an external review phase engine.
func NewExternalReviewPhase(opts ExternalReviewPhaseOpts) *ExternalReviewPhase {
	return &ExternalReviewPhase{
		cfg: opts.Cfg, log: opts.Log, reviewers: opts.Reviewers,
		review: opts.Review, policy: opts.Policy, prompts: opts.Prompts, breaks: opts.Breaks,
		git: opts.Git, phaseHolder: opts.PhaseHolder, iterationDelay: opts.IterationDelay,
	}
}

// Enabled reports whether at least one external reviewer is configured.
func (p *ExternalReviewPhase) Enabled() bool { return len(p.reviewers) > 0 }

// Label returns the provider names in execution order.
func (p *ExternalReviewPhase) Label() string {
	tools := make([]string, 0, len(p.reviewers))
	for _, reviewer := range p.reviewers {
		tools = append(tools, reviewer.Tool)
	}
	return strings.Join(tools, " → ")
}

// Run executes the configured external review loop and reports whether fixes need post-review.
func (p *ExternalReviewPhase) Run(ctx context.Context) (ExternalReviewOutcome, error) {
	if !p.Enabled() {
		p.log.Print("external review disabled, skipping...")
		return ExternalReviewOutcome{}, nil
	}

	outcome := ExternalReviewOutcome{}
	for _, reviewer := range p.reviewers {
		if reviewer.Exec == nil {
			if reviewer.Tool == config.ExternalReviewToolCustom {
				return outcome, errors.New("custom review script not configured")
			}
			return outcome, fmt.Errorf("%s review executor not configured", reviewer.Tool)
		}
		p.log.PrintSection(status.NewGenericSection("external review (" + reviewer.Tool + ")"))
		reviewerOutcome, interrupted, err := p.runLoop(ctx, reviewer)
		outcome.HadFindings = outcome.HadFindings || reviewerOutcome.HadFindings
		if err != nil {
			return outcome, err
		}
		if interrupted {
			return outcome, nil
		}
	}
	return outcome, nil
}

func (p *ExternalReviewPhase) showSummary(toolName, output string) {
	summary := output
	if idx := strings.Index(summary, "```"); idx > 0 {
		summary = summary[:idx]
	}
	if runes := []rune(summary); len(runes) > maxExternalSummaryLen {
		summary = string(runes[:maxExternalSummaryLen]) + "..."
	}

	summary = strings.TrimSpace(summary)
	if summary == "" {
		return
	}

	p.log.Print("%s findings:", toolName)
	for line := range strings.SplitSeq(summary, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		p.log.PrintAligned("  " + line)
	}
}

func (p *ExternalReviewPhase) runLoop(ctx context.Context, reviewer ExternalReviewer) (ExternalReviewOutcome, bool, error) {
	outcome := ExternalReviewOutcome{}
	loopCtx, loopCancel := p.breaks.context(ctx)
	defer loopCancel()

	var evaluatorResponse string
	firstCompleted := false
	stalemate := newStalemateState(p.cfg, p.log)

	for i := 1; i <= p.maxIterations(); i++ {
		result, err := p.runIteration(loopCtx, externalReviewIterationOpts{
			parent:            ctx,
			tool:              reviewer.Tool,
			exec:              reviewer.Exec,
			iteration:         i,
			firstCompleted:    firstCompleted,
			evaluatorResponse: evaluatorResponse,
		})
		if err != nil {
			if errors.Is(err, errExternalReviewBreak) {
				return outcome, true, nil
			}
			return outcome, false, err
		}

		if result.firstCompleted {
			firstCompleted = true
			evaluatorResponse = result.evaluatorResponse
		}
		if result.hadFindings {
			outcome.HadFindings = true
			if stalemate.Update(result.before, p.git.snapshot()) {
				return outcome, false, nil
			}
		}

		switch result.action {
		case externalReviewContinue:
			// fall through to the sleep before the next iteration below
		case externalReviewStop:
			return outcome, false, nil
		case externalReviewRetry:
			continue
		}

		if err := p.sleepBeforeNext(loopCtx, ctx); err != nil {
			if errors.Is(err, errExternalReviewBreak) {
				return outcome, true, nil
			}
			return outcome, false, err
		}
	}

	p.log.Print("max %s iterations reached, continuing to next phase...", reviewer.Tool)
	return outcome, false, nil
}

type externalReviewIterationAction int

const (
	externalReviewContinue externalReviewIterationAction = iota
	externalReviewRetry
	externalReviewStop
)

type externalReviewIterationOpts struct {
	parent            context.Context
	tool              string
	exec              Executor
	iteration         int
	firstCompleted    bool
	evaluatorResponse string
}

type externalReviewIterationResult struct {
	action            externalReviewIterationAction
	before            gitSnapshot
	evaluatorResponse string
	firstCompleted    bool
	hadFindings       bool
}

func (p *ExternalReviewPhase) runIteration(ctx context.Context, opts externalReviewIterationOpts) (externalReviewIterationResult, error) {
	if err := p.checkLoopDone(ctx, opts.parent, opts.tool); err != nil {
		return externalReviewIterationResult{}, err
	}

	if p.phaseHolder != nil {
		p.phaseHolder.Set(status.PhaseExternalReview)
	}
	p.log.PrintSection(status.NewExternalReviewIterationSection(opts.tool, opts.iteration))

	reviewExecResult := p.runReviewTool(ctx, opts.tool, opts.exec, p.prompts.ExternalReviewPrompt(opts.tool, !opts.firstCompleted, opts.evaluatorResponse))
	reviewResult := reviewExecResult.Result
	if reviewResult.Error != nil {
		if err := p.handleExecutorError(ctx, opts.parent, opts.tool, reviewResult.Error); err != nil {
			return externalReviewIterationResult{}, err
		}
		// handleExecutorError always returns non-nil for a non-nil error, so this
		// fall-through is intentionally unreachable; kept defensive against future changes.
	}

	if reviewExecResult.TimedOut {
		p.log.Print("%s review session timed out, retrying on next iteration...", opts.tool)
		return externalReviewIterationResult{action: externalReviewRetry}, nil
	}

	// Empty output is still evaluated: the evaluator prompt explicitly treats it
	// as a clean result and owns the completion signal. Skipping evaluation here
	// would let a truncated reviewer response terminate the loop silently.
	if opts.tool == config.ExternalReviewToolCodex {
		p.showSummary(opts.tool, reviewResult.Output)
	}

	before := p.snapshotBeforeEval()
	evalExecResult, err := p.runEvaluation(ctx, opts.parent, opts.tool, reviewResult.Output)
	if err != nil {
		return externalReviewIterationResult{}, err
	}

	if evalExecResult.TimedOut {
		p.log.Print("%s eval session timed out, retrying %s iteration...", p.cfg.executorName(), opts.tool)
		return externalReviewIterationResult{action: externalReviewRetry}, nil
	}

	evalResult := evalExecResult.Result
	result := externalReviewIterationResult{before: before, evaluatorResponse: evalResult.Output, firstCompleted: true}
	if IsExternalReviewDone(evalResult.Signal) {
		p.log.Print("%s review complete - no more findings", opts.tool)
		result.action = externalReviewStop
		return result, nil
	}

	result.hadFindings = true
	return result, nil
}

var errExternalReviewBreak = errors.New("external review interrupted by manual break")

func (p *ExternalReviewPhase) checkLoopDone(loopCtx, parent context.Context, tool string) error {
	select {
	case <-loopCtx.Done():
		if p.breaks.isBreak(loopCtx, parent) {
			p.log.Print("manual break requested, external review terminated early")
			return errExternalReviewBreak
		}
		return fmt.Errorf("%s loop: %w", tool, parent.Err())
	default:
		return nil
	}
}

func (p *ExternalReviewPhase) handleExecutorError(loopCtx, parent context.Context, tool string, err error) error {
	if p.breaks.isBreak(loopCtx, parent) {
		p.log.Print("manual break requested, external review terminated early")
		return errExternalReviewBreak
	}
	if patternErr := p.policy.HandlePatternMatchError(err, tool); patternErr != nil {
		return fmt.Errorf("%s pattern handling: %w", tool, patternErr)
	}
	return fmt.Errorf("%s execution: %w", tool, err)
}

func (p *ExternalReviewPhase) snapshotBeforeEval() gitSnapshot {
	if p.cfg.ReviewPatience <= 0 {
		return gitSnapshot{}
	}
	return p.git.snapshot()
}

func (p *ExternalReviewPhase) runEvaluation(loopCtx, parent context.Context, reviewer, output string) (ExecutionResult, error) {
	evaluator := p.cfg.executorName()
	if p.phaseHolder != nil {
		p.phaseHolder.Set(status.PhaseExternalEval)
	}
	p.log.PrintSection(status.NewExternalEvaluationSection(evaluator, reviewer))
	result := p.policy.Run(loopCtx, p.review.Run, p.prompts.ExternalEvaluationPrompt(reviewer, output), evaluator)
	if p.phaseHolder != nil {
		p.phaseHolder.Set(status.PhaseExternalReview)
	}

	if result.Result.Error == nil {
		return result, nil
	}
	if err := p.handleExecutorError(loopCtx, parent, evaluator, result.Result.Error); err != nil {
		return ExecutionResult{}, err
	}
	return result, nil
}

func (p *ExternalReviewPhase) sleepBeforeNext(loopCtx, parent context.Context) error {
	if err := p.policy.Sleep(loopCtx, p.iterationDelay); err != nil {
		if p.breaks.isBreak(loopCtx, parent) {
			p.log.Print("manual break requested, external review terminated early")
			return errExternalReviewBreak
		}
		return fmt.Errorf("interrupted: %w", err)
	}
	return nil
}

func (p *ExternalReviewPhase) maxIterations() int {
	maxIterations := max(minExternalReviewIterations, p.cfg.MaxIterations/externalIterationDivisor)
	if p.cfg.MaxExternalIterations > 0 {
		maxIterations = p.cfg.MaxExternalIterations
	}
	return maxIterations
}

func (p *ExternalReviewPhase) runReviewTool(ctx context.Context, tool string, exec Executor, prompt string) ExecutionResult {
	return p.policy.Run(ctx, exec.Run, prompt, tool)
}
