//go:build !windows

package runs

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var testEpoch = time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

func TestList(t *testing.T) {
	root := t.TempDir()
	runs, err := List(root)
	require.NoError(t, err)
	assert.Empty(t, runs, "a checkout without a runs directory has no runs")

	older := writeTestRun(t, root, "plan-a-1", "docs/plans/a.md", "a", 1, testEpoch)
	newer := writeTestRun(t, root, "plan-b-1", "docs/plans/b.md", "b", 1, testEpoch.Add(time.Hour))
	tie := writeTestRun(t, root, "plan-c-1", "docs/plans/c.md", "c", 1, testEpoch)
	writeTestOutcome(t, older, 1, StatusSucceeded)
	holdTestLock(t, filepath.Join(newer, LockFile))

	// entries that are not runs are skipped
	runsDir := RunsDir(root)
	require.NoError(t, os.WriteFile(filepath.Join(runsDir, "notes.txt"), []byte("x"), 0o600))
	require.NoError(t, os.Mkdir(filepath.Join(runsDir, "Not An ID"), 0o750))
	require.NoError(t, os.Mkdir(filepath.Join(runsDir, "pending-1"), 0o750)) // descriptor not written yet
	require.NoError(t, os.Symlink(older, filepath.Join(runsDir, "link-1")))
	release, err := AcquireLaunchLock(root, "docs/plans/a.md", "a", time.Second)
	require.NoError(t, err)
	require.NoError(t, release())

	runs, err = List(root)
	require.NoError(t, err)
	require.Len(t, runs, 3)
	assert.Equal(t, []string{"plan-b-1", "plan-c-1", "plan-a-1"}, runIDs(runs),
		"newest first, ties broken by id")
	assert.Equal(t, Run{Dir: newer, Descriptor: mustLoadDescriptor(t, newer), Status: StatusRunning}, runs[0])
	assert.Equal(t, tie, runs[1].Dir)
	assert.Equal(t, StatusCrashed, runs[1].Status)
	assert.Equal(t, StatusSucceeded, runs[2].Status)
}

func TestListErrors(t *testing.T) {
	t.Run("corrupt descriptor", func(t *testing.T) {
		root := t.TempDir()
		writeTestRun(t, root, "plan-a-1", "docs/plans/a.md", "a", 1, testEpoch)
		dir := Dir(root, "plan-b-1")
		require.NoError(t, os.MkdirAll(dir, 0o750))
		require.NoError(t, os.WriteFile(filepath.Join(dir, DescriptorFile), []byte("{"), 0o600))

		_, err := List(root)
		require.ErrorContains(t, err, "list runs")
		require.ErrorContains(t, err, filepath.Join("plan-b-1", DescriptorFile))
		require.ErrorContains(t, err, "run plan-b-1 cannot be read: ", "the error names the run")
		require.ErrorContains(t, err, "; check that no loopai process still uses "+dir+", then remove it",
			"and how to clear it")
		_, _, err = LatestFor(root, "docs/plans/a.md", "a")
		require.ErrorContains(t, err, "run plan-b-1 cannot be read",
			"a query filtered by plan still fails: the broken run could be of the same plan")
	})

	t.Run("corrupt outcome", func(t *testing.T) {
		root := t.TempDir()
		dir := writeTestRun(t, root, "plan-a-1", "docs/plans/a.md", "a", 1, testEpoch)
		require.NoError(t, os.WriteFile(filepath.Join(dir, OutcomeFile), []byte("nope"), 0o600))

		_, err := List(root)
		require.ErrorContains(t, err, "classify run plan-a-1")
		require.ErrorContains(t, err, "run plan-a-1 cannot be read: ")
		assert.ErrorContains(t, err, "then remove it")
	})

	t.Run("unknown descriptor version", func(t *testing.T) {
		root := t.TempDir()
		dir := Dir(root, "plan-c-1")
		require.NoError(t, os.MkdirAll(dir, 0o750))
		require.NoError(t, os.WriteFile(filepath.Join(dir, DescriptorFile), []byte(`{"version":2,"id":"plan-c-1"}`), 0o600))

		_, _, err := LiveRunForBranch(root, "c")
		require.ErrorContains(t, err, "run plan-c-1 cannot be read: ")
		require.ErrorContains(t, err, "unsupported version 2")
		require.ErrorContains(t, err, dir)
		require.ErrorContains(t, PruneSucceeded(root, 0), "run plan-c-1 cannot be read: ")
	})

	t.Run("a platform without detached runs names no directory to remove", func(t *testing.T) {
		root := t.TempDir()
		writeTestRun(t, root, "plan-a-1", "docs/plans/a.md", "a", 1, testEpoch)
		probeLock = func(string) (bool, error) { return false, ErrUnsupported }
		t.Cleanup(func() { probeLock = Probe })

		_, err := List(root)
		require.ErrorIs(t, err, ErrUnsupported)
		assert.NotContains(t, err.Error(), "remove it")
	})

	t.Run("unreadable runs directory", func(t *testing.T) {
		root := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Dir(RunsDir(root)), 0o750))
		require.NoError(t, os.WriteFile(RunsDir(root), []byte("x"), 0o600))

		_, err := List(root)
		require.ErrorContains(t, err, "list runs")
	})
}

func TestResolve(t *testing.T) {
	root := t.TempDir()
	const plan = "docs/plans/20261020-feature-x.md"
	writeTestRun(t, root, "feature-x-20261020-100000-aaaa", plan, "feature-x", 1, testEpoch)
	writeTestRun(t, root, "feature-x-20261021-100000-bbbb", plan, "feature-x", 1, testEpoch.Add(time.Hour))
	writeTestRun(t, root, "other-20261019-100000-cccc", "docs/plans/other.md", "other-branch", 1, testEpoch.Add(-time.Hour))
	// a plan match beats a newer branch match
	writeTestRun(t, root, "hotfix-1", "docs/plans/hotfix.md", "hotfix-branch", 1, testEpoch.Add(-2*time.Hour))
	writeTestRun(t, root, "renamed-1", "docs/plans/renamed.md", "hotfix", 1, testEpoch.Add(2*time.Hour))
	// an id match beats a newer plan match
	writeTestRun(t, root, "solo", "docs/plans/solo-plan.md", "solo-branch", 1, testEpoch.Add(-3*time.Hour))
	writeTestRun(t, root, "solo-2", "docs/plans/solo.md", "solo-2-branch", 1, testEpoch.Add(3*time.Hour))

	tests := []struct {
		name  string
		ident string
		want  string
	}{
		{name: "exact id of an older run", ident: "feature-x-20261020-100000-aaaa", want: "feature-x-20261020-100000-aaaa"},
		{name: "plan path relative to root", ident: plan, want: "feature-x-20261021-100000-bbbb"},
		{name: "absolute plan path", ident: filepath.Join(root, plan), want: "feature-x-20261021-100000-bbbb"},
		{name: "plan file name", ident: "20261020-feature-x.md", want: "feature-x-20261021-100000-bbbb"},
		{name: "plan stem", ident: "20261020-feature-x", want: "feature-x-20261021-100000-bbbb"},
		{name: "branch", ident: "feature-x", want: "feature-x-20261021-100000-bbbb"},
		{name: "branch of another run", ident: "other-branch", want: "other-20261019-100000-cccc"},
		{name: "plan before branch", ident: "hotfix", want: "hotfix-1"},
		{name: "id before plan", ident: "solo", want: "solo"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			run, err := Resolve(root, tc.ident)
			require.NoError(t, err)
			assert.Equal(t, tc.want, run.Descriptor.ID)
		})
	}
}

func TestResolvePlanPathRelativeToCwd(t *testing.T) {
	root := t.TempDir()
	writeTestRun(t, root, "feature-1", "docs/plans/feature.md", "feature", 1, testEpoch)
	require.NoError(t, os.MkdirAll(filepath.Join(root, "docs"), 0o750))
	t.Chdir(filepath.Join(root, "docs"))

	run, err := Resolve(root, filepath.Join("plans", "feature.md"))
	require.NoError(t, err)
	assert.Equal(t, "feature-1", run.Descriptor.ID)
}

func TestResolveNotFound(t *testing.T) {
	root := t.TempDir()
	_, err := Resolve(root, "anything")
	require.ErrorIs(t, err, ErrNotFound)

	writeTestRun(t, root, "feature-1", "docs/plans/feature.md", "feature", 1, testEpoch)
	_, err = Resolve(root, "missing")
	require.ErrorIs(t, err, ErrNotFound)
	require.ErrorContains(t, err, "missing")
	_, err = Resolve(root, "")
	require.ErrorIs(t, err, ErrNotFound)
	_, err = Resolve(root, "docs/plans/other/feature.md")
	require.ErrorIs(t, err, ErrNotFound, "a different path with the same file name is not a path match")
}

func TestDefaultRun(t *testing.T) {
	t.Run("no runs", func(t *testing.T) {
		_, err := DefaultRun(t.TempDir())
		require.ErrorIs(t, err, ErrNotFound)
	})

	t.Run("single live run beats a newer finished one", func(t *testing.T) {
		root := t.TempDir()
		live := writeTestRun(t, root, "live-1", "docs/plans/live.md", "live", 1, testEpoch)
		done := writeTestRun(t, root, "done-1", "docs/plans/done.md", "done", 1, testEpoch.Add(time.Hour))
		holdTestLock(t, filepath.Join(live, LockFile))
		writeTestOutcome(t, done, 1, StatusSucceeded)

		run, err := DefaultRun(root)
		require.NoError(t, err)
		assert.Equal(t, "live-1", run.Descriptor.ID)
	})

	t.Run("a starting run counts as live", func(t *testing.T) {
		root := t.TempDir()
		writeTestRun(t, root, "starting-1", "docs/plans/s.md", "s", 1, testEpoch)
		writeTestRun(t, root, "crashed-1", "docs/plans/c.md", "c", 1, testEpoch.Add(time.Hour))
		release, err := AcquireLaunchLock(root, "docs/plans/s.md", "s", time.Second)
		require.NoError(t, err)
		t.Cleanup(func() { _ = release() })

		run, err := DefaultRun(root)
		require.NoError(t, err)
		assert.Equal(t, "starting-1", run.Descriptor.ID)
	})

	t.Run("several live runs fall back to the newest", func(t *testing.T) {
		root := t.TempDir()
		first := writeTestRun(t, root, "live-1", "docs/plans/a.md", "a", 1, testEpoch)
		second := writeTestRun(t, root, "live-2", "docs/plans/b.md", "b", 1, testEpoch.Add(time.Hour))
		writeTestRun(t, root, "crashed-1", "docs/plans/c.md", "c", 1, testEpoch.Add(2*time.Hour))
		holdTestLock(t, filepath.Join(first, LockFile))
		holdTestLock(t, filepath.Join(second, LockFile))

		run, err := DefaultRun(root)
		require.NoError(t, err)
		assert.Equal(t, "crashed-1", run.Descriptor.ID)
	})

	t.Run("no live run falls back to the newest", func(t *testing.T) {
		root := t.TempDir()
		writeTestRun(t, root, "old-1", "docs/plans/a.md", "a", 1, testEpoch)
		writeTestRun(t, root, "new-1", "docs/plans/b.md", "b", 1, testEpoch.Add(time.Hour))

		run, err := DefaultRun(root)
		require.NoError(t, err)
		assert.Equal(t, "new-1", run.Descriptor.ID)
	})
}

func TestLatestFor(t *testing.T) {
	root := t.TempDir()
	const plan = "docs/plans/feature.md"
	writeTestRun(t, root, "feature-1", plan, "feature", 1, testEpoch)
	writeTestRun(t, root, "feature-2", plan, "feature", 1, testEpoch.Add(time.Hour))
	writeTestRun(t, root, "feature-3", plan, "feature-alt", 1, testEpoch.Add(2*time.Hour))

	run, found, err := LatestFor(root, plan, "feature")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "feature-2", run.Descriptor.ID)

	run, found, err = LatestFor(root, "./"+plan, "feature-alt")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "feature-3", run.Descriptor.ID)

	_, found, err = LatestFor(root, plan, "missing")
	require.NoError(t, err)
	assert.False(t, found)
	_, found, err = LatestFor(root, "docs/plans/other.md", "feature")
	require.NoError(t, err)
	assert.False(t, found)
	_, found, err = LatestFor(t.TempDir(), plan, "feature")
	require.NoError(t, err)
	assert.False(t, found)
}

func TestLatestForUnderOwnLaunchLock(t *testing.T) {
	const plan, branch = "docs/plans/feature.md", "feature"
	tests := []struct {
		name    string
		lock    bool   // the run holds its lifetime lock
		outcome Status // outcome of the current attempt, empty for none
		stop    bool   // the stop marker is present
		finish  Status // a finish event of the current attempt in the journal, empty for none
		want    Status
	}{
		{name: "crashed run", want: StatusCrashed},
		{name: "live run", lock: true, want: StatusRunning},
		{name: "failed run", outcome: StatusFailed, want: StatusFailed},
		{name: "stopped run", stop: true, want: StatusStopped},
		{name: "succeeded run", outcome: StatusSucceeded, want: StatusSucceeded},
		// a launcher must never resume a run that finished but could not write its outcome
		{name: "succeeded run without its outcome", finish: StatusSucceeded, want: StatusSucceeded},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dir := writeTestRun(t, root, "feature-1", plan, branch, 1, testEpoch)
			if tc.lock {
				holdTestLock(t, filepath.Join(dir, LockFile))
			}
			if tc.outcome != "" {
				writeTestOutcome(t, dir, 1, tc.outcome)
			}
			if tc.stop {
				require.NoError(t, RequestStop(dir))
			}
			if tc.finish != "" {
				writeTestFinish(t, dir, 1, tc.finish)
			}
			// the launcher decides the run under its own launch lock, spelled as the user typed it
			release, err := AcquireLaunchLock(root, "./"+plan, branch, time.Second)
			require.NoError(t, err)
			t.Cleanup(func() { _ = release() })

			run, found, err := LatestFor(root, "./"+plan, branch)
			require.NoError(t, err)
			require.True(t, found)
			assert.Equal(t, tc.want, run.Status, "the launcher's own launch lock is not a starting run")
		})
	}

	t.Run("observers still see the launch as starting", func(t *testing.T) {
		root := t.TempDir()
		dir := writeTestRun(t, root, "feature-1", plan, branch, 1, testEpoch)
		other := writeTestRun(t, root, "other-1", "docs/plans/other.md", "other", 1, testEpoch.Add(time.Hour))
		release, err := AcquireLaunchLock(root, plan, branch, time.Second)
		require.NoError(t, err)
		t.Cleanup(func() { _ = release() })
		otherRelease, err := AcquireLaunchLock(root, "docs/plans/other.md", "other", time.Second)
		require.NoError(t, err)
		t.Cleanup(func() { _ = otherRelease() })

		status, err := Classify(root, dir)
		require.NoError(t, err)
		assert.Equal(t, StatusStarting, status)

		// only the caller's own plan and branch skip the launch-lock tier
		all, err := list(root, classifyOptions{ownLaunchLock: launchLockPath(root, plan, branch)})
		require.NoError(t, err)
		require.Len(t, all, 2)
		assert.Equal(t, other, all[0].Dir)
		assert.Equal(t, StatusStarting, all[0].Status)
		assert.Equal(t, StatusCrashed, all[1].Status)
	})
}

func TestLiveRunForBranch(t *testing.T) {
	root := t.TempDir()
	running := writeTestRun(t, root, "feature-1", "docs/plans/feature.md", "feature", 1, testEpoch)
	done := writeTestRun(t, root, "feature-2", "docs/plans/feature.md", "feature", 1, testEpoch.Add(time.Hour))
	writeTestRun(t, root, "crashed-1", "docs/plans/crashed.md", "crashed", 1, testEpoch)
	writeTestRun(t, root, "starting-1", "docs/plans/starting.md", "starting", 1, testEpoch)
	holdTestLock(t, filepath.Join(running, LockFile))
	writeTestOutcome(t, done, 1, StatusSucceeded)
	release, err := AcquireLaunchLock(root, "docs/plans/starting.md", "starting", time.Second)
	require.NoError(t, err)
	t.Cleanup(func() { _ = release() })

	run, found, err := LiveRunForBranch(root, "feature")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "feature-1", run.Descriptor.ID, "the live run, not the newer finished one")
	assert.Equal(t, StatusRunning, run.Status)

	run, found, err = LiveRunForBranch(root, "starting")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, StatusStarting, run.Status)

	_, found, err = LiveRunForBranch(root, "crashed")
	require.NoError(t, err)
	assert.False(t, found)
	_, found, err = LiveRunForBranch(root, "missing")
	require.NoError(t, err)
	assert.False(t, found)
}

func TestPruneSucceeded(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, PruneSucceeded(root, 20), "a checkout without runs prunes nothing")

	// the oldest runs of every other status would be pruned first if they counted
	live := writeTestRun(t, root, "live-1", "docs/plans/live.md", "live", 1, testEpoch.Add(-4*time.Hour))
	holdTestLock(t, filepath.Join(live, LockFile))
	writeTestOutcome(t, live, 1, StatusSucceeded) // written but the run still holds its lock
	failed := writeTestRun(t, root, "failed-1", "docs/plans/failed.md", "failed", 1, testEpoch.Add(-3*time.Hour))
	writeTestOutcome(t, failed, 1, StatusFailed)
	writeTestRun(t, root, "crashed-1", "docs/plans/crashed.md", "crashed", 1, testEpoch.Add(-2*time.Hour))
	stopped := writeTestRun(t, root, "stopped-1", "docs/plans/stopped.md", "stopped", 1, testEpoch.Add(-time.Hour))
	require.NoError(t, RequestStop(stopped))

	succeeded := make([]string, 0, 23)
	for i := range 23 {
		id := fmt.Sprintf("done-%02d", i)
		dir := writeTestRun(t, root, id, "docs/plans/done.md", "done", 1, testEpoch.Add(time.Duration(i)*time.Minute))
		writeTestOutcome(t, dir, 1, StatusSucceeded)
		succeeded = append(succeeded, id)
	}

	require.NoError(t, PruneSucceeded(root, 20))

	for _, id := range succeeded[:3] {
		assert.NoDirExists(t, Dir(root, id), "older succeeded run %s is pruned", id)
	}
	for _, id := range succeeded[3:] {
		assert.DirExists(t, Dir(root, id), "one of the newest 20 succeeded runs: %s", id)
	}
	for _, id := range []string{"live-1", "failed-1", "crashed-1", "stopped-1"} {
		assert.DirExists(t, Dir(root, id), "%s is never pruned", id)
	}

	require.NoError(t, PruneSucceeded(root, 20), "a second prune removes nothing more")
	runs, err := List(root)
	require.NoError(t, err)
	assert.Len(t, runs, 24)
}

func TestPruneSucceededKeepsKeptWorktrees(t *testing.T) {
	root := t.TempDir()
	kept := filepath.Join(root, ".loopai", "worktrees", "kept")
	require.NoError(t, os.MkdirAll(kept, 0o750))
	lost := filepath.Join(root, ".loopai", "worktrees", "lost")
	require.NoError(t, os.MkdirAll(lost, 0o750))

	// the three oldest runs, each first in line for pruning if it counted
	keptDir := writeTestRun(t, root, "kept-1", "docs/plans/kept.md", "kept", 1, testEpoch.Add(-3*time.Hour))
	require.NoError(t, SaveOutcome(keptDir, &Outcome{RunID: "kept-1", Attempt: 1, Status: StatusSucceeded,
		WorktreeKept: kept}))
	lostDir := writeTestRun(t, root, "lost-1", "docs/plans/lost.md", "lost", 1, testEpoch.Add(-2*time.Hour))
	desc := mustLoadDescriptor(t, lostDir)
	desc.Worktree = lost
	require.NoError(t, SaveDescriptor(lostDir, &desc))
	writeTestFinish(t, lostDir, 1, StatusSucceeded) // its outcome was never written
	goneDir := writeTestRun(t, root, "gone-1", "docs/plans/gone.md", "gone", 1, testEpoch.Add(-time.Hour))
	require.NoError(t, SaveOutcome(goneDir, &Outcome{RunID: "gone-1", Attempt: 1, Status: StatusSucceeded,
		WorktreeKept: filepath.Join(root, ".loopai", "worktrees", "gone")}))

	succeeded := make([]string, 0, 3)
	for i := range 3 {
		id := fmt.Sprintf("done-%02d", i)
		dir := writeTestRun(t, root, id, "docs/plans/done.md", "done", 1, testEpoch.Add(time.Duration(i)*time.Minute))
		writeTestOutcome(t, dir, 1, StatusSucceeded)
		succeeded = append(succeeded, id)
	}

	require.NoError(t, PruneSucceeded(root, 2))

	assert.DirExists(t, keptDir, "a run whose kept worktree is in place is never pruned")
	assert.DirExists(t, lostDir, "nor one whose outcome is lost while its worktree is in place")
	assert.NoDirExists(t, goneDir, "a run whose kept worktree is gone is pruned like any other")
	assert.NoDirExists(t, Dir(root, succeeded[0]), "the kept runs do not count toward keep")
	for _, id := range succeeded[1:] {
		assert.DirExists(t, Dir(root, id))
	}

	require.NoError(t, os.RemoveAll(kept))
	require.NoError(t, PruneSucceeded(root, 2))
	assert.NoDirExists(t, keptDir, "once its worktree is gone the run is pruned")
	assert.DirExists(t, lostDir)
}

func TestKeptWorktree(t *testing.T) {
	root := t.TempDir()
	present := filepath.Join(root, "present")
	require.NoError(t, os.Mkdir(present, 0o750))
	missing := filepath.Join(root, "missing")

	tests := []struct {
		name     string
		worktree string   // the descriptor's worktree
		outcome  *Outcome // nil for no outcome
		want     string
	}{
		{name: "an outcome that kept a present worktree", outcome: &Outcome{Attempt: 1, WorktreeKept: present},
			want: present},
		{name: "an outcome that kept a worktree that is gone", outcome: &Outcome{Attempt: 1, WorktreeKept: missing}},
		{name: "an outcome that kept nothing", worktree: present, outcome: &Outcome{Attempt: 1}},
		{name: "no outcome and the run's worktree in place", worktree: present, want: present},
		{name: "no outcome and the run's worktree gone", worktree: missing},
		{name: "an earlier attempt's outcome", worktree: present,
			outcome: &Outcome{Attempt: 0, WorktreeKept: missing}, want: present},
		{name: "no outcome and no worktree"},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			id := fmt.Sprintf("run-%d", i)
			dir := writeTestRun(t, root, id, "docs/plans/x.md", "x", 1, testEpoch)
			desc := mustLoadDescriptor(t, dir)
			desc.Worktree = tc.worktree
			require.NoError(t, SaveDescriptor(dir, &desc))
			if tc.outcome != nil {
				tc.outcome.RunID, tc.outcome.Status = id, StatusSucceeded
				require.NoError(t, SaveOutcome(dir, tc.outcome))
			}
			assert.Equal(t, tc.want, KeptWorktree(Run{Dir: dir, Descriptor: mustLoadDescriptor(t, dir)}))
		})
	}
}

func TestPruneSucceededListError(t *testing.T) {
	root := t.TempDir()
	dir := Dir(root, "plan-1")
	require.NoError(t, os.MkdirAll(dir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, DescriptorFile), []byte("{"), 0o600))
	require.ErrorContains(t, PruneSucceeded(root, 0), "prune runs")
	assert.DirExists(t, dir)
}

func runIDs(runs []Run) []string {
	ids := make([]string, 0, len(runs))
	for _, run := range runs {
		ids = append(ids, run.Descriptor.ID)
	}
	return ids
}

func mustLoadDescriptor(t *testing.T, dir string) Descriptor {
	t.Helper()
	desc, err := LoadDescriptor(dir)
	require.NoError(t, err)
	return desc
}
