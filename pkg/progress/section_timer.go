package progress

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/umputun/ralphex/pkg/status"
)

// SectionLogger mirrors the logger interfaces shared by cmux and processor.
// It lives in progress because importing either package here would create the
// cmux/processor -> plan -> progress import cycle. Compile-time assertions in
// cmd/loopai tests catch any drift between these interfaces.
type SectionLogger interface {
	Print(format string, args ...any)
	PrintRaw(format string, args ...any)
	PrintSection(section status.Section)
	PrintAligned(text string)
	LogQuestion(question string, options []string)
	LogAnswer(answer string)
	LogDraftReview(action string, feedback string)
	Path() string
}

// SectionTimer records wall-clock time spent in structured log sections.
type SectionTimer struct {
	SectionLogger

	mu       sync.Mutex
	now      func() time.Time
	current  *status.Section
	started  time.Time
	finished bool
	buckets  [bucketCount]sectionBucket
}

type sectionBucket struct {
	duration time.Duration
	count    int
}

type bucket int

const (
	bucketTasks bucket = iota
	bucketInternalReview
	bucketExternalReview
	bucketEvaluation
	bucketPlanning
	bucketOther
	bucketCount
)

var bucketNames = [bucketCount]string{
	"tasks",
	"internal review",
	"external review",
	"evaluation",
	"planning",
	"other",
}

// NewSectionTimer wraps inner with section duration tracking.
func NewSectionTimer(inner SectionLogger, now func() time.Time) *SectionTimer {
	if now == nil {
		now = time.Now
	}
	return &SectionTimer{SectionLogger: inner, now: now}
}

// PrintSection closes the current section, if any, before opening the next one.
func (t *SectionTimer) PrintSection(section status.Section) {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := t.now()
	t.closeCurrent(now)
	t.SectionLogger.PrintSection(section)
	t.current = &section
	t.started = now
}

// FinishRun closes the final section and emits the aggregate phase summary.
// Repeated calls are no-ops.
func (t *SectionTimer) FinishRun() {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.finished {
		return
	}
	if t.current == nil {
		return
	}
	t.finished = true

	t.closeCurrent(t.now())
	parts := make([]string, 0, bucketCount)
	for i, value := range t.buckets {
		if value.count == 0 {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s %s (%d)", bucketNames[i], formatSectionDuration(value.duration), value.count))
	}
	t.Print("phase durations: %s", strings.Join(parts, ", "))
}

func (t *SectionTimer) closeCurrent(now time.Time) {
	if t.current == nil {
		return
	}
	duration := now.Sub(t.started)
	t.Print("%s took %s", t.current.Label, formatSectionDuration(duration))
	value := &t.buckets[bucketForSection(t.current.Type)]
	value.duration += duration
	value.count++
	t.current = nil
}

func bucketForSection(sectionType status.SectionType) bucket {
	switch sectionType {
	case status.SectionTaskIteration:
		return bucketTasks
	case status.SectionInternalReview:
		return bucketInternalReview
	case status.SectionExternalReviewIteration:
		return bucketExternalReview
	case status.SectionExternalEvaluation:
		return bucketEvaluation
	case status.SectionPlanIteration:
		return bucketPlanning
	default:
		return bucketOther
	}
}

func formatSectionDuration(duration time.Duration) string {
	if duration >= time.Hour {
		return strings.TrimSuffix(duration.Truncate(time.Minute).String(), "0s")
	}
	return duration.Truncate(time.Second).String()
}
