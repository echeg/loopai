// Package runs is the registry of detached loopai runs: per-run directories under
// <checkout>/.loopai/progress/runs/, their descriptor and outcome records, the lifetime and
// launch locks, and the classification and resolution built on them.
package runs

import (
	"path/filepath"
)

// Per-run file names inside a run directory. None of them matches progress-*.txt, which the
// dashboard discovers recursively under .loopai/progress and would replay as a session.
const (
	DescriptorFile  = "run.json"          // the run descriptor
	LockFile        = "run.lock"          // the lifetime lock held by the run process
	JournalFile     = "events.jsonl"      // the event journal
	OutcomeFile     = "outcome.json"      // the current attempt's outcome
	ReportFile      = "report.md"         // the uncapped completion report
	ConsoleLogFile  = "console.log"       // stdout and stderr of every attempt
	StopFile        = "stop-requested"    // the stop marker
	AttachLockFile  = "attach.lock"       // held by an attached ACP provider
	SourcePlansFile = "source-plans.json" // source plan state captured at the first attempt
)

// RunsDir returns <root>/.loopai/progress/runs. root is made absolute against the current
// working directory first, so the path stays valid after a later chdir into a worktree.
func RunsDir(root string) string {
	return filepath.Join(absPath(root), ".loopai", "progress", "runs")
}

// Dir returns the directory of run id under root. The caller validates id with ValidID when it
// comes from outside the registry, so it cannot name a path outside the runs directory.
func Dir(root, id string) string {
	return filepath.Join(RunsDir(root), id)
}

// absPath makes p absolute, falling back to the cleaned path when the working directory is
// unreadable; that failure would break every relative path anyway.
func absPath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return filepath.Clean(p)
	}
	return abs
}
