package phase

import (
	"context"
	"errors"
	"fmt"

	"github.com/umputun/ralphex/pkg/status"
)

// ReportPhase runs the optional best-effort completion report step.
type ReportPhase struct {
	cfg         Config
	log         ReportLogger
	exec        Executor
	policy      Policy
	prompts     ReportPrompts
	phaseHolder *status.PhaseHolder
}

// ReportPhaseOpts contains dependencies for ReportPhase.
type ReportPhaseOpts struct {
	Cfg         Config
	Log         ReportLogger
	Exec        Executor
	Policy      Policy
	Prompts     ReportPrompts
	PhaseHolder *status.PhaseHolder
}

// NewReportPhase creates a completion report phase engine.
func NewReportPhase(opts ReportPhaseOpts) *ReportPhase {
	return &ReportPhase{
		cfg: opts.Cfg, log: opts.Log, exec: opts.Exec,
		policy: opts.Policy, prompts: opts.Prompts, phaseHolder: opts.PhaseHolder,
	}
}

// Run asks the review executor to turn deterministic facts into a completion report.
// Ordinary failures are logged and return an empty report so the caller can use its
// deterministic fallback. Parent context cancellation remains blocking.
func (p *ReportPhase) Run(ctx context.Context, facts string) (string, error) {
	if !p.cfg.ReportEnabled {
		return "", nil
	}

	if p.phaseHolder != nil {
		p.phaseHolder.Set(status.PhaseReport)
	}
	p.log.PrintSection(status.NewGenericSection("report step"))

	execName := p.cfg.executorName()
	execResult := p.policy.Run(ctx, p.exec.Run, p.prompts.ReportPrompt(facts), execName)
	result := execResult.Result

	if execResult.TimedOut {
		p.log.Print("report step timed out (non-blocking)")
		return "", nil
	}
	if result.Error != nil && (errors.Is(result.Error, context.Canceled) || errors.Is(result.Error, context.DeadlineExceeded)) {
		return "", fmt.Errorf("report step: %w", result.Error)
	}
	if result.Error != nil {
		if p.policy.HandlePatternMatchError(result.Error, execName) != nil {
			return "", nil //nolint:nilerr // intentional: report generation is best-effort
		}
		p.log.Print("report step failed: %v", result.Error)
		return "", nil
	}
	if result.Signal == SignalFailed {
		p.log.Print("report step reported failure (non-blocking)")
		return "", nil
	}

	p.log.Print("report step completed")
	return result.Output, nil
}
