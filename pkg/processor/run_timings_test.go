package processor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/umputun/ralphex/pkg/processor/phase"
)

func TestRunTimingsResumeAddsEachInvocationOnce(t *testing.T) {
	cfg := Config{Mode: ModeFull, PlanFile: "plan.md", ReportEnabled: true}
	checkpoints := &checkpointMemoryStore{}
	store := &runRecordMemoryStore{}
	git := &checkpointGit{head: "before", branch: "feature", contains: true}
	first, _, external, _ := newCheckpointRunner(cfg, checkpoints, git)
	first.SetRunRecordStore(store)
	first.SetRunTimingsSource(func() (map[string]time.Duration, time.Duration, int) {
		return map[string]time.Duration{"task": time.Second, "review": 2 * time.Second}, 3 * time.Second, 2
	})
	first.phases.task.(*checkpointTask).onRun = func() {
		first.recorder.TaskIteration(false)
		git.head = "after"
	}
	external.run = func(context.Context) (phase.ExternalReviewOutcome, error) {
		return phase.ExternalReviewOutcome{}, errors.New("interrupted")
	}
	require.ErrorContains(t, first.Run(t.Context()), "interrupted")
	started := store.record.StartedAt

	second, review, secondExternal, _ := newCheckpointRunner(cfg, checkpoints, git)
	secondExternal.enabled = false
	second.SetRunRecordStore(store)
	second.SetRunTimingsSource(func() (map[string]time.Duration, time.Duration, int) {
		return map[string]time.Duration{"review": 4 * time.Second}, time.Second, 1
	})
	second.phases.report = testReportPhase{runFunc: func(_ context.Context, facts string) (string, error) {
		assert.Contains(t, facts, "| review | 6000 |")
		assert.Contains(t, facts, "| task | 1000 |")
		assert.Contains(t, facts, "- duration_ms: 4000\n- runs: 3")
		return "", nil
	}}
	require.NoError(t, second.Run(t.Context()))
	assert.Zero(t, review.first, "resume should honor the internal-review checkpoint")
	assert.Equal(t, started, store.record.StartedAt)
	assert.Equal(t, 1, store.record.Tasks.Iterations)
	assert.Equal(t, Duration(6*time.Second), store.record.PhaseDurations["review"])
	assert.Equal(t, &ValidationRunRecord{Duration: Duration(4 * time.Second), Runs: 3}, store.record.Validation)
}
