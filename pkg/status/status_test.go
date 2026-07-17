package status

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestExternalReviewStatusValues(t *testing.T) {
	assert.Equal(t, "<<<RALPHEX:EXTERNAL_REVIEW_DONE>>>", ExternalReviewDone)
	assert.Equal(t, PhaseExternalReview, Phase("external-review"))
	assert.Equal(t, PhaseExternalEval, Phase("external-eval"))
	assert.Equal(t, PhaseExternalEval, PhaseExternalEvaluation)

	assert.Equal(t, "<<<RALPHEX:CODEX_REVIEW_DONE>>>", CodexDone)
	assert.Equal(t, PhaseCodex, Phase("codex"))
	assert.Equal(t, PhaseClaudeEval, Phase("claude-eval"))
}
