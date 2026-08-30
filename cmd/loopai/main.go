// Package main provides loopai - autonomous plan execution with Claude Code.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/jessevdk/go-flags"

	"github.com/umputun/ralphex/pkg/claudeswap"
	"github.com/umputun/ralphex/pkg/cmux"
	"github.com/umputun/ralphex/pkg/config"
	"github.com/umputun/ralphex/pkg/git"
	"github.com/umputun/ralphex/pkg/input"
	"github.com/umputun/ralphex/pkg/limits"
	"github.com/umputun/ralphex/pkg/notify"
	"github.com/umputun/ralphex/pkg/orca"
	"github.com/umputun/ralphex/pkg/plan"
	"github.com/umputun/ralphex/pkg/processor"
	"github.com/umputun/ralphex/pkg/progress"
	"github.com/umputun/ralphex/pkg/status"
	"github.com/umputun/ralphex/pkg/web"
)

// opts holds all command-line options.
type opts struct {
	MaxIterations           int           `short:"m" long:"max-iterations" description:"maximum task iterations (default: 50)"`
	MaxExternalIterations   int           `long:"max-external-iterations" default:"0" description:"override external review iteration limit (0 = auto)"`
	ReviewPatience          int           `long:"review-patience" default:"0" description:"terminate external review after N unchanged rounds (0 = disabled)"`
	PlanModel               string        `long:"plan-model" description:"model for plan creation as model[:effort] (falls back to --task-model)"`
	TaskModel               string        `long:"task-model" description:"model for task execution as model[:effort] (e.g., opus, opus:high, :medium)"`
	ReviewModel             string        `long:"review-model" description:"model for review phases as model[:effort] (falls back to --task-model)"`
	ClaudeCommand           string        `long:"claude-command" description:"override claude-compatible command for this run"`
	ClaudeArgs              string        `long:"claude-args" description:"override claude-compatible command args for this run"`
	CodexArgs               string        `long:"codex-args" description:"extra arguments appended to every codex invocation (additive; explicit -c values override loopai's)"`
	ExternalReviewTool      string        `long:"external-review-tool" choice:"auto" choice:"claude" choice:"codex" choice:"custom" choice:"none" description:"override external review tool for this run"`
	ExternalReviewModel     string        `long:"external-review-model" description:"external review model as model[:effort]"`
	ExternalReviewers       string        `long:"external-reviewers" description:"ordered external reviewers as provider[:model[:effort]],..."`
	CustomReviewScript      string        `long:"custom-review-script" description:"override custom external review script for this run"`
	Review                  bool          `short:"r" long:"review" description:"skip task execution, run full review pipeline"`
	ExternalOnly            bool          `short:"e" long:"external-only" description:"skip tasks and first review; run external review, conditional post-review, and finalize"`
	CodexOnly               bool          `long:"codex-only" description:"alias for --external-only (deprecated)"`
	TasksOnly               bool          `short:"t" long:"tasks-only" description:"run only task phase, skip all reviews"`
	BaseRef                 string        `short:"b" long:"base-ref" description:"override the base for review diffs; a branch name also becomes the base for non-worktree branch creation (branch name or commit hash)"`
	Wait                    time.Duration `long:"wait" description:"wait duration on rate limit before retry (default: 10m; 0 disables retries)"`
	SessionTimeout          time.Duration `long:"session-timeout" description:"per-session timeout (e.g. 30m, 1h); external Codex/custom review under a Claude primary excluded"`
	IdleTimeout             time.Duration `long:"idle-timeout" description:"kill claude/codex executor session after no output for this duration (e.g. 5m, 10m)"`
	SkipFinalize            bool          `long:"skip-finalize" description:"skip finalize step even if enabled in config"`
	PreserveAnthropicAPIKey bool          `long:"preserve-anthropic-api-key" description:"pass ANTHROPIC_API_KEY through to claude (for users authenticating Claude Code via API key rather than OAuth/keychain)"`
	NoClaudeSwap            bool          `long:"no-claude-swap" description:"disable automatic claude-swap account rotation for this run"`
	Codex                   bool          `long:"codex" description:"use codex CLI as the executor for task, review, and finalize phases"`
	PassClaudeMd            bool          `long:"pass-claude-md" description:"pass project CLAUDE.md to codex via project_doc_fallback_filenames; user-level ~/.claude/CLAUDE.md is NOT auto-passed but a one-time setup hint is shown (codex executor only)"`
	Worktree                bool          `long:"worktree" description:"run in isolated git worktree"`
	Commit                  bool          `short:"c" long:"commit" description:"auto-commit the dirty source checkout before creating the worktree (requires --worktree)"`
	Branch                  string        `long:"branch" description:"override branch name for worktree/branch creation (default: derived from plan filename)"`
	PlanDescription         string        `long:"plan" description:"create plan interactively (enter plan description)"`
	GenAgents               bool          `long:"gen-agents" description:"generate project-specific review agents into .loopai/agents/ and exit"`
	Debug                   bool          `short:"d" long:"debug" description:"enable debug logging"`
	NoColor                 bool          `long:"no-color" description:"disable color output"`
	Orca                    bool          `long:"orca" env:"LOOPAI_ORCA" description:"emit terminal title status for orca"`
	Version                 bool          `short:"v" long:"version" description:"print version and exit"`
	Serve                   bool          `short:"s" long:"serve" description:"start web dashboard for real-time streaming"`
	Port                    int           `short:"p" long:"port" default:"8080" description:"web dashboard port"`
	Host                    string        `long:"host" default:"127.0.0.1" env:"LOOPAI_WEB_HOST" description:"web dashboard listen address"`
	Watch                   []string      `short:"w" long:"watch" description:"directories to watch for progress files (repeatable)"`
	Init                    bool          `long:"init" description:"initialize local .loopai/ config directory in current project"`
	Reset                   bool          `long:"reset" description:"interactively reset global config to embedded defaults"`
	Clear                   bool          `long:"clear" description:"remove loopai cmux status pill"`
	CmuxWorkspace           string        `long:"cmux-workspace" optional:"true" optional-value:"always" choice:"always" choice:"auto" description:"relaunch in a new cmux workspace: bare/always = unconditionally, auto = only when the current workspace already runs loopai"`
	Merge                   string        `long:"merge" optional:"true" optional-value:"" value-name:"base" description:"merge feature branch into base branch; positional argument names the feature (branch or plan), default current branch"`
	PR                      string        `long:"pr" optional:"true" optional-value:"" value-name:"base" description:"push feature branch and create a GitHub pull request; positional argument names the feature (branch or plan), default current branch"`
	DumpDefaults            string        `long:"dump-defaults" description:"extract raw embedded defaults to specified directory"`
	ConfigDir               string        `long:"config-dir" env:"LOOPAI_CONFIG_DIR" description:"custom config directory"`

	PlanFile  string   `positional-arg-name:"plan-file" description:"path to one plan file or a comma-separated plan chain (optional, uses fzf if omitted); with --merge/--pr it names the feature to close out"`
	PlanFiles []string // normalized comma-separated plan chain; PlanFile is always its first entry

	// positional arguments beyond the first, recorded by main so close-out validation can reject
	// them instead of silently acting on the wrong feature
	extraArgs []string

	// set by markFlagsSet after parsing; true when the flag was explicitly provided on the CLI
	waitSet           bool
	sessionTimeoutSet bool
	idleTimeoutSet    bool
	mergeSet          bool
	prSet             bool
	executionModeSet  bool

	claudeCommandSet       bool
	claudeArgsSet          bool
	codexArgsSet           bool
	externalReviewToolSet  bool
	externalReviewModelSet bool
	externalReviewersSet   bool
	customReviewScriptSet  bool
}

const commandUsage = "[OPTIONS] [plan-file[,plan-file...]]"

// applyPositionalArgs records the parsed positional arguments: the first names a comma-separated
// plan chain, or the feature to close out under --merge/--pr. the rest are kept so close-out
// validation can reject them; go-flags never fills a positional field beyond the first one
// declared. Close-out identifiers are deliberately not split because they do not name plans.
func (o *opts) applyPositionalArgs(args []string) error {
	if len(args) == 0 {
		return nil
	}
	o.PlanFile = args[0]
	o.extraArgs = args[1:]
	if closeoutRequested(*o) {
		return nil
	}

	planFiles, err := parsePlanFiles(args[0])
	if err != nil {
		return err
	}
	o.PlanFiles = planFiles
	o.PlanFile = planFiles[0]
	return nil
}

func parsePlanFiles(arg string) ([]string, error) {
	parts := strings.Split(arg, ",")
	result := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for i, part := range parts {
		planFile := strings.TrimSpace(part)
		if planFile == "" {
			return nil, fmt.Errorf("plan chain entry %d is empty", i+1)
		}
		duplicateKey := canonicalPlanPath(planFile)
		if _, exists := seen[duplicateKey]; exists {
			return nil, fmt.Errorf("plan chain contains duplicate entry %q", planFile)
		}
		seen[duplicateKey] = struct{}{}
		result = append(result, planFile)
	}
	return result, nil
}

func canonicalPlanPath(planFile string) string {
	absPath, err := filepath.Abs(planFile)
	if err != nil {
		return filepath.Clean(planFile)
	}
	if resolved, resolveErr := filepath.EvalSymlinks(absPath); resolveErr == nil {
		absPath = resolved
	}
	return filepath.Clean(absPath)
}

// markFlagsSet detects options whose explicit presence matters even when their parsed value is
// zero or empty. This both supports zero-valued config overrides and prevents standalone close-out
// commands from being combined with execution options such as --max-iterations=0.
func (o *opts) markFlagsSet(parser *flags.Parser) {
	if parser == nil {
		return
	}
	o.waitSet = isFlagSet(parser, "wait")
	o.sessionTimeoutSet = isFlagSet(parser, "session-timeout")
	o.idleTimeoutSet = isFlagSet(parser, "idle-timeout")
	o.mergeSet = isFlagSet(parser, "merge")
	o.prSet = isFlagSet(parser, "pr")
	o.claudeCommandSet = isFlagSet(parser, "claude-command")
	o.claudeArgsSet = isFlagSet(parser, "claude-args")
	o.codexArgsSet = isFlagSet(parser, "codex-args")
	o.externalReviewToolSet = isFlagSet(parser, "external-review-tool")
	o.externalReviewModelSet = isFlagSet(parser, "external-review-model")
	o.externalReviewersSet = isFlagSet(parser, "external-reviewers")
	o.customReviewScriptSet = isFlagSet(parser, "custom-review-script")
	for _, name := range []string{
		"max-iterations", "max-external-iterations", "review-patience",
		"plan-model", "task-model", "review-model", "claude-command", "claude-args", "codex-args",
		"external-review-tool", "external-review-model", "external-reviewers", "custom-review-script",
		"review", "external-only", "codex-only", "tasks-only", "base-ref", "wait",
		"session-timeout", "idle-timeout", "skip-finalize", "preserve-anthropic-api-key",
		"no-claude-swap", "codex", "pass-claude-md", "worktree",
		"branch", "plan", "gen-agents", "serve", "watch", "init", "reset", "dump-defaults",
	} {
		if isFlagSet(parser, name) {
			o.executionModeSet = true
			break
		}
	}
}

var revision = "unknown"

// resolveVersion returns the best available version string.
// priority: ldflags revision → module version from go install → VCS commit hash → "unknown".
func resolveVersion() string {
	if revision != "unknown" {
		return revision
	}
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return revision
	}
	// go install sets module version to the tag (e.g. v0.10.0)
	if bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	// local build without ldflags — try VCS revision
	for _, s := range bi.Settings {
		if s.Key == "vcs.revision" && len(s.Value) >= 7 {
			return s.Value[:7]
		}
	}
	return revision
}

// stderrLog is a simple logger that writes to stderr.
// satisfies notify.logger interface for use before progress logger is available.
type stderrLog struct{}

func (stderrLog) Print(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
}

// startupInfo holds parameters for printing startup information.
type startupInfo struct {
	PlanFile                string
	PlanDescription         string // used for plan mode instead of PlanFile
	Branch                  string
	Mode                    processor.Mode
	MaxIterations           int
	ProgressPath            string
	Executor                string
	PassClaudeMd            bool
	PreserveAnthropicAPIKey bool   // when true, surfaced in the banner so users can spot wrong-context runs before claude bills the wrong account
	CodexModel              string // resolved model for codex plan/task phase; "" means codex picks from ~/.codex/config.toml
	CodexEffort             string // resolved reasoning effort for codex plan/task phase; "" means codex default
	CodexReviewModel        string // resolved model for codex review phase; shown only when it differs from CodexModel
	CodexReviewEffort       string // resolved reasoning effort for codex review phase; shown only when it differs from CodexEffort
	CodexSandbox            string // resolved sandbox for codex executor; always non-empty when Executor == codex
	ExternalReview          externalReviewSelection
}

// executePlanRequest holds parameters for plan execution.
type executePlanRequest struct {
	PlanFile            string
	MainPlanFile        string // original plan path in main repo (worktree mode); empty in normal mode
	Mode                processor.Mode
	GitSvc              *git.Service
	MainGitSvc          *git.Service // main repo service for cross-boundary ops (worktree mode); nil in normal mode
	Config              *config.Config
	Colors              *progress.Colors
	DefaultBranch       string             // base for non-worktree branch creation; worktrees are created from current HEAD
	BaseRef             string             // base reference for review diffs and templates (--base-ref override or DefaultBranch)
	WorktreeStartRef    string             // immutable predecessor tip for stacked chain worktrees and branch hand-off
	ChainSuccessor      bool               // true for plan N+1; non-worktree runs branch from the current plan N tip
	ChainResume         bool               // true for the first pending member of a resumed chain
	ChainResumePrepared bool               // retry follows a fully prepared first-member branch/worktree
	ChainPlanFiles      []string           // all chain inputs; non-empty only for multi-plan execution
	ChainPrepared       func(string) error // persists the prepared branch tip before execution begins
	NotifySvc           *notify.Service
	BranchOverride      string                // branch name override (--branch flag); empty = derive from plan filename
	WtCleanup           *cleanupHolder        // worktree cleanup for interrupt handler; nil when not in worktree mode
	CmuxStop            *cleanupHolder        // cmux sidebar reset for interrupt handler; nil when not wired
	OrcaStop            *cleanupHolder        // terminal-title reset for interrupt handler; nil when not wired
	CmuxHandoff         func()                // releases a quiesced predecessor after this reporter starts
	CmuxPredecessorStop func()                // clears a retained predecessor on force-exit before hand-off
	CmuxRetain          func(retainedCmuxRun) // keeps a successful chain member busy until its successor starts
	SetupTitles         *orca.Reporter        // setup reporter retained until the phase-specific reporter starts
	BeforeCmuxFinish    func(bool)            // final repository/log cleanup; bool reports execution success
	Outcome             *planExecutionOutcome // explicit completion state; aborts return nil but do not succeed
	ProgressLog         *progress.Logger      // pre-created logger (worktree mode); nil in normal mode
	PhaseHolder         *status.PhaseHolder   // pre-created holder (worktree mode); nil in normal mode
	ExternalReview      externalReviewSelection
	LimitRecovery       limits.Recovery
}

type retainedCmuxRun struct {
	reporter *cmux.Reporter
	planFile string
	branch   string
	elapsed  string
}

// planExecutionOutcome separates a completed plan from a nil command error. executePlan returns
// nil when the user aborts, so chain callers must require this explicit success signal before
// starting a dependent plan.
type planExecutionOutcome struct {
	succeeded bool
	branchTip string
}

// cleanupHolder holds a cleanup function with mutex for safe cross-goroutine access.
// the interrupt watcher goroutine calls cleanup on force-exit, while the main goroutine populates it.
// used for both worktree removal and the cmux sidebar reset: on the force-exit path defers never
// run, so anything that must not outlive the process has to hang off the interrupt handler.
type cleanupHolder struct {
	mu sync.Mutex
	fn func()
}

func (c *cleanupHolder) set(fn func()) {
	c.mu.Lock()
	c.fn = fn
	c.mu.Unlock()
}

func (c *cleanupHolder) call() {
	c.mu.Lock()
	fn := c.fn
	c.mu.Unlock()
	if fn != nil {
		fn()
	}
}

func main() {
	if os.Getenv("GO_FLAGS_COMPLETION") == "" {
		fmt.Printf("loopai %s\n", resolveVersion())
	}

	var o opts
	parser := flags.NewParser(&o, flags.Default)
	parser.Usage = commandUsage

	args, err := parser.Parse()
	if err != nil {
		var flagsErr *flags.Error
		if errors.As(err, &flagsErr) && flagsErr.Type == flags.ErrHelp {
			os.Exit(0)
		}
		os.Exit(1)
	}

	if o.Version {
		os.Exit(0)
	}

	// detect explicitly-set zero values for duration flags so --flag 0 can disable config values.
	// go-flags can't distinguish "not provided" from "set to zero" via the field alone.
	o.markFlagsSet(parser)

	// handle positional arguments after marking flags so --merge/--pr identifiers remain opaque.
	if err := o.applyPositionalArgs(args); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	// setup context with signal handling
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := run(ctx, o); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, o opts) (runErr error) {
	// suppress ^C echo in terminal before setting up interrupt watcher
	restoreTerminal := disableCtrlCEcho()
	defer restoreTerminal()

	// worktree cleanup function, populated after worktree creation.
	// synchronized for safe access from the interrupt watcher goroutine.
	wtCleanup := &cleanupHolder{}

	// cmux sidebar reset, populated once the reporter is created. cmux has no TTL on the
	// workspace loading spinner, so a run that exits without clearing it leaves it in the
	// sidebar until cmux itself restarts.
	cmuxStop := &cleanupHolder{}
	defer cmuxStop.call()
	orcaStop := &cleanupHolder{}

	// print immediate feedback when context is canceled (Ctrl+C).
	// returned cleanup ensures goroutine exits when run() returns, avoiding leaks in tests.
	defer startInterruptWatcher(ctx, forceExitCleanup(restoreTerminal, cmuxStop, orcaStop, wtCleanup))()

	// A normal invocation replaces any prior completion outcome even when startup or preflight
	// fails. Auto mode waits until it owns a reservation or reporter, since a pre-validation pill
	// may belong to another live run. Explicit clear and close-out attempts retain the old pill until
	// their own success paths clear it. A possible watch-only invocation waits for config loading so
	// configured watch directories can be considered.
	resolveStaleCmuxStatus := prepareStaleCmuxStatus(o)

	// Validate before querying or spawning so an invalid invocation cannot create an orphan
	// workspace. Auto mode still preserves the existing pill here: this process has not queried it
	// or acquired a reservation, so clearing it could erase another live run's busy marker.
	if err := validateFlags(o); err != nil {
		resolveStaleCmuxStatus(false)
		return err
	}

	done, autoWorkspaceReserved, preserveEarlyStatus, err := prepareRunBeforeConfig(o, cmuxStop)
	if err != nil || done {
		resolveStaleCmuxStatus(preserveEarlyStatus)
		return err
	}
	// load config first to get custom command paths
	cfg, err := loadRunConfig(o)
	if err != nil {
		resolveStaleCmuxStatus(false)
		return err
	}

	// create colors from config (all colors guaranteed populated via fallback)
	colors := progress.NewColors(cfg.Colors)

	// standalone git close-out commands do not require executor or notification dependencies.
	if closeoutRequested(o) {
		return runCloseoutCommand(ctx, o, cfg, colors)
	}
	watchOnly := isWatchOnlyMode(o, cfg.WatchDirs)
	resolveAutoWorkspaceReservationAfterConfig(
		o, watchOnly, autoWorkspaceReserved, cmuxStop, os.Stderr)
	resolveStaleCmuxStatus(watchOnly)

	// watch-only mode: --serve with watch dirs (CLI or config) and no plan file
	// runs web dashboard without plan execution, can run from any directory
	if watchOnly {
		return runWatchOnly(ctx, o, cfg, colors)
	}

	mode := determineMode(o)
	// Startup and setup happen before executePlan or runPlanMode can construct their reporters.
	// Keep a title reporter alive across those boundaries so prompts and genuine preflight errors
	// are visible. Standalone agent generation deliberately does not emit Orca titles.
	var setupTitles *orca.Reporter
	if mode != processor.ModeGenAgents {
		setupTitles = startOrcaReporter(cfg, "", initialOrcaPhase(mode))
		setOrcaCleanup(orcaStop, setupTitles)
	}
	defer func() {
		finishOrcaFailure(setupTitles, runErr)
		setupTitles.Stop()
	}()

	externalReview, resolveErr := resolveExternalReviewSelection(o, cfg, mode)
	if resolveErr != nil {
		return resolveErr
	}
	printExternalReviewWarnings(o, externalReview, cfg, os.Stderr)
	externalReview, err = checkExecutionDeps(cfg, externalReview, os.Stderr)
	if err != nil {
		return err
	}
	applyEffectiveExternalReview(cfg, externalReview)
	limitRecovery := detectClaudeSwapRecovery(o, cfg, externalReview)

	if depErr := ctx.Err(); depErr != nil {
		return fmt.Errorf("execution context: %w", depErr)
	}

	if repoErr := requireRepoRoot(cfg); repoErr != nil {
		return repoErr
	}

	// agent generation is standalone: it needs the executor and the repository, but no
	// branch, plan selection, or review pipeline. routed after the dependency check so a
	// missing executor fails the same way it does for --plan.
	if mode == processor.ModeGenAgents {
		return runGenAgentsMode(ctx, o, cfg, colors, limitRecovery)
	}

	// create notification service (nil if no channels configured). notify.New validates the
	// configured channels and fails fast on a misconfigured one, so it runs only on the paths
	// that actually notify: watch-only, the close-out commands, and --gen-agents send nothing,
	// and a half-filled slack or email block must not be what stops them from running.
	notifySvc, err := notify.New(cfg.NotifyParams, stderrLog{})
	if err != nil {
		return fmt.Errorf("create notification service: %w", err)
	}

	// open git repository via Service
	gitSvc, err := openGitService(colors, cfg.VcsCommand)
	if err != nil {
		return fmt.Errorf("open git repo: %w", err)
	}
	gitSvc.SetCommitTrailer(cfg.CommitTrailer)

	// Ensure the repository is executable and reject repository-dependent chain problems before
	// resolving branches or creating any plan artifacts.
	if repoErr := validateExecutionRepository(ctx, o, gitSvc, cfg.WorktreeEnabled, setupTitles); repoErr != nil {
		return repoErr
	}

	// defaultBranch is for non-worktree branch creation, baseRef for review diffs and the
	// {{DEFAULT_BRANCH}} template variable. In worktree mode --base-ref stays diff-only because
	// the worktree branch is always cut from the current HEAD.
	branchMode := modeCreatesBranch(mode)
	creationMode := branchMode
	defaultBranch, baseRef, err := resolveBaseRefs(gitSvc, o.BaseRef, cfg.DefaultBranch,
		creationMode, cfg.WorktreeEnabled && creationMode)
	if err != nil {
		return err
	}

	// create plan selector for use by plan selection and plan mode
	selector := plan.NewSelector(cfg.PlansDir, colors)
	if setupTitles != nil {
		selector.SetInputWait(setupTitles.WithInputWait)
	}

	// plan mode has different flow - doesn't require plan file selection
	if mode == processor.ModePlan {
		return runPlanMode(ctx, o, executePlanRequest{
			Mode:           processor.ModePlan,
			GitSvc:         gitSvc,
			Config:         cfg,
			Colors:         colors,
			DefaultBranch:  defaultBranch,
			BaseRef:        baseRef,
			NotifySvc:      notifySvc,
			WtCleanup:      wtCleanup,
			CmuxStop:       cmuxStop,
			OrcaStop:       orcaStop,
			SetupTitles:    setupTitles,
			BranchOverride: o.Branch,
			ExternalReview: externalReview,
			LimitRecovery:  limitRecovery,
		}, selector)
	}

	req := executePlanRequest{
		Mode:           mode,
		GitSvc:         gitSvc,
		Config:         cfg,
		Colors:         colors,
		DefaultBranch:  defaultBranch,
		BaseRef:        baseRef,
		NotifySvc:      notifySvc,
		WtCleanup:      wtCleanup,
		CmuxStop:       cmuxStop,
		OrcaStop:       orcaStop,
		SetupTitles:    setupTitles,
		BranchOverride: o.Branch,
		ExternalReview: externalReview,
		LimitRecovery:  limitRecovery,
	}
	return runSelectedPlans(ctx, o, req, selector, setupTitles, os.Stdout, selectAndExecutePlan)
}

func validateExecutionRepository(
	ctx context.Context, o opts, gitSvc *git.Service, worktree bool, setupTitles *orca.Reporter,
) error {
	if err := ensureRepoHasCommits(ctx, gitSvc, os.Stdin, os.Stdout, setupTitles); err != nil {
		return err
	}
	if len(o.PlanFiles) <= 1 {
		return nil
	}
	if err := gitSvc.EnsureLocalGitignore(); err != nil {
		return fmt.Errorf("prepare plan chain runtime state: %w", err)
	}
	state, found, err := verifiedPlanChainCheckpoint(ctx, o, gitSvc, worktree)
	if err != nil {
		return err
	}
	completed := 0
	if found {
		completed = state.Completed
	}
	if err := gitSvc.ValidatePlanChain(o.PlanFiles[completed:]); err != nil {
		return fmt.Errorf("validate plan chain repository state: %w", err)
	}
	return nil
}

// prepareRunBeforeConfig decides hand-off before taking a local reservation: otherwise auto mode
// would see its own "starting" pill and always classify the workspace as busy. Local utilities run
// after reservation because a combined --reset + run may block for input at its first prompt. A
// maybe-watch-only reset also reserves until the post-reset config proves whether execution follows.
func prepareRunBeforeConfig(o opts, cmuxStop *cleanupHolder) (done, reserved, preserve bool, err error) {
	if stop, handOffErr := handOffToCmuxWorkspace(o, os.Args[1:], os.Stdout, os.Stderr); handOffErr != nil || stop {
		return stop, false, handOffSucceeded(o, handOffErr), handOffErr
	}

	reserveBeforeConfig := !mayBeWatchOnlyMode(o) || o.Reset
	reserved = ensureAutoWorkspaceReservation(o, reserveBeforeConfig, false, cmuxStop, os.Stderr)
	if earlyDone, earlyErr := handleEarlyFlags(o); earlyErr != nil || earlyDone {
		return earlyDone, reserved, false, earlyErr
	}
	return false, reserved, false, nil
}

// requireRepoRoot enforces that execution starts at a repository root.
// when using a non-git vcs command, the .git check is skipped — NewService's
// rev-parse --show-toplevel validates the repo instead (pure hg repos have no .git).
func requireRepoRoot(cfg *config.Config) error {
	if cfg.VcsCommand != "" && cfg.VcsCommand != "git" {
		return nil
	}
	if _, err := os.Stat(".git"); err != nil {
		return errors.New("must run from repository root (no .git directory found); run from the repo root or 'git init' for a new project")
	}
	return nil
}

func loadRunConfig(o opts) (*config.Config, error) {
	cfg, err := config.Load(o.ConfigDir)
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	// Close-out modes reject execution flags and only consume VCS/color configuration. Do not
	// let executor-only semantic checks make an otherwise valid merge or PR impossible.
	if closeoutRequested(o) {
		return cfg, nil
	}
	if overrideErr := applyCLIOverrides(o, cfg); overrideErr != nil {
		return nil, overrideErr
	}
	return cfg, nil
}

// selectAndExecutePlan selects a plan file, sets up branch or worktree, and runs execution.
func selectAndExecutePlan(ctx context.Context, o opts, req executePlanRequest, selector *plan.Selector) (runErr error) {
	defer func() { finishOrcaFailure(req.SetupTitles, runErr) }()

	// plan is optional only for review modes (ModeReview, ModeCodexOnly)
	planOptional := req.Mode == processor.ModeReview || req.Mode == processor.ModeCodexOnly
	planFile, err := selector.Select(ctx, o.PlanFile, planOptional)
	if err != nil {
		// check for auto-plan-mode: no plans found on default branch
		handled, autoPlanErr := tryAutoPlanMode(ctx, err, o, req, selector)
		if handled {
			return autoPlanErr
		}
		return fmt.Errorf("select plan: %w", err)
	}

	req.PlanFile = planFile

	// worktree mode: create worktree, chdir into it, run execution from there.
	if req.Config.WorktreeEnabled && planFile != "" && modeRequiresBranch(req.Mode) {
		return runWithWorktree(ctx, o, req)
	}

	if err := req.GitSvc.EnsureLocalGitignore(); err != nil {
		return fmt.Errorf("ensure gitignore: %w", err)
	}
	if planFile != "" && modeRequiresBranch(req.Mode) {
		if err := prepareSelectedPlanBranch(ctx, req, planFile); err != nil {
			return err
		}
	}

	return executePlan(ctx, o, req)
}

// prepareSelectedPlanBranch keeps the historical single-plan behavior while forcing every
// non-worktree chain member onto its own plan-derived branch.
func prepareSelectedPlanBranch(ctx context.Context, req executePlanRequest, planFile string) error {
	if len(req.ChainPlanFiles) > 1 && !req.ChainSuccessor {
		branch := req.GitSvc.EffectiveBranchName(planFile, req.BranchOverride)
		if req.ChainResume && req.GitSvc.BranchExists(branch) {
			if err := req.GitSvc.ResumeFirstPlanChainBranchContext(ctx, branch, req.ChainPlanFiles); err != nil {
				return fmt.Errorf("resume first chain branch for plan: %w", err)
			}
		} else if err := req.GitSvc.CreateBranchForPlanChainContext(
			ctx, planFile, req.BranchOverride, req.ChainPlanFiles,
		); err != nil {
			return fmt.Errorf("create first chain branch for plan: %w", err)
		}
		return recordPreparedChainBranch(req, branch)
	}
	if req.ChainSuccessor {
		branch := req.GitSvc.EffectiveBranchName(planFile, req.BranchOverride)
		if req.ChainResume && req.GitSvc.BranchExists(branch) {
			if err := req.GitSvc.ResumePlanChainBranchContext(ctx, branch, req.WorktreeStartRef); err != nil {
				return fmt.Errorf("resume branch for plan: %w", err)
			}
			return recordPreparedChainBranch(req, branch)
		}
		if err := req.GitSvc.CreateBranchForPlanFromExpectedHEADContext(
			ctx, planFile, req.BranchOverride, req.WorktreeStartRef,
		); err != nil {
			return fmt.Errorf("create branch for plan: %w", err)
		}
		return recordPreparedChainBranch(req, branch)
	}
	if err := req.GitSvc.CreateBranchForPlan(planFile, req.DefaultBranch, req.BranchOverride); err != nil {
		return fmt.Errorf("create branch for plan: %w", err)
	}
	return nil
}

func recordPreparedChainBranch(req executePlanRequest, branch string) error {
	if req.ChainPrepared == nil {
		return nil
	}
	tip, err := req.GitSvc.BranchHash(branch)
	if err != nil {
		return fmt.Errorf("capture prepared chain branch tip: %w", err)
	}
	if err := req.ChainPrepared(tip); err != nil {
		return fmt.Errorf("checkpoint prepared chain branch: %w", err)
	}
	return nil
}

type planChainExecutor func(context.Context, opts, executePlanRequest, *plan.Selector) error

func initializePlanChainRun(
	ctx context.Context, o opts, req executePlanRequest,
) (planChainCheckpoint, bool, error) {
	if err := req.GitSvc.EnsureLocalGitignore(); err != nil {
		return planChainCheckpoint{}, false, fmt.Errorf("prepare plan chain runtime state: %w", err)
	}
	state, found, err := verifiedPlanChainCheckpoint(ctx, o, req.GitSvc, req.Config.WorktreeEnabled)
	if err != nil {
		return planChainCheckpoint{}, false, err
	}
	if found {
		return state, true, nil
	}
	state = planChainCheckpoint{Mode: string(req.Mode), Worktree: req.Config.WorktreeEnabled}
	if req.Config.WorktreeEnabled && !o.Commit {
		state.SourcePlans, err = req.GitSvc.CapturePlanChainSourceState(o.PlanFiles)
		if err != nil {
			return planChainCheckpoint{}, false, fmt.Errorf("capture plan chain source state: %w", err)
		}
	}
	if err := savePlanChainCheckpoint(req.GitSvc.Root(), o.PlanFiles, state); err != nil {
		return planChainCheckpoint{}, false, err
	}
	return state, false, nil
}

// runSelectedPlans preserves the existing single-plan path and activates chain lifecycle handling
// only for validated comma-separated inputs.
func runSelectedPlans(
	ctx context.Context,
	o opts,
	req executePlanRequest,
	selector *plan.Selector,
	startupTitles *orca.Reporter,
	out io.Writer,
	execute planChainExecutor,
) error {
	if len(o.PlanFiles) <= 1 {
		return execute(ctx, o, req, selector)
	}
	return runPlanChain(ctx, o, req, selector, startupTitles, out, execute)
}

// runPlanChain executes validated plan files in order. Each call must return only after its
// reporter, progress logger, and worktree cwd have been released; the next logger therefore cannot
// contend with the previous plan and the next worktree is always prepared from the source cwd.
func runPlanChain(
	ctx context.Context,
	o opts,
	req executePlanRequest,
	selector *plan.Selector,
	startupTitles *orca.Reporter,
	out io.Writer,
	execute planChainExecutor,
) (runErr error) {
	total := len(o.PlanFiles)
	state, resumed, err := initializePlanChainRun(ctx, o, req)
	if err != nil {
		return err
	}
	completed := state.Completed
	startIndex := state.Completed
	previousTip := state.PreviousTip
	if startIndex > 0 && startIndex < total {
		fmt.Fprintf(out, "\nresuming plan chain at plan %d/%d from %s\n", startIndex+1, total, previousTip)
	}
	var retained *retainedCmuxRun
	defer func() {
		finishRetainedChainCmux(retained, runErr, completed == total)
		if completed == total {
			fmt.Fprintf(out, "\nplan chain complete: %d/%d plans succeeded\n", completed, total)
			return
		}
		fmt.Fprintf(out, "\nplan chain stopped: %d/%d plans succeeded\n", completed, total)
	}()

	for i := startIndex; i < total; i++ {
		planFile := o.PlanFiles[i]
		if ctx.Err() != nil {
			return nil
		}
		fmt.Fprintf(out, "\nplan %d/%d: %s\n", i+1, total, planFile)
		state.Active = i + 1
		state.ActiveStartTip = ""
		state.ActivePrepared = false
		activeBranch := req.GitSvc.EffectiveBranchName(planFile, "")
		if req.GitSvc.BranchExists(activeBranch) {
			state.ActiveStartTip, err = req.GitSvc.BranchHash(activeBranch)
			if err != nil {
				return fmt.Errorf("capture active plan branch tip: %w", err)
			}
		}
		if err := savePlanChainCheckpoint(req.GitSvc.Root(), o.PlanFiles, state); err != nil {
			return err
		}

		outcome, nextRetained, err := executePlanChainMember(
			ctx, o, req, selector, startupTitles, execute, i, startIndex, planFile, previousTip,
			resumed, &state, retained,
		)
		retained = nextRetained
		if err != nil {
			return err
		}
		if !outcome.succeeded {
			// executePlan deliberately maps a user abort to nil. Do not start a dependent plan.
			return nil
		}

		completed++
		previousTip = outcome.branchTip
		if previousTip == "" {
			previousTip, err = req.GitSvc.HeadHash()
			if err != nil {
				return fmt.Errorf("capture completed plan tip: %w", err)
			}
		}
		state.Completed = completed
		state.Active = 0
		state.ActiveStartTip = ""
		state.ActivePrepared = false
		state.ResumePreparedTip = ""
		state.PreviousTip = previousTip
		if err := savePlanChainCheckpoint(req.GitSvc.Root(), o.PlanFiles, state); err != nil {
			return err
		}
	}
	return finalizeCompletedPlanChain(o, req, &state)
}

func finishRetainedChainCmux(retained *retainedCmuxRun, runErr error, complete bool) {
	if retained == nil {
		return
	}
	if runErr != nil {
		retained.reporter.Finish(false, runErr.Error())
	} else if complete {
		finishCmuxCompletion(retained.reporter, nil, retained.planFile, retained.branch, retained.elapsed, nil)
	}
	retained.reporter.Stop()
}

func executePlanChainMember(
	ctx context.Context,
	o opts,
	req executePlanRequest,
	selector *plan.Selector,
	startupTitles *orca.Reporter,
	execute planChainExecutor,
	index, startIndex int,
	planFile, previousTip string,
	resumed bool,
	state *planChainCheckpoint,
	retained *retainedCmuxRun,
) (*planExecutionOutcome, *retainedCmuxRun, error) {
	setupTitles := startOrcaReporter(req.Config, planFile, initialOrcaPhase(req.Mode))
	setOrcaCleanup(req.OrcaStop, setupTitles)
	if index == startIndex && startupTitles != nil {
		startupTitles.Stop()
	}
	planOpts := o
	planOpts.PlanFile = planFile
	if index > 0 {
		planOpts.Commit = false
	}
	outcome := &planExecutionOutcome{}
	planReq := req
	planReq.SetupTitles = setupTitles
	predecessor := retained
	if predecessor != nil {
		planReq.CmuxPredecessorStop = predecessor.reporter.Stop
	}
	planReq.CmuxHandoff = func() {
		if predecessor != nil {
			predecessor.reporter.Release()
			if retained == predecessor {
				retained = nil
			}
		}
		setupTitles.Stop()
	}
	planReq.CmuxRetain = func(run retainedCmuxRun) { retained = &run }
	planReq.Outcome = outcome
	planReq.ChainSuccessor = index > 0
	planReq.ChainResume = index == startIndex && resumed
	planReq.ChainResumePrepared = planReq.ChainResume && state.ResumePreparedTip != ""
	planReq.ChainPlanFiles = o.PlanFiles
	planReq.WorktreeStartRef = previousTip
	planReq.ChainPrepared = func(tip string) error {
		state.ActiveStartTip = tip
		state.ActivePrepared = true
		return savePlanChainCheckpoint(req.GitSvc.Root(), o.PlanFiles, *state)
	}
	err := execute(ctx, planOpts, planReq, selector)
	finishOrcaFailure(setupTitles, err)
	setupTitles.Stop()
	return outcome, retained, err
}

func finalizeCompletedPlanChain(o opts, req executePlanRequest, state *planChainCheckpoint) error {
	if req.Config.WorktreeEnabled && !state.SourceReconciled {
		if err := req.GitSvc.ReconcilePlanChainSourceState(state.SourcePlans); err != nil {
			return fmt.Errorf("reconcile completed plan chain source files: %w", err)
		}
		state.SourceReconciled = true
		if err := savePlanChainCheckpoint(req.GitSvc.Root(), o.PlanFiles, *state); err != nil {
			return err
		}
	}
	return removePlanChainCheckpoint(req.GitSvc.Root(), o.PlanFiles, state.Mode)
}

// getCurrentBranch returns the current git branch name or "unknown" if unavailable.
func getCurrentBranch(gitSvc *git.Service) string {
	branch, err := gitSvc.CurrentBranch()
	if err != nil || branch == "" {
		return "unknown"
	}
	return branch
}

// tryAutoPlanMode attempts to switch to plan mode when no plans are found on the default branch.
// when no plans are found but auto-plan-mode does not apply, it returns (true, err) with an
// explanatory error so the user always learns why interactive plan creation was not offered.
// returns (true, nil) if the user canceled, (true, err) if plan mode was attempted or refused
// with a reason, or (false, nil) if the selection error is unrelated to missing plans.
func tryAutoPlanMode(ctx context.Context, err error, o opts, req executePlanRequest,
	selector *plan.Selector) (bool, error) {
	// only a missing-plans error is a candidate for auto-plan-mode; other errors propagate as-is.
	if !errors.Is(err, plan.ErrNoPlansFound) {
		return false, nil
	}

	// interactive plan creation only runs in full execution mode; explain when another mode suppresses it.
	if o.Review || o.ExternalOnly || o.CodexOnly || o.TasksOnly {
		return true, fmt.Errorf("interactive plan creation is not available in this mode; provide an existing plan file: %w", err)
	}

	isDefault, branchErr := req.GitSvc.IsDefaultBranch(req.DefaultBranch)
	if branchErr != nil {
		return true, fmt.Errorf(
			"cannot offer interactive plan creation: failed to determine current branch (%v); "+
				"pass a plan file or use --plan: %w", branchErr, err)
	}
	if !isDefault {
		// normalize the default-branch name for display the same way matchesDefaultBranch compares it:
		// strip the origin/ prefix and fall back to main/master when unset, so the hint names the
		// local branch the user can actually switch to rather than "origin/main" or an empty string.
		defaultName := strings.TrimPrefix(req.DefaultBranch, "origin/")
		if defaultName == "" {
			defaultName = "main/master"
		}
		return true, fmt.Errorf(
			"interactive plan creation is only offered on the default branch %q (currently on %q); "+
				"switch to %q, pass a plan file, or use --plan: %w",
			defaultName, getCurrentBranch(req.GitSvc), defaultName, err)
	}

	var description string
	if !req.SetupTitles.WithInputWait(func() bool {
		description = plan.PromptDescription(ctx, os.Stdin, req.Colors)
		return description != ""
	}) {
		return true, nil // user canceled
	}

	o.PlanDescription = description
	req.Mode = processor.ModePlan
	return true, runPlanMode(ctx, o, req, selector)
}

// progressLogResult holds the result of progress logger setup.
type progressLogResult struct {
	holder   *status.PhaseHolder
	baseLog  *progress.Logger
	closeLog func()
}

// setupProgressLogger creates or reuses a progress logger and phase holder.
// when req.ProgressLog and req.PhaseHolder are pre-created (worktree mode), uses them directly.
func setupProgressLogger(o opts, req executePlanRequest, branch string) (progressLogResult, error) {
	holder := req.PhaseHolder
	if holder == nil {
		holder = &status.PhaseHolder{}
	}

	var baseLog *progress.Logger
	var closeOnce sync.Once
	closeLog := func() {} // no-op default for externally-owned logger
	if req.ProgressLog != nil {
		baseLog = req.ProgressLog
	} else {
		var err error
		baseLog, err = progress.NewLogger(progress.Config{
			PlanFile:       req.PlanFile,
			Mode:           string(req.Mode),
			Branch:         branch,
			BranchOverride: req.BranchOverride,
			Params:         runHeaderParams(o, req.Config, req.Mode, req.ExternalReview),
			NoColor:        o.NoColor,
		}, req.Colors, holder)
		if err != nil {
			return progressLogResult{}, fmt.Errorf("create progress logger: %w", err)
		}
		closeLog = func() {
			closeOnce.Do(func() {
				if closeErr := baseLog.Close(); closeErr != nil {
					fmt.Fprintf(os.Stderr, "warning: failed to close progress log: %v\n", closeErr)
				}
			})
		}
	}
	return progressLogResult{holder: holder, baseLog: baseLog, closeLog: closeLog}, nil
}

// sendNotification sends a completion or failure notification.
// uses context.Background() because the parent ctx may be canceled (e.g. SIGINT),
// and the notification timeout is applied inside Send() independently.
func sendNotification(req executePlanRequest, branch, elapsed string, stats git.DiffStats, runErr error) {
	req.NotifySvc.Send(context.Background(), buildNotifyResult(req, branch, elapsed, stats, runErr))
}

// notifyCmuxCompletion raises the end-of-run cmux notification. no-op outside cmux and for a
// user abort, where the decision is made by cmuxCompletionNotice.
func notifyCmuxCompletion(rep *cmux.Reporter, planFile, branch, elapsed string, runErr error) {
	subtitle, body, ok := cmuxCompletionNotice(planFile, branch, elapsed, runErr)
	if !ok {
		return
	}
	rep.Notify(subtitle, body)
}

// finishCmuxCompletion raises the transient completion notification and leaves the matching
// persistent final-status pill. It is used by execution and review runs only; plan creation uses
// notifyCmuxCompletion directly because a created plan is not a finished implementation run.
func finishCmuxCompletion(rep *cmux.Reporter, titles *orca.Reporter, planFile, branch, elapsed string, runErr error) {
	subtitle, body, ok := cmuxCompletionNotice(planFile, branch, elapsed, runErr)
	if !ok {
		return
	}
	rep.Notify(subtitle, body)
	if runErr != nil {
		rep.Finish(false, runErr.Error())
		titles.Finish(false)
		return
	}
	rep.Finish(true, elapsed)
	titles.Finish(true)
}

// finishOrcaFailure publishes a failed title for genuine errors that occur outside the normal
// execution-completion path. User-controlled cancellations remain neutral stops.
func finishOrcaFailure(titles *orca.Reporter, runErr error) {
	if runErr == nil || isNeutralOrcaStop(runErr) {
		return
	}
	titles.Finish(false)
}

func isNeutralOrcaStop(runErr error) bool {
	return errors.Is(runErr, processor.ErrUserAborted) ||
		errors.Is(runErr, processor.ErrUserRejectedPlan) ||
		errors.Is(runErr, plan.ErrPlanSelectionCanceled) ||
		errors.Is(runErr, git.ErrInitialCommitDeclined) ||
		errors.Is(runErr, context.Canceled)
}

// cmuxCompletionNotice builds the subtitle and body of the end-of-run cmux notification.
// ok is false for a user abort: the person who aborted is already at the terminal, so a
// banner would only be noise. both abort routes count — Ctrl+\ declining to resume yields
// ErrUserAborted, while Ctrl+C cancels the context and surfaces as a wrapped context.Canceled.
func cmuxCompletionNotice(planFile, branch, elapsed string, runErr error) (subtitle, body string, ok bool) {
	if errors.Is(runErr, processor.ErrUserAborted) || errors.Is(runErr, context.Canceled) {
		return "", "", false
	}
	target := cmuxNotifyTarget(planFile, branch)
	if runErr != nil {
		return "run failed", fmt.Sprintf("%s: %v", target, runErr), true
	}
	return "run completed", fmt.Sprintf("%s in %s", target, elapsed), true
}

// cmuxNotifyTarget names the run in a notification body: the plan basename when there is a
// plan, the branch for review modes that run without one.
func cmuxNotifyTarget(planFile, branch string) string {
	if planFile != "" {
		return filepath.Base(planFile)
	}
	return branch
}

// buildNotifyResult constructs a notify.Result from execution parameters.
func buildNotifyResult(req executePlanRequest, branch, elapsed string, stats git.DiffStats, runErr error) notify.Result {
	result := notify.Result{
		Mode:           string(req.Mode),
		PlanFile:       req.PlanFile,
		Branch:         branch,
		Duration:       elapsed,
		ExternalReview: externalReviewNotificationLabel(req.ExternalReview),
	}
	if runErr != nil {
		result.Status = "failure"
		result.Error = runErr.Error()
	} else {
		result.Status = "success"
		result.Files = stats.Files
		result.Additions = stats.Additions
		result.Deletions = stats.Deletions
	}
	return result
}

func externalReviewNotificationLabel(selection externalReviewSelection) string {
	if len(selection.Reviewers) <= 1 {
		return ""
	}
	return selection.chainLabel()
}

// displayStats prints completion summary with optional diff statistics and paths.
// mirrors the startup header format using displayMeta for plan/branch/progress.
// reflects where the plan actually lives: completed/ only when the move actually
// succeeded; original path when the move was skipped or failed.
func displayStats(req executePlanRequest, baseLog *progress.Logger, stats git.DiffStats, elapsed, branch string, planMoved bool) {
	if stats.Files > 0 {
		baseLog.LogDiffStats(stats.Files, stats.Additions, stats.Deletions)
		req.Colors.Info().Printf("\ncompleted in %s (%d files, +%d/-%d lines)\n",
			elapsed, stats.Files, stats.Additions, stats.Deletions)
	} else {
		req.Colors.Info().Printf("\ncompleted in %s\n", elapsed)
	}

	planPath := ""
	if req.PlanFile != "" {
		planFile := req.PlanFile
		// A worktree chain archives on the execution branch, not in the source checkout. Keep
		// its displayed path relative to that branch instead of claiming the source copy moved.
		if req.MainPlanFile != "" && len(req.ChainPlanFiles) <= 1 {
			planFile = req.MainPlanFile
		}
		planPath = planFile
		if planMoved {
			planPath = filepath.Join(filepath.Dir(planFile), "completed", filepath.Base(planFile))
		}
	}
	displayMeta(req.Colors, 2, planPath, branch, baseLog.Path())
}

// displayMeta prints plan (if set), branch, and progress log path with the given indent.
// file paths are converted to relative for readability.
func displayMeta(colors *progress.Colors, indent int, planFile, branch, progressPath string) {
	pad := strings.Repeat(" ", indent)
	if planFile != "" {
		colors.Info().Printf("%splan: %s\n", pad, toRelPath(planFile))
	}
	colors.Info().Printf("%sbranch: %s\n", pad, branch)
	colors.Info().Printf("%sprogress log: %s\n", pad, toRelPath(progressPath))
}

// keepDashboardAlive keeps the web dashboard running after execution completes.
// blocks until context is canceled (Ctrl+C). no-op if --serve is not enabled.
func keepDashboardAlive(ctx context.Context, o opts, req executePlanRequest, closeLog func()) {
	if !o.Serve {
		return
	}
	closeLog()
	req.Colors.Info().Printf("web dashboard still running at http://%s:%d (press Ctrl+C to exit)\n",
		web.ConnectHost(o.Host), o.Port)
	<-ctx.Done()
}

var newOrcaReporter = orca.New

// orcaReporter constructs the stdout title reporter selected by configuration. ExecutorClaude is
// the empty config value, so map it to the explicit agent name expected in terminal titles.
func orcaReporter(cfg *config.Config, planFile string) *orca.Reporter {
	if cfg == nil || !cfg.Orca {
		return nil
	}
	executor := "claude"
	if cfg.Executor == config.ExecutorCodex {
		executor = "codex"
	}
	return newOrcaReporter(true, planFile, executor)
}

// startOrcaReporter publishes a working title immediately so setup prompts can restore a working
// state and reporter handoffs never introduce a false working-to-idle completion transition.
func startOrcaReporter(cfg *config.Config, planFile string, phase status.Phase) *orca.Reporter {
	titles := orcaReporter(cfg, planFile)
	titles.OnPhase("", phase)
	return titles
}

func initialOrcaPhase(mode processor.Mode) status.Phase {
	switch mode {
	case processor.ModeFull, processor.ModeTasksOnly:
		return status.PhaseTask
	case processor.ModeReview:
		return status.PhaseReview
	case processor.ModeCodexOnly:
		return status.PhaseExternalReview
	case processor.ModePlan:
		return status.PhasePlan
	default:
		return ""
	}
}

func setOrcaCleanup(holder *cleanupHolder, titles *orca.Reporter) {
	if holder != nil {
		holder.set(titles.Stop)
	}
}

// buildRunnerLogger installs the orca title wrapper below cmux and above section timing. Keeping
// cmux outermost preserves its optional rate-limit reporting methods.
func buildRunnerLogger(rep *cmux.Reporter, titles *orca.Reporter, inner progress.SectionLogger) (processor.Logger, *progress.SectionTimer) {
	timer := progress.NewSectionTimer(inner, nil)
	return rep.WrapLogger(titles.WrapLogger(timer)), timer
}

// runWithSectionTiming guarantees the final section and aggregate summary are
// written before callers handle the runner result.
func runWithSectionTiming(ctx context.Context, run func(context.Context) error, timer *progress.SectionTimer) error {
	runErr := run(ctx)
	timer.FinishRun()
	return runErr
}

// executePlan runs the main execution loop for a plan file.
// handles progress logging, web dashboard, runner execution, and post-execution tasks.
// when req.ProgressLog and req.PhaseHolder are pre-created (worktree mode), uses them directly.
// when req.MainGitSvc is set, uses it for plan file operations (plan is in main repo).
func executePlan(ctx context.Context, o opts, req executePlanRequest) error {
	branch := getCurrentBranch(req.GitSvc)

	// set up progress logger and phase holder
	plr, err := setupProgressLogger(o, req, branch)
	if err != nil {
		finishOrcaFailure(req.SetupTitles, err)
		return err
	}
	defer plr.closeLog()

	// cmux sidebar and orca terminal-title reporters. Both are nil-safe no-ops when disabled. Stop
	// is also registered with the interrupt handler because defers are skipped on force exit.
	rep := cmux.New(req.PlanFile, cmuxRunModels(o, req.Config, req.ExternalReview))
	titles := startOrcaReporter(req.Config, req.PlanFile, initialOrcaPhase(req.Mode))
	setOrcaCleanup(req.OrcaStop, titles)
	req.SetupTitles.Quiesce()
	var cmuxCleanupOnce sync.Once
	cmuxRetained := false
	completeCmux := func(elapsed string, runErr error) {
		cmuxCleanupOnce.Do(func() {
			finishCmuxAfterCleanup(req, plr, rep, titles, branch, elapsed, runErr)
		})
	}
	defer func() {
		cmuxCleanupOnce.Do(func() {
			stopCmuxAfterCleanup(req, plr, rep, titles)
		})
		if !cmuxRetained {
			rep.Stop()
		}
		titles.Stop()
	}()
	if req.CmuxStop != nil {
		req.CmuxStop.set(func() {
			rep.Stop()
			titles.Stop()
			if req.CmuxPredecessorStop != nil {
				req.CmuxPredecessorStop()
			}
		})
	}
	rep.Start(ctx)
	if req.CmuxHandoff != nil {
		req.CmuxHandoff()
	}

	validationCommands := make([]string, 0)
	if req.PlanFile != "" {
		parsedPlan, parseErr := plan.ParsePlanFile(req.PlanFile)
		if parseErr != nil {
			wrapped := fmt.Errorf("parse plan validation commands: %w", parseErr)
			plr.baseLog.SetFailed(wrapped)
			notifyCmuxCompletion(rep, req.PlanFile, branch, plr.baseLog.Elapsed(), wrapped)
			finishOrcaFailure(titles, wrapped)
			return wrapped
		}
		validationCommands = parsedPlan.ValidationCommands
	}

	// wrap logger with broadcast logger if --serve is enabled
	var runnerLog processor.Logger = plr.baseLog
	if o.Serve {
		params := runHeaderParams(o, req.Config, req.Mode, req.ExternalReview)
		dashboard := web.NewDashboard(web.DashboardConfig{
			BaseLog:         plr.baseLog,
			Port:            o.Port,
			Host:            o.Host,
			PlanFile:        req.PlanFile,
			Branch:          branch,
			RunParams:       web.FormatRunParams(params.Executor, params.PlanModel, params.TaskModel, params.ReviewModel, params.ExternalReview, params.ExternalReviewModel),
			WatchDirs:       o.Watch,
			ConfigWatchDirs: req.Config.WatchDirs,
			Colors:          req.Colors,
		}, plr.holder)
		var dashErr error
		runnerLog, dashErr = dashboard.Start(ctx)
		if dashErr != nil {
			wrapped := fmt.Errorf("start dashboard: %w", dashErr)
			plr.baseLog.SetFailed(wrapped)
			// dashboard startup is still preflight: raise a banner, but let Stop clear every
			// transient artifact rather than leaving a persistent execution-failure pill.
			notifyCmuxCompletion(rep, req.PlanFile, branch, plr.baseLog.Elapsed(), wrapped)
			finishOrcaFailure(titles, wrapped)
			return wrapped
		}
	}
	runnerLog, sectionTimer := buildRunnerLogger(rep, titles, runnerLog)
	validationTimer := progress.NewValidationTimer(validationCommands, runnerLog)

	// subscribe status reporters after the dashboard so all observers coexist
	plr.holder.OnChange(rep.OnPhase)
	plr.holder.OnChange(titles.OnPhase)

	// resolve effective codex model/effort for the banner so it reflects what
	// the codex task and review executors actually receive (--task-model /
	// --review-model resolved against codex_model / codex_reasoning_effort).
	// only under the codex executor — in claude mode the banner codex lines are
	// not shown and the max-effort warning would be a false positive (max is a
	// valid claude effort).
	var codex codexBannerInfo
	if req.Config.Executor == config.ExecutorCodex {
		codex = codexModelBanner(o, req.Config)
	}

	// print startup info
	printStartupInfo(startupInfo{
		PlanFile:                req.PlanFile,
		Branch:                  branch,
		Mode:                    req.Mode,
		MaxIterations:           resolveMaxIterations(o.MaxIterations, req.Config),
		ProgressPath:            plr.baseLog.Path(),
		Executor:                req.Config.Executor,
		PassClaudeMd:            req.Config.PassClaudeMd,
		PreserveAnthropicAPIKey: req.Config.PreserveAnthropicAPIKey,
		CodexModel:              codex.taskModel,
		CodexEffort:             codex.taskEffort,
		CodexReviewModel:        codex.reviewModel,
		CodexReviewEffort:       codex.reviewEffort,
		CodexSandbox:            req.Config.CodexExecutorSandbox(),
		ExternalReview:          req.ExternalReview,
	}, req.Colors)
	if codex.maxDropped {
		req.Colors.Warn().Printf("codex does not support 'max' reasoning effort; ignoring (valid: low, medium, high, xhigh)\n")
	}

	// create and run the runner
	r := createRunner(req, o, runnerLog, plr.holder, validationTimer.Handler())

	// listen for SIGQUIT (Ctrl+\) for manual break during task and review loops
	if breakCh := startBreakSignal(); breakCh != nil {
		r.SetBreakCh(breakCh)
		r.SetPauseHandler(makePauseHandler(os.Stdin, os.Stdout, titles))
	}

	runErr := runWithSectionTiming(ctx, r.Run, sectionTimer)
	validationTimer.FinishRun()
	if runErr != nil {
		// mark logger as failed so Close writes "Failed:" footer, preserving history
		// for restart. Applies to ErrUserAborted too — user aborts are not completions.
		// abort keeps the raw error in the footer (self-descriptive); real failures
		// use the wrapped error so the footer matches what the caller sees.
		if errors.Is(runErr, processor.ErrUserAborted) {
			plr.baseLog.SetFailed(runErr)
			fmt.Fprintln(os.Stderr, "aborted by user, plan left in place")
			return nil
		}
		wrapped := fmt.Errorf("runner: %w", runErr)
		plr.baseLog.SetFailed(wrapped)
		sendNotification(req, branch, plr.baseLog.Elapsed(), git.DiffStats{}, runErr)
		completeCmux(plr.baseLog.Elapsed(), runErr)
		return wrapped
	}

	elapsed := plr.baseLog.Elapsed()

	// get diff stats for completion message (optional - errors logged but don't block).
	// use worktree GitSvc (has correct HEAD with committed changes).
	stats, statsErr := req.GitSvc.DiffStats(req.BaseRef)
	if statsErr != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to get diff stats: %v\n", statsErr)
	}

	// Move completed plans before capturing the branch tip so archival is part of the stack. A
	// single worktree run retains its historical source-checkout archival behavior; a chain archives
	// inside each execution worktree so the source checkout stays untouched between plans and the
	// final branch contains every completed-plan move.
	// track actual success so the completion summary reflects where the plan really lives.
	planMoved, moveErr := moveCompletedPlan(req)
	if moveErr != nil {
		plr.baseLog.SetFailed(moveErr)
		sendNotification(req, branch, elapsed, stats, moveErr)
		completeCmux(elapsed, moveErr)
		return moveErr
	}
	sendNotification(req, branch, elapsed, stats, nil)

	displayStats(req, plr.baseLog, stats, elapsed, branch, planMoved)
	if outcomeErr := capturePlanOutcome(req); outcomeErr != nil {
		return outcomeErr
	}
	if req.CmuxRetain != nil {
		cmuxCleanupOnce.Do(func() {
			retainCmuxAfterCleanup(req, plr, rep)
			cmuxRetained = true
			req.CmuxRetain(retainedCmuxRun{reporter: rep, planFile: req.PlanFile, branch: branch, elapsed: elapsed})
		})
	} else {
		completeCmux(elapsed, nil)
	}

	// clear the sidebar before the dashboard idles: with --serve the run is done but the process
	// stays alive until Ctrl+C, and a spinner left spinning reports it as still working
	stopCmuxUnlessRetained(rep, cmuxRetained)
	titles.Stop()
	keepDashboardAlive(ctx, o, req, plr.closeLog)
	if req.Outcome != nil {
		req.Outcome.succeeded = true
	}

	return nil
}

func stopCmuxUnlessRetained(rep *cmux.Reporter, retained bool) {
	if !retained {
		rep.Stop()
	}
}

func moveCompletedPlan(req executePlanRequest) (bool, error) {
	if !shouldMovePlan(req) {
		return false, nil
	}
	moveSvc := req.GitSvc
	movePlanFile := req.PlanFile
	chainRun := len(req.ChainPlanFiles) > 1
	if req.MainGitSvc != nil && !chainRun {
		moveSvc = req.MainGitSvc
	}
	if req.MainPlanFile != "" && !chainRun {
		movePlanFile = req.MainPlanFile
	}
	if err := moveSvc.MovePlanToCompleted(movePlanFile); err != nil {
		if chainRun {
			return false, fmt.Errorf("archive completed chain plan: %w", err)
		}
		fmt.Fprintf(os.Stderr, "warning: failed to move plan to completed: %v\n", err)
		return false, nil
	}
	return true, nil
}

func capturePlanOutcome(req executePlanRequest) error {
	if req.Outcome == nil {
		return nil
	}
	branchTip, err := req.GitSvc.HeadHash()
	if err != nil {
		return fmt.Errorf("capture completed plan tip: %w", err)
	}
	req.Outcome.branchTip = branchTip
	return nil
}

// stopCmuxAfterCleanup removes a non-final pill only after every operation that must stay isolated
// from a subsequent local auto run. It covers preflight failures and user aborts, which deliberately
// do not publish a persistent completion pill.
func stopCmuxAfterCleanup(req executePlanRequest, plr progressLogResult, rep *cmux.Reporter, titles *orca.Reporter) {
	rep.Quiesce()
	plr.closeLog()
	if req.BeforeCmuxFinish != nil {
		req.BeforeCmuxFinish(false)
	}
	rep.Stop()
	titles.Stop()
}

// finishCmuxAfterCleanup publishes the final/free pill only after every operation that must stay
// isolated from a subsequent local auto run. Normal runs close their owned log here; worktree runs
// additionally restore cwd, remove or preserve the worktree as appropriate, and close their
// externally-owned log through BeforeCmuxFinish. A successful --serve run keeps its worktree
// until the dashboard exits because /api/plan continues to read the worktree plan.
func finishCmuxAfterCleanup(
	req executePlanRequest,
	plr progressLogResult,
	rep *cmux.Reporter,
	titles *orca.Reporter,
	branch, elapsed string,
	runErr error,
) {
	rep.Quiesce()
	plr.closeLog()
	if req.BeforeCmuxFinish != nil {
		req.BeforeCmuxFinish(runErr == nil)
	}
	finishCmuxCompletion(rep, titles, req.PlanFile, branch, elapsed, runErr)
}

// retainCmuxAfterCleanup preserves the non-final busy pill while the chain coordinator checkpoints
// the completed member and prepares its successor. Ownership transfers only after the next reporter
// has started, eliminating the free-workspace gap between dependent plans.
func retainCmuxAfterCleanup(req executePlanRequest, plr progressLogResult, rep *cmux.Reporter) {
	rep.Quiesce()
	plr.closeLog()
	if req.BeforeCmuxFinish != nil {
		req.BeforeCmuxFinish(true)
	}
}

// runWithWorktree creates or resumes a worktree, creates the progress logger (before chdir so it
// lands in the main repo), chdirs into the worktree, and runs executePlan. New worktrees are
// cleaned up on every return. Resumed worktrees are cleaned up only on success so interrupted
// work is preserved for another retry. req.WtCleanup is populated for interrupt handler use.
func runWithWorktree(ctx context.Context, o opts, req executePlanRequest) (err error) {
	// derived from the plan file, so it is known before the worktree exists — both the progress
	// logger below and the setup failure banner need it.
	branch := req.GitSvc.EffectiveBranchName(req.PlanFile, req.BranchOverride)

	// everything up to executePlan runs before the downstream reporter exists, so a setup failure
	// would be invisible in cmux: the plan-mode handoff has already stopped its own reporter and a
	// direct run never had one. notify on those errors only — once executePlan is entered it owns
	// its own reporting, and notifying on its return value would double-banner every run failure.
	// elapsed is empty on purpose: the failure notice does not use it and the log may not exist yet.
	handedOff := false
	defer func() {
		if err != nil && !handedOff {
			notifyCmuxCompletion(cmux.New(req.PlanFile, cmuxRunModels(o, req.Config, req.ExternalReview)), req.PlanFile, branch, "", err)
			finishOrcaFailure(req.SetupTitles, err)
		}
	}()

	wt, err := prepareWorktreeRunContext(ctx, o, req, branch)
	if err != nil {
		return err
	}
	releaseRunLock := wt.releaseRunLock
	defer releaseRunLock()

	var removeOnce sync.Once
	removeWorktree := func() {
		removeOnce.Do(func() {
			removeWorktreeWithRunLock(req.GitSvc, wt.path, releaseRunLock)
		})
	}
	setupCleanup := func(remove bool) {
		if remove {
			removeWorktree()
			return
		}
		releaseRunLock()
	}
	if wt.resumed {
		// A forced exit must not explicitly release a resumed worktree while execution may still
		// be active. The operating system releases the advisory lock when os.Exit terminates us.
		req.WtCleanup.set(nil)
	} else {
		req.WtCleanup.set(func() { setupCleanup(true) })
	}

	// Safety net for errors before cwd-aware cleanup is registered below. Fresh worktrees are
	// removed while ownership is transferred under the repository coordination lock; resumed
	// worktrees are preserved and only release their run lock.
	setupDone := false
	defer func() {
		if !setupDone {
			setupCleanup(!wt.resumed)
		}
	}()
	if preparedErr := recordPreparedChainBranch(req, branch); preparedErr != nil {
		return preparedErr
	}

	if igErr := req.GitSvc.EnsureLocalGitignore(); igErr != nil {
		fmt.Fprintf(os.Stderr, "warning: gitignore setup: %v\n", igErr)
	}

	origDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("get working directory: %w", err)
	}

	// create progress logger BEFORE chdir so progress files land in main repo's .loopai/progress/.
	// uses the branch name derived from the plan file above, since gitSvc still points at the main
	// repo (on master). Its exclusive file lock also rejects a live run using the same progress path.
	holder := &status.PhaseHolder{}
	baseLog, err := progress.NewLogger(progress.Config{
		PlanFile:       req.PlanFile,
		Mode:           string(req.Mode),
		Branch:         branch,
		BranchOverride: req.BranchOverride,
		Params:         runHeaderParams(o, req.Config, req.Mode, req.ExternalReview),
		NoColor:        o.NoColor,
	}, req.Colors, holder)
	if err != nil {
		return fmt.Errorf("create progress logger: %w", err)
	}
	var closeLogOnce sync.Once
	closeLog := func() {
		closeLogOnce.Do(func() {
			// mark failure on any error return so Close writes "Failed:" instead of "Completed:",
			// preserving progress history across restart (issue #288)
			if err != nil {
				baseLog.SetFailed(err)
			}
			if closeErr := baseLog.Close(); closeErr != nil {
				fmt.Fprintf(os.Stderr, "warning: failed to close progress log: %v\n", closeErr)
			}
		})
	}
	defer closeLog()

	// chdir into worktree
	if err = os.Chdir(wt.path); err != nil {
		return fmt.Errorf("chdir to worktree: %w", err)
	}

	// register cleanup. New worktrees retain the existing remove-on-any-exit behavior. Resumed
	// worktrees are removed only after success; failure or interruption leaves them available for
	// another auto-resume run.
	var restoreOnce sync.Once
	restoreCWD := func() {
		restoreOnce.Do(func() {
			if chdirErr := os.Chdir(origDir); chdirErr != nil {
				fmt.Fprintf(os.Stderr, "warning: failed to restore working directory: %v\n", chdirErr)
			}
		})
	}
	cleanup := func(remove bool) {
		restoreCWD()
		setupCleanup(remove)
	}
	finishState := worktreeFinishState{cleanup: worktreeCmuxFinishCleanup(
		wt.resumed, o.Serve, restoreCWD, cleanup, closeLog,
		func() {
			// During execution, resumed worktrees must survive interruption. Once execution has
			// succeeded and only the dashboard is alive, force-exit cleanup may remove it too.
			req.WtCleanup.set(func() { cleanup(true) })
		},
	)}

	setupDone = true // disable safety-net defer, main cleanup takes over
	if wt.resumed {
		// Restore process-global cwd on forced exit, but retain run ownership until process death.
		// Normal-return cleanup below explicitly releases the lock after execution has stopped.
		req.WtCleanup.set(restoreCWD)
		defer func() {
			// executePlan deliberately returns nil for a user abort. Use its internal completion
			// outcome instead of the public return value so aborted work remains resumable.
			if finishState.succeeded {
				cleanup(true)
				return
			}
			cleanup(false)
			fmt.Fprintf(os.Stderr, "worktree preserved for resume: %s\n", wt.path)
		}()
	} else {
		req.WtCleanup.set(func() { cleanup(true) })
		defer cleanup(true)
	}

	// setup is done: executePlan creates its own reporter and owns every error from here on
	handedOff = true

	return executePlan(ctx, o, executePlanRequest{
		PlanFile:            wt.planFile,
		MainPlanFile:        req.PlanFile, // original path in main repo for MovePlanToCompleted
		Mode:                req.Mode,
		GitSvc:              wt.gitSvc,
		MainGitSvc:          req.GitSvc,
		Config:              req.Config,
		Colors:              req.Colors,
		DefaultBranch:       req.DefaultBranch,
		BaseRef:             req.BaseRef,
		NotifySvc:           req.NotifySvc,
		CmuxStop:            req.CmuxStop,
		OrcaStop:            req.OrcaStop,
		CmuxHandoff:         req.CmuxHandoff,
		CmuxPredecessorStop: req.CmuxPredecessorStop,
		CmuxRetain:          req.CmuxRetain,
		SetupTitles:         req.SetupTitles,
		BeforeCmuxFinish:    finishState.beforeCmuxFinish,
		Outcome:             req.Outcome,
		ChainSuccessor:      req.ChainSuccessor,
		ChainResume:         req.ChainResume,
		ChainResumePrepared: req.ChainResumePrepared,
		ChainPlanFiles:      req.ChainPlanFiles,
		ChainPrepared:       req.ChainPrepared,
		WorktreeStartRef:    req.WorktreeStartRef,
		ProgressLog:         baseLog,
		PhaseHolder:         holder,
		ExternalReview:      req.ExternalReview,
		LimitRecovery:       req.LimitRecovery,
	})
}

// worktreeFinishState keeps repository cleanup keyed to execution completion rather than the
// public executePlan error. User abort is intentionally a successful CLI exit, but it is not a
// completed execution and a resumed worktree must remain available for retry.
type worktreeFinishState struct {
	succeeded bool
	cleanup   func(bool)
}

func (s *worktreeFinishState) beforeCmuxFinish(success bool) {
	s.succeeded = success
	s.cleanup(success)
}

func worktreeCmuxFinishCleanup(
	resumed, serve bool,
	restoreCWD func(), cleanup func(remove bool), closeLog, enableDeferredCleanup func(),
) func(bool) {
	return func(success bool) {
		restoreCWD()
		// Successful --serve runs remain blocked in keepDashboardAlive after this callback. Keep the
		// plan-bearing worktree available to /api/plan; runWithWorktree's existing defer removes it
		// when the dashboard exits. Failures never enter that idle period and retain normal cleanup.
		if !serve || !success {
			cleanup(!resumed || success)
		}
		closeLog()
		if serve && success {
			enableDeferredCleanup()
		}
	}
}

type worktreeRun struct {
	path            string
	planFile        string
	gitSvc          *git.Service
	planNeedsCommit bool
	resumed         bool
	releaseRunLock  func()
}

func prepareWorktreeRunContext(
	ctx context.Context, o opts, req executePlanRequest, branch string,
) (wt worktreeRun, err error) {
	releaseCreationLock, err := req.GitSvc.AcquireWorktreeCreationLockContext(ctx)
	if err != nil {
		return worktreeRun{}, fmt.Errorf("lock worktree preparation: %w", err)
	}
	createdPath := ""
	var releaseOnError func()
	worktreePath := filepath.Join(req.GitSvc.Root(), ".loopai", "worktrees", branch)
	markerActive := false
	clearPreparationMarker := func() error {
		if !markerActive {
			return nil
		}
		if clearErr := req.GitSvc.ClearWorktreePreparation(worktreePath); clearErr != nil {
			return fmt.Errorf("clear worktree preparation: %w", clearErr)
		}
		markerActive = false
		return nil
	}
	defer func() {
		err = finalizeWorktreePreparation(err, createdPath, releaseOnError, releaseCreationLock,
			req.GitSvc.RemoveWorktree, clearPreparationMarker,
			func() { removeWorktreeWithRunLock(req.GitSvc, createdPath, releaseOnError) })
	}()

	markerActive, err = recoverInterruptedWorktreePreparationLocked(req.GitSvc, worktreePath)
	if err != nil {
		return worktreeRun{}, err
	}
	if !markerActive {
		if _, statErr := os.Lstat(worktreePath); os.IsNotExist(statErr) || errors.Is(statErr, syscall.ENOTDIR) {
			if markErr := req.GitSvc.MarkWorktreePreparation(worktreePath); markErr != nil {
				return worktreeRun{}, fmt.Errorf("mark worktree preparation: %w", markErr)
			}
			markerActive = true
		}
	}

	wt, createdPath, err = prepareWorktreeTargetLocked(ctx, o, req, branch)
	if err != nil {
		return worktreeRun{}, err
	}
	wt, err = openAndLockWorktreeRun(ctx, req, wt)
	if err != nil {
		return worktreeRun{}, err
	}
	releaseOnError = wt.releaseRunLock
	// Committing an untracked/modified plan is part of fresh initialization. Keep both the durable
	// marker and shared preparation lock until it finishes so a crash cannot turn a partially
	// staged or uncommitted checkout into an auto-resume target.
	if commitErr := commitPreparedWorktreePlans(wt, req); commitErr != nil {
		return worktreeRun{}, commitErr
	}
	if !wt.resumed && wt.planNeedsCommit {
		wt.planNeedsCommit = false
	}
	if clearErr := clearPreparationMarker(); clearErr != nil {
		return worktreeRun{}, fmt.Errorf("complete worktree preparation: %w", clearErr)
	}
	if wt.resumed {
		// WtCleanup is only for the forced os.Exit path. Keep the lock held until process death;
		// preparation's normal error paths still release it through the defer above.
		req.WtCleanup.set(nil)
	} else {
		req.WtCleanup.set(func() {
			removeWorktreeWithRunLock(req.GitSvc, wt.path, wt.releaseRunLock)
		})
	}

	if !wt.resumed {
		return wt, nil
	}
	if validationErr := validateResumeWorktree(ctx, req.GitSvc, wt, branch, req.WorktreeStartRef); validationErr != nil {
		return worktreeRun{}, validationErr
	}
	if o.Commit {
		req.Colors.Warn().Printf("warning: -c/--commit is ignored when resuming interrupted worktree %s\n", wt.path)
	}
	req.Colors.Info().Printf("resuming interrupted worktree %s\n", wt.path)
	return wt, nil
}

func commitPreparedWorktreePlans(wt worktreeRun, req executePlanRequest) error {
	if wt.resumed || !wt.planNeedsCommit {
		return nil
	}
	planFiles := []string{req.PlanFile}
	if len(req.ChainPlanFiles) > 1 && !req.ChainSuccessor {
		planFiles = req.ChainPlanFiles
	}
	if err := wt.gitSvc.CommitPlanFiles(planFiles, req.GitSvc.Root()); err != nil {
		return fmt.Errorf("commit plan in worktree: %w", err)
	}
	return nil
}

func recoverInterruptedWorktreePreparationLocked(gitSvc *git.Service, path string) (bool, error) {
	interrupted, err := gitSvc.HasWorktreePreparationMarker(path)
	if err != nil {
		return false, fmt.Errorf("inspect interrupted worktree preparation: %w", err)
	}
	if !interrupted {
		return false, nil
	}
	if err := gitSvc.RemoveWorktree(path); err != nil {
		// Keep the marker so the next invocation retries recovery instead of treating the partial
		// target as a resumable worktree.
		return false, fmt.Errorf("recover interrupted worktree preparation: %w", err)
	}
	if _, err := os.Lstat(path); err == nil || !os.IsNotExist(err) {
		return false, fmt.Errorf("recover interrupted worktree preparation: target still exists at %s", path)
	}
	return true, nil
}

func rollbackWorktreePreparationLocked(
	preparationErr error, createdPath string, releaseRunLock func(), removeWorktree func(string) error,
) (result error, cleanupComplete bool) {
	if releaseRunLock != nil {
		releaseRunLock()
	}
	if createdPath == "" {
		return preparationErr, true
	}
	if removeErr := removeWorktree(createdPath); removeErr != nil {
		return errors.Join(preparationErr,
			fmt.Errorf("remove worktree after preparation error: %w", removeErr)), false
	}
	if _, statErr := os.Lstat(createdPath); statErr == nil || !os.IsNotExist(statErr) {
		return errors.Join(preparationErr,
			fmt.Errorf("remove worktree after preparation error: target still exists at %s", createdPath)), false
	}
	return preparationErr, true
}

func finalizeWorktreePreparation(
	preparationErr error,
	createdPath string,
	releaseRunLock func(),
	releasePreparationLock func() error,
	removeWorktreeLocked func(string) error,
	clearPreparationMarker func() error,
	removeWorktreeAfterUnlock func(),
) error {
	result := preparationErr
	clearMarker := true
	if preparationErr != nil {
		result, clearMarker = rollbackWorktreePreparationLocked(
			preparationErr, createdPath, releaseRunLock, removeWorktreeLocked,
		)
	}
	if clearMarker {
		if markerErr := clearPreparationMarker(); markerErr != nil {
			result = errors.Join(result, fmt.Errorf("clear worktree preparation marker: %w", markerErr))
		}
	}
	releaseErr := releasePreparationLock()
	if releaseErr == nil {
		return result
	}
	if preparationErr == nil {
		if createdPath != "" {
			removeWorktreeAfterUnlock()
		} else if releaseRunLock != nil {
			releaseRunLock()
		}
	}
	return errors.Join(result, releaseErr)
}

func prepareWorktreeTargetLocked(
	ctx context.Context, o opts, req executePlanRequest, branch string,
) (wt worktreeRun, createdPath string, err error) {
	wt.path = filepath.Join(req.GitSvc.Root(), ".loopai", "worktrees", branch)
	_, statErr := os.Lstat(wt.path)
	switch {
	case statErr == nil:
		wt.resumed = true
		if resumeErr := requireResumeWorktree(wt.path); resumeErr != nil {
			return worktreeRun{}, "", resumeErr
		}
		return wt, "", nil
	case os.IsNotExist(statErr), errors.Is(statErr, syscall.ENOTDIR):
		// The marker makes this expected path ours to clean up even if the creation API reports an
		// error without returning the path after Git has already materialized part of the checkout.
		createdPath = wt.path
		wt.path, wt.planNeedsCommit, err = prepareFreshWorktree(ctx, o, req, branch)
		if err != nil {
			return worktreeRun{}, createdPath, err
		}
		return wt, wt.path, nil
	default:
		return worktreeRun{}, "", fmt.Errorf("inspect plan worktree %s: %w", wt.path, statErr)
	}
}

func openAndLockWorktreeRun(
	ctx context.Context, req executePlanRequest, wt worktreeRun,
) (worktreeRun, error) {
	wtSvc, err := req.GitSvc.OpenWorktree(wt.path)
	if err != nil {
		if wt.resumed {
			if errors.Is(err, git.ErrNotSameRepository) {
				return worktreeRun{}, fmt.Errorf("resume worktree: %s is not a registered git worktree: %w", wt.path, err)
			}
			return worktreeRun{}, fmt.Errorf("resume worktree: open existing worktree: %w", err)
		}
		return worktreeRun{}, fmt.Errorf("open worktree git service: %w", err)
	}
	wtSvc.SetCommitTrailer(req.Config.CommitTrailer)
	wt.gitSvc = wtSvc
	wt.planFile = resolveWorktreePlanFile(req.PlanFile, req.GitSvc.Root(), wtSvc.Root())

	release, lockErr := wtSvc.AcquireWorktreeRunLockContext(ctx)
	if lockErr != nil {
		if wt.resumed {
			var busyErr *git.ErrWorktreeBusy
			if errors.As(lockErr, &busyErr) {
				return worktreeRun{}, busyErr
			}
			return worktreeRun{}, fmt.Errorf("resume worktree: acquire run lock: %w", lockErr)
		}
		return worktreeRun{}, fmt.Errorf("acquire run lock for newly created worktree: %w", lockErr)
	}
	var releaseLockOnce sync.Once
	wt.releaseRunLock = func() {
		releaseLockOnce.Do(func() {
			if releaseErr := release(); releaseErr != nil {
				fmt.Fprintf(os.Stderr, "warning: failed to release worktree run lock: %v\n", releaseErr)
			}
		})
	}
	if validationErr := wtSvc.ValidatePlanFile(wt.planFile); validationErr != nil {
		wt.releaseRunLock()
		return worktreeRun{}, fmt.Errorf("validate plan inside worktree: %w", validationErr)
	}
	return wt, nil
}

// prepareWorktreeSource optionally commits the source checkout before a fresh worktree is cut.
func prepareWorktreeSource(o opts, req executePlanRequest, branch string) (bool, error) {
	if !o.Commit {
		return false, nil
	}
	committed, err := req.GitSvc.AutoCommitAll("auto-commit working tree before plan: " + branch)
	if err != nil {
		return false, fmt.Errorf("auto-commit working tree: %w", err)
	}
	if committed {
		req.Colors.Info().Printf("auto-committed working tree before creating branch: %s\n", branch)
		return true, nil
	}
	req.Colors.Info().Printf("working tree clean; no auto-commit needed before creating branch: %s\n", branch)
	return false, nil
}

// prepareFreshWorktree mutates the source and creates the worktree while its caller holds the
// repository worktree-preparation lock.
func prepareFreshWorktree(ctx context.Context, o opts, req executePlanRequest, branch string) (path string, planNeedsCommit bool, err error) {
	if o.Commit && req.WorktreeStartRef != "" {
		return "", false, errors.New("source auto-commit cannot be combined with an explicit worktree start ref")
	}
	if preflightErr := req.GitSvc.PreflightWorktreeForPlanFromRefContext(
		ctx, req.PlanFile, req.BranchOverride, req.WorktreeStartRef,
	); preflightErr != nil {
		return "", false, fmt.Errorf("preflight worktree creation: %w", preflightErr)
	}
	if len(req.ChainPlanFiles) > 1 {
		if chainErr := req.GitSvc.ValidatePlanChain(req.ChainPlanFiles); chainErr != nil {
			return "", false, fmt.Errorf("preflight plan chain: %w", chainErr)
		}
	}

	// Install runtime ignores while holding the repository lock so another fresh run
	// cannot observe this run's worktree directory as an untracked source change.
	// This must also precede AutoCommitAll so runtime artifacts are never staged.
	if ignoreErr := req.GitSvc.EnsureLocalGitignore(); ignoreErr != nil {
		return "", false, fmt.Errorf("ensure gitignore before worktree creation: %w", ignoreErr)
	}
	// Ignore installation can change whether an untracked plan is visible to Git,
	// particularly for plans placed under loopai's reserved runtime directories.
	// Revalidate before an auto-commit is allowed to advance the source checkout.
	if preflightErr := req.GitSvc.PreflightWorktreeForPlanFromRefContext(
		ctx, req.PlanFile, req.BranchOverride, req.WorktreeStartRef,
	); preflightErr != nil {
		return "", false, fmt.Errorf("preflight worktree creation after ignore setup: %w", preflightErr)
	}
	if len(req.ChainPlanFiles) > 1 {
		if chainErr := req.GitSvc.ValidatePlanChain(req.ChainPlanFiles); chainErr != nil {
			return "", false, fmt.Errorf("preflight plan chain after ignore setup: %w", chainErr)
		}
	}
	if o.Commit {
		if preflightErr := req.GitSvc.PreflightWorktreeForPlanAutoCommitContext(ctx, req.PlanFile, req.BranchOverride); preflightErr != nil {
			return "", false, fmt.Errorf("preflight source auto-commit: %w", preflightErr)
		}
	}
	sourceHeadBefore := ""
	if o.Commit {
		sourceHeadBefore, err = req.GitSvc.HeadHash()
		if err != nil {
			return "", false, fmt.Errorf("identify source HEAD before auto-commit: %w", err)
		}
	}
	committed, sourceErr := prepareWorktreeSource(o, req, branch)
	if sourceErr != nil {
		return "", false, sourceErr
	}
	if committed {
		path, planNeedsCommit, err = req.GitSvc.CreateWorktreeForPlanAfterAutoCommit(
			ctx, req.PlanFile, req.BranchOverride, sourceHeadBefore)
	} else {
		path, planNeedsCommit, err = createUncommittedPlanWorktree(ctx, req)
	}
	if err != nil {
		return "", false, fmt.Errorf("create worktree: %w", err)
	}
	return path, planNeedsCommit, nil
}

func createUncommittedPlanWorktree(ctx context.Context, req executePlanRequest) (string, bool, error) {
	var path string
	var needsCommit bool
	var err error
	switch {
	case len(req.ChainPlanFiles) <= 1:
		path, needsCommit, err = req.GitSvc.CreateWorktreeForPlanFromRefContext(
			ctx, req.PlanFile, req.BranchOverride, req.WorktreeStartRef,
		)
	case req.ChainResumePrepared && !req.ChainSuccessor:
		path, needsCommit, err = req.GitSvc.CreateWorktreeForResumedPlanChainContext(
			ctx, req.PlanFile, req.BranchOverride, req.WorktreeStartRef, req.ChainPlanFiles,
		)
	default:
		path, needsCommit, err = req.GitSvc.CreateWorktreeForPlanChainContext(
			ctx, req.PlanFile, req.BranchOverride, req.WorktreeStartRef, req.ChainPlanFiles,
		)
	}
	if err != nil {
		return "", false, fmt.Errorf("prepare plan worktree target: %w", err)
	}
	return path, needsCommit, nil
}

// removeWorktreeWithRunLock serializes the ownership handoff with worktree classification. The
// run lock must be released before Git removes the private metadata on Windows, while the shared
// preparation lock prevents another process from acquiring the run lock in that interval.
func removeWorktreeWithRunLock(gitSvc *git.Service, path string, releaseRunLock func()) {
	removeWorktreeWithRunLockContext(context.Background(), gitSvc, path, releaseRunLock)
}

func removeWorktreeWithRunLockContext(
	ctx context.Context, gitSvc *git.Service, path string, releaseRunLock func(),
) {
	releasePreparationLock, err := gitSvc.AcquireWorktreeCreationLockContext(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to lock worktree removal: %v\n", err)
		// Do not expose an unlocked, unremoved checkout. Normal cleanup waits with a background
		// context; a bounded force-exit caller leaves ownership to the OS when the process dies.
		return
	}
	defer func() {
		if releaseErr := releasePreparationLock(); releaseErr != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to release worktree removal lock: %v\n", releaseErr)
		}
	}()

	releaseRunLock()
	if rmErr := gitSvc.RemoveWorktree(path); rmErr != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to remove worktree: %v\n", rmErr)
	}
}

func requireResumeWorktree(path string) error {
	_, err := os.Stat(path)
	if err == nil {
		return nil
	}
	if os.IsNotExist(err) {
		return fmt.Errorf("resume worktree: worktree does not exist at %s", path)
	}
	return fmt.Errorf("resume worktree: stat %s: %w", path, err)
}

func validateResumeWorktree(
	ctx context.Context, sourceGitSvc *git.Service, wt worktreeRun, expectedBranch, requiredStart string,
) error {
	resolvedPath, err := filepath.EvalSymlinks(wt.path)
	if err != nil {
		return fmt.Errorf("resume worktree: resolve path: %w", err)
	}
	rootRel, relErr := filepath.Rel(resolvedPath, wt.gitSvc.Root())
	if relErr != nil || rootRel != "." {
		return fmt.Errorf("resume worktree: %s is not a registered git worktree", wt.path)
	}
	worktrees, err := sourceGitSvc.Worktrees()
	if err != nil {
		return fmt.Errorf("resume worktree: list registered worktrees: %w", err)
	}
	registered := false
	for _, candidate := range worktrees {
		if sameProgressRoot(candidate.Path, resolvedPath) && !sameProgressRoot(candidate.Path, sourceGitSvc.Root()) {
			registered = true
			break
		}
	}
	if !registered {
		return fmt.Errorf("resume worktree: %s is not a registered git worktree", wt.path)
	}

	branch, err := wt.gitSvc.CurrentBranch()
	if err != nil {
		return fmt.Errorf("resume worktree: determine branch: %w", err)
	}
	if branch != expectedBranch {
		return fmt.Errorf("resume worktree: expected branch %q, found %q at %s", expectedBranch, branch, wt.path)
	}
	if requiredStart != "" {
		containsStart, containsErr := wt.gitSvc.ContainsRevisionContext(ctx, requiredStart)
		if containsErr != nil {
			return fmt.Errorf("resume worktree: validate predecessor tip: %w", containsErr)
		}
		if !containsStart {
			return fmt.Errorf("resume worktree: branch %q does not contain required predecessor tip %s; remove the stale worktree and rerun the chain",
				expectedBranch, requiredStart)
		}
	}

	planInfo, err := os.Stat(wt.planFile)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("resume worktree: plan file not found inside worktree: %s", wt.planFile)
		}
		return fmt.Errorf("resume worktree: stat plan file: %w", err)
	}
	if planInfo.IsDir() {
		return fmt.Errorf("resume worktree: plan path is a directory: %s", wt.planFile)
	}
	return nil
}

// resolveWorktreePlanFile maps an absolute plan path from the main repo into a worktree.
// It resolves symlinks on the plan path to match the repo root (macOS: /tmp -> /private/tmp),
// then makes the path relative to the main root and joins it to the worktree root.
// Falls back to the original path if any step fails or the path is not absolute.
func resolveWorktreePlanFile(planFile, repoRoot, worktreeRoot string) string {
	if !filepath.IsAbs(planFile) {
		return planFile
	}
	resolved := planFile
	if r, err := filepath.EvalSymlinks(resolved); err == nil {
		resolved = r
	}
	rel, err := filepath.Rel(repoRoot, resolved)
	if err != nil {
		return planFile
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return planFile
	}
	return filepath.Join(worktreeRoot, rel)
}

// openGitService creates a git.Service for the current directory.
// vcsCmd specifies the vcs command to use (e.g. "git" or path to a wrapper script).
func openGitService(colors *progress.Colors, vcsCmd string) (*git.Service, error) {
	svc, err := git.NewService(".", colors.Info(), vcsCmd)
	if err != nil {
		return nil, fmt.Errorf("new git service: %w", err)
	}
	return svc, nil
}

// checkClaudeDep checks that the claude command is available in PATH.
func checkClaudeDep(cfg *config.Config) error {
	claudeCmd := cfg.ClaudeCommand
	if claudeCmd == "" {
		claudeCmd = "claude"
	}
	if _, err := exec.LookPath(claudeCmd); err != nil {
		return fmt.Errorf("%s not found in PATH; install Claude Code or set claude_command in config to a compatible CLI", claudeCmd)
	}
	return nil
}

// checkCodexDep checks that the codex command is available in PATH.
// used when executor=codex (--codex) so codex absence is reported up-front
// with a clean message rather than a cryptic exec error on the first task.
func checkCodexDep(cfg *config.Config) error {
	codexCmd := cfg.CodexCommand
	if codexCmd == "" {
		codexCmd = "codex"
	}
	if _, err := exec.LookPath(codexCmd); err != nil {
		return fmt.Errorf("%s not found in PATH; install the codex CLI or set codex_command in config", codexCmd)
	}
	return nil
}

type resolvedReviewer struct {
	Provider   string
	Model      string
	Effort     string
	MaxDropped bool
}

func (r resolvedReviewer) modelSpec() string {
	switch {
	case r.Model == "" && r.Effort == "":
		return ""
	case r.Effort == "":
		return r.Model
	default:
		return r.Model + ":" + r.Effort
	}
}

// externalReviewSelection is the resolved ordered external-review chain used
// by dependency checks, startup metadata, and processor setup. Reviewers always
// contain concrete providers; legacy auto/none state is kept as selection metadata.
type externalReviewSelection struct {
	Reviewers              []resolvedReviewer
	Resolved               bool
	AutoSelected           bool
	Explicit               bool
	DisabledByCodexEnabled bool
	DisabledByMissing      bool
}

func (s externalReviewSelection) modelSpec() string {
	if reviewer, ok := s.firstReviewer(); ok {
		return reviewer.modelSpec()
	}
	return ""
}

// chainLabel formats the complete external-review sequence for metadata that
// can represent a reviewer chain in one field. Empty selections retain the
// legacy "none" label; callers that previously omitted disabled review should
// continue to gate on the selection state before using it.
func (s externalReviewSelection) chainLabel() string {
	if len(s.Reviewers) == 0 {
		return config.ExternalReviewToolNone
	}

	labels := make([]string, 0, len(s.Reviewers))
	for _, reviewer := range s.Reviewers {
		label := reviewer.Provider
		if model := reviewer.modelSpec(); model != "" {
			label += " (" + model + ")"
		}
		labels = append(labels, label)
	}
	return strings.Join(labels, " → ")
}

func (s externalReviewSelection) firstReviewer() (resolvedReviewer, bool) {
	if len(s.Reviewers) == 0 {
		return resolvedReviewer{}, false
	}
	return s.Reviewers[0], true
}

func (s externalReviewSelection) providerLabel() string {
	if s.DisabledByCodexEnabled {
		return config.ExternalReviewToolNone + " (auto disabled by codex_enabled=false)"
	}
	if s.DisabledByMissing {
		return config.ExternalReviewToolNone + " (auto-selected reviewer unavailable)"
	}
	reviewer, ok := s.firstReviewer()
	if !ok {
		return config.ExternalReviewToolNone
	}
	label := reviewer.Provider
	if s.AutoSelected {
		label += " (auto-selected)"
	}
	return label
}

func primaryProvider(cfg *config.Config) string {
	if cfg != nil && cfg.Executor == config.ExecutorCodex {
		return config.ExternalReviewToolCodex
	}
	return config.ExternalReviewToolClaude
}

// resolveExternalReviewSelection applies tool and model defaults after CLI and
// config merging. ModeCodexOnly deliberately bypasses the legacy codex_enabled
// gate because the user explicitly requested the external-review pipeline.
func resolveExternalReviewSelection(o opts, cfg *config.Config, mode processor.Mode) (externalReviewSelection, error) {
	if cfg == nil {
		return externalReviewSelection{Resolved: true}, nil
	}
	// modes that never run a review phase must not require reviewer binaries: --gen-agents
	// only writes agent files, so a configured reviewer missing from PATH is irrelevant to it.
	if mode == processor.ModeTasksOnly || mode == processor.ModeGenAgents {
		return externalReviewSelection{Resolved: true}, nil
	}

	if cfg.ExternalReviewersSet {
		specs, err := config.ParseExternalReviewers(cfg.ExternalReviewers)
		if err != nil {
			return externalReviewSelection{}, fmt.Errorf("parse external_reviewers: %w", err)
		}
		selection := externalReviewSelection{Resolved: true, Explicit: true, Reviewers: make([]resolvedReviewer, 0, len(specs))}
		for _, spec := range specs {
			model, effort, maxDropped := processor.ResolveExternalReviewerModelEffort(
				spec.Provider, spec.ModelSpec, cfg.CodexModel, cfg.CodexReasoningEffort)
			selection.Reviewers = append(selection.Reviewers, resolvedReviewer{
				Provider: spec.Provider, Model: model, Effort: effort, MaxDropped: maxDropped,
			})
		}
		return selection, nil
	}

	requested := cfg.ExternalReviewTool
	if requested == "" {
		requested = config.ExternalReviewToolAuto
	}
	selection := externalReviewSelection{
		Resolved:     true,
		AutoSelected: requested == config.ExternalReviewToolAuto,
		Explicit:     requested != config.ExternalReviewToolAuto,
	}

	if requested == config.ExternalReviewToolAuto {
		if !cfg.CodexEnabled && mode != processor.ModeCodexOnly {
			selection.DisabledByCodexEnabled = true
			return selection, nil
		}
		if primaryProvider(cfg) == config.ExternalReviewToolCodex {
			requested = config.ExternalReviewToolClaude
		} else {
			requested = config.ExternalReviewToolCodex
		}
	}

	modelExplicit := o.externalReviewModelSet || cfg.ExternalReviewModelSet
	if requested == config.ExternalReviewToolCustom && modelExplicit && cfg.ExternalReviewModel != "" {
		return externalReviewSelection{}, errors.New("external_review_model cannot be used with external_review_tool=custom")
	}

	switch requested {
	case config.ExternalReviewToolClaude, config.ExternalReviewToolCodex:
		model, effort, maxDropped := processor.ResolveExternalReviewerModelEffort(
			requested, cfg.ExternalReviewModel, cfg.CodexModel, cfg.CodexReasoningEffort)
		selection.Reviewers = []resolvedReviewer{{Provider: requested, Model: model, Effort: effort, MaxDropped: maxDropped}}
	case config.ExternalReviewToolCustom:
		selection.Reviewers = []resolvedReviewer{{Provider: requested}}
	case config.ExternalReviewToolNone:
		// custom reviewers do not have a provider model; none disables the phase.
	default:
		return externalReviewSelection{}, fmt.Errorf("unsupported external review tool %q", requested)
	}
	return selection, nil
}

func printExternalReviewWarnings(o opts, selection externalReviewSelection, cfg *config.Config, w io.Writer) {
	if w == nil || cfg == nil {
		return
	}
	if cfg.ExternalReviewersSet {
		switch {
		case o.externalReviewToolSet || o.externalReviewModelSet:
			fmt.Fprintln(w, "warning: external_reviewers takes precedence; legacy external-review CLI flags are ignored (use --external-reviewers= to clear or disable the configured chain)")
		case cfg.ExternalReviewToolSet || cfg.ExternalReviewModelSet:
			fmt.Fprintln(w, "warning: external_reviewers takes precedence; legacy external_review_tool and external_review_model config keys are ignored (set external_reviewers = in the more-specific config file to clear or disable the inherited chain)")
		}
	}
	warnedPrimaryMatch := make(map[string]bool)
	maxDroppedWarned := false
	for _, reviewer := range selection.Reviewers {
		if selection.Explicit && reviewer.Provider == primaryProvider(cfg) &&
			(reviewer.Provider == config.ExternalReviewToolClaude || reviewer.Provider == config.ExternalReviewToolCodex) &&
			!warnedPrimaryMatch[reviewer.Provider] {
			fmt.Fprintf(w, "warning: external reviewer %q matches the primary executor; cross-model review signal will be weaker\n", reviewer.Provider)
			warnedPrimaryMatch[reviewer.Provider] = true
		}
		if reviewer.MaxDropped && !maxDroppedWarned {
			fmt.Fprintln(w, "warning: codex does not support 'max' reasoning effort for external review; ignoring (valid: low, medium, high, xhigh)")
			maxDroppedWarned = true
		}
	}
}

// checkExecutionDeps verifies the primary provider and then the selected
// external provider. A missing auto-selected reviewer is the one startup case
// that degrades to no external review; explicit selections remain hard errors.
func checkExecutionDeps(cfg *config.Config, selection externalReviewSelection, warnW io.Writer) (externalReviewSelection, error) {
	var primaryErr error
	if primaryProvider(cfg) == config.ExternalReviewToolCodex {
		primaryErr = checkCodexDep(cfg)
	} else {
		primaryErr = checkClaudeDep(cfg)
	}
	if primaryErr != nil {
		return selection, primaryErr
	}

	checked := make(map[string]bool)
	for _, reviewer := range selection.Reviewers {
		if checked[reviewer.Provider] {
			continue
		}
		checked[reviewer.Provider] = true
		var externalErr error
		switch reviewer.Provider {
		case config.ExternalReviewToolClaude:
			externalErr = checkClaudeDep(cfg)
		case config.ExternalReviewToolCodex:
			externalErr = checkCodexDep(cfg)
		case config.ExternalReviewToolCustom:
			if strings.TrimSpace(cfg.CustomReviewScript) == "" {
				externalErr = errors.New("custom external reviewer requires custom_review_script")
			}
		}
		if externalErr == nil {
			continue
		}
		if !selection.AutoSelected {
			return selection, externalErr
		}
		if warnW != nil {
			fmt.Fprintf(warnW, "warning: automatically selected external reviewer unavailable (%v); disabling external review for this run\n", externalErr)
		}
		selection.Reviewers = nil
		selection.DisabledByMissing = true
		break
	}
	return selection, nil
}

func applyEffectiveExternalReview(cfg *config.Config, selection externalReviewSelection) {
	if cfg == nil {
		return
	}
	reviewer, ok := selection.firstReviewer()
	if !ok {
		cfg.ExternalReviewTool = config.ExternalReviewToolNone
		cfg.ExternalReviewModel = ""
		return
	}
	cfg.ExternalReviewTool = reviewer.Provider
	cfg.ExternalReviewModel = reviewer.modelSpec()
}

// isWatchOnlyMode returns true if running in watch-only mode.
// watch-only mode runs the web dashboard without executing any plan.
func isWatchOnlyMode(o opts, configWatchDirs []string) bool {
	return o.Serve && o.PlanFile == "" && o.PlanDescription == "" && (len(o.Watch) > 0 || len(configWatchDirs) > 0)
}

// mayBeWatchOnlyMode reports whether config loading is needed to distinguish a dashboard-only
// invocation from a normal run. Bare --serve can still be a normal run when no watch dirs exist.
func mayBeWatchOnlyMode(o opts) bool {
	return o.Serve && o.PlanFile == "" && o.PlanDescription == ""
}

// runWatchOnly starts the web dashboard in watch-only mode without plan execution.
func runWatchOnly(ctx context.Context, o opts, cfg *config.Config, colors *progress.Colors) error {
	dirs := web.ResolveWatchDirs(o.Watch, cfg.WatchDirs)
	dashboard := web.NewDashboard(web.DashboardConfig{
		Port:   o.Port,
		Host:   o.Host,
		Colors: colors,
	}, nil)
	if watchErr := dashboard.RunWatchOnly(ctx, dirs); watchErr != nil {
		return fmt.Errorf("run watch-only mode: %w", watchErr)
	}
	return nil
}

// determineMode returns the execution mode based on CLI flags.
func determineMode(o opts) processor.Mode {
	switch {
	case o.GenAgents:
		return processor.ModeGenAgents
	case o.PlanDescription != "":
		return processor.ModePlan
	case o.TasksOnly:
		return processor.ModeTasksOnly
	case o.ExternalOnly || o.CodexOnly:
		return processor.ModeCodexOnly
	case o.Review:
		return processor.ModeReview
	default:
		return processor.ModeFull
	}
}

// modeRequiresBranch returns true if the mode requires creating a feature branch.
// ModeFull and ModeTasksOnly both execute tasks that make commits, requiring a branch.
func modeRequiresBranch(mode processor.Mode) bool {
	return mode == processor.ModeFull || mode == processor.ModeTasksOnly
}

// modeCreatesBranch reports whether the run eventually creates a branch, which is what decides
// if --base-ref needs branch-base resolution outside worktree mode. plan mode counts even though
// plan creation itself runs in place: it hands off to an implementation run once the plan exists.
func modeCreatesBranch(mode processor.Mode) bool {
	return modeRequiresBranch(mode) || mode == processor.ModePlan
}

// makePauseHandler returns a context-aware pause handler for task loop breaks.
// on break, prints a message and waits for Enter to resume or context cancellation to abort.
// stdin read runs in a goroutine so the handler responds to Ctrl+C (SIGINT) promptly.
func makePauseHandler(stdin io.Reader, stdout io.Writer, titles *orca.Reporter) func(ctx context.Context) bool {
	return func(ctx context.Context) bool {
		return titles.WithInputWait(func() bool {
			fmt.Fprintln(stdout, "\nsession interrupted. press Enter to continue, Ctrl+C to abort")

			resultCh := make(chan bool, 1)
			go func() {
				buf := make([]byte, 1)
				n, _ := stdin.Read(buf) // blocks until Enter or EOF
				resultCh <- n > 0       // true = Enter (resume), false = EOF (abort)
			}()

			select {
			case resume := <-resultCh:
				return resume
			case <-ctx.Done():
				return false
			}
		})
	}
}

// shouldMovePlan returns true when a completed plan file should be moved to the
// completed/ directory: plan file is set, mode requires a branch, and the user
// has not opted out via move_plan_on_completion=false.
func shouldMovePlan(req executePlanRequest) bool {
	return req.PlanFile != "" && modeRequiresBranch(req.Mode) && req.Config.MovePlanOnCompletion
}

// validateFlags checks for conflicting CLI flags.
func validateFlags(o opts) error {
	if err := validateCloseoutFlags(o); err != nil {
		return err
	}
	if err := validatePlanChain(o); err != nil {
		return err
	}
	if o.PlanDescription != "" && o.PlanFile != "" {
		return errors.New("--plan flag conflicts with plan file argument; use one or the other")
	}
	if err := validateGenAgentsFlags(o); err != nil {
		return err
	}
	if err := validateCommitFlags(o); err != nil {
		return err
	}
	if o.Wait < 0 {
		return fmt.Errorf("--wait must be non-negative, got %s", o.Wait)
	}
	if o.SessionTimeout < 0 {
		return fmt.Errorf("--session-timeout must be non-negative, got %s", o.SessionTimeout)
	}
	if o.IdleTimeout < 0 {
		return fmt.Errorf("--idle-timeout must be non-negative, got %s", o.IdleTimeout)
	}
	if err := validateExternalReviewFlags(o); err != nil {
		return err
	}
	// --codex / --pass-claude-md / --external-only / --codex-only / --external-review-tool
	// mutual-exclusion checks are deferred to applyCodexOverrides, which runs after the
	// config-file merge so that executor=codex coming from config is also enforced.
	return nil
}

// validatePlanChain rejects chain-only incompatibilities and verifies every input before any
// config, git service, branch, or worktree is created. Single-plan invocations retain the existing
// selector-driven validation path.
func validatePlanChain(o opts) error {
	if len(o.PlanFiles) <= 1 {
		return nil
	}
	for _, conflict := range []struct {
		flag string
		set  bool
	}{
		{"--branch", o.Branch != ""},
		{"--serve", o.Serve},
		{"--plan", o.PlanDescription != ""},
		{"--review", o.Review},
		{"--external-only", o.ExternalOnly},
		{"--codex-only", o.CodexOnly},
	} {
		if conflict.set {
			return fmt.Errorf("%s cannot be combined with a plan chain", conflict.flag)
		}
	}
	completedPrefix := 0
	if root, err := os.Getwd(); err == nil {
		completedPrefix = checkpointCompletedPrefix(root, o)
	}
	seenFiles := make([]os.FileInfo, 0, len(o.PlanFiles))
	seenFileNames := make([]string, 0, len(o.PlanFiles))
	seenBranches := make(map[string]string, len(o.PlanFiles))
	for planIndex, planFile := range o.PlanFiles {
		if planIndex >= completedPrefix {
			if reason := planFileRefusal(planFile); reason != "" {
				return errors.New(reason)
			}
			info, err := os.Stat(planFile)
			if err != nil {
				return fmt.Errorf("inspect plan file %q: %w", planFile, err)
			}
			for i, previousInfo := range seenFiles {
				if os.SameFile(previousInfo, info) {
					return fmt.Errorf("plan chain contains duplicate file %q (same file as %q)", planFile, seenFileNames[i])
				}
			}
			seenFiles = append(seenFiles, info)
			seenFileNames = append(seenFileNames, planFile)
		}

		branchName := plan.ExtractBranchName(planFile)
		branchKey := strings.ToLower(branchName)
		if previous, exists := seenBranches[branchKey]; exists {
			return fmt.Errorf("plan chain entries %q and %q resolve to the same branch %q", previous, planFile, branchName)
		}
		seenBranches[branchKey] = planFile
	}
	return nil
}

// validateGenAgentsFlags keeps --gen-agents standalone: it neither executes a plan
// nor runs any review phase, so combining it with execution modes is a user error
// rather than something to silently ignore.
func validateGenAgentsFlags(o opts) error {
	if !o.GenAgents {
		return nil
	}
	if o.PlanFile != "" {
		return errors.New("--gen-agents cannot be combined with a plan file argument")
	}
	conflicts := []struct {
		flag string
		set  bool
	}{
		{"--plan", o.PlanDescription != ""},
		{"--review", o.Review},
		{"--external-only", o.ExternalOnly},
		{"--codex-only", o.CodexOnly},
		{"--tasks-only", o.TasksOnly},
		{"--worktree", o.Worktree},
		{"--commit", o.Commit},
		{"--init", o.Init},
		{"--reset", o.Reset},
		{"--dump-defaults", o.DumpDefaults != ""},
		// watch-only routing is decided before the gen-agents branch, so without these
		// two entries "--gen-agents --serve" would silently start the dashboard instead
		{"--serve", o.Serve},
		{"--watch", len(o.Watch) > 0},
	}
	for _, conflict := range conflicts {
		if conflict.set {
			return fmt.Errorf("--gen-agents cannot be combined with %s", conflict.flag)
		}
	}
	return nil
}

func validateCommitFlags(o opts) error {
	if !o.Commit {
		return nil
	}
	if o.Review || o.ExternalOnly || o.CodexOnly {
		return errors.New("--commit is only supported for full, --tasks-only, or --plan worktree execution")
	}
	return nil
}

func hasExecutionMode(o opts) bool {
	if o.executionModeSet {
		return true
	}
	for _, set := range []bool{
		o.PlanFile != "", o.MaxIterations != 0, o.MaxExternalIterations != 0,
		o.ReviewPatience != 0, o.PlanModel != "", o.TaskModel != "", o.ReviewModel != "",
		o.ClaudeCommand != "", o.ClaudeArgs != "", o.CodexArgs != "", o.ExternalReviewTool != "",
		o.ExternalReviewModel != "", o.ExternalReviewers != "", o.CustomReviewScript != "",
		o.PlanDescription != "", o.Review, o.ExternalOnly, o.CodexOnly, o.TasksOnly,
		o.BaseRef != "", o.waitSet || o.Wait != 0, o.sessionTimeoutSet || o.SessionTimeout != 0,
		o.idleTimeoutSet || o.IdleTimeout != 0, o.SkipFinalize, o.PreserveAnthropicAPIKey,
		o.NoClaudeSwap, o.Codex, o.PassClaudeMd, o.Worktree, o.Commit, o.Branch != "",
		o.Serve, len(o.Watch) != 0, o.Init, o.Reset, o.DumpDefaults != "", o.GenAgents,
	} {
		if set {
			return true
		}
	}
	return false
}

func mergeRequested(o opts) bool { return o.mergeSet || o.Merge != "" }

func prRequested(o opts) bool { return o.prSet || o.PR != "" }

func closeoutRequested(o opts) bool { return mergeRequested(o) || prRequested(o) }

func validateCloseoutFlags(o opts) error {
	merge, pr, execution := mergeRequested(o), prRequested(o), hasExecutionMode(o)
	if o.Clear && (merge || pr || execution) {
		return errors.New("--clear cannot be combined with a plan file or other mode flags")
	}
	// with --merge/--pr the positional argument names the feature to close out instead of
	// selecting a plan to run, so it does not count as an execution mode for these checks.
	closeoutExecution := execution
	if merge || pr {
		bare := o
		bare.PlanFile = ""
		closeoutExecution = hasExecutionMode(bare)
	}
	if merge && closeoutExecution {
		return errors.New("--merge cannot be combined with other mode flags")
	}
	if pr && (merge || closeoutExecution) {
		return errors.New("--pr cannot be combined with other mode flags")
	}
	if (merge || pr) && len(o.extraArgs) > 0 {
		// --merge and --pr take an optional base value only in the --merge=<base> form, so
		// "--merge <base> <feature>" parses <base> as the feature and would merge and delete it.
		// a surplus positional is the only observable trace of that mistake: reject it rather than
		// silently closing out a branch the caller never named.
		flag := "--merge"
		if pr {
			flag = "--pr"
		}
		return fmt.Errorf("%s accepts at most one feature argument, got %d; use %s=<base> to set the base branch",
			flag, len(o.extraArgs)+1, flag)
	}
	return nil
}

func validateExternalReviewFlags(o opts) error {
	if o.externalReviewersSet && (o.externalReviewToolSet || o.externalReviewModelSet) {
		return errors.New("--external-reviewers cannot be combined with --external-review-tool or --external-review-model")
	}
	return nil
}

// createRunner creates a processor.Runner with the given configuration.
func createRunner(req executePlanRequest, o opts, log processor.Logger, holder *status.PhaseHolder, commandTimingHandler func(string, time.Duration)) *processor.Runner {
	externalReview := req.ExternalReview
	applyEffectiveExternalReview(req.Config, externalReview)
	reviewer, enabled := externalReview.firstReviewer()
	reviewers := make([]config.ReviewerSpec, 0, len(externalReview.Reviewers))
	for _, resolved := range externalReview.Reviewers {
		reviewers = append(reviewers, config.ReviewerSpec{Provider: resolved.Provider, ModelSpec: resolved.modelSpec()})
	}
	// resolve max external iterations: CLI flag > config file > 0 (auto)
	maxExtIter := req.Config.MaxExternalIterations
	if o.MaxExternalIterations > 0 {
		maxExtIter = o.MaxExternalIterations
	}

	// resolve review patience: CLI flag > config file > 0 (disabled)
	reviewPatience := req.Config.ReviewPatience
	if o.ReviewPatience > 0 {
		reviewPatience = o.ReviewPatience
	}

	if req.LimitRecovery != nil {
		log.Print("claude-swap detected: automatic Claude account failover enabled")
	}
	r := processor.New(processor.Config{
		PlanFile:              req.PlanFile,
		ProgressPath:          log.Path(),
		Mode:                  req.Mode,
		MaxIterations:         resolveMaxIterations(o.MaxIterations, req.Config),
		MaxExternalIterations: maxExtIter,
		ReviewPatience:        reviewPatience,
		Debug:                 o.Debug,
		NoColor:               o.NoColor,
		IterationDelayMs:      req.Config.IterationDelayMs,
		TaskRetryCount:        req.Config.TaskRetryCount,
		CodexEnabled:          enabled,
		ExternalReviewTool:    reviewer.Provider,
		ExternalReviewModel:   reviewer.Model,
		ExternalReviewEffort:  reviewer.Effort,
		ExternalReviewers:     reviewers,
		FinalizeEnabled:       req.Config.FinalizeEnabled,
		DefaultBranch:         req.BaseRef,
		TaskModel:             resolveSpec(o.TaskModel, req.Config.TaskModel),
		ReviewModel:           resolveReviewSpec(o, req.Config),
		AppConfig:             req.Config,
		LimitRecovery:         req.LimitRecovery,
		CommandTimingHandler:  commandTimingHandler,
	}, log, holder)
	if req.GitSvc != nil {
		r.SetGitChecker(req.GitSvc)
	}
	return r
}

func detectClaudeSwapRecovery(o opts, cfg *config.Config, externalReview externalReviewSelection) limits.Recovery {
	if cfg == nil || o.NoClaudeSwap || !cfg.ClaudeSwapEnabled {
		return nil
	}
	claudeCmd := strings.TrimSpace(cfg.ClaudeCommand)
	if claudeCmd == "" {
		claudeCmd = "claude"
	}
	if filepath.Base(claudeCmd) != "claude" {
		return nil // custom stream-json compatible wrappers do not share Claude Code auth
	}
	usesClaude := cfg.Executor != config.ExecutorCodex
	for _, reviewer := range externalReview.Reviewers {
		usesClaude = usesClaude || reviewer.Provider == config.ExternalReviewToolClaude
	}
	if !usesClaude {
		return nil
	}
	recovery, ok := claudeswap.Detect(config.DefaultConfigDir())
	if !ok {
		return nil
	}
	return recovery
}

func printStartupInfo(info startupInfo, colors *progress.Colors) {
	if info.Mode == processor.ModePlan {
		colors.Info().Printf("starting interactive plan creation\n")
		colors.Info().Printf("request: %s\n", info.PlanDescription)
		colors.Info().Printf("branch: %s (max %d iterations)\n", info.Branch, info.MaxIterations)
		colors.Info().Printf("progress log: %s\n", toRelPath(info.ProgressPath))
		printExecutorInfo(info, colors)
		if info.PreserveAnthropicAPIKey {
			colors.Warn().Printf("auth: ANTHROPIC_API_KEY passthrough enabled\n")
		}
		colors.Info().Printf("\n")
		return
	}

	modeStr := ""
	if info.Mode != processor.ModeFull {
		modeStr = fmt.Sprintf(" (%s mode)", info.Mode)
	}
	colors.Info().Printf("starting loopai loop (max %d iterations)%s\n", info.MaxIterations, modeStr)
	displayMeta(colors, 0, info.PlanFile, info.Branch, info.ProgressPath)
	printExecutorInfo(info, colors)
	if info.PreserveAnthropicAPIKey {
		colors.Warn().Printf("auth: ANTHROPIC_API_KEY passthrough enabled\n")
	}
	colors.Info().Printf("\n")
}

func printExecutorInfo(info startupInfo, colors *progress.Colors) {
	if info.Executor == config.ExecutorCodex {
		printCodexExecutorInfo(info, colors)
	}
	printExternalReviewInfo(info.ExternalReview, colors)
}

func printExternalReviewInfo(selection externalReviewSelection, colors *progress.Colors) {
	reviewer, enabled := selection.firstReviewer()
	if !enabled && !selection.Resolved {
		return
	}
	if len(selection.Reviewers) > 1 {
		colors.Info().Printf("external review: %s\n", selection.chainLabel())
		return
	}

	colors.Info().Printf("external review: %s\n", selection.providerLabel())
	if reviewer.Provider == config.ExternalReviewToolCodex && reviewer.Model == "" {
		colors.Info().Printf("  model: %s\n", codexBannerValue(""))
	} else if reviewer.Model != "" {
		colors.Info().Printf("  model: %s\n", reviewer.Model)
	}
	if reviewer.Provider == config.ExternalReviewToolCodex && reviewer.Effort == "" {
		colors.Info().Printf("  reasoning effort: %s\n", codexBannerValue(""))
	} else if reviewer.Effort != "" {
		colors.Info().Printf("  reasoning effort: %s\n", reviewer.Effort)
	}
}

func printCodexExecutorInfo(info startupInfo, colors *progress.Colors) {
	colors.Info().Printf("executor: codex\n")
	// codex effective config: skip lines we don't know (loopai did not
	// override them, so codex picks from ~/.codex/config.toml). sandbox is
	// always resolved via CodexExecutorSandbox so it's always present.
	if info.CodexModel != "" {
		colors.Info().Printf("  model: %s\n", info.CodexModel)
	}
	if info.CodexSandbox != "" {
		colors.Info().Printf("  sandbox: %s\n", info.CodexSandbox)
	}
	if info.CodexEffort != "" {
		colors.Info().Printf("  reasoning effort: %s\n", info.CodexEffort)
	}
	if info.CodexReviewModel != info.CodexModel {
		colors.Info().Printf("  review model: %s\n", codexBannerValue(info.CodexReviewModel))
	}
	if info.CodexReviewEffort != info.CodexEffort {
		colors.Info().Printf("  review reasoning effort: %s\n", codexBannerValue(info.CodexReviewEffort))
	}
	if info.PassClaudeMd {
		colors.Info().Printf("claude.md: project CLAUDE.md passthrough enabled\n")
	}
}

// codexBannerValue renders a resolved codex model/effort value for the startup
// banner. an empty value means the codex executor inherits that field from the
// user's ~/.codex/config.toml, so it is labeled explicitly rather than shown blank.
func codexBannerValue(v string) string {
	if v == "" {
		return "(inherits ~/.codex/config.toml)"
	}
	return v
}

// codexBannerInfo holds the resolved codex primary/review model and effort for
// the startup banner.
type codexBannerInfo struct {
	taskModel, taskEffort     string
	reviewModel, reviewEffort string
	maxDropped                bool // a claude-only "max" effort was requested and dropped
}

func resolveSpec(cliVal, cfgVal string) string {
	if cliVal != "" {
		return cliVal
	}
	return cfgVal
}

// runHeaderParams returns run parameters recorded in the progress file header
// and web dashboard. Primary model fields preserve the existing user-set-only
// behavior; external fields record the effective provider and resolved model
// separately so they cannot be mistaken for the primary review model.
func runHeaderParams(o opts, cfg *config.Config, mode processor.Mode, external ...externalReviewSelection) progress.RunParams {
	p := progress.RunParams{}
	if cfg == nil {
		return p
	}
	var externalReview externalReviewSelection
	if len(external) > 0 {
		externalReview = external[0]
	}
	if cfg.Executor == config.ExecutorCodex {
		p.Executor = config.ExecutorCodex
	}
	if reviewer, ok := externalReview.firstReviewer(); ok || externalReview.Resolved {
		if len(externalReview.Reviewers) > 1 {
			p.ExternalReview = externalReview.chainLabel()
		} else {
			p.ExternalReview = externalReview.providerLabel()
			p.ExternalReviewModel = externalReview.modelSpec()
			if reviewer.Provider == config.ExternalReviewToolCodex && p.ExternalReviewModel == "" {
				p.ExternalReviewModel = "(inherits ~/.codex/config.toml)"
			}
		}
	}
	if mode == processor.ModePlan {
		p.PlanModel = resolvePlanSpec(o, cfg)
		return p
	}
	if mode == processor.ModeGenAgents {
		// one analysis session driven by task_model; no review phase runs, so do not
		// print a review model the mode never uses
		p.TaskModel = resolveSpec(o.TaskModel, cfg.TaskModel)
		return p
	}
	p.TaskModel = resolveSpec(o.TaskModel, cfg.TaskModel)
	p.ReviewModel = resolveSpec(o.ReviewModel, cfg.ReviewModel)
	return p
}

func resolvePlanSpec(o opts, cfg *config.Config) string {
	if planSpec := resolveSpec(o.PlanModel, cfg.PlanModel); planSpec != "" {
		return planSpec
	}
	return resolveSpec(o.TaskModel, cfg.TaskModel)
}

func resolveReviewSpec(o opts, cfg *config.Config) string {
	if reviewSpec := resolveSpec(o.ReviewModel, cfg.ReviewModel); reviewSpec != "" {
		return reviewSpec
	}
	return resolveSpec(o.TaskModel, cfg.TaskModel)
}

// cmuxRunModels resolves the model labels shown beside live cmux phases.
// Codex values mirror executor resolution; Claude's empty spec is explicitly
// labeled as the CLI default because loopai cannot know which model Claude selects.
func cmuxRunModels(o opts, cfg *config.Config, externalReview externalReviewSelection) cmux.Models {
	if cfg == nil {
		return cmux.Models{}
	}

	var planModel, taskModel, reviewModel string
	if cfg.Executor == config.ExecutorCodex {
		planInfo := codexPlanBanner(o, cfg)
		info := codexModelBanner(o, cfg)
		planModel = modelEffortLabel("codex default", planInfo.taskModel, planInfo.taskEffort)
		taskModel = modelEffortLabel("codex default", info.taskModel, info.taskEffort)
		reviewModel = modelEffortLabel("codex default", info.reviewModel, info.reviewEffort)
	} else {
		planModel = configuredModelLabel("claude default", resolvePlanSpec(o, cfg))
		taskModel = configuredModelLabel("claude default", resolveSpec(o.TaskModel, cfg.TaskModel))
		reviewModel = configuredModelLabel("claude default", resolveReviewSpec(o, cfg))
	}

	externalReviewModel := externalReview.modelSpec()
	if len(externalReview.Reviewers) > 1 {
		externalReviewModel = externalReview.chainLabel()
	}

	return cmux.Models{
		Plan:           planModel,
		Task:           taskModel,
		Review:         reviewModel,
		ExternalReview: externalReviewModel,
	}
}

func configuredModelLabel(fallback, spec string) string {
	switch {
	case spec == "":
		return fallback
	case strings.HasPrefix(spec, ":"):
		return fallback + spec
	default:
		return spec
	}
}

func modelEffortLabel(fallback, model, effort string) string {
	if model == "" {
		model = fallback
	}
	if effort != "" {
		return model + ":" + effort
	}
	return model
}

func codexBannerForSpec(spec string, cfg *config.Config) codexBannerInfo {
	model, effort, maxDropped := processor.ResolveCodexModelEffort(spec, cfg.CodexModel, cfg.CodexReasoningEffort)
	return codexBannerInfo{
		taskModel: model, taskEffort: effort,
		reviewModel: model, reviewEffort: effort,
		maxDropped: maxDropped,
	}
}

// codexModelBanner resolves the codex task and review model/effort for the startup
// banner from the task/review model specs (--task-model / --review-model CLI flag >
// task_model / review_model config) against codex_model / codex_reasoning_effort. it
// mirrors the resolution buildCodexExecutors performs so the banner shows what the
// codex executors will actually receive. review fields equal the task fields unless a
// distinct review spec is given.
func codexModelBanner(o opts, cfg *config.Config) codexBannerInfo {
	taskSpec := resolveSpec(o.TaskModel, cfg.TaskModel)
	info := codexBannerForSpec(taskSpec, cfg)
	reviewSpec := resolveSpec(o.ReviewModel, cfg.ReviewModel)
	if reviewSpec != "" {
		reviewInfo := codexBannerForSpec(reviewSpec, cfg)
		info.reviewModel, info.reviewEffort = reviewInfo.taskModel, reviewInfo.taskEffort
		info.maxDropped = info.maxDropped || reviewInfo.maxDropped
	}
	return info
}

// codexPlanBanner resolves the codex model/effort for plan creation. plan_model
// falls back to task_model, then to codex_model/codex_reasoning_effort defaults.
func codexPlanBanner(o opts, cfg *config.Config) codexBannerInfo {
	return codexBannerForSpec(resolvePlanSpec(o, cfg), cfg)
}

// runPlanMode executes interactive plan creation mode.
// creates input collector, progress logger, and runs the plan creation loop.
// after plan creation, prompts user to continue with implementation or exit.
func runPlanMode(ctx context.Context, o opts, req executePlanRequest, selector *plan.Selector) (runErr error) {
	defer func() { finishOrcaFailure(req.SetupTitles, runErr) }()

	if err := req.GitSvc.EnsureLocalGitignore(); err != nil {
		return fmt.Errorf("ensure gitignore: %w", err)
	}

	branch := getCurrentBranch(req.GitSvc)

	// create shared phase holder (single source of truth for current phase)
	holder := &status.PhaseHolder{}

	// create progress logger for plan mode
	baseLog, err := progress.NewLogger(progress.Config{
		PlanDescription: o.PlanDescription,
		Mode:            string(processor.ModePlan),
		Branch:          branch,
		Params:          runHeaderParams(o, req.Config, processor.ModePlan, req.ExternalReview),
		NoColor:         o.NoColor,
	}, req.Colors, holder)
	if err != nil {
		return fmt.Errorf("create progress logger: %w", err)
	}
	// planCreationErr is scoped to the plan-creation phase only. If r.Run fails, the
	// deferred Close writes "Failed:" so restart preserves Q&A history (issue #288).
	// Follow-on execution errors (executePlan/runWithWorktree below) do not affect
	// this log since plan creation already succeeded by that point.
	var planCreationErr error
	defer func() {
		if planCreationErr != nil {
			baseLog.SetFailed(planCreationErr)
		}
		if closeErr := baseLog.Close(); closeErr != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to close progress log: %v\n", closeErr)
		}
	}()

	// Status reporters for plan creation. No plan file exists yet, so cmux progress stays empty;
	// orca still receives phase and iteration titles.
	rep := cmux.New("", cmuxRunModels(o, req.Config, req.ExternalReview))
	titles := startOrcaReporter(req.Config, "", status.PhasePlan)
	setOrcaCleanup(req.OrcaStop, titles)
	req.SetupTitles.Quiesce()
	defer rep.Stop()
	defer titles.Stop()
	if req.CmuxStop != nil {
		req.CmuxStop.set(func() {
			rep.Stop()
			titles.Stop()
		})
	}
	rep.Start(ctx)
	holder.OnChange(rep.OnPhase)
	holder.OnChange(titles.OnPhase)
	planLog, sectionTimer := buildRunnerLogger(rep, titles, baseLog)

	maxIter := resolveMaxIterations(o.MaxIterations, req.Config)

	// resolve effective codex model/effort so the plan-mode banner reflects what
	// the codex executor receives. codex executor only, so the max-effort warning
	// is not a false positive in claude mode.
	var codex codexBannerInfo
	if req.Config.Executor == config.ExecutorCodex {
		codex = codexPlanBanner(o, req.Config)
	}

	// print startup info for plan mode
	printStartupInfo(startupInfo{
		PlanDescription:         o.PlanDescription,
		Branch:                  branch,
		Mode:                    processor.ModePlan,
		MaxIterations:           maxIter,
		ProgressPath:            baseLog.Path(),
		Executor:                req.Config.Executor,
		PassClaudeMd:            req.Config.PassClaudeMd,
		PreserveAnthropicAPIKey: req.Config.PreserveAnthropicAPIKey,
		CodexModel:              codex.taskModel,
		CodexEffort:             codex.taskEffort,
		CodexReviewModel:        codex.reviewModel,
		CodexReviewEffort:       codex.reviewEffort,
		CodexSandbox:            req.Config.CodexExecutorSandbox(),
		ExternalReview:          req.ExternalReview,
	}, req.Colors)
	if codex.maxDropped {
		req.Colors.Warn().Printf("codex does not support 'max' reasoning effort; ignoring (valid: low, medium, high, xhigh)\n")
	}

	// create input collector
	collector := input.NewTerminalCollector(o.NoColor)

	// record start time for finding the created plan
	startTime := time.Now()

	if req.LimitRecovery != nil {
		planLog.Print("claude-swap detected: automatic Claude account failover enabled")
	}
	r := processor.New(processor.Config{
		PlanDescription:  o.PlanDescription,
		ProgressPath:     baseLog.Path(),
		Mode:             processor.ModePlan,
		MaxIterations:    maxIter,
		Debug:            o.Debug,
		NoColor:          o.NoColor,
		IterationDelayMs: req.Config.IterationDelayMs,
		DefaultBranch:    req.BaseRef,
		TaskModel:        resolvePlanSpec(o, req.Config),
		AppConfig:        req.Config,
		LimitRecovery:    req.LimitRecovery,
	}, planLog, holder)
	// the collector is called exactly when the run stalls waiting for a human, so wrapping it
	// notifies cmux on questions and on the ready draft without touching the phase engines
	r.SetInputCollector(rep.WrapInput(titles.WrapInput(collector)))

	// run the plan creation loop
	runErr = runWithSectionTiming(ctx, r.Run, sectionTimer)
	if runErr != nil {
		wrapped := fmt.Errorf("plan creation: %w", runErr)
		planCreationErr = wrapped
		notifyCmuxCompletion(rep, "", branch, baseLog.Elapsed(), runErr)
		finishOrcaFailure(titles, runErr)
		return wrapped
	}

	// find the newly created plan file
	planFile := selector.FindRecent(startTime)
	elapsed := baseLog.Elapsed()

	// print completion message with plan file path if found
	if planFile != "" {
		req.Colors.Info().Printf("\nplan creation completed in %s, created %s\n", elapsed, toRelPath(planFile))
	} else {
		req.Colors.Info().Printf("\nplan creation completed in %s\n", elapsed)
	}

	// if no plan file found, can't continue to implementation. no cmux banner either: nothing
	// was created, and the run ends here rather than waiting for the user
	if planFile == "" {
		return nil
	}

	// not a completion notice: the implementation run may still follow, and the continue prompt
	// below waits for the user either way, so the banner says the plan is ready rather than done
	rep.Notify("plan created", fmt.Sprintf("%s in %s", filepath.Base(planFile), elapsed))

	// ask user if they want to continue with plan implementation
	if !titles.WithInputWait(func() bool {
		return input.AskYesNo(ctx, "Continue with plan implementation?", os.Stdin, os.Stdout)
	}) {
		return nil
	}

	// resolve plan file to absolute path before potential chdir. assigned through a separate
	// variable: filepath.Abs returns "" on error, so writing planFile first would leave the
	// notification below with nothing to name the run by
	absPlanFile, absErr := filepath.Abs(planFile)
	if absErr != nil {
		wrapped := fmt.Errorf("resolve plan file: %w", absErr)
		notifyCmuxCompletion(rep, planFile, branch, baseLog.Elapsed(), wrapped)
		finishOrcaFailure(titles, wrapped)
		return wrapped
	}
	planFile = absPlanFile

	// continue with plan implementation
	req.Colors.Info().Printf("\ncontinuing with plan implementation...\n")

	// Keep the non-final pill in place across branch/worktree setup, but tear down this reporter's
	// spinner and polling before the execution reporter takes ownership.
	rep.Quiesce()
	cmuxHandoff := func() {
		rep.Release()
		titles.Stop()
	}

	// worktree mode: create worktree and run from there
	if req.Config.WorktreeEnabled {
		runErr = runWithWorktree(ctx, o, executePlanRequest{
			PlanFile:       planFile,
			Mode:           processor.ModeFull,
			GitSvc:         req.GitSvc,
			Config:         req.Config,
			Colors:         req.Colors,
			DefaultBranch:  req.DefaultBranch,
			BaseRef:        req.BaseRef,
			NotifySvc:      req.NotifySvc,
			WtCleanup:      req.WtCleanup,
			CmuxStop:       req.CmuxStop,
			OrcaStop:       req.OrcaStop,
			CmuxHandoff:    cmuxHandoff,
			SetupTitles:    titles,
			BranchOverride: req.BranchOverride,
			ExternalReview: req.ExternalReview,
			LimitRecovery:  req.LimitRecovery,
		})
		finishOrcaFailure(titles, runErr)
		return runErr
	}

	// normal mode: create branch and run in place
	if err := req.GitSvc.CreateBranchForPlan(planFile, req.DefaultBranch, req.BranchOverride); err != nil {
		wrapped := fmt.Errorf("create branch for plan: %w", err)
		// the handoff dies before executePlan gets its own reporter, so this one raises the banner.
		// Quiesce only tore down transient state; the deferred Stop clears the preserved pill.
		notifyCmuxCompletion(rep, planFile, branch, baseLog.Elapsed(), wrapped)
		finishOrcaFailure(titles, wrapped)
		return wrapped
	}

	runErr = executePlan(ctx, o, executePlanRequest{
		PlanFile:       planFile,
		Mode:           processor.ModeFull,
		GitSvc:         req.GitSvc,
		Config:         req.Config,
		Colors:         req.Colors,
		DefaultBranch:  req.DefaultBranch,
		BaseRef:        req.BaseRef,
		NotifySvc:      req.NotifySvc,
		CmuxStop:       req.CmuxStop,
		OrcaStop:       req.OrcaStop,
		CmuxHandoff:    cmuxHandoff,
		SetupTitles:    titles,
		ExternalReview: req.ExternalReview,
		LimitRecovery:  req.LimitRecovery,
	})
	finishOrcaFailure(titles, runErr)
	return runErr
}

// reservedAgentNames are the built-in review agent names. A generated file using one
// of them replaces the built-in agent through per-file fallback, so it is reported as
// a warning rather than rejected — the user still reviews the files before committing.
var reservedAgentNames = []string{"quality", "implementation", "testing", "simplification", "documentation"}

// runGenAgentsMode runs one executor session that writes project-specific review
// agents into .loopai/agents/, then reports what ended up on disk. No branch, no
// worktree, no notifications: the session only produces files for the user to review.
func runGenAgentsMode(ctx context.Context, o opts, cfg *config.Config, colors *progress.Colors, recovery limits.Recovery) error {
	// the session writes a progress log under .loopai/ and then asks the user to inspect
	// git status, so ignore those artifacts the way every other mode does before starting.
	gitSvc, err := openGitService(colors, cfg.VcsCommand)
	if err != nil {
		return fmt.Errorf("open git repo: %w", err)
	}
	if ignoreErr := gitSvc.EnsureLocalGitignore(); ignoreErr != nil {
		return fmt.Errorf("ensure gitignore: %w", ignoreErr)
	}

	holder := &status.PhaseHolder{}
	baseLog, err := progress.NewLogger(progress.Config{
		Mode:    string(processor.ModeGenAgents),
		Params:  runHeaderParams(o, cfg, processor.ModeGenAgents),
		NoColor: o.NoColor,
	}, colors, holder)
	if err != nil {
		return fmt.Errorf("create progress logger: %w", err)
	}
	var genErr error
	defer func() {
		if genErr != nil {
			baseLog.SetFailed(genErr)
		}
		if closeErr := baseLog.Close(); closeErr != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to close progress log: %v\n", closeErr)
		}
	}()

	genLog, sectionTimer := buildRunnerLogger(nil, nil, baseLog)

	colors.Info().Printf("generating project-specific review agents\n")
	colors.Info().Printf("progress log: %s\n", toRelPath(baseLog.Path()))
	printExecutorInfo(startupInfo{Executor: cfg.Executor, CodexSandbox: cfg.CodexExecutorSandbox()}, colors)
	colors.Info().Printf("\n")

	if recovery != nil {
		genLog.Print("claude-swap detected: automatic Claude account failover enabled")
	}
	r := processor.New(processor.Config{
		Mode:          processor.ModeGenAgents,
		ProgressPath:  baseLog.Path(),
		Debug:         o.Debug,
		NoColor:       o.NoColor,
		TaskModel:     resolveSpec(o.TaskModel, cfg.TaskModel),
		AppConfig:     cfg,
		LimitRecovery: recovery,
	}, genLog, holder)

	if runErr := runWithSectionTiming(ctx, r.Run, sectionTimer); runErr != nil {
		// the runner already scopes the message ("agent generation phase: ..."); wrapping
		// again only repeats the same words in the stderr line and the progress footer
		genErr = runErr
		// a session that fails, times out, or is interrupted can still have written agent
		// files, and those files join the review catalog on the next run. report them
		// anyway: the reserved-name warning is the only signal that a generated file
		// replaces a built-in agent, and it would be lost with the error otherwise
		if reportErr := reportGeneratedAgents(genAgentsDir(cfg), os.Stdout); reportErr != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to report generated agents: %v\n", reportErr)
		}
		return genErr
	}

	colors.Info().Printf("\nagent generation completed in %s\n", baseLog.Elapsed())
	// the session succeeded and the agent files are on disk; a listing that cannot be
	// read is a warning, not a failed run. marking the log failed here would tell every
	// later reader the generation itself failed, and the failure path above already
	// treats the same error as a warning only
	if reportErr := reportGeneratedAgents(genAgentsDir(cfg), os.Stdout); reportErr != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to report generated agents: %v\n", reportErr)
	}
	return nil
}

// genAgentsDir resolves the directory the generation session writes agents to. The
// local config directory may not exist yet when the session starts, so an undetected
// local dir falls back to the .loopai/agents/ path named in the prompt.
func genAgentsDir(cfg *config.Config) string {
	if localDir := cfg.LocalDir(); localDir != "" {
		return filepath.Join(localDir, "agents")
	}
	return filepath.Join(".loopai", "agents")
}

// reportGeneratedAgents lists the agent files in dir with their descriptions, warns
// about missing descriptions and reserved built-in names, and reminds the user to
// review the result. A missing directory means the session wrote nothing.
func reportGeneratedAgents(dir string, w io.Writer) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(w, "no agent files found in %s\n", toRelPath(dir))
			return nil
		}
		return fmt.Errorf("read agents dir %s: %w", dir, err)
	}

	names := make([]string, 0, len(entries))
	var ignored []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		// the agent loader reads .txt only, so a session that ignored the prompt and
		// wrote .md leaves files git status shows but nothing ever loads. naming them
		// here is the only signal the user gets
		if !strings.HasSuffix(entry.Name(), ".txt") {
			ignored = append(ignored, entry.Name())
			continue
		}
		names = append(names, entry.Name())
	}
	slices.Sort(ignored)
	if len(names) == 0 {
		fmt.Fprintf(w, "no agent files found in %s\n", toRelPath(dir))
		reportIgnoredAgentFiles(ignored, w)
		return nil
	}
	slices.Sort(names)

	fmt.Fprintf(w, "agents in %s:\n", toRelPath(dir))
	var warnings []string
	for _, filename := range names {
		name := strings.TrimSuffix(filename, ".txt")
		reserved := slices.Contains(reservedAgentNames, name)
		var report agentFileReport
		content, readErr := os.ReadFile(filepath.Join(dir, filename)) //nolint:gosec // path from the project config dir
		switch {
		case readErr != nil:
			// one unreadable file must not hide the rest of the report: the files are
			// already written and the user still needs to see what to review
			report = agentFileReport{detail: fmt.Sprintf("(unreadable: %v)", readErr)}
		default:
			report = describeAgentFile(string(content), reserved)
		}
		fmt.Fprintf(w, "  - %s — %s\n", name, report.detail)
		// an inert file (only comments or frontmatter) leaves the built-in agent in
		// place, so warning that it replaces one would be wrong - a plain --init fills
		// this directory with exactly such all-commented copies of the built-in agents
		if reserved && report.active {
			warnings = append(warnings, fmt.Sprintf("warning: %s uses the reserved built-in agent name %q and replaces the built-in agent", filename, name))
		}
	}
	for _, warning := range warnings {
		fmt.Fprintln(w, warning)
	}
	reportIgnoredAgentFiles(ignored, w)
	fmt.Fprintln(w, "review the generated files (git diff / git status) and commit the ones worth keeping")
	return nil
}

// reportIgnoredAgentFiles warns about files in the agents directory the loader skips.
func reportIgnoredAgentFiles(ignored []string, w io.Writer) {
	if len(ignored) == 0 {
		return
	}
	fmt.Fprintf(w, "warning: ignoring non-.txt file(s) in the agents dir, agent files must use .txt: %s\n", strings.Join(ignored, ", "))
}

// agentFileReport is the outcome of inspecting one agent file: the text to print and
// whether the loader will actually use the file. An inactive file overrides nothing.
type agentFileReport struct {
	detail string
	active bool
}

// describeAgentFile renders the report line for one agent file. It reports what the
// agent loader will actually do with the file rather than the description alone: a
// file with no prompt body once comments are stripped is ignored — replaced by the
// embedded default for a reserved name, dropped entirely otherwise — and frontmatter
// that fails to parse (an unquoted description containing ": " is the likely cause) is
// indistinguishable from having none, so both would be reported as working agents.
func describeAgentFile(content string, reserved bool) agentFileReport {
	if !config.AgentFileHasBody(content) {
		if reserved {
			return agentFileReport{detail: "(no prompt body - file is ignored, built-in agent used instead)"}
		}
		return agentFileReport{detail: "(no prompt body - file is ignored)"}
	}
	agentOpts, _ := config.ParseAgentOptions(content)
	switch {
	case strings.TrimSpace(agentOpts.Description) != "":
		return agentFileReport{detail: strings.TrimSpace(agentOpts.Description), active: true}
	case config.AgentFrontmatterUnparsable(content):
		return agentFileReport{detail: "(unparsable frontmatter - not offered to the review phase, quote a description containing \": \")", active: true}
	default:
		return agentFileReport{detail: "(no description - not offered to the review phase)", active: true}
	}
}

// runReset runs the interactive config reset flow.
func runReset(configDir string, stdin io.Reader, stdout io.Writer) error {
	_, err := config.Reset(configDir, stdin, stdout)
	if err != nil {
		return fmt.Errorf("reset config: %w", err)
	}
	return nil
}

// handleEarlyFlags processes local flags that should run before full config load
// (--clear, --reset, --dump-defaults). Workspace hand-off is decided by run before this function,
// allowing an eligible auto run to reserve the current workspace before --reset can block.
// returns (true, nil) if an early exit occurred, (true, err) on error, or (false, nil) to continue.
func handleEarlyFlags(o opts) (bool, error) {
	if o.Clear {
		clearCmuxStatus(os.Stdout)
		return true, nil
	}

	if o.Reset {
		if err := runReset(o.ConfigDir, os.Stdin, os.Stdout); err != nil {
			return true, err
		}
		if isResetOnly(o) {
			return true, nil
		}
	}

	if o.Init {
		return true, initLocal(o.ConfigDir)
	}

	if o.DumpDefaults != "" {
		return true, dumpDefaults(o.DumpDefaults)
	}

	return false, nil
}

// clearCmuxStatus removes the completion pill when cmux is available. Outside cmux there is
// nothing to clear, which is a successful no-op rather than a configuration error.
func clearCmuxStatus(stdout io.Writer) {
	rep := cmux.New("", cmux.Models{})
	if rep == nil {
		fmt.Fprintln(stdout, "no loopai cmux status pill to clear (not running inside cmux)")
		return
	}
	rep.Clear()
}

// handOffToCmuxWorkspace relaunches this invocation in a new cmux workspace and reports whether
// the caller must stop, plus an error when stopping is not a success. hand-off is best-effort like
// the rest of the cmux integration: outside cmux, or when workspace creation is cleanly refused, a
// warning is printed and (false, nil) is returned so the run continues in the current terminal.
// auto mode also continues locally when the workspace is free or its status cannot be queried, but
// stays quiet unless debug logging is enabled. after it positively detects a busy workspace, every
// refusal or spawn failure stops instead of starting a conflicting run here. an ambiguous creation
// timeout stops in either mode because cmux may already have launched the plan. standalone commands
// are excluded, they are short synchronous commands whose output belongs to the terminal the user
// typed them in.
func handOffToCmuxWorkspace(o opts, args []string, stdout, stderr io.Writer) (bool, error) {
	if o.CmuxWorkspace == "" || isStandaloneCommand(o) {
		return false, nil
	}
	handOffRequired := false
	if o.CmuxWorkspace == "auto" {
		busy, err := cmux.WorkspaceBusy()
		if err != nil {
			if o.Debug {
				fmt.Fprintf(stderr, "debug: cmux workspace auto query failed, running here: %v\n", err)
			}
			return false, nil
		}
		if !busy {
			return false, nil
		}
		handOffRequired = true
	}

	exe, exeErr := os.Executable()
	if reason := executableHandOffRefusal(exe, exeErr); reason != "" {
		return handOffRefusal(reason, handOffRequired, stderr)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return handOffRefusal("resolve working directory: "+err.Error(), handOffRequired, stderr)
	}

	// the repository-root requirement is otherwise only enforced in the child, and for the same
	// reason as the plan file below: running from a subdirectory would create and focus a workspace
	// whose run dies immediately while this terminal reported success and exited 0. .git is the
	// marker the run itself checks for, so the hand-off stays config-independent wherever it exists;
	// only its absence consults read-only config, for the two invocations that legitimately have no
	// marker.
	if !fileExists(".git") && !handOffAllowedOutsideRepo(o) {
		return handOffRefusal("not a repository root: "+cwd, handOffRequired, stderr)
	}

	// An unusable plan file is otherwise only detected in the child, long after the workspace was
	// created and focused: the terminal the user typed in prints a success line and exits 0 while
	// the new card dies immediately, leaving an orphan to close by hand. A chain is handed off as
	// one child invocation, so every entry must be usable before that workspace is spawned.
	planFiles := o.PlanFiles
	if len(planFiles) == 0 && o.PlanFile != "" {
		planFiles = []string{o.PlanFile}
	}
	completedPrefix := checkpointCompletedPrefix(cwd, o)
	for i, planFile := range planFiles {
		if i < completedPrefix {
			continue
		}
		if reason := planFileRefusal(planFile); reason != "" {
			return handOffRefusal(reason, handOffRequired, stderr)
		}
	}

	name := cmuxWorkspaceName(o)
	if err := cmux.SpawnWorkspace(name, cwd, cmuxHandOffArgv(exe, args)); err != nil {
		return handOffSpawnFailure(err, handOffRequired, stderr)
	}
	warnAPIKeyNotCarried(o, stderr)
	fmt.Fprintf(stdout, "handed off to cmux workspace %s\n", name)
	return true, nil
}

// ensureAutoWorkspaceReservation marks an eligible local auto-mode run busy before lengthy
// startup. Possible watch-only runs are normally ineligible until config resolves their watch
// directories; combined resets opt in earlier because their prompt may change that configuration.
// The cleanup holder owns the reservation until a normal reporter replaces it, clearing "starting"
// on every startup error and on interrupts without erasing a later final pill.
func ensureAutoWorkspaceReservation(o opts, eligible, reserved bool, cmuxStop *cleanupHolder, stderr io.Writer) bool {
	if o.CmuxWorkspace != "auto" || isStandaloneCommand(o) || !eligible || reserved {
		return reserved
	}
	rep := cmux.New("", cmux.Models{})
	if err := rep.Reserve(); err != nil {
		if o.Debug && !errors.Is(err, cmux.ErrNotInCmux) {
			fmt.Fprintf(stderr, "debug: cmux workspace auto reservation failed: %v\n", err)
		}
		return false
	}
	cmuxStop.set(rep.Stop)
	return true
}

func resolveAutoWorkspaceReservationAfterConfig(
	o opts,
	watchOnly, reserved bool,
	cmuxStop *cleanupHolder,
	stderr io.Writer,
) {
	reserved = ensureAutoWorkspaceReservation(o, !watchOnly, reserved, cmuxStop, stderr)
	if !watchOnly || !reserved {
		return
	}
	// A combined reset may have needed a reservation while its prompt was blocking, before
	// configuration could prove the invocation was dashboard-only. Release that temporary
	// ownership once no execution run will follow.
	cmuxStop.call()
}

// anthropicAPIKeyEnv is the credential --preserve-anthropic-api-key opts into passing through to
// claude, which is otherwise stripped from the child environment.
const anthropicAPIKeyEnv = "ANTHROPIC_API_KEY" //nolint:gosec // the name of an environment variable, not a credential

// warnAPIKeyNotCarried reports that the ANTHROPIC_API_KEY pass-through survives the hand-off but
// the key itself does not. the request travels in argv or config, the environment does not: the new
// workspace starts a shell of cmux's own. a key exported only in this terminal is therefore absent
// there, and claude falls back to OAuth or the keychain without saying so, so the run bills an
// account the user did not pick — the wrong-context run the pass-through exists to make visible.
// the key is deliberately not forwarded, since the command reaches the new workspace as text typed
// into its shell, so the gap is reported here instead. nothing is said when the variable is unset,
// because then there is nothing to preserve in this terminal either; that test comes first so the
// quiet case costs no config read.
func warnAPIKeyNotCarried(o opts, stderr io.Writer) {
	if os.Getenv(anthropicAPIKeyEnv) == "" || !preserveAPIKeyRequested(o) {
		return
	}
	fmt.Fprintf(stderr, "warning: %s is not carried into the new cmux workspace, "+
		"the API key pass-through applies there only if the key comes from your shell profile\n",
		anthropicAPIKeyEnv)
}

// preserveAPIKeyRequested reports whether this run passes ANTHROPIC_API_KEY through to claude. the
// flag is only half of it: preserve_anthropic_api_key is a config key too and applyCLIOverrides ORs
// the two, so reading argv alone would stay silent for exactly the users who set it once and never
// type it again. config is read read-only here, the same way handOffAllowedOutsideRepo does, and an
// unreadable config leaves the flag as the only answer since the child would fail to load it too.
func preserveAPIKeyRequested(o opts) bool {
	if o.PreserveAnthropicAPIKey {
		return true
	}
	cfg, err := config.LoadReadOnly(o.ConfigDir)
	return err == nil && cfg.PreserveAnthropicAPIKey
}

// handOffSpawnFailure turns a workspace creation failure into the caller's verdict. unconditional
// mode warns and continues after a clean refusal, which is its best-effort contract. auto mode must
// stop once a busy workspace made hand-off mandatory. an ambiguous timeout stops in either mode:
// cmux may already have created the workspace and started the same plan there, so running it here
// too would put two agents on one checkout.
func handOffSpawnFailure(err error, required bool, stderr io.Writer) (bool, error) {
	if errors.Is(err, cmux.ErrSpawnAmbiguous) {
		return true, fmt.Errorf("cmux workspace hand-off: %w; not running here, check the sidebar and re-run", err)
	}
	if required {
		return true, fmt.Errorf("cmux workspace hand-off required because the current workspace is busy: %w; not running here", err)
	}
	fmt.Fprintf(stderr, "warning: cmux workspace hand-off failed, running here: %v\n", err)
	return false, nil
}

func handOffRefusal(reason string, required bool, stderr io.Writer) (bool, error) {
	if required {
		return true, fmt.Errorf("cmux workspace hand-off required because the current workspace is busy: %s; not running here", reason)
	}
	fmt.Fprintf(stderr, "warning: cmux workspace hand-off skipped, running here: %s\n", reason)
	return false, nil
}

// executableHandOffRefusal reports why this binary cannot be relaunched in the new workspace, or ""
// when it can. resolution failing is the obvious half; the other is that os.Executable can succeed
// and still name a path the new workspace's shell cannot run. on Linux it reads /proc/self/exe,
// which keeps naming an unlinked binary with a " (deleted)" suffix; the child would then die on
// "command not found" while this terminal printed its success line and exited 0, the orphan-card
// outcome the plan-file and repository-root guards exist to prevent. the path is checked against
// the same working directory the child is given, so no hand-off that would have worked is refused.
// presence is all this can test, so it does not reach a binary that disappears afterwards: "go run"
// unlinks its temporary binary only once the successful hand-off has exited 0, so that invocation
// still hands off and fails in the new workspace. build the binary to smoke-test the flag.
func executableHandOffRefusal(exe string, err error) string {
	if err != nil {
		return "resolve executable: " + err.Error()
	}
	if !fileExists(exe) {
		return "executable not found: " + exe
	}
	return ""
}

// planFileRefusal reports why a non-empty plan path cannot produce a run that survives, or "" when
// it can. Existence is the plan selector's whole test, resolved here against the same working
// directory, but it is not enough on its own: a directory stats fine, so "loopai --cmux-workspace
// docs/plans" (the filename forgotten) would hand off, and the child would only fail once it read
// the plan, in full mode after the branch already exists. an unreadable file fails the same read.
// both are refused here for the same reason a missing plan is, and neither refuses a hand-off that
// would have worked, since the child cannot read either one. the regular-file test comes before the
// open because opening a fifo blocks until a writer appears, and this check must not hang.
func planFileRefusal(planFile string) string {
	info, err := os.Stat(planFile)
	if err != nil {
		return "plan file not found: " + planFile
	}
	if !info.Mode().IsRegular() {
		return "plan file is not a regular file: " + planFile
	}
	f, err := os.Open(planFile) //nolint:gosec // the plan path is the user's own argument, only opened to test readability
	if err != nil {
		return "plan file not readable: " + planFile
	}
	_ = f.Close()
	return ""
}

// handOffAllowedOutsideRepo reports whether a hand-off from a directory without a .git marker still
// leads to a run that survives. two invocations legitimately have no marker: a non-git VCS backend,
// which the run skips the marker check for, and a watch-only --serve, which never reaches that
// check. both are config-dependent, and watch dirs decide the second one: a bare --serve without
// any is a normal run that would die on "must run from repository root". a config that cannot be
// read is neither, the child would fail to load it too, so that error belongs in this terminal.
func handOffAllowedOutsideRepo(o opts) bool {
	cfg, err := config.LoadReadOnly(o.ConfigDir)
	if err != nil {
		return false
	}
	return isWatchOnlyMode(o, cfg.WatchDirs) || (cfg.VcsCommand != "" && cfg.VcsCommand != "git")
}

// cmuxWorkspaceName titles the new workspace after the branch the run will use, so sidebar cards
// line up with .loopai/worktrees/<branch>. A chain uses its first plan because that is the branch
// checked out when the workspace opens; successor plans create their stacked branches inside the
// same workspace. Plan creation has no plan file yet and falls back to the app name.
func cmuxWorkspaceName(o opts) string {
	if name := strings.TrimSpace(o.Branch); name != "" {
		return name
	}
	planFile := o.PlanFile
	if len(o.PlanFiles) > 0 {
		planFile = o.PlanFiles[0]
	}
	// an empty plan file must not reach ExtractBranchName: filepath.Base("") is "."
	if strings.TrimSpace(planFile) == "" {
		return "loopai"
	}
	if name := strings.TrimSpace(plan.ExtractBranchName(planFile)); name != "" {
		return name
	}
	return "loopai"
}

// cmuxEnvOptions lists the environment variables go-flags reads option values from. cmux starts
// the new workspace from a shell of its own, which inherits cmux's environment and not this
// process's, so an option provided through the environment would silently revert to its default
// after hand-off. TestCmuxEnvOptionsCoversOptionTags keeps the list in sync with the struct tags.
var cmuxEnvOptions = []string{"LOOPAI_CONFIG_DIR", "LOOPAI_ORCA", "LOOPAI_WEB_HOST"}

// cmuxHandOffArgv builds the command the new workspace runs: this executable, the arguments minus
// the hand-off flag, and an env prefix carrying the environment-provided options across. env is
// used rather than shell assignment prefixes because the target shell is unknown and not every
// shell supports them.
func cmuxHandOffArgv(exe string, args []string) []string {
	var prefix []string
	for _, key := range cmuxEnvOptions {
		if value, ok := os.LookupEnv(key); ok {
			prefix = append(prefix, key+"="+value)
		}
	}
	if len(prefix) > 0 {
		prefix = append([]string{"env"}, prefix...)
	}
	return append(append(prefix, exe), stripCmuxWorkspaceArg(args)...)
}

// stripCmuxWorkspaceArg removes --cmux-workspace and its attached value forms from the arguments
// the new workspace is relaunched with, which is the recursion guard: the child performs a normal
// run. optional go-flags values must be attached, so a following argument is always preserved.
func stripCmuxWorkspaceArg(args []string) []string {
	const flag = "--cmux-workspace"
	filtered := make([]string, 0, len(args))
	for _, a := range args {
		if a == flag || strings.HasPrefix(a, flag+"=") {
			continue
		}
		filtered = append(filtered, a)
	}
	return filtered
}

func clearStaleCmuxStatus(o opts) {
	if isStandaloneCommand(o) {
		return
	}
	cmux.New("", cmux.Models{}).Clear()
}

// isStandaloneCommand reports whether the invocation is a config utility or close-out command
// rather than a run. those own no sidebar state, so they neither replace a previous run's pill
// nor get handed over to a new cmux workspace. --gen-agents belongs here: it executes no plan
// and never constructs a reporter.
func isStandaloneCommand(o opts) bool {
	return o.Clear || closeoutRequested(o) || o.Init || o.DumpDefaults != "" || o.GenAgents || (o.Reset && isResetOnly(o))
}

// handOffSucceeded reports whether an early stop came from a successful cmux workspace hand-off,
// which means the run happens in another workspace and this one's pill is not ours to replace.
func handOffSucceeded(o opts, earlyErr error) bool {
	return o.CmuxWorkspace != "" && earlyErr == nil
}

// prepareStaleCmuxStatus clears immediately for definite runs. Auto mode never clears through this
// path because every status observed before reservation is unowned and may belong to another live
// run. Its reservation or normal reporter replaces the old status once this process owns the card.
// Always mode and possible watch-only mode defer until their local/preserve verdict is known.
func prepareStaleCmuxStatus(o opts) func(preserve bool) {
	if o.CmuxWorkspace == "auto" {
		return func(bool) {}
	}
	if mayBeWatchOnlyMode(o) || o.CmuxWorkspace != "" {
		return func(preserve bool) {
			if !preserve {
				clearStaleCmuxStatus(o)
			}
		}
	}
	clearStaleCmuxStatus(o)
	return func(bool) {}
}

type cmuxStatusClearer interface {
	Clear()
}

// featureBranchResolver reports whether a local branch exists and derives the branch a plan file
// implies; satisfied by *git.Service. Deriving through the service keeps --merge/--pr resolution
// byte-identical to the name worktree creation produced, including on-disk filename case.
type featureBranchResolver interface {
	BranchExists(name string) bool
	EffectiveBranchName(planFile, branchOverride string) string
}

// resolveFeatureBranch resolves a feature identifier supplied to --merge/--pr into a local
// branch name. resolution is deterministic: an exact local branch match wins, then a plan file
// located by path or by basename in plansDir and plansDir/completed. the branch a run recorded
// for that plan wins over the name derived from the filename, because a --branch override makes
// the filename imply a branch the run never created; either way the branch must still exist.
// progressRoots names the checkouts that can hold .loopai/progress, which are not always the one
// plansDir sits in.
func resolveFeatureBranch(gitSvc featureBranchResolver, progressRoots []string, plansDir, arg string) (string, error) {
	identifier := strings.TrimSpace(arg)
	if identifier == "" {
		return "", errors.New("empty feature identifier")
	}
	if gitSvc.BranchExists(identifier) {
		return identifier, nil
	}

	completedDir := filepath.Join(plansDir, "completed")
	planFile := findFeaturePlanFile(identifier, plansDir, completedDir)
	if planFile == "" {
		return "", fmt.Errorf("unknown feature %q: no local branch with this name and no plan file in %q or %q",
			identifier, plansDir, completedDir)
	}

	branch, err := recordedBranchForPlan(progressRoots, planFile)
	if err != nil {
		return "", err
	}
	if branch == "" {
		branch = gitSvc.EffectiveBranchName(planFile, "")
	}
	if !gitSvc.BranchExists(branch) {
		return "", fmt.Errorf("plan %q resolves to branch %q, which does not exist locally (already merged?)",
			filepath.Base(planFile), branch)
	}
	return branch, nil
}

// findFeaturePlanFile locates a plan file for identifier, accepting an explicit path or a
// basename with an optional .md extension. an unresolvable plan path falls back to a basename
// lookup in dirs, covering plans already moved to the completed directory.
func findFeaturePlanFile(identifier string, dirs ...string) string {
	if filepath.Base(identifier) != identifier {
		if path := existingPlanFile(identifier); path != "" {
			return path
		}
		if filepath.Ext(identifier) != ".md" {
			// a path-shaped identifier that names no plan file is a namespaced branch name such
			// as feature/login. reducing it to its last segment would resolve an unrelated plan,
			// and --merge then merges and deletes a branch the caller never named
			return ""
		}
	}
	base := filepath.Base(identifier)
	for _, dir := range dirs {
		if path := existingPlanFile(filepath.Join(dir, base)); path != "" {
			return path
		}
	}
	return ""
}

// existingPlanFile returns path if it names a regular file, retrying with an added .md extension.
// os.Stat matches case-insensitively on macOS and Windows, so the returned path may carry the
// caller's case; git.Service.EffectiveBranchName resolves it to the on-disk name before deriving
// the branch.
func existingPlanFile(path string) string {
	candidates := []string{path}
	if filepath.Ext(path) != ".md" {
		candidates = append(candidates, path+".md")
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
			return candidate
		}
	}
	return ""
}

// closeoutTarget carries the optional feature identifier supplied as the positional argument of
// --merge/--pr together with the plans directory used to resolve it. a zero value means the
// close-out applies to the currently checked-out branch.
type closeoutTarget struct {
	identifier string
	plansDir   string
}

// resolveCloseoutBranch determines the feature branch a close-out command operates on: the
// explicitly named feature when given, otherwise the currently checked-out branch.
func resolveCloseoutBranch(gitSvc *git.Service, target closeoutTarget, flagName string) (string, error) {
	if strings.TrimSpace(target.identifier) != "" {
		// anchor the plans directory at the invoking checkout's root, matching the PR metadata
		// lookup, so a basename identifier resolves the same from any directory in the checkout
		root := gitSvc.Root()
		progressRoots, err := progressRecordRoots(gitSvc)
		if err != nil {
			return "", err
		}
		return resolveFeatureBranch(gitSvc, progressRoots, plansDirPath(root, target.plansDir), target.identifier)
	}
	branch, err := gitSvc.CurrentBranch()
	if err != nil {
		return "", fmt.Errorf("read current branch: %w", err)
	}
	if branch == "" {
		return "", fmt.Errorf("%s requires a checked-out feature branch; detached HEAD is not supported", flagName)
	}
	return branch, nil
}

// progressRecordRoots returns every checkout that can own .loopai/progress: the primary first, the
// invoking checkout next, then every other registered worktree. the progress logger resolves its
// path against the working directory before loopai changes into a worktree, so a run started from
// the primary checkout records there even when it executed in a linked worktree, while a run
// started inside any linked worktree records in that worktree. scanning only the primary and the
// invoking checkout silently disables the recorded-branch lookup for a run started in a third
// worktree, and a miss is the dangerous direction: it falls back to deriving the branch from the
// plan filename, which is what --merge needs the record to override to stay off an unrelated
// branch. a worktree holding no progress directory simply contributes nothing.
func progressRecordRoots(gitSvc *git.Service) ([]string, error) {
	worktrees, err := gitSvc.Worktrees()
	if err != nil {
		return nil, fmt.Errorf("inspect repository worktrees: %w", err)
	}
	if len(worktrees) == 0 {
		return nil, errors.New("inspect repository worktrees: Git returned no registered worktrees")
	}
	roots := []string{worktrees[0].Path}
	appendRoot := func(candidate string) {
		if candidate == "" {
			return
		}
		for _, root := range roots {
			if sameProgressRoot(candidate, root) {
				return
			}
		}
		roots = append(roots, candidate)
	}
	// the invoking checkout keeps second place: it is the likeliest owner of the record after the
	// primary, and it stays in the list even in the unlikely event Git does not report it
	appendRoot(gitSvc.Root())
	for _, wt := range worktrees[1:] {
		appendRoot(wt.Path)
	}
	return roots, nil
}

// sameProgressRoot reports whether two checkout roots name the same directory, resolving symlinks
// so a primary worktree reached through one does not get scanned twice.
func sameProgressRoot(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	resolvedA, errA := filepath.EvalSymlinks(a)
	resolvedB, errB := filepath.EvalSymlinks(b)
	if errA != nil || errB != nil {
		return false
	}
	return filepath.Clean(resolvedA) == filepath.Clean(resolvedB)
}

// runMergeCommand merges the feature branch into an explicit or detected base.
// The completion pill is deliberately retained on every failure so the pending action stays visible.
func runMergeCommand(ctx context.Context, gitSvc *git.Service, explicitBase string, target closeoutTarget,
	rep cmuxStatusClearer, stdout io.Writer) error {
	explicit := strings.TrimSpace(target.identifier) != ""
	// without an explicit feature the invoking checkout is the feature worktree and must be clean.
	// with one the merge touches only the feature and base worktrees, both validated separately by
	// prepareMergeWorktrees, so unrelated work in the invoking checkout must not block the close-out.
	if !explicit {
		dirty, dirtyErr := gitSvc.IsDirtyAll()
		if dirtyErr != nil {
			return fmt.Errorf("check working tree: %w", dirtyErr)
		}
		if dirty {
			return errors.New("--merge requires a clean working tree; commit, stash, or remove changes first")
		}
	}

	feature, err := resolveCloseoutBranch(gitSvc, target, "--merge")
	if err != nil {
		return err
	}
	base, err := gitSvc.ResolveBaseBranch(explicitBase)
	if err != nil {
		return fmt.Errorf("resolve merge base branch: %w", err)
	}
	if feature == base {
		if explicit {
			return fmt.Errorf("feature %q is already the base branch; name a different feature", base)
		}
		return fmt.Errorf("current branch %q is already the base branch; check out the feature branch first", base)
	}

	targets, err := prepareMergeWorktrees(gitSvc, feature, base, explicit)
	if err != nil {
		return err
	}

	featureHead, err := gitSvc.BranchHash(feature)
	if err != nil {
		return fmt.Errorf("read feature branch head: %w", err)
	}
	mergeResult, err := mergeForCloseout(ctx, targets.mergeSvc, feature, base, featureHead)
	if err != nil {
		return err
	}

	removedWorktree := targets.removableWorktree()
	if cleanupErr := cleanupMergedWorktree(targets, feature); cleanupErr != nil {
		return restoreMergeWorktree(targets.mergeSvc, mergeResult, cleanupErr)
	}
	if deleteErr := targets.mergeSvc.DeleteBranch(feature); deleteErr != nil {
		return restoreMergeWorktree(targets.mergeSvc, mergeResult, fmt.Errorf("delete merged feature branch: %w", deleteErr))
	}
	if restoreErr := restoreMergeWorktree(targets.mergeSvc, mergeResult, nil); restoreErr != nil {
		return restoreErr
	}
	if rep != nil {
		rep.Clear()
	}
	// name the removed directory: with an explicit feature the removal target is resolved from the
	// worktree list rather than being the caller's own directory, so it is otherwise invisible, and
	// removal takes ignored files such as .env with it.
	if removedWorktree != "" {
		fmt.Fprintf(stdout, "merged %s into %s (%s); deleted branch %s and worktree %s\n",
			feature, base, mergeResult.mergeType, feature, removedWorktree)
		return nil
	}
	fmt.Fprintf(stdout, "merged %s into %s (%s); deleted branch %s\n", feature, base, mergeResult.mergeType, feature)
	return nil
}

// mergeTargets describes where a close-out merge executes and which feature worktree, if any,
// must be cleaned up afterwards. featureSvc and featurePath are empty when the feature branch is
// not checked out in any registered worktree, in which case the merge only deletes the branch.
type mergeTargets struct {
	mergeSvc    *git.Service
	featureSvc  *git.Service
	featurePath string
	primaryPath string
}

// removableWorktree returns the linked worktree the close-out removes after a successful merge, or
// an empty string when nothing is removed: the feature has no worktree of its own, or it shares the
// primary worktree, which is never removed.
func (t mergeTargets) removableWorktree() string {
	if t.featurePath == "" || filepath.Clean(t.featurePath) == filepath.Clean(t.primaryPath) {
		return ""
	}
	return t.featurePath
}

// prepareMergeWorktrees locates the worktree the merge runs in and the feature worktree to clean
// up. without an explicit feature the feature branch must be checked out at gitSvc's root, which
// keeps the no-argument close-out behavior unchanged. with an explicit feature the command may run
// from anywhere in the repository and the feature may have no worktree at all.
func prepareMergeWorktrees(gitSvc *git.Service, feature, base string, explicit bool) (mergeTargets, error) {
	worktrees, err := gitSvc.Worktrees()
	if err != nil {
		return mergeTargets{}, fmt.Errorf("inspect repository worktrees: %w", err)
	}
	if len(worktrees) == 0 {
		return mergeTargets{}, errors.New("inspect repository worktrees: Git returned no registered worktrees")
	}
	primaryPath := worktrees[0].Path
	featurePath := worktreePathForBranch(worktrees, feature)
	basePath := worktreePathForBranch(worktrees, base)
	if !explicit && (featurePath == "" || filepath.Clean(featurePath) != filepath.Clean(gitSvc.Root())) {
		return mergeTargets{}, fmt.Errorf("current branch %q is not registered at repository root %q", feature, gitSvc.Root())
	}

	if featurePath != "" && filepath.Clean(featurePath) == filepath.Clean(primaryPath) {
		return primaryMergeTargets(gitSvc, feature, base, basePath, primaryPath)
	}

	mergePath := primaryPath
	if basePath != "" {
		mergePath = basePath
	}
	mergeSvc, err := openMergeWorktree(gitSvc, mergePath)
	if err != nil {
		return mergeTargets{}, err
	}
	baseDirty, err := mergeSvc.IsDirtyAll()
	if err != nil {
		return mergeTargets{}, fmt.Errorf("check base worktree: %w", err)
	}
	if baseDirty {
		if basePath == "" {
			// the merge checks base out here, so name why this worktree has to be clean
			return mergeTargets{}, fmt.Errorf("--merge requires a clean base worktree at %s: base branch %q is not checked out anywhere, so the merge runs in the primary worktree",
				mergeSvc.Root(), base)
		}
		return mergeTargets{}, fmt.Errorf("--merge requires a clean base worktree at %s", mergeSvc.Root())
	}
	if featurePath == "" {
		return mergeTargets{mergeSvc: mergeSvc, primaryPath: primaryPath}, nil
	}

	featureSvc, err := openMergeWorktree(gitSvc, featurePath)
	if err != nil {
		return mergeTargets{}, err
	}
	if cleanErr := requireCleanFeatureWorktree(featureSvc); cleanErr != nil {
		return mergeTargets{}, cleanErr
	}
	return mergeTargets{mergeSvc: mergeSvc, featureSvc: featureSvc, featurePath: featurePath, primaryPath: primaryPath}, nil
}

// primaryMergeTargets handles the case where the feature branch is checked out in the primary
// worktree: the merge runs there, and no worktree is removed afterwards.
func primaryMergeTargets(gitSvc *git.Service, feature, base, basePath, primaryPath string) (mergeTargets, error) {
	if basePath != "" && filepath.Clean(basePath) != filepath.Clean(primaryPath) {
		return mergeTargets{}, fmt.Errorf("cannot close branch %q: it is checked out in the primary worktree %q while base branch %q is checked out at %q",
			feature, primaryPath, base, basePath)
	}
	primarySvc, err := openMergeWorktree(gitSvc, primaryPath)
	if err != nil {
		return mergeTargets{}, err
	}
	// the merge runs in the feature's own worktree here, so it must be clean no matter where the
	// command was invoked from
	if cleanErr := requireCleanFeatureWorktree(primarySvc); cleanErr != nil {
		return mergeTargets{}, cleanErr
	}
	return mergeTargets{mergeSvc: primarySvc, featureSvc: primarySvc, featurePath: primaryPath, primaryPath: primaryPath}, nil
}

// requireCleanFeatureWorktree rejects a feature worktree with uncommitted changes before the merge
// touches it, so no work is lost by the later cleanup.
func requireCleanFeatureWorktree(featureSvc *git.Service) error {
	dirty, err := featureSvc.IsDirtyAll()
	if err != nil {
		return fmt.Errorf("check feature worktree: %w", err)
	}
	if dirty {
		return fmt.Errorf("--merge requires a clean feature worktree at %s", featureSvc.Root())
	}
	return nil
}

// openMergeWorktree returns gitSvc itself when path already names its root, avoiding a redundant
// repository open for the common single-worktree case.
func openMergeWorktree(gitSvc *git.Service, path string) (*git.Service, error) {
	if filepath.Clean(path) == filepath.Clean(gitSvc.Root()) {
		return gitSvc, nil
	}
	// a worktree whose directory was deleted by hand stays registered until it is pruned, and the
	// close-out must not delete a branch Git still considers checked out there
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("registered worktree %q is missing; run \"git worktree prune\" and retry: %w", path, err)
	}
	svc, err := gitSvc.OpenWorktree(path)
	if err != nil {
		return nil, fmt.Errorf("open worktree %q: %w", path, err)
	}
	return svc, nil
}

func worktreePathForBranch(worktrees []git.Worktree, branch string) string {
	for _, wt := range worktrees {
		if wt.Branch == branch {
			return wt.Path
		}
	}
	return ""
}

type closeoutMergeResult struct {
	mergeType    string
	original     string
	originalHead string
	restore      bool
}

func mergeForCloseout(ctx context.Context, gitSvc *git.Service, feature, base, featureHead string) (closeoutMergeResult, error) {
	original, err := gitSvc.CurrentBranch()
	if err != nil {
		return closeoutMergeResult{}, fmt.Errorf("read merge worktree branch: %w", err)
	}
	originalHead, err := gitSvc.HeadHash()
	if err != nil {
		return closeoutMergeResult{}, fmt.Errorf("read merge worktree head: %w", err)
	}
	if original != base {
		if checkoutErr := gitSvc.CheckoutBranch(base); checkoutErr != nil {
			return closeoutMergeResult{}, fmt.Errorf("check out base branch %q: %w", base, checkoutErr)
		}
	}
	baseHead, err := gitSvc.HeadHash()
	if err != nil {
		return closeoutMergeResult{}, fmt.Errorf("read base branch head: %w", err)
	}
	if err = gitSvc.MergeBranchCommitContext(ctx, feature, featureHead); err != nil {
		return closeoutMergeResult{}, closeoutMergeError(gitSvc, original, originalHead, feature, base, err)
	}
	mergedHead, err := gitSvc.HeadHash()
	if err != nil {
		return closeoutMergeResult{}, fmt.Errorf("read merged branch head: %w", err)
	}
	result := closeoutMergeResult{original: original, originalHead: originalHead, restore: original != base && original != feature}
	if mergedHead == baseHead {
		result.mergeType = "already up to date"
		return result, nil
	}
	if mergedHead == featureHead {
		result.mergeType = "fast-forward"
		return result, nil
	}
	result.mergeType = "merge commit"
	return result, nil
}

func restoreMergeWorktree(gitSvc *git.Service, result closeoutMergeResult, priorErr error) error {
	if !result.restore {
		return priorErr
	}
	if restoreErr := restoreCheckout(gitSvc, result.original, result.originalHead); restoreErr != nil {
		if priorErr != nil {
			return fmt.Errorf("%w; additionally failed to restore the merge worktree: %w", priorErr, restoreErr)
		}
		return fmt.Errorf("merge succeeded but failed to restore the merge worktree: %w", restoreErr)
	}
	return priorErr
}

func closeoutMergeError(gitSvc *git.Service, original, originalHead, feature, base string, mergeErr error) error {
	if original != base {
		if restoreErr := restoreCheckout(gitSvc, original, originalHead); restoreErr != nil {
			return fmt.Errorf("merge %q into %q failed: %w; additionally failed to restore the merge worktree: %w", feature, base, mergeErr, restoreErr)
		}
	}
	if errors.Is(mergeErr, git.ErrMergeConflict) {
		return fmt.Errorf("merge %q into %q conflicted and was aborted; resolve the branches and rerun --merge: %w", feature, base, mergeErr)
	}
	return fmt.Errorf("merge %q into %q failed: %w", feature, base, mergeErr)
}

func restoreCheckout(gitSvc *git.Service, branch, head string) error {
	target := branch
	if target == "" {
		target = head
	}
	if err := gitSvc.CheckoutBranch(target); err != nil {
		return fmt.Errorf("restore %q: %w", target, err)
	}
	return nil
}

// cleanupMergedWorktree removes the feature worktree after a successful merge. it is a no-op when
// the feature has no worktree of its own, either because it was never checked out or because it
// shares the primary worktree.
func cleanupMergedWorktree(targets mergeTargets, feature string) error {
	if targets.removableWorktree() == "" {
		return nil
	}
	// Revalidate both identity and cleanliness immediately before removal. The standalone
	// close-out command must never force-delete an unrelated or newly modified worktree.
	latest, err := targets.mergeSvc.Worktrees()
	if err != nil {
		return fmt.Errorf("revalidate feature worktree: %w", err)
	}
	if registeredPath := worktreePathForBranch(latest, feature); filepath.Clean(registeredPath) != filepath.Clean(targets.featurePath) {
		return fmt.Errorf("refuse to remove worktree %q: it is no longer registered for branch %q", targets.featurePath, feature)
	}
	dirty, err := targets.featureSvc.IsDirtyAll()
	if err != nil {
		return fmt.Errorf("recheck feature worktree: %w", err)
	}
	if dirty {
		return fmt.Errorf("refuse to remove modified feature worktree at %s", targets.featurePath)
	}
	if err := leaveWorktreeBeforeRemoval(targets.featurePath, targets.mergeSvc.Root()); err != nil {
		return err
	}
	if err := targets.mergeSvc.RemoveWorktreeSafe(targets.featurePath); err != nil {
		return fmt.Errorf("clean up worktree for %q: %w", feature, err)
	}
	return nil
}

func leaveWorktreeBeforeRemoval(featurePath, destination string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("read current directory before worktree cleanup: %w", err)
	}
	if !pathWithin(cwd, featurePath) {
		return nil
	}
	if err := os.Chdir(destination); err != nil {
		return fmt.Errorf("leave feature worktree before cleanup: %w", err)
	}
	return nil
}

func pathWithin(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// runPRCommand pushes the feature branch and creates a GitHub pull request.
// The completion pill is retained until gh confirms that the PR was created.
func runPRCommand(ctx context.Context, gitSvc *git.Service, explicitBase string, target closeoutTarget,
	rep cmuxStatusClearer, stdout io.Writer) error {
	ghPath, err := exec.LookPath("gh")
	if err != nil {
		return errors.New("--pr requires GitHub CLI (gh) in PATH; install it from https://cli.github.com/")
	}

	explicit := strings.TrimSpace(target.identifier) != ""
	branch, err := resolveCloseoutBranch(gitSvc, target, "--pr")
	if err != nil {
		return err
	}
	base, err := gitSvc.ResolveBaseBranch(explicitBase)
	if err != nil {
		return fmt.Errorf("resolve PR base branch: %w", err)
	}
	if branch == base {
		if explicit {
			return fmt.Errorf("feature %q is already the base branch; name a different feature", base)
		}
		return fmt.Errorf("current branch %q is already the base branch; check out the feature branch first", base)
	}
	// measured against the branch tip, so an explicitly named feature need not be checked out
	stats, err := gitSvc.BranchDiffStats(base, branch)
	if err != nil {
		return fmt.Errorf("calculate PR diff stats for %q against %q: %w", branch, base, err)
	}

	title, body, err := buildPRTitleBody(gitSvc.Root(), target.plansDir, branch, stats)
	if err != nil {
		return err
	}
	if metadataErr := validatePRMetadata(title, body); metadataErr != nil {
		return metadataErr
	}
	repoSpec, err := validateGitHubOrigin(ctx, ghPath, gitSvc)
	if err != nil {
		return err
	}
	if pushErr := gitSvc.PushContext(ctx, branch); pushErr != nil {
		return fmt.Errorf("push PR branch: %w", pushErr)
	}

	cmd := exec.CommandContext(ctx, ghPath, "pr", "create", "--repo", repoSpec,
		"--base", base, "--head", branch,
		"--title", title, "--body-file", "-")
	cmd.Dir = gitSvc.Root()
	cmd.Stdin = strings.NewReader(body)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		details := strings.TrimSpace(stderr.String() + "\n" + string(out))
		return fmt.Errorf("create GitHub PR: %w: %s", err, details)
	}
	prURL := strings.TrimSpace(string(out))
	if prURL != "" {
		fmt.Fprintln(stdout, prURL)
	}
	if rep != nil {
		rep.Clear()
	}
	return nil
}

func validateGitHubOrigin(ctx context.Context, ghPath string, gitSvc *git.Service) (string, error) {
	originURL, err := gitSvc.OriginURL()
	if err != nil {
		return "", fmt.Errorf("validate GitHub origin: %w", err)
	}
	repoSpec, err := githubRepoSpec(originURL)
	if err != nil {
		return "", fmt.Errorf("validate GitHub origin: %w", err)
	}
	pushURLs, err := gitSvc.OriginPushURLs()
	if err != nil {
		return "", fmt.Errorf("validate GitHub origin: %w", err)
	}
	for _, pushURL := range pushURLs {
		pushRepoSpec, pushErr := githubRepoSpec(pushURL)
		if pushErr != nil {
			return "", fmt.Errorf("validate GitHub origin push destination: %w", pushErr)
		}
		if !strings.EqualFold(pushRepoSpec, repoSpec) {
			return "", fmt.Errorf("validate GitHub origin: push destination %q does not match PR repository %q", pushRepoSpec, repoSpec)
		}
	}
	cmd := exec.CommandContext(ctx, ghPath, "repo", "view", repoSpec,
		"--json", "nameWithOwner", "--jq", ".nameWithOwner")
	cmd.Dir = gitSvc.Root()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("validate GitHub origin with gh: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if strings.TrimSpace(string(out)) == "" {
		return "", errors.New("validate GitHub origin with gh: repository lookup returned no name")
	}
	return repoSpec, nil
}

func githubRepoSpec(remoteURL string) (string, error) {
	remoteURL = strings.TrimSpace(remoteURL)
	if remoteURL == "" {
		return "", errors.New("origin URL is empty")
	}

	var host, repoPath string
	if strings.Contains(remoteURL, "://") {
		parsed, err := url.Parse(remoteURL)
		if err != nil || parsed.Hostname() == "" {
			return "", errors.New("origin is not a valid hosted repository URL")
		}
		host, repoPath = parsed.Hostname(), parsed.Path
	} else {
		colon := strings.IndexByte(remoteURL, ':')
		if colon <= 0 || strings.ContainsAny(remoteURL[:colon], `/\\`) {
			return "", errors.New("origin is not a GitHub repository URL")
		}
		host = remoteURL[:colon]
		if at := strings.LastIndexByte(host, '@'); at >= 0 {
			host = host[at+1:]
		}
		repoPath = remoteURL[colon+1:]
	}

	parts := strings.Split(strings.TrimSuffix(strings.Trim(repoPath, "/"), ".git"), "/")
	if host == "" || len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", errors.New("origin is not a GitHub owner/repository URL")
	}
	if strings.EqualFold(host, "github.com") {
		return parts[0] + "/" + parts[1], nil
	}
	return host + "/" + parts[0] + "/" + parts[1], nil
}

func runCloseoutCommand(ctx context.Context, o opts, cfg *config.Config, colors *progress.Colors) error {
	if cfg.VcsCommand == "" || cfg.VcsCommand == "git" {
		if _, err := os.Stat(".git"); err != nil {
			return errors.New("must run from repository root (no .git directory found); run from the repo root")
		}
	}
	gitSvc, err := openGitService(colors, cfg.VcsCommand)
	if err != nil {
		return fmt.Errorf("open git repo: %w", err)
	}
	rep := cmux.New("", cmux.Models{})
	target := closeoutTarget{identifier: o.PlanFile, plansDir: cfg.PlansDir}
	if mergeRequested(o) {
		return runMergeCommand(ctx, gitSvc, o.Merge, target, rep, os.Stdout)
	}
	return runPRCommand(ctx, gitSvc, o.PR, target, rep, os.Stdout)
}

// buildPRTitleBody derives PR metadata from the plan associated with branch. Completed plans
// are preferred, while the active plans directory covers worktree runs whose archival commit
// exists only on the base branch.
func buildPRTitleBody(repoRoot, plansDir, branch string, stats git.DiffStats) (title, body string, err error) {
	title = branch
	planPath, err := findPRPlan(repoRoot, plansDir, branch)
	if err != nil {
		return "", "", err
	}
	var overview string
	if planPath != "" {
		content, readErr := readPRPlan(repoRoot, planPath)
		if readErr != nil {
			return "", "", fmt.Errorf("read PR plan: %w", readErr)
		}
		title, overview = parsePRPlan(string(content), branch)
	}

	statsText := fmt.Sprintf("## Changes\n\n- Files changed: %d\n- Additions: %d\n- Deletions: %d", stats.Files, stats.Additions, stats.Deletions)
	if overview == "" {
		return title, statsText, nil
	}
	return title, overview + "\n\n" + statsText, nil
}

const (
	maxPRPlanSize           int64 = 1 << 20
	maxPRProgressHeaderSize int64 = 64 << 10
	maxPRTitleRunes               = 256
	maxPRBodyRunes                = 65_536
)

func validatePRMetadata(title, body string) error {
	if utf8.RuneCountInString(title) > maxPRTitleRunes {
		return fmt.Errorf("PR title exceeds %d-character GitHub limit", maxPRTitleRunes)
	}
	if utf8.RuneCountInString(body) > maxPRBodyRunes {
		return fmt.Errorf("PR body exceeds %d-character GitHub limit", maxPRBodyRunes)
	}
	return nil
}

// readPRPlan reads a bounded regular file below repoRoot without following symlinked path
// components. The identity check also rejects a file swapped between inspection and open.
func readPRPlan(repoRoot, path string) ([]byte, error) {
	root, err := filepath.Abs(repoRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve repository root: %w", err)
	}
	target, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve plan path: %w", err)
	}
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, errors.New("plan path is outside repository root")
	}

	current := root
	var inspected os.FileInfo
	for component := range strings.SplitSeq(rel, string(filepath.Separator)) {
		current = filepath.Join(current, component)
		inspected, err = os.Lstat(current)
		if err != nil {
			return nil, fmt.Errorf("inspect plan path: %w", err)
		}
		if inspected.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("plan path contains a symlink")
		}
	}
	if inspected == nil || !inspected.Mode().IsRegular() {
		return nil, errors.New("plan is not a regular file")
	}
	if inspected.Size() > maxPRPlanSize {
		return nil, fmt.Errorf("plan exceeds %d-byte size limit", maxPRPlanSize)
	}

	f, err := os.Open(target) //nolint:gosec // every component was constrained and inspected above
	if err != nil {
		return nil, fmt.Errorf("open plan: %w", err)
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect opened plan: %w", err)
	}
	if !opened.Mode().IsRegular() || !os.SameFile(inspected, opened) {
		return nil, errors.New("plan changed while being opened")
	}
	content, err := io.ReadAll(io.LimitReader(f, maxPRPlanSize+1))
	if err != nil {
		return nil, fmt.Errorf("read plan: %w", err)
	}
	if int64(len(content)) > maxPRPlanSize {
		return nil, fmt.Errorf("plan exceeds %d-byte size limit", maxPRPlanSize)
	}
	return content, nil
}

// plansDirPath resolves the configured plans directory against repoRoot. an empty setting falls
// back to the embedded default so PR metadata keeps working without loaded configuration, and an
// absolute setting inside the repository is re-anchored at repoRoot so the resulting plan paths
// stay comparable to the root Git reports even when the checkout sits behind a symlink.
func plansDirPath(repoRoot, plansDir string) string {
	if plansDir == "" {
		plansDir = filepath.Join("docs", "plans")
	}
	if !filepath.IsAbs(plansDir) {
		return filepath.Join(repoRoot, plansDir)
	}
	root, rootErr := filepath.EvalSymlinks(repoRoot)
	dir, dirErr := filepath.EvalSymlinks(plansDir)
	if rootErr != nil || dirErr != nil {
		return plansDir
	}
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return plansDir
	}
	return filepath.Join(repoRoot, rel)
}

func findPRPlan(repoRoot, plansDir, branch string) (string, error) {
	recordedPath, err := findRecordedPRPlan(repoRoot, branch)
	if err != nil {
		return "", err
	}
	if recordedPath != "" {
		return recordedPath, nil
	}

	root := plansDirPath(repoRoot, plansDir)
	if !pathWithin(root, repoRoot) {
		// readPRPlan confines plan reads to the repository, so a plans_dir pointing outside it
		// yields no metadata and a stats-only PR body rather than a fatal read error
		return "", nil
	}
	dirs := []string{filepath.Join(root, "completed"), root}
	var fallbackPath string
	for _, dir := range dirs {
		exactPath, candidateFallback, findErr := findPRPlanInDir(repoRoot, dir, branch)
		if findErr != nil {
			return "", findErr
		}
		if exactPath != "" {
			return exactPath, nil
		}
		if fallbackPath == "" {
			fallbackPath = candidateFallback
		}
	}
	return fallbackPath, nil
}

func findPRPlanInDir(repoRoot, dir, branch string) (exactPath, fallbackPath string, err error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return "", "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("list PR plans in %q: %w", dir, err)
	}

	var exactTime time.Time
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".md" || plan.ExtractBranchName(entry.Name()) != branch {
			continue
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			return "", "", fmt.Errorf("inspect associated PR plan %q: %w", entry.Name(), infoErr)
		}
		if exactPath == "" || info.ModTime().After(exactTime) {
			exactPath, exactTime = filepath.Join(dir, entry.Name()), info.ModTime()
		}
	}

	var fallbackTime time.Time
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".md" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		content, readErr := readPRPlan(repoRoot, path)
		if readErr != nil {
			continue
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			continue
		}
		if planMentionsBranch(string(content), branch) &&
			(fallbackPath == "" || info.ModTime().After(fallbackTime)) {
			fallbackPath, fallbackTime = path, info.ModTime()
		}
	}
	return exactPath, fallbackPath, nil
}

// findRecordedPRPlan uses the progress header's exact branch-to-plan association. This covers
// arbitrary --branch overrides without guessing from a slash-delimited branch basename.
func findRecordedPRPlan(repoRoot, branch string) (string, error) {
	assocs, err := readProgressAssociations(repoRoot)
	if err != nil {
		return "", err
	}
	var matched string
	var matchedTime time.Time
	for _, assoc := range assocs {
		// planPath is empty when the recorded plan names no file inside the repository; the PR body
		// is read from it, so only a repo-contained path is usable here
		if assoc.branch != branch || assoc.planPath == "" {
			continue
		}
		if matched == "" || assoc.modTime.After(matchedTime) {
			matched, matchedTime = assoc.planPath, assoc.modTime
		}
	}
	return matched, nil
}

// recordedBranchForPlan returns the branch the most recent run associated with planFile, or an
// empty string when no run recorded it. A plan filename only implies a branch when the run used
// no --branch override, so close-out resolution consults the recorded association first rather
// than merging and deleting an unrelated branch that happens to carry the derived name.
// progressRoots are the checkouts that can hold the records, which progressRecordRoots resolves;
// the newest matching record across all of them wins.
func recordedBranchForPlan(progressRoots []string, planFile string) (string, error) {
	target := planAssociationKey(planFile)
	var assocs []progressAssociation
	for _, root := range progressRoots {
		found, err := readProgressAssociations(root)
		if err != nil {
			return "", err
		}
		assocs = append(assocs, found...)
	}
	var matched string
	var matchedTime time.Time
	for _, assoc := range assocs {
		// match the path the record actually names, not its repo-contained resolution: this lookup
		// only needs the branch, so a plan the repository does not contain - an out-of-tree
		// plans_dir, or a checkout moved since the run - must still supply it
		if planAssociationKey(assoc.recordedPlan) != target {
			continue
		}
		// a review-only or plan-creation run over the same plan writes its own record, in the same
		// directory and with a later mtime than the run that created the branch. its Branch header
		// names whatever was checked out then, so honoring it would resolve the close-out to an
		// unrelated branch and merge and delete it
		if !recordedBranchIsFeature(assoc.mode) {
			continue
		}
		if matched == "" || assoc.modTime.After(matchedTime) {
			matched, matchedTime = assoc.branch, assoc.modTime
		}
	}
	return matched, nil
}

// planAssociationKey reduces a plan path to the identity used to match a progress record against a
// resolved plan file. loopai already keys plans by filename everywhere - branch derivation, PR plan
// lookup, and completed/ archiving all do - and matching that way survives the ways the two paths
// legitimately diverge: a case-insensitive filesystem handing back the caller's spelling, the
// record naming the completed/ copy while the lookup found the active one, the record living in
// the primary checkout while the lookup ran in a linked worktree, and a plans directory outside
// the repository. Comparing absolute paths misses all of them, and a miss is the dangerous
// direction: it falls back to deriving the branch from the filename, which is exactly what the
// recorded value exists to override.
func planAssociationKey(path string) string {
	return strings.ToLower(filepath.Base(path))
}

// progressAssociation pairs a branch with the plan file a past run recorded for it.
type progressAssociation struct {
	recordedPlan string // plan path exactly as the record names it
	planPath     string // recordedPlan resolved inside repoRoot, empty when it names no file there
	branch       string
	mode         string // run mode from the record header, empty when the record names none
	modTime      time.Time
}

// recordedBranchIsFeature reports whether a record's mode means its Branch header names the
// branch that run created. only task-executing modes create one; --review, --codex-only, and
// plan creation all record whatever branch happened to be checked out, which is unrelated to the
// plan and may be the base branch or "unknown" on detached HEAD. an unrecognized or absent mode
// is accepted, since dropping a valid association falls back to deriving the branch from the plan
// filename - exactly what the recorded value exists to override.
func recordedBranchIsFeature(mode string) bool {
	switch processor.Mode(mode) {
	case processor.ModeReview, processor.ModeCodexOnly, processor.ModePlan:
		return false
	default:
		return true
	}
}

// readProgressAssociations collects the branch-to-plan pairings recorded in progress headers.
// records naming no plan at all are skipped; a plan the repository does not contain still yields a
// pair with an empty planPath, since only the PR-metadata consumer needs a readable in-repo file.
func readProgressAssociations(repoRoot string) ([]progressAssociation, error) {
	dir := filepath.Join(repoRoot, ".loopai", "progress")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list progress records: %w", err)
	}

	var assocs []progressAssociation
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".txt" || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		info, infoErr := entry.Info()
		if infoErr != nil {
			return nil, fmt.Errorf("inspect progress record %q: %w", entry.Name(), infoErr)
		}
		if !info.Mode().IsRegular() {
			continue
		}
		f, openErr := os.Open(path) //nolint:gosec // direct regular-file child of the fixed progress directory
		if openErr != nil {
			return nil, fmt.Errorf("open progress record %q: %w", entry.Name(), openErr)
		}
		content, readErr := io.ReadAll(io.LimitReader(f, maxPRProgressHeaderSize))
		closeErr := f.Close()
		if readErr != nil {
			return nil, fmt.Errorf("read progress record %q: %w", entry.Name(), readErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close progress record %q: %w", entry.Name(), closeErr)
		}
		planPath, branch, mode := parseProgressAssociation(string(content))
		if branch == "" || planPath == "" || planPath == "(no plan - review only)" {
			continue
		}
		assocs = append(assocs, progressAssociation{
			recordedPlan: planPath,
			planPath:     resolveRecordedPlan(repoRoot, planPath),
			branch:       branch,
			mode:         mode,
			modTime:      info.ModTime(),
		})
	}
	return assocs, nil
}

// parseProgressAssociation reads the association fields from a progress record's header block.
// Parsing stops at the dashed separator or blank line that closes the header, never at a set of
// collected fields: Mode is absent from records written before the header carried it, so requiring
// it would run the scan into the log body, where executor output beginning with "Plan: " or
// "Branch: " would overwrite the header values and misdirect the close-out to another branch.
func parseProgressAssociation(content string) (planPath, branch, mode string) {
	for line := range strings.SplitSeq(content, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "---") {
			break
		}
		switch {
		case strings.HasPrefix(line, "Plan: "):
			planPath = strings.TrimSpace(strings.TrimPrefix(line, "Plan: "))
		case strings.HasPrefix(line, "Branch: "):
			branch = strings.TrimSpace(strings.TrimPrefix(line, "Branch: "))
		case strings.HasPrefix(line, "Mode: "):
			mode = strings.TrimSpace(strings.TrimPrefix(line, "Mode: "))
		}
	}
	return planPath, branch, mode
}

func resolveRecordedPlan(repoRoot, recorded string) string {
	candidate := recorded
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(repoRoot, candidate)
	}
	if filepath.Base(filepath.Dir(candidate)) != "completed" {
		completed := filepath.Join(filepath.Dir(candidate), "completed", filepath.Base(candidate))
		if resolved := recordedPlanInRepo(repoRoot, completed); resolved != "" {
			return resolved
		}
	}
	return recordedPlanInRepo(repoRoot, candidate)
}

// recordedPlanInRepo returns path re-anchored at repoRoot when it names a regular file inside the
// repository, and an empty string otherwise. A progress record stores the path the run saw, while
// Git reports worktree roots with symlinks resolved, so a purely lexical containment test drops
// valid records wherever the checkout sits behind a symlink - every macOS temporary directory,
// via /var and /tmp. Re-anchoring rather than returning the recorded spelling keeps the result
// acceptable to readPRPlan's own containment check.
func recordedPlanInRepo(repoRoot, path string) string {
	// Lstat, not Stat: a symlinked plan is not a regular file and must not resolve
	if info, err := os.Lstat(path); err != nil || !info.Mode().IsRegular() {
		return ""
	}
	if pathWithin(path, repoRoot) {
		return path
	}
	root, rootErr := filepath.EvalSymlinks(repoRoot)
	dir, dirErr := filepath.EvalSymlinks(filepath.Dir(path))
	if rootErr != nil || dirErr != nil {
		return ""
	}
	resolved := filepath.Join(dir, filepath.Base(path))
	rel, err := filepath.Rel(root, resolved)
	if err != nil || !pathWithin(resolved, root) {
		return ""
	}
	return filepath.Join(repoRoot, rel)
}

func planMentionsBranch(content, branch string) bool {
	if branch == "" {
		return false
	}
	for searchFrom := 0; searchFrom+len(branch) <= len(content); {
		relative := strings.Index(content[searchFrom:], branch)
		if relative < 0 {
			return false
		}
		idx := searchFrom + relative
		beforeOK := branchBoundaryBefore(content, idx)
		after := idx + len(branch)
		afterOK := branchBoundaryAfter(content, after)
		if beforeOK && afterOK {
			return true
		}
		searchFrom = idx + 1
	}
	return false
}

func branchBoundaryBefore(content string, idx int) bool {
	if idx == 0 {
		return true
	}
	if content[idx-1] != '.' {
		return !isBranchTokenByte(content[idx-1])
	}
	return idx == 1 || !isBranchTokenByte(content[idx-2])
}

func branchBoundaryAfter(content string, idx int) bool {
	if idx == len(content) {
		return true
	}
	if content[idx] != '.' {
		return !isBranchTokenByte(content[idx])
	}
	return idx+1 == len(content) || !isBranchTokenByte(content[idx+1])
}

func isBranchTokenByte(ch byte) bool {
	return ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' ||
		ch == '-' || ch == '_' || ch == '.' || ch == '/'
}

func parsePRPlan(content, fallbackTitle string) (title, overview string) {
	title = fallbackTitle
	lines := strings.Split(content, "\n")
	for _, line := range lines {
		if heading, ok := strings.CutPrefix(line, "# "); ok && strings.TrimSpace(heading) != "" {
			title = strings.TrimSpace(heading)
			break
		}
	}

	overviewStart := -1
	for idx, line := range lines {
		if strings.TrimSpace(line) == "## Overview" {
			overviewStart = idx + 1
			continue
		}
		if overviewStart >= 0 && strings.HasPrefix(strings.TrimSpace(line), "## ") {
			overview = strings.TrimSpace(strings.Join(lines[overviewStart:idx], "\n"))
			return title, overview
		}
	}
	if overviewStart >= 0 {
		overview = strings.TrimSpace(strings.Join(lines[overviewStart:], "\n"))
	}
	return title, overview
}

// initLocal creates .loopai/ config directory in current project.
// requires running from repository root to avoid creating config in a subdirectory
// that would never be found during normal execution.
func initLocal(configDir string) error {
	// check for the Git repository root marker to prevent creating
	// config in subdirectories where loopai won't find it during normal execution.
	// when a custom VCS backend is configured (not "git"), validate the repo
	// by running the configured command with rev-parse --show-toplevel.
	hasGit := fileExists(".git")
	if !hasGit {
		cfg, loadErr := config.LoadReadOnly(configDir)
		if loadErr != nil || cfg.VcsCommand == "" || cfg.VcsCommand == "git" {
			return errors.New("must run from repository root (no .git directory found); cd to the repository root before running --init")
		}
		// custom VCS backend configured — validate repo root using the backend command
		if validErr := validateRepoRoot(cfg.VcsCommand); validErr != nil {
			return fmt.Errorf("must run from repository root (%w)", validErr)
		}
	}

	const localDir = ".loopai"
	if err := config.InitLocal(localDir); err != nil {
		return fmt.Errorf("init local config: %w", err)
	}
	fmt.Printf("local config initialized in %s/\n", localDir)
	return nil
}

// fileExists returns true if the path exists (file or directory).
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// validateRepoRoot runs the configured VCS command to check we're at the repo root.
// stricter than newExternalBackend (which only validates "inside a repo"):
// here we require cwd == repo root so .loopai/ is created at the right level.
func validateRepoRoot(vcsCommand string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, vcsCommand, "rev-parse", "--show-toplevel")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("custom VCS backend %q cannot validate repository: %w\n%s", vcsCommand, err, strings.TrimSpace(string(out)))
	}
	root := strings.TrimSpace(string(out))
	if root == "" {
		return errors.New("VCS returned empty repository root")
	}
	// resolve symlinks for consistent comparison (macOS /var -> /private/var)
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("resolve repo root: %w", err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("get working directory: %w", err)
	}
	cwd, err = filepath.EvalSymlinks(cwd)
	if err != nil {
		return fmt.Errorf("resolve working directory: %w", err)
	}
	if root != cwd {
		return fmt.Errorf("not at repository root (root is %s); cd %q and re-run", root, root)
	}
	return nil
}

// dumpDefaults extracts raw embedded defaults to the specified directory.
func dumpDefaults(dir string) error {
	if err := config.DumpDefaults(dir); err != nil {
		return fmt.Errorf("dump defaults: %w", err)
	}
	fmt.Printf("defaults extracted to %s\n", dir)
	return nil
}

// toRelPath converts an absolute path to relative (from cwd). returns original on error.
func toRelPath(p string) string {
	cwd, err := os.Getwd()
	if err != nil {
		return p
	}
	rel, err := filepath.Rel(cwd, p)
	if err != nil {
		return p
	}
	// if relative path escapes too far (e.g. worktree -> main repo), use absolute path instead
	if strings.HasPrefix(rel, "../../") {
		return p
	}
	return rel
}

// isResetOnly returns true if --reset was the only meaningful flag/arg specified.
// this allows reset to work standalone (exit after reset) while also supporting
// combined usage like "loopai --reset docs/plans/feature.md".
func isResetOnly(o opts) bool {
	return o.PlanFile == "" &&
		!o.Review &&
		!o.ExternalOnly &&
		!o.CodexOnly &&
		!o.TasksOnly &&
		!o.Serve &&
		o.PlanDescription == "" &&
		len(o.Watch) == 0 &&
		o.DumpDefaults == "" &&
		!o.Init
}

// startInterruptWatcher prints immediate feedback when context is canceled.
// if graceful shutdown doesn't complete within 5 seconds, force exits.
// cleanup, if not nil, is called only on the force-exit (5s timeout) path before
// os.Exit; it gets an additional bounded wait (2s, via runCleanupBounded) so a
// stuck cleanup cannot prevent the exit — worst-case total is ~7s after cancel.
// returns a cleanup function that must be called (via defer) to prevent goroutine leaks.
func startInterruptWatcher(ctx context.Context, cleanup func()) func() {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			fmt.Fprintf(os.Stderr, "\ninterrupting... (force exit in 5s)\n")
			select {
			case <-time.After(5 * time.Second):
				fmt.Fprintf(os.Stderr, "force exit\n")
				runCleanupBounded(cleanup, 2*time.Second)
				os.Exit(1)
			case <-done:
			}
		case <-done:
		}
	}()
	return func() { close(done) }
}

// forceExitCleanup builds the callback the interrupt watcher runs right before os.Exit on the
// force-exit path. the holders run concurrently but the callback waits for all of them: a stuck
// git worktree removal must not be what leaves the cmux spinner hanging forever, and vice versa,
// while runCleanupBounded's timeout bounds the total. spawning a holder without waiting would
// simply lose the race against os.Exit, which never runs defers.
func forceExitCleanup(restoreTerminal func(), holders ...*cleanupHolder) func() {
	return func() {
		restoreTerminal()
		var wg sync.WaitGroup
		for _, h := range holders {
			wg.Go(h.call)
		}
		wg.Wait()
	}
}

// runCleanupBounded runs cleanup in a separate goroutine and waits for it to
// finish, but no longer than timeout. this bounds the force-exit path: cleanup
// shares a sync.Once with the graceful shutdown's deferred worktree cleanup, so
// when that cleanup is already in flight and stuck (e.g. a hanging git worktree
// remove), calling it directly would block inside Once.Do forever and os.Exit
// would never be reached.
func runCleanupBounded(cleanup func(), timeout time.Duration) {
	if cleanup == nil {
		return
	}
	doneCh := make(chan struct{})
	go func() {
		cleanup()
		close(doneCh)
	}()
	select {
	case <-doneCh:
	case <-time.After(timeout):
		fmt.Fprintf(os.Stderr, "cleanup did not finish in time, exiting anyway\n")
	}
}

// applyCLIOverrides applies CLI flag overrides to config.
// uses opts.*Set bools (populated by markFlagsSet) to detect explicitly-set zero values
// so that e.g. --idle-timeout 0 can disable a non-zero config value.
// returns an error if a post-merge validation fails (e.g. --pass-claude-md requires
// codex executor, which may come from config file rather than CLI).
func applyCLIOverrides(o opts, cfg *config.Config) error {
	if o.SkipFinalize {
		cfg.FinalizeEnabled = false
	}
	if o.PreserveAnthropicAPIKey {
		cfg.PreserveAnthropicAPIKey = true
	}
	cfg.Orca = enabledByCLI(cfg.Orca, o.Orca)
	if o.Worktree {
		cfg.WorktreeEnabled = true
	}
	if o.Commit && !cfg.WorktreeEnabled {
		return errors.New("--commit requires --worktree")
	}
	if o.Wait > 0 || (o.Wait == 0 && o.waitSet) {
		cfg.WaitOnLimit = o.Wait
		cfg.WaitOnLimitSet = true
	}
	if o.SessionTimeout > 0 || (o.SessionTimeout == 0 && o.sessionTimeoutSet) {
		cfg.SessionTimeout = o.SessionTimeout
		cfg.SessionTimeoutSet = true
	}
	if o.IdleTimeout > 0 || (o.IdleTimeout == 0 && o.idleTimeoutSet) {
		cfg.IdleTimeout = o.IdleTimeout
		cfg.IdleTimeoutSet = true
	}
	if o.claudeCommandSet {
		cfg.ClaudeCommand = o.ClaudeCommand
	}
	if o.claudeArgsSet {
		cfg.ClaudeArgs = o.ClaudeArgs
		cfg.ClaudeArgsSet = true
	}
	if o.codexArgsSet {
		// assigning unconditionally under the set guard is what clears an inherited
		// config value on an explicit --codex-args=; empty extras append nothing, so
		// unlike claude args there is nothing downstream that needs "set" vs "unset"
		cfg.CodexArgs = o.CodexArgs
	}
	if err := applyExternalReviewCLIOverrides(o, cfg); err != nil {
		return err
	}
	if o.customReviewScriptSet {
		cfg.CustomReviewScript = o.CustomReviewScript
	}
	return applyCodexOverrides(o, cfg, os.Stderr)
}

func enabledByCLI(configured, requested bool) bool {
	return configured || requested
}

func applyExternalReviewCLIOverrides(o opts, cfg *config.Config) error {
	if err := validateExternalReviewFlags(o); err != nil {
		return err
	}
	if o.externalReviewToolSet {
		cfg.ExternalReviewTool = o.ExternalReviewTool
	}
	if o.externalReviewModelSet {
		cfg.ExternalReviewModel = o.ExternalReviewModel
		cfg.ExternalReviewModelSet = true
	}
	if o.externalReviewersSet {
		cfg.ExternalReviewers = o.ExternalReviewers
		cfg.ExternalReviewersSet = true
	}
	return nil
}

// applyCodexOverrides applies --codex / --pass-claude-md CLI flags after config
// merging. The name is retained for compatibility with existing callers; external
// review selection is now resolved separately and is valid for either primary.
func applyCodexOverrides(o opts, cfg *config.Config, warnW io.Writer) error {
	_ = warnW
	if o.Codex {
		cfg.Executor = config.ExecutorCodex
	}
	if o.PassClaudeMd {
		cfg.PassClaudeMd = true
	}
	if cfg.PassClaudeMd && cfg.Executor != config.ExecutorCodex {
		return errors.New("--pass-claude-md requires --codex (or executor = codex in config)")
	}
	return nil
}

// isFlagSet returns true if the named CLI flag was explicitly provided on the command line.
func isFlagSet(parser *flags.Parser, name string) bool {
	if parser == nil {
		return false
	}
	opt := parser.FindOptionByLongName(name)
	// IsSet alone also reports true when go-flags applied a `default:"..."` tag value
	// (e.g. max-external-iterations, review-patience), which would make every run look
	// like an explicit CLI invocation; only a non-default set counts as user-provided.
	return opt != nil && opt.IsSet() && !opt.IsSetDefault()
}

// resolveMaxIterations returns the effective max iterations value.
// precedence: explicit CLI flag > config file > built-in default (50).
// CLI value of 0 means "not set" (go-flags default when no default tag).
func resolveMaxIterations(cliValue int, cfg *config.Config) int {
	if cliValue > 0 {
		return cliValue
	}
	if cfg.MaxIterationsSet {
		return cfg.MaxIterations
	}
	return 50
}

// resolveDefaultBranch returns the default branch using precedence: CLI flag > config > auto-detect.
func resolveDefaultBranch(cliRef, configBranch, autoDetected string) string {
	if cliRef != "" {
		return cliRef
	}
	if configBranch != "" {
		return configBranch
	}
	return autoDetected
}

// remoteBranchPrefix is the remote-tracking form branch auto-detection may produce
// (e.g. "origin/main"); it names the same branch as the local ref without it.
const remoteBranchPrefix = "origin/"

// resolveBaseRefs resolves the two bases a run needs: branchBase for non-worktree branch creation
// and diffBase for review diffs and the {{DEFAULT_BRANCH}} template variable. In worktree mode,
// branchBase remains the configured/auto-detected default because worktree creation uses current
// HEAD; --base-ref is purely a diff base. branchMode says whether the run creates a branch at all:
// review modes never do, so their --base-ref is also a pure diff base.
func resolveBaseRefs(gitSvc *git.Service, cliBaseRef, configBranch string, branchMode, worktreeMode bool) (branchBase, diffBase string, err error) {
	autoDetected := gitSvc.GetDefaultBranch()
	diffBase = resolveDefaultBranch(cliBaseRef, configBranch, autoDetected)
	defaultBranch := resolveDefaultBranch("", configBranch, autoDetected)

	if !branchMode || cliBaseRef == "" || worktreeMode {
		return defaultBranch, diffBase, nil
	}

	branchBase, err = resolveBranchBase(cliBaseRef, localBranchRef(gitSvc, cliBaseRef), defaultBranch,
		getCurrentBranch(gitSvc))
	if err != nil {
		return "", "", err
	}
	return branchBase, diffBase, nil
}

// localBranchRef returns the local branch named by ref, or "" when ref names something else.
// a remote-tracking form like "origin/main" resolves to the local branch it tracks, because
// that is exactly what branch auto-detection produces and what the rest of the code compares
// against after stripping the prefix.
func localBranchRef(gitSvc *git.Service, ref string) string {
	if gitSvc.BranchExists(ref) {
		return ref
	}
	if local := strings.TrimPrefix(ref, remoteBranchPrefix); local != ref && gitSvc.BranchExists(local) {
		return local
	}
	return ""
}

// resolveBranchBase decides which ref is the base for non-worktree branch creation.
// cliRefBranch is the local branch --base-ref names, empty when it names anything else: a commit
// hash is a valid diff base but cannot be branched from, so the configured/auto-detected default
// branch stays in place. Worktree mode bypasses this function because its branch is cut from
// current HEAD and --base-ref is used only for diffs.
// a branch base is honored only from that branch itself, since branch creation cuts from HEAD:
// from another branch CreateBranchForPlan reads the mismatch as "already on a feature branch" and
// skips silently, which would leave the whole run committing onto the default branch.
func resolveBranchBase(cliRef, cliRefBranch, defaultBranch, currentBranch string) (string, error) {
	if cliRef == "" {
		return defaultBranch, nil
	}
	if cliRefBranch == "" {
		return defaultBranch, nil
	}
	if !sameBranch(currentBranch, cliRefBranch) && sameBranch(currentBranch, defaultBranch) {
		return "", fmt.Errorf("--base-ref %q names a branch but the checkout is on %q; run \"git checkout %s\" "+
			"to work off it, or drop --base-ref to work off %s", cliRef, currentBranch, cliRefBranch, currentBranch)
	}
	return cliRefBranch, nil
}

// sameBranch reports whether branch is the one ref names, tolerating the remote-tracking form
// the same way git.Service compares against the default branch.
func sameBranch(branch, ref string) bool {
	return branch == strings.TrimPrefix(ref, remoteBranchPrefix)
}

// ensureRepoHasCommits checks that the repository has at least one commit.
// If the repository is empty, prompts the user to create an initial commit.
func ensureRepoHasCommits(
	ctx context.Context,
	gitSvc *git.Service,
	stdin io.Reader,
	stdout io.Writer,
	titles *orca.Reporter,
) error {
	// track if we actually created a commit
	createdCommit := false
	promptFn := func() bool {
		return titles.WithInputWait(func() bool {
			fmt.Fprintln(stdout, "repository has no commits")
			fmt.Fprintln(stdout, "loopai needs at least one commit to create feature branches.")
			fmt.Fprintln(stdout)
			if !input.AskYesNo(ctx, "create initial commit?", stdin, stdout) {
				return false
			}
			createdCommit = true
			return true
		})
	}

	if err := gitSvc.EnsureHasCommits(promptFn); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("create initial commit: %w", ctx.Err())
		}
		return fmt.Errorf("ensure has commits: %w", err)
	}
	if createdCommit {
		fmt.Fprintln(stdout, "created initial commit")
	}
	return nil
}
