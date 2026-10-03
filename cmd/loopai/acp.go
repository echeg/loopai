package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/fatih/color"

	"github.com/umputun/ralphex/pkg/acp"
	"github.com/umputun/ralphex/pkg/config"
	"github.com/umputun/ralphex/pkg/processor"
)

// acpPromptUsage is the prompt grammar, returned with every malformed prompt.
const acpPromptUsage = "usage: <plan-file> [--task-model provider[:model[:effort]]] " +
	"[--review-model provider[:model[:effort]]] [--external-reviewers provider[:model[:effort]],...]"

// versionBannerWriter is where main prints the version banner: stderr in ACP mode, where stdout
// carries JSON-RPC only, and stdout otherwise.
func versionBannerWriter(args []string) io.Writer {
	if acpRequested(args) {
		return os.Stderr
	}
	return os.Stdout
}

// acpRequested reports whether the raw arguments ask for ACP mode. main needs the answer before
// flag parsing, because the banner is printed first.
func acpRequested(args []string) bool {
	for _, arg := range args {
		if arg == "--" {
			return false
		}
		if arg == "--acp" {
			return true
		}
	}
	return false
}

// validateACPFlags keeps --acp standalone. Each prompt names its plan and the three pass-through
// flags, so a plan argument or an execution flag on the command line would be silently ignored.
// --t3 and --orca are not rejected because they may come from the environment; each prompt forces
// them off instead.
func validateACPFlags(o opts) error {
	if !o.ACP {
		return nil
	}
	if o.PlanFile != "" {
		return errors.New("--acp cannot be combined with a plan file argument; name the plan in the prompt")
	}
	for _, conflict := range []struct {
		flag string
		set  bool
	}{
		{"--t3-launch", o.T3Launch},
		{"--cmux-workspace", o.CmuxWorkspace != ""},
		{"--clear", o.Clear},
		{"--merge, --pr, or --report", closeoutRequested(o)},
	} {
		if conflict.set {
			return fmt.Errorf("--acp cannot be combined with %s", conflict.flag)
		}
	}
	if hasExecutionMode(o) {
		return errors.New("--acp cannot be combined with execution flags; " +
			"pass --task-model, --review-model, or --external-reviewers in the prompt")
	}
	return nil
}

// runACPCommand serves the Agent Client Protocol on the process's stdin and stdout until the
// client closes stdin or ctx is canceled. Every human-readable line goes to stderr or the
// progress log.
func runACPCommand(ctx context.Context, o opts) error {
	defer catchSIGPIPE()()
	in, out, restore, err := redirectStdioForACP()
	if err != nil {
		return err
	}
	defer restore()
	return serveACP(ctx, o, in, out, os.Stderr)
}

// catchSIGPIPE turns a write to a closed stdout or stderr pipe into an EPIPE error. Both are pipes
// owned by the client, and without a handler Go kills the process on the first such write once
// the client is gone, skipping the run's cancellation and leaving its providers, which run in their
// own sessions, unsupervised. Notify rather than Ignore keeps the default disposition for child
// processes, since an ignored signal survives exec. The returned function restores the default.
func catchSIGPIPE() func() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGPIPE)
	return func() { signal.Stop(ch) }
}

// redirectStdioForACP reserves the real stdin and stdout for the protocol and points os.Stdin at
// the null device and os.Stdout and color.Output at stderr, so a stray read or write anywhere in a
// run can neither consume nor corrupt JSON-RPC traffic. restore puts the original streams back.
func redirectStdioForACP() (in, out *os.File, restore func(), err error) {
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("open %s: %w", os.DevNull, err)
	}
	in, out = os.Stdin, os.Stdout
	prevColor := color.Output
	os.Stdin, os.Stdout, color.Output = devNull, os.Stderr, color.Error
	return in, out, func() {
		os.Stdin, os.Stdout, color.Output = in, out, prevColor
		_ = devNull.Close()
	}, nil
}

// serveACP runs the ACP server over in and out. A canceled ctx cancels the running prompt and
// returns once it has been answered, without waiting for in to end.
func serveACP(ctx context.Context, o opts, in io.Reader, out, stderr io.Writer) error {
	// every prompt loads config after entering its session cwd, so a relative config directory
	// is pinned to the directory loopai was started in
	configDir := o.ConfigDir
	if configDir != "" {
		abs, err := filepath.Abs(configDir)
		if err != nil {
			return fmt.Errorf("resolve config directory: %w", err)
		}
		configDir = abs
	}
	runner := &acpRunner{base: opts{ConfigDir: configDir, Debug: o.Debug, NoColor: o.NoColor}, out: stderr}
	var debug io.Writer
	if o.Debug {
		debug = stderr
	}
	srv := acp.NewServer(in, out, acp.Options{Version: resolveVersion(), Run: runner.run, Debug: debug})
	served := make(chan error, 1)
	go func() { served <- srv.Serve() }()
	select {
	case err := <-served:
		if err != nil {
			return fmt.Errorf("acp: %w", err)
		}
		return nil
	case <-ctx.Done():
		srv.Shutdown()
		return nil
	}
}

// acpRunner executes ACP prompts as plan runs. The server runs one prompt at a time, which a run
// relies on because it changes the process working directory.
type acpRunner struct {
	base opts      // process-level options every prompt inherits
	out  io.Writer // human-readable run output
}

// run executes one prompt: it parses the plan and pass-through flags, enters the session's
// working directory, loads config there, and runs the plan in place through the normal execution
// path with the sink observing sections, output, and phases.
func (a *acpRunner) run(ctx context.Context, req acp.PromptRequest, sink *acp.Sink) (acp.Result, error) {
	prompt, err := parseACPPrompt(req.Text)
	if err != nil {
		return acp.Result{Message: fmt.Sprintf("%v\n\n%s", err, acpPromptUsage)}, fmt.Errorf("malformed prompt: %w", err)
	}
	o := a.base
	o.PlanFile, o.PlanFiles = prompt.PlanFile, prompt.PlanFiles
	o.TaskModel, o.ReviewModel = prompt.TaskModel, prompt.ReviewModel
	o.ExternalReviewers, o.externalReviewersSet = prompt.ExternalReviewers, prompt.externalReviewersSet

	leave, err := enterDir(req.Cwd, a.out)
	if err != nil {
		return acp.Result{}, err
	}
	defer leave()

	if reason := planFileRefusal(o.PlanFile); reason != "" {
		return acp.Result{}, errors.New(reason)
	}
	cfg, err := loadACPSessionConfig(o)
	if err != nil {
		return acp.Result{}, err
	}

	execReq, selector, release, err := prepareNonInteractiveRequest(ctx, o, cfg, a.out)
	if err != nil {
		return acp.Result{}, err
	}
	// --acp is routed before run() creates its holder, so each prompt holds its own
	keepAwake := newAwakeHolder(cfg.KeepAwake)
	defer keepAwake.Stop()
	execReq.KeepAwake = keepAwake
	sink.SetPlan(absPath(req.Cwd, o.PlanFile), acpStages(cfg, execReq.ExternalReview)...)
	execReq.LogDecorator = func(l processor.Logger) processor.Logger { return sink.WrapLogger(l) }
	execReq.PhaseObserver = sink.OnPhase

	runErr := errors.Join(selectAndExecutePlan(ctx, o, execReq, selector), release())
	return acpRunResult(o.PlanFile, execReq.Outcome, runErr)
}

// loadACPSessionConfig loads config in the session's working directory. The session reports
// through ACP rather than the T3 thread API or terminal titles, and the plan runs in place because
// T3 Code owns the thread's worktree, so t3, orca, and use_worktree are forced off.
func loadACPSessionConfig(o opts) (*config.Config, error) {
	cfg, err := loadRunConfig(o)
	if err != nil {
		return nil, err
	}
	cfg.T3, cfg.Orca, cfg.WorktreeEnabled = false, false, false
	return cfg, nil
}

// parseACPPrompt parses a prompt's first text block: one plan file plus --task-model,
// --review-model, and --external-reviewers, each as "--flag value" or "--flag=value", with the
// values --t3-launch accepts. Plan paths containing whitespace are not supported.
func parseACPPrompt(text string) (opts, error) {
	var o opts
	seen := map[string]bool{}
	fields := strings.Fields(text)
	for i := 0; i < len(fields); i++ {
		tok := fields[i]
		if !strings.HasPrefix(tok, "-") {
			if o.PlanFile != "" {
				return opts{}, fmt.Errorf("unexpected argument %q: the prompt names exactly one plan file", tok)
			}
			o.PlanFile = tok
			continue
		}
		name, value, inline := strings.Cut(tok, "=")
		var target *string
		switch name {
		case "--task-model":
			target = &o.TaskModel
		case "--review-model":
			target = &o.ReviewModel
		case "--external-reviewers":
			target = &o.ExternalReviewers
			o.externalReviewersSet = true
		default:
			return opts{}, fmt.Errorf("unsupported option %q", name)
		}
		if seen[name] {
			return opts{}, fmt.Errorf("%s given more than once", name)
		}
		seen[name] = true
		if !inline && i+1 < len(fields) {
			i++
			value = fields[i]
		}
		if value == "" || strings.HasPrefix(value, "-") {
			return opts{}, fmt.Errorf("%s requires a value", name)
		}
		*target = value
	}
	if o.PlanFile == "" {
		return opts{}, errors.New("the prompt must name a plan file")
	}
	if strings.Contains(o.PlanFile, ",") {
		return opts{}, errors.New("the prompt names exactly one plan file; plan chains are not supported")
	}
	o.PlanFiles = []string{o.PlanFile}
	if err := validatePassThroughValues("prompt", o); err != nil {
		return opts{}, err
	}
	return o, nil
}

// enterDir changes the process working directory to dir and returns a function restoring the
// previous one; a failed restore is reported to warnings.
func enterDir(dir string, warnings io.Writer) (func(), error) {
	prev, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("read working directory: %w", err)
	}
	if err := os.Chdir(dir); err != nil {
		return nil, fmt.Errorf("enter session directory: %w", err)
	}
	return func() {
		if err := os.Chdir(prev); err != nil {
			fmt.Fprintf(warnings, "warning: restore working directory: %v\n", err)
		}
	}, nil
}

func absPath(dir, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(dir, path)
}

// acpStages lists the post-task stages a full run is expected to reach, so the client shows them
// as pending plan entries from the start. A stage reached without being listed is still shown.
func acpStages(cfg *config.Config, review externalReviewSelection) []acp.Stage {
	stages := []acp.Stage{acp.StageReview}
	if len(review.Reviewers) > 0 {
		stages = append(stages, acp.StageExternalReview)
	}
	if cfg.FinalizeEnabled {
		stages = append(stages, acp.StageFinalize)
	}
	if cfg.ReportEnabled {
		stages = append(stages, acp.StageReport)
	}
	return stages
}

// acpRunResult turns an execution outcome into the prompt result. The completion report becomes
// the final message; a run that returned nil without succeeding, such as an abort, still fails.
func acpRunResult(planFile string, outcome *planExecutionOutcome, runErr error) (acp.Result, error) {
	res := acp.Result{Message: outcome.report}
	switch {
	case runErr != nil:
		return res, runErr
	case !outcome.succeeded && outcome.failure != nil:
		return res, outcome.failure
	case !outcome.succeeded:
		return res, errors.New("loopai run did not complete")
	}
	if res.Message == "" {
		res.Message = "loopai completed " + planFile
	}
	return res, nil
}
