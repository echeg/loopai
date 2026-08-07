package processor

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/umputun/ralphex/pkg/config"
	"github.com/umputun/ralphex/pkg/executor"
)

type executorFactory struct{}

func (f *executorFactory) Build(cfg Config, log Logger) (Config, Executors) {
	externals := f.buildExternalReviewers(&cfg, log)

	if cfg.isCodexExecutor() {
		if cfg.AppConfig.PassClaudeMd {
			maybeEmitClaudeMdSetupHint(log)
		}
		codexTask, codexReview := cfg.buildCodexExecutors(log)
		return cfg, Executors{Task: codexTask, Review: codexReview, Externals: externals}
	}

	claudeExec, reviewExec := cfg.buildClaudeExecutors(log)
	return cfg, Executors{Task: claudeExec, Review: reviewExec, Externals: externals}
}

// buildExternalReviewers builds one executor for every ordered reviewer. When
// callers have not supplied the new list, it synthesizes the legacy one-entry
// chain, including executor-aware auto selection and its missing-binary downgrade.
func (f *executorFactory) buildExternalReviewers(cfg *Config, log Logger) []ExternalReviewer {
	specs := cfg.ExternalReviewers
	if len(specs) == 0 {
		provider, autoSelected := cfg.externalReviewProvider()
		if autoSelected && f.externalBinaryMissing(cfg.AppConfig, provider, log) {
			provider = config.ExternalReviewToolNone
			cfg.CodexEnabled = false
			cfg.ExternalReviewTool = config.ExternalReviewToolNone
			if cfg.AppConfig != nil {
				cfg.AppConfig.ExternalReviewTool = config.ExternalReviewToolNone
			}
		}
		cfg.ExternalReviewTool = provider
		if provider == config.ExternalReviewToolNone {
			return nil
		}
		model, effort := cfg.externalReviewModelEffort(provider)
		specs = []config.ReviewerSpec{{Provider: provider, ModelSpec: joinModelEffort(model, effort)}}
	}

	reviewers := make([]ExternalReviewer, 0, len(specs))
	for _, spec := range specs {
		reviewerExec, displayName := cfg.buildExternalReviewerExecutor(log, spec)
		reviewers = append(reviewers, ExternalReviewer{
			Tool: spec.Provider, DisplayName: displayName, Exec: reviewerExec,
		})
	}
	return reviewers
}

func joinModelEffort(model, effort string) string {
	if effort == "" {
		return model
	}
	return model + ":" + effort
}

// externalReviewProvider returns the concrete external provider expected by the
// factory. The CLI normally resolves auto before processor construction; the
// fallback here keeps direct processor.New callers executor-aware too.
func (cfg Config) externalReviewProvider() (provider string, autoSelected bool) {
	provider = cfg.ExternalReviewTool
	if provider == "" && cfg.AppConfig != nil {
		provider = cfg.AppConfig.ExternalReviewTool
	}
	if provider == "" {
		provider = config.ExternalReviewToolAuto
	}
	autoSelected = provider == config.ExternalReviewToolAuto
	if provider != config.ExternalReviewToolAuto {
		return provider, autoSelected
	}
	if !cfg.CodexEnabled && cfg.Mode != ModeCodexOnly {
		return config.ExternalReviewToolNone, autoSelected
	}
	if cfg.isCodexExecutor() {
		return config.ExternalReviewToolClaude, autoSelected
	}
	return config.ExternalReviewToolCodex, autoSelected
}

func (f *executorFactory) externalBinaryMissing(appConfig *config.Config, provider string, log Logger) bool {
	if appConfig == nil {
		return false
	}
	switch provider {
	case config.ExternalReviewToolClaude:
		claudeCmd := appConfig.ClaudeCommand
		if claudeCmd == "" {
			claudeCmd = "claude"
		}
		if _, err := exec.LookPath(claudeCmd); err != nil {
			log.Print("warning: claude not found (%s: %v), disabling external review phase", claudeCmd, err)
			return true
		}
	case config.ExternalReviewToolCodex:
		codexCmd := appConfig.CodexCommand
		if codexCmd == "" {
			codexCmd = "codex"
		}
		if _, err := exec.LookPath(codexCmd); err != nil {
			log.Print("warning: codex not found (%s: %v), disabling external review phase", codexCmd, err)
			return true
		}
	}
	return false
}

func (cfg Config) buildExternalReviewerExecutor(log Logger, spec config.ReviewerSpec) (Executor, string) {
	var codexModel, codexEffort string
	if cfg.AppConfig != nil {
		codexModel, codexEffort = cfg.AppConfig.CodexModel, cfg.AppConfig.CodexReasoningEffort
	}
	model, effort, _ := ResolveExternalReviewerModelEffort(spec.Provider, spec.ModelSpec, codexModel, codexEffort)
	displayName := spec.Provider
	if modelSpec := joinModelEffort(model, effort); modelSpec != "" {
		displayName += " (" + modelSpec + ")"
	}
	switch spec.Provider {
	case config.ExternalReviewToolClaude:
		return cfg.buildExternalClaudeExecutor(log, model, effort), displayName
	case config.ExternalReviewToolCodex:
		return cfg.buildExternalCodexExecutor(log, model, effort), displayName
	case config.ExternalReviewToolCustom:
		custom := cfg.buildCustomExecutor(log)
		if custom == nil {
			return nil, displayName
		}
		return custom, displayName
	default:
		return nil, displayName
	}
}

func (cfg Config) externalReviewModelEffort(provider string) (model, effort string) {
	spec := cfg.ExternalReviewModel
	if spec == "" && cfg.ExternalReviewEffort == "" && cfg.AppConfig != nil {
		spec = cfg.AppConfig.ExternalReviewModel
	}

	var defaultModel, defaultEffort string
	if cfg.AppConfig != nil {
		defaultModel = cfg.AppConfig.CodexModel
		defaultEffort = cfg.AppConfig.CodexReasoningEffort
	}
	model, effort, _ = ResolveExternalReviewerModelEffort(provider, spec, defaultModel, defaultEffort)
	if cfg.ExternalReviewEffort != "" {
		effort = cfg.ExternalReviewEffort
	}
	return model, effort
}

// ResolveExternalReviewerModelEffort applies provider-specific defaults to an
// external reviewer model specification. It is shared by CLI resolution and
// executor construction so both paths produce identical settings.
func ResolveExternalReviewerModelEffort(provider, spec, codexModel, codexEffort string) (model, effort string, maxDropped bool) {
	switch provider {
	case config.ExternalReviewToolClaude:
		model, effort = "opus", "xhigh"
		resolvedModel, resolvedEffort := parseModelEffort(spec)
		if resolvedModel != "" {
			model = resolvedModel
		}
		if resolvedEffort != "" {
			effort = resolvedEffort
		}
	case config.ExternalReviewToolCodex:
		model, effort, maxDropped = ResolveCodexModelEffort(spec, codexModel, codexEffort)
	}
	return model, effort, maxDropped
}

// buildClaudeExecutors constructs the claude executors for task and review phases.
// returns a single executor in the Review slot only when review_model differs from
// the task executor model — otherwise the task executor handles both roles.
func (cfg Config) buildClaudeExecutors(log Logger) (*executor.ClaudeExecutor, Executor) {
	claudeExec := &executor.ClaudeExecutor{
		OutputHandler: func(text string) {
			log.PrintAligned(text)
		},
		CommandTimingHandler: cfg.CommandTimingHandler,
		Debug:                cfg.Debug,
	}
	cfg.applyClaudeAppConfig(claudeExec)

	taskModel, taskEffort := parseModelEffort(cfg.TaskModel)
	claudeExec.Model, claudeExec.Effort = taskModel, taskEffort

	reviewSpec := cfg.ReviewModel
	if reviewSpec == "" {
		reviewSpec = cfg.TaskModel
	}
	reviewModel, reviewEffort := parseModelEffort(reviewSpec)
	if reviewModel == taskModel && reviewEffort == taskEffort {
		return claudeExec, nil
	}

	reviewExec := &executor.ClaudeExecutor{
		OutputHandler:        claudeExec.OutputHandler,
		CommandTimingHandler: cfg.CommandTimingHandler,
		Debug:                cfg.Debug,
		Model:                reviewModel,
		Effort:               reviewEffort,
	}
	cfg.applyClaudeAppConfig(reviewExec)
	return claudeExec, reviewExec
}

// applyClaudeAppConfig copies AppConfig-sourced fields onto a claude executor.
// no-op when AppConfig is nil.
func (cfg Config) applyClaudeAppConfig(e *executor.ClaudeExecutor) {
	if cfg.AppConfig == nil {
		return
	}
	e.Command = cfg.AppConfig.ClaudeCommand
	e.Args = cfg.AppConfig.ClaudeArgs
	e.ArgsSet = cfg.AppConfig.ClaudeArgsSet
	e.ErrorPatterns = cfg.AppConfig.ClaudeErrorPatterns
	e.LimitPatterns = cfg.AppConfig.ClaudeLimitPatterns
	e.RetryPatterns = cfg.AppConfig.ClaudeRetryPatterns
	e.IdleTimeout = cfg.AppConfig.IdleTimeout
	e.PreserveAPIKey = cfg.AppConfig.PreserveAnthropicAPIKey
}

// buildExternalClaudeExecutor preserves the configured Claude command, wrappers,
// authentication, and pattern handling while enabling the review-only policy.
func (cfg Config) buildExternalClaudeExecutor(log Logger, model, effort string) *executor.ClaudeExecutor {
	e := &executor.ClaudeExecutor{
		OutputHandler:        func(text string) { log.PrintAligned(text) },
		CommandTimingHandler: cfg.CommandTimingHandler,
		Debug:                cfg.Debug,
		ExternalReview:       true,
		Model:                model,
		Effort:               effort,
	}
	cfg.applyClaudeAppConfig(e)
	return e
}

// buildExternalCodexExecutor builds the read-only codex external reviewer.
// MultiAgent and PassClaudeMd intentionally stay off. When codex is the primary
// executor, its idle-timeout guarantee also covers this same-provider review call.
func (cfg Config) buildExternalCodexExecutor(log Logger, model, effort string) *executor.CodexExecutor {
	e := cfg.newBaseCodexExecutor(log)
	e.Model, e.ReasoningEffort = model, effort
	e.Sandbox = "read-only"
	e.ForceReadOnly = true
	if cfg.AppConfig != nil {
		if cfg.isCodexExecutor() {
			e.IdleTimeout = cfg.AppConfig.IdleTimeout
		}
	}
	return e
}

// buildCodexExecutor builds the codex executor used for first-class --codex mode.
// MultiAgent is always enabled so any phase (task, review, finalize) can spawn sub-agents,
// and PassClaudeMd is sourced from config. IdleTimeout is wired here (and only here)
// because the user explicitly opted into --codex; the external-review codex used in
// claude mode keeps master semantics with no idle timeout.
func (cfg Config) buildCodexExecutor(log Logger) *executor.CodexExecutor {
	e := cfg.newBaseCodexExecutor(log)
	e.MultiAgent = true
	if cfg.AppConfig != nil {
		e.Sandbox = cfg.AppConfig.CodexExecutorSandbox()
		e.PassClaudeMd = cfg.AppConfig.PassClaudeMd
		e.IdleTimeout = cfg.AppConfig.IdleTimeout
	}
	return e
}

// buildCodexExecutors constructs the codex executors for the task and review phases
// in first-class --codex mode. the review slot is non-nil only when the resolved
// review model/effort differs from task — otherwise the task executor handles review
// and finalize too. --task-model / --review-model (and their config equivalents) are
// resolved against codex_model / codex_reasoning_effort: review_model falls back to
// task_model when unset, and an unset spec inherits the codex config defaults.
func (cfg Config) buildCodexExecutors(log Logger) (*executor.CodexExecutor, Executor) {
	var defModel, defEffort string
	if cfg.AppConfig != nil {
		defModel, defEffort = cfg.AppConfig.CodexModel, cfg.AppConfig.CodexReasoningEffort
	}
	taskModel, taskEffort, _ := ResolveCodexModelEffort(cfg.TaskModel, defModel, defEffort)
	reviewModel, reviewEffort := taskModel, taskEffort
	if cfg.ReviewModel != "" {
		reviewModel, reviewEffort, _ = ResolveCodexModelEffort(cfg.ReviewModel, defModel, defEffort)
	}

	taskExec := cfg.buildCodexExecutor(log)
	taskExec.Model, taskExec.ReasoningEffort = taskModel, taskEffort
	if reviewModel == taskModel && reviewEffort == taskEffort {
		return taskExec, nil
	}
	reviewExec := cfg.buildCodexExecutor(log)
	reviewExec.Model, reviewExec.ReasoningEffort = reviewModel, reviewEffort
	return taskExec, reviewExec
}

// newBaseCodexExecutor returns a CodexExecutor populated with the fields shared
// between the external-review and first-class --codex builders. Callers layer on
// Sandbox, MultiAgent, PassClaudeMd, and IdleTimeout as appropriate for their
// role — see buildCodexExecutor (first-class) and buildExternalCodexExecutor
// (claude mode). IdleTimeout is intentionally NOT set here: applying it to the
// external codex review path silently shortened previously-idle-tolerant
// review sessions for default-claude users, so it is wired only by
// buildCodexExecutor where the user opted into --codex.
func (cfg Config) newBaseCodexExecutor(log Logger) *executor.CodexExecutor {
	e := &executor.CodexExecutor{
		OutputHandler:        func(text string) { log.PrintAligned(text) },
		CommandTimingHandler: cfg.CommandTimingHandler,
		Debug:                cfg.Debug,
	}
	if cfg.AppConfig == nil {
		return e
	}
	e.Command = cfg.AppConfig.CodexCommand
	e.Model = cfg.AppConfig.CodexModel
	e.ReasoningEffort = cfg.AppConfig.CodexReasoningEffort
	e.TimeoutMs = cfg.AppConfig.CodexTimeoutMs
	e.ErrorPatterns = cfg.AppConfig.CodexErrorPatterns
	e.LimitPatterns = cfg.AppConfig.CodexLimitPatterns
	return e
}

// buildCustomExecutor returns the optional custom external review executor.
// returns nil when no custom_review_script is configured.
func (cfg Config) buildCustomExecutor(log Logger) *executor.CustomExecutor {
	if cfg.AppConfig == nil || cfg.AppConfig.CustomReviewScript == "" {
		return nil
	}
	return &executor.CustomExecutor{
		Script: cfg.AppConfig.CustomReviewScript,
		OutputHandler: func(text string) {
			log.PrintAligned(text)
		},
		ErrorPatterns: cfg.AppConfig.CodexErrorPatterns,
		LimitPatterns: cfg.AppConfig.CodexLimitPatterns,
	}
}

// claudeMdHintOnce ensures the user-level CLAUDE.md setup hint emits at most once
// per process, regardless of how many runners or phases are constructed.
var claudeMdHintOnce sync.Once

// maybeEmitClaudeMdSetupHint prints a one-time hint when ~/.claude/CLAUDE.md exists
// but ~/.codex/AGENTS.md does not. loopai never creates the symlink itself; the
// user owns ~/.codex/. probing errors are swallowed so a missing or unreadable
// home directory simply suppresses the hint.
func maybeEmitClaudeMdSetupHint(log Logger) {
	claudeMdHintOnce.Do(func() {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return
		}
		claudeMd := filepath.Join(home, ".claude", "CLAUDE.md")
		codexAgents := filepath.Join(home, ".codex", "AGENTS.md")
		if !fileExists(claudeMd) {
			return
		}
		if fileExists(codexAgents) {
			return
		}
		log.Print("hint: ~/.claude/CLAUDE.md exists but ~/.codex/AGENTS.md does not. " +
			"to get user-level CLAUDE.md content into codex, link it: " +
			"ln -s ~/.claude/CLAUDE.md ~/.codex/AGENTS.md")
	})
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// parseModelEffort splits a "model[:effort]" spec into separate parts.
// empty input returns ("", ""). missing colon returns (s, "").
// a leading colon (":high") returns ("", "high"); a trailing colon ("opus:") returns ("opus", "").
// only the first colon is treated as the separator; anything after is passed through as effort.
func parseModelEffort(s string) (model, effort string) {
	model, effort, _ = strings.Cut(s, ":")
	return model, effort
}

// ResolveCodexModelEffort resolves a "model[:effort]" spec against codex default
// model and effort. an empty spec returns the defaults unchanged. each populated
// half of the spec overrides its default. the claude-only "max" effort is not valid
// for codex: maxDropped reports that the spec requested it (the caller surfaces the
// warning) and the default effort is kept.
func ResolveCodexModelEffort(spec, defModel, defEffort string) (model, effort string, maxDropped bool) {
	model, effort = defModel, defEffort
	if spec == "" {
		return model, effort, false
	}
	m, e := parseModelEffort(spec)
	if m != "" {
		model = m
	}
	if e == "" {
		return model, effort, false
	}
	if strings.EqualFold(e, "max") {
		return model, effort, true
	}
	return model, e, false
}
