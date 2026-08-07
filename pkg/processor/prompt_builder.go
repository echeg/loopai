package processor

import (
	"strings"

	"github.com/umputun/ralphex/pkg/config"
)

type promptBuilder struct {
	cfg                    Config
	log                    Logger
	locator                *planLocator
	codexFrontmatterWarned map[string]bool
	catalogMissingWarned   bool
}

type promptBuilderOpts struct {
	cfg     Config
	log     Logger
	locator *planLocator
}

func newPromptBuilder(opts promptBuilderOpts) *promptBuilder {
	cfg := opts.cfg
	if cfg.AppConfig == nil {
		cfg.AppConfig = &config.Config{}
	}
	locator := opts.locator
	if locator == nil {
		locator = newPlanLocator(cfg)
	}
	return &promptBuilder{cfg: cfg, log: opts.log, locator: locator}
}

func (b *promptBuilder) TaskPrompt() string {
	return b.prependCodexTaskGuidance(b.replacePromptVariables(b.cfg.AppConfig.TaskPrompt))
}

func (b *promptBuilder) FirstReviewPrompt() string {
	b.warnMissingDynamicCatalog(b.cfg.AppConfig.ReviewFirstPrompt)
	return b.prependCodexReviewGuidance(b.replacePromptVariables(b.cfg.AppConfig.ReviewFirstPrompt))
}

func (b *promptBuilder) SecondReviewPrompt(prefix string) string {
	return prefix + b.prependCodexReviewGuidance(b.replacePromptVariables(b.cfg.AppConfig.ReviewSecondPrompt))
}

// ExternalReviewPrompt renders the prompt for the selected external reviewer.
func (b *promptBuilder) ExternalReviewPrompt(reviewer string, isFirst bool, evaluatorResponse string) string {
	var prompt string
	switch reviewer {
	case config.ExternalReviewToolClaude:
		prompt = b.cfg.AppConfig.ExternalClaudeReviewPrompt
	case config.ExternalReviewToolCustom:
		prompt = b.cfg.AppConfig.CustomReviewPrompt
	default:
		prompt = b.cfg.AppConfig.CodexReviewPrompt
	}
	return b.replaceExternalVariablesWithIteration(prompt, isFirst, reviewer, b.evaluatorName(), evaluatorResponse)
}

func (b *promptBuilder) evaluatorName() string {
	if b.cfg.isCodexExecutor() {
		return config.ExternalReviewToolCodex
	}
	return config.ExternalReviewToolClaude
}

// ExternalEvaluationPrompt renders the primary executor's evaluation prompt for
// findings from the selected reviewer.
func (b *promptBuilder) ExternalEvaluationPrompt(reviewer, findings string) string {
	var prompt, outputVariable string
	switch reviewer {
	case config.ExternalReviewToolClaude:
		prompt, outputVariable = b.cfg.AppConfig.ExternalClaudeEvalPrompt, "{{CLAUDE_OUTPUT}}"
	case config.ExternalReviewToolCustom:
		prompt, outputVariable = b.cfg.AppConfig.CustomEvalPrompt, "{{CUSTOM_OUTPUT}}"
	default:
		prompt, outputVariable = b.cfg.AppConfig.CodexPrompt, "{{CODEX_OUTPUT}}"
	}
	prompt = b.replacePromptVariables(prompt)
	return strings.ReplaceAll(prompt, outputVariable, findings)
}

func (b *promptBuilder) PlanPrompt() string {
	prompt := b.cfg.AppConfig.MakePlanPrompt
	prompt = strings.ReplaceAll(prompt, "{{PLAN_DESCRIPTION}}", b.cfg.PlanDescription)
	result := b.replaceBaseVariables(prompt)
	return b.appendCommitTrailerInstruction(result)
}

// GenAgentsPrompt renders the prompt for the --gen-agents standalone mode. only base
// variables are expanded: the session writes agent files rather than launching review
// agents, so {{agent:name}} and {{agents:dynamic}} have no meaning here.
func (b *promptBuilder) GenAgentsPrompt() string {
	return b.replaceBaseVariables(b.cfg.AppConfig.GenAgentsPrompt)
}

func (b *promptBuilder) FinalizePrompt() string {
	return b.replacePromptVariables(b.cfg.AppConfig.FinalizePrompt)
}
