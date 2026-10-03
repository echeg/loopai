package phase

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/ralphex/pkg/executor"
	"github.com/umputun/ralphex/pkg/git"
	"github.com/umputun/ralphex/pkg/status"
)

// finalizeRepo is a plan-branch checkout with a bare origin and an upstream clone that advances
// origin's master branch.
type finalizeRepo struct {
	dir      string
	upstream string
	svc      *git.Service
}

type nopGitLogger struct{}

func (nopGitLogger) Printf(string, ...any) (int, error) { return 0, nil }

func setupFinalizeRepo(t *testing.T) finalizeRepo {
	t.Helper()
	dir := t.TempDir()
	finalizeGit(t, dir, "init")
	finalizeGit(t, dir, "checkout", "-B", "master")
	configureFinalizeGitUser(t, dir)
	commitFinalizeFile(t, dir, "base.txt", "base\n", "add base")
	commitFinalizeFile(t, dir, "feature.txt", "feature\n", "add feature file")

	remote := t.TempDir()
	finalizeGit(t, remote, "init", "--bare")
	finalizeGit(t, remote, "symbolic-ref", "HEAD", "refs/heads/master")
	finalizeGit(t, dir, "remote", "add", "origin", remote)
	finalizeGit(t, dir, "push", "origin", "master")
	finalizeGit(t, dir, "checkout", "-b", "feature")
	commitFinalizeFile(t, dir, "work.txt", "plan work\n", "plan work")

	upstream := t.TempDir()
	finalizeGit(t, upstream, "clone", remote, ".")
	configureFinalizeGitUser(t, upstream)

	svc, err := git.NewService(dir, nopGitLogger{})
	require.NoError(t, err)
	return finalizeRepo{dir: dir, upstream: upstream, svc: svc}
}

func configureFinalizeGitUser(t *testing.T, dir string) {
	t.Helper()
	finalizeGit(t, dir, "config", "user.email", "test@test.com")
	finalizeGit(t, dir, "config", "user.name", "test")
	finalizeGit(t, dir, "config", "commit.gpgsign", "false")
}

// advanceBase commits name in the upstream clone, pushes it to origin master, and returns the commit.
func (r finalizeRepo) advanceBase(t *testing.T, name, content string) string {
	t.Helper()
	commitFinalizeFile(t, r.upstream, name, content, "base change to "+name)
	finalizeGit(t, r.upstream, "push", "origin", "master")
	return r.rev(t, r.upstream, "HEAD")
}

func (r finalizeRepo) head(t *testing.T) string {
	t.Helper()
	return r.rev(t, r.dir, "HEAD")
}

func (r finalizeRepo) rev(t *testing.T, dir, ref string) string {
	t.Helper()
	return strings.TrimSpace(finalizeGit(t, dir, "rev-parse", ref))
}

func (r finalizeRepo) read(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(r.dir, name)) //nolint:gosec // test reads files under t.TempDir
	require.NoError(t, err)
	return string(data)
}

func (r finalizeRepo) write(t *testing.T, name, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(r.dir, name), []byte(content), 0o600))
}

func (r finalizeRepo) mergeInProgress() bool {
	cmd := exec.Command("git", "rev-parse", "--verify", "--quiet", "MERGE_HEAD")
	cmd.Dir = r.dir
	return cmd.Run() == nil
}

func finalizeGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
	return string(out)
}

func commitFinalizeFile(t *testing.T, dir, name, content, msg string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600))
	finalizeGit(t, dir, "add", "--", name)
	finalizeGit(t, dir, "commit", "-m", msg)
}

// recordingFinalizePrompts records the conflicted paths each finalize prompt was rendered with.
type recordingFinalizePrompts struct {
	mu    sync.Mutex
	calls [][]string
}

func (p *recordingFinalizePrompts) FinalizePrompt(conflicts []string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, append([]string(nil), conflicts...))
	return "finalize prompt"
}

type finalizeTestOpts struct {
	cfg  Config
	git  FinalizeGit
	exec func(ctx context.Context) executor.Result
}

func newFinalizeTestPhase(t *testing.T, opts finalizeTestOpts) (*FinalizePhase, *mockLogger, *recordingFinalizePrompts, *executorMock) {
	t.Helper()
	if opts.cfg.AppConfig == nil {
		opts.cfg.AppConfig = testAppConfig(t)
	}
	if opts.cfg.FinalizeBase == "" {
		opts.cfg.FinalizeBase = "master"
	}
	log := newMockLogger("progress.txt")
	prompts := &recordingFinalizePrompts{}
	run := opts.exec
	if run == nil {
		run = func(context.Context) executor.Result { return executor.Result{Signal: SignalFinalizeDone} }
	}
	mock := &executorMock{RunFunc: func(ctx context.Context, _ string) executor.Result { return run(ctx) }}
	p := NewFinalizePhase(FinalizePhaseOpts{
		Cfg: opts.cfg, Log: log, Exec: mock, Policy: newTestPolicy(opts.cfg, log), Prompts: prompts,
		Deps: &Deps{FinalizeGit: opts.git}, PhaseHolder: &status.PhaseHolder{},
	})
	return p, log, prompts, mock
}

// conflictingFinalizeRepo makes the plan branch and origin master both change base.txt, with origin
// also adding clean.txt, so the merge leaves base.txt conflicted and clean.txt merged.
func conflictingFinalizeRepo(t *testing.T) (finalizeRepo, string, string) {
	t.Helper()
	r := setupFinalizeRepo(t)
	commitFinalizeFile(t, r.dir, "base.txt", "plan side\n", "plan edits base")
	commitFinalizeFile(t, r.upstream, "clean.txt", "clean\n", "upstream clean")
	base := r.advanceBase(t, "base.txt", "upstream side\n")
	return r, r.head(t), base
}

// resolveConflict writes the resolution, stages only base.txt, and commits the merge.
func resolveConflict(t *testing.T, r finalizeRepo) {
	t.Helper()
	r.write(t, "base.txt", "plan side\nupstream side\n")
	finalizeGit(t, r.dir, "add", "--", "base.txt")
	finalizeGit(t, r.dir, "commit", "--no-edit")
}

func TestFinalizePhase_SkippedWhenDisabled(t *testing.T) {
	p, log, _, mock := newFinalizeTestPhase(t, finalizeTestOpts{cfg: Config{FinalizeEnabled: false}})

	outcome, err := p.Run(t.Context())

	require.NoError(t, err)
	assert.Equal(t, FinalizeSkipped, outcome.Status)
	assert.Empty(t, mock.RunCalls())
	assert.Empty(t, log.PrintSectionCalls())
}

func TestFinalizePhase_UpToDate(t *testing.T) {
	r := setupFinalizeRepo(t)
	head := r.head(t)
	p, log, prompts, mock := newFinalizeTestPhase(t, finalizeTestOpts{cfg: Config{FinalizeEnabled: true}, git: r.svc})

	outcome, err := p.Run(t.Context())

	require.NoError(t, err)
	assert.Equal(t, FinalizeUpToDate, outcome.Status)
	assert.Equal(t, "origin/master", outcome.Base)
	assert.Equal(t, r.rev(t, r.dir, "origin/master"), outcome.BaseSHA)
	assert.Equal(t, head, r.head(t))
	assert.Len(t, mock.RunCalls(), 1, "validation session runs even when the branch is up to date")
	assert.Equal(t, [][]string{nil}, prompts.calls)
	assert.Equal(t, status.PhaseFinalize, p.phaseHolder.Get())
	assertFinalizeSectionPrinted(t, log)
	assertLogContains(t, log, "finalize: %s")
}

func TestFinalizePhase_CleanMergeCommittedThenValidated(t *testing.T) {
	r := setupFinalizeRepo(t)
	head := r.head(t)
	base := r.advanceBase(t, "upstream.txt", "upstream\n")
	var headDuringSession string
	p, _, prompts, _ := newFinalizeTestPhase(t, finalizeTestOpts{
		cfg: Config{FinalizeEnabled: true, FinalizeBase: "origin/master"}, git: r.svc,
		exec: func(context.Context) executor.Result {
			headDuringSession = r.head(t)
			return executor.Result{Output: "all validation passed", Signal: SignalFinalizeDone}
		},
	})

	outcome, err := p.Run(t.Context())

	require.NoError(t, err)
	assert.Equal(t, FinalizeMerged, outcome.Status)
	assert.Equal(t, "origin/master", outcome.Base, "an origin/ prefix on the base is stripped once")
	assert.Equal(t, base, outcome.BaseSHA)
	assert.Empty(t, outcome.Files)
	assert.Equal(t, [][]string{nil}, prompts.calls)
	assert.Equal(t, r.head(t), headDuringSession, "the clean merge is committed before validation")
	parents := strings.Fields(finalizeGit(t, r.dir, "rev-list", "--parents", "-n", "1", "HEAD"))[1:]
	assert.Equal(t, []string{head, base}, parents)
	assert.Equal(t, "Merge remote-tracking branch 'origin/master'",
		strings.TrimSpace(finalizeGit(t, r.dir, "log", "-1", "--format=%s")))
	assert.False(t, r.mergeInProgress())
}

func TestFinalizePhase_ConflictResolved(t *testing.T) {
	r, head, base := conflictingFinalizeRepo(t)
	p, log, prompts, _ := newFinalizeTestPhase(t, finalizeTestOpts{
		cfg: Config{FinalizeEnabled: true}, git: r.svc,
		exec: func(context.Context) executor.Result {
			resolveConflict(t, r)
			return executor.Result{Signal: SignalFinalizeDone}
		},
	})

	outcome, err := p.Run(t.Context())

	require.NoError(t, err)
	assert.Equal(t, FinalizeResolved, outcome.Status, outcome.Reason)
	assert.Equal(t, []string{"base.txt"}, outcome.Files)
	assert.Equal(t, base, outcome.BaseSHA)
	assert.Equal(t, [][]string{{"base.txt"}}, prompts.calls)
	parents := strings.Fields(finalizeGit(t, r.dir, "rev-list", "--parents", "-n", "1", "HEAD"))[1:]
	assert.Equal(t, []string{head, base}, parents)
	assert.Equal(t, "clean\n", r.read(t, "clean.txt"))
	assert.Equal(t, "merged origin/master (1 file resolved)", outcome.Summary())
	assertLogContains(t, log, "finalize: %s")
}

func TestFinalizePhase_RejectedResultsRestorePreMergeHead(t *testing.T) {
	tests := []struct {
		name       string
		conflict   bool
		session    func(t *testing.T, r finalizeRepo) executor.Result
		wantReason string
		wantFiles  []string
	}{
		{
			name: "validation failure after a clean merge", conflict: false,
			session: func(*testing.T, finalizeRepo) executor.Result {
				return executor.Result{Output: "go test failed\n" + SignalFinalizeBlocked + "\ngo test ./... failed in pkg/foo",
					Signal: SignalFinalizeBlocked}
			},
			wantReason: "go test ./... failed in pkg/foo",
		},
		{
			name: "missing signal after a committed resolution", conflict: true,
			session: func(t *testing.T, r finalizeRepo) executor.Result {
				resolveConflict(t, r)
				return executor.Result{Output: "resolved"}
			},
			wantReason: "session ended without FINALIZE_DONE", wantFiles: []string{"base.txt"},
		},
		{
			name: "blocked conflict left uncommitted", conflict: true,
			session: func(t *testing.T, r finalizeRepo) executor.Result {
				r.write(t, "base.txt", "half resolved\n")
				finalizeGit(t, r.dir, "add", "--", "base.txt")
				return executor.Result{Output: SignalFinalizeBlocked + " both sides rename the same API",
					Signal: SignalFinalizeBlocked}
			},
			wantReason: "both sides rename the same API", wantFiles: []string{"base.txt"},
		},
		{
			name: "resolution that tampers with an unrelated path", conflict: true,
			session: func(t *testing.T, r finalizeRepo) executor.Result {
				r.write(t, "base.txt", "resolved\n")
				r.write(t, "feature.txt", "tampered\n")
				finalizeGit(t, r.dir, "add", "--", "base.txt", "feature.txt")
				finalizeGit(t, r.dir, "commit", "--no-edit")
				return executor.Result{Signal: SignalFinalizeDone}
			},
			wantReason: "changed paths outside the conflicted set: feature.txt", wantFiles: []string{"base.txt"},
		},
		{
			name: "merge signaled done but never committed", conflict: true,
			session: func(t *testing.T, r finalizeRepo) executor.Result {
				r.write(t, "base.txt", "resolved\n")
				finalizeGit(t, r.dir, "add", "--", "base.txt")
				return executor.Result{Signal: SignalFinalizeDone}
			},
			wantReason: "merge left uncommitted", wantFiles: []string{"base.txt"},
		},
		{
			name: "extra commit after the merge", conflict: true,
			session: func(t *testing.T, r finalizeRepo) executor.Result {
				resolveConflict(t, r)
				commitFinalizeFile(t, r.dir, "fix.txt", "fix\n", "fix validation")
				return executor.Result{Signal: SignalFinalizeDone}
			},
			wantReason: "HEAD is not a single merge commit of origin/master into the plan branch",
			wantFiles:  []string{"base.txt"},
		},
		{
			name: "dirty tree after the session", conflict: false,
			session: func(t *testing.T, r finalizeRepo) executor.Result {
				r.write(t, "work.txt", "uncommitted\n")
				return executor.Result{Signal: SignalFinalizeDone}
			},
			wantReason: "working tree has uncommitted changes after the session",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var r finalizeRepo
			var head string
			if tc.conflict {
				r, head, _ = conflictingFinalizeRepo(t)
			} else {
				r = setupFinalizeRepo(t)
				head = r.head(t)
				r.advanceBase(t, "upstream.txt", "upstream\n")
			}
			p, _, _, _ := newFinalizeTestPhase(t, finalizeTestOpts{
				cfg: Config{FinalizeEnabled: true}, git: r.svc,
				exec: func(context.Context) executor.Result { return tc.session(t, r) },
			})

			outcome, err := p.Run(t.Context())

			require.NoError(t, err)
			assert.Equal(t, FinalizeBlocked, outcome.Status)
			assert.Equal(t, tc.wantReason, outcome.Reason)
			assert.Equal(t, tc.wantFiles, outcome.Files)
			assert.Equal(t, head, r.head(t), "the pre-merge HEAD is restored")
			assert.False(t, r.mergeInProgress())
			assert.Equal(t, "feature\n", r.read(t, "feature.txt"))
		})
	}
}

func TestFinalizePhase_UpToDateBranchMustStayUnchanged(t *testing.T) {
	r := setupFinalizeRepo(t)
	head := r.head(t)
	p, _, _, _ := newFinalizeTestPhase(t, finalizeTestOpts{
		cfg: Config{FinalizeEnabled: true}, git: r.svc,
		exec: func(context.Context) executor.Result {
			commitFinalizeFile(t, r.dir, "fix.txt", "fix\n", "fix")
			return executor.Result{Signal: SignalFinalizeDone}
		},
	})

	outcome, err := p.Run(t.Context())

	require.NoError(t, err)
	assert.Equal(t, FinalizeBlocked, outcome.Status)
	assert.Equal(t, "session committed changes to an up-to-date branch", outcome.Reason)
	assert.Equal(t, head, r.head(t))
}

func TestFinalizePhase_RestoreFailureIsReported(t *testing.T) {
	r := setupFinalizeRepo(t)
	r.advanceBase(t, "feature.txt", "upstream rewrite\n")
	p, log, _, _ := newFinalizeTestPhase(t, finalizeTestOpts{
		cfg: Config{FinalizeEnabled: true}, git: r.svc,
		exec: func(context.Context) executor.Result {
			// a local edit to a file the merge changed makes git reset --keep refuse
			r.write(t, "feature.txt", "local edit\n")
			return executor.Result{Signal: SignalFinalizeDone}
		},
	})

	outcome, err := p.Run(t.Context())

	require.NoError(t, err)
	assert.Equal(t, FinalizeBlocked, outcome.Status)
	assert.Contains(t, outcome.Reason, "working tree has uncommitted changes after the session; restore failed:")
	assert.Equal(t, "local edit\n", r.read(t, "feature.txt"))
	assertLogContains(t, log, "warning: finalize could not restore")
}

func TestFinalizePhase_SessionFailuresBlock(t *testing.T) {
	tests := []struct {
		name       string
		result     ExecutionResult
		wantReason string
	}{
		{name: "timeout", result: ExecutionResult{TimedOut: true}, wantReason: "finalize session timed out"},
		{name: "executor error", result: ExecutionResult{Result: executor.Result{Error: errors.New("boom")}},
			wantReason: "claude session failed: boom"},
		{name: "limit pattern", result: ExecutionResult{Result: executor.Result{
			Error: &executor.LimitPatternError{Pattern: "limit", HelpCmd: "usage"}}},
			wantReason: "claude session stopped:"},
		{name: "blocked without reason", result: ExecutionResult{Result: executor.Result{
			Output: SignalFinalizeBlocked, Signal: SignalFinalizeBlocked}},
			wantReason: "session reported FINALIZE_BLOCKED without a reason"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := setupFinalizeRepo(t)
			head := r.head(t)
			r.advanceBase(t, "upstream.txt", "upstream\n")
			p, log, _, _ := newFinalizeTestPhase(t, finalizeTestOpts{cfg: Config{FinalizeEnabled: true}, git: r.svc})
			p.policy = newScriptedTestPolicy(log, tc.result)

			outcome, err := p.Run(t.Context())

			require.NoError(t, err)
			assert.Equal(t, FinalizeBlocked, outcome.Status)
			assert.Contains(t, outcome.Reason, tc.wantReason)
			assert.Equal(t, head, r.head(t))
		})
	}
}

func TestFinalizePhase_CancellationRestoresAndPropagates(t *testing.T) {
	r, head, _ := conflictingFinalizeRepo(t)
	ctx, cancel := context.WithCancel(t.Context())
	p, _, _, _ := newFinalizeTestPhase(t, finalizeTestOpts{
		cfg: Config{FinalizeEnabled: true}, git: r.svc,
		exec: func(context.Context) executor.Result {
			cancel()
			return executor.Result{Error: context.Canceled}
		},
	})

	_, err := p.Run(ctx)

	require.ErrorIs(t, err, context.Canceled)
	assert.False(t, r.mergeInProgress(), "the merge is aborted even though the run was canceled")
	assert.Equal(t, head, r.head(t))
}

func TestFinalizePhase_BlockedBeforeMerge(t *testing.T) {
	t.Run("no git repository", func(t *testing.T) {
		p, _, _, mock := newFinalizeTestPhase(t, finalizeTestOpts{cfg: Config{FinalizeEnabled: true}})
		outcome, err := p.Run(t.Context())
		require.NoError(t, err)
		assert.Equal(t, FinalizeBlocked, outcome.Status)
		assert.Equal(t, "no git repository available", outcome.Reason)
		assert.Empty(t, mock.RunCalls())
	})

	t.Run("dirty tree", func(t *testing.T) {
		r := setupFinalizeRepo(t)
		r.write(t, "work.txt", "uncommitted\n")
		p, _, _, mock := newFinalizeTestPhase(t, finalizeTestOpts{cfg: Config{FinalizeEnabled: true}, git: r.svc})
		outcome, err := p.Run(t.Context())
		require.NoError(t, err)
		assert.Equal(t, FinalizeBlocked, outcome.Status)
		assert.Equal(t, "working tree has uncommitted changes", outcome.Reason)
		assert.Empty(t, mock.RunCalls())
	})

	t.Run("unknown base branch", func(t *testing.T) {
		r := setupFinalizeRepo(t)
		head := r.head(t)
		p, _, _, mock := newFinalizeTestPhase(t, finalizeTestOpts{
			cfg: Config{FinalizeEnabled: true, FinalizeBase: "no-such-branch"}, git: r.svc,
		})
		outcome, err := p.Run(t.Context())
		require.NoError(t, err)
		assert.Equal(t, FinalizeBlocked, outcome.Status)
		assert.Contains(t, outcome.Reason, "fetch origin/no-such-branch")
		assert.Equal(t, "origin/no-such-branch", outcome.Base)
		assert.Empty(t, mock.RunCalls())
		assert.Equal(t, head, r.head(t))
	})

	t.Run("missing origin", func(t *testing.T) {
		r := setupFinalizeRepo(t)
		finalizeGit(t, r.dir, "remote", "remove", "origin")
		p, _, _, mock := newFinalizeTestPhase(t, finalizeTestOpts{cfg: Config{FinalizeEnabled: true}, git: r.svc})
		outcome, err := p.Run(t.Context())
		require.NoError(t, err)
		assert.Equal(t, FinalizeBlocked, outcome.Status)
		assert.Contains(t, outcome.Reason, "fetch origin/master")
		assert.Empty(t, mock.RunCalls())
	})
}

func TestFinalizeOutcome_Summary(t *testing.T) {
	tests := []struct {
		outcome FinalizeOutcome
		want    string
	}{
		{FinalizeOutcome{Status: FinalizeSkipped}, "skipped"},
		{FinalizeOutcome{}, "skipped"},
		{FinalizeOutcome{Status: FinalizeUpToDate, Base: "origin/main"}, "up to date with origin/main"},
		{FinalizeOutcome{Status: FinalizeMerged, Base: "origin/main"}, "merged origin/main"},
		{FinalizeOutcome{Status: FinalizeResolved, Base: "origin/main", Files: []string{"a", "b"}}, "merged origin/main (2 files resolved)"},
		{FinalizeOutcome{Status: FinalizeBlocked, Reason: "tests failed"}, "blocked: tests failed"},
	}
	for _, tc := range tests {
		assert.Equal(t, tc.want, tc.outcome.Summary())
	}
}

func assertFinalizeSectionPrinted(t *testing.T, log *mockLogger) {
	t.Helper()
	for _, call := range log.PrintSectionCalls() {
		if strings.Contains(call.Section.Label, "finalize") {
			return
		}
	}
	assert.Fail(t, "finalize section header was not printed")
}
