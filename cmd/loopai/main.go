// Package main provides loopai - autonomous plan execution with Claude Code.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jessevdk/go-flags"

	"github.com/umputun/ralphex/pkg/cmux"
	"github.com/umputun/ralphex/pkg/config"
	"github.com/umputun/ralphex/pkg/git"
	"github.com/umputun/ralphex/pkg/input"
	"github.com/umputun/ralphex/pkg/notify"
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
	ExternalReviewTool      string        `long:"external-review-tool" choice:"auto" choice:"claude" choice:"codex" choice:"custom" choice:"none" description:"override external review tool for this run"`
	ExternalReviewModel     string        `long:"external-review-model" description:"external review model as model[:effort]"`
	CustomReviewScript      string        `long:"custom-review-script" description:"override custom external review script for this run"`
	Review                  bool          `short:"r" long:"review" description:"skip task execution, run full review pipeline"`
	ExternalOnly            bool          `short:"e" long:"external-only" description:"skip tasks and first review; run external review, conditional post-review, and finalize"`
	CodexOnly               bool          `short:"c" long:"codex-only" description:"alias for --external-only (deprecated)"`
	TasksOnly               bool          `short:"t" long:"tasks-only" description:"run only task phase, skip all reviews"`
	BaseRef                 string        `short:"b" long:"base-ref" description:"override default branch for review diffs; a branch name also becomes the base for branch/worktree creation (branch name or commit hash)"`
	Wait                    time.Duration `long:"wait" description:"wait duration on rate limit before retry (e.g. 1h, 30m)"`
	SessionTimeout          time.Duration `long:"session-timeout" description:"per-session timeout (e.g. 30m, 1h); external Codex/custom review under a Claude primary excluded"`
	IdleTimeout             time.Duration `long:"idle-timeout" description:"kill claude/codex executor session after no output for this duration (e.g. 5m, 10m)"`
	SkipFinalize            bool          `long:"skip-finalize" description:"skip finalize step even if enabled in config"`
	PreserveAnthropicAPIKey bool          `long:"preserve-anthropic-api-key" description:"pass ANTHROPIC_API_KEY through to claude (for users authenticating Claude Code via API key rather than OAuth/keychain)"`
	Codex                   bool          `long:"codex" description:"use codex CLI as the executor for task, review, and finalize phases"`
	PassClaudeMd            bool          `long:"pass-claude-md" description:"pass project CLAUDE.md to codex via project_doc_fallback_filenames; user-level ~/.claude/CLAUDE.md is NOT auto-passed but a one-time setup hint is shown (codex executor only)"`
	Worktree                bool          `long:"worktree" description:"run in isolated git worktree"`
	Branch                  string        `long:"branch" description:"override branch name for worktree/branch creation (default: derived from plan filename)"`
	PlanDescription         string        `long:"plan" description:"create plan interactively (enter plan description)"`
	Debug                   bool          `short:"d" long:"debug" description:"enable debug logging"`
	NoColor                 bool          `long:"no-color" description:"disable color output"`
	Version                 bool          `short:"v" long:"version" description:"print version and exit"`
	Serve                   bool          `short:"s" long:"serve" description:"start web dashboard for real-time streaming"`
	Port                    int           `short:"p" long:"port" default:"8080" description:"web dashboard port"`
	Host                    string        `long:"host" default:"127.0.0.1" env:"LOOPAI_WEB_HOST" description:"web dashboard listen address"`
	Watch                   []string      `short:"w" long:"watch" description:"directories to watch for progress files (repeatable)"`
	Init                    bool          `long:"init" description:"initialize local .loopai/ config directory in current project"`
	Reset                   bool          `long:"reset" description:"interactively reset global config to embedded defaults"`
	DumpDefaults            string        `long:"dump-defaults" description:"extract raw embedded defaults to specified directory"`
	ConfigDir               string        `long:"config-dir" env:"LOOPAI_CONFIG_DIR" description:"custom config directory"`

	PlanFile string `positional-arg-name:"plan-file" description:"path to plan file (optional, uses fzf if omitted)"`

	// set by markFlagsSet after parsing; true when the flag was explicitly provided on the CLI
	waitSet           bool
	sessionTimeoutSet bool
	idleTimeoutSet    bool

	claudeCommandSet       bool
	claudeArgsSet          bool
	externalReviewToolSet  bool
	externalReviewModelSet bool
	customReviewScriptSet  bool
}

// markFlagsSet detects which duration flags were explicitly provided on the CLI
// so that --flag 0 can override a non-zero config value.
func (o *opts) markFlagsSet(parser *flags.Parser) {
	if parser == nil {
		return
	}
	o.waitSet = isFlagSet(parser, "wait")
	o.sessionTimeoutSet = isFlagSet(parser, "session-timeout")
	o.idleTimeoutSet = isFlagSet(parser, "idle-timeout")
	o.claudeCommandSet = isFlagSet(parser, "claude-command")
	o.claudeArgsSet = isFlagSet(parser, "claude-args")
	o.externalReviewToolSet = isFlagSet(parser, "external-review-tool")
	o.externalReviewModelSet = isFlagSet(parser, "external-review-model")
	o.customReviewScriptSet = isFlagSet(parser, "custom-review-script")
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
	PlanFile       string
	MainPlanFile   string // original plan path in main repo (worktree mode); empty in normal mode
	Mode           processor.Mode
	GitSvc         *git.Service
	MainGitSvc     *git.Service // main repo service for cross-boundary ops (worktree mode); nil in normal mode
	Config         *config.Config
	Colors         *progress.Colors
	DefaultBranch  string // actual default branch for branch/worktree creation (config or auto-detect)
	BaseRef        string // base reference for review diffs and templates (--base-ref override or DefaultBranch)
	NotifySvc      *notify.Service
	BranchOverride string              // branch name override (--branch flag); empty = derive from plan filename
	WtCleanup      *cleanupHolder      // worktree cleanup for interrupt handler; nil when not in worktree mode
	CmuxStop       *cleanupHolder      // cmux sidebar reset for interrupt handler; nil when not wired
	ProgressLog    *progress.Logger    // pre-created logger (worktree mode); nil in normal mode
	PhaseHolder    *status.PhaseHolder // pre-created holder (worktree mode); nil in normal mode
	ExternalReview externalReviewSelection
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
	parser.Usage = "[OPTIONS] [plan-file]"

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

	// handle positional argument
	if len(args) > 0 {
		o.PlanFile = args[0]
	}

	// setup context with signal handling
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// detect explicitly-set zero values for duration flags so --flag 0 can disable config values.
	// go-flags can't distinguish "not provided" from "set to zero" via the field alone.
	o.markFlagsSet(parser)

	if err := run(ctx, o); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, o opts) error {
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

	// print immediate feedback when context is canceled (Ctrl+C).
	// returned cleanup ensures goroutine exits when run() returns, avoiding leaks in tests.
	defer startInterruptWatcher(ctx, forceExitCleanup(restoreTerminal, cmuxStop, wtCleanup))()

	// validate conflicting flags
	if err := validateFlags(o); err != nil {
		return err
	}

	// handle early-exit flags (before full config load)
	if done, err := handleEarlyFlags(o); err != nil || done {
		return err
	}

	// load config first to get custom command paths
	cfg, err := config.Load(o.ConfigDir)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if err := applyCLIOverrides(o, cfg); err != nil { //nolint:govet // intentional shadow: scoped err for early return
		return err
	}

	// create colors from config (all colors guaranteed populated via fallback)
	colors := progress.NewColors(cfg.Colors)

	// create notification service (nil if no channels configured)
	notifySvc, err := notify.New(cfg.NotifyParams, stderrLog{})
	if err != nil {
		return fmt.Errorf("create notification service: %w", err)
	}

	// watch-only mode: --serve with watch dirs (CLI or config) and no plan file
	// runs web dashboard without plan execution, can run from any directory
	if isWatchOnlyMode(o, cfg.WatchDirs) {
		return runWatchOnly(ctx, o, cfg, colors)
	}

	mode := determineMode(o)
	externalReview, resolveErr := resolveExternalReviewSelection(o, cfg, mode)
	if resolveErr != nil {
		return resolveErr
	}
	printExternalReviewWarnings(externalReview, cfg, os.Stderr)
	externalReview, err = checkExecutionDeps(cfg, externalReview, os.Stderr)
	if err != nil {
		return err
	}
	applyEffectiveExternalReview(cfg, externalReview)

	if depErr := ctx.Err(); depErr != nil {
		return fmt.Errorf("execution context: %w", depErr)
	}

	// require running from repo root.
	// when using a non-git vcs command, skip the .git check — rely on NewService's
	// rev-parse --show-toplevel for repo validation instead (pure hg repos have no .git).
	if cfg.VcsCommand == "" || cfg.VcsCommand == "git" {
		if _, statErr := os.Stat(".git"); statErr != nil {
			return errors.New("must run from repository root (no .git directory found); run from the repo root or 'git init' for a new project")
		}
	}

	// open git repository via Service
	gitSvc, err := openGitService(colors, cfg.VcsCommand)
	if err != nil {
		return fmt.Errorf("open git repo: %w", err)
	}
	gitSvc.SetCommitTrailer(cfg.CommitTrailer)

	// ensure repository has commits (prompts to create initial commit if empty)
	if ensureErr := ensureRepoHasCommits(ctx, gitSvc, os.Stdin, os.Stdout); ensureErr != nil {
		return ensureErr
	}

	// defaultBranch is for branch/worktree creation, baseRef for review diffs and the
	// {{DEFAULT_BRANCH}} template variable. --base-ref feeds both when it names a branch;
	// a commit hash stays diff-only and is rejected outright in worktree mode.
	branchMode := modeCreatesBranch(mode)
	defaultBranch, baseRef, err := resolveBaseRefs(gitSvc, o.BaseRef, cfg.DefaultBranch,
		branchMode, cfg.WorktreeEnabled && branchMode)
	if err != nil {
		return err
	}

	// create plan selector for use by plan selection and plan mode
	selector := plan.NewSelector(cfg.PlansDir, colors)

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
			BranchOverride: o.Branch,
			ExternalReview: externalReview,
		}, selector)
	}

	return selectAndExecutePlan(ctx, o, executePlanRequest{
		Mode:           mode,
		GitSvc:         gitSvc,
		Config:         cfg,
		Colors:         colors,
		DefaultBranch:  defaultBranch,
		BaseRef:        baseRef,
		NotifySvc:      notifySvc,
		WtCleanup:      wtCleanup,
		CmuxStop:       cmuxStop,
		BranchOverride: o.Branch,
		ExternalReview: externalReview,
	}, selector)
}

// selectAndExecutePlan selects a plan file, sets up branch or worktree, and runs execution.
func selectAndExecutePlan(ctx context.Context, o opts, req executePlanRequest, selector *plan.Selector) error {
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
		if err := req.GitSvc.CreateBranchForPlan(planFile, req.DefaultBranch, req.BranchOverride); err != nil {
			return fmt.Errorf("create branch for plan: %w", err)
		}
	}

	return executePlan(ctx, o, req)
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

	description := plan.PromptDescription(ctx, os.Stdin, req.Colors)
	if description == "" {
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
		Mode:     string(req.Mode),
		PlanFile: req.PlanFile,
		Branch:   branch,
		Duration: elapsed,
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
		if req.MainPlanFile != "" {
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

// executePlan runs the main execution loop for a plan file.
// handles progress logging, web dashboard, runner execution, and post-execution tasks.
// when req.ProgressLog and req.PhaseHolder are pre-created (worktree mode), uses them directly.
// when req.MainGitSvc is set, uses it for plan file operations (plan is in main repo).
func executePlan(ctx context.Context, o opts, req executePlanRequest) error {
	branch := getCurrentBranch(req.GitSvc)

	// set up progress logger and phase holder
	plr, err := setupProgressLogger(o, req, branch)
	if err != nil {
		return err
	}
	defer plr.closeLog()

	// cmux sidebar reporter, nil and fully no-op outside cmux. Stop is also registered with the
	// interrupt handler because defers are skipped on the force-exit path.
	rep := cmux.New(req.PlanFile)
	rep.Start(ctx)
	defer rep.Stop()
	if req.CmuxStop != nil {
		req.CmuxStop.set(rep.Stop)
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
			// the spinner is already up, so the failure gets a banner too: in cmux the workspace may
			// well be in the background by now, and a run that only ever stopped spinning reads as done
			notifyCmuxCompletion(rep, req.PlanFile, branch, plr.baseLog.Elapsed(), wrapped)
			return wrapped
		}
	}

	// subscribe the sidebar to phase changes after the dashboard so both observers coexist
	plr.holder.OnChange(rep.OnPhase)

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
	r := createRunner(req, o, runnerLog, plr.holder)

	// listen for SIGQUIT (Ctrl+\) for manual break during task and review loops
	if breakCh := startBreakSignal(); breakCh != nil {
		r.SetBreakCh(breakCh)
		r.SetPauseHandler(makePauseHandler(os.Stdin, os.Stdout))
	}

	if runErr := r.Run(ctx); runErr != nil {
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
		notifyCmuxCompletion(rep, req.PlanFile, branch, plr.baseLog.Elapsed(), runErr)
		return wrapped
	}

	elapsed := plr.baseLog.Elapsed()

	// get diff stats for completion message (optional - errors logged but don't block).
	// use worktree GitSvc (has correct HEAD with committed changes).
	stats, statsErr := req.GitSvc.DiffStats(req.BaseRef)
	if statsErr != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to get diff stats: %v\n", statsErr)
	}

	sendNotification(req, branch, elapsed, stats, nil)
	notifyCmuxCompletion(rep, req.PlanFile, branch, elapsed, nil)

	// move completed plan to completed/ directory.
	// use MainGitSvc+MainPlanFile when available (worktree mode) because the plan file is in the main repo.
	// track actual success so the completion summary reflects where the plan really lives.
	planMoved := false
	if shouldMovePlan(req) {
		moveSvc := req.GitSvc
		movePlanFile := req.PlanFile
		if req.MainGitSvc != nil {
			moveSvc = req.MainGitSvc
		}
		if req.MainPlanFile != "" {
			movePlanFile = req.MainPlanFile
		}
		if moveErr := moveSvc.MovePlanToCompleted(movePlanFile); moveErr != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to move plan to completed: %v\n", moveErr)
		} else {
			planMoved = true
		}
	}

	displayStats(req, plr.baseLog, stats, elapsed, branch, planMoved)

	// clear the sidebar before the dashboard idles: with --serve the run is done but the process
	// stays alive until Ctrl+C, and a spinner left spinning reports it as still working
	rep.Stop()
	keepDashboardAlive(ctx, o, req, plr.closeLog)

	return nil
}

// runWithWorktree creates a worktree, creates the progress logger (before chdir so it lands
// in the main repo), chdirs into the worktree, and runs executePlan. On return the worktree
// is cleaned up and CWD is restored. req.WtCleanup is populated for interrupt handler use.
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
			notifyCmuxCompletion(cmux.New(req.PlanFile), req.PlanFile, branch, "", err)
		}
	}()

	wtPath, planNeedsCommit, err := req.GitSvc.CreateWorktreeForPlan(req.PlanFile, req.DefaultBranch, req.BranchOverride)
	if err != nil {
		return fmt.Errorf("create worktree: %w", err)
	}

	// register early cleanup so the interrupt handler's force-exit path (os.Exit after 5s)
	// can remove the worktree even during setup. overwritten with full cleanup after chdir.
	// RemoveWorktree is idempotent, so double-call from both early and safety-net defer is safe.
	req.WtCleanup.set(func() {
		if rmErr := req.GitSvc.RemoveWorktree(wtPath); rmErr != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to remove worktree: %v\n", rmErr)
		}
	})

	// safety net: remove worktree if setup fails before main cleanup is registered.
	// once main cleanup takes over (setupDone=true), this defer becomes a no-op.
	setupDone := false
	defer func() {
		if !setupDone {
			if rmErr := req.GitSvc.RemoveWorktree(wtPath); rmErr != nil {
				fmt.Fprintf(os.Stderr, "warning: failed to remove worktree after setup error: %v\n", rmErr)
			}
		}
	}()

	if igErr := req.GitSvc.EnsureLocalGitignore(); igErr != nil {
		fmt.Fprintf(os.Stderr, "warning: gitignore setup: %v\n", igErr)
	}

	origDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("get working directory: %w", err)
	}

	// create progress logger BEFORE chdir so progress files land in main repo's .loopai/progress/.
	// uses the branch name derived from the plan file above, since gitSvc still points at the main
	// repo (on master).
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
	defer func() {
		// mark failure on any error return so Close writes "Failed:" instead of "Completed:",
		// preserving progress history across restart (issue #288)
		if err != nil {
			baseLog.SetFailed(err)
		}
		if closeErr := baseLog.Close(); closeErr != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to close progress log: %v\n", closeErr)
		}
	}()

	// chdir into worktree
	if err = os.Chdir(wtPath); err != nil {
		return fmt.Errorf("chdir to worktree: %w", err)
	}

	// register cleanup: restore CWD and remove worktree.
	// sync.Once prevents double-execution between defer and interrupt handler's force-exit path.
	var cleanupOnce sync.Once
	cleanup := func() {
		cleanupOnce.Do(func() {
			if chdirErr := os.Chdir(origDir); chdirErr != nil {
				fmt.Fprintf(os.Stderr, "warning: failed to restore working directory: %v\n", chdirErr)
			}
			if rmErr := req.GitSvc.RemoveWorktree(wtPath); rmErr != nil {
				fmt.Fprintf(os.Stderr, "warning: failed to remove worktree: %v\n", rmErr)
			}
		})
	}
	setupDone = true // disable safety-net defer, main cleanup takes over
	req.WtCleanup.set(cleanup)
	defer cleanup()

	// open git service inside worktree
	wtGitSvc, err := git.NewService(".", req.Colors.Info(), req.Config.VcsCommand)
	if err != nil {
		return fmt.Errorf("open worktree git service: %w", err)
	}
	wtGitSvc.SetCommitTrailer(req.Config.CommitTrailer)

	// resolve plan file path inside the worktree so Claude operates on the local copy,
	// not the original in the main repo. the plan was copied by CreateWorktreeForPlan.
	wtPlanFile := resolveWorktreePlanFile(req.PlanFile, req.GitSvc.Root())

	// commit plan file on the feature branch (inside worktree), not on the default branch
	if planNeedsCommit {
		if commitErr := wtGitSvc.CommitPlanFile(req.PlanFile, req.GitSvc.Root()); commitErr != nil {
			return fmt.Errorf("commit plan in worktree: %w", commitErr)
		}
	}

	// setup is done: executePlan creates its own reporter and owns every error from here on
	handedOff = true

	return executePlan(ctx, o, executePlanRequest{
		PlanFile:       wtPlanFile,
		MainPlanFile:   req.PlanFile, // original path in main repo for MovePlanToCompleted
		Mode:           req.Mode,
		GitSvc:         wtGitSvc,
		MainGitSvc:     req.GitSvc,
		Config:         req.Config,
		Colors:         req.Colors,
		DefaultBranch:  req.DefaultBranch,
		BaseRef:        req.BaseRef,
		NotifySvc:      req.NotifySvc,
		CmuxStop:       req.CmuxStop,
		ProgressLog:    baseLog,
		PhaseHolder:    holder,
		ExternalReview: req.ExternalReview,
	})
}

// resolveWorktreePlanFile maps an absolute plan path from the main repo into the worktree CWD.
// It resolves symlinks on the plan path to match the repo root (macOS: /tmp -> /private/tmp),
// then makes the path relative to the root and absolute within the worktree.
// Falls back to the original path if any step fails or the path is not absolute.
func resolveWorktreePlanFile(planFile, repoRoot string) string {
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
	abs, err := filepath.Abs(rel)
	if err != nil {
		return planFile
	}
	return abs
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

// externalReviewSelection is the single resolved view of external-review
// configuration used by dependency checks, startup metadata, and processor setup.
// Provider is always concrete (claude, codex, custom, or none), never auto.
type externalReviewSelection struct {
	Provider               string
	Model                  string
	Effort                 string
	AutoSelected           bool
	Explicit               bool
	MaxDropped             bool
	DisabledByCodexEnabled bool
	DisabledByMissing      bool
}

func (s externalReviewSelection) modelSpec() string {
	switch {
	case s.Model == "" && s.Effort == "":
		return ""
	case s.Effort == "":
		return s.Model
	default:
		return s.Model + ":" + s.Effort
	}
}

func (s externalReviewSelection) providerLabel() string {
	if s.DisabledByCodexEnabled {
		return config.ExternalReviewToolNone + " (auto disabled by codex_enabled=false)"
	}
	if s.DisabledByMissing {
		return config.ExternalReviewToolNone + " (auto-selected reviewer unavailable)"
	}
	label := s.Provider
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
		return externalReviewSelection{Provider: config.ExternalReviewToolNone}, nil
	}

	requested := cfg.ExternalReviewTool
	if requested == "" {
		requested = config.ExternalReviewToolAuto
	}
	selection := externalReviewSelection{
		Provider:     requested,
		AutoSelected: requested == config.ExternalReviewToolAuto,
		Explicit:     requested != config.ExternalReviewToolAuto,
	}
	if mode == processor.ModeTasksOnly {
		selection.Provider = config.ExternalReviewToolNone
		selection.AutoSelected = false
		selection.Explicit = false
		return selection, nil
	}

	if requested == config.ExternalReviewToolAuto {
		if !cfg.CodexEnabled && mode != processor.ModeCodexOnly {
			selection.Provider = config.ExternalReviewToolNone
			selection.DisabledByCodexEnabled = true
			return selection, nil
		}
		if primaryProvider(cfg) == config.ExternalReviewToolCodex {
			selection.Provider = config.ExternalReviewToolClaude
		} else {
			selection.Provider = config.ExternalReviewToolCodex
		}
	}

	modelExplicit := o.externalReviewModelSet || cfg.ExternalReviewModelSet
	if selection.Provider == config.ExternalReviewToolCustom && modelExplicit && cfg.ExternalReviewModel != "" {
		return externalReviewSelection{}, errors.New("external_review_model cannot be used with external_review_tool=custom")
	}

	switch selection.Provider {
	case config.ExternalReviewToolClaude:
		selection.Model, selection.Effort = "opus", "xhigh"
		if cfg.ExternalReviewModel != "" {
			model, effort, _ := strings.Cut(cfg.ExternalReviewModel, ":")
			if model != "" {
				selection.Model = model
			}
			if effort != "" {
				selection.Effort = effort
			}
		}
	case config.ExternalReviewToolCodex:
		selection.Model, selection.Effort, selection.MaxDropped = processor.ResolveCodexModelEffort(
			cfg.ExternalReviewModel, cfg.CodexModel, cfg.CodexReasoningEffort)
	case config.ExternalReviewToolCustom, config.ExternalReviewToolNone:
		// custom reviewers do not have a provider model; none disables the phase.
	default:
		return externalReviewSelection{}, fmt.Errorf("unsupported external review tool %q", selection.Provider)
	}
	return selection, nil
}

func printExternalReviewWarnings(selection externalReviewSelection, cfg *config.Config, w io.Writer) {
	if w == nil || cfg == nil {
		return
	}
	if selection.Explicit && selection.Provider == primaryProvider(cfg) &&
		(selection.Provider == config.ExternalReviewToolClaude || selection.Provider == config.ExternalReviewToolCodex) {
		fmt.Fprintf(w, "warning: external reviewer %q matches the primary executor; cross-model review signal will be weaker\n", selection.Provider)
	}
	if selection.MaxDropped {
		fmt.Fprintln(w, "warning: codex does not support 'max' reasoning effort for external review; ignoring (valid: low, medium, high, xhigh)")
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

	var externalErr error
	switch selection.Provider {
	case config.ExternalReviewToolClaude:
		externalErr = checkClaudeDep(cfg)
	case config.ExternalReviewToolCodex:
		externalErr = checkCodexDep(cfg)
	}
	if externalErr == nil {
		return selection, nil
	}
	if !selection.AutoSelected {
		return selection, externalErr
	}
	if warnW != nil {
		fmt.Fprintf(warnW, "warning: automatically selected external reviewer unavailable (%v); disabling external review for this run\n", externalErr)
	}
	selection.Provider = config.ExternalReviewToolNone
	selection.Model = ""
	selection.Effort = ""
	selection.DisabledByMissing = true
	return selection, nil
}

func applyEffectiveExternalReview(cfg *config.Config, selection externalReviewSelection) {
	if cfg == nil {
		return
	}
	cfg.ExternalReviewTool = selection.Provider
	cfg.ExternalReviewModel = selection.modelSpec()
}

// isWatchOnlyMode returns true if running in watch-only mode.
// watch-only mode runs the web dashboard without executing any plan.
func isWatchOnlyMode(o opts, configWatchDirs []string) bool {
	return o.Serve && o.PlanFile == "" && o.PlanDescription == "" && (len(o.Watch) > 0 || len(configWatchDirs) > 0)
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
// if --base-ref may serve as its base. plan mode counts even though plan creation itself runs in
// place: it hands off to the implementation run once the plan exists, so its --base-ref has to
// pass the same validation up front instead of falling back silently and failing much later.
func modeCreatesBranch(mode processor.Mode) bool {
	return modeRequiresBranch(mode) || mode == processor.ModePlan
}

// makePauseHandler returns a context-aware pause handler for task loop breaks.
// on break, prints a message and waits for Enter to resume or context cancellation to abort.
// stdin read runs in a goroutine so the handler responds to Ctrl+C (SIGINT) promptly.
func makePauseHandler(stdin io.Reader, stdout io.Writer) func(ctx context.Context) bool {
	return func(ctx context.Context) bool {
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
	if o.PlanDescription != "" && o.PlanFile != "" {
		return errors.New("--plan flag conflicts with plan file argument; use one or the other")
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
	// --codex / --pass-claude-md / --external-only / --codex-only / --external-review-tool
	// mutual-exclusion checks are deferred to applyCodexOverrides, which runs after the
	// config-file merge so that executor=codex coming from config is also enforced.
	return nil
}

// createRunner creates a processor.Runner with the given configuration.
func createRunner(req executePlanRequest, o opts, log processor.Logger, holder *status.PhaseHolder) *processor.Runner {
	externalReview := req.ExternalReview
	if externalReview.Provider == "" {
		var err error
		externalReview, err = resolveExternalReviewSelection(o, req.Config, req.Mode)
		if err != nil {
			// run() validates this before runner construction; this fallback is for
			// direct test helpers and preserves a safely disabled phase on bad input.
			externalReview = externalReviewSelection{Provider: config.ExternalReviewToolNone}
		}
	}
	applyEffectiveExternalReview(req.Config, externalReview)
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
		CodexEnabled:          externalReview.Provider != config.ExternalReviewToolNone,
		ExternalReviewToolSet: true,
		ExternalReviewTool:    externalReview.Provider,
		ExternalReviewModel:   externalReview.Model,
		ExternalReviewEffort:  externalReview.Effort,
		FinalizeEnabled:       req.Config.FinalizeEnabled,
		DefaultBranch:         req.BaseRef,
		TaskModel:             resolveSpec(o.TaskModel, req.Config.TaskModel),
		ReviewModel:           resolveReviewSpec(o, req.Config),
		AppConfig:             req.Config,
	}, log, holder)
	if req.GitSvc != nil {
		r.SetGitChecker(req.GitSvc)
	}
	return r
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

	if info.ExternalReview.Provider != "" {
		colors.Info().Printf("external review: %s\n", info.ExternalReview.providerLabel())
		if info.ExternalReview.Provider == config.ExternalReviewToolCodex && info.ExternalReview.Model == "" {
			colors.Info().Printf("  model: %s\n", codexBannerValue(""))
		} else if info.ExternalReview.Model != "" {
			colors.Info().Printf("  model: %s\n", info.ExternalReview.Model)
		}
		if info.ExternalReview.Provider == config.ExternalReviewToolCodex && info.ExternalReview.Effort == "" {
			colors.Info().Printf("  reasoning effort: %s\n", codexBannerValue(""))
		} else if info.ExternalReview.Effort != "" {
			colors.Info().Printf("  reasoning effort: %s\n", info.ExternalReview.Effort)
		}
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
	if externalReview.Provider != "" {
		p.ExternalReview = externalReview.providerLabel()
		p.ExternalReviewModel = externalReview.modelSpec()
		if externalReview.Provider == config.ExternalReviewToolCodex && p.ExternalReviewModel == "" {
			p.ExternalReviewModel = "(inherits ~/.codex/config.toml)"
		}
	}
	if mode == processor.ModePlan {
		p.PlanModel = resolvePlanSpec(o, cfg)
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
func runPlanMode(ctx context.Context, o opts, req executePlanRequest, selector *plan.Selector) error {
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

	// cmux sidebar reporter, nil and fully no-op outside cmux. no plan file exists yet, so the
	// progress bar stays empty and only the spinner and the phase pill are reported.
	rep := cmux.New("")
	rep.Start(ctx)
	defer rep.Stop()
	if req.CmuxStop != nil {
		req.CmuxStop.set(rep.Stop)
	}
	holder.OnChange(rep.OnPhase)

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
	}, baseLog, holder)
	// the collector is called exactly when the run stalls waiting for a human, so wrapping it
	// notifies cmux on questions and on the ready draft without touching the phase engines
	r.SetInputCollector(rep.WrapInput(collector))

	// run the plan creation loop
	if runErr := r.Run(ctx); runErr != nil {
		wrapped := fmt.Errorf("plan creation: %w", runErr)
		planCreationErr = wrapped
		notifyCmuxCompletion(rep, "", branch, baseLog.Elapsed(), runErr)
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
	if !input.AskYesNo(ctx, "Continue with plan implementation?", os.Stdin, os.Stdout) {
		return nil
	}

	// resolve plan file to absolute path before potential chdir. assigned through a separate
	// variable: filepath.Abs returns "" on error, so writing planFile first would leave the
	// notification below with nothing to name the run by
	absPlanFile, absErr := filepath.Abs(planFile)
	if absErr != nil {
		wrapped := fmt.Errorf("resolve plan file: %w", absErr)
		notifyCmuxCompletion(rep, planFile, branch, baseLog.Elapsed(), wrapped)
		return wrapped
	}
	planFile = absPlanFile

	// continue with plan implementation
	req.Colors.Info().Printf("\ncontinuing with plan implementation...\n")

	// hand the sidebar over to the execution reporter: stop this one before the next one takes
	// the same keys, otherwise the deferred Stop would clear the execution run's own state
	rep.Stop()

	// worktree mode: create worktree and run from there
	if req.Config.WorktreeEnabled {
		return runWithWorktree(ctx, o, executePlanRequest{
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
			BranchOverride: req.BranchOverride,
			ExternalReview: req.ExternalReview,
		})
	}

	// normal mode: create branch and run in place
	if err := req.GitSvc.CreateBranchForPlan(planFile, req.DefaultBranch, req.BranchOverride); err != nil {
		wrapped := fmt.Errorf("create branch for plan: %w", err)
		// the handoff dies before executePlan gets its own reporter, so this one raises the banner.
		// Stop above only tore down the sidebar; notifications are not sidebar state and still go out
		notifyCmuxCompletion(rep, planFile, branch, baseLog.Elapsed(), wrapped)
		return wrapped
	}

	return executePlan(ctx, o, executePlanRequest{
		PlanFile:       planFile,
		Mode:           processor.ModeFull,
		GitSvc:         req.GitSvc,
		Config:         req.Config,
		Colors:         req.Colors,
		DefaultBranch:  req.DefaultBranch,
		BaseRef:        req.BaseRef,
		NotifySvc:      req.NotifySvc,
		CmuxStop:       req.CmuxStop,
		ExternalReview: req.ExternalReview,
	})
}

// runReset runs the interactive config reset flow.
func runReset(configDir string, stdin io.Reader, stdout io.Writer) error {
	_, err := config.Reset(configDir, stdin, stdout)
	if err != nil {
		return fmt.Errorf("reset config: %w", err)
	}
	return nil
}

// handleEarlyFlags processes flags that should run before full config load (--reset, --dump-defaults).
// returns (true, nil) if an early exit occurred, (true, err) on error, or (false, nil) to continue.
func handleEarlyFlags(o opts) (bool, error) {
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
	if o.Worktree {
		cfg.WorktreeEnabled = true
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
	if o.externalReviewToolSet {
		cfg.ExternalReviewTool = o.ExternalReviewTool
	}
	if o.externalReviewModelSet {
		cfg.ExternalReviewModel = o.ExternalReviewModel
		cfg.ExternalReviewModelSet = true
	}
	if o.customReviewScriptSet {
		cfg.CustomReviewScript = o.CustomReviewScript
	}
	return applyCodexOverrides(o, cfg, os.Stderr)
}

// applyCodexOverrides applies --codex / --pass-claude-md CLI flags after config
// merging. The name is retained for compatibility with existing callers; external
// review selection is now resolved separately and is valid for either primary.
func applyCodexOverrides(o opts, cfg *config.Config, warnW io.Writer) error {
	_ = warnW
	if o.Codex {
		cfg.Executor = config.ExecutorCodex
	}
	if o.externalReviewToolSet {
		cfg.ExternalReviewTool = o.ExternalReviewTool
	}
	if o.externalReviewModelSet {
		cfg.ExternalReviewModel = o.ExternalReviewModel
		cfg.ExternalReviewModelSet = true
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
	return opt != nil && opt.IsSet()
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

// resolveBaseRefs resolves the two bases a run needs: branchBase for branch/worktree creation
// and diffBase for review diffs and the {{DEFAULT_BRANCH}} template variable.
// they differ only when --base-ref names a revision that is not a branch.
// branchMode says whether the run creates a branch at all: review modes never do, so their
// --base-ref is a pure diff base and must not be validated against the checkout.
func resolveBaseRefs(gitSvc *git.Service, cliBaseRef, configBranch string, branchMode, worktreeMode bool) (branchBase, diffBase string, err error) {
	autoDetected := gitSvc.GetDefaultBranch()
	diffBase = resolveDefaultBranch(cliBaseRef, configBranch, autoDetected)
	defaultBranch := resolveDefaultBranch("", configBranch, autoDetected)

	if !branchMode || cliBaseRef == "" {
		return defaultBranch, diffBase, nil
	}

	branchBase, err = resolveBranchBase(cliBaseRef, localBranchRef(gitSvc, cliBaseRef), defaultBranch,
		getCurrentBranch(gitSvc), worktreeMode)
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

// resolveBranchBase decides which ref is the base for branch and worktree creation.
// cliRefBranch is the local branch --base-ref names, empty when it names anything else: a commit
// hash is a valid diff base but cannot be branched from, so the configured/auto-detected default
// branch stays in place — except in worktree mode, where a silent fallback would surface later as
// a confusing "requires <default> branch" message.
// a branch base is honored only from that branch itself, since branch creation cuts from HEAD:
// from another branch CreateBranchForPlan reads the mismatch as "already on a feature branch" and
// skips silently, which would leave the whole run committing onto the default branch.
func resolveBranchBase(cliRef, cliRefBranch, defaultBranch, currentBranch string, worktreeMode bool) (string, error) {
	if cliRef == "" {
		return defaultBranch, nil
	}
	if cliRefBranch == "" {
		if worktreeMode {
			return "", fmt.Errorf("--base-ref %q is not a branch; worktree creation needs a branch to base the "+
				"new worktree on, pass a branch name or drop --worktree", cliRef)
		}
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
func ensureRepoHasCommits(ctx context.Context, gitSvc *git.Service, stdin io.Reader, stdout io.Writer) error {
	// track if we actually created a commit
	createdCommit := false
	promptFn := func() bool {
		fmt.Fprintln(stdout, "repository has no commits")
		fmt.Fprintln(stdout, "loopai needs at least one commit to create feature branches.")
		fmt.Fprintln(stdout)
		if !input.AskYesNo(ctx, "create initial commit?", stdin, stdout) {
			return false
		}
		createdCommit = true
		return true
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
