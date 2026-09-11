package awake

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/ralphex/pkg/status"
)

type fakeBackend struct {
	acquires   atomic.Int32
	releases   atomic.Int32
	acquireErr error
}

func (b *fakeBackend) Acquire() error {
	b.acquires.Add(1)
	return b.acquireErr
}

func (b *fakeBackend) Release() { b.releases.Add(1) }

const (
	shortIdle = 30 * time.Millisecond
	waitFor   = 2 * time.Second
	tick      = 2 * time.Millisecond
)

func TestHolder_TouchAcquiresOnce(t *testing.T) {
	b := &fakeBackend{}
	h := NewWithBackend(b, time.Hour)
	t.Cleanup(h.Stop)

	h.Touch()
	h.Touch()
	h.Touch()

	assert.Equal(t, int32(1), b.acquires.Load())
	assert.Equal(t, int32(0), b.releases.Load())
}

func TestHolder_IdleExpiryReleasesAndTouchReacquires(t *testing.T) {
	b := &fakeBackend{}
	h := NewWithBackend(b, shortIdle)
	t.Cleanup(h.Stop)

	h.Touch()
	require.Eventually(t, func() bool { return b.releases.Load() == 1 }, waitFor, tick)

	h.Touch()
	assert.Equal(t, int32(2), b.acquires.Load())
}

func TestHolder_TouchDefersExpiry(t *testing.T) {
	b := &fakeBackend{}
	h := NewWithBackend(b, shortIdle)
	t.Cleanup(h.Stop)

	deadline := time.Now().Add(4 * shortIdle)
	for time.Now().Before(deadline) {
		h.Touch()
		time.Sleep(shortIdle / 5)
	}

	assert.Equal(t, int32(1), b.acquires.Load())
	assert.Equal(t, int32(0), b.releases.Load())
}

func TestHolder_StopReleasesAndIgnoresLaterTouches(t *testing.T) {
	b := &fakeBackend{}
	h := NewWithBackend(b, time.Hour)

	h.Touch()
	h.Stop()
	h.Stop()
	h.Touch()

	assert.Equal(t, int32(1), b.acquires.Load())
	assert.Equal(t, int32(1), b.releases.Load())
}

func TestHolder_StopWithoutHoldReleasesNothing(t *testing.T) {
	b := &fakeBackend{}
	h := NewWithBackend(b, time.Hour)

	h.Stop()

	assert.Equal(t, int32(0), b.acquires.Load())
	assert.Equal(t, int32(0), b.releases.Load())
}

func TestHolder_AcquireFailureDisablesHolder(t *testing.T) {
	b := &fakeBackend{acquireErr: errors.New("no inhibitor")}
	h := NewWithBackend(b, time.Hour)
	t.Cleanup(h.Stop)

	h.Touch()
	h.Touch()

	assert.Equal(t, int32(1), b.acquires.Load(), "a failed acquire is not retried")
	h.Stop()
	assert.Equal(t, int32(0), b.releases.Load(), "nothing to release after a failed acquire")
}

func TestHolder_LimitWaitPinsHold(t *testing.T) {
	b := &fakeBackend{}
	h := NewWithBackend(b, shortIdle)
	t.Cleanup(h.Stop)

	h.OnPhase(status.PhaseTask, status.PhaseLimitWait)
	time.Sleep(4 * shortIdle)
	assert.Equal(t, int32(1), b.acquires.Load())
	assert.Equal(t, int32(0), b.releases.Load(), "a scheduled provider-limit wait is not idleness")

	h.OnPhase(status.PhaseLimitWait, status.PhaseTask)
	require.Eventually(t, func() bool { return b.releases.Load() == 1 }, waitFor, tick)
}

func TestHolder_PhaseChangeTouches(t *testing.T) {
	b := &fakeBackend{}
	h := NewWithBackend(b, time.Hour)
	t.Cleanup(h.Stop)

	h.OnPhase("", status.PhaseTask)

	assert.Equal(t, int32(1), b.acquires.Load())
}

type recordingLogger struct {
	mu    sync.Mutex
	calls []string
}

func (l *recordingLogger) record(call string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, call)
}

func (l *recordingLogger) Print(format string, _ ...any)           { l.record("print:" + format) }
func (l *recordingLogger) PrintRaw(format string, _ ...any)        { l.record("raw:" + format) }
func (l *recordingLogger) PrintSection(section status.Section)     { l.record("section:" + section.Label) }
func (l *recordingLogger) PrintAligned(text string)                { l.record("aligned:" + text) }
func (l *recordingLogger) LogQuestion(question string, _ []string) { l.record("question:" + question) }
func (l *recordingLogger) LogAnswer(answer string)                 { l.record("answer:" + answer) }
func (l *recordingLogger) LogDraftReview(action, _ string)         { l.record("draft:" + action) }
func (l *recordingLogger) Path() string                            { return "progress.log" }

func TestHolder_WrapLoggerTouchesOnOutput(t *testing.T) {
	b := &fakeBackend{}
	h := NewWithBackend(b, shortIdle)
	t.Cleanup(h.Stop)
	inner := &recordingLogger{}
	log := h.WrapLogger(inner)

	log.Print("p")
	require.Eventually(t, func() bool { return b.releases.Load() == 1 }, waitFor, tick)
	log.PrintRaw("r")
	require.Eventually(t, func() bool { return b.releases.Load() == 2 }, waitFor, tick)
	log.PrintSection(status.NewTaskIterationSection(1))
	require.Eventually(t, func() bool { return b.releases.Load() == 3 }, waitFor, tick)
	log.PrintAligned("a")
	require.Eventually(t, func() bool { return b.releases.Load() == 4 }, waitFor, tick)

	assert.Equal(t, int32(4), b.acquires.Load())
	assert.Equal(t, []string{"print:p", "raw:r", "section:task iteration 1", "aligned:a"}, inner.calls)
	assert.Equal(t, "progress.log", log.Path())
}

func TestHolder_WrapLoggerForwardsInteractiveCalls(t *testing.T) {
	b := &fakeBackend{}
	h := NewWithBackend(b, time.Hour)
	t.Cleanup(h.Stop)
	inner := &recordingLogger{}
	log := h.WrapLogger(inner)

	log.LogQuestion("q", nil)
	log.LogAnswer("a")
	log.LogDraftReview("approve", "")

	assert.Equal(t, []string{"question:q", "answer:a", "draft:approve"}, inner.calls)
	assert.Equal(t, int32(0), b.acquires.Load(), "waiting for a human is not activity")
}

func TestHolder_NilIsNoop(t *testing.T) {
	var h *Holder
	inner := &recordingLogger{}

	h.Touch()
	h.OnPhase("", status.PhaseTask)
	h.Stop()
	assert.Same(t, inner, h.WrapLogger(inner).(*recordingLogger))
}

func TestNew_DisabledReturnsNil(t *testing.T) {
	assert.Nil(t, New(false))
}
