package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/ralphex/pkg/runs"
)

// journalDirRepo creates a git repository whose .gitignore ignores ignored/, makes it the working
// directory, and returns its path.
func journalDirRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cmd := exec.Command("git", "init", "-q", dir)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("ignored/\n"), 0o600))
	t.Chdir(dir)
	return dir
}

func TestPrepareJournalDir(t *testing.T) {
	tests := []struct {
		name string
		// setup prepares the working directory and returns the --journal-dir argument and the
		// absolute path it must resolve to.
		setup   func(t *testing.T) (arg, want string)
		wantErr string
		// createdOnRefusal reports whether the directory exists after a refusal (it did before).
		createdOnRefusal bool
	}{
		{name: "relative path made absolute", setup: func(t *testing.T) (string, string) {
			cwd := t.TempDir()
			t.Chdir(cwd)
			return "journal", filepath.Join(cwd, "journal")
		}},
		{name: "missing nested directory created", setup: func(t *testing.T) (string, string) {
			dir := filepath.Join(t.TempDir(), "a", "b")
			return dir, dir
		}},
		{name: "existing empty directory accepted", setup: func(t *testing.T) (string, string) {
			dir := t.TempDir()
			return dir, dir
		}},
		{name: "existing events.jsonl refused", setup: func(t *testing.T) (string, string) {
			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, runs.JournalFile), nil, 0o600))
			return dir, dir
		}, wantErr: "already holds events.jsonl", createdOnRefusal: true},
		{name: "existing outcome.json refused", setup: func(t *testing.T) (string, string) {
			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, runs.OutcomeFile), []byte("{}\n"), 0o600))
			return dir, dir
		}, wantErr: "already holds outcome.json", createdOnRefusal: true},
		{name: "dangling events.jsonl symlink refused", setup: func(t *testing.T) (string, string) {
			dir := t.TempDir()
			require.NoError(t, os.Symlink(filepath.Join(dir, "gone"), filepath.Join(dir, runs.JournalFile)))
			return dir, dir
		}, wantErr: "already holds events.jsonl", createdOnRefusal: true},
		{name: "regular file refused", setup: func(t *testing.T) (string, string) {
			path := filepath.Join(t.TempDir(), "file")
			require.NoError(t, os.WriteFile(path, []byte("x"), 0o600))
			return path, path
		}, wantErr: "is not a directory", createdOnRefusal: true},
		{name: "in-tree non-ignored directory refused", setup: func(t *testing.T) (string, string) {
			repo := journalDirRepo(t)
			return "journal", filepath.Join(repo, "journal")
		}, wantErr: "inside the repository work tree and git does not ignore events.jsonl"},
		{name: "in-tree non-ignored existing directory refused", setup: func(t *testing.T) (string, string) {
			repo := journalDirRepo(t)
			require.NoError(t, os.Mkdir(filepath.Join(repo, "journal"), 0o700))
			return filepath.Join(repo, "journal"), filepath.Join(repo, "journal")
		}, wantErr: "inside the repository work tree", createdOnRefusal: true},
		{name: "in-tree ignored directory accepted", setup: func(t *testing.T) (string, string) {
			repo := journalDirRepo(t)
			return filepath.Join("ignored", "run-1"), filepath.Join(repo, "ignored", "run-1")
		}},
		{name: "directory outside the repository accepted", setup: func(t *testing.T) (string, string) {
			journalDirRepo(t)
			dir := filepath.Join(t.TempDir(), "journal")
			return dir, dir
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			arg, want := tc.setup(t)
			got, err := prepareJournalDir(arg)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				assert.Empty(t, got)
				_, statErr := os.Stat(want)
				assert.Equal(t, tc.createdOnRefusal, statErr == nil, "a refusal must not create the directory")
				return
			}
			require.NoError(t, err)
			wantResolved, evalErr := filepath.EvalSymlinks(want)
			require.NoError(t, evalErr)
			gotResolved, evalErr := filepath.EvalSymlinks(got)
			require.NoError(t, evalErr)
			assert.True(t, filepath.IsAbs(got))
			assert.Equal(t, wantResolved, gotResolved)
			info, statErr := os.Stat(got)
			require.NoError(t, statErr)
			assert.True(t, info.IsDir())
			entries, readErr := os.ReadDir(got)
			require.NoError(t, readErr)
			assert.Empty(t, entries, "preparing the directory writes nothing into it")
		})
	}
}

func TestPrepareJournalDirCreatesWithMode0700(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "journal")
	got, err := prepareJournalDir(dir)
	require.NoError(t, err)
	assert.Equal(t, dir, got)
	info, err := os.Stat(dir)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())
}

func TestPrepareJournalDirKeepsExistingMode(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "journal")
	require.NoError(t, os.Mkdir(dir, 0o750))
	_, err := prepareJournalDir(dir)
	require.NoError(t, err)
	info, err := os.Stat(dir)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o750), info.Mode().Perm())
}

func TestRunRefusesJournalDirBeforeConfig(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)
	journal := filepath.Join(t.TempDir(), "journal")
	require.NoError(t, os.Mkdir(journal, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(journal, runs.OutcomeFile), []byte("{}\n"), 0o600))
	configDir := filepath.Join(t.TempDir(), "config")

	err := run(t.Context(), opts{PlanFile: "p.md", JournalDir: journal, journalDirSet: true, ConfigDir: configDir})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already holds outcome.json")
	_, statErr := os.Stat(configDir)
	assert.True(t, os.IsNotExist(statErr), "the refusal comes before config loading installs defaults")
}
