package processor

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/umputun/ralphex/pkg/config"
	"github.com/umputun/ralphex/pkg/executor"
	"github.com/umputun/ralphex/pkg/limits"
	"github.com/umputun/ralphex/pkg/processor/phase"
	"github.com/umputun/ralphex/pkg/status"
)

type retryPolicy struct {
	cfg         Config
	log         Logger
	waitOnLimit time.Duration
	phaseHolder *status.PhaseHolder
	recovery    limits.Recovery
}

type retryPolicyOpts struct {
	cfg         Config
	log         Logger
	waitOnLimit time.Duration
	phaseHolder *status.PhaseHolder
	recovery    limits.Recovery
}

func newRetryPolicy(opts retryPolicyOpts) *retryPolicy {
	return &retryPolicy{
		cfg: opts.cfg, log: opts.log, waitOnLimit: opts.waitOnLimit, phaseHolder: opts.phaseHolder,
		recovery: opts.recovery,
	}
}

func (p *retryPolicy) Run(ctx context.Context, run func(context.Context, string) executor.Result,
	prompt string, toolName string) phase.ExecutionResult {
	for {
		var snapshot limits.Snapshot
		if p.recovery != nil && toolName == config.ExternalReviewToolClaude {
			snapshot = p.recovery.Snapshot(ctx)
		}
		result := p.runWithSessionTimeout(ctx, run, prompt, toolName)
		if result.Result.Error == nil {
			return result
		}

		var retryErr *executor.RetryPatternError
		if errors.As(result.Result.Error, &retryErr) {
			p.log.Print("transient %s error detected: %q, treating as session timeout", toolName, retryErr.Pattern)
			result.Result.Error = nil
			result.Result.Signal = ""
			return phase.ExecutionResult{Result: result.Result, TimedOut: true}
		}

		var limitErr *executor.LimitPatternError
		if !errors.As(result.Result.Error, &limitErr) {
			return result
		}

		if p.waitOnLimit <= 0 {
			return result
		}

		restorePhase := p.enterLimitWait()
		if p.recovery != nil && toolName == config.ExternalReviewToolClaude {
			if err := p.recoverClaudeLimit(ctx, snapshot); err != nil {
				restorePhase()
				return phase.ExecutionResult{Result: executor.Result{Error: err}}
			}
			restorePhase()
			continue
		}
		waitLabel := formatLimitWait(p.waitOnLimit)
		p.logLimitWait(limitErr.Pattern, toolName, waitLabel)

		if err := p.Sleep(ctx, p.waitOnLimit); err != nil {
			restorePhase()
			return phase.ExecutionResult{Result: executor.Result{Error: fmt.Errorf("interrupted during limit wait: %w", ctx.Err())}}
		}
		restorePhase()
	}
}

func (p *retryPolicy) recoverClaudeLimit(ctx context.Context, snapshot limits.Snapshot) error {
	p.logRecovery("switching Claude account", "rate limit detected in Claude output; attempting claude-swap account rotation...")
	recovered, recoverErr := p.recovery.Recover(ctx, snapshot, p.waitOnLimit)
	if recoverErr == nil && recovered.Recovered {
		p.logRecoveredAccount(recovered)
		if recovered.RetryAfter <= 0 {
			return nil
		}
		if err := p.Sleep(ctx, recovered.RetryAfter); err != nil {
			return fmt.Errorf("interrupted during account switch settle: %w", ctx.Err())
		}
		return nil
	}

	waitLabel := formatLimitWait(p.waitOnLimit)
	if recoverErr != nil {
		p.logRecovery("swap unavailable · retry in "+waitLabel,
			fmt.Sprintf("claude-swap unavailable (%s); waiting %s before retry...", recovered.Reason, waitLabel))
	} else {
		p.logRecovery("swap unavailable · retry in "+waitLabel,
			fmt.Sprintf("claude-swap could not select another account (%s); waiting %s before retry...",
				recovered.Reason, waitLabel))
	}
	if err := p.Sleep(ctx, p.waitOnLimit); err != nil {
		return fmt.Errorf("interrupted during limit wait: %w", ctx.Err())
	}
	return nil
}

func (p *retryPolicy) logRecoveredAccount(recovered limits.RecoveryResult) {
	waitLabel := formatLimitWait(recovered.RetryAfter)
	if recovered.Switched {
		p.logRecovery("account switched · retry in "+waitLabel,
			fmt.Sprintf("claude-swap switched account slot %d to slot %d; retrying in %s...",
				recovered.From, recovered.To, waitLabel))
		return
	}
	p.logRecovery("account already switched · retry in "+waitLabel,
		fmt.Sprintf("another process already changed the Claude account to slot %d; retrying in %s...",
			recovered.To, waitLabel))
}

// limitRecoveryLogger is an optional logger extension used by the cmux
// decorator to show account-switch progress without coupling processor to cmux.
type limitRecoveryLogger interface {
	LogLimitRecovery(statusText, message string)
}

func (p *retryPolicy) logRecovery(statusText, message string) {
	if log, ok := p.log.(limitRecoveryLogger); ok {
		log.LogLimitRecovery(statusText, message)
		return
	}
	p.log.Print("%s", message)
}

// limitWaitLogger is an optional logger extension used by the cmux decorator to enrich the
// temporary rate-limit status without coupling the processor package to cmux.
type limitWaitLogger interface {
	LogLimitWait(pattern, tool, waitLabel string)
}

func (p *retryPolicy) logLimitWait(pattern, tool, waitLabel string) {
	if log, ok := p.log.(limitWaitLogger); ok {
		log.LogLimitWait(pattern, tool, waitLabel)
		return
	}
	p.log.Print("rate limit detected: %q in %s output, waiting %s before retry...",
		pattern, tool, waitLabel)
}

func (p *retryPolicy) enterLimitWait() func() {
	if p.phaseHolder == nil {
		return func() {}
	}
	previous := p.phaseHolder.Get()
	p.phaseHolder.Set(status.PhaseLimitWait)
	return func() {
		// Do not overwrite a newer phase if another observer or cancellation path advanced it.
		if p.phaseHolder.Get() == status.PhaseLimitWait {
			p.phaseHolder.Set(previous)
		}
	}
}

func formatLimitWait(d time.Duration) string {
	if d > 0 && d%time.Hour == 0 {
		return fmt.Sprintf("%dh", int64(d/time.Hour))
	}
	if d > 0 && d%time.Minute == 0 {
		return fmt.Sprintf("%dm", int64(d/time.Minute))
	}
	return d.String()
}

func (p *retryPolicy) HandlePatternMatchError(err error, tool string) error {
	var patternErr *executor.PatternMatchError
	if errors.As(err, &patternErr) {
		p.log.Print("error: detected %q in %s output", patternErr.Pattern, tool)
		p.log.Print("run '%s' for more information", patternErr.HelpCmd)
		return err
	}
	var limitErr *executor.LimitPatternError
	if errors.As(err, &limitErr) {
		p.log.Print("error: detected %q in %s output", limitErr.Pattern, tool)
		p.log.Print("run '%s' for more information", limitErr.HelpCmd)
		return err
	}
	return nil
}

func (p *retryPolicy) Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("sleep interrupted: %w", ctx.Err())
	}
}

func (p *retryPolicy) runWithSessionTimeout(ctx context.Context, run func(context.Context, string) executor.Result,
	prompt string, toolName string) phase.ExecutionResult {
	sessionTimeout := p.sessionTimeout()
	codexMode := p.cfg.isCodexExecutor()
	useTimeout := sessionTimeout > 0 && (codexMode || toolName == config.ExternalReviewToolClaude)

	if !useTimeout {
		result := run(ctx, prompt)
		return phase.ExecutionResult{Result: result, TimedOut: p.handleIdleTimeout(result, toolName)}
	}

	childCtx, cancel := context.WithTimeout(ctx, sessionTimeout)
	defer cancel()

	result := run(childCtx, prompt)

	if childCtx.Err() != nil && ctx.Err() == nil {
		p.log.Print("warning: %s session timed out after %s, the agent may have started a blocking operation",
			toolName, sessionTimeout)
		result.Error = nil
		result.Signal = ""
		return phase.ExecutionResult{Result: result, TimedOut: true}
	}

	return phase.ExecutionResult{Result: result, TimedOut: p.handleIdleTimeout(result, toolName)}
}

func (p *retryPolicy) handleIdleTimeout(result executor.Result, toolName string) bool {
	if result.IdleTimedOut && result.Signal == "" {
		p.log.Print("warning: %s session idle timed out, no output activity detected", toolName)
		return true
	}
	return false
}

func (p *retryPolicy) sessionTimeout() time.Duration {
	if p.cfg.AppConfig == nil {
		return 0
	}
	return p.cfg.AppConfig.SessionTimeout
}
