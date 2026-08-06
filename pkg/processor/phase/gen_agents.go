package phase

import (
	"context"
	"errors"

	"github.com/umputun/ralphex/pkg/status"
)

// GenAgentsPhase runs the single-session project agent generation used by the
// --gen-agents standalone mode. The session analyzes the repository and writes
// agent files; loopai only reports what appeared on disk afterwards.
type GenAgentsPhase struct {
	cfg         Config
	log         GenAgentsLogger
	exec        Executor
	policy      Policy
	prompts     GenAgentsPrompts
	phaseHolder *status.PhaseHolder
}

// GenAgentsPhaseOpts contains dependencies for GenAgentsPhase.
type GenAgentsPhaseOpts struct {
	Cfg         Config
	Log         GenAgentsLogger
	Exec        Executor
	Policy      Policy
	Prompts     GenAgentsPrompts
	PhaseHolder *status.PhaseHolder
}

// NewGenAgentsPhase creates an agent generation phase engine.
func NewGenAgentsPhase(opts GenAgentsPhaseOpts) *GenAgentsPhase {
	return &GenAgentsPhase{
		cfg: opts.Cfg, log: opts.Log, exec: opts.Exec,
		policy: opts.Policy, prompts: opts.Prompts, phaseHolder: opts.PhaseHolder,
	}
}

// Run executes one executor session with the agent generation prompt.
// unlike finalize, failures are propagated: the mode has nothing else to do.
// the phase reuses PhasePlan for coloring — like plan creation it is a standalone
// analysis session, and adding a phase constant would ripple through the color,
// cmux, and dashboard mappings for no user-visible gain.
func (p *GenAgentsPhase) Run(ctx context.Context) error {
	if p.phaseHolder != nil {
		p.phaseHolder.Set(status.PhasePlan)
	}
	p.log.PrintSection(status.NewGenericSection("agent generation"))

	execName := p.cfg.executorName()
	execResult := p.policy.Run(ctx, p.exec.Run, p.prompts.GenAgentsPrompt(), execName)
	result := execResult.Result
	if err := wrapExecutorError(p.policy, result.Error, execName); err != nil {
		return err
	}

	if result.Signal == SignalFailed {
		return errors.New("agent generation failed (FAILED signal received)")
	}
	if execResult.TimedOut {
		return errors.New("agent generation session timed out")
	}

	p.log.Print("agent generation completed")
	return nil
}
