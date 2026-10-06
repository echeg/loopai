package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/umputun/ralphex/pkg/config"
)

func TestLaunchHistoryPath(t *testing.T) {
	t.Run("explicit config dir", func(t *testing.T) {
		dir := t.TempDir()
		assert.Equal(t, filepath.Join(dir, "launch-history"), launchHistoryPath(dir))
	})
	t.Run("default config dir", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("USERPROFILE", home)
		assert.Equal(t, filepath.Join(config.DefaultConfigDir(), "launch-history"), launchHistoryPath(""))
		assert.Equal(t, filepath.Join(home, ".config", "loopai", "launch-history"), launchHistoryPath(""))
	})
}

func TestReadLaunchHistory(t *testing.T) {
	t1 := time.Date(2026, 10, 6, 12, 34, 56, 0, time.UTC)
	t2 := time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC)
	t3 := time.Date(2026, 10, 4, 9, 30, 0, 0, time.UTC)

	tests := []struct {
		name    string
		missing bool
		content string
		want    []launchEntry
	}{
		{name: "missing file", missing: true, want: nil},
		{name: "empty file", content: "", want: nil},
		{
			name: "order preserved",
			content: "2026-10-06T12:34:56Z\tt3\t--task-model claude:opus:high\n" +
				"2026-10-05T08:00:00Z\torca\t--task-model codex:gpt-6-astra --external-reviewers claude:opus\n" +
				"2026-10-04T09:30:00Z\tcli\t\n",
			want: []launchEntry{
				{When: t1, Launcher: launcherT3, Flags: "--task-model claude:opus:high"},
				{When: t2, Launcher: launcherOrca, Flags: "--task-model codex:gpt-6-astra --external-reviewers claude:opus"},
				{When: t3, Launcher: launcherCLI, Flags: ""},
			},
		},
		{
			name: "blank malformed and unknown lines skipped",
			content: "\n   \n" +
				"not a history line\n" +
				"2026-10-06T12:34:56Z\tt3\n" +
				"yesterday\tcli\t--task-model claude:opus\n" +
				"2026-10-06T12:34:56Z\tvscode\t--task-model claude:opus\n" +
				"2026-10-06T12:34:56Z\tT3\t--task-model claude:opus\n" +
				"2026-10-05T08:00:00Z\torca\t--task-model claude:sonnet\r\n",
			want: []launchEntry{{When: t2, Launcher: launcherOrca, Flags: "--task-model claude:sonnet"}},
		},
		{
			name:    "non-UTC timestamp normalized",
			content: "2026-10-06T14:34:56+02:00\tcli\t--task-model claude:opus\n",
			want:    []launchEntry{{When: t1, Launcher: launcherCLI, Flags: "--task-model claude:opus"}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "launch-history")
			if !tc.missing {
				require.NoError(t, os.WriteFile(path, []byte(tc.content), 0o600))
			}
			assert.Equal(t, tc.want, readLaunchHistory(path))
		})
	}
}

func TestRecordLaunchCreatesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "loopai", "launch-history")
	when := time.Date(2026, 10, 6, 12, 34, 56, 0, time.FixedZone("x", 3600))

	require.NoError(t, recordLaunch(path, launchEntry{When: when, Launcher: launcherT3, Flags: "--task-model claude:opus:high"}))

	got, err := os.ReadFile(path) //nolint:gosec // test temp path
	require.NoError(t, err)
	assert.Equal(t, "2026-10-06T11:34:56Z\tt3\t--task-model claude:opus:high\n", string(got))
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}
	assertNoTempFiles(t, filepath.Dir(path), ".launch-history-*.tmp")
}

func TestRecordLaunchEmptyFlags(t *testing.T) {
	path := filepath.Join(t.TempDir(), "launch-history")
	base := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

	require.NoError(t, recordLaunch(path, launchEntry{When: base, Launcher: launcherCLI, Flags: "--task-model claude:opus"}))
	require.NoError(t, recordLaunch(path, launchEntry{When: base.Add(time.Minute), Launcher: launcherOrca, Flags: ""}))

	assert.Equal(t, []launchEntry{
		{When: base.Add(time.Minute), Launcher: launcherOrca, Flags: ""},
		{When: base, Launcher: launcherCLI, Flags: "--task-model claude:opus"},
	}, readLaunchHistory(path))
}

func TestRecordLaunchDedupKeepsNewerLauncher(t *testing.T) {
	path := filepath.Join(t.TempDir(), "launch-history")
	base := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

	require.NoError(t, recordLaunch(path, launchEntry{When: base, Launcher: launcherOrca, Flags: "--task-model claude:opus"}))
	require.NoError(t, recordLaunch(path, launchEntry{When: base.Add(time.Minute), Launcher: launcherCLI, Flags: "--task-model codex:gpt-6-astra"}))
	require.NoError(t, recordLaunch(path, launchEntry{When: base.Add(2 * time.Minute), Launcher: launcherT3, Flags: "--task-model claude:opus"}))

	assert.Equal(t, []launchEntry{
		{When: base.Add(2 * time.Minute), Launcher: launcherT3, Flags: "--task-model claude:opus"},
		{When: base.Add(time.Minute), Launcher: launcherCLI, Flags: "--task-model codex:gpt-6-astra"},
	}, readLaunchHistory(path))
}

func TestRecordLaunchCapsAtLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "launch-history")
	base := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

	for i := range launchHistoryLimit + 3 {
		entry := launchEntry{When: base.Add(time.Duration(i) * time.Minute), Launcher: launcherCLI, Flags: fmt.Sprintf("--task-model claude:m%d", i)}
		require.NoError(t, recordLaunch(path, entry))
	}

	got := readLaunchHistory(path)
	require.Len(t, got, launchHistoryLimit)
	assert.Equal(t, fmt.Sprintf("--task-model claude:m%d", launchHistoryLimit+2), got[0].Flags)
	assert.Equal(t, "--task-model claude:m3", got[launchHistoryLimit-1].Flags)
	content, err := os.ReadFile(path) //nolint:gosec // test temp path
	require.NoError(t, err)
	assert.Equal(t, launchHistoryLimit, strings.Count(string(content), "\n"))
}

func TestRecordLaunchDropsUnreadableLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "launch-history")
	require.NoError(t, os.WriteFile(path, []byte("garbage\n2026-10-05T08:00:00Z\tcli\t--task-model claude:opus\n"), 0o600))

	when := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	require.NoError(t, recordLaunch(path, launchEntry{When: when, Launcher: launcherT3, Flags: ""}))

	got, err := os.ReadFile(path) //nolint:gosec // test temp path
	require.NoError(t, err)
	assert.Equal(t, "2026-10-06T12:00:00Z\tt3\t\n2026-10-05T08:00:00Z\tcli\t--task-model claude:opus\n", string(got))
}

func TestRecordLaunchErrors(t *testing.T) {
	entry := launchEntry{When: time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC), Launcher: launcherCLI, Flags: "--task-model claude:opus"}

	t.Run("unwritable directory", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("directory permissions are not enforced on Windows")
		}
		if os.Geteuid() == 0 {
			t.Skip("root ignores directory permissions")
		}
		dir := filepath.Join(t.TempDir(), "readonly")
		require.NoError(t, os.Mkdir(dir, 0o500))
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) //nolint:gosec // restore permissions for cleanup

		err := recordLaunch(filepath.Join(dir, "launch-history"), entry)
		require.ErrorContains(t, err, "launch history")
		assert.NoFileExists(t, filepath.Join(dir, "launch-history"))
		assertNoTempFiles(t, dir, ".launch-history-*.tmp")
	})

	t.Run("replace fails", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "launch-history")
		require.NoError(t, os.Mkdir(path, 0o750))
		require.NoError(t, os.WriteFile(filepath.Join(path, "keep"), []byte("x"), 0o600))

		err := recordLaunch(path, entry)
		require.ErrorContains(t, err, "replace launch history")
		assertNoTempFiles(t, dir, ".launch-history-*.tmp")
	})
}
