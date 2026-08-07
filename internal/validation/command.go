// Package validation provides validation-command matching without depending on
// plan parsing or progress logging packages.
package validation

import "strings"

// MatchesCommand reports whether command equals or extends one of entries at a
// whitespace boundary.
func MatchesCommand(command string, entries []string) bool {
	command = normalize(command)
	if command == "" {
		return false
	}

	for _, entry := range entries {
		entry = normalize(entry)
		if entry != "" && (command == entry || strings.HasPrefix(command, entry+" ")) {
			return true
		}
	}
	return false
}

func normalize(command string) string {
	command = strings.Trim(strings.TrimSpace(command), "`")
	return strings.Join(strings.Fields(command), " ")
}
