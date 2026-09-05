package processor

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/ralphex/pkg/config"
	"github.com/umputun/ralphex/pkg/executor"
	"github.com/umputun/ralphex/pkg/processor/phase"
	"github.com/umputun/ralphex/pkg/status"
)

type checkpointMemoryStore struct {
	mu        sync.Mutex
	cp        ReviewCheckpoint
	found     bool
	loadErr   error
	saveErr   error
	removeErr error
	saves     []ReviewCheckpoint
	removes   int
}

func (s *checkpointMemoryStore) Load() (ReviewCheckpoint, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cp, s.found, s.loadErr
}

func (s *checkpointMemoryStore) Save(cp ReviewCheckpoint) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saves = append(s.saves, cp)
	if s.saveErr == nil {
		s.cp, s.found = cp, true
	}
	return s.saveErr
}

func (s *checkpointMemoryStore) Remove() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removes++
	if s.removeErr == nil {
		s.cp, s.found = ReviewCheckpoint{}, false
	}
	return s.removeErr
}

type checkpointGit struct {
	head        string
	heads       []string
	diff        string
	branch      string
	headErr     error
	diffErr     error
	contains    bool
	containsErr error
	mu          sync.Mutex
}

func (g *checkpointGit) HeadHash() (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.headErr != nil {
		return "", g.headErr
	}
	if len(g.heads) > 0 {
		head := g.heads[0]
		g.heads = g.heads[1:]
		return head, nil
	}
	return g.head, nil
}
func (g *checkpointGit) DiffFingerprint() (string, error) { return g.diff, g.diffErr }
func (g *checkpointGit) ContainsRevisionContext(context.Context, string) (bool, error) {
	return g.contains, g.containsErr
}
func (g *checkpointGit) CurrentBranch() (string, error) { return g.branch, nil }

type checkpointTask struct{ runs int }

func (p *checkpointTask) Run(context.Context) error { p.runs++; return nil }
func (*checkpointTask) ValidatePlanHasTasks() error { return nil }

type checkpointReview struct {
	first int
	loops []string
}

func (p *checkpointReview) First(context.Context) error { p.first++; return nil }
func (p *checkpointReview) Loop(_ context.Context, prefix string) error {
	p.loops = append(p.loops, prefix)
	return nil
}

type checkpointExternal struct {
	enabled     bool
	completed   int
	hadFindings bool
	run         func(context.Context) (phase.ExternalReviewOutcome, error)
}

func (p *checkpointExternal) Enabled() bool { return p.enabled }
func (*checkpointExternal) Label() string   { return "reviewers" }
func (p *checkpointExternal) SetResume(completed int, hadFindings bool) {
	p.completed, p.hadFindings = completed, hadFindings
}
func (p *checkpointExternal) Run(ctx context.Context) (phase.ExternalReviewOutcome, error) {
	return p.run(ctx)
}

type checkpointFinalize struct{ runs int }

func (p *checkpointFinalize) Run(context.Context) error { p.runs++; return nil }

func newCheckpointRunner(cfg Config, store ReviewCheckpointStore, git GitChecker) (*Runner, *checkpointReview, *checkpointExternal, *checkpointFinalize) {
	log := newMockLogger()
	review := &checkpointReview{}
	external := &checkpointExternal{enabled: true}
	finalize := &checkpointFinalize{}
	task := &checkpointTask{}
	r := &Runner{
		cfg: cfg, log: log, phaseHolder: &status.PhaseHolder{}, git: git, checkpoints: store,
		phases: runnerPhases{task: task, taskValidator: task, review: review, external: external, finalize: finalize},
	}
	return r, review, external, finalize
}

func TestRunnerReviewCheckpoint_FullModeCrashAndResume(t *testing.T) {
	cfg := Config{Mode: ModeFull, PlanFile: "plan.md", ExternalReviewers: []config.ReviewerSpec{
		{Provider: "claude", ModelSpec: "opus:high"}, {Provider: "codex", ModelSpec: "gpt:high"},
	}}
	store := &checkpointMemoryStore{}
	git := &checkpointGit{head: "abc1234", branch: "feature", contains: true}

	first, firstReview, firstExternal, _ := newCheckpointRunner(cfg, store, git)
	firstExternal.run = func(ctx context.Context) (phase.ExternalReviewOutcome, error) {
		require.NoError(t, first.onReviewerDone(ctx, phase.ReviewerCompletion{Index: 0, HadFindings: true, EndedBy: "done"}))
		return phase.ExternalReviewOutcome{HadFindings: true}, errors.New("reviewer crashed")
	}
	require.ErrorContains(t, first.Run(t.Context()), "reviewer crashed")
	assert.Equal(t, 1, firstReview.first)
	require.Len(t, store.cp.Stages, 2)
	assert.Equal(t, []string{reviewStageInternal, reviewStageExternal}, []string{store.cp.Stages[0].Stage, store.cp.Stages[1].Stage})

	second, secondReview, secondExternal, secondFinalize := newCheckpointRunner(cfg, store, git)
	secondExternal.run = func(ctx context.Context) (phase.ExternalReviewOutcome, error) {
		require.NoError(t, second.onReviewerDone(ctx, phase.ReviewerCompletion{Index: 1, EndedBy: "done"}))
		return phase.ExternalReviewOutcome{HadFindings: true}, nil
	}
	require.NoError(t, second.Run(t.Context()))
	assert.Zero(t, secondReview.first, "completed internal review must be skipped")
	assert.Equal(t, 1, secondExternal.completed)
	assert.True(t, secondExternal.hadFindings)
	require.Len(t, secondReview.loops, 1, "only post-review should run")
	assert.NotEmpty(t, secondReview.loops[0])
	assert.Equal(t, 1, secondFinalize.runs)
	assert.False(t, store.found, "successful finalize must remove the checkpoint")
	require.NotEmpty(t, store.saves)
	assert.Equal(t, reviewStagePostReview, store.saves[len(store.saves)-1].Stages[len(store.saves[len(store.saves)-1].Stages)-1].Stage)
}

func TestRunnerReviewCheckpoint_TaskCommitInvalidates(t *testing.T) {
	store := &checkpointMemoryStore{found: true, cp: ReviewCheckpoint{Version: 1}}
	git := &checkpointGit{heads: []string{"old", "new"}, head: "new", branch: "feature", contains: true}
	r, review, external, _ := newCheckpointRunner(Config{Mode: ModeFull, PlanFile: "plan.md"}, store, git)
	external.enabled = false
	external.run = func(context.Context) (phase.ExternalReviewOutcome, error) { return phase.ExternalReviewOutcome{}, nil }
	require.NoError(t, r.Run(t.Context()))
	assert.Equal(t, 1, review.first, "reviews must restart after task commits")
	assert.GreaterOrEqual(t, store.removes, 1)
	assertLogContains(t, r.log.(*testLoggerMock), "review checkpoint: cleared")
}

func TestRunnerReviewCheckpoint_ReviewModesAndPostReviewResume(t *testing.T) {
	reviewers := []config.ReviewerSpec{{Provider: "codex", ModelSpec: "gpt:high"}}
	for _, tc := range []struct {
		name        string
		mode        Mode
		stages      []ReviewStage
		expectFirst int
		expectLoops int
	}{
		{name: "review mode", mode: ModeReview, stages: []ReviewStage{{Stage: reviewStageInternal, Head: "head"}, {Stage: reviewStageExternal, Reviewer: "codex:gpt:high", Head: "head"}, {Stage: reviewStagePostReview, Head: "head"}}},
		{name: "external only", mode: ModeCodexOnly, stages: []ReviewStage{{Stage: reviewStageExternal, Reviewer: "codex:gpt:high", Head: "head"}}, expectFirst: 0, expectLoops: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &checkpointMemoryStore{found: true, cp: ReviewCheckpoint{Version: 1, Mode: tc.mode, Branch: "feature", Reviewers: []string{"codex:gpt:high"}, Stages: tc.stages}}
			git := &checkpointGit{head: "head", branch: "feature", contains: true}
			r, review, external, _ := newCheckpointRunner(Config{Mode: tc.mode, ExternalReviewers: reviewers}, store, git)
			external.run = func(context.Context) (phase.ExternalReviewOutcome, error) {
				return phase.ExternalReviewOutcome{HadFindings: true}, nil
			}
			require.NoError(t, r.Run(t.Context()))
			assert.Equal(t, tc.expectFirst, review.first)
			assert.Len(t, review.loops, tc.expectLoops)
			assert.Equal(t, 1, external.completed)
		})
	}
}

func TestRunnerReviewCheckpoint_SaveGuardsAndErrors(t *testing.T) {
	t.Run("dirty tree", func(t *testing.T) {
		store := &checkpointMemoryStore{}
		git := &checkpointGit{head: "head", branch: "main", diff: "dirty"}
		r, _, _, _ := newCheckpointRunner(Config{Mode: ModeReview}, store, git)
		r.saveReviewStage(t.Context(), ReviewStage{Stage: reviewStageInternal})
		assert.Empty(t, store.saves)
		assertLogContains(t, r.log.(*testLoggerMock), "review checkpoint skipped: uncommitted changes")
	})

	t.Run("unreadable checkpoint starts fresh", func(t *testing.T) {
		store := &checkpointMemoryStore{loadErr: errors.New("corrupt")}
		git := &checkpointGit{head: "head", branch: "main", contains: true}
		r, _, _, _ := newCheckpointRunner(Config{Mode: ModeReview}, store, git)
		assert.Equal(t, reviewResume{}, r.loadReviewResume(t.Context()))
		assertLogContains(t, r.log.(*testLoggerMock), "review checkpoint unreadable")
	})

	t.Run("save error is logged", func(t *testing.T) {
		store := &checkpointMemoryStore{saveErr: errors.New("disk full")}
		git := &checkpointGit{head: "head", branch: "main"}
		r, _, _, _ := newCheckpointRunner(Config{Mode: ModeReview}, store, git)
		r.saveReviewStage(t.Context(), ReviewStage{Stage: reviewStageInternal})
		assertLogContains(t, r.log.(*testLoggerMock), "review checkpoint save failed")
	})
}

func TestRunnerReviewCheckpoint_NewWithExecutorsWiresReviewerCallback(t *testing.T) {
	store := &checkpointMemoryStore{}
	git := &checkpointGit{head: "head", branch: "feature", contains: true}
	review := newMockExecutor([]executor.Result{{Output: "done", Signal: status.ExternalReviewDone}})
	external := newMockExecutor([]executor.Result{{Output: ""}})
	cfg := Config{
		Mode: ModeCodexOnly, MaxIterations: 2,
		ExternalReviewers: []config.ReviewerSpec{{Provider: config.ExternalReviewToolCodex, ModelSpec: "gpt:high"}},
		AppConfig:         testAppConfig(t),
	}
	r := NewWithExecutors(cfg, newMockLogger(), Executors{
		Task: review, Externals: []ExternalReviewer{{Tool: config.ExternalReviewToolCodex, Exec: external}},
	}, &status.PhaseHolder{})
	r.SetGitChecker(git)
	r.SetReviewCheckpoints(store)

	require.NoError(t, r.Run(t.Context()))
	require.Len(t, store.saves, 1)
	require.Len(t, store.saves[0].Stages, 1)
	assert.Equal(t, reviewStageExternal, store.saves[0].Stages[0].Stage)
	assert.Equal(t, "codex:gpt:high", store.saves[0].Stages[0].Reviewer)
	assert.False(t, store.found, "checkpoint should be removed after finalize")
}
