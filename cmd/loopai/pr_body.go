package main

import (
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/umputun/ralphex/pkg/git"
)

type reportSection struct {
	heading string
	body    string
}

// factsOnlySummaryPattern identifies the deterministic counters used when report assessment fails.
var factsOnlySummaryPattern = regexp.MustCompile(`^- task iterations: [0-9]+\n- failed retries: [0-9]+\n` +
	`- external reviewers: [0-9]+\n- post-review iterations: [0-9]+(?:\n|$)`)

var reportFencePattern = regexp.MustCompile("^ {0,3}(`{3,}|~{3,})(.*)$")

// splitReportSections ignores the report preamble and keeps nested headings in their section body.
func splitReportSections(report string) []reportSection {
	var sections []reportSection
	var body strings.Builder
	var fence string
	for line := range strings.SplitSeq(strings.ReplaceAll(report, "\r\n", "\n"), "\n") {
		if match := reportFencePattern.FindStringSubmatch(line); match != nil {
			marker, rest := match[1], match[2]
			switch {
			case fence == "":
				// Backtick fence info strings cannot contain backticks.
				if marker[0] != '`' || !strings.ContainsRune(rest, '`') {
					fence = marker
				}
			case marker[0] == fence[0] && len(marker) >= len(fence) && strings.Trim(rest, " \t") == "":
				fence = ""
			}
		}
		if heading, ok := strings.CutPrefix(line, "## "); ok && fence == "" {
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
// A report without an assessed Summary cannot replace the legacy plan overview.
func reportPRBody(report string, stats git.DiffStats) (full, trimmed string, ok bool) {
	sections := make(map[string]string)
	for _, section := range splitReportSections(report) {
		if _, exists := sections[section.heading]; !exists {
			sections[section.heading] = section.body
		}
	}
	summary, ok := sections["Summary"]
	if !ok || (factsOnlySummaryPattern.MatchString(summary) && sections["Risk"] == "_assessment unavailable_" &&
		sections["Migrations and operational steps"] == "_assessment unavailable_") {
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
		planPath, lookupErr := findReportPlanForBranch(gitSvc, target.plansDir, branch)
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
