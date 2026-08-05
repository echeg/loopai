package progress

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/ralphex/pkg/status"
)

type recordingSectionLogger struct {
	calls []string
}

func (l *recordingSectionLogger) Print(format string, args ...any) {
	l.calls = append(l.calls, fmt.Sprintf("print: "+format, args...))
}
func (l *recordingSectionLogger) PrintRaw(format string, args ...any) {
	l.calls = append(l.calls, fmt.Sprintf("raw: "+format, args...))
}
func (l *recordingSectionLogger) PrintSection(section status.Section) {
	l.calls = append(l.calls, "section: "+section.Label)
}
func (l *recordingSectionLogger) PrintAligned(text string) {
	l.calls = append(l.calls, "aligned: "+text)
}
func (l *recordingSectionLogger) LogQuestion(question string, options []string) {
	l.calls = append(l.calls, "question: "+question)
}
func (l *recordingSectionLogger) LogAnswer(answer string) {
	l.calls = append(l.calls, "answer: "+answer)
}
func (l *recordingSectionLogger) LogDraftReview(action, feedback string) {
	l.calls = append(l.calls, "draft review: "+action)
}
func (l *recordingSectionLogger) Path() string { return "progress.log" }

func clockSequence(t *testing.T, times ...time.Time) func() time.Time {
	t.Helper()
	index := 0
	return func() time.Time {
		require.Less(t, index, len(times), "fake clock exhausted")
		value := times[index]
		index++
		return value
	}
}

func TestSectionTimer_SequenceAndSummary(t *testing.T) {
	start := time.Date(2026, 8, 5, 10, 0, 0, 0, time.UTC)
	inner := &recordingSectionLogger{}
	timer := NewSectionTimer(inner, clockSequence(t, start, start.Add(61*time.Second), start.Add(2*time.Hour+2*time.Minute+2*time.Second)))

	timer.PrintSection(status.Section{Type: status.SectionTaskIteration, Label: "task 100% ready"})
	timer.PrintSection(status.Section{Type: status.SectionInternalReview, Label: "review"})
	timer.FinishRun()

	assert.Equal(t, []string{
		"section: task 100% ready",
		"print: task 100% ready took 1m1s",
		"section: review",
		"print: review took 2h1m",
		"print: phase durations: tasks 1m1s (1), internal review 2h1m (1)",
	}, inner.calls)
}

func TestSectionTimer_FinishRunClosesSingleSectionOnce(t *testing.T) {
	start := time.Date(2026, 8, 5, 10, 0, 0, 0, time.UTC)
	inner := &recordingSectionLogger{}
	timer := NewSectionTimer(inner, clockSequence(t, start, start.Add(45*time.Second)))

	timer.PrintSection(status.NewTaskIterationSection(1))
	timer.FinishRun()
	timer.FinishRun()

	assert.Equal(t, []string{
		"section: task iteration 1",
		"print: task iteration 1 took 45s",
		"print: phase durations: tasks 45s (1)",
	}, inner.calls)
}

func TestSectionTimer_FinishRunWithoutSectionsIsSilent(t *testing.T) {
	inner := &recordingSectionLogger{}
	timer := NewSectionTimer(inner, nil)

	timer.FinishRun()

	assert.Empty(t, inner.calls)
}

func TestSectionTimer_BucketMapping(t *testing.T) {
	tests := []struct {
		name        string
		sectionType status.SectionType
		wantBucket  string
	}{
		{name: "task", sectionType: status.SectionTaskIteration, wantBucket: "tasks"},
		{name: "internal review", sectionType: status.SectionInternalReview, wantBucket: "internal review"},
		{name: "external review", sectionType: status.SectionExternalReviewIteration, wantBucket: "external review"},
		{name: "evaluation", sectionType: status.SectionExternalEvaluation, wantBucket: "evaluation"},
		{name: "planning", sectionType: status.SectionPlanIteration, wantBucket: "planning"},
		{name: "generic external review", sectionType: status.SectionGeneric, wantBucket: "other"},
		{name: "custom", sectionType: status.SectionCustomIteration, wantBucket: "other"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start := time.Date(2026, 8, 5, 10, 0, 0, 0, time.UTC)
			inner := &recordingSectionLogger{}
			timer := NewSectionTimer(inner, clockSequence(t, start, start.Add(time.Minute)))
			timer.PrintSection(status.Section{Type: tt.sectionType, Label: "external review (claude)"})

			timer.FinishRun()

			require.Len(t, inner.calls, 3)
			assert.Equal(t, "print: phase durations: "+tt.wantBucket+" 1m0s (1)", inner.calls[2])
		})
	}
}

func TestSectionTimer_AggregatesRepeatedBucketOccurrences(t *testing.T) {
	start := time.Date(2026, 8, 5, 10, 0, 0, 0, time.UTC)
	inner := &recordingSectionLogger{}
	timer := NewSectionTimer(inner, clockSequence(t, start, start.Add(10*time.Second), start.Add(25*time.Second)))

	timer.PrintSection(status.NewTaskIterationSection(1))
	timer.PrintSection(status.NewTaskIterationSection(1))
	timer.FinishRun()

	require.Len(t, inner.calls, 5)
	assert.Equal(t, "print: phase durations: tasks 25s (2)", inner.calls[4])
}

func TestFormatSectionDuration(t *testing.T) {
	tests := []struct {
		name     string
		duration time.Duration
		want     string
	}{
		{name: "zero", duration: 0, want: "0s"},
		{name: "sub-second", duration: 999 * time.Millisecond, want: "0s"},
		{name: "under a minute", duration: 59*time.Second + 900*time.Millisecond, want: "59s"},
		{name: "over a minute", duration: 61*time.Second + 900*time.Millisecond, want: "1m1s"},
		{name: "exactly one hour", duration: time.Hour, want: "1h0m"},
		{name: "hour truncates seconds", duration: time.Hour + 2*time.Minute + 59*time.Second, want: "1h2m"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, formatSectionDuration(tt.duration))
		})
	}
}

func TestSectionTimer_ForwardsEmbeddedMethods(t *testing.T) {
	inner := &recordingSectionLogger{}
	timer := NewSectionTimer(inner, nil)

	timer.PrintAligned("delegated")

	assert.Equal(t, []string{"aligned: delegated"}, inner.calls)
}
