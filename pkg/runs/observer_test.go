//go:build !windows

package runs

import (
	"context"
	"io/fs"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/ralphex/pkg/status"
)

// followTimeout bounds a test's wait for a follower, which polls every 250 ms.
const followTimeout = 10 * time.Second

func TestFollow(t *testing.T) {
	root := t.TempDir()
	dir := writeTestRun(t, root, "feature-1", "docs/plans/feature.md", "feature", 1, time.Now())
	release, err := AcquireBounded(filepath.Join(dir, LockFile), time.Second)
	require.NoError(t, err)
	t.Cleanup(func() { _ = release() })
	j := openTestJournal(t, dir, 1)
	appendTestEvents(t, j, Event{Type: EventAttemptStart, Mode: "full", TaskTotal: 2},
		Event{Type: EventPhase, Phase: status.PhaseTask}, Event{Type: EventTask, N: 1, M: 2, Title: "one"})

	got := &eventRecorder{}
	type result struct {
		status Status
		err    error
	}
	done := make(chan result, 1)
	go func() {
		st, err := Follow(t.Context(), root, dir, 2, got.add)
		done <- result{st, err}
	}()

	require.Eventually(t, func() bool { return len(got.seqs()) == 1 }, followTimeout, 10*time.Millisecond)
	assert.Equal(t, []int{3}, got.seqs(), "only events after the starting seq are delivered")

	appendTestEvents(t, j, Event{Type: EventTask, N: 2, M: 2, Title: "two"})
	require.Eventually(t, func() bool { return len(got.seqs()) == 2 }, followTimeout, 10*time.Millisecond,
		"appends are followed")

	// the run writes its last events and outcome, then releases its lifetime lock
	appendTestEvents(t, j, Event{Type: EventFinish, Status: StatusSucceeded})
	writeTestOutcome(t, dir, 1, StatusSucceeded)
	select {
	case res := <-done:
		t.Fatalf("follow returned while the lock was held: %+v", res)
	case <-time.After(3 * followPoll):
	}
	require.NoError(t, release())

	select {
	case res := <-done:
		require.NoError(t, res.err)
		assert.Equal(t, StatusSucceeded, res.status)
	case <-time.After(followTimeout):
		t.Fatal("follow did not return after the lock was released")
	}
	assert.Equal(t, []int{3, 4, 5}, got.seqs(), "the journal is drained before follow returns")
}

func TestFollowDrainsEventsWrittenBeforeRelease(t *testing.T) {
	root := t.TempDir()
	dir := writeTestRun(t, root, "feature-1", "docs/plans/feature.md", "feature", 1, time.Now())
	j := openTestJournal(t, dir, 1)
	appendTestEvents(t, j, Event{Type: EventAttemptStart}, Event{Type: EventFinish, Status: StatusFailed, ExitCode: 1})
	writeTestOutcome(t, dir, 1, StatusFailed)

	got := &eventRecorder{}
	st, err := Follow(t.Context(), root, dir, 0, got.add)
	require.NoError(t, err)
	assert.Equal(t, StatusFailed, st)
	assert.Equal(t, []int{1, 2}, got.seqs())
}

func TestFollowCanceled(t *testing.T) {
	root := t.TempDir()
	dir := writeTestRun(t, root, "feature-1", "docs/plans/feature.md", "feature", 1, time.Now())
	holdTestLock(t, filepath.Join(dir, LockFile))

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := Follow(ctx, root, dir, 0, func(Event) {})
		done <- err
	}()
	time.Sleep(2 * followPoll)
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(followTimeout):
		t.Fatal("follow did not return on cancel")
	}
}

func TestFollowCrashedRun(t *testing.T) {
	root := t.TempDir()
	dir := writeTestRun(t, root, "feature-1", "docs/plans/feature.md", "feature", 1, time.Now())
	j := openTestJournal(t, dir, 1)
	appendTestEvents(t, j, Event{Type: EventAttemptStart}, Event{Type: EventPhase, Phase: status.PhaseTask})

	// the lock is free and there is no outcome: the process died mid-run
	got := &eventRecorder{}
	st, err := Follow(t.Context(), root, dir, 0, got.add)
	require.NoError(t, err)
	assert.Equal(t, StatusCrashed, st)
	assert.Equal(t, []int{1, 2}, got.seqs())
}

func TestFollowErrors(t *testing.T) {
	root := t.TempDir()
	_, err := Follow(t.Context(), root, Dir(root, "feature-1"), 0, func(Event) {})
	require.ErrorIs(t, err, fs.ErrNotExist)
	require.ErrorContains(t, err, "follow run")
}

func TestFollowUntilFinish(t *testing.T) {
	root := t.TempDir()
	dir := writeTestRun(t, root, "feature-1", "docs/plans/feature.md", "feature", 2, time.Now())
	// the run process holds its own lifetime lock and follows its journal
	holdTestLock(t, filepath.Join(dir, LockFile))
	first := openTestJournal(t, dir, 1)
	appendTestEvents(t, first, Event{Type: EventAttemptStart}, Event{Type: EventFinish, Status: StatusFailed, ExitCode: 1})
	require.NoError(t, first.Close())
	j := openTestJournal(t, dir, 2)
	appendTestEvents(t, j, Event{Type: EventAttemptStart})

	got := &eventRecorder{}
	type result struct {
		finish Event
		err    error
	}
	done := make(chan result, 1)
	go func() {
		finish, err := FollowUntilFinish(t.Context(), dir, 2, 0, got.add)
		done <- result{finish, err}
	}()

	require.Eventually(t, func() bool { return len(got.seqs()) == 3 }, followTimeout, 10*time.Millisecond,
		"an earlier attempt's finish does not end the follow")
	appendTestEvents(t, j, Event{Type: EventPhase, Phase: status.PhaseReport},
		Event{Type: EventFinish, Status: StatusSucceeded}, Event{Type: EventPhase, Phase: status.PhaseTask})

	select {
	case res := <-done:
		require.NoError(t, res.err)
		assert.Equal(t, EventFinish, res.finish.Type)
		assert.Equal(t, 2, res.finish.Attempt)
		assert.Equal(t, StatusSucceeded, res.finish.Status)
		assert.Equal(t, 5, res.finish.Seq)
	case <-time.After(followTimeout):
		t.Fatal("follow did not return at the attempt's finish event while the lock was held")
	}
	assert.Equal(t, []int{1, 2, 3, 4, 5}, got.seqs(), "nothing after the finish event is delivered")
}

func TestFollowUntilFinishFromMidJournal(t *testing.T) {
	dir := t.TempDir()
	j := openTestJournal(t, dir, 1)
	appendTestEvents(t, j, Event{Type: EventAttemptStart}, Event{Type: EventPhase, Phase: status.PhaseTask},
		Event{Type: EventFinish, Status: StatusStopped, ExitCode: 2})

	got := &eventRecorder{}
	finish, err := FollowUntilFinish(t.Context(), dir, 1, 1, got.add)
	require.NoError(t, err)
	assert.Equal(t, StatusStopped, finish.Status)
	assert.Equal(t, []int{2, 3}, got.seqs())
}

func TestFollowUntilFinishCanceled(t *testing.T) {
	dir := t.TempDir()
	j := openTestJournal(t, dir, 1)
	appendTestEvents(t, j, Event{Type: EventAttemptStart})

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := FollowUntilFinish(ctx, dir, 1, 0, func(Event) {})
		done <- err
	}()
	time.Sleep(2 * followPoll)
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(followTimeout):
		t.Fatal("follow did not return on cancel")
	}
}

func TestSnapshotTasksAndStages(t *testing.T) {
	// the events the journal hooks write: a tasks event after every task section and after the task
	// phase hands over to a limit wait or a review block
	stages := []string{StageReview, StageExternalReview, StageFinalize, StageReport}
	start := Event{Type: EventAttemptStart, Attempt: 1, TaskTotal: 2, Stages: stages,
		Tasks: []TaskInfo{{N: 1, Title: "one"}, {N: 2, Title: "two"}}}
	pending := []string{"pending", "pending", "pending", "pending"}
	steps := []struct {
		name       string
		ev         Event
		task, of   int
		title      string
		taskStates []string
		stages     []string
	}{
		{name: "attempt start", ev: start, of: 2, taskStates: []string{"pending", "pending"}, stages: pending},
		{name: "task phase", ev: phaseEvent(status.PhaseTask), of: 2, taskStates: []string{"pending", "pending"},
			stages: pending},
		{name: "task 1", ev: Event{Type: EventTask, N: 1, M: 2, Title: "one"}, task: 1, of: 2, title: "one",
			taskStates: []string{"active", "pending"}, stages: pending},
		{name: "task 1 statuses", ev: tasksEvent("pending", "pending"), task: 1, of: 2, title: "one",
			taskStates: []string{"active", "pending"}, stages: pending},
		{name: "limit wait keeps the task active", ev: phaseEvent(status.PhaseLimitWait), task: 1, of: 2, title: "one",
			taskStates: []string{"active", "pending"}, stages: pending},
		{name: "statuses at the limit wait", ev: tasksEvent("active", "pending"), task: 1, of: 2, title: "one",
			taskStates: []string{"active", "pending"}, stages: pending},
		{name: "back from the wait", ev: phaseEvent(status.PhaseTask), task: 1, of: 2, title: "one",
			taskStates: []string{"active", "pending"}, stages: pending},
		{name: "per-task external review", ev: phaseEvent(status.PhaseExternalReview), task: 1, of: 2, title: "one",
			taskStates: []string{"active", "pending"}, stages: pending},
		{name: "task 1 ticked", ev: tasksEvent("done", "pending"), task: 1, of: 2, title: "one",
			taskStates: []string{"done", "pending"}, stages: pending},
		{name: "per-task evaluation", ev: phaseEvent(status.PhaseExternalEval), task: 1, of: 2, title: "one",
			taskStates: []string{"done", "pending"}, stages: pending},
		{name: "back to tasks", ev: phaseEvent(status.PhaseTask), task: 1, of: 2, title: "one",
			taskStates: []string{"done", "pending"}, stages: pending},
		{name: "task 2 of a grown plan", ev: Event{Type: EventTask, N: 2, M: 3, Title: "two"}, task: 2, of: 3,
			title: "two", taskStates: []string{"done", "active"}, stages: pending},
		{name: "statuses of the grown plan", ev: tasksEvent("done", "pending", "pending"), task: 2, of: 3, title: "two",
			taskStates: []string{"done", "active", "pending"}, stages: pending},
		{name: "review completes every task", ev: phaseEvent(status.PhaseReview), task: 2, of: 3, title: "two",
			taskStates: []string{"done", "done", "done"}, stages: []string{"active", "pending", "pending", "pending"}},
		{name: "statuses on leaving the task phase", ev: tasksEvent("done", "done", "done"), task: 2, of: 3,
			title: "two", taskStates: []string{"done", "done", "done"},
			stages: []string{"active", "pending", "pending", "pending"}},
		{name: "external review", ev: phaseEvent(status.PhaseExternalReview), task: 2, of: 3, title: "two",
			taskStates: []string{"done", "done", "done"}, stages: []string{"done", "active", "pending", "pending"}},
		{name: "post-review never reopens review", ev: phaseEvent(status.PhaseReview), task: 2, of: 3, title: "two",
			taskStates: []string{"done", "done", "done"}, stages: []string{"done", "active", "pending", "pending"}},
		{name: "report skips past finalize", ev: phaseEvent(status.PhaseReport), task: 2, of: 3, title: "two",
			taskStates: []string{"done", "done", "done"}, stages: []string{"done", "done", "done", "active"}},
		{name: "success", ev: Event{Type: EventFinish, Status: StatusSucceeded}, task: 2, of: 3, title: "two",
			taskStates: []string{"done", "done", "done"}, stages: []string{"done", "done", "done", "done"}},
	}
	var s Snapshot
	for i, step := range steps {
		step.ev.Seq = i + 1
		s.Apply(step.ev)
		assert.Equal(t, i+1, s.Seq, step.name)
		assert.Equal(t, step.task, s.Task, step.name)
		assert.Equal(t, step.of, s.TaskTotal, step.name)
		assert.Equal(t, step.title, s.TaskTitle, step.name)
		assert.Equal(t, step.taskStates, taskStates(s), step.name)
		assert.Equal(t, step.stages, stageStates(s), step.name)
	}
	assert.Equal(t, []string{"one", "two", ""}, []string{s.Tasks[0].Title, s.Tasks[1].Title, s.Tasks[2].Title},
		"titles survive a tasks event; a task never seen has none")
	require.NotNil(t, s.Finish)
	assert.Equal(t, StatusSucceeded, s.Finish.Status)
	assert.Equal(t, 1, s.Attempt)
	assert.Equal(t, status.PhaseReport, s.Phase)
}

func TestSnapshotTaskStatusCorrections(t *testing.T) {
	start := Event{Type: EventAttemptStart, Attempt: 1, TaskTotal: 3,
		Tasks: []TaskInfo{{N: 1, Title: "one"}, {N: 2, Title: "two"}, {N: 3, Title: "three"}}}
	tests := []struct {
		name   string
		events []Event
		want   []string
	}{
		{name: "running task", events: []Event{start, phaseEvent(status.PhaseTask),
			{Type: EventTask, N: 3, M: 3, Title: "three"}, tasksEvent("done", "done", "pending")},
			want: []string{"done", "done", "active"}},
		{name: "tasks-only success with no phase after the last task", events: []Event{start,
			phaseEvent(status.PhaseTask), {Type: EventTask, N: 3, M: 3, Title: "three"}, tasksEvent("done", "done", "pending"),
			{Type: EventFinish, Status: StatusSucceeded}},
			want: []string{"done", "done", "done"}},
		{name: "a failed finish leaves the recorded statuses", events: []Event{start, phaseEvent(status.PhaseTask),
			{Type: EventTask, N: 2, M: 3, Title: "two"}, tasksEvent("done", "pending", "active"),
			{Type: EventFinish, Status: StatusFailed}},
			want: []string{"done", "pending", "active"}},
		{name: "a stopped finish leaves the recorded statuses", events: []Event{start, phaseEvent(status.PhaseTask),
			{Type: EventTask, N: 1, M: 3, Title: "one"}, {Type: EventFinish, Status: StatusStopped}},
			want: []string{"pending", "pending", "pending"}},
		{name: "a review block between tasks is not the task phase", events: []Event{start,
			phaseEvent(status.PhaseTask), {Type: EventTask, N: 2, M: 3, Title: "two"},
			phaseEvent(status.PhaseExternalReview)},
			want: []string{"pending", "pending", "pending"}},
		{name: "a post-task stage completes every task without a tasks event", events: []Event{start,
			phaseEvent(status.PhaseTask), {Type: EventTask, N: 1, M: 3, Title: "one"}, phaseEvent(status.PhaseReview)},
			want: []string{"done", "done", "done"}},
		{name: "a new attempt starts over", events: []Event{start, phaseEvent(status.PhaseReview),
			{Type: EventFinish, Status: StatusSucceeded}, start, phaseEvent(status.PhaseTask)},
			want: []string{"pending", "pending", "pending"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, taskStates(Fold(tc.events)))
		})
	}

	t.Run("a copy keeps its tasks", func(t *testing.T) {
		s := Fold([]Event{start, phaseEvent(status.PhaseTask), {Type: EventTask, N: 1, M: 3, Title: "one"}})
		before := s
		s.Apply(Event{Type: EventFinish, Status: StatusSucceeded})
		assert.Equal(t, []string{"active", "pending", "pending"}, taskStates(before))
		assert.Equal(t, []string{"done", "done", "done"}, taskStates(s))
	})
}

func TestSnapshotSectionsLimitWaitAndFinish(t *testing.T) {
	waitStart := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	s := Fold([]Event{
		{Seq: 1, Attempt: 1, Type: EventAttemptStart, TaskTotal: 1, Tasks: []TaskInfo{{N: 1, Title: "one"}},
			Stages: []string{StageReview, StageExternalReview}},
		{Seq: 2, Type: EventPhase, Phase: status.PhaseTask},
		{Seq: 3, Type: EventTask, N: 1, M: 1, Title: "one"},
		{Seq: 4, Type: EventPhase, Phase: status.PhaseReview},
		{Seq: 5, Type: EventReview, Kind: ReviewInternal, Iteration: 0, Label: "review 0"},
		{Seq: 6, Type: EventPhase, Phase: status.PhaseLimitWait, WaitSeconds: 600, Time: waitStart},
	})
	assert.Equal(t, SectionMark{Type: EventReview, Kind: ReviewInternal, Label: "review 0"}, s.Section)
	assert.Equal(t, status.PhaseLimitWait, s.Phase)
	assert.Equal(t, 10*time.Minute, s.LimitWait)
	assert.Equal(t, waitStart, s.LimitWaitSince)
	assert.Equal(t, []string{"active", "pending"}, stageStates(s), "a limit wait leaves the stages alone")

	s.Apply(Event{Seq: 7, Type: EventPhase, Phase: status.PhaseReview})
	assert.Zero(t, s.LimitWait, "the limit wait ends with the next phase")
	assert.True(t, s.LimitWaitSince.IsZero())

	s.Apply(Event{Seq: 8, Type: EventPhase, Phase: status.PhaseExternalReview})
	s.Apply(Event{Seq: 9, Type: EventReview, Kind: ReviewExternal, Iteration: 2, Label: "codex external review iteration 2"})
	assert.Equal(t, SectionMark{Type: EventReview, Kind: ReviewExternal, N: 2, Label: "codex external review iteration 2"},
		s.Section)
	s.Apply(Event{Seq: 10, Type: EventFinish, Status: StatusFailed, Reason: "external review: boom", ExitCode: 1})
	require.NotNil(t, s.Finish)
	assert.Equal(t, Finish{Status: StatusFailed, Reason: "external review: boom", ExitCode: 1}, *s.Finish)
	assert.Equal(t, []string{"done", "pending"}, stageStates(s), "a failed run's active stage drops back to pending")

	// a new attempt starts the fold over
	s.Apply(Event{Seq: 11, Attempt: 2, Type: EventAttemptStart, TaskTotal: 1, Tasks: []TaskInfo{{N: 1, Title: "one"}},
		Stages: []string{StageReview}})
	assert.Equal(t, 2, s.Attempt)
	assert.Nil(t, s.Finish)
	assert.Empty(t, s.Phase)
	assert.Zero(t, s.Task)
	assert.Equal(t, SectionMark{}, s.Section)
	assert.Equal(t, []string{"pending"}, stageStates(s))
	assert.Equal(t, 11, s.Seq)
}

func TestSnapshotTaskSection(t *testing.T) {
	s := Fold([]Event{
		{Seq: 1, Type: EventAttemptStart, TaskTotal: 3},
		{Seq: 2, Type: EventTask, N: 3, M: 3, Title: "three"},
	})
	assert.Equal(t, SectionMark{Type: EventTask, N: 3, Label: "three"}, s.Section)
	assert.Equal(t, 3, s.Task)
	assert.Equal(t, 3, s.TaskTotal)
}

func TestSnapshotResumedPastInternalReview(t *testing.T) {
	// a review checkpoint skipped the internal review: the final external block follows the task
	// phase directly and its stage is completed by the next stage entered
	s := Fold([]Event{
		{Seq: 1, Type: EventAttemptStart, Stages: []string{StageReview, StageExternalReview, StageFinalize}},
		{Seq: 2, Type: EventPhase, Phase: status.PhaseTask},
		{Seq: 3, Type: EventPhase, Phase: status.PhaseExternalReview},
		{Seq: 4, Type: EventPhase, Phase: status.PhaseLimitWait},
		{Seq: 5, Type: EventPhase, Phase: status.PhaseExternalReview},
		{Seq: 6, Type: EventPhase, Phase: status.PhaseFinalize},
	})
	assert.Equal(t, []string{"done", "done", "active"}, stageStates(s))
}

func TestSnapshotUndeclaredStage(t *testing.T) {
	// a stage the attempt did not list completes the stages before it and the active one
	s := Fold([]Event{
		{Seq: 1, Type: EventAttemptStart, Stages: []string{StageReview, StageReport}},
		{Seq: 2, Type: EventPhase, Phase: status.PhaseReview},
		{Seq: 3, Type: EventPhase, Phase: status.PhaseFinalize},
	})
	assert.Equal(t, []string{"done", "pending"}, stageStates(s))
}

// eventRecorder collects the events a follower delivers from its goroutine.
type eventRecorder struct {
	mu     sync.Mutex
	events []Event
}

func (r *eventRecorder) add(ev Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
}

func (r *eventRecorder) seqs() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return eventSeqs(r.events)
}

// openTestJournal opens the journal of dir for attempt and closes it at cleanup.
func openTestJournal(t *testing.T, dir string, attempt int) *Journal {
	t.Helper()
	j, err := OpenJournal(dir, attempt)
	require.NoError(t, err)
	t.Cleanup(func() { _ = j.Close() })
	return j
}

// appendTestEvents appends events to j in order.
func appendTestEvents(t *testing.T, j *Journal, events ...Event) {
	t.Helper()
	for _, ev := range events {
		_, err := j.Append(ev)
		require.NoError(t, err)
	}
}

func phaseEvent(p status.Phase) Event {
	return Event{Type: EventPhase, Phase: p}
}

// tasksEvent is a tasks event with the given statuses for positions 1, 2, ...
func tasksEvent(statuses ...string) Event {
	ev := Event{Type: EventTasks}
	for i, st := range statuses {
		ev.Statuses = append(ev.Statuses, TaskStatus{N: i + 1, Status: st})
	}
	return ev
}

func taskStates(s Snapshot) []string {
	states := make([]string, 0, len(s.Tasks))
	for _, t := range s.Tasks {
		states = append(states, t.Status)
	}
	return states
}

func stageStates(s Snapshot) []string {
	states := make([]string, 0, len(s.Stages))
	for _, st := range s.Stages {
		states = append(states, st.Status)
	}
	return states
}
