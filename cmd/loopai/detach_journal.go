package main

import (
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/umputun/ralphex/pkg/config"
	"github.com/umputun/ralphex/pkg/plan"
	"github.com/umputun/ralphex/pkg/processor"
	"github.com/umputun/ralphex/pkg/runs"
	"github.com/umputun/ralphex/pkg/status"
)

// journalHooks records a detached run's execution in its event journal. decorateLogger fills the
// request's LogDecorator and observePhase its PhaseObserver. A failed append never reaches the run:
// the first one is reported once as a warning on warnOut, the console log in a detached child.
type journalHooks struct {
	journal     *runs.Journal
	waitOnLimit time.Duration
	warnOut     io.Writer

	mu       sync.Mutex
	planFile string          // the worktree plan copy the run ticks, from attempt_start
	tasks    []runs.TaskInfo // the plan's tasks as last parsed
	warnOnce sync.Once
}

// newJournalHooks returns hooks appending to j; cfg supplies the limit-wait duration.
func newJournalHooks(j *runs.Journal, cfg *config.Config) *journalHooks {
	return &journalHooks{journal: j, waitOnLimit: cfg.WaitOnLimit, warnOut: os.Stderr}
}

// startAttempt appends an attempt_start event built by attemptStartEvent and remembers the
// worktree plan and tasks it names, which later task sections re-read and fall back to. The
// stamped event is returned. Unlike the section and phase events, a failed append is an error: the
// attempt must not run without its start in the journal.
func (h *journalHooks) startAttempt(ev runs.Event) (runs.Event, error) {
	h.mu.Lock()
	h.planFile, h.tasks = ev.WorktreePlan, ev.Tasks
	h.mu.Unlock()
	stamped, err := h.journal.Append(ev)
	if err != nil {
		return runs.Event{}, fmt.Errorf("record attempt start: %w", err)
	}
	return stamped, nil
}

// decorateLogger wraps the runner logger so its structured sections become journal events. Every
// call is forwarded to inner unchanged.
func (h *journalHooks) decorateLogger(inner processor.Logger) processor.Logger {
	return &journalLogger{Logger: inner, hooks: h}
}

// observePhase records a phase change. A limit wait carries the configured wait_on_limit, because
// the duration label never reaches the loggers below section timing.
func (h *journalHooks) observePhase(_, cur status.Phase) {
	ev := runs.Event{Type: runs.EventPhase, Phase: cur}
	if cur == status.PhaseLimitWait {
		ev.WaitSeconds = int(h.waitOnLimit / time.Second)
	}
	h.append(ev)
}

// onSection records the task and review sections; other sections leave no event.
func (h *journalHooks) onSection(section status.Section) {
	var kind string
	switch section.Type {
	case status.SectionTaskIteration:
		h.onTask(section.Iteration)
		return
	case status.SectionInternalReview:
		kind = runs.ReviewInternal
	case status.SectionExternalReviewIteration:
		kind = runs.ReviewExternal
	case status.SectionCustomIteration:
		kind = runs.ReviewCustom
	case status.SectionExternalEvaluation:
		kind = runs.ReviewEvaluation
	default:
		return
	}
	h.append(runs.Event{Type: runs.EventReview, Kind: kind, Iteration: section.Iteration, Label: section.Label})
}

// onTask records the task at plan position n, then re-reads the worktree plan for every task's
// status. A plan that cannot be read leaves the task event to the tasks known so far and records no
// statuses.
func (h *journalHooks) onTask(n int) {
	h.mu.Lock()
	planFile := h.planFile
	h.mu.Unlock()

	var parsed *plan.Plan
	if planFile != "" {
		if p, err := plan.ParsePlanFile(planFile); err == nil {
			parsed = p
		}
	}

	h.mu.Lock()
	if parsed != nil {
		h.tasks = taskInfos(parsed)
	}
	tasks := h.tasks
	h.mu.Unlock()

	ev := runs.Event{Type: runs.EventTask, N: n, M: len(tasks)}
	if n >= 1 && n <= len(tasks) {
		ev.Title = tasks[n-1].Title
	}
	h.append(ev)
	if parsed != nil {
		h.append(runs.Event{Type: runs.EventTasks, Statuses: taskStatuses(parsed)})
	}
}

// append writes ev to the journal, reporting only the first failure.
func (h *journalHooks) append(ev runs.Event) {
	if _, err := h.journal.Append(ev); err != nil {
		h.warnOnce.Do(func() {
			fmt.Fprintf(h.warnOut, "warning: run journal not updated: %v\n", err)
		})
	}
}

// journalLogger forwards every logger call and records the sections it sees.
type journalLogger struct {
	processor.Logger
	hooks *journalHooks
}

// PrintSection forwards the section, then records it.
func (l *journalLogger) PrintSection(section status.Section) {
	l.Logger.PrintSection(section)
	l.hooks.onSection(section)
}

// attemptStartEvent builds the attempt_start event of an attempt about to run in start's worktree:
// the plans, branch, worktree, and progress log from start, the console log's size at the start of
// the attempt, the mode, the worktree plan's tasks, and the stages the mode will pass through. A
// plan that cannot be parsed lists no tasks; the run reports it itself.
func attemptStartEvent(start attemptStart, mode processor.Mode, cfg *config.Config, sel externalReviewSelection,
	consoleOffset int64) runs.Event {
	ev := runs.Event{
		Type:          runs.EventAttemptStart,
		Mode:          string(mode),
		Plan:          start.MainPlanFile,
		Branch:        start.Branch,
		Worktree:      start.WorktreePath,
		WorktreePlan:  start.PlanFile,
		ProgressLog:   start.ProgressLog,
		ConsoleOffset: consoleOffset,
		Stages:        attemptStages(mode, cfg, sel),
	}
	if parsed, err := plan.ParsePlanFile(start.PlanFile); err == nil {
		ev.Tasks = taskInfos(parsed)
	}
	ev.TaskTotal = len(ev.Tasks)
	return ev
}

// attemptStages lists the pipeline stages a run in mode passes through after its tasks: none for
// tasks-only; otherwise review, then external review when the reviewer chain is not empty,
// finalize when a finalize mode is set, and the report when it is enabled.
func attemptStages(mode processor.Mode, cfg *config.Config, sel externalReviewSelection) []string {
	if mode == processor.ModeTasksOnly {
		return nil
	}
	stages := []string{runs.StageReview}
	if len(sel.Reviewers) > 0 {
		stages = append(stages, runs.StageExternalReview)
	}
	if cfg.EffectiveFinalize() != config.FinalizeNone {
		stages = append(stages, runs.StageFinalize)
	}
	if cfg.ReportEnabled {
		stages = append(stages, runs.StageReport)
	}
	return stages
}

// taskInfos lists a parsed plan's tasks by 1-based plan position, the position task sections use.
func taskInfos(p *plan.Plan) []runs.TaskInfo {
	infos := make([]runs.TaskInfo, 0, len(p.Tasks))
	for i, t := range p.Tasks {
		infos = append(infos, runs.TaskInfo{N: i + 1, Title: t.Title})
	}
	return infos
}

// taskStatuses lists every task's status by plan position. A task with no actionable work left is
// done, as the task phase treats it when choosing the next position, so a task without checkboxes
// is done too; a partially ticked task is active.
func taskStatuses(p *plan.Plan) []runs.TaskStatus {
	statuses := make([]runs.TaskStatus, 0, len(p.Tasks))
	for i, t := range p.Tasks {
		st := runs.ProgressPending
		switch {
		case !t.HasUncompletedActionableWork():
			st = runs.ProgressDone
		case t.Status == plan.TaskStatusActive:
			st = runs.ProgressActive
		}
		statuses = append(statuses, runs.TaskStatus{N: i + 1, Status: st})
	}
	return statuses
}
