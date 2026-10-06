package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/ralphex/pkg/config"
	"github.com/umputun/ralphex/pkg/git"
)

func TestRunPRReportAcceptance(t *testing.T) {
	for _, tc := range []struct {
		name, report string
	}{
		{name: "committed sidecar", report: prReportFixture},
		{name: "no report keeps legacy body"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setupFinalizePRFixture(t)
			planPath := filepath.Join(f.dir, "docs", "plans", "completed", "20261005-feature.md")
			require.NoError(t, os.MkdirAll(filepath.Dir(planPath), 0o750))
			require.NoError(t, os.WriteFile(planPath, []byte("# Feature title\n\n## Overview\n\nLegacy overview.\n"), 0o600))
			runGit(t, f.dir, "add", "docs/plans/completed/20261005-feature.md")
			if tc.report != "" {
				writePRReport(t, f.dir, "20261005-feature.report.md", tc.report)
				runGit(t, f.dir, "add", "docs/plans/completed/20261005-feature.report.md")
			}
			runGit(t, f.dir, "commit", "-m", "completed plan")
			var output bytes.Buffer

			require.NoError(t, runPRCommand(t.Context(), f.svc, "master", closeoutTarget{},
				&recordingStatusClearer{}, &output))

			assert.Equal(t, "https://github.com/acme/repo/pull/7\n", output.String())
			calls := f.ghCalls(t)
			require.Len(t, calls, 2)
			assert.Contains(t, calls[1], "--title Feature title --body-file -")
			stats, err := f.svc.BranchDiffStats("master", "feature")
			require.NoError(t, err)
			statsBody := fmt.Sprintf("## Changes\n\n- Files changed: %d\n- Additions: %d\n- Deletions: %d",
				stats.Files, stats.Additions, stats.Deletions)
			body := f.prBody(t)
			if tc.report == "" {
				assert.Equal(t, "Legacy overview.\n\n"+statsBody, body)
				return
			}
			assert.True(t, strings.HasPrefix(body, "Delivered report-backed PRs.\n"), body)
			assert.Contains(t, body, "## Merge danger\n\n**Door:** two-way\n**Blast radius:** small")
			assert.Contains(t, body, "<details><summary>External review</summary>\n\n### reviewer")
			assert.Contains(t, body, "<details><summary>Validation</summary>\n\nTests passed.")
			assert.True(t, strings.HasSuffix(body, statsBody), body)
			assert.NotContains(t, body, "Legacy overview.")
			pushed, err := f.svc.ShowFile("refs/heads/feature", "docs/plans/completed/20261005-feature.report.md")
			require.NoError(t, err)
			assert.Equal(t, tc.report, string(pushed))
			assert.Equal(t, strings.TrimSpace(gitOutput(t, f.dir, "rev-parse", "feature")),
				strings.TrimSpace(gitOutput(t, f.remote, "rev-parse", "refs/heads/feature")))
		})
	}
}

const oldCompletionReport = `# Report: Finalize
Plan: finalize.md; branch: finalize; base: master; mode: full

## Summary
Delivered with the previous report contract.

## Change scope
One feature file.

## Risk
low

## Migrations and operational steps
none

## Plan deviation
none

## Backlog
There were none.

## External review
### reviewer
body compatibility -> fixed

## Validation
Acceptance tests passed.
`

func TestExecutePlanFinalizeReportAcceptance(t *testing.T) {
	for _, tc := range []struct {
		name, promptPath, report string
		oldContract              bool
	}{
		{name: "worktree carries in-memory report", promptPath: "../../pkg/config/defaults/prompts/report.txt", report: prReportFixture},
		{name: "unmodified pre-change prompt remains compatible", promptPath: "testdata/report-pre-evidence.txt",
			report: oldCompletionReport, oldContract: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prompt, err := os.ReadFile(tc.promptPath)
			require.NoError(t, err)
			run := runFinalizePlanWithReport(t, config.FinalizePR, true, string(prompt), tc.report)

			assert.Equal(t, 1, strings.Count(strings.Join(run.sessions, " "), "report"))
			archived, err := os.ReadFile(filepath.Join(run.f.dir, "docs", "plans", "completed", "finalize.report.md"))
			require.NoError(t, err)
			assert.Equal(t, strings.TrimSpace(tc.report), strings.TrimSpace(string(archived)))
			_, err = run.f.svc.ShowFile("refs/heads/finalize", "docs/plans/completed/finalize.report.md")
			require.ErrorIs(t, err, git.ErrPathNotFound, "the sidecar is archived in the source checkout, outside the PR branch")
			body := run.f.prBody(t)
			assert.Contains(t, body, "<details><summary>External review</summary>\n\n### reviewer")
			assert.Contains(t, body, "<details><summary>Validation</summary>\n\n")
			assert.Contains(t, body, "## Changes\n\n- Files changed: 1\n- Additions: 1\n- Deletions: 0")
			assert.NoDirExists(t, filepath.Join(run.f.dir, ".loopai", "worktrees", "finalize"))
			if tc.oldContract {
				assert.True(t, strings.HasPrefix(body, "Delivered with the previous report contract.\n"), body)
				assert.Contains(t, body, "## Risk\n\nlow")
				assert.Contains(t, body, "## Migrations and operational steps\n\nnone")
				assert.Contains(t, body, "## Plan deviation\n\nnone")
				assert.NotContains(t, body, "## Evidence")
				assert.NotContains(t, body, "## Merge danger")
				actualPrompt, err := os.ReadFile(os.Getenv("FINALIZE_REPORT_PROMPT_LOG"))
				require.NoError(t, err)
				assert.Contains(t, string(actualPrompt), "The report must use exactly these sections in this order:")
				assert.NotContains(t, string(actualPrompt), "## Evidence")
				assert.NotContains(t, string(actualPrompt), "## Merge danger")
				return
			}
			assert.True(t, strings.HasPrefix(body, "Delivered report-backed PRs.\n"), body)
			assert.Contains(t, body, "## Evidence")
			assert.Contains(t, body, "## Merge danger")
		})
	}
}

// withFinalizeReport supplies a deterministic model response while exercising the real report prompt,
// executor, archival, and closeout path. The response is not an actual model assessment.
func withFinalizeReport(t *testing.T, script, prompt, report string) string {
	t.Helper()
	event, err := json.Marshal(map[string]any{
		"type": "content_block_delta", "delta": map[string]string{"type": "text_delta", "text": report},
	})
	require.NoError(t, err)
	eventsPath := filepath.Join(t.TempDir(), "report-events.jsonl")
	require.NoError(t, os.WriteFile(eventsPath, append(event, []byte("\n{\"type\":\"result\",\"result\":\"\"}\n")...), 0o600))
	t.Setenv("FINALIZE_REPORT_PROMPT_LOG", filepath.Join(t.TempDir(), "report-prompt.txt"))
	require.Contains(t, prompt, "Create the completion report for the implementation at")
	return strings.Replace(script, "prompt=$(cat)", `prompt=$(cat)
case "$prompt" in
*'Create the completion report for the implementation at'*)
  printf '%s\n' report >> '%SESSIONS%'
  printf '%s\n' "$prompt" > "$FINALIZE_REPORT_PROMPT_LOG"
  cat '`+eventsPath+`'
  exit 0 ;;
esac`, 1)
}

func TestFinalizeFactsOnlyReportKeepsPlanOverview(t *testing.T) {
	prompt, err := os.ReadFile("../../pkg/config/defaults/prompts/report.txt")
	require.NoError(t, err)
	run := runFinalizePlanWithReport(t, config.FinalizePR, true, string(prompt), "No report heading; use deterministic facts.")
	report, err := os.ReadFile(filepath.Join(run.f.dir, "docs", "plans", "completed", "finalize.report.md"))
	require.NoError(t, err)
	require.Contains(t, string(report), "## Summary\n- task iterations:")
	require.Contains(t, string(report), "_assessment unavailable_")
	assert.NotContains(t, run.f.prBody(t), "task iterations:")
	assert.NotContains(t, run.f.prBody(t), "_assessment unavailable_")

	// Feed the actual factsOnlyReport output back through both PR sources, using a
	// plan with an overview to verify it survives instead of just the diff stats.
	planPath := filepath.Join(run.f.dir, "docs", "plans", "completed", "finalize.md")
	require.NoError(t, os.WriteFile(planPath, []byte("# Finalize\n\n## Overview\n\nPreserve the plan overview.\n"), 0o600))
	for _, memory := range []string{string(report), ""} {
		var warnings bytes.Buffer
		_, body, err := buildReportPRTitleBody(run.f.svc, closeoutTarget{report: memory}, "finalize", git.DiffStats{}, &warnings)
		require.NoError(t, err)
		assert.Equal(t, "Preserve the plan overview.\n\n## Changes\n\n- Files changed: 0\n- Additions: 0\n- Deletions: 0", body)
		assert.Empty(t, warnings.String())
	}
}
