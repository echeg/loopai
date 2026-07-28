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
	"strings"
	"time"
)

const (
	// execTimeout bounds a single cmux CLI call, the socket is local so a hanging call must not stall the run.
	execTimeout = 2 * time.Second

	// workspaceEnv is injected by cmux into every terminal it owns and inherited by child processes.
	workspaceEnv = "CMUX_WORKSPACE_ID"

	// binName is the cmux CLI binary looked up in PATH.
	binName = "cmux"
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
