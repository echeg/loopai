package processor

import (
	"strings"

	"github.com/umputun/ralphex/pkg/config"
	"github.com/umputun/ralphex/pkg/plan"
	"github.com/umputun/ralphex/pkg/status"
)

type promptBuilder struct {
	cfg                    Config
	log                    Logger
	locator                *planLocator
	codexFrontmatterWarned map[string]bool
	catalogMissingWarned   bool
	finalizeSignalWarned   bool
	// reviewScopeMissingWarned is set once a per-task block rendered a prompt without {{REVIEW_SCOPE}}
	reviewScopeMissingWarned bool

	// diffBase and reviewScope narrow the external review prompts to one task while a
	// per-task review block runs. the builder is shared by every phase, so the runner
	// sets them for the block and clears them afterwards.
	diffBase    string
	reviewScope string
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
	return b.prependCodexTaskGuidance(b.replacePromptVariables(b.cfg.AppConfig.TaskPrompt, b.cfg.taskProvider()))
}

func (b *promptBuilder) FirstReviewPrompt() string {
	b.warnMissingDynamicCatalog(b.cfg.AppConfig.ReviewFirstPrompt)
	return b.prependCodexReviewGuidance(b.replacePromptVariables(b.cfg.AppConfig.ReviewFirstPrompt, b.cfg.reviewProvider()))
}

func (b *promptBuilder) SecondReviewPrompt(prefix string) string {
	return prefix + b.prependCodexReviewGuidance(b.replacePromptVariables(b.cfg.AppConfig.ReviewSecondPrompt, b.cfg.reviewProvider()))
}

// SetReviewScope narrows the external prompts to one task: diffBase replaces the default
// branch in {{DEFAULT_BRANCH}} and {{DIFF_INSTRUCTION}}, and scope is rendered for
// {{REVIEW_SCOPE}}. {{FINALIZE_BASE}} keeps naming the configured branch.
func (b *promptBuilder) SetReviewScope(diffBase, scope string) {
	b.diffBase = diffBase
	b.reviewScope = scope
}

// ClearReviewScope restores whole-branch rendering after a per-task review block.
func (b *promptBuilder) ClearReviewScope() {
	b.diffBase = ""
	b.reviewScope = ""
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

// evaluatorName names the provider that evaluates external findings: the review block's.
func (b *promptBuilder) evaluatorName() string {
	if b.cfg.reviewProvider() == config.ExecutorCodex {
		return config.ExternalReviewToolCodex
	}
	return config.ExternalReviewToolClaude
}

// ExternalEvaluationPrompt renders the review provider's evaluation prompt for
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
	prompt = b.replaceReviewScope(b.replacePromptVariables(prompt, b.cfg.reviewProvider()))
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

// FinalizePrompt renders the base-sync prompt. conflicts lists the paths the merge left
// unmerged; it is empty for a clean merge or an up-to-date branch. Conflict paths and
// validation commands are inserted after template expansion so their contents are never
// interpreted as template variables.
func (b *promptBuilder) FinalizePrompt(conflicts []string) string {
	b.warnMissingFinalizeSignal(b.cfg.AppConfig.FinalizePrompt)
	prompt := b.replacePromptVariables(b.cfg.AppConfig.FinalizePrompt, b.cfg.reviewProvider())
	if !strings.Contains(prompt, "{{FINALIZE_CONFLICTS}}") && !strings.Contains(prompt, "{{VALIDATION_COMMANDS}}") {
		return prompt
	}
	prompt = strings.ReplaceAll(prompt, "{{FINALIZE_CONFLICTS}}",
		formatPromptList(conflicts, "(none - the merge is already committed or the branch was up to date)"))
	return strings.ReplaceAll(prompt, "{{VALIDATION_COMMANDS}}",
		formatPromptList(b.validationCommands(), "(none listed in the plan)"))
}

// warnMissingFinalizeSignal warns once when the effective finalize prompt never asks for
// FINALIZE_DONE. a customized copy of the old rebase prompt survives the switch from
// finalize_enabled to the finalize modes, and without the signal every base sync is rejected.
func (b *promptBuilder) warnMissingFinalizeSignal(prompt string) {
	if b.finalizeSignalWarned || b.log == nil || strings.Contains(prompt, status.FinalizeDone) {
		return
	}
	b.finalizeSignalWarned = true
	b.log.Print("[WARN] finalize prompt never asks for %s, so every base sync will be reported as blocked: "+
		"update your customized finalize.txt from the embedded default (loopai --dump-defaults)", status.FinalizeDone)
}

// validationCommands returns the plan's ## Validation Commands entries. a missing
// or unparsable plan yields none, which the finalize prompt reports as such.
func (b *promptBuilder) validationCommands() []string {
	path := b.locator.Path()
	if path == "" {
		return nil
	}
	parsed, err := plan.ParsePlanFile(path)
	if err != nil {
		if b.log != nil {
			b.log.Print("[WARN] finalize: cannot read validation commands from %s: %v", path, err)
		}
		return nil
	}
	return parsed.ValidationCommands
}

// formatPromptList renders items as "- item" lines, or empty when there are none.
func formatPromptList(items []string, empty string) string {
	var lines []string
	for _, item := range items {
		if item = strings.TrimSpace(item); item != "" {
			lines = append(lines, "- "+item)
		}
	}
	if len(lines) == 0 {
		return empty
	}
	return strings.Join(lines, "\n")
}

// ReportPrompt renders the completion report prompt with deterministic facts.
// Facts are inserted after base-variable expansion so their contents are never
// interpreted as template variables.
func (b *promptBuilder) ReportPrompt(facts string) string {
	prompt := b.replaceBaseVariables(b.cfg.AppConfig.ReportPrompt)
	return strings.ReplaceAll(prompt, "{{RUN_FACTS}}", facts)
}
