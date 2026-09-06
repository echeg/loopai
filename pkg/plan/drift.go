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
		switch {
		case strings.HasPrefix(line, "➕"):
			result.Added = append(result.Added, line)
		case strings.HasPrefix(line, "⚠️"):
			result.Blocked = append(result.Blocked, line)
		}
		if matches := checkboxPattern.FindStringSubmatch(line); len(matches) >= 3 &&
			strings.Contains(strings.ToLower(matches[2]), "(skipped") {
			result.Skipped = append(result.Skipped, strings.TrimSpace(matches[2]))
		}
	}
	return result
}
