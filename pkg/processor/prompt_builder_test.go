package processor

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/umputun/ralphex/pkg/config"
)

func TestPromptBuilder_FinalPrompts(t *testing.T) {
	appCfg := &config.Config{
		TaskPrompt:                 "task {{PLAN_FILE}} {{PROGRESS_FILE}}",
		ReviewFirstPrompt:          "first {{GOAL}}",
		ReviewSecondPrompt:         "second {{DEFAULT_BRANCH}}",
		CodexReviewPrompt:          "codex {{DIFF_INSTRUCTION}} {{PREVIOUS_REVIEW_CONTEXT}}",
		CodexPrompt:                "eval {{CODEX_OUTPUT}} {{GOAL}}",
		CustomReviewPrompt:         "custom {{DIFF_INSTRUCTION}} {{PREVIOUS_REVIEW_CONTEXT}}",
		CustomEvalPrompt:           "custom eval {{CUSTOM_OUTPUT}}",
		ExternalClaudeReviewPrompt: "claude {{DIFF_INSTRUCTION}} {{PREVIOUS_REVIEW_CONTEXT}}",
		ExternalClaudeEvalPrompt:   "codex eval {{CLAUDE_OUTPUT}}",
		MakePlanPrompt:             "make {{PLAN_DESCRIPTION}} {{PLANS_DIR}}",
		FinalizePrompt:             "finalize {{GOAL}}",
		PlansDir:                   "custom/plans",
	}
	cfg := Config{
		PlanFile: "docs/plans/test.md", ProgressPath: "progress.txt", PlanDescription: "add feature",
		DefaultBranch: "main", AppConfig: appCfg,
	}
	builder := newPromptBuilder(promptBuilderOpts{cfg: cfg, log: newMockLogger(), locator: newPlanLocator(cfg)})

	assert.Equal(t, "task docs/plans/test.md progress.txt", builder.TaskPrompt())
	assert.Equal(t, "first implementation of plan at docs/plans/test.md", builder.FirstReviewPrompt())
	assert.Equal(t, "prefix: second main", builder.SecondReviewPrompt("prefix: "))
	assert.Contains(t, builder.ExternalReviewPrompt(config.ExternalReviewToolCodex, true, ""), "git diff main...HEAD")
	assert.Contains(t, builder.ExternalReviewPrompt(config.ExternalReviewToolCustom, false, "fixed"), "PREVIOUS REVIEW CONTEXT")
	assert.Equal(t, "eval findings implementation of plan at docs/plans/test.md", builder.ExternalEvaluationPrompt(config.ExternalReviewToolCodex, "findings"))
	assert.Equal(t, "custom eval custom findings", builder.ExternalEvaluationPrompt(config.ExternalReviewToolCustom, "custom findings"))
	assert.Contains(t, builder.ExternalReviewPrompt(config.ExternalReviewToolClaude, false, "fixed"), "Claude (primary evaluator)")
	assert.Equal(t, "codex eval claude findings", builder.ExternalEvaluationPrompt(config.ExternalReviewToolClaude, "claude findings"))
	assert.Equal(t, "make add feature custom/plans", builder.PlanPrompt())
	assert.Equal(t, "finalize implementation of plan at docs/plans/test.md", builder.FinalizePrompt())
}

func TestPromptBuilder_GenAgentsPrompt(t *testing.T) {
	appCfg := &config.Config{
		GenAgentsPrompt: "generate agents, log to {{PROGRESS_FILE}} for {{DEFAULT_BRANCH}} {{agent:quality}}",
		CustomAgents:    []config.CustomAgent{{Name: "quality", Prompt: "check quality"}},
		CommitTrailer:   "Co-Authored-By: bot",
	}
	cfg := Config{ProgressPath: "progress.txt", DefaultBranch: "main", AppConfig: appCfg}
	builder := newPromptBuilder(promptBuilderOpts{cfg: cfg, log: newMockLogger(), locator: newPlanLocator(cfg)})

	prompt := builder.GenAgentsPrompt()

	assert.Equal(t, "generate agents, log to progress.txt for main {{agent:quality}}", prompt,
		"generation is not a review: agent references and the commit trailer stay out")
}

func TestPromptBuilder_GenAgentsPrompt_NoProgressFile(t *testing.T) {
	cfg := Config{AppConfig: &config.Config{GenAgentsPrompt: "log: {{PROGRESS_FILE}}"}}
	builder := newPromptBuilder(promptBuilderOpts{cfg: cfg, log: newMockLogger(), locator: newPlanLocator(cfg)})

	assert.Equal(t, "log: (no progress file available)", builder.GenAgentsPrompt())
}

func TestPromptBuilder_NilConfigDependencies(t *testing.T) {
	builder := newPromptBuilder(promptBuilderOpts{cfg: Config{}, log: newMockLogger()})

	assert.NotPanics(t, func() {
		assert.Empty(t, builder.TaskPrompt())
		assert.Empty(t, builder.FirstReviewPrompt())
		assert.Empty(t, builder.ExternalEvaluationPrompt(config.ExternalReviewToolCodex, "findings"))
		assert.Empty(t, builder.FinalizePrompt())
		assert.Empty(t, builder.GenAgentsPrompt())
	})
}

func TestPromptBuilder_CodexTaskGuidance(t *testing.T) {
	appCfg := &config.Config{TaskPrompt: "do work", Executor: config.ExecutorCodex}
	cfg := Config{AppConfig: appCfg}
	builder := newPromptBuilder(promptBuilderOpts{cfg: cfg, log: newMockLogger(), locator: newPlanLocator(cfg)})

	prompt := builder.TaskPrompt()
	assert.True(t, strings.HasPrefix(prompt, codexTaskGuidance))
	assert.Contains(t, prompt, "do work")
}
