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
		assert.ErrorContains(t, err, filepath.Join("plan-b-1", DescriptorFile))
	})

	t.Run("corrupt outcome", func(t *testing.T) {
		root := t.TempDir()
		dir := writeTestRun(t, root, "plan-a-1", "docs/plans/a.md", "a", 1, testEpoch)
		require.NoError(t, os.WriteFile(filepath.Join(dir, OutcomeFile), []byte("nope"), 0o600))

		_, err := List(root)
		require.ErrorContains(t, err, "classify run plan-a-1")
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
