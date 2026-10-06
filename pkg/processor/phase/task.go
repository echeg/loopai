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
	uncommitted    func(taskNum int)
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
	// UncommittedTask, when set, is told about a task whose AfterTask call was skipped because an
	// interrupted, aborted, or failed session ticked it but stopped before committing, so the caller
	// can have that work committed before its final review reads the branch.
	UncommittedTask func(taskNum int)
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
		uncommitted: opts.UncommittedTask,
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
			// a session canceled or failed after ticking its task may not have committed it, and the
			// next invocation starts at the following position, so no review runs but the task is reported
			p.reportAdvancedUncommittedTask(&start)
			return err
		}

		if execResult.TimedOut {
			// a session that ticked its task before hanging advanced the plan; the next iteration
			// starts at another position, so this is the only point its review can run
			if err := p.runAfterTask(ctx, &start, "", true); err != nil {
				return err
			}
			p.log.Print("%s session timed out, retrying task iteration after %s...", execName, retryBackoff)
			if err := p.policy.Sleep(ctx, retryBackoff); err != nil {
				return fmt.Errorf("interrupted: %w", err)
			}
			continue
		}

		if err := p.runAfterTask(ctx, &start, result.Signal, false); err != nil {
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
// still gets that task's review before the retry starts at the next position. On abort no review
// runs, but a ticked task left uncommitted is still reported: the next invocation starts at the
// following position and would otherwise never commit that work before its final review.
func (p *TaskPhase) resumeAfterBreak(ctx context.Context, start *taskStart) error {
	p.log.Print("session interrupted by break signal")
	p.breaks.drain()
	if p.deps.PauseHandler == nil || !p.deps.PauseHandler(ctx) {
		p.reportAdvancedUncommittedTask(start)
		return ErrUserAborted
	}
	p.breaks.drain()
	return p.runAfterTask(ctx, start, "", true)
}

// taskStart is the plan position an iteration started on, and the HEAD and uncommitted-changes
// fingerprint recorded when it first became current.
type taskStart struct {
	pos  int
	head string
	diff string
}

// trackTaskStart records HEAD when the plan position changes. Keeping it across a retried, timed-out,
// or non-advancing attempt at the same task keeps the diff base before that task's own commits.
func (p *TaskPhase) trackTaskStart(start *taskStart, pos int) {
	if p.afterTask == nil || pos == start.pos {
		return
	}
	git := NewGitState(p.deps, p.log)
	start.pos, start.head, start.diff = pos, git.headHash(), git.diffFingerprint()
}

// runAfterTask calls the after-task hook when an iteration that did not fail advanced the plan:
// the first uncompleted position moved forward, or no uncompleted task remains. That includes a
// timed-out or interrupted session that ticked its task before it stopped. An iteration that
// started without a known position, or ticked nothing, does not call it. task.txt ticks the plan
// before it commits, so an interrupted session can stop in between: when it left uncommitted
// changes the reviewer's diff against HEAD would miss that work, so the review is skipped and the
// uncommitted callback reports the task, letting the final block commit the work before reviewing it.
func (p *TaskPhase) runAfterTask(ctx context.Context, start *taskStart, signal string, interrupted bool) error {
	if signal == SignalFailed {
		return nil
	}
	pos, ok := p.advancedTask(start)
	if !ok {
		return nil
	}
	if interrupted && p.reportUncommittedTask(start, pos) {
		return nil
	}
	if err := p.afterTask(ctx, pos, start.head); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, ErrUserAborted) {
			return err
		}
		return fmt.Errorf("after task %d: %w", pos, err)
	}
	return nil
}

// advancedTask reports the plan position the iteration started on when the after-task hook is
// installed and the iteration advanced the plan past it, consuming the recorded start so the
// position is handled once.
func (p *TaskPhase) advancedTask(start *taskStart) (int, bool) {
	if p.afterTask == nil || start.pos <= 0 {
		return 0, false
	}
	pos := start.pos
	next := p.NextPlanTaskPosition()
	advanced := next > pos
	if next == 0 {
		advanced = !p.HasUncompletedTasks()
	}
	if !advanced {
		return 0, false
	}
	start.pos = -1
	return pos, true
}

// reportAdvancedUncommittedTask reports the iteration's task when it advanced the plan but left its
// changes uncommitted, for exits that run no per-task review.
func (p *TaskPhase) reportAdvancedUncommittedTask(start *taskStart) {
	if pos, ok := p.advancedTask(start); ok {
		p.reportUncommittedTask(start, pos)
	}
}

// reportUncommittedTask reports a task whose session ticked it but stopped before committing, which
// shows as an uncommitted-changes fingerprint that differs from the one recorded at the task's start.
// It returns true when it reported the task, so the caller skips its per-task review.
func (p *TaskPhase) reportUncommittedTask(start *taskStart, pos int) bool {
	if start.diff == "" {
		return false
	}
	now := NewGitState(p.deps, p.log).diffFingerprint()
	if now == "" || now == start.diff {
		return false
	}
	p.log.Print("task %d was interrupted before its changes were committed, skipping its per-task review; "+
		"the final review block commits and reviews them", pos)
	if p.uncommitted != nil {
		p.uncommitted(pos)
	}
	return true
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
