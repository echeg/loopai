package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/ralphex/pkg/git"
)

func TestSplitReportSections(t *testing.T) {
	tests := []struct {
		name   string
		report string
		want   []reportSection
	}{
		{name: "empty"},
		{name: "preamble only", report: "# Report: Feature\nMetadata\n### Summary\nnot a section"},
		{
			name: "nested headings and whitespace",
			report: "# Report: Feature\nignored\n\n## Summary\n\nDelivered.\n\n" +
				"## External review\n\n### reviewer\nfixed\n\n## Validation\npassed\n",
			want: []reportSection{
				{heading: "Summary", body: "Delivered."},
				{heading: "External review", body: "### reviewer\nfixed"},
				{heading: "Validation", body: "passed"},
			},
		},
		{
			name:   "CRLF and empty section",
			report: "metadata\r\n## Summary \r\n\r\n## Risk\r\nlow\r\n",
			want:   []reportSection{{heading: "Summary"}, {heading: "Risk", body: "low"}},
		},
		{
			name:   "only level two headings split",
			report: "## Summary\n# Title\n### Child\n#### Grandchild\n ## Indented\n##No space\n",
			want:   []reportSection{{heading: "Summary", body: "# Title\n### Child\n#### Grandchild\n ## Indented\n##No space"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, splitReportSections(tt.report))
		})
	}
}

func TestReportPRBody(t *testing.T) {
	stats := git.DiffStats{Files: 3, Additions: 12, Deletions: 4}
	statsText := "## Changes\n\n- Files changed: 3\n- Additions: 12\n- Deletions: 4"
	details := "<details><summary>External review</summary>\n\n### reviewer\nfinding -> fixed\n\n</details>\n\n" +
		"<details><summary>Validation</summary>\n\nTests passed.\n\n</details>"
	selected := "Delivered.\n\n## Evidence\n\nBefore failed, now passes.\n\n" +
		"## Merge danger\n\n**Door:** two-way\n**Blast radius:** small\n\n" +
		"## Risk\n\nLow.\n\n## Migrations and operational steps\n\nNone.\n\n## Plan deviation\n\nNone."
	oldSelected := "Delivered.\n\n## Risk\n\nLow.\n\n## Migrations and operational steps\n\nNone.\n\n## Plan deviation\n\nNone."
	tests := []struct {
		name    string
		report  string
		full    string
		trimmed string
		ok      bool
	}{
		{
			name: "full eleven section report",
			report: "# Report: Feature\nMetadata\n\n## Summary\nDelivered.\n\n" +
				"## Change scope\nOmitted scope.\n\n## Evidence\nBefore failed, now passes.\n\n" +
				"## Risk\nLow.\n\n## Merge danger\n**Door:** two-way\n**Blast radius:** small\n\n" +
				"## Migrations and operational steps\nNone.\n\n## Plan deviation\nNone.\n\n" +
				"## Backlog\nOmitted backlog.\n\n## External review\n### reviewer\nfinding -> fixed\n\n" +
				"## Validation\nTests passed.\n",
			full:    selected + "\n\n" + details + "\n\n" + statsText,
			trimmed: selected + "\n\n" + statsText,
			ok:      true,
		},
		{
			name: "report predating new sections",
			report: "## Summary\nDelivered.\n## Change scope\nOmitted scope.\n## Risk\nLow.\n" +
				"## Migrations and operational steps\nNone.\n## Plan deviation\nNone.\n## Backlog\nOmitted backlog.\n" +
				"## External review\n### reviewer\nfinding -> fixed\n## Validation\nTests passed.",
			full:    oldSelected + "\n\n" + details + "\n\n" + statsText,
			trimmed: oldSelected + "\n\n" + statsText,
			ok:      true,
		},
		{name: "missing summary", report: "# Report: Feature\n## Risk\nLow."},
		{name: "nested summary is insufficient", report: "## Risk\n### Summary\nDelivered."},
		{
			name:    "customized section order and absent sections",
			report:  "## Validation\nTests passed.\n## Risk\nLow.\n## Summary\nDelivered.\n## Custom\nOmitted.",
			full:    "Delivered.\n\n## Risk\n\nLow.\n\n<details><summary>Validation</summary>\n\nTests passed.\n\n</details>\n\n" + statsText,
			trimmed: "Delivered.\n\n## Risk\n\nLow.\n\n" + statsText,
			ok:      true,
		},
		{name: "empty summary", report: "## Summary\n", full: statsText, trimmed: statsText, ok: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			full, trimmed, ok := reportPRBody(tt.report, stats)
			assert.Equal(t, tt.ok, ok)
			assert.Equal(t, tt.full, full)
			assert.Equal(t, tt.trimmed, trimmed)
		})
	}
}

func TestFitPRBody(t *testing.T) {
	legacy := "Legacy overview.\n\n## Changes\n\n- Files changed: 0\n- Additions: 0\n- Deletions: 0"
	tests := []struct {
		name   string
		report string
		want   string
	}{
		{name: "full body fits", report: "## Summary\nDelivered.\n## External review\nReviewed.", want: "full"},
		{
			name: "oversized external review drops both details blocks",
			report: "## Summary\nDelivered.\n## External review\n" + strings.Repeat("x", maxPRBodyRunes) +
				"\n## Validation\nTests passed.",
			want: "trimmed",
		},
		{name: "oversized summary uses legacy", report: "## Summary\n" + strings.Repeat("x", maxPRBodyRunes), want: "legacy"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			full, trimmed, ok := reportPRBody(tt.report, git.DiffStats{})
			require.True(t, ok)
			candidates := map[string]string{"full": full, "trimmed": trimmed, "legacy": legacy}
			assert.Equal(t, candidates[tt.want], fitPRBody(full, trimmed, legacy))
		})
	}

	t.Run("rune limit is inclusive and counts unicode characters", func(t *testing.T) {
		body := strings.Repeat("\u754c", maxPRBodyRunes)
		assert.Equal(t, body, fitPRBody(body, "trimmed", legacy))
		assert.Equal(t, "trimmed", fitPRBody(body+"x", "trimmed", legacy))
	})
	t.Run("oversized legacy remains subject to metadata validation", func(t *testing.T) {
		oversized := strings.Repeat("x", maxPRBodyRunes+1)
		body := fitPRBody(oversized, oversized, oversized)
		assert.Equal(t, oversized, body)
		require.ErrorContains(t, validatePRMetadata("Feature", body), "GitHub limit")
	})
}

func TestReportPRBodyPreservesFencedSections(t *testing.T) {
	for _, tc := range []struct {
		name, block string
	}{
		{name: "backticks", block: "```markdown\n## Risk\nQuoted risk.\n## Custom\nExample.\n```"},
		{name: "tildes", block: "~~~markdown\n## Validation\nQuoted validation.\n~~~"},
		{name: "longer outer fence", block: "````\n```markdown\n## Risk\nExample.\n```\n## Validation\nStill quoted.\n````"},
		{name: "indented fence and longer closer", block: "   ~~~markdown\n## Risk\nExample.\n   ~~~~  "},
		{name: "mismatched fence", block: "```\n~~~\n## Validation\nQuoted.\n```"},
		{name: "info string does not close", block: "```\n```markdown\n## Validation\nQuoted.\n```"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.block = "Example:\n" + tc.block + "\nEnd example."
			report := "# Report: Feature\n## Summary\nDelivered.\n## Evidence\n" + tc.block +
				"\n## Risk\nActual risk.\n## External review\n" + tc.block + "\n## Validation\nActual validation."
			full, trimmed, ok := reportPRBody(report, git.DiffStats{})
			require.True(t, ok)
			assert.Contains(t, full, "## Evidence\n\n"+tc.block)
			assert.Contains(t, full, "## Risk\n\nActual risk.")
			assert.Contains(t, full, "<details><summary>External review</summary>\n\n"+tc.block+"\n\n</details>")
			assert.Contains(t, full, "<details><summary>Validation</summary>\n\nActual validation.\n\n</details>")
			assert.Contains(t, trimmed, "## Evidence\n\n"+tc.block)
			assert.Contains(t, trimmed, "## Risk\n\nActual risk.")
		})
	}
}

func TestSplitReportSectionsFenceBoundaries(t *testing.T) {
	t.Run("fenced preamble is not a summary", func(t *testing.T) {
		assert.Equal(t, []reportSection{{heading: "Risk", body: "Actual."}},
			splitReportSections("```\n## Summary\nExample.\n```\n## Risk\nActual."))
	})
	t.Run("unclosed fence consumes remaining headings", func(t *testing.T) {
		assert.Equal(t, []reportSection{{heading: "Evidence", body: "~~~\n## Risk\nQuoted."}},
			splitReportSections("## Evidence\n~~~\n## Risk\nQuoted."))
	})
	t.Run("backtick in info string is not a fence", func(t *testing.T) {
		assert.Equal(t, []reportSection{{heading: "Summary", body: "Actual."}},
			splitReportSections("```not`a-fence\n## Summary\nActual."))
	})
}

func TestReportRiskLevel(t *testing.T) {
	tests := []struct {
		name   string
		report string
		want   string
	}{
		{name: "bold low", report: "# Report: F\n## Summary\nDone.\n## Risk\n\n**low**\n\n- Public APIs unchanged.", want: "low"},
		{name: "capitalized with period", report: "## Risk\nLow.\n## Validation\npassed", want: "low"},
		{name: "code medium", report: "## Risk\n`medium`", want: "medium"},
		{name: "high with explanation", report: "## Risk\nhigh - rewrites the merge path", want: "high"},
		{name: "underscore emphasis and colon", report: "## Risk\n_Medium_: touches config", want: "medium"},
		{name: "first risk section wins", report: "## Risk\nlow\n## Risk\nhigh", want: "low"},
		{name: "fenced risk heading ignored", report: "## Evidence\n```\n## Risk\nlow\n```\n## Summary\nDone.", want: ""},
		{name: "fenced heading before real section", report: "## Evidence\n~~~\n## Risk\nlow\n~~~\n## Risk\nhigh", want: "high"},
		{name: "missing section", report: "# Report: F\n## Summary\nDone.", want: ""},
		{name: "empty section", report: "## Risk\n\n## Validation\npassed", want: ""},
		{name: "facts-only fallback", report: "## Risk\n\n_assessment unavailable_", want: ""},
		{name: "another first word", report: "## Risk\nMostly low, but the config loader changed.", want: ""},
		{name: "hyphenated range", report: "## Risk\nlow-to-medium", want: ""},
		{name: "CRLF", report: "# Report: F\r\n## Risk\r\n\r\n**Medium**\r\n\r\n- note\r\n", want: "medium"},
		{name: "empty report"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, reportRiskLevel(tt.report))
		})
	}
}
