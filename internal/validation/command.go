// Package validation provides validation-command matching without depending on
// plan parsing or progress logging packages.
package validation

import "strings"

// MatchCommand returns the normalized configured entry matched by command.
// Returning the configured label lets callers report validation activity
// without persisting provider-supplied arguments or control characters.
func MatchCommand(command string, entries []string) (string, bool) {
	command = NormalizeCommand(command)
	if command == "" {
		return "", false
	}

	for _, entry := range entries {
		entry = NormalizeCommand(entry)
		if entry != "" && (command == entry || strings.HasPrefix(command, entry+" ")) {
			return entry, true
		}
	}
	return "", false
}

// NormalizeCommand strips optional surrounding backticks and collapses
// whitespace so plan parsing and runtime matching use one canonical form.
func NormalizeCommand(command string) string {
	command = strings.Trim(strings.TrimSpace(command), "`")
	return strings.Join(strings.Fields(command), " ")
}
