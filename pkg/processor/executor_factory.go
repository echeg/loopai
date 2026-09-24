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

// Build constructs the task executor from the task spec's provider and the review
// executor from the review spec's provider, so a codex task phase can hand review,
// external-findings evaluation, finalize, and report to claude and vice versa.
func (f *executorFactory) Build(cfg Config, log Logger) (Config, Executors) {
	externals := f.buildExternalReviewers(&cfg, log)

	if cfg.runsCodexPhase() && cfg.AppConfig != nil && cfg.AppConfig.PassClaudeMd {
		maybeEmitClaudeMdSetupHint(log)
	}
	task, review := cfg.buildPhaseExecutors(log)
	return cfg, Executors{Task: task, Review: review, Externals: externals}
}

// taskSpec returns the task phase's provider[:model[:effort]] spec. An unset spec
// means claude with its CLI defaults. Startup validation rejects an invalid spec,
// or a custom one, before any executor is built, so a direct caller's invalid value
// gets the same claude default instead of reaching a provider CLI.
func (c Config) taskSpec() config.ProviderSpec {
	return parsePhaseSpec(c.TaskModel, config.ProviderSpec{Provider: config.ExternalReviewToolClaude})
}

// reviewSpec returns the spec governing internal review, external-findings evaluation,
// finalize, and report. An unset review spec inherits the task spec whole, provider included.
func (c Config) reviewSpec() config.ProviderSpec {
	return parsePhaseSpec(c.ReviewModel, c.taskSpec())
}

// taskProvider returns the provider that runs the task phase.
func (c Config) taskProvider() string {
	return c.taskSpec().Provider
}

// reviewProvider returns the provider that runs internal review, external-findings
// evaluation, finalize, and report.
func (c Config) reviewProvider() string {
	return c.reviewSpec().Provider
}

// runsCodexPhase reports whether codex runs the task phase or the review block.
func (c Config) runsCodexPhase() bool {
	return c.taskSpec().Provider == config.ExternalReviewToolCodex ||
		c.reviewSpec().Provider == config.ExternalReviewToolCodex
}

func parsePhaseSpec(value string, fallback config.ProviderSpec) config.ProviderSpec {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	spec, err := config.ParseProviderSpec(value)
	if err != nil || spec.Provider == config.ExternalReviewToolCustom {
		return fallback
	}
	return spec
}

// buildPhaseExecutors builds the task executor and, only when the review phase resolves
// to a different provider, model, or effort, a separate review executor; otherwise the
// Review slot stays nil and the task executor handles review too. Both come from the
// phase builders, never the external-review ones, because the review block fixes
// what it finds and must be able to write.
func (cfg Config) buildPhaseExecutors(log Logger) (task, review Executor) {
	taskSpec, reviewSpec := cfg.taskSpec(), cfg.reviewSpec()
	task = cfg.buildPhaseExecutor(log, taskSpec)
	taskModel, taskEffort, _ := ResolveModelEffort(taskSpec)
	reviewModel, reviewEffort, _ := ResolveModelEffort(reviewSpec)
	if reviewSpec.Provider == taskSpec.Provider && reviewModel == taskModel && reviewEffort == taskEffort {
		return task, nil
	}
	return task, cfg.buildPhaseExecutor(log, reviewSpec)
}

// buildPhaseExecutor builds one write-capable phase executor for spec's provider.
func (cfg Config) buildPhaseExecutor(log Logger, spec config.ProviderSpec) Executor {
	model, effort, _ := ResolveModelEffort(spec)
	if spec.Provider == config.ExternalReviewToolCodex {
		e := cfg.buildCodexExecutor(log)
		e.Model, e.ReasoningEffort = model, effort
		return e
	}
	e := &executor.ClaudeExecutor{
		OutputHandler:        func(text string) { log.PrintAligned(text) },
		CommandTimingHandler: cfg.CommandTimingHandler,
		Debug:                cfg.Debug,
		Model:                model,
		Effort:               effort,
	}
	cfg.applyClaudeAppConfig(e)
	return e
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
			Tool: spec.Provider, ModelSpec: spec.ModelSpec, DisplayName: displayName, Exec: reviewerExec,
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
	// the review-only modes run no task phase, so the provider evaluating the findings is
	// the one to differ from, as the CLI's reviewerBaseProvider does
	base := cfg.taskProvider()
	if cfg.Mode == ModeReview || cfg.Mode == ModeCodexOnly {
		base = cfg.reviewProvider()
	}
	if base == config.ExternalReviewToolCodex {
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
	model, effort, _ := ResolveExternalReviewerModelEffort(spec.Provider, spec.ModelSpec)
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
	model, effort, _ = ResolveExternalReviewerModelEffort(provider, cfg.ExternalReviewModel)
	if cfg.ExternalReviewEffort != "" {
		effort = cfg.ExternalReviewEffort
	}
	return model, effort
}

// ResolveExternalReviewerModelEffort applies provider-specific defaults to an
// external reviewer model specification. It is shared by CLI resolution and
// executor construction so both paths produce identical settings. codex has no
// loopai-side default: an empty model or effort leaves the codex CLI's own choice.
// A reviewer's model[:effort] remainder is re-joined with its provider so the shared
// ParseProviderSpec grammar splits it; ParseExternalReviewers has already validated it.
func ResolveExternalReviewerModelEffort(provider, spec string) (model, effort string, maxDropped bool) {
	parsed, err := config.ParseProviderSpec(provider + ":" + spec)
	if err != nil {
		return "", "", false
	}
	switch parsed.Provider {
	case config.ExternalReviewToolClaude:
		model, effort = "opus", "xhigh"
		if parsed.Model != "" {
			model = parsed.Model
		}
		if parsed.Effort != "" {
			effort = parsed.Effort
		}
	case config.ExternalReviewToolCodex:
		model, effort, maxDropped = ResolveModelEffort(parsed)
	}
	return model, effort, maxDropped
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
// MultiAgent and PassClaudeMd intentionally stay off. When codex runs the task phase
// or the review block, its idle-timeout guarantee also covers this same-provider call.
func (cfg Config) buildExternalCodexExecutor(log Logger, model, effort string) *executor.CodexExecutor {
	e := cfg.newBaseCodexExecutor(log)
	e.Model, e.ReasoningEffort = model, effort
	e.Sandbox = "read-only"
	e.ForceReadOnly = true
	if cfg.AppConfig != nil && cfg.runsCodexPhase() {
		e.IdleTimeout = cfg.AppConfig.IdleTimeout
	}
	return e
}

// buildCodexExecutor builds a write-capable codex executor for a phase whose spec names
// codex. MultiAgent is always enabled so any phase (task, review, finalize) can spawn
// sub-agents, and PassClaudeMd is sourced from config. IdleTimeout is wired here (and only
// here) because the user explicitly chose codex for the phase; the external-review codex
// under claude phases keeps master semantics with no idle timeout.
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

// newBaseCodexExecutor returns a CodexExecutor populated with the fields shared
// between the external-review and codex phase builders. Callers layer on
// Sandbox, MultiAgent, PassClaudeMd, and IdleTimeout as appropriate for their
// role — see buildCodexExecutor (phase) and buildExternalCodexExecutor
// (reviewer). IdleTimeout is intentionally NOT set here: applying it to the
// external codex review path silently shortened previously-idle-tolerant
// review sessions when every phase runs on claude, so it is wired by
// buildCodexExecutor, where a phase spec names codex, and by
// buildExternalCodexExecutor only when some phase does.
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
	// set here so both codex paths carry the extras: codex phase executors and the
	// external codex reviewer
	e.ExtraArgs = cfg.AppConfig.CodexArgs
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

// ResolveModelEffort returns the model and effort a phase executor receives for spec.
// An empty half leaves the provider CLI's own default; for codex that is
// ~/.codex/config.toml. The claude-only "max" effort is not valid for codex: maxDropped
// reports that the spec requested it (the caller surfaces the warning) and the effort is
// left to codex.
func ResolveModelEffort(spec config.ProviderSpec) (model, effort string, maxDropped bool) {
	if spec.Provider == config.ExternalReviewToolCodex && strings.EqualFold(spec.Effort, "max") {
		return spec.Model, "", true
	}
	return spec.Model, spec.Effort, false
}
