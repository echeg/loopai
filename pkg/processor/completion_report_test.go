package processor

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gitpkg "github.com/umputun/ralphex/pkg/git"
	planpkg "github.com/umputun/ralphex/pkg/plan"
	"github.com/umputun/ralphex/pkg/status"
)

func TestRenderRunFacts_EmptyRecord(t *testing.T) {
	got := renderRunFacts(RunRecord{}, RunFacts{})
	want := `## Metadata
- plan: not recorded
- branch: not recorded
- base: not recorded
- mode: not recorded
- executor: not recorded
- task model: not recorded
- review model: not recorded
- started: not recorded
- finished: not recorded
- task iterations: 0
- task failed retries: 0
- internal review first ran: false
- internal review loop iterations: 0
- internal review ended by: none
- post-review ran: false
- post-review iterations: 0

## Phase durations
- none

## Files
- none

## Diff totals
- files: 0
- additions: 0
- deletions: 0

## Commits
- none

## Backlog files
- none

## Plan drift
### Added
- none
### Blocked
- none
### Skipped
- none

## Validation
### Commands
- none
### Timings
- not recorded

## Finalize
- not run

## External reviewers
- none`
	assert.Equal(t, want, got)
}

func TestRenderRunFacts_Finalize(t *testing.T) {
	record := RunRecord{Finalize: &FinalizeOutcome{
		Status: FinalizeResolved, Base: "origin/main", BaseSHA: "abc123", Files: []string{"pkg/a.go", "b`c.md"},
	}}
	got := renderRunFacts(record, RunFacts{})
	assert.Contains(t, got, "## Finalize\n- status: resolved\n- base: origin/main\n- base commit: abc123\n"+
		"- result: merged origin/main (2 files resolved)\n- conflicted files:\n  - `pkg/a.go`\n  - `b\\`c.md`\n")

	record.Finalize = &FinalizeOutcome{Status: FinalizeBlocked, Reason: "go test failed", Base: "origin/main"}
	got = renderRunFacts(record, RunFacts{})
	assert.Contains(t, got, "- status: blocked\n- base: origin/main\n- base commit: none\n- result: blocked: go test failed\n")
	assert.NotContains(t, got, "conflicted files")
	assert.Contains(t, factsOnlyReport(record, RunFacts{}), "- finalize: blocked: go test failed\n")
	assert.NotContains(t, factsOnlyReport(RunRecord{}, RunFacts{}), "- finalize:")
}

func TestRenderRunFacts_TablesAndTwoReviewers(t *testing.T) {
	record := RunRecord{
		Plan: "docs/plans/20260906-report.md", Branch: "report", BaseRef: "main", Mode: ModeFull,
		Executor: "codex", TaskModel: "task:high", ReviewModel: "review:medium",
		StartedAt:      time.Date(2026, 9, 6, 10, 0, 0, 0, time.FixedZone("EEST", 3*60*60)),
		FinishedAt:     time.Date(2026, 9, 6, 8, 30, 0, 0, time.UTC),
		PhaseDurations: map[string]Duration{"review": Duration(2 * time.Second), "task": Duration(1500 * time.Millisecond)},
		External: []ExternalReviewerRecord{
			{
				Key: "codex:gpt-5.5", Label: "Codex", Duration: Duration(3 * time.Second), EndedBy: "clean", HadFindings: true,
				Iterations: []ExternalIterationRecord{{Index: 1, ReviewerOutput: "finding with ``` fence", EvaluatorResponse: "fixed", Truncated: true}},
			},
			{Key: "claude:opus", Label: "Claude", Duration: Duration(time.Second), EndedBy: "no_findings"},
		},
	}
	facts := RunFacts{
		Commits:            []gitpkg.Commit{{Hash: "abc123", Subject: "add | report"}},
		Files:              []gitpkg.FileChange{{Status: "M", Path: "pkg/a|b.go"}},
		DiffStats:          gitpkg.DiffStats{Files: 1, Additions: 12, Deletions: 3},
		ValidationCommands: []string{"go test ./pkg/..."},
		Drift:              planpkg.Drift{Added: []string{"➕ extra coverage"}},
	}

	got := renderRunFacts(record, facts)

	assert.Contains(t, got, "| review | 2000 |\n| task | 1500 |", "phase rows must be sorted")
	assert.Contains(t, got, "| M | pkg/a\\|b.go |")
	assert.Contains(t, got, "- `abc123` add | report")
	assert.Contains(t, got, "## Backlog files\n- none")
	assert.Contains(t, got, "### codex:gpt-5.5\n- label: Codex\n- iterations: 1\n- duration_ms: 3000")
	assert.Contains(t, got, "#### Iteration 1\n- truncated: true")
	assert.Contains(t, got, "````\nfinding with ``` fence\n````")
	assert.Contains(t, got, "### claude:opus\n- label: Claude\n- iterations: 0")
	assert.Contains(t, got, "- started: 2026-09-06T07:00:00Z")
}

func TestRenderRunFacts_BacklogTable(t *testing.T) {
	facts := RunFacts{Backlog: []BacklogFile{{Status: "A", Path: "docs/backlog/item.md", Title: "A | B"}}}
	got := renderRunFacts(RunRecord{}, facts)
	assert.Contains(t, got, "| A | docs/backlog/item.md | A \\| B |")
}

func TestRenderRunFacts_BoundsLegacyReviewText(t *testing.T) {
	output := strings.Repeat("x", runRecordTextCap)
	record := RunRecord{External: []ExternalReviewerRecord{{Key: "legacy", EndedBy: "done", HadFindings: true}}}
	for i := 1; i <= 30; i++ {
		record.External[0].Iterations = append(record.External[0].Iterations, ExternalIterationRecord{
			Index: i, ReviewerOutput: output, EvaluatorResponse: output,
		})
	}
	for _, rendered := range []string{renderRunFacts(record, RunFacts{}), factsOnlyReport(record, RunFacts{})} {
		assert.LessOrEqual(t, strings.Count(rendered, "x"), runRecordExternalTextCap)
		assert.Contains(t, rendered, "- iterations: 30")
		assert.Contains(t, rendered, "#### Iteration 30\n- truncated: true")
		assert.Contains(t, rendered, "[truncated]")
		assert.Contains(t, rendered, "- ended by: done\n- had findings: true")
	}
	assert.Equal(t, output, record.External[0].Iterations[0].ReviewerOutput)
	assert.False(t, record.External[0].Iterations[0].Truncated)
}

func TestExtractReport(t *testing.T) {
	const reportWithEvidence = "# Report: feature\n\n## Summary\nDone\n\n## Change scope\nOne file\n\n" +
		"## Evidence\nBefore: regression test failed. After: regression test passes.\n\n## Risk\nlow\n\n" +
		"## Merge danger\n**Door:** two-way\nA revert restores the previous behavior.\n\n**Blast radius:** small"
	tests := []struct {
		name   string
		output string
		want   string
		ok     bool
	}{
		{name: "preamble", output: "I inspected the diff.\n# Report: feature\n\n## Summary\nDone", want: "# Report: feature\n\n## Summary\nDone", ok: true},
		{name: "new sections unchanged", output: reportWithEvidence, want: reportWithEvidence, ok: true},
		{name: "missing heading", output: "## Summary\nDone", ok: false},
		{name: "trailing signal", output: "# Report: feature\nbody\n" + status.Completed + "\n" + status.ReviewDone, want: "# Report: feature\nbody", ok: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := extractReport(tc.output)
			assert.Equal(t, tc.ok, ok)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestFactsOnlyReport_ContainsAllElevenHeadings(t *testing.T) {
	record := RunRecord{
		Plan: "docs/plans/20260906-completion-report.md", Branch: "completion-report", BaseRef: "main", Mode: ModeFull,
		External: []ExternalReviewerRecord{{Key: "codex:gpt-5.5", Iterations: []ExternalIterationRecord{{Index: 1}}}},
	}
	facts := RunFacts{
		Files:              []gitpkg.FileChange{{Status: "A", Path: "pkg/report.go"}},
		Backlog:            []BacklogFile{{Status: "A", Path: "docs/backlog/item.md", Title: "Item"}},
		ValidationCommands: []string{"go test ./pkg/..."}, Drift: planpkg.Drift{Skipped: []string{"manual test (skipped)"}},
	}

	report := factsOnlyReport(record, facts)

	headings := []string{
		"# Report:", "## Summary", "## Change scope", "## Evidence", "## Risk", "## Merge danger",
		"## Migrations and operational steps",
		"## Plan deviation", "## Backlog", "## External review", "## Validation",
	}
	var gotHeadings []string
	for line := range strings.SplitSeq(report, "\n") {
		switch {
		case strings.HasPrefix(line, "# Report:"):
			gotHeadings = append(gotHeadings, "# Report:")
		case strings.HasPrefix(line, "## "):
			gotHeadings = append(gotHeadings, line)
		}
	}
	assert.Equal(t, headings, gotHeadings, "all eleven sections must appear in contract order")
	assert.Equal(t, 10, strings.Count(report, "\n## "))
	assert.Contains(t, report, "## Evidence\n_assessment unavailable_\n")
	assert.Contains(t, report, "## Merge danger\n_assessment unavailable_\n")
	assert.Contains(t, report, "_assessment unavailable_")
	assert.Contains(t, report, "### codex:gpt-5.5")
	assert.Contains(t, report, "docs/backlog/item.md")
	require.NotContains(t, report, status.Completed)
}
