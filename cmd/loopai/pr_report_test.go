package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/ralphex/pkg/git"
)

const prReportFixture = `# Report: Feature

## Summary

Delivered report-backed PRs.

## Evidence

The PR body regression now passes.

## Merge danger

**Door:** two-way
**Blast radius:** small

## Risk

low

## External review

### reviewer

body lookup -> fixed

## Validation

Tests passed.
`

func writePRReport(t *testing.T, root, name, report string) string {
	t.Helper()
	path := filepath.Join(root, "docs", "plans", "completed", name)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
	require.NoError(t, os.WriteFile(path, []byte(report), 0o600))
	return path
}

func TestBuildPRTitleBodyReport(t *testing.T) {
	tests := []struct {
		name, committed, disk, memory, branch, warning string
		wantReport                                     string
	}{
		{name: "branch only", committed: prReportFixture, wantReport: prReportFixture},
		{name: "working tree only", disk: prReportFixture, wantReport: prReportFixture},
		{name: "branch wins over disk", committed: prReportFixture, disk: "## Summary\nStale disk.", wantReport: prReportFixture},
		{name: "no sidecar"},
		{name: "memory wins over branch and disk", committed: prReportFixture, disk: prReportFixture,
			memory: "## Summary\nIn memory.\n## Merge danger\nTwo-way.", wantReport: "## Summary\nIn memory.\n## Merge danger\nTwo-way."},
		{name: "memory works without sidecar", memory: prReportFixture, wantReport: prReportFixture},
		{name: "old report", disk: "## Summary\nOld summary.\n## Risk\nLow.", wantReport: "## Summary\nOld summary.\n## Risk\nLow."},
		{name: "no summary", disk: "## Risk\nLow."},
		{name: "oversized review trims details", disk: "## Summary\nDone.\n## External review\n" + strings.Repeat("x", maxPRBodyRunes),
			wantReport: "## Summary\nDone."},
		{name: "oversized summary uses legacy", disk: "## Summary\n" + strings.Repeat("x", maxPRBodyRunes)},
		{name: "oversized committed report warns", committed: strings.Repeat("x", int(maxPRPlanSize)+1), disk: prReportFixture,
			warning: "size limit"},
		{name: "oversized disk report warns", disk: strings.Repeat("x", int(maxPRPlanSize)+1), warning: "size limit"},
		{name: "invalid revision warns", branch: "missing", disk: prReportFixture, warning: "read completion report from branch"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := setupTestRepo(t)
			planDir := filepath.Join(dir, "docs", "plans")
			require.NoError(t, os.MkdirAll(planDir, 0o750))
			require.NoError(t, os.WriteFile(filepath.Join(planDir, "20261005-feature.md"),
				[]byte("# Feature title\n\n## Overview\n\nLegacy overview."), 0o600))
			runGit(t, dir, "add", "docs/plans")
			runGit(t, dir, "commit", "-m", "plan")
			runGit(t, dir, "checkout", "-b", "feature")
			if tc.committed != "" {
				writePRReport(t, dir, "20261005-feature.report.md", tc.committed)
				runGit(t, dir, "add", "docs/plans/completed")
				runGit(t, dir, "commit", "-m", "report")
			}
			runGit(t, dir, "checkout", "master")
			if tc.disk != "" {
				writePRReport(t, dir, "20261005-feature.report.md", tc.disk)
			}
			svc, err := git.NewService(dir, noopLogger())
			require.NoError(t, err)
			branch := tc.branch
			if branch == "" {
				branch = "feature"
			}
			// Keep plan identity stable even in the invalid-revision warning case.
			writeProgressRecord(t, dir, "progress-feature.txt", filepath.Join(planDir, "20261005-feature.md"), branch, 1)
			var warnings bytes.Buffer
			stats := git.DiffStats{Files: 2, Additions: 3, Deletions: 1}
			title, body, err := buildReportPRTitleBody(svc, closeoutTarget{report: tc.memory}, branch, stats, &warnings)
			require.NoError(t, err)
			assert.Equal(t, "Feature title", title)
			if tc.wantReport != "" {
				full, _, ok := reportPRBody(tc.wantReport, stats)
				require.True(t, ok)
				assert.Equal(t, full, body)
			} else {
				assert.Equal(t, "Legacy overview.\n\n## Changes\n\n- Files changed: 2\n- Additions: 3\n- Deletions: 1", body)
			}
			if tc.warning == "" {
				assert.Empty(t, warnings.String())
			} else {
				assert.Contains(t, warnings.String(), "warning: completion report unavailable for PR body:")
				assert.Contains(t, warnings.String(), tc.warning)
			}
		})
	}
}

func TestLocateCompletionReport(t *testing.T) {
	t.Run("alternate committed path precedes canonical disk path", func(t *testing.T) {
		dir := setupTestRepo(t)
		runGit(t, dir, "checkout", "-b", "feature")
		path := writePRReport(t, dir, "2026-10-05-feature.report.md", prReportFixture)
		runGit(t, dir, "add", "docs/plans/completed")
		runGit(t, dir, "commit", "-m", "alternate report")
		runGit(t, dir, "checkout", "master")
		writePRReport(t, dir, "20261005-feature.report.md", "disk")
		svc, err := git.NewService(dir, noopLogger())
		require.NoError(t, err)
		body, source, err := locateCompletionReport(svc, "docs/plans", "20261005-feature.md", "feature")
		require.NoError(t, err)
		assert.Equal(t, prReportFixture, string(body))
		assert.Equal(t, "refs/heads/feature:"+path, source)
	})

	t.Run("missing report is a sentinel", func(t *testing.T) {
		dir := setupTestRepo(t)
		svc, err := git.NewService(dir, noopLogger())
		require.NoError(t, err)
		body, source, err := locateCompletionReport(svc, "docs/plans", "feature.md", "master")
		require.ErrorIs(t, err, errCompletionReportNotFound)
		assert.Nil(t, body)
		assert.Empty(t, source)
	})

	t.Run("working tree source with no branch", func(t *testing.T) {
		dir := setupTestRepo(t)
		path := writePRReport(t, dir, "20261005-feature.report.md", prReportFixture)
		svc, err := git.NewService(dir, noopLogger())
		require.NoError(t, err)
		body, source, err := locateCompletionReport(svc, "docs/plans", "20261005-feature.md", "")
		require.NoError(t, err)
		assert.Equal(t, prReportFixture, string(body))
		assert.Equal(t, path, source)
	})

	t.Run("symlink errors warn and preserve legacy body", func(t *testing.T) {
		dir := setupTestRepo(t)
		path := writePRReport(t, dir, "20261005-feature.md", "# Feature\n## Overview\nLegacy.")
		reportPath := strings.TrimSuffix(path, ".md") + ".report.md"
		outside := filepath.Join(t.TempDir(), "report.md")
		require.NoError(t, os.WriteFile(outside, []byte(prReportFixture), 0o600))
		require.NoError(t, os.Symlink(outside, reportPath))
		runGit(t, dir, "branch", "feature")
		svc, err := git.NewService(dir, noopLogger())
		require.NoError(t, err)
		_, _, err = locateCompletionReport(svc, "docs/plans", path, "master")
		require.ErrorContains(t, err, "symlink")
		var warnings bytes.Buffer
		_, body, err := buildReportPRTitleBody(svc, closeoutTarget{}, "feature", git.DiffStats{}, &warnings)
		require.NoError(t, err)
		assert.Contains(t, body, "Legacy.")
		assert.Contains(t, warnings.String(), "warning:")
	})
}

func TestBuildReportPRTitleBodyPlanError(t *testing.T) {
	dir := setupTestRepo(t)
	path := writePRReport(t, dir, "20261005-feature.md", strings.Repeat("x", int(maxPRPlanSize)+1))
	svc, err := git.NewService(dir, noopLogger())
	require.NoError(t, err)
	_, _, err = buildReportPRTitleBody(svc, closeoutTarget{report: prReportFixture}, "feature", git.DiffStats{}, io.Discard)
	require.ErrorContains(t, err, "read PR plan")
	assert.FileExists(t, path)
}
