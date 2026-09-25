package t3

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/umputun/ralphex/pkg/config"
	"github.com/umputun/ralphex/pkg/plan"
	"github.com/umputun/ralphex/pkg/status"
)

// stopTimeout bounds how long Stop waits for the final title to reach the server.
const stopTimeout = 3 * time.Second

type waitingKind uint8

const (
	waitingNone waitingKind = iota
	waitingInput
	waitingLimit
)

type finalKind uint8

const (
	finalNone finalKind = iota
	finalDone
	finalFailed
	finalStopped
)

// state is the title-relevant execution state. It deliberately duplicates the small pkg/orca
// model instead of sharing it, so the two best-effort sinks evolve independently.
type state struct {
	phase     status.Phase
	task      int
	total     int
	iteration int
	waiting   waitingKind
	final     finalKind
}

// Dispatcher is the orchestration API surface the reporter needs.
type Dispatcher interface {
	Dispatch(ctx context.Context, cmd Command) (int64, error)
	Shell(ctx context.Context) (Shell, error)
}

// Options configures a Reporter.
type Options struct {
	PlanFile string // plan being executed; empty for review-only runs
	Executor string // config executor name
	Model    string // effective task model recorded on a created thread
	RepoRoot string // main checkout root, matched against T3 project workspace roots
	// WorktreePath is recorded on a created thread. Leave it empty when the directory will not
	// outlive the run (a loopai --worktree checkout is removed after success).
	WorktreePath string
	Branch       string
	// ThreadID binds the reporter to an existing thread instead of creating one.
	ThreadID string
	// Warn receives at most one line explaining why reporting stopped. It may be nil.
	Warn func(format string, args ...any)
}

// Reporter publishes loopai execution state as the title of a T3 Code thread. A nil *Reporter is
// valid and every exported method is a no-op. All network work happens on one background
// goroutine, so no method ever blocks on T3 Code except Stop, which waits a bounded time for the
// final title.
type Reporter struct {
	api  Dispatcher
	opts Options
	name string

	mu       sync.Mutex
	stopOnce sync.Once
	current  state
	stopped  bool
	finished bool
	pending  string

	wake chan struct{}
	quit chan struct{}
	done chan struct{}
}

// New returns a reporter bound to the T3 Code server described by the environment. Callers
// construct it only when reporting is enabled; the error explains why it could not start, and
// callers print it once and continue without one.
func New(opts Options, getenv func(string) string) (*Reporter, error) {
	ep, err := ResolveEndpoint(getenv)
	if err != nil {
		return nil, err
	}
	if opts.ThreadID == "" {
		opts.ThreadID = strings.TrimSpace(getenv(EnvThreadID))
	}
	return newReporter(NewClient(ep), opts), nil
}

// NewWithDispatcher is the dependency-injected form of New for callers that verify wiring.
func NewWithDispatcher(api Dispatcher, opts Options) *Reporter {
	return newReporter(api, opts)
}

func newReporter(api Dispatcher, opts Options) *Reporter {
	r := &Reporter{
		api:  api,
		opts: opts,
		name: runName(opts.PlanFile),
		wake: make(chan struct{}, 1),
		quit: make(chan struct{}),
		done: make(chan struct{}),
	}
	go r.run()
	return r
}

// runName is the plan file stem without a leading date prefix, or "loopai" without a plan.
func runName(planFile string) string {
	if planFile == "" {
		return "loopai"
	}
	stem := strings.TrimSuffix(filepath.Base(planFile), filepath.Ext(planFile))
	trimmed := strings.TrimLeft(stem, "0123456789-")
	if trimmed == "" {
		return stem
	}
	return trimmed
}

// OnPhase publishes the working or provider-limit title. Its signature matches
// status.PhaseHolder.OnChange.
func (r *Reporter) OnPhase(_, cur status.Phase) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped || r.finished {
		return
	}
	if cur == status.PhaseLimitWait {
		r.current.waiting = waitingLimit
		r.publishLocked()
		return
	}
	if r.current.phase != cur {
		r.current = state{phase: cur}
	} else {
		r.current.waiting = waitingNone
	}
	r.publishLocked()
}

// OnSection publishes task progress and review iteration titles.
func (r *Reporter) OnSection(section status.Section) {
	if r == nil {
		return
	}
	var next state
	switch section.Type {
	case status.SectionTaskIteration:
		next = state{phase: status.PhaseTask, task: section.Iteration, total: planTaskTotal(r.opts.PlanFile)}
	case status.SectionInternalReview:
		next = state{phase: status.PhaseReview, iteration: section.Iteration}
	case status.SectionExternalReviewIteration:
		next = state{phase: status.PhaseExternalReview, iteration: section.Iteration}
	case status.SectionPlanIteration:
		next = state{phase: status.PhasePlan, iteration: section.Iteration}
	default:
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped || r.finished {
		return
	}
	r.current = next
	r.publishLocked()
}

// Logger is the execution logger surface needed to observe structured sections.
type Logger interface {
	Print(format string, args ...any)
	PrintRaw(format string, args ...any)
	PrintSection(section status.Section)
	PrintAligned(text string)
	LogQuestion(question string, options []string)
	LogAnswer(answer string)
	LogDraftReview(action string, feedback string)
	Path() string
}

// WrapLogger decorates a logger so structured sections update the thread title. A disabled
// reporter returns the original logger unchanged.
func (r *Reporter) WrapLogger(logger Logger) Logger {
	if r == nil {
		return logger
	}
	return &titleLogger{Logger: logger, rep: r}
}

type titleLogger struct {
	Logger
	rep *Reporter
}

func (l *titleLogger) PrintSection(section status.Section) {
	l.Logger.PrintSection(section)
	l.rep.OnSection(section)
}

// WithInputWait publishes the waiting title while wait blocks and restores the preceding title.
func (r *Reporter) WithInputWait(wait func() bool) bool {
	if r == nil {
		return wait()
	}
	restore := r.beginInputWait()
	defer restore()
	return wait()
}

func (r *Reporter) beginInputWait() func() {
	r.mu.Lock()
	if r.stopped || r.finished {
		r.mu.Unlock()
		return func() {}
	}
	previous := r.current
	r.current.waiting = waitingInput
	r.publishLocked()
	r.mu.Unlock()

	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.stopped || r.finished {
			return
		}
		r.current = previous
		r.publishLocked()
	}
}

// Finish publishes the final outcome and freezes the reporter against later updates.
func (r *Reporter) Finish(success bool) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped || r.finished {
		return
	}
	if success {
		r.current = state{final: finalDone}
	} else {
		r.current = state{final: finalFailed}
	}
	r.finished = true
	r.publishLocked()
}

// Stop publishes a stopped title unless Finish already published an outcome, then waits a bounded
// time for pending updates to reach the server. It is safe to call more than once.
func (r *Reporter) Stop() {
	if r == nil {
		return
	}
	r.stopOnce.Do(func() {
		r.mu.Lock()
		if !r.stopped && !r.finished {
			r.current = state{final: finalStopped}
			r.publishLocked()
		}
		r.stopped = true
		r.mu.Unlock()

		close(r.quit)
		select {
		case <-r.done:
		case <-time.After(stopTimeout):
		}
	})
}

// publishLocked records the latest title for the worker; intermediate titles coalesce.
func (r *Reporter) publishLocked() {
	title := r.titleFor(r.current)
	if title == "" {
		return
	}
	r.pending = title
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

func (r *Reporter) takePending() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	title := r.pending
	r.pending = ""
	return title
}

// run owns every network call: it binds the thread once, then sends each changed title. Any
// error disables reporting for the rest of the run.
func (r *Reporter) run() {
	defer close(r.done)
	threadID := ""
	sent := ""
	for {
		select {
		case <-r.wake:
		case <-r.quit:
			// drain the final title, if any, before exiting
		}
		title := r.takePending()
		if title != "" && title != sent {
			var err error
			if threadID == "" {
				threadID, err = r.bind(title)
			} else {
				err = r.send(threadID, title)
			}
			if err != nil {
				r.warn(err)
				r.drainUntilQuit()
				return
			}
			sent = title
		}
		select {
		case <-r.quit:
			if r.hasPending() {
				continue
			}
			return
		default:
		}
	}
}

func (r *Reporter) hasPending() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pending != ""
}

// drainUntilQuit keeps publishers from blocking after reporting has been disabled.
func (r *Reporter) drainUntilQuit() {
	for {
		select {
		case <-r.wake:
			r.takePending()
		case <-r.quit:
			return
		}
	}
}

func (r *Reporter) bind(title string) (string, error) {
	if r.opts.ThreadID != "" {
		return r.opts.ThreadID, r.send(r.opts.ThreadID, title)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*requestTimeout)
	defer cancel()
	shell, err := r.api.Shell(ctx)
	if err != nil {
		return "", fmt.Errorf("t3 status disabled: %w", err)
	}
	project, ok := shell.FindProject(r.opts.RepoRoot)
	if !ok {
		return "", fmt.Errorf("t3 status disabled: no T3 Code project for %s", r.opts.RepoRoot)
	}
	threadID := NewID()
	cmd := NewThreadCreate(threadID, project.ID, title, modelSelection(r.opts.Executor, r.opts.Model),
		r.opts.Branch, r.opts.WorktreePath)
	if _, err := r.api.Dispatch(ctx, cmd); err != nil {
		return "", fmt.Errorf("t3 status disabled: create thread: %w", err)
	}
	return threadID, nil
}

func (r *Reporter) send(threadID, title string) error {
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	if _, err := r.api.Dispatch(ctx, NewThreadTitleUpdate(threadID, title)); err != nil {
		return fmt.Errorf("t3 status disabled: %w", err)
	}
	return nil
}

func (r *Reporter) warn(err error) {
	if r.opts.Warn != nil {
		r.opts.Warn("%v", err)
	}
}

func modelSelection(executor, model string) ModelSelection {
	instance := InstanceClaude
	if executor == config.ExecutorCodex {
		instance = InstanceCodex
	}
	model = strings.TrimSpace(model)
	if i := strings.Index(model, ":"); i >= 0 {
		model = model[:i] // drop the loopai reasoning-effort suffix
	}
	if model == "" {
		model = "default"
	}
	return ModelSelection{InstanceID: instance, Model: model}
}

func planTaskTotal(planFile string) int {
	if planFile == "" {
		return 0
	}
	parsed, err := plan.ParsePlanFile(planFile)
	if err != nil {
		return 0
	}
	return len(parsed.Tasks)
}

// titleFor formats the thread title. Final and waiting states take precedence over phases.
func (r *Reporter) titleFor(s state) string {
	switch s.final {
	case finalDone:
		return r.name + " · done"
	case finalFailed:
		return r.name + " · failed"
	case finalStopped:
		return r.name + " · stopped"
	case finalNone:
	}
	switch s.waiting {
	case waitingInput:
		return r.name + " · waiting for input"
	case waitingLimit:
		return r.name + " · waiting for limit"
	case waitingNone:
	}
	label := phaseLabel(s)
	if label == "" {
		return ""
	}
	return r.name + " · " + label
}

func phaseLabel(s state) string {
	switch s.phase {
	case status.PhaseTask:
		switch {
		case s.task > 0 && s.total > 0:
			return fmt.Sprintf("task %d/%d", s.task, s.total)
		case s.task > 0:
			return fmt.Sprintf("task %d", s.task)
		default:
			return "task"
		}
	case status.PhaseReview:
		return iterationLabel("review", s.iteration)
	case status.PhaseExternalReview:
		return iterationLabel("external review", s.iteration)
	case status.PhaseExternalEval:
		return "external eval"
	case status.PhasePlan:
		return iterationLabel("plan", s.iteration)
	case status.PhaseFinalize:
		return "finalize"
	case status.PhaseReport:
		return "report"
	default:
		return ""
	}
}

func iterationLabel(label string, iteration int) string {
	if iteration <= 0 {
		return label
	}
	return fmt.Sprintf("%s · iteration %d", label, iteration)
}
