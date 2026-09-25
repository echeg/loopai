package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestModelProvider(t *testing.T) {
	tests := []struct {
		spec string
		want string
	}{
		{spec: "claude-opus-5", want: ExternalReviewToolClaude},
		{spec: "opus", want: ExternalReviewToolClaude},
		{spec: "Opus:high", want: ExternalReviewToolClaude},
		{spec: "sonnet", want: ExternalReviewToolClaude},
		{spec: "haiku", want: ExternalReviewToolClaude},
		{spec: "fable:high", want: ExternalReviewToolClaude},
		{spec: "fable-next", want: ExternalReviewToolClaude},
		{spec: "gpt-6-astra:medium", want: ExternalReviewToolCodex},
		{spec: "GPT-5", want: ExternalReviewToolCodex},
		{spec: "codex-mini", want: ExternalReviewToolCodex},
		{spec: "o1", want: ExternalReviewToolCodex},
		{spec: "o1-mini", want: ExternalReviewToolCodex},
		{spec: "o3", want: ExternalReviewToolCodex},
		{spec: "o3-mini:high", want: ExternalReviewToolCodex},
		{spec: "o4", want: ExternalReviewToolCodex},
		{spec: "o4-mini", want: ExternalReviewToolCodex},
		{spec: "O4-MINI:high", want: ExternalReviewToolCodex},
		{spec: "o10"},
		{spec: "o3alias"},
		{spec: "o4alias"},
		{spec: ":high"},
		{spec: ""},
		{spec: "my-alias"},
		{spec: "github-copilot/claude-opus-4.6"},
		{spec: " \t\n"},
		{spec: " \tOpus:high\n", want: ExternalReviewToolClaude},
		{spec: " gpt-6-astra:medium ", want: ExternalReviewToolCodex},
		{spec: "opus:high:extra", want: ExternalReviewToolClaude},
		{spec: "my-alias:opus"},
	}

	for _, tt := range tests {
		t.Run(tt.spec, func(t *testing.T) {
			assert.Equal(t, tt.want, ModelProvider(tt.spec))
		})
	}
}
