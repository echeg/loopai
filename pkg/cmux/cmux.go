// Package cmux reports loopai execution state to the sidebar of the cmux terminal.
//
// the integration is best-effort and one-directional: it shells out to the public cmux CLI and
// never lets a failure reach the run. outside cmux (no binary in PATH or no CMUX_WORKSPACE_ID)
// New returns a nil *Reporter and every exported method is a no-op, so callers never check for nil.
package cmux

import (
	"context"
	"fmt"
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

	// pollInterval is how often the plan file is re-read for the progress bar. a task iteration
	// takes minutes, so a tighter interval would only re-read the file for nothing.
	pollInterval = 10 * time.Second

	// workspaceEnv is injected by cmux into every terminal it owns and inherited by child processes.
	workspaceEnv = "CMUX_WORKSPACE_ID"

	// binName is the cmux CLI binary looked up in PATH.
	binName = "cmux"

	// statusKey names both the pill and the sidebar loader entry. an own key is used instead of an
	// agent key like claude_code because cmux shows non-allowlisted pills unconditionally, while
	// allowlisted ones are hidden unless it sees a live pid of that agent.
	statusKey = "loopai"

	// statusPriority orders the loopai pill among other pills in the tab row.
	statusPriority = "90"

	// notifyTitle is the title of every notification loopai raises, i.e. the app name on the banner.
	// same text as statusKey but a separate constant: one is a cmux entry key, the other is user-visible.
	notifyTitle = "loopai"

	// notifyBodyLimit caps the notification body, the macOS banner does not render more anyway.
	notifyBodyLimit = 200

	// unknownPhaseIcon is used for a phase missing from phaseStyles.
	unknownPhaseIcon = "circle"
)

// commandRunner runs a single cmux CLI invocation. defined here because the reporter is the only
// consumer; tests replace it with a fake recording argv instead of spawning the real binary.
type commandRunner interface {
	run(ctx context.Context, args ...string) error
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

// Reporter pushes sidebar state to cmux for the current workspace.
// the zero value is not usable, use New; a nil *Reporter is valid and does nothing.
type Reporter struct {
	runner   commandRunner
	planFile string        // plan file polled for task progress, may be empty
	models   Models        // effective models shown alongside phase names
	timeout  time.Duration // per-call timeout
	interval time.Duration // plan file poll interval

	mu       sync.Mutex         // guards the poller handles below, Stop may run on another goroutine
	cancel   context.CancelFunc // stops the poll goroutine, nil until Start
	pollDone chan struct{}      // closed by the poll goroutine on exit, nil until Start
	stopOnce sync.Once          // Stop runs at most once, it is called from a defer and from the interrupt handler

	// statusMu is held across a pill update, so Stop taking it waits out an update in flight and
	// the clear can never be overtaken by a set. stopped gates updates once Stop began.
	statusMu sync.Mutex
	stopped  bool

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

// exec runs a cmux command best-effort. errors are swallowed on purpose: the sidebar is an
// indication, not functionality, and reporting failures would only pollute the progress file.
func (r *Reporter) exec(args ...string) {
	if r == nil || r.runner == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), r.timeout)
	defer cancel()
	_ = r.runner.run(ctx, args...) // best-effort by design, the error is intentionally dropped
}

// loadingOn shows the spinner in the sidebar and moves the workspace lane to "working".
// this is the only path to a real running signal that needs neither the agent allowlist nor vault registration.
func (r *Reporter) loadingOn() { r.exec("workspace", "loading", "on", "--id", statusKey) }

// loadingOff removes the spinner from the sidebar.
func (r *Reporter) loadingOff() { r.exec("workspace", "loading", "off", "--id", statusKey) }

// setStatus sets the loopai pill in the tab row. empty icon or color skip their own flags,
// leaving the cmux-side default instead of passing an empty value.
func (r *Reporter) setStatus(text, icon, color string) {
	args := []string{"set-status", statusKey, text}
	if icon != "" {
		args = append(args, "--icon", icon)
	}
	if color != "" {
		args = append(args, "--color", color)
	}
	args = append(args, "--priority", statusPriority)
	r.exec(args...)
}

// clearStatus removes the loopai pill.
func (r *Reporter) clearStatus() { r.exec("clear-status", statusKey) }

// setProgress sets the sidebar progress bar. ratio is expected in [0, 1]: reportProgress, the
// only caller, derives it from a task count it has already checked, so it cannot fall outside.
func (r *Reporter) setProgress(ratio float64, label string) {
	args := []string{"set-progress", strconv.FormatFloat(ratio, 'f', 2, 64)}
	if label != "" {
		args = append(args, "--label", label)
	}
	r.exec(args...)
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

// Start shows the spinner and begins polling the plan file for task progress in the background.
// polling is used instead of hooking into the phase engines so the progress bar keeps moving
// during a long task phase without touching pkg/processor.
func (r *Reporter) Start(ctx context.Context) {
	if r == nil {
		return
	}
	r.loadingOn()

	// plan creation has no plan file yet, so there is nothing to poll and no goroutine to run
	if r.planFile == "" {
		return
	}

	// report once up front: the bar is then there from the start rather than a tick later,
	// which also covers runs shorter than the poll interval
	r.reportProgress()

	pollCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	r.mu.Lock()
	r.cancel = cancel
	r.pollDone = done
	r.mu.Unlock()

	go r.poll(pollCtx, done)
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
			r.reportProgress()
		}
	}
}

// reportProgress counts done tasks in the plan file and pushes the ratio to the sidebar.
// a missing path, a read error (the plan may have moved to completed/) or an unchanged pair
// skip the tick silently, the goroutine keeps running.
func (r *Reporter) reportProgress() {
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
	r.setProgress(float64(done)/float64(total), fmt.Sprintf("%d/%d tasks", done, total))
}

// Stop ends background polling, waits for the goroutine to exit and removes every sidebar
// artifact. safe to call twice and safe to call without a preceding Start, which matters because
// it runs both from a defer and from the interrupt handler.
func (r *Reporter) Stop() {
	if r == nil {
		return
	}
	r.stopOnce.Do(func() {
		// the spinner goes first, before the poller is joined: cmux puts no ttl on it, so a leftover
		// spinner survives until cmux restarts. canceling does not shorten a poll call already in
		// flight — exec times out against its own background context — so on the interrupt handler's
		// bounded cleanup the join could otherwise eat the whole budget before this ever lands.
		r.loadingOff()

		// gate the pill before anything else: OnPhase runs on the execution goroutine, which on the
		// force-exit path is still live and may change phase while the sidebar is being torn down.
		// taking statusMu waits out an update in flight, so a set-status cannot land behind the
		// clear below and leave the pill in the tab row — cmux drops it only when told to.
		r.statusMu.Lock()
		r.stopped = true
		r.statusMu.Unlock()

		r.mu.Lock()
		cancel, done := r.cancel, r.pollDone
		r.mu.Unlock()

		if cancel != nil {
			cancel()
			<-done
		}
		// after the poller is gone, so a tick in flight cannot re-add the bar behind the clear
		r.clearStatus()
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
	return &reportingLogger{inner: logger, rep: r}
}

type reportingLogger struct {
	inner Logger
	rep   *Reporter
}

func (l *reportingLogger) Print(format string, args ...any) {
	l.inner.Print(format, args...)
}

func (l *reportingLogger) PrintRaw(format string, args ...any) {
	l.inner.PrintRaw(format, args...)
}

func (l *reportingLogger) PrintSection(section status.Section) {
	l.inner.PrintSection(section)
	l.rep.OnSection(section)
}

func (l *reportingLogger) PrintAligned(text string) {
	l.inner.PrintAligned(text)
}

func (l *reportingLogger) LogQuestion(question string, options []string) {
	l.inner.LogQuestion(question, options)
}

func (l *reportingLogger) LogAnswer(answer string) {
	l.inner.LogAnswer(answer)
}

func (l *reportingLogger) LogDraftReview(action, feedback string) {
	l.inner.LogDraftReview(action, feedback)
}

func (l *reportingLogger) Path() string {
	return l.inner.Path()
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
	if r.stopped {
		return
	}
	s := styleForPhase(cur)
	r.setStatus(r.statusText(s.text, cur, 0, false), s.icon, s.color)
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
	if r.stopped {
		return
	}
	s := styleForPhase(phase)
	r.setStatus(r.statusText(s.text, phase, section.Iteration, true), s.icon, s.color)
}

func (r *Reporter) statusText(text string, phase status.Phase, iteration int, withIteration bool) string {
	model := r.modelForPhase(phase)
	if model != "" {
		text += " (" + model + ")"
	}
	if withIteration && iteration > 0 {
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
