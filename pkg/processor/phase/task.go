package phase

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/umputun/ralphex/pkg/plan"
	"github.com/umputun/ralphex/pkg/status"
)

// TaskPhase executes plan tasks until completion.
type TaskPhase struct {
	cfg            Config
	log            TaskLogger
	exec           Executor
	policy         Policy
	prompts        TaskPrompts
	locator        Locator
	deps           *Deps
	breaks         *BreakController
	iterationDelay time.Duration
	retryCount     int
	afterTask      func(ctx context.Context, taskNum int, headBefore string) error
}

// TaskPhaseOpts contains dependencies for TaskPhase.
type TaskPhaseOpts struct {
	Cfg            Config
	Log            TaskLogger
	Exec           Executor
	Policy         Policy
	Prompts        TaskPrompts
	Locator        Locator
	Deps           *Deps
	Breaks         *BreakController
	IterationDelay time.Duration
	RetryCount     int
	// AfterTask, when set, runs after a task iteration that advanced the plan. taskNum is the plan
	// position the iteration started on, headBefore the HEAD hash recorded before it (empty without git).
	AfterTask func(ctx context.Context, taskNum int, headBefore string) error
}

// NewTaskPhase creates a task phase engine.
func NewTaskPhase(opts TaskPhaseOpts) *TaskPhase {
	breaks := opts.Breaks
	if breaks == nil {
		breaks = NewBreakController(opts.Deps)
	}
	return &TaskPhase{
		cfg: opts.Cfg, log: opts.Log, exec: opts.Exec, policy: opts.Policy,
		prompts: opts.Prompts, locator: opts.Locator, deps: opts.Deps, breaks: breaks,
		iterationDelay: opts.IterationDelay, retryCount: opts.RetryCount, afterTask: opts.AfterTask,
	}
}

// Run executes one plan task per iteration until all actionable task checkboxes are complete.
func (p *TaskPhase) Run(ctx context.Context) error {
	prompt := p.prompts.TaskPrompt()
	retryCount := 0
	start := taskStart{pos: -1}

	for i := 1; i <= p.cfg.MaxIterations; i++ {
		select {
		case <-ctx.Done():
			return fmt.Errorf("task phase: %w", ctx.Err())
		default:
		}

		taskNum := i
		pos := p.NextPlanTaskPosition()
		if pos > 0 {
			taskNum = pos
		}
		p.trackTaskStart(&start, pos)
		p.log.PrintSection(status.NewTaskIterationSection(taskNum))

		loopCtx, loopCancel := p.breaks.context(ctx)

		execName := p.cfg.taskExecutorName()
		execResult := p.policy.Run(loopCtx, p.exec.Run, prompt, execName)
		result := execResult.Result
		if p.deps != nil && p.deps.Recorder != nil {
			p.deps.Recorder.TaskIteration(result.Signal == SignalFailed)
		}

		manualBreak := p.breaks.isBreak(loopCtx, ctx)
		loopCancel()

		if manualBreak {
			if err := p.resumeAfterBreak(ctx, &start); err != nil {
				return err
			}
			i--
			retryCount = 0
			continue
		}

		if err := wrapExecutorError(p.policy, result.Error, execName); err != nil {
			return err
		}

		if execResult.TimedOut {
			// a session that ticked its task before hanging advanced the plan; the next iteration
			// starts at another position, so this is the only point its review can run
			if err := p.runAfterTask(ctx, &start, ""); err != nil {
				return err
			}
			p.log.Print("%s session timed out, retrying task iteration after %s...", execName, retryBackoff)
			if err := p.policy.Sleep(ctx, retryBackoff); err != nil {
				return fmt.Errorf("interrupted: %w", err)
			}
			continue
		}

		if err := p.runAfterTask(ctx, &start, result.Signal); err != nil {
			return err
		}

		if result.Signal == SignalCompleted {
			if p.HasUncompletedTasks() {
				p.log.Print("warning: completion signal received but plan still has [ ] items, continuing...")
				continue
			}
			p.log.PrintRaw("\nall tasks completed, starting code review...\n")
			return nil
		}

		if result.Signal == SignalFailed {
			if retryCount < p.retryCount {
				p.log.Print("task failed, retrying...")
				retryCount++
				if err := p.policy.Sleep(ctx, p.iterationDelay); err != nil {
					return fmt.Errorf("interrupted: %w", err)
				}
				continue
			}
			return errors.New("task execution failed after retry (FAILED signal received)")
		}

		retryCount = 0
		if err := p.policy.Sleep(ctx, p.iterationDelay); err != nil {
			return fmt.Errorf("interrupted: %w", err)
		}
	}

	return fmt.Errorf("max iterations (%d) reached without completion", p.cfg.MaxIterations)
}

// resumeAfterBreak asks the pause handler whether to continue after a manual break and returns
// ErrUserAborted when it declines. A session interrupted after it ticked and committed its task
// still gets that task's review before the retry starts at the next position.
func (p *TaskPhase) resumeAfterBreak(ctx context.Context, start *taskStart) error {
	p.log.Print("session interrupted by break signal")
	p.breaks.drain()
	if p.deps.PauseHandler == nil || !p.deps.PauseHandler(ctx) {
		return ErrUserAborted
	}
	p.breaks.drain()
	return p.runAfterTask(ctx, start, "")
}

// taskStart is the plan position an iteration started on and the HEAD recorded when it first became current.
type taskStart struct {
	pos  int
	head string
}

// trackTaskStart records HEAD when the plan position changes. Keeping it across a retried, timed-out,
// or non-advancing attempt at the same task keeps the diff base before that task's own commits.
func (p *TaskPhase) trackTaskStart(start *taskStart, pos int) {
	if p.afterTask == nil || pos == start.pos {
		return
	}
	start.pos, start.head = pos, NewGitState(p.deps, p.log).headHash()
}

// runAfterTask calls the after-task hook when an iteration that did not fail advanced the plan:
// the first uncompleted position moved forward, or no uncompleted task remains. That includes a
// timed-out or interrupted session that ticked its task before it stopped. An iteration that
// started without a known position, or ticked nothing, does not call it.
func (p *TaskPhase) runAfterTask(ctx context.Context, start *taskStart, signal string) error {
	if p.afterTask == nil || signal == SignalFailed || start.pos <= 0 {
		return nil
	}
	pos := start.pos
	next := p.NextPlanTaskPosition()
	advanced := next > pos
	if next == 0 {
		advanced = !p.HasUncompletedTasks()
	}
	if !advanced {
		return nil
	}
	start.pos = -1
	if err := p.afterTask(ctx, pos, start.head); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, ErrUserAborted) {
			return err
		}
		return fmt.Errorf("after task %d: %w", pos, err)
	}
	return nil
}

// ValidatePlanHasTasks rejects plan files without executable task sections.
func (p *TaskPhase) ValidatePlanHasTasks() error {
	path := p.locator.Path()
	parsed, err := plan.ParsePlanFile(path)
	if err != nil {
		return fmt.Errorf("parse plan for validation: %w", err)
	}
	if len(parsed.Tasks) == 0 {
		return fmt.Errorf("plan file %q has no executable task sections (### Task N: or ### Iteration N:); add task sections or pass a different plan file", path)
	}
	return nil
}

// HasUncompletedTasks reports whether the current plan still has actionable unchecked task work.
func (p *TaskPhase) HasUncompletedTasks() bool {
	path := p.locator.Path()
	if path == "" {
		return false
	}
	parsed, err := plan.ParsePlanFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false
		}
		p.log.Print("[WARN] failed to parse plan file for completion check: %v", err)
		return true
	}
	for _, t := range parsed.Tasks {
		if t.HasUncompletedActionableWork() {
			return true
		}
	}
	if len(parsed.Tasks) == 0 {
		has, err := plan.FileHasUncompletedCheckbox(path)
		if err != nil {
			return true
		}
		if has {
			return true
		}
	}
	return false
}

// NextPlanTaskPosition returns the 1-indexed first uncompleted task position, or zero when unavailable.
func (p *TaskPhase) NextPlanTaskPosition() int {
	parsed, err := plan.ParsePlanFile(p.locator.Path())
	if err != nil {
		p.log.Print("[WARN] failed to parse plan file for task position: %v", err)
		return 0
	}
	for i, t := range parsed.Tasks {
		if t.HasUncompletedActionableWork() {
			return i + 1
		}
	}
	return 0
}
