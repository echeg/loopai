package runs

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"time"

	"github.com/umputun/ralphex/pkg/status"
)

// followPoll is how often a follower reads the journal for new events.
const followPoll = 250 * time.Millisecond

// Progress states of tasks and stages: the statuses a tasks event records and a Snapshot keeps.
const (
	ProgressPending = "pending" // not started
	ProgressActive  = "active"  // in progress, or a partially ticked task
	ProgressDone    = "done"    // completed
)

// Follow calls fn with every event of run directory dir under checkout root whose seq is greater
// than fromSeq, in journal order, and keeps following appends until the run is finished. Finish
// comes from the lifetime lock and the outcome record: once Classify reports a status that is not
// live, the journal is drained, since the run writes its last events before it releases the lock,
// and that status is returned (crashed for a run that ended without an outcome). A canceled ctx
// returns its error.
//
// A caller inside the run process holds the lifetime lock itself and would never see it free; it
// uses FollowUntilFinish instead.
func Follow(ctx context.Context, root, dir string, fromSeq int, fn func(Event)) (Status, error) {
	t := newTail(dir, fromSeq)
	for {
		st, err := Classify(root, dir)
		if err != nil {
			return "", fmt.Errorf("follow run: %w", err)
		}
		if _, _, err := t.deliver(fn, 0); err != nil {
			return "", err
		}
		if !st.Live() {
			return st, nil
		}
		if err := waitPoll(ctx); err != nil {
			return "", err
		}
	}
}

// FollowUntilFinish calls fn with every event of run directory dir whose seq is greater than
// fromSeq, in journal order, until it delivers the finish event of attempt, which it returns.
// It never looks at the lifetime lock, so the run process itself can follow its own journal; a
// finish event of an earlier attempt is delivered like any other event. A canceled ctx returns its
// error, which is the only way out when the attempt never writes its finish event.
func FollowUntilFinish(ctx context.Context, dir string, attempt, fromSeq int, fn func(Event)) (Event, error) {
	t := newTail(dir, fromSeq)
	for {
		finish, found, err := t.deliver(fn, attempt)
		if err != nil {
			return Event{}, err
		}
		if found {
			return finish, nil
		}
		if err := waitPoll(ctx); err != nil {
			return Event{}, err
		}
	}
}

// waitPoll waits one poll interval, or returns the error of a canceled ctx.
func waitPoll(ctx context.Context) error {
	timer := time.NewTimer(followPoll)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return fmt.Errorf("follow run: %w", ctx.Err())
	case <-timer.C:
		return nil
	}
}

// tail reads a journal incrementally from the end of the last complete line it read.
type tail struct {
	path   string
	offset int64
	after  int // seq of the last event delivered, or the starting seq
}

func newTail(dir string, fromSeq int) *tail {
	return &tail{path: filepath.Join(dir, JournalFile), after: fromSeq}
}

// deliver passes the events appended since the previous call to fn. With a positive
// finishAttempt it stops at that attempt's finish event, which it returns with found set, and
// delivers nothing after it.
func (t *tail) deliver(fn func(Event), finishAttempt int) (finish Event, found bool, err error) {
	events, offset, err := readJournal(t.path, t.offset)
	if err != nil {
		return Event{}, false, fmt.Errorf("follow run: %w", err)
	}
	t.offset = offset
	for _, ev := range events {
		if ev.Seq <= t.after {
			continue
		}
		t.after = ev.Seq
		fn(ev)
		if finishAttempt > 0 && ev.Type == EventFinish && ev.Attempt == finishAttempt {
			return ev, true, nil
		}
	}
	return Event{}, false, nil
}

// TaskState is one plan task in a Snapshot. Status is ProgressPending, ProgressActive, or
// ProgressDone.
type TaskState struct {
	N      int
	Title  string
	Status string
}

// StageState is one pipeline stage in a Snapshot. Status is ProgressPending, ProgressActive, or
// ProgressDone.
type StageState struct {
	Name   string
	Status string
}

// SectionMark is the latest task or review section a Snapshot saw.
type SectionMark struct {
	Type  EventType // EventTask or EventReview; empty before the first section
	Kind  string    // the review kind; empty for a task
	N     int       // the task's plan position, or the review iteration
	Label string    // the task title, or the review label
}

// Finish is the end of an attempt as its finish event recorded it.
type Finish struct {
	Status   Status
	Reason   string
	ExitCode int
}

// Snapshot is the state of a run's current attempt folded from its journal events alone. Apply
// each event in journal order; an attempt_start event starts the fold over for the new attempt.
//
// Tasks holds each task's status as the latest tasks event recorded it from the plan, with three
// corrections the plan alone cannot give: the task the task phase is working on is active until the
// plan records it done; entering a post-task stage means the task phase completed, so every task is
// done; and so does a successful finish, which also covers a tasks-only run, where no phase follows
// the last task. A finish that is not a success leaves the recorded statuses.
type Snapshot struct {
	Seq       int // seq of the last event applied
	Attempt   int
	Phase     status.Phase
	Task      int // plan position of the current task; 0 before the first task section
	TaskTotal int
	TaskTitle string
	Tasks     []TaskState
	Stages    []StageState
	Section   SectionMark
	// LimitWait is the configured wait of the limit wait in progress, and LimitWaitSince when it
	// began; both are zero outside a limit wait.
	LimitWait      time.Duration
	LimitWaitSince time.Time
	Finish         *Finish // nil until the attempt's finish event

	workPhase     status.Phase // the latest phase other than a limit wait
	perTaskBlock  bool         // an external review block runs between tasks
	recorded      []TaskState  // the tasks as the journal recorded them, before the corrections
	taskRunning   bool         // the task phase is working on Task
	tasksComplete bool         // a post-task stage was entered, so the task phase completed
}

// Fold returns the Snapshot of events applied in order.
func Fold(events []Event) Snapshot {
	var s Snapshot
	for _, ev := range events {
		s.Apply(ev)
	}
	return s
}

// Apply folds ev into s.
func (s *Snapshot) Apply(ev Event) {
	s.Seq = ev.Seq
	switch ev.Type {
	case EventAttemptStart:
		s.startAttempt(ev)
	case EventPhase:
		s.applyPhase(ev)
	case EventTask:
		s.Task, s.TaskTotal, s.TaskTitle = ev.N, ev.M, ev.Title
		s.Section = SectionMark{Type: EventTask, N: ev.N, Label: ev.Title}
		s.taskRunning = true
	case EventTasks:
		s.applyTaskStatuses(ev.Statuses)
	case EventReview:
		s.Section = SectionMark{Type: EventReview, Kind: ev.Kind, N: ev.Iteration, Label: ev.Label}
		return
	case EventFinish:
		s.applyFinish(ev)
	default:
		return
	}
	s.refreshTasks()
}

// startAttempt resets s to the new attempt's tasks and stages, all pending.
func (s *Snapshot) startAttempt(ev Event) {
	*s = Snapshot{Seq: ev.Seq, Attempt: ev.Attempt, TaskTotal: ev.TaskTotal}
	for _, t := range ev.Tasks {
		s.recorded = append(s.recorded, TaskState{N: t.N, Title: t.Title, Status: ProgressPending})
	}
	for _, name := range ev.Stages {
		s.Stages = append(s.Stages, StageState{Name: name, Status: ProgressPending})
	}
}

// applyPhase records a phase change and moves the stages along.
//
// An external review block entered from the task phase belongs to a task under
// review_cadence = task and leaves the stages alone until the task phase resumes. A run resumed
// past its internal review through a review checkpoint enters the final external review block
// from the task phase too; leaving that block for anything but the task phase shows it was the
// final one, so its stage is entered first. The post-review loop that may follow then stays under
// it, and the stages never move back to review.
func (s *Snapshot) applyPhase(ev Event) {
	s.Phase = ev.Phase
	if ev.Phase == status.PhaseLimitWait {
		s.LimitWait = time.Duration(ev.WaitSeconds) * time.Second
		s.LimitWaitSince = ev.Time
		return
	}
	s.LimitWait, s.LimitWaitSince = 0, time.Time{}
	previous := s.workPhase
	s.workPhase = ev.Phase
	if ev.Phase != status.PhaseTask {
		s.taskRunning = false
	}
	stage, ok := stageForPhase(ev.Phase)
	switch {
	case ev.Phase == status.PhaseTask:
		s.perTaskBlock = false
	case !ok:
	case stage == StageExternalReview && (s.perTaskBlock || previous == status.PhaseTask):
		s.perTaskBlock = true
	default:
		if s.perTaskBlock {
			s.enterStage(StageExternalReview)
		}
		s.perTaskBlock = false
		s.tasksComplete = true
		s.enterStage(stage)
	}
}

// enterStage marks stage active, and the stage active before it and every stage earlier in the
// pipeline done; the latter covers stages a resumed run skipped through its review checkpoint.
// A done stage never reopens: the review loop that follows external review findings runs under
// the external review stage, so progress does not go backwards.
func (s *Snapshot) enterStage(stage string) {
	idx := -1
	for i, st := range s.Stages {
		if st.Name == stage {
			idx = i
		}
	}
	if idx >= 0 && s.Stages[idx].Status == ProgressDone {
		return
	}
	rank := stageRank(stage)
	for i := range s.Stages {
		st := &s.Stages[i]
		switch {
		case i == idx:
			st.Status = ProgressActive
		case st.Status == ProgressActive || stageRank(st.Name) < rank:
			st.Status = ProgressDone
		}
	}
}

// applyTaskStatuses replaces the recorded tasks with the statuses of a tasks event, keeping the
// titles already known for each position.
func (s *Snapshot) applyTaskStatuses(statuses []TaskStatus) {
	titles := make(map[int]string, len(s.recorded)+1)
	for _, t := range s.recorded {
		titles[t.N] = t.Title
	}
	if s.Task > 0 && s.TaskTitle != "" {
		titles[s.Task] = s.TaskTitle
	}
	tasks := make([]TaskState, 0, len(statuses))
	for _, st := range statuses {
		tasks = append(tasks, TaskState{N: st.N, Title: titles[st.N], Status: st.Status})
	}
	s.recorded = tasks
}

// refreshTasks derives Tasks from the recorded tasks: every task is done once the task phase
// completed or the attempt succeeded, and the task the task phase is working on is active until it
// is recorded done. Tasks is a fresh slice, so a copy of an earlier Snapshot keeps its own.
func (s *Snapshot) refreshTasks() {
	tasks := slices.Clone(s.recorded)
	allDone := s.tasksComplete || (s.Finish != nil && s.Finish.Status == StatusSucceeded)
	for i := range tasks {
		switch {
		case allDone:
			tasks[i].Status = ProgressDone
		case s.taskRunning && tasks[i].N == s.Task && tasks[i].Status != ProgressDone:
			tasks[i].Status = ProgressActive
		}
	}
	s.Tasks = tasks
}

// applyFinish records the attempt's end. A successful run has passed every stage, including any it
// skipped through a review checkpoint; otherwise the stage it stopped in drops back to pending, and
// the task it stopped in drops back to its recorded status.
func (s *Snapshot) applyFinish(ev Event) {
	s.Finish = &Finish{Status: ev.Status, Reason: ev.Reason, ExitCode: ev.ExitCode}
	s.LimitWait, s.LimitWaitSince = 0, time.Time{}
	s.taskRunning = false
	for i := range s.Stages {
		switch {
		case ev.Status == StatusSucceeded:
			s.Stages[i].Status = ProgressDone
		case s.Stages[i].Status == ProgressActive:
			s.Stages[i].Status = ProgressPending
		}
	}
}

// stageForPhase maps an execution phase to the pipeline stage it belongs to.
func stageForPhase(p status.Phase) (string, bool) {
	switch p {
	case status.PhaseReview:
		return StageReview, true
	case status.PhaseExternalReview, status.PhaseExternalEval:
		return StageExternalReview, true
	case status.PhaseFinalize:
		return StageFinalize, true
	case status.PhaseReport:
		return StageReport, true
	default:
		return "", false
	}
}

// stageRank is a stage's position in the pipeline order.
func stageRank(stage string) int {
	switch stage {
	case StageReview:
		return 1
	case StageExternalReview:
		return 2
	case StageFinalize:
		return 3
	case StageReport:
		return 4
	default:
		return 0
	}
}
