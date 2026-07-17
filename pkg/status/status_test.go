package status

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestExternalReviewStatusValues(t *testing.T) {
	assert.Equal(t, "<<<RALPHEX:EXTERNAL_REVIEW_DONE>>>", ExternalReviewDone)
	assert.Equal(t, Phase("external-review"), PhaseExternalReview)
	assert.Equal(t, Phase("external-eval"), PhaseExternalEval)
	assert.Equal(t, PhaseExternalEval, PhaseExternalEvaluation)

	assert.Equal(t, "<<<RALPHEX:CODEX_REVIEW_DONE>>>", CodexDone)
	assert.Equal(t, Phase("codex"), PhaseCodex)
	assert.Equal(t, Phase("claude-eval"), PhaseClaudeEval)
}
