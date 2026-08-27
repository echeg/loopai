// Package orca reports loopai execution state through OSC terminal titles.
//
// The integration is best-effort and one-directional: write failures never reach the run.
// When reporting is disabled, callers receive a nil *Reporter and every exported method is a
// no-op, so callers never need to check for nil.
package orca

import (
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/umputun/ralphex/pkg/config"
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
	quiesced bool
	stopped  bool
	finished bool
}

// New returns a reporter when title reporting is enabled and stdout is a terminal. Otherwise it
// returns nil. All Reporter methods are nil-safe, so callers do not need to check the result.
func New(enabled bool, planFile, executor string) *Reporter {
	return newReporter(enabled, planFile, executor, os.Stdout, func() bool {
		return term.IsTerminal(int(os.Stdout.Fd()))
	})
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
	if r.quiesced || r.stopped || r.finished {
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

// OnSection observes a structured log section. Section-specific title updates are implemented by
// the logger integration; the lifecycle gate lives here so late observations stay harmless.
func (r *Reporter) OnSection(_ status.Section) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.quiesced || r.stopped || r.finished {
		return
	}
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
