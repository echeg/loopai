package plan

import "strings"

// Drift contains plan annotations that describe work added, blocked, or skipped during execution.
type Drift struct {
	Added   []string `json:"added"`
	Blocked []string `json:"blocked"`
	Skipped []string `json:"skipped"`
}

// ExtractDrift extracts execution annotations from plan markdown, ignoring fenced examples.
func ExtractDrift(content string) Drift {
	result := Drift{
		Added:   make([]string, 0),
		Blocked: make([]string, 0),
		Skipped: make([]string, 0),
	}
	var ft fenceTracker
	for rawLine := range strings.SplitSeq(content, "\n") {
		sourceLine := strings.TrimSuffix(rawLine, "\r")
		if ft.skip(sourceLine) {
			continue
		}
		line := strings.TrimSpace(sourceLine)
		annotation := line
		if len(annotation) >= 2 && strings.ContainsAny(annotation[:1], "-*+") &&
			(annotation[1] == ' ' || annotation[1] == '\t') {
			annotation = strings.TrimSpace(annotation[2:])
			if matches := checkboxPattern.FindStringSubmatch("- " + annotation); len(matches) >= 3 {
				annotation = strings.TrimSpace(matches[2])
			}
		}
		switch {
		case strings.HasPrefix(annotation, "➕"):
			result.Added = append(result.Added, annotation)
		case strings.HasPrefix(annotation, "⚠️"):
			result.Blocked = append(result.Blocked, annotation)
		}
		if matches := checkboxPattern.FindStringSubmatch(line); len(matches) >= 3 &&
			strings.Contains(strings.ToLower(matches[2]), "(skipped") {
			result.Skipped = append(result.Skipped, strings.TrimSpace(matches[2]))
		}
	}
	return result
}
