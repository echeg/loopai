package processor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	gitpkg "github.com/umputun/ralphex/pkg/git"
	"github.com/umputun/ralphex/pkg/plan"
)

// RunFactsSource provides deterministic repository facts used by completion reports.
type RunFactsSource interface {
	CommitsBetween(base, head string) ([]gitpkg.Commit, error)
	DiffNameStatus(base string) ([]gitpkg.FileChange, error)
	DiffStats(base string) (gitpkg.DiffStats, error)
}

// BacklogFile describes an added backlog entry and its optional markdown title.
type BacklogFile struct {
	Status string `json:"status"`
	Path   string `json:"path"`
	Title  string `json:"title"`
}

// RunFacts contains deterministic repository and plan facts collected at report time.
type RunFacts struct {
	Commits            []gitpkg.Commit     `json:"commits"`
	Files              []gitpkg.FileChange `json:"files"`
	DiffStats          gitpkg.DiffStats    `json:"diff_stats"`
	Backlog            []BacklogFile       `json:"backlog"`
	ValidationCommands []string            `json:"validation_commands"`
	Drift              plan.Drift          `json:"drift"`
}

// SetRunFactsSource configures deterministic repository fact collection.
func (r *Runner) SetRunFactsSource(source RunFactsSource) {
	r.factsSource = source
}

func (r *Runner) collectRunFacts(ctx context.Context) RunFacts {
	_ = ctx // fact-source methods are synchronous; retained for the report phase contract.
	facts := RunFacts{
		Commits:            make([]gitpkg.Commit, 0),
		Files:              make([]gitpkg.FileChange, 0),
		Backlog:            make([]BacklogFile, 0),
		ValidationCommands: make([]string, 0),
		Drift: plan.Drift{
			Added: make([]string, 0), Blocked: make([]string, 0), Skipped: make([]string, 0),
		},
	}

	r.collectRepositoryFacts(&facts)

	parsedPlan, err := plan.ParsePlanFile(r.cfg.PlanFile)
	if err != nil {
		r.logRunFactsError("validation commands", err)
	} else {
		facts.ValidationCommands = parsedPlan.ValidationCommands
	}
	content, err := os.ReadFile(r.cfg.PlanFile)
	if err != nil {
		r.logRunFactsError("plan drift", err)
	} else {
		facts.Drift = plan.ExtractDrift(string(content))
	}

	backlog, err := collectBacklogFiles(facts.Files, r.backlogDir())
	if err != nil {
		r.logRunFactsError("backlog", err)
	} else {
		facts.Backlog = backlog
	}
	return facts
}

func (r *Runner) collectRepositoryFacts(facts *RunFacts) {
	if r.factsSource == nil {
		return
	}
	commits, err := r.factsSource.CommitsBetween(r.cfg.DefaultBranch, "HEAD")
	if err != nil {
		r.logRunFactsError("commits", err)
	} else {
		facts.Commits = commits
	}
	files, err := r.factsSource.DiffNameStatus(r.cfg.DefaultBranch)
	if err != nil {
		r.logRunFactsError("files", err)
	} else {
		facts.Files = files
	}
	stats, err := r.factsSource.DiffStats(r.cfg.DefaultBranch)
	if err != nil {
		r.logRunFactsError("diff stats", err)
	} else {
		facts.DiffStats = stats
	}
}

func (r *Runner) backlogDir() string {
	if r.cfg.AppConfig == nil || r.cfg.AppConfig.BacklogDir == "" {
		return "docs/backlog"
	}
	return r.cfg.AppConfig.BacklogDir
}

func (r *Runner) logRunFactsError(field string, err error) {
	if r.log != nil {
		r.log.Print("run facts: %s: %v", field, err)
	}
}

func collectBacklogFiles(changes []gitpkg.FileChange, backlogDir string) ([]BacklogFile, error) {
	result := make([]BacklogFile, 0)
	dir := filepath.Clean(backlogDir)
	for _, change := range changes {
		path := filepath.Clean(filepath.FromSlash(change.Path))
		if change.Status != "A" || !pathWithinDir(path, dir) {
			continue
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", change.Path, err)
		}
		result = append(result, BacklogFile{
			Status: change.Status, Path: change.Path, Title: markdownTitle(string(content)),
		})
	}
	return result, nil
}

func pathWithinDir(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func markdownTitle(content string) string {
	for rawLine := range strings.SplitSeq(content, "\n") {
		line := strings.TrimSpace(strings.TrimSuffix(rawLine, "\r"))
		if title, ok := strings.CutPrefix(line, "# "); ok {
			return strings.TrimSpace(title)
		}
	}
	return ""
}
