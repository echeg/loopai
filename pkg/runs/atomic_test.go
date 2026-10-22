package runs

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWriteFileAtomic(t *testing.T) {
	tests := []struct {
		name string
		perm os.FileMode
	}{
		{name: "private", perm: 0o600},
		{name: "readable", perm: 0o644},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "state.json")

			require.NoError(t, WriteFileAtomic(path, []byte("first"), tc.perm))
			require.NoError(t, WriteFileAtomic(path, []byte("second"), tc.perm))

			got, err := os.ReadFile(path) //nolint:gosec // test temp path
			require.NoError(t, err)
			assert.Equal(t, "second", string(got))
			if runtime.GOOS != "windows" {
				info, err := os.Stat(path)
				require.NoError(t, err)
				assert.Equal(t, tc.perm, info.Mode().Perm())
			}
			assertDirEntries(t, dir, "state.json")
		})
	}
}

func TestWriteFileAtomicMissingDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "state.json")
	err := WriteFileAtomic(path, []byte("x"), 0o600)
	require.ErrorContains(t, err, "create temp file")
	assert.NoDirExists(t, filepath.Dir(path), "the directory is never recreated")
}

func TestWriteFileAtomicRenameFailureCleansUp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	require.NoError(t, os.Mkdir(path, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(path, "keep"), []byte("x"), 0o600))

	err := WriteFileAtomic(path, []byte("x"), 0o600)
	require.ErrorContains(t, err, "rename temp file")
	assertDirEntries(t, dir, "state.json")
	assert.FileExists(t, filepath.Join(path, "keep"))
}

// assertDirEntries asserts that dir holds exactly the named entries, so a leftover temp file
// fails whatever its name.
func assertDirEntries(t *testing.T, dir string, names ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	got := make([]string, 0, len(entries))
	for _, entry := range entries {
		got = append(got, entry.Name())
	}
	assert.ElementsMatch(t, names, got)
}
