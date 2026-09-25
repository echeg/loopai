package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseProviderSpec(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    ProviderSpec
		wantErr error
		wantMsg string
	}{
		{name: "provider model effort", value: "codex:gpt-6-astra:high",
			want: ProviderSpec{Provider: ExternalReviewToolCodex, Model: "gpt-6-astra", Effort: "high"}},
		{name: "provider model", value: "claude:opus", want: ProviderSpec{Provider: ExternalReviewToolClaude, Model: "opus"}},
		{name: "provider only", value: "codex", want: ProviderSpec{Provider: ExternalReviewToolCodex}},
		{name: "default model explicit effort", value: "codex::medium",
			want: ProviderSpec{Provider: ExternalReviewToolCodex, Effort: "medium"}},
		{name: "trailing separator leaves model empty", value: "claude:", want: ProviderSpec{Provider: ExternalReviewToolClaude}},
		{name: "custom", value: "custom", want: ProviderSpec{Provider: ExternalReviewToolCustom}},
		{name: "surrounding and inner spaces", value: "  codex : gpt-6-astra : xhigh  ",
			want: ProviderSpec{Provider: ExternalReviewToolCodex, Model: "gpt-6-astra", Effort: "xhigh"}},
		{name: "provider is case-insensitive", value: "Claude:Opus:high",
			want: ProviderSpec{Provider: ExternalReviewToolClaude, Model: "Opus", Effort: "high"}},
		{name: "provider prefix wins over model-name prefix", value: "codex:high",
			want: ProviderSpec{Provider: ExternalReviewToolCodex, Model: "high"}},
		{name: "empty string", value: "", wantErr: ErrEmptyProviderSpec, wantMsg: "provider[:model[:effort]]"},
		{name: "whitespace only", value: "   ", wantErr: ErrEmptyProviderSpec},
		{name: "four segments", value: "codex:gpt-6-astra:high:extra", wantErr: ErrTooManySpecSegments,
			wantMsg: `"codex:gpt-6-astra:high:extra" has too many ':' separators; expected provider[:model[:effort]]`},
		{name: "unknown provider", value: "gemini:pro", wantErr: ErrUnknownProvider,
			wantMsg: `"gemini:pro" names unknown provider "gemini"; expected claude, codex, or custom`},
		{name: "legacy auto is not a provider", value: "auto", wantErr: ErrUnknownProvider},
		{name: "empty provider", value: ":opus:high", wantErr: ErrMissingProvider,
			wantMsg: `":opus:high" is missing a provider prefix; expected claude, codex, or custom before the first ':'`},
		{name: "legacy effort-only form", value: ":medium", wantErr: ErrMissingProvider},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseProviderSpec(tc.value)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				if tc.wantMsg != "" {
					assert.Contains(t, err.Error(), tc.wantMsg)
				}
				assert.Equal(t, ProviderSpec{}, got)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestParseProviderSpec_MissingProviderSuggestsRewrite(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		wantMsg string
	}{
		{name: "codex model with effort", value: "gpt-6-astra:medium",
			wantMsg: `"gpt-6-astra:medium" is missing a provider prefix; write "codex:gpt-6-astra:medium"`},
		{name: "claude alias with effort", value: "opus:high",
			wantMsg: `"opus:high" is missing a provider prefix; write "claude:opus:high"`},
		{name: "full claude model name", value: "claude-sonnet-4-5",
			wantMsg: `"claude-sonnet-4-5" is missing a provider prefix; write "claude:claude-sonnet-4-5"`},
		{name: "o-series model", value: "o3",
			wantMsg: `"o3" is missing a provider prefix; write "codex:o3"`},
		{name: "spaces are normalized in the rewrite", value: " opus : high ",
			wantMsg: `"opus : high" is missing a provider prefix; write "claude:opus:high"`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseProviderSpec(tc.value)
			require.ErrorIs(t, err, ErrMissingProvider)
			require.EqualError(t, err, tc.wantMsg)
			assert.Equal(t, ProviderSpec{}, got)
		})
	}

	t.Run("three bare segments get no rewrite", func(t *testing.T) {
		// prefixing a provider would produce a four-segment spec, which is not a valid rewrite
		_, err := ParseProviderSpec("opus:high:extra")
		require.ErrorIs(t, err, ErrUnknownProvider)
		assert.NotContains(t, err.Error(), "write")
	})

	t.Run("unknown bare model gets no rewrite", func(t *testing.T) {
		_, err := ParseProviderSpec("llama3:high")
		require.ErrorIs(t, err, ErrUnknownProvider)
		assert.NotContains(t, err.Error(), "write")
	})
}

func TestProviderSpec_ModelSpec(t *testing.T) {
	tests := []struct {
		name string
		spec ProviderSpec
		want string
	}{
		{name: "provider only", spec: ProviderSpec{Provider: ExternalReviewToolCodex}, want: ""},
		{name: "model only", spec: ProviderSpec{Provider: ExternalReviewToolCodex, Model: "gpt-6-astra"}, want: "gpt-6-astra"},
		{name: "model and effort", spec: ProviderSpec{Provider: ExternalReviewToolCodex, Model: "gpt-6-astra", Effort: "high"},
			want: "gpt-6-astra:high"},
		{name: "effort keeps its leading colon", spec: ProviderSpec{Provider: ExternalReviewToolCodex, Effort: "medium"}, want: ":medium"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.spec.ModelSpec())
		})
	}
}
