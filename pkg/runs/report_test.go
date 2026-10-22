package runs

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReportRoundTrip(t *testing.T) {
	dir := t.TempDir()
	// longer than the outcome cap: report.md keeps the full text
	report := "# Report: x\n" + strings.Repeat("line\n", maxOutcomeReport/4)

	require.NoError(t, WriteReport(dir, "# Report: old\n"))
	require.NoError(t, WriteReport(dir, report))
	got, err := ReadReport(dir)
	require.NoError(t, err)
	assert.Equal(t, report, got)

	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(dir, ReportFile))
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}
}

func TestReportErrors(t *testing.T) {
	_, err := ReadReport(t.TempDir())
	require.ErrorIs(t, err, fs.ErrNotExist)

	require.ErrorContains(t, WriteReport(filepath.Join(t.TempDir(), "missing"), "x"), "write run report")
}
