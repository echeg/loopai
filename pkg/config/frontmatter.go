package config

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// Options holds agent options parsed from YAML frontmatter in agent files.
// a non-empty Description marks the agent as dynamic (project-specific): it is
// listed in the {{agents:dynamic}} catalog for model-side selection during review.
type Options struct {
	Model       string `yaml:"model"`
	AgentType   string `yaml:"agent"`
	Description string `yaml:"description"`
}

var validModels = map[string]bool{"haiku": true, "sonnet": true, "opus": true, "fable": true}

// String returns a human-readable summary of the options for logging.
func (o Options) String() string {
	model := o.Model
	if model == "" {
		model = "default"
	}
	subagent := o.AgentType
	if subagent == "" {
		subagent = "general-purpose"
	}
	return fmt.Sprintf("model=%s, subagent=%s", model, subagent)
}

// Validate returns warnings for invalid option values.
// called after parseOptions which normalizes model to keyword form.
func (o Options) Validate() []string {
	var warnings []string
	if o.Model != "" && !validModels[o.Model] {
		warnings = append(warnings, fmt.Sprintf("unknown model %q, must be one of: haiku, sonnet, opus, fable", o.Model))
	}
	return warnings
}

// ParseAgentOptions extracts agent frontmatter options and the agent body from raw
// agent file content. Exported for callers outside the loader: the --gen-agents mode
// reports the descriptions of freshly written agent files without reloading config.
// It applies the same normalization the loader does (CRLF endings, surrounding
// whitespace, leading comment lines) so the reported description always matches the
// one the review phase will use.
func ParseAgentOptions(content string) (opts Options, body string) {
	return parseOptionsWithCommentRetry(strings.TrimSpace(normalizeCRLF(content)))
}

// AgentFileHasBody reports whether an agent file contributes a prompt body. A file
// that is only comments, whitespace, or frontmatter is ignored by the loader in favor
// of the embedded default with the same name, and dropped entirely when no embedded
// default exists. Exported so the --gen-agents report agrees with the loader instead
// of listing an inert file as a working agent.
func AgentFileHasBody(content string) bool {
	_, body := agentPromptBody(content)
	return body != ""
}

// AgentFrontmatterUnparsable reports whether content opens a frontmatter block that
// neither parse attempt could read. Exported for the --gen-agents report: parsing
// returns the whole content as the body both for a broken block and for a file with
// no frontmatter at all, so the reported reason would otherwise be a guess. Checking
// the parsed body for a "---" prefix is not enough — a working agent may open its
// body with a markdown rule, and a broken block behind a leading comment line never
// reaches the parsed form at all.
func AgentFrontmatterUnparsable(content string) bool {
	normalized := strings.TrimSpace(normalizeCRLF(content))
	if !strings.HasPrefix(strings.TrimSpace(stripLeadingCommentLines(normalized)), "---\n") {
		return false
	}
	// parseOptionsWithCommentRetry returns its input unchanged only when no attempt
	// found parsable frontmatter; a parsed block always yields a shorter body
	opts, body := parseOptionsWithCommentRetry(normalized)
	return opts == (Options{}) && body == normalized
}

// agentPromptBody normalizes agent file content and returns the frontmatter options
// and prompt body the loader sees. Comments are stripped before parsing so an
// all-commented file reads as empty and a "# ..." header above "---" does not hide
// the frontmatter.
func agentPromptBody(content string) (Options, string) {
	normalized := strings.TrimSpace(normalizeCRLF(content))
	stripped := strings.TrimSpace(stripComments(normalized))
	return parseOptions(stripped)
}

// parseOptionsWithCommentRetry parses frontmatter, retrying after leading comment
// lines are stripped so a "# ..." header written above "---" does not hide the
// options. Returns the zero Options and the original content when neither attempt
// finds frontmatter.
func parseOptionsWithCommentRetry(content string) (Options, string) {
	opts, body := parseOptions(content)
	if opts != (Options{}) || body != content {
		return opts, body
	}
	stripped := stripLeadingCommentLines(content)
	if stripped == content {
		return opts, body
	}
	if strippedOpts, strippedBody := parseOptions(stripped); strippedOpts != (Options{}) {
		return strippedOpts, strippedBody
	}
	return Options{}, content
}

// normalizeModel extracts the keyword (haiku, sonnet, opus, fable) from a model string.
// e.g. "claude-sonnet-4-5-20250929" → "sonnet", "opus" → "opus", "" → "".
func normalizeModel(model string) string {
	lower := strings.ToLower(model)
	for kw := range validModels {
		if strings.Contains(lower, kw) {
			return kw
		}
	}
	return model // return as-is if no keyword found (Validate will catch it)
}

// parseOptions extracts agent options from YAML frontmatter delimited by "---".
// we only support YAML with "---" delimiters because agent files are our own format —
// no need for TOML/JSON/multi-format support that libraries like adrg/frontmatter provide.
// CutPrefix + Cut handle delimiter splitting without index arithmetic.
// returns parsed options and body. if no frontmatter, returns zero value and original content.
func parseOptions(content string) (Options, string) {
	after, found := strings.CutPrefix(content, "---\n")
	if !found {
		return Options{}, content
	}

	header, body, found := strings.Cut(after, "\n---")
	if !found {
		return Options{}, content
	}
	// closing delimiter must be on its own line
	if body != "" && body[0] != '\n' {
		return Options{}, content
	}

	var opts Options
	if err := yaml.Unmarshal([]byte(header), &opts); err != nil {
		return Options{}, content // malformed YAML → treat as no frontmatter
	}

	opts.Model = normalizeModel(opts.Model)

	return opts, strings.TrimSpace(body)
}
