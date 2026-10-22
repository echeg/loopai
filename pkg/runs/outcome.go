package runs

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"time"
	"unicode/utf8"
)

const (
	// maxOutcomeReport caps Outcome.Report; report.md keeps the full text.
	maxOutcomeReport = 256 << 10
	// maxOutcomeSummary caps Outcome.ReportSummary.
	maxOutcomeSummary = 4 << 10
)

// Finalize is the finalize close-out result recorded in an outcome.
type Finalize struct {
	Mode       string `json:"mode"`
	StatusNote string `json:"status_note"`
	PRURL      string `json:"pr_url"`
	Merged     bool   `json:"merged"`
	Incomplete string `json:"incomplete"`
}

// Outcome is outcome.json, written by the run before it releases its lifetime lock. It describes
// one attempt; ArchiveOutcome keeps earlier attempts as history.
type Outcome struct {
	Version           int       `json:"version"`
	RunID             string    `json:"run_id"`
	Attempt           int       `json:"attempt"`
	Status            Status    `json:"status"` // succeeded, failed, or stopped
	Reason            string    `json:"reason"`
	ExitCode          int       `json:"exit_code"`
	Branch            string    `json:"branch"`
	Commit            string    `json:"commit"`
	WorktreeKept      string    `json:"worktree_kept"`
	Report            string    `json:"report"`         // capped at 256 KiB
	ReportSummary     string    `json:"report_summary"` // capped at 4 KiB
	Risk              string    `json:"risk"`
	Finalize          Finalize  `json:"finalize"`
	PlanMoved         bool      `json:"plan_moved"`
	ArchiveIncomplete string    `json:"archive_incomplete"`
	Notes             []string  `json:"notes"`
	FinishedAt        time.Time `json:"finished_at"`
}

// SaveOutcome writes o to dir/outcome.json with mode 0600. It stamps the schema version on o and
// caps the report and summary at a UTF-8 boundary in what it writes; o keeps the full text.
func SaveOutcome(dir string, o *Outcome) error {
	o.Version = schemaVersion
	rec := *o
	rec.Report = truncateUTF8(rec.Report, maxOutcomeReport)
	rec.ReportSummary = truncateUTF8(rec.ReportSummary, maxOutcomeSummary)
	if rec.Notes == nil {
		rec.Notes = []string{}
	}
	if err := saveRecord(filepath.Join(dir, OutcomeFile), &rec); err != nil {
		return fmt.Errorf("save run outcome: %w", err)
	}
	return nil
}

// LoadOutcome reads dir/outcome.json. A missing file wraps fs.ErrNotExist; a corrupt one, an
// unknown schema version, or a status other than succeeded, failed, or stopped is an error.
func LoadOutcome(dir string) (Outcome, error) {
	path := filepath.Join(dir, OutcomeFile)
	var o Outcome
	if err := loadRecord(path, &o); err != nil {
		return Outcome{}, fmt.Errorf("load run outcome: %w", err)
	}
	if o.Version != schemaVersion {
		return Outcome{}, fmt.Errorf("load run outcome %s: unsupported version %d", path, o.Version)
	}
	switch o.Status {
	case StatusSucceeded, StatusFailed, StatusStopped:
	default:
		return Outcome{}, fmt.Errorf("load run outcome %s: invalid status %q", path, o.Status)
	}
	return o, nil
}

// ArchiveOutcome renames dir/outcome.json to outcome-attempt-<attempt>.json before a resumed
// attempt starts, keeping it as history. A missing outcome (a crashed attempt) is not an error.
func ArchiveOutcome(dir string, attempt int) error {
	src := filepath.Join(dir, OutcomeFile)
	dst := filepath.Join(dir, "outcome-attempt-"+strconv.Itoa(attempt)+".json")
	if err := os.Rename(src, dst); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("archive run outcome: %w", err)
	}
	return nil
}

// truncateUTF8 returns s cut to at most limit bytes without splitting a UTF-8 sequence.
func truncateUTF8(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
