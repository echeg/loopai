package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/ralphex/pkg/config"
	"github.com/umputun/ralphex/pkg/processor"
	processormocks "github.com/umputun/ralphex/pkg/processor/mocks"
	"github.com/umputun/ralphex/pkg/runs"
	"github.com/umputun/ralphex/pkg/status"
)

const journalTestPlan = "# Feature\n\n### Task 1: First step\n- [x] done\n\n### Task 2: Second step\n- [x] half\n- [ ] rest\n\n" +
	"### Task 3: Third step\n- [ ] todo\n"

func TestJournalLoggerForwardsEveryCall(t *testing.T) {
	inner := &processormocks.LoggerMock{
		PrintFunc:          func(string, ...any) {},
		PrintRawFunc:       func(string, ...any) {},
		PrintSectionFunc:   func(status.Section) {},
		PrintAlignedFunc:   func(string) {},
		LogQuestionFunc:    func(string, []string) {},
		LogAnswerFunc:      func(string) {},
		LogDraftReviewFunc: func(string, string) {},
		PathFunc:           func() string { return "/progress.txt" },
	}
	hooks, _, _ := newTestJournalHooks(t, &config.Config{})
	log := hooks.decorateLogger(inner)

	log.Print("print %d", 1)
	log.PrintRaw("raw %s", "x")
	sections := []status.Section{
		status.NewTaskIterationSection(1), status.NewInternalReviewSection(0, ""),
		status.NewExternalReviewIterationSection("codex", 1), status.NewCustomIterationSection(2),
		status.NewExternalEvaluationSection("claude", "codex"), status.NewGenericSection("generic"),
		status.NewPlanIterationSection(1),
	}
	for _, section := range sections {
		log.PrintSection(section)
	}
	log.PrintAligned("aligned")
	log.LogQuestion("question", []string{"a", "b"})
	log.LogAnswer("answer")
	log.LogDraftReview("accept", "feedback")

	assert.Equal(t, "/progress.txt", log.Path())
	require.Len(t, inner.PrintCalls(), 1)
	assert.Equal(t, "print %d", inner.PrintCalls()[0].Format)
	assert.Equal(t, []any{1}, inner.PrintCalls()[0].Args)
	require.Len(t, inner.PrintRawCalls(), 1)
	assert.Equal(t, "raw %s", inner.PrintRawCalls()[0].Format)
	assert.Equal(t, []any{"x"}, inner.PrintRawCalls()[0].Args)
	require.Len(t, inner.PrintSectionCalls(), len(sections))
	for i, section := range sections {
		assert.Equal(t, section, inner.PrintSectionCalls()[i].Section, "sections reach the wrapped logger unchanged")
	}
	require.Len(t, inner.PrintAlignedCalls(), 1)
	assert.Equal(t, "aligned", inner.PrintAlignedCalls()[0].Text)
	require.Len(t, inner.LogQuestionCalls(), 1)
	assert.Equal(t, "question", inner.LogQuestionCalls()[0].Question)
	assert.Equal(t, []string{"a", "b"}, inner.LogQuestionCalls()[0].Options)
	require.Len(t, inner.LogAnswerCalls(), 1)
	assert.Equal(t, "answer", inner.LogAnswerCalls()[0].Answer)
	require.Len(t, inner.LogDraftReviewCalls(), 1)
	assert.Equal(t, "accept", inner.LogDraftReviewCalls()[0].Action)
	assert.Equal(t, "feedback", inner.LogDraftReviewCalls()[0].Feedback)
	assert.Len(t, inner.PathCalls(), 1)
}

func TestJournalLoggerSectionEvents(t *testing.T) {
	tests := []struct {
		name    string
		section status.Section
		want    []runs.Event
	}{
		{name: "task iteration", section: status.NewTaskIterationSection(2), want: []runs.Event{
			{Type: runs.EventTask, N: 2, M: 3, Title: "Second step"},
			{Type: runs.EventTasks, Statuses: []runs.TaskStatus{{N: 1, Status: runs.ProgressDone},
				{N: 2, Status: runs.ProgressActive}, {N: 3, Status: runs.ProgressPending}}},
		}},
		{name: "internal review", section: status.NewInternalReviewSection(0, ": critical"), want: []runs.Event{
			{Type: runs.EventReview, Kind: runs.ReviewInternal, Iteration: 0, Label: "review 0: critical"},
		}},
		{name: "claude review", section: status.NewClaudeReviewSection(3, ""), want: []runs.Event{
			{Type: runs.EventReview, Kind: runs.ReviewInternal, Iteration: 3, Label: "claude review 3"},
		}},
		{name: "external review iteration", section: status.NewExternalReviewIterationSection("codex", 2),
			want: []runs.Event{{Type: runs.EventReview, Kind: runs.ReviewExternal, Iteration: 2,
				Label: "codex external review iteration 2"}}},
		{name: "custom iteration", section: status.NewCustomIterationSection(1), want: []runs.Event{
			{Type: runs.EventReview, Kind: runs.ReviewCustom, Iteration: 1, Label: "custom review iteration 1"},
		}},
		{name: "external evaluation", section: status.NewExternalEvaluationSection("claude", "codex"),
			want: []runs.Event{{Type: runs.EventReview, Kind: runs.ReviewEvaluation,
				Label: "claude evaluating codex findings"}}},
		{name: "generic section", section: status.NewGenericSection("all done"), want: []runs.Event{}},
		{name: "plan iteration", section: status.NewPlanIterationSection(1), want: []runs.Event{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hooks, dir, warn := newTestJournalHooks(t, &config.Config{})
			startTestAttempt(t, hooks, writeJournalTestPlan(t))
			inner := &runnerLoggerRecorder{}
			hooks.decorateLogger(inner).PrintSection(tc.section)

			assert.Equal(t, []string{"section: " + tc.section.Label}, inner.calls)
			assert.Equal(t, tc.want, journalEventsAfterStart(t, dir))
			assert.Empty(t, warn.String())
		})
	}
}

func TestJournalLoggerTaskFollowsPlanEdits(t *testing.T) {
	hooks, dir, _ := newTestJournalHooks(t, &config.Config{})
	planFile := writeJournalTestPlan(t)
	startTestAttempt(t, hooks, planFile)
	require.NoError(t, os.WriteFile(planFile, []byte(journalTestPlan+"\n### Task 4: Added step\n- [ ] new\n"), 0o600))

	hooks.decorateLogger(&runnerLoggerRecorder{}).PrintSection(status.NewTaskIterationSection(4))

	events := journalEventsAfterStart(t, dir)
	require.Len(t, events, 2)
	assert.Equal(t, runs.Event{Type: runs.EventTask, N: 4, M: 4, Title: "Added step"}, events[0],
		"a task added to the plan during the run is counted")
	assert.Len(t, events[1].Statuses, 4)
}

func TestJournalLoggerMissingPlan(t *testing.T) {
	hooks, dir, warn := newTestJournalHooks(t, &config.Config{})
	planFile := writeJournalTestPlan(t)
	startTestAttempt(t, hooks, planFile)
	require.NoError(t, os.Remove(planFile))

	inner := &runnerLoggerRecorder{}
	hooks.decorateLogger(inner).PrintSection(status.NewTaskIterationSection(3))

	assert.Equal(t, []string{"section: task iteration 3"}, inner.calls, "the run's logger is unaffected")
	assert.Equal(t, []runs.Event{{Type: runs.EventTask, N: 3, M: 3, Title: "Third step"}}, journalEventsAfterStart(t, dir),
		"the task is recorded from the tasks known at attempt start, without statuses")
	assert.Empty(t, warn.String(), "an unreadable plan is not reported to the run")
}

func TestJournalLoggerTaskBeforeAttempt(t *testing.T) {
	hooks, dir, _ := newTestJournalHooks(t, &config.Config{})
	hooks.decorateLogger(&runnerLoggerRecorder{}).PrintSection(status.NewTaskIterationSection(1))

	events, err := runs.ReadFrom(dir, 0)
	require.NoError(t, err)
	require.Len(t, events, 1, "without an attempt there is no plan to re-read")
	assert.Equal(t, runs.EventTask, events[0].Type)
	assert.Equal(t, 1, events[0].N)
	assert.Zero(t, events[0].M)
	assert.Empty(t, events[0].Title)
}

func TestJournalHooksObservePhase(t *testing.T) {
	hooks, dir, _ := newTestJournalHooks(t, &config.Config{WaitOnLimit: 90 * time.Minute})
	hooks.observePhase("", status.PhaseTask)
	hooks.observePhase(status.PhaseTask, status.PhaseLimitWait)
	hooks.observePhase(status.PhaseLimitWait, status.PhaseTask)

	events, err := runs.ReadFrom(dir, 0)
	require.NoError(t, err)
	require.Len(t, events, 3)
	assert.Equal(t, status.PhaseTask, events[0].Phase)
	assert.Zero(t, events[0].WaitSeconds)
	assert.Equal(t, runs.EventPhase, events[1].Type)
	assert.Equal(t, status.PhaseLimitWait, events[1].Phase)
	assert.Equal(t, 90*60, events[1].WaitSeconds, "a limit wait records wait_on_limit")
	assert.Zero(t, events[2].WaitSeconds)
}

func TestJournalHooksPhaseLeavingTasksRecordsStatuses(t *testing.T) {
	hooks, dir, warn := newTestJournalHooks(t, &config.Config{WaitOnLimit: time.Minute})
	planFile := writeJournalTestPlan(t)
	startTestAttempt(t, hooks, planFile)

	hooks.observePhase("", status.PhaseTask)
	hooks.observePhase(status.PhaseTask, status.PhaseLimitWait)
	hooks.observePhase(status.PhaseLimitWait, status.PhaseTask)
	hooks.observePhase(status.PhaseTask, status.PhaseExternalReview)
	hooks.observePhase(status.PhaseExternalReview, status.PhaseExternalEval)
	hooks.observePhase(status.PhaseExternalEval, status.PhaseTask)
	hooks.observePhase(status.PhaseTask, status.PhaseReview)
	require.NoError(t, os.Remove(planFile))
	hooks.observePhase(status.PhaseReview, status.PhaseTask)
	hooks.observePhase(status.PhaseTask, status.PhaseReview)

	statuses := []runs.TaskStatus{{N: 1, Status: runs.ProgressDone}, {N: 2, Status: runs.ProgressActive},
		{N: 3, Status: runs.ProgressPending}}
	got := journalEventsAfterStart(t, dir)
	want := []runs.Event{
		{Type: runs.EventPhase, Phase: status.PhaseTask},
		{Type: runs.EventPhase, Phase: status.PhaseLimitWait, WaitSeconds: 60},
		{Type: runs.EventTasks, Statuses: statuses},
		{Type: runs.EventPhase, Phase: status.PhaseTask},
		{Type: runs.EventPhase, Phase: status.PhaseExternalReview},
		{Type: runs.EventTasks, Statuses: statuses},
		{Type: runs.EventPhase, Phase: status.PhaseExternalEval},
		{Type: runs.EventPhase, Phase: status.PhaseTask},
		{Type: runs.EventPhase, Phase: status.PhaseReview},
		{Type: runs.EventTasks, Statuses: statuses},
		{Type: runs.EventPhase, Phase: status.PhaseTask},
		{Type: runs.EventPhase, Phase: status.PhaseReview},
	}
	assert.Equal(t, want, got, "only leaving the task phase re-reads the plan; an unreadable plan records nothing")
	assert.Empty(t, warn.String())
}

func TestJournalHooksFoldTaskStatuses(t *testing.T) {
	const planText = "# Feature\n\n### Task 1: First\n- [ ] a\n\n### Task 2: Second\n- [ ] b\n\n### Task 3: Third\n- [ ] c\n"
	// tick marks the checkbox of each named item done, as a task session does
	tick := func(t *testing.T, planFile string, items ...string) {
		t.Helper()
		data, err := os.ReadFile(planFile) //nolint:gosec // test-owned temp file
		require.NoError(t, err)
		text := string(data)
		for _, item := range items {
			text = strings.Replace(text, "- [ ] "+item, "- [x] "+item, 1)
		}
		require.NoError(t, os.WriteFile(planFile, []byte(text), 0o600))
	}
	// setup starts an attempt of mode over a fresh plan and returns the hooks, the run directory,
	// the plan file, and the decorated logger
	setup := func(t *testing.T, mode processor.Mode) (*journalHooks, string, string, processor.Logger) {
		t.Helper()
		hooks, dir, _ := newTestJournalHooks(t, &config.Config{})
		planFile := filepath.Join(t.TempDir(), "plan.md")
		require.NoError(t, os.WriteFile(planFile, []byte(planText), 0o600))
		ev := attemptStartEvent(attemptStart{PlanFile: planFile}, mode, &config.Config{}, externalReviewSelection{}, 0)
		_, err := hooks.startAttempt(ev)
		require.NoError(t, err)
		log := hooks.decorateLogger(&runnerLoggerRecorder{})
		return hooks, dir, planFile, log
	}
	finish := func(t *testing.T, hooks *journalHooks, st runs.Status) {
		t.Helper()
		_, err := hooks.journal.Append(runs.Event{Type: runs.EventFinish, Status: st})
		require.NoError(t, err)
	}

	t.Run("full run", func(t *testing.T) {
		hooks, dir, planFile, log := setup(t, processor.ModeFull)
		hooks.observePhase("", status.PhaseTask)
		log.PrintSection(status.NewTaskIterationSection(1))
		s := foldTestJournal(t, dir)
		assert.Equal(t, []string{"active", "pending", "pending"}, snapshotTaskStates(s), "the running task is active")
		assert.Equal(t, []string{"First", "Second", "Third"}, snapshotTaskTitles(s))

		tick(t, planFile, "a")
		log.PrintSection(status.NewTaskIterationSection(2))
		assert.Equal(t, []string{"done", "active", "pending"}, snapshotTaskStates(foldTestJournal(t, dir)))

		// the last session ticks the rest; the task phase ends without another section
		tick(t, planFile, "b", "c")
		hooks.observePhase(status.PhaseTask, status.PhaseReview)
		events, err := runs.ReadFrom(dir, 0)
		require.NoError(t, err)
		last := events[len(events)-1]
		assert.Equal(t, runs.EventTasks, last.Type, "leaving the task phase records the plan's statuses")
		assert.Equal(t, []runs.TaskStatus{{N: 1, Status: runs.ProgressDone}, {N: 2, Status: runs.ProgressDone},
			{N: 3, Status: runs.ProgressDone}}, last.Statuses)
		s = foldTestJournal(t, dir)
		assert.Equal(t, []string{"done", "done", "done"}, snapshotTaskStates(s))
		assert.Equal(t, runs.ProgressActive, s.Stages[0].Status)

		finish(t, hooks, runs.StatusSucceeded)
		s = foldTestJournal(t, dir)
		assert.Equal(t, []string{"done", "done", "done"}, snapshotTaskStates(s))
		assert.Equal(t, runs.ProgressDone, s.Stages[0].Status)
	})

	t.Run("per-task review block", func(t *testing.T) {
		hooks, dir, planFile, log := setup(t, processor.ModeFull)
		hooks.observePhase("", status.PhaseTask)
		log.PrintSection(status.NewTaskIterationSection(1))
		tick(t, planFile, "a")
		hooks.observePhase(status.PhaseTask, status.PhaseExternalReview)
		s := foldTestJournal(t, dir)
		assert.Equal(t, []string{"done", "pending", "pending"}, snapshotTaskStates(s))
		assert.Equal(t, runs.ProgressPending, s.Stages[0].Status, "a per-task block leaves the stages alone")
		hooks.observePhase(status.PhaseExternalReview, status.PhaseTask)
		log.PrintSection(status.NewTaskIterationSection(2))
		assert.Equal(t, []string{"done", "active", "pending"}, snapshotTaskStates(foldTestJournal(t, dir)))
	})

	t.Run("tasks-only success", func(t *testing.T) {
		hooks, dir, planFile, log := setup(t, processor.ModeTasksOnly)
		hooks.observePhase("", status.PhaseTask)
		tick(t, planFile, "a")
		log.PrintSection(status.NewTaskIterationSection(2))
		tick(t, planFile, "b")
		log.PrintSection(status.NewTaskIterationSection(3))
		tick(t, planFile, "c")
		// tasks-only mode changes no phase after its last task
		assert.Equal(t, []string{"done", "done", "active"}, snapshotTaskStates(foldTestJournal(t, dir)))
		finish(t, hooks, runs.StatusSucceeded)
		assert.Equal(t, []string{"done", "done", "done"}, snapshotTaskStates(foldTestJournal(t, dir)))
	})

	t.Run("failed task", func(t *testing.T) {
		hooks, dir, planFile, log := setup(t, processor.ModeFull)
		hooks.observePhase("", status.PhaseTask)
		tick(t, planFile, "a")
		log.PrintSection(status.NewTaskIterationSection(2))
		finish(t, hooks, runs.StatusFailed)
		assert.Equal(t, []string{"done", "pending", "pending"}, snapshotTaskStates(foldTestJournal(t, dir)),
			"the task the run stopped in drops back to pending")
	})
}

func TestJournalHooksStartAttempt(t *testing.T) {
	hooks, dir, _ := newTestJournalHooks(t, &config.Config{})
	stamped, err := hooks.startAttempt(runs.Event{Type: runs.EventAttemptStart, Mode: "full", WorktreePlan: "/wt/plan.md",
		Tasks: []runs.TaskInfo{{N: 1, Title: "one"}}, TaskTotal: 1})
	require.NoError(t, err)
	assert.Equal(t, 1, stamped.Seq)
	assert.Equal(t, 3, stamped.Attempt)

	events, err := runs.ReadFrom(dir, 0)
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, runs.EventAttemptStart, events[0].Type)
	assert.Equal(t, "/wt/plan.md", events[0].WorktreePlan)

	require.NoError(t, hooks.journal.Close())
	_, err = hooks.startAttempt(runs.Event{Type: runs.EventAttemptStart})
	require.ErrorContains(t, err, "record attempt start")
}

func TestJournalHooksAppendFailureWarnsOnce(t *testing.T) {
	hooks, _, warn := newTestJournalHooks(t, &config.Config{})
	require.NoError(t, hooks.journal.Close())

	inner := &runnerLoggerRecorder{}
	log := hooks.decorateLogger(inner)
	log.PrintSection(status.NewInternalReviewSection(1, ""))
	hooks.observePhase(status.PhaseTask, status.PhaseReview)
	log.PrintSection(status.NewInternalReviewSection(2, ""))

	assert.Equal(t, []string{"section: review 1", "section: review 2"}, inner.calls, "the run continues")
	assert.Equal(t, 1, strings.Count(warn.String(), "warning: run journal not updated"), warn.String())
}

func TestAttemptStartEvent(t *testing.T) {
	planFile := writeJournalTestPlan(t)
	start := attemptStart{PlanFile: planFile, MainPlanFile: "/repo/docs/plans/feature.md", Branch: "feature",
		WorktreePath: "/repo/.loopai/worktrees/feature", ProgressLog: "/repo/.loopai/progress/progress-feature.txt"}
	chain := externalReviewSelection{Reviewers: []resolvedReviewer{{Provider: "codex"}}}

	ev := attemptStartEvent(start, processor.ModeFull, &config.Config{}, chain, 4096)
	assert.Equal(t, runs.EventAttemptStart, ev.Type)
	assert.Equal(t, "full", ev.Mode)
	assert.Equal(t, "/repo/docs/plans/feature.md", ev.Plan)
	assert.Equal(t, "feature", ev.Branch)
	assert.Equal(t, "/repo/.loopai/worktrees/feature", ev.Worktree)
	assert.Equal(t, planFile, ev.WorktreePlan)
	assert.Equal(t, "/repo/.loopai/progress/progress-feature.txt", ev.ProgressLog)
	assert.Equal(t, int64(4096), ev.ConsoleOffset)
	assert.Equal(t, []runs.TaskInfo{{N: 1, Title: "First step"}, {N: 2, Title: "Second step"}, {N: 3, Title: "Third step"}},
		ev.Tasks)
	assert.Equal(t, 3, ev.TaskTotal)

	tests := []struct {
		name string
		mode processor.Mode
		cfg  *config.Config
		sel  externalReviewSelection
		want []string
	}{
		{name: "full mode with a chain, finalize, and report", mode: processor.ModeFull,
			cfg: &config.Config{Finalize: config.FinalizeSync, ReportEnabled: true}, sel: chain,
			want: []string{runs.StageReview, runs.StageExternalReview, runs.StageFinalize, runs.StageReport}},
		{name: "empty chain", mode: processor.ModeFull,
			cfg:  &config.Config{Finalize: config.FinalizePR, ReportEnabled: true},
			want: []string{runs.StageReview, runs.StageFinalize, runs.StageReport}},
		{name: "finalize none and no report", mode: processor.ModeFull,
			cfg: &config.Config{Finalize: config.FinalizeNone}, sel: chain,
			want: []string{runs.StageReview, runs.StageExternalReview}},
		{name: "tasks-only", mode: processor.ModeTasksOnly,
			cfg: &config.Config{Finalize: config.FinalizeMerge, ReportEnabled: true}, sel: chain},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ev := attemptStartEvent(start, tc.mode, tc.cfg, tc.sel, 0)
			assert.Equal(t, tc.want, ev.Stages)
			assert.Equal(t, string(tc.mode), ev.Mode)
		})
	}
}

func TestAttemptStartEventUnreadablePlan(t *testing.T) {
	start := attemptStart{PlanFile: filepath.Join(t.TempDir(), "missing.md"), Branch: "feature"}
	ev := attemptStartEvent(start, processor.ModeTasksOnly, &config.Config{}, externalReviewSelection{}, 12)
	assert.Empty(t, ev.Tasks)
	assert.Zero(t, ev.TaskTotal)
	assert.Equal(t, "feature", ev.Branch)
	assert.Equal(t, int64(12), ev.ConsoleOffset)
}

// newTestJournalHooks returns hooks on a fresh journal of attempt 3 in a temp run directory, the
// directory, and the buffer that receives their warnings.
func newTestJournalHooks(t *testing.T, cfg *config.Config) (*journalHooks, string, *bytes.Buffer) {
	t.Helper()
	dir := t.TempDir()
	j, err := runs.OpenJournal(dir, 3)
	require.NoError(t, err)
	t.Cleanup(func() { _ = j.Close() })
	hooks := newJournalHooks(j, cfg)
	warn := &bytes.Buffer{}
	hooks.warnOut = warn
	return hooks, dir, warn
}

// writeJournalTestPlan writes journalTestPlan into a temp directory and returns its path.
func writeJournalTestPlan(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "plan.md")
	require.NoError(t, os.WriteFile(path, []byte(journalTestPlan), 0o600))
	return path
}

// startTestAttempt records an attempt over the worktree plan planFile.
func startTestAttempt(t *testing.T, hooks *journalHooks, planFile string) {
	t.Helper()
	ev := attemptStartEvent(attemptStart{PlanFile: planFile}, processor.ModeFull, &config.Config{},
		externalReviewSelection{}, 0)
	_, err := hooks.startAttempt(ev)
	require.NoError(t, err)
}

// journalEventsAfterStart returns the events after the attempt_start event, with the stamped
// header fields cleared so tests compare payloads only.
func journalEventsAfterStart(t *testing.T, dir string) []runs.Event {
	t.Helper()
	events, err := runs.ReadFrom(dir, 0)
	require.NoError(t, err)
	require.NotEmpty(t, events)
	require.Equal(t, runs.EventAttemptStart, events[0].Type)
	result := make([]runs.Event, 0, len(events)-1)
	for _, ev := range events[1:] {
		ev.Seq, ev.Time, ev.Attempt = 0, time.Time{}, 0
		result = append(result, ev)
	}
	return result
}

// foldTestJournal folds every event of the journal in dir.
func foldTestJournal(t *testing.T, dir string) runs.Snapshot {
	t.Helper()
	events, err := runs.ReadFrom(dir, 0)
	require.NoError(t, err)
	return runs.Fold(events)
}

// snapshotTaskStates lists the status of each task of s.
func snapshotTaskStates(s runs.Snapshot) []string {
	states := make([]string, 0, len(s.Tasks))
	for _, task := range s.Tasks {
		states = append(states, task.Status)
	}
	return states
}

// snapshotTaskTitles lists the title of each task of s.
func snapshotTaskTitles(s runs.Snapshot) []string {
	titles := make([]string, 0, len(s.Tasks))
	for _, task := range s.Tasks {
		titles = append(titles, task.Title)
	}
	return titles
}
