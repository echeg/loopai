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
