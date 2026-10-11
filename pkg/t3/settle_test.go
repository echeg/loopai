package t3

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSettleMergedBranch(t *testing.T) {
	timestamp := "2026-10-11T00:00:00Z"
	shell := Shell{
		Projects: []Project{
			{ID: "main", WorkspaceRoot: `C:\Repo`},
			{ID: "worktree", WorkspaceRoot: `C:\Worktrees\feature`},
			{ID: "unrelated", WorkspaceRoot: `C:\Other`},
		},
		Threads: []Thread{
			{ID: "main-match", ProjectID: "main", Branch: new("feature")},
			{ID: "worktree-match", ProjectID: "worktree", Branch: new("feature")},
			{ID: "other-branch", ProjectID: "main", Branch: new("main")},
			{ID: "other-project", ProjectID: "unrelated", Branch: new("feature")},
			{ID: "no-branch", ProjectID: "main"},
			{ID: "archived", ProjectID: "main", Branch: new("feature"), ArchivedAt: &timestamp},
			{ID: "settled", ProjectID: "main", Branch: new("feature"), SettledAt: &timestamp},
			{ID: "approval", ProjectID: "main", Branch: new("feature"), HasPendingApprovals: true},
			{ID: "input", ProjectID: "main", Branch: new("feature"), HasPendingUserInput: true},
		},
	}
	t.Run("matches repository and branch across registered worktrees", func(t *testing.T) {
		api := &fakeDispatcher{shell: shell}
		n, err := SettleMergedBranch(t.Context(), api,
			[]string{"/elsewhere", "c:/repo/", "c:/worktrees/feature", "c:/repo"}, "feature")
		require.NoError(t, err)
		assert.Equal(t, 2, n)
		require.Len(t, api.commands, 2)
		first, ok := api.commands[0].(*ThreadSettle)
		require.True(t, ok)
		assert.Equal(t, "main-match", first.ThreadID)
		second, ok := api.commands[1].(*ThreadSettle)
		require.True(t, ok)
		assert.Equal(t, "worktree-match", second.ThreadID)
	})
	t.Run("no matching thread is a no-op", func(t *testing.T) {
		api := &fakeDispatcher{shell: shell}
		n, err := SettleMergedBranch(t.Context(), api, []string{"c:/repo"}, "absent")
		require.NoError(t, err)
		assert.Zero(t, n)
		assert.Empty(t, api.commands)
	})
	t.Run("no project", func(t *testing.T) {
		api := &fakeDispatcher{shell: shell}
		n, err := SettleMergedBranch(t.Context(), api, []string{"/unrelated"}, "feature")
		require.ErrorContains(t, err, "no T3 Code project for /unrelated")
		assert.Zero(t, n)
		assert.Empty(t, api.commands)
	})
	t.Run("shell error", func(t *testing.T) {
		api := &fakeDispatcher{shellErr: errors.New("offline")}
		n, err := SettleMergedBranch(t.Context(), api, []string{"c:/repo"}, "feature")
		require.ErrorContains(t, err, "offline")
		assert.Zero(t, n)
		assert.Empty(t, api.commands)
	})
	t.Run("empty branch rejected before contacting server", func(t *testing.T) {
		api := &fakeDispatcher{shell: shell}
		n, err := SettleMergedBranch(t.Context(), api, []string{"c:/repo"}, " ")
		require.ErrorContains(t, err, "merged branch is empty")
		assert.Zero(t, n)
		assert.Zero(t, api.shellHits)
	})
	t.Run("cancellation stops dispatch", func(t *testing.T) {
		api := &fakeDispatcher{shell: shell}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		n, err := SettleMergedBranch(ctx, api, []string{"c:/repo"}, "feature")
		require.ErrorIs(t, err, context.Canceled)
		assert.Zero(t, n)
		assert.Empty(t, api.commands)
	})
}

type settleDispatcher struct {
	shell    Shell
	shells   []Shell
	shellErr error
	shellHit int
	failed   map[string]error
	attempts []string
}

func (f *settleDispatcher) Shell(context.Context) (Shell, error) {
	f.shellHit++
	if len(f.shells) > 0 {
		shell := f.shells[0]
		if len(f.shells) > 1 {
			f.shells = f.shells[1:]
		}
		return shell, nil
	}
	return f.shell, f.shellErr
}

func (f *settleDispatcher) Dispatch(_ context.Context, cmd Command) (int64, error) {
	settle := cmd.(*ThreadSettle)
	f.attempts = append(f.attempts, settle.ThreadID)
	return 0, f.failed[settle.ThreadID]
}

func TestSettleMergedBranchContinuesAfterFailure(t *testing.T) {
	blocked := errors.New("active turn")
	other := errors.New("server unavailable")
	api := &settleDispatcher{
		shell: Shell{
			Projects: []Project{{ID: "p", WorkspaceRoot: "/repo"}},
			Threads: []Thread{
				{ID: "busy", ProjectID: "p", Branch: new("feature")},
				{ID: "finished", ProjectID: "p", Branch: new("feature")},
				{ID: "failure", ProjectID: "p", Branch: new("feature")},
			},
		},
		failed: map[string]error{"busy": blocked, "failure": other},
	}
	n, err := SettleMergedBranch(t.Context(), api, []string{"/repo"}, "feature")
	assert.Equal(t, 1, n)
	require.ErrorIs(t, err, blocked)
	require.ErrorIs(t, err, other)
	require.ErrorContains(t, err, "settle thread busy")
	require.ErrorContains(t, err, "settle thread failure")
	assert.Equal(t, []string{"busy", "finished", "failure"}, api.attempts)
}

func settlementShell(threads ...Thread) Shell {
	return Shell{Projects: []Project{{ID: "p", WorkspaceRoot: "/repo"}}, Threads: threads}
}

func settlementThread(status string) Thread {
	return Thread{
		ID: "thread", ProjectID: "p", Branch: new("feature"),
		Session: &ThreadSession{Status: status}, LatestTurn: &ThreadTurn{TurnID: "turn-1", State: "running"},
		LatestUserMessageAt: new("2026-10-11T00:00:00Z"),
	}
}

func TestSettleMergedBranchDoesNotWaitForBusyThread(t *testing.T) {
	api := &settleDispatcher{shell: settlementShell(settlementThread("running"))}
	n, err := SettleMergedBranch(t.Context(), api, []string{"/repo"}, "feature")
	require.ErrorContains(t, err, "still has an active turn")
	assert.Zero(t, n)
	assert.Equal(t, 1, api.shellHit)
	assert.Empty(t, api.attempts)
}

func TestPreparedSettlementWaitsForCompletion(t *testing.T) {
	busy, ready := settlementThread("running"), settlementThread("ready")
	ready.LatestTurn.State = "completed"
	api := &settleDispatcher{shells: []Shell{
		settlementShell(busy), settlementShell(busy), settlementShell(ready),
	}}
	settlement, err := PrepareBranchSettlement(t.Context(), api, []string{"/repo"}, "feature")
	require.NoError(t, err)
	assert.Empty(t, api.attempts)
	n, err := settlement.Settle(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, 3, api.shellHit)
	assert.Equal(t, []string{"thread"}, api.attempts)
	// A completed action cannot settle the thread again after the user reopens it.
	n, err = settlement.Settle(t.Context())
	require.NoError(t, err)
	assert.Zero(t, n)
	assert.Equal(t, 3, api.shellHit)
}

func TestPreparedSettlementAbandonsChangedThread(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Thread)
	}{
		{"new turn", func(t *Thread) { t.LatestTurn.TurnID = "turn-2" }},
		{"new message", func(t *Thread) { t.LatestUserMessageAt = new("2026-10-11T00:01:00Z") }},
		{"project", func(t *Thread) { t.ProjectID = "other" }},
		{"branch", func(t *Thread) { t.Branch = new("other") }},
		{"archived", func(t *Thread) { t.ArchivedAt = new("2026-10-11T00:01:00Z") }},
		{"settled", func(t *Thread) { t.SettledAt = new("2026-10-11T00:01:00Z") }},
		{"approval", func(t *Thread) { t.HasPendingApprovals = true }},
		{"input", func(t *Thread) { t.HasPendingUserInput = true }},
		{"failed turn", func(t *Thread) { t.LatestTurn.State = "error" }},
		{"interrupted turn", func(t *Thread) { t.LatestTurn.State = "interrupted" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before, after := settlementThread("running"), settlementThread("ready")
			tc.change(&after)
			api := &settleDispatcher{shells: []Shell{settlementShell(before), settlementShell(after)}}
			settlement, err := PrepareBranchSettlement(t.Context(), api, []string{"/repo"}, "feature")
			require.NoError(t, err)
			n, err := settlement.Settle(t.Context())
			require.NoError(t, err)
			assert.Zero(t, n)
			assert.Empty(t, api.attempts)
		})
	}
}

func TestPreparedSettlementCancellationAndFailures(t *testing.T) {
	t.Run("canceled before poll", func(t *testing.T) {
		api := &settleDispatcher{shell: settlementShell(settlementThread("running"))}
		settlement, err := PrepareBranchSettlement(t.Context(), api, []string{"/repo"}, "feature")
		require.NoError(t, err)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		n, err := settlement.Settle(ctx)
		require.ErrorIs(t, err, context.Canceled)
		assert.Zero(t, n)
		assert.Equal(t, 1, api.shellHit)
		assert.Empty(t, api.attempts)
	})
	t.Run("deadline while busy", func(t *testing.T) {
		api := &settleDispatcher{shell: settlementShell(settlementThread("running"))}
		settlement, err := PrepareBranchSettlement(t.Context(), api, []string{"/repo"}, "feature")
		require.NoError(t, err)
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
		defer cancel()
		n, err := settlement.Settle(ctx)
		require.ErrorIs(t, err, context.DeadlineExceeded)
		assert.Zero(t, n)
		assert.Empty(t, api.attempts)
	})
	t.Run("shell error after preparation", func(t *testing.T) {
		api := &settleDispatcher{shell: settlementShell(settlementThread("running"))}
		settlement, err := PrepareBranchSettlement(t.Context(), api, []string{"/repo"}, "feature")
		require.NoError(t, err)
		api.shellErr = errors.New("offline")
		n, err := settlement.Settle(t.Context())
		require.ErrorContains(t, err, "offline")
		assert.Zero(t, n)
		assert.Empty(t, api.attempts)
	})
	t.Run("one rejection does not prevent another thread settling", func(t *testing.T) {
		first, second := settlementThread("ready"), settlementThread("ready")
		second.ID = "second"
		blocked := errors.New("rejected")
		api := &settleDispatcher{shell: settlementShell(first, second), failed: map[string]error{"thread": blocked}}
		settlement, err := PrepareBranchSettlement(t.Context(), api, []string{"/repo"}, "feature")
		require.NoError(t, err)
		n, err := settlement.Settle(t.Context())
		require.ErrorIs(t, err, blocked)
		assert.Equal(t, 1, n)
		assert.Equal(t, []string{"thread", "second"}, api.attempts)
	})
}
