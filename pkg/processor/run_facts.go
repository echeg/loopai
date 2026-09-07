package processor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

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
	}
	facts.Backlog = backlog
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
	var readErrors []error
	dir := filepath.Clean(backlogDir)
	for _, change := range changes {
		path := filepath.Clean(filepath.FromSlash(change.Path))
		if change.Status != "A" || !pathWithinDir(path, dir) {
			continue
		}
		content, err := os.ReadFile(path)
		if err != nil {
			readErrors = append(readErrors, fmt.Errorf("read %s: %w", change.Path, err))
		}
		result = append(result, BacklogFile{
			Status: change.Status, Path: change.Path, Title: markdownTitle(string(content)),
		})
	}
	return result, errors.Join(readErrors...)
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

// renderRunFacts renders deterministic run facts for the report model. Numeric
// durations remain milliseconds so the model can copy Go-provided values exactly.
func renderRunFacts(record RunRecord, facts RunFacts) string {
	var b strings.Builder
	b.WriteString("## Metadata\n")
	writeMetadata(&b, record)

	b.WriteString("\n## Phase durations\n")
	writePhaseDurations(&b, record.PhaseDurations)

	b.WriteString("\n## Files\n")
	writeFilesTable(&b, facts.Files)

	fmt.Fprintf(&b, "\n## Diff totals\n- files: %d\n- additions: %d\n- deletions: %d\n",
		facts.DiffStats.Files, facts.DiffStats.Additions, facts.DiffStats.Deletions)

	b.WriteString("\n## Commits\n")
	if len(facts.Commits) == 0 {
		b.WriteString("- none\n")
	} else {
		for _, commit := range facts.Commits {
			fmt.Fprintf(&b, "- `%s` %s\n", inlineCode(commit.Hash), commit.Subject)
		}
	}

	b.WriteString("\n## Backlog files\n")
	writeBacklogTable(&b, facts.Backlog)

	b.WriteString("\n## Plan drift\n")
	writeDrift(&b, facts.Drift)

	b.WriteString("\n## Validation\n### Commands\n")
	writeStringList(&b, facts.ValidationCommands, true)
	b.WriteString("### Timings\n")
	writeValidationTimings(&b, record.Validation)

	b.WriteString("\n## External reviewers\n")
	writeExternalReviewers(&b, record.External)
	return strings.TrimSpace(b.String())
}

func writeMetadata(b *strings.Builder, record RunRecord) {
	fields := []struct{ name, value string }{
		{"plan", record.Plan}, {"branch", record.Branch}, {"base", record.BaseRef},
		{"mode", string(record.Mode)}, {"executor", record.Executor}, {"task model", record.TaskModel},
		{"review model", record.ReviewModel}, {"started", formatRecordTime(record.StartedAt)},
		{"finished", formatRecordTime(record.FinishedAt)},
	}
	for _, field := range fields {
		value := field.value
		if value == "" {
			value = "not recorded"
		}
		fmt.Fprintf(b, "- %s: %s\n", field.name, value)
	}
	fmt.Fprintf(b, "- task iterations: %d\n- task failed retries: %d\n", record.Tasks.Iterations, record.Tasks.FailedRetries)
	fmt.Fprintf(b, "- internal review first ran: %t\n- internal review loop iterations: %d\n- internal review ended by: %s\n",
		record.InternalReview.FirstRan, record.InternalReview.LoopIterations, valueOrNone(record.InternalReview.EndedBy))
	fmt.Fprintf(b, "- post-review ran: %t\n- post-review iterations: %d\n", record.PostReview.Ran, record.PostReview.Iterations)
}

func writePhaseDurations(b *strings.Builder, durations map[string]Duration) {
	if len(durations) == 0 {
		b.WriteString("- none\n")
		return
	}
	keys := make([]string, 0, len(durations))
	for key := range durations {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	b.WriteString("| phase | duration_ms |\n|---|---:|\n")
	for _, key := range keys {
		fmt.Fprintf(b, "| %s | %d |\n", tableCell(key), time.Duration(durations[key]).Milliseconds())
	}
}

func writeValidationTimings(b *strings.Builder, validation *ValidationRunRecord) {
	if validation == nil {
		b.WriteString("- not recorded\n")
		return
	}
	fmt.Fprintf(b, "- duration_ms: %d\n- runs: %d\n", time.Duration(validation.Duration).Milliseconds(), validation.Runs)
}

func writeFilesTable(b *strings.Builder, files []gitpkg.FileChange) {
	if len(files) == 0 {
		b.WriteString("- none\n")
		return
	}
	b.WriteString("| status | path |\n|---|---|\n")
	for _, file := range files {
		fmt.Fprintf(b, "| %s | %s |\n", tableCell(file.Status), tableCell(file.Path))
	}
}

func writeBacklogTable(b *strings.Builder, backlog []BacklogFile) {
	if len(backlog) == 0 {
		b.WriteString("- none\n")
		return
	}
	b.WriteString("| status | path | title |\n|---|---|---|\n")
	for _, file := range backlog {
		fmt.Fprintf(b, "| %s | %s | %s |\n", tableCell(file.Status), tableCell(file.Path), tableCell(valueOrNone(file.Title)))
	}
}

func writeDrift(b *strings.Builder, drift plan.Drift) {
	groups := []struct {
		name  string
		items []string
	}{{"Added", drift.Added}, {"Blocked", drift.Blocked}, {"Skipped", drift.Skipped}}
	for _, group := range groups {
		fmt.Fprintf(b, "### %s\n", group.name)
		writeStringList(b, group.items, false)
	}
}

func writeExternalReviewers(b *strings.Builder, reviewers []ExternalReviewerRecord) {
	if len(reviewers) == 0 {
		b.WriteString("- none\n")
		return
	}
	// Also bound records persisted by older versions, without changing the caller's snapshot.
	reviewers = cloneRunRecord(RunRecord{External: reviewers}).External
	boundExternalReviewText(reviewers)
	for _, reviewer := range reviewers {
		fmt.Fprintf(b, "### %s\n", valueOrNone(reviewer.Key))
		fmt.Fprintf(b, "- label: %s\n- iterations: %d\n- duration_ms: %d\n- ended by: %s\n- had findings: %t\n",
			valueOrNone(reviewer.Label), len(reviewer.Iterations), time.Duration(reviewer.Duration).Milliseconds(),
			valueOrNone(reviewer.EndedBy), reviewer.HadFindings)
		for _, iteration := range reviewer.Iterations {
			fmt.Fprintf(b, "#### Iteration %d\n- truncated: %t\n", iteration.Index, iteration.Truncated)
			b.WriteString("Reviewer output:\n")
			writeFencedBlock(b, iteration.ReviewerOutput)
			b.WriteString("Evaluator response:\n")
			writeFencedBlock(b, iteration.EvaluatorResponse)
		}
	}
}

func writeStringList(b *strings.Builder, items []string, code bool) {
	if len(items) == 0 {
		b.WriteString("- none\n")
		return
	}
	for _, item := range items {
		if code {
			fmt.Fprintf(b, "- `%s`\n", inlineCode(item))
		} else {
			fmt.Fprintf(b, "- %s\n", item)
		}
	}
}

func writeFencedBlock(b *strings.Builder, content string) {
	fence := "```"
	for strings.Contains(content, fence) {
		fence += "`"
	}
	fmt.Fprintf(b, "%s\n%s\n%s\n", fence, content, fence)
}

func tableCell(value string) string {
	return strings.NewReplacer("|", "\\|", "\r", " ", "\n", " ").Replace(value)
}

func inlineCode(value string) string {
	return strings.ReplaceAll(value, "`", "\\`")
}

func formatRecordTime(value time.Time) string {
	if value.IsZero() {
		return "not recorded"
	}
	return value.UTC().Format(time.RFC3339)
}

func valueOrNone(value string) string {
	if value == "" {
		return "none"
	}
	return value
}
