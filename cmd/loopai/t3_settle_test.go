package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/ralphex/pkg/git"
	"github.com/umputun/ralphex/pkg/t3"
)

func TestMergeSettlesT3FeatureAfterWorktreeRemoval(t *testing.T) {
	dir := setupTestRepo(t)
	worktree := filepath.Join(t.TempDir(), "feature")
	runGit(t, dir, "worktree", "add", "-b", "feature", worktree)
	runGit(t, worktree, "commit", "--allow-empty", "-m", "feature work")
	svc, err := git.NewService(worktree, noopLogger())
	require.NoError(t, err)
	api := &t3ShellDispatcher{shell: t3.Shell{
		// the project can be registered at the checkout --merge is about to remove.
		Projects: []t3.Project{{ID: "project", WorkspaceRoot: worktree}},
		Threads: []t3.Thread{
			{ID: "feature-thread", ProjectID: "project", Branch: new("feature")},
			{ID: "coordinator", ProjectID: "project", Branch: new("master")},
		},
	}}
	original := newT3SettleDispatcher
	t.Cleanup(func() { newT3SettleDispatcher = original })
	newT3SettleDispatcher = func(context.Context, func(string) string) (t3.Dispatcher, error) {
		assert.NoDirExists(t, worktree, "settlement happens only after successful cleanup")
		assert.False(t, branchExists(t, dir, "feature"))
		return api, nil
	}
	t.Setenv(t3.EnvThreadID, "coordinator")
	require.NoError(t, runMergeCommand(t.Context(), svc, "master", closeoutTarget{}, nil, io.Discard))
	require.Len(t, api.commands, 1)
	assert.Equal(t, "feature-thread", api.commands[0].(*t3.ThreadSettle).ThreadID)
}

func TestMergeT3SettlementIsBestEffort(t *testing.T) {
	for _, tc := range []struct {
		name  string
		dirty bool
	}{
		{name: "authentication failure preserves successful merge"},
		{name: "refused merge never authenticates", dirty: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := setupTestRepo(t)
			runGit(t, dir, "checkout", "-b", "feature")
			runGit(t, dir, "commit", "--allow-empty", "-m", "feature work")
			svc, err := git.NewService(dir, noopLogger())
			require.NoError(t, err)
			if tc.dirty {
				runGit(t, dir, "update-index", "--force-remove", "README.md")
			}
			original := newT3SettleDispatcher
			t.Cleanup(func() { newT3SettleDispatcher = original })
			calls := 0
			newT3SettleDispatcher = func(context.Context, func(string) string) (t3.Dispatcher, error) {
				calls++
				return nil, errors.New("T3 unavailable")
			}
			err = runMergeCommand(t.Context(), svc, "master", closeoutTarget{}, nil, io.Discard)
			if tc.dirty {
				require.Error(t, err)
				assert.Zero(t, calls)
				assert.True(t, branchExists(t, dir, "feature"))
				return
			}
			require.NoError(t, err)
			assert.Equal(t, 1, calls)
			assert.False(t, branchExists(t, dir, "feature"))
		})
	}
}

func TestPreparedT3SettlementRechecksGit(t *testing.T) {
	for _, change := range []string{"unchanged", "feature advanced", "base moved back", "canceled"} {
		t.Run(change, func(t *testing.T) {
			repo := setupACPMergeRepo(t)
			runGit(t, repo.primary, "merge", "feature")
			api := &t3ShellDispatcher{shell: t3.Shell{
				Projects: []t3.Project{{ID: "p", WorkspaceRoot: repo.primary}},
				Threads:  []t3.Thread{{ID: "feature-thread", ProjectID: "p", Branch: new("feature")}},
			}}
			original := newT3SettleDispatcher
			t.Cleanup(func() { newT3SettleDispatcher = original })
			newT3SettleDispatcher = func(context.Context, func(string) string) (t3.Dispatcher, error) { return api, nil }
			var output bytes.Buffer
			afterReply := prepareT3MergedBranchSettlement(t.Context(), repo.svc, "feature", "master", &output)
			require.NotNil(t, afterReply)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch change {
			case "feature advanced":
				runGit(t, repo.worktree, "commit", "--allow-empty", "-m", "new work")
			case "base moved back":
				runGit(t, repo.primary, "reset", "--hard", "HEAD^")
			case "canceled":
				cancel()
			}
			afterReply(ctx)
			if change == "unchanged" {
				require.Len(t, api.commands, 1)
				assert.Contains(t, output.String(), "settled 1 T3 Code thread")
			} else {
				assert.Empty(t, api.commands)
			}
		})
	}
}

func TestReportT3Settlement(t *testing.T) {
	for _, err := range []error{nil, t3.ErrNoRuntime, context.Canceled} {
		var output bytes.Buffer
		reportT3Settlement(&output, 0, err)
		assert.Empty(t, output.String())
	}
	var output bytes.Buffer
	reportT3Settlement(&output, 1, errors.New("another thread rejected settlement"))
	assert.Contains(t, output.String(), "settled 1")
	assert.Contains(t, output.String(), "warning: t3 thread settlement incomplete")
}
