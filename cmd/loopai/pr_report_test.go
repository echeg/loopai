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

func TestBuildReportPRTitleBodyAcrossWorktrees(t *testing.T) {
	for _, recordInThirdWorktree := range []bool{false, true} {
		name := "primary progress record"
		if recordInThirdWorktree {
			name = "third worktree progress record"
		}
		t.Run(name, func(t *testing.T) {
			repo := setupTestRepo(t)
			const branch = "custom/report-branch"
			linked := filepath.Join(t.TempDir(), "feature")
			runGit(t, repo, "worktree", "add", "-b", branch, linked)
			planFile := writePRReport(t, linked, "20261005-feature.md", "# Feature title\n## Overview\nLegacy overview.")
			writePRReport(t, linked, "20261005-feature.report.md", prReportFixture)
			runGit(t, linked, "add", "docs/plans/completed")
			runGit(t, linked, "commit", "-m", "archive with report")
			recordRoot := repo
			if recordInThirdWorktree {
				recordRoot = filepath.Join(t.TempDir(), "source")
				runGit(t, repo, "worktree", "add", "-b", "source", recordRoot)
			}
			writeProgressRecord(t, recordRoot, "progress-feature.txt", planFile, branch, 1)
			svc, err := git.NewService(linked, noopLogger())
			require.NoError(t, err)
			var output, warnings bytes.Buffer
			require.NoError(t, runReportCommand(t.Context(), svc, closeoutTarget{identifier: branch}, &output))
			assert.Contains(t, output.String(), prReportFixture)
			stats := git.DiffStats{Files: 2, Additions: 3, Deletions: 1}
			legacyTitle, _, err := buildPRTitleBody(linked, "", branch, stats)
			require.NoError(t, err)
			title, body, err := buildReportPRTitleBody(svc, closeoutTarget{}, branch, stats, &warnings)
			require.NoError(t, err)
			assert.Equal(t, legacyTitle, title)
			assert.Contains(t, body, "Delivered report-backed PRs.")
			assert.Contains(t, body, "## Merge danger")
			assert.Contains(t, body, "<details><summary>Validation</summary>")
			assert.Empty(t, warnings.String())
		})
	}
}

func TestRunReportCommandUnrestrictedFallback(t *testing.T) {
	for _, kind := range []string{"external plans directory", "symlinked plans directory", "symlinked report", "oversized report"} {
		t.Run(kind, func(t *testing.T) {
			dir := setupTestRepo(t)
			plansDir := filepath.Join(dir, "docs", "plans")
			switch kind {
			case "external plans directory":
				plansDir = t.TempDir()
			case "symlinked plans directory":
				require.NoError(t, os.MkdirAll(filepath.Dir(plansDir), 0o750))
				require.NoError(t, os.Symlink(t.TempDir(), plansDir))
			}
			completed := filepath.Join(plansDir, "completed")
			require.NoError(t, os.MkdirAll(completed, 0o750))
			planPath := filepath.Join(completed, "feature.md")
			require.NoError(t, os.WriteFile(planPath, []byte("# Feature\n"), 0o600))
			reportPath := filepath.Join(completed, "feature.report.md")
			report := prReportFixture
			if kind == "oversized report" {
				report += strings.Repeat("x", int(maxPRPlanSize)) + "\n"
			}
			if kind == "symlinked report" {
				outside := filepath.Join(t.TempDir(), "report.md")
				require.NoError(t, os.WriteFile(outside, []byte(report), 0o600))
				require.NoError(t, os.Symlink(outside, reportPath))
			} else {
				require.NoError(t, os.WriteFile(reportPath, []byte(report), 0o600))
			}
			runGit(t, dir, "branch", "feature")
			runGit(t, dir, "branch", "-d", "feature")
			svc, err := git.NewService(dir, noopLogger())
			require.NoError(t, err)
			var output bytes.Buffer
			require.NoError(t, runReportCommand(t.Context(), svc, closeoutTarget{identifier: "feature", plansDir: plansDir}, &output))
			assert.Equal(t, "branch: (merged)\n\n"+report, output.String())
		})
	}
}
