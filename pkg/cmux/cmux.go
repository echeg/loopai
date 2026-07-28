// Package cmux reports ralphex execution state to the sidebar of the cmux terminal.
//
// the integration is best-effort and one-directional: it shells out to the public cmux CLI and
// never lets a failure reach the run. outside cmux (no binary in PATH or no CMUX_WORKSPACE_ID)
// New returns a nil *Reporter and every exported method is a no-op, so callers never check for nil.
package cmux

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/umputun/ralphex/pkg/status"
)

const (
	// execTimeout bounds a single cmux CLI call, the socket is local so a hanging call must not stall the run.
	execTimeout = 2 * time.Second

	// workspaceEnv is injected by cmux into every terminal it owns and inherited by child processes.
	workspaceEnv = "CMUX_WORKSPACE_ID"

	// binName is the cmux CLI binary looked up in PATH.
	binName = "cmux"

	// statusKey names both the pill and the sidebar loader entry. an own key is used instead of an
	// agent key like claude_code because cmux shows non-allowlisted pills unconditionally, while
	// allowlisted ones are hidden unless it sees a live pid of that agent.
	statusKey = "ralphex"

	// statusPriority orders the ralphex pill among other pills in the tab row.
	statusPriority = "90"

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
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
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
	timeout  time.Duration // per-call timeout
}

// New returns a reporter for the current cmux workspace, or nil when ralphex does not run
// inside cmux. all methods are nil-safe, so the result can be used without a nil check.
func New(planFile string) *Reporter {
	if strings.TrimSpace(os.Getenv(workspaceEnv)) == "" {
		return nil
	}
	bin, err := exec.LookPath(binName)
	if err != nil {
		return nil
	}
	return &Reporter{runner: &execRunner{bin: bin}, planFile: planFile, timeout: execTimeout}
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

// LoadingOn shows the spinner in the sidebar and moves the workspace lane to "working".
// this is the only path to a real running signal that needs neither the agent allowlist nor vault registration.
func (r *Reporter) LoadingOn() { r.exec("workspace", "loading", "on", "--id", statusKey) }

// LoadingOff removes the spinner from the sidebar.
func (r *Reporter) LoadingOff() { r.exec("workspace", "loading", "off", "--id", statusKey) }

// Status sets the ralphex pill in the tab row. empty icon or color skip their own flags,
// leaving the cmux-side default instead of passing an empty value.
func (r *Reporter) Status(text, icon, color string) {
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

// ClearStatus removes the ralphex pill.
func (r *Reporter) ClearStatus() { r.exec("clear-status", statusKey) }

// Progress sets the sidebar progress bar from a done/total pair. a non-positive total skips the
// call entirely, so there is no division by zero, and the ratio is clamped to [0, 1] because a
// plan may report more done tasks than parsed ones.
func (r *Reporter) Progress(done, total int, label string) {
	if total <= 0 {
		return
	}
	ratio := float64(done) / float64(total)
	switch {
	case ratio < 0:
		ratio = 0
	case ratio > 1:
		ratio = 1
	}
	args := []string{"set-progress", strconv.FormatFloat(ratio, 'f', 2, 64)}
	if label != "" {
		args = append(args, "--label", label)
	}
	r.exec(args...)
}

// ClearProgress removes the sidebar progress bar.
func (r *Reporter) ClearProgress() { r.exec("clear-progress") }

// Notify raises a cmux notification: blue ring on the panel, macOS banner and an entry in the
// notification panel. empty subtitle or body skip their own flags, the body is truncated to fit the banner.
func (r *Reporter) Notify(title, subtitle, body string) {
	args := []string{"notify", "--title", title}
	if subtitle != "" {
		args = append(args, "--subtitle", subtitle)
	}
	if body != "" {
		args = append(args, "--body", truncateRunes(body, notifyBodyLimit))
	}
	r.exec(args...)
}

// Clear removes every sidebar artifact ralphex owns. idempotent, calling it twice is safe,
// which matters because cleanup runs both from a defer and from the interrupt handler.
func (r *Reporter) Clear() {
	r.LoadingOff()
	r.ClearStatus()
	r.ClearProgress()
}

// OnPhase updates the pill on a phase change, the signature matches status.PhaseHolder.OnChange.
func (r *Reporter) OnPhase(_, cur status.Phase) {
	s := styleForPhase(cur)
	r.Status(s.text, s.icon, s.color)
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
