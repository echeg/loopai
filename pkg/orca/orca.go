// Package orca reports loopai execution state through OSC terminal titles.
//
// The integration is best-effort and one-directional: write failures never reach the run.
// When reporting is disabled, callers receive a nil *Reporter and every exported method is a
// no-op, so callers never need to check for nil.
package orca

import (
	"fmt"
	"io"

	"github.com/umputun/ralphex/pkg/status"
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
