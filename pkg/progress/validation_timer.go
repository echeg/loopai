package progress

import (
	"sync"
	"time"

	"github.com/umputun/ralphex/internal/validation"
)

// ValidationTimer records completed validation commands. Command durations can
// overlap when executors run tools concurrently, so the aggregate is not a
// measure of elapsed run time.
type ValidationTimer struct {
	mu       sync.Mutex
	commands []string
	logger   SectionLogger
	total    time.Duration
	runs     int
	finished bool
}

// NewValidationTimer creates a validation timer for the configured commands.
func NewValidationTimer(commands []string, logger SectionLogger) *ValidationTimer {
	return &ValidationTimer{commands: append([]string(nil), commands...), logger: logger}
}

// Handler returns the executor callback, or nil when validation timing is disabled.
func (t *ValidationTimer) Handler() func(command string, duration time.Duration) {
	if t == nil || len(t.commands) == 0 || t.logger == nil {
		return nil
	}
	return t.record
}

// FinishRun emits the aggregate validation summary. Repeated calls are no-ops.
func (t *ValidationTimer) FinishRun() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.finished {
		return
	}
	t.finished = true
	if t.runs == 0 {
		return
	}
	t.logger.Print("validation: %s (%d runs)", formatSectionDuration(t.total), t.runs)
}

func (t *ValidationTimer) record(command string, duration time.Duration) {
	label, matched := validation.MatchCommand(command, t.commands)
	if !matched {
		return
	}
	if duration < 0 {
		duration = 0
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	if t.finished {
		return
	}
	t.total += duration
	t.runs++
	t.logger.Print("validation: %s took %s", label, formatSectionDuration(duration))
}
