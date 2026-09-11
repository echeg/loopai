// Package awake keeps the machine from sleeping while loopai is working.
//
// The integration is best-effort: a missing or failing system inhibitor never reaches the run.
// The hold is renewed by activity and expires after an idle window, so a hung run does not keep a
// laptop awake indefinitely. When keeping awake is disabled or unsupported, callers receive a nil
// *Holder and every exported method is a no-op, so callers never need to check for nil.
package awake

import (
	"sync"
	"time"

	"github.com/umputun/ralphex/pkg/status"
)

// DefaultIdle is the idle window after which an unrenewed hold is released.
const DefaultIdle = time.Hour

// Backend acquires and releases one system sleep inhibitor.
type Backend interface {
	Acquire() error
	Release()
}

// Holder renews a sleep inhibitor on activity and releases it after an idle window. The zero
// value is not usable; use New or NewWithBackend. A nil *Holder is valid and every exported
// method is a no-op.
type Holder struct {
	backend Backend
	idle    time.Duration

	mu      sync.Mutex
	timer   *time.Timer
	held    bool
	pinned  bool // a scheduled provider-limit wait is in progress; do not expire
	stopped bool
}

// New returns a holder backed by the platform sleep inhibitor, or nil when keeping awake is
// disabled or no inhibitor is available on this system.
func New(enabled bool) *Holder {
	if !enabled {
		return nil
	}
	backend := platformBackend()
	if backend == nil {
		return nil
	}
	return NewWithBackend(backend, DefaultIdle)
}

// NewWithBackend returns a holder over the given backend with the given idle window.
func NewWithBackend(backend Backend, idle time.Duration) *Holder {
	return &Holder{backend: backend, idle: idle}
}

// Touch records activity: it acquires the inhibitor when not held and restarts the idle window.
func (h *Holder) Touch() {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.touchLocked()
}

func (h *Holder) touchLocked() {
	if h.stopped {
		return
	}
	if !h.held {
		if err := h.backend.Acquire(); err != nil {
			// the inhibitor is unavailable for this run; do not retry on every output line
			h.stopped = true
			return
		}
		h.held = true
	}
	h.armLocked()
}

func (h *Holder) armLocked() {
	if h.pinned {
		if h.timer != nil {
			h.timer.Stop()
		}
		return
	}
	if h.timer == nil {
		h.timer = time.AfterFunc(h.idle, h.expire)
		return
	}
	h.timer.Reset(h.idle)
}

func (h *Holder) expire() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.stopped || h.pinned || !h.held {
		return
	}
	h.backend.Release()
	h.held = false
}

// OnPhase treats a phase change as activity. A provider-limit wait pins the hold until the phase
// changes again, because the wait is scheduled rather than idle and can exceed the idle window.
// Its signature matches status.PhaseHolder.OnChange.
func (h *Holder) OnPhase(_, cur status.Phase) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.pinned = cur == status.PhaseLimitWait
	h.touchLocked()
}

// Stop releases the inhibitor and ignores later activity. It is safe to call more than once.
func (h *Holder) Stop() {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.stopped {
		return
	}
	h.stopped = true
	if h.timer != nil {
		h.timer.Stop()
	}
	if h.held {
		h.backend.Release()
		h.held = false
	}
}

// Logger is the execution logger surface needed to observe output activity.
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

// WrapLogger decorates an execution logger so output renews the hold. Interactive question and
// answer records are forwarded without renewing it: waiting for a human is not activity. A
// disabled holder returns the original logger unchanged.
func (h *Holder) WrapLogger(logger Logger) Logger {
	if h == nil {
		return logger
	}
	return &activityLogger{Logger: logger, holder: h}
}

type activityLogger struct {
	Logger
	holder *Holder
}

func (l *activityLogger) Print(format string, args ...any) {
	l.Logger.Print(format, args...)
	l.holder.Touch()
}

func (l *activityLogger) PrintRaw(format string, args ...any) {
	l.Logger.PrintRaw(format, args...)
	l.holder.Touch()
}

func (l *activityLogger) PrintSection(section status.Section) {
	l.Logger.PrintSection(section)
	l.holder.Touch()
}

func (l *activityLogger) PrintAligned(text string) {
	l.Logger.PrintAligned(text)
	l.holder.Touch()
}
