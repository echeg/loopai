package phase

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/umputun/ralphex/pkg/config"
	"github.com/umputun/ralphex/pkg/executor"
	"github.com/umputun/ralphex/pkg/status"
)

type recorderSpy struct {
	tasks    []bool
	internal []internalReviewCall
	external []externalIterationCall
	done     []ReviewerCompletion
	post     []int
}

type internalReviewCall struct {
	iterations int
	endedBy    string
}

type externalIterationCall struct {
	index                                         int
	key, label, reviewerOutput, evaluatorResponse string
}

func (r *recorderSpy) TaskIteration(failed bool) { r.tasks = append(r.tasks, failed) }
func (r *recorderSpy) InternalReviewDone(iterations int, endedBy string) {
	r.internal = append(r.internal, internalReviewCall{iterations: iterations, endedBy: endedBy})
}
func (r *recorderSpy) ExternalIteration(index int, key, label, reviewerOutput, evaluatorResponse string) {
	r.external = append(r.external, externalIterationCall{
		index: index, key: key, label: label, reviewerOutput: reviewerOutput, evaluatorResponse: evaluatorResponse,
	})
}
func (r *recorderSpy) ExternalDone(done ReviewerCompletion) { r.done = append(r.done, done) }
func (r *recorderSpy) PostReviewDone(iterations int)        { r.post = append(r.post, iterations) }

func TestTaskPhaseRecordsEveryExecutorIteration(t *testing.T) {
	planFile := writeTaskPhasePlan(t, "# Plan\n### Task 1: first\n- [ ] todo")
	phase := taskPhaseFromRunner(t, taskPhaseTestOpts{
		cfg: Config{MaxIterations: 2}, planFile: planFile,
		exec: newTaskPhaseMockExecutor([]executor.Result{{Signal: status.Failed}, {Output: "retry"}}),
		log:  newMockLogger(""), retryCount: 1,
	})
	recorder := &recorderSpy{}
	phase.deps.Recorder = recorder

	require.Error(t, phase.Run(t.Context()))
	assert.Equal(t, []bool{true, false}, recorder.tasks)
}

func TestReviewPhaseRecordsCompletionReasonsAndPostReview(t *testing.T) {
	t.Run("review done", func(t *testing.T) {
		phase, _ := reviewPhaseFromRunner(t, reviewPhaseTestOpts{
			cfg:  Config{MaxIterations: 30},
			exec: newTaskPhaseMockExecutor([]executor.Result{{Signal: status.ReviewDone}}),
		})
		recorder := &recorderSpy{}
		phase.deps.Recorder = recorder

		require.NoError(t, phase.Loop(t.Context(), ""))
		assert.Equal(t, []internalReviewCall{{iterations: 1, endedBy: "review_done"}}, recorder.internal)
	})

	t.Run("no changes", func(t *testing.T) {
		phase, _ := reviewPhaseFromRunner(t, reviewPhaseTestOpts{
			cfg: Config{MaxIterations: 30}, exec: newTaskPhaseMockExecutor([]executor.Result{{Output: "clean"}}),
		})
		phase.git.deps.Git = &gitCheckerMock{HeadHashFunc: func() (string, error) { return "same", nil }}
		recorder := &recorderSpy{}
		phase.deps.Recorder = recorder

		require.NoError(t, phase.Loop(t.Context(), ""))
		assert.Equal(t, []internalReviewCall{{iterations: 1, endedBy: "no_changes"}}, recorder.internal)
	})

	t.Run("max iterations", func(t *testing.T) {
		phase, _ := reviewPhaseFromRunner(t, reviewPhaseTestOpts{
			cfg: Config{MaxIterations: 30}, exec: newTaskPhaseMockExecutor(nil),
		})
		recorder := &recorderSpy{}
		phase.deps.Recorder = recorder

		require.NoError(t, phase.Loop(t.Context(), ""))
		assert.Equal(t, []internalReviewCall{{iterations: 3, endedBy: "max_iterations"}}, recorder.internal)
	})

	t.Run("post review", func(t *testing.T) {
		phase, _ := reviewPhaseFromRunner(t, reviewPhaseTestOpts{
			cfg:  Config{MaxIterations: 30},
			exec: newTaskPhaseMockExecutor([]executor.Result{{Signal: status.ReviewDone}}),
		})
		recorder := &recorderSpy{}
		phase.deps.Recorder = recorder

		require.NoError(t, phase.Loop(t.Context(), "post-review prefix"))
		assert.Equal(t, []int{1}, recorder.post)
		assert.Empty(t, recorder.internal)
	})
}

func TestExternalReviewPhaseRecordsIterationsAndCompletion(t *testing.T) {
	reviewer := newTaskPhaseMockExecutor([]executor.Result{{Output: "finding details"}})
	evaluator := newTaskPhaseMockExecutor([]executor.Result{{Output: "all clear", Signal: status.ExternalReviewDone}})
	phase, _ := externalReviewPhaseFromRunner(t, externalReviewPhaseTestOpts{
		cfg: Config{MaxIterations: 30}, review: evaluator,
		reviewers: []ExternalReviewer{{
			Tool: config.ExternalReviewToolCodex, ModelSpec: "gpt:high",
			DisplayName: "codex (gpt:high)", Exec: reviewer,
		}},
	})
	recorder := &recorderSpy{}
	phase.deps.Recorder = recorder

	_, err := phase.Run(t.Context())
	require.NoError(t, err)
	require.Len(t, recorder.external, 1)
	assert.Equal(t, externalIterationCall{
		index: 1, key: "codex:gpt:high", label: "codex",
		reviewerOutput: "finding details", evaluatorResponse: "all clear",
	}, recorder.external[0])
	require.Len(t, recorder.done, 1)
	assert.Equal(t, "done", recorder.done[0].EndedBy)
	assert.Equal(t, "gpt:high", recorder.done[0].Reviewer.ModelSpec)
	assert.GreaterOrEqual(t, recorder.done[0].Duration, time.Duration(0))
}

func TestPhasesAllowNilRecorder(t *testing.T) {
	planFile := writeTaskPhasePlan(t, "# Plan\n### Task 1: done\n- [x] done")
	task := taskPhaseFromRunner(t, taskPhaseTestOpts{
		cfg: Config{MaxIterations: 1}, planFile: planFile,
		exec: newTaskPhaseMockExecutor([]executor.Result{{Signal: status.Completed}}), log: newMockLogger(""),
	})
	task.deps.Recorder = nil
	require.NoError(t, task.Run(t.Context()))

	review, _ := reviewPhaseFromRunner(t, reviewPhaseTestOpts{
		cfg: Config{MaxIterations: 30}, exec: newTaskPhaseMockExecutor([]executor.Result{{Signal: status.ReviewDone}}),
	})
	review.deps.Recorder = nil
	require.NoError(t, review.Loop(t.Context(), ""))

	external, _ := externalReviewPhaseFromRunner(t, externalReviewPhaseTestOpts{
		cfg: Config{MaxIterations: 30}, external: newTaskPhaseMockExecutor([]executor.Result{{Output: "clean"}}),
		review: newTaskPhaseMockExecutor([]executor.Result{{Signal: status.ExternalReviewDone}}),
	})
	external.deps.Recorder = nil
	_, err := external.Run(t.Context())
	require.NoError(t, err)
}
