//go:build !windows

package runs

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClassify(t *testing.T) {
	const plan, branch = "docs/plans/feature.md", "feature"
	tests := []struct {
		name    string
		attempt int
		lock    bool   // the run holds its lifetime lock
		outcome Status // outcome recorded for outcomeAttempt, empty for none
		// outcomeAttempt is the attempt the outcome belongs to; zero means the current one
		outcomeAttempt int
		stop           bool   // the stop marker is present
		launchBranch   string // a launcher holds the launch lock of plan on this branch
		finish         Status // a finish event recorded in the journal for finishAttempt, empty for none
		// finishAttempt is the attempt the finish event belongs to; zero means the current one
		finishAttempt int
		want          Status
	}{
		{name: "lock held", attempt: 1, lock: true, want: StatusRunning},
		{name: "lock held over a finished outcome", attempt: 1, lock: true, outcome: StatusSucceeded,
			stop: true, want: StatusRunning},
		{name: "succeeded", attempt: 1, outcome: StatusSucceeded, want: StatusSucceeded},
		{name: "failed", attempt: 1, outcome: StatusFailed, want: StatusFailed},
		{name: "stopped outcome", attempt: 1, outcome: StatusStopped, want: StatusStopped},
		{name: "outcome wins over the stop marker", attempt: 1, outcome: StatusFailed, stop: true,
			want: StatusFailed},
		{name: "stop marker without outcome", attempt: 1, stop: true, want: StatusStopped},
		{name: "outcome of a previous attempt does not count", attempt: 2, outcome: StatusSucceeded,
			outcomeAttempt: 1, want: StatusCrashed},
		{name: "previous attempt outcome with stop marker", attempt: 2, outcome: StatusFailed,
			outcomeAttempt: 1, stop: true, want: StatusStopped},
		{name: "launch lock held", attempt: 1, launchBranch: branch, want: StatusStarting},
		{name: "lock held while the launcher still holds the launch lock", attempt: 1, lock: true,
			launchBranch: branch, want: StatusRunning},
		{name: "outcome wins over the launch lock", attempt: 2, outcome: StatusFailed, launchBranch: branch,
			want: StatusFailed},
		{name: "stop marker wins over the launch lock", attempt: 1, stop: true, launchBranch: branch,
			want: StatusStopped},
		{name: "launch lock of another branch", attempt: 1, launchBranch: "other", want: StatusCrashed},
		{name: "nothing", attempt: 1, want: StatusCrashed},
		// a success whose outcome could not be written after its finish event
		{name: "succeeded finish without its outcome", attempt: 1, finish: StatusSucceeded, want: StatusSucceeded},
		{name: "succeeded finish wins over the stop marker", attempt: 1, finish: StatusSucceeded, stop: true,
			want: StatusSucceeded},
		{name: "succeeded finish wins over the launch lock", attempt: 1, finish: StatusSucceeded,
			launchBranch: branch, want: StatusSucceeded},
		{name: "outcome wins over a succeeded finish", attempt: 1, finish: StatusSucceeded, outcome: StatusFailed,
			want: StatusFailed},
		{name: "lock held over a succeeded finish", attempt: 1, finish: StatusSucceeded, lock: true,
			want: StatusRunning},
		{name: "succeeded finish of a previous attempt does not count", attempt: 2, finish: StatusSucceeded,
			finishAttempt: 1, want: StatusCrashed},
		{name: "failed finish without its outcome", attempt: 1, finish: StatusFailed, want: StatusCrashed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dir := writeTestRun(t, root, "feature-1", plan, branch, tc.attempt, time.Now())
			if tc.lock {
				holdTestLock(t, filepath.Join(dir, LockFile))
			}
			if tc.outcome != "" {
				attempt := tc.outcomeAttempt
				if attempt == 0 {
					attempt = tc.attempt
				}
				writeTestOutcome(t, dir, attempt, tc.outcome)
			}
			if tc.stop {
				require.NoError(t, RequestStop(dir))
			}
			if tc.finish != "" {
				attempt := tc.finishAttempt
				if attempt == 0 {
					attempt = tc.attempt
				}
				writeTestFinish(t, dir, attempt, tc.finish)
			}
			if tc.launchBranch != "" {
				release, err := AcquireLaunchLock(root, plan, tc.launchBranch, time.Second)
				require.NoError(t, err)
				t.Cleanup(func() { _ = release() })
			}

			got, err := Classify(root, dir)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestClassifyStartingRunSeenBetweenProbes(t *testing.T) {
	const plan, branch = "docs/plans/feature.md", "feature"
	root := t.TempDir()
	dir := writeTestRun(t, root, "feature-1", plan, branch, 1, time.Now())
	release, err := AcquireLaunchLock(root, plan, branch, time.Second)
	require.NoError(t, err)
	t.Cleanup(func() { _ = release() })

	// between classify's two probes the child takes its lifetime lock and acks, and the
	// launcher releases the launch lock; one of the probes must still see the run
	var probed []string
	probeLock = func(path string) (bool, error) {
		probed = append(probed, filepath.Base(path))
		if len(probed) == 2 {
			holdTestLock(t, filepath.Join(dir, LockFile))
			require.NoError(t, release())
		}
		return Probe(path)
	}
	t.Cleanup(func() { probeLock = Probe })

	got, err := Classify(root, dir)
	require.NoError(t, err)
	assert.Equal(t, StatusRunning, got, "a run that starts during classification is never crashed")
	require.Len(t, probed, 2)
	assert.Equal(t, filepath.Base(launchLockPath(root, plan, branch)), probed[0], "the launch lock is probed first")
	assert.Equal(t, LockFile, probed[1])
}

func TestClassifyErrors(t *testing.T) {
	t.Run("missing descriptor", func(t *testing.T) {
		root := t.TempDir()
		dir := Dir(root, "feature-1")
		require.NoError(t, os.MkdirAll(dir, 0o750))
		_, err := Classify(root, dir)
		require.ErrorIs(t, err, fs.ErrNotExist)
	})

	t.Run("corrupt descriptor", func(t *testing.T) {
		root := t.TempDir()
		dir := Dir(root, "feature-1")
		require.NoError(t, os.MkdirAll(dir, 0o750))
		require.NoError(t, os.WriteFile(filepath.Join(dir, DescriptorFile), []byte("{not json"), 0o600))
		_, err := Classify(root, dir)
		require.ErrorContains(t, err, "classify run feature-1")
		assert.ErrorContains(t, err, "decode")
	})

	t.Run("corrupt outcome", func(t *testing.T) {
		root := t.TempDir()
		dir := writeTestRun(t, root, "feature-1", "plan.md", "feature", 1, time.Now())
		require.NoError(t, os.WriteFile(filepath.Join(dir, OutcomeFile), []byte(`{"version":1,`), 0o600))
		_, err := Classify(root, dir)
		require.ErrorContains(t, err, "classify run feature-1")
		assert.ErrorContains(t, err, "decode")
	})

	t.Run("launch lock probe fails", func(t *testing.T) {
		root := t.TempDir()
		dir := writeTestRun(t, root, "feature-1", "plan.md", "feature", 1, time.Now())
		probeLock = func(path string) (bool, error) {
			if filepath.Base(path) != LockFile {
				return false, errors.New("boom")
			}
			return Probe(path)
		}
		t.Cleanup(func() { probeLock = Probe })

		_, err := Classify(root, dir)
		require.ErrorContains(t, err, "classify run feature-1: probe launch lock: boom")
	})

	t.Run("outcome with an invalid status", func(t *testing.T) {
		root := t.TempDir()
		dir := writeTestRun(t, root, "feature-1", "plan.md", "feature", 1, time.Now())
		require.NoError(t, os.WriteFile(filepath.Join(dir, OutcomeFile),
			[]byte(`{"version":1,"attempt":1,"status":"crashed"}`), 0o600))
		_, err := Classify(root, dir)
		require.ErrorContains(t, err, `invalid status "crashed"`)
	})
}

func TestStatusLive(t *testing.T) {
	live := map[Status]bool{
		StatusStarting: true, StatusRunning: true, StatusSucceeded: false, StatusFailed: false,
		StatusStopped: false, StatusCrashed: false,
	}
	for status, want := range live {
		assert.Equal(t, want, status.Live(), "%s", status)
	}
}

// writeTestRun creates run id under root with a saved descriptor and returns its directory.
func writeTestRun(t *testing.T, root, id, plan, branch string, attempt int, created time.Time) string {
	t.Helper()
	dir := Dir(root, id)
	require.NoError(t, os.MkdirAll(dir, 0o750))
	desc := &Descriptor{ID: id, Attempt: attempt, CreatedAt: created, Cwd: root, Mode: "full",
		Plan: plan, Branch: branch, Origin: Origin{Kind: OriginCLI}}
	require.NoError(t, SaveDescriptor(dir, desc))
	return dir
}

// writeTestOutcome records an outcome with status for attempt in run directory dir.
func writeTestOutcome(t *testing.T, dir string, attempt int, status Status) {
	t.Helper()
	require.NoError(t, SaveOutcome(dir, &Outcome{RunID: filepath.Base(dir), Attempt: attempt, Status: status}))
}

// writeTestFinish appends attempt's start and a finish event with status to the journal of run
// directory dir.
func writeTestFinish(t *testing.T, dir string, attempt int, status Status) {
	t.Helper()
	j, err := OpenJournal(dir, attempt)
	require.NoError(t, err)
	_, err = j.Append(Event{Type: EventAttemptStart})
	require.NoError(t, err)
	_, err = j.Append(Event{Type: EventFinish, Status: status})
	require.NoError(t, err)
	require.NoError(t, j.Close())
}

// holdTestLock takes the lock at path for the rest of the test, as a live run holds its
// lifetime lock.
func holdTestLock(t *testing.T, path string) {
	t.Helper()
	release, err := AcquireBounded(path, time.Second)
	require.NoError(t, err)
	t.Cleanup(func() { _ = release() })
}
