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

	// progressLogger's sink: the decorated runner logger once decorateLogger built it, the lines
	// queued for it until then, and whether the run ended without one
	logMu     sync.Mutex
	logger    processor.Logger
	queued    []string
	loggerEnd bool
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
// call is forwarded to inner unchanged. The wrapped logger also becomes progressLogger's sink, and
// the lines queued for it are written first.
func (h *journalHooks) decorateLogger(inner processor.Logger) processor.Logger {
	decorated := &journalLogger{Logger: inner, hooks: h}
	h.logMu.Lock()
	defer h.logMu.Unlock()
	h.logger = decorated
	for _, line := range h.queued {
		decorated.Print("%s", line)
	}
	h.queued = nil
	return decorated
}

// progressLogger returns a logger that writes into the run's progress log, and with it the console
// log, through the logger decorateLogger wrapped. It works before the run built that logger: lines
// are queued until decorateLogger runs, or written to warnOut once endProgressLog finds the run
// ended without one.
func (h *journalHooks) progressLogger() progressLine {
	return progressLine{hooks: h}
}

// endProgressLog is called when the attempt ends. A run that never built its logger has its queued
// lines and every later one written to warnOut instead, so none is lost.
func (h *journalHooks) endProgressLog() {
	h.logMu.Lock()
	defer h.logMu.Unlock()
	if h.logger != nil {
		return
	}
	for _, line := range h.queued {
		fmt.Fprintln(h.warnOut, line)
	}
	h.queued, h.loggerEnd = nil, true
}

// progressLine is the logger progressLogger returns.
type progressLine struct {
	hooks *journalHooks
}

// Print writes one line, or queues it until the run's logger exists.
func (p progressLine) Print(format string, args ...any) {
	h := p.hooks
	h.logMu.Lock()
	defer h.logMu.Unlock()
	switch {
	case h.logger != nil:
		h.logger.Print(format, args...)
	case h.loggerEnd:
		fmt.Fprintf(h.warnOut, format+"\n", args...)
	default:
		h.queued = append(h.queued, fmt.Sprintf(format, args...))
	}
}

// observePhase records a phase change. A limit wait carries the configured wait_on_limit, because
// the duration label never reaches the loggers below section timing.
//
// Leaving the task phase also re-reads the worktree plan for every task's status. The task phase
// prints its section when an iteration starts and returns once the plan is complete without
// printing another, so the tasks its last session ticked are recorded only here, when it hands
// over to a review block or a limit wait.
func (h *journalHooks) observePhase(old, cur status.Phase) {
	ev := runs.Event{Type: runs.EventPhase, Phase: cur}
	if cur == status.PhaseLimitWait {
		ev.WaitSeconds = int(h.waitOnLimit / time.Second)
	}
	h.append(ev)
	if old == status.PhaseTask && cur != status.PhaseTask {
		parsed, _ := h.parsePlan()
		h.appendStatuses(parsed)
	}
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
//
// The task phase numbers its section by the iteration instead when it finds no task left to do, so
// n names a plan position only when the fresh plan still has work there; otherwise the event
// carries no title rather than an unrelated task's.
func (h *journalHooks) onTask(n int) {
	parsed, tasks := h.parsePlan()
	ev := runs.Event{Type: runs.EventTask, N: n, M: len(tasks)}
	if n >= 1 && n <= len(tasks) && (parsed == nil || parsed.Tasks[n-1].HasUncompletedActionableWork()) {
		ev.Title = tasks[n-1].Title
	}
	h.append(ev)
	h.appendStatuses(parsed)
}

// recordStatuses re-reads the worktree plan and records every task's status. A run that ends
// without success calls it before its finish event, since its last session may have ticked tasks
// after the last section or phase change recorded them.
func (h *journalHooks) recordStatuses() {
	parsed, _ := h.parsePlan()
	h.appendStatuses(parsed)
}

// parsePlan re-reads the worktree plan and returns it with the tasks known so far, which a
// successful read refreshes. The plan is nil before an attempt names it and when it cannot be read.
func (h *journalHooks) parsePlan() (*plan.Plan, []runs.TaskInfo) {
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
	defer h.mu.Unlock()
	if parsed != nil {
		h.tasks = taskInfos(parsed)
	}
	return parsed, h.tasks
}

// appendStatuses records the status of every task of a parsed plan; a nil plan records nothing.
func (h *journalHooks) appendStatuses(parsed *plan.Plan) {
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

// finalizeEvent is the finalize event of a run's close-out result f.
func finalizeEvent(f runs.Finalize) runs.Event {
	return runs.Event{Type: runs.EventFinalize, Mode: f.Mode, StatusNote: f.StatusNote, PRURL: f.PRURL,
		Merged: f.Merged, Incomplete: f.Incomplete}
}

// reportEvent is the report event of a completion report: its bounded summary and its risk level.
func reportEvent(report string) runs.Event {
	return runs.Event{Type: runs.EventReport, Summary: reportSummary(report), Risk: reportRiskLevel(report)}
}

// finishEvent is the event that closes an attempt with st, reason, and exit code.
func finishEvent(st runs.Status, reason string, exitCode int) runs.Event {
	return runs.Event{Type: runs.EventFinish, Status: st, Reason: reason, ExitCode: exitCode}
}

// reportSummary returns the body of a report's first Summary section, cut to the 4 KiB an outcome
// keeps; a report without one has no summary.
func reportSummary(report string) string {
	for _, section := range splitReportSections(report) {
		if section.heading == "Summary" {
			return runs.CapReportSummary(section.body)
		}
	}
	return ""
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
