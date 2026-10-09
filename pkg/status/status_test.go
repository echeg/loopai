package status

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestExternalReviewStatusValues(t *testing.T) {
	assert.Equal(t, "<<<RALPHEX:EXTERNAL_REVIEW_DONE>>>", ExternalReviewDone)
	assert.Equal(t, PhaseExternalReview, Phase("external-review"))
	assert.Equal(t, PhaseExternalEval, Phase("external-eval"))
	assert.Equal(t, "<<<RALPHEX:CODEX_REVIEW_DONE>>>", CodexDone)
	assert.Equal(t, PhaseCodex, Phase("codex"))
	assert.Equal(t, PhaseClaudeEval, Phase("claude-eval"))
}

func TestFinalizeStatusValues(t *testing.T) {
	assert.Equal(t, "<<<RALPHEX:FINALIZE_DONE>>>", FinalizeDone)
	assert.Equal(t, "<<<RALPHEX:FINALIZE_BLOCKED>>>", FinalizeBlocked)
}

func TestACPStatusValues(t *testing.T) {
	assert.Equal(t, "<<<RALPHEX:ACP_MERGE>>>", ACPMerge)
	assert.Equal(t, "<<<RALPHEX:ACP_PR>>>", ACPPR)
}

func TestStripSignals(t *testing.T) {
	tests := []struct {
		name string
		text string
		want string
	}{
		{name: "no signal unchanged", text: "  indented answer\n\n", want: "  indented answer\n\n"},
		{name: "trailing signal line", text: "Merging now.\n\n" + ACPMerge + "\n", want: "Merging now."},
		{name: "signal between paragraphs", text: "first\n\n" + ACPPR + "\n\nsecond", want: "first\n\n\nsecond"},
		{name: "text sharing a line kept", text: "done " + ACPMerge + "  ", want: "done"},
		{name: "leading signal line", text: ACPPR + "\nopening a PR", want: "opening a PR"},
		{name: "indentation of other lines kept", text: "list:\n  - item\n" + ACPMerge, want: "list:\n  - item"},
		{name: "every known signal", text: Completed + Failed + ReviewDone + ExternalReviewDone + CodexDone +
			Question + PlanReady + PlanDraft + FinalizeDone + FinalizeBlocked + ACPMerge + ACPPR + "\nanswer", want: "answer"},
		{name: "unknown marker kept", text: "<<<RALPHEX:SOMETHING_ELSE>>>\nanswer", want: "<<<RALPHEX:SOMETHING_ELSE>>>\nanswer"},
		{name: "only a signal", text: ACPMerge, want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, StripSignals(tc.text))
		})
	}
}
