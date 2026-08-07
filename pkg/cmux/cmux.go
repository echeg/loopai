// Package cmux reports loopai execution state to the sidebar of the cmux terminal.
//
// the integration is best-effort and one-directional: it shells out to the public cmux CLI and
// never lets a failure reach the run. outside cmux (no binary in PATH or no CMUX_WORKSPACE_ID)
// New returns a nil *Reporter and every exported method is a no-op, so callers never check for nil.
package cmux

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/umputun/ralphex/pkg/plan"
	"github.com/umputun/ralphex/pkg/status"
)

const (
	// execTimeout bounds a single cmux CLI call, the socket is local so a hanging call must not stall the run.
	execTimeout = 2 * time.Second

	// outputWaitDelay bounds waiting for output pipes after the cmux client exits or its context is
	// canceled. This protects output-capturing calls from a grandchild that inherited stdout.
	outputWaitDelay = 100 * time.Millisecond

	// spawnTimeout bounds cmux new-workspace. it is far more generous than execTimeout because
	// creating a workspace starts a terminal instead of updating a label, and because a timeout here
	// is ambiguous rather than merely cosmetic: cmux may have created the workspace already while the
	// caller reads the error as a failure and runs the plan locally as well, giving two concurrent
	// runs over one repository.
	spawnTimeout = 10 * time.Second

	// pollInterval is how often the plan file is re-read for the progress bar. a task iteration
	// takes minutes, so a tighter interval would only re-read the file for nothing.
	pollInterval = 10 * time.Second

	// workspaceEnv is injected by cmux into every terminal it owns and inherited by child processes.
	workspaceEnv = "CMUX_WORKSPACE_ID"

	// quietEnv silences cmux's own advisory notices, leaving its errors alone. spawnRunner sets it
	// so a legacy-verb deprecation hint cannot crowd the refusal reason out of the stderr excerpt.
	quietEnv = "CMUX_QUIET"

	// binName is the cmux CLI binary looked up in PATH.
	binName = "cmux"

	// statusKey names both the pill and the sidebar loader entry. an own key is used instead of an
	// agent key like claude_code because cmux shows non-allowlisted pills unconditionally, while
	// allowlisted ones are hidden unless it sees a live pid of that agent.
	statusKey = "loopai"

	// statusPriority orders the loopai pill among other pills in the tab row.
	statusPriority = "90"

	// final pill prefixes are shared by Finish and workspace busy detection so the producer and
	// consumer of the persistent completion status cannot drift apart.
	finalDonePrefix   = "done"
	finalFailedPrefix = "failed"
	startingStatus    = "starting"

	// notifyTitle is the title of every notification loopai raises, i.e. the app name on the banner.
	// same text as statusKey but a separate constant: one is a cmux entry key, the other is user-visible.
	notifyTitle = "loopai"

	// notifyBodyLimit caps the notification body, the macOS banner does not render more anyway.
	notifyBodyLimit = 200

	// failureDetailLimit keeps the final pill compact. Longer errors remain available in the
	// terminal summary and notification, while the pill shows only the outcome.
	failureDetailLimit = 80

	// unknownPhaseIcon is used for a phase missing from phaseStyles.
	unknownPhaseIcon = "circle"

	// stderrDetailLimit bounds the cmux stderr excerpt carried in a workspace creation error. the
	// message ends up in a single warning line, so a verbose refusal is truncated rather than
	// flooding the terminal.
	stderrDetailLimit = 200
)

// commandRunner runs a single cmux CLI invocation. defined here because the reporter is the only
// consumer; tests replace it with a fake recording argv instead of spawning the real binary.
type commandRunner interface {
	run(ctx context.Context, args ...string) error
}

// outputRunner runs a cmux query and returns stdout. It is deliberately separate from
// commandRunner because reporter commands must continue discarding all output.
type outputRunner interface {
	runOutput(ctx context.Context, args ...string) (string, error)
}

// execRunner is the default runner shelling out to the cmux binary, output is discarded.
type execRunner struct {
	bin string
}

func (r *execRunner) run(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, r.bin, args...)
	// stdout/stderr are left nil on purpose, which connects the child to /dev/null. assigning an
	// io.Writer (io.Discard) instead would make os/exec allocate a pipe and a copy goroutine, and
	// cmd.Wait blocks until that pipe reaches EOF — a grandchild inheriting the write end would keep
	// it open past the context deadline and hang the caller, which runs on the execution goroutine.
	cmd.Stdout, cmd.Stderr = nil, nil
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("run %s %s: %w", r.bin, strings.Join(args, " "), err)
	}
	return nil
}

func (r *execRunner) runOutput(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, r.bin, args...)
	cmd.WaitDelay = outputWaitDelay
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("run %s %s: %w", r.bin, strings.Join(args, " "), err)
	}
	return string(out), nil
}

// spawnRunner runs cmux new-workspace and carries what cmux printed on stderr into the error. the
// Reporter's failures are swallowed, so execRunner can discard output, but this is the one cmux
// call whose error reaches the user, and "exit status 1" on its own says nothing about why the
// workspace was refused. stderr goes to a temp file rather than a pipe: os/exec hands an *os.File
// to the child directly and starts no copy goroutine, so a grandchild inheriting the descriptor
// cannot keep cmd.Wait blocked past the deadline the way the pipe execRunner avoids would.
type spawnRunner struct {
	bin string
}

func (r *spawnRunner) run(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, r.bin, args...)
	cmd.Stdout = nil // as in execRunner, the child's stdout belongs to /dev/null
	// new-workspace is a legacy alias for "workspace create", and cmux prints a ~150-character
	// deprecation hint on stderr ahead of anything else on every call. that alone fills most of
	// stderrDetailLimit, so the refusal reason this capture exists to surface would be truncated
	// away. quietEnv silences the hint only, cmux's own "Error: ..." line still arrives. the
	// inherited environment is kept, the client needs it to find the socket and its own workspace.
	cmd.Env = append(os.Environ(), quietEnv+"=1")
	capture, err := os.CreateTemp("", "loopai-cmux-spawn-*.err")
	if err == nil {
		defer func() { _ = capture.Close(); _ = os.Remove(capture.Name()) }()
		cmd.Stderr = capture
	}
	// a capture file that could not be created only makes the diagnostics poorer, the call itself
	// still has to happen, so cmd.Stderr is left nil and the error carries no detail.
	runErr := cmd.Run()
	if runErr == nil {
		return nil
	}
	if detail := stderrDetail(capture); detail != "" {
		return fmt.Errorf("run %s %s: %w: %s", r.bin, strings.Join(args, " "), runErr, detail)
	}
	return fmt.Errorf("run %s %s: %w", r.bin, strings.Join(args, " "), runErr)
}

// stderrDetail reads back what the child wrote to the capture file and folds it into one bounded
// line. a nil file is the "capture unavailable" case and yields no detail, as does an unreadable
// one, since a missing reason must not replace the exit status the caller already has.
func stderrDetail(f *os.File) string {
	if f == nil {
		return ""
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return ""
	}
	// four bytes per rune is the widest UTF-8 encoding, so this always covers stderrDetailLimit runes
	data, err := io.ReadAll(io.LimitReader(f, 4*stderrDetailLimit))
	if err != nil {
		return ""
	}
	detail := strings.Join(strings.Fields(string(data)), " ")
	if runes := []rune(detail); len(runes) > stderrDetailLimit {
		detail = string(runes[:stderrDetailLimit]) + "…"
	}
	return detail
}

// Reporter pushes sidebar state to cmux for the current workspace.
// the zero value is not usable, use New; a nil *Reporter is valid and does nothing.
type Reporter struct {
	runner   commandRunner
	planFile string        // plan file polled for task progress, may be empty
	models   Models        // effective models shown alongside phase names
	timeout  time.Duration // per-call timeout
	interval time.Duration // plan file poll interval

	mu        sync.Mutex         // guards the poller handles below, Stop may run on another goroutine
	cancel    context.CancelFunc // stops the poll goroutine, nil until Start
	startDone chan struct{}      // closed when synchronous Start setup has returned
	pollDone  chan struct{}      // closed by the poll goroutine on exit, nil until Start
	stopOnce  sync.Once          // Stop runs at most once, it is called from a defer and from the interrupt handler

	// statusMu is held across a pill update, so Stop taking it waits out an update in flight and
	// the clear can never be overtaken by a set. stopped gates updates once Stop began.
	statusMu sync.Mutex
	started  bool
	stopped  bool
	finished bool

	// last reported pair, touched by the poll goroutine only. -1 never matches a real count,
	// so the first tick always reports.
	lastDone, lastTotal int
}

// Models contains the effective model specs used by each execution role.
// Empty values are omitted from cmux status text.
type Models struct {
	Plan           string
	Task           string
	Review         string
	ExternalReview string
}

// New returns a reporter for the current cmux workspace, or nil when loopai does not run
// inside cmux. all methods are nil-safe, so the result can be used without a nil check.
func New(planFile string, models Models) *Reporter {
	if strings.TrimSpace(os.Getenv(workspaceEnv)) == "" {
		return nil
	}
	bin, err := exec.LookPath(binName)
	if err != nil {
		return nil
	}
	return &Reporter{
		runner:    &execRunner{bin: bin},
		planFile:  planFile,
		models:    models,
		timeout:   execTimeout,
		interval:  pollInterval,
		lastDone:  -1,
		lastTotal: -1,
	}
}

// ErrNotInCmux reports that loopai does not run inside cmux, so no workspace can be created.
// callers match it with errors.Is to tell "cmux is absent" apart from "cmux refused the request".
var ErrNotInCmux = errors.New("not running inside cmux")

// ErrSpawnAmbiguous reports that workspace creation neither succeeded nor cleanly failed: the
// deadline expired, which kills the local cmux client but says nothing about the request it may
// already have delivered. callers match it with errors.Is to refuse the local fallback they take
// on a clean refusal, since falling back to a run cmux may already have started duplicates it.
var ErrSpawnAmbiguous = errors.New("workspace creation timed out with an unknown outcome")

// WorkspaceBusy reports whether the current cmux workspace has a non-final loopai status pill.
// The absence of cmux itself is reported with ErrNotInCmux, matching SpawnWorkspace.
func WorkspaceBusy() (bool, error) {
	if strings.TrimSpace(os.Getenv(workspaceEnv)) == "" {
		return false, fmt.Errorf("%s is not set: %w", workspaceEnv, ErrNotInCmux)
	}
	bin, err := exec.LookPath(binName)
	if err != nil {
		return false, fmt.Errorf("no %s binary in PATH: %w", binName, ErrNotInCmux)
	}
	return workspaceBusy(&execRunner{bin: bin}, execTimeout)
}

// workspaceBusy is the runner-injectable core of WorkspaceBusy.
func workspaceBusy(runner outputRunner, timeout time.Duration) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	out, err := runner.runOutput(ctx, "list-status")
	if err != nil {
		return false, fmt.Errorf("query cmux workspace status: %w", err)
	}
	return workspaceBusyFromStatus(out), nil
}

// workspaceBusyFromStatus parses cmux list-status output. Metadata begins at a documented
// space-prefixed field, so spaces and suffixes in the human-readable pill text are preserved.
func workspaceBusyFromStatus(out string) bool {
	for line := range strings.SplitSeq(out, "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != statusKey {
			continue
		}
		text := value
		for _, suffix := range []string{" icon=", " color=", " priority="} {
			if i := strings.Index(text, suffix); i >= 0 {
				text = text[:i]
			}
		}
		text = strings.TrimSpace(text)
		return !strings.HasPrefix(text, finalDonePrefix) && !strings.HasPrefix(text, finalFailedPrefix)
	}
	return false
}

// SpawnWorkspace creates a new cmux workspace running argv in cwd and titled name.
//
// unlike the Reporter methods this is not best-effort: the error is returned so the caller can
// choose between exiting after a successful hand-off and continuing the run locally. availability
// is detected exactly like New does, and a missing workspace env or binary yields ErrNotInCmux.
// it uses spawnRunner rather than execRunner, so a refusal reaches the caller with cmux's own
// message instead of a bare exit status.
func SpawnWorkspace(name, cwd string, argv []string) error {
	if strings.TrimSpace(os.Getenv(workspaceEnv)) == "" {
		return fmt.Errorf("%s is not set: %w", workspaceEnv, ErrNotInCmux)
	}
	bin, err := exec.LookPath(binName)
	if err != nil {
		return fmt.Errorf("no %s binary in PATH: %w", binName, ErrNotInCmux)
	}
	return spawnWorkspace(&spawnRunner{bin: bin}, spawnTimeout, name, cwd, argv)
}

// spawnWorkspace is the runner-injectable core of SpawnWorkspace, kept separate so tests can
// record argv without a live cmux socket and bound a blocking call without waiting spawnTimeout.
func spawnWorkspace(runner commandRunner, timeout time.Duration, name, cwd string, argv []string) error {
	if len(argv) == 0 {
		return errors.New("no command for the new workspace")
	}

	// --command is delivered to the new workspace's shell as text plus Enter, so argv has to survive
	// one round of shell word splitting. every element is single-quoted, which is the only escaping
	// form that needs no knowledge of the target shell's expansions.
	quoted := make([]string, 0, len(argv))
	for _, a := range argv {
		quoted = append(quoted, shellQuote(a))
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	args := []string{"new-workspace", "--name", name, "--cwd", cwd, "--focus", "true", "--command", strings.Join(quoted, " ")}
	if err := runner.run(ctx, args...); err != nil {
		// the deadline kills the local cmux client, not the request it already delivered, so the
		// outcome is unknown and the caller must not fall back to a local run.
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("create cmux workspace %q: %w: %w", name, ErrSpawnAmbiguous, err)
		}
		return fmt.Errorf("create cmux workspace %q: %w", name, err)
	}
	return nil
}

// shellQuote wraps s in POSIX single quotes, ending and reopening the quoted run around every
// literal quote ('\”). the result is safe in sh, bash and zsh alike, since nothing but the closing
// quote is special inside a single-quoted string.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// exec runs a cmux command best-effort. errors are swallowed on purpose: the sidebar is an
// indication, not functionality, and reporting failures would only pollute the progress file.
func (r *Reporter) exec(args ...string) {
	r.execContext(context.Background(), args...)
}

// execContext is exec with caller cancellation. Start and the progress poller use it so Stop can
// cancel an in-flight setup/update before issuing the final cleanup commands.
func (r *Reporter) execContext(parent context.Context, args ...string) {
	if r == nil || r.runner == nil {
		return
	}
	ctx, cancel := context.WithTimeout(parent, r.timeout)
	defer cancel()
	_ = r.runner.run(ctx, args...) // best-effort by design, the error is intentionally dropped
}

// loadingOnContext shows the spinner in the sidebar and moves the workspace lane to "working".
// this is the only path to a real running signal that needs neither the agent allowlist nor vault registration.
func (r *Reporter) loadingOnContext(ctx context.Context) {
	r.execContext(ctx, "workspace", "loading", "on", "--id", statusKey)
}

// loadingOff removes the spinner from the sidebar.
func (r *Reporter) loadingOff() { r.exec("workspace", "loading", "off", "--id", statusKey) }

// setStatus sets the loopai pill in the tab row. empty icon or color skip their own flags,
// leaving the cmux-side default instead of passing an empty value.
func (r *Reporter) setStatus(text, icon, color string) {
	r.exec(statusArgs(text, icon, color)...)
}

func statusArgs(text, icon, color string) []string {
	args := []string{"set-status", statusKey, text}
	if icon != "" {
		args = append(args, "--icon", icon)
	}
	if color != "" {
		args = append(args, "--color", color)
	}
	args = append(args, "--priority", statusPriority)
	return args
}

// clearStatus removes the loopai pill.
func (r *Reporter) clearStatus() { r.exec("clear-status", statusKey) }

// Clear removes the persistent loopai status pill. It is nil-safe so standalone commands
// can call it without checking whether they are running inside cmux.
func (r *Reporter) Clear() { r.clearStatus() }

// Reserve installs a non-final pill before normal startup reaches its first execution phase.
// Auto-workspace mode uses it to close the otherwise-unobservable preflight window: a later
// invocation sees "starting" as busy and hands off. The returned error lets the caller retry or
// clear stale state normally instead of assuming the reservation replaced it.
func (r *Reporter) Reserve() error {
	if r == nil {
		return ErrNotInCmux
	}
	r.statusMu.Lock()
	defer r.statusMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), r.timeout)
	defer cancel()
	if err := r.runner.run(ctx, statusArgs(startingStatus, "hourglass", "#3b82f6")...); err != nil {
		return fmt.Errorf("reserve cmux workspace status: %w", err)
	}
	return nil
}

// setProgress sets the sidebar progress bar. ratio is expected in [0, 1]: reportProgress, the
// only caller, derives it from a task count it has already checked, so it cannot fall outside.
func (r *Reporter) setProgress(ratio float64, label string) {
	r.setProgressContext(context.Background(), ratio, label)
}

func (r *Reporter) setProgressContext(ctx context.Context, ratio float64, label string) {
	args := []string{"set-progress", strconv.FormatFloat(ratio, 'f', 2, 64)}
	if label != "" {
		args = append(args, "--label", label)
	}
	r.execContext(ctx, args...)
}

// clearProgress removes the sidebar progress bar.
func (r *Reporter) clearProgress() { r.exec("clear-progress") }

// Notify raises a cmux notification: blue ring on the panel, macOS banner and an entry in the
// notification panel. empty subtitle or body skip their own flags, the body is truncated to fit the banner.
func (r *Reporter) Notify(subtitle, body string) {
	args := []string{"notify", "--title", notifyTitle}
	if subtitle != "" {
		args = append(args, "--subtitle", subtitle)
	}
	if body != "" {
		args = append(args, "--body", truncateRunes(body, notifyBodyLimit))
	}
	r.exec(args...)
}

// Finish replaces the running phase pill with a persistent final outcome. Stop still removes
// transient artifacts, but preserves this pill until the next run or an explicit clear command.
func (r *Reporter) Finish(success bool, detail string) {
	if r == nil {
		return
	}
	r.statusMu.Lock()
	defer r.statusMu.Unlock()
	if r.stopped || r.finished {
		return
	}

	detail = strings.TrimSpace(detail)
	if success {
		text := finalDonePrefix
		if detail != "" {
			text += " in " + detail
		}
		r.setStatus(text, "bolt", "#34c759")
	} else {
		text := finalFailedPrefix
		if detail != "" && len([]rune(detail)) <= failureDetailLimit {
			text += " · " + detail
		}
		r.setStatus(text, "exclamationmark.triangle", "#ff3b30")
	}
	r.finished = true
}

// Start publishes the startup reservation, shows the spinner, and begins polling the plan file for
// task progress in the background. polling is used instead of hooking into the phase engines so the
// progress bar keeps moving during a long task phase without touching pkg/processor.
func (r *Reporter) Start(ctx context.Context) {
	if r == nil {
		return
	}
	startCtx, cancel := context.WithCancel(ctx)
	startDone := make(chan struct{})

	r.statusMu.Lock()
	if r.started || r.stopped || r.finished {
		r.statusMu.Unlock()
		cancel()
		return
	}
	r.started = true
	r.mu.Lock()
	r.cancel = cancel
	r.startDone = startDone
	r.mu.Unlock()
	r.statusMu.Unlock()
	defer close(startDone)

	r.execContext(startCtx, statusArgs(startingStatus, "hourglass", "#3b82f6")...)
	if startCtx.Err() != nil {
		return
	}
	r.loadingOnContext(startCtx)
	if startCtx.Err() != nil {
		return
	}

	// plan creation has no plan file yet, so there is nothing to poll and no goroutine to run
	if r.planFile == "" {
		return
	}

	// report once up front: the bar is then there from the start rather than a tick later,
	// which also covers runs shorter than the poll interval
	r.reportProgressContext(startCtx)
	if startCtx.Err() != nil {
		return
	}

	done := make(chan struct{})

	r.statusMu.Lock()
	if r.stopped {
		r.statusMu.Unlock()
		return
	}
	r.mu.Lock()
	r.pollDone = done
	r.mu.Unlock()
	r.statusMu.Unlock()

	go r.poll(startCtx, done)
}

// poll re-reads the plan file on every tick until the context is canceled.
func (r *Reporter) poll(ctx context.Context, done chan struct{}) {
	defer close(done)

	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.reportProgressContext(ctx)
		}
	}
}

// reportProgress counts done tasks in the plan file and pushes the ratio to the sidebar.
// a missing path, a read error (the plan may have moved to completed/) or an unchanged pair
// skip the tick silently, the goroutine keeps running.
func (r *Reporter) reportProgress() {
	r.reportProgressContext(context.Background())
}

func (r *Reporter) reportProgressContext(ctx context.Context) {
	if r.planFile == "" {
		return
	}
	p, err := plan.ParsePlanFile(r.planFile)
	if err != nil {
		return
	}

	total := len(p.Tasks)
	if total == 0 {
		return
	}
	done := 0
	for _, t := range p.Tasks {
		if t.Status == plan.TaskStatusDone {
			done++
		}
	}

	if done == r.lastDone && total == r.lastTotal {
		return
	}
	r.lastDone, r.lastTotal = done, total
	r.setProgressContext(ctx, float64(done)/float64(total), fmt.Sprintf("%d/%d tasks", done, total))
}

// Stop ends background polling, waits for the goroutine to exit and removes transient sidebar
// artifacts. A final pill installed by Finish is preserved. Stop is safe to call twice and safe
// to call without a preceding Start, which matters because it runs both from a defer and from the
// interrupt handler.
func (r *Reporter) Stop() {
	if r == nil {
		return
	}
	r.stopOnce.Do(func() {
		// Cancel Start/poll commands before cleanup. Waiting for the synchronous Start section is
		// bounded by the current command's context-aware timeout, so a loading-on call cannot land
		// after loading-off and recreate a stale spinner.
		r.mu.Lock()
		cancel, startDone, pollDone := r.cancel, r.startDone, r.pollDone
		r.mu.Unlock()

		if cancel != nil {
			cancel()
		}
		if startDone != nil {
			<-startDone
		}
		// the spinner is cleared before joining the poller; its in-flight command was canceled above
		r.loadingOff()

		// Gate pill producers before clearing status. Taking statusMu waits out an update in flight,
		// so no set-status call can land after the clear below.
		r.statusMu.Lock()
		r.stopped = true
		finished := r.finished
		r.statusMu.Unlock()

		if pollDone != nil {
			<-pollDone
		}
		// after the poller is gone, so a tick in flight cannot re-add the bar behind the clear
		if !finished {
			r.clearStatus()
		}
		r.clearProgress()
	})
}

// Logger is the execution logger surface needed to observe structured sections.
type Logger interface {
	Print(format string, args ...any)
	PrintRaw(format string, args ...any)
	PrintSection(section status.Section)
	PrintAligned(text string)
	LogQuestion(question string, options []string)
	LogAnswer(answer string)
	LogDraftReview(action string, feedback string)
	Path() string
}

// WrapLogger decorates an execution logger so review section iterations update cmux.
// Outside cmux the original logger is returned unchanged.
func (r *Reporter) WrapLogger(logger Logger) Logger {
	if r == nil {
		return logger
	}
	return &reportingLogger{Logger: logger, rep: r}
}

type reportingLogger struct {
	Logger
	rep *Reporter
}

// LogLimitWait forwards the regular progress message and enriches the cmux pill with the
// configured retry delay. retryPolicy discovers this method through an optional interface.
func (l *reportingLogger) LogLimitWait(pattern, tool, waitLabel string) {
	l.Print("rate limit detected: %q in %s output, waiting %s before retry...",
		pattern, tool, waitLabel)
	l.rep.OnLimitWait(waitLabel)
}

// LogLimitRecovery forwards the regular progress message and replaces the
// temporary rate-limit pill with account-rotation progress.
func (l *reportingLogger) LogLimitRecovery(statusText, message string) {
	l.Print("%s", message)
	l.rep.OnLimitRecovery(statusText)
}

func (l *reportingLogger) PrintSection(section status.Section) {
	l.Logger.PrintSection(section)
	l.rep.OnSection(section)
}

// inputCollector collects interactive input during plan creation. it mirrors
// processor.InputCollector and is declared here because the wrapper below is the only consumer,
// which also keeps pkg/cmux free of a dependency on pkg/processor.
type inputCollector interface {
	AskQuestion(ctx context.Context, question string, options []string) (string, error)
	AskDraftReview(ctx context.Context, question, planContent string) (action, feedback string, err error)
}

// WrapInput decorates an input collector so cmux is notified whenever the run stalls waiting for
// a human. outside cmux the original collector is returned unchanged, so nothing is added to the path.
func (r *Reporter) WrapInput(c inputCollector) inputCollector {
	if r == nil {
		return c
	}
	return &notifyingCollector{rep: r, inner: c}
}

// notifyingCollector raises a cmux notification before delegating to the wrapped collector.
// the return values are passed through untouched, the notification is a side effect only.
type notifyingCollector struct {
	rep   *Reporter
	inner inputCollector
}

// AskQuestion notifies that loopai waits for an answer, then delegates.
func (c *notifyingCollector) AskQuestion(ctx context.Context, question string, options []string) (string, error) {
	c.rep.Notify("input needed", question)
	return c.inner.AskQuestion(ctx, question, options) //nolint:wrapcheck // decorator passes the inner error through unchanged
}

// AskDraftReview notifies that a plan draft is ready for review, then delegates.
func (c *notifyingCollector) AskDraftReview(ctx context.Context, question, planContent string) (action, feedback string, err error) {
	c.rep.Notify("plan draft ready", question)
	return c.inner.AskDraftReview(ctx, question, planContent) //nolint:wrapcheck // decorator passes the inner error through unchanged
}

// OnPhase updates the pill on a phase change, the signature matches status.PhaseHolder.OnChange.
// after Stop it does nothing: the observer is called from the execution goroutine, which outlives
// the sidebar teardown on the force-exit path, and a late pill would then never be cleared.
// Notify is deliberately not gated the same way — a banner is transient and needs no cleanup.
func (r *Reporter) OnPhase(_, cur status.Phase) {
	if r == nil {
		return
	}
	r.statusMu.Lock()
	defer r.statusMu.Unlock()
	if r.stopped || r.finished {
		return
	}
	s := styleForPhase(cur)
	r.setStatus(r.statusText(s.text, cur, 0), s.icon, s.color)
}

// OnLimitWait shows the configured retry delay while execution is paused by a provider limit.
func (r *Reporter) OnLimitWait(waitLabel string) {
	if r == nil {
		return
	}
	r.statusMu.Lock()
	defer r.statusMu.Unlock()
	if r.stopped || r.finished {
		return
	}
	s := styleForPhase(status.PhaseLimitWait)
	r.setStatus("rate limited · retry in "+waitLabel, s.icon, s.color)
}

// OnLimitRecovery shows sanitized claude-swap progress. Account emails and raw
// command output never reach the sidebar.
func (r *Reporter) OnLimitRecovery(text string) {
	if r == nil {
		return
	}
	r.statusMu.Lock()
	defer r.statusMu.Unlock()
	if r.stopped || r.finished {
		return
	}
	s := styleForPhase(status.PhaseLimitWait)
	r.setStatus(text, s.icon, s.color)
}

// OnSection enriches review phase status with the structured iteration number.
// It is called by the logger wrapper after the section reaches the regular log.
func (r *Reporter) OnSection(section status.Section) {
	if r == nil {
		return
	}

	var phase status.Phase
	switch section.Type {
	case status.SectionInternalReview:
		phase = status.PhaseReview
	case status.SectionExternalReviewIteration:
		phase = status.PhaseExternalReview
	default:
		return
	}

	r.statusMu.Lock()
	defer r.statusMu.Unlock()
	if r.stopped || r.finished {
		return
	}
	s := styleForPhase(phase)
	r.setStatus(r.statusText(s.text, phase, section.Iteration), s.icon, s.color)
}

func (r *Reporter) statusText(text string, phase status.Phase, iteration int) string {
	model := r.modelForPhase(phase)
	if model != "" {
		text += " (" + model + ")"
	}
	if iteration > 0 {
		text += fmt.Sprintf(" · iteration %d", iteration)
	}
	return text
}

func (r *Reporter) modelForPhase(phase status.Phase) string {
	switch phase {
	case status.PhasePlan:
		return r.models.Plan
	case status.PhaseTask:
		return r.models.Task
	case status.PhaseReview, status.PhaseExternalEval, status.PhaseFinalize:
		return r.models.Review
	case status.PhaseExternalReview:
		return r.models.ExternalReview
	default:
		return ""
	}
}

// phaseStyle is the sidebar presentation of a phase: pill text, SF Symbol name and hex color.
type phaseStyle struct {
	text  string
	icon  string
	color string
}

// phaseStyles maps execution phases to their sidebar presentation. legacy phases kept in
// pkg/status for replaying old progress files are deliberately absent, they fall back to the
// unknown-phase style and are never set on a live run.
var phaseStyles = map[status.Phase]phaseStyle{
	status.PhaseTask:           {text: "task", icon: "hammer", color: "#22c55e"},
	status.PhaseReview:         {text: "review", icon: "magnifyingglass", color: "#06b6d4"},
	status.PhaseExternalReview: {text: "external review", icon: "person.2", color: "#a855f7"},
	status.PhaseExternalEval:   {text: "evaluating findings", icon: "checkmark.seal", color: "#a855f7"},
	status.PhaseFinalize:       {text: "finalize", icon: "flag.checkered", color: "#22c55e"},
	status.PhasePlan:           {text: "planning", icon: "list.bullet.clipboard", color: "#3b82f6"},
	status.PhaseLimitWait:      {text: "rate limited", icon: "clock.arrow.circlepath", color: "#ef4444"},
}

// styleForPhase returns the presentation of a phase, falling back to its raw value with a
// neutral icon and no color so an unmapped phase still shows something meaningful.
func styleForPhase(p status.Phase) phaseStyle {
	if s, ok := phaseStyles[p]; ok {
		return s
	}
	return phaseStyle{text: string(p), icon: unknownPhaseIcon}
}

// truncateRunes cuts s to at most limit runes, counting runes rather than bytes so unicode text
// is not cut mid-character.
func truncateRunes(s string, limit int) string {
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit])
}
