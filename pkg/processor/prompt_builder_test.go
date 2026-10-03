package processor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/ralphex/pkg/config"
	"github.com/umputun/ralphex/pkg/status"
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
		ReportPrompt:               "report {{PLAN_FILE}} {{DEFAULT_BRANCH}} {{RUN_FACTS}}",
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
	assert.Contains(t, builder.ExternalReviewPrompt(config.ExternalReviewToolClaude, false, "fixed"), "Claude (evaluator)")
	assert.Equal(t, "codex eval claude findings", builder.ExternalEvaluationPrompt(config.ExternalReviewToolClaude, "claude findings"))
	assert.Equal(t, "make add feature custom/plans", builder.PlanPrompt())
	assert.Equal(t, "finalize implementation of plan at docs/plans/test.md", builder.FinalizePrompt(nil))
	assert.Equal(t, "report docs/plans/test.md main facts {{PLAN_FILE}}", builder.ReportPrompt("facts {{PLAN_FILE}}"))
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
		assert.Empty(t, builder.FinalizePrompt(nil))
		assert.Empty(t, builder.ReportPrompt("facts"))
		assert.Empty(t, builder.GenAgentsPrompt())
	})
}

func TestPromptBuilder_CodexTaskGuidance(t *testing.T) {
	appCfg := &config.Config{TaskPrompt: "do work"}
	cfg := Config{TaskModel: "codex", AppConfig: appCfg}
	builder := newPromptBuilder(promptBuilderOpts{cfg: cfg, log: newMockLogger(), locator: newPlanLocator(cfg)})

	prompt := builder.TaskPrompt()
	assert.True(t, strings.HasPrefix(prompt, codexTaskGuidance))
	assert.Contains(t, prompt, "do work")
}

func TestPromptBuilder_FinalizePrompt(t *testing.T) {
	withCommands := "# Plan\n\n## Validation Commands\n\n- `make test`\n- make lint\n\n## Implementation Steps\n\n### Task 1: Work\n\n- [x] done\n"
	withoutCommands := "# Plan\n\n## Implementation Steps\n\n### Task 1: Work\n\n- [x] done\n"
	template := "sync origin/{{DEFAULT_BRANCH}} for {{PLAN_FILE}}\nconflicts:\n{{FINALIZE_CONFLICTS}}\nvalidate:\n{{VALIDATION_COMMANDS}}"

	tests := []struct {
		name        string
		planContent string // empty means no plan file is configured
		conflicts   []string
		want        []string
	}{
		{name: "conflicts and validation commands", planContent: withCommands,
			conflicts: []string{"pkg/a.go", " ", "docs/b.md"},
			want:      []string{"sync origin/main for ", "conflicts:\n- pkg/a.go\n- docs/b.md\nvalidate:", "validate:\n- make test\n- make lint"},
		},
		{name: "clean merge", planContent: withCommands,
			want: []string{"conflicts:\n(none - the merge is already committed or the branch was up to date)", "- make test"},
		},
		{name: "no validation commands", planContent: withoutCommands, conflicts: []string{"go.mod"},
			want: []string{"- go.mod", "validate:\n(none listed in the plan)"},
		},
		{name: "no plan file", want: []string{"(no plan file - reviewing current branch)", "(none listed in the plan)"}},
		{name: "values are not template-expanded", planContent: withCommands, conflicts: []string{"{{DEFAULT_BRANCH}}.go"},
			want: []string{"- {{DEFAULT_BRANCH}}.go"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{DefaultBranch: "main", AppConfig: &config.Config{FinalizePrompt: template}}
			if tc.planContent != "" {
				cfg.PlanFile = filepath.Join(t.TempDir(), "plan.md")
				require.NoError(t, os.WriteFile(cfg.PlanFile, []byte(tc.planContent), 0o600))
			}
			builder := newPromptBuilder(promptBuilderOpts{cfg: cfg, log: newMockLogger(), locator: newPlanLocator(cfg)})

			prompt := builder.FinalizePrompt(tc.conflicts)

			for _, want := range tc.want {
				assert.Contains(t, prompt, want)
			}
			assert.NotContains(t, prompt, "{{FINALIZE_CONFLICTS}}")
			assert.NotContains(t, prompt, "{{VALIDATION_COMMANDS}}")
		})
	}
}

func TestPromptBuilder_FinalizePrompt_UnreadablePlanWarns(t *testing.T) {
	planDir := filepath.Join(t.TempDir(), "plan.md")
	require.NoError(t, os.Mkdir(planDir, 0o750)) // a directory cannot be parsed as a plan
	cfg := Config{PlanFile: planDir, AppConfig: &config.Config{FinalizePrompt: "{{VALIDATION_COMMANDS}}"}}
	log := newMockLogger()
	builder := newPromptBuilder(promptBuilderOpts{cfg: cfg, log: log, locator: newPlanLocator(cfg)})

	assert.Equal(t, "(none listed in the plan)", builder.FinalizePrompt(nil))
	assertLogContains(t, log, "cannot read validation commands")
}

func TestPromptBuilder_FinalizePrompt_WithoutPlaceholdersSkipsPlanRead(t *testing.T) {
	planDir := filepath.Join(t.TempDir(), "plan.md")
	require.NoError(t, os.Mkdir(planDir, 0o750))
	cfg := Config{PlanFile: planDir, AppConfig: &config.Config{FinalizePrompt: "custom finalize for {{DEFAULT_BRANCH}}, then " + status.FinalizeDone}}
	log := newMockLogger()
	builder := newPromptBuilder(promptBuilderOpts{cfg: cfg, log: log, locator: newPlanLocator(cfg)})

	assert.Equal(t, "custom finalize for master, then "+status.FinalizeDone, builder.FinalizePrompt([]string{"a.go"}))
	assert.Empty(t, log.PrintCalls(), "a customized prompt without the placeholders must not read the plan")
}

func TestPromptBuilder_FinalizePrompt_WarnsOnceWithoutDoneSignal(t *testing.T) {
	// a customized copy of the old rebase finalize prompt never asks for the signal
	cfg := Config{AppConfig: &config.Config{FinalizePrompt: "Rebase onto origin/{{DEFAULT_BRANCH}}"}}
	log := newMockLogger()
	builder := newPromptBuilder(promptBuilderOpts{cfg: cfg, log: log, locator: newPlanLocator(cfg)})

	assert.Equal(t, "Rebase onto origin/master", builder.FinalizePrompt(nil))
	builder.FinalizePrompt(nil)

	require.Len(t, log.PrintCalls(), 1, "the warning is printed once per run")
	assertLogContains(t, log, "finalize prompt never asks for")
	assert.Equal(t, []any{status.FinalizeDone}, log.PrintCalls()[0].Args)
}

func TestPromptBuilder_FinalizePrompt_Embedded(t *testing.T) {
	planFile := filepath.Join(t.TempDir(), "plan.md")
	require.NoError(t, os.WriteFile(planFile,
		[]byte("# Plan\n\n## Validation Commands\n\n- make test\n\n### Task 1: Work\n\n- [x] done\n"), 0o600))
	// a commit diff base must not leak into the fetch and merge the prompt describes
	cfg := Config{PlanFile: planFile, DefaultBranch: "abc123", FinalizeBase: "trunk", AppConfig: testAppConfig(t)}
	builder := newPromptBuilder(promptBuilderOpts{cfg: cfg, log: newMockLogger(), locator: newPlanLocator(cfg)})

	prompt := builder.FinalizePrompt([]string{"pkg/conflicted.go"})

	assert.Contains(t, prompt, "git merge origin/trunk")
	assert.NotContains(t, prompt, "abc123")
	assert.Contains(t, prompt, "- pkg/conflicted.go")
	assert.Contains(t, prompt, "- make test")
	assert.Contains(t, prompt, "<<<RALPHEX:FINALIZE_DONE>>>")
	assert.Contains(t, prompt, "<<<RALPHEX:FINALIZE_BLOCKED>>>")
	assert.Contains(t, prompt, "Never use `git add -A`", "conflict staging must stay pathspec-bound")
	assert.NotContains(t, prompt, "git rebase origin", "the old rebase finalize is gone")
	assert.NotRegexp(t, `\{\{[A-Z_]+\}\}`, prompt, "no raw placeholder may reach the executor")
}
