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
