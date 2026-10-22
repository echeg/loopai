package runs

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunsDirIsAbsolute(t *testing.T) {
	t.Chdir(t.TempDir())
	wd, err := os.Getwd()
	require.NoError(t, err)

	got := RunsDir(".")
	assert.True(t, filepath.IsAbs(got))
	assert.Equal(t, filepath.Join(wd, ".loopai", "progress", "runs"), got)
	assert.Equal(t, filepath.Join(wd, ".loopai", "progress", "runs", "x-1"), Dir(".", "x-1"))

	// a path computed before a chdir keeps naming the same directory afterwards
	t.Chdir(t.TempDir())
	assert.Equal(t, filepath.Join(wd, ".loopai", "progress", "runs"), got)
}

func TestRunsDirAbsoluteRoot(t *testing.T) {
	root := t.TempDir()
	assert.Equal(t, filepath.Join(root, ".loopai", "progress", "runs"), RunsDir(root))
	assert.Equal(t, filepath.Join(root, ".loopai", "progress", "runs", "plan-20261022-153000-a1b2"),
		Dir(root, "plan-20261022-153000-a1b2"))
}

func TestRunFileNamesAvoidDashboardPattern(t *testing.T) {
	names := []string{
		DescriptorFile, LockFile, JournalFile, OutcomeFile, ReportFile, ConsoleLogFile, StopFile,
		AttachLockFile, SourcePlansFile, "outcome-attempt-1.json",
		filepath.Base(launchLockPath(t.TempDir(), "docs/plans/progress-x.md", "progress-x")),
	}
	for _, name := range names {
		matched, err := filepath.Match("progress-*.txt", name)
		require.NoError(t, err)
		assert.False(t, matched, "%s would be replayed by the dashboard as a session", name)
	}
}
