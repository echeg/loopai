package phase

import (
	"context"
	"errors"
	"fmt"
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
	// exact-content assertions must not depend on a system-wide core.autocrlf
	finalizeGit(t, dir, "config", "core.autocrlf", "false")
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
	assertFinalizeLogged(t, log, "finalize: up to date with origin/master")
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
	assertFinalizeLogged(t, log, "finalize: merged origin/master (1 file resolved)")
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
			name: "clean merge amended with an unrelated edit", conflict: false,
			session: func(t *testing.T, r finalizeRepo) executor.Result {
				r.write(t, "feature.txt", "amended\n")
				finalizeGit(t, r.dir, "add", "--", "feature.txt")
				finalizeGit(t, r.dir, "commit", "--amend", "--no-edit")
				return executor.Result{Signal: SignalFinalizeDone}
			},
			wantReason: "changed paths outside the conflicted set: feature.txt",
		},
		{
			name: "dirty tree after the session", conflict: false,
			session: func(t *testing.T, r finalizeRepo) executor.Result {
				r.write(t, "work.txt", "uncommitted\n")
				return executor.Result{Signal: SignalFinalizeDone}
			},
			wantReason: "working tree has uncommitted changes after the session; " +
				"the session's uncommitted changes were left in the working tree",
		},
		{
			name: "blocked signal in an earlier text block wins over a later done", conflict: false,
			session: func(*testing.T, finalizeRepo) executor.Result {
				return executor.Result{Output: SignalFinalizeBlocked + "\ngo vet failed\nretrying\n" + SignalFinalizeDone,
					Signal: SignalFinalizeDone}
			},
			wantReason: "go vet failed",
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
			p, log, _, _ := newFinalizeTestPhase(t, finalizeTestOpts{
				cfg: Config{FinalizeEnabled: true}, git: r.svc,
				exec: func(context.Context) executor.Result { return tc.session(t, r) },
			})

			outcome, err := p.Run(t.Context())

			require.NoError(t, err)
			assert.Equal(t, FinalizeBlocked, outcome.Status)
			assert.Equal(t, tc.wantReason, outcome.Reason)
			assert.Equal(t, tc.wantFiles, outcome.Files)
			assertFinalizeLogged(t, log, "finalize blocked: "+tc.wantReason)
			assert.Equal(t, head, r.head(t), "the pre-merge HEAD is restored")
			assert.False(t, r.mergeInProgress())
			assert.Equal(t, "feature\n", r.read(t, "feature.txt"))
		})
	}
}

// failingSnapshotGit fails the index snapshot taken right after the merge starts.
type failingSnapshotGit struct{ FinalizeGit }

func (failingSnapshotGit) StageZeroSnapshot() (git.MergeSnapshot, error) {
	return git.MergeSnapshot{}, errors.New("index unreadable")
}

func TestFinalizePhase_FailuresAfterTheMergeStarts(t *testing.T) {
	t.Run("snapshot failure aborts the fresh merge", func(t *testing.T) {
		r, head, _ := conflictingFinalizeRepo(t)
		p, _, _, mock := newFinalizeTestPhase(t, finalizeTestOpts{
			cfg: Config{FinalizeEnabled: true}, git: failingSnapshotGit{r.svc},
		})

		outcome, err := p.Run(t.Context())

		require.NoError(t, err)
		assert.Equal(t, FinalizeBlocked, outcome.Status)
		assert.Equal(t, "snapshot merge index: index unreadable", outcome.Reason)
		assert.Empty(t, mock.RunCalls())
		assert.False(t, r.mergeInProgress())
		assert.Equal(t, head, r.head(t))
		assert.Equal(t, "plan side\n", r.read(t, "base.txt"))
	})

	t.Run("a hook rejecting the merge commit aborts the merge", func(t *testing.T) {
		r := setupFinalizeRepo(t)
		head := r.head(t)
		r.advanceBase(t, "upstream.txt", "upstream\n")
		hook := filepath.Join(r.dir, ".git", "hooks", "commit-msg")
		require.NoError(t, os.WriteFile(hook, []byte("#!/bin/sh\necho rejected by hook >&2\nexit 1\n"), 0o700)) //nolint:gosec // executable test hook
		p, _, _, mock := newFinalizeTestPhase(t, finalizeTestOpts{cfg: Config{FinalizeEnabled: true}, git: r.svc})

		outcome, err := p.Run(t.Context())

		require.NoError(t, err)
		assert.Equal(t, FinalizeBlocked, outcome.Status)
		assert.Contains(t, outcome.Reason, "commit merge:")
		assert.NotContains(t, outcome.Reason, "restore failed")
		assert.False(t, outcome.Unrestored)
		assert.Empty(t, mock.RunCalls())
		assert.False(t, r.mergeInProgress())
		assert.Equal(t, head, r.head(t))
		assert.NoFileExists(t, filepath.Join(r.dir, "upstream.txt"))
	})
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

func TestFinalizePhase_SessionMustStayOnThePlanBranch(t *testing.T) {
	t.Run("up-to-date branch left for another branch", func(t *testing.T) {
		r := setupFinalizeRepo(t)
		head := r.head(t)
		p, _, _, _ := newFinalizeTestPhase(t, finalizeTestOpts{
			cfg: Config{FinalizeEnabled: true}, git: r.svc,
			exec: func(context.Context) executor.Result {
				finalizeGit(t, r.dir, "checkout", "-b", "other")
				return executor.Result{Signal: SignalFinalizeDone}
			},
		})

		outcome, err := p.Run(t.Context())

		require.NoError(t, err)
		assert.Equal(t, FinalizeBlocked, outcome.Status)
		assert.Equal(t, "the checkout moved from feature to other; restore failed: the checkout moved from feature to other",
			outcome.Reason)
		assert.Equal(t, head, r.rev(t, r.dir, "refs/heads/feature"))
	})

	t.Run("merge committed after switching branches is not reset on the wrong branch", func(t *testing.T) {
		r, head, _ := conflictingFinalizeRepo(t)
		p, log, _, _ := newFinalizeTestPhase(t, finalizeTestOpts{
			cfg: Config{FinalizeEnabled: true}, git: r.svc,
			exec: func(context.Context) executor.Result {
				resolveConflict(t, r)
				finalizeGit(t, r.dir, "checkout", "--detach")
				return executor.Result{Signal: SignalFinalizeDone}
			},
		})

		outcome, err := p.Run(t.Context())

		require.NoError(t, err)
		assert.Equal(t, FinalizeBlocked, outcome.Status)
		assert.Equal(t, "the checkout moved from feature to a detached HEAD; "+
			"restore failed: the checkout moved from feature to a detached HEAD", outcome.Reason)
		assert.NotEqual(t, head, r.head(t), "the detached HEAD is not reset")
		assert.True(t, outcome.Unrestored)
		assertLogContains(t, log, "warning: finalize could not restore")
	})
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
	assert.True(t, outcome.Unrestored)
	assertLogContains(t, log, "warning: finalize could not restore")
}

func TestFinalizePhase_RestoreRefusesToDiscardWork(t *testing.T) {
	t.Run("an unfinished cherry-pick is left for the user", func(t *testing.T) {
		r := setupFinalizeRepo(t)
		r.advanceBase(t, "upstream.txt", "upstream\n")
		var sessionHead string
		p, log, _, _ := newFinalizeTestPhase(t, finalizeTestOpts{
			cfg: Config{FinalizeEnabled: true}, git: r.svc,
			exec: func(context.Context) executor.Result {
				finalizeGit(t, r.dir, "switch", "-c", "side", "master")
				commitFinalizeFile(t, r.dir, "feature.txt", "side\n", "side edit")
				side := r.head(t)
				finalizeGit(t, r.dir, "switch", "feature")
				commitFinalizeFile(t, r.dir, "feature.txt", "plan\n", "plan edit")
				cmd := exec.Command("git", "cherry-pick", side)
				cmd.Dir = r.dir
				require.Error(t, cmd.Run(), "the cherry-pick must stop on a conflict")
				sessionHead = r.head(t)
				return executor.Result{Signal: SignalFinalizeDone}
			},
		})

		outcome, err := p.Run(t.Context())

		require.NoError(t, err)
		assert.Equal(t, FinalizeBlocked, outcome.Status)
		assert.Equal(t, "a cherry-pick is in progress; restore failed: a cherry-pick is in progress", outcome.Reason)
		assert.Equal(t, sessionHead, r.head(t), "nothing is reset while the cherry-pick is unfinished")
		assert.FileExists(t, filepath.Join(r.dir, ".git", "CHERRY_PICK_HEAD"))
		assert.True(t, outcome.Unrestored)
		assertLogContains(t, log, "warning: finalize could not restore")
	})

	t.Run("staged changes outside the merge keep the merge in place", func(t *testing.T) {
		r, _, _ := conflictingFinalizeRepo(t)
		p, log, _, _ := newFinalizeTestPhase(t, finalizeTestOpts{
			cfg: Config{FinalizeEnabled: true}, git: r.svc,
			exec: func(context.Context) executor.Result {
				r.write(t, "work.txt", "staged extra\n")
				finalizeGit(t, r.dir, "add", "--", "work.txt")
				return executor.Result{Output: SignalFinalizeBlocked + " cannot resolve", Signal: SignalFinalizeBlocked}
			},
		})

		outcome, err := p.Run(t.Context())

		require.NoError(t, err)
		assert.Equal(t, FinalizeBlocked, outcome.Status)
		assert.Contains(t, outcome.Reason, "cannot resolve; restore failed: abort merge: refuse to abort merge")
		assert.True(t, r.mergeInProgress(), "the merge is kept rather than discarding the staged work")
		assert.Equal(t, "staged extra\n", r.read(t, "work.txt"))
		assert.True(t, outcome.Unrestored)
		assertLogContains(t, log, "warning: finalize could not restore")
	})

	t.Run("a merge the session started on an up-to-date branch is not aborted", func(t *testing.T) {
		r := setupFinalizeRepo(t)
		head := r.head(t)
		p, log, _, _ := newFinalizeTestPhase(t, finalizeTestOpts{
			cfg: Config{FinalizeEnabled: true}, git: r.svc,
			exec: func(context.Context) executor.Result {
				finalizeGit(t, r.dir, "switch", "-c", "side", "master")
				commitFinalizeFile(t, r.dir, "side.txt", "side\n", "side edit")
				finalizeGit(t, r.dir, "switch", "feature")
				finalizeGit(t, r.dir, "merge", "--no-commit", "--no-ff", "side")
				return executor.Result{Signal: SignalFinalizeDone}
			},
		})

		outcome, err := p.Run(t.Context())

		require.NoError(t, err)
		assert.Equal(t, FinalizeBlocked, outcome.Status)
		assert.Equal(t, "merge left uncommitted; restore failed: a merge is in progress", outcome.Reason)
		assert.True(t, outcome.Unrestored)
		assert.True(t, r.mergeInProgress(), "finalize aborts only a merge it started")
		assert.Equal(t, head, r.head(t))
		assertLogContains(t, log, "warning: finalize could not restore")
	})
}

// cancelingGit cancels the run during one repository step of the sync and reports the cancellation.
type cancelingGit struct {
	FinalizeGit
	step   string
	cancel context.CancelFunc
}

func (g cancelingGit) FetchContext(ctx context.Context, remote, branch string) (string, error) {
	if g.step == "fetch" {
		g.cancel()
		return "", context.Canceled
	}
	return g.FinalizeGit.FetchContext(ctx, remote, branch) //nolint:wrapcheck // test double
}

func (g cancelingGit) MergeRemoteNoCommitContext(ctx context.Context, rev string) (git.MergeResult, error) {
	if g.step == "merge" {
		g.cancel()
		return git.MergeResult{}, context.Canceled
	}
	return g.FinalizeGit.MergeRemoteNoCommitContext(ctx, rev) //nolint:wrapcheck // test double
}

func (g cancelingGit) CommitMergeContext(ctx context.Context, message string) error {
	if g.step == "commit" {
		g.cancel()
		return context.Canceled
	}
	return g.FinalizeGit.CommitMergeContext(ctx, message) //nolint:wrapcheck // test double
}

func TestFinalizePhase_CancellationDuringGitStepsPropagates(t *testing.T) {
	for _, step := range []string{"fetch", "merge", "commit"} {
		t.Run(step, func(t *testing.T) {
			r := setupFinalizeRepo(t)
			head := r.head(t)
			r.advanceBase(t, "upstream.txt", "upstream\n")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			p, _, _, mock := newFinalizeTestPhase(t, finalizeTestOpts{
				cfg: Config{FinalizeEnabled: true}, git: cancelingGit{FinalizeGit: r.svc, step: step, cancel: cancel},
			})

			_, err := p.Run(ctx)

			require.ErrorIs(t, err, context.Canceled)
			assert.Empty(t, mock.RunCalls())
			assert.False(t, r.mergeInProgress(), "a canceled sync leaves no merge behind")
			assert.Equal(t, head, r.head(t))
			assert.NoFileExists(t, filepath.Join(r.dir, "upstream.txt"))
		})
	}
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

	t.Run("checkout on the base branch", func(t *testing.T) {
		r := setupFinalizeRepo(t)
		finalizeGit(t, r.dir, "checkout", "master")
		head := r.head(t)
		r.advanceBase(t, "upstream.txt", "upstream\n")
		p, _, _, mock := newFinalizeTestPhase(t, finalizeTestOpts{cfg: Config{FinalizeEnabled: true}, git: r.svc})
		outcome, err := p.Run(t.Context())
		require.NoError(t, err)
		assert.Equal(t, FinalizeBlocked, outcome.Status)
		assert.Equal(t, "the checkout is on the base branch master", outcome.Reason)
		assert.Empty(t, mock.RunCalls())
		assert.Equal(t, head, r.head(t), "the local base branch is never merged into")
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

// assertFinalizeLogged checks the rendered text of a progress line, not just its format string.
func assertFinalizeLogged(t *testing.T, log *mockLogger, want string) {
	t.Helper()
	var got []string
	for _, call := range log.PrintCalls() {
		line := fmt.Sprintf(call.Format, call.Args...)
		if line == want {
			return
		}
		got = append(got, line)
	}
	assert.Failf(t, "missing log", "expected log line %q, got %q", want, got)
}
