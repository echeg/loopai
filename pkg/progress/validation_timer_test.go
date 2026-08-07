package progress

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidationTimer_MatchedCommandAndSummary(t *testing.T) {
	inner := &recordingSectionLogger{}
	timer := NewValidationTimer([]string{"go test ./...", "make lint"}, inner)

	timer.Handler()("go   test ./... -count=1", 72*time.Second)
	timer.Handler()("make test", 5*time.Second)
	timer.Handler()("make lint", 3*time.Second)
	timer.FinishRun()

	assert.Equal(t, []string{
		"print: validation: go   test ./... -count=1 took 1m12s",
		"print: validation: make lint took 3s",
		"print: validation: 1m15s (2 runs)",
	}, inner.calls)
}

func TestValidationTimer_ZeroMatchedRunsIsSilent(t *testing.T) {
	inner := &recordingSectionLogger{}
	timer := NewValidationTimer([]string{"make test"}, inner)

	timer.Handler()("make test-wrappers", time.Second)
	timer.FinishRun()

	assert.Empty(t, inner.calls)
}

func TestValidationTimer_EmptyCommandsIsDisabled(t *testing.T) {
	inner := &recordingSectionLogger{}
	timer := NewValidationTimer(nil, inner)

	assert.Nil(t, timer.Handler())
	timer.FinishRun()

	assert.Empty(t, inner.calls)
}

func TestValidationTimer_FinishRunIsIdempotent(t *testing.T) {
	inner := &recordingSectionLogger{}
	timer := NewValidationTimer([]string{"make test"}, inner)
	timer.Handler()("make test", time.Second)

	timer.FinishRun()
	timer.FinishRun()

	assert.Equal(t, []string{
		"print: validation: make test took 1s",
		"print: validation: 1s (1 runs)",
	}, inner.calls)
}

func TestValidationTimer_ConcurrentHandlerCalls(t *testing.T) {
	inner := &recordingSectionLogger{}
	timer := NewValidationTimer([]string{"go test"}, inner)
	handler := timer.Handler()
	require.NotNil(t, handler)

	const runs = 100
	var wg sync.WaitGroup
	for range runs {
		wg.Go(func() {
			handler("go test ./...", time.Second)
		})
	}
	wg.Wait()
	timer.FinishRun()

	require.Len(t, inner.calls, runs+1)
	assert.Equal(t, "print: validation: 1m40s (100 runs)", inner.calls[runs])
}
