package plan

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestExtractDrift(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    Drift
	}{
		{
			name: "collects annotations and skipped checkboxes",
			content: "# Plan\n\n➕ added API endpoint\n" +
				"  ⚠️ blocked by upstream\r\n" +
				"- [x] deploy preview (Skipped - not automatable)\n" +
				"- [ ] ordinary task\n",
			want: Drift{
				Added:   []string{"➕ added API endpoint"},
				Blocked: []string{"⚠️ blocked by upstream"},
				Skipped: []string{"deploy preview (Skipped - not automatable)"},
			},
		},
		{
			name: "ignores fenced examples",
			content: "before\n```markdown\n➕ fake addition\n⚠️ fake blocker\n" +
				"- [x] fake (skipped)\n```\n➕ real addition\n" +
				"~~~\n- [ ] other (SKIPPED manually)\n~~~\n⚠️ real blocker\n",
			want: Drift{
				Added:   []string{"➕ real addition"},
				Blocked: []string{"⚠️ real blocker"},
				Skipped: []string{},
			},
		},
		{
			name: "collects markdown bullets with indentation",
			content: "- ➕ dash\n  * ➕ star\n\t+ ➕ plus\n" +
				" - ⚠️ dash blocker\n*\t⚠️ star blocker\n+ ⚠️ plus blocker\n" +
				"-➕ not a bullet\nprose ➕ not an annotation\n" +
				"```md\n- ➕ fenced\n* ⚠️ fenced\n```\n",
			want: Drift{
				Added:   []string{"➕ dash", "➕ star", "➕ plus"},
				Blocked: []string{"⚠️ dash blocker", "⚠️ star blocker", "⚠️ plus blocker"},
				Skipped: []string{},
			},
		},
		{
			name: "collects checkbox annotations",
			content: "- [x] ➕ completed addition\n  - [X] ➕ uppercase completion\r\n" +
				"- [ ] ⚠️ pending blocker\n* [x] ➕ star addition\n+ [ ] ⚠️ plus blocker\n" +
				"- [x] ⚠️ blocked work (Skipped manually)\n" +
				"- [x] prose ➕ not an annotation\n- [y] ➕ invalid checkbox\n" +
				"```md\n- [x] ➕ fenced addition\n- [ ] ⚠️ fenced blocker\n```\n",
			want: Drift{
				Added:   []string{"➕ completed addition", "➕ uppercase completion", "➕ star addition"},
				Blocked: []string{"⚠️ pending blocker", "⚠️ plus blocker", "⚠️ blocked work (Skipped manually)"},
				Skipped: []string{"⚠️ blocked work (Skipped manually)"},
			},
		},
		{
			name:    "no drift",
			content: "# Plan\n\n- [x] regular work\n",
			want:    Drift{Added: []string{}, Blocked: []string{}, Skipped: []string{}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, ExtractDrift(tt.content))
		})
	}
}
