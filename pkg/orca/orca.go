// Package orca reports loopai execution state through OSC terminal titles.
//
// The integration is best-effort and one-directional: write failures never reach the run.
// When reporting is disabled, callers receive a nil *Reporter and every exported method is a
// no-op, so callers never need to check for nil.
package orca

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/umputun/ralphex/pkg/config"
	"github.com/umputun/ralphex/pkg/plan"
	"github.com/umputun/ralphex/pkg/status"
	"golang.org/x/term"
)

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

// state contains the title-relevant execution state. A zero total means that the number of plan
// tasks is unknown; zero task and iteration values omit their corresponding counters.
type state struct {
	phase     status.Phase
	task      int
	total     int
	iteration int
	waiting   waitingKind
	final     finalKind
}

// Reporter publishes execution state as terminal titles. The zero value is not usable; use New.
// A nil *Reporter is valid and every exported method is a no-op.
type Reporter struct {
	writer   io.Writer
	planFile string
	executor string

	mu       sync.Mutex
	stopOnce sync.Once
	current  state
	stopped  bool
	finished bool
}

// New returns a reporter when title reporting is enabled and stdout is a terminal. Otherwise it
// returns nil. All Reporter methods are nil-safe, so callers do not need to check the result.
func New(enabled bool, planFile, executor string) *Reporter {
	return NewWithOutput(enabled, planFile, executor, os.Stdout, func() bool {
		return term.IsTerminal(int(os.Stdout.Fd()))
	})
}

// NewWithOutput is the dependency-injected form of New. It is useful to callers that need to
// verify title wiring without depending on the process stdout or its terminal state.
func NewWithOutput(
	enabled bool,
	planFile, executor string,
	writer io.Writer,
	isTerminal func() bool,
) *Reporter {
	return newReporter(enabled, planFile, executor, writer, isTerminal)
}

func newReporter(enabled bool, planFile, executor string, writer io.Writer, isTerminal func() bool) *Reporter {
	if !enabled || isTerminal == nil || !isTerminal() {
		return nil
	}
	return &Reporter{
		writer:   writer,
		planFile: planFile,
		executor: executorName(executor),
	}
}

func executorName(executor string) string {
	if executor == config.ExecutorCodex {
		return "codex"
	}
	return "claude"
}

// OnPhase publishes the current working or provider-limit title. Its signature matches
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
		r.emitLocked()
		return
	}

	if r.current.phase != cur {
		r.current = state{phase: cur}
	} else {
		r.current.waiting = waitingNone
	}
	r.emitLocked()
}

// OnSection publishes task progress and review or plan iteration titles. It is called by the
// logger wrapper after the section reaches the regular log.
func (r *Reporter) OnSection(section status.Section) {
	if r == nil {
		return
	}

	var next state
	switch section.Type {
	case status.SectionTaskIteration:
		next = state{phase: status.PhaseTask, task: section.Iteration, total: planTaskTotal(r.planFile)}
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
	r.emitLocked()
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

// WrapLogger decorates an execution logger so structured sections update the terminal title. A
// disabled reporter returns the original logger unchanged.
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

// inputCollector collects interactive input during plan creation. It mirrors
// processor.InputCollector and is declared here because the wrapper below is the only consumer,
// which also keeps pkg/orca free of a dependency on pkg/processor.
type inputCollector interface {
	AskQuestion(ctx context.Context, question string, options []string) (string, error)
	AskDraftReview(ctx context.Context, question, planContent string) (action, feedback string, err error)
}

// WrapInput decorates an input collector so terminal titles show when the run is waiting for a
// human. A disabled reporter returns the original collector unchanged.
func (r *Reporter) WrapInput(collector inputCollector) inputCollector {
	if r == nil {
		return collector
	}
	return &titleCollector{rep: r, inner: collector}
}

type titleCollector struct {
	rep   *Reporter
	inner inputCollector
}

// WithInputWait publishes the waiting title while wait blocks and restores the preceding working
// title afterward. It covers blocking prompts that do not use the plan input-collector interface.
func (r *Reporter) WithInputWait(wait func() bool) bool {
	if r == nil {
		return wait()
	}
	restore := r.beginInputWait()
	defer restore()
	return wait()
}

// AskQuestion publishes the waiting title, delegates, and restores the preceding working title.
func (c *titleCollector) AskQuestion(ctx context.Context, question string, options []string) (string, error) {
	restore := c.rep.beginInputWait()
	defer restore()
	return c.inner.AskQuestion(ctx, question, options) //nolint:wrapcheck // decorator passes the inner error through unchanged
}

// AskDraftReview publishes the waiting title, delegates, and restores the preceding working title.
func (c *titleCollector) AskDraftReview(ctx context.Context, question, planContent string) (action, feedback string, err error) {
	restore := c.rep.beginInputWait()
	defer restore()
	return c.inner.AskDraftReview(ctx, question, planContent) //nolint:wrapcheck // decorator passes the inner error through unchanged
}

func (r *Reporter) beginInputWait() func() {
	r.mu.Lock()
	if r.stopped || r.finished {
		r.mu.Unlock()
		return func() {}
	}

	previous := r.current
	r.current.waiting = waitingInput
	r.emitLocked()
	r.mu.Unlock()

	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.stopped || r.finished {
			return
		}
		r.current = previous
		// Setup reporters can enter an input wait before any phase or section has supplied a
		// working title. Restore those reporters to a visible, non-final idle title instead of
		// leaving Orca stuck on the permission state. Do not store finalStopped here: the same
		// reporter must remain able to publish later setup waits and failures.
		if titleFor(r.current, r.executor) == "" {
			writeTitle(r.writer, "✳ loopai")
			return
		}
		r.emitLocked()
	}
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

// Finish publishes the final idle outcome and freezes the reporter against later updates.
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
	r.emitLocked()
}

// Quiesce freezes the reporter without changing the terminal title. It is used when a successor
// reporter has already published its working title and takes ownership of subsequent updates.
func (r *Reporter) Quiesce() {
	if r == nil {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.finished {
		return
	}
	r.stopped = true
}

// Stop publishes a bare idle title when Finish has not already published an outcome. It is safe
// to call more than once.
func (r *Reporter) Stop() {
	if r == nil {
		return
	}
	r.stopOnce.Do(func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.stopped {
			return
		}
		r.stopped = true
		if r.finished {
			return
		}
		r.current = state{final: finalStopped}
		r.emitLocked()
	})
}

func (r *Reporter) emitLocked() {
	title := titleFor(r.current, r.executor)
	if title != "" {
		writeTitle(r.writer, title)
	}
}

// titleFor formats the terminal title for an execution state. Final and waiting states take
// precedence over working phases because they describe the reporter's current lifecycle state.
func titleFor(s state, executor string) string {
	switch s.final {
	case finalDone:
		return "✳ loopai · done"
	case finalFailed:
		return "✳ loopai · failed"
	case finalStopped:
		return "✳ loopai"
	case finalNone:
	}

	switch s.waiting {
	case waitingInput:
		return "loopai · waiting for input · " + executor
	case waitingLimit:
		return "loopai · waiting for limit · " + executor
	case waitingNone:
	}

	label := phaseLabel(s)
	if label == "" {
		return ""
	}
	return "◐ loopai · " + label + " · " + executor
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

// writeTitle emits one complete OSC title sequence in a single write. Terminal status reporting is
// cosmetic, so both short writes and writer errors are intentionally ignored.
func writeTitle(w io.Writer, title string) {
	if w == nil {
		return
	}
	_, _ = w.Write([]byte("\x1b]0;" + title + "\a"))
}
