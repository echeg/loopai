//go:build !windows

package runs

import (
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
		want           Status
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
		{name: "stop marker wins over the launch lock", attempt: 1, stop: true, launchBranch: branch,
			want: StatusStopped},
		{name: "launch lock of another branch", attempt: 1, launchBranch: "other", want: StatusCrashed},
		{name: "nothing", attempt: 1, want: StatusCrashed},
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

// holdTestLock takes the lock at path for the rest of the test, as a live run holds its
// lifetime lock.
func holdTestLock(t *testing.T, path string) {
	t.Helper()
	release, err := AcquireBounded(path, time.Second)
	require.NoError(t, err)
	t.Cleanup(func() { _ = release() })
}
