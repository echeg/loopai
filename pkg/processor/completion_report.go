package processor

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/umputun/ralphex/pkg/plan"
)

var trailingReportSignal = regexp.MustCompile(`(?:\r?\n)?<<<RALPHEX:[A-Z_]+>>>\s*$`)

// extractReport removes executor preamble and trailing protocol signals from a
// model response, returning only the markdown completion report.
func extractReport(output string) (string, bool) {
	output = strings.ReplaceAll(output, "\r\n", "\n")
	start := -1
	offset := 0
	for line := range strings.SplitSeq(output, "\n") {
		if strings.HasPrefix(line, "# Report:") {
			start = offset
			break
		}
		offset += len(line) + 1
	}
	if start < 0 {
		return "", false
	}
	report := strings.TrimSpace(output[start:])
	for trailingReportSignal.MatchString(report) {
		report = strings.TrimSpace(trailingReportSignal.ReplaceAllString(report, ""))
	}
	return report, report != ""
}

// factsOnlyReport renders the deterministic fallback used when the report model
// is unavailable. The section contract matches the model-authored report.
func factsOnlyReport(record RunRecord, facts RunFacts) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Report: %s\n", reportTitle(record.Plan))
	fmt.Fprintf(&b, "plan: %s | branch: %s | base: %s | mode: %s | executor: %s | task model: %s | review model: %s | started: %s | finished: %s\n",
		valueOrNone(record.Plan), valueOrNone(record.Branch), valueOrNone(record.BaseRef), valueOrNone(string(record.Mode)),
		valueOrNone(record.Executor), valueOrNone(record.TaskModel), valueOrNone(record.ReviewModel),
		formatRecordTime(record.StartedAt), formatRecordTime(record.FinishedAt))

	b.WriteString("\n## Summary\n")
	fmt.Fprintf(&b, "- task iterations: %d\n- failed retries: %d\n- external reviewers: %d\n- post-review iterations: %d\n",
		record.Tasks.Iterations, record.Tasks.FailedRetries, len(record.External), record.PostReview.Iterations)
	writePhaseDurations(&b, record.PhaseDurations)

	b.WriteString("\n## Change scope\n")
	fmt.Fprintf(&b, "- commits: %d\n- files: %d\n- additions: %d\n- deletions: %d\n",
		len(facts.Commits), facts.DiffStats.Files, facts.DiffStats.Additions, facts.DiffStats.Deletions)
	writeFilesTable(&b, facts.Files)

	b.WriteString("\n## Risk\n_assessment unavailable_\n")
	b.WriteString("\n## Migrations and operational steps\n_assessment unavailable_\n")
	b.WriteString("\n## Plan deviation\n_assessment unavailable_\n")
	writeDrift(&b, facts.Drift)

	b.WriteString("\n## Backlog\n")
	writeBacklogTable(&b, facts.Backlog)

	b.WriteString("\n## External review\n")
	writeExternalReviewers(&b, record.External)

	b.WriteString("\n## Validation\n")
	writeStringList(&b, facts.ValidationCommands, true)
	writeValidationTimings(&b, record.Validation)
	return strings.TrimSpace(b.String())
}

func reportTitle(planPath string) string {
	stem := strings.TrimSuffix(filepath.Base(planPath), filepath.Ext(planPath))
	if alt := plan.AltDateBasename(stem + ".md"); alt != "" {
		stem = strings.TrimSuffix(alt, ".md")
	}
	if stem == "" {
		return "completion"
	}
	return strings.ReplaceAll(stem, "-", " ")
}
