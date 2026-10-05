package main

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/umputun/ralphex/pkg/git"
)

type reportSection struct {
	heading string
	body    string
}

// splitReportSections ignores the report preamble and keeps nested headings in their section body.
func splitReportSections(report string) []reportSection {
	var sections []reportSection
	var body strings.Builder
	for line := range strings.SplitSeq(strings.ReplaceAll(report, "\r\n", "\n"), "\n") {
		if heading, ok := strings.CutPrefix(line, "## "); ok {
			if len(sections) > 0 {
				sections[len(sections)-1].body = strings.TrimSpace(body.String())
				body.Reset()
			}
			sections = append(sections, reportSection{heading: strings.TrimSpace(heading)})
			continue
		}
		if len(sections) > 0 {
			body.WriteString(line)
			body.WriteByte('\n')
		}
	}
	if len(sections) > 0 {
		sections[len(sections)-1].body = strings.TrimSpace(body.String())
	}
	return sections
}

// reportPRBody renders selected report sections, with and without the optional details blocks.
// A report without Summary cannot replace the legacy plan overview.
func reportPRBody(report string, stats git.DiffStats) (full, trimmed string, ok bool) {
	sections := make(map[string]string)
	for _, section := range splitReportSections(report) {
		if _, exists := sections[section.heading]; !exists {
			sections[section.heading] = section.body
		}
	}
	summary, ok := sections["Summary"]
	if !ok {
		return "", "", false
	}

	var parts []string
	if summary != "" {
		parts = append(parts, summary)
	}
	for _, heading := range []string{"Evidence", "Merge danger", "Risk", "Migrations and operational steps", "Plan deviation"} {
		if body, exists := sections[heading]; exists {
			parts = append(parts, "## "+heading+"\n\n"+body)
		}
	}
	statsText := fmt.Sprintf("## Changes\n\n- Files changed: %d\n- Additions: %d\n- Deletions: %d",
		stats.Files, stats.Additions, stats.Deletions)
	trimmed = strings.Join(parts, "\n\n")
	if trimmed != "" {
		trimmed += "\n\n"
	}
	trimmed += statsText

	for _, heading := range []string{"External review", "Validation"} {
		if body, exists := sections[heading]; exists {
			parts = append(parts, "<details><summary>"+heading+"</summary>\n\n"+body+"\n\n</details>")
		}
	}
	parts = append(parts, statsText)
	return strings.Join(parts, "\n\n"), trimmed, true
}

// fitPRBody drops details first, then falls back to the legacy body to respect GitHub's rune limit.
func fitPRBody(full, trimmed, legacy string) string {
	for _, body := range []string{full, trimmed, legacy} {
		if utf8.RuneCountInString(body) <= maxPRBodyRunes {
			return body
		}
	}
	// Preserve legacy metadata validation when even the plan overview exceeds the limit.
	return legacy
}

// buildReportPRTitleBody enriches legacy metadata with the optional completion report.
// Lookup failures are warnings so reports can never prevent opening a pull request.
func buildReportPRTitleBody(gitSvc *git.Service, target closeoutTarget, branch string, stats git.DiffStats,
	stderr io.Writer) (title, body string, err error) {
	title, legacy, err := buildPRTitleBody(gitSvc.Root(), target.plansDir, branch, stats)
	if err != nil {
		return "", "", err
	}
	report := target.report
	if report == "" {
		planPath, lookupErr := findPRPlan(gitSvc.Root(), target.plansDir, branch)
		if lookupErr == nil {
			var content []byte
			content, _, lookupErr = locateCompletionReport(gitSvc, target.plansDir, planPath, branch)
			report = string(content)
		}
		if lookupErr != nil && !errors.Is(lookupErr, errCompletionReportNotFound) {
			fmt.Fprintf(stderr, "warning: completion report unavailable for PR body: %v\n", lookupErr)
		}
	}
	if full, trimmed, ok := reportPRBody(report, stats); ok {
		return title, fitPRBody(full, trimmed, legacy), nil
	}
	return title, legacy, nil
}
