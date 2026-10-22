package runs

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOutcomeRoundTrip(t *testing.T) {
	dir := t.TempDir()
	o := &Outcome{
		RunID:         "feature-x-20261022-153000-a1b2",
		Attempt:       2,
		Status:        StatusFailed,
		Reason:        "runner: task 3: failed",
		ExitCode:      1,
		Branch:        "feature-x",
		Commit:        "db78b1ba",
		WorktreeKept:  "/Users/u/repo/.loopai/worktrees/feature-x",
		Report:        "# Report: feature x\n",
		ReportSummary: "summary",
		Risk:          "medium",
		Finalize: Finalize{Mode: "sync", StatusNote: "synced", PRURL: "https://example.com/pr/1",
			Merged: true, Incomplete: "checks failed"},
		PlanMoved:         true,
		ArchiveIncomplete: "hook rejected",
		Notes:             []string{"source plan kept"},
		FinishedAt:        time.Date(2026, 10, 23, 9, 12, 0, 0, time.UTC),
	}
	require.NoError(t, SaveOutcome(dir, o))
	assert.Equal(t, schemaVersion, o.Version)

	loaded, err := LoadOutcome(dir)
	require.NoError(t, err)
	assert.Equal(t, *o, loaded)

	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(dir, OutcomeFile))
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}
}

func TestOutcomeWireKeys(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, SaveOutcome(dir, &Outcome{RunID: "x", Attempt: 1, Status: StatusSucceeded}))

	raw := readJSONObject(t, filepath.Join(dir, OutcomeFile))
	assert.ElementsMatch(t, []string{
		"version", "run_id", "attempt", "status", "reason", "exit_code", "branch", "commit",
		"worktree_kept", "report", "report_summary", "risk", "finalize", "plan_moved",
		"archive_incomplete", "notes", "finished_at",
	}, mapKeys(raw))
	assert.Equal(t, []any{}, raw["notes"], "nil notes are written as an empty list")
	finalize, ok := raw["finalize"].(map[string]any)
	require.True(t, ok)
	assert.ElementsMatch(t, []string{"mode", "status_note", "pr_url", "merged", "incomplete"}, mapKeys(finalize))
}

func TestSaveOutcomeCapsReportAndSummary(t *testing.T) {
	dir := t.TempDir()
	// a three-byte rune straddles both limits, so a byte cut would split it
	report := strings.Repeat("a", maxOutcomeReport-1) + "€tail"
	summary := strings.Repeat("b", maxOutcomeSummary-2) + "€tail"
	o := &Outcome{RunID: "x", Attempt: 1, Status: StatusSucceeded, Report: report, ReportSummary: summary}
	require.NoError(t, SaveOutcome(dir, o))
	assert.Equal(t, report, o.Report, "the caller keeps the full report")
	assert.Equal(t, summary, o.ReportSummary)

	loaded, err := LoadOutcome(dir)
	require.NoError(t, err)
	assert.Equal(t, strings.Repeat("a", maxOutcomeReport-1), loaded.Report)
	assert.Equal(t, strings.Repeat("b", maxOutcomeSummary-2), loaded.ReportSummary)
	assert.True(t, utf8.ValidString(loaded.Report))
	assert.True(t, utf8.ValidString(loaded.ReportSummary))
}

func TestCapReportSummary(t *testing.T) {
	short := "Delivered the feature."
	assert.Equal(t, short, CapReportSummary(short))
	long := strings.Repeat("b", maxOutcomeSummary-2) + "€tail"
	got := CapReportSummary(long)
	assert.Equal(t, strings.Repeat("b", maxOutcomeSummary-2), got, "the cap matches the outcome's")
	assert.True(t, utf8.ValidString(got))
}

func TestTruncateUTF8(t *testing.T) {
	tests := []struct {
		name  string
		in    string
		limit int
		want  string
	}{
		{name: "under the limit", in: "abc", limit: 5, want: "abc"},
		{name: "at the limit", in: "abc", limit: 3, want: "abc"},
		{name: "ascii cut", in: "abcdef", limit: 4, want: "abcd"},
		{name: "cut inside a rune", in: "ab€", limit: 4, want: "ab"},
		{name: "cut after a rune", in: "ab€c", limit: 5, want: "ab€"},
		{name: "zero limit", in: "abc", limit: 0, want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, truncateUTF8(tc.in, tc.limit))
		})
	}
}

func TestLoadOutcomeErrors(t *testing.T) {
	tests := []struct {
		name    string
		content string
		wantErr string
	}{
		{name: "corrupt json", content: `{"version": 1, "status": `, wantErr: "decode"},
		{name: "unknown version", content: `{"version": 3, "status": "failed"}`, wantErr: "unsupported version 3"},
		{name: "live status", content: `{"version": 1, "status": "running"}`, wantErr: `invalid status "running"`},
		{name: "empty status", content: `{"version": 1}`, wantErr: `invalid status ""`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, OutcomeFile), []byte(tc.content), 0o600))
			_, err := LoadOutcome(dir)
			require.ErrorContains(t, err, tc.wantErr)
			assert.NotErrorIs(t, err, fs.ErrNotExist)
		})
	}
}

func TestLoadOutcomeMissing(t *testing.T) {
	_, err := LoadOutcome(t.TempDir())
	require.ErrorIs(t, err, fs.ErrNotExist)
}

func TestArchiveOutcome(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, SaveOutcome(dir, &Outcome{RunID: "x", Attempt: 1, Status: StatusFailed, Reason: "first"}))

	require.NoError(t, ArchiveOutcome(dir, 1))
	_, err := LoadOutcome(dir)
	require.ErrorIs(t, err, fs.ErrNotExist, "the current outcome is gone")
	assertDirEntries(t, dir, "outcome-attempt-1.json")

	archived := readJSONObject(t, filepath.Join(dir, "outcome-attempt-1.json"))
	assert.Equal(t, "first", archived["reason"])

	require.NoError(t, ArchiveOutcome(dir, 2), "an attempt without an outcome archives nothing")
	assertDirEntries(t, dir, "outcome-attempt-1.json")
}

func TestArchiveOutcomeError(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, SaveOutcome(dir, &Outcome{RunID: "x", Attempt: 1, Status: StatusFailed}))
	target := filepath.Join(dir, "outcome-attempt-1.json")
	require.NoError(t, os.Mkdir(target, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(target, "keep"), []byte("x"), 0o600))

	require.ErrorContains(t, ArchiveOutcome(dir, 1), "archive run outcome")
	_, err := LoadOutcome(dir)
	require.NoError(t, err, "a failed archive leaves the outcome in place")
}
