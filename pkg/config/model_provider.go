package config

import "strings"

var claudeModelPrefixes = []string{"claude", "opus", "sonnet", "haiku", "fable"}
var codexModelPrefixes = []string{"gpt", "codex"}
var codexModelExact = []string{"o1", "o3", "o4"}

// ModelProvider returns ExternalReviewToolClaude or ExternalReviewToolCodex for
// a recognized model[:effort] spec, or "" for unknown names. The lists are
// deliberately prefixes, not regexes; o1/o3/o4 require a whole name or a hyphen suffix.
func ModelProvider(spec string) string {
	model, _, _ := strings.Cut(strings.TrimSpace(spec), ":")
	model = strings.ToLower(model)
	for _, prefix := range claudeModelPrefixes {
		if strings.HasPrefix(model, prefix) {
			return ExternalReviewToolClaude
		}
	}
	for _, prefix := range codexModelPrefixes {
		if strings.HasPrefix(model, prefix) {
			return ExternalReviewToolCodex
		}
	}
	for _, name := range codexModelExact {
		if model == name || strings.HasPrefix(model, name+"-") {
			return ExternalReviewToolCodex
		}
	}
	return ""
}
